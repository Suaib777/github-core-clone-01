// app.ts — the forge's HTTP server: routing, auth, request ids and body reading.
// The surfaces: git smart HTTP at /{owner}/{repo}.git, REST v3 under /api/v3, GraphQL v4
// at /api/graphql and MCP at /mcp. Every REST/GraphQL/MCP reply is JSON with a non-empty
// X-GitHub-Request-Id; an error carries a JSON body with a string message; anything else is
// a JSON 404. main.ts owns the entry <-> env lifecycle and stays as it is.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { infoRefs, receivePack, uploadPack } from './gitproto.ts';
import { handleGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { routeRest } from './rest.ts';
import { Store } from './store.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const EXPECTED_TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';

/** A monotonic request counter; the request id is deterministic per request sequence. */
let requestSeq = 0;
function requestId(): string {
  requestSeq += 1;
  return `req-${String(requestSeq).padStart(8, '0')}`;
}

function isAuthenticated(req: IncomingMessage): boolean {
  const auth = req.headers.authorization ?? '';
  if (!auth.startsWith('Bearer ')) return false;
  const token = auth.slice('Bearer '.length).trim();
  if (token === '') return false;
  if (EXPECTED_TOKEN !== '' && token !== EXPECTED_TOKEN) return false;
  return true;
}

function sendJson(res: ServerResponse, status: number, body: unknown, extraHeaders: Record<string, string> = {}): void {
  const text = typeof body === 'string' ? body : `${JSON.stringify(body)}\n`;
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': requestId(),
    ...extraHeaders,
  });
  res.end(text);
}

function sendBuffer(res: ServerResponse, status: number, contentType: string, body: Buffer, extraHeaders: Record<string, string> = {}): void {
  res.writeHead(status, {
    'content-type': contentType,
    'content-length': body.length,
    'x-github-request-id': requestId(),
    ...extraHeaders,
  });
  res.end(body);
}

function sendEmpty(res: ServerResponse, status: number): void {
  res.writeHead(status, { 'x-github-request-id': requestId() });
  res.end();
}

/** Read the whole request body (bounded; the forge's largest body is a git pack). */
function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > 64 * 1024 * 1024) {
        reject(new Error('request body too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function parseUrl(req: IncomingMessage): { path: string; query: URLSearchParams } {
  try {
    const url = new URL(req.url ?? '/', 'http://entry');
    return { path: url.pathname, query: url.searchParams };
  } catch {
    return { path: '/', query: new URLSearchParams() };
  }
}

function decodeSegment(segment: string): string {
  // Most path segments carry no percent-encoding; skip the decode in that case.
  if (!segment.includes('%')) return segment;
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

export function createApp(): Server {
  const store = new Store();
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      const { path, query } = parseUrl(req);
      const method = req.method ?? 'GET';
      const segments = path.split('/').filter((part) => part !== '').map(decodeSegment);

      // The env's boot check calls this twice: 200 {"ok":true}. No auth.
      if (method === 'GET' && path === '/healthz') {
        sendJson(res, 200, { ok: true });
        return;
      }

      // Git smart HTTP: /{owner}/{repo}.git/info/refs | git-upload-pack | git-receive-pack.
      if (segments.length >= 3 && segments[1] !== undefined && segments[1].endsWith('.git')) {
        const owner = segments[0] as string;
        const repoName = (segments[1] as string).slice(0, -'.git'.length);
        if (!isAuthenticated(req)) {
          sendJson(res, 401, { message: 'Requires authentication' });
          return;
        }
        if (method === 'GET' && segments[2] === 'info' && segments[3] === 'refs') {
          const service = query.get('service') ?? '';
          const result = infoRefs(store, owner, repoName, service, req.headers['git-protocol']);
          if (result.status === 200) {
            sendBuffer(res, 200, result.contentType, result.body);
          } else {
            sendJson(res, result.status, JSON.parse(result.body.toString('utf8')));
          }
          return;
        }
        if (method === 'POST' && segments[2] === 'git-upload-pack') {
          const body = await readBody(req);
          const result = uploadPack(store, owner, repoName, body, req.headers['git-protocol']);
          if (result.status === 200) {
            sendBuffer(res, 200, result.contentType, result.body);
          } else {
            sendJson(res, result.status, JSON.parse(result.body.toString('utf8')));
          }
          return;
        }
        if (method === 'POST' && segments[2] === 'git-receive-pack') {
          const body = await readBody(req);
          const result = receivePack(store, owner, repoName, body);
          if (result.status === 200) {
            sendBuffer(res, 200, result.contentType, result.body);
          } else {
            sendJson(res, result.status, JSON.parse(result.body.toString('utf8')));
          }
          return;
        }
        sendJson(res, 404, { message: 'Not Found' });
        return;
      }

      // GraphQL v4.
      if (method === 'POST' && path === '/api/graphql') {
        if (!isAuthenticated(req)) {
          sendJson(res, 401, { message: 'Requires authentication' });
          return;
        }
        const raw = await readBody(req);
        let body: unknown = null;
        try {
          body = raw.length ? JSON.parse(raw.toString('utf8')) : null;
        } catch {
          sendJson(res, 400, { message: 'malformed JSON body' });
          return;
        }
        const result = handleGraphQL(store, body);
        sendJson(res, result.status, result.body);
        return;
      }

      // MCP over Streamable HTTP.
      if (method === 'POST' && path === '/mcp') {
        if (!isAuthenticated(req)) {
          sendJson(res, 401, { message: 'Requires authentication' });
          return;
        }
        const raw = await readBody(req);
        const result = handleMcp(store, raw);
        if (result.status === 202) {
          sendEmpty(res, 202);
          return;
        }
        sendJson(res, result.status, result.body);
        return;
      }

      // REST v3.
      if (path === '/api/v3' || path.startsWith('/api/v3/')) {
        const restSegments = path === '/api/v3' ? [] : path.slice('/api/v3/'.length).split('/').filter((part) => part !== '').map(decodeSegment);
        // The API root and /meta are public; everything else needs a token.
        const needsAuth = !(restSegments.length === 0 || (restSegments.length === 1 && restSegments[0] === 'meta'));
        if (needsAuth && !isAuthenticated(req)) {
          sendJson(res, 401, { message: 'Requires authentication' });
          return;
        }
        let body: unknown = null;
        if (method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE') {
          const raw = await readBody(req);
          if (raw.length > 0) {
            try {
              body = JSON.parse(raw.toString('utf8'));
            } catch {
              sendJson(res, 400, { message: 'malformed JSON body' });
              return;
            }
          }
        }
        const host = req.headers.host ?? 'localhost';
        const result = routeRest(store, method, restSegments, query, body, host);
        if (result.status === 204) {
          sendEmpty(res, 204);
          return;
        }
        sendJson(res, result.status, result.body, result.headers ?? {});
        return;
      }

      // Everything else, every method: GitHub's 404 body.
      sendJson(res, 404, { message: 'Not Found' });
    })().catch((err: unknown) => {
      // A handler must never hang the connection: answer 500 JSON and keep serving.
      try {
        sendJson(res, 500, { message: err instanceof Error ? err.message : 'Internal Server Error' });
      } catch {
        try {
          res.destroy();
        } catch {
          // Nothing left to do.
        }
      }
    });
  });
}

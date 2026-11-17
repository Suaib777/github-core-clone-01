// mcp.ts — Model Context Protocol over Streamable HTTP at /mcp (JSON-RPC 2.0).
// Speaks the modern revision (2026-07-28) via server/discover, and the legacy handshake
// (initialize + notifications/initialized) for older clients. The six required tools act on
// the same store as REST, so a repository or issue created over MCP reads back over /api/v3
// as the same object. No Authorization header -> 401. A body that is not JSON -> HTTP 400
// with JSON-RPC -32700. An unknown tool -> -32602. A call missing a required argument ->
// isError:true.

import { issueJson, pullJson, repoJson } from './rest.ts';
import { VIEWER, type Store } from './store.ts';

export const MCP_SERVER_NAME = 'github-forge';
export const MCP_SERVER_VERSION = '1.0.0';
export const MCP_SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

interface ToolDef {
  name: string;
  description: string;
  inputSchema: {
    type: 'object';
    properties: Record<string, { type: string; description: string }>;
    required: string[];
  };
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The repository name.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
        owner: { type: 'string', description: 'The owner login (defaults to the authenticated user).' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The issue title.' },
        body: { type: 'string', description: 'The issue body.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        state: { type: 'string', description: 'Filter by state: open, closed or all.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The pull request title.' },
        body: { type: 'string', description: 'The pull request body.' },
        head: { type: 'string', description: 'The head branch name.' },
        base: { type: 'string', description: 'The base branch name.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        pull_number: { type: 'number', description: 'The pull request number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

type JsonRpcId = string | number | null;

function rpcResult(id: JsonRpcId, result: unknown): { status: number; body: unknown } {
  return { status: 200, body: { jsonrpc: '2.0', id, result } };
}

function rpcError(id: JsonRpcId, code: number, message: string): { status: number; body: unknown } {
  return { status: 200, body: { jsonrpc: '2.0', id, error: { code, message } } };
}

function toolResult(text: string, isError = false): Record<string, unknown> {
  return { content: [{ type: 'text', text }], isError };
}

function str(value: unknown): string | undefined {
  return typeof value === 'string' && value !== '' ? value : undefined;
}

function num(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

/** Split "owner/repo" into its parts. */
function splitFullName(value: string): { owner: string; repo: string } | null {
  const slash = value.indexOf('/');
  if (slash <= 0 || slash === value.length - 1) return null;
  return { owner: value.slice(0, slash), repo: value.slice(slash + 1) };
}

/** Resolve owner/repo from tool arguments (owner+repo, or a full_name). */
function resolveRepoArgs(args: Record<string, unknown>): { owner: string; repo: string } | null {
  const owner = str(args.owner);
  const repo = str(args.repo) ?? str(args.repository);
  if (owner && repo) return { owner, repo };
  const fullName = str(args.full_name) ?? str(args.repo_full_name) ?? str(args.fullName);
  if (fullName) return splitFullName(fullName);
  return null;
}

function callTool(store: Store, name: string, args: Record<string, unknown>): Record<string, unknown> {
  const normalized = name.toLowerCase().replace(/[^a-z0-9]/g, '');
  const tool = TOOLS.find((candidate) => candidate.name.replace(/[^a-z0-9]/g, '') === normalized)
    ?? TOOLS.find((candidate) => normalized.endsWith(candidate.name.replace(/[^a-z0-9]/g, '')));
  if (!tool) {
    return { __rpcError: { code: -32602, message: `unknown tool: ${name}` } };
  }
  switch (tool.name) {
    case 'create_repo': {
      const repoName = str(args.name) ?? str(args.repo) ?? str(args.repository);
      if (!repoName) return toolResult('create_repo requires a name', true);
      if (store.getRepo(VIEWER.login, repoName)) {
        return toolResult(`Repository already exists: ${VIEWER.login}/${repoName}`, true);
      }
      const repo = store.createRepo(repoName, {
        private: typeof args.private === 'boolean' ? args.private : false,
        description: str(args.description) ?? '',
      });
      return toolResult(JSON.stringify(repoJson(repo)));
    }
    case 'get_repo': {
      const resolved = resolveRepoArgs(args);
      if (!resolved) return toolResult('get_repo requires owner and repo', true);
      const repo = store.getRepo(resolved.owner, resolved.repo);
      if (!repo) return toolResult(`Not Found: ${resolved.owner}/${resolved.repo}`, true);
      return toolResult(JSON.stringify(repoJson(repo)));
    }
    case 'create_issue': {
      const resolved = resolveRepoArgs(args);
      const title = str(args.title);
      if (!resolved || !title) return toolResult('create_issue requires owner, repo and title', true);
      const repo = store.getRepo(resolved.owner, resolved.repo);
      if (!repo) return toolResult(`Not Found: ${resolved.owner}/${resolved.repo}`, true);
      const issue = store.createIssue(repo, title, str(args.body) ?? '', []);
      if ('error' in issue) return toolResult(issue.error, true);
      return toolResult(JSON.stringify(issueJson(repo, issue)));
    }
    case 'list_issues': {
      const resolved = resolveRepoArgs(args);
      if (!resolved) return toolResult('list_issues requires owner and repo', true);
      const repo = store.getRepo(resolved.owner, resolved.repo);
      if (!repo) return toolResult(`Not Found: ${resolved.owner}/${resolved.repo}`, true);
      const state = str(args.state);
      const issues = store.listIssues(repo, state, 'created', 'desc');
      return toolResult(JSON.stringify(issues.map((issue) => issueJson(repo, issue))));
    }
    case 'create_pull_request': {
      const resolved = resolveRepoArgs(args);
      const title = str(args.title);
      const head = str(args.head) ?? str(args.head_ref) ?? str(args.head_branch);
      const base = str(args.base) ?? str(args.base_ref) ?? str(args.base_branch);
      if (!resolved || !title || !head || !base) {
        return toolResult('create_pull_request requires owner, repo, title, head and base', true);
      }
      const repo = store.getRepo(resolved.owner, resolved.repo);
      if (!repo) return toolResult(`Not Found: ${resolved.owner}/${resolved.repo}`, true);
      const pull = store.createPull(repo, title, str(args.body) ?? '', head, base);
      if ('error' in pull) return toolResult(pull.error, true);
      return toolResult(JSON.stringify(pullJson(repo, pull)));
    }
    case 'merge_pull_request': {
      const resolved = resolveRepoArgs(args);
      const number = num(args.pull_number) ?? num(args.pull_request_number) ?? num(args.number);
      if (!resolved || number === undefined) {
        return toolResult('merge_pull_request requires owner, repo and pull_number', true);
      }
      const repo = store.getRepo(resolved.owner, resolved.repo);
      if (!repo) return toolResult(`Not Found: ${resolved.owner}/${resolved.repo}`, true);
      const pull = store.getPull(repo, number);
      if (!pull) return toolResult(`Not Found: pull request #${number}`, true);
      const result = store.mergePull(repo, pull);
      if ('error' in result) return toolResult(result.error, true);
      return toolResult(JSON.stringify(pullJson(repo, pull)));
    }
    default:
      return { __rpcError: { code: -32602, message: `unknown tool: ${name}` } };
  }
}

/**
 * Handle one POST /mcp body. `rawBody` is the request bytes (already read). Returns the HTTP
 * status and the JSON body (null body means an empty 202 for a notification).
 */
export function handleMcp(
  store: Store,
  rawBody: Buffer,
): { status: number; body: unknown } {
  let parsed: unknown;
  try {
    parsed = JSON.parse(rawBody.toString('utf8'));
  } catch {
    return { status: 400, body: { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } } };
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return { status: 400, body: { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } } };
  }
  const request = parsed as Record<string, unknown>;
  const method = typeof request.method === 'string' ? request.method : '';
  const id = (request.id === undefined ? null : request.id) as JsonRpcId;
  const hasId = request.id !== undefined && request.id !== null;
  const params = (request.params ?? {}) as Record<string, unknown>;

  // A notification (no id) gets no JSON-RPC response.
  if (!hasId) {
    return { status: 202, body: null };
  }

  if (method === 'server/discover') {
    const meta = (params._meta ?? {}) as Record<string, unknown>;
    const requested = typeof meta['io.modelcontextprotocol/protocolVersion'] === 'string'
      ? (meta['io.modelcontextprotocol/protocolVersion'] as string)
      : typeof params.protocolVersion === 'string'
        ? (params.protocolVersion as string)
        : '2026-07-28';
    if (!MCP_SUPPORTED_VERSIONS.includes(requested)) {
      return rpcError(id, -32022, `Unsupported protocol version: ${requested}`);
    }
    return rpcResult(id, {
      resultType: 'discover',
      supportedVersions: MCP_SUPPORTED_VERSIONS,
      capabilities: { tools: {} },
      instructions: 'A GitHub-compatible git forge: repositories, issues, pull requests, git smart HTTP, GraphQL and MCP tools.',
      serverInfo: { name: MCP_SERVER_NAME, version: MCP_SERVER_VERSION },
    });
  }

  if (method === 'initialize') {
    const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : '2026-07-28';
    const chosen = MCP_SUPPORTED_VERSIONS.includes(requested) ? requested : MCP_SUPPORTED_VERSIONS[0] as string;
    return rpcResult(id, {
      protocolVersion: chosen,
      capabilities: { tools: {} },
      serverInfo: { name: MCP_SERVER_NAME, version: MCP_SERVER_VERSION },
    });
  }

  if (method === 'tools/list') {
    return rpcResult(id, { tools: TOOLS });
  }

  if (method === 'tools/call') {
    const toolName = typeof params.name === 'string' ? params.name : '';
    const args = (params.arguments ?? {}) as Record<string, unknown>;
    if (toolName === '') return rpcError(id, -32602, 'tools/call requires a tool name');
    const outcome = callTool(store, toolName, args);
    const rpcErr = (outcome as { __rpcError?: { code: number; message: string } }).__rpcError;
    if (rpcErr) return rpcError(id, rpcErr.code, rpcErr.message);
    return rpcResult(id, outcome);
  }

  if (method === 'ping') {
    return rpcResult(id, {});
  }

  return rpcError(id, -32601, `Method not found: ${method}`);
}

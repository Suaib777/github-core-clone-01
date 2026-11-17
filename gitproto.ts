// gitproto.ts — git smart HTTP: info/refs (v0 + v2), upload-pack v2 (ls-refs, fetch) and
// receive-pack v0 (push with report-status). No git binary anywhere; the protocol is built on
// gitcore.ts primitives. The wire format follows what the suite's own git client sends and
// parses (kit program/env/src/git-client.ts).

import {
  ZERO_OID,
  collectReachable,
  commitGenerations,
  decodePktLines,
  encodePktLines,
  hashObject,
  isAncestor,
  parseCommit,
  parseTag,
  parseTree,
  peelToCommit,
  readPack,
  writePack,
  type GitObject,
  type GitObjectType,
} from './gitcore.ts';
import { DEFAULT_BRANCH, VIEWER, type Repo, type Store } from './store.ts';

export const AGENT = 'forge/1.0';

function pktText(text: string): Buffer {
  return encodePktLines([text]);
}

/** Split the pack bytes into sideband (band 1) pkt-lines of at most ~32 KiB of payload. */
export function sidebandPack(pack: Buffer): Buffer {
  const parts: Buffer[] = [];
  const CHUNK = 32768;
  for (let off = 0; off < pack.length; off += CHUNK) {
    const slice = pack.subarray(off, Math.min(off + CHUNK, pack.length));
    const payload = Buffer.concat([Buffer.from([1]), slice]);
    parts.push(encodePktLines([payload]));
  }
  if (pack.length === 0) {
    parts.push(encodePktLines([Buffer.from([1])]));
  }
  return Buffer.concat(parts);
}

/** All refs of a repo as (name, oid) pairs, HEAD first, then branches, then tags, sorted. */
function repoRefs(repo: Repo): { name: string; oid: string }[] {
  const out: { name: string; oid: string }[] = [];
  const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  if (tip) out.push({ name: 'HEAD', oid: tip });
  const branches = [...repo.refs.entries()]
    .filter(([ref]) => ref.startsWith('refs/heads/'))
    .sort(([a], [b]) => (a < b ? -1 : 1));
  for (const [ref, oid] of branches) out.push({ name: ref, oid });
  const tags = [...repo.refs.entries()]
    .filter(([ref]) => ref.startsWith('refs/tags/'))
    .sort(([a], [b]) => (a < b ? -1 : 1));
  for (const [ref, oid] of tags) out.push({ name: ref, oid });
  return out;
}

/** The peeled commit of a tag ref (for ls-refs `peeled:` and v0 `^{}` lines). */
function peeledOid(repo: Repo, oid: string): string | null {
  const object = repo.objects.get(oid);
  if (!object || object.type !== 'tag') return null;
  return peelToCommit(oid, (id) => repo.objects.get(id));
}

// -------------------------------------------------------------------------------------------
// info/refs
// -------------------------------------------------------------------------------------------

function v2Advertisement(service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const lines: (string | 'flush' | 'delim')[] = [
    `# service=${service}\n`,
    'flush',
    'version 2\n',
    `agent=${AGENT}\n`,
    'object-format=sha1\n',
    'ls-refs=unborn\n',
    'fetch=shallow filter\n',
    'server-option\n',
    'object-info\n',
    'flush',
  ];
  return encodePktLines(lines);
}

function v0Advertisement(
  repo: Repo,
  service: 'git-upload-pack' | 'git-receive-pack',
): Buffer {
  const caps =
    service === 'git-upload-pack'
      ? `symref=HEAD:refs/heads/${repo.defaultBranch} agent=${AGENT} side-band-64k thin-pack ofs-delta multi_ack shallow`
      : `report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=${AGENT}`;
  const parts: Buffer[] = [pktText(`# service=${service}\n`), Buffer.from('0000', 'ascii')];
  const refs = repoRefs(repo);
  if (refs.length === 0) {
    parts.push(pktText(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    let first = true;
    for (const ref of refs) {
      const line = first ? `${ref.oid} ${ref.name}\0${caps}\n` : `${ref.oid} ${ref.name}\n`;
      parts.push(pktText(line));
      first = false;
      const peeled = peeledOid(repo, ref.oid);
      if (peeled !== null) parts.push(pktText(`${peeled} ${ref.name}^{}\n`));
    }
  }
  parts.push(Buffer.from('0000', 'ascii'));
  return Buffer.concat(parts);
}

/** GET /{owner}/{repo}.git/info/refs — returns {status, contentType, body}. */
export function infoRefs(
  store: Store,
  owner: string,
  repoName: string,
  service: string,
  gitProtocol: string | undefined,
): { status: number; contentType: string; body: Buffer } {
  const repo = store.getRepo(owner, repoName);
  if (!repo) {
    return { status: 404, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'Not Found' }) + '\n') };
  }
  if (service === 'git-upload-pack') {
    if (gitProtocol === 'version=2') {
      return { status: 200, contentType: 'application/x-git-upload-pack-advertisement', body: v2Advertisement('git-upload-pack') };
    }
    return { status: 200, contentType: 'application/x-git-upload-pack-advertisement', body: v0Advertisement(repo, 'git-upload-pack') };
  }
  if (service === 'git-receive-pack') {
    return { status: 200, contentType: 'application/x-git-receive-pack-advertisement', body: v0Advertisement(repo, 'git-receive-pack') };
  }
  return { status: 404, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'Not Found' }) + '\n') };
}

// -------------------------------------------------------------------------------------------
// upload-pack v2: ls-refs
// -------------------------------------------------------------------------------------------

interface LsRefsArgs {
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
  prefixes: string[];
}

function parseLsRefsArgs(packets: ReturnType<typeof decodePktLines>): LsRefsArgs {
  const args: LsRefsArgs = { peel: false, symrefs: false, unborn: false, prefixes: [] };
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').replace(/\n$/, '');
    if (line === 'peel') args.peel = true;
    else if (line === 'symrefs') args.symrefs = true;
    else if (line === 'unborn') args.unborn = true;
    else if (line.startsWith('ref-prefix ')) args.prefixes.push(line.slice('ref-prefix '.length));
  }
  return args;
}

function lsRefsResponse(repo: Repo, args: LsRefsArgs): Buffer {
  const lines: (string | 'flush')[] = [];
  const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  const matches = (name: string): boolean =>
    args.prefixes.length === 0 || args.prefixes.some((prefix) => name.startsWith(prefix));
  // HEAD is only reported when no prefix filters it out (it is not a ref).
  if (args.prefixes.length === 0) {
    if (tip) {
      const symref = args.symrefs ? ` symref-target:refs/heads/${repo.defaultBranch}` : '';
      lines.push(`${tip} HEAD${symref}\n`);
    } else if (args.unborn) {
      const symref = args.symrefs ? ` symref-target:refs/heads/${repo.defaultBranch}` : '';
      lines.push(`unborn HEAD${symref}\n`);
    }
  }
  for (const [ref, oid] of [...repo.refs.entries()].sort(([a], [b]) => (a < b ? -1 : 1))) {
    if (!matches(ref)) continue;
    let line = `${oid} ${ref}`;
    if (args.peel) {
      const peeled = peeledOid(repo, oid);
      if (peeled !== null) line += ` peeled:${peeled}`;
    }
    lines.push(`${line}\n`);
  }
  lines.push('flush');
  return encodePktLines(lines);
}

// -------------------------------------------------------------------------------------------
// upload-pack v2: fetch
// -------------------------------------------------------------------------------------------

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  deepen: number | null;
  deepenSince: number | null;
  deepenNot: string[];
}

function parseFetchArgs(packets: ReturnType<typeof decodePktLines>): FetchArgs {
  const args: FetchArgs = { wants: [], haves: [], shallow: [], deepen: null, deepenSince: null, deepenNot: [] };
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').replace(/\n$/, '');
    if (line.startsWith('want ')) args.wants.push(line.slice(5).trim());
    else if (line.startsWith('have ')) args.haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) args.shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) {
      const n = Number.parseInt(line.slice(7).trim(), 10);
      if (Number.isFinite(n)) args.deepen = n;
    } else if (line.startsWith('deepen-since ')) {
      const n = Number.parseInt(line.slice(12).trim(), 10);
      if (Number.isFinite(n)) args.deepenSince = n;
    } else if (line.startsWith('deepen-not ')) args.deepenNot.push(line.slice(11).trim());
  }
  return args;
}

/**
 * Compute the fetch: which objects to send, and the shallow/unshallow boundary report.
 * `haveSet` is the objects the client already holds (reachable from its haves, stopping at its
 * shallow boundaries). `sendCommits` are the commits to deliver (their trees/blobs follow).
 */
function computeFetch(
  repo: Repo,
  args: FetchArgs,
): { objects: GitObject[]; shallow: string[]; unshallow: string[]; acks: string[]; nak: boolean } {
  const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
  const shallowBoundaries = new Set(args.shallow);
  // A want that names a tag is really a want for the commit it peels to.
  const wantCommits = args.wants.map((oid) => {
    const object = lookup(oid);
    return object && object.type === 'tag' ? peelToCommit(oid, lookup) : oid;
  });
  // What the client already holds: reachable from haves. A shallow boundary commit is fully
  // present (its tree and blobs) but its parents are not held, so traversal stops there.
  const haveSet = new Set<string>();
  {
    const stack = [...args.haves];
    while (stack.length) {
      const oid = stack.pop() as string;
      if (haveSet.has(oid)) continue;
      const object = lookup(oid);
      if (!object) continue;
      haveSet.add(oid);
      if (object.type === 'commit') {
        const info = parseCommit(object.data);
        stack.push(info.tree);
        if (!shallowBoundaries.has(oid)) {
          for (const parent of info.parents) stack.push(parent);
        }
      } else if (object.type === 'tree') {
        for (const entry of parseTree(object.data)) stack.push(entry.oid);
      } else if (object.type === 'tag') {
        stack.push(parseTag(object.data).object);
      }
    }
  }
  // Which commits to send.
  const sendCommits = new Set<string>();
  const newShallow = new Set<string>();
  const unshallow = new Set<string>();
  if (args.deepen !== null && args.deepen > 0) {
    const generations = commitGenerations(wantCommits, lookup, args.deepen);
    for (const [oid, gen] of generations) {
      sendCommits.add(oid);
      if (gen === args.deepen - 1) {
        const object = lookup(oid);
        if (object && object.type === 'commit' && parseCommit(object.data).parents.length > 0) {
          newShallow.add(oid);
        }
      }
    }
    // An old shallow boundary becomes unshallow when its parents are now being sent.
    for (const boundary of shallowBoundaries) {
      if (!sendCommits.has(boundary)) continue;
      const object = lookup(boundary);
      if (!object || object.type !== 'commit') continue;
      const parents = parseCommit(object.data).parents;
      if (parents.some((parent) => sendCommits.has(parent))) unshallow.add(boundary);
    }
    // A boundary that is itself a want at generation 0 with depth 1 stays shallow (it is the tip).
  } else if (args.deepenSince !== null) {
    // deepen-since: send commits newer than the timestamp, plus the boundary.
    const generations = commitGenerations(wantCommits, lookup);
    for (const [oid] of generations) {
      const object = lookup(oid);
      if (!object || object.type !== 'commit') continue;
      const info = parseCommit(object.data);
      const ts = commitTimestamp(info.author);
      if (ts !== null && ts >= args.deepenSince) sendCommits.add(oid);
      else if (object) newShallow.add(oid);
    }
  } else {
    for (const oid of collectReachable(args.wants, lookup)) {
      const object = lookup(oid);
      if (object && object.type === 'commit') sendCommits.add(oid);
    }
  }
  // The objects to send: everything reachable from the send commits, minus what the client has.
  // A shallow boundary commit is sent in full (tree and blobs) but its parents are cut off.
  // Tag wants are seeded too, so the tag objects themselves travel.
  const toSend = new Set<string>();
  {
    const stack = [...sendCommits, ...args.wants];
    while (stack.length) {
      const oid = stack.pop() as string;
      if (toSend.has(oid) || haveSet.has(oid)) continue;
      const object = lookup(oid);
      if (!object) continue;
      toSend.add(oid);
      if (object.type === 'commit') {
        const info = parseCommit(object.data);
        stack.push(info.tree);
        if (!newShallow.has(oid)) {
          for (const parent of info.parents) stack.push(parent);
        }
      } else if (object.type === 'tree') {
        for (const entry of parseTree(object.data)) stack.push(entry.oid);
      } else if (object.type === 'tag') {
        stack.push(parseTag(object.data).object);
      }
    }
  }
  const objects: GitObject[] = [];
  for (const oid of toSend) {
    const object = lookup(oid);
    if (object) objects.push(object);
  }
  // Acknowledgements.
  const acks: string[] = [];
  let nak = false;
  if (args.haves.length === 0) {
    nak = true;
  } else {
    for (const have of args.haves) if (lookup(have)) acks.push(have);
    if (acks.length === 0) nak = true;
  }
  return { objects, shallow: [...newShallow], unshallow: [...unshallow], acks, nak };
}

function commitTimestamp(identity: string): number | null {
  const match = / (\d+) [+-]\d{4}$/.exec(identity);
  return match ? Number.parseInt(match[1] as string, 10) : null;
}

function fetchV2Response(repo: Repo, args: FetchArgs): Buffer {
  const result = computeFetch(repo, args);
  const parts: Buffer[] = [];
  // acknowledgments
  parts.push(pktText('acknowledgments\n'));
  if (result.nak) parts.push(pktText('NAK\n'));
  for (const ack of result.acks) parts.push(pktText(`ACK ${ack}\n`));
  if (!result.nak) parts.push(pktText('ready\n'));
  parts.push(Buffer.from('0001', 'ascii'));
  // shallow-info
  if (result.shallow.length || result.unshallow.length) {
    parts.push(pktText('shallow-info\n'));
    for (const oid of result.shallow) parts.push(pktText(`shallow ${oid}\n`));
    for (const oid of result.unshallow) parts.push(pktText(`unshallow ${oid}\n`));
    parts.push(Buffer.from('0001', 'ascii'));
  }
  // packfile
  parts.push(pktText('packfile\n'));
  parts.push(sidebandPack(writePack(result.objects)));
  parts.push(Buffer.from('0000', 'ascii'));
  return Buffer.concat(parts);
}

// -------------------------------------------------------------------------------------------
// upload-pack v0 fetch (best effort; the suite's client prefers v2)
// -------------------------------------------------------------------------------------------

function fetchV0Response(repo: Repo, body: Buffer): Buffer {
  const packets = decodePktLines(body);
  const args: FetchArgs = { wants: [], haves: [], shallow: [], deepen: null, deepenSince: null, deepenNot: [] };
  let seenFlush = false;
  for (const packet of packets) {
    if (packet.kind === 'flush') {
      seenFlush = true;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').replace(/\n$/, '');
    if (line.startsWith('want ')) args.wants.push(line.slice(5).split(' ')[0] as string);
    else if (line.startsWith('shallow ')) args.shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) {
      const n = Number.parseInt(line.slice(7).trim(), 10);
      if (Number.isFinite(n)) args.deepen = n;
    } else if (seenFlush && line.startsWith('have ')) args.haves.push(line.slice(5).trim());
  }
  const result = computeFetch(repo, args);
  const parts: Buffer[] = [];
  if (result.nak) parts.push(pktText('NAK\n'));
  for (const ack of result.acks) parts.push(pktText(`ACK ${ack}\n`));
  for (const oid of result.shallow) parts.push(pktText(`shallow ${oid}\n`));
  for (const oid of result.unshallow) parts.push(pktText(`unshallow ${oid}\n`));
  parts.push(sidebandPack(writePack(result.objects)));
  parts.push(Buffer.from('0000', 'ascii'));
  return Buffer.concat(parts);
}

// -------------------------------------------------------------------------------------------
// receive-pack v0: push
// -------------------------------------------------------------------------------------------

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

/** Parse a receive-pack body: command lines, then the pack. Returns null on a malformed body. */
function parsePushBody(body: Buffer): { commands: PushCommand[]; pack: Buffer } | null {
  // Parse pkt-lines one at a time until the flush; everything after it is the pack.
  const commands: PushCommand[] = [];
  let off = 0;
  let flushEnd = -1;
  while (off < body.length) {
    if (off + 4 > body.length) return null;
    const head = body.subarray(off, off + 4).toString('ascii');
    if (head === '0000') {
      flushEnd = off + 4;
      break;
    }
    if (head === '0001') {
      off += 4;
      continue;
    }
    const len = parseInt(head, 16);
    if (!Number.isFinite(len) || len < 4 || off + len > body.length) return null;
    const payload = body.subarray(off + 4, off + len);
    let text = payload.toString('utf8');
    const nul = text.indexOf('\0');
    if (nul >= 0) text = text.slice(0, nul);
    const line = text.replace(/\n$/, '');
    const parts = line.split(' ');
    if (parts.length < 3) return null;
    const [oldOid, newOid, ref] = parts as [string, string, string];
    if (!/^[0-9a-f]{40}$/i.test(oldOid) || !/^[0-9a-f]{40}$/i.test(newOid)) return null;
    commands.push({ oldOid, newOid, ref });
    off += len;
  }
  if (commands.length === 0 || flushEnd < 0) return null;
  const pack = body.subarray(flushEnd);
  return { commands, pack };
}

/** Handle POST git-receive-pack. Returns {status, contentType, body}. */
export function receivePack(
  store: Store,
  owner: string,
  repoName: string,
  body: Buffer,
): { status: number; contentType: string; body: Buffer } {
  const repo = store.getRepo(owner, repoName);
  if (!repo) {
    return { status: 404, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'Not Found' }) + '\n') };
  }
  const parsed = parsePushBody(body);
  if (!parsed) {
    // A malformed body: report an unpack failure rather than crashing.
    const report = encodePktLines([Buffer.concat([Buffer.from([1]), Buffer.from('unpack error\n', 'utf8')])]);
    return { status: 200, contentType: 'application/x-git-receive-pack-result', body: Buffer.concat([report, Buffer.from('0000', 'ascii')]) };
  }
  const { commands, pack } = parsed;
  const lookup = (oid: string): GitObject | undefined => repo.objects.get(oid);
  // Store the pack's objects (a delete-only push carries no pack).
  let unpackOk = true;
  if (pack.length > 0) {
    try {
      const read = readPack(pack, lookup);
      for (const [oid, object] of read.objects) {
        if (!repo.objects.has(oid)) repo.objects.set(oid, object);
      }
    } catch {
      unpackOk = false;
    }
  }
  const reportLines: string[] = [];
  if (!unpackOk) {
    reportLines.push('unpack error\n');
  } else {
    reportLines.push('unpack ok\n');
  }
  if (unpackOk) {
    for (const command of commands) {
      const current = repo.refs.get(command.ref);
      if (command.newOid === ZERO_OID) {
        // Delete.
        if (current === undefined) {
          reportLines.push(`ng ${command.ref} no such ref\n`);
        } else if (command.oldOid !== ZERO_OID && command.oldOid.toLowerCase() !== current.toLowerCase()) {
          reportLines.push(`ng ${command.ref} old value mismatch\n`);
        } else {
          repo.refs.delete(command.ref);
          reportLines.push(`ok ${command.ref}\n`);
        }
        continue;
      }
      const newObject = repo.objects.get(command.newOid);
      if (!newObject) {
        reportLines.push(`ng ${command.ref} object not found\n`);
        continue;
      }
      if (current === undefined) {
        // Create: old must be zero.
        if (command.oldOid !== ZERO_OID) {
          reportLines.push(`ng ${command.ref} old value mismatch\n`);
          continue;
        }
        repo.refs.set(command.ref, command.newOid);
        reportLines.push(`ok ${command.ref}\n`);
        continue;
      }
      // Update: must fast-forward.
      if (command.oldOid.toLowerCase() !== current.toLowerCase()) {
        reportLines.push(`ng ${command.ref} old value mismatch\n`);
        continue;
      }
      const newCommit = peelToCommit(command.newOid, lookup);
      const oldCommit = peelToCommit(current, lookup);
      if (!isAncestor(oldCommit, newCommit, lookup)) {
        reportLines.push(`ng ${command.ref} non-fast-forward\n`);
        continue;
      }
      repo.refs.set(command.ref, command.newOid);
      reportLines.push(`ok ${command.ref}\n`);
    }
  }
  // Report-status inside sideband band 1 (the client advertised side-band-64k).
  const reportText = reportLines.join('');
  const reportPackets = encodePktLines([Buffer.concat([Buffer.from([1]), Buffer.from(reportText, 'utf8')])]);
  const bodyOut = Buffer.concat([reportPackets, Buffer.from('0000', 'ascii')]);
  return { status: 200, contentType: 'application/x-git-receive-pack-result', body: bodyOut };
}

/** Handle POST git-upload-pack (v2 ls-refs/fetch, or v0 fetch). */
export function uploadPack(
  store: Store,
  owner: string,
  repoName: string,
  body: Buffer,
  gitProtocol: string | undefined,
): { status: number; contentType: string; body: Buffer } {
  const repo = store.getRepo(owner, repoName);
  if (!repo) {
    return { status: 404, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'Not Found' }) + '\n') };
  }
  let packets: ReturnType<typeof decodePktLines>;
  try {
    packets = decodePktLines(body);
  } catch {
    return { status: 400, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'malformed pkt-line' }) + '\n') };
  }
  if (gitProtocol === 'version=2') {
    const first = packets.find((packet) => packet.kind === 'data');
    const command = first ? first.payload.toString('utf8').replace(/\n$/, '') : '';
    if (command === 'command=ls-refs') {
      return { status: 200, contentType: 'application/x-git-upload-pack-result', body: lsRefsResponse(repo, parseLsRefsArgs(packets)) };
    }
    if (command === 'command=fetch') {
      return { status: 200, contentType: 'application/x-git-upload-pack-result', body: fetchV2Response(repo, parseFetchArgs(packets)) };
    }
    return { status: 400, contentType: 'application/json; charset=utf-8', body: Buffer.from(JSON.stringify({ message: 'unknown command' }) + '\n') };
  }
  // v0 fetch.
  return { status: 200, contentType: 'application/x-git-upload-pack-result', body: fetchV0Response(repo, body) };
}

export { VIEWER, DEFAULT_BRANCH };

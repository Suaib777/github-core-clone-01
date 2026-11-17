// gitcore.ts — the git object layer: SHA-1, zlib, pkt-lines, packfiles, trees, commits,
// tags, reachability, merge-base and tree diffs. Pure functions over an object lookup; no I/O.
// Written for the github-core-clone-01 forge. Only node:crypto and node:zlib are used.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export const ZERO_OID = '0000000000000000000000000000000000000000';
export type GitObjectType = 'blob' | 'tree' | 'commit' | 'tag';

export interface GitObject {
  readonly type: GitObjectType;
  readonly data: Buffer;
}

export type ObjectLookup = (oid: string) => GitObject | undefined;

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

/** The id of an object: SHA-1 over `<type> <size>\0<data>`. */
export function hashObject(type: GitObjectType, data: Buffer): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'ascii');
  return sha1Hex(Buffer.concat([header, data]));
}

export function deflate(data: Buffer): Buffer {
  return deflateSync(data);
}

export function inflate(data: Buffer): Buffer {
  return inflateSync(data);
}

// -------------------------------------------------------------------------------------------
// pkt-lines
// -------------------------------------------------------------------------------------------

export type PktKind = 'data' | 'flush' | 'delim';
export interface Pkt {
  readonly kind: PktKind;
  /** The payload without the 4-byte length prefix (for data packets, without the trailing newline stripped). */
  readonly payload: Buffer;
}

/** Encode one pkt-line: 4 hex digits of (payload length + 4), then the payload. */
export function pktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const len = body.length + 4;
  if (len > 0xffff) throw new Error(`pkt-line of ${len} bytes exceeds the 65535 limit`);
  const head = Buffer.alloc(4);
  head.write(len.toString(16).padStart(4, '0'), 0, 4, 'ascii');
  return Buffer.concat([head, body]);
}

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');

/** Encode several lines; the strings 'flush' and 'delim' become the control packets. */
export function encodePktLines(lines: readonly (string | Buffer | 'flush' | 'delim')[]): Buffer {
  const parts: Buffer[] = [];
  for (const line of lines) {
    if (line === 'flush') parts.push(PKT_FLUSH);
    else if (line === 'delim') parts.push(PKT_DELIM);
    else parts.push(pktLine(line));
  }
  return Buffer.concat(parts);
}

/** Split a buffer of pkt-lines into packets. Throws on a malformed length prefix. */
export function decodePktLines(buf: Buffer): Pkt[] {
  const out: Pkt[] = [];
  let off = 0;
  while (off < buf.length) {
    if (off + 4 > buf.length) throw new Error('pkt-line truncated at length prefix');
    const head = buf.subarray(off, off + 4).toString('ascii');
    if (head === '0000') {
      out.push({ kind: 'flush', payload: Buffer.alloc(0) });
      off += 4;
      continue;
    }
    if (head === '0001') {
      out.push({ kind: 'delim', payload: Buffer.alloc(0) });
      off += 4;
      continue;
    }
    const len = parseInt(head, 16);
    if (!Number.isFinite(len) || len < 4 || off + len > buf.length) {
      throw new Error(`malformed pkt-line length ${JSON.stringify(head)}`);
    }
    out.push({ kind: 'data', payload: Buffer.from(buf.subarray(off + 4, off + len)) });
    off += len;
  }
  return out;
}

// -------------------------------------------------------------------------------------------
// trees, commits, tags
// -------------------------------------------------------------------------------------------

export interface TreeEntry {
  readonly mode: string;
  readonly name: string;
  readonly oid: string;
}

/** Parse a tree object into its entries. */
export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let off = 0;
  while (off < data.length) {
    const space = data.indexOf(0x20, off);
    if (space < 0) throw new Error('tree entry has no mode');
    const mode = data.subarray(off, space).toString('ascii');
    const nul = data.indexOf(0, space + 1);
    if (nul < 0 || nul + 21 > data.length) throw new Error('tree entry has no name/oid');
    const name = data.subarray(space + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    off = nul + 21;
  }
  return entries;
}

/** Git's tree sort: entries sort by name, directories as if the name ended in '/'. */
export function treeSortKey(entry: TreeEntry): string {
  return entry.mode === '40000' ? `${entry.name}/` : entry.name;
}

/** Build a tree object from entries (sorted git's way). */
export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const ka = treeSortKey(a);
    const kb = treeSortKey(b);
    return ka < kb ? -1 : ka > kb ? 1 : 0;
  });
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface CommitInfo {
  readonly tree: string;
  readonly parents: string[];
  readonly author: string;
  readonly committer: string;
  readonly message: string;
}

/** Parse a commit object. */
export function parseCommit(data: Buffer): CommitInfo {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
  }
  return { tree, parents, author, committer, message };
}

/** Build a commit object. */
export function buildCommit(info: CommitInfo): Buffer {
  const lines: string[] = [`tree ${info.tree}`];
  for (const parent of info.parents) lines.push(`parent ${parent}`);
  lines.push(`author ${info.author}`);
  lines.push(`committer ${info.committer}`);
  return Buffer.from(`${lines.join('\n')}\n\n${info.message}`, 'utf8');
}

export interface TagInfo {
  readonly object: string;
  readonly type: string;
  readonly tag: string;
  readonly tagger: string;
  readonly message: string;
}

/** Parse an annotated tag object. */
export function parseTag(data: Buffer): TagInfo {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  let object = '';
  let type = '';
  let tag = '';
  let tagger = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
    else if (line.startsWith('tag ')) tag = line.slice(4);
    else if (line.startsWith('tagger ')) tagger = line.slice(7);
  }
  return { object, type, tag, tagger, message };
}

/** Build an annotated tag object. */
export function buildTag(info: TagInfo): Buffer {
  return Buffer.from(
    `object ${info.object}\ntype ${info.type}\ntag ${info.tag}\ntagger ${info.tagger}\n\n${info.message}`,
    'utf8',
  );
}

/** The commit an annotated tag peels to (following nested tags), or the oid itself. */
export function peelToCommit(oid: string, lookup: ObjectLookup): string {
  let current = oid;
  for (let depth = 0; depth < 8; depth += 1) {
    const object = lookup(current);
    if (!object || object.type !== 'tag') return current;
    current = parseTag(object.data).object;
  }
  return current;
}

// -------------------------------------------------------------------------------------------
// packfiles (no deltas on write; deltas resolved on read)
// -------------------------------------------------------------------------------------------

const TYPE_CODE: Record<GitObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, GitObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

function packObjectHeader(type: GitObjectType, size: number): Buffer {
  const code = TYPE_CODE[type];
  const bytes: number[] = [];
  let remaining = size;
  let first = (code << 4) | (remaining & 0x0f);
  remaining = Math.floor(remaining / 16);
  while (remaining > 0) {
    bytes.push(first | 0x80);
    first = remaining & 0x7f;
    remaining = Math.floor(remaining / 128);
  }
  bytes.push(first);
  return Buffer.from(bytes);
}

/** Build a packfile from plain objects (no deltas). Trailer is SHA-1 over everything before it. */
export function writePack(objects: Iterable<GitObject>): Buffer {
  const list = [...objects];
  const parts: Buffer[] = [];
  const header = Buffer.allocUnsafe(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(list.length, 8);
  parts.push(header);
  for (const object of list) {
    parts.push(packObjectHeader(object.type, object.data.length));
    parts.push(deflate(object.data));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let off = 0;
  const readVarint = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[off] ?? 0;
      off += 1;
      value |= (byte & 0x7f) << shift;
      shift += 7;
      if ((byte & 0x80) === 0) break;
    }
    return value;
  };
  const baseSize = readVarint();
  if (baseSize !== base.length) throw new Error('delta base size mismatch');
  const resultSize = readVarint();
  const chunks: Buffer[] = [];
  let total = 0;
  while (off < delta.length) {
    const op = delta[off] ?? 0;
    off += 1;
    if (op & 0x80) {
      let offset = 0;
      let size = 0;
      for (let i = 0; i < 4; i += 1) if (op & (1 << i)) { offset |= (delta[off] ?? 0) << (8 * i); off += 1; }
      for (let i = 0; i < 3; i += 1) if (op & (0x10 << i)) { size |= (delta[off] ?? 0) << (8 * i); off += 1; }
      if (size === 0) size = 0x10000;
      chunks.push(base.subarray(offset, offset + size));
      total += size;
    } else if (op !== 0) {
      chunks.push(delta.subarray(off, off + op));
      total += op;
      off += op;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  const out = Buffer.concat(chunks);
  if (out.length !== resultSize || total !== resultSize) throw new Error('delta result size mismatch');
  return out;
}

export interface PackRead {
  readonly objects: Map<string, GitObject>;
  readonly deltaCount: number;
}

/** Inflate one zlib stream at `offset`; returns its bytes and the offset just past the stream. */
export function inflateAt(buffer: Buffer, offset: number): { data: Buffer; next: number } {
  const result = inflateSync(buffer.subarray(offset), { info: true }) as unknown as {
    buffer: Buffer;
    engine: { bytesWritten: number };
  };
  const written = result.engine.bytesWritten;
  if (!written || written <= 0) throw new Error(`zlib stream at offset ${offset} consumed no bytes`);
  return { data: result.buffer, next: offset + written };
}

/** Read a packfile (v2/v3), resolving ofs-delta and ref-delta entries against the pack itself. */
export function readPack(pack: Buffer, resolveBase?: (oid: string) => GitObject | undefined): PackRead {
  if (pack.length < 32) throw new Error('pack too short');
  if (pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('not a packfile');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const trailer = pack.subarray(pack.length - 20).toString('hex');
  const expected = sha1Hex(pack.subarray(0, pack.length - 20));
  if (trailer !== expected) throw new Error('pack trailer checksum mismatch');
  const objects = new Map<string, GitObject>();
  const byOffset = new Map<number, GitObject>();
  interface RawEntry {
    readonly offset: number;
    readonly kind: number;
    readonly baseOffset?: number;
    readonly baseOid?: string;
    readonly data: Buffer;
  }
  const entries: RawEntry[] = [];
  let deltaCount = 0;
  let off = 12;
  const end = pack.length - 20;
  for (let i = 0; i < count; i += 1) {
    if (off >= end) throw new Error('pack truncated');
    const entryOffset = off;
    let byte = pack[off] ?? 0;
    off += 1;
    const kind = (byte >> 4) & 0x7;
    let size = byte & 0x0f;
    let shift = 4;
    while (byte & 0x80) {
      byte = pack[off] ?? 0;
      off += 1;
      size |= (byte & 0x7f) << shift;
      shift += 7;
    }
    let baseOffset: number | undefined;
    let baseOid: string | undefined;
    if (kind === 6) {
      byte = pack[off] ?? 0;
      off += 1;
      let offset = byte & 0x7f;
      while (byte & 0x80) {
        byte = pack[off] ?? 0;
        off += 1;
        offset = ((offset + 1) << 7) | (byte & 0x7f);
      }
      baseOffset = entryOffset - offset;
      deltaCount += 1;
    } else if (kind === 7) {
      baseOid = pack.subarray(off, off + 20).toString('hex');
      off += 20;
      deltaCount += 1;
    } else if (CODE_TYPE[kind] === undefined) {
      throw new Error(`unknown pack object type ${kind}`);
    }
    const inflated = inflateAt(pack.subarray(0, end), off);
    if (inflated.data.length !== size) throw new Error('pack object size mismatch');
    off = inflated.next;
    entries.push({ offset: entryOffset, kind, baseOffset, baseOid, data: inflated.data });
  }
  // Resolve entries in order; a delta's base is always an earlier entry or an external object.
  const resolve = (entry: RawEntry): GitObject => {
    if (entry.kind === 6 || entry.kind === 7) {
      let base: GitObject | undefined;
      if (entry.baseOffset !== undefined) base = byOffset.get(entry.baseOffset);
      else if (entry.baseOid !== undefined) base = objects.get(entry.baseOid) ?? resolveBase?.(entry.baseOid);
      if (!base) throw new Error('delta base not found');
      const data = applyDelta(base.data, entry.data);
      return { type: base.type, data };
    }
    return { type: CODE_TYPE[entry.kind] as GitObjectType, data: entry.data };
  };
  for (const entry of entries) {
    const object = resolve(entry);
    const oid = hashObject(object.type, object.data);
    objects.set(oid, object);
    byOffset.set(entry.offset, object);
  }
  return { objects, deltaCount };
}

// -------------------------------------------------------------------------------------------
// reachability, merge-base, tree diffs
// -------------------------------------------------------------------------------------------

/** All objects reachable from `tips` (commits, their trees/blobs, tags and their targets). */
export function collectReachable(tips: Iterable<string>, lookup: ObjectLookup): Set<string> {
  const seen = new Set<string>();
  const stack = [...tips];
  while (stack.length) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    const object = lookup(oid);
    if (!object) continue;
    seen.add(oid);
    if (object.type === 'commit') {
      const info = parseCommit(object.data);
      stack.push(info.tree);
      for (const parent of info.parents) stack.push(parent);
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) stack.push(entry.oid);
    } else if (object.type === 'tag') {
      stack.push(parseTag(object.data).object);
    }
  }
  return seen;
}

/** True when `ancestor` is reachable from `descendant` by following commit parents. */
export function isAncestor(ancestor: string, descendant: string, lookup: ObjectLookup): boolean {
  if (ancestor === descendant) return true;
  const seen = new Set<string>();
  const stack = [descendant];
  while (stack.length) {
    const oid = stack.pop() as string;
    if (oid === ancestor) return true;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = lookup(oid);
    if (!object || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) stack.push(parent);
  }
  return false;
}

/** The nearest common ancestor commit of `a` and `b` (first found by a joint BFS), or null. */
export function mergeBase(a: string, b: string, lookup: ObjectLookup): string | null {
  if (a === b) return a;
  const aAncestors = new Set<string>();
  const queueA = [a];
  while (queueA.length) {
    const oid = queueA.shift() as string;
    if (aAncestors.has(oid)) continue;
    aAncestors.add(oid);
    const object = lookup(oid);
    if (!object || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) queueA.push(parent);
  }
  const queueB = [b];
  const seenB = new Set<string>();
  while (queueB.length) {
    const oid = queueB.shift() as string;
    if (aAncestors.has(oid)) return oid;
    if (seenB.has(oid)) continue;
    seenB.add(oid);
    const object = lookup(oid);
    if (!object || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) queueB.push(parent);
  }
  return null;
}

/** The flat file map of a tree: path -> {mode, oid}. Directories are expanded. */
export function flattenTree(treeOid: string, lookup: ObjectLookup): Map<string, { mode: string; oid: string }> {
  const out = new Map<string, { mode: string; oid: string }>();
  const walk = (oid: string, prefix: string): void => {
    const object = lookup(oid);
    if (!object || object.type !== 'tree') return;
    for (const entry of parseTree(object.data)) {
      const path = prefix ? `${prefix}/${entry.name}` : entry.name;
      if (entry.mode === '40000') walk(entry.oid, path);
      else out.set(path, { mode: entry.mode, oid: entry.oid });
    }
  };
  walk(treeOid, '');
  return out;
}

/**
 * Build the tree objects for a flat file map. Returns the root tree oid; every tree object is
 * registered through `put`.
 */
export function buildTreeFromMap(
  files: Map<string, { mode: string; oid: string }>,
  put: (type: GitObjectType, data: Buffer) => string,
): string {
  interface Node {
    files: Map<string, { mode: string; oid: string }>;
    dirs: Map<string, Node>;
  }
  const make = (): Node => ({ files: new Map(), dirs: new Map() });
  const root = make();
  for (const [path, entry] of files) {
    const segments = path.split('/');
    let node = root;
    for (let i = 0; i < segments.length - 1; i += 1) {
      const name = segments[i] as string;
      if (!node.dirs.has(name)) node.dirs.set(name, make());
      node = node.dirs.get(name) as Node;
    }
    node.files.set(segments[segments.length - 1] as string, entry);
  }
  const build = (node: Node): string => {
    const entries: TreeEntry[] = [];
    for (const [name, entry] of node.files) entries.push({ mode: entry.mode, name, oid: entry.oid });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
    return put('tree', buildTree(entries));
  };
  return build(root);
}

/** The set of paths that differ between two trees (added, modified or deleted). */
export function diffTrees(
  baseTreeOid: string,
  headTreeOid: string,
  lookup: ObjectLookup,
): Map<string, { oldOid: string | null; newOid: string | null }> {
  const base = baseTreeOid === ZERO_OID ? new Map() : flattenTree(baseTreeOid, lookup);
  const head = headTreeOid === ZERO_OID ? new Map() : flattenTree(headTreeOid, lookup);
  const out = new Map<string, { oldOid: string | null; newOid: string | null }>();
  for (const [path, entry] of head) {
    const old = base.get(path);
    if (!old || old.oid !== entry.oid) out.set(path, { oldOid: old?.oid ?? null, newOid: entry.oid });
  }
  for (const [path, entry] of base) {
    if (!head.has(path)) out.set(path, { oldOid: entry.oid, newOid: null });
  }
  return out;
}

/** Commit generations from `tips` (0 = the tips). Stops expanding past `maxDepth` when given. */
export function commitGenerations(
  tips: Iterable<string>,
  lookup: ObjectLookup,
  maxDepth?: number,
): Map<string, number> {
  const generations = new Map<string, number>();
  const queue: Array<{ oid: string; gen: number }> = [...tips].map((oid) => ({ oid, gen: 0 }));
  while (queue.length) {
    const { oid, gen } = queue.shift() as { oid: string; gen: number };
    const known = generations.get(oid);
    if (known !== undefined && known <= gen) continue;
    generations.set(oid, gen);
    if (maxDepth !== undefined && gen + 1 >= maxDepth) continue;
    const object = lookup(oid);
    if (!object || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) queue.push({ oid: parent, gen: gen + 1 });
  }
  return generations;
}

// store.ts — the forge's in-memory store: users, repositories, git objects, issues, labels,
// pull requests and comments. One store per process; everything the surfaces read and write
// lives here, so REST, GraphQL, MCP and git all see the same state.
//
// Determinism: no wall clock, no Math.random. Every timestamp comes from a single monotonic
// tick seeded by MAKEFUN_TRIAL_NOW (the instance's opening time, identical in live and replay),
// so two runs of the same request sequence produce byte-identical git objects.

import {
  ZERO_OID,
  buildCommit,
  buildTreeFromMap,
  collectReachable,
  diffTrees,
  flattenTree,
  hashObject,
  mergeBase,
  parseCommit,
  parseTree,
  peelToCommit,
  type GitObject,
  type GitObjectType,
} from './gitcore.ts';

export interface User {
  readonly login: string;
  readonly id: number;
  readonly name: string;
  readonly email: string;
}

/** The trial's viewer, from the scenario's fixed seed (wire.ts SCENARIO_SEED). */
export const VIEWER: User = {
  login: 'adaiversen',
  id: 1000,
  name: 'ada iversen',
  email: 'adaiversen@example.invalid',
};

export const DEFAULT_BRANCH = 'main';
export const FORGE_IDENTITY = `${VIEWER.name} <${VIEWER.email}>`;

const TRIAL_NOW_MS = Number.parseInt(process.env.MAKEFUN_TRIAL_NOW ?? '', 10);
const BASE_TIME_SEC = Number.isFinite(TRIAL_NOW_MS) && TRIAL_NOW_MS > 0 ? Math.floor(TRIAL_NOW_MS / 1000) : 1700000000;

/** The deterministic clock: one tick per timestamp, starting at the trial's opening second. */
let tick = 0;
export function nowSec(): number {
  return BASE_TIME_SEC + tick++;
}
export function nowIso(): string {
  return new Date(nowSec() * 1000).toISOString();
}

export interface Label {
  name: string;
  color: string;
}

export interface Comment {
  id: number;
  body: string;
  user: User;
  createdAt: string;
  updatedAt: string;
}

export interface Issue {
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: Comment[];
  user: User;
  createdAt: string;
  updatedAt: string;
  closedAt: string | null;
}

export interface PullFile {
  filename: string;
  status: 'added' | 'removed' | 'modified';
  additions: number;
  deletions: number;
  changes: number;
}

export interface Pull {
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  head: { ref: string; sha: string };
  base: { ref: string; sha: string };
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  user: User;
  createdAt: string;
  updatedAt: string;
}

export interface Repo {
  id: number;
  databaseId: number;
  name: string;
  owner: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  createdAt: string;
  updatedAt: string;
  /** refs/heads/<name> and refs/tags/<name> -> commit/tag oid. */
  refs: Map<string, string>;
  /** oid -> object. */
  objects: Map<string, GitObject>;
  labels: Label[];
  issues: Issue[];
  pulls: Pull[];
  /** Shared issue/PR number sequence (GitHub numbers issues and PRs together). */
  seq: number;
  commentSeq: number;
  /** Flattened tree cache: commit oid -> path -> {mode, oid}. */
  treeCache: Map<string, Map<string, { mode: string; oid: string }>>;
}

export class Store {
  readonly repos = new Map<string, Repo>();
  private repoSeq = 0;

  key(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.key(owner, name));
  }

  createRepo(name: string, options: { private?: boolean; description?: string }): Repo {
    this.repoSeq += 1;
    const id = this.repoSeq;
    const now = nowIso();
    const repo: Repo = {
      id,
      databaseId: id,
      name,
      owner: VIEWER.login,
      description: options.description ?? '',
      private: options.private ?? false,
      defaultBranch: DEFAULT_BRANCH,
      createdAt: now,
      updatedAt: now,
      refs: new Map(),
      objects: new Map(),
      labels: [],
      issues: [],
      pulls: [],
      seq: 0,
      commentSeq: 0,
      treeCache: new Map(),
    };
    this.repos.set(this.key(VIEWER.login, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.key(owner, name));
  }

  /** All repositories owned by the viewer, in creation order. */
  viewerRepos(): Repo[] {
    return [...this.repos.values()].filter((repo) => repo.owner === VIEWER.login);
  }

  // -------------------------------------------------------------------------------------------
  // git objects
  // -------------------------------------------------------------------------------------------

  /** Register an object in a repo (idempotent) and return its oid. */
  putObject(repo: Repo, type: GitObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
    return oid;
  }

  lookup(repo: Repo): (oid: string) => GitObject | undefined {
    return (oid: string) => repo.objects.get(oid);
  }

  refTip(repo: Repo, ref: string): string | undefined {
    return repo.refs.get(ref);
  }

  branchTip(repo: Repo, branch: string): string | undefined {
    return repo.refs.get(`refs/heads/${branch}`);
  }

  /** The tip of the repo's default branch, or undefined when unborn. */
  defaultTip(repo: Repo): string | undefined {
    return repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  }

  /** A commit object for a tree with the given parents and message. */
  makeCommit(repo: Repo, tree: string, parents: readonly string[], message: string): string {
    const stamp = `${nowSec()} +0000`;
    const text = message.endsWith('\n') ? message : `${message}\n`;
    const data = buildCommit({
      tree,
      parents: [...parents],
      author: `${FORGE_IDENTITY} ${stamp}`,
      committer: `${FORGE_IDENTITY} ${stamp}`,
      message: text,
    });
    return this.putObject(repo, 'commit', data);
  }

  /** The flat file map of a commit's tree (cached; objects are immutable). */
  flatTree(repo: Repo, commitOid: string): Map<string, { mode: string; oid: string }> {
    const cached = repo.treeCache.get(commitOid);
    if (cached) return cached;
    const commit = repo.objects.get(commitOid);
    if (!commit || commit.type !== 'commit') return new Map();
    const map = flattenTree(parseCommit(commit.data).tree, this.lookup(repo));
    repo.treeCache.set(commitOid, map);
    return map;
  }

  /** The flat file map of a tree oid (uncached; used for merge results). */
  flatTreeOid(repo: Repo, treeOid: string): Map<string, { mode: string; oid: string }> {
    return flattenTree(treeOid, this.lookup(repo));
  }

  /** Build a new tree from a flat file map, registering every tree object. */
  buildTree(repo: Repo, files: Map<string, { mode: string; oid: string }>): string {
    return buildTreeFromMap(files, (type, data) => this.putObject(repo, type, data));
  }

  // -------------------------------------------------------------------------------------------
  // contents
  // -------------------------------------------------------------------------------------------

  /** Read a file or directory from a commit. Returns null when the path is absent. */
  readPath(
    repo: Repo,
    commitOid: string,
    path: string,
  ): { kind: 'file'; mode: string; oid: string; data: Buffer } | { kind: 'dir'; entries: { name: string; path: string; type: 'file' | 'dir'; oid: string }[] } | null {
    const commit = repo.objects.get(commitOid);
    if (!commit || commit.type !== 'commit') return null;
    const rootTree = parseCommit(commit.data).tree;
    if (path === '') {
      const entries = this.dirEntries(repo, rootTree, '');
      return entries === null ? null : { kind: 'dir', entries };
    }
    const files = this.flatTree(repo, commitOid);
    const file = files.get(path);
    if (file) {
      const object = repo.objects.get(file.oid);
      if (!object) return null;
      return { kind: 'file', mode: file.mode, oid: file.oid, data: object.data };
    }
    const dirTree = this.dirOid(repo, rootTree, path);
    if (dirTree === null) return null;
    const entries = this.dirEntries(repo, dirTree, path);
    return entries === null ? null : { kind: 'dir', entries };
  }

  /** The entries of one tree object, with full paths. */
  dirEntries(
    repo: Repo,
    treeOid: string,
    dirPath: string,
  ): { name: string; path: string; type: 'file' | 'dir'; oid: string }[] | null {
    const object = repo.objects.get(treeOid);
    if (!object || object.type !== 'tree') return null;
    return parseTree(object.data).map((entry) => ({
      name: entry.name,
      path: dirPath === '' ? entry.name : `${dirPath}/${entry.name}`,
      type: entry.mode === '40000' ? ('dir' as const) : ('file' as const),
      oid: entry.oid,
    }));
  }

  /** The tree oid of a directory path within a tree ('' = the root tree), or null when absent. */
  dirOid(repo: Repo, treeOid: string, dirPath: string): string | null {
    if (dirPath === '') return treeOid;
    const segments = dirPath.split('/');
    let current = treeOid;
    for (const segment of segments) {
      const object = repo.objects.get(current);
      if (!object || object.type !== 'tree') return null;
      const entry = parseTree(object.data).find((candidate) => candidate.name === segment);
      if (!entry || entry.mode !== '40000') return null;
      current = entry.oid;
    }
    return current;
  }

  // -------------------------------------------------------------------------------------------
  // contents: create / update / delete a file, each as one commit
  // -------------------------------------------------------------------------------------------

  /** The ref a contents call targets (branch name, or the default branch). */
  contentsRef(repo: Repo, branch?: string): string {
    return `refs/heads/${branch ?? repo.defaultBranch}`;
  }

  /**
   * Create or update a file. Returns the new commit and blob oids, or an error string.
   * `sha` is the blob the caller read (stale when it no longer matches).
   */
  putContent(
    repo: Repo,
    path: string,
    content: Buffer,
    message: string,
    branch: string | undefined,
    sha: string | undefined,
  ): { commitSha: string; contentSha: string } | { error: string; status: number } {
    const ref = this.contentsRef(repo, branch);
    const tip = repo.refs.get(ref);
    const files = tip ? new Map(this.flatTree(repo, tip)) : new Map<string, { mode: string; oid: string }>();
    const existing = tip ? files.get(path) : undefined;
    if (existing !== undefined) {
      if (sha === undefined) return { error: 'sha is required to update a file', status: 409 };
      if (sha.toLowerCase() !== existing.oid.toLowerCase()) {
        return { error: 'sha does not match the current blob', status: 409 };
      }
    }
    const blobOid = this.putObject(repo, 'blob', content);
    files.set(path, { mode: '100644', oid: blobOid });
    const treeOid = this.buildTree(repo, files);
    const parents = tip ? [tip] : [];
    const commitSha = this.makeCommit(repo, treeOid, parents, message);
    repo.refs.set(ref, commitSha);
    repo.updatedAt = nowIso();
    return { commitSha, contentSha: blobOid };
  }

  /** Delete a file. Returns the new commit oid, or an error string. */
  deleteContent(
    repo: Repo,
    path: string,
    message: string,
    branch: string | undefined,
    sha: string | undefined,
  ): { commitSha: string } | { error: string; status: number } {
    const ref = this.contentsRef(repo, branch);
    const tip = repo.refs.get(ref);
    if (!tip) return { error: 'Not Found', status: 404 };
    const files = new Map(this.flatTree(repo, tip));
    const existing = files.get(path);
    if (!existing) return { error: 'Not Found', status: 404 };
    if (sha !== undefined && sha.toLowerCase() !== existing.oid.toLowerCase()) {
      return { error: 'sha does not match the current blob', status: 409 };
    }
    files.delete(path);
    const treeOid = this.buildTree(repo, files);
    const commitSha = this.makeCommit(repo, treeOid, [tip], message);
    repo.refs.set(ref, commitSha);
    repo.updatedAt = nowIso();
    return { commitSha };
  }

  // -------------------------------------------------------------------------------------------
  // commits and branches
  // -------------------------------------------------------------------------------------------

  /** Commits reachable from a tip, newest first (parents after children). */
  listCommits(repo: Repo, tip: string | undefined): { sha: string; message: string; author: string; committer: string }[] {
    if (!tip) return [];
    const out: { sha: string; message: string; author: string; committer: string }[] = [];
    const seen = new Set<string>();
    const queue = [tip];
    while (queue.length) {
      const oid = queue.shift() as string;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const object = repo.objects.get(oid);
      if (!object || object.type !== 'commit') continue;
      const info = parseCommit(object.data);
      // The stored commit object ends the message with a newline (git convention); the
      // REST view returns the message as it was given, without that trailing newline.
      const message = info.message.endsWith('\n') ? info.message.slice(0, -1) : info.message;
      out.push({ sha: oid, message, author: info.author, committer: info.committer });
      for (const parent of info.parents) queue.push(parent);
    }
    return out;
  }

  listBranches(repo: Repo): { name: string; sha: string }[] {
    const out: { name: string; sha: string }[] = [];
    for (const [ref, sha] of repo.refs) {
      if (ref.startsWith('refs/heads/')) out.push({ name: ref.slice('refs/heads/'.length), sha });
    }
    return out.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  }

  createBranch(repo: Repo, ref: string, sha: string): { ok: true } | { error: string; status: number } {
    if (!ref.startsWith('refs/heads/') && !ref.startsWith('refs/tags/')) {
      return { error: 'ref must start with refs/heads/ or refs/tags/', status: 422 };
    }
    if (!repo.objects.has(sha) && sha !== ZERO_OID) return { error: 'object not found', status: 422 };
    if (repo.refs.has(ref)) return { error: 'Reference already exists', status: 422 };
    repo.refs.set(ref, sha);
    return { ok: true };
  }

  // -------------------------------------------------------------------------------------------
  // labels, issues, comments
  // -------------------------------------------------------------------------------------------

  createLabel(repo: Repo, name: string, color: string): Label {
    const existing = repo.labels.find((label) => label.name === name);
    if (existing) return existing;
    const label: Label = { name, color };
    repo.labels.push(label);
    return label;
  }

  createIssue(repo: Repo, title: string, body: string, labels: string[]): Issue | { error: string; status: number } {
    for (const name of labels) {
      if (!repo.labels.some((label) => label.name === name)) {
        return { error: `Label does not exist: ${name}`, status: 422 };
      }
    }
    repo.seq += 1;
    const now = nowIso();
    const issue: Issue = {
      number: repo.seq,
      title,
      body,
      state: 'open',
      labels: [...labels],
      comments: [],
      user: VIEWER,
      createdAt: now,
      updatedAt: now,
      closedAt: null,
    };
    repo.issues.push(issue);
    return issue;
  }

  getIssue(repo: Repo, number: number): Issue | undefined {
    return repo.issues.find((issue) => issue.number === number);
  }

  listIssues(repo: Repo, state: string | undefined, sort: string, direction: string): Issue[] {
    let list = [...repo.issues];
    if (state && state !== 'all') list = list.filter((issue) => issue.state === state);
    const dir = direction === 'asc' ? 1 : -1;
    if (sort === 'created' || sort === 'updated') {
      list.sort((a, b) => (a.number - b.number) * dir);
    } else {
      list.sort((a, b) => (a.number - b.number) * dir);
    }
    return list;
  }

  setIssueState(repo: Repo, number: number, state: 'open' | 'closed'): Issue | undefined {
    const issue = this.getIssue(repo, number);
    if (!issue) return undefined;
    issue.state = state;
    issue.updatedAt = nowIso();
    issue.closedAt = state === 'closed' ? issue.updatedAt : null;
    return issue;
  }

  addComment(repo: Repo, issueNumber: number, body: string): Comment | undefined {
    const issue = this.getIssue(repo, issueNumber);
    if (!issue) return undefined;
    repo.commentSeq += 1;
    const now = nowIso();
    const comment: Comment = { id: repo.commentSeq, body, user: VIEWER, createdAt: now, updatedAt: now };
    issue.comments.push(comment);
    return comment;
  }

  // -------------------------------------------------------------------------------------------
  // pull requests
  // -------------------------------------------------------------------------------------------

  createPull(repo: Repo, title: string, body: string, head: string, base: string): Pull | { error: string; status: number } {
    const headTip = repo.refs.get(`refs/heads/${head}`);
    const baseTip = repo.refs.get(`refs/heads/${base}`);
    if (!headTip) return { error: `head branch not found: ${head}`, status: 422 };
    if (!baseTip) return { error: `base branch not found: ${base}`, status: 422 };
    repo.seq += 1;
    const now = nowIso();
    const pull: Pull = {
      number: repo.seq,
      title,
      body,
      state: 'open',
      head: { ref: head, sha: headTip },
      base: { ref: base, sha: baseTip },
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      user: VIEWER,
      createdAt: now,
      updatedAt: now,
    };
    repo.pulls.push(pull);
    return pull;
  }

  getPull(repo: Repo, number: number): Pull | undefined {
    return repo.pulls.find((pull) => pull.number === number);
  }

  listPulls(repo: Repo, state: string | undefined): Pull[] {
    let list = [...repo.pulls];
    if (!state || state === 'open') list = list.filter((pull) => pull.state === 'open');
    else if (state === 'closed') list = list.filter((pull) => pull.state === 'closed');
    return list.sort((a, b) => b.number - a.number);
  }

  /** The files a pull request changes (merge-base vs head), with line counts. */
  pullFiles(repo: Repo, pull: Pull): PullFile[] {
    const headTip = repo.refs.get(`refs/heads/${pull.head.ref}`) ?? pull.head.sha;
    const baseTip = repo.refs.get(`refs/heads/${pull.base.ref}`) ?? pull.base.sha;
    const base = mergeBase(baseTip, headTip, this.lookup(repo)) ?? baseTip;
    const baseCommit = repo.objects.get(base);
    const headCommit = repo.objects.get(headTip);
    if (!baseCommit || !headCommit) return [];
    const baseTree = parseCommit(baseCommit.data).tree;
    const headTree = parseCommit(headCommit.data).tree;
    const changes = diffTrees(baseTree, headTree, this.lookup(repo));
    const out: PullFile[] = [];
    for (const [path, change] of [...changes.entries()].sort(([a], [b]) => (a < b ? -1 : 1))) {
      const status = change.newOid === null ? 'removed' : change.oldOid === null ? 'added' : 'modified';
      let additions = 0;
      let deletions = 0;
      const oldText = change.oldOid ? repo.objects.get(change.oldOid)?.data.toString('utf8') ?? '' : '';
      const newText = change.newOid ? repo.objects.get(change.newOid)?.data.toString('utf8') ?? '' : '';
      const oldLines = oldText === '' ? [] : oldText.split('\n');
      const newLines = newText === '' ? [] : newText.split('\n');
      const oldSet = new Set(oldLines);
      const newSet = new Set(newLines);
      for (const line of newLines) if (!oldSet.has(line)) additions += 1;
      for (const line of oldLines) if (!newSet.has(line)) deletions += 1;
      if (status === 'added') additions = Math.max(additions, newLines.length);
      if (status === 'removed') deletions = Math.max(deletions, oldLines.length);
      out.push({ filename: path, status, additions, deletions, changes: additions + deletions });
    }
    return out;
  }

  /** Whether a pull request can merge cleanly (a 3-way merge of head onto base). */
  mergeable(repo: Repo, pull: Pull): boolean {
    if (pull.state !== 'open') return false;
    return this.mergePreview(repo, pull).conflict === null;
  }

  /** The merge preview: the merged file map, or the conflicting path. */
  mergePreview(repo: Repo, pull: Pull): { conflict: string | null; files: Map<string, { mode: string; oid: string }> | null } {
    const headTip = repo.refs.get(`refs/heads/${pull.head.ref}`) ?? pull.head.sha;
    const baseTip = repo.refs.get(`refs/heads/${pull.base.ref}`) ?? pull.base.sha;
    const base = mergeBase(baseTip, headTip, this.lookup(repo));
    if (base === null) return { conflict: null, files: new Map(this.flatTree(repo, headTip)) };
    const baseCommit = repo.objects.get(base);
    const baseTipCommit = repo.objects.get(baseTip);
    const headCommit = repo.objects.get(headTip);
    if (!baseCommit || !baseTipCommit || !headCommit) return { conflict: null, files: null };
    const baseTree = parseCommit(baseCommit.data).tree;
    const baseTipTree = parseCommit(baseTipCommit.data).tree;
    const headTree = parseCommit(headCommit.data).tree;
    const lookup = this.lookup(repo);
    const baseChanges = diffTrees(baseTree, baseTipTree, lookup);
    const headChanges = diffTrees(baseTree, headTree, lookup);
    for (const [path, headChange] of headChanges) {
      const baseChange = baseChanges.get(path);
      if (baseChange && baseChange.newOid !== headChange.newOid) {
        return { conflict: path, files: null };
      }
    }
    const files = new Map(this.flatTree(repo, baseTip));
    for (const [path, change] of headChanges) {
      if (change.newOid === null) files.delete(path);
      else files.set(path, { mode: '100644', oid: change.newOid });
    }
    return { conflict: null, files };
  }

  /**
   * Squash-merge a pull request onto its base branch. Returns the merge commit sha, or an error.
   */
  mergePull(repo: Repo, pull: Pull): { sha: string } | { error: string; status: number } {
    if (pull.state !== 'open') return { error: 'pull request is not open', status: 405 };
    const preview = this.mergePreview(repo, pull);
    if (preview.conflict !== null || preview.files === null) {
      return { error: `merge conflict in ${preview.conflict ?? 'unknown path'}`, status: 405 };
    }
    const baseTip = repo.refs.get(`refs/heads/${pull.base.ref}`) ?? pull.base.sha;
    const treeOid = this.buildTree(repo, preview.files);
    const message = `Merge pull request #${pull.number} from ${repo.owner}/${pull.head.ref}`;
    const mergeSha = this.makeCommit(repo, treeOid, [baseTip], message);
    repo.refs.set(`refs/heads/${pull.base.ref}`, mergeSha);
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = nowIso();
    pull.mergeCommitSha = mergeSha;
    pull.updatedAt = pull.mergedAt;
    repo.updatedAt = pull.mergedAt;
    return { sha: mergeSha };
  }
}

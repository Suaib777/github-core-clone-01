// rest.ts — GitHub REST v3: repositories, branches, commits, contents, labels, issues,
// comments and pull requests. Every reply is JSON with a non-empty X-GitHub-Request-Id.
// Auth: a Bearer token is required (401 without one, or when it does not match
// MAKEFUN_SHOP_TOKEN while that is set). A repository belongs to the token's login, so the
// same name under another owner is 404, never 403.

import { VIEWER, nowIso, type Issue, type Pull, type Repo, type Store } from './store.ts';

export interface RestResponse {
  status: number;
  headers?: Record<string, string>;
  body: unknown;
}

export function json(status: number, body: unknown, headers: Record<string, string> = {}): RestResponse {
  return { status, headers, body };
}

export function err(status: number, message: string): RestResponse {
  return { status, body: { message } };
}

export function repoGlobalId(repo: Repo): string {
  return Buffer.from(`Repository:${repo.databaseId}`, 'utf8').toString('base64');
}

export function issueGlobalId(repo: Repo, issue: Issue): string {
  return Buffer.from(`Issue:${repo.databaseId}:${issue.number}`, 'utf8').toString('base64');
}

export function pullGlobalId(repo: Repo, pull: Pull): string {
  return Buffer.from(`PullRequest:${repo.databaseId}:${pull.number}`, 'utf8').toString('base64');
}

function userJson(login: string, id: number): Record<string, unknown> {
  return { login, id, type: 'User', site_admin: false };
}

export function repoJson(repo: Repo): Record<string, unknown> {
  return {
    id: repo.databaseId,
    node_id: repoGlobalId(repo),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: userJson(repo.owner, VIEWER.id),
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
    html_url: `https://github.com/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    clone_url: `https://github.com/${repo.owner}/${repo.name}.git`,
    git_url: `git://github.com/${repo.owner}/${repo.name}.git`,
    ssh_url: `git@github.com:${repo.owner}/${repo.name}.git`,
    has_issues: true,
    has_projects: true,
    has_wiki: true,
    has_pages: false,
    has_downloads: true,
    archived: false,
    disabled: false,
    fork: false,
    open_issues_count: repo.issues.filter((issue) => issue.state === 'open').length,
    license: null,
    topics: [],
    visibility: repo.private ? 'private' : 'public',
  };
}

export function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: repo.databaseId * 1000000 + issue.number,
    node_id: issueGlobalId(repo, issue),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => {
      const label = repo.labels.find((candidate) => candidate.name === name);
      return { name, color: label?.color ?? 'ededed', description: '' };
    }),
    user: userJson(VIEWER.login, VIEWER.id),
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    closed_at: issue.closedAt,
    comments: issue.comments.length,
    html_url: `https://github.com/${repo.owner}/${repo.name}/issues/${issue.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
  };
}

export function pullJson(repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    id: repo.databaseId * 1000000 + pull.number,
    node_id: pullGlobalId(repo, pull),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    head: {
      ref: pull.head.ref,
      sha: pull.head.sha,
      label: `${repo.owner}:${pull.head.ref}`,
      user: userJson(VIEWER.login, VIEWER.id),
      repo: repoJson(repo),
    },
    base: {
      ref: pull.base.ref,
      sha: pull.base.sha,
      label: `${repo.owner}:${pull.base.ref}`,
      user: userJson(VIEWER.login, VIEWER.id),
      repo: repoJson(repo),
    },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    mergeable: null,
    user: userJson(VIEWER.login, VIEWER.id),
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
    html_url: `https://github.com/${repo.owner}/${repo.name}/pull/${pull.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
  };
}

/** Build a Link header for a paginated list, and slice the page. */
export function paginate<T>(
  items: readonly T[],
  query: URLSearchParams,
  basePath: string,
  host: string,
): { page: T[]; link: string | null } {
  const perPageRaw = Number.parseInt(query.get('per_page') ?? '30', 10);
  const perPage = Number.isFinite(perPageRaw) && perPageRaw > 0 ? Math.min(perPageRaw, 100) : 30;
  const pageRaw = Number.parseInt(query.get('page') ?? '1', 10);
  const pageNum = Number.isFinite(pageRaw) && pageRaw > 0 ? pageRaw : 1;
  const totalPages = Math.max(1, Math.ceil(items.length / perPage));
  const start = (pageNum - 1) * perPage;
  const page = items.slice(start, start + perPage);
  const origin = `http://${host}`;
  const urlFor = (page: number): string => {
    const params = new URLSearchParams(query);
    params.set('per_page', String(perPage));
    params.set('page', String(page));
    return `${origin}${basePath}?${params.toString()}`;
  };
  const links: string[] = [];
  if (pageNum < totalPages) links.push(`<${urlFor(pageNum + 1)}>; rel="next"`);
  if (pageNum > 1) links.push(`<${urlFor(pageNum - 1)}>; rel="prev"`);
  links.push(`<${urlFor(1)}>; rel="first"`);
  links.push(`<${urlFor(totalPages)}>; rel="last"`);
  return { page, link: links.length ? links.join(', ') : null };
}

/** Resolve a ref query value (branch name or commit sha) to a commit oid. */
function resolveRef(store: Store, repo: Repo, ref: string | null): string | undefined {
  if (ref === null || ref === '') return store.defaultTip(repo);
  const branch = store.branchTip(repo, ref);
  if (branch) return branch;
  if (/^[0-9a-f]{40}$/i.test(ref) && repo.objects.has(ref)) return ref;
  return undefined;
}

function bodyOf(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function str(value: unknown, fallback = ''): string {
  return typeof value === 'string' ? value : fallback;
}

/**
 * Route one REST call. `segments` are the path parts after /api/v3 (already URL-decoded).
 * Returns a response; app.ts serialises it.
 */
export function routeRest(
  store: Store,
  method: string,
  segments: string[],
  query: URLSearchParams,
  body: unknown,
  host: string,
): RestResponse {
  const input = bodyOf(body);
  // API root.
  if (segments.length === 0) {
    if (method !== 'GET') return err(404, 'Not Found');
    return json(200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
  }
  if (segments.length === 1 && segments[0] === 'meta') {
    if (method !== 'GET') return err(404, 'Not Found');
    return json(200, { installed_version: '1.0.0' });
  }
  // POST /user/repos and GET /user/repos.
  if (segments.length === 2 && segments[0] === 'user' && segments[1] === 'repos') {
    if (method === 'POST') {
      const name = str(input.name);
      if (name === '') return err(422, 'name is required');
      if (store.getRepo(VIEWER.login, name)) return err(422, 'Repository already exists');
      const repo = store.createRepo(name, {
        private: typeof input.private === 'boolean' ? input.private : false,
        description: str(input.description),
      });
      return json(201, repoJson(repo));
    }
    if (method === 'GET') {
      const repos = store.viewerRepos();
      const { page, link } = paginate(repos, query, '/api/v3/user/repos', host);
      const headers: Record<string, string> = {};
      if (link) headers.link = link;
      return json(200, page.map((repo) => repoJson(repo)), headers);
    }
    return err(404, 'Not Found');
  }
  // /repos/{owner}/{name}[/...]
  if (segments.length >= 3 && segments[0] === 'repos') {
    const owner = segments[1] as string;
    const name = segments[2] as string;
    const repo = store.getRepo(owner, name);
    if (!repo) return err(404, 'Not Found');
    const rest = segments.slice(3);
    if (rest.length === 0) {
      if (method === 'GET') return json(200, repoJson(repo));
      if (method === 'PATCH') {
        if (typeof input.description === 'string') repo.description = input.description;
        repo.updatedAt = nowIso();
        return json(200, repoJson(repo));
      }
      if (method === 'DELETE') {
        store.deleteRepo(owner, name);
        return { status: 204, body: '' };
      }
      return err(404, 'Not Found');
    }
    return routeRepoSub(store, repo, method, rest, query, input, host);
  }
  return err(404, 'Not Found');
}

function routeRepoSub(
  store: Store,
  repo: Repo,
  method: string,
  rest: string[],
  query: URLSearchParams,
  input: Record<string, unknown>,
  host: string,
): RestResponse {
  const head = rest[0] as string;
  // branches
  if (head === 'branches' && rest.length === 1) {
    if (method !== 'GET') return err(404, 'Not Found');
    return json(200, store.listBranches(repo).map((branch) => ({ name: branch.name, commit: { sha: branch.sha } })));
  }
  // commits
  if (head === 'commits' && rest.length === 1) {
    if (method !== 'GET') return err(404, 'Not Found');
    const tip = resolveRef(store, repo, query.get('sha'));
    if (!tip) return err(404, 'Not Found');
    const commits = store.listCommits(repo, tip);
    return json(200, commits.map((commit) => ({
      sha: commit.sha,
      commit: { message: commit.message, author: { name: commit.author }, committer: { name: commit.committer } },
      author: null,
      committer: null,
    })));
  }
  // git/refs (create a branch)
  if (head === 'git' && rest[1] === 'refs' && rest.length === 2) {
    if (method !== 'POST') return err(404, 'Not Found');
    const ref = str(input.ref);
    const sha = str(input.sha);
    const result = store.createBranch(repo, ref, sha);
    if ('error' in result) return err(result.status, result.error);
    return json(201, { ref, object: { sha } });
  }
  // labels
  if (head === 'labels' && rest.length === 1) {
    if (method === 'POST') {
      const name = str(input.name);
      if (name === '') return err(422, 'name is required');
      const label = store.createLabel(repo, name, str(input.color, 'ededed'));
      return json(201, { name: label.name, color: label.color });
    }
    if (method === 'GET') {
      return json(200, repo.labels.map((label) => ({ name: label.name, color: label.color })));
    }
    return err(404, 'Not Found');
  }
  // contents
  if (head === 'contents') {
    const path = rest.slice(1).join('/');
    if (method === 'GET') {
      const tip = resolveRef(store, repo, query.get('ref'));
      if (!tip) return err(404, 'Not Found');
      const found = store.readPath(repo, tip, path);
      if (!found) return err(404, 'Not Found');
      if (found.kind === 'file') {
        return json(200, {
          type: 'file',
          encoding: 'base64',
          size: found.data.length,
          name: path.split('/').pop() ?? path,
          path,
          content: found.data.toString('base64'),
          sha: found.oid,
        });
      }
      return json(200, found.entries.map((entry) => ({
        type: entry.type,
        name: entry.name,
        path: entry.path,
        sha: entry.oid,
        size: entry.type === 'dir' ? 0 : undefined,
      })));
    }
    if (method === 'PUT') {
      const contentB64 = str(input.content);
      if (contentB64 === '') return err(422, 'content is required');
      const content = Buffer.from(contentB64.replace(/[\r\n\t ]+/g, ''), 'base64');
      const branchName = typeof input.branch === 'string' ? input.branch : undefined;
      const tipBefore = store.branchTip(repo, branchName ?? repo.defaultBranch);
      const existedBefore = tipBefore !== undefined && store.readPath(repo, tipBefore, path)?.kind === 'file';
      const result = store.putContent(
        repo,
        path,
        content,
        str(input.message, 'Update'),
        branchName,
        typeof input.sha === 'string' ? input.sha : undefined,
      );
      if ('error' in result) return err(result.status, result.error);
      return json(existedBefore ? 200 : 201, {
        content: { name: path.split('/').pop() ?? path, path, sha: result.contentSha, size: content.length, encoding: 'base64', type: 'file' },
        commit: { sha: result.commitSha, message: str(input.message, 'Update') },
      });
    }
    if (method === 'DELETE') {
      const result = store.deleteContent(
        repo,
        path,
        str(input.message, 'Delete'),
        typeof input.branch === 'string' ? input.branch : undefined,
        typeof input.sha === 'string' ? input.sha : undefined,
      );
      if ('error' in result) return err(result.status, result.error);
      return json(200, { commit: { sha: result.commitSha } });
    }
    return err(404, 'Not Found');
  }
  // issues
  if (head === 'issues') {
    if (rest.length === 1) {
      if (method === 'POST') {
        const title = str(input.title);
        if (title === '') return err(422, 'title is required');
        const labels = Array.isArray(input.labels) ? input.labels.filter((l): l is string => typeof l === 'string') : [];
        const issue = store.createIssue(repo, title, str(input.body), labels);
        if ('error' in issue) return err(issue.status, issue.error);
        return json(201, issueJson(repo, issue));
      }
      if (method === 'GET') {
        const state = query.get('state') ?? undefined;
        const sort = query.get('sort') ?? 'created';
        const direction = query.get('direction') ?? 'desc';
        const issues = store.listIssues(repo, state, sort, direction);
        const { page, link } = paginate(issues, query, `/api/v3/repos/${repo.owner}/${repo.name}/issues`, host);
        const headers: Record<string, string> = {};
        if (link) headers.link = link;
        return json(200, page.map((issue) => issueJson(repo, issue)), headers);
      }
      return err(404, 'Not Found');
    }
    const number = Number.parseInt(rest[1] as string, 10);
    if (!Number.isFinite(number)) return err(404, 'Not Found');
    if (rest.length === 2) {
      if (method === 'GET') {
        const issue = store.getIssue(repo, number);
        if (!issue) return err(404, 'Not Found');
        return json(200, issueJson(repo, issue));
      }
      if (method === 'PATCH') {
        const state = input.state;
        if (state !== 'open' && state !== 'closed') return err(422, 'state must be open or closed');
        const issue = store.setIssueState(repo, number, state);
        if (!issue) return err(404, 'Not Found');
        return json(200, issueJson(repo, issue));
      }
      return err(404, 'Not Found');
    }
    if (rest.length === 3 && rest[2] === 'comments') {
      if (method === 'POST') {
        const comment = store.addComment(repo, number, str(input.body));
        if (!comment) return err(404, 'Not Found');
        return json(201, { id: comment.id, body: comment.body, user: { login: VIEWER.login, id: VIEWER.id }, created_at: comment.createdAt, updated_at: comment.updatedAt });
      }
      if (method === 'GET') {
        const issue = store.getIssue(repo, number);
        if (!issue) return err(404, 'Not Found');
        return json(200, issue.comments.map((comment) => ({ id: comment.id, body: comment.body, user: { login: VIEWER.login, id: VIEWER.id }, created_at: comment.createdAt, updated_at: comment.updatedAt })));
      }
      return err(404, 'Not Found');
    }
    return err(404, 'Not Found');
  }
  // pulls
  if (head === 'pulls') {
    if (rest.length === 1) {
      if (method === 'POST') {
        const title = str(input.title);
        const headRef = str(input.head);
        const baseRef = str(input.base);
        if (title === '' || headRef === '' || baseRef === '') return err(422, 'title, head and base are required');
        const pull = store.createPull(repo, title, str(input.body), headRef, baseRef);
        if ('error' in pull) return err(pull.status, pull.error);
        return json(201, pullJson(repo, pull));
      }
      if (method === 'GET') {
        const state = query.get('state') ?? undefined;
        const pulls = store.listPulls(repo, state);
        const { page, link } = paginate(pulls, query, `/api/v3/repos/${repo.owner}/${repo.name}/pulls`, host);
        const headers: Record<string, string> = {};
        if (link) headers.link = link;
        return json(200, page.map((pull) => pullJson(repo, pull)), headers);
      }
      return err(404, 'Not Found');
    }
    const number = Number.parseInt(rest[1] as string, 10);
    if (!Number.isFinite(number)) return err(404, 'Not Found');
    if (rest.length === 2) {
      if (method !== 'GET') return err(404, 'Not Found');
      const pull = store.getPull(repo, number);
      if (!pull) return err(404, 'Not Found');
      return json(200, pullJson(repo, pull));
    }
    if (rest.length === 3 && rest[2] === 'files') {
      if (method !== 'GET') return err(404, 'Not Found');
      const pull = store.getPull(repo, number);
      if (!pull) return err(404, 'Not Found');
      return json(200, store.pullFiles(repo, pull));
    }
    if (rest.length === 3 && rest[2] === 'merge') {
      if (method !== 'PUT') return err(404, 'Not Found');
      const pull = store.getPull(repo, number);
      if (!pull) return err(404, 'Not Found');
      const result = store.mergePull(repo, pull);
      if ('error' in result) return err(result.status, result.error);
      return json(200, { merged: true, sha: result.sha, message: 'Pull request successfully merged' });
    }
    return err(404, 'Not Found');
  }
  return err(404, 'Not Found');
}

// graphql.ts — GitHub GraphQL v4 for the documents gh and github-mcp-server actually send.
// A small parser for the GraphQL subset those documents use (fields, aliases, arguments,
// variables, object/enum/list literals, inline fragments) plus resolvers over the shared store.
// Unauthenticated -> 401. Unknown field -> HTTP 200 with an errors array naming it. Unknown
// repository -> data.repository = null with no errors. Batches (a JSON array body) answer an
// array of per-operation bodies in order.

import { issueGlobalId, pullGlobalId, repoGlobalId } from './rest.ts';
import { VIEWER, type Issue, type Pull, type Repo, type Store } from './store.ts';

// -------------------------------------------------------------------------------------------
// tokenizer + parser (the GraphQL subset the suite's documents use)
// -------------------------------------------------------------------------------------------

type Token =
  | { kind: 'name'; value: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'var'; value: string }
  | { kind: 'punct'; value: string };

function tokenize(input: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < input.length) {
    const ch = input[i] as string;
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < input.length && input[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '"') {
      let j = i + 1;
      let out = '';
      while (j < input.length && input[j] !== '"') {
        if (input[j] === '\\' && j + 1 < input.length) {
          const esc = input[j + 1] as string;
          out += esc === 'n' ? '\n' : esc === 't' ? '\t' : esc === 'r' ? '\r' : esc;
          j += 2;
        } else {
          out += input[j] as string;
          j += 1;
        }
      }
      tokens.push({ kind: 'string', value: out });
      i = j + 1;
      continue;
    }
    if (ch === '$') {
      let j = i + 1;
      while (j < input.length && /[A-Za-z0-9_]/.test(input[j] as string)) j += 1;
      tokens.push({ kind: 'var', value: input.slice(i + 1, j) });
      i = j;
      continue;
    }
    if (/[0-9-]/.test(ch)) {
      let j = i;
      while (j < input.length && /[0-9]/.test(input[j] as string)) j += 1;
      tokens.push({ kind: 'int', value: Number.parseInt(input.slice(i, j), 10) });
      i = j;
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      let j = i;
      while (j < input.length && /[A-Za-z0-9_]/.test(input[j] as string)) j += 1;
      tokens.push({ kind: 'name', value: input.slice(i, j) });
      i = j;
      continue;
    }
    if ('{}()[]:!@=&|'.includes(ch) || ch === '.') {
      if (ch === '.' && input.slice(i, i + 3) === '...') {
        tokens.push({ kind: 'punct', value: '...' });
        i += 3;
        continue;
      }
      tokens.push({ kind: 'punct', value: ch });
      i += 1;
      continue;
    }
    throw new Error(`unexpected character ${JSON.stringify(ch)} in GraphQL document`);
  }
  return tokens;
}

export type GqlValue =
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'var'; name: string }
  | { kind: 'list'; items: GqlValue[] }
  | { kind: 'object'; fields: Record<string, GqlValue> };

export interface GqlField {
  readonly alias: string | null;
  readonly name: string;
  readonly args: Record<string, GqlValue>;
  readonly selections: GqlSelection[] | null;
}
export type GqlSelection =
  | { kind: 'field'; field: GqlField }
  | { kind: 'inline'; typeCondition: string | null; selections: GqlSelection[] }
  | { kind: 'spread'; name: string };
export interface GqlOperation {
  readonly type: 'query' | 'mutation';
  readonly name: string | null;
  readonly selections: GqlSelection[];
}
export interface GqlDocument {
  readonly operations: GqlOperation[];
  readonly fragments: Record<string, GqlSelection[]>;
}

class Parser {
  private pos = 0;
  private readonly tokens: Token[];
  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token | undefined {
    return this.tokens[this.pos];
  }
  private next(): Token {
    const token = this.tokens[this.pos];
    if (!token) throw new Error('unexpected end of GraphQL document');
    this.pos += 1;
    return token;
  }
  private expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) {
      throw new Error(`expected ${JSON.stringify(value)} in GraphQL document, got ${JSON.stringify(token)}`);
    }
  }
  private expectName(value?: string): string {
    const token = this.next();
    if (token.kind !== 'name') throw new Error(`expected a name in GraphQL document, got ${JSON.stringify(token)}`);
    if (value !== undefined && token.value !== value) throw new Error(`expected ${value}, got ${token.value}`);
    return token.value;
  }

  parseDocument(): GqlDocument {
    const operations: GqlOperation[] = [];
    const fragments: Record<string, GqlSelection[]> = {};
    while (this.pos < this.tokens.length) {
      const token = this.peek() as Token;
      if (token.kind === 'punct' && token.value === '{') {
        operations.push({ type: 'query', name: null, selections: this.parseSelections() });
        continue;
      }
      if (token.kind === 'name' && (token.value === 'query' || token.value === 'mutation')) {
        const type = this.next().value as 'query' | 'mutation';
        let name: string | null = null;
        const next = this.peek();
        if (next && next.kind === 'name') name = this.next().value;
        // Skip variable definitions: ($x: Type!, ...).
        if (this.peek()?.kind === 'punct' && (this.peek() as Token).value === '(') this.skipParens();
        operations.push({ type, name, selections: this.parseSelections() });
        continue;
      }
      if (token.kind === 'name' && token.value === 'fragment') {
        this.next();
        const name = this.expectName();
        this.expectName('on');
        this.expectName();
        fragments[name] = this.parseSelections();
        continue;
      }
      throw new Error(`unexpected token ${JSON.stringify(token)} in GraphQL document`);
    }
    return { operations, fragments };
  }

  private skipParens(): void {
    this.expectPunct('(');
    let depth = 1;
    while (depth > 0) {
      const token = this.next();
      if (token.kind === 'punct' && token.value === '(') depth += 1;
      else if (token.kind === 'punct' && token.value === ')') depth -= 1;
    }
  }

  private parseSelections(): GqlSelection[] {
    this.expectPunct('{');
    const selections: GqlSelection[] = [];
    for (;;) {
      const token = this.peek();
      if (!token) throw new Error('unterminated selection set');
      if (token.kind === 'punct' && token.value === '}') {
        this.next();
        return selections;
      }
      selections.push(this.parseSelection());
    }
  }

  private parseSelection(): GqlSelection {
    const token = this.peek() as Token;
    if (token.kind === 'punct' && token.value === '...') {
      this.next();
      const next = this.peek() as Token;
      if (next.kind === 'name' && next.value === 'on') {
        this.next();
        const typeCondition = this.expectName();
        return { kind: 'inline', typeCondition, selections: this.parseSelections() };
      }
      if (next.kind === 'name') {
        return { kind: 'spread', name: this.next().value };
      }
      return { kind: 'inline', typeCondition: null, selections: this.parseSelections() };
    }
    const first = this.expectName();
    let alias: string | null = null;
    let name = first;
    const next = this.peek();
    if (next && next.kind === 'punct' && next.value === ':') {
      this.next();
      alias = first;
      name = this.expectName();
    }
    const args: Record<string, GqlValue> = {};
    if (this.peek()?.kind === 'punct' && (this.peek() as Token).value === '(') {
      this.next();
      for (;;) {
        const t = this.peek() as Token;
        if (t.kind === 'punct' && t.value === ')') {
          this.next();
          break;
        }
        const argName = this.expectName();
        this.expectPunct(':');
        args[argName] = this.parseValue();
      }
    }
    let selections: GqlSelection[] | null = null;
    if (this.peek()?.kind === 'punct' && (this.peek() as Token).value === '{') {
      selections = this.parseSelections();
    }
    return { kind: 'field', field: { alias, name, args, selections } };
  }

  private parseValue(): GqlValue {
    const token = this.next();
    switch (token.kind) {
      case 'string':
        return { kind: 'string', value: token.value };
      case 'int':
        return { kind: 'int', value: token.value };
      case 'var':
        return { kind: 'var', name: token.value };
      case 'name':
        if (token.value === 'true') return { kind: 'boolean', value: true };
        if (token.value === 'false') return { kind: 'boolean', value: false };
        if (token.value === 'null') return { kind: 'null' };
        return { kind: 'enum', value: token.value };
      case 'punct':
        if (token.value === '[') {
          const items: GqlValue[] = [];
          for (;;) {
            const t = this.peek() as Token;
            if (t.kind === 'punct' && t.value === ']') {
              this.next();
              return { kind: 'list', items };
            }
            items.push(this.parseValue());
          }
        }
        if (token.value === '{') {
          const fields: Record<string, GqlValue> = {};
          for (;;) {
            const t = this.peek() as Token;
            if (t.kind === 'punct' && t.value === '}') {
              this.next();
              return { kind: 'object', fields };
            }
            const name = this.expectName();
            this.expectPunct(':');
            fields[name] = this.parseValue();
          }
        }
        throw new Error(`unexpected punctuation ${token.value} as a GraphQL value`);
      default:
        throw new Error('unexpected token as a GraphQL value');
    }
  }
}

function parseDocument(query: string): GqlDocument {
  return new Parser(tokenize(query)).parseDocument();
}

// -------------------------------------------------------------------------------------------
// value resolution
// -------------------------------------------------------------------------------------------

type Variables = Record<string, unknown>;

function resolveValue(value: GqlValue, variables: Variables): unknown {
  switch (value.kind) {
    case 'string':
    case 'int':
    case 'boolean':
      return value.value;
    case 'null':
      return null;
    case 'enum':
      return value.value;
    case 'var':
      return variables[value.name];
    case 'list':
      return value.items.map((item) => resolveValue(item, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const [key, child] of Object.entries(value.fields)) out[key] = resolveValue(child, variables);
      return out;
    }
  }
}

// -------------------------------------------------------------------------------------------
// the schema's field lists (for __type introspection)
// -------------------------------------------------------------------------------------------

const TYPE_FIELDS: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'body', 'state', 'author', 'createdAt', 'updatedAt', 'closedAt', 'labels', 'comments', 'url', 'repository'],
  PullRequest: ['id', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName', 'headRefOid', 'baseRefOid', 'files', 'author', 'createdAt', 'updatedAt', 'url', 'repository'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'createdAt', 'updatedAt', 'url'],
  User: ['id', 'login', 'name', 'email'],
  SearchType: [],
};

const SEARCH_TYPE_VALUES = ['ISSUE', 'PULL_REQUEST', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT', 'CODE', 'MILESTONE'];

// -------------------------------------------------------------------------------------------
// resolved values: every object carries its __typename and a field map
// -------------------------------------------------------------------------------------------

interface GqlObject {
  readonly __typename: string;
  readonly fields: Record<string, unknown>;
}

function obj(typename: string, fields: Record<string, unknown>): GqlObject {
  return { __typename: typename, fields };
}

function userObject(): GqlObject {
  return obj('User', { id: Buffer.from(`User:${VIEWER.id}`, 'utf8').toString('base64'), login: VIEWER.login, name: VIEWER.name, email: VIEWER.email });
}

function repoObject(store: Store, repo: Repo): GqlObject {
  return obj('Repository', {
    id: repoGlobalId(repo),
    databaseId: repo.databaseId,
    name: repo.name,
    nameWithOwner: `${repo.owner}/${repo.name}`,
    owner: userObject(),
    defaultBranchRef: repo.refs.has(`refs/heads/${repo.defaultBranch}`)
      ? obj('Ref', { name: repo.defaultBranch })
      : null,
    viewerPermission: 'ADMIN',
    hasIssuesEnabled: true,
    description: repo.description,
    createdAt: repo.createdAt,
    updatedAt: repo.updatedAt,
    url: `https://github.com/${repo.owner}/${repo.name}`,
    __repo: repo,
  });
}

function issueObject(store: Store, repo: Repo, issue: Issue): GqlObject {
  return obj('Issue', {
    id: issueGlobalId(repo, issue),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state === 'open' ? 'OPEN' : 'CLOSED',
    author: userObject(),
    createdAt: issue.createdAt,
    updatedAt: issue.updatedAt,
    closedAt: issue.closedAt,
    url: `https://github.com/${repo.owner}/${repo.name}/issues/${issue.number}`,
    __repo: repo,
    __issue: issue,
  });
}

function pullObject(store: Store, repo: Repo, pull: Pull): GqlObject {
  const headTip = repo.refs.get(`refs/heads/${pull.head.ref}`) ?? pull.head.sha;
  const baseTip = repo.refs.get(`refs/heads/${pull.base.ref}`) ?? pull.base.sha;
  return obj('PullRequest', {
    id: pullGlobalId(repo, pull),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.merged ? 'MERGED' : pull.state === 'open' ? 'OPEN' : 'CLOSED',
    merged: pull.merged,
    headRefName: pull.head.ref,
    baseRefName: pull.base.ref,
    headRefOid: headTip,
    baseRefOid: baseTip,
    mergeable: store.mergeable(repo, pull) ? 'MERGEABLE' : 'CONFLICTING',
    author: userObject(),
    createdAt: pull.createdAt,
    updatedAt: pull.updatedAt,
    url: `https://github.com/${repo.owner}/${repo.name}/pull/${pull.number}`,
    __repo: repo,
    __pull: pull,
  });
}

/** Decode a global id of the form base64("Type:rest"). */
function decodeGlobalId(id: string): { type: string; rest: string } | null {
  try {
    const text = Buffer.from(id, 'base64').toString('utf8');
    const colon = text.indexOf(':');
    if (colon < 0) return null;
    return { type: text.slice(0, colon), rest: text.slice(colon + 1) };
  } catch {
    return null;
  }
}

function cursorFor(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64');
}

function numberAfterCursor(cursor: string | null | undefined): number | null {
  if (typeof cursor !== 'string' || cursor === '') return null;
  try {
    const text = Buffer.from(cursor, 'base64').toString('utf8');
    if (!text.startsWith('cursor:')) return null;
    const n = Number.parseInt(text.slice('cursor:'.length), 10);
    return Number.isFinite(n) ? n : null;
  } catch {
    return null;
  }
}

// -------------------------------------------------------------------------------------------
// execution
// -------------------------------------------------------------------------------------------

interface ExecContext {
  store: Store;
  variables: Variables;
  errors: { message: string }[];
}

function argValue(field: GqlField, name: string, ctx: ExecContext): unknown {
  const raw = field.args[name];
  return raw === undefined ? undefined : resolveValue(raw, ctx.variables);
}

function executeSelections(
  selections: GqlSelection[],
  parent: GqlObject | null,
  typeName: string,
  ctx: ExecContext,
  doc: GqlDocument,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const selection of selections) {
    if (selection.kind === 'spread') {
      const fragment = doc.fragments[selection.name];
      if (fragment) Object.assign(out, executeSelections(fragment, parent, typeName, ctx, doc));
      continue;
    }
    if (selection.kind === 'inline') {
      if (selection.typeCondition === null || selection.typeCondition === typeName) {
        Object.assign(out, executeSelections(selection.selections, parent, typeName, ctx, doc));
      }
      continue;
    }
    const field = selection.field;
    const key = field.alias ?? field.name;
    const value = resolveField(parent, typeName, field, ctx, doc);
    out[key] = value;
  }
  return out;
}

function resolveField(
  parent: GqlObject | null,
  typeName: string,
  field: GqlField,
  ctx: ExecContext,
  doc: GqlDocument,
): unknown {
  const { store } = ctx;
  // Root fields.
  if (parent === null) {
    if (typeName === 'Mutation') {
      return resolveMutation(parent, field, ctx, doc);
    }
    if (field.name === 'viewer') {
      if (!field.selections) return userObject();
      return executeSelections(field.selections, userObject(), 'User', ctx, doc);
    }
    if (field.name === 'repository') {
      const owner = argValue(field, 'owner', ctx);
      const name = argValue(field, 'name', ctx);
      const repo = typeof owner === 'string' && typeof name === 'string' ? store.getRepo(owner, name) : undefined;
      if (!repo) return null;
      if (!field.selections) return repoObject(store, repo);
      return executeSelections(field.selections, repoObject(store, repo), 'Repository', ctx, doc);
    }
    if (field.name === 'node') {
      const id = argValue(field, 'id', ctx);
      if (typeof id !== 'string') return null;
      const decoded = decodeGlobalId(id);
      if (!decoded) return null;
      let target: GqlObject | null = null;
      if (decoded.type === 'Repository') {
        const databaseId = Number.parseInt(decoded.rest, 10);
        const repo = [...store.repos.values()].find((candidate) => candidate.databaseId === databaseId);
        if (repo) target = repoObject(store, repo);
      } else if (decoded.type === 'Issue') {
        const [db, num] = decoded.rest.split(':');
        const repo = [...store.repos.values()].find((candidate) => candidate.databaseId === Number.parseInt(db ?? '', 10));
        const issue = repo?.issues.find((candidate) => candidate.number === Number.parseInt(num ?? '', 10));
        if (repo && issue) target = issueObject(store, repo, issue);
      } else if (decoded.type === 'PullRequest') {
        const [db, num] = decoded.rest.split(':');
        const repo = [...store.repos.values()].find((candidate) => candidate.databaseId === Number.parseInt(db ?? '', 10));
        const pull = repo?.pulls.find((candidate) => candidate.number === Number.parseInt(num ?? '', 10));
        if (repo && pull) target = pullObject(store, repo, pull);
      }
      if (!target) return null;
      if (!field.selections) return target;
      return executeSelections(field.selections, target, target.__typename, ctx, doc);
    }
    if (field.name === '__type') {
      const name = argValue(field, 'name', ctx);
      if (typeof name !== 'string') return null;
      const typeObj = obj('__Type', {
        name,
        fields: (TYPE_FIELDS[name] ?? []).map((fieldName) => obj('__Field', { name: fieldName })),
        enumValues: name === 'SearchType' ? SEARCH_TYPE_VALUES.map((value) => obj('__EnumValue', { name: value })) : [],
      });
      if (!field.selections) return typeObj;
      return executeSelections(field.selections, typeObj, '__Type', ctx, doc);
    }
    ctx.errors.push({ message: `Cannot query field "${field.name}" on type "Query".` });
    return null;
  }

  // Repository fields.
  if (typeName === 'Repository') {
    const repo = parent.fields.__repo as Repo;
    if (field.name === 'issue') {
      const number = argValue(field, 'number', ctx);
      const issue = typeof number === 'number' ? store.getIssue(repo, number) : undefined;
      if (!issue) return null;
      const target = issueObject(store, repo, issue);
      return field.selections ? executeSelections(field.selections, target, 'Issue', ctx, doc) : target;
    }
    if (field.name === 'issues') {
      const first = argValue(field, 'first', ctx);
      const after = argValue(field, 'after', ctx);
      const states = argValue(field, 'states', ctx);
      const orderBy = argValue(field, 'orderBy', ctx) as { field?: string; direction?: string } | undefined;
      let issues = [...repo.issues];
      if (Array.isArray(states) && states.length) {
        const wanted = new Set(states.map((state) => String(state).toLowerCase()));
        issues = issues.filter((issue) => wanted.has(issue.state));
      }
      const direction = orderBy?.direction === 'ASC' ? 1 : -1;
      issues.sort((a, b) => (a.number - b.number) * direction);
      const afterNumber = numberAfterCursor(typeof after === 'string' ? after : null);
      // The cursor marks the last node of the previous page; the next page continues in
      // the same direction: below the cursor when descending, above it when ascending.
      if (afterNumber !== null) {
        issues = issues.filter((issue) => (direction === 1 ? issue.number > afterNumber : issue.number < afterNumber));
      }
      const limit = typeof first === 'number' ? first : issues.length;
      const page = issues.slice(0, limit);
      const hasNextPage = issues.length > limit;
      const endCursor = page.length ? cursorFor(page[page.length - 1]?.number ?? 0) : null;
      const connection = obj('IssueConnection', {
        nodes: page.map((issue) => issueObject(store, repo, issue)),
        pageInfo: obj('PageInfo', { hasNextPage, endCursor }),
        totalCount: issues.length,
      });
      return field.selections ? executeSelections(field.selections, connection, 'IssueConnection', ctx, doc) : connection;
    }
    if (field.name === 'pullRequests') {
      const first = argValue(field, 'first', ctx);
      const pulls = [...repo.pulls].sort((a, b) => b.number - a.number);
      const limit = typeof first === 'number' ? first : pulls.length;
      const page = pulls.slice(0, limit);
      const connection = obj('PullRequestConnection', {
        nodes: page.map((pull) => pullObject(store, repo, pull)),
        pageInfo: obj('PageInfo', { hasNextPage: pulls.length > limit, endCursor: null }),
        totalCount: pulls.length,
      });
      return field.selections ? executeSelections(field.selections, connection, 'PullRequestConnection', ctx, doc) : connection;
    }
    if (field.name === 'pullRequest') {
      const number = argValue(field, 'number', ctx);
      const pull = typeof number === 'number' ? store.getPull(repo, number) : undefined;
      if (!pull) return null;
      const target = pullObject(store, repo, pull);
      return field.selections ? executeSelections(field.selections, target, 'PullRequest', ctx, doc) : target;
    }
    if (field.name === 'owner') {
      const target = userObject();
      return field.selections ? executeSelections(field.selections, target, 'User', ctx, doc) : target;
    }
    if (field.name === 'defaultBranchRef') {
      const ref = parent.fields.defaultBranchRef as GqlObject | null;
      if (!ref) return null;
      return field.selections ? executeSelections(field.selections, ref, 'Ref', ctx, doc) : ref;
    }
    const value = parent.fields[field.name];
    if (value !== undefined) return value;
    ctx.errors.push({ message: `Cannot query field "${field.name}" on type "Repository".` });
    return null;
  }

  // IssueConnection / PullRequestConnection / files connection.
  if (typeName === 'IssueConnection' || typeName === 'PullRequestConnection' || typeName === 'PullRequestFileConnection') {
    if (field.name === 'nodes') {
      const nodes = (parent.fields.nodes as GqlObject[]) ?? [];
      const childType = typeName === 'PullRequestConnection' ? 'PullRequest' : typeName === 'PullRequestFileConnection' ? 'PullRequestFile' : 'Issue';
      if (!field.selections) return nodes;
      return nodes.map((node) => executeSelections(field.selections as GqlSelection[], node, childType, ctx, doc));
    }
    if (field.name === 'pageInfo') {
      const pageInfo = parent.fields.pageInfo as GqlObject;
      return field.selections ? executeSelections(field.selections, pageInfo, 'PageInfo', ctx, doc) : pageInfo;
    }
    const value = parent.fields[field.name];
    if (value !== undefined) return value;
    ctx.errors.push({ message: `Cannot query field "${field.name}" on type "${typeName}".` });
    return null;
  }

  // PullRequest fields.
  if (typeName === 'PullRequest') {
    if (field.name === 'files') {
      const first = argValue(field, 'first', ctx);
      const pull = parent.fields.__pull as Pull;
      const repo = parent.fields.__repo as Repo;
      const files = store.pullFiles(repo, pull);
      const limit = typeof first === 'number' ? first : files.length;
      const page = files.slice(0, limit);
      const connection = obj('PullRequestFileConnection', {
        nodes: page.map((file) => obj('PullRequestFile', { path: file.filename, additions: file.additions, deletions: file.deletions, changeType: file.status })),
        pageInfo: obj('PageInfo', { hasNextPage: files.length > limit, endCursor: null }),
        totalCount: files.length,
      });
      return field.selections ? executeSelections(field.selections, connection, 'PullRequestFileConnection', ctx, doc) : connection;
    }
    const value = parent.fields[field.name];
    if (value !== undefined) return value;
    ctx.errors.push({ message: `Cannot query field "${field.name}" on type "PullRequest".` });
    return null;
  }

  // __Type introspection.
  if (typeName === '__Type') {
    if (field.name === 'fields') {
      const fields = (parent.fields.fields as GqlObject[]) ?? [];
      if (!field.selections) return fields;
      return fields.map((entry) => executeSelections(field.selections as GqlSelection[], entry, '__Field', ctx, doc));
    }
    if (field.name === 'enumValues') {
      const values = (parent.fields.enumValues as GqlObject[]) ?? [];
      if (!field.selections) return values;
      return values.map((entry) => executeSelections(field.selections as GqlSelection[], entry, '__EnumValue', ctx, doc));
    }
    const value = parent.fields[field.name];
    if (value !== undefined) return value;
    ctx.errors.push({ message: `Cannot query field "${field.name}" on type "__Type".` });
    return null;
  }

  // Plain object fields (User, Ref, PageInfo, __Field, __EnumValue, Issue, PullRequestFile).
  const value = parent.fields[field.name];
  if (value !== undefined) {
    if (value !== null && typeof value === 'object' && '__typename' in (value as Record<string, unknown>) && field.selections) {
      const target = value as GqlObject;
      return executeSelections(field.selections, target, target.__typename, ctx, doc);
    }
    return value;
  }
  ctx.errors.push({ message: `Cannot query field "${field.name}" on type "${typeName}".` });
  return null;
}

function resolveMutation(
  _parent: GqlObject | null,
  field: GqlField,
  ctx: ExecContext,
  doc: GqlDocument,
): unknown {
  const { store } = ctx;
  const input = argValue(field, 'input', ctx) as Record<string, unknown> | undefined;
  if (field.name === 'createIssue') {
    const repositoryId = input?.repositoryId;
    const title = input?.title;
    const decoded = typeof repositoryId === 'string' ? decodeGlobalId(repositoryId) : null;
    const repo = decoded?.type === 'Repository'
      ? [...store.repos.values()].find((candidate) => candidate.databaseId === Number.parseInt(decoded.rest, 10))
      : undefined;
    if (!repo || typeof title !== 'string') {
      ctx.errors.push({ message: 'createIssue: repository not found' });
      return null;
    }
    const issue = store.createIssue(repo, title, typeof input?.body === 'string' ? input.body : '', []);
    if ('error' in issue) {
      ctx.errors.push({ message: issue.error });
      return null;
    }
    const payload = obj('CreateIssuePayload', { issue: issueObject(store, repo, issue) });
    return field.selections ? executeSelections(field.selections, payload, 'CreateIssuePayload', ctx, doc) : payload;
  }
  if (field.name === 'closeIssue') {
    const issueId = input?.issueId;
    const decoded = typeof issueId === 'string' ? decodeGlobalId(issueId) : null;
    let target: { repo: Repo; issue: Issue } | null = null;
    if (decoded?.type === 'Issue') {
      const [db, num] = decoded.rest.split(':');
      const repo = [...store.repos.values()].find((candidate) => candidate.databaseId === Number.parseInt(db ?? '', 10));
      const issue = repo?.issues.find((candidate) => candidate.number === Number.parseInt(num ?? '', 10));
      if (repo && issue) target = { repo, issue };
    }
    if (!target) {
      ctx.errors.push({ message: 'closeIssue: issue not found' });
      return null;
    }
    store.setIssueState(target.repo, target.issue.number, 'closed');
    const payload = obj('CloseIssuePayload', { issue: issueObject(store, target.repo, target.issue) });
    return field.selections ? executeSelections(field.selections, payload, 'CloseIssuePayload', ctx, doc) : payload;
  }
  if (field.name === 'createPullRequest') {
    const repositoryId = input?.repositoryId;
    const decoded = typeof repositoryId === 'string' ? decodeGlobalId(repositoryId) : null;
    const repo = decoded?.type === 'Repository'
      ? [...store.repos.values()].find((candidate) => candidate.databaseId === Number.parseInt(decoded.rest, 10))
      : undefined;
    const headRefName = input?.headRefName;
    const baseRefName = input?.baseRefName;
    const title = input?.title;
    if (!repo || typeof headRefName !== 'string' || typeof baseRefName !== 'string' || typeof title !== 'string') {
      ctx.errors.push({ message: 'createPullRequest: repository or refs not found' });
      return null;
    }
    const pull = store.createPull(repo, title, typeof input?.body === 'string' ? input.body : '', headRefName, baseRefName);
    if ('error' in pull) {
      ctx.errors.push({ message: pull.error });
      return null;
    }
    const payload = obj('CreatePullRequestPayload', { pullRequest: pullObject(store, repo, pull) });
    return field.selections ? executeSelections(field.selections, payload, 'CreatePullRequestPayload', ctx, doc) : payload;
  }
  if (field.name === 'mergePullRequest') {
    const pullRequestId = input?.pullRequestId;
    const decoded = typeof pullRequestId === 'string' ? decodeGlobalId(pullRequestId) : null;
    let target: { repo: Repo; pull: Pull } | null = null;
    if (decoded?.type === 'PullRequest') {
      const [db, num] = decoded.rest.split(':');
      const repo = [...store.repos.values()].find((candidate) => candidate.databaseId === Number.parseInt(db ?? '', 10));
      const pull = repo?.pulls.find((candidate) => candidate.number === Number.parseInt(num ?? '', 10));
      if (repo && pull) target = { repo, pull };
    }
    if (!target) {
      ctx.errors.push({ message: 'mergePullRequest: pull request not found' });
      return null;
    }
    const result = store.mergePull(target.repo, target.pull);
    if ('error' in result) {
      ctx.errors.push({ message: result.error });
      return null;
    }
    const payload = obj('MergePullRequestPayload', { pullRequest: pullObject(store, target.repo, target.pull) });
    return field.selections ? executeSelections(field.selections, payload, 'MergePullRequestPayload', ctx, doc) : payload;
  }
  if (field.name === 'addComment') {
    const subjectId = input?.subjectId;
    const body = input?.body;
    const decoded = typeof subjectId === 'string' ? decodeGlobalId(subjectId) : null;
    let target: { repo: Repo; issue: Issue } | null = null;
    if (decoded?.type === 'Issue') {
      const [db, num] = decoded.rest.split(':');
      const repo = [...store.repos.values()].find((candidate) => candidate.databaseId === Number.parseInt(db ?? '', 10));
      const issue = repo?.issues.find((candidate) => candidate.number === Number.parseInt(num ?? '', 10));
      if (repo && issue) target = { repo, issue };
    }
    if (!target || typeof body !== 'string') {
      ctx.errors.push({ message: 'addComment: subject not found' });
      return null;
    }
    const comment = store.addComment(target.repo, target.issue.number, body);
    if (!comment) {
      ctx.errors.push({ message: 'addComment: subject not found' });
      return null;
    }
    const node = obj('IssueComment', {
      id: Buffer.from(`IssueComment:${target.repo.databaseId}:${target.issue.number}:${comment.id}`, 'utf8').toString('base64'),
      body: comment.body,
    });
    const payload = obj('AddCommentPayload', { commentEdge: obj('IssueCommentEdge', { node }) });
    return field.selections ? executeSelections(field.selections, payload, 'AddCommentPayload', ctx, doc) : payload;
  }
  ctx.errors.push({ message: `Cannot query field "${field.name}" on type "Mutation".` });
  return null;
}

/** Execute one operation body; returns the GraphQL response object. */
export function executeGraphQL(
  store: Store,
  query: string,
  variables: Variables,
): { data?: Record<string, unknown> | null; errors?: { message: string }[] } {
  let doc: GqlDocument;
  try {
    doc = parseDocument(query);
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : 'parse error' }] };
  }
  const operation = doc.operations[0];
  if (!operation) return { data: null, errors: [{ message: 'no operation in document' }] };
  const ctx: ExecContext = { store, variables, errors: [] };
  const rootType = operation.type === 'mutation' ? 'Mutation' : 'Query';
  const data = executeSelections(operation.selections, null, rootType, ctx, doc);
  const response: { data?: Record<string, unknown> | null; errors?: { message: string }[] } = { data };
  if (ctx.errors.length) response.errors = ctx.errors;
  return response;
}

/** Handle POST /api/graphql: one operation or a JSON array batch. */
export function handleGraphQL(store: Store, body: unknown): { status: number; body: unknown } {
  if (Array.isArray(body)) {
    const responses = body.map((entry) => {
      const record = entry !== null && typeof entry === 'object' ? (entry as Record<string, unknown>) : {};
      const query = typeof record.query === 'string' ? record.query : '';
      const variables = (record.variables ?? {}) as Variables;
      return executeGraphQL(store, query, variables);
    });
    return { status: 200, body: responses };
  }
  const record = body !== null && typeof body === 'object' ? (body as Record<string, unknown>) : {};
  const query = typeof record.query === 'string' ? record.query : '';
  const variables = (record.variables ?? {}) as Variables;
  return { status: 200, body: executeGraphQL(store, query, variables) };
}

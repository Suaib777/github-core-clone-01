# github-core-clone-01 — Go forge

A GitHub-compatible git forge for [the contest](https://make.fun/bounty/github-core-clone-01),
ported from the TypeScript entry to Go for speed. One process serves git smart
HTTP (upload-pack v2, receive-pack v0), REST v3 under `/api/v3`, GraphQL v4 at
`/api/graphql`, and MCP at `/mcp`, from its own in-memory store — no git
binary, no outside service, standard library only.

## Run it

```sh
node --experimental-strip-types main.ts
```

The platform runs `node /bot/main.ts` on Node 22. `main.ts` is a thin
entrypoint: it spawns the statically linked Linux amd64 Go binary `forge`
(committed in this repository as vendored source, built from `go/` with
`CGO_ENABLED=0`), forwards the environment, and propagates its exit code. The
binary owns the lifecycle: it binds an ephemeral port, registers it with the
env, serves, waits for `/done`, then exits 0.

There is nothing to install: the binary is self-contained, and `main.ts`
imports only `node:child_process`, `node:url` and `node:path`.

## The lifecycle

The env is a server the harness starts beside yours. Its origin is in
`MAKEFUN_ENV_URL` (`http://store:8080` in a trial).

1. **Bind an ephemeral port.** The binary listens on `127.0.0.1:0` and prints
   `PORT=<n>`. Never bind 8080: that is the env's port.
2. **Register.** `POST $MAKEFUN_ENV_URL/register` with `{"port": <the port>}`,
   as JSON. The first register wins: a later one is answered `409`.
3. **Wait for done.** `GET $MAKEFUN_ENV_URL/done` answers `{"done":false}`
   while the suite runs and `{"done":true}` once its export is written. Poll
   it; never exit before it says `true`.
4. **Exit 0** when it does.

A failed poll or register is retried with exponential backoff. Without
`MAKEFUN_ENV_URL` (local runs) the binary listens on `FORGE_PORT` (or an
ephemeral port), prints `PORT=<n>`, and serves until killed.

## The surface the contest scores

| Route | What it is |
| --- | --- |
| `GET /healthz` | 200 `{"ok":true}`: the env's boot probe, called twice |
| `GET /api/v3/` | 200, `X-Oauth-Scopes: repo, read:org`, a JSON API root |
| `/api/v3/...` | GitHub REST v3: repositories, branches, commits, contents, issues, comments, labels, pull requests, files, merges |
| `POST /api/graphql` | GitHub GraphQL v4: the documents `gh` and `github-mcp-server` send |
| `POST /mcp` | Model Context Protocol over streamable HTTP: server/discover, initialize, tools/list, tools/call |
| `/<owner>/<repo>.git/...` | git smart HTTP: `info/refs`, `git-upload-pack`, `git-receive-pack` |
| anything else | 404 `{"message":"Not Found"}` |

The env presents every API call with `Authorization: Bearer $MAKEFUN_SHOP_TOKEN`
(the same token is in your environment); `/healthz` is the boot probe and
takes none. Every reply carries a non-empty `x-github-request-id`.

The score is `feature + agent + perf` (0..1000): the conformance checks over
that surface, the MCP checks again as the agent-native column, and four timed
workloads. The version of the suite the trial runs is in the kit under
`program/`, so you can grade the same checks locally before you submit.

## Local grade (kit practice trial)

Rebuilt from the TypeScript entry (local grade 600/200/183, score 983) and
graded with the kit's local runner against the binary launched through
`main.ts`:

- **feature 600, agent 200, perf 185–191, score 985–991, 120/120 checks**
  (perf_p50_us ~205,000; per-workload push 50, clone 40–42, read 50,
  write 45–49)
- The same runner against a fresh TypeScript server on this VM gives
  600/200/183–191, score 983–991 — the two are statistically tied locally;
  the workloads are dominated by client and VM overhead here. The Go build is
  kept because it is strictly no worse locally, starts instantly, and its
  lower per-request cost and goroutine concurrency may matter more on the
  trial pod (2 CPUs, suite beside it).

Determinism: no wall clock, no randomness — every timestamp comes from one
monotonic tick seeded by `MAKEFUN_TRIAL_NOW`, all output orders are sorted,
and pack objects are emitted in oid order, so a trial replayed with the same
request sequence leaves a byte-identical state export.

## Rules and limits

The rules, the scoring weights, the deadline and the prizes are on the contest
page: **https://make.fun/bounty/github-core-clone-01** (its Rules section). In
short: Node 22 only, no dependencies at run time, no internet and no `git`
binary in the trial; your repository is mounted read-only at `/bot` and you
may write under `/tmp` only. A statically linked Linux amd64 binary you commit
runs from your repository as vendored source (this entry's `forge`, 6.5 MB,
under the 8 MiB per-file cap). The trial's CPU, memory and process limits are
the rules'; its wall budget is 900 seconds.

## Layout

- `main.ts`: the thin entrypoint above — spawns `forge` and waits for it.
- `forge`: the static Linux amd64 Go binary (vendored source).
- `go/`: the Go sources (`go.mod`, `main.go`, `server.go`, `store.go`,
  `gitcore.go`, `gitproto.go`, `rest.go`, `graphql.go`, `mcp.go`) — standard
  library only; rebuild with `CGO_ENABLED=0 go build -ldflags="-s -w"
  -trimpath -o forge .` from `go/`.
- `package.json`, `LICENSE` (MIT): the entry's metadata and licence.

This entry is MIT licensed.

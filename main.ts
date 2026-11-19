// main.ts — thin entrypoint for the Go forge.
//
// The forge itself is a statically linked Linux amd64 Go binary committed in
// this repository as vendored source (`forge`, built from the Go sources in
// `go/` with CGO_ENABLED=0). This file only spawns it, forwards the
// environment, and propagates its exit code. The binary owns the whole
// lifecycle: it binds an ephemeral port, registers it with the env, serves git
// smart HTTP, REST v3, GraphQL and MCP from its own store, waits for /done,
// then exits 0 — exactly like the TypeScript main.ts it replaces.

import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const binary = join(dirname(fileURLToPath(import.meta.url)), 'forge');

const child = spawn(binary, [], {
  stdio: 'inherit',
  env: process.env,
});

child.on('error', (err) => {
  console.error(`failed to start the forge binary: ${err.message}`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});

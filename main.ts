import { createApp } from './app.ts';

const envUrl = process.env.MAKEFUN_ENV_URL;
if (!envUrl) {
  console.error('MAKEFUN_ENV_URL is not set');
  process.exit(1);
}

const base = envUrl.replace(/\/+$/, '');

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function register(port: number): Promise<void> {
  let delay = 500;
  for (;;) {
    let res: Response;
    try {
      res = await fetch(`${base}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
      });
    } catch {
      await sleep(delay);
      delay = Math.min(delay * 2, 8000);
      continue;
    }
    if (res.status === 200 || res.status === 409) return;
    if (res.status === 400 || res.status === 405) {
      console.error(`register failed: HTTP ${res.status}`);
      process.exit(1);
    }
    await sleep(delay);
    delay = Math.min(delay * 2, 8000);
  }
}

async function waitDone(): Promise<void> {
  let delay = 500;
  for (;;) {
    let res: Response;
    try {
      res = await fetch(`${base}/done`);
    } catch {
      await sleep(delay);
      delay = Math.min(delay * 2, 8000);
      continue;
    }
    if (res.ok) {
      const body = (await res.json()) as { done?: boolean };
      if (body.done === true) return;
      delay = 500;
      await sleep(1000);
      continue;
    }
    await sleep(delay);
    delay = Math.min(delay * 2, 8000);
  }
}

const server = createApp();

server.listen(0, () => {
  const address = server.address();
  const port =
    typeof address === 'object' && address !== null ? address.port : 0;
  (async () => {
    await register(port);
    await waitDone();
    server.closeAllConnections();
    server.close(() => process.exit(0));
  })().catch((err) => {
    console.error(err);
    process.exit(1);
  });
});

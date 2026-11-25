/**
 * Deterministic E2E stack launcher (ADR-0005 §8).
 *
 * Invoked by Playwright's `webServer` (`npm run e2e:stack`):
 *   1. Starts compose PostgreSQL 17 (`docker compose up --wait postgres`) unless
 *      an explicit `DATABASE_URL` is provided.
 *   2. Compiles `pulse-worker` and `pulse-api`.
 *   3. Seeds every per-project fixture user (`seed-dev -user-id -connection`)
 *      with `PULSE_DEV_NOW` so mock metrics and cursors are deterministic.
 *   4. Starts `pulse-worker` in poll mode (`PULSE_SYNC_POLL_INTERVAL=1s`),
 *      `pulse-api` (`ENV=development`, `PULSE_ENABLE_DEV_LOGIN=true`,
 *      `PULSE_DEV_NOW`) on a free loopback port, and Vite on `:5173` proxying
 *      `/api` to `pulse-api`.
 */
import { type ChildProcess, execFileSync, spawn } from 'node:child_process';
import { mkdirSync } from 'node:fs';
import { createServer } from 'node:net';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { allFixtureUsers, E2E_DEV_NOW } from './fixtureUsers.ts';

const DEFAULT_DATABASE_URL =
  'postgres://pulse:pulse_dev_password@localhost:5432/pulse_pipeline?sslmode=disable';

const webRoot = fileURLToPath(new URL('..', import.meta.url));
const repoRoot = path.resolve(webRoot, '..');
const backendRoot = path.join(repoRoot, 'backend');
const binDir = path.join(repoRoot, 'bin', 'e2e');

function log(message: string): void {
  process.stderr.write(`[e2e-stack] ${message}\n`);
}

function ensurePostgres(): string {
  const explicit = process.env.DATABASE_URL?.trim();
  if (explicit) {
    log('using explicit DATABASE_URL from environment');
    return explicit;
  }
  log('ensuring compose PostgreSQL is healthy...');
  execFileSync('docker', ['compose', 'up', '--wait', 'postgres'], {
    cwd: repoRoot,
    stdio: 'inherit',
  });
  return DEFAULT_DATABASE_URL;
}

function buildBackendBinaries(): { readonly workerBin: string; readonly apiBin: string } {
  mkdirSync(binDir, { recursive: true });
  const ext = process.platform === 'win32' ? '.exe' : '';
  const workerBin = path.join(binDir, `pulse-worker${ext}`);
  const apiBin = path.join(binDir, `pulse-api${ext}`);

  log('building pulse-worker and pulse-api...');
  execFileSync('go', ['build', '-o', workerBin, './cmd/worker'], {
    cwd: backendRoot,
    stdio: 'inherit',
  });
  execFileSync('go', ['build', '-o', apiBin, './cmd/api'], {
    cwd: backendRoot,
    stdio: 'inherit',
  });
  return { workerBin, apiBin };
}

function sanitizedBackendEnv(databaseUrl: string, overrides: Readonly<Record<string, string>>): NodeJS.ProcessEnv {
  return {
    ...process.env,
    DATABASE_URL: databaseUrl,
    ENV: 'development',
    PULSE_DEV_NOW: E2E_DEV_NOW,
    GOOGLE_OAUTH_CLIENT_ID: '',
    GOOGLE_OAUTH_CLIENT_SECRET: '',
    GOOGLE_WEB_OAUTH_CLIENT_ID: '',
    GOOGLE_WEB_OAUTH_CLIENT_SECRET: '',
    PULSE_ALLOWED_EMAILS: '',
    TOKEN_ENCRYPTION_KEY: '',
    TOKEN_ENCRYPTION_PREVIOUS_KEYS: '',
    ...overrides,
  };
}

function seedArgs(user: ReturnType<typeof allFixtureUsers>[number]): string[] {
  return [
    'seed-dev',
    '-user-id',
    user.id,
    '-email',
    user.email,
    '-tz',
    user.timezone,
    '-connection',
    user.connection,
  ];
}

function runSeedOne(
  workerBin: string,
  args: readonly string[],
  env: NodeJS.ProcessEnv,
): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const child = spawn(workerBin, args, { cwd: repoRoot, env, stdio: 'inherit' });
    child.once('error', reject);
    child.once('exit', (code, signal) => {
      if (code === 0) {
        resolve();
        return;
      }
      reject(
        new Error(
          `seed-dev ${args.join(' ')} exited with code=${String(code)} signal=${String(signal)}`,
        ),
      );
    });
  });
}

async function seedFixtures(workerBin: string, databaseUrl: string): Promise<void> {
  const fixtures = allFixtureUsers();
  const [first, ...rest] = fixtures;
  if (!first) {
    return;
  }
  log(`seeding ${String(fixtures.length)} fixture users at PULSE_DEV_NOW=${E2E_DEV_NOW}...`);
  const env = sanitizedBackendEnv(databaseUrl, { PULSE_ENABLE_MOCK_PROVIDER: 'true' });

  // Run the first fixture alone so pending migrations apply once without lock contention.
  execFileSync(workerBin, seedArgs(first), { cwd: repoRoot, env, stdio: 'inherit' });

  // Each remaining fixture owns a distinct user_id and email, so bounded concurrency is contention-free.
  const concurrency = 4;
  let nextIndex = 0;
  const runWorker = async (): Promise<void> => {
    for (;;) {
      const user = rest[nextIndex];
      nextIndex += 1;
      if (!user) {
        return;
      }
      await runSeedOne(workerBin, seedArgs(user), env);
    }
  };
  await Promise.all(Array.from({ length: Math.min(concurrency, rest.length) }, () => runWorker()));
}

async function findFreePort(): Promise<number> {
  return new Promise<number>((resolve, reject) => {
    const server = createServer();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      if (address === null || typeof address === 'string') {
        server.close(() => {
          reject(new Error('failed to resolve ephemeral TCP port'));
        });
        return;
      }
      const { port } = address;
      server.close((err) => {
        if (err) {
          reject(err);
          return;
        }
        resolve(port);
      });
    });
  });
}

async function waitForHttpOk(url: string, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const response = await fetch(url);
      if (response.ok) {
        return;
      }
    } catch {
      // Server is still binding its socket.
    }
    await new Promise<void>((resolve) => {
      setTimeout(resolve, 100);
    });
  }
  throw new Error(`Timed out after ${String(timeoutMs)}ms waiting for ${url}`);
}

async function terminateChildren(children: readonly ChildProcess[]): Promise<void> {
  const live = children.filter((child) => child.exitCode === null && child.signalCode === null);
  if (live.length === 0) {
    return;
  }
  for (const child of live) {
    child.kill('SIGTERM');
  }
  await Promise.race([
    Promise.all(
      live.map(
        (child) =>
          new Promise<void>((resolve) => {
            child.once('exit', () => {
              resolve();
            });
          }),
      ),
    ),
    new Promise<void>((resolve) => {
      setTimeout(() => {
        for (const child of live) {
          if (child.exitCode === null && child.signalCode === null) {
            child.kill('SIGKILL');
          }
        }
        resolve();
      }, 10_000);
    }),
  ]);
}

async function main(): Promise<void> {
  const databaseUrl = ensurePostgres();
  const { workerBin, apiBin } = buildBackendBinaries();
  await seedFixtures(workerBin, databaseUrl);

  const apiPort = await findFreePort();
  const apiOrigin = `http://localhost:${String(apiPort)}`;
  const children: ChildProcess[] = [];
  let shuttingDown = false;

  const shutdown = async (exitCode: number): Promise<void> => {
    if (shuttingDown) {
      return;
    }
    shuttingDown = true;
    await terminateChildren(children);
    process.exit(exitCode);
  };

  for (const sig of ['SIGINT', 'SIGTERM'] as const) {
    process.on(sig, () => {
      void shutdown(0);
    });
  }

  const spawnManaged = (name: string, command: string, args: readonly string[], cwd: string, env: NodeJS.ProcessEnv): ChildProcess => {
    const child = spawn(command, args, { cwd, env, stdio: 'inherit' });
    children.push(child);
    child.once('error', (err) => {
      log(`${name} failed to start: ${err.message}`);
      void shutdown(1);
    });
    child.once('exit', (code, signal) => {
      if (!shuttingDown) {
        log(`${name} exited unexpectedly (code=${String(code)}, signal=${String(signal)})`);
        void shutdown(1);
      }
    });
    return child;
  };

  log('starting polling pulse-worker (PULSE_SYNC_POLL_INTERVAL=1s)...');
  spawnManaged(
    'pulse-worker',
    workerBin,
    ['run'],
    repoRoot,
    sanitizedBackendEnv(databaseUrl, {
      PULSE_ENABLE_MOCK_PROVIDER: 'true',
      PULSE_SYNC_POLL_INTERVAL: '1s',
    }),
  );

  log(`starting pulse-api on ${apiOrigin}...`);
  spawnManaged(
    'pulse-api',
    apiBin,
    [],
    repoRoot,
    sanitizedBackendEnv(databaseUrl, {
      API_PORT: String(apiPort),
      PULSE_ENABLE_DEV_LOGIN: 'true',
      PULSE_PUBLIC_URL: 'http://localhost:5173',
    }),
  );

  await waitForHttpOk(`http://127.0.0.1:${String(apiPort)}/api/v1/health`, 15_000);

  log('starting Vite dev server on http://localhost:5173...');
  const viteBin = path.join(webRoot, 'node_modules', 'vite', 'bin', 'vite.js');
  spawnManaged(
    'vite',
    process.execPath,
    [viteBin],
    webRoot,
    {
      ...process.env,
      PULSE_API_ORIGIN: apiOrigin,
    },
  );
}

void main().catch((err: unknown) => {
  const message = err instanceof Error ? (err.stack ?? err.message) : String(err);
  log(`fatal: ${message}`);
  process.exit(1);
});

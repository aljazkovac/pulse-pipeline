/** Playwright's isolated PostgreSQL, worker, API and Vite stack (ADR-0005 §8). */
import { type ChildProcess, execFileSync, spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { createServer } from 'node:net';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { allFixtureUsers, E2E_DEV_NOW, E2E_RETRIES } from './fixtureUsers.ts';

const postgresName = `pulse-e2e-${randomUUID()}`;
const database = 'pulse_e2e';

const webRoot = fileURLToPath(new URL('..', import.meta.url));
const repoRoot = path.resolve(webRoot, '..');
const backendRoot = path.join(repoRoot, 'backend');
const binDir = path.join(repoRoot, 'bin', 'e2e');

function log(message: string): void {
  process.stderr.write(`[e2e-stack] ${message}\n`);
}

async function startPostgres(): Promise<string> {
  // Never use the developer's database or inherit DATABASE_URL.
  log('starting disposable PostgreSQL 17...');
  execFileSync('docker', [
    'run', '--detach', '--rm', '--name', postgresName,
    '--tmpfs', '/var/lib/postgresql/data',
    '--publish', '127.0.0.1::5432',
    '--env', `POSTGRES_DB=${database}`,
    '--env', 'POSTGRES_PASSWORD=e2e',
    'postgres:17',
  ], { stdio: 'inherit' });
  const binding = execFileSync('docker', ['port', postgresName, '5432/tcp'], {
    encoding: 'utf8',
    timeout: 5_000,
  }).trim();
  if (!/^127\.0\.0\.1:[0-9]+$/.test(binding)) {
    throw new Error('PostgreSQL did not publish a loopback port');
  }
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    try {
      // The image's temporary initialization server does not listen on TCP.
      execFileSync('docker', [
        'exec', postgresName, 'pg_isready', '-h', '127.0.0.1', '-U', 'postgres', '-d', database,
      ], { stdio: 'ignore', timeout: 5_000 });
      return `postgres://postgres:e2e@${binding}/${database}?sslmode=disable`;
    } catch {
      await new Promise<void>((resolve) => { setTimeout(resolve, 250); });
    }
  }
  throw new Error('Disposable PostgreSQL did not become ready');
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

function seedFixtures(workerBin: string, databaseUrl: string): void {
  const fixtures = allFixtureUsers(process.env.CI ? E2E_RETRIES : 0);
  log(`seeding ${String(fixtures.length)} fixture users at PULSE_DEV_NOW=${E2E_DEV_NOW}...`);
  const env = sanitizedBackendEnv(databaseUrl, { PULSE_ENABLE_MOCK_PROVIDER: 'true' });
  const dir = mkdtempSync(path.join(tmpdir(), 'pulse-e2e-'));
  try {
    const manifest = path.join(dir, 'users.json');
    writeFileSync(manifest, JSON.stringify(fixtures));
    execFileSync(workerBin, ['seed-dev', '-fixtures', manifest], {
      cwd: repoRoot,
      env,
      stdio: 'inherit',
    });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
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
      const response = await fetch(url, { signal: AbortSignal.timeout(1_000) });
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
  const children: ChildProcess[] = [];
  const stop = new AbortController();

  const shutdown = async (exitCode: number): Promise<void> => {
    if (stop.signal.aborted) {
      return;
    }
    stop.abort();
    await terminateChildren(children);
    try {
      execFileSync('docker', ['rm', '--force', postgresName], { stdio: 'ignore', timeout: 5_000 });
    } catch {
      // It may already be gone (--rm), or creation may have failed.
    }
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
      if (!stop.signal.aborted) {
        log(`${name} exited unexpectedly (code=${String(code)}, signal=${String(signal)})`);
        void shutdown(1);
      }
    });
    return child;
  };

  try {
    const { workerBin, apiBin } = buildBackendBinaries();
    const databaseUrl = await startPostgres();
    stop.signal.throwIfAborted();
    seedFixtures(workerBin, databaseUrl);

    const apiPort = await findFreePort();
    stop.signal.throwIfAborted();
    const apiOrigin = `http://127.0.0.1:${String(apiPort)}`;
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

    await waitForHttpOk(`${apiOrigin}/api/v1/health`, 15_000);
    stop.signal.throwIfAborted();

    log('starting Vite dev server on http://localhost:5173...');
    const viteBin = path.join(webRoot, 'node_modules', 'vite', 'bin', 'vite.js');
    spawnManaged(
      'vite',
      process.execPath,
      [viteBin, '--host', 'localhost'],
      webRoot,
      {
        ...process.env,
        PULSE_API_ORIGIN: apiOrigin,
      },
    );
  } catch (err) {
    if (stop.signal.aborted) {
      return;
    }
    log(`fatal: ${err instanceof Error ? err.message : String(err)}`);
    await shutdown(1);
  }
}

void main();

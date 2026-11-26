import { allFixtureUsers, E2E_RETRIES, fixtureUser } from './fixtureUsers.ts';
import { E2E_DEV_NOW, expect, type FixtureState, test } from './fixtures.ts';

interface MeResponse {
  readonly email: string;
  readonly timezone: string;
}

interface ProblemResponse {
  readonly type: string;
  readonly status: number;
}

const ALL_STATES: readonly FixtureState[] = [
  'synced',
  'empty',
  'revoked',
  'none',
  'syncNow',
  'timezonePrompt',
  'settings',
];

test.describe('deterministic E2E stack (ADR-0005 §8)', () => {
  test('fixture matrix isolates every state and viewport project', () => {
    const all = allFixtureUsers();
    expect(allFixtureUsers(0)).toHaveLength(ALL_STATES.length * 2);

    const ids = new Set(all.map((u) => u.id));
    const emails = new Set(all.map((u) => u.email.toLowerCase()));
    expect(ids.size).toBe(all.length);
    expect(emails.size).toBe(all.length);
  });

  test('retries use distinct users for scenarios that mutate server state', ({}, testInfo) => {
    const project = testInfo.project.name === 'mobile' ? 'mobile' : 'desktop';
    const seededIds = new Set(allFixtureUsers().map((user) => user.id));
    for (const state of ['syncNow', 'timezonePrompt', 'settings'] as const) {
      const ids = Array.from({ length: E2E_RETRIES + 1 }, (_, retry) =>
        fixtureUser(project, state, retry).id,
      );
      expect(new Set(ids).size).toBe(ids.length);
      expect(ids.every((id) => seededIds.has(id))).toBe(true);
    }
    expect(fixtureUser(project, 'synced', 1)).toBe(fixtureUser(project, 'synced', 0));
  });

  test('matches target viewport dimensions and pins the browser clock to E2E_DEV_NOW', async ({
    page,
    projectViewport,
  }) => {
    await page.goto('/');

    const expectedSize =
      projectViewport === 'desktop' ? { width: 1280, height: 800 } : { width: 390, height: 844 };
    expect(page.viewportSize()).toEqual(expectedSize);

    const browserIsoNow = await page.evaluate(() => new Date().toISOString());
    expect(browserIsoNow).toBe(new Date(E2E_DEV_NOW).toISOString());
  });

  test('proxies /api/v1/health with Cache-Control: no-store and rejects unauthenticated /me', async ({
    page,
  }) => {
    await page.goto('/');

    const health = await page.request.get('/api/v1/health');
    expect(health.status()).toBe(200);
    expect(health.headers()['cache-control']).toBe('no-store');
    const healthBody = (await health.json()) as { status: string };
    expect(healthBody).toEqual({ status: 'ok' });

    const unauth = await page.evaluate(async () => {
      const res = await fetch('/api/v1/me');
      const body = (await res.json()) as ProblemResponse;
      return { status: res.status, cacheControl: res.headers.get('cache-control'), body };
    });
    expect(unauth.status).toBe(401);
    expect(unauth.cacheControl).toBe('no-store');
    expect(unauth.body.type).toContain('#unauthenticated');
  });

  test('signs in each project-isolated fixture user via HttpOnly cookie and signs out cleanly', async ({
    page,
    projectViewport,
    signInAs,
  }) => {
    await page.goto('/');

    for (const state of ALL_STATES) {
      const expected = await signInAs(state);
      expect(expected.project).toBe(projectViewport);

      const result = await page.evaluate(async () => {
        const res = await fetch('/api/v1/me');
        const body = (await res.json()) as MeResponse;
        return {
          status: res.status,
          body,
          documentCookie: document.cookie,
        };
      });

      expect(result.status).toBe(200);
      expect(result.body).toEqual({
        email: expected.email,
        timezone: expected.timezone,
      });
      // Session cookie must remain HttpOnly and invisible to scripts.
      expect(result.documentCookie).toBe('');
    }

    const logoutStatus = await page.evaluate(async () => {
      const res = await fetch('/api/v1/auth/logout', { method: 'POST' });
      return res.status;
    });
    expect(logoutStatus).toBe(204);

    const afterLogout = await page.request.get('/api/v1/me');
    expect(afterLogout.status()).toBe(401);
  });

  test('rejects unknown fixture email on POST /api/v1/dev/login with 404 problem', async ({
    page,
  }) => {
    const res = await page.request.post('/api/v1/dev/login', {
      data: { email: 'nonexistent-fixture@example.com' },
    });
    expect(res.status()).toBe(404);
    expect(res.headers()['cache-control']).toBe('no-store');
    const body = (await res.json()) as ProblemResponse;
    expect(body.type).toContain('#not-found');
  });
});

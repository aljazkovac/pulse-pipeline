/**
 * Deterministic E2E clock and per-project fixture users (ADR-0005 §8,
 * docs/design/phase-2-dashboards.md).
 *
 * Kept free of `@playwright/test` imports so both Node's stack launcher
 * (`e2e/stack.ts`) and Playwright specs (`e2e/fixtures.ts`) share a single
 * source of truth.
 */

/**
 * Fixed RFC 3339 UTC instant passed as `PULSE_DEV_NOW` to `pulse-api` and
 * `pulse-worker` and installed on every Playwright `page` clock.
 *
 * 14:30 UTC is 15:30 in `Europe/Stockholm`: overnight main sleep has finished
 * and the local day is partially recorded, while preceding days are complete.
 */
export const E2E_DEV_NOW = '2026-11-15T14:30:00Z';
/** Maximum seeded retry index, including retries enabled via the local CLI. */
export const E2E_RETRIES = 2;

export type ViewportProject = 'desktop' | 'mobile';

export type SeedConnectionState = 'synced' | 'empty' | 'revoked' | 'none';

/**
 * Acceptance states from `docs/design/phase-2-dashboards.md`. Scenarios that
 * mutate state (`syncNow`, `timezonePrompt`, `settings`) have dedicated users
 * per viewport project so parallel runs and retries do not share mutable state.
 * Use each mutating state for a single test per viewport.
 */
export type FixtureState =
  | 'synced'
  | 'empty'
  | 'revoked'
  | 'none'
  | 'syncNow'
  | 'timezonePrompt'
  | 'settings';

export interface FixtureUser {
  readonly id: string;
  readonly email: string;
  readonly timezone: string;
  readonly connection: SeedConnectionState;
  readonly project: ViewportProject;
  readonly state: FixtureState;
}

export const FIXTURE_USERS: Readonly<Record<ViewportProject, Readonly<Record<FixtureState, FixtureUser>>>> = {
  desktop: {
    synced: {
      id: '00000000-0000-4000-8000-000000000101',
      email: 'e2e-desktop-synced@example.com',
      timezone: 'Europe/Stockholm',
      connection: 'synced',
      project: 'desktop',
      state: 'synced',
    },
    empty: {
      id: '00000000-0000-4000-8000-000000000102',
      email: 'e2e-desktop-empty@example.com',
      timezone: 'Europe/Stockholm',
      connection: 'empty',
      project: 'desktop',
      state: 'empty',
    },
    revoked: {
      id: '00000000-0000-4000-8000-000000000103',
      email: 'e2e-desktop-revoked@example.com',
      timezone: 'Europe/Stockholm',
      connection: 'revoked',
      project: 'desktop',
      state: 'revoked',
    },
    none: {
      id: '00000000-0000-4000-8000-000000000104',
      email: 'e2e-desktop-none@example.com',
      timezone: 'UTC',
      connection: 'none',
      project: 'desktop',
      state: 'none',
    },
    syncNow: {
      id: '00000000-0000-4000-8000-000000000105',
      email: 'e2e-desktop-sync-now@example.com',
      timezone: 'Europe/Stockholm',
      connection: 'synced',
      project: 'desktop',
      state: 'syncNow',
    },
    timezonePrompt: {
      id: '00000000-0000-4000-8000-000000000106',
      email: 'e2e-desktop-tz-prompt@example.com',
      timezone: 'UTC',
      connection: 'synced',
      project: 'desktop',
      state: 'timezonePrompt',
    },
    settings: {
      id: '00000000-0000-4000-8000-000000000107',
      email: 'e2e-desktop-settings@example.com',
      timezone: 'Europe/Stockholm',
      connection: 'synced',
      project: 'desktop',
      state: 'settings',
    },
  },
  mobile: {
    synced: {
      id: '00000000-0000-4000-8000-000000000201',
      email: 'e2e-mobile-synced@example.com',
      timezone: 'Europe/Stockholm',
      connection: 'synced',
      project: 'mobile',
      state: 'synced',
    },
    empty: {
      id: '00000000-0000-4000-8000-000000000202',
      email: 'e2e-mobile-empty@example.com',
      timezone: 'Europe/Stockholm',
      connection: 'empty',
      project: 'mobile',
      state: 'empty',
    },
    revoked: {
      id: '00000000-0000-4000-8000-000000000203',
      email: 'e2e-mobile-revoked@example.com',
      timezone: 'Europe/Stockholm',
      connection: 'revoked',
      project: 'mobile',
      state: 'revoked',
    },
    none: {
      id: '00000000-0000-4000-8000-000000000204',
      email: 'e2e-mobile-none@example.com',
      timezone: 'UTC',
      connection: 'none',
      project: 'mobile',
      state: 'none',
    },
    syncNow: {
      id: '00000000-0000-4000-8000-000000000205',
      email: 'e2e-mobile-sync-now@example.com',
      timezone: 'Europe/Stockholm',
      connection: 'synced',
      project: 'mobile',
      state: 'syncNow',
    },
    timezonePrompt: {
      id: '00000000-0000-4000-8000-000000000206',
      email: 'e2e-mobile-tz-prompt@example.com',
      timezone: 'UTC',
      connection: 'synced',
      project: 'mobile',
      state: 'timezonePrompt',
    },
    settings: {
      id: '00000000-0000-4000-8000-000000000207',
      email: 'e2e-mobile-settings@example.com',
      timezone: 'Europe/Stockholm',
      connection: 'synced',
      project: 'mobile',
      state: 'settings',
    },
  },
};

export function isViewportProject(name: string): name is ViewportProject {
  return name === 'desktop' || name === 'mobile';
}

const MUTATING_STATES = new Set<FixtureState>(['syncNow', 'timezonePrompt', 'settings']);

export function fixtureUser(project: ViewportProject, state: FixtureState, retry: number): FixtureUser {
  const user = FIXTURE_USERS[project][state];
  if (retry === 0 || !MUTATING_STATES.has(state)) {
    return user;
  }
  if (!Number.isInteger(retry) || retry < 0 || retry > E2E_RETRIES) {
    throw new Error(`No seeded fixture for retry ${String(retry)}`);
  }
  return {
    ...user,
    // The first digit in the last UUID group is reserved for the retry.
    id: `${user.id.slice(0, -12)}${String(retry)}${user.id.slice(-11)}`,
    email: user.email.replace('@', `-retry-${String(retry)}@`),
  };
}

export function allFixtureUsers(): readonly FixtureUser[] {
  const originals = [...Object.values(FIXTURE_USERS.desktop), ...Object.values(FIXTURE_USERS.mobile)];
  return [
    ...originals,
    ...originals.filter((user) => MUTATING_STATES.has(user.state)).flatMap((user) =>
      Array.from({ length: E2E_RETRIES }, (_, i) => fixtureUser(user.project, user.state, i + 1)),
    ),
  ];
}

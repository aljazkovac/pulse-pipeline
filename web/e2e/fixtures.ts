import { test as base, expect } from '@playwright/test';
import {
  E2E_DEV_NOW,
  FIXTURE_USERS,
  type FixtureState,
  type FixtureUser,
  isViewportProject,
  type ViewportProject,
} from './fixtureUsers.ts';

export interface PulseFixtures {
  readonly projectViewport: ViewportProject;
  readonly users: Readonly<Record<FixtureState, FixtureUser>>;
  readonly signInAs: (state: FixtureState) => Promise<FixtureUser>;
}

/**
 * Extended Playwright `test` for Pulse Pipeline E2E scenarios (ADR-0005 §8):
 * - Pins the browser clock (`Date.now()`, `new Date()`) to `E2E_DEV_NOW`
 *   without freezing `setTimeout` / UI polling.
 * - Resolves the active viewport project's isolated fixture user set.
 * - Provides `signInAs(state)` backed by `POST /api/v1/dev/login`.
 */
export const test = base.extend<PulseFixtures>({
  page: async ({ page }, use) => {
    await page.clock.setFixedTime(E2E_DEV_NOW);
    await use(page);
  },

  projectViewport: async ({}, use, testInfo) => {
    const name = testInfo.project.name;
    if (!isViewportProject(name)) {
      throw new Error(`Unknown Playwright project "${name}"; expected "desktop" or "mobile"`);
    }
    await use(name);
  },

  users: async ({ projectViewport }, use) => {
    await use(FIXTURE_USERS[projectViewport]);
  },

  signInAs: async ({ page, users }, use) => {
    await use(async (state: FixtureState): Promise<FixtureUser> => {
      const user = users[state];
      const response = await page.request.post('/api/v1/dev/login', {
        data: { email: user.email },
      });
      expect(response.status(), `dev login failed for ${user.email}`).toBe(204);
      return user;
    });
  },
});

export { E2E_DEV_NOW, expect, FIXTURE_USERS };
export type { FixtureState, FixtureUser, ViewportProject };

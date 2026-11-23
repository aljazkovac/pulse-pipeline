import { expect, test } from '@playwright/test';

// Runs in both the desktop (1280x800) and mobile (390x844) projects.
test.describe('app shell', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/');
  });

  test('shows the page heading inside the main landmark', async ({ page }) => {
    await expect(page).toHaveTitle('Pulse Pipeline');
    await expect(
      page.getByRole('main').getByRole('heading', { level: 1, name: 'Pulse Pipeline' }),
    ).toBeVisible();
  });

  test('skip link is the first keyboard stop and moves focus to main', async ({ page }) => {
    await page.keyboard.press('Tab');
    const skip = page.getByRole('link', { name: 'Skip to main content' });
    await expect(skip).toBeFocused();
    await expect(skip).toBeVisible();

    await page.keyboard.press('Enter');
    await expect(page.getByRole('main')).toBeFocused();
  });

  test('has no horizontal page scroll', async ({ page }) => {
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow).toBe(0);
  });
});

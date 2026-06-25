const { test, expect } = require('@playwright/test');
const { login, ADMIN_EMAIL, ADMIN_PW } = require('./helpers');

test('rejects invalid credentials', async ({ page }) => {
  await page.goto('/');
  await page.fill('#li-email', ADMIN_EMAIL);
  await page.fill('#li-pass', 'wrong-password');
  await page.click('#li-btn');
  await expect(page.locator('#login-err')).toBeVisible();
  await expect(page.locator('#app')).toBeHidden();
});

test('admin can sign in and sees the console', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await expect(page.locator('#hdr-email')).toHaveText(ADMIN_EMAIL);
  await expect(page.locator('#menu')).toContainText('IP Search');
  await expect(page.locator('#menu')).toContainText('ISPs'); // director-only item visible
});

test('session persists across reload; logout returns to login', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await page.reload();
  await expect(page.locator('#app')).toBeVisible();
  await page.click('#logout');
  await expect(page.locator('#login')).toBeVisible();
});

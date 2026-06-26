const { test, expect } = require('@playwright/test');
const { login, nav, ADMIN_EMAIL, ADMIN_PW } = require('./helpers');

test('logs search resolves a public endpoint to flow records', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await nav(page, 'Logs');
  await expect(page.locator('#s-pub')).toBeVisible();
  await page.fill('#s-pub', '203.0.113.42');
  await page.fill('#s-reason', 'E2E-CASE-1');
  await page.click('#s-run');
  await expect(page.locator('#s-results table tbody tr')).not.toHaveCount(0);
  await expect(page.locator('#s-results')).toContainText('100.64.'); // private (NAT) side resolved
});

test('logs search by private IP works (NAT mapping filter)', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await nav(page, 'Logs');
  await page.fill('#s-priv', '100.64.2.37');
  await page.click('#s-run');
  await expect(page.locator('#s-results table')).toBeVisible();
});

test('logs search offers CSV / Excel / PDF exports', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await nav(page, 'Logs');
  await expect(page.locator('#s-pub')).toBeVisible();
  await page.fill('#s-pub', '203.0.113.42');
  await page.click('#s-run');
  const csv = page.locator('#s-results a:has-text("CSV")');
  await expect(csv).toHaveAttribute('href', /\/api\/v1\/report\?format=csv&.*ip=203\.0\.113\.42/);
  await expect(page.locator('#s-results a:has-text("Excel")')).toHaveAttribute('href', /format=xlsx/);
  await expect(page.locator('#s-results a:has-text("PDF")')).toHaveAttribute('href', /format=pdf/);
  const [ download ] = await Promise.all([ page.waitForEvent('download'), csv.click() ]);
  expect(download.suggestedFilename()).toMatch(/\.csv$/);
});

test('a search is recorded in the access audit', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await nav(page, 'Logs');
  await page.fill('#s-pub', '203.0.113.41');
  await page.fill('#s-reason', 'E2E-AUDIT');
  await page.click('#s-run');
  await expect(page.locator('#s-results table')).toBeVisible();
  await nav(page, 'Audit');
  await expect(page.locator('table tbody')).toContainText('203.0.113.41');
  await expect(page.locator('table tbody')).toContainText('E2E-AUDIT');
});

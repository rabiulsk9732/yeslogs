const { test, expect } = require('@playwright/test');
const { login, nav, ADMIN_EMAIL, ADMIN_PW } = require('./helpers');

test('lawful IP search resolves a public endpoint to subscriber rows', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await nav(page, 'IP Search');
  await expect(page.locator('#s-ip')).toBeVisible();
  await page.fill('#s-ip', '203.0.113.42');
  await page.fill('#s-case', 'E2E-CASE-1');
  await page.click('#s-run');
  // result table renders with at least one record + the private (CGNAT) column
  await expect(page.locator('#s-results table tbody tr')).not.toHaveCount(0);
  await expect(page.locator('#s-results')).toContainText('100.64.'); // resolved private CGNAT side
});

test('search offers CSV / Excel / PDF report downloads with correct endpoints', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await nav(page, 'IP Search');
  await page.fill('#s-ip', '203.0.113.42');
  await page.click('#s-run');
  const csv = page.locator('#s-results a:has-text("CSV")');
  await expect(csv).toHaveAttribute('href', /\/api\/v1\/report\?format=csv&.*ip=203\.0\.113\.42/);
  await expect(page.locator('#s-results a:has-text("Excel")')).toHaveAttribute('href', /format=xlsx/);
  await expect(page.locator('#s-results a:has-text("PDF")')).toHaveAttribute('href', /format=pdf/);

  // actually download the CSV and confirm it streams a file
  const [ download ] = await Promise.all([
    page.waitForEvent('download'),
    csv.click(),
  ]);
  expect(download.suggestedFilename()).toMatch(/\.csv$/);
});

test('search records an audit entry', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await nav(page, 'IP Search');
  await page.fill('#s-ip', '203.0.113.41');
  await page.fill('#s-case', 'E2E-AUDIT');
  await page.click('#s-run');
  await expect(page.locator('#s-results table')).toBeVisible();
  await nav(page, 'Audit');
  await expect(page.locator('table tbody')).toContainText('203.0.113.41');
  await expect(page.locator('table tbody')).toContainText('E2E-AUDIT');
});

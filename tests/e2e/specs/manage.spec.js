const { test, expect } = require('@playwright/test');
const { login, nav, ADMIN_EMAIL, ADMIN_PW } = require('./helpers');

test('director can add and delete a device (exporter)', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await nav(page, 'Devices');
  await expect(page.locator('#d-add')).toBeVisible();

  const ip = '10.77.' + Math.floor(Math.random() * 250 + 1) + '.' + Math.floor(Math.random() * 250 + 1);
  await page.selectOption('#d-isp', { index: 1 });
  await page.fill('#d-name', 'e2e-router');
  await page.fill('#d-ip', ip);
  await page.fill('#d-id', String(Math.floor(Math.random() * 9000 + 1000)));
  await page.click('#d-add');

  const row = page.locator('table tbody tr', { hasText: ip });
  await expect(row).toBeVisible();

  page.on('dialog', d => d.accept()); // confirm() on delete
  await row.locator('button.d-del').click();
  await expect(page.locator('table tbody tr', { hasText: ip })).toHaveCount(0);
});

test('retention page shows the 180-day window, storage and per-day breakdown', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await nav(page, 'Retention');
  await expect(page.locator('#pageBody')).toContainText('Retention target');
  await expect(page.locator('#pageBody')).toContainText('180 days');
  await expect(page.locator('#pageBody')).toContainText('S3 Cold Archive');
  await expect(page.locator('#pageBody table tbody tr')).not.toHaveCount(0);
});

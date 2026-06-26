const { test, expect } = require('@playwright/test');
const { login, nav, ADMIN_EMAIL, ADMIN_PW } = require('./helpers');

test('settings: dataplane tuning saves and persists', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await nav(page, 'Settings');
  await expect(page.locator('#dp-batch')).toBeVisible();
  await page.fill('#dp-batch', '8123');
  await page.click('#dp-save');
  await expect(page.locator('.toast')).toBeVisible();
  await nav(page, 'Overview');
  await nav(page, 'Settings');
  await expect(page.locator('#dp-batch')).toHaveValue('8123');
  await page.fill('#dp-batch', '5000');
  await page.click('#dp-save'); // restore
});

test('settings: skip-rules / retention / s3 tabs render', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await nav(page, 'Settings');
  await page.click('.tab:has-text("Skip Rules")');
  await expect(page.locator('#sk-dns')).toBeVisible();
  await page.click('.tab:has-text("Retention")');
  await expect(page.locator('#rt-days')).toBeVisible();
  await page.click('.tab:has-text("S3 Archive")');
  await expect(page.locator('#s3-bk')).toBeVisible();
});

test('dataplanes page shows the dataplane + host resources', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await nav(page, 'Dataplanes');
  await expect(page.locator('#pageBody')).toContainText('Host resources');
  await expect(page.locator('#pageBody')).toContainText('CPU load');
  await expect(page.locator('#pageBody')).toContainText('Memory');
});

test('device edit modal updates a device', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await nav(page, 'Devices');
  await page.locator('table tbody .d-edit').first().click();
  await expect(page.locator('.modal')).toBeVisible();
  const newName = 'edited-' + (Date.now() % 100000);
  await page.fill('#e-name', newName);
  await page.click('#e-save');
  await expect(page.locator('.modal-bg')).toBeHidden();
  await expect(page.locator('table tbody')).toContainText(newName);
});

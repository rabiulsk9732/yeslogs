const { test, expect } = require('@playwright/test');
const { login, nav, ADMIN_EMAIL, ADMIN_PW } = require('./helpers');

const DIRECTOR_NAV = ['Overview', 'ISPs', 'Dataplanes', 'Devices', 'Capture Policies', 'Logs', 'Reports', 'Retention', 'Audit', 'Settings'];

test('product is YesLogs Director, not an IPDR/compliance console', async ({ page }) => {
  await page.goto('/');
  await expect(page).toHaveTitle(/YesLogs Director/);
  await expect(page.locator('#login')).not.toContainText('IPDR');
  await expect(page.locator('#login')).not.toContainText('lawful');
  await expect(page.locator('#login')).not.toContainText('Compliance Division');
});

test('director navigation has all 10 sections in order', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  const items = await page.locator('#menu a').allInnerTexts();
  const norm = items.map(s => s.trim());
  for (const want of DIRECTOR_NAV) {
    expect(norm.some(t => t.includes(want))).toBeTruthy();
  }
});

test('overview shows the operations cards', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await nav(page, 'Overview');
  await expect(page.locator('#pageBody')).toContainText('Flows Ingested');
  await expect(page.locator('#pageBody')).toContainText('Active Dataplanes');
  await expect(page.locator('#pageBody')).toContainText('Hot Storage Used');
  await expect(page.locator('#pageBody')).toContainText('Queue Pressure');
});

test('capture policies can be created and assigned', async ({ page }) => {
  await login(page, ADMIN_EMAIL, ADMIN_PW);
  await nav(page, 'Capture Policies');
  await expect(page.locator('#p-name')).toBeVisible();
  const name = 'e2e-pol-' + (Date.now() % 100000);
  await page.fill('#p-name', name);
  await page.click('#p-add');
  await expect(page.locator('table tbody')).toContainText(name);
  // it appears as an option in the device capture-policy dropdown
  await nav(page, 'Devices');
  await expect(page.locator('#d-pol')).toContainText(name);
});

const { expect } = require('@playwright/test');

const ADMIN_EMAIL = process.env.ADMIN_EMAIL || 'admin@sayra.io';
const ADMIN_PW = process.env.ADMIN_PW || '';
const ISP_EMAIL = process.env.ISP_EMAIL || 'isp@demo.io';
const ISP_PW = process.env.ISP_PW || 'demo12345';

async function login(page, email, pw) {
  await page.goto('/');
  await expect(page.locator('#login')).toBeVisible();
  await page.fill('#li-email', email);
  await page.fill('#li-pass', pw);
  await page.click('#li-btn');
  await expect(page.locator('#app')).toBeVisible();
}

const nav = (page, name) => page.locator('#menu a', { hasText: name }).first().click();

module.exports = { login, nav, ADMIN_EMAIL, ADMIN_PW, ISP_EMAIL, ISP_PW };

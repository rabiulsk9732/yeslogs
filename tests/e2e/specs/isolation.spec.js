const { test, expect } = require('@playwright/test');
const { login, ISP_EMAIL, ISP_PW } = require('./helpers');

// Tenant isolation: an ISP-role user must not see the director-only ISPs admin,
// and cannot reach another tenant's flows via the API.
test('ISP user does not see the director-only ISPs menu', async ({ page }) => {
  await login(page, ISP_EMAIL, ISP_PW);
  await expect(page.locator('#menu')).toContainText('Devices');
  await expect(page.locator('#menu')).toContainText('Logs');
  await expect(page.locator('#menu')).not.toContainText('ISPs');
});

test('ISP user is blocked from cross-tenant flow queries (?isp=other → 403)', async ({ page }) => {
  await login(page, ISP_EMAIL, ISP_PW);
  // ISP user is pinned to their own isp_id; requesting another isp must be forbidden.
  const status = await page.evaluate(async () => {
    const r = await fetch('/api/v1/devices?isp=999999', { headers: { 'Content-Type': 'application/json' } });
    return r.status;
  });
  expect(status).toBe(403);
});

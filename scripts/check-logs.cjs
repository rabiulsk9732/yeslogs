// Render populated search responses in Chromium: an empty table cannot exercise row helpers.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('../tests/e2e/node_modules/@playwright/test');
const root = path.resolve(process.argv[2] || 'internal/director/web/console');
const mime = { '.html': 'text/html', '.js': 'application/javascript', '.css': 'text/css', '.woff2': 'font/woff2' };

async function main() {
  const browser = await chromium.launch({ args: ['--no-sandbox'] });
  try {
    for (const director of [true, false]) {
      const page = await browser.newPage();
      page.setDefaultTimeout(5000);
      const errors = [], requests = [];
      let mode = 'rows';
      const base = { date: '2026-09-11', clock: '00:38:59', sub: 'Test edge', privIp: '100.64.1.10', privPort: 1234, pubIp: '203.0.113.10', pubPort: 4321, proto: 'TCP', dest: '192.0.2.1:443', action: 'IPFIX' };
      const rows = [
        { ...base, username: '<subscriber>', crmUsername: 'crm-fallback', crmStatus: 'matched', crmName: '<Customer>' },
        { ...base, crmUsername: '<crm-user>', crmStatus: 'matched' },
        { ...base, pubIp: '', crmStatus: 'not_found' },
      ];
      page.on('pageerror', e => errors.push(e.message));
      await page.route('http://logs.test/**', async route => {
        const p = new URL(route.request().url()).pathname;
        const reply = (status, data) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(data) });
        if (!p.startsWith('/api/')) {
          const relative = p === '/' ? 'index.html' : p.replace(/^\/_ui\/[a-f0-9]{40}\//, '/').slice(1);
          const file = path.resolve(root, relative);
          if (!file.startsWith(root + path.sep) || !fs.existsSync(file)) return route.fulfill({ status: 404 });
          return route.fulfill({ contentType: mime[path.extname(file)] || 'application/octet-stream', body: fs.readFileSync(file) });
        }
        if (p === '/api/v1/me') return reply(200, { email: 'test@example.invalid', isDirector: director, ispId: director ? 0 : 5, csrf: 'test-csrf' });
        if (p === '/api/v1/devices') return reply(200, { devices: [{ ISPID: 5, DeviceID: 8, Name: 'Test edge' }], ...(director ? { isps: [{ ID: 5, Name: 'Test ISP' }] } : {}) });
        if (p === '/api/v1/search') {
          const body = route.request().postDataJSON();
          requests.push(body);
          assert.equal(route.request().headers()['x-csrf-token'], 'test-csrf');
          if (mode === 'network') return route.abort('failed');
          if (mode === 'server') return reply(503, { error: 'Flow store unavailable' });
          const records = mode === 'empty' ? [] : rows;
          return reply(200, { records, total: records.length ? 72932 : 0, elapsedMs: 1399, cold: mode === 'cold', crm: { enabled: mode !== 'no-crm', matched: 2, rows: 3, status: 'partial' } });
        }
        return reply(200, {});
      });
      await page.goto('http://logs.test/#/logs');
      await page.locator('#s-dev').waitFor();
      if (director) await page.locator('#s-isp').selectOption('5');
      await page.locator('#s-dev').selectOption('8');
      await page.locator('#s-from').fill('2026-09-11 00:36');
      await page.locator('#s-to').fill('2026-09-11 00:39');
      const search = () => page.locator('#s-run').click();
      const table = () => page.locator('#logTable').waitFor();
      await search();
      await table();
      assert.equal(await page.locator('#logTable tbody tr').count(), 3);
      const cells = page.locator('#logTable tbody tr').first().locator('td');
      assert.equal(await cells.nth(3).innerText(), '<subscriber>');
      assert.equal(await cells.nth(4).innerText(), '203.0.113.10:4321');
      assert.match(await page.locator('#logTable tbody tr').nth(1).locator('td').nth(3).innerText(), /<crm-user>.*via CRM/);
      assert.equal(await page.locator('#logTable tbody tr').nth(2).locator('td').nth(3).innerText(), '—');
      assert.equal(await page.locator('#logTable tbody tr').nth(2).locator('td').nth(4).innerText(), '—');
      assert.equal(await page.locator('#logTable subscriber, #logTable crm-user, #logTable customer').count(), 0);
      assert.equal(await page.locator('#s-msg').innerText(), '');
      assert.equal(await page.locator('.index-stats .v').first().innerText(), '72,932');
      assert.equal(await page.locator('#s-results a[href^="/api/v1/report"]').count(), 3);
      await page.locator('.pgb', { hasText: 'Next' }).click();
      await table();
      assert.equal(requests.at(-1).Offset, 50);
      assert.equal(requests.at(-1).DeviceID, 8);
      assert.equal(requests.at(-1).ISPID, director ? 5 : 0);
      await page.locator('#lq-size').selectOption('25');
      await table();
      assert.equal(requests.at(-1).Limit, 25);
      assert.equal(requests.at(-1).Offset, 0);
      mode = 'cold'; await search(); await table();
      assert.equal(await page.locator('.index-stats .v').nth(3).innerText(), 'Hot + S3');
      mode = 'no-crm'; await search(); await table();
      assert.equal(await page.locator('#logTable thead th').count(), 8);
      mode = 'empty'; await search(); await table();
      assert.match(await page.locator('#logTable').innerText(), /No matching flow logs/);
      for (mode of ['network', 'server']) {
        await search();
        await page.waitForFunction(() => document.querySelector('#s-msg').textContent.length > 0);
        assert.match(await page.locator('#s-msg').innerText(), mode === 'network' ? /connection failed/ : /Flow store unavailable/);
        assert.equal(await page.locator('#logTable').count(), 0);
        assert.deepEqual(await page.locator('.index-stats .v').allInnerTexts(), ['—', '—', '—', '—', '—']);
      }
      // A render failure must never be described as a network timeout or retain success totals.
      await page.evaluate(() => { window.savedSubCell = subCell; subCell = () => { throw new Error('Injected rendering failure'); }; });
      mode = 'rows'; await search();
      await page.waitForFunction(() => document.querySelector('#s-msg').textContent.includes('could not be displayed'));
      assert.deepEqual(await page.locator('.index-stats .v').allInnerTexts(), ['—', '—', '—', '—', '—']);
      await page.evaluate(() => { subCell = window.savedSubCell; });
      await search(); await table();
      assert.equal(await page.locator('#s-msg').innerText(), '');
      assert.deepEqual(errors, []);
      await page.close();
    }
    console.log('Logs browser checks passed: populated rows, subscriber/CRM/NAT rendering, escaping, pagination, exports, empty/cold responses, failures and retry for both roles.');
  } finally { await browser.close(); }
}
main().catch(e => { console.error(e); process.exitCode = 1; });

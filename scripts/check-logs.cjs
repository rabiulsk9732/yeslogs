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
      let delayMs = 0;
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
          if (delayMs) await new Promise(resolve => setTimeout(resolve,delayMs));
          assert.equal(route.request().headers()['x-csrf-token'], 'test-csrf');
          if (mode === 'network') return route.abort('failed');
          if (mode === 'server') return reply(503, { error: 'Flow store unavailable' });
          const records = mode === 'empty' ? [] : mode === 'large' ? Array.from({length:body.Limit},(_,i)=>({...rows[i%3],clock:'00:38:'+String(i%60).padStart(2,'0')})) : rows;
          if (mode === 'invalid') return reply(200, {records: 'invalid', total: 10});
          return reply(200, { records, total: records.length ? 72932 : 0, elapsedMs: 1399, cold: mode === 'cold', crm: { enabled: mode !== 'no-crm', matched: 2, rows: 3, status: 'partial' } });
        }
        return reply(200, {});
      });
      const shot = async name => { if(process.env.LOGS_SHOTS_DIR){fs.mkdirSync(process.env.LOGS_SHOTS_DIR,{recursive:true});await page.screenshot({path:path.join(process.env.LOGS_SHOTS_DIR,`${director?'director':'isp'}-${name}.png`)});} };
      await page.setViewportSize({width:1440,height:1100});
      await page.goto('http://logs.test/#/logs');
      await page.locator('#logs-open-filters:enabled').waitFor();
      assert.equal(requests.length,0,'Opening Logs does not run an unaudited default search');
      assert.equal(await page.locator('#logs-filter-modal').isVisible(),false);
      await shot('initial');
      await page.setViewportSize({width:390,height:844});
      assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
      await page.locator('[data-log-action=filters]').scrollIntoViewIfNeeded();
      assert(await page.locator('[data-log-action=filters]').evaluate(n=>n.getBoundingClientRect().right<=innerWidth));
      await shot('mobile-initial');
      await page.setViewportSize({width:1440,height:1100});
      await page.locator('#logs-open-filters').click();
      await page.locator('#s-dev').waitFor();
      await shot('filters');
      if (director) await page.locator('#s-isp').selectOption('5');
      await page.locator('#s-dev').selectOption('8');
      assert(await page.locator('#s-from').inputValue(),'Recent 15 minutes is a visible default');
      await page.locator('#s-from').fill('2026-09-11 00:36');
      await page.locator('#s-to').fill('2026-09-11 00:39');
      const search = async () => { if (!await page.locator('#logs-filter-modal').isVisible()) await page.locator('#logs-open-filters').click(); await page.locator('#logs-filter-title').click(); await page.locator('#s-run').click(); };
      const table = () => page.locator('#logs-pagination:not(:empty)').waitFor();
      await search();
      await table();
      assert.equal(await page.locator('#logTable tbody tr').count(), 3);
      const cells = page.locator('#logTable tbody tr').first().locator('td');
      assert.match(await cells.nth(3).innerText(), /<subscriber>\s+From exporter/);
      assert.equal(await cells.nth(4).innerText(), '203.0.113.10:4321');
      assert.match(await page.locator('#logTable tbody tr').nth(1).locator('td').nth(3).innerText(), /<crm-user>\s+via CRM/);
      assert.equal(await page.locator('#logTable tbody tr').nth(2).locator('td').nth(3).innerText(), '—');
      assert.equal(await page.locator('#logTable tbody tr').nth(2).locator('td').nth(4).innerText(), '—');
      assert.equal(await page.locator('#logTable subscriber, #logTable crm-user, #logTable customer').count(), 0);
      assert.equal(await page.locator('#s-msg').innerText(), '');
      assert.equal(await page.locator('.index-stats .v').first().innerText(), '72,932');
      assert.equal(await page.locator('#s-results a[href^="/api/v1/report"]').count(), 3);
      await shot('rows');
      const appliedChips = await page.locator('#logs-chips').innerText(), exportsBefore = await page.locator('#logs-exports').innerHTML();
      await page.locator('#logs-open-filters').click();
      const beforeEdit = requests.length;
      await page.locator('#s-pub').fill('192.0.2.99');
      if(director) await page.locator('#s-isp').selectOption('0');
      assert.equal(await page.locator('#logs-chips').innerText(),appliedChips);
      assert.equal(await page.locator('#logs-exports').innerHTML(),exportsBefore);
      await page.locator('[data-filter-close]').last().click();
      assert.equal(requests.length,beforeEdit);
      assert.equal(await page.locator('#s-pub').inputValue(),'');
      assert.equal(await page.locator('#s-dev').inputValue(),'8');
      await page.locator('[data-details="0"]').click();
      await page.getByRole('dialog').getByText('Flow record details').waitFor();
      assert.match(await page.getByRole('dialog').innerText(), /<subscriber>/);
      await page.keyboard.press('Escape');
      assert.equal(await page.getByRole('dialog').count(),0);
      await page.locator('.pgb', { hasText: 'Next' }).click();
      await table();
      assert.equal(requests.at(-1).Offset, 50);
      assert.equal(requests.at(-1).DeviceID, 8);
      assert.equal(requests.at(-1).ISPID, director ? 5 : 0);
      const beforeCached = requests.length;
      await page.locator('.pgb',{hasText:'Prev'}).click(); await table();
      assert.equal(requests.length,beforeCached,'Previous page reuses only this search cache');
      assert.match(await page.locator('#logs-timing').innerText(),/Previously loaded page/);
      await page.locator('#logs-refresh').click(); await table();
      assert.equal(requests.length,beforeCached+1,'Refresh bypasses cached results');
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
      await shot('empty');
      for (mode of ['network', 'server']) {
        await search();
        await page.waitForFunction(() => document.querySelector('#s-msg').textContent.length > 0);
        assert.match(await page.locator('#s-msg').innerText(), mode === 'network' ? /connection failed/ : /Flow store unavailable/);
        assert.equal(await page.locator('#logTable [data-record]').count(), 0);
        assert.deepEqual(await page.locator('.index-stats .v').allInnerTexts(), ['—', '—', '—', '—', '—']);
      }
      // A render failure must never be described as a network timeout or retain success totals.
      mode = 'invalid';
      await search();
      await page.waitForFunction(() => document.querySelector('#s-msg').textContent.includes('could not be displayed'));
      assert.deepEqual(await page.locator('.index-stats .v').allInnerTexts(), ['—', '—', '—', '—', '—']);
      mode = 'rows';
      await search(); await table();
      assert.equal(await page.locator('#s-msg').innerText(), '');
      // A cancelled response must never overwrite the cancellation or next query.
      delayMs=350;await search();await page.locator('[data-log-action=cancel]').click();
      await page.getByText('Search cancelled',{exact:true}).waitFor();
      await page.waitForTimeout(400);
      assert.equal(await page.locator('#logTable [data-record]').count(),0);
      delayMs=0;mode='large';await search();await table();
      await page.locator('#lq-size').selectOption('200');await table();
      assert.equal(await page.locator('#logTable [data-record]').count(),200);
      await shot('large');
      await page.locator('.pgb',{hasText:'Next'}).click();await table();
      const paints = await page.evaluate(async () => {
        const samples=[];
        for(let i=0;i<20;i++){
          const button=[...document.querySelectorAll('.pgb')].find(n=>n.textContent.includes(i%2?'Next':'Prev'));
          const start=performance.now();button.click();
          await new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)));
          samples.push(performance.now()-start);
        }
        return samples.sort((a,b)=>a-b);
      });
      console.log(JSON.stringify({case:'cached_200_row_page_paint',role:director?'director':'isp',samples:paints.length,p50_ms:+paints[10].toFixed(2),p95_ms:+paints[18].toFixed(2)}));
      // Draft validation and quick time windows work without sending invalid searches.
      await page.locator('#logs-open-filters').click();await page.locator('#logs-advanced summary').click();
      await page.locator('#s-priv').fill('999.1.2.3');const beforeInvalid=requests.length;
      await page.locator('#s-run').click();assert.equal(requests.length,beforeInvalid);
      assert.equal(await page.locator('#s-priv').getAttribute('aria-invalid'),'true');
      await page.locator('#s-priv').fill('');await page.locator('[data-minutes="15"]').click();
      const from=await page.locator('#s-from').inputValue(),to=await page.locator('#s-to').inputValue();
      assert.equal(Date.parse(to.replace(' ','T')+':00+05:30')-Date.parse(from.replace(' ','T')+':00+05:30'),15*60000);
      await page.locator('[data-filter-close]').last().click();
      await page.setViewportSize({width:390,height:844});
      assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
      await shot('mobile-rows');await page.locator('#logs-open-filters').click();
      assert(await page.locator('#s-run').evaluate(n=>n.getBoundingClientRect().bottom<=innerHeight));
      assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
      await shot('mobile-filters');
      await page.locator('[data-filter-close]').last().click();
      assert.deepEqual(errors, []);
      await page.unrouteAll({behavior:'ignoreErrors'});await page.close();
    }
    console.log('Logs browser checks passed: populated rows, subscriber/CRM/NAT rendering, escaping, pagination, exports, empty/cold responses, failures and retry for both roles.');
  } finally { await browser.close(); }
}
main().catch(e => { console.error(e); process.exitCode = 1; });

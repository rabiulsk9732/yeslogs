// Exercise a release against an isolated HTTP server; never use production accounts.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromium } = require('../tests/e2e/node_modules/@playwright/test');

async function main() {
  const root = path.resolve(process.argv[2]);
  const { revision } = JSON.parse(fs.readFileSync(path.join(root, 'ui-version.json')));
  const prefix = `/_ui/${revision}/`;
  const mime = { '.html': 'text/html', '.js': 'application/javascript', '.css': 'text/css', '.woff2': 'font/woff2' };
  const server = http.createServer((req, res) => {
    const pathname = new URL(req.url, 'http://localhost').pathname;
    if (pathname.startsWith('/api/')) {
      res.writeHead(401, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: 'Test sign-in rejected' }));
      return;
    }
    const relative = pathname === '/' ? 'index.html' : pathname.startsWith(prefix) ? pathname.slice(prefix.length) : pathname.slice(1);
    const file = path.resolve(root, relative);
    if (!file.startsWith(root + path.sep) || !fs.existsSync(file) || !fs.statSync(file).isFile()) {
      res.writeHead(404); res.end(); return;
    }
    res.writeHead(200, { 'Content-Type': mime[path.extname(file)] || 'application/octet-stream' });
    fs.createReadStream(file).pipe(res);
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  let browser;
  try {
    browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
    const page = await browser.newPage({ viewport: { width: 1440, height: 900 } });
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('response', response => {
      if (response.url().includes('/assets/') && response.status() !== 200) errors.push(`Asset ${response.status()}: ${response.url()}`);
    });
    await page.goto(`http://127.0.0.1:${server.address().port}/`, { waitUntil: 'networkidle' });
    await page.locator('#login').waitFor({ state: 'visible' });
    const urls = await page.locator('script[src],link[href]').evaluateAll(nodes => nodes.map(node => node.getAttribute('src') || node.getAttribute('href')));
    assert(urls.length > 0 && urls.every(url => url.startsWith(prefix)), 'Assets must stay pinned to this release');
    assert.equal(await page.evaluate(() => typeof window.jQuery), 'function');
    await page.locator('#li-email').fill('console-test@example.invalid');
    await page.locator('#li-pass').fill('test-only');
    await page.locator('#li-btn').click();
    await page.locator('#login-err').waitFor({ state: 'visible' });
    assert.match(await page.locator('#login-err').innerText(), /Test sign-in rejected/);
    assert.deepEqual(errors, [], 'Release must load assets and run without JavaScript errors');
    console.log(`Browser check passed: ${revision}; versioned assets, login and error handling work.`);
  } finally {
    if (browser) await browser.close();
    await new Promise(resolve => server.close(resolve));
  }
}

main().catch(error => { console.error(error); process.exitCode = 1; });

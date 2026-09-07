// Isolated dashboard integration checks. All API responses are synthetic.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('../tests/e2e/node_modules/@playwright/test');
const root = path.resolve(process.argv[2] || 'internal/director/web/console');
const mime = {'.js':'application/javascript','.css':'text/css','.html':'text/html','.woff2':'font/woff2'};
const isps = [{ID:1,Name:'North Broadband',Enabled:true},{ID:2,Name:'South Networks',Enabled:true}];
const devices = [
  {ID:11,ISPID:1,DeviceID:1,Name:'North edge',ExporterIP:'192.0.2.1',Protocol:'netflow9',Enabled:true},
  {ID:12,ISPID:1,DeviceID:2,Name:'North backup',ExporterIP:'192.0.2.2',Protocol:'ipfix',Enabled:true},
  {ID:13,ISPID:1,DeviceID:3,Name:'North disabled',ExporterIP:'192.0.2.3',Protocol:'auto',Enabled:false},
  {ID:21,ISPID:2,DeviceID:1,Name:'South edge',ExporterIP:'198.51.100.1',Protocol:'netflow9',Enabled:true}
];
const health={11:{status:'online',lastSeen:new Date().toISOString(),agoSecs:10},12:{status:'silent',lastSeen:new Date(Date.now()-3600000).toISOString(),agoSecs:3600},21:{status:'nodata',lastSeen:'0001-01-01T00:00:00Z',agoSecs:0}};
const widget=(label,value)=>({label,value});

async function open(browser, director, options={}) {
  const context=await browser.newContext({viewport:{width:1440,height:1100}});
  const page=await context.newPage();
  await page.clock.install();
  const calls=[],errors=[];
  const scenario={fail:new Set(),today:'24,000',empty:false,...options};
  page.on('pageerror',e=>errors.push(e.message));
  await page.route('http://dashboard.test/**',async route=>{
    const url=new URL(route.request().url());
    if(!url.pathname.startsWith('/api/')){
      const relative=url.pathname==='/'?'index.html':url.pathname.replace(/^\/_ui\/[a-f0-9]{40}\//,'/').slice(1);
      const file=path.resolve(root,relative);
      if(!file.startsWith(root+path.sep) || !fs.existsSync(file)){await route.fulfill({status:404,body:'Missing'});return;}
      await route.fulfill({contentType:mime[path.extname(file)] || 'application/octet-stream',body:fs.readFileSync(file)});return;
    }
    calls.push(url.pathname);
    if(scenario.fail.has(url.pathname)){await route.fulfill({status:503,contentType:'application/json',body:'{}'});return;}
    const own=scenario.empty?[]:devices.filter(d=>director || d.ISPID===1);
    const items=own.map(d=>({storeId:d.ID,deviceId:d.DeviceID,name:d.Name,state:!d.Enabled?'disabled':d.ID===11?'ok':d.ID===12?'no_port':'silent'}));
    const responses={
      '/api/v1/me':{email:director?'director@example.invalid':'isp@example.invalid',role:director?'director':'isp',isDirector:director,ispId:director?0:1,csrf:'test-only'},
      '/api/v1/devices':{devices:scenario.leakInventory?devices:own,health:scenario.noHealth?{}:health,...(director?{isps:scenario.empty?[]:isps}:{}),isDirector:director},
      '/api/v1/overview':{cards:[widget('Flows Stored Today',scenario.empty?'0':scenario.today),...(director?[widget('Flows Ingested','2,500,000'),widget('Flows Skipped','150,000'),widget('Hot Storage Used','42.5 GiB'),{...widget('Queue Pressure','0%'),pct:0}]:[])]},
      '/api/v1/console/data':{widgets:[widget('NAT Flows (window)','48,000'),widget('Subscribers Seen','1,200'),widget('Logged Volume','82.1 GiB')],infoBoxes:[widget('CGNAT Public IPs Seen','12')],hourly:Array.from({length:24},(_,i)=>scenario.empty?0:(i+1)*100),records:scenario.empty?[]:Array.from({length:12},(_,i)=>({devId:1,sub:director?'Shared device ID':'North edge',time:'2026-09-07 12:00:'+String(i).padStart(2,'0'),privIp:'100.64.0.1',privPort:45000,pubIp:'203.0.113.1',pubPort:54000,proto:'TCP',dest:'192.0.2.80'}))},
      '/api/v1/system':{disk:{total:100000000000,used:40000000000,pct:40},memory:{total:16000000000,pct:30},cpu:{load1:0.25,cores:8},process:{uptime:86450}},
      '/api/v1/compliance':{available:true,generatedAt:new Date().toISOString(),devices:items,missingDays:scenario.empty?[]:['2026-09-01'],days:[{date:'2026-09-07',flows:24000}]},
      '/api/v1/audit':{queries:[]}
    };
    await route.fulfill({status:responses[url.pathname]?200:404,contentType:'application/json',body:JSON.stringify(responses[url.pathname] || {})});
  });
  await page.goto('http://dashboard.test/');
  await page.locator('.dash-kpi').last().waitFor();
  await page.waitForFunction(()=>document.querySelector('#dash-refresh')?.disabled===false && !document.querySelector('#dash-quality-status')?.textContent.includes('Loading'));
  return {page,context,calls,errors,scenario};
}

async function run() {
  const browser=await chromium.launch({headless:true,args:['--no-sandbox']});
  try{
    const director=await open(browser,true);
    const {page}=director;
    assert.equal(await page.locator('.dash-kpi').count(),10);
    for(const width of [1280,1440,1920]){
      await page.setViewportSize({width,height:1100});
      const boxes=await page.locator('.dash-kpi').evaluateAll(nodes=>nodes.map(n=>({x:n.offsetLeft,y:n.offsetTop,width:n.offsetWidth})));
      const rows=[...new Set(boxes.map(b=>b.y))];
      assert.equal(rows.length,2,`Two card rows at ${width}`);
      assert.equal(boxes.filter(b=>b.y===rows[0]).length,5,`Five cards per row at ${width}`);
      assert(boxes.every(b=>Math.abs(b.width-boxes[0].width)<=1),'Equal-width cards');
    }
    assert.equal(await page.locator('[data-kpi="enabled"] .v').innerText(),'3');
    assert.equal(await page.locator('[data-kpi="online"] .v').innerText(),'1');
    assert.equal(await page.locator('[data-kpi="silent"] .v').innerText(),'1');
    assert.equal(await page.locator('[data-kpi="nodata"] .v').innerText(),'1');
    assert.equal(await page.locator('#dash-recent-rows tr').count(),10);
    // Repeated DeviceID across ISPs cannot be confidently attributed.
    assert.match(await page.locator('#dash-recent-rows tr').first().innerText(),/Not reported/);
    assert(!await page.locator('.dashboard').innerText().then(t=>t.includes('ALL OK')));
    await page.locator('#dash-filter').fill('North');
    assert.equal(await page.locator('#dash-status-rows tr').count(),1);
    await page.locator('#dash-refresh').click();
    await page.waitForFunction(()=>!document.querySelector('#dash-refresh').disabled);
    assert.equal(await page.locator('#dash-filter').inputValue(),'North');
    assert.equal(await page.locator('#dash-status-rows tr').count(),1);
    await page.locator('#dash-filter').fill('');
    await page.locator('#dash-status-rows [data-go="Logs"]').first().focus();
    await page.evaluate(()=>YesLogsDashboard.refresh(true));
    assert.equal(await page.evaluate(()=>document.activeElement?.dataset.isp),'1','Polling preserves focused row action');
    await page.locator('#dash-pause').click();
    const pausedCalls=director.calls.length;
    await page.clock.fastForward(16000);
    assert.equal(director.calls.length,pausedCalls,'Paused dashboard makes no polling requests');
    director.scenario.today='25,000';
    director.scenario.fail.add('/api/v1/devices');
    await page.locator('#dash-refresh').click();
    await page.waitForFunction(()=>!document.querySelector('#dash-refresh').disabled);
    assert.equal(await page.locator('[data-kpi="today"] .v').innerText(),'25,000');
    assert.equal(await page.locator('[data-kpi="online"] .v').innerText(),'1');
    assert.match(await page.locator('[data-kpi="online"]').innerText(),/Stale/);
    assert.equal(await page.locator('#dash-errors').isVisible(),true);
    director.scenario.fail.clear();
    await page.locator('#dash-refresh').click();
    await page.waitForFunction(()=>!document.querySelector('#dash-refresh').disabled);
    assert.equal(await page.locator('#dash-errors').isVisible(),false);
    if(process.env.DASHBOARD_SHOTS_DIR){fs.mkdirSync(process.env.DASHBOARD_SHOTS_DIR,{recursive:true});await page.setViewportSize({width:1440,height:1100});await page.screenshot({path:path.join(process.env.DASHBOARD_SHOTS_DIR,'director.png'),fullPage:true});}
    await page.evaluate(()=>Object.defineProperty(document,'hidden',{configurable:true,value:true}));
    await page.locator('#dash-pause').click(); // resume while hidden
    const hiddenCalls=director.calls.length;await page.clock.fastForward(16000);
    assert.equal(director.calls.length,hiddenCalls,'Hidden dashboard does not poll');
    await page.evaluate(()=>Object.defineProperty(document,'hidden',{configurable:true,value:false}));
    await page.locator('#dash-status-rows [data-go="Logs"]').first().click();
    await page.locator('#s-isp').waitFor();
    await page.waitForFunction(()=>document.querySelector('#s-isp')?.value==='1');
    assert.equal(await page.locator('#s-dev').isDisabled(),false);
    const afterNavigation=director.calls.length;await page.clock.fastForward(16000);
    assert.equal(director.calls.length,afterNavigation,'Dashboard stops polling after navigation');
    assert.deepEqual(director.errors,[]);
    await director.context.close();

    const isp=await open(browser,false);
    assert.equal(await isp.page.locator('.dash-kpi').count(),10);
    assert(!isp.calls.includes('/api/v1/system'),'ISP never requests global host metrics');
    assert(!isp.calls.includes('/api/v1/isps'),'ISP never requests another tenant directory');
    const text=await isp.page.locator('.dashboard').innerText();
    assert(!/South Networks|South edge|Collector resources|Hot data size|Queue pressure|Add ISP/.test(text));
    assert.equal(await isp.page.locator('.sbrand-sub').innerText(),'ISP CONSOLE');
    assert.equal(await isp.page.locator('[data-kpi="enabled"] .v').innerText(),'2');
    assert.equal(await isp.page.locator('[data-kpi="online"] .v').innerText(),'1');
    if(process.env.DASHBOARD_SHOTS_DIR)await isp.page.screenshot({path:path.join(process.env.DASHBOARD_SHOTS_DIR,'isp.png'),fullPage:true});
    for(const width of [900,390]){
      await isp.page.setViewportSize({width,height:1000});
      const overflow=await isp.page.evaluate(()=>document.documentElement.scrollWidth>innerWidth);
      assert.equal(overflow,false,`No document overflow at ${width}`);
    }
    if(process.env.DASHBOARD_SHOTS_DIR)await isp.page.screenshot({path:path.join(process.env.DASHBOARD_SHOTS_DIR,'mobile.png'),fullPage:true});
    assert.deepEqual(isp.errors,[]);await isp.context.close();

    const extra=await open(browser,false,{leakInventory:true});
    assert.equal(await extra.page.locator('[data-kpi="registered"] .v').innerText(),'3');
    assert(!/South edge/.test(await extra.page.locator('.dashboard').innerText()));
    await extra.context.close();

    const unknown=await open(browser,false,{noHealth:true});
    assert.equal(await unknown.page.locator('[data-kpi="online"] .v').innerText(),'—');
    assert.match(await unknown.page.locator('#dash-attention-body').innerText(),/could not be measured/);
    await unknown.context.close();
    const empty=await open(browser,false,{empty:true});
    assert.equal(await empty.page.locator('[data-kpi="enabled"] .v').innerText(),'0');
    assert.equal(await empty.page.locator('[data-kpi="online"] .v').innerText(),'0');
    assert.match(await empty.page.locator('#dash-recent-rows').innerText(),/No recent stored records/);
    await empty.context.close();
    const failed=await open(browser,false,{fail:new Set(['/api/v1/devices','/api/v1/overview','/api/v1/compliance','/api/v1/console/data'])});
    assert.equal(await failed.page.locator('[data-kpi="today"] .v').innerText(),'—');
    assert.match(await failed.page.locator('[data-kpi="today"]').innerText(),/Unavailable/);
    assert.equal(await failed.page.locator('#dash-errors').isVisible(),true);
    await failed.context.close();
    console.log('Dashboard checks passed: Director/ISP, 5×2 desktop grid, mobile, scoped requests, truthful health, stale/error/empty states, refresh/pause and drill-down.');
  }finally{await browser.close();}
}
run().catch(error=>{console.error(error);process.exitCode=1;});

// Isolated browser regression checks. No request reaches a real collector.
const assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path');
const {chromium}=require('../tests/e2e/node_modules/@playwright/test');
const root=path.resolve(process.argv[2]||'internal/director/web/console');
const mime={'.js':'application/javascript','.css':'text/css','.html':'text/html','.woff2':'font/woff2'};
(async()=>{
 const browser=await chromium.launch({args:['--no-sandbox']});
 try{
  for(const director of [true,false]){
   const page=await browser.newPage({viewport:{width:1440,height:1100}}),errors=[],writes=[];
   let failDevices=false,failPolicies=false,healthMissing=false,failWrite=0,delayWrite=false,next=50;
   const isps=[{ID:1,Name:'Alpha ISP with a long company name'},{ID:2,Name:'Beta ISP'}];
   const policies=[{ID:1,ISPID:0,Name:'Global keep',SkipDNS:false,SkipPrivate:false,SkipZero:false},{ID:2,ISPID:1,Name:'Alpha DNS',SkipDNS:true,SkipPrivate:false,SkipZero:false},{ID:3,ISPID:2,Name:'Beta zero',SkipDNS:false,SkipPrivate:false,SkipZero:true}];
   const items=Array.from({length:12},(_,n)=>({ID:n+1,ISPID:n<8?1:2,Name:`Router ${String(n+1).padStart(2,'0')}`,ExporterIP:`192.0.2.${n+1}`,DeviceID:n<8?n+1:n-7,Protocol:n%2?'auto':'ipfix',Profile:'generic',CapturePolicy:'',Enabled:n!==7,SkipDNS:false,SkipPrivate:false,SkipZero:false,UpdatedAt:'2026-09-08T08:00:00Z'}));
   const health=Object.fromEntries(items.map((i,n)=>[i.ID,{status:['online','silent','nodata'][n%3],lastSeen:n%3===2?'0001-01-01T00:00:00Z':'2026-09-08T08:00:00Z',agoSecs:100}]));
   page.on('pageerror',e=>errors.push(e.message));
   await page.route('http://devices.test/**',async r=>{
    const url=new URL(r.request().url()),method=r.request().method();
    const reply=(status,data)=>r.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
    if(!url.pathname.startsWith('/api/')){const relative=url.pathname==='/'?'index.html':url.pathname.replace(/^\/_ui\/[a-f0-9]{40}\//,'/').slice(1),file=path.resolve(root,relative);if(!file.startsWith(root+path.sep)||!fs.existsSync(file))return r.fulfill({status:404});return r.fulfill({contentType:mime[path.extname(file)]||'application/octet-stream',body:fs.readFileSync(file)})}
    if(url.pathname==='/api/v1/me')return reply(200,{email:'operator@example.invalid',isDirector:director,ispId:director?0:1,role:director?'director':'isp',csrf:'test-csrf'});
    if(process.env.DEVICE_TEST_DELAY_MS)await new Promise(resolve=>setTimeout(resolve,Number(process.env.DEVICE_TEST_DELAY_MS)));
    if(url.pathname==='/api/v1/policies')return failPolicies?reply(503,{error:'Policies temporarily unavailable'}):reply(200,{policies:policies.filter(p=>director||p.ISPID===0||p.ISPID===1),isDirector:director});
    if(url.pathname.startsWith('/api/v1/devices')){
     if(method==='GET')return failDevices?reply(503,{error:'Devices temporarily unavailable'}):reply(200,{devices:items.filter(i=>director||i.ISPID===1),isDirector:director,...(director?{isps}:{}),...(healthMissing?{}:{health})});
     const b=r.request().postData()?r.request().postDataJSON():undefined,id=Number(url.pathname.split('/')[4]);writes.push({method,url:url.pathname,body:b,csrf:r.request().headers()['x-csrf-token']});
     if(delayWrite)await new Promise(resolve=>setTimeout(resolve,150));
     if(failWrite)return reply(failWrite,{error:failWrite===409?'Exporter IP already exists.':'Save result unavailable.'});
     if(method==='POST'&&!id){const i={...b,ID:next++,DeviceID:99,Enabled:true,UpdatedAt:new Date().toISOString()};items.push(i);health[i.ID]={status:'nodata'};return reply(200,i)}
     const at=items.findIndex(i=>i.ID===id);if(!director&&items[at]?.ISPID!==1)return reply(404,{error:'not found'});
     if(method==='PUT'){items[at]={...items[at],...b,Enabled:b.Enabled===true};return reply(200,items[at])}
     if(method==='DELETE'){items.splice(at,1);return reply(200,{deleted:true})}
     items[at].Enabled=!items[at].Enabled;return reply(200,{enabled:items[at].Enabled});
    }
    if(url.pathname==='/api/v1/console/data')return reply(200,{isps,devices:items.filter(i=>director||i.ISPID===1),records:[]});
    return reply(200,{});
   });
   const loaded=()=>page.waitForFunction(()=>{const n=document.querySelector('.index-stats .v');return n&&n.textContent!=='—'&&!document.querySelector('#device-refresh').disabled});
   const row=id=>page.locator(`#device-rows tr[data-id="${id}"]`);
   const open=async(id,kind)=>{await row(id).locator(`[data-action=${kind}]`).click();await page.locator('.device-modal').waitFor()};
   const submit=()=>page.locator('.device-modal [type=submit]').click();
   const closed=()=>page.locator('.device-modal').waitFor({state:'hidden'});
   const refreshed=async()=>{await page.locator('#device-refresh').click();await page.waitForFunction(()=>!document.querySelector('#device-refresh').disabled)};
   await page.goto('http://devices.test/#/devices');await loaded();await page.reload();await loaded();assert.equal(await page.locator('#pageTitle').innerText(),'Devices');
   assert.deepEqual(await page.locator('.index-stats .v').allTextContents(),director?['12','11','4','3','4']:['8','7','3','2','2']);
   assert.equal(await page.locator('#device-isp-filter').count(),director?1:0);assert(await page.locator('.isp-scroll').evaluate(n=>n.scrollWidth<=n.clientWidth+1),'All columns fit a 1440px desktop');
   for(const width of [1280,1440,1920]){await page.setViewportSize({width,height:1100});assert.equal(await page.locator('.index-stats .tile').evaluateAll(ns=>new Set(ns.map(n=>n.offsetTop)).size),1)}
   if(director){assert.equal(await page.locator('#device-rows tr').count(),10);await page.locator('#device-next').click();assert.equal(await page.locator('#device-rows tr').count(),2);await page.locator('#device-isp-filter').selectOption('1');assert.equal(await page.locator('.index-stats .v').first().innerText(),'8')}
   await page.locator('#device-status-filter').selectOption('disabled');assert.equal(await page.locator('#device-rows tr').count(),1);await refreshed();assert.equal(await page.locator('#device-status-filter').inputValue(),'disabled');await page.locator('#device-status-filter').selectOption('all');
   await page.locator('#device-protocol-filter').selectOption('ipfix');await page.locator('#device-search').fill('Router 01');assert.equal(await page.locator('#device-rows tr[data-id]').count(),1);await page.locator('#device-search').fill('');await page.locator('#device-protocol-filter').selectOption('');
   // Enabled must be explicit in every PUT; the actual API defaults an omitted bool to false.
   await open(1,'edit');assert.equal(await page.locator('[name=Enabled]').inputValue(),'true');assert(await page.locator('[name=ISPID]').evaluate(n=>n.readOnly));assert(await page.locator('[name=DeviceID]').evaluate(n=>n.readOnly));
   await page.locator('[name=Name]').fill('Router one');await submit();await closed();await loaded();assert.equal(writes.at(-1).body.Enabled,true);assert.equal(items[0].Enabled,true);assert.equal(writes.at(-1).body.ISPID,1);assert.equal(writes.at(-1).body.DeviceID,1);
   await open(8,'edit');await page.locator('[name=Name]').fill('Disabled router');await submit();await closed();await loaded();assert.equal(writes.at(-1).body.Enabled,false);assert.equal(items.find(i=>i.ID===8).Enabled,false);
   await open(1,'edit');await page.locator('[name=CapturePolicy]').selectOption('Alpha DNS');await submit();await closed();await loaded();assert.equal(writes.at(-1).body.SkipDNS,true);assert.equal(writes.at(-1).body.Enabled,true);
   await open(1,'edit');policies[1].SkipDNS=false;const policyCount=writes.length;await submit();await page.getByText('This capture policy changed or is unavailable. Close and reopen the form before saving.',{exact:true}).waitFor();assert.equal(writes.length,policyCount);await page.keyboard.press('Escape');policies[1].SkipDNS=true;await refreshed();
   await page.locator('#open-add').click();
   const fields=await page.locator('.device-modal .isp-field').evaluateAll(ns=>ns.map(n=>{const l=n.querySelector('label'),s=l.querySelector('.req'),v=n.querySelector('input,select');return{icon:!!l.querySelector('.ms'),prompt:v.placeholder||v.querySelector('option[value=""]')?.textContent,right:!s||Math.abs(s.getBoundingClientRect().right-l.getBoundingClientRect().right)<2,color:!s||getComputedStyle(s).color==='rgb(193, 67, 67)'}}));
   assert(fields.every(f=>f.icon&&f.prompt&&f.right&&f.color),'Icon, prompt and right-aligned required star');
   const count=writes.length;await submit();assert.equal(writes.length,count);assert.equal(await page.locator('[aria-invalid=true]').count(),2);
   await page.locator('[name=Name]').fill('New exporter');
   for(const ip of ['999.2.3.4','192.0.2.1/24','example.com','127.1','192.0.2.01','fe80::1%eth0','::ffff:192.0.2.1']){await page.locator('[name=ExporterIP]').fill(ip);await submit();assert.equal(writes.length,count);assert.equal(await page.locator('[name=ExporterIP]').getAttribute('aria-invalid'),'true')}
   let options=await page.locator('[name=CapturePolicy] option').allTextContents();assert(options.some(s=>s.includes('Alpha DNS')));assert(!options.some(s=>s.includes('Beta zero')));
   await page.locator('[name=SkipPrivate]').check();await page.locator('[name=CapturePolicy]').selectOption('Alpha DNS');assert(await page.locator('[name=SkipDNS]').isChecked());assert(await page.locator('[name=SkipPrivate]').isDisabled());
   await page.locator('[name=CapturePolicy]').selectOption('');assert(await page.locator('[name=SkipPrivate]').isChecked());
   if(director){await page.locator('[name=CapturePolicy]').selectOption('Alpha DNS');await page.locator('[name=ISPID]').selectOption('2');options=await page.locator('[name=CapturePolicy] option').allTextContents();assert(!options.some(s=>s.includes('Alpha DNS')));assert(options.some(s=>s.includes('Beta zero')));assert.equal(await page.locator('[name=CapturePolicy]').inputValue(),'');await page.locator('[name=ISPID]').selectOption('1')}
   await page.locator('[name=ExporterIP]').fill('2001:0db8:0:0:0:0:0:50');failWrite=409;await submit();await page.getByText('Exporter IP already exists.',{exact:true}).waitFor();assert.equal(await page.locator('[name=Name]').inputValue(),'New exporter');assert(await page.locator('[name=Enabled]').isDisabled());
   failWrite=0;delayWrite=true;await submit();await page.waitForFunction(()=>document.querySelector('.device-modal [type=submit]').disabled);await page.keyboard.press('Escape');assert(await page.locator('.device-modal').isVisible());await closed();await loaded();delayWrite=false;
   assert.equal(writes.at(-1).body.ExporterIP,'2001:db8::50');assert.equal(writes.at(-1).body.DeviceID,0);assert.equal(writes.at(-1).body.Enabled,true);assert(writes.every(w=>w.csrf==='test-csrf'));
   await page.locator('#device-search').fill('New exporter');await open(50,'view');await page.getByRole('dialog').getByText('2001:db8::50',{exact:true}).waitFor();await page.locator('#device-logs').click();await page.locator('#s-pub').waitFor();assert.equal(new URL(page.url()).hash,'#/logs');assert.equal(await page.locator('#s-dev').inputValue(),'99');if(director)assert.equal(await page.locator('#s-isp').inputValue(),'1');await page.goBack();await loaded();assert.equal(new URL(page.url()).hash,'#/devices');
   await page.locator('#device-search').fill('New exporter');await open(50,'toggle');const beforeToggle=writes.length;await page.keyboard.press('Escape');assert.equal(writes.length,beforeToggle);await open(50,'toggle');await submit();await closed();await loaded();assert.equal(items.find(i=>i.ID===50).Enabled,false);
   // A stale modal cannot overwrite a concurrent change detected by a fresh read.
   await open(50,'edit');items.find(i=>i.ID===50).Name='Changed elsewhere';const staleCount=writes.length;await submit();await page.getByText('This device changed or was removed. Close and reopen it before saving.',{exact:true}).waitFor();assert.equal(writes.length,staleCount);await page.keyboard.press('Escape');await page.locator('#device-search').fill('');await refreshed();
   await page.locator('#device-search').fill('Changed elsewhere');await open(50,'edit');failWrite=500;await submit();await page.getByText(/The outcome is uncertain/).waitFor();assert(await page.locator('.device-modal [type=submit]').isDisabled());assert(!(await page.locator('.device-modal [data-close]').first().isDisabled()));await page.keyboard.press('Escape');failWrite=0;
   await open(50,'delete');const beforeDelete=writes.length;await submit();assert.equal(writes.length,beforeDelete);await page.locator('[name=Confirm]').fill('Changed elsewhere');await submit();await closed();await loaded();assert(!items.some(i=>i.ID===50));
   await page.locator('#device-search').fill('');healthMissing=true;await refreshed();assert.deepEqual((await page.locator('.index-stats .v').allTextContents()).slice(2),['—','—','—']);assert(await page.getByText(/Health is unavailable for/).isVisible());healthMissing=false;
   await page.locator('#device-limit').selectOption('25');failPolicies=true;await refreshed();assert(await page.locator('#open-add').isDisabled());await open(1,'view');assert(await page.locator('#device-detail-edit').isDisabled());await page.keyboard.press('Escape');failPolicies=false;
   failDevices=true;await refreshed();assert(await page.getByText(/Showing the last loaded list/).isVisible());assert.notEqual(await page.locator('.index-stats .v').first().innerText(),'—');failDevices=false;await refreshed();
   items[0].Name='Router " onfocus="window.injected=1';items[0].CapturePolicy='Removed policy';await refreshed();await open(1,'edit');await page.locator('[name=Name]').focus();assert.equal(await page.evaluate(()=>window.injected),undefined);assert.equal(await page.locator('[name=CapturePolicy]').inputValue(),'Removed policy');const missingCount=writes.length;await submit();assert.equal(writes.length,missingCount);assert.equal(await page.locator('[name=CapturePolicy]').getAttribute('aria-invalid'),'true');await page.keyboard.press('Escape');
   if(process.env.DEVICE_SHOTS_DIR){fs.mkdirSync(process.env.DEVICE_SHOTS_DIR,{recursive:true});await page.setViewportSize({width:1440,height:1100});await page.locator('.content-scroll').evaluate(n=>n.scrollTop=0);await page.screenshot({path:path.join(process.env.DEVICE_SHOTS_DIR,`${director?'director':'isp'}-index.png`)})}
   await page.locator('#open-add').click();await page.setViewportSize({width:390,height:900});assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);assert(await page.locator('.device-modal').evaluate(n=>n.getBoundingClientRect().width<=innerWidth));
   assert(await page.locator('.device-modal .isp-modal-foot').evaluate(n=>n.getBoundingClientRect().bottom<=innerHeight),'Modal actions stay in viewport');
   assert.equal(await page.locator('.device-modal .btn').first().evaluate(n=>getComputedStyle(n).borderRadius),'0px');
   if(process.env.DEVICE_SHOTS_DIR)await page.screenshot({path:path.join(process.env.DEVICE_SHOTS_DIR,`${director?'director':'isp'}-mobile.png`)});
   assert.deepEqual(errors,[]);await page.close();
  }
  console.log('Devices checks passed for Director and ISP: five stats, routing, scoped policies, enabled preservation, IPv4/IPv6, modal CRUD, stale/failed saves, health gaps and mobile.');
 }finally{await browser.close()}
})().catch(e=>{console.error(e);console.error('::error title=Devices browser checks::'+String(e).replace(/\n/g,'%0A'));process.exitCode=1});

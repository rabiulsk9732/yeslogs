// Isolated policy CRUD browser tests; no production traffic or credentials.
const assert=require('node:assert/strict'),fs=require('node:fs'),path=require('node:path');
const {chromium}=require('../tests/e2e/node_modules/@playwright/test');
const root=path.resolve(process.argv[2]||'internal/director/web/console'),mime={'.js':'application/javascript','.css':'text/css','.html':'text/html','.woff2':'font/woff2'};
(async()=>{const browser=await chromium.launch({args:['--no-sandbox']});try{
 for(const director of [true,false]){
  const page=await browser.newPage({viewport:{width:1440,height:1100}}),errors=[],writes=[];let failRead=false,failWrite=0,delayWrite=false,next=30;
  let finishWrite;const pendingWrite=new Promise(resolve=>{finishWrite=resolve});
  const isps=[{ID:1,Name:'Alpha ISP'},{ID:2,Name:'Beta ISP'}];
  const items=Array.from({length:12},(_,n)=>({ID:n+1,Name:`Policy ${String(n+1).padStart(2,'0')}`,ISPID:n<2?0:n<10?1:2,SkipDNS:!!(n%2),SkipPrivate:false,SkipZero:false,CreatedAt:'2026-09-08T08:00:00Z',Version:'version-1'}));
  const devices=[{ID:1,ISPID:1,DeviceID:1,Name:'Own exporter',CapturePolicy:'Policy 01'},{ID:2,ISPID:2,DeviceID:1,Name:'Other tenant exporter',CapturePolicy:'Policy 01'},{ID:3,ISPID:1,DeviceID:2,Name:'Linked exporter',CapturePolicy:'Policy 03'}].map((d,n)=>({...d,ExporterIP:`192.0.2.${n+1}`,Enabled:true,SkipDNS:false,SkipPrivate:false,SkipZero:false,Protocol:'auto',Profile:'generic'}));
  const visible=()=>items.filter(p=>director||p.ISPID===0||p.ISPID===1);
  const view=p=>{const linked=devices.filter(d=>(director||d.ISPID===1)&&(p.ISPID===0||p.ISPID===d.ISPID)&&d.CapturePolicy===p.Name),canEdit=director||p.ISPID===1;return {...p,DeviceCount:linked.length,CanEdit:canEdit,CanDelete:canEdit&&!linked.length,Devices:linked.map(d=>({...d,RulesMatch:['SkipDNS','SkipPrivate','SkipZero'].every(k=>d[k]===p[k])}))}};
  page.on('pageerror',e=>errors.push(e.message));
  await page.route('http://policies.test/**',async r=>{
   const url=new URL(r.request().url()),method=r.request().method(),reply=(status,data)=>r.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
   if(!url.pathname.startsWith('/api/')){const relative=url.pathname==='/'?'index.html':url.pathname.replace(/^\/_ui\/[a-f0-9]{40}\//,'/').slice(1),file=path.resolve(root,relative);if(!file.startsWith(root+path.sep)||!fs.existsSync(file))return r.fulfill({status:404});return r.fulfill({contentType:mime[path.extname(file)]||'application/octet-stream',body:fs.readFileSync(file)})}
   if(url.pathname==='/api/v1/me')return reply(200,{email:'operator@example.invalid',isDirector:director,ispId:director?0:1,role:director?'director':'isp',csrf:'test-csrf'});
   if(process.env.POLICY_TEST_DELAY_MS)await new Promise(resolve=>setTimeout(resolve,Number(process.env.POLICY_TEST_DELAY_MS)));
   if(url.pathname==='/api/v1/devices')return reply(200,{devices:devices.filter(d=>director||d.ISPID===1),isDirector:director,...(director?{isps}:{}),health:{1:{status:'online'},2:{status:'online'},3:{status:'online'}}});
   if(!url.pathname.startsWith('/api/v1/policies'))return reply(200,{});
   const id=Number(url.pathname.split('/')[4]),item=items.find(p=>p.ID===id);
   if(method==='GET'){if(failRead)return reply(503,{error:'Policies unavailable'});if(id)return item?reply(200,view(item)):reply(404,{error:'not found'});return reply(200,{policies:visible().map(view),isDirector:director,editablePolicies:true,...(director?{isps}:{})})}
   const b=r.request().postDataJSON();writes.push({method,body:b,csrf:r.request().headers()['x-csrf-token']});if(delayWrite)await pendingWrite;
   if(failWrite)return reply(failWrite,{error:failWrite===409?'Duplicate policy name.':'Save result unavailable.',...(failWrite===422?{fields:{Name:'Name rejected by server.'}}:{})});
   if(method==='POST'){const p={...b,ID:next++,CreatedAt:new Date().toISOString(),Version:'version-1'};items.push(p);return reply(200,view(p))}
   if(b.Version!==item.Version)return reply(409,{error:'This policy changed. Close and reopen it before saving.'});
   if(method==='PUT'){Object.assign(item,b,{Version:'version-'+(Number(item.Version.split('-')[1])+1)});return reply(200,view(item))}
   items.splice(items.indexOf(item),1);return reply(200,{deleted:true});
  });
  const loaded=()=>page.waitForFunction(()=>{const n=document.querySelector('.index-stats .v');return n&&n.textContent!=='—'&&!document.querySelector('#policy-refresh').disabled});
  const row=id=>page.locator(`#policy-rows tr[data-id="${id}"]`),open=async(id,kind)=>{await row(id).locator(`[data-action=${kind}]`).click();await page.locator('.policy-modal').waitFor()};
  const submit=()=>page.locator('.policy-modal [type=submit]').click(),closed=()=>page.locator('.policy-modal').waitFor({state:'hidden'}),refresh=async()=>{await page.locator('#policy-refresh').click();await page.waitForFunction(()=>!document.querySelector('#policy-refresh').disabled)};
  await page.goto('http://policies.test/#/capture-policies');await loaded();await page.reload();await loaded();assert.equal(await page.locator('#pageTitle').innerText(),'Capture Policies');
  assert.deepEqual(await page.locator('.index-stats .v').allTextContents(),director?['12','2','10','6','6']:['10','2','8','5','5']);
  for(const width of [1280,1440,1920]){await page.setViewportSize({width,height:1100});assert.equal(await page.locator('.index-stats .tile').evaluateAll(ns=>new Set(ns.map(n=>n.offsetTop)).size),1)}
  if(director){await page.locator('#policy-next').click();assert.equal(await page.locator('#policy-rows tr').count(),2)}
  await page.locator('#policy-scope-filter').selectOption('0');assert.equal(await page.locator('.index-stats .v').first().innerText(),'2');await page.locator('#policy-rules-filter').selectOption('keep');assert.equal(await page.locator('#policy-rows tr').count(),1);await refresh();assert.equal(await page.locator('#policy-rules-filter').inputValue(),'keep');
  if(!director){assert.equal(await row(1).locator('[data-action=edit]').count(),0);await open(1,'view');assert.equal(await page.getByRole('dialog').getByText('Other tenant exporter',{exact:true}).count(),0);assert.equal(await page.locator('#policy-detail-edit').count(),0);await page.keyboard.press('Escape')}
  await page.locator('#policy-scope-filter').selectOption('1');await page.locator('#policy-rules-filter').selectOption('all');await open(3,'delete');assert.equal(await page.locator('.policy-modal [type=submit]').count(),0);await page.getByText(/Reassign them to another policy/).waitFor();await page.keyboard.press('Escape');
  await open(3,'edit');assert(await page.locator('[name=Name]').evaluate(n=>n.readOnly));if(director)assert(await page.locator('[name=ISPID]').isDisabled());await page.locator('[name=SkipDNS]').selectOption('true');await submit();await closed();await loaded();assert.equal(writes.at(-1).body.SkipDNS,true);assert.equal(writes.at(-1).body.Version,'version-1');assert.equal(devices[2].SkipDNS,false);
  await open(3,'view');await page.getByRole('dialog').getByText('Different saved rules',{exact:true}).waitFor();await page.keyboard.press('Escape');
  await open(3,'edit');items.find(p=>p.ID===3).Version='version-3';await submit();await page.getByText('This policy changed. Close and reopen it before saving.',{exact:true}).waitFor();await page.keyboard.press('Escape');
  await page.locator('#policy-scope-filter').selectOption('all');await page.locator('#policy-create').click();
  const fields=await page.locator('.policy-modal .isp-field').evaluateAll(ns=>ns.map(n=>{const l=n.querySelector('label'),s=l.querySelector('.req'),v=n.querySelector('input,select');return{icon:!!l.querySelector('.ms'),prompt:v.placeholder||v.querySelector('option[value=""]')?.textContent,right:!s||Math.abs(s.getBoundingClientRect().right-l.getBoundingClientRect().right)<2,color:!s||getComputedStyle(s).color==='rgb(193, 67, 67)'}}));assert.equal(fields.length,5);assert(fields.every(f=>f.icon&&f.prompt&&f.right&&f.color));
  const before=writes.length;await submit();assert.equal(writes.length,before);assert.equal(await page.locator('.policy-modal [aria-invalid=true]').count(),director?2:1);await page.locator('[name=Name]').fill('Policy 01');if(director)await page.locator('[name=ISPID]').selectOption('1');await submit();assert.equal(writes.length,before);
  await page.locator('[name=Name]').fill('New capture policy');for(const k of ['SkipDNS','SkipPrivate','SkipZero'])assert.equal(await page.locator(`[name=${k}]`).inputValue(),'false');
  failWrite=422;await submit();await page.getByText('Name rejected by server.',{exact:true}).waitFor();assert.equal(await page.locator('[name=Name]').inputValue(),'New capture policy');failWrite=409;await submit();await page.getByText('Duplicate policy name.',{exact:true}).waitFor();failWrite=0;delayWrite=true;await submit();await page.locator('.policy-modal [type=submit]:disabled').waitFor();await page.keyboard.press('Escape');assert(await page.locator('.policy-modal').isVisible());finishWrite();await closed();await loaded();delayWrite=false;
  assert(writes.every(w=>w.csrf==='test-csrf'));assert.equal(writes.at(-1).body.ISPID,1);assert.equal(writes.at(-1).body.SkipPrivate,false);
  await page.locator('#policy-search').fill('New capture policy');await open(30,'edit');assert(!(await page.locator('[name=Name]').evaluate(n=>n.readOnly)));await page.locator('[name=Name]').fill('Renamed preset');await page.locator('[name=SkipZero]').selectOption('true');await submit();await closed();await loaded();assert.equal(items.find(p=>p.ID===30).Name,'Renamed preset');await page.locator('#policy-search').fill('Renamed preset');
  await open(30,'view');await page.locator('#policy-devices').click();await page.waitForFunction(()=>document.querySelector('#device-search')?.value==='Renamed preset');assert.equal(new URL(page.url()).hash,'#/devices');if(director)assert.equal(await page.locator('#device-isp-filter').inputValue(),'1');await page.goBack();await loaded();assert.equal(new URL(page.url()).hash,'#/capture-policies');
  await page.locator('#policy-search').fill('Renamed preset');await open(30,'edit');failWrite=500;await submit();await page.getByText(/The outcome is uncertain/).waitFor();assert(await page.locator('.policy-modal [type=submit]').isDisabled());await page.keyboard.press('Escape');failWrite=0;
  await open(30,'delete');const delCount=writes.length;await submit();assert.equal(writes.length,delCount);await page.locator('[name=Name]').fill('Renamed preset');await submit();await closed();await loaded();assert(!items.some(p=>p.ID===30));
  await page.locator('#policy-search').fill('');failRead=true;await refresh();assert(await page.getByText(/Showing the last loaded list/).isVisible());assert(await page.locator('#policy-create').isDisabled());failRead=false;await refresh();
  items.find(p=>p.ID===4).Name='Preset " onfocus="window.injected=1';await refresh();await page.locator('#policy-search').fill('Preset "');await open(4,'edit');await page.locator('[name=Name]').focus();assert.equal(await page.evaluate(()=>window.injected),undefined);await page.keyboard.press('Escape');await page.locator('#policy-search').fill('');
  if(process.env.POLICY_SHOTS_DIR){fs.mkdirSync(process.env.POLICY_SHOTS_DIR,{recursive:true});await page.setViewportSize({width:1440,height:1100});await page.locator('.content-scroll').evaluate(n=>n.scrollTop=0);await page.screenshot({path:path.join(process.env.POLICY_SHOTS_DIR,`${director?'director':'isp'}-index.png`)})}
  await page.locator('#policy-create').click();await page.setViewportSize({width:390,height:900});assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);assert(await page.locator('.policy-modal .isp-modal-foot').evaluate(n=>n.getBoundingClientRect().bottom<=innerHeight));assert.equal(await page.locator('.policy-modal .btn').first().evaluate(n=>getComputedStyle(n).borderRadius),'0px');
  if(process.env.POLICY_SHOTS_DIR)await page.screenshot({path:path.join(process.env.POLICY_SHOTS_DIR,`${director?'director':'isp'}-mobile.png`)});
  assert.deepEqual(errors,[]);await page.close();
 }
 console.log('Policy checks passed: Director/ISP scopes, five cards, modal CRUD, presets vs device rules, linked-delete guard, conflicts, validation, CSRF, routing and mobile.');
 }finally{await browser.close()}
})().catch(e=>{console.error(e);console.error('::error title=Policies browser checks::'+String(e).replace(/\n/g,'%0A'));process.exitCode=1});

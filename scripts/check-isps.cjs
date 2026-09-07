// Isolated ISP CRUD browser checks; requests never reach production.
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const {chromium}=require('../tests/e2e/node_modules/@playwright/test');
const root=path.resolve(process.argv[2]||'internal/director/web/console');
const mime={'.js':'application/javascript','.css':'text/css','.html':'text/html','.woff2':'font/woff2'};
(async()=>{
 const browser=await chromium.launch({args:['--no-sandbox']});
 try{
  const page=await browser.newPage({viewport:{width:1440,height:1100}});const errors=[],writes=[];let fail=false,next=20;
  const items=Array.from({length:12},(_,n)=>({ID:n+1,Name:`ISP ${String(n+1).padStart(2,'0')}`,Username:`isp${n+1}`,Email:`isp${n+1}@example.invalid`,Phone:'+91 9876543210',Enabled:n%3!==0,AdminUserID:n+100,Version:1,CreatedAt:'2026-09-01T10:00:00Z',DeviceCount:n?2:0,UserCount:1}));
  page.on('pageerror',e=>errors.push(e.message));
  await page.route('http://isps.test/**',async r=>{
   const url=new URL(r.request().url());const method=r.request().method();
   const reply=(status,data)=>r.fulfill({status,contentType:'application/json',body:JSON.stringify(data)});
   if(!url.pathname.startsWith('/api/')){const relative=url.pathname==='/'?'index.html':url.pathname.replace(/^\/_ui\/[a-f0-9]{40}\//,'/').slice(1);const file=path.resolve(root,relative);if(!file.startsWith(root+path.sep)||!fs.existsSync(file))return r.fulfill({status:404});return r.fulfill({contentType:mime[path.extname(file)]||'application/octet-stream',body:fs.readFileSync(file)})}
   if(url.pathname==='/api/v1/me')return reply(200,{email:'director@example.invalid',isDirector:true,ispId:0,role:'director',csrf:'test-csrf'});
   if(!url.pathname.startsWith('/api/v1/isps'))return reply(200,{});
   if(process.env.ISP_TEST_DELAY_MS)await new Promise(resolve=>setTimeout(resolve,Number(process.env.ISP_TEST_DELAY_MS)));
   const id=Number(url.pathname.split('/')[4]);
   if(method==='GET'){if(fail)return reply(503,{error:'Service temporarily unavailable'});return reply(200,id?items.find(i=>i.ID===id):{isps:items})}
   const b=r.request().postDataJSON();writes.push({method,body:b,csrf:r.request().headers()['x-csrf-token']});
   if(b.Email==='duplicate@example.invalid')return reply(409,{error:'Email already exists.'});
   if(b.Phone==='1234567')return reply(422,{error:'Check the highlighted fields.',fields:{Phone:'Phone rejected by server.'}});
   if(method==='POST'){const i={...b,ID:next++,AdminUserID:200,Version:1,CreatedAt:new Date().toISOString(),DeviceCount:0,UserCount:1};delete i.Password;delete i.ConfirmPassword;items.push(i);return reply(200,i)}
   const at=items.findIndex(i=>i.ID===id);if(method==='PUT'){items[at]={...items[at],...b,Version:items[at].Version+1};return reply(200,items[at])}
   items.splice(at,1);return reply(200,{ok:true});
  });
  await page.goto('http://isps.test/');await page.locator('#menu .menua').filter({hasText:'ISPs'}).click();
  await page.waitForFunction(()=>document.querySelector('.index-stats .v')?.textContent==='12');
  assert.equal(await page.locator('.index-stats .tile').count(),5);
  for(const width of [1280,1440,1920]){await page.setViewportSize({width,height:1100});const rows=await page.locator('.index-stats .tile').evaluateAll(ns=>new Set(ns.map(n=>n.offsetTop)).size);assert.equal(rows,1,'Five cards occupy one desktop row')}
  assert.equal(await page.locator('#isp-rows tr').count(),10);
  await page.locator('#isp-next').click();assert.equal(await page.locator('#isp-rows tr').count(),2);
  await page.locator('#isp-search').fill('isp1@example.invalid');assert.equal(await page.locator('#isp-rows tr').count(),1);
  await page.locator('#isp-search').fill('');await page.locator('#isp-status-filter').selectOption('disabled');assert.equal(await page.locator('#isp-rows tr').count(),4);
  await page.locator('#isp-status-filter').selectOption('all');await page.locator('#isp-create').click();
  await page.getByRole('button',{name:'Create ISP',exact:true}).last().click();assert.equal(writes.length,0);assert.equal(await page.locator('.isp-modal [aria-invalid=true]').count(),7);
  const data={Name:'New ISP',Username:'new.isp',Email:'new@example.invalid',Phone:'+91 9999999999',Password:'test-only-password',ConfirmPassword:'mismatch'};
  for(const [k,v]of Object.entries(data))await page.locator(`[name="${k}"]`).fill(v);
  await page.locator('[name="Enabled"]').selectOption('true');await page.locator('.isp-modal [type=submit]').click();assert.equal(writes.length,0);
  await page.locator('[name="ConfirmPassword"]').fill(data.Password);await page.locator('[name="Email"]').fill('duplicate@example.invalid');await page.locator('.isp-modal [type=submit]').click();await page.getByText('Email already exists.',{exact:true}).waitFor();assert.equal(await page.locator('[name="Name"]').inputValue(),'New ISP');
  await page.locator('[name="Email"]').fill(data.Email);await page.locator('[name="Phone"]').fill('1234567');await page.locator('.isp-modal [type=submit]').click();await page.getByText('Phone rejected by server.').waitFor();
  await page.locator('[name="Phone"]').fill(data.Phone);await page.locator('.isp-modal [type=submit]').click();await page.locator('.isp-modal').waitFor({state:'hidden'});
  assert(writes.every(w=>w.csrf==='test-csrf'));assert.equal(writes.at(-1).body.Enabled,true);
  await page.locator('#isp-search').fill('New ISP');await page.getByRole('button',{name:'Edit New ISP',exact:true}).click();
  assert.equal(await page.locator('[name="Password"]').inputValue(),'');assert.equal(await page.locator('[name="Username"]').inputValue(),'new.isp');
  await page.locator('[name="Name"]').fill('New ISP edited');await page.locator('[name="Enabled"]').selectOption('false');await page.locator('.isp-modal [type=submit]').click();await page.locator('.isp-modal').waitFor({state:'hidden'});
  assert.equal(writes.at(-1).method,'PUT');assert.equal(writes.at(-1).body.Password,'');assert.equal(writes.at(-1).body.Version,1);
  await page.getByRole('button',{name:'View New ISP edited',exact:true}).click();await page.getByRole('dialog').getByText('new.isp',{exact:true}).waitFor();await page.keyboard.press('Escape');
  await page.getByRole('button',{name:'Delete New ISP edited',exact:true}).click();const before=writes.length;await page.locator('.isp-modal [type=submit]').click();assert.equal(writes.length,before);await page.locator('#isp-delete-name').fill('New ISP edited');await page.locator('.isp-modal [type=submit]').click();await page.locator('.isp-modal').waitFor({state:'hidden'});assert.equal(writes.at(-1).method,'DELETE');
  await page.locator('#isp-search').fill('');fail=true;await page.locator('#isp-refresh').click();await page.locator('#isp-banner').waitFor();assert.equal(await page.locator('.index-stats .v').first().innerText(),'12');fail=false;await page.locator('#isp-refresh').click();await page.locator('#isp-banner').waitFor({state:'hidden'});
  // User-controlled names must stay text in table attributes and form values.
  items[0].Name='ISP " onfocus="window.injected=1';await page.locator('#isp-refresh').click();await page.waitForFunction(()=>!document.querySelector('#isp-refresh').disabled);
  await page.locator('#isp-rows [data-action=edit]').first().click();await page.locator('.isp-modal').waitFor();await page.locator('[name=Name]').focus();assert.equal(await page.evaluate(()=>window.injected),undefined);await page.keyboard.press('Escape');
  const shots=process.env.ISP_SHOTS_DIR;if(shots){fs.mkdirSync(shots,{recursive:true});await page.setViewportSize({width:1440,height:1100});await page.screenshot({path:path.join(shots,'index.png')})}
  await page.locator('#isp-create').click();if(shots)await page.screenshot({path:path.join(shots,'create.png')});
  await page.setViewportSize({width:390,height:900});assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);assert.equal(await page.locator('.isp-modal').evaluate(n=>n.getBoundingClientRect().width<=innerWidth),true);if(shots)await page.screenshot({path:path.join(shots,'mobile.png')});
  assert.deepEqual(errors,[]);console.log('ISP browser checks passed: five stats, filters/pagination, modal CRUD, required fields, AJAX/CSRF, password confirmation, server errors, escaping and mobile.');
 }finally{await browser.close()}
})().catch(e=>{console.error(e);console.error('::error title=ISP browser checks::'+String(e).replace(/\n/g,'%0A'));process.exitCode=1});

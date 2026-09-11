// Read-only deployed-console checks. Sessions are supplied in a protected file;
// credentials/cookies must never be printed or committed.
const fs=require('node:fs'),assert=require('node:assert/strict');
const {chromium}=require('../tests/e2e/node_modules/@playwright/test');
(async()=>{
 const origin=process.env.FLEET_ORIGIN,expected=process.env.EXPECTED_REVISION;
 if(!origin||!expected||!process.env.FLEET_SESSION_FILE)throw Error('Missing deployment check configuration');
 const sessions=JSON.parse(fs.readFileSync(process.env.FLEET_SESSION_FILE)),browser=await chromium.launch({args:['--no-sandbox']});
 try{for(const session of sessions){
  const context=await browser.newContext({viewport:{width:1440,height:1100}});
  await context.addCookies([{name:'nf_session',value:session.cookie,url:origin,httpOnly:true,sameSite:'Lax'}]);
  const page=await context.newPage(),errors=[],writes=[];page.setDefaultTimeout(90000);
  page.on('pageerror',e=>errors.push(e.name));
  await page.route(origin+'/api/**',r=>{if(r.request().method()!=='GET'){writes.push(new URL(r.request().url()).pathname);return r.abort()}return r.continue()});
  const revision=await context.request.get(origin+'/ui-version.json').then(r=>r.json());assert.equal(revision.revision,expected);
  const me=await context.request.get(origin+'/api/v1/me').then(r=>r.json());assert.equal(me.isDirector,session.role==='director');assert.equal(me.ispId,session.ispId);
  const pages=['dashboard',...(session.role==='director'?['isps','dataplanes']:[]),'devices','capture-policies','users','logs','reports','audit','retention','compliance',...(session.role==='director'?['settings']:[])];
  for(const name of pages){
   await page.goto(origin+'/#/'+name);await page.locator('#app').waitFor({state:'visible'});
   if(name==='dashboard'){await page.locator('.dash-kpis .tile').first().waitFor();assert.equal(await page.locator('.dash-kpis .tile').count(),10)}
   else {await page.locator('.index-stats .tile').first().waitFor();await page.waitForFunction(()=>document.querySelector('.index-stats .l')?.textContent!=='Loading…');assert.equal(await page.locator('.index-stats .tile').count(),5)}
   if(name==='logs')await page.locator('#logs-open-filters:enabled').waitFor();
   if(name==='users'){await page.locator('[data-action=create]:enabled').waitFor();await page.locator('[data-action=create]').click();await page.getByRole('dialog').waitFor();await page.getByRole('dialog').locator('[type=submit]').click();assert(await page.getByRole('dialog').locator('[aria-invalid=true]').count()>0);await page.keyboard.press('Escape')}
   if(name==='settings'){await page.locator('[data-section=dataplane]').waitFor();for(const section of ['dataplane','s3','notifications']){await page.locator('[data-section='+section+']').click();await page.getByRole('dialog').waitFor();if(section==='s3')assert.equal(await page.locator('[name=secretKey]').inputValue(),'');await page.keyboard.press('Escape')}}
   console.log(session.role+': '+name+' rendered');
  }
  for(const endpoint of ['/users','/devices','/policies','/audit','/retention','/compliance',...(session.role==='director'?['/isps','/dataplanes','/settings']:[])]){
   const response=await context.request.get(origin+'/api/v1'+endpoint);assert.equal(response.status(),200,endpoint+' must respond');const data=await response.json();
   if(endpoint==='/users'){assert.equal(data.editableUsers,true);assert(data.users.every(u=>typeof u.primary==='boolean'&&u.version?.length===64));if(session.role==='isp')assert(data.users.every(u=>u.ispId===session.ispId))}
   if(endpoint==='/devices'&&session.role==='isp')assert(data.devices.every(d=>d.ISPID===session.ispId));
   if(endpoint==='/settings')assert(data.versions?.dataplane?.length===64);
  }
  await page.setViewportSize({width:390,height:900});assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
  assert.deepEqual(writes,[]);assert.deepEqual(errors,[]);
  console.log(session.role+': deployed API, scope, five-card layout and read-only modal checks passed');
  await page.unrouteAll({behavior:'ignoreErrors'});await context.close();
 }}finally{await browser.close()}
})().catch(e=>{console.error(e.name+': fleet browser check failed. '+String(e.message).split('\n')[0]);process.exitCode=1});

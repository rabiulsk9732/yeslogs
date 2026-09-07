/* Dashboard v1: independently refreshed components, scoped to the signed-in role. */
window.YesLogsDashboard = (() => {
  'use strict';
  let active = null;
  const escape = value => String(value == null ? '' : value).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const number = value => Number.isFinite(Number(value)) && value !== null ? Number(value).toLocaleString('en-US') : '—';
  const compact = value => new Intl.NumberFormat('en', { notation:'compact', maximumFractionDigits:1 }).format(value);
  const bytes = value => { if (!Number.isFinite(value)) return '—'; const units=['B','KiB','MiB','GiB','TiB']; let i=0; while(value>=1024 && i<4){value/=1024;i++;} return `${value.toFixed(i?1:0)} ${units[i]}`; };
  const clock = value => { const d=new Date(value); return Number.isFinite(d.getTime()) && d.getFullYear()>2000 ? new Intl.DateTimeFormat('en-GB',{timeZone:'Asia/Kolkata',day:'2-digit',month:'short',hour:'2-digit',minute:'2-digit',second:'2-digit',hour12:false}).format(d) : 'Not observed'; };
  const badge = (text,tone='mut') => `<span class="pill ${tone}">${escape(text)}</span>`;
  const empty = text => `<div class="dash-empty"><span class="ms">info</span>${escape(text)}</div>`;
  const action = (text,view,isp='',device='') => `<button class="dash-link" data-go="${view}" data-isp="${escape(isp)}" data-device="${escape(device)}">${escape(text)}</button>`;
  const sourceNames = {overview:'Summary',devices:'Exporter health',data:'Record statistics',system:'Host resources',quality:'NAT quality / coverage'};
  const qualityNames = {ok:'NAT fields present',partial_port:'Ports incomplete',no_port:'Missing public port',no_nat_fields:'Missing NAT fields',no_translation:'No translations',silent:'No records in audit',disabled:'Disabled'};

  function dispose() {
    if (!active) return;
    active.controllers.forEach(controller=>controller.abort());
    active.root.removeEventListener('click',active.click);
    active.root.removeEventListener('input',active.input);
    active.root.removeEventListener('change',active.change);
    document.removeEventListener('visibilitychange',active.visibility);
    active=null;
  }

  function model(s) {
    const devices=s.entries.devices?.value;
    const list=devices?.devices?.filter(d=>s.user.isDirector || Number(d.ISPID)===Number(s.user.ispId)) || [];
    const enabled=list.filter(d=>d.Enabled);
    const health=devices?.health || {};
    const status=d=>!d.Enabled?'disabled':health[d.ID]?.status || 'unknown';
    const counts={online:0,silent:0,nodata:0,unknown:0};
    enabled.forEach(d=>{const key=status(d); counts[key in counts?key:'unknown']++;});
    const complete=Boolean(devices) && counts.unknown===0;
    const report=s.entries.quality?.value;
    const grades=new Map((report?.available?report.devices || []:[]).map(d=>[Number(d.storeId),d]));
    return {devices,list,enabled,health,status,counts,complete,grades,report,isps:s.user.isDirector?(devices?.isps || []):[]};
  }

  function cards(s,m) {
    const ov=s.entries.overview?.value?.cards || [];
    const data=s.entries.data?.value;
    const find=(items,label)=>items?.find(x=>x.label===label)?.value ?? null;
    const common=[
      {key:'enabled',label:'Enabled exporters',value:m.devices?number(m.enabled.length):null,sub:'Configuration state',icon:'router',color:'#2490d8',source:'devices',view:'Devices'},
      {key:'online',label:'Online exporters',value:m.complete?number(m.counts.online):null,sub:'Latest observed flow evidence',icon:'settings_input_antenna',color:'#12946a',source:'devices',view:'Devices'},
      {key:'silent',label:'Silent exporters',value:m.complete?number(m.counts.silent):null,sub:'Beyond configured silence threshold',icon:'power_off',color:m.counts.silent?'#c14343':'#5a6d84',source:'devices',view:'Devices'},
      {key:'nodata',label:'No recent evidence',value:m.complete?number(m.counts.nodata):null,sub:'Enabled · no evidence in 3 days',icon:'help',color:'#cf7f18',source:'devices',view:'Devices'},
      {key:'today',label:'Records stored today',value:find(ov,'Flows Stored Today'),sub:'Today · 00:00 onward · IST',icon:'database',color:'#12946a',source:'overview',view:'Logs'}
    ];
    if(s.user.isDirector) return [
      {key:'isps',label:'Registered ISPs',value:Array.isArray(m.devices?.isps)?number(m.isps.length):null,sub:m.devices?.isps?`${m.isps.filter(i=>i.Enabled).length} enabled · this installation`:'This installation',icon:'apartment',color:'#2490d8',source:'devices',view:'ISPs'},
      ...common,
      {key:'decoded',label:'Flows decoded',value:find(ov,'Flows Ingested'),sub:'Since collector process started',icon:'bolt',color:'#2490d8',source:'overview',view:'Dataplanes'},
      {key:'skipped',label:'Flows skipped',value:find(ov,'Flows Skipped'),sub:'Capture rules · since process start',icon:'filter_alt',color:'#5a6d84',source:'overview',view:'Capture Policies'},
      {key:'storage',label:'Hot data size',value:find(ov,'Hot Storage Used'),sub:'Flow table · compressed on disk',icon:'storage',color:'#1c9ad0',source:'overview',view:'Retention'},
      {key:'queue',label:'Queue pressure',value:find(ov,'Queue Pressure'),sub:'Current writer queue / capacity',icon:'speed',color:'#cf7f18',source:'overview',view:'Dataplanes'}
    ];
    return [
      {key:'registered',label:'Registered exporters',value:m.devices?number(m.list.length):null,sub:'Your ISP only',icon:'router',color:'#2490d8',source:'devices',view:'Devices'},
      ...common,
      {key:'records',label:'Records in window',value:find(data?.widgets,'NAT Flows (window)'),sub:'Since yesterday 00:00 · IST',icon:'table_rows',color:'#2490d8',source:'data',view:'Logs'},
      {key:'subscribers',label:'Subscriber IPs seen',value:find(data?.widgets,'Subscribers Seen'),sub:'Approx. distinct IPs · current window',icon:'group',color:'#1c9ad0',source:'data',view:'Logs'},
      {key:'traffic',label:'Logged traffic volume',value:find(data?.widgets,'Logged Volume'),sub:'Sum of flow bytes · current window',icon:'swap_calls',color:'#12946a',source:'data',view:'Logs'},
      {key:'public',label:'Public NAT IPs seen',value:find(data?.infoBoxes,'CGNAT Public IPs Seen'),sub:'Distinct addresses · current window',icon:'hub',color:'#cf7f18',source:'data',view:'Logs'}
    ];
  }

  function renderCards(s,m) {
    const items=cards(s,m);
    items.forEach(c=>{
      let node=s.root.querySelector(`[data-kpi="${c.key}"]`);
      if(!node){node=document.createElement('button');node.className='tile dash-kpi';node.dataset.kpi=c.key;s.root.querySelector('.dash-kpis').appendChild(node);}
      const entry=s.entries[c.source];
      const unavailable=c.value===null;
      const state=entry?.error?(entry.value?'Stale':'Unavailable'):(unavailable?'Not reported':'');
      const full=String(c.value??'—');
      const display=/^[\d,]+$/.test(full) && full.length>10?compact(Number(full.replace(/,/g,''))):full;
      node.dataset.go=c.view;node.style.setProperty('--tc',unavailable?'#5a6d84':c.color);
      node.setAttribute('aria-label',`${c.label}: ${full}. ${c.sub}. ${state}`);
      node.innerHTML=`<div class="glass"></div><span class="ms wm" aria-hidden="true">${c.icon}</span><div class="body"><div class="v" title="${escape(full)}">${escape(display)}</div><div class="l">${escape(c.label)}</div><div class="f"><span class="s">${escape(c.sub)}</span></div><span class="dash-source">${escape(state || 'View details →')}</span></div>`;
    });
  }

  function renderChart(s) {
    const data=s.entries.data?.value, body=s.root.querySelector('#dash-chart-body');
    if(!Array.isArray(data?.hourly) || data.hourly.length!==24){body.innerHTML=empty(s.entries.data?.error?'Record statistics are unavailable.':'Waiting for hourly record statistics…');return;}
    const values=data.hourly.map(n=>Math.max(0,Number(n)||0)),max=Math.max(1,...values),sum=values.reduce((a,b)=>a+b,0);
    const cols=values.map((n,i)=>`<div class="dash-bar" tabindex="0" role="img" aria-label="${i}:00 IST: ${n} records" title="${String(i).padStart(2,'0')}:00–${String(i).padStart(2,'0')}:59 IST · ${number(n)} records"><i style="height:${n?Math.max(1,n/max*100):0}%"></i></div>`).join('');
    body.innerHTML=`<span class="dash-chart-total">${number(sum)}</span> <span class="dash-muted">records in hourly buckets</span><div class="dash-chart"><div class="dash-axis"><span>${compact(max)}</span><span>${compact(max/2)}</span><span>0</span></div><div><div class="dash-bars">${cols}</div><div class="dash-chart-labels"><span>00:00</span><span>06:00</span><span>12:00</span><span>18:00</span><span>23:00</span></div></div></div><div class="dash-muted">Today · Asia/Kolkata (IST). Aggregated stored records may lag incoming flows.${sum===0?' No records reported in today’s hourly buckets.':''}</div>`;
  }

  const healthRow=(label,value,tone)=>`<div class="dash-health-row"><span>${escape(label)}</span><strong>${tone?badge(value,tone):escape(value)}</strong></div>`;
  function renderHealth(s,m) {
    const node=s.root.querySelector('#dash-health-body');
    if(s.user.isDirector){
      const sys=s.entries.system?.value;
      const queue=s.entries.overview?.value?.cards?.find(c=>c.label==='Queue Pressure');
      const disk=sys?.disk,total=Number(disk?.total),free=total-Number(disk?.used);
      const up=Number(sys?.process?.uptime);
      node.innerHTML=healthRow('Collector uptime',sys?`${Math.floor(up/86400)}d ${Math.floor(up%86400/3600)}h ${Math.floor(up%3600/60)}m`:'Unknown')+
        healthRow('Writer queue',queue?.value || 'Unknown')+
        healthRow('Disk free',total>0?`${bytes(free)} / ${bytes(total)}`:'Unknown')+
        healthRow('Memory used',sys?.memory?.total>0?`${sys.memory.pct}%`:'Unknown')+
        healthRow('CPU load / cores',sys?.cpu?`${sys.cpu.load1.toFixed(2)} / ${sys.cpu.cores}`:'Unknown')+
        `<p class="dash-muted">Installation-wide resource snapshot. Device silence and data-quality findings are listed below.</p>`;
    }else{
      const times=m.enabled.map(d=>m.health[d.ID]?.lastSeen).filter(t=>t && new Date(t).getFullYear()>2000).sort((a,b)=>new Date(b)-new Date(a));
      node.innerHTML=healthRow('Enabled exporters',m.devices?number(m.enabled.length):'Unknown')+
        healthRow('Online / enabled',m.complete?`${m.counts.online} / ${m.enabled.length}`:'Unknown')+
        healthRow('Latest flow evidence',times.length?clock(times[0])+' IST':'Not observed')+
        healthRow('Unmeasured health',m.devices?number(m.counts.unknown):'Unknown')+
        `<p class="dash-muted">Only your ISP. Liveness uses the configured silence threshold and observed flow evidence; it is not a packet-loss measurement.</p>`;
    }
  }

  function issues(s,m) {
    const list=[];
    m.enabled.forEach(d=>{
      const status=m.status(d),q=m.grades.get(Number(d.ID));
      const reasons=[];
      if(status==='silent')reasons.push('No recent flow evidence beyond the configured silence threshold.');
      if(status==='nodata')reasons.push('No flow evidence observed in the last three days.');
      if(status==='unknown')reasons.push('Exporter health could not be measured.');
      if(q && !['ok','disabled'].includes(q.state))reasons.push(qualityNames[q.state] || 'NAT quality needs review');
      if(reasons.length)list.push({title:d.Name || `Device ${d.DeviceID}`,detail:reasons.join(' '),view:q && q.state!=='ok'?'Compliance':'Logs',isp:d.ISPID,device:d.DeviceID,critical:status==='silent' || Boolean(q && !['ok','partial_port','disabled'].includes(q.state))});
    });
    if(m.report?.available && m.report.missingDays?.length)list.push({title:`${m.report.missingDays.length} dates without records`,detail:'Coverage audit found dates with no retained records. Review the affected dates.',view:'Compliance',critical:true});
    if(s.user.isDirector){
      const sys=s.entries.system?.value;
      if(sys?.disk?.total>0 && sys.disk.pct>=85)list.push({title:'Disk capacity needs attention',detail:`${sys.disk.pct}% of the data filesystem is used.`,view:'Dataplanes',critical:sys.disk.pct>=95});
      const queue=s.entries.overview?.value?.cards?.find(c=>c.label==='Queue Pressure');
      if(Number(queue?.pct)>=60)list.push({title:'Writer queue pressure',detail:`${queue.value} of the writer queue is occupied.`,view:'Dataplanes',critical:Number(queue.pct)>=85});
    }
    return list.sort((a,b)=>Number(b.critical)-Number(a.critical));
  }

  function renderAttention(s,m) {
    const items=issues(s,m),body=s.root.querySelector('#dash-attention-body');
    s.root.querySelector('#dash-attention-count').textContent=items.length?`${items.length} findings`:'Current findings';
    if(!m.devices){body.innerHTML=empty('Exporter status is not available yet.');return;}
    body.innerHTML=items.length?`<ul class="dash-attention">${items.map(x=>`<li class="${x.critical?'critical':''}"><span class="ms">${x.critical?'error':'warning'}</span><div><b>${escape(x.title)}</b><p>${escape(x.detail)}</p>${action('Review →',x.view,x.isp,x.device)}</div></li>`).join('')}</ul>`:empty(m.list.length?'No device issues in the available observations. Review data quality below.':'Add an exporter to start collecting records.');
  }

  function renderQuality(s,m) {
    const body=s.root.querySelector('#dash-quality-body'),rep=m.report;
    if(!rep?.available){body.innerHTML=empty(s.entries.quality?.error?'Quality audit is unavailable. Retry or open the full audit.':'Loading the latest NAT quality audit…')+action('Open data-quality audit →','Compliance');return;}
    // Match by database ID, not DeviceID (which can repeat across ISPs).
    const assessed=m.enabled.map(d=>m.grades.get(Number(d.ID))).filter(Boolean);
    const good=assessed.filter(d=>d.state==='ok').length;
    const partial=assessed.filter(d=>d.state==='partial_port').length;
    const unknown=m.enabled.length-assessed.length;
    body.innerHTML=`<div class="dash-quality"><div><b>${good}</b><span>NAT fields present</span></div><div><b>${partial}</b><span>Ports incomplete</span></div><div><b>${Math.max(0,assessed.length-good-partial)}</b><span>Need review</span></div></div><div class="dash-muted">${unknown?`${unknown} enabled device(s) not graded. `:''}${assessed.length===0?'No enabled devices were graded. ':''}Technical NAT field quality · audit ${escape(clock(rep.generatedAt))} IST.<br>${Array.isArray(rep.days)?`${rep.days.filter(d=>Number(d.flows)>0).length} observed dates with records. `:''}${Array.isArray(rep.missingDays)?`${rep.missingDays.length} missing dates in the audited window.`:'Coverage has not been reported.'}</div><div style="margin-top:12px">${action('Review devices and coverage →','Compliance')}</div>`;
  }

  function renderStatus(s,m) {
    const search=s.filter.toLowerCase(),rows=[];
    if(s.user.isDirector){
      m.isps.forEach(isp=>{
        const devs=m.list.filter(d=>Number(d.ISPID)===Number(isp.ID)),enabled=devs.filter(d=>d.Enabled);
        const counts={online:0,silent:0,nodata:0,unknown:0};enabled.forEach(d=>{const k=m.status(d);counts[k in counts?k:'unknown']++;});
        const qualityBad=enabled.filter(d=>{const q=m.grades.get(Number(d.ID));return q && !['ok','disabled'].includes(q.state);}).length;
        const last=enabled.map(d=>m.health[d.ID]?.lastSeen).filter(t=>new Date(t).getFullYear()>2000).sort((a,b)=>new Date(b)-new Date(a))[0];
        if(search && !`${isp.Name} ${isp.ID}`.toLowerCase().includes(search))return;
        if(s.onlyIssues && !counts.silent && !counts.nodata && !counts.unknown && !qualityBad)return;
        const observations=[counts.silent?badge(counts.silent+' silent','bad'):'',counts.nodata?badge(counts.nodata+' no data','warn'):'',counts.unknown?badge('Unmeasured','mut'):'',qualityBad?badge(qualityBad+' NAT issues','warn'):''].filter(Boolean).join(' ');
        rows.push(`<tr><td>${escape(isp.Name)}<div class="dash-muted">ISP #${escape(isp.ID)}</div></td><td>${badge(isp.Enabled?'Enabled':'Disabled',isp.Enabled?'info':'mut')}</td><td>${enabled.length} / ${devs.length}</td><td>${counts.unknown?'—':counts.online}</td><td>${observations || badge(enabled.length?'Recent evidence':'No enabled devices',enabled.length?'ok':'mut')}</td><td class="mono">${escape(clock(last))}</td><td>${action('Search logs','Logs',isp.ID)}</td></tr>`);
      });
    }else m.list.forEach(d=>{
      const status=m.status(d),q=m.grades.get(Number(d.ID));
      if(search && !`${d.Name} ${d.ExporterIP} ${d.DeviceID}`.toLowerCase().includes(search))return;
      if(s.onlyIssues && ['online','disabled'].includes(status) && (!q || q.state==='ok'))return;
      rows.push(`<tr><td>${escape(d.Name)}<div class="dash-muted">Device #${escape(d.DeviceID)}</div></td><td class="mono">${escape(d.ExporterIP)}</td><td>${escape(d.Protocol)}</td><td>${badge(status==='nodata'?'No evidence':status,status==='online'?'ok':status==='silent'?'bad':'mut')}</td><td class="mono">${escape(clock(m.health[d.ID]?.lastSeen))}</td><td>${escape(q?qualityNames[q.state] || q.state:'Not graded')}</td><td>${action('Search logs','Logs','',d.DeviceID)}</td></tr>`);
    });
    const pages=Math.max(1,Math.ceil(rows.length/10));s.page=Math.min(s.page,pages);
    s.root.querySelector('#dash-status-rows').innerHTML=rows.slice((s.page-1)*10,s.page*10).join('') || `<tr><td colspan="7">${empty(!m.devices?'Waiting for device inventory…':s.user.isDirector && !Array.isArray(m.devices.isps)?'ISP inventory was not reported.':search || s.onlyIssues?'No matches for these filters.':s.user.isDirector?'No ISPs registered yet.':'No exporters registered for your ISP.')}</td></tr>`;
    s.root.querySelector('#dash-page-label').textContent=`${rows.length} ${s.user.isDirector?'ISPs':'devices'} · page ${s.page} of ${pages}`;
    s.root.querySelector('#dash-prev').disabled=s.page<=1;s.root.querySelector('#dash-next').disabled=s.page>=pages;
  }

  function renderRecent(s,m) {
    const data=s.entries.data?.value,rows=(data?.records || []).slice(0,10);
    const body=s.root.querySelector('#dash-recent-rows');
    if(!rows.length){body.innerHTML=`<tr><td colspan="${s.user.isDirector?7:6}">${empty(!data?'Recent records are unavailable.':'No recent stored records in the current window.')}</td></tr>`;return;}
    body.innerHTML=rows.map(r=>{
      const candidates=m.list.filter(d=>Number(d.DeviceID)===Number(r.devId));
      const dev=candidates.length===1?candidates[0]:null;
      const isp=dev?m.isps.find(i=>Number(i.ID)===Number(dev.ISPID)):null;
      const pub=!r.pubIp?'Not reported':`${r.pubIp}:${r.pubPort??'—'}${r.untranslated?' (unchanged)':''}`;
      return `<tr><td class="mono">${escape(r.time || `${r.date} ${r.clock}`)}</td>${s.user.isDirector?`<td>${escape(isp?.Name || 'Not reported')}</td>`:''}<td>${escape(r.sub || r.devId)}</td><td class="mono">${escape(r.privIp)}:${escape(r.privPort)}</td><td class="mono">${escape(pub)}</td><td>${badge(r.proto,'info')}</td><td class="mono">${escape(r.dest)}</td></tr>`;
    }).join('');
  }

  function render(s) {
    if(active!==s || !s.root.isConnected)return;
    const focused=document.activeElement;
    const focusKey=s.root.contains(focused) && focused.matches('[data-go]')?{...focused.dataset}:null;
    const m=model(s);
    renderCards(s,m);renderChart(s);renderHealth(s,m);renderAttention(s,m);renderQuality(s,m);renderStatus(s,m);renderRecent(s,m);
    const failed=Object.keys(s.entries).filter(k=>s.entries[k].error);
    const banner=s.root.querySelector('#dash-errors');banner.hidden=!failed.length;
    banner.textContent=`Could not refresh: ${failed.map(k=>sourceNames[k]).join(', ')}. Last successful values, where available, are retained. Use Refresh to retry.`;
    const dates=Object.values(s.entries).map(e=>e.at || 0).filter(Boolean);
    s.root.querySelector('#dash-updated').textContent=`${s.paused?'Auto-refresh paused':'Auto-refresh 15s'} · ${dates.length?'Last response '+clock(Math.max(...dates))+' IST':'Waiting for data'}${failed.length?' · Partial / stale data':''}`;
    ['chart','health','quality','recent'].forEach((id,i)=>{
      const keys=[['data'],s.user.isDirector?['system','overview']:['devices'],['quality'],['data']][i];
      const el=s.root.querySelector(`#dash-${id}-status`),errors=keys.some(k=>s.entries[k]?.error);
      const at=Math.min(...keys.map(k=>s.entries[k]?.at || 0));
      el.textContent=errors?'Unavailable / stale':at?'Updated '+clock(at)+' IST':'Loading';
    });
    const button=s.root.querySelector('#dash-refresh');button.disabled=s.busy;button.setAttribute('aria-busy',String(s.busy));
    if(focusKey && !focused.isConnected){
      const replacement=Array.from(s.root.querySelectorAll('[data-go]')).find(n=>n.dataset.go===focusKey.go && n.dataset.isp===focusKey.isp && n.dataset.device===focusKey.device);
      replacement?.focus({preventScroll:true});
    }
  }

  async function request(s,key,url) {
    const controller=new AbortController();s.controllers.add(controller);
    const timer=setTimeout(()=>controller.abort(),30000);
    try{
      const response=await fetch(url,{signal:controller.signal,headers:{'Accept':'application/json'}});
      if(active!==s)return;
      if(response.status===401){dispose();s.expired();return;}
      if(!response.ok)throw new Error('Request failed');
      const value=await response.json();
      if(key==='quality' && value.available!==true)throw new Error('Audit unavailable');
      if(key==='devices' && !Array.isArray(value.devices))throw new Error('Inventory unavailable');
      if(key==='data' && !Array.isArray(value.widgets))throw new Error('Flow store unavailable');
      if(key==='overview' && !Array.isArray(value.cards))throw new Error('Summary unavailable');
      if(active!==s)return;
      s.entries[key]={value,at:Date.now(),error:false};
    }catch(error){if(active===s)s.entries[key]={...s.entries[key],error:true};}
    finally{clearTimeout(timer);s.controllers.delete(controller);render(s);}
  }

  async function refresh(force=false) {
    const s=active;
    if(!s || s.busy || (!force && (s.paused || document.hidden)))return;
    s.busy=true;render(s);
    const calls=[request(s,'overview','/api/v1/overview'),request(s,'devices','/api/v1/devices')];
    if(force || !s.entries.data?.at || Date.now()-s.entries.data.at>=60000)calls.push(request(s,'data','/api/v1/console/data?days=1'));
    if(s.user.isDirector && (force || !s.entries.system?.at || Date.now()-s.entries.system.at>=60000))calls.push(request(s,'system','/api/v1/system'));
    // The quality/coverage audit is expensive and is refreshed separately.
    if(!s.qualityBusy && (!s.qualityAttempt || Date.now()-s.qualityAttempt>=300000 || (force && s.entries.quality?.error))){
      s.qualityBusy=true;s.qualityAttempt=Date.now();
      request(s,'quality','/api/v1/compliance').finally(()=>{s.qualityBusy=false;});
    }
    await Promise.allSettled(calls);
    s.busy=false;render(s);
  }

  function mount(root,options) {
    dispose();
    const s={root,...options,entries:{},controllers:new Set(),busy:false,paused:false,filter:'',onlyIssues:false,page:1};active=s;
    const director=s.user.isDirector;
    root.innerHTML=`<section class="dashboard" aria-label="${director?'Director':'ISP'} dashboard">
      <div class="dash-toolbar"><div class="dash-scope"><strong>${director?'All ISPs · this installation':'Your ISP · #'+escape(s.user.ispId)}</strong><span class="dash-updated" id="dash-updated" role="status">Waiting for data</span></div><div class="dash-actions">
      <button class="btn out sm" data-go="Logs"><span class="ms">search</span>Search logs</button><button class="btn out sm" data-go="Reports"><span class="ms">summarize</span>Reports</button>${director?'<button class="btn out sm" data-go="ISPs" data-add="true"><span class="ms">add</span>Add ISP</button>':''}<button class="btn out sm" data-go="Devices" data-add="true"><span class="ms">add</span>Add device</button>
      <button class="btn out sm" id="dash-pause" aria-pressed="false"><span class="ms">pause</span>Pause</button><button class="btn sm" id="dash-refresh"><span class="ms">refresh</span>Refresh</button></div></div>
      <div id="dash-errors" class="dash-banner" role="status" hidden></div><div class="dash-kpis" aria-label="Dashboard summary"></div>
      <div class="dash-panels"><section class="card"><div class="ch"><span class="ms">show_chart</span><h2>Records stored by hour</h2><span class="ch-tag">Today · IST</span><span class="dash-muted dash-status" id="dash-chart-status"></span></div><div class="dash-panel-body" id="dash-chart-body"></div></section>
      <section class="card"><div class="ch"><span class="ms">monitor_heart</span><h2>${director?'Collector resources':'Collection health'}</h2><span class="dash-muted dash-status" id="dash-health-status"></span></div><div class="dash-panel-body" id="dash-health-body"></div></section></div>
      <div class="dash-panels"><section class="card"><div class="ch"><span class="ms">notifications</span><h2>Needs attention</h2><span class="ch-tag" id="dash-attention-count"></span></div><div class="dash-panel-body" id="dash-attention-body"></div></section>
      <section class="card"><div class="ch"><span class="ms">verified_user</span><h2>NAT data quality & coverage</h2><span class="dash-muted dash-status" id="dash-quality-status"></span></div><div class="dash-panel-body" id="dash-quality-body"></div></section></div>
      <section class="card dash-wide" style="margin-bottom:18px"><div class="ch"><span class="ms">${director?'apartment':'router'}</span><h2>${director?'ISP collection status':'Exporter status'}</h2><div class="dash-table-tools"><input id="dash-filter" placeholder="${director?'Find an ISP…':'Find a device…'}" aria-label="Filter dashboard status"><select id="dash-status-filter" aria-label="Filter by attention"><option value="all">All ${director?'ISPs':'devices'}</option><option value="issues">Needs attention</option></select></div></div>
      <div class="dash-table-scroll"><table class="dash-table"><thead><tr>${(director?['ISP','Account','Enabled / total','Online','Observation','Latest evidence · IST','Action']:['Device','Exporter IP','Protocol','Observation','Latest evidence · IST','NAT quality','Action']).map(t=>`<th>${t}</th>`).join('')}</tr></thead><tbody id="dash-status-rows"></tbody></table></div><div class="dash-pagination"><span id="dash-page-label"></span><div class="dash-actions"><button class="btn out sm" id="dash-prev">Previous</button><button class="btn out sm" id="dash-next">Next</button></div></div></section>
      <section class="card dash-wide"><div class="ch"><span class="ms">table_rows</span><h2>Recent stored records</h2><span class="ch-tag">Latest 10 · since yesterday 00:00 IST</span><span class="dash-muted dash-status" id="dash-recent-status"></span>${action('Open Logs →','Logs')}</div><div class="dash-table-scroll"><table class="dash-table"><thead><tr><th>Record timestamp · IST</th>${director?'<th>ISP</th>':''}<th>Device</th><th>Private IP:port</th><th>Public IP:port</th><th>Protocol</th><th>Destination</th></tr></thead><tbody id="dash-recent-rows"></tbody></table></div></section>
    </section>`;
    s.click=event=>{
      const target=event.target.closest('button');if(!target || !root.contains(target))return;
      if(target.dataset.go){s.navigate(target.dataset.go,{isp:target.dataset.isp,device:target.dataset.device,add:target.dataset.add==='true'});return;}
      if(target.id==='dash-refresh')refresh(true);
      if(target.id==='dash-pause'){s.paused=!s.paused;target.setAttribute('aria-pressed',String(s.paused));target.innerHTML=`<span class="ms">${s.paused?'play_arrow':'pause'}</span>${s.paused?'Resume':'Pause'}`;render(s);if(!s.paused)refresh();}
      if(target.id==='dash-prev' || target.id==='dash-next'){s.page+=target.id==='dash-prev'?-1:1;renderStatus(s,model(s));}
    };
    s.input=event=>{if(event.target.id==='dash-filter'){s.filter=event.target.value;s.page=1;renderStatus(s,model(s));}};
    s.change=event=>{if(event.target.id==='dash-status-filter'){s.onlyIssues=event.target.value==='issues';s.page=1;renderStatus(s,model(s));}};
    s.visibility=()=>{if(!document.hidden)refresh();};
    root.addEventListener('click',s.click);root.addEventListener('input',s.input);root.addEventListener('change',s.change);document.addEventListener('visibilitychange',s.visibility);
    render(s);refresh();
  }
  return {mount,refresh,dispose};
})();

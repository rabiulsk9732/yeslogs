/* Exporter registration UI. All writes use the tenant-scoped Devices API. */
(function($){
 'use strict';
 let active=null;
 const esc=v=>$('<span>').text(v==null?'':v).html().replace(/"/g,'&quot;').replace(/'/g,'&#39;');
 const icon=v=>`<span class="ms" aria-hidden="true">${v}</span>`;
 const pill=(v,c)=>`<span class="pill ${c}">${esc(v)}</span>`;
 const protocols={auto:'Auto detect',netflow5:'NetFlow v5',netflow9:'NetFlow v9',ipfix:'IPFIX'};
 const profiles={generic:'Generic',mikrotik:'MikroTik',cisco:'Cisco',juniper:'Juniper',huawei:'Huawei'};
 const rules={SkipDNS:'Skip DNS',SkipPrivate:'Skip private → private',SkipZero:'Skip zero-byte records'};
 const labels={ISPID:['ISP','apartment'],Name:['Device name','router'],ExporterIP:['Exporter IP','dns'],DeviceID:['Device ID','tag'],Protocol:['Protocol','swap_calls'],Profile:['Vendor profile','memory'],CapturePolicy:['Capture policy','shield'],Enabled:['Status','toggle_on'],Confirm:['Type the device name to confirm','router']};
 const editable=['ISPID','Name','ExporterIP','DeviceID','Protocol','Profile','CapturePolicy','Enabled',...Object.keys(rules)];
 const date=v=>{const d=new Date(v);return Number.isFinite(+d)&&d.getFullYear()>2000?new Intl.DateTimeFormat('en-GB',{timeZone:'Asia/Kolkata',dateStyle:'medium',timeStyle:'short'}).format(d):'—'};
 const alive=c=>active===c&&c.root.isConnected;
 const ispName=(c,id)=>c.isps.find(i=>i.ID===id)?.Name||`ISP #${id}`;
 const health=(c,i)=>!i.Enabled?'disabled':['online','silent','nodata'].includes(c.health[i.ID]?.status)?c.health[i.ID].status:'unknown';
 const healthNames={online:'Online',silent:'Silent',nodata:'No recent data',unknown:'Unavailable',disabled:'Disabled'};
 const healthColor={online:'ok',silent:'bad',nodata:'warn',unknown:'mut',disabled:'mut'};
 function ajax(c,url,method='GET',body){
  const r=$.ajax({url,method,dataType:'json',contentType:'application/json',headers:{'X-CSRF-Token':c.csrf},data:body===undefined?undefined:JSON.stringify(body),timeout:30000});
  if(method==='GET'){c.reads.add(r);r.always(()=>c.reads.delete(r))}return r;
 }
 function message(e){return e.message||e.responseJSON?.error||(e.status===0?'Connection interrupted.':'Request failed. Please try again.');}
 function banner(c,text){$(c.root).find('#device-banner').text(text).prop('hidden',!text)}
 function scoped(c){return c.items.filter(i=>!c.isp||String(i.ISPID)===c.isp)}
 function render(c){
  if(!alive(c))return;
  const scope=scoped(c),enabled=scope.filter(i=>i.Enabled),unknown=enabled.filter(i=>health(c,i)==='unknown').length;
  const counts=k=>enabled.filter(i=>health(c,i)===k).length;
  const stats=[['Total devices',scope.length,`${scope.filter(i=>!i.Enabled).length} disabled`,'router','var(--pri)'],['Enabled',enabled.length,'Collection enabled','toggle_on','var(--cyan)'],['Online',unknown?null:counts('online'),unknown?`${counts('online')} known · ${unknown} unavailable`:'Recent flow evidence','monitor_heart','var(--ok)'],['Silent',unknown?null:counts('silent'),unknown?`${counts('silent')} known · ${unknown} unavailable`:'Past silence threshold','schedule','var(--bad)'],['No recent data',unknown?null:counts('nodata'),unknown?`${counts('nodata')} known · ${unknown} unavailable`:'No evidence in health window','database','var(--warn)']];
  $(c.root).find('.index-stats').html(stats.map(([label,value,sub,symbol,color])=>`<div class="tile" style="--tc:${color}"><div class="glass"></div><span class="ms wm" aria-hidden="true">${symbol}</span><div class="body"><div class="v">${c.loaded&&value!==null?value.toLocaleString('en-IN'):'—'}</div><div class="l">${label}</div><div class="f">${c.loaded?esc(sub):'Waiting for devices'}</div></div></div>`).join(''));
  $(c.root).find('#device-scope').text(c.director?(c.isp?ispName(c,+c.isp):'All ISPs'):'Your ISP');
  $(c.root).find('#device-health-note').text('Health uses stored flow evidence from the last 3 days and collector signals; it is not a packet counter. Disabled devices are excluded.'+(unknown?` Health is unavailable for ${unknown} enabled device(s).`:''));
  const list=scope.filter(i=>(c.filter==='all'||c.filter==='enabled'&&i.Enabled||c.filter===health(c,i))&&(!c.protocol||i.Protocol===c.protocol)&&[i.Name,i.ExporterIP,i.DeviceID,i.CapturePolicy,ispName(c,i.ISPID)].join(' ').toLowerCase().includes(c.query.toLowerCase()));
  const pages=Math.max(1,Math.ceil(list.length/c.limit));c.page=Math.min(c.page,pages);
  $(c.root).find('#device-rows').html(list.length?list.slice((c.page-1)*c.limit,c.page*c.limit).map(i=>`<tr data-id="${i.ID}"><td><strong>${esc(i.Name||'Unnamed device')}</strong><span class="isp-secondary mono">Device #${i.DeviceID}</span></td>${c.director?`<td>${esc(ispName(c,i.ISPID))}<span class="isp-secondary mono">ISP #${i.ISPID}</span></td>`:''}<td class="mono">${esc(i.ExporterIP)}</td><td>${pill(protocols[i.Protocol]||i.Protocol,'info')}<span class="isp-secondary">${esc(profiles[i.Profile]||i.Profile)}</span></td><td>${esc(i.CapturePolicy||'Custom rules')}<span class="isp-secondary">${esc(Object.keys(rules).filter(k=>i[k]).map(k=>rules[k].replace('Skip ','')).join(', ')||'No skip rules')}</span></td><td>${pill(i.Enabled?'Enabled':'Disabled',i.Enabled?'ok':'mut')}</td><td>${pill(healthNames[health(c,i)],healthColor[health(c,i)])}<span class="isp-secondary">${esc(date(c.health[i.ID]?.lastSeen))}</span></td><td><div class="isp-row-actions">${[['view','visibility','View'],['edit','edit','Edit'],['toggle',i.Enabled?'pause':'play_arrow',i.Enabled?'Disable':'Enable'],['delete','delete','Delete']].map(([action,symbol,title])=>`<button class="btn out sm ${action==='delete'?'device-delete':''}" data-action="${action}" title="${title} device" aria-label="${title} ${esc(i.Name)}">${icon(symbol)}</button>`).join('')}</div></td></tr>`).join(''):`<tr><td colspan="${c.director?8:7}"><div class="dash-empty">${c.loaded?(c.items.length?'No devices match these filters.':'No devices yet. Add an exporter to get started.'):'Device data is not available yet.'}</div></td></tr>`);
  $(c.root).find('#device-count').text(`${list.length?(c.page-1)*c.limit+1:0}–${Math.min(c.page*c.limit,list.length)} of ${list.length} devices`);
  $(c.root).find('#device-page').text(`${c.page} / ${pages}`);
  $(c.root).find('#device-prev').prop('disabled',c.page<=1);$(c.root).find('#device-next').prop('disabled',c.page>=pages);
  $(c.root).find('#open-add').prop('disabled',!c.loaded||!c.policiesReady||c.director&&!c.ispsReady);
 }
 async function refresh(c){
  if(c.loading)return false;c.loading=true;$(c.root).find('#device-refresh').prop('disabled',true);
  try{
   const results=await Promise.allSettled([ajax(c,'/api/v1/devices'),ajax(c,'/api/v1/policies')]);if(!alive(c))return false;
   if(results.some(r=>r.status==='rejected'&&r.reason.status===401)){c.expired();return false}
   const [devices,policies]=results;c.policiesReady=policies.status==='fulfilled'&&Array.isArray(policies.value.policies);
   c.policies=c.policiesReady?policies.value.policies:[];
   if(devices.status==='rejected')throw devices.reason;
   const data=devices.value;
   if(!Array.isArray(data.devices)||data.isDirector!==c.director)throw Error('Unexpected device response. Refresh to try again.');
   c.items=data.devices.filter(i=>c.director||i.ISPID===c.user.ispId).sort((a,b)=>(a.Name||'').localeCompare(b.Name||'')||a.ID-b.ID);c.health=data.health||{};c.isps=data.isps||[];c.ispsReady=Array.isArray(data.isps);c.loaded=true;
   if(c.director){const select=$(c.root).find('#device-isp-filter');select.html('<option value="">All ISPs</option>'+c.isps.map(i=>`<option value="${i.ID}">${esc(i.Name)}</option>`).join(''));if(c.isp&&!c.isps.some(i=>String(i.ID)===c.isp))c.isp='';select.val(c.isp)}
   banner(c,!c.policiesReady?'Devices loaded, but capture policies are unavailable. Refresh before creating or editing.':c.director&&!c.ispsReady?'ISP names are unavailable. Refresh before adding a device.':'');
   $(c.root).find('#device-updated').text('Updated '+date(new Date())+' IST');render(c);return true;
  }catch(e){if(alive(c)){banner(c,(c.loaded?'Showing the last loaded list. ':'')+message(e));render(c)}return false}
  finally{c.loading=false;if(alive(c))$(c.root).find('#device-refresh').prop('disabled',false)}
 }
 function close(c,force=false){if(c.saving&&!force)return;c.dialog?.remove();c.dialog=null;$(document).off('keydown.deviceDialog');if(c.returnFocus?.isConnected)c.returnFocus.focus()}
 function dialog(c,title,sub,body,footer=''){
  close(c);c.returnFocus=document.activeElement;
  const d=$(`<div class="isp-modal-bg device-modal-bg"><section class="isp-modal device-modal" role="dialog" aria-modal="true" aria-labelledby="device-modal-title"><div class="isp-modal-head">${icon('router')}<div><h2 id="device-modal-title">${esc(title)}</h2><p>${esc(sub)}</p></div><button type="button" class="icon-btn" data-close aria-label="Close dialog">${icon('close')}</button></div><form novalidate><div class="isp-modal-body">${body}<div class="isp-form-error" role="alert"></div></div><div class="isp-modal-foot"><button type="button" class="btn out" data-close>${footer?'Cancel':'Close'}</button>${footer}</div></form></section></div>`).appendTo('body');c.dialog=d;
  d.on('click','[data-close]',()=>close(c));d.on('submit',e=>e.preventDefault());
  $(document).on('keydown.deviceDialog',e=>{if(e.key==='Escape'){e.preventDefault();close(c)}if(e.key==='Tab'){const nodes=d.find('button:not(:disabled),input:not(:disabled),select:not(:disabled)').filter(':visible').toArray();const first=nodes[0],last=nodes.at(-1);if(e.shiftKey&&document.activeElement===first){e.preventDefault();last?.focus()}else if(!e.shiftKey&&document.activeElement===last){e.preventDefault();first?.focus()}}});
  d.find('input:not([readonly]),select:not(:disabled),button').filter(':visible').first().trigger('focus');return d;
 }
 function field(key,control,required=false,hint=''){
  const [label,symbol]=labels[key];return `<div class="isp-field"><label class="lbl" for="device-${key}">${icon(symbol)}<span>${label}</span>${required?'<span class="req" aria-hidden="true">*</span>':''}</label>${control}<div class="hint" id="device-${key}-hint">${esc(hint)}</div><div class="isp-field-error" id="device-${key}-error"></div></div>`;
 }
 const attrs=(key,required)=>`class="inp" id="device-${key}" name="${key}" ${required?'required':''} aria-describedby="device-${key}-hint device-${key}-error"`;
 const input=(key,value,placeholder,required=false,readonly=false)=>field(key,`<input ${attrs(key,required)} value="${esc(value||'')}" placeholder="${esc(placeholder)}" ${readonly?'readonly':''} maxlength="${key==='ExporterIP'?45:190}" autocomplete="off">`,required);
 function select(key,options,value,required=false,hint='',disabled=false){return field(key,`<select ${attrs(key,required)} ${disabled?'disabled':''}>${Object.entries(options).map(([v,label])=>`<option value="${esc(v)}" ${String(value)===v?'selected':''}>${esc(label)}</option>`).join('')}</select>`,required,hint)}
 function available(c,isp){
  const map=new Map();for(const p of c.policies.filter(p=>p.ISPID===0||p.ISPID===isp)){const prior=map.get(p.Name);map.set(p.Name,prior?{...prior,ambiguous:true}:p)}return [...map.values()];
 }
 function canonicalIP(value){
  if(/^\d+\.\d+\.\d+\.\d+$/.test(value)){const parts=value.split('.');return parts.every(p=>/^(0|[1-9]\d{0,2})$/.test(p)&&+p<=255)?parts.join('.'):null}
  if(!value.includes(':')||!/^[a-f\d:.]+$/i.test(value))return null;
  try{const h=new URL('http://['+value+']/').hostname.slice(1,-1);const mapped=h.match(/^::ffff:([a-f\d]+):([a-f\d]+)$/i);if(mapped){const n=parseInt(mapped[1],16),m=parseInt(mapped[2],16);return [n>>8,n&255,m>>8,m&255].join('.')}return h}catch{return null}
 }
 function values(d,i,c){const b={};d.find('input[name],select[name]').each(function(){b[this.name]=this.type==='checkbox'?this.checked:this.value.trim()});b.ISPID=i?i.ISPID:c.director?Number(b.ISPID):c.user.ispId;b.DeviceID=i?i.DeviceID:0;return b}
 function validate(c,b,i){
  const issues={};if(!b.Name||Array.from(b.Name).length>190)issues.Name='Enter a device name (up to 190 characters).';
  const ip=canonicalIP(b.ExporterIP);if(!ip)issues.ExporterIP='Enter a valid IPv4 or IPv6 address, without a port or subnet.';
  else if(c.items.some(row=>row.ID!==i?.ID&&canonicalIP(row.ExporterIP)===ip))issues.ExporterIP='This exporter IP is already registered.';
  if(!b.ISPID||c.director&&!c.isps.some(isp=>isp.ID===b.ISPID))issues.ISPID='Choose an ISP.';
  if(!Object.hasOwn(protocols,b.Protocol))issues.Protocol='Choose a protocol.';if(!Object.hasOwn(profiles,b.Profile))issues.Profile='Choose a vendor profile.';
  if(!['true','false'].includes(b.Enabled))issues.Enabled='Choose a status.';
  if(b.CapturePolicy){const p=available(c,b.ISPID).find(p=>p.Name===b.CapturePolicy);if(!p||p.ambiguous)issues.CapturePolicy='Choose an available, uniquely named policy or use custom rules.'}
  return issues;
 }
 function errors(d,fields){d.find('.isp-field-error').text('');d.find('[aria-invalid]').removeAttr('aria-invalid');for(const [key,text]of Object.entries(fields)){if(!Object.hasOwn(labels,key))continue;d.find(`[name="${key}"]`).attr('aria-invalid','true');d.find('#device-'+key+'-error').text(text)}}
 async function save(c,d,url,method,body,original){
  if(c.saving||d.data('uncertain'))return;c.saving=true;const disabled=d.find(':disabled').toArray();d.find('button,input,select').prop('disabled',true);d.find('.isp-form-error').text('');let wrote=false;
  try{
   // Catch edits made since opening the modal. The API has no atomic version token.
   const latest=await ajax(c,'/api/v1/devices');if(!alive(c))return;
   if(!Array.isArray(latest.devices))throw Error('Cannot verify the current device list. Close and refresh.');
   if(original){const row=latest.devices.find(i=>i.ID===original.ID);if(!row||editable.some(key=>row[key]!==original[key]))throw Error('This device changed or was removed. Close and reopen it before saving.')}
   if(body?.ExporterIP&&latest.devices.some(i=>i.ID!==original?.ID&&canonicalIP(i.ExporterIP)===canonicalIP(body.ExporterIP)))throw Error('This exporter IP is already registered.');
   if(body?.CapturePolicy){
    const data=await ajax(c,'/api/v1/policies');if(!alive(c))return;
    const matches=(data.policies||[]).filter(p=>p.Name===body.CapturePolicy&&(p.ISPID===0||p.ISPID===body.ISPID));
    if(matches.length!==1||Object.keys(rules).some(k=>!!matches[0][k]!==body[k]))throw Error('This capture policy changed or is unavailable. Close and reopen the form before saving.');
   }
   wrote=true;await ajax(c,url,method,body);if(!alive(c))return;c.saving=false;close(c);c.toast(method==='DELETE'?'Device deleted':url.endsWith('/toggle')?'Device status updated':method==='POST'?'Device created':'Device saved');await refresh(c);
  }catch(e){if(!alive(c))return;if(e.status===401){c.saving=false;close(c,true);c.expired();return}
   const uncertain=wrote&&(e.status===0||e.status>=500);d.data('uncertain',uncertain);errors(d,e.responseJSON?.fields||{});d.find('.isp-form-error').text(message(e)+(uncertain?' The outcome is uncertain. Close and refresh the list before trying again.':''));
  }finally{c.saving=false;d.find('button,input,select').prop('disabled',false);$(disabled).prop('disabled',true);if(d.data('uncertain'))d.find('[type=submit]').prop('disabled',true)}
 }
 function edit(c,i){
  if(!c.policiesReady){c.toast('Refresh to load capture policies before editing.','err');return}
  const isp=i?.ISPID||(c.director?+c.isp:c.user.ispId),pols=available(c,isp);
  const ispControl=c.director&&!i?select('ISPID',{'':'Choose ISP',...Object.fromEntries(c.isps.map(p=>[p.ID,p.Name]))},isp||'',true):input('ISPID',ispName(c,isp),'Assigned ISP',false,true);
  const d=dialog(c,i?'Edit device':'Add device',i?`${i.Name} · Device #${i.DeviceID}`:'Register a NetFlow / IPFIX exporter',`<p class="isp-form-note">Exporter IP must match the source address received by the collector. ${i?'ISP and device identity stay fixed to preserve log history.':'New devices are enabled on creation; the device ID is assigned automatically.'}</p><div class="isp-fields">${ispControl}${input('Name',i?.Name,'e.g. edge-router-01',true)}${input('ExporterIP',i?.ExporterIP,'e.g. 192.0.2.10 or 2001:db8::10',true)}${input('DeviceID',i?.DeviceID,'Assigned automatically',false,true)}${select('Protocol',{'':'Choose protocol',...protocols},i?.Protocol||'auto',true)}${select('Profile',{'':'Choose vendor profile',...profiles},i?.Profile||'generic',true)}${select('CapturePolicy',{'':'Use custom rules',...Object.fromEntries(pols.map(p=>[p.Name,p.Name+(p.ambiguous?' (ambiguous)':p.ISPID===0?' (global)':'')]))},i?.CapturePolicy||'',false,'A named policy supplies the skip rules when saved.')}${select('Enabled',{'':'Choose status',true:'Enabled',false:'Disabled'},i?String(i.Enabled):'true',true,i?'Disabling stops collection for this registered exporter.':'New registrations start enabled.',!i)}</div><fieldset class="device-rules"><legend>${icon('filter_alt')}Capture rules</legend><p class="isp-form-note">Checked rules discard matching flow records. Leave all unchecked to keep these records.</p><div>${Object.entries(rules).map(([k,label])=>`<label><input type="checkbox" name="${k}" ${i?.[k]?'checked':''}>${esc(label)}</label>`).join('')}</div><p class="hint" id="device-policy-note"></p></fieldset>`,`<button type="submit" class="btn">${icon('save')}${i?'Save changes':'Add device'}</button>`);
  let custom=Object.fromEntries(Object.keys(rules).map(k=>[k,!!i?.[k]])),previous=i?.CapturePolicy||'',tried=false;
  const sync=(initial=false)=>{
   const name=d.find('[name=CapturePolicy]').val(),scope=values(d,i,c).ISPID,p=available(c,scope).find(p=>p.Name===name);
   if(!initial&&!previous)custom=Object.fromEntries(Object.keys(rules).map(k=>[k,d.find(`[name=${k}]`).is(':checked')]));
   for(const k of Object.keys(rules))d.find(`[name=${k}]`).prop('disabled',!!name).prop('checked',name?(p&&!p.ambiguous?!!p[k]:!!i?.[k]):custom[k]);
   d.find('#device-policy-note').text(name?(!p||p.ambiguous?'This policy is missing or ambiguous. Choose another policy or custom rules.':'Policy rules will be applied when you save.'):'Custom rules apply to this device only.');previous=name;
  };
  // Retain a missing policy visibly; never silently replace it with custom rules.
  if(i?.CapturePolicy&&!pols.some(p=>p.Name===i.CapturePolicy))d.find('[name=CapturePolicy]').append($('<option>').val(i.CapturePolicy).text(i.CapturePolicy+' (unavailable)').prop('selected',true));
  sync(true);d.on('change','[name=CapturePolicy]',()=>sync());
  if(!i)d.on('change','[name=ISPID]',()=>{const list=available(c,values(d,i,c).ISPID);d.find('[name=CapturePolicy]').html('<option value="">Use custom rules</option>'+list.map(p=>`<option value="${esc(p.Name)}">${esc(p.Name+(p.ambiguous?' (ambiguous)':p.ISPID===0?' (global)':''))}</option>`).join(''));sync()});
  d.on('submit','form',()=>{if(c.saving)return;tried=true;const b=values(d,i,c),issues=validate(c,b,i);errors(d,issues);if(Object.keys(issues).length){d.find('[aria-invalid=true]').first().trigger('focus');return}b.ExporterIP=canonicalIP(b.ExporterIP);b.Enabled=b.Enabled==='true';save(c,d,'/api/v1/devices'+(i?'/'+i.ID:''),i?'PUT':'POST',b,i)});
  d.on('input change','input,select',()=>{if(tried)errors(d,validate(c,values(d,i,c),i))});
 }
 function view(c,i){
  const entries=[['Device name',i.Name],['ISP',ispName(c,i.ISPID)],['Device ID',i.DeviceID],['Exporter IP',i.ExporterIP],['Protocol',protocols[i.Protocol]],['Vendor profile',profiles[i.Profile]],['Status',i.Enabled?'Enabled':'Disabled'],['Health',healthNames[health(c,i)]],['Last evidence (IST)',date(c.health[i.ID]?.lastSeen)],['Updated (IST)',date(i.UpdatedAt)],['Capture policy',i.CapturePolicy||'Custom rules'],['Saved skip rules',Object.keys(rules).filter(k=>i[k]).map(k=>rules[k]).join(', ')||'None']];
  const d=dialog(c,'Device details',i.Name,`<dl class="isp-details">${entries.map(([key,v])=>`<div><dt>${key}</dt><dd>${esc(v||'—')}</dd></div>`).join('')}</dl><p class="isp-form-note device-detail-note">Saved rules are the device’s current configuration. Selecting a policy in Edit applies its rules when saved.</p>`,`<button type="button" class="btn out" id="device-logs">${icon('receipt_long')}Search logs</button><button type="button" class="btn" id="device-detail-edit" ${c.policiesReady?'':'disabled'}>${icon('edit')}Edit device</button>`);
  d.find('[data-close]').last().text('Close');d.find('#device-detail-edit').on('click',()=>edit(c,i));d.find('#device-logs').on('click',()=>{close(c);c.navigate('Logs',{isp:i.ISPID,device:i.DeviceID})});
 }
 function confirm(c,i,kind){
  const deleting=kind==='delete',title=deleting?'Delete device':i.Enabled?'Disable device':'Enable device';
  const d=dialog(c,title,i.Name,`<p class="isp-form-note">${deleting?'This removes the exporter registration. Stored logs remain searchable by ISP and device ID. Unregistered traffic follows the collector’s configured policy.':i.Enabled?'Collection for this registered exporter will stop when the registry reloads. Stored logs are retained.':'Collection for this registered exporter will be enabled when the registry reloads.'}</p>${deleting?input('Confirm','',i.Name,true):''}`,`<button type="submit" class="btn ${deleting?'bad':''}">${icon(deleting?'delete':i.Enabled?'pause':'play_arrow')}${title}</button>`);
  d.on('submit','form',()=>{if(deleting&&d.find('[name=Confirm]').val()!==i.Name){errors(d,{Confirm:'Enter the exact device name to confirm.'});d.find('input').trigger('focus');return}save(c,d,'/api/v1/devices/'+i.ID+(deleting?'':'/toggle'),deleting?'DELETE':'POST',undefined,i)});
 }
 async function action(c,id,kind){
  if(c.opening||c.dialog)return;c.opening=true;
  try{if(!await refresh(c)||!alive(c))return;const i=c.items.find(i=>i.ID===id);if(!i){c.toast('This device is no longer available.','err');return}if(kind==='view')view(c,i);else if(kind==='edit')edit(c,i);else confirm(c,i,kind)}finally{c.opening=false}
 }
 function dispose(){if(active){const c=active;active=null;for(const r of c.reads)r.abort();close(c,true);$(c.root).off('.devices')}}
 async function mount(root,options){
  dispose();const c={...options,root,director:!!options.user.isDirector,reads:new Set(),items:[],isps:[],policies:[],health:{},loaded:false,policiesReady:false,query:'',filter:'all',protocol:'',isp:'',page:1,limit:10};active=c;
  $(root).html(`<section class="device-index"><div class="index-stats" aria-label="Device statistics"></div><div class="isp-banner" id="device-banner" role="alert" hidden></div><div class="isp-tools device-tools"><input class="inp" id="device-search" type="search" placeholder="Search name, IP, ID or policy…" aria-label="Search devices">${c.director?'<select class="inp" id="device-isp-filter" aria-label="Filter ISP"><option value="">All ISPs</option></select>':''}<select class="inp" id="device-status-filter" aria-label="Filter device status"><option value="all">All statuses</option><option value="enabled">Enabled</option><option value="disabled">Disabled</option><option value="online">Online</option><option value="silent">Silent</option><option value="nodata">No recent data</option><option value="unknown">Health unavailable</option></select><select class="inp" id="device-protocol-filter" aria-label="Filter protocol"><option value="">All protocols</option>${Object.entries(protocols).map(([v,label])=>`<option value="${v}">${label}</option>`).join('')}</select><div class="isp-actions"><button class="btn out" id="device-refresh">${icon('refresh')}Refresh</button><button class="btn" id="open-add" disabled>${icon('add')}Add device</button></div></div><div class="card"><div class="ch">${icon('router')}<h2>Exporter devices</h2><span class="pill info" id="device-scope"></span><span class="hint" id="device-updated"></span></div><p class="device-health-note" id="device-health-note"></p><div class="isp-scroll"><table class="isp-table device-table"><thead><tr><th>Device</th>${c.director?'<th>ISP</th>':''}<th class="device-ip-head">Exporter IP</th><th>Protocol / profile</th><th>Capture rules</th><th class="device-status-head">Status</th><th class="device-health-head">Health / last evidence (IST)</th><th class="device-actions-head">Actions</th></tr></thead><tbody id="device-rows"></tbody></table></div><div class="isp-pagination"><span id="device-count" aria-live="polite"></span><div><label for="device-limit">Rows</label><select class="inp" id="device-limit"><option>10</option><option>25</option><option>50</option></select><button class="btn out sm" id="device-prev" aria-label="Previous page">${icon('chevron_left')}</button><span id="device-page"></span><button class="btn out sm" id="device-next" aria-label="Next page">${icon('chevron_right')}</button></div></div></div></section>`);
  $(root).on('input.devices','#device-search',function(){c.query=this.value;c.page=1;render(c)}).on('change.devices','#device-status-filter,#device-protocol-filter,#device-isp-filter,#device-limit',function(){const key={'device-status-filter':'filter','device-protocol-filter':'protocol','device-isp-filter':'isp','device-limit':'limit'}[this.id];c[key]=key==='limit'?+this.value:this.value;c.page=1;render(c)}).on('click.devices','#device-refresh',()=>refresh(c)).on('click.devices','#open-add',()=>{if(!c.dialog&&!c.opening)edit(c,null)}).on('click.devices','#device-prev',()=>{c.page--;render(c)}).on('click.devices','#device-next',()=>{c.page++;render(c)}).on('click.devices','[data-action]',function(){action(c,+$(this).closest('tr').attr('data-id'),this.dataset.action)});
  render(c);await refresh(c);
 }
 window.YesLogsDevices={mount,dispose};
})(jQuery);

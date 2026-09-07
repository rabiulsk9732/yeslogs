/* ISP index and modal CRUD. jQuery owns AJAX, form events and inline validation. */
(function($){
 'use strict';
 let active=null;
 const esc=v=>$('<span>').text(v==null?'':v).html().replace(/"/g,'&quot;').replace(/'/g,'&#39;');
 const icon=v=>`<span class="ms" aria-hidden="true">${v}</span>`;
 const number=v=>Number(v||0).toLocaleString('en-IN');
 const date=v=>v?new Intl.DateTimeFormat('en-GB',{timeZone:'Asia/Kolkata',dateStyle:'medium',timeStyle:'short'}).format(new Date(v)):'—';
 const status=v=>`<span class="pill ${v?'ok':'mut'}">${v?'Active':'Disabled'}</span>`;
 function ajax(ctx,url,method='GET',data){
  return $.ajax({url,method,dataType:'json',contentType:'application/json',headers:{'X-CSRF-Token':ctx.csrf},data:data===undefined?undefined:JSON.stringify(data),timeout:30000});
 }
 function errorText(xhr){return xhr.responseJSON?.error||(xhr.status===0?'Connection interrupted. Refresh the list to check whether your change was saved.':'Request failed. Please try again.');}
 function alive(ctx){return active===ctx&&ctx.root.isConnected;}
 function cards(ctx){
  const rows=ctx.items;const ready=ctx.loaded;
  const stats=[['Total ISPs',rows.length,'Registered tenants','apartment','var(--pri)'],['Active ISPs',rows.filter(i=>i.Enabled).length,'Sign-in enabled','check_circle','var(--ok)'],['Disabled ISPs',rows.filter(i=>!i.Enabled).length,'Sign-in blocked','block','var(--mut)'],['Exporters',rows.reduce((n,i)=>n+i.DeviceCount,0),'Registered devices','router','var(--cyan)'],['ISP users',rows.reduce((n,i)=>n+i.UserCount,0),'Tenant login accounts','group','var(--warn)']];
  $(ctx.root).find('.index-stats').html(stats.map(([label,value,sub,symbol,color])=>`<div class="tile" style="--tc:${color}"><div class="glass"></div>${icon(symbol).replace('class="ms"','class="ms wm"')}<div class="body"><div class="v">${ready?number(value):'—'}</div><div class="l">${label}</div><div class="f">${sub}</div></div></div>`).join(''));
 }
 function render(ctx){
  if(!alive(ctx))return;cards(ctx);
  const term=ctx.query.toLowerCase();const list=ctx.items.filter(i=>(ctx.filter==='all'||(ctx.filter==='active')===i.Enabled)&&[i.ID,i.Name,i.Username,i.Email,i.Phone].join(' ').toLowerCase().includes(term));
  const pages=Math.max(1,Math.ceil(list.length/10));ctx.page=Math.min(ctx.page,pages);
  const slice=list.slice((ctx.page-1)*10,ctx.page*10);
  $(ctx.root).find('#isp-rows').html(slice.length?slice.map(i=>`<tr data-id="${i.ID}"><td><strong>${esc(i.Name)}</strong><span class="isp-secondary mono">ISP #${i.ID}</span></td><td>${esc(i.Username||'Profile incomplete')}<span class="isp-secondary">${esc(i.Email||'No primary login')}</span></td><td class="mono">${esc(i.Phone||'Not added')}</td><td class="mono">${number(i.DeviceCount)}</td><td class="mono">${number(i.UserCount)}</td><td>${status(i.Enabled)}</td><td class="mono">${esc(date(i.CreatedAt))}</td><td><div class="isp-row-actions"><button class="btn out sm" data-action="view" aria-label="View ${esc(i.Name)}" title="View ISP">${icon('visibility')}</button><button class="btn out sm" data-action="edit" aria-label="Edit ${esc(i.Name)}" title="Edit ISP">${icon('edit')}</button><button class="btn danger sm" data-action="delete" aria-label="Delete ${esc(i.Name)}" title="Delete ISP">${icon('delete')}</button></div></td></tr>`).join(''):`<tr><td colspan="8"><div class="dash-empty">${ctx.loaded?(ctx.items.length?'No ISPs match these filters.':'No ISPs yet. Create your first tenant to get started.'):'ISP data is not available yet.'}</div></td></tr>`);
  $(ctx.root).find('#isp-count').text(`${list.length?((ctx.page-1)*10+1):0}–${Math.min(ctx.page*10,list.length)} of ${list.length} ISPs`);
  $(ctx.root).find('#isp-page').text(`${ctx.page} / ${pages}`);
  $(ctx.root).find('#isp-prev').prop('disabled',ctx.page<=1);$(ctx.root).find('#isp-next').prop('disabled',ctx.page>=pages);
 }
 async function refresh(ctx){
  if(ctx.loading)return;ctx.loading=true;$(ctx.root).find('#isp-refresh').prop('disabled',true);
  try{ctx.request=ajax(ctx,'/api/v1/isps');const data=await ctx.request;if(!alive(ctx))return;
   if(!Array.isArray(data.isps)||data.isps.some(i=>typeof i.Enabled!=='boolean'||!Number.isInteger(i.DeviceCount)||!Number.isInteger(i.UserCount)))throw Error('The ISP API has not been upgraded.');
   ctx.items=data.isps.sort((a,b)=>a.Name.localeCompare(b.Name));ctx.loaded=true;$(ctx.root).find('#isp-banner').prop('hidden',true);render(ctx);
  }catch(xhr){if(alive(ctx)){if(xhr.status===401){ctx.expired();return}$(ctx.root).find('#isp-banner').text((ctx.loaded?'Showing the last loaded list. ':'')+(xhr.message||errorText(xhr))).prop('hidden',false)}}
  finally{ctx.loading=false;if(alive(ctx))$(ctx.root).find('#isp-refresh').prop('disabled',false)}
 }
 function close(ctx,force=false){
  if(ctx.saving&&!force)return;
  ctx.dialog?.remove();ctx.dialog=null;$(document).off('keydown.ispDialog');
  if(ctx.returnFocus?.isConnected)ctx.returnFocus.focus();
 }
 function dialog(ctx,title,subtitle,body,footer){
  close(ctx);ctx.returnFocus=document.activeElement;
  const d=$(`<div class="isp-modal-bg"><section class="isp-modal" role="dialog" aria-modal="true" aria-labelledby="isp-modal-title"><div class="isp-modal-head">${icon('apartment')}<div><h2 id="isp-modal-title">${esc(title)}</h2><p>${esc(subtitle)}</p></div><button type="button" class="icon-btn" data-close aria-label="Close dialog">${icon('close')}</button></div><form id="isp-form" novalidate><div class="isp-modal-body">${body}<div class="isp-form-error" role="alert"></div></div><div class="isp-modal-foot"><button type="button" class="btn out" data-close>Cancel</button>${footer||''}</div></form></section></div>`).appendTo('body');
  ctx.dialog=d;d.on('click','[data-close]',()=>close(ctx));
  $(document).on('keydown.ispDialog',e=>{if(e.key==='Escape'){e.preventDefault();close(ctx)}if(e.key==='Tab'){
   const nodes=d.find('button:not(:disabled),input:not(:disabled),select:not(:disabled),[tabindex="0"]').filter(':visible').toArray();
   const first=nodes[0],last=nodes[nodes.length-1];if(e.shiftKey&&document.activeElement===first){e.preventDefault();last?.focus()}else if(!e.shiftKey&&document.activeElement===last){e.preventDefault();first?.focus()}
  }});
  d.find('input,select').first().trigger('focus');if(!d.find('input,select').length)d.find('[data-close]').first().trigger('focus');return d;
 }
 const labels={Name:'ISP name',Username:'Username',Email:'Email',Phone:'Phone',Password:'Password',ConfirmPassword:'Confirm password',Enabled:'Status'};
 const fieldIcons={Name:'apartment',Username:'person',Password:'lock',ConfirmPassword:'verified_user',Enabled:'toggle_on'};
 const placeholders={Name:'Enter ISP name',Username:'e.g. acme.admin',Email:'e.g. admin@acme.in',Phone:'e.g. +91 9876543210',Password:'Enter password (minimum 8 characters)',ConfirmPassword:'Re-enter password'};
 function fieldIcon(key){
  if(key==='Email')return '<svg class="isp-label-icon" viewBox="0 0 24 24" aria-hidden="true"><path fill="currentColor" d="M2 4h20v16H2V4zm2 2v1l8 5 8-5V6l-8 5-8-5zm0 3.4V18h16V9.4l-8 5-8-5z"/></svg>';
  if(key==='Phone')return '<svg class="isp-label-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 3h4l2 5-3 2c1 3 3 5 6 6l2-3 5 2v4c0 1-1 2-2 2C10 21 3 14 3 5c0-1 1-2 2-2Z" fill="none" stroke="currentColor" stroke-width="2"/></svg>';
  return icon(fieldIcons[key]||'label');
 }
 function fieldLabel(key,id,required,text=labels[key]){return `<label class="lbl" for="${id}">${fieldIcon(key)}<span>${esc(text)}</span>${required?'<span class="req" aria-hidden="true">*</span>':''}</label>`;}
 function field(key,type,value,required,hint=''){
  const id='isp-'+key;
  return `<div class="isp-field">${fieldLabel(key,id,required)}<input class="inp" id="${id}" name="${key}" type="${type}" placeholder="${esc(type==='password'&&!required?'Leave blank to keep current password':placeholders[key])}" value="${esc(value||'')}" ${required?'required':''} ${key==='Username'?'autocomplete="off" maxlength="64"':key==='Email'?'autocomplete="email" maxlength="190"':key==='Phone'?'autocomplete="tel" maxlength="25"':type==='password'?'autocomplete="new-password" maxlength="72"':'maxlength="190"'} aria-describedby="${id}-hint ${id}-error"><div class="hint" id="${id}-hint">${hint}</div><div class="isp-field-error" id="${id}-error"></div></div>`;
 }
 function values(d){const out={};d.find('input[name],select[name]').each(function(){out[this.name]=this.type==='password'?this.value:this.value.trim()});out.Username=out.Username?.toLowerCase();out.Email=out.Email?.toLowerCase();return out;}
 function validate(b,needsPassword){
  const e={};if(!b.Name||Array.from(b.Name).length>190)e.Name='Enter an ISP name (up to 190 characters).';
  if(!/^[a-z0-9][a-z0-9_.-]{2,63}$/i.test(b.Username))e.Username='Use 3–64 letters, numbers, dots, underscores or hyphens.';
  if(!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(b.Email)||b.Email.length>190)e.Email='Enter a valid email address.';
  const digits=(b.Phone.match(/[0-9]/g)||[]).length;if(!/^\+?[0-9 ()-]{7,25}$/.test(b.Phone)||digits<7||digits>15)e.Phone='Enter a phone number with 7–15 digits.';
  if(!['true','false'].includes(b.Enabled))e.Enabled='Choose Active or Disabled.';
  if(needsPassword||b.Password||b.ConfirmPassword){if(Array.from(b.Password).length<8||new TextEncoder().encode(b.Password).length>72)e.Password='Use at least 8 characters, up to 72 UTF-8 bytes.';if(!b.ConfirmPassword||b.Password!==b.ConfirmPassword)e.ConfirmPassword='Passwords must match.'}
  return e;
 }
 function errors(d,fields){
  d.find('.isp-field-error').text('');d.find('[aria-invalid]').removeAttr('aria-invalid');
  $.each(fields,(key,message)=>{d.find(`[name="${key}"]`).attr('aria-invalid','true');d.find('#isp-'+key+'-error').text(message)});
 }
 async function submit(ctx,d,url,method,payload){
  if(ctx.saving)return;ctx.saving=true;d.find('button,input,select').prop('disabled',true);d.find('.isp-form-error').text('');
  try{await ajax(ctx,url,method,payload);if(!alive(ctx))return;ctx.saving=false;close(ctx);ctx.toast(method==='DELETE'?'ISP deleted':method==='POST'?'ISP created':'ISP saved');await refresh(ctx)}
  catch(xhr){if(!alive(ctx))return;if(xhr.status===401){ctx.saving=false;close(ctx,true);ctx.expired();return}d.find('input,select').prop('disabled',false);errors(d,xhr.responseJSON?.fields||{});d.find('.isp-form-error').text(errorText(xhr));d.find('[aria-invalid=true]').first().trigger('focus')}
  finally{ctx.saving=false;d.find('button,input,select').prop('disabled',false)}
 }
 function edit(ctx,i){
  const needsPassword=!i||!i.AdminUserID;
  const d=dialog(ctx,i?'Edit ISP':'Create ISP',i?`${i.Name} · ISP #${i.ID}`:'Tenant details and primary login',`<p class="isp-form-note">All marked fields are required. The primary user can sign in with their username or email.${i&&!needsPassword?' Leave both password fields blank to keep the current password.':''}</p><div class="isp-fields">${field('Name','text',i?.Name,true)}${field('Username','text',i?.Username,true,'3–64 characters; unique across ISPs.')}${field('Email','email',i?.Email,true,'Primary login and contact email.')}${field('Phone','tel',i?.Phone,true,'Include the country code when available.')}${field('Password','password','',needsPassword)}${field('ConfirmPassword','password','',needsPassword)}<div class="isp-field">${fieldLabel('Enabled','isp-Enabled',true)}<select class="inp" id="isp-Enabled" name="Enabled" required aria-describedby="isp-Enabled-hint isp-Enabled-error"><option value="">Choose status</option><option value="true" ${i?.Enabled===true?'selected':''}>Active</option><option value="false" ${i?.Enabled===false?'selected':''}>Disabled</option></select><div class="hint" id="isp-Enabled-hint">Blocks ISP user access. Manage exporter collection on the Devices page.</div><div class="isp-field-error" id="isp-Enabled-error"></div></div></div>`,`<button type="submit" class="btn">${icon('save')}${i?'Save changes':'Create ISP'}</button>`);
  let tried=false;
  d.on('submit','form',e=>{e.preventDefault();if(ctx.saving)return;tried=true;const b=values(d),issues=validate(b,needsPassword);errors(d,issues);if(Object.keys(issues).length){d.find('[aria-invalid=true]').first().trigger('focus');return}b.Enabled=b.Enabled==='true';if(i)b.Version=i.Version;submit(ctx,d,'/api/v1/isps'+(i?'/'+i.ID:''),i?'PUT':'POST',b)});
  d.on('input change','input,select',()=>{if(tried)errors(d,validate(values(d),needsPassword))});
 }
 function view(ctx,i){
  const entries=[['ISP name',i.Name],['ISP ID',i.ID],['Username',i.Username||'Not added'],['Email',i.Email||'No primary login'],['Phone',i.Phone||'Not added'],['Status',i.Enabled?'Active':'Disabled'],['Exporters',i.DeviceCount],['ISP users',i.UserCount],['Created (IST)',date(i.CreatedAt)]];
  const d=dialog(ctx,'ISP details',i.Name,`<dl class="isp-details">${entries.map(([k,v])=>`<div><dt>${k}</dt><dd>${esc(v)}</dd></div>`).join('')}</dl>`,`<button type="button" class="btn" id="isp-detail-edit">${icon('edit')}Edit ISP</button>`);d.find('[data-close]').last().text('Close');d.find('#isp-detail-edit').on('click',()=>edit(ctx,i));d.on('submit',e=>e.preventDefault());
 }
 function remove(ctx,i){
  if(i.Enabled||i.DeviceCount){const d=dialog(ctx,'Delete ISP',i.Name,`<p class="isp-form-note">${i.Enabled?'Disable this ISP before deleting it.':'Remove or reassign its registered exporters before deleting it.'} Stored logs and audit history are retained.</p>`);d.find('[data-close]').last().text('Close');d.on('submit',e=>e.preventDefault());return}
  const d=dialog(ctx,'Delete ISP',i.Name,`<p class="isp-form-note">This permanently removes the ISP profile and its ${number(i.UserCount)} login account(s). Stored flow logs, archives and audit history are retained. Linked capture policies must be removed first.</p><div class="isp-field">${fieldLabel('Name','isp-delete-name',true,'Type the ISP name to confirm')}<input class="inp" id="isp-delete-name" name="Name" placeholder="${esc(i.Name)}" required autocomplete="off" aria-describedby="isp-Name-error"><div class="isp-field-error" id="isp-Name-error"></div></div>`,`<button type="submit" class="btn bad">${icon('delete')}Delete ISP</button>`);
  d.on('submit',e=>{e.preventDefault();const name=d.find('input').val();if(name!==i.Name){errors(d,{Name:'Enter the exact ISP name to confirm.'});d.find('input').trigger('focus');return}submit(ctx,d,'/api/v1/isps/'+i.ID,'DELETE',{Name:name,Version:i.Version})});
 }
 async function action(ctx,id,kind){
  try{const i=await ajax(ctx,'/api/v1/isps/'+id);if(!alive(ctx))return;({edit,view,delete:remove}[kind])(ctx,i)}catch(xhr){if(alive(ctx)){if(xhr.status===401)ctx.expired();else ctx.toast(errorText(xhr),'err')}}
 }
 function dispose(){if(active){active.request?.abort();close(active,true);active=null}}
 async function mount(root,options){
  dispose();const ctx={...options,root,items:[],loaded:false,query:'',filter:'all',page:1,loading:false,saving:false};active=ctx;
  $(root).html(`<section class="isp-index"><div class="index-stats" aria-label="ISP statistics"></div><div class="isp-banner" id="isp-banner" role="alert" hidden></div><div class="isp-tools"><input class="inp" id="isp-search" type="search" placeholder="Search ISPs…" title="Search name, username, email or phone" aria-label="Search ISPs"><select class="inp" id="isp-status-filter" aria-label="Filter ISP status"><option value="all">All statuses</option><option value="active">Active</option><option value="disabled">Disabled</option></select><div class="isp-actions"><button class="btn out" id="isp-refresh">${icon('refresh')}Refresh</button><button class="btn" id="isp-create">${icon('add')}Create ISP</button></div></div><div class="card"><div class="ch">${icon('apartment')}<h2>ISPs</h2><span class="ch-tag">Tenant directory</span></div><div class="isp-scroll"><table class="isp-table"><thead><tr><th>ISP</th><th>Primary user</th><th>Phone</th><th>Exporters</th><th>Users</th><th>Status</th><th>Created (IST)</th><th style="text-align:right">Actions</th></tr></thead><tbody id="isp-rows"></tbody></table></div><div class="isp-pagination"><span id="isp-count"></span><div><button class="btn out sm" id="isp-prev" aria-label="Previous page">${icon('chevron_left')}</button><span id="isp-page"></span><button class="btn out sm" id="isp-next" aria-label="Next page">${icon('chevron_right')}</button></div></div></div></section>`);
  $(root).find('#isp-search').on('input',function(){ctx.query=this.value;ctx.page=1;render(ctx)});
  $(root).find('#isp-status-filter').on('change',function(){ctx.filter=this.value;ctx.page=1;render(ctx)});
  $(root).find('#isp-create').on('click',()=>edit(ctx,null));$(root).find('#isp-refresh').on('click',()=>refresh(ctx));
  $(root).find('#isp-prev').on('click',()=>{ctx.page--;render(ctx)});$(root).find('#isp-next').on('click',()=>{ctx.page++;render(ctx)});
  $(root).find('#isp-rows').on('click','[data-action]',function(){action(ctx,$(this).closest('tr').data('id'),this.dataset.action)});
  render(ctx);await refresh(ctx);
 }
 window.YesLogsISPs={mount,dispose};
})(window.jQuery);

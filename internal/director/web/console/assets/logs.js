/* Search workspace. Draft filters, applied filters and result pages have separate lifetimes. */
(function ($) {
  'use strict';
  const K = window.YesLogsKit, icon = K.icon;
  const entities = { '&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;' };
  const esc = v => String(v ?? '').replace(/[&<>"']/g, ch => entities[ch]);
  let c;
  const emptyBody = () => ({ ISPID: 0, DeviceID: 0, ExporterIP: '', PublicIP: '', PublicPort: 0, PrivateIP: '', DestIP: '', Username: '', Proto: 'Any', From: '', To: '', Reason: '', CaseID: 0 });
  const fields = { ISPID: 's-isp', DeviceID: 's-dev', PublicIP: 's-pub', PublicPort: 's-port', PrivateIP: 's-priv', DestIP: 's-dst', Username: 's-user', Proto: 's-proto', From: 's-from', To: 's-to', Reason: 's-reason', CaseID: 's-case' };
  const columns = ['Start Date(mm:dd:yyyy) & Time(hh:mm:ss)', 'End Date(mm:dd:yyyy) & Time(hh:mm:ss)', 'Source IP Address', 'Source Port', 'Translated IP address', 'Translated Port', 'Destination IP Address', 'Destination Port'];
  const num = n => Number(n).toLocaleString();
  const alive = x => c === x && x.root.isConnected;
  function stats(d, count) {
    K.stats(c.root, [
      ['Matching records', d ? num(d.total) + (d.capped ? '+' : '') : '—', d?.capped ? 'Result window · lower bound' : 'Current search window', 'table_rows'],
      ['Rows on page', d ? count : '—', d ? `Page ${Math.floor(c.offset / c.size) + 1} · newest first` : 'Results appear after a search', 'list'],
      ['Search duration', d ? num(d.elapsedMs) + ' ms' : '—', d ? 'Server search + enrichment' : 'Measured for each response', 'timer'],
      ['Storage searched', d ? d.cold ? 'Hot + S3' : 'Hot' : '—', 'Explicit dates include available archives', 'storage'],
      ['CRM matches', d ? d.crm?.enabled ? num(d.crm.matched || 0) : 'Off' : '—', 'Subscriber enrichment for this page', 'person_search']
    ]);
  }
  function read() {
    const b = emptyBody();
    for (const [key, id] of Object.entries(fields)) if ($('#' + id).length) b[key] = String($('#' + id).val() ?? '').trim();
    b.DeviceID = b.DeviceID.split('@')[0];
    b.ExporterIP = $('#s-dev option:selected').attr('data-exporter') || '';
    if (typeof b.PublicPort === 'string' && b.PublicPort.includes('-')) {
      b.PortRange = b.PublicPort;
      b.PublicPort = 0;
    } else {
      b.PublicPort = Number(b.PublicPort) || 0;
    }
    for (const key of ['ISPID', 'DeviceID', 'CaseID']) b[key] = Number(b[key]) || 0;
    return b;
  }
  function deviceKey(d) { return c.devs.filter(v => +v.ISPID === +d.ISPID && +v.DeviceID === +d.DeviceID).length > 1 ? d.DeviceID + '@' + d.ExporterIP : String(d.DeviceID); }
  function devices() {
    const isp = Number($('#s-isp').val());
    const list = c.devs.filter(d => !c.user.isDirector || +d.ISPID === isp);
    const shared = [...new Set(list.filter(d => deviceKey(d).includes('@')).map(d => d.DeviceID))];
    $('#s-dev').html('<option value="0">' + (c.user.isDirector && !isp ? 'Select an ISP first' : 'Any device') + '</option>' + shared.map(id => `<option value="${id}">Device #${id} · all exporters</option>`).join('') + list.map(d => `<option value="${esc(deviceKey(d))}" data-exporter="${esc(d.ExporterIP || '')}">${esc(d.Name)} (#${d.DeviceID}${d.ExporterIP ? ' · ' + esc(d.ExporterIP) : ''})</option>`).join('')).prop('disabled', c.user.isDirector && !isp);
    if (list.length === 1) $('#s-dev').val(deviceKey(list[0]));
  }
  function cases() { const isp=Number($('#s-isp').val())||c.user.ispId||0,list=c.cases.filter(x=>!isp||+x.ispId===isp);$('#s-case').html('<option value="0">No linked case</option>'+list.map(x=>`<option value="${x.id}">${esc(x.reference)} · ${esc(x.title)}</option>`).join('')); }
  function fill(b) {
    $('#s-isp').val(String(b.ISPID || 0)); devices(); cases();
    for (const [key, id] of Object.entries(fields)) if (key !== 'ISPID') $('#' + id).val(['DeviceID', 'CaseID'].includes(key) ? String(b[key] || 0) : b[key] || (key === 'Proto' ? 'Any' : ''));
    const matches = c.devs.filter(d => +d.DeviceID === b.DeviceID && (!b.ISPID || +d.ISPID === b.ISPID) && (!b.ExporterIP || d.ExporterIP === b.ExporterIP));
    $('#s-dev').val(matches.length === 1 ? deviceKey(matches[0]) : matches.length > 1 && !b.ExporterIP ? String(b.DeviceID) : '0');
    $('#logs-form [aria-invalid]').removeAttr('aria-invalid'); $('#logs-form .isp-field-error').text(''); $('#s-modal-msg').text(''); $('.logs-presets button').removeClass('on').attr('aria-pressed','false');
  }
  function open() {
    if (!c || !c.ready) return;
    c.beforeDraft = read(); c.returnFocus = document.activeElement;
    $('#logs-filter-modal').prop('hidden', false);
    $('#logs-form').find('input,select').filter(':visible:enabled').first().trigger('focus');
  }
  function close(discard = true) {
    if (!c) return;
    $('#s-from,#s-to').datetimepicker('hide');
    if (discard && c.beforeDraft) fill(c.beforeDraft);
    $('#logs-filter-modal').prop('hidden', true);
    if (c.returnFocus?.isConnected) c.returnFocus.focus();
  }
  function chips() {
    const b = c.applied;
    if (!b) { $('#logs-chips').html('<span class="logs-muted">Choose filters to start an audited search.</span>'); return; }
    const parts = [];
    if (c.user.isDirector) parts.push(['ISP', b.ISPID ? c.isps.find(i => +i.ID === b.ISPID)?.Name || '#' + b.ISPID : 'All ISPs']);
    if (b.DeviceID) parts.push(['Device', !b.ExporterIP && c.devs.filter(d => +d.DeviceID === b.DeviceID && (!b.ISPID || +d.ISPID === b.ISPID)).length > 1 ? '#' + b.DeviceID + ' · all exporters' : c.devs.find(d => +d.DeviceID === b.DeviceID && (!b.ISPID || +d.ISPID === b.ISPID) && (!b.ExporterIP || d.ExporterIP === b.ExporterIP))?.Name || '#' + b.DeviceID]);
    if (b.ExporterIP) parts.push(['Exporter', b.ExporterIP]);
    for (const [key, label] of [['PublicIP','Public IP'],['PublicPort','Port'],['PortRange','Port Range'],['PrivateIP','Private IP'],['DestIP','Destination'],['Username','Subscriber']]) if (b[key]) parts.push([label,b[key]]);
    if (b.Proto !== 'Any') parts.push(['Protocol', b.Proto]);
    if (b.CaseID) parts.push(['Case', c.cases.find(x=>+x.id===+b.CaseID)?.reference||'#'+b.CaseID]);
    parts.push(['IST', b.From ? b.From + ' → ' + b.To : 'All indexed hot logs']);
    $('#logs-chips').html(parts.map(([label, value]) => `<span class="logs-chip"><span>${label}</span><b>${esc(value)}</b></span>`).join(''));
    $('#logs-filter-count').text(parts.length);
  }
  function state(kind, message) {
    const data = {
      idle: ['search', 'Find the flow. Trace the subscriber.', 'Search a public IP and port, a private IP, subscriber or device. Add a time window for a more focused search.', 'Choose filters'],
      empty: ['search', 'No matching flow logs', 'No records matched this search. Check the ISP and device, widen the time window, or remove an optional filter.', 'Adjust filters'],
      loading: ['schedule', 'Searching flow logs', 'Reading the selected window and resolving available subscriber details…', 'Cancel search'],
      error: ['error', 'We couldn’t load this search', message || 'The search failed. Your applied filters are kept so you can retry.', 'Retry search'],
      cancelled: ['pause', 'Search cancelled', 'Your filters are ready. Retry the search or adjust its scope.', 'Retry search']
    }[kind];
    c.state = kind;
    $('#logs-result-count').text(kind === 'loading' ? 'Searching…' : kind === 'empty' ? '0 records' : 'Flow records');
    $('#logs-exports,#logs-pagination').empty(); $('#logs-timing,#logs-evidence-note').text('');
    $('#logTable thead').html('<tr>' + columns.map(v => '<th>' + esc(v) + '</th>').join('') + '</tr>');
    $('#logTable tbody').html(`<tr class="logs-state-row"><td colspan="${columns.length}"><div class="logs-state logs-state-${kind}" role="${kind === 'error' ? 'alert' : 'status'}"><div class="logs-state-icon">${icon(data[0])}</div><span class="logs-eyebrow">${kind === 'idle' ? 'NAT MAPPING SEARCH' : kind === 'empty' ? 'SEARCH COMPLETE' : 'SEARCH STATUS'}</span><h3>${data[1]}</h3><p id="logs-state-description">${esc(data[2])}</p><div class="logs-state-actions"><button type="button" class="btn" data-log-action="${kind === 'loading' ? 'cancel' : ['error','cancelled'].includes(kind) ? 'retry' : 'filters'}">${icon(kind === 'loading' ? 'close' : 'search')}${data[3]}</button>${kind === 'error' || kind === 'cancelled' ? '<button type="button" class="btn out" data-log-action="filters">Edit filters</button>' : ''}</div>${kind === 'idle' ? '<div class="logs-state-tips"><span>1. Select a scope</span><span>2. Add an endpoint or device</span><span>3. Search & inspect</span></div>' : ''}</div></td></tr>`);
    $('#s-results').attr('aria-busy', String(kind === 'loading'));
    $('#logs-refresh').prop('disabled', !c.applied || kind === 'loading');
    $('#s-msg').text(kind === 'error' ? data[2] : '');
  }
  const timestamp = r => r.startTime || r.time || [r.date, r.clock].filter(Boolean).join(' ');
  // API timestamps are already IST. Reorder their date components without
  // interpreting them in the browser's local timezone or inventing an end time.
  const ipdrTime = value => {
    const match = /^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}:\d{2}:\d{2})$/.exec(value || '');
    return match ? `${match[2]}:${match[3]}:${match[1]} & ${match[4]}` : '—';
  };
  const reported = v => v === undefined || v === null || v === '' ? '—' : v;
  const port = (ip, value) => ip ? reported(value) : '—';
  const translationLabels = { source: 'Source NAT', destination: 'Destination NAT', both: 'Both sides', none: 'Unchanged', unknown: 'Incomplete fields' };
  function translation(r) {
    if (Object.hasOwn(translationLabels, r.translation)) return { kind: r.translation, ip: ['source','both'].includes(r.translation) ? r.natIp || '' : '', port: r.natPort };
    // Older APIs preserve only post-source fields. An unchanged source cannot rule out destination NAT.
    const changed = r.pubIp && r.pubIp !== '0.0.0.0' && (r.pubIp !== r.privIp || r.pubPort !== r.privPort);
    return { kind: changed ? 'source' : 'unknown', ip: changed ? r.pubIp : '', port: changed ? r.pubPort : undefined };
  }
  function queryString(b) {
    const p = new URLSearchParams({ csrf: c.csrf });
    for (const [key, param] of [['PublicIP','ip'],['PrivateIP','priv'],['DestIP','dst'],['Username','user'],['PublicPort','port'],['PortRange','port_range'],['DeviceID','device'],['ExporterIP','exporter'],['ISPID','isp'],['From','from'],['To','to'],['Reason','reason'],['CaseID','case_id']]) if (b[key]) p.set(param, b[key]);
    if (b.Proto !== 'Any') p.set('proto', b.Proto);
    return p.toString();
  }
  function render(d, cached = false) {
    if (!d || !Array.isArray(d.records) || !Number.isFinite(+d.total) || +d.total < 0) throw new Error('Invalid search response');
    c.result = d; c.state = 'ready'; $('#s-msg').text('');
    const rows = d.records, qs = queryString(c.applied);
    if (!rows.length) state('empty');
    else {
      $('#logTable thead').html('<tr>' + columns.map(v => '<th>' + esc(v) + '</th>').join('') + '</tr>');
      const tableHTML = d.tableHTML || rows.map((r, i) => {
        const nat = translation(r);
        return `<tr data-record="${i}"><td class="mono"><button type="button" class="logs-record-time" data-details="${i}" aria-label="View record ${i + 1} details" title="View record details">${esc(ipdrTime(timestamp(r)))}</button></td><td class="mono">${esc(ipdrTime(r.endTime))}</td><td class="mono">${esc(reported(r.privIp))}</td><td class="mono">${esc(port(r.privIp,r.privPort))}</td><td class="mono">${esc(reported(nat.ip))}</td><td class="mono">${esc(port(nat.ip,nat.port))}</td><td class="mono">${esc(reported(r.dstIp))}</td><td class="mono">${esc(port(r.dstIp,r.dstPort))}</td></tr>`;
      }).join('');
      d.tableHTML = tableHTML; $('#logTable tbody').html(tableHTML);
    }
    stats(d, rows.length);
    const missingNAT = rows.filter(r => !translation(r).ip).length;
    $('#logs-evidence-note').text(missingNAT ? `${num(missingNAT)} displayed ${missingNAT === 1 ? 'record has' : 'records have'} no confirmed source translation. All matching records are retained in this view.` : '');
    const total = num(d.total) + (d.capped ? '+' : '');
    $('#logs-result-count').text(total + ' records');
    $('#logs-timing').text(cached ? 'Previously loaded page · ' + new Date(d.loadedAt).toLocaleTimeString('en-GB', { timeZone: 'Asia/Kolkata' }) + ' IST · Refresh for latest' : `${num(Math.round(d.clientMs || 0))} ms to display · ${d.cold ? 'Hot + S3' : 'Hot storage'}`);
    const dlLink = (fmt, label, ico, schema) => `<a class="btn out sm" href="/api/v1/report?format=${fmt}${schema ? '&schema=' + schema : ''}&${esc(qs)}" title="${schema === 'dot16' ? '16-Column Enriched DoT Compliance Report' : '8-Column Technical NAT Report'}">${icon(ico)}${label}</a>`;
    $('#logs-exports').html([
      dlLink('csv','DoT CSV (16-col)','description','dot16'),
      dlLink('xlsx','DoT Excel','grid_on','dot16'),
      dlLink('pdf','DoT PDF','picture_as_pdf','dot16'),
      dlLink('csv','Raw CSV (8-col)','table_view','raw8'),
      `<button type="button" class="btn out sm" data-log-action="async-export">${icon('schedule')}Background CSV</button>`,
    ].join(''));
    $('#logs-pagination').html(`<div><label for="lq-size">Rows per page</label><select id="lq-size" class="inp">${[25,50,100,200].map(n => `<option ${n === c.size ? 'selected' : ''}>${n}</option>`).join('')}</select><span>${rows.length ? num(c.offset + 1) + '–' + num(c.offset + rows.length) : '0'} of <b>${total}</b></span></div><div><button class="btn out sm pgb" data-offset="${c.offset - c.size}" ${!c.offset ? 'disabled' : ''}>${icon('chevron_left')}Prev</button><span>Page <b>${num(Math.floor(c.offset / c.size) + 1)}</b> of ${num(Math.max(1,Math.ceil(d.total / c.size)))}${d.capped ? '+' : ''}</span><button class="btn out sm pgb" data-offset="${c.offset + c.size}" ${c.offset + c.size >= d.total ? 'disabled' : ''}>Next${icon('chevron_right')}</button></div>`);
    $('#s-results').attr('aria-busy','false'); $('#logs-refresh').prop('disabled',false);
  }
  async function asyncExport(){if(!c?.applied)return;const a={...c.applied},toRFC=v=>v?v.replace(' ', 'T')+(v.length===16?':00':'')+'+05:30':null;a.From=toRFC(a.From);a.To=toRFC(a.To);try{const j=await c.kit.ajax('/api/v1/exports','POST',{filter:a,caseId:a.CaseID||0,reason:a.Reason||'',schema:'dot16'});c.kit.toast?.('Background export queued');const poll=async()=>{if(!c)return;const s=await c.kit.ajax('/api/v1/exports/'+j.id);if(s.status==='complete'){c.kit.toast?.(`Export ready · ${num(s.rows)} rows`);window.location.assign('/api/v1/exports/'+j.id+'/download');return}if(s.status==='failed'){c.kit.toast?.('Background export failed: '+s.error);return}setTimeout(poll,2000)};setTimeout(poll,1000)}catch(e){c.kit.toast?.(c.kit.error(e))}}
  function validate(b) {
    const errors = {};
    const isValidIPv4 = v => /^(\d{1,3}\.){3}\d{1,3}$/.test(v) && v.split('.').every(n => +n <= 255);
    const isValidIPv6 = v => v.includes(':') && /^[0-9a-f:]+$/i.test(v) && !v.includes(':::') && v.split('::').length <= 2 && v.split(':').filter(Boolean).length <= 8;
    const isValidCIDR = v => {
      const parts = v.split('/');
      if (parts.length !== 2) return false;
      const prefix = Number(parts[1]);
      return (isValidIPv4(parts[0]) && Number.isInteger(prefix) && prefix >= 0 && prefix <= 32) || (isValidIPv6(parts[0]) && Number.isInteger(prefix) && prefix >= 0 && prefix <= 128);
    };
    const isValidIPOrCIDR = v => {
      v = (v || '').trim();
      if (v.includes(',')) {
        const list = v.split(',').map(s => s.trim()).filter(Boolean);
        return list.length > 0 && list.every(v => isValidIPv4(v) || isValidIPv6(v)) && new Set(list.map(v => isValidIPv4(v) ? 4 : 6)).size === 1;
      }
      return isValidIPv4(v) || isValidIPv6(v) || isValidCIDR(v);
    };

    if (!b.PublicIP) {
      errors['s-pub'] = 'Public NAT IP is mandatory.';
    } else if (!isValidIPOrCIDR(b.PublicIP)) {
      errors['s-pub'] = 'Enter an IPv4/IPv6 address, CIDR, or comma-separated list.';
    }
    for (const key of ['PrivateIP','DestIP']) if (b[key] && !isValidIPOrCIDR(b[key])) errors[fields[key]] = 'Enter an IPv4/IPv6 address or CIDR.';
    if ($('#s-port').val()) {
      const pVal = $('#s-port').val().trim();
      if (pVal.includes('-')) {
        const parts = pVal.split('-');
        const p1 = Number(parts[0]), p2 = Number(parts[1]);
        if (!parts[0] || !parts[1] || isNaN(p1) || isNaN(p2) || p1 < 1 || p2 > 65535 || p1 > p2) {
          errors['s-port'] = 'Enter a valid port range, e.g. 20000-25000.';
        }
      } else if (!Number.isInteger(b.PublicPort) || b.PublicPort < 1 || b.PublicPort > 65535) {
        errors['s-port'] = 'Enter a port from 1 to 65535 or a range like 20000-25000.';
      }
    }
    if (!!b.From !== !!b.To) errors[b.From ? 's-to' : 's-from'] = 'Set both From and To, or leave both blank.';
    for (const key of ['From','To']) if (b[key]) {
      const withSeconds = b[key].length === 16 ? b[key] + ':00' : b[key], date = Date.parse(withSeconds.replace(' ','T') + '+05:30');
      if (!/^\d{4}-\d\d-\d\d \d\d:\d\d(?::\d\d)?$/.test(b[key]) || !Number.isFinite(date) || new Date(date + 330 * 60000).toISOString().slice(0,19).replace('T',' ') !== withSeconds) errors[fields[key]] = 'Use YYYY-MM-DD HH:MM:SS in IST.';
    }
    if (b.From && b.To && b.From > b.To) errors['s-to'] = 'To must be on or after From.';
    $('#logs-form [aria-invalid]').removeAttr('aria-invalid'); $('#logs-form .isp-field-error').text('');
    for (const [id, msg] of Object.entries(errors)) { $('#' + id).attr('aria-invalid','true'); $('#' + id + '-error').text(msg); }
    const selector = b.PublicIP;
    $('#s-modal-msg').text(Object.keys(errors).length ? 'Check the highlighted fields.' : !selector ? 'Public NAT IP is mandatory.' : '');
    if (Object.keys(errors).length) { if ($('#logs-advanced [aria-invalid=true]').length) $('#logs-advanced').prop('open',true); $('#logs-form [aria-invalid=true]').first().trigger('focus'); }
    return !Object.keys(errors).length && !!selector;
  }
  function cancel(show = true) {
    if (!c) return;
    c.request++; clearTimeout(c.slowTimer); c.xhr?.abort(); c.xhr = null;
    if (show) { stats(); state('cancelled'); }
  }
  async function search(fresh = false) {
    if (!c?.applied) return;
    cancel(false);
    const x = c, id = x.request, key = x.size + ':' + x.offset;
    if (fresh) x.cache.clear();
    const saved = x.cache.get(key);
    if (saved && Date.now() - saved.loadedAt < 60000) { render(saved, true); return; }
    stats(); state('loading');
    const started = performance.now();
    x.slowTimer = setTimeout(() => { if (alive(x) && id === x.request) $('#logs-state-description').text('Still searching. Large windows, archived days and subscriber lookups can take longer. You can cancel and narrow the filters.'); }, 3000);
    try {
      const data = await (x.xhr = $.ajax({ url: '/api/v1/search', method: 'POST', contentType: 'application/json', dataType: 'json', headers: { 'X-CSRF-Token': x.csrf }, data: JSON.stringify({ ...x.applied, Limit: x.size, Offset: x.offset }), timeout: 120000 }));
      if (!alive(x) || id !== x.request) return;
      const d = { ...data, loadedAt: Date.now(), clientMs: performance.now() - started };
      render(d);
      d.clientMs = performance.now() - started;
      $('#logs-timing').text(`${num(Math.round(d.clientMs))} ms to display · ${d.cold ? 'Hot + S3' : 'Hot storage'}`);
      if (x.cache.size >= 8) x.cache.delete(x.cache.keys().next().value);
      x.cache.set(key,d);
    } catch (e) {
      if (!alive(x) || id !== x.request) return;
      if (e.status === 401) { x.expired(); return; }
      stats();
      state('error', e instanceof Error ? 'Search returned data, but the log table could not be displayed. Refresh and retry.' : e.responseJSON?.error || 'Search connection failed or timed out. Retry or narrow the time window.');
    } finally {
      if (alive(x) && id === x.request) { clearTimeout(x.slowTimer); x.xhr = null; }
    }
  }
  function details(index) {
    const r = c.result?.records[index]; if (!r) return;
    const nat = translation(r);
    const values = [['Start Date & Time · IST',ipdrTime(timestamp(r))],['End Date & Time · IST',ipdrTime(r.endTime)],['src_ip',r.privIp],['src_port',port(r.privIp,r.privPort)],['dst_ip',r.dstIp],['dst_port',port(r.dstIp,r.dstPort)],['nat_ip',nat.ip],['nat_port',port(nat.ip,nat.port)],['Exporter IP',r.exporterIp],['Device',r.sub],['Protocol',r.proto],['Exporter subscriber',r.username || 'Not reported'],['Translation',translationLabels[nat.kind]],['Export type',r.action],['NAT event',r.natEvent === 1 ? 'Allocation' : r.natEvent === 2 ? 'Release' : 'Not reported'],['CRM status',r.crmStatus || 'Not enabled'],['CRM subscriber',r.crmName || r.crmUsername || 'Not resolved'],['CRM MAC (CallingStationId)',r.crmMac || '—'],['CRM account',r.crmAccountId || '—'],['CRM phone',r.crmPhone || '—'],['CRM address',r.crmAddress || '—'],['CRM reference',r.crmReference || '—']];
    c.kit.form({ title:'Flow record details', icon:'receipt_long', subtitle:'Reported fields for this record · times in IST', html:'<dl class="module-details">' + values.map(([k,v]) => `<div><dt>${esc(k)}</dt><dd>${esc(reported(v))}</dd></div>`).join('') + '</dl>' });
  }
  const field = (id, label, symbol, input, hint = '') => `<div class="isp-field"><label class="lbl" for="${id}">${icon(symbol)}<span>${label}</span></label>${input}<div class="hint">${hint}</div><div class="isp-field-error" id="${id}-error"></div></div>`;
  const input = (id, placeholder, type = 'text') => `<input id="${id}" class="inp ${type === 'text' ? 'mono' : ''}" type="${type}" placeholder="${placeholder}" autocomplete="off" aria-describedby="${id}-error">`;
  async function mount(root, opts) {
    dispose(); c = { ...opts, root, ready:false, request:0, size:50, offset:0, cache:new Map(), devs:[], isps:[], cases:[] };
    const x = c; x.kit = K.context(root,opts);
    $(root).addClass('logs-page').html(`<div class="index-stats" aria-label="Logs statistics"></div><div class="card logs-toolbar"><div class="logs-toolbar-top"><div><h2>${icon('receipt_long')}Log explorer</h2><p>IPDR records · all times in IST. Select a start time for record details.</p></div><div class="logs-toolbar-actions"><span class="pill warn">${icon('receipt_long')}Every search is audited</span><button id="logs-refresh" class="btn out" disabled>${icon('refresh')}Refresh</button><button id="logs-open-filters" class="btn" disabled>${icon('filter_alt')}Filters <span id="logs-filter-count">0</span></button></div></div><div id="logs-chips" class="logs-chips"></div></div><div id="s-msg" class="logs-error-summary" role="alert"></div><div id="s-results" class="card logs-results" aria-busy="false"><div class="ch"><span class="ms">table_rows</span><h2 id="logs-result-count">Flow records</h2><div id="logs-exports" class="ch-ctl"></div></div><div id="logs-evidence-note" class="logs-evidence-note" role="status"></div><div class="logs-table-scroll"><table id="logTable"><thead></thead><tbody></tbody></table></div><div id="logs-pagination" class="logs-pagination"></div><div id="logs-timing" class="logs-timing" aria-live="polite"></div></div>`);
    stats(); chips(); state('idle');
    $(root).on('click.logs', '[data-log-action]', function () { const action = this.dataset.logAction; if (action === 'filters') open(); if (action === 'cancel') cancel(); if (action === 'retry') search(true); if(action==='async-export')asyncExport(); });
    $('#logs-open-filters').on('click',open); $('#logs-refresh').on('click',() => search(true));
    $(root).on('click.logs','.pgb', function () { c.offset = Math.max(0,+this.dataset.offset); search(); });
    $(root).on('change.logs','#lq-size',function () { c.size = +this.value; c.offset = 0; search(); });
    $(root).on('click.logs','[data-details]',function () { details(+this.dataset.details); });
    try {
      const d = await (x.inventory = x.kit.ajax('/api/v1/devices'));
      if (!alive(x)) return;
      x.devs = d.devices || []; x.isps = d.isps || [];
      try { const cd=await x.kit.ajax('/api/v1/cases');x.cases=cd.cases||[] } catch (_) { x.cases=[] }
      x.ready = true;
    } catch (e) { if (alive(x)) { stats(); state('error','Cannot load search filters. Refresh the page to retry.'); $('#s-results [data-log-action=retry]').attr('data-log-action','reload').text('Reload page').on('click',() => location.reload()); } return; }
    const isp = x.user.isDirector ? field('s-isp','ISP','apartment',`<select id="s-isp" class="inp"><option value="0">All ISPs</option>${x.isps.map(i => `<option value="${i.ID}">${esc(i.Name)} (#${i.ID})</option>`).join('')}</select>`) : '';
    $(root).append(`<div id="logs-filter-modal" class="isp-modal-bg logs-modal-bg" hidden><section class="isp-modal logs-modal" role="dialog" aria-modal="true" aria-labelledby="logs-filter-title"><div class="isp-modal-head">${icon('filter_alt')}<div><h2 id="logs-filter-title">Search filters</h2><p>Set the scope, endpoint and time window.</p></div><button class="icon-btn" data-filter-close aria-label="Close filters">${icon('close')}</button></div><form id="logs-form" novalidate><div class="isp-modal-body"><div class="logs-filter-section"><div class="logs-section-title"><b>01</b><h3>Scope & endpoint</h3><span>Public NAT IP is mandatory</span></div><div class="isp-fields">${isp}${field('s-dev','Device','router','<select id="s-dev" class="inp"></select>')}${field('s-pub','Public NAT IP *','dns',input('s-pub','203.0.113.10'),'The translated IP seen by the outside world (Mandatory).')}${field('s-port','Public port','tag',input('s-port','Any port','number'),'Pair with a public IP to identify a mapping.')}</div></div><div class="logs-filter-section"><div class="logs-section-title"><b>02</b><h3>Time window</h3><span>India Standard Time · UTC+05:30</span></div><div class="logs-presets" aria-label="Quick time ranges"><button type="button" data-minutes="15">Last 15 min</button><button type="button" data-minutes="60">Last hour</button><button type="button" data-minutes="1440">Last 24 hours</button><button type="button" data-minutes="0">All hot logs</button></div><div class="isp-fields">${field('s-from','From (IST)','event',input('s-from','YYYY-MM-DD HH:MM:SS'))}${field('s-to','To (IST)','event',input('s-to','YYYY-MM-DD HH:MM:SS'))}</div><p class="logs-filter-note">${icon('info')}Set both dates to include available S3 archives. Blank dates search indexed hot storage. Narrow windows usually return faster.</p></div><details id="logs-advanced" class="logs-advanced"><summary>${icon('tune')}Advanced filters <span>Private IP, destination, subscriber & protocol</span>${icon('expand_more')}</summary><div class="isp-fields">${field('s-priv','Private IP','dns',input('s-priv','100.64.1.10'))}${field('s-dst','Destination IP','send',input('s-dst','Any destination IP'))}${field('s-user','Subscriber','person',input('s-user','Exporter username'),'Matches identity reported by the exporter (IE 371).')}${field('s-proto','Protocol','swap_calls','<select id="s-proto" class="inp"><option>Any</option><option>TCP</option><option>UDP</option><option>ICMP</option></select>')}${field('s-case','Official case','inventory_2','<select id="s-case" class="inp"><option value="0">No linked case</option></select>','Links this search and generated reports to an official case.')}${field('s-reason','Audit reason','sticky_note_2',input('s-reason','Additional investigation reason'),'Recorded with your search in the audit trail.')}</div></details><div id="s-modal-msg" class="isp-form-error" role="alert"></div></div><div class="isp-modal-foot"><button type="button" id="s-clear" class="btn out">Clear filters</button><div class="logs-modal-submit"><button type="button" class="btn out" data-filter-close>Cancel</button><button type="submit" id="s-run" class="btn">${icon('search')}Search logs</button></div></div></form></section></div>`);
    $('#logs-open-filters').prop('disabled',false); devices();cases();
    $('#s-isp').on('change',()=>{devices();cases()});
    $('#s-from,#s-to').datetimepicker({ format:'Y-m-d H:i:s', step:1 });
    $('#logs-filter-modal [data-filter-close]').on('click',() => close());
    $('#logs-filter-modal').on('mousedown', e => { if (e.target.id === 'logs-filter-modal') close(); });
    $('#logs-form').on('submit',e => { e.preventDefault(); const b = read(); if (!validate(b)) return; c.applied = b; c.offset = 0; c.cache.clear(); close(false); chips(); search(true); });
    $('#s-clear').on('click',() => fill(emptyBody()));
    $('.logs-presets').on('click','button',function () { const minutes = +this.dataset.minutes, now = Date.now(); const ist = t => new Date(t + 330*60000).toISOString().slice(0,19).replace('T',' '); $('#s-from').val(minutes ? ist(now - minutes*60000) : ''); $('#s-to').val(minutes ? ist(now) : ''); $('.logs-presets button').removeClass('on').attr('aria-pressed','false'); $(this).addClass('on').attr('aria-pressed','true'); });
    $('.logs-presets [data-minutes=15]').trigger('click');
    $('#s-from,#s-to').on('input change',() => $('.logs-presets button').removeClass('on').attr('aria-pressed','false'));
    $(document).on('keydown.logs',e => {
      if ($('#logs-filter-modal').prop('hidden')) return;
      if (e.key === 'Escape') { e.preventDefault(); if ($('.xdsoft_datetimepicker:visible').length) $('#s-from,#s-to').datetimepicker('hide'); else close(); }
      if (e.key === 'Tab') { const nodes = $('#logs-filter-modal').find('button,input,select,summary').filter(':visible:enabled').toArray(), first = nodes[0], last = nodes.at(-1); if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last?.focus(); } else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first?.focus(); } }
    });
  }
  function dispose() { if (!c) return; cancel(false); c.inventory?.abort(); $('#s-from,#s-to').each(function () { if ($(this).data('xdsoft_datetimepicker')) $(this).datetimepicker('destroy'); }); c.kit.dispose(); $(c.root).off('.logs'); $(document).off('.logs'); c.cache.clear(); c = null; }
  window.YesLogsLogs = { mount, dispose, open };
})(jQuery);

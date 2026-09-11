#!/usr/bin/env python3
"""Bounded read-only search benchmark. Never generates or modifies production flows.

Runs the current hot count + page SQL, and an explicit archived Parquet day.
Uses local root MariaDB socket access for archive config; never prints credentials,
IPs, subscriber data, S3 paths or raw database errors. Each query has resource caps.
"""
import argparse
import concurrent.futures
import datetime as dt
import json
import math
import re
from pathlib import Path
import statistics
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

P = argparse.ArgumentParser(description=__doc__)
P.add_argument('--http', default='http://127.0.0.1:8123/')
P.add_argument('--database', default='natlogs')
P.add_argument('--control-database', default='natflow_cp')
P.add_argument('--isp', type=int, default=5)
P.add_argument('--device', type=int, default=8)
P.add_argument('--rounds', type=int, default=8)
P.add_argument('--concurrency', default='1,2,4,8')
P.add_argument('--output', required=True)
P.add_argument('--memory-mib', type=int, default=512)
P.add_argument('--seconds', type=int, default=12)
P.add_argument('--cold', action='store_true')
P.add_argument('--archive-isp', type=int)
P.add_argument('--archive-device', type=int)
P.add_argument('--archive-day', type=dt.date.fromisoformat)
P.add_argument('--archive-sample-window', action='store_true', help='Use the first archived record to choose a populated historical device/time window')
P.add_argument('--cases', default='', help='Comma-separated case names; blank runs all')
args = P.parse_args()
assert args.database.replace('_','').isalnum()
assert 1 <= args.rounds <= 100
assert 128 <= args.memory_mib <= 2048 and 1 <= args.seconds <= 30
levels = [int(n) for n in args.concurrency.split(',')]
assert all(1 <= n <= 8 for n in levels)

def sqlstr(value): return "'" + str(value).replace('\\','\\\\').replace("'","\\'") + "'"
def ch(sql, timeout=None):
    timeout=timeout or args.seconds
    params = {'default_format':'JSON','max_threads':2,'max_memory_usage':args.memory_mib*1024*1024,'max_execution_time':timeout,'log_queries':0}
    req = urllib.request.Request(args.http+'?'+urllib.parse.urlencode(params),data=sql.encode())
    try:
        with urllib.request.urlopen(req,timeout=timeout+5) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        body=e.read().decode(errors='replace')
        codes=re.findall(r'\(([A-Z0-9_]+)\)',body)
        raise RuntimeError('bounded query failed: '+(codes[-1] if codes else 'HTTP '+str(e.code))) from None
    except Exception as e:
        # SQL may include S3 credentials. Do not propagate server query text.
        raise RuntimeError('bounded query failed ('+type(e).__name__+')') from None

def mysql(sql):
    return subprocess.check_output(['mysql','-N','-r',args.control_database,'-e',sql],text=True).strip()

KEY='flow_start, flow_end, src_ip, src_port, dst_ip, dst_port, nat_public_ip, nat_public_port, protocol, bytes, packets, flow_type, nat_event, username'
SELECT='flow_start, device_id, src_ip, src_port, nat_public_ip, nat_public_port, dst_ip, dst_port, protocol, flow_type, username, nat_event'
base=f'isp_id={args.isp} AND device_id={args.device} AND nat_public_ip!=toIPv4(\'0.0.0.0\')'
meta=ch("SELECT sum(rows) AS rows, sum(bytes_on_disk) AS bytes FROM system.parts WHERE active AND database="+sqlstr(args.database)+" AND table='flow_logs'")['data'][0]
# A stable known time window prevents current ingestion changing comparison results.
sample=ch(f"SELECT flow_start,toString(nat_public_ip) AS ip,nat_public_port AS port FROM {args.database}.flow_logs WHERE {base} AND nat_public_port > 0 AND event_date >= today()-1 ORDER BY flow_start DESC LIMIT 1")['data'][0]
end=dt.datetime.fromisoformat(sample['flow_start'])

def window(minutes):
    start=end-dt.timedelta(minutes=minutes)
    return base+f" AND event_date BETWEEN toDate({sqlstr(str(start.date()))}) AND toDate({sqlstr(str(end.date()))}) AND flow_start BETWEEN toDateTime({sqlstr(str(start))}) AND toDateTime({sqlstr(str(end))})"

def hot(where, offset=0):
    return [f'SELECT count() AS count FROM (SELECT 1 FROM {args.database}.flow_logs WHERE {where} LIMIT 1 BY {KEY} LIMIT 100000)',f'SELECT {SELECT} FROM {args.database}.flow_logs WHERE {where} ORDER BY flow_start DESC LIMIT 1 BY {KEY} LIMIT 50 OFFSET {offset}']
cases={ 'hot_device_15m':hot(window(15)), 'hot_exact_ip_port_15m':hot(window(15)+f" AND nat_public_ip=toIPv4({sqlstr(sample['ip'])}) AND nat_public_port={sample['port']}"), 'hot_device_24h':hot(window(1440)), 'hot_device_page_20':hot(window(15),950) }
report={'timestamp_utc':dt.datetime.now(dt.timezone.utc).isoformat(),'stored_rows':int(meta['rows']),'stored_bytes':int(meta['bytes']),'scope':{'isp':args.isp,'device':args.device},'limits':{'max_threads_per_query':2,'max_memory_bytes':args.memory_mib*1024*1024,'max_query_seconds':args.seconds},'boundary':'Hot count + 50-row page, or S3 read capped at 5000 rows. Excludes HTTP authentication, archive planning, CRM and browser rendering. Existing caches are retained; not a cold-cache test.','results':[]}
if args.cold:
    cfg=json.loads(mysql("SELECT data FROM settings WHERE section='s3'"))
    days=mysql("SELECT day FROM archived_days"+(" WHERE day="+sqlstr(args.archive_day) if args.archive_day else '')+" ORDER BY day DESC LIMIT 1").splitlines()
    if cfg.get('enabled') and cfg.get('exportFormat')=='parquet' and days:
        day=dt.date.fromisoformat(days[0]); report['archive_day']=str(day)
        archive_isp=args.archive_isp or args.isp; archive_device=args.archive_device or args.device
        report['archive_scope']={'isp':archive_isp,'device':archive_device}
        url='/'.join(part.strip('/') for part in [cfg['endpoint'].rstrip('/'),cfg['bucket'],cfg.get('pathPrefix','')] if part)+f'/isp_id={archive_isp}/year={day.year:04d}/month={day.month:02d}/day={day.day:02d}/part-000.parquet'
        src=f"s3({sqlstr(url)},{sqlstr(cfg['accessKey'])},{sqlstr(cfg['secretKey'])},'Parquet')"
        archive_start=str(day)+' 23:45:00'; archive_end=str(day)+' 23:59:59'
        if args.archive_sample_window:
            first=ch(f"SELECT device_id,formatDateTime(flow_start,'%Y-%m-%d %H:%i:%S','Asia/Kolkata') AS flow_start FROM {src} WHERE nat_public_ip!='' AND nat_public_ip!='0.0.0.0' LIMIT 1")["data"][0]
            archive_device=int(first['device_id'])
            archive_start=str(first['flow_start']);archive_end=str(dt.datetime.fromisoformat(archive_start)+dt.timedelta(minutes=15))
            report['archive_scope']['device']=archive_device
        report['archive_window']={'from':archive_start,'to':archive_end}
        coldkey='flow_start, src_ip, src_port, dst_ip, dst_port, nat_public_ip, nat_public_port, protocol, flow_type'
        coldwhere=f"device_id={archive_device} AND nat_public_ip!='' AND nat_public_ip!='0.0.0.0'"
        cases['s3_device_day']=[f'SELECT flow_start,device_id,src_ip,src_port,nat_public_ip,nat_public_port,dst_ip,dst_port,protocol,flow_type FROM {src} WHERE {coldwhere} ORDER BY flow_start DESC LIMIT 1 BY {coldkey} LIMIT 5000 SETTINGS input_format_parquet_allow_missing_columns=1']
        narrow=coldwhere+f" AND flow_start BETWEEN toDateTime({sqlstr(archive_start)},'Asia/Kolkata') AND toDateTime({sqlstr(archive_end)},'Asia/Kolkata')"
        cases['s3_device_15m']=[f'SELECT flow_start,device_id,src_ip,src_port,nat_public_ip,nat_public_port,dst_ip,dst_port,protocol,flow_type FROM {src} WHERE {narrow} ORDER BY flow_start DESC LIMIT 1 BY {coldkey} LIMIT 5000 SETTINGS input_format_parquet_allow_missing_columns=1']
        if not args.cases or 's3_exact_ip_port_15m' in args.cases.split(','):
            try:
                archived_sample=ch(f'SELECT nat_public_ip,nat_public_port FROM {src} WHERE {narrow} AND nat_public_port > 0 LIMIT 1')['data']
                if archived_sample:
                    one=archived_sample[0]
                    exact=narrow+f" AND nat_public_ip={sqlstr(one['nat_public_ip'])} AND nat_public_port={int(one['nat_public_port'])}"
                    cases['s3_exact_ip_port_15m']=[f'SELECT flow_start,device_id,src_ip,src_port,nat_public_ip,nat_public_port,dst_ip,dst_port,protocol,flow_type FROM {src} WHERE {exact} ORDER BY flow_start DESC LIMIT 1 BY {coldkey} LIMIT 5000 SETTINGS input_format_parquet_allow_missing_columns=1']
                else: report['archive_exact_unavailable']='No endpoint with a positive port in the selected 15-minute window'
            except RuntimeError as error: report['archive_exact_unavailable']=str(error)

    else: report['archive_unavailable']='No configured Parquet archive day'

def run(queries):
    began=time.perf_counter(); scanned=0; result_rows=0
    try:
        for q in queries:
            d=ch(q);scanned+=int(d.get('statistics',{}).get('rows_read',0));result_rows=d.get('rows',0)
        return {'ms':round((time.perf_counter()-began)*1000,2),'read_rows':scanned,'result_rows':result_rows}
    except RuntimeError as e: return {'ms':round((time.perf_counter()-began)*1000,2),'error':str(e)}
for name, queries in cases.items():
    if args.cases and name not in args.cases.split(','): continue
    for concurrency in levels:
        # Stop escalating this case after any resource or query failure.
        n=args.rounds
        start=time.perf_counter(); samples=[]
        if name.startswith('s3'):
            samples.append(run(queries))
        remaining=0 if samples and 'error' in samples[0] else n-len(samples)
        with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
            samples.extend(pool.map(lambda _:run(queries),range(remaining)))
        seconds=time.perf_counter()-start; n=len(samples)
        values=sorted(s['ms'] for s in samples);failures=sum('error' in s for s in samples)
        row={'case':name,'concurrency':concurrency,'samples':n,'failures':failures,'p50_ms':round(statistics.median(values),2),'p95_ms':values[max(0,math.ceil(len(values)*.95)-1)],'max_ms':max(values),'searches_per_second':round(n/seconds,2),'max_read_rows':max(s.get('read_rows',0) for s in samples),'returned_rows':sorted(set(s.get('result_rows',0) for s in samples)), 'errors':sorted(set(s['error'] for s in samples if 'error' in s)), 'measurements':samples}
        report['results'].append(row);print(json.dumps({k:v for k,v in row.items() if k!='measurements'}),flush=True)
        Path(args.output).write_text(json.dumps(report,indent=2)+'\n')
        if failures: break
print('Benchmark report: '+args.output)

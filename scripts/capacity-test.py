#!/usr/bin/env python3
import json, subprocess, time, urllib.request, http.cookiejar
BG="/opt/yeslogs/natflow-dataplane/bin/benchgen"
TARGET="127.0.0.1:2055"; MET="http://127.0.0.1:9101/metrics"; API="http://127.0.0.1:8080"
cj=http.cookiejar.CookieJar(); op=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
def metric(k):
    for ln in op.open(MET).read().decode().splitlines():
        if ln.startswith(k+" "): return float(ln.split()[1])
    return 0.0
def login():
    r=op.open(urllib.request.Request(API+"/api/v1/login",
        data=json.dumps({"email":"admin@sayra.io","password":"REDACTED-PW"}).encode(),
        headers={"Content-Type":"application/json"})); return json.load(r)["csrf"]
def get_dp(): return json.load(op.open(API+"/api/v1/settings"))["settings"]["dataplane"]
def apply(csrf, dp, **kw):
    dp=dict(dp); dp.update(kw)
    op.open(urllib.request.Request(API+"/api/v1/settings/dataplane", data=json.dumps(dp).encode(),
        headers={"Content-Type":"application/json","X-CSRF-Token":csrf}, method="PUT")); time.sleep(4)
def measure(pps, senders, secs=42):
    p=subprocess.Popen([BG,"--target",TARGET,"--pps",str(pps),"--senders",str(senders),
        "--flows-per-packet","30","--duration",f"{secs}s","--dns-percent","25","--private-percent","15","--zero-byte-percent","5"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    samples=[]; peakq=0; time.sleep(10)
    for _ in range(int((secs-14)/3)):
        t=time.time(); samples.append((t,metric("flows_inserted_total"))); peakq=max(peakq,metric("current_queue_size")); time.sleep(3)
    p.wait()
    drp=metric("flows_dropped_total")
    rate=(samples[-1][1]-samples[0][1])/(samples[-1][0]-samples[0][0]) if len(samples)>1 else 0
    return rate, peakq, drp, float(open("/proc/loadavg").read().split()[0])
csrf=login(); base=get_dp()
print(f"{'workers':>7} {'batch':>8} {'queue':>9} {'gen(pps:s)':>11} {'sustained/s':>13} {'peakQ':>9} {'dropped':>9} {'load1':>6}")
best=(0,None)
runs=[(8,50000,1000000,15000,2),(12,50000,1000000,15000,2),(16,50000,1000000,15000,2),
      (16,100000,1500000,20000,2),(12,100000,1500000,20000,3)]
for workers,batch,queue,pps,sn in runs:
    apply(csrf, base, writerWorkers=workers, batchSize=batch, maxQueueRows=queue, backpressureMode="block")
    rate,peakq,drp,load1=measure(pps,sn)
    print(f"{workers:>7} {batch:>8,} {queue:>9,} {f'{pps}:{sn}':>11} {int(rate):>13,} {int(peakq):>9,} {int(drp):>9,} {load1:>6.2f}")
    if rate>best[0]: best=(rate,(workers,batch,queue))
print(f"\nPEAK sustained ingest on this 6-vCPU box (generator co-resident): {int(best[0]):,} flows/s @ {best[1]}")
apply(csrf, base, **base); print("restored baseline")

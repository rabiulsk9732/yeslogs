#!/usr/bin/env python3
"""Install a reviewed UI/gateway beside a running legacy collector, then activate.

Run prepare first, verify through an SSH tunnel to :80, then run activate.
The payload manifest pins all installed executables/files. No collector restart,
credential transfer, natlog config edit or existing-proxy overwrite is performed.
"""
import argparse
import fcntl
import hashlib
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import socket
import subprocess
import sys
import time
import urllib.request
import yaml

BACKUP = Path('/var/backups/yeslogs/console-fleet-v1.13')
STATE = BACKUP / 'deployment.json'
ROOT = Path('/srv/yeslogs-ui')
REPOSITORY = Path('/var/lib/yeslogs-ui/repository.git')
REMOTE = 'https://github.com/rabiulsk9732/yeslogs.git'


def run(args, **kw):
    return subprocess.run(args, check=True, **kw)


def output(args):
    return subprocess.check_output(args).decode().strip()


def write(path, data, mode=0o644):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_name(path.name + '.console-next')
    temp.touch(mode=mode)
    os.chmod(temp, mode)
    temp.write_text(data)
    os.replace(temp, path)


def collector():
    return output(['systemctl', 'show', 'natlog', '-p', 'MainPID', '-p', 'ExecMainStartTimestamp'])


def metrics():
    body = urllib.request.urlopen('http://127.0.0.1:9101/metrics', timeout=10).read().decode()
    names = {'packets_received_total', 'packets_dropped_total', 'flows_inserted_total', 'flows_dropped_total', 'spool_lost_total'}
    return {p[0]: float(p[1]) for line in body.splitlines() if len(p := line.split()) == 2 and p[0] in names}


def fingerprints(db):
    tables = output(['mariadb', '--batch', '--skip-column-names', db, '-e', 'SHOW TABLES']).splitlines()
    queries = {'isps': 'SELECT * FROM isps ORDER BY id', 'accounts': 'SELECT * FROM users ORDER BY id',
               'devices': 'SELECT * FROM devices ORDER BY id', 'policies': 'SELECT * FROM capture_policies ORDER BY id',
               'settings': 'SELECT * FROM settings ORDER BY section'}
    if 'isp_profiles' in tables:
        queries['profiles'] = 'SELECT * FROM isp_profiles ORDER BY isp_id'
    return {name: hashlib.sha256(subprocess.check_output(['mariadb', '--batch', '--raw', '--skip-column-names', db, '-e', q])).hexdigest() for name, q in queries.items()}


def verify_collector(state):
    assert collector() == state['collector'], 'Collector PID/start time changed'
    now = fingerprints(state['database'])
    assert all(now[k] == v for k, v in state['fingerprints'].items()), 'Existing control-plane records changed'
    assert hashlib.sha256(Path('/usr/local/bin/natlog').read_bytes()).hexdigest() == state['collector_sha256'], 'Collector binary changed'


def get(url):
    return json.load(urllib.request.urlopen(url, timeout=15))


def prepare(args):
    assert not STATE.exists(), 'Bootstrap already prepared; inspect saved state before continuing'
    manifest = json.loads((args.payload / 'manifest.json').read_text())
    revision = manifest['revision']
    assert re.fullmatch('[0-9a-f]{40}', revision)
    for name, digest in manifest['files'].items():
        path = (args.payload / name).resolve()
        assert path.is_relative_to(args.payload.resolve()) and path.is_file()
        assert hashlib.sha256(path.read_bytes()).hexdigest() == digest, 'Payload checksum mismatch: ' + name
    assert ipaddress.ip_address(args.host).version == 4, 'Use the verified public IPv4 address'
    assert not Path('/etc/caddy/Caddyfile').exists(), 'Existing proxy requires an explicit integration'
    assert not ROOT.joinpath('current').exists(), 'Existing static deployment must use its release pipeline'
    assert collector() == (BACKUP / 'collector-before.txt').read_text().strip()
    for port in [80, 8083]:
        with socket.socket() as probe:
            probe.bind(('0.0.0.0', port))
    raw = Path('/usr/local/bin/natlog').read_bytes()
    baseline = re.findall(rb'vcs.revision=([0-9a-f]{40})', raw)
    assert baseline and len(set(baseline)) == 1
    assert b'vcs.modified=false' in raw
    cfg = yaml.safe_load(Path('/etc/natlog/natlog.yaml').read_text())
    cp = cfg['cp']
    assert cp['bind'] in ['0.0.0.0:8080', ':8080'], 'Unexpected collector HTTP bind'
    assert cp['session_key']
    db = re.search(r'\)/([A-Za-z0-9_]+)(?:\?|$)', cp['mysql_dsn']).group(1)
    state = {'revision': revision, 'host': args.host, 'database': db, 'collector': collector(),
             'backend_revision': baseline[0].decode(), 'collector_sha256': hashlib.sha256(raw).hexdigest(),
             'fingerprints': fingerprints(db), 'metrics_before': metrics(), 'stage': 'backed-up'}
    with (BACKUP / 'control-plane.sql').open('wb') as target:
        os.chmod(target.name, 0o600)
        run(['mariadb-dump', '--single-transaction', '--routines', '--triggers', '--events', db], stdout=target)
    write(STATE, json.dumps(state, indent=2), 0o600)
    for name, destination in {'caddy': '/usr/local/bin/caddy', 'console-release.py': '/usr/local/lib/yeslogs-ui/console-release.py'}.items():
        dest = Path(destination); dest.parent.mkdir(parents=True, exist_ok=True)
        assert not dest.exists(), 'Existing executable needs review: ' + destination
        shutil.copyfile(args.payload / name, dest); dest.chmod(0o755)
    release = Path('/usr/local/lib/yeslogs-management/releases') / revision
    release.mkdir(parents=True)
    shutil.copyfile(args.payload / 'director', release / 'director'); (release / 'director').chmod(0o755)
    Path('/usr/local/lib/yeslogs-management/8083').symlink_to(Path('releases') / revision)
    dsn = cp['mysql_dsn']
    if 'parseTime=' not in dsn:
        dsn += ('&' if '?' in dsn else '?') + 'parseTime=true'
    assert not cp.get('cookie_secure', False), 'HTTPS configuration requires a separate site setup'
    management = {'bind': '127.0.0.1:8083', 'upstream': 'http://127.0.0.1:8080', 'mysql_dsn': dsn,
                  'session_key': cp['session_key'], 'cookie_secure': False, 'flow_days': cp.get('flow_days', 1)}
    write('/etc/natlog/management-8083.yaml', yaml.safe_dump(management), 0o600)
    REPOSITORY.parent.mkdir(parents=True, exist_ok=True)
    run(['git', 'init', '--bare', str(REPOSITORY)], stdout=subprocess.DEVNULL)
    run(['git', '-C', str(REPOSITORY), 'remote', 'add', 'origin', REMOTE])
    run(['git', '-C', str(REPOSITORY), 'fetch', '--atomic', '--no-tags', 'origin',
         '+refs/heads/main:refs/remotes/origin/main', '+refs/heads/console-live:refs/remotes/origin/console-live'])
    spec = importlib.util.spec_from_file_location('console_release', '/usr/local/lib/yeslogs-ui/console-release.py')
    helper = importlib.util.module_from_spec(spec); spec.loader.exec_module(helper)
    helper.git(REPOSITORY, 'merge-base', '--is-ancestor', revision, 'refs/remotes/origin/console-live')
    helper.check_backend(REPOSITORY, state['backend_revision'], revision, revision)
    helper.build(REPOSITORY, revision, ROOT / 'releases' / revision)
    ROOT.joinpath('current').symlink_to(Path('releases') / revision)
    snippet = (args.payload / 'console-static.caddy').read_text().replace('127.0.0.1:8081', '127.0.0.1:8083')
    write('/etc/caddy/console-static.caddy', snippet)
    write('/etc/caddy/Caddyfile', '{\n auto_https off\n admin 127.0.0.1:2019\n}\nimport /etc/caddy/console-static.caddy\n:80 {\n encode gzip\n import yeslogs_console_static\n}\n')
    Path('/var/lib/caddy').mkdir(parents=True, exist_ok=True)
    units = {'yeslogs-fleet-caddy.service': 'caddy.service', 'yeslogs-fleet-management.service': 'yeslogs-fleet-management.service',
             'yeslogs-fleet-console-route.service': 'yeslogs-fleet-console-route.service',
             'yeslogs-ui-fetch.service': 'yeslogs-ui-fetch.service', 'yeslogs-ui-fetch.timer': 'yeslogs-ui-fetch.timer'}
    for src, dest in units.items():
        target = Path('/etc/systemd/system') / dest
        assert not target.exists(), 'Existing service requires review: ' + dest
        write(target, (args.payload / src).read_text())
    write('/etc/natlog/console-route.nft', (args.payload / 'console-route.nft').read_text())
    config = {'remote': REMOTE, 'repository': str(REPOSITORY), 'root': str(ROOT),
              'backend_revision': state['backend_revision'], 'management_revision': revision,
              'management_probe_url': 'http://127.0.0.1:8083/api/v1/management-version',
              'probe_url': 'http://127.0.0.1', 'probe_host': args.host}
    write('/etc/natlog/console-release.json', json.dumps(config, indent=2) + '\n', 0o600)
    run(['systemctl', 'daemon-reload'])
    run(['systemctl', 'start', 'yeslogs-fleet-management.service'])
    for attempt in range(20):
        try:
            assert get(config['management_probe_url'])['revision'] == revision
            break
        except (OSError, AssertionError):
            if attempt == 19: raise
            time.sleep(0.25)
    run(['/usr/local/bin/caddy', 'validate', '--config', '/etc/caddy/Caddyfile'], stdout=subprocess.DEVNULL)
    run(['systemctl', 'start', 'caddy.service'])
    run(['nft', '--check', '--file', '/etc/natlog/console-route.nft'])
    assert get('http://127.0.0.1/ui-version.json')['revision'] == revision
    verify_collector(state)
    state['stage'] = 'prepared'; write(STATE, json.dumps(state, indent=2), 0o600)
    print(json.dumps({'stage': 'prepared', 'host': args.host, 'revision': revision, 'collectorUnchanged': True}))


def activate():
    state = json.loads(STATE.read_text())
    assert state['stage'] == 'prepared', 'Prepare and verify candidate first'
    verify_collector(state)
    # The approved static UI may advance independently of the installed gateway.
    run(['systemctl', 'start', 'yeslogs-ui-fetch.service'])
    assert get('http://127.0.0.1:8083/api/v1/management-version')['revision'] == state['revision']
    state['ufw_before'] = output(['ufw', 'status'])
    if 'Status: active' in state['ufw_before']:
        run(['ufw', 'allow', '80/tcp', 'comment', 'YesLogs static console'])
    run(['systemctl', 'enable', 'caddy.service', 'yeslogs-fleet-management.service'])
    run(['systemctl', 'enable', '--now', 'yeslogs-fleet-console-route.service'])
    run(['systemctl', 'enable', '--now', 'yeslogs-ui-fetch.timer'])
    run(['systemctl', 'start', 'yeslogs-ui-fetch.service'])
    verify_collector(state)
    state['stage'] = 'active'; state['metrics_after'] = metrics(); state['ui_revision'] = get('http://127.0.0.1/ui-version.json')['revision']
    write(STATE, json.dumps(state, indent=2), 0o600)
    print(json.dumps({'stage': 'active', 'host': state['host'], 'revision': state['ui_revision'], 'collectorUnchanged': True}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('phase', choices=['prepare', 'activate'])
    parser.add_argument('--payload', type=Path)
    parser.add_argument('--host')
    args = parser.parse_args()
    assert os.geteuid() == 0, 'Run as root'
    BACKUP.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (BACKUP / 'bootstrap.lock').open('w') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.phase == 'prepare':
            assert args.payload and args.host
            prepare(args)
        else:
            activate()


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        # DSNs and session keys must not appear in command logs or tracebacks.
        import traceback
        write(BACKUP / 'bootstrap-error.txt', traceback.format_exc(), 0o600)
        print(type(error).__name__ + ': bootstrap failed; inspect the protected state and service logs.', file=sys.stderr)
        sys.exit(1)

#!/usr/bin/env python3
"""Start each binary on a fresh seed copy, sign in as david, save identity and gzip bodies of each route."""
import json, os, shutil, socket, sqlite3, subprocess, sys, tempfile, time, urllib.request, gzip
from pathlib import Path
root, out = Path(sys.argv[1]), Path(sys.argv[2]); apps = dict(a.split('=', 1) for a in sys.argv[3:])
rust = root / 'rust'; seed = rust / 'parity/.seed/default'; labels = json.loads((seed / 'labels.json').read_text())
lg = rust / 'target/bench/release/loadgen'
env = {k: v for k, v in os.environ.items() if not k.startswith(('CAMPFIRE_', 'THRUSTER_'))}
for line in (rust / 'parity/.env.reference').read_text().splitlines():
    if '=' in line and not line.startswith('#'): k, v = line.split('=', 1); env[k] = v
env |= dict(DISABLE_SSL='1', GOMAXPROCS='4', RAILS_ENV='production', CAMPFIRE_FROZEN_TIME='2026-03-02T16:00:00Z')
def port():
    s = socket.socket(); s.bind(('127.0.0.1', 0)); p = s.getsockname()[1]; s.close(); return p
room = labels['rooms.watercooler']
paths = dict(room_show=f'/rooms/{room}', messages_page=f'/rooms/{room}/messages?before={labels["messages.busy_060"]}', sidebar='/users/me/sidebar', search='/searches?q=coffee')
out.mkdir(parents=True, exist_ok=True)
for app, binary in apps.items():
    with tempfile.TemporaryDirectory() as tmp:
        tmp = Path(tmp); db = tmp / 'db/production.sqlite3'; db.parent.mkdir()
        with sqlite3.connect(f'file:{seed}/db/production.sqlite3?mode=ro', uri=True) as s, sqlite3.connect(db) as d: s.backup(d)
        shutil.copytree(seed / 'storage', tmp / 'files')
        front = port()
        e = env | dict(CAMPFIRE_STORAGE_PATH=str(tmp), CAMPFIRE_DATABASE_PATH=str(db), CAMPFIRE_FILES_PATH=str(tmp / 'files'), TARGET_PORT=str(port()), HTTP_PORT=str(front))
        proc = subprocess.Popen([binary, 'server'], cwd=tmp, env=e, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        base = f'http://127.0.0.1:{front}'
        try:
            for _ in range(300):
                try: urllib.request.urlopen(base + '/up', timeout=1).read(); break
                except OSError: time.sleep(0.05)
            cookie = json.loads(subprocess.check_output([str(lg), 'login', '--base', base, '--email', labels['emails.david'], '--password', labels['passwords.all']]))['cookie']
            for name, path in paths.items():
                for enc in ('identity', 'gzip', 'gzip'):
                    req = urllib.request.Request(base + path, headers={'Cookie': cookie, 'Accept-Encoding': enc})
                    with urllib.request.urlopen(req) as r:
                        body = r.read(); hdr = dict(r.headers)
                    if hdr.get('Content-Encoding') == 'gzip': body = gzip.decompress(body)
                    (out / f'{app}-{name}-{enc}.html').write_bytes(body)
                    (out / f'{app}-{name}-{enc}.headers').write_text(json.dumps({k: v for k, v in hdr.items() if k not in ('Date', 'X-Request-Id', 'X-Runtime')}, indent=1, sort_keys=True))
        finally:
            proc.terminate(); proc.wait(10)
print('saved to', out)

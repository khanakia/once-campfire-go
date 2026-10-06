#!/usr/bin/env python3
"""macOS A/B bench for the Campfire Go and Rust binaries, shaped like DHH's harness.

Each app runs on a fresh copy of the Rust parity seed; requests go to the public HTTP_PORT (the
front server, as in once-campfire-elixir/bench/run) through the Rust loadgen. On macOS there is no
CPU pinning, so besides req/s it records the server process's CPU time per successful request
(ps cputime delta), which is comparable across apps whatever core count they use.

usage: macbench.py ROOT APP=BINARY [APP=BINARY...] [--gzip 1] [--conc 16] [--secs 5] [--reps 2]
"""
import argparse, json, os, shutil, socket, sqlite3, subprocess, tempfile, time, urllib.request
from pathlib import Path


def free_port():
    s = socket.socket(); s.bind(('127.0.0.1', 0)); p = s.getsockname()[1]; s.close(); return p


def cpu_seconds(pid):
    out = subprocess.check_output(['ps', '-o', 'time=', '-p', str(pid)], text=True).strip()
    parts = out.replace('-', ':').split(':')
    total = 0.0
    for p in parts:
        total = total * 60 + float(p)
    return total


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('root', type=Path, help='session root holding rust/')
    ap.add_argument('apps', nargs='+')
    ap.add_argument('--gzip', type=int, nargs='+', default=[1, 0])
    ap.add_argument('--conc', type=int, nargs='+', default=[1, 16])
    ap.add_argument('--secs', type=float, default=5)
    ap.add_argument('--reps', type=int, default=2)
    ap.add_argument('--routes', nargs='+', default=['room_show', 'messages_page', 'sidebar', 'search'])
    ap.add_argument('--threads', default='4', help='GOMAXPROCS / TOKIO_WORKER_THREADS')
    ap.add_argument('--out', type=Path)
    a = ap.parse_args()
    rust = a.root / 'rust'
    seed = rust / 'parity/.seed/default'
    labels = json.loads((seed / 'labels.json').read_text())
    lg = rust / 'target/bench/release/loadgen'
    env = {k: v for k, v in os.environ.items() if not k.startswith(('CAMPFIRE_', 'THRUSTER_'))}
    for line in (rust / 'parity/.env.reference').read_text().splitlines():
        if '=' in line and not line.startswith('#'):
            k, v = line.split('=', 1); env[k] = v
    env |= dict(DISABLE_SSL='1', GOMAXPROCS=a.threads, TOKIO_WORKER_THREADS=a.threads, RAILS_ENV='production')
    apps = dict(x.split('=', 1) for x in a.apps)
    rows = []
    for rep in range(a.reps):
        order = list(apps) if rep % 2 == 0 else list(reversed(apps))
        for app in order:
            with tempfile.TemporaryDirectory(prefix=f'{app}-') as tmp:
                tmp = Path(tmp); db = tmp / 'db/production.sqlite3'; db.parent.mkdir()
                with sqlite3.connect(f'file:{seed}/db/production.sqlite3?mode=ro', uri=True) as s, sqlite3.connect(db) as d:
                    s.backup(d)
                shutil.copytree(seed / 'storage', tmp / 'files')
                target, front = free_port(), free_port()
                runenv = env | dict(CAMPFIRE_STORAGE_PATH=str(tmp), CAMPFIRE_DATABASE_PATH=str(db), CAMPFIRE_FILES_PATH=str(tmp / 'files'),
                                    STORAGE_PATH=str(tmp), TARGET_PORT=str(target), HTTP_PORT=str(front))
                log = open(tmp / 'server.log', 'w')
                proc = subprocess.Popen([apps[app], 'server'], cwd=tmp, env=runenv, stdout=log, stderr=subprocess.STDOUT)
                base = f'http://127.0.0.1:{front}'
                try:
                    t0 = time.monotonic()
                    while True:
                        if proc.poll() is not None:
                            raise SystemExit(f'{app} exited:\n' + (tmp / 'server.log').read_text()[-3000:])
                        try:
                            urllib.request.urlopen(base + '/up', timeout=1).read(); break
                        except OSError:
                            if time.monotonic() - t0 > 30: raise SystemExit('startup timeout')
                            time.sleep(0.05)

                    def load(cmd, **o):
                        args = [str(lg), cmd, '--base', base]
                        for k, v in o.items(): args += ['--' + k.replace('_', '-'), str(v)]
                        return json.loads(subprocess.check_output(args, text=True, stderr=subprocess.DEVNULL, timeout=300))

                    cookie = load('login', email=labels['emails.david'], password=labels['passwords.all'])['cookie']
                    room = labels['rooms.watercooler']
                    paths = dict(room_show=f'/rooms/{room}', messages_page=f'/rooms/{room}/messages?before={labels["messages.busy_060"]}',
                                 sidebar='/users/me/sidebar', search='/searches?q=coffee')
                    for name in a.routes:
                        for gz in a.gzip:
                            load('http', path=paths[name], cookie=cookie, conc=4, duration=1.5, gzip=gz)  # warm-up
                            for c in a.conc:
                                before = cpu_seconds(proc.pid)
                                r = load('http', path=paths[name], cookie=cookie, conc=c, duration=a.secs, gzip=gz)
                                cpu = cpu_seconds(proc.pid) - before
                                bad = r['errors'] or set(r['statuses']) != {'200'}
                                row = dict(app=app, rep=rep, route=name, gzip=gz, conc=c, rps=r['rps'], ok=r['ok'],
                                           p50=r['latency'].get('p50_ms'), p99=r['latency'].get('p99_ms'),
                                           cpu_us=round(cpu * 1e6 / max(1, r['ok']), 1), bad=bool(bad))
                                rows.append(row)
                                print(json.dumps(row), flush=True)
                finally:
                    proc.terminate(); proc.wait(10)
    if a.out:
        a.out.write_text(json.dumps(rows, indent=1))
    # median summary
    from statistics import median
    keys = sorted({(r['route'], r['gzip'], r['conc']) for r in rows})
    print('\n| route | gzip | c | ' + ' | '.join(f'{x} req/s | {x} CPU µs' for x in apps) + ' |')
    print('|---|---|---|' + '---:|---:|' * len(apps))
    for k in keys:
        cells = []
        for app in apps:
            rs = [r for r in rows if (r['route'], r['gzip'], r['conc']) == k and r['app'] == app]
            cells += [f"{median(r['rps'] for r in rs):,.0f}", f"{median(r['cpu_us'] for r in rs):,.0f}"]
        print(f'| {k[0]} | {k[1]} | {k[2]} | ' + ' | '.join(cells) + ' |')


main()

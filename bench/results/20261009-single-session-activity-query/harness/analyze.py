#!/usr/bin/env python3
"""Summarise the alternating blocks: per candidate and profile, median and min-max req/s per route
against the baseline blocks of the same comparison, peak memory per round, validation totals."""
import json, os, re, statistics, sys
from collections import defaultdict

S = os.path.dirname(os.path.abspath(__file__))
RAW = sys.argv[1] if len(sys.argv) > 1 else os.path.join(S, 'results-raw')
ROUTES = ['room_show', 'messages_page', 'sidebar', 'search', 'post_message']
NAMES = {'room_show': 'Room page', 'messages_page': 'Messages page', 'sidebar': 'Sidebar', 'search': 'Search', 'post_message': 'Post a message'}

blocks = sorted(d for d in os.listdir(RAW) if re.match(r'\d\d-(normal|mixed)-(main|a|b)$', d))
mem = defaultdict(dict)  # (label) -> container -> (hwm_kb, cgroup_peak)
MEM = os.path.join(RAW, '..', 'logs', 'mem.log') if os.path.exists(os.path.join(RAW, '..', 'logs', 'mem.log')) else os.path.join(S, 'mem.log')
for line in open(MEM):
    parts = line.split()
    label, cid = parts[1], parts[2]
    kv = dict(p.split('=', 1) for p in parts[3:] if '=' in p)
    if not kv.get('hwm_kb'):
        continue  # process already gone: its cgroup path was empty, so memory.peak read the root cgroup
    hwm = int(kv['hwm_kb']); peak = int(kv.get('cgroup_peak') or 0)
    old = mem[label].get(cid, (0, 0))
    mem[label][cid] = (max(old[0], hwm), max(old[1], peak))

def comparison(profile, cand):
    # Blocks run in quartets main,X,X,main; a candidate's comparison is every quartet whose
    # middle blocks are that candidate, so each candidate is compared only with its own baselines.
    mine = [b for b in blocks if b.split('-')[1] == profile]
    sel = []
    for i in range(0, len(mine) - 3, 4):
        quartet = mine[i:i + 4]
        if quartet[1].endswith('-' + cand) and quartet[2].endswith('-' + cand):
            sel += quartet
    return sel

out = {}
for profile in ['normal', 'mixed']:
    for cand in ['a', 'b']:
        if not any(b.split('-')[1] == profile and b.endswith('-' + cand) for b in blocks):
            continue
        sel = comparison(profile, cand)
        samples = defaultdict(lambda: defaultdict(list))
        peaks = defaultdict(list)
        totals = defaultdict(lambda: {'timed_ok': 0, 'invalid': 0, 'errors': 0, 'gen_cpu_max': 0.0, 'writes': 0, 'writer_rates': []})
        for b in sel:
            who = 'base' if b.endswith('-main') else 'cand'
            for f in sorted(os.listdir(os.path.join(RAW, b))):
                if not re.match(r'go-\d+\.json$', f):
                    continue
                row = json.load(open(os.path.join(RAW, b, f)))
                for item in row['mixed_http' if profile == 'mixed' else 'http']:
                    samples[who][item['route']].append(item['rps'])
                    t = totals[who]
                    t['timed_ok'] += item['ok']; t['invalid'] += item['invalid_responses']; t['errors'] += item['errors']
                    t['gen_cpu_max'] = max(t['gen_cpu_max'], item.get('generator_cpu_percent', 0))
                    if 'writer' in item:
                        t['writes'] += item['writer']['ok']; t['writer_rates'].append(item['writer'].get('rps', 0))
            for cid, (hwm, peak) in mem.get(b, {}).items():
                peaks[who].append((hwm, peak))
        out[(profile, cand)] = (sel, samples, peaks, totals)

def fmt(v):
    return f'{v:,.0f}'

for (profile, cand), (sel, samples, peaks, totals) in out.items():
    print(f'\n## {profile} profile, candidate {cand.upper()} (blocks {", ".join(sel)})\n')
    print('| Route | Base median (min–max) | Candidate median (min–max) | Change | n |')
    print('|---|---:|---:|---:|---:|')
    for r in ROUTES:
        b, c = samples['base'].get(r), samples['cand'].get(r)
        if not b or not c:
            continue
        mb, mc = statistics.median(b), statistics.median(c)
        print(f'| {NAMES[r]} | {fmt(mb)} ({fmt(min(b))}–{fmt(max(b))}) | {fmt(mc)} ({fmt(min(c))}–{fmt(max(c))}) | {100*(mc/mb-1):+.1f}% | {len(b)}/{len(c)} |')
    print()
    print('| Peak memory per round | Base | Candidate |')
    print('|---|---:|---:|')
    for i, name in [(0, 'Server process peak RSS (VmHWM), median (min–max) MiB'), (1, 'Container cgroup memory.peak, median (min–max) MiB')]:
        cells = []
        for who in ['base', 'cand']:
            vals = [p[i] / (1024 if i == 0 else 1024 * 1024) for p in peaks[who]]
            cells.append(f'{statistics.median(vals):.1f} ({min(vals):.1f}–{max(vals):.1f}), {len(vals)} rounds' if vals else 'n/a')
        print(f'| {name} | {cells[0]} | {cells[1]} |')
    for who in ['base', 'cand']:
        t = totals[who]
        extra = f", timed mixed writes {t['writes']}, writer req/s {min(t['writer_rates']):.1f}–{max(t['writer_rates']):.1f}" if t['writer_rates'] else ''
        print(f"\n{who}: validated timed responses {t['timed_ok']:,}, invalid {t['invalid']}, errors {t['errors']}, peak generator CPU {t['gen_cpu_max']:.1f}%{extra}")

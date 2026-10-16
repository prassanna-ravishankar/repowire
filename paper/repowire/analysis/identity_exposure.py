#!/usr/bin/env python3
"""Retrospective address-key exposure audit, not a replay of registration rules.

Read a SQLite backup with mode=ro&immutable=1. Never export message text, paths,
peer ids, names, session ids, or credentials. All outputs are aggregate counts.
See README.md for assumptions, denominators, censoring and limitations.
"""
import argparse
import collections
import datetime as dt
import hashlib
import itertools
import json
import re
from pathlib import Path
import sqlite3
from zoneinfo import ZoneInfo

UTC = dt.timezone.utc

def timestamp(value):
    return dt.datetime.fromisoformat(value.replace('Z', '+00:00')).astimezone(UTC)

def iso(value):
    return value.isoformat().replace('+00:00', 'Z')

def derived_base_name(path, backend):
    # registry.go baseFolder/sanitizeFolder/buildDisplayName default branch.
    # Preferred names are not reconstructible from the retained mappings.
    folder = path.rstrip('/').rsplit('/', 1)[-1]
    folder = re.sub('-+', '-', re.sub('[^a-zA-Z0-9._-]', '-', folder)).strip('-') or 'peer'
    return folder + '-' + backend

def load(conn, start):
    events = [dict(r) for r in conn.execute(
        'SELECT event_id,type,timestamp,peer_id,peer_name,session_id,payload_json FROM events')]
    for e in events:
        e['time'] = timestamp(e['timestamp'])
    events.sort(key=lambda e: (e['time'], e['event_id']))
    end = max(e['time'] for e in events)
    online = [e for e in events if e['type'] == 'peer_online' and e['time'] >= start]
    peers = {e['peer_id'] for e in online if e['peer_id']}
    mappings = {r['session_id']: dict(r) for r in conn.execute('SELECT * FROM peer_session_mappings')}
    bindings = collections.defaultdict(list)
    for r in conn.execute('SELECT peer_id,backend,runtime_session_id,project_path,created_at,last_seen_at FROM session_bindings'):
        if r['peer_id']:
            bindings[r['peer_id']].append(dict(r))
    names = {}
    name_history = collections.defaultdict(set)
    for e in online:
        if e['peer_name']:
            names[e['peer_id']] = e['peer_name']
            name_history[e['peer_id']].add(e['peer_name'])
    for pid, m in mappings.items():
        if m['display_name']:
            names[pid] = m['display_name']
    keys = {p: {} for p in ['name','name_circle','name_path','backend_session','peer_id','base_name','base_circle','base_path']}
    multibinding = set()
    for pid in peers | set(mappings) | set(bindings):
        keys['peer_id'][pid] = (pid,)
        name = names.get(pid)
        if name:
            keys['name'][pid] = (name,)
        m = mappings.get(pid)
        if m and name and m['circle'] is not None:
            keys['name_circle'][pid] = (name, m['circle'])
        if m and name and m['path']:
            keys['name_path'][pid] = (name, m['path'])
        if m and m['path'] and m['backend']:
            base = derived_base_name(m['path'], m['backend'])
            keys['base_name'][pid] = (base,)
            keys['base_path'][pid] = (base, m['path'])
            if m['circle'] is not None:
                keys['base_circle'][pid] = (base, m['circle'])
        session_keys = {(b['backend'],b['runtime_session_id']) for b in bindings[pid]
                        if b['backend'] and b['runtime_session_id']}
        if len(session_keys) == 1:
            keys['backend_session'][pid] = next(iter(session_keys))
        elif len(session_keys) > 1:
            multibinding.add(pid)
    # All resolved attempts, not successful deliveries. Earliest resolution per
    # trace+kind, breaking timestamp ties by seq and row id. No text is selected.
    traces = [dict(r) for r in conn.execute(
        'SELECT id,trace_id,kind,stage,status,peer_id,ts,seq FROM delivery_traces')]
    for t in traces:
        t['time'] = timestamp(t['ts'])
    traces.sort(key=lambda t: (t['time'], t['seq'], t['id']))
    attempts = {}
    for t in traces:
        if start <= t['time'] <= end and t['stage'] == 'resolved_peer':
            attempts.setdefault((t['kind'], t['trace_id']), t)
    return events, online, peers, mappings, bindings, keys, list(attempts.values()), {
        'start': iso(start), 'end': iso(end),
        'peer_online_rows': len(online), 'online_peer_ids': len(peers),
        'online_session_column_equals_peer_id': sum(e['session_id'] == e['peer_id'] for e in online),
        'empty_online_payloads': sum(not json.loads(e['payload_json']) for e in online),
        'online_peers_with_retained_mapping': len(peers & mappings.keys()),
        'online_peers_with_multiple_observed_names': sum(len(name_history[p]) > 1 for p in peers),
        'mapped_names_different_from_last_online_name': sum(bool(name_history[p]) and m['display_name'] not in name_history[p] for p,m in mappings.items() if p in peers),
        'online_peers_with_multiple_binding_keys': len(peers & multibinding),
        'resolved_attempts': len(attempts),
        'attempt_kinds': dict(sorted(collections.Counter(t['kind'] for t in attempts.values()).items())),
        'static_attribute_warning': 'Snapshot mappings and bindings are not event-time registration evidence. Binding created_at/last_seen_at are observation bounds, not assignment validity intervals.',
    }, end

def intervals_for(events, peers, start, end, tie_mode='exclude'):
    grouped = collections.defaultdict(set)
    for e in events:
        if start <= e['time'] <= end and e['peer_id'] in peers and e['type'] in ('peer_online','peer_offline','peer_reaped'):
            grouped[(e['time'], e['peer_id'])].add(e['type'])
    opened, spans = {}, collections.defaultdict(list)
    ties = 0
    for (time, pid), kinds in sorted(grouped.items()):
        online = 'peer_online' in kinds
        closing = bool(kinds & {'peer_offline','peer_reaped'})
        if online and closing:
            ties += 1
            if pid in opened and opened[pid] < time:
                spans[pid].append((opened[pid],time))
            opened.pop(pid,None)
            if tie_mode == 'online_last':
                opened[pid] = time
        elif closing:
            if pid in opened and opened[pid] < time:
                spans[pid].append((opened[pid],time))
            opened.pop(pid,None)
        elif online:
            opened.setdefault(pid,time)  # repeated registration is not a new session
    censored = len(opened)
    for pid,time in opened.items():
        if time < end:
            spans[pid].append((time,end))
    return spans, {'conflicting_peer_timestamps': ties, 'right_censored_peers': censored,
                   'online_intervals': sum(len(v) for v in spans.values()),'tie_policy':tie_mode}

def audit_policy(keys, peers, spans, attempts):
    points = collections.defaultdict(lambda: [set(),set(),[]])
    for pid,ranges in spans.items():
        if pid not in keys:
            continue
        for start,end in ranges:
            points[start][1].add(pid)
            points[end][0].add(pid)
    for a in attempts:
        points[a['time']][2].append(a)
    live = collections.defaultdict(set)
    pairs, sets = set(), set()
    pair_micros = key_micros = 0
    exposed = target_live = covered = no_target_interval = 0
    unknown_competitor_policy = 'Only peers with computable keys can compete; uncomputable keys are excluded.'
    times = sorted(points)
    for i,time in enumerate(times):
        endings,starts,deliveries = points[time]
        for pid in endings:
            live[keys[pid]].discard(pid)
        for pid in starts:
            live[keys[pid]].add(pid)
        for a in deliveries:
            pid = a['peer_id']
            if pid not in keys:
                continue
            covered += 1
            group = live[keys[pid]]
            if pid not in group:
                no_target_interval += 1
                continue
            target_live += 1
            if len(group) > 1:
                exposed += 1
        if i+1 == len(times):
            continue
        delta = times[i+1]-time
        micros = (delta.days*86400 + delta.seconds)*1000000 + delta.microseconds
        if micros <= 0:
            continue
        for group in live.values():
            if len(group) < 2:
                continue
            sets.add(frozenset(group))
            pairs.update(itertools.combinations(sorted(group),2))
            key_micros += micros
            pair_micros += micros * (len(group)*(len(group)-1)//2)
    ids_per_key = collections.defaultdict(set)
    for pid in peers:
        if pid in keys:
            ids_per_key[keys[pid]].add(pid)
    counts = [len(v) for v in ids_per_key.values()]
    return {
        'online_peers_with_key': len(peers & keys.keys()),
        'distinct_keys': len(counts),
        'keys_with_multiple_peer_ids_over_window': sum(n>1 for n in counts),
        'fraction_keys_with_multiple_peer_ids': round(sum(n>1 for n in counts)/len(counts),6) if counts else None,
        'excess_peer_ids_over_distinct_keys': sum(n-1 for n in counts),
        'max_peer_ids_per_key': max(counts,default=0),
        'concurrent_peer_pairs': len(pairs), 'distinct_concurrent_peer_sets': len(sets),
        'pair_overlap_seconds': pair_micros/1000000, 'key_collision_seconds': key_micros/1000000,
        'attempts_with_key': covered, 'attempts_with_online_target_and_key': target_live,
        'attempts_without_observed_online_target': no_target_interval,
        'ambiguous_target_exposures': exposed,
        'observed_misdeliveries': None, 'orphaned_deliveries': None,
        'actual_session_splits': None, 'missing_competitor_rule': unknown_competitor_policy,
    }

def last_observations(conn, events):
    last = {}
    for e in events:
        pid = e['peer_id']
        if pid:
            last[pid] = max(last.get(pid, e['time']), e['time'])
    for pid, value in conn.execute('SELECT peer_id,ts FROM delivery_traces'):
        if pid:
            time = timestamp(value)
            last[pid] = max(last.get(pid, time), time)
    return last

def cap_at_last_observation(spans, last):
    return {pid: [(a, min(b, last[pid])) for a,b in ranges if a < min(b,last[pid])]
            for pid,ranges in spans.items() if pid in last}

def exposure_details(keys, spans, attempts, window_end, mappings):
    counts = collections.Counter()
    targets, all_competitors = set(), set()
    exposed = censored_only = same_path_circle = 0
    for attempt in attempts:
        pid, time = attempt['peer_id'], attempt['time']
        if pid not in keys or not any(a <= time < b for a,b in spans.get(pid, [])):
            continue
        competitors = [(other,b) for other,ranges in spans.items()
                       if other != pid and other in keys and keys[other] == keys[pid]
                       for a,b in ranges if a <= time < b]
        if not competitors:
            continue
        exposed += 1
        counts[keys[pid]] += 1
        targets.add(pid)
        all_competitors.update(other for other,_ in competitors)
        censored_only += all(b == window_end for _,b in competitors)
        m = mappings.get(pid)
        same_path_circle += bool(m) and any(
            other in mappings and mappings[other]['path'] == m['path']
            and mappings[other]['circle'] == m['circle']
            and mappings[other]['display_name'] != m['display_name']
            for other,_ in competitors)
    return {'exposed_attempts':exposed, 'distinct_targets':len(targets),
            'distinct_competitors':len(all_competitors), 'distinct_keys':len(counts),
            'largest_key_exposures':max(counts.values(),default=0),
            'all_competitors_right_censored_attempts':censored_only,
            'same_path_circle_different_display_name_attempts':same_path_circle}

def restart_sensitivity(events, peers, start, end, keys, attempts, starts_path, timezone):
    naive = [dt.datetime.strptime(line.strip(), '%Y-%m-%d %H:%M:%S')
             for line in starts_path.read_text().splitlines() if line.strip()]
    starts = sorted({t.replace(tzinfo=ZoneInfo(timezone)).astimezone(UTC) for t in naive})
    starts = [t for t in starts if start <= t <= end]
    synthetic = [{'peer_id':pid,'type':'peer_offline','time':t} for t in starts for pid in peers]
    spans, intervals = intervals_for(events+synthetic, peers, start, end)
    return {'timezone':timezone, 'start_markers':len(starts),
            'utc_offsets_seconds':sorted({int(t.replace(tzinfo=ZoneInfo(timezone)).utcoffset().total_seconds()) for t in naive}),
            'intervals':intervals,
            'policies':{p:audit_policy(k,peers,spans,attempts) for p,k in keys.items()}}

def supporting(conn, events):
    reasons = collections.Counter()
    spared = []
    for e in events:
        payload = json.loads(e['payload_json'])
        if e['type'] == 'peer_offline' and payload.get('reason'):
            reasons[payload['reason']] += 1
        if e['type'] == 'offline_peer_still_has_runtime_evidence':
            spared.append(e)
    closed = collections.Counter()
    for r in conn.execute("SELECT detail_json FROM delivery_traces WHERE kind='ask' AND stage='closed'"):
        closed[json.loads(r[0]).get('close_reason','unknown')] += 1
    return {'offline_reasons_all_eras':dict(sorted(reasons.items())),
            'spared_events_all_eras':len(spared),
            'spared_distinct_peer_ids':len({e['peer_id'] for e in spared}),
            'spared_by_month':dict(sorted(collections.Counter(e['time'].strftime('%Y-%m') for e in spared).items())),
            'ask_trace_close_reasons':dict(sorted(closed.items()))}

def run(db,start,starts_path=None):
    conn=sqlite3.connect(db.resolve().as_uri()+'?mode=ro&immutable=1',uri=True)
    conn.row_factory=sqlite3.Row
    events,online,peers,mappings,bindings,keys,attempts,coverage,end=load(conn,start)
    results={}
    for tie_mode in ['exclude','online_last']:
        spans,ic=intervals_for(events,peers,start,end,tie_mode)
        results[tie_mode]={'intervals':ic,'policies':{p:audit_policy(k,peers,spans,attempts) for p,k in keys.items()}}
    spans,_=intervals_for(events,peers,start,end)
    common={}
    for policy in ['name_circle','name_path']:
        cohort=peers & keys[policy].keys() & keys['name'].keys()
        common[policy]={p:audit_policy({pid:k for pid,k in keys[p].items() if pid in cohort},cohort,spans,attempts) for p in ['name',policy]}
    last=last_observations(conn,events)
    capped=cap_at_last_observation(spans,last)
    results['last_observation_cap']={'policies':{p:audit_policy(k,peers,capped,attempts) for p,k in keys.items()}}
    concentration={mode:{p:exposure_details(keys[p],ss,attempts,end,mappings)
                         for p in ['name','name_path','base_name','base_circle','base_path']}
                   for mode,ss in [('uncapped',spans),('capped',capped)]}
    coverage['mapped_peers_right_censored']=sum(any(b==end for _,b in spans.get(pid,[])) for pid in peers & mappings.keys())
    restart={}
    if starts_path:
        restart={zone:restart_sensitivity(events,peers,start,end,keys,attempts,starts_path,zone) for zone in ['Europe/London','UTC']}
    extra=supporting(conn,events)
    conn.close()
    with db.open('rb') as source:
        digest=hashlib.file_digest(source,'sha256').hexdigest()
    return {'schema_version':2,'database_sha256':digest,
            'coverage':coverage,'matched_coverage':common,'audit':results,'concentration':concentration,'restart_sensitivity':restart,'supporting':extra}

def main():
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--db',type=Path,required=True)
    ap.add_argument('--output',type=Path,required=True)
    ap.add_argument('--starts',type=Path,help='Extracted naive Go start timestamps; compare Europe/London and UTC interpretations')
    ap.add_argument('--start',default='2026-08-10T00:00:00Z')
    args=ap.parse_args()
    result=run(args.db,timestamp(args.start),args.starts)
    args.output.parent.mkdir(parents=True,exist_ok=True)
    args.output.write_text(json.dumps(result,indent=2,sort_keys=True)+'\n')
    print(json.dumps(result,indent=2,sort_keys=True))

if __name__ == '__main__':
    main()

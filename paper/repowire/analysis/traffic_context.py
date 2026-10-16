#!/usr/bin/env python3
"""Context splits for the inventory's unified asks. Aggregate output only.

Reuses the evidence inventory journal.py identity/classification and ask
reconstruction. Read a completed immutable backup, never a live database.
"""
import argparse
import bisect
import collections
import datetime as dt
import json
import itertools
from pathlib import Path
import re
import sqlite3

def ts(s):
    return dt.datetime.fromisoformat(s.replace('Z','+00:00'))

def reconstruct(db, include_sources=False):
    names = collections.defaultdict(collections.Counter)  # peer_id -> display names
    backend = {}
    role = {}


    def see(pid, name=None, be=None, rl=None):
        if not pid:
            return
        if name:
            names[pid][name] += 1
        if be and be not in ("unknown",):
            backend.setdefault(pid, be)
        if rl:
            role[pid] = rl


    for sid, name, be, rl in db.execute("select session_id, display_name, backend, role from peer_session_mappings"):
        see(sid, name, be, rl)
    for pid, be in db.execute("select peer_id, backend from session_bindings"):
        see(pid, None, be)

    EVENTS = []
    for eid, typ, t, pid, pname, payload in db.execute(
        "select event_id, type, timestamp, peer_id, peer_name, payload_json from events order by timestamp"
    ):
        p = json.loads(payload)
        EVENTS.append((typ, ts(t), pid, pname, p))
        see(pid, pname)
        see(p.get("from_peer_id"), p.get("from"))
        see(p.get("to_peer_id"), p.get("to"))
        see(p.get("peer_id"), p.get("display_name") or p.get("peer_name") or p.get("peer"), p.get("backend"), p.get("role"))

    TRACES = []
    for trace_id, kind, stage, status, pid, fpid, t, detail in db.execute(
        "select trace_id, kind, stage, status, peer_id, from_peer_id, ts, detail_json from delivery_traces order by ts, seq"
    ):
        d = json.loads(detail)
        TRACES.append((trace_id, kind, stage, status, pid, fpid, ts(t), d))
        if stage == "route_decision":
            for c in d.get("candidates", []) + [d.get("sender") or {}]:
                see(c.get("peer_id"), c.get("display_name"), c.get("backend"), c.get("role"))

    ephemeral = set()
    for tr in TRACES:
        if tr[2] == "route_decision":
            for c in tr[7].get("candidates", []):
                if c.get("ephemeral"):
                    ephemeral.add(c["peer_id"])

    job_peers = {pid for (pid,) in db.execute("select distinct assigned_peer_id from tracked_work where assigned_peer_id is not null")}
    for typ, t, pid, pname, p in EVENTS:
        if typ == "job_state_changed" and p.get("assigned_peer_id"):
            job_peers.add(p["assigned_peer_id"])
    spawned = {pid for typ, t, pid, pname, p in EVENTS if typ in ("spawn_seed_delivered", "spawn_seed_failed")}

    SUFFIX = ["claude-code", "codex", "opencode", "gemini", "pi", "antigravity"]


    def name_of(pid):
        return names[pid].most_common(1)[0][0] if names.get(pid) else pid


    def backend_of(pid):
        if pid in backend:
            return backend[pid]
        n = name_of(pid) or ""
        for s in SUFFIX:
            if n.endswith("-" + s):
                return s
        return "unknown"


    HUMAN_NAMES = {"telegram", "telegram-claude-code", "dashboard", "human", "slack", "cli"}
    MACHINE_NAMES = {"jobs", "daemon-job-runner"}
    ORCH_IDS = {pid for pid, r in role.items() if r == "orchestrator"}
    # Anya (orchestrator) broadcasts under this id; see events.type='broadcast'.
    for typ, t, pid, pname, p in EVENTS:
        if typ == "broadcast" and "orchestrator" in (p.get("text") or "").lower():
            ORCH_IDS.add(p.get("from_peer_id"))
    for typ, t, pid, pname, p in EVENTS:
        if typ == "role_claimed":
            ORCH_IDS.add(p.get("peer_id"))


    def cls(pid, name=None):
        n = name or name_of(pid) or ""
        if n in HUMAN_NAMES or role.get(pid) == "human":
            return "human"
        if n in MACHINE_NAMES:
            return "machine"
        if n.startswith("relay-mcp-"):
            return "relay-mcp"
        if pid in ORCH_IDS or n.startswith("orchestrator"):
            return "orchestrator"
        if role.get(pid) == "service":
            return "service"
        return "peer"


    # ---------------------------------------------------------------- unified asks
    asks = {}  # cid -> dict
    for typ, t, pid, pname, p in EVENTS:
        if typ == "ask":
            asks[p["correlation_id"]] = dict(
                cid=p["correlation_id"], t=t, era="A", fpid=p.get("from_peer_id"), fname=p.get("from"),
                tpid=p.get("to_peer_id"), tname=p.get("to"), self=bool(p.get("self_target")), text=p.get("text") or "",
            )
    for trace_id, kind, stage, status, pid, fpid, t, d in TRACES:
        if kind == "ask" and stage == "created" and trace_id not in asks:
            fp = d.get("from_peer") or fpid
            fname = fp if not str(fp).startswith("repow-") else name_of(fp)
            asks[trace_id] = dict(
                cid=trace_id, t=t, era="B", fpid=fpid, fname=fname, tpid=pid, tname=name_of(pid),
                self=(fpid == pid and fpid is not None), text=d.get("text") or "",
            )
    for a in asks.values():
        a["fcls"] = cls(a["fpid"], a["fname"])
        a["tcls"] = cls(a["tpid"], a["tname"])
    ASKS = sorted(asks.values(), key=lambda a: a["t"])
    if include_sources:
        return ASKS, name_of, cls, EVENTS, TRACES
    return ASKS, name_of

def run(path):
    db=sqlite3.connect(path.resolve().as_uri()+'?mode=ro&immutable=1',uri=True)
    asks,name_of,cls,events,traces=reconstruct(db,True)
    paths=collections.defaultdict(set)
    for pid,path in db.execute('SELECT session_id,path FROM peer_session_mappings UNION ALL SELECT peer_id,project_path FROM session_bindings'):
        if pid and path: paths[pid].add(path.rstrip('/'))
    path_key={pid:next(iter(v)) for pid,v in paths.items() if len(v)==1}
    db.close()
    result=summarize(asks,path_key,name_of)
    result['expanded_dispatch']=dispatch_context(asks,events,traces,cls)
    return result

def dispatch_context(asks,events,traces,cls):
    controls={'human','orchestrator','machine'}
    entries=[]
    for a in asks:
        if a['fcls'] in controls and a['tpid']:
            entries.append((a['tpid'],a['t'],'ask'))
    for kind,time,pid,name,payload in events:
        if kind=='notification' and payload.get('to_peer_id') and cls(payload.get('from_peer_id'),payload.get('from')) in controls:
            entries.append((payload['to_peer_id'],time,'notification_event'))
        if kind=='job_state_changed' and payload.get('assigned_peer_id'):
            # A conservative proxy: includes status reports, not just dispatch.
            entries.append((payload['assigned_peer_id'],time,'assigned_job_state'))
    trace_classes=collections.Counter()
    for _,kind,stage,_,pid,sender,time,_ in traces:
        if kind=='notify' and stage=='resolved_peer':
            sender_class=cls(sender)
            trace_classes[sender_class]+=1
            if pid and sender_class in controls:
                entries.append((pid,time,'notify_trace'))
    times=collections.defaultdict(list)
    for pid,time,_ in entries:times[pid].append(time)
    for values in times.values():values.sort()
    pp=[a for a in asks if a['fcls']=='peer' and a['tcls']=='peer']
    no_prior={minutes:0 for minutes in (30,120)}
    for a in pp:
        values=times[a['fpid']]
        index=bisect.bisect_left(values,a['t'])-1
        for minutes in no_prior:
            no_prior[minutes]+=index<0 or a['t']-values[index]>dt.timedelta(minutes=minutes)
    return {'peer_to_peer_asks':len(pp),'no_prior_dispatch_30m':no_prior[30],
            'no_prior_dispatch_2h':no_prior[120],
            'candidate_records_by_channel':dict(sorted(collections.Counter(c for _,_,c in entries).items())),
            'notify_trace_sender_classes':dict(sorted(trace_classes.items())),
            'definition':'Preceding control-sender asks and notifications (events and resolved notify traces), plus all job-state events naming an assigned executor; not proof of causal dispatch.'}

def summarize(asks,path_key,name_of):
    pp=[a for a in asks if a['fcls']=='peer' and a['tcls']=='peer']
    pair_days=collections.defaultdict(set)
    covered=different=no_dispatch=0
    last_dispatch={}
    # Strictly preceding ask; peers are registration ids, not pooled aliases.
    for _,group in itertools.groupby(asks,key=lambda a:a['t']):
        batch=list(group)
        for a in batch:
            if a['fcls']=='peer' and a['tcls']=='peer':
                previous=last_dispatch.get(a['fpid'])
                no_dispatch += previous is None or a['t']-previous > dt.timedelta(minutes=30)
                pair_days[(a['fpid'],a['tpid'])].add(a['t'].date())
                if a['fpid'] in path_key and a['tpid'] in path_key:
                    covered += 1
                    different += path_key[a['fpid']] != path_key[a['tpid']]
        for a in batch:
            if a['fcls'] in ('human','orchestrator') and a['tpid']:
                last_dispatch[a['tpid']]=a['t']
    # Match J5 eligibility, with peer senders only and no pooled humans.
    peer_asks=[a for a in asks if a['fcls']=='peer' and not a['self'] and a['tpid']]
    by_sender=collections.defaultdict(list)
    for a in peer_asks: by_sender[a['fpid']].append(a)
    gaps=collections.defaultdict(lambda:collections.Counter())
    opportunities=repeats=0
    expected=0.0
    for sender,seq in sorted(by_sender.items()):
        n=len(seq)
        counts=collections.Counter(a['tpid'] for a in seq)
        expected += max(0,n-1)*sum((c/n)**2 for c in counts.values())
        for prev,cur in zip(seq,seq[1:]):
            gap=cur['t']-prev['t']
            bucket=('later_utc_day' if prev['t'].date()!=cur['t'].date()
                    else 'same_day_under_1h' if gap<dt.timedelta(hours=1)
                    else 'same_day_at_least_1h')
            same=prev['tpid']==cur['tpid']
            gaps[bucket]['opportunities']+=1
            gaps[bucket]['repeats']+=same
            opportunities+=1;repeats+=same
    alias_pairs=collections.defaultdict(list)
    for a in pp:alias_pairs[(name_of(a['fpid']),name_of(a['tpid']))].append(a['t'])
    recurring={k:values for k,values in alias_pairs.items() if len({t.date() for t in values})>=2}
    alias_senders=collections.defaultdict(list)
    for a in peer_asks:alias_senders[name_of(a['fpid'])].append(a)
    alias_later_opportunities=alias_later_repeats=0
    for seq in alias_senders.values():
        for prev,cur in zip(seq,seq[1:]):
            if prev['t'].date()!=cur['t'].date():
                alias_later_opportunities+=1
                alias_later_repeats+=name_of(prev['tpid'])==name_of(cur['tpid'])
    alias_stats={'distinct_pairs':len(alias_pairs),'pairs_on_multiple_utc_days':len(recurring),
                 'asks_in_recurring_pairs':sum(map(len,recurring.values())),
                 'max_distinct_active_utc_days_per_pair':max((len({t.date() for t in v}) for v in alias_pairs.values()),default=0),
                 'longest_pair_elapsed_calendar_days':max(((max(v).date()-min(v).date()).days for v in alias_pairs.values()),default=0),
                 'peer_sender_later_day_opportunities':alias_later_opportunities,
                 'peer_sender_later_day_repeats':alias_later_repeats,
                 'definition':'Most frequently observed name per peer id, from inventory reconstruction; alias recurrence is not session or context continuity.'}
    human=collections.Counter()
    for a in asks:
        if a['fcls']!='human':continue
        name=a['fname'] or name_of(a['fpid'])
        surface=('telegram' if name in ('telegram','telegram-claude-code') else
                 'slack' if name=='slack' else 'dashboard' if name=='dashboard' else 'other_human')
        human[surface]+=1
    return {'peer_to_peer':{'asks':len(pp),'distinct_id_pairs':len(pair_days),
                           'id_pairs_on_multiple_utc_days':sum(len(v)>=2 for v in pair_days.values()),
                           'no_recorded_human_or_orchestrator_dispatch_within_30m':no_dispatch,
                           'both_endpoints_have_unique_retained_path':covered,
                           'different_retained_paths':different,
                           'path_warning':'Distinct retained paths can be worktrees of one repository; not a cross-repository classification.'},
            'alias_recurrence':alias_stats,
            'peer_sender_repeats':{'opportunities':opportunities,'repeats':repeats,
                                  'observed_rate':repeats/opportunities if opportunities else None,
                                  'recipient_frequency_null':expected/opportunities if opportunities else None,
                                  'null_definition':'Within each sender, iid draws from observed recipient frequencies: sum p_i squared; weighted by n_sender-1.',
                                  'gaps':dict(gaps)},
            'human_asks_by_surface':dict(sorted(human.items()))}

if __name__=='__main__':
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--db',type=Path,required=True)
    ap.add_argument('--output',type=Path,required=True)
    args=ap.parse_args()
    result=run(args.db)
    args.output.write_text(json.dumps(result,sort_keys=True,indent=2)+'\n')
    print(json.dumps(result,sort_keys=True,indent=2))

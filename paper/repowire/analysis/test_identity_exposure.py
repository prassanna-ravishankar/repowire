"""Small event histories with hand-computed answers for the audit."""
import unittest
from identity_exposure import audit_policy, intervals_for, timestamp, derived_base_name, load, cap_at_last_observation, restart_sensitivity

T=timestamp('2026-08-10T00:00:00Z')
def t(n):
    import datetime
    return T+datetime.timedelta(seconds=n)
def event(pid,kind,n):
    return {'peer_id':pid,'type':kind,'time':t(n)}

class AuditTests(unittest.TestCase):
    def test_default_base_name_keeps_legitimate_numeric_folder_suffix(self):
        self.assertEqual(derived_base_name('/repos/project-2/', 'codex'), 'project-2-codex')
        self.assertEqual(derived_base_name('/repos/ A & B /', 'pi'), 'A-B-pi')
        self.assertEqual(derived_base_name('/', 'codex'), 'peer-codex')

    def test_overlapping_triple_and_boundaries(self):
        spans={'a':[(t(0),t(10))],'b':[(t(2),t(6))],'c':[(t(4),t(8))]}
        attempts=[{'peer_id':p,'time':t(n)} for p,n in [('a',1),('b',2),('c',4),('b',6),('a',8),('x',5)]]
        r=audit_policy({p:('same',) for p in spans},set(spans),spans,attempts)
        self.assertEqual(r['pair_overlap_seconds'],10) # 4 + 4 + 2
        self.assertEqual(r['key_collision_seconds'],6)
        self.assertEqual(r['concurrent_peer_pairs'],3)
        self.assertEqual(r['ambiguous_target_exposures'],2)
        self.assertEqual(r['attempts_with_key'],5)
        self.assertEqual(r['attempts_with_online_target_and_key'],4)
        self.assertEqual(r['keys_with_multiple_peer_ids_over_window'],1)
    def test_duplicate_online_ties_and_censoring(self):
        events=[event('a','peer_online',1),event('a','peer_online',2),event('a','peer_offline',4),event('a','peer_online',4),event('a','peer_online',7)]
        s,c=intervals_for(events,{'a'},t(0),t(10))
        self.assertEqual(s['a'],[(t(1),t(4)),(t(7),t(10))])
        self.assertEqual(c['conflicting_peer_timestamps'],1)
        self.assertEqual(c['right_censored_peers'],1)
        s,_=intervals_for(events,{'a'},t(0),t(10),'online_last')
        self.assertEqual(s['a'],[(t(1),t(4)),(t(4),t(10))])
    def test_sequential_reuse_is_not_concurrent_collision(self):
        spans={'a':[(t(0),t(5))],'b':[(t(5),t(10))]}
        r=audit_policy({'a':('alias',),'b':('alias',)},set(spans),spans,
                       [{'peer_id':'b','time':t(5)}])
        self.assertEqual(r['keys_with_multiple_peer_ids_over_window'],1)
        self.assertEqual(r['concurrent_peer_pairs'],0)
        self.assertEqual(r['ambiguous_target_exposures'],0)
        self.assertEqual(r['attempts_with_online_target_and_key'],1)

    def test_snapshot_coverage_excludes_multi_session_bindings(self):
        import sqlite3
        db=sqlite3.connect(':memory:')
        db.row_factory=sqlite3.Row
        db.executescript("""
            CREATE TABLE events(event_id,type,timestamp,peer_id,peer_name,session_id,payload_json);
            INSERT INTO events VALUES(1,'peer_online','2026-08-10T00:00:01Z','a','repo-2-codex','a','{}');
            INSERT INTO events VALUES(2,'peer_online','2026-08-10T00:00:02Z','b','repo-codex','b','{}');
            CREATE TABLE peer_session_mappings(session_id,display_name,circle,backend,path);
            INSERT INTO peer_session_mappings VALUES('a','repo-2-codex','work','codex','/repos/repo');
            CREATE TABLE session_bindings(peer_id,backend,runtime_session_id,project_path,created_at,last_seen_at);
            INSERT INTO session_bindings VALUES('a','codex','s1','/repos/repo',NULL,NULL);
            INSERT INTO session_bindings VALUES('a','codex','s2','/repos/repo',NULL,NULL);
            INSERT INTO session_bindings VALUES('b','codex','s3',NULL,NULL,NULL);
            CREATE TABLE delivery_traces(id,trace_id,kind,stage,status,peer_id,ts,seq);
        """)
        _,_,peers,_,_,keys,_,coverage,_=load(db,T)
        self.assertEqual(keys['base_name'],{'a':('repo-codex',)})
        self.assertNotIn('a',keys['backend_session'])
        self.assertEqual(keys['backend_session']['b'],('codex','s3'))
        self.assertEqual(coverage['online_peers_with_multiple_binding_keys'],1)
        self.assertEqual(coverage['online_peers_with_retained_mapping'],1)
        db.close()

    def test_cap_removes_silent_competitor_tail(self):
        spans={'a':[(t(1),t(10))],'b':[(t(2),t(10))]}
        keys={'a':('same',),'b':('same',)}
        attempts=[{'peer_id':'a','time':t(8)}]
        capped=cap_at_last_observation(spans,{'a':t(9),'b':t(3)})
        self.assertEqual(audit_policy(keys,set(spans),spans,attempts)['ambiguous_target_exposures'],1)
        self.assertEqual(audit_policy(keys,set(spans),capped,attempts)['ambiguous_target_exposures'],0)

    def test_restart_rebuild_preserves_later_reregistration(self):
        import tempfile
        from pathlib import Path
        events=[event('a','peer_online',1),event('a','peer_online',7)]
        with tempfile.TemporaryDirectory() as directory:
            starts=Path(directory)/'starts.txt'
            starts.write_text('2026-08-10 00:00:05\n')
            result=restart_sensitivity(events,{'a'},t(0),t(10),{'name':{'a':('a',)}},
                                       [{'peer_id':'a','time':t(8)}],starts,'UTC')
        self.assertEqual(result['intervals']['online_intervals'],2)
        self.assertEqual(result['policies']['name']['attempts_with_online_target_and_key'],1)

    def test_unknown_competitor_is_excluded(self):
        s={'a':[(t(0),t(10))],'b':[(t(0),t(10))]}
        r=audit_policy({'a':('key',)},{'a','b'},s,[{'peer_id':'a','time':t(5)}])
        self.assertEqual(r['ambiguous_target_exposures'],0)
        self.assertEqual(r['online_peers_with_key'],1)
        self.assertIsNone(r['orphaned_deliveries'])

if __name__=='__main__': unittest.main()

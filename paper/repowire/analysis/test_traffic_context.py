import unittest
from traffic_context import summarize,ts,dispatch_context

def ask(sender,target,time,role='peer',name=None):
    return dict(fpid=sender,tpid=target,t=ts(time),fcls=role,tcls='peer',self=False,fname=name)

class TrafficTests(unittest.TestCase):
    def test_sender_null_gap_buckets_and_dispatch_window(self):
        asks=[ask('h','a','2026-08-10T12:00:00Z','human','telegram'),
              ask('a','x','2026-08-10T12:10:00Z'),
              ask('a','x','2026-08-10T12:20:00Z'),
              ask('a','y','2026-08-11T12:00:00Z'),
              ask('a','y','2026-08-12T12:00:00Z')]
        result=summarize(asks,{'a':'a','x':'b','y':'c'},lambda pid:pid)
        repeat=result['peer_sender_repeats']
        self.assertEqual(repeat['opportunities'],3)
        self.assertEqual(repeat['repeats'],2)
        self.assertEqual(repeat['recipient_frequency_null'],0.5)
        self.assertEqual(repeat['gaps']['later_utc_day']['opportunities'],2)
        self.assertEqual(repeat['gaps']['later_utc_day']['repeats'],1)
        self.assertEqual(result['peer_to_peer']['no_recorded_human_or_orchestrator_dispatch_within_30m'],2)
        self.assertEqual(result['peer_to_peer']['id_pairs_on_multiple_utc_days'],1)
        self.assertEqual(result['human_asks_by_surface'],{'telegram':1})

    def test_alias_pairs_join_registration_churn_without_proving_session_continuity(self):
        asks=[ask('a1','x1','2026-08-10T12:00:00Z'),
              ask('a1','x1','2026-08-11T12:00:00Z'),
              ask('a2','x2','2026-08-12T12:00:00Z')]
        names={'a1':'A','a2':'A','x1':'X','x2':'X'}
        result=summarize(asks,{},names.get)
        self.assertEqual(result['peer_to_peer']['distinct_id_pairs'],2)
        self.assertEqual(result['peer_to_peer']['id_pairs_on_multiple_utc_days'],1)
        self.assertEqual(result['alias_recurrence']['distinct_pairs'],1)
        self.assertEqual(result['alias_recurrence']['asks_in_recurring_pairs'],3)
        self.assertEqual(result['alias_recurrence']['peer_sender_later_day_repeats'],2)

    def test_expanded_dispatch_includes_notifications_and_assigned_jobs(self):
        asks=[ask('a','x','2026-08-10T'+time+'Z') for time in ['12:10:00','13:10:00','14:45:00','16:00:00']]
        events=[('notification',ts('2026-08-10T12:00:00Z'),None,None,
                 {'from_peer_id':'h','from':'human','to_peer_id':'a'}),
                ('job_state_changed',ts('2026-08-10T13:00:00Z'),None,None,{'assigned_peer_id':'a'})]
        result=dispatch_context(asks,events,[],lambda pid,name=None:'human')
        self.assertEqual(result['no_prior_dispatch_30m'],2)
        self.assertEqual(result['no_prior_dispatch_2h'],1)
        self.assertEqual(result['candidate_records_by_channel']['notification_event'],1)
        self.assertEqual(result['candidate_records_by_channel']['assigned_job_state'],1)

if __name__=='__main__':unittest.main()

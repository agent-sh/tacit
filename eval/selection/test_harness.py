import unittest
from typing import Any
from harness import context_query, baseline_select, feature_vector, fit, calibrate, predict, family_ids, labelled_rows, learned_select


class ContractTests(unittest.TestCase):
    def case(self, prompt, prior='Inspect violet7 cache invalidation') -> dict[str, Any]:
        return dict(id='synthetic', episode_id='ep1', split='dev', prompt=prompt,
                    cwd='/synthetic', history=[dict(role='user', content=prior)])

    def test_original_identifier_literal(self):
        c = self.case('Trace CACHE_KEY_7 exactly')
        self.assertEqual(context_query(c, 'original')['query'], c['prompt'])
        self.assertEqual(context_query(c, 'gated')['query'], c['prompt'])

    def test_followup_provenance(self):
        q = context_query(self.case('Fix that'), 'gated')
        self.assertIn('violet7', q['query'])
        self.assertEqual(q['sources'][0]['history_index'], 0)
        self.assertEqual(q['original'], 'Fix that')

    def test_topic_switch(self):
        c = self.case('Inspect amber8 socket timeout')
        self.assertEqual(context_query(c, 'gated')['query'], c['prompt'])
        self.assertIn('violet7', context_query(c, 'unconditional')['query'])

    def test_null(self):
        for p in ['', 'thanks', 'ok', '<tool-result>Fix that</tool-result>']:
            self.assertEqual(context_query(self.case(p), 'gated')['query'], p)

    def test_labels_and_assistant_never_enter_context(self):
        c = self.case('Fix that')
        before = context_query(c, 'gated')
        c.update(expected=['SECRET'], rejected=['BAD'], provenance={'answer': 'FUTURE'}, category='ANSWER')
        c['history'].append(dict(role='assistant', content='FUTURE ANSWER'))
        self.assertEqual(before, context_query(c, 'gated'))

    def test_no_backfill(self):
        hits = [dict(id=str(i), Name=str(i), Scope='user', Score=30) for i in range(6)]
        self.assertEqual([h['id'] for h in baseline_select(hits, [], '', {'0'})], ['1', '2'])

    def test_scope_not_learned_or_overridden(self):
        h = dict(Score=25, Scope='projects/violet7', Evidence={'Coverage': .8})
        self.assertEqual(feature_vector(h, 'violet7')[3], 1)
        self.assertEqual(feature_vector(h, 'amber8')[3], 0)

    def test_training_and_calibration_split_enforced(self):
        with self.assertRaises(ValueError):
            fit([dict(split='test')])
        with self.assertRaises(ValueError):
            calibrate({}, [dict(split='dev')])

    def test_origin_families_transitive_not_independent(self):
        hs = [dict(id='a', origin_ids=['o']), dict(id='b', origin_ids=['o','p']), dict(id='c', origin_ids=['p'])]
        self.assertEqual(len(set(family_ids(hs).values())), 1)

    def test_unknown_is_not_negative(self):
        h = dict(id='user/x.md', Name='x', Scope='user', Score=30)
        pool = dict(id='p', case_id='c', split='dev', episode_id='ep', repo='', candidates=[h], derived=[])
        labels = {'c': dict(split='dev', episode_id='ep', expected=[], rejected=[])}
        rows, coverage = labelled_rows([pool], labels, None)
        self.assertEqual(rows, [])
        self.assertEqual(coverage, dict(positive=0,negative=0,unknown=1))

    def test_learned_no_backfill(self):
        hs = [dict(id=str(i), Name=str(i)) for i in range(6)]
        values = {str(i): 6-i for i in range(6)}
        self.assertEqual([h['id'] for h in learned_select(hs,[],values,0,{'0'})], ['1','2'])

    def test_changed_cwd_blocks_context(self):
        c = self.case('Fix that')
        c['history'][0]['cwd']='/other-synthetic'
        self.assertEqual(context_query(c,'gated')['query'], c['prompt'])

    def test_episode_overlap_rejected(self):
        m=fit([dict(split='dev',episode_id='same',x=[1,0,0,0],y=1)])
        with self.assertRaises(ValueError):
            calibrate(m,[dict(split='calibration',episode_id='same',x=[1,0,0,0],y=1)])

    def test_fit_calibration_exercised(self):
        rows=[]
        for split in ['dev','calibration']:
            for i in range(4):
                rows.append(dict(split=split, episode_id=split+str(i), x=[i,0,0,0], y=int(i>=2)))
        m=fit([r for r in rows if r['split']=='dev'])
        m=calibrate(m,[r for r in rows if r['split']=='calibration'])
        self.assertLess(predict(m,[0,0,0,0]), m['threshold'])
        self.assertGreaterEqual(predict(m,[3,0,0,0]), m['threshold'])


if __name__ == '__main__':
    unittest.main()

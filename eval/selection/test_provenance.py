import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import harness


class ProvenanceGuards(unittest.TestCase):
    def test_episode_consistency_includes_unknown_candidates(self):
        pools = [dict(episode_id='shared', split='dev'), dict(episode_id='shared', split='calibration')]
        with self.assertRaises(ValueError):
            harness.validate_episode_splits(pools)

    def test_test_episode_rejected_against_both_input_splits(self):
        for split in ('dev', 'calibration'):
            with self.subTest(split=split), tempfile.TemporaryDirectory() as directory:
                r = Path(directory)
                (r/'pools.json').write_text(json.dumps([dict(id='testpool', split='test', episode_id='shared', context={'arm':'gated'}, ceiling=6, candidates=[], derived=[],repo='',pool_hash='empty',families={},frozen_baseline='')]))
                (r/'model.json').write_text(json.dumps(dict(semantic=False, arm='gated',ceiling=6,dev_episodes=['shared'] if split=='dev' else [],calibration_episodes=['shared'] if split=='calibration' else [],features=4,weights=[0]*5,mean=[0]*4,scale=[1]*4,threshold=1)))
                args = SimpleNamespace(pools=r/'pools.json',scores=None,model=r/'model.json',out=r/'selected.jsonl')
                with patch.object(harness,'private_output',lambda p, audit_dir=None:Path(p)):
                    with self.assertRaises(ValueError):
                        harness.select(args)

    def test_train_persists_other_arm_and_label_only_episodes(self):
        pools=[]; labels=[]
        for split in ('dev','calibration'):
            for y in (0,1):
                cid=split+str(y)
                hit=dict(id='user/'+cid+'.md',Name=cid,Scope='user',Score=10+20*y)
                pools.append(dict(id=cid,case_id=cid,episode_id=cid,split=split,context={'arm':'original'},ceiling=6,repo='',candidates=[hit],derived=[]))
                labels.append(dict(id=cid,episode_id=cid,split=split,expected=[cid] if y else [],rejected=[] if y else [cid]))
        pools.append(dict(id='other',case_id='other',episode_id='other-arm',split='dev',context={'arm':'gated'},ceiling=6,candidates=[],derived=[]))
        labels.extend([dict(id='other',episode_id='other-arm',split='dev'),dict(id='label-only',episode_id='label-only',split='calibration')])
        args=SimpleNamespace(pools='pools',labels='labels',scores=None,arm='original',ceiling=6,model='/synthetic/model.json')
        with patch.object(harness,'load',lambda p:pools if p=='pools' else labels), patch.object(harness,'sha',lambda p:'a'*64), patch.object(harness,'private_output',lambda p, audit_dir=None:Path(p)), patch.object(harness,'dump') as dump:
            harness.train(args)
            model=dump.call_args.args[1]
        self.assertIn('other-arm',model['dev_episodes'])
        self.assertIn('label-only',model['calibration_episodes'])

    def test_score_only_train_keeps_raw_margin_and_calibrates(self):
        pools=[]; labels=[]; scores={}
        for split in ('dev','calibration'):
            for y in (0,1):
                cid=split+str(y); fid='user/'+cid+'.md'
                hit=dict(id=fid,Name=cid,Scope='user',Score=10+20*y)
                pools.append(dict(id=cid,case_id=cid,episode_id=cid,split=split,context={'arm':'original'},ceiling=6,repo='',candidates=[hit],derived=[]))
                labels.append(dict(id=cid,episode_id=cid,split=split,expected=[fid] if y else [],rejected=[] if y else [fid]))
                scores[cid]={fid: (-2.0+5*y) if split=='dev' else (-0.5+y)}
        args=SimpleNamespace(pools='pools',labels='labels',scores='scores',selector='score-only',arm='original',ceiling=6,model='/synthetic/model.json')
        with patch.object(harness,'load',lambda p:pools if p=='pools' else labels), patch.object(harness,'sha',lambda p:'a'*64), patch.object(harness,'private_output',lambda p, audit_dir=None:Path(p)), patch.object(harness,'verify_score_manifest',return_value={'identity_sha256':'b'*64}), patch.object(harness,'score_map',return_value=scores), patch.object(harness,'dump') as dump:
            harness.train(args)
            model=dump.call_args.args[1]
        self.assertEqual(model['weights'],[0,0,0,0,0,1])
        self.assertEqual(model['mean'],[0]*5)
        self.assertEqual(model['scale'],[1]*5)
        self.assertEqual(model['threshold'],0.5)
        self.assertEqual(harness.predict(model,[100,200,300,400,-2.7]),-2.7)

    def test_cpu_execution_metadata_rejection_matrix(self):
        with tempfile.TemporaryDirectory() as directory:
            r=Path(directory)
            (r/'pools').write_text('[]'); (r/'scores').write_text('[]')
            def verify(execution,runtime):
                identity=dict(scorer_sha256='a'*64,model_manifest_sha256='b'*64,lock_sha256='c'*64,runtime=runtime,execution=execution)
                manifest=dict(identity=identity,identity_sha256=harness.object_sha(identity),scores_sha256=harness.sha(r/'scores'),pools_sha256=harness.sha(r/'pools'),input_records_sha256=harness.object_sha([]))
                (r/'scores.manifest.json').write_text(json.dumps(manifest))
                return harness.verify_score_manifest(r/'scores',r/'pools')
            for provider,backend in [('CPUExecutionProvider',{'onnxruntime':'test'}),('PyTorchCPU',{'torch':'test','transformers':'test'})]:
                runtime=dict(numpy='test',tokenizers='test',**backend)
                execution=dict(provider=provider,intra_threads=1,inter_threads=1,batch_size=16,max_length=512,score_type='raw_logit')
                verify(execution,runtime)
                for field in ('intra_threads','inter_threads','batch_size','max_length'):
                    for value in (None,True,0,-1,1.5,'1'):
                        with self.subTest(provider=provider,field=field,value=value), self.assertRaises(ValueError):
                            verify(dict(execution,**{field:value}),runtime)
                with self.assertRaises(ValueError):
                    verify(dict(execution,provider='CUDAExecutionProvider'),runtime)
                with self.assertRaises(ValueError):
                    verify(dict(execution,score_type=None),runtime)
                for key in backend:
                    with self.subTest(provider=provider,missing=key), self.assertRaises(ValueError):
                        verify(execution,{k:v for k,v in runtime.items() if k!=key})

    def test_empty_scorer_identity_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            r=Path(directory)
            (r/'pools').write_text('[]');(r/'scores').write_text('[]')
            manifest=dict(identity={},identity_sha256=harness.object_sha({}),scores_sha256=harness.sha(r/'scores'),pools_sha256=harness.sha(r/'pools'),input_records_sha256=harness.object_sha([]))
            (r/'scores.manifest.json').write_text(json.dumps(manifest))
            with self.assertRaises(ValueError):
                harness.verify_score_manifest(r/'scores',r/'pools')

    def test_summary_view_excludes_body_and_binds_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            r=Path(directory)
            pools=[dict(id='p',context={'query':'task'},candidates=[dict(id='f',Name='fact',Description='printed',Body='hidden answer')],derived=[])]
            (r/'pools').write_text(json.dumps(pools));(r/'scores').write_text('[]')
            identity={'scorer_sha256':'a'*64,'model_manifest_sha256':'b'*64,'lock_sha256':'c'*64,'runtime':{'numpy':'test','onnxruntime':'test','tokenizers':'test'},'document_view':'summary','execution':{'provider':'CPUExecutionProvider','intra_threads':1,'inter_threads':1,'batch_size':16,'max_length':512,'score_type':'raw_logit'}}
            records=[dict(id='p',query='task',candidates=[dict(id='f',text='fact\nprinted')])]
            manifest=dict(identity=identity,identity_sha256=harness.object_sha(identity),text_view='summary',scores_sha256=harness.sha(r/'scores'),pools_sha256=harness.sha(r/'pools'),input_records_sha256=harness.object_sha(records))
            (r/'scores.manifest.json').write_text(json.dumps(manifest))
            harness.verify_score_manifest(r/'scores',r/'pools')
            identity['document_view']='full';manifest['identity_sha256']=harness.object_sha(identity)
            (r/'scores.manifest.json').write_text(json.dumps(manifest))
            with self.assertRaises(ValueError):
                harness.verify_score_manifest(r/'scores',r/'pools')

    def test_semantic_manifest_binds_exact_scores_and_pool(self):
        with tempfile.TemporaryDirectory() as directory:
            r = Path(directory)
            (r/'pools.jsonl').write_text('[]')
            (r/'scores.jsonl').write_text('[]')
            identity={'scorer_sha256':'a'*64,'model_manifest_sha256':'b'*64,'lock_sha256':'c'*64,'runtime':{'numpy':'test','onnxruntime':'test','tokenizers':'test'},'execution':{'provider':'CPUExecutionProvider','intra_threads':1,'inter_threads':1,'batch_size':16,'max_length':512,'score_type':'raw_logit'}}
            manifest={'identity':identity,'identity_sha256':harness.object_sha(identity),'scores_sha256':harness.sha(r/'scores.jsonl'),'pools_sha256':harness.sha(r/'pools.jsonl'),'input_records_sha256':harness.object_sha([])}
            (r/'scores.jsonl.manifest.json').write_text(json.dumps(manifest))
            self.assertEqual(harness.verify_score_manifest(r/'scores.jsonl',r/'pools.jsonl')['identity_sha256'],manifest['identity_sha256'])
            (r/'scores.jsonl').write_text('[1]')
            with self.assertRaises(ValueError):
                harness.verify_score_manifest(r/'scores.jsonl',r/'pools.jsonl')


if __name__ == '__main__':
    unittest.main()

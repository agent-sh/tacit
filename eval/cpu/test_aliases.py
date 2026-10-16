"""Synthetic, stdlib-only pre-write safety tests; never load a model or call an API."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

CPU = Path(__file__).resolve().parent
SELECTION = CPU.parent / 'selection'


def command(script, *args):
    return [sys.executable, '-S', str(script), *map(str, args)]


def alias(source, target, kind):
    target.parent.mkdir(parents=True, exist_ok=True)
    if source == target:
        return
    target.unlink(missing_ok=True)
    if kind == 'symlink':
        target.symlink_to(source)
    else:
        os.link(source, target)


class AliasTests(unittest.TestCase):
    def run_guard(self, cmd, root):
        before = {p: p.read_bytes() for p in root.rglob('*') if p.is_file()}
        result = subprocess.run(cmd, text=True, capture_output=True, timeout=10)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn('must be different files', result.stderr, result.stderr)
        self.assertEqual(before, {p: p.read_bytes() for p in root.rglob('*') if p.is_file()})

    def specs(self, r):
        inp, out = r/'input', r/'out'
        work = r/'workspace'
        base = ['--input', inp, '--output', out]
        yield CPU/'qwen/jev_devcal.py', base+['--allow-egress'], [inp, r/'key'], [out/n for n in ['identity.json','requests.jsonl','summary.json']+[a+'-scores.jsonl'+s for a in ('baseline','rubric','choice') for s in ('','.partial')]], ['--key-file', r/'key']
        yield CPU/'qwen/evaluate_jev.py', ['--pools',inp,'--primary',r/'primary','--reviewed',r/'reviewed','--scores','x='+str(r/'scores'),'--output',out], [inp,r/'primary',r/'reviewed',r/'scores'], [out/n for n in ('quality-evaluation.json','calibration-curves.json','quality-errors.json')], []
        for script in ('jina/verify.py','laya/verify.py'):
            sources = [inp,r/'scores',r/'reverse',r/'timings',r/'measurements',r/'runtime']
            yield CPU/script, ['--input',inp,'--output',sources[1],'--reversed',sources[2],'--timings',sources[3],'--measurements',sources[4],'--runtime',sources[5],'--report',out], sources, [out], []
        yield CPU/'qwen/aggregate.py', base+['--run',r/'run'], [inp]+[r/'run'/n for n in ('measurements.jsonl','default-scores.jsonl','utility-scores.jsonl')], [out], []
        for script in ('qwen/verify.py','qwen/try_perchannel.py'):
            yield CPU/script, base+['--model','0.6B','--workspace',work], [inp], [out], []
        yield CPU/'qwen/test_cli.py', ['--output',out,'--workspace',work], [work/'qwen/manifest-0.6B.json',work/'qwen/model-0.6B/config.json'], [out], []
        yield CPU/'laya/receipt.py', ['--laya-source',r/'source','--output',out], [r/'source/laya/agent.py'], [out], []
        yield CPU/'qwen/prompt_ablation.py', base+['--model','base','--workspace',work], [inp], [out], []
        yield SELECTION/'smoke.py', ['--audit-dir',r], [], [r/'selection-fit-synthetic'/n for n in ('train-pools.jsonl','test-input-pools.jsonl','labels.jsonl','model.json','selected.jsonl')], []
        yield CPU/'controls/benchmark.py', base+['--workspace',work], [inp], [out], []
        yield CPU/'controls/run_checks.py', base+['--workspace',work,'--benchmark'], [inp], [out/n for n in ('scores.jsonl','execution.json','benchmark.json')], []
        yield CPU/'qwen/score.py', base+['--workspace',work,'--benchmark','--instruction-mode','both','--quant','int8','--control-cache',r/'cache'], [inp,r/'cache'], [out/n for n in ('measurements.jsonl','validation.json','runtime.json','default-scores.jsonl','default-unique.jsonl','utility-scores.jsonl','utility-unique.jsonl')]+[work/'qwen/0.6B-validation.json'], []
        yield CPU/'jina/score.py', base+['--workspace',work,'--order','both'], [inp], [Path(str(out)+s) for s in ('.natural.jsonl','.reversed.jsonl','.measurements.jsonl','.runtime.jsonl')], []
        yield CPU/'laya/score.py', base+['--workspace',work,'--model','base'], [inp], [out,Path(str(out)+'.timings.jsonl'),Path(str(out)+'.metrics.json')], []
        for operation in ('select','fit'):
            flags = [operation,'--pools',inp,'--scores',r/'scores','--model',r/'model','--audit-dir',r]
            sources = [inp,r/'scores',r/'scores.manifest.json']
            if operation == 'select':
                sources += [r/'model']; flags += ['--out',out]
            else:
                sources += [r/'labels']; flags += ['--labels',r/'labels']
            yield SELECTION/'harness.py', flags, sources, [out if operation=='select' else r/'model'], []
        yield SELECTION/'harness.py', ['retrieve','--cases',inp,'--store',r/'store','--store-head','synthetic','--baseline',r/'baseline','--exporter',r/'exporter','--out',out,'--audit-dir',r], [inp,r/'baseline',r/'exporter',r/'store/fact.md'], [out/n for n in ('offline-settings.yaml','manifest.json','pools.jsonl','scorer-input.jsonl','request.json','response.json')], []

    def test_each_input_against_each_generated_output(self):
        # Test lexical paths as well as aliases. Every invocation must fail at
        # preflight, not during parsing, model imports, credentials or network.
        for index in range(len(list(self.specs(Path('/synthetic'))))):
            for kind in ('lexical','symlink','hardlink'):
                with tempfile.TemporaryDirectory(dir=Path.home()/'.cache') as tmp:
                    r = Path(tmp)
                    specs = list(self.specs(r))
                    if index >= len(specs):
                        break
                    script, args, sources, destinations, extra = specs[index]
                    for source in sources:
                        for dest in destinations:
                            with self.subTest(script=script, source=source.name, dest=dest.name, kind=kind):
                                # Reset all fixtures between cases, including aliases.
                                for p in sorted(r.rglob('*'), key=lambda p: len(p.parts), reverse=True):
                                    if p.is_symlink() or p.is_file(): p.unlink()
                                    elif p.is_dir(): p.rmdir()
                                for p in sources:
                                    p.parent.mkdir(parents=True,exist_ok=True)
                                    p.write_text('{"id":"synthetic","query":"","candidates":[]}\n')
                                callargs = [str(a) for a in args+extra]
                                if kind == 'lexical':
                                    # Move the chosen input to the generated destination.
                                    dest.parent.mkdir(parents=True,exist_ok=True)
                                    dest.write_bytes(source.read_bytes())
                                    callargs = [a.replace(str(source),str(dest)) for a in callargs]
                                    # Implicit directory inputs cannot be moved individually.
                                    if str(source) not in [str(a) for a in args+extra] and not any('='+str(source) in str(a) for a in args):
                                        continue
                                else:
                                    alias(source,dest,kind)
                                self.run_guard(command(script,*callargs),r)

    def test_output_pairs_before_any_write(self):
        for kind in ('symlink','hardlink'):
            with tempfile.TemporaryDirectory(dir=Path.home()/'.cache') as tmp:
                r = Path(tmp)
                for script,args,sources,destinations,extra in self.specs(r):
                    if len(destinations)<2: continue
                    for p in sorted(r.rglob('*'), key=lambda p:len(p.parts), reverse=True):
                        if p.is_symlink() or p.is_file(): p.unlink()
                        elif p.is_dir(): p.rmdir()
                    for p in sources:
                        p.parent.mkdir(parents=True,exist_ok=True);p.write_text('[]\n')
                    first,last=destinations[0],destinations[-1]
                    first.parent.mkdir(parents=True,exist_ok=True);first.write_text('sentinel\n')
                    alias(first,last,kind)
                    with self.subTest(script=script,kind=kind):
                        self.run_guard(command(script,*(args+extra)),r)

    def test_report_and_select_original_reproductions(self):
        for kind in ('lexical','symlink','hardlink'):
            with tempfile.TemporaryDirectory(dir=Path.home()/'.cache') as tmp:
                r=Path(tmp);source=r/'pools';source.write_text('[]')
                dest=source if kind=='lexical' else r/'selected'
                alias(source,dest,kind)
                self.run_guard(command(SELECTION/'harness.py','select','--pools',source,'--out',dest,'--audit-dir',r),r)
                source.write_text('{"id":"synthetic","query":"","candidates":[]}\n')
                scores=r/'scores';scores.write_text('{"id":"synthetic","scores":[]}\n')
                self.run_guard(command(CPU/'jina/verify.py','--input',source,'--output',scores,'--report',dest),r)

    def test_preflight_missing_targets_trees_and_streams(self):
        from common import preflight_files, distinct_files
        with tempfile.TemporaryDirectory(dir=Path.home()/'.cache') as tmp:
            r=Path(tmp);missing=r/'missing';link=r/'link';link.symlink_to(missing)
            for outputs in ([missing,missing], [missing,link]):
                with self.assertRaisesRegex(ValueError,'must be different files'):
                    preflight_files([],outputs)
                with self.assertRaisesRegex(ValueError,'must be different files'):
                    distinct_files('-',*outputs)
            preflight_files([missing,link],[r/'safe'])  # input/input aliases are fine
            distinct_files('-','-')
            with self.assertRaisesRegex(ValueError,'must be different files'):
                preflight_files(['-'],['-'])  # non-streaming '-' is a filename
            tree=r/'tree';tree.mkdir();(tree/'data').write_text('sentinel')
            (tree/'loop').symlink_to(tree,target_is_directory=True)
            linked=r/'linked';os.link(tree/'data',linked)
            with self.assertRaisesRegex(ValueError,'must be different files'):
                preflight_files([tree],[linked])
            with self.assertRaisesRegex(ValueError,'must be different files'):
                preflight_files([tree],[tree/'new'])
            with self.assertRaisesRegex(ValueError,'must be different files'):
                preflight_files([linked],[tree])

    def test_download_batch_checks_before_any_artifact_request(self):
        import artifacts
        import io
        from unittest.mock import patch
        for kind in ('symlink','hardlink'):
            for group,key in [('qwen','0.6B'),('controls','tiny'),('laya','base'),('jina','v3.5')]:
                with self.subTest(kind=kind,group=group), tempfile.TemporaryDirectory(dir=Path.home()/'.cache') as tmp:
                    r=Path(tmp);base=r/key if group in ('controls','laya') else r
                    model=base/('model-'+key if group=='qwen' else 'model');model.mkdir(parents=True)
                    names=['config.json','model.safetensors'] if group!='controls' else ['config.json','vocab.txt']
                    if group=='laya': names=['rl_agent_config.json','model.safetensors']
                    repo,revision=artifacts.PINS[group][key]
                    metadata={'id':repo,'sha':revision,'siblings':[{'rfilename':n} for n in names]}
                    first=model/names[0];first.write_text('sentinel')
                    # A later pending download must not truncate an earlier output.
                    alias(first,model/(names[1]+'.partial'),kind)
                    before={p:p.read_bytes() for p in r.rglob('*') if p.is_file()}
                    with patch('urllib.request.urlopen',return_value=io.BytesIO(json.dumps(metadata).encode())) as opened:
                        with self.assertRaisesRegex(ValueError,'must be different files'):
                            if group=='laya': artifacts.download_laya(key,r)
                            else: artifacts.download(group,key,r)
                        self.assertEqual(opened.call_count,1)  # metadata only, mocked
                    self.assertEqual(before,{p:p.read_bytes() for p in r.rglob('*') if p.is_file()})

    def test_laya_receipt_copy_alias(self):
        import artifacts
        from unittest.mock import patch
        for kind in ('symlink','hardlink'):
            with tempfile.TemporaryDirectory(dir=Path.home()/'.cache') as tmp:
                r=Path(tmp);(r/'base').mkdir();source=r/'base/base-artifacts.json';source.write_text('sentinel')
                alias(source,r/'base-artifacts.json',kind)
                with patch('urllib.request.urlopen',side_effect=AssertionError('network forbidden')):
                    with self.assertRaisesRegex(ValueError,'must be different files'):
                        artifacts.download_laya('base',r)
                self.assertEqual(source.read_text(),'sentinel')

    def test_hosted_empty_input_never_calls_api_and_rejects_alias(self):
        for kind in ('lexical','symlink','hardlink'):
            with tempfile.TemporaryDirectory(dir=Path.home()/'.cache') as tmp:
                r=Path(tmp);source=r/'input';out=r/'out';out.mkdir()
                source.write_text('{"id":"synthetic","query":"","candidates":[]}\n')
                dest=out/'baseline-scores.jsonl'
                if kind=='lexical':
                    source.rename(dest);source=dest
                else: alias(source,dest,kind)
                self.run_guard(command(CPU/'qwen/jev_devcal.py','--input',source,'--output',out,'--allow-egress'),r)

    def test_valid_empty_selection_and_verification(self):
        with tempfile.TemporaryDirectory(dir=Path.home()/'.cache') as tmp:
            r=Path(tmp);source=r/'pools';source.write_text('[]')
            result=subprocess.run(command(SELECTION/'harness.py','select','--pools',source,'--out',r/'selected','--audit-dir',r),capture_output=True,text=True)
            self.assertEqual(result.returncode,0,result.stderr)
            source.write_text('{"id":"synthetic","query":"","candidates":[]}\n')
            (r/'scores').write_text('{"id":"synthetic","scores":[]}\n')
            result=subprocess.run(command(CPU/'jina/verify.py','--input',source,'--output',r/'scores','--report',r/'report'),capture_output=True,text=True)
            self.assertEqual(result.returncode,0,result.stderr)
            self.assertEqual(json.loads((r/'report').read_text())['rows'],1)


if __name__ == '__main__':
    unittest.main()

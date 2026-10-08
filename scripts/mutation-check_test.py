#!/usr/bin/env python3
"""Prove mutation outcomes using compilable, broken, unasserted and stale Go fixtures."""
import sys
sys.dont_write_bytecode = True
import importlib.util
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

SPEC=importlib.util.spec_from_file_location('mutation',Path(__file__).with_name('mutation-check.py'))
MODULE=importlib.util.module_from_spec(SPEC);SPEC.loader.exec_module(MODULE)

class MutationTest(unittest.TestCase):
    def test_default_manifest_includes_transport_and_file_regressions(self):
        cases=json.loads(Path(__file__).with_name('mutation-cases.json').read_text())
        required={
            'fetch-expired-deadline-in-decoder',
            'fetch-intermediate-decompression-budget',
            'fetch-content-encoding-layer-cap',
            'fetch-cancellation-before-body-read',
            'fetch-cancellation-after-body-read',
            'fetch-cancellation-in-buffered-decoder',
            'fetch-decompressed-byte-cap',
            'fetch-bypass-decompression',
            'fetch-utf16-budget',
            'fetch-url-idna-mapping',
            'fetch-address-family-interleave',
            'fetch-close-late-connections',
            'fetch-cancelled-winner',
            'fetch-block-fallback',
            'file-cancel-before-publication',
            'file-physical-parent',
            'file-missing-parent',
            'file-offset-range',
        }
        missing=required-{case['id'] for case in cases}
        self.assertEqual(missing,set(),'default mutation gate omits: '+', '.join(sorted(missing)))

    def test_invalid_manifests_fail_at_cli_before_execution(self):
        case=dict(id='value',file='internal/core/value/value.go',before='return 1',after='return 0',test='^TestValue$')
        invalid=[
            ('object root',json.dumps(case),'manifest must be an array'),
            ('null case','[null]','invalid case shape'),
            ('array case','[[]]','invalid case shape'),
            ('unknown field',json.dumps([dict(case,extra='value')]),'invalid case shape'),
            ('missing field',json.dumps([{key:value for key,value in case.items() if key!='test'}]),'invalid case shape'),
            ('empty id',json.dumps([dict(case,id='')]),'invalid case id'),
            ('padded id',json.dumps([dict(case,id=' value')]),'invalid case id'),
            ('duplicate id',json.dumps([case,case]),'duplicate case id'),
            ('duplicate field','['+json.dumps(case)[:-1]+',"id":"other"}]','duplicate JSON field'),
            ('empty before',json.dumps([dict(case,before='')]),'real replacement'),
            ('no replacement',json.dumps([dict(case,after=case['before'])]),'real replacement'),
            ('unanchored test',json.dumps([dict(case,test='TestValue')]),'exact test selection'),
            ('escaped path',json.dumps([dict(case,file='../value.go')]),'invalid source path'),
        ]
        for field in case:
            invalid.append((field+' type',json.dumps([dict(case,**{field:1})]),'case fields must be strings'))
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);manifest=root/'cases.json';report=root/'report.json'
            for name,contents,reason in invalid:
                with self.subTest(name=name):
                    manifest.write_text(contents)
                    report.write_text('stale report')
                    result=subprocess.run([sys.executable,str(Path(__file__).with_name('mutation-check.py')),
                                           '--root',str(root),'--manifest',str(manifest),'--report',str(report)],
                                          capture_output=True,text=True,timeout=30,check=False)
                    self.assertEqual(result.returncode,1)
                    self.assertIn('mutation: '+reason,result.stderr)
                    self.assertNotIn('Traceback',result.stderr)
                    self.assertFalse(report.exists())

    def test_real_go_outcomes_and_no_cache(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);pkg=root/'internal/core/value';pkg.mkdir(parents=True)
            (root/'go.mod').write_text('module example.test/probe\n\ngo 1.26.0\n')
            source=pkg/'value.go';source.write_text('package value\nfunc Value() int { return 1 }\n')
            tests=pkg/'value_test.go';tests.write_text('package value\nimport "testing"\nfunc TestValue(t *testing.T) { if Value()!=1 { t.Fatal("wrong value") } }\n')
            manifest=root/'cases.json';report=root/'report.json'
            case=dict(id='value',file='internal/core/value/value.go',before='return 1',after='return 0',test='^TestValue$')
            def check(status,passed=False):
                manifest.write_text(json.dumps([case]))
                code=MODULE.run(root,manifest,report,30)
                self.assertEqual(code,0 if passed else 1)
                self.assertEqual(json.loads(report.read_text())['results'][0]['status'],status)
                self.assertEqual(source.read_text(),'package value\nfunc Value() int { return 1 }\n')
            check('killed',True)
            tests.write_text('package value\nimport "testing"\nfunc TestValue(t *testing.T) { _ = Value() }\n')
            check('survived')
            case['after']='return missing';check('build-error')
            case['after']='return 0';case['test']='^TestAbsent$';check('baseline-no-tests')
            case['before']='return 2';check('stale-site')
            manifest.write_text('[]')
            with self.assertRaisesRegex(ValueError,'no-sites'): MODULE.run(root,manifest,report,30)

    def test_failures_are_not_kills(self):
        self.assertEqual(MODULE.verdict(None,''),'timeout')
        self.assertEqual(MODULE.verdict(127,'missing executable'),'infrastructure-error')
        self.assertEqual(MODULE.verdict(1,'{"Action":"fail","Package":"x"}\n'),'infrastructure-error')
        self.assertEqual(MODULE.verdict(0,''),'no-tests')

    def test_make_gate_rejects_a_surviving_mutation(self):
        repository=Path(__file__).resolve().parent.parent
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);pkg=root/'internal/core/value';pkg.mkdir(parents=True)
            (root/'go.mod').write_text('module example.test/probe\n\ngo 1.26.0\n')
            source=pkg/'value.go';original='package value\nfunc Value() int { return 1 }\n';source.write_text(original)
            tests=pkg/'value_test.go'
            scripts=root/'scripts';scripts.mkdir()
            shutil.copyfile(repository/'scripts/mutation-check.py',scripts/'mutation-check.py')
            (scripts/'mutation-cases.json').write_text(json.dumps([
                dict(id='probe',file='internal/core/value/value.go',before='return 1',after='return 0',test='^TestValue$')]))
            for asserted,status in ((True,'killed'),(False,'survived')):
                with self.subTest(status=status):
                    body='if Value()!=1 { t.Fatal("wrong value") }' if asserted else '_ = Value()'
                    tests.write_text('package value\nimport "testing"\nfunc TestValue(t *testing.T) { '+body+' }\n')
                    code,output=MODULE.command(['make','--no-print-directory','-f',str(repository/'Makefile'),'mutation'],root,60)
                    self.assertEqual(code,0 if asserted else 2,output)
                    self.assertIn('probe: '+status,output)
                    report=json.loads((root/'.cache/mutation/report.json').read_text())
                    self.assertEqual(report['results'][0]['status'],status)
                    self.assertEqual(source.read_text(),original)

    def test_process_timeout(self):
        import sys
        with tempfile.TemporaryDirectory() as directory:
            code,_=MODULE.command([sys.executable,'-c','import time;time.sleep(10)'],Path(directory),0.05)
            self.assertIsNone(code)

if __name__=='__main__':unittest.main()

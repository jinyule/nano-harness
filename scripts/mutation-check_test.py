#!/usr/bin/env python3
"""Prove mutation outcomes using compilable, broken, unasserted and stale Go fixtures."""
import sys
sys.dont_write_bytecode = True
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

SPEC=importlib.util.spec_from_file_location('mutation',Path(__file__).with_name('mutation-check.py'))
MODULE=importlib.util.module_from_spec(SPEC);SPEC.loader.exec_module(MODULE)

class MutationTest(unittest.TestCase):
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

    def test_process_timeout(self):
        import sys
        with tempfile.TemporaryDirectory() as directory:
            code,_=MODULE.command([sys.executable,'-c','import time;time.sleep(10)'],Path(directory),0.05)
            self.assertIsNone(code)

if __name__=='__main__':unittest.main()

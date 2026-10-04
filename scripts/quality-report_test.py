#!/usr/bin/env python3
"""Check quality-report failures and real analyzers using an isolated Go module."""
import sys
sys.dont_write_bytecode = True
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SPEC=importlib.util.spec_from_file_location('quality',Path(__file__).with_name('quality-report.py'))
MODULE=importlib.util.module_from_spec(SPEC);SPEC.loader.exec_module(MODULE)
ROOT=Path(__file__).resolve().parent.parent

class QualityTest(unittest.TestCase):
    def test_analyzer_error_is_not_an_observation(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            with patch.object(MODULE.subprocess,'run',side_effect=[subprocess.CompletedProcess([],0,'version 2.12.2 '),subprocess.CompletedProcess([],3,'','broken configuration')]):
                with self.assertRaisesRegex(ValueError,'analyzer failed'): MODULE.run(root,root/'report','')

    def test_real_analyzers_detect_injected_code(self):
        self.assertIsNotNone(shutil.which('golangci-lint'), 'make quality-tests requires pinned golangci-lint')
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            (root/'cmd').mkdir();(root/'internal').mkdir()
            (root/'go.mod').write_text('module example.test/quality\n\ngo 1.26.0\n')
            shutil.copyfile(ROOT/'.golangci-quality.yml',root/'.golangci-quality.yml')
            subprocess.run(['git','init','-q',str(root)],check=True)
            env=dict(os.environ,GIT_AUTHOR_NAME='Fixture',GIT_AUTHOR_EMAIL='fixture@example.invalid',GIT_COMMITTER_NAME='Fixture',GIT_COMMITTER_EMAIL='fixture@example.invalid')
            subprocess.run(['git','-C',str(root),'add','.'],check=True)
            subprocess.run(['git','-C',str(root),'-c','core.hooksPath=/dev/null','commit','-qm','fixture'],check=True,env=env)
            body='\n'.join(f'if x == {i} {{ x += {i+1} }}' for i in range(20))
            for name in ('one','two'):
                (root/'internal'/name).mkdir()
                (root/'internal'/name/'value.go').write_text('package '+name+'\nfunc Value(x int) int {\n'+body+'\nreturn x\n}\n')
            (root/'cmd/main.go').write_text('package main\nfunc main() {}\n')
            MODULE.run(root,root/'report','HEAD')
            data=json.loads((root/'report/summary.json').read_text())
            self.assertGreater(data['counts']['gocyclo'],0)
            self.assertGreater(data['counts']['dupl'],0)
            self.assertIn('internal/one/value.go',data['changed_files'])
            (root/'.golangci-quality.yml').write_text('version: \"2\"\nlinters: {enable: [nonexistent-analyzer]}\n')
            result=subprocess.run([sys.executable,str(ROOT/'scripts/quality-report.py'),'--output',str(root/'report')],cwd=root,capture_output=True,text=True)
            self.assertNotEqual(result.returncode,0)
            self.assertIn('quality: analyzer failed',result.stderr)
            self.assertFalse((root/'report/summary.json').exists())

if __name__=='__main__':unittest.main()

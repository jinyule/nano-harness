#!/usr/bin/env python3
"""Exercise the real publication command against a local fake GitHub CLI."""
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parent
FAKE_GH = '''#!/usr/bin/env python3
import os, sys, shutil
from pathlib import Path
root=Path(os.environ['RELEASE_FIXTURE'])
args=sys.argv[1:]; command=args[1]
assets=root/'remote'; mode=os.environ.get('RELEASE_SCENARIO','')
with (root/'calls').open('a') as log: log.write(command+'\\n')
if command=='view':
 if not (root/'exists').exists(): sys.exit(1)
 if '--json' in args:
  if mode=='read-failure': sys.exit(71)
  names=sorted(p.name for p in assets.iterdir())
  if mode=='duplicate' and names: names.append(names[0])
  print('\\n'.join(names),end='\\n' if names else '')
elif command=='create': (root/'exists').touch()
elif command=='download':
 name=args[args.index('--pattern')+1]; dest=Path(args[args.index('--dir')+1])
 shutil.copyfile(assets/name,dest/name)
elif command=='upload':
 if mode=='upload-failure': sys.exit(72)
 source=Path(args[3])
 if mode!='missing-after-upload': shutil.copyfile(source,assets/source.name)
 if mode=='extra-after-upload': (assets/'unexpected.exe').write_text('extra')
 if mode=='corrupt-after-upload': (assets/source.name).write_text('corrupt')
elif command=='edit': (root/'published').touch()
else: sys.exit(99)
'''

class PublicationTest(unittest.TestCase):
    def test_publication_contract(self):
        cases = {
            'new': None, 'partial': None, 'complete': None,
            'extra': 'unexpected remote asset', 'mismatch': 'different content',
            'duplicate': 'duplicate remote asset', 'read-failure': 'cannot read remote assets',
            'upload-failure': '', 'missing-after-upload': 'incomplete remote asset set',
            'extra-after-upload': 'unexpected remote asset',
            'corrupt-after-upload': 'differs after upload',
        }
        for scenario, reason in cases.items():
            with self.subTest(scenario=scenario), tempfile.TemporaryDirectory() as directory:
                root=Path(directory)
                payload=root/'payload'; payload.mkdir()
                remote=root/'remote'; remote.mkdir()
                entries=[]
                for system in ('Darwin','Linux','Windows'):
                    for arch in ('arm64','x86_64'):
                        name=f'nano-harness_1.2.3_{system}_{arch}.'+('zip' if system=='Windows' else 'tar.gz')
                        data=name.encode(); (payload/name).write_bytes(data)
                        entries.append(f'{hashlib.sha256(data).hexdigest()}  {name}\n')
                (payload/'checksums.txt').write_text(''.join(entries))
                if scenario!='new': (root/'exists').touch()
                if scenario in ('complete','extra','mismatch','duplicate'):
                    shutil.copytree(payload,remote,dirs_exist_ok=True)
                if scenario=='partial': shutil.copyfile(payload/'checksums.txt',remote/'checksums.txt')
                if scenario=='extra': (remote/'unexpected.exe').write_text('extra')
                if scenario=='mismatch': (remote/'checksums.txt').write_text('different')
                binary=root/'bin'; binary.mkdir(); gh=binary/'gh';gh.write_text(FAKE_GH);gh.chmod(0o700)
                env=dict(os.environ,PATH=str(binary)+os.pathsep+os.environ['PATH'],RELEASE_FIXTURE=str(root),RELEASE_SCENARIO=scenario)
                result=subprocess.run([str(SCRIPTS/'publish-release.sh'),str(payload),'v1.2.3'],env=env,capture_output=True,text=True,timeout=30)
                if reason is None:
                    self.assertEqual(result.returncode,0,result.stdout+result.stderr)
                    self.assertTrue((root/'published').exists())
                    self.assertEqual(sorted(p.name for p in payload.iterdir()),sorted(p.name for p in remote.iterdir()))
                    for p in payload.iterdir(): self.assertEqual(p.read_bytes(),(remote/p.name).read_bytes())
                else:
                    self.assertNotEqual(result.returncode,0,result.stdout+result.stderr)
                    self.assertFalse((root/'published').exists())
                    self.assertIn(reason,result.stderr)
                    if scenario=='upload-failure': self.assertIn('upload',(root/'calls').read_text())
                    if scenario=='extra': self.assertNotIn('upload',(root/'calls').read_text())

if __name__=='__main__': unittest.main()

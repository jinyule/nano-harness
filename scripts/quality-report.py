#!/usr/bin/env python3
"""Report whole-corpus Go complexity and duplication without treating findings as errors."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys


def run(root, destination, base):
    destination.mkdir(parents=True,exist_ok=True)
    for name in ('summary.json','lint.json','duplicates.txt'): (destination/name).unlink(missing_ok=True)
    version = subprocess.run(['golangci-lint','version'],cwd=root,capture_output=True,text=True,check=True).stdout
    if 'version 2.12.2 ' not in version:
        raise ValueError('quality: golangci-lint v2.12.2 is required')
    destination.mkdir(parents=True,exist_ok=True)
    raw=destination/'lint.json'
    raw.unlink(missing_ok=True)
    result=subprocess.run(['golangci-lint','run','-c','.golangci-quality.yml','--issues-exit-code=42',
                           '--output.json.path',str(raw),'./cmd/...','./internal/...'],cwd=root,capture_output=True,text=True)
    if result.returncode not in (0,42):
        raise ValueError(f'quality: analyzer failed ({result.returncode}): {result.stderr}')
    data=json.loads(raw.read_text())
    issues=data.get('Issues') or []
    if any(i['FromLinter'] not in ('gocyclo','dupl') for i in issues):
        raise ValueError('quality: unexpected analyzer diagnostic')
    if (result.returncode==42) != bool(issues):
        raise ValueError('quality: analyzer status disagrees with report')
    # golangci-lint runs dupl per package. The pinned standalone command compares
    # all product files, including build-tagged files, across package boundaries.
    files=sorted(str(p.relative_to(root)) for tree in ('cmd','internal') for p in (root/tree).rglob('*.go')
                 if not p.name.endswith('_test.go') and 'tools' not in p.relative_to(root).parts and 'testdata' not in p.parts)
    if not files: raise ValueError('quality: no product source files')
    tool_dir=destination/'tools';tool_dir.mkdir(exist_ok=True)
    subprocess.run(['go','install','github.com/golangci/dupl@v0.0.0-20260401084720-c99c5cf5c202'],
                   cwd=root,env=dict(os.environ,GOBIN=str(tool_dir)),capture_output=True,text=True,check=True)
    duplicate=subprocess.run([str(tool_dir/'dupl'),'-plumbing','-threshold','100','-files'],input='\n'.join(files)+'\n',
                             cwd=root,capture_output=True,text=True,check=True)
    if duplicate.stderr.strip(): raise ValueError('quality: dupl diagnostics: '+duplicate.stderr)
    (destination/'duplicates.txt').write_text(duplicate.stdout)
    changed=None
    if base:
        merge=subprocess.run(['git','merge-base',base,'HEAD'],cwd=root,capture_output=True,text=True,check=True).stdout.strip()
        changed=subprocess.run(['git','diff','--name-only',merge,'--','cmd','internal'],cwd=root,capture_output=True,text=True,check=True).stdout.splitlines()
        untracked=subprocess.run(['git','ls-files','--others','--exclude-standard','--','cmd','internal'],cwd=root,capture_output=True,text=True,check=True).stdout.splitlines()
        changed=sorted(set(changed+untracked))
    head=subprocess.run(['git','rev-parse','HEAD'],cwd=root,capture_output=True,text=True,check=True).stdout.strip()
    summary={'mode':'observation','head':head,'base':base,'tool':version.strip(),'changed_files':changed,
             'counts':{'gocyclo':sum(i['FromLinter']=='gocyclo' for i in issues),'dupl':len(duplicate.stdout.splitlines())},
             'issues':issues}
    (destination/'summary.json').write_text(json.dumps(summary,indent=2)+'\n')
    print(json.dumps({key:value for key,value in summary.items() if key!='issues'},indent=2))


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output',default='.cache/quality')
    parser.add_argument('--base',default='')
    args=parser.parse_args()
    try:
        run(Path.cwd(),Path(args.output).resolve(),args.base)
    except (ValueError,OSError,subprocess.CalledProcessError) as error:
        print(error,file=sys.stderr);return 1
    return 0

if __name__=='__main__':sys.exit(main())

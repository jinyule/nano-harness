#!/usr/bin/env python3
"""Run reviewed regression mutations in a private source copy; only named test failures kill them."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile


def command(args,root,timeout):
    with tempfile.TemporaryFile(mode='w+') as output:
        with subprocess.Popen(args,cwd=root,stdout=output,stderr=subprocess.STDOUT,start_new_session=True) as process:
            try:
                code=process.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid,signal.SIGKILL)
                process.wait()
                code=None
        output.seek(0)
        return code,output.read()


def verdict(code,output):
    if code is None: return 'timeout'
    events=[]
    for line in output.splitlines():
        try: events.append(json.loads(line))
        except json.JSONDecodeError: pass
    passed=[e for e in events if e.get('Test') and e.get('Action')=='pass']
    failed=[e for e in events if e.get('Test') and e.get('Action')=='fail']
    if code==0: return 'survived' if passed else 'no-tests'
    if code==1 and failed: return 'killed'
    return 'infrastructure-error'


def copy_source(root,destination):
    for name in ('go.mod','go.sum','cmd','internal'):
        source=root/name
        if not source.exists(): continue
        if source.is_symlink() or (source.is_dir() and any(p.is_symlink() for p in source.rglob('*'))):
            raise ValueError('mutation: source symlinks are not supported')
        if source.is_dir(): shutil.copytree(source,destination/name)
        else: shutil.copyfile(source,destination/name)


def run(root,manifest,report,timeout):
    if os.name!='posix': raise ValueError('mutation: process-group cleanup requires a Unix host')
    report.parent.mkdir(parents=True,exist_ok=True)
    report.unlink(missing_ok=True)
    cases=json.loads(manifest.read_text())
    if not cases: raise ValueError('mutation: no-sites')
    ids=set();results=[]
    for case in cases:
        if set(case)!={'id','file','before','after','test'} or case['id'] in ids:
            raise ValueError('mutation: invalid or duplicate case')
        ids.add(case['id'])
        path=Path(case['file'])
        if path.is_absolute() or '..' in path.parts or not path.parts or path.parts[0] not in ('cmd','internal') or path.suffix!='.go' or path.name.endswith('_test.go'):
            raise ValueError('mutation: invalid source path')
        if not case['before'] or case['before']==case['after'] or not case['test'].startswith('^Test') or not case['test'].endswith('$'):
            raise ValueError('mutation: an exact test selection and real replacement are required')
    with tempfile.TemporaryDirectory(prefix='nano-mutation-') as directory:
        private=Path(directory);copy_source(root,private)
        digest=hashlib.sha256()
        for source in sorted(p for p in private.rglob('*') if p.is_file()):
            digest.update(str(source.relative_to(private)).encode()+b'\0'+source.read_bytes()+b'\0')
        version=subprocess.run(['go','version'],capture_output=True,text=True,check=True).stdout.strip()
        for case in cases:
            path=private/case['file'];original=path.read_text()
            item={'id':case['id'],'file':case['file'],'test':case['test'],'source_sha256':hashlib.sha256(original.encode()).hexdigest()}
            results.append(item)
            if original.count(case['before'])!=1:
                item['status']='stale-site';continue
            package='./'+str(Path(case['file']).parent)
            test=['go','test','-json','-count=1','-run',case['test'],package]
            code,output=command(test,private,timeout)
            baseline=verdict(code,output)
            if baseline!='survived':
                item.update(status='baseline-'+baseline,output=output[-12000:]);continue
            try:
                path.write_text(original.replace(case['before'],case['after'],1))
                binary=private/'mutation.test'
                code,output=command(['go','test','-c','-o',str(binary),package],private,timeout)
                if code!=0:
                    item.update(status='timeout' if code is None else 'build-error',output=output[-12000:]);continue
                code,output=command(test,private,timeout)
                item.update(status=verdict(code,output),output=output[-12000:])
            finally:
                path.write_text(original)
    report.parent.mkdir(parents=True,exist_ok=True)
    report.write_text(json.dumps({'mode':'targeted-regressions','cache':False,'coverage_selection':False,'toolchain':version,
                                 'source_tree_sha256':digest.hexdigest(),'manifest_sha256':hashlib.sha256(manifest.read_bytes()).hexdigest(),'results':results},indent=2)+'\n')
    for item in results: print(item['id']+': '+item['status'])
    return 0 if results and all(item['status']=='killed' for item in results) else 1


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root',type=Path,default=Path.cwd())
    parser.add_argument('--manifest',type=Path,default=Path('scripts/mutation-cases.json'))
    parser.add_argument('--report',type=Path,default=Path('.cache/mutation/report.json'))
    parser.add_argument('--timeout',type=float,default=60)
    args=parser.parse_args()
    try:
        if args.timeout<=0: raise ValueError('mutation: timeout must be positive')
        return run(args.root.resolve(),args.manifest.resolve(),args.report.resolve(),args.timeout)
    except (ValueError,OSError,subprocess.CalledProcessError) as error:
        print(error,file=sys.stderr);return 1

if __name__=='__main__':sys.exit(main())

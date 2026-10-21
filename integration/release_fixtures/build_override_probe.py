"""Source-override regression against actual builder in a miniature tagged clone."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

SOURCE=Path(sys.argv.pop(1)).resolve()

class SourceOverride(unittest.TestCase):
    def test_external_source_overrides_rejected_or_ignored(self):
        with tempfile.TemporaryDirectory(prefix='cb-release-override-') as temp:
            root=Path(temp);clone=root/'source'
            env=os.environ.copy()
            env.update(GIT_CONFIG_NOSYSTEM='1',GIT_CONFIG_GLOBAL=os.devnull,GIT_TERMINAL_PROMPT='0',GIT_AUTHOR_NAME='Fixture',GIT_AUTHOR_EMAIL='fixture@example.invalid',GIT_COMMITTER_NAME='Fixture',GIT_COMMITTER_EMAIL='fixture@example.invalid',GOPROXY='off',GOSUMDB='off',GOTOOLCHAIN='local',GOFLAGS='',GOWORK='off')
            def run(args, cwd=clone):
                r=subprocess.run(args,cwd=cwd,env=env,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=120)
                self.assertEqual(r.returncode,0,(args,r.stdout.decode(errors='replace')))
                return r.stdout
            run(['git','clone','--quiet','--no-hardlinks','--no-local',str(SOURCE),str(clone)],SOURCE)
            for name in ['scripts/build-release.sh','scripts/install.sh','scripts/install.ps1','internal/buildinfo/buildinfo.go']:
                dst=clone/name;dst.parent.mkdir(parents=True,exist_ok=True);shutil.copy2(SOURCE/name,dst)
            main=clone/'cmd/clawbridge/main.go'
            main.write_text('package main\nimport("fmt";"clawbridge/internal/buildinfo";"example.invalid/releasefixture")\nfunc main(){fmt.Println(buildinfo.Version,releasefixture.Value())}\n')
            gomod='module clawbridge\n\ngo 1.26.0\n\nrequire example.invalid/releasefixture v0.0.0\nreplace example.invalid/releasefixture => ./fixture-dependency\n'
            (clone/'go.mod').write_text(gomod)
            dependency=clone/'fixture-dependency';dependency.mkdir()
            (dependency/'go.mod').write_text('module example.invalid/releasefixture\n\ngo 1.26.0\n')
            (dependency/'value.go').write_text('package releasefixture\nfunc Value()string{return "TRACKED-SOURCE"}\n')
            git=['git','-c','core.hooksPath='+os.devnull,'-c','commit.gpgsign=false']
            run(git+['add','-A']);run(git+['commit','--quiet','-m','isolated source override regression']);run(git+['tag','v1.2.3'])
            external=root/'external';external.mkdir()
            shutil.copy2(dependency/'go.mod',external/'go.mod')
            marker='EXTERNAL_SOURCE_OVERRIDE_620ca4bb'
            (external/'value.go').write_text('package releasefixture\nfunc Value()string{return "'+marker+'"}\n')
            alternate=root/'main-override.go'
            alternate.write_text('package main\nimport("fmt";"clawbridge/internal/buildinfo")\nfunc main(){fmt.Println(buildinfo.Version,"'+marker+'!")}\n')
            overlay=root/'overlay.json';overlay.write_text(json.dumps({'Replace':{str(main):str(alternate)}}))
            modfile=root/'override.mod';modfile.write_text(gomod.replace('./fixture-dependency',str(external)))
            goenv=root/'external-goenv';goenv.write_text('GOFLAGS=-modfile='+str(modfile)+'\n')
            workspace=root/'go.work';workspace.write_text('go 1.26.0\nuse (\n'+json.dumps(str(clone))+'\n'+json.dumps(str(external))+'\n)\n')
            variants=[('overlay',{'GOFLAGS':'-overlay='+str(overlay),'GOWORK':'off'}),('modfile',{'GOFLAGS':'-modfile='+str(modfile),'GOWORK':'off'}),('workspace',{'GOFLAGS':'','GOWORK':str(workspace)}),('goenv-modfile',{'GOFLAGS':'','GOWORK':'off','GOENV':str(goenv)})]
            for name, overrides in variants:
                with self.subTest(source_override=name):
                    actual=env.copy();actual.update(overrides)
                    output=root/('output-'+name)
                    r=subprocess.run(['bash',str(clone/'scripts/build-release.sh'),'v1.2.3',str(output)],cwd=clone,env=actual,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=120)
                    raw=list(output.glob('clawbridge_*')) if output.exists() else []
                    contaminated=[f.name for f in raw if marker.encode() in f.read_bytes()]
                    self.assertEqual(contaminated,[], 'builder emitted code from outside clean tagged source via '+name+'; exit='+str(r.returncode))
                    if r.returncode:
                        self.assertEqual(raw,[],'rejection left executable payloads')
                    else:
                        self.assertEqual(len(raw),5,'successful sanitized build missing targets')
            self.assertEqual(run(git+['status','--porcelain']).strip(),b'','builder mutated tracked source')

if __name__=='__main__':unittest.main(verbosity=2)

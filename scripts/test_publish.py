#!/usr/bin/env python3
"""Publication transaction fixture: a fake gh transport, never a real release."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
GH = r'''#!PYTHON
import json,os,pathlib,shutil,sys
p=pathlib.Path(os.environ['PUBLISH_FIXTURE']);args=sys.argv[1:];mode=os.environ.get('PUBLISH_MODE','ok')
with (p/'calls').open('a') as f:f.write(json.dumps(args)+'\n')
if args[0]=='api':
 if mode=='api-failure':sys.exit(1)
 if mode=='existing':print('123')
elif args[:2]==['release','create']:
 assert '--draft' in args and '--verify-tag' in args
 assert '--notes-file' in args
 assert pathlib.Path(args[args.index('--notes-file')+1]).read_text()=='Tagged release fixture notes\n'
 (p/'draft').write_text('draft')
elif args[:2]==['release','upload']:
 assert (p/'draft').exists() and '--clobber' not in args
 remote=p/'remote';remote.mkdir()
 for name in args[3:]:shutil.copyfile(name,remote/pathlib.Path(name).name)
 if mode=='upload-failure':sys.exit(1)
elif args[:2]==['release','download']:
 out=pathlib.Path(args[args.index('--dir')+1])
 for src in (p/'remote').iterdir():shutil.copyfile(src,out/src.name)
 if mode=='download-mismatch':(out/'install.sh').write_text('corrupt')
 if mode=='extra-asset':(out/'extra').write_text('unexpected')
elif args[:2]==['release','view']:print('true')
elif args[:2]==['release','edit']:
 assert '--draft=false' in args
 (p/'published').write_text('published')
else:raise RuntimeError(args)
'''


class PublishTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="clawbridge-publish-fixture-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.repo = self.root / "source"
        self.repo.mkdir()
        (self.repo / "marker").write_text("isolated tag fixture")
        (self.repo / "RELEASE-NOTES.md").write_text("Tagged release fixture notes\n")
        self.env = os.environ.copy()
        self.env.update(GIT_AUTHOR_NAME="Fixture", GIT_AUTHOR_EMAIL="fixture@example.test", GIT_COMMITTER_NAME="Fixture", GIT_COMMITTER_EMAIL="fixture@example.test", GITHUB_ACTIONS="true", GITHUB_EVENT_NAME="push", GITHUB_REF="refs/tags/v0.1.0", GITHUB_REPOSITORY="jkai-chen/ClawBridge", PUBLISH_FIXTURE=str(self.root))
        for args in [("init", "-q"), ("add", "."), ("commit", "-qm", "fixture"), ("tag", "v0.1.0")]:
            subprocess.run(["git", *args], cwd=self.repo, env=self.env, check=True, capture_output=True)
        commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=self.repo, text=True).strip()
        self.assets = self.root / "assets"
        self.assets.mkdir()
        for name in ["LICENSE", "install.sh", "install.ps1", *["clawbridge_v0.1.0_" + t for t in ["darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64", "windows_amd64.exe"]]]:
            (self.assets / name).write_text("fixture bytes " + name)
        (self.assets / "SOURCE-COMMIT.txt").write_text(commit + "\n")
        (self.assets / "SHA256SUMS").write_text("".join(hashlib.sha256(p.read_bytes()).hexdigest() + "  " + p.name + "\n" for p in sorted(self.assets.iterdir())))
        stub = self.root / "gh"
        stub.write_text(GH.replace("#!PYTHON", "#!" + sys.executable))
        stub.chmod(0o755)
        self.env["PATH"] = str(self.root) + os.pathsep + self.env["PATH"]

    def publish(self, mode="ok", ok=False):
        result = subprocess.run(["bash", str(ROOT / "scripts/publish-release.sh"), "v0.1.0", str(self.assets)], cwd=self.repo, env=dict(self.env, PUBLISH_MODE=mode), capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, ok, result.stdout + result.stderr)
        return [json.loads(line) for line in (self.root / "calls").read_text().splitlines()] if (self.root / "calls").exists() else []

    def test_complete_draft_then_verify_then_publish(self):
        # Mutable checkout text must not replace the notes at the exact tag.
        (self.repo / "RELEASE-NOTES.md").write_text("uncommitted notes must not publish\n")
        calls = self.publish(ok=True)
        self.assertEqual([c[:2] for c in calls[1:]], [["release", "create"], ["release", "upload"], ["release", "download"], ["release", "view"], ["release", "edit"]])
        self.assertTrue((self.root / "published").exists())

    def test_existing_or_api_failure_never_creates(self):
        for mode in ["existing", "api-failure"]:
            with self.subTest(mode=mode):
                self.publish(mode)
                self.assertFalse((self.root / "draft").exists())
                self.assertFalse((self.root / "published").exists())

    def test_failed_upload_leaves_only_draft(self):
        self.publish("upload-failure")
        self.assertTrue((self.root / "draft").exists())
        self.assertFalse((self.root / "published").exists())

    def test_bad_download_leaves_only_draft(self):
        self.publish("download-mismatch")
        self.assertTrue((self.root / "draft").exists())
        self.assertFalse((self.root / "published").exists())

    def test_extra_remote_asset_never_publishes(self):
        self.publish("extra-asset")
        self.assertFalse((self.root / "published").exists())

    def test_dispatch_and_bad_local_payload_never_contact_gh(self):
        self.env["GITHUB_EVENT_NAME"] = "workflow_dispatch"
        self.assertEqual(self.publish(), [])
        self.env["GITHUB_EVENT_NAME"] = "push"
        (self.assets / "unexpected").write_text("extra")
        self.assertEqual(self.publish(), [])


if __name__ == "__main__":
    unittest.main()

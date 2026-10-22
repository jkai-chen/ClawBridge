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
# The checks below are asserts; PYTHONOPTIMIZE would strip them silently.
if sys.flags.optimize: sys.exit('fixture stub needs asserts enabled (unset PYTHONOPTIMIZE)')
p=pathlib.Path(os.environ['PUBLISH_FIXTURE']);args=sys.argv[1:];mode=os.environ.get('PUBLISH_MODE','ok')
with (p/'calls').open('a') as f:f.write(json.dumps(args)+'\n')
if args[0]=='api':
 query=args[args.index('--jq')+1]
 if 'prerelease' in query:
  # The latest-release query: one JSON object per published or draft release.
  if mode=='latest-api-failure':sys.exit(1)
  for release in json.loads(os.environ.get('PUBLISH_RELEASES','[]')):print(json.dumps(release))
 else:
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
 latest=[a for a in args if a.startswith('--latest')]
 assert latest in (['--latest=true'],['--latest=false']),latest
 (p/'published').write_text(latest[0].split('=')[1])
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
        self.assertEqual([c[:2] for c in calls[1:]], [["release", "create"], ["release", "upload"], ["release", "download"], ["release", "view"], ["api", "--paginate"], ["release", "edit"]])
        self.assertEqual((self.root / "published").read_text(), "true")

    def publish_latest(self, releases):
        self.env["PUBLISH_RELEASES"] = json.dumps([{"tag_name": tag, "draft": draft, "prerelease": prerelease} for tag, draft, prerelease in releases])
        self.publish(ok=True)
        return (self.root / "published").read_text()

    def test_first_release_becomes_latest(self):
        self.assertEqual(self.publish_latest([]), "true")

    def test_newest_stable_release_becomes_latest(self):
        self.assertEqual(self.publish_latest([("v0.0.9", False, False), ("v0.0.10", False, False)]), "true")

    def test_backport_does_not_become_latest(self):
        self.assertEqual(self.publish_latest([("v0.10.0", False, False), ("v0.0.3", False, False)]), "false")

    def test_prereleases_drafts_and_noncanonical_tags_are_ignored(self):
        self.assertEqual(self.publish_latest([("v0.2.0", False, True), ("v0.3.0", True, False), ("v0.4.0-rc1", False, False), ("nightly", False, False), ("v0.0.1", False, False)]), "true")

    def test_failed_latest_query_leaves_only_draft(self):
        calls = self.publish("latest-api-failure")
        self.assertTrue((self.root / "draft").exists())
        self.assertFalse((self.root / "published").exists())
        self.assertNotIn(["release", "edit"], [c[:2] for c in calls])

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

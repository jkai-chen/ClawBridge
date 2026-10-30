#!/usr/bin/env python3
"""Publication transaction fixture: a fake gh transport, never a real release."""
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.dont_write_bytecode = True
import release_body  # noqa: E402

ROOT = Path(__file__).resolve().parent.parent
# The tagged notes' top heading: the release being published (release_body.py
# refuses any other).
TOP = "# ClawBridge v0.1.0 (released 1 Jan)"
BINARIES = ["clawbridge_v0.1.0_" + t for t in ["darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64", "windows_amd64.exe", "windows_arm64.exe"]]
# Every asset a release publishes: the attested ones, SHA256SUMS, and the
# attestation bundle (FR24).
PUBLISHED = {"LICENSE", "install.sh", "install.ps1", "SOURCE-COMMIT.txt", "IMAGE-DIGEST.txt", "AGENTD-IMAGE-DIGEST.txt", "AGENT-IMAGE-DIGEST.txt", "SHA256SUMS", "PROVENANCE.sigstore.json", *BINARIES}
DIGEST = "sha256:" + "ab" * 32
IMAGE = "ghcr.io/jkai-chen/clawbridge@" + DIGEST
# The agentd (egress) and dispatcher agent images: each an env the workflow
# passes, its digest asset and the line that asset must hold.
AGENTD_DIGEST = "sha256:" + "12" * 32
AGENT_DIGEST = "sha256:" + "34" * 32
IMAGE_ASSETS = {
    "AGENTD_IMAGE_DIGEST": ("AGENTD-IMAGE-DIGEST.txt", "ghcr.io/jkai-chen/clawbridge-agentd@" + AGENTD_DIGEST),
    "AGENT_IMAGE_DIGEST": ("AGENT-IMAGE-DIGEST.txt", "ghcr.io/jkai-chen/clawbridge-dispatcher-agent@" + AGENT_DIGEST),
}
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
elif args[:2]==['attestation','verify'] and pathlib.Path(args[2]).parent.name=='upload':
 # The tag-push run: the bundle's signature on one asset, before anything is
 # created. publish-draft's check of the downloaded draft is below.
 upload=pathlib.Path(args[2]).parent
 assert args==['attestation','verify',str(upload/'clawbridge_v0.1.0_linux_amd64'),'--bundle',str(upload/'PROVENANCE.sigstore.json'),'--repo','jkai-chen/ClawBridge','--signer-workflow','jkai-chen/ClawBridge/.github/workflows/release.yml','--source-ref','refs/tags/v0.1.0','--source-digest',(upload/'SOURCE-COMMIT.txt').read_text().strip(),'--predicate-type','https://slsa.dev/provenance/v1','--deny-self-hosted-runners'],args
 assert upload.name=='upload' and (upload/'PROVENANCE.sigstore.json').is_file(),args
 if mode=='attestation-failure':sys.exit(1)
elif args[:2]==['release','create']:
 assert '--draft' in args and '--verify-tag' in args
 assert '--notes-file' in args
 notes=pathlib.Path(args[args.index('--notes-file')+1]).read_text()
 (p/'notes').write_text(notes)
 # The tagged notes' top section, the link to the full notes, then the image
 # the workflow pushed.
 assert '\n\nThe full release notes: https://github.com/jkai-chen/ClawBridge/blob/v0.1.0/RELEASE-NOTES.md\n\n## Server image\n' in notes,notes
 assert '`'+os.environ['FIXTURE_IMAGE']+'`' in notes and 'gh attestation verify oci://'+os.environ['FIXTURE_IMAGE'] in notes,notes
 # Then the two images agentd deploys, each with its own verify line.
 for name,var in (('clawbridge-agentd','AGENTD_IMAGE_DIGEST'),('clawbridge-dispatcher-agent','AGENT_IMAGE_DIGEST')):
  ref='ghcr.io/jkai-chen/'+name+'@'+os.environ[var]
  assert '`'+ref+'`' in notes and 'gh attestation verify oci://'+ref in notes,notes
 (p/'draft').write_text('draft')
 # The release as gh release view reports it: a draft of this tag, targeting
 # the default branch by name.
 (p/'release.json').write_text(json.dumps({'isDraft':True,'tagName':args[2],'targetCommitish':'main'}))
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
elif args[:2]==['release','view']:
 # A missing release fails, as gh does; tests edit release.json to stand
 # for a published release or another tag's.
 if not (p/'release.json').exists():sys.exit('release not found')
 release=json.loads((p/'release.json').read_text());query=args[args.index('--jq')+1]
 if query=='.isDraft':print(json.dumps(release['isDraft']))
 else:
  assert query=='"\\(.isDraft) \\(.tagName) \\(.targetCommitish)"',query
  print(json.dumps(release['isDraft']),release['tagName'],release['targetCommitish'])
elif args[:2]==['attestation','verify']:
 # Stands in for Sigstore: a bundle verifies only when it carries the
 # fixture's signature, which stands for release.yml's at this tag.
 bundle=json.loads(pathlib.Path(args[args.index('--bundle')+1]).read_text())
 if mode=='attest-failure' or bundle['dsseEnvelope']['signatures']!=[{'sig':'c3ludGhldGlj'}]:sys.exit('verification failed')
elif args[:2]==['release','edit']:
 assert '--draft=false' in args
 latest=[a for a in args if a.startswith('--latest')]
 assert latest in (['--latest=true'],['--latest=false']),latest
 (p/'published').write_text(latest[0].split('=')[1])
 if '--notes-file' in args:(p/'published-notes').write_text(pathlib.Path(args[args.index('--notes-file')+1]).read_text())
 release=json.loads((p/'release.json').read_text());release['isDraft']=False
 (p/'release.json').write_text(json.dumps(release))
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
        (self.repo / "RELEASE-NOTES.md").write_text(TOP + "\n\nTagged release fixture notes\n")
        self.env = os.environ.copy()
        self.env.update(GIT_AUTHOR_NAME="Fixture", GIT_AUTHOR_EMAIL="fixture@example.test", GIT_COMMITTER_NAME="Fixture", GIT_COMMITTER_EMAIL="fixture@example.test", GITHUB_ACTIONS="true", GITHUB_EVENT_NAME="push", GITHUB_REF="refs/tags/v0.1.0", GITHUB_REPOSITORY="jkai-chen/ClawBridge", PUBLISH_FIXTURE=str(self.root), IMAGE_DIGEST=DIGEST, AGENTD_IMAGE_DIGEST=AGENTD_DIGEST, AGENT_IMAGE_DIGEST=AGENT_DIGEST, FIXTURE_IMAGE=IMAGE)
        for args in [("init", "-q"), ("add", "."), ("commit", "-qm", "fixture"), ("tag", "v0.1.0")]:
            subprocess.run(["git", *args], cwd=self.repo, env=self.env, check=True, capture_output=True)
        commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=self.repo, text=True).strip()
        self.assets = self.root / "assets"
        self.assets.mkdir()
        for name in ["LICENSE", "install.sh", "install.ps1", *BINARIES]:
            (self.assets / name).write_text("fixture bytes " + name)
        (self.assets / "SOURCE-COMMIT.txt").write_text(commit + "\n")
        (self.assets / "IMAGE-DIGEST.txt").write_text(IMAGE + "\n")
        for name, line in IMAGE_ASSETS.values():
            (self.assets / name).write_text(line + "\n")
        (self.assets / "SHA256SUMS").write_text("".join(hashlib.sha256(p.read_bytes()).hexdigest() + "  " + p.name + "\n" for p in sorted(self.assets.iterdir())))
        stub = self.root / "gh"
        stub.write_text(GH.replace("#!PYTHON", "#!" + sys.executable))
        stub.chmod(0o755)
        self.env["PATH"] = str(self.root) + os.pathsep + self.env["PATH"]
        self.bundle = self.root / "bundle.json"
        self.env["PROVENANCE_BUNDLE"] = str(self.bundle)
        # Tests that hand-craft the bundle set this to False.
        self.fresh_bundle = True

    def write_bundle(self, change=None, predicate="https://slsa.dev/provenance/v1", payload_type="application/vnd.in-toto+json", signatures=({"sig": "c3ludGhldGlj"},)):
        """A Sigstore-shaped bundle whose in-toto statement names every
        attested asset and SHA256SUMS by digest, as attest-build-provenance
        writes it; change edits the subject map first, and the keywords
        replace the statement's or envelope's fields."""
        subjects = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in self.assets.iterdir()}
        if change:
            change(subjects)
        statement = {"_type": "https://in-toto.io/Statement/v1", "subject": [{"name": n, "digest": {"sha256": d}} for n, d in sorted(subjects.items())], "predicateType": predicate, "predicate": {}}
        bundle = {"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json", "verificationMaterial": {}, "dsseEnvelope": {"payload": base64.b64encode(json.dumps(statement).encode()).decode(), "payloadType": payload_type, "signatures": list(signatures)}}
        self.bundle.write_text(json.dumps(bundle))
        self.fresh_bundle = False

    def remote(self):
        return {p.name for p in (self.root / "remote").iterdir()}

    def reset(self):
        """Forget an earlier publish in this test: the fake's remote, draft,
        release and calls."""
        shutil.rmtree(self.root / "remote", ignore_errors=True)
        for f in ["draft", "published", "published-notes", "calls", "release.json"]:
            (self.root / f).unlink(missing_ok=True)

    def retag(self, notes):
        """Commit notes as RELEASE-NOTES.md and move the tag and the source
        marker to that commit."""
        (self.repo / "RELEASE-NOTES.md").write_text(notes)
        for args in [("add", "."), ("commit", "-qm", "notes"), ("tag", "-f", "v0.1.0")]:
            subprocess.run(["git", *args], cwd=self.repo, env=self.env, check=True, capture_output=True)
        commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=self.repo, text=True).strip()
        (self.assets / "SOURCE-COMMIT.txt").write_text(commit + "\n")
        (self.assets / "SHA256SUMS").write_text("".join(hashlib.sha256(p.read_bytes()).hexdigest() + "  " + p.name + "\n" for p in sorted(self.assets.iterdir()) if p.name != "SHA256SUMS"))

    def test_the_body_is_the_top_section_of_a_long_file(self):
        # v0.3.10's failure: the whole file was the body, over GitHub's limit.
        older = "".join("# ClawBridge v0.0.%d (released)\n\n- " % i + "x" * 2000 + "\n\n" for i in range(80))
        self.retag(TOP + "\n\n## Linear\n\n- the new bullet\n\n" + older)
        self.publish(ok=True)
        notes = (self.root / "notes").read_text()
        self.assertTrue(notes.startswith(TOP + "\n\n## Linear\n\n- the new bullet\n\nThe full release notes: "), notes[:200])
        self.assertNotIn("v0.0.1 (released)", notes)
        self.assertIn("## Server image", notes)
        self.assertLess(len(notes), release_body.MAX_BODY)

    def test_a_top_section_too_long_fails_before_any_release(self):
        self.retag(TOP + "\n\n- " + "y" * (release_body.MAX_BODY + 10) + "\n\n# ClawBridge v0.0.1\n")
        calls = self.publish()
        self.assertNotIn(["release", "create"], [c[:2] for c in calls])

    def test_the_top_heading_must_name_this_release(self):
        # The cut renames "# Unreleased"; a tag on notes it didn't fails here,
        # before anything is created.
        cases = {
            "unreleased": ("# Unreleased\n\n- a\n\n# ClawBridge v0.0.1 (released 1 Jan)\n", "is '# Unreleased', not \"# ClawBridge v0.1.0 (released DATE)\": rename it at the cut"),
            "another version": ("# ClawBridge v0.0.9 (released 1 Jan)\n\n- a\n", "is for v0.0.9, not v0.1.0"),
            "no date": ("# ClawBridge v0.1.0\n\n- a\n", "is '# ClawBridge v0.1.0', not \"# ClawBridge v0.1.0 (released DATE)\": rename it at the cut"),
        }
        for name, (notes, why) in cases.items():
            with self.subTest(name):
                self.reset()
                self.retag(notes)
                self.write_bundle()
                result = subprocess.run(["bash", str(ROOT / "scripts/publish-release.sh"), "v0.1.0", str(self.assets)], cwd=self.repo, env=dict(self.env, PUBLISH_MODE="ok"), capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("the top heading of RELEASE-NOTES.md " + why, result.stderr)
                self.assertIn("nothing was created", result.stderr)
                calls = [json.loads(line) for line in (self.root / "calls").read_text().splitlines()] if (self.root / "calls").exists() else []
                self.assertNotIn(["release", "create"], [c[:2] for c in calls])
                self.assertNotIn(["release", "edit"], [c[:2] for c in calls])

    def publish(self, mode="ok", ok=False):
        if self.fresh_bundle:
            self.write_bundle()
            self.fresh_bundle = True
        result = subprocess.run(["bash", str(ROOT / "scripts/publish-release.sh"), "v0.1.0", str(self.assets)], cwd=self.repo, env=dict(self.env, PUBLISH_MODE=mode), capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, ok, result.stdout + result.stderr)
        return [json.loads(line) for line in (self.root / "calls").read_text().splitlines()] if (self.root / "calls").exists() else []

    def test_complete_draft_then_verify_then_publish(self):
        # Mutable checkout text must not replace the notes at the exact tag.
        (self.repo / "RELEASE-NOTES.md").write_text("uncommitted notes must not publish\n")
        calls = self.publish(ok=True)
        self.assertEqual([c[:2] for c in calls], [["attestation", "verify"], ["api", "--paginate"], ["release", "create"], ["release", "upload"], ["release", "download"], ["release", "view"], ["api", "--paginate"], ["release", "edit"]])
        self.assertEqual((self.root / "published").read_text(), "true")
        # The dry run's asset list: exactly the release's assets, the bundle
        # byte for byte, and no manual-update marker for ordinary notes.
        self.assertEqual(self.remote(), PUBLISHED)
        self.assertEqual((self.root / "remote" / "PROVENANCE.sigstore.json").read_bytes(), self.bundle.read_bytes())
        self.assertLess(len((self.root / "notes").read_text()), release_body.MAX_BODY)

    def test_no_bundle_never_contacts_gh(self):
        self.env.pop("PROVENANCE_BUNDLE")
        result = subprocess.run(["bash", str(ROOT / "scripts/publish-release.sh"), "v0.1.0", str(self.assets)], cwd=self.repo, env=self.env, capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("requires the assets attestation bundle", result.stderr)
        self.assertFalse((self.root / "calls").exists())

    def test_a_bundle_that_does_not_attest_the_assets_never_creates(self):
        changes = {
            "wrong digest": lambda m: m.update({BINARIES[0]: "00" * 32}),
            "missing asset": lambda m: m.pop("install.sh"),
            "extra subject": lambda m: m.update({"other": "11" * 32}),
            "no SHA256SUMS": lambda m: m.pop("SHA256SUMS"),
        }
        for name, change in changes.items():
            with self.subTest(name):
                self.reset()
                self.write_bundle(change)
                self.assertEqual(self.publish(), [])
                self.assertFalse((self.root / "draft").exists())
        envelopes = {
            "SLSA v0.2 predicate": dict(predicate="https://slsa.dev/provenance/v0.2"),
            "SBOM predicate": dict(predicate="https://spdx.dev/Document/v2.3"),
            "not in-toto": dict(payload_type="application/json"),
            "unsigned": dict(signatures=()),
        }
        for name, fields in envelopes.items():
            with self.subTest(name):
                self.reset()
                self.write_bundle(**fields)
                self.assertEqual(self.publish(), [])
        for bad in ['{"mediaType": "text/plain"}', "not json"]:
            with self.subTest(bad):
                self.reset()
                self.bundle.write_text(bad)
                self.fresh_bundle = False
                self.assertEqual(self.publish(), [])

    def test_a_manual_update_marker_publishes_its_asset(self):
        self.retag(TOP + "\n\n**Manual update required.** Google sign-in is gone; sign in again with GitHub.\n\n- a bullet\n\n# ClawBridge v0.0.1\n")
        self.publish(ok=True)
        self.assertEqual(self.remote(), PUBLISHED | {"MANUAL-UPDATE-REQUIRED"})
        text = (self.root / "remote" / "MANUAL-UPDATE-REQUIRED").read_text()
        self.assertTrue(text.startswith("v0.1.0 needs a manual update"), text)
        self.assertIn("Google sign-in is gone", text)

    def test_the_marker_counts_only_at_a_line_start_in_this_release(self):
        cases = {
            "an older release": TOP + "\n\n- a\n\n# ClawBridge v0.0.1\n\n**Manual update required.** old\n",
            "a code fence": TOP + "\n\n```md\n**Manual update required.** an example\n```\n",
            "mid-line": TOP + "\n\n- auto-updates skip a release marked `**Manual update required.**`\n",
        }
        for name, notes in cases.items():
            with self.subTest(name):
                self.retag(notes)
                self.reset()
                self.publish(ok=True)
                self.assertEqual(self.remote(), PUBLISHED)

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

    def test_a_bundle_that_does_not_verify_never_creates(self):
        calls = self.publish("attestation-failure")
        self.assertEqual([c[:2] for c in calls], [["attestation", "verify"]])
        self.assertFalse((self.root / "draft").exists())
        self.assertFalse((self.root / "remote").exists())
        self.assertFalse((self.root / "published").exists())

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

    def test_missing_or_malformed_image_digest_never_contacts_gh(self):
        for digest in [None, "", "sha256:" + "ab" * 31, "sha512:" + "ab" * 32, DIGEST + "\n"]:
            with self.subTest(digest=digest):
                if digest is None:
                    self.env.pop("IMAGE_DIGEST", None)
                else:
                    self.env["IMAGE_DIGEST"] = digest
                result = subprocess.run(["bash", str(ROOT / "scripts/publish-release.sh"), "v0.1.0", str(self.assets)], cwd=self.repo, env=self.env, capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("requires the pushed image digest", result.stderr)
                self.assertFalse((self.root / "calls").exists())

    def test_image_asset_must_name_the_pushed_digest(self):
        self.env["IMAGE_DIGEST"] = "sha256:" + "cd" * 32
        self.assertEqual(self.publish(), [])

    def test_missing_or_malformed_agent_image_digests_never_contact_gh(self):
        for env in IMAGE_ASSETS:
            for digest in [None, "", "sha256:" + "12" * 31, "sha512:" + "12" * 32, "sha256:" + "AB" * 32, AGENTD_DIGEST + "\n"]:
                with self.subTest(env=env, digest=digest):
                    environment = dict(self.env)
                    if digest is None:
                        environment.pop(env)
                    else:
                        environment[env] = digest
                    result = subprocess.run(["bash", str(ROOT / "scripts/publish-release.sh"), "v0.1.0", str(self.assets)], cwd=self.repo, env=environment, capture_output=True, text=True)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("requires the pushed", result.stderr)
                    self.assertIn("(" + env + ")", result.stderr)
                    self.assertFalse((self.root / "calls").exists())

    def test_agent_image_assets_must_name_the_pushed_digests(self):
        for env, (name, line) in IMAGE_ASSETS.items():
            # The workflow pushed another digest than the asset names.
            with self.subTest(env=env, case="other pushed digest"):
                self.reset()
                self.assertEqual(self.publish_with({env: "sha256:" + "cd" * 32}), [])
            # The asset names another image or digest, or the line is
            # malformed, with SHA256SUMS and the bundle matching it.
            for content in [line.replace("@sha256:", "@sha256:0"), IMAGE + "\n", line, line + "\n\n", line.replace("ghcr.io/", "docker.io/") + "\n"]:
                with self.subTest(env=env, content=content):
                    self.reset()
                    original = (self.assets / name).read_text()
                    (self.assets / name).write_text(content)
                    self.rehash()
                    try:
                        self.assertEqual(self.publish(), [])
                        self.assertFalse((self.root / "draft").exists())
                    finally:
                        (self.assets / name).write_text(original)
                        self.rehash()
        # A swapped pair: each asset holds the other's image.
        with self.subTest(case="swapped"):
            self.reset()
            self.assertEqual(self.publish_with({"AGENTD_IMAGE_DIGEST": AGENT_DIGEST, "AGENT_IMAGE_DIGEST": AGENTD_DIGEST}), [])
        # And the fixture itself still publishes.
        self.reset()
        self.publish(ok=True)
        for name, line in IMAGE_ASSETS.values():
            self.assertEqual((self.root / "remote" / name).read_text(), line + "\n")

    def test_a_missing_agent_image_asset_never_contacts_gh(self):
        for name, _ in IMAGE_ASSETS.values():
            with self.subTest(name=name):
                self.reset()
                original = (self.assets / name).read_text()
                (self.assets / name).unlink()
                self.rehash()
                try:
                    self.assertEqual(self.publish(), [])
                finally:
                    (self.assets / name).write_text(original)
                    self.rehash()

    def publish_with(self, env):
        saved = {k: self.env[k] for k in env}
        self.env.update(env)
        try:
            return self.publish()
        finally:
            self.env.update(saved)

    def rehash(self):
        """Rewrite SHA256SUMS for the assets as they are now."""
        (self.assets / "SHA256SUMS").write_text("".join(hashlib.sha256(p.read_bytes()).hexdigest() + "  " + p.name + "\n" for p in sorted(self.assets.iterdir()) if p.name != "SHA256SUMS"))

    def test_dispatch_and_bad_local_payload_never_contact_gh(self):
        self.env["GITHUB_EVENT_NAME"] = "workflow_dispatch"
        self.assertEqual(self.publish(), [])
        self.env["GITHUB_EVENT_NAME"] = "push"
        (self.assets / "unexpected").write_text("extra")
        self.assertEqual(self.publish(), [])

    # The hold (RELEASE_HOLD_DRAFT) and publish-draft, for v1.0's soak: the
    # tag-push run stops at the verified draft, and a later dispatch publishes
    # that same draft.

    def hold(self):
        """A tag-push run with the hold: the fake's remote then holds the
        draft as the release workflow left it, and its calls are forgotten."""
        self.env["RELEASE_HOLD_DRAFT"] = "true"
        self.publish(ok=True)
        del self.env["RELEASE_HOLD_DRAFT"]
        (self.root / "calls").unlink()

    def publish_draft(self, ok=False, version="v0.1.0", **env):
        """The publish-draft workflow's run: a dispatch from the tag, with no
        assets, digest or bundle of its own."""
        run_env = dict(self.env, GITHUB_EVENT_NAME="workflow_dispatch", PUBLISH_MODE="ok")
        for name in ["IMAGE_DIGEST", "PROVENANCE_BUNDLE", "FIXTURE_IMAGE"]:
            run_env.pop(name, None)
        run_env.update(env)
        result = subprocess.run(["bash", str(ROOT / "scripts/publish-release.sh"), "--publish-draft", version], cwd=self.repo, env=run_env, capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, ok, result.stdout + result.stderr)
        calls = [json.loads(line) for line in (self.root / "calls").read_text().splitlines()] if (self.root / "calls").exists() else []
        return calls, result

    def release_state(self, **change):
        state = json.loads((self.root / "release.json").read_text())
        state.update(change)
        (self.root / "release.json").write_text(json.dumps(state))

    def test_the_tag_run_verifies_the_bundle_with_publish_drafts_identity(self):
        # The same identity publish-draft demands: release.yml at this tag and
        # commit, SLSA v1, a GitHub-hosted runner.
        calls = self.publish(ok=True)
        commit = subprocess.check_output(["git", "rev-parse", "v0.1.0^{commit}"], cwd=self.repo, text=True).strip()
        verify = calls[0]
        self.assertEqual(verify[:2], ["attestation", "verify"])
        self.assertTrue(verify[2].endswith("/upload/clawbridge_v0.1.0_linux_amd64"), verify[2])
        self.assertEqual(verify[3], "--bundle")
        self.assertTrue(verify[4].endswith("/upload/PROVENANCE.sigstore.json"), verify[4])
        self.assertEqual(verify[5:], ["--repo", "jkai-chen/ClawBridge", "--signer-workflow", "jkai-chen/ClawBridge/.github/workflows/release.yml", "--source-ref", "refs/tags/v0.1.0", "--source-digest", commit, "--predicate-type", "https://slsa.dev/provenance/v1", "--deny-self-hosted-runners"])

    def test_the_hold_stops_at_the_verified_draft(self):
        self.write_bundle()
        self.env["RELEASE_HOLD_DRAFT"] = "true"
        result = subprocess.run(["bash", str(ROOT / "scripts/publish-release.sh"), "v0.1.0", str(self.assets)], cwd=self.repo, env=dict(self.env, PUBLISH_MODE="ok"), capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout.strip().splitlines()[-1], "held as draft: v0.1.0; image " + IMAGE + "; publish with the publish-draft workflow")
        calls = [json.loads(line) for line in (self.root / "calls").read_text().splitlines()]
        self.assertEqual([c[:2] for c in calls], [["attestation", "verify"], ["api", "--paginate"], ["release", "create"], ["release", "upload"], ["release", "download"], ["release", "view"]])
        self.assertEqual(self.remote(), PUBLISHED)
        self.assertTrue((self.root / "draft").exists())
        self.assertFalse((self.root / "published").exists())
        self.assertTrue(json.loads((self.root / "release.json").read_text())["isDraft"])

    def test_no_hold_publishes_as_before(self):
        for value in ["false", ""]:
            with self.subTest(value=value):
                self.reset()
                self.env["RELEASE_HOLD_DRAFT"] = value
                calls = self.publish(ok=True)
                self.assertEqual([c[:2] for c in calls][-1], ["release", "edit"])
                self.assertEqual((self.root / "published").read_text(), "true")

    def test_a_bad_hold_value_never_contacts_gh(self):
        for value in ["TRUE", "True", "yes", "1", " true", "true\n", "flase"]:
            with self.subTest(value=value):
                self.env["RELEASE_HOLD_DRAFT"] = value
                self.assertEqual(self.publish(), [])
                calls, result = self.publish_draft()
                self.assertEqual(calls, [])
                self.assertIn("RELEASE_HOLD_DRAFT must be true, false or empty", result.stderr)

    def test_publish_draft_publishes_the_held_draft(self):
        self.hold()
        held = {name: (self.root / "remote" / name).read_bytes() for name in self.remote()}
        calls, result = self.publish_draft(ok=True)
        self.assertEqual([c[:2] for c in calls], [["release", "view"], ["release", "download"], ["attestation", "verify"], ["api", "--paginate"], ["release", "edit"]])
        self.assertEqual((self.root / "published").read_text(), "true")
        self.assertIn("published v0.1.0; image " + IMAGE, result.stdout)
        # The body published is the one the tag-push run wrote, set again.
        self.assertEqual((self.root / "published-notes").read_text(), (self.root / "notes").read_text())
        # The same bytes: nothing was created, uploaded or replaced.
        self.assertEqual({name: (self.root / "remote" / name).read_bytes() for name in self.remote()}, held)

    def test_publish_draft_verifies_the_provenance_signature(self):
        self.hold()
        calls, _ = self.publish_draft(ok=True)
        commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=self.repo, text=True).strip()
        verify = [c for c in calls if c[:2] == ["attestation", "verify"]]
        self.assertEqual(len(verify), 1)
        args = verify[0]
        self.assertEqual(Path(args[2]).name, "SHA256SUMS")
        self.assertEqual(Path(args[args.index("--bundle") + 1]).name, "PROVENANCE.sigstore.json")
        self.assertEqual(Path(args[2]).parent, Path(args[args.index("--bundle") + 1]).parent)
        self.assertEqual(args[3:], ["--bundle", args[4], "--repo", "jkai-chen/ClawBridge", "--signer-workflow", "jkai-chen/ClawBridge/.github/workflows/release.yml", "--source-ref", "refs/tags/v0.1.0", "--source-digest", commit, "--predicate-type", "https://slsa.dev/provenance/v1", "--deny-self-hosted-runners"])

    def test_publish_draft_refuses_a_bundle_that_does_not_verify(self):
        # A forged bundle: the subjects match a swapped binary, SHA256SUMS is
        # redone, but the signature is not release.yml's.
        self.hold()
        (self.root / "remote" / BINARIES[0]).write_text("swapped binary")
        self.resign_remote()
        bundle = json.loads((self.root / "remote" / "PROVENANCE.sigstore.json").read_text())
        bundle["dsseEnvelope"]["signatures"] = [{"sig": "Zm9yZ2Vk"}]
        (self.root / "remote" / "PROVENANCE.sigstore.json").write_text(json.dumps(bundle))
        calls, result = self.publish_draft()
        self.assertEqual([c[:2] for c in calls], [["release", "view"], ["release", "download"], ["attestation", "verify"]])
        self.assertIn("provenance does not verify", result.stderr)
        self.assertFalse((self.root / "published").exists())
        # A failing verification of an untouched draft refuses as well.
        self.reset()
        self.hold()
        calls, _ = self.publish_draft(PUBLISH_MODE="attest-failure")
        self.assertNotIn(["release", "edit"], [c[:2] for c in calls])
        self.assertFalse((self.root / "published").exists())

    def test_only_publish_draft_sets_the_body_on_publication(self):
        calls = self.publish(ok=True)
        self.assertNotIn("--notes-file", [c for c in calls if c[:2] == ["release", "edit"]][0])
        self.assertFalse((self.root / "published-notes").exists())
        self.reset()
        self.retag(TOP + "\n\n- the held release\n\n# ClawBridge v0.0.1\n")
        self.hold()
        self.publish_draft(ok=True)
        notes = (self.root / "published-notes").read_text()
        # The body is set again from the draft's own digest assets, all three images.
        self.assertEqual(notes, release_body.release_body(TOP + "\n\n- the held release\n\n# ClawBridge v0.0.1\n", "v0.1.0", IMAGE, "ghcr.io/jkai-chen/clawbridge-agentd@" + AGENTD_DIGEST, "ghcr.io/jkai-chen/clawbridge-dispatcher-agent@" + AGENT_DIGEST))

    def test_publish_draft_decides_latest_like_a_tag_push(self):
        self.hold()
        self.env["PUBLISH_RELEASES"] = json.dumps([{"tag_name": "v0.10.0", "draft": False, "prerelease": False}])
        self.publish_draft(ok=True)
        self.assertEqual((self.root / "published").read_text(), "false")

    def test_publish_draft_refuses_anything_but_a_draft_of_the_tag(self):
        cases = {
            "published": dict(isDraft=False),
            "another tag": dict(tagName="v0.1.1"),
            "another commit": dict(targetCommitish="0" * 40),
        }
        for name, change in cases.items():
            with self.subTest(name):
                self.reset()
                self.hold()
                self.release_state(**change)
                calls, _ = self.publish_draft()
                self.assertEqual([c[:2] for c in calls], [["release", "view"]])
                self.assertFalse((self.root / "published").exists())
        with self.subTest("no release"):
            self.reset()
            calls, result = self.publish_draft()
            self.assertEqual([c[:2] for c in calls], [["release", "view"]])
            self.assertIn("no release v0.1.0 to publish", result.stderr)

    def test_publish_draft_accepts_a_draft_targeting_the_tagged_commit(self):
        self.hold()
        commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=self.repo, text=True).strip()
        self.release_state(targetCommitish=commit)
        self.publish_draft(ok=True)

    def test_publish_draft_once_published_refuses_again(self):
        self.hold()
        self.publish_draft(ok=True)
        (self.root / "calls").unlink()
        calls, result = self.publish_draft()
        self.assertEqual([c[:2] for c in calls], [["release", "view"]])
        self.assertIn("is not a draft", result.stderr)

    def test_publish_draft_validates_the_remote_assets(self):
        cases = {
            "tampered binary": lambda r: (r / BINARIES[2]).write_text("tampered"),
            "missing provenance": lambda r: (r / "PROVENANCE.sigstore.json").unlink(),
            "missing asset": lambda r: (r / "install.ps1").unlink(),
            "extra asset": lambda r: (r / "extra").write_text("unexpected"),
            "unexpected manual marker": lambda r: (r / "MANUAL-UPDATE-REQUIRED").write_text("v0.1.0 needs a manual update\n"),
        }
        for name, change in cases.items():
            with self.subTest(name):
                self.reset()
                self.hold()
                change(self.root / "remote")
                calls, _ = self.publish_draft()
                self.assertEqual([c[:2] for c in calls], [["release", "view"], ["release", "download"]])
                self.assertFalse((self.root / "published").exists())

    def resign_remote(self):
        """Make the fake's remote self-consistent again after an edit:
        SHA256SUMS and the bundle cover its files as they are now, so only the
        check under test can refuse it."""
        remote = self.root / "remote"
        sums = "".join(hashlib.sha256(p.read_bytes()).hexdigest() + "  " + p.name + "\n" for p in sorted(remote.iterdir()) if p.name not in {"SHA256SUMS", "PROVENANCE.sigstore.json", "MANUAL-UPDATE-REQUIRED"})
        (remote / "SHA256SUMS").write_text(sums)
        subjects = {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in remote.iterdir() if p.name not in {"PROVENANCE.sigstore.json", "MANUAL-UPDATE-REQUIRED"}}
        bundle = json.loads((remote / "PROVENANCE.sigstore.json").read_text())
        statement = json.loads(base64.b64decode(bundle["dsseEnvelope"]["payload"]))
        statement["subject"] = [{"name": n, "digest": {"sha256": d}} for n, d in sorted(subjects.items())]
        bundle["dsseEnvelope"]["payload"] = base64.b64encode(json.dumps(statement).encode()).decode()
        (remote / "PROVENANCE.sigstore.json").write_text(json.dumps(bundle))

    def test_publish_draft_requires_a_strict_image_digest(self):
        # Each draft is otherwise consistent, so the format check alone refuses.
        for text in ["\n", IMAGE.upper() + "\n", IMAGE.replace("jkai-chen", "other") + "\n", IMAGE[:-2] + "\n", IMAGE + "0\n", "docker.io/jkai-chen/clawbridge@" + DIGEST + "\n", " " + IMAGE + "\n", IMAGE.replace("sha256:", "sha512:") + "\n"]:
            with self.subTest(text=text):
                self.reset()
                self.hold()
                (self.root / "remote" / "IMAGE-DIGEST.txt").write_text(text)
                self.resign_remote()
                calls, result = self.publish_draft()
                self.assertEqual([c[:2] for c in calls], [["release", "view"], ["release", "download"]])
                self.assertIn("does not name a ClawBridge image digest", result.stderr)
                self.assertFalse((self.root / "published").exists())
        # A trailing byte after the line is refused by the exact-text check.
        for text in [IMAGE, IMAGE + "\n\n"]:
            with self.subTest(text=text):
                self.reset()
                self.hold()
                (self.root / "remote" / "IMAGE-DIGEST.txt").write_text(text)
                self.resign_remote()
                calls, _ = self.publish_draft()
                self.assertNotIn(["release", "edit"], [c[:2] for c in calls])
        with self.subTest("missing"):
            self.reset()
            self.hold()
            (self.root / "remote" / "IMAGE-DIGEST.txt").unlink()
            calls, result = self.publish_draft()
            self.assertIn("no IMAGE-DIGEST.txt", result.stderr)
            self.assertNotIn(["release", "edit"], [c[:2] for c in calls])
        # The control: the same resigning of an untouched draft publishes.
        self.reset()
        self.hold()
        self.resign_remote()
        self.publish_draft(ok=True)

    def test_publish_draft_requires_strict_agent_image_digests(self):
        # The two agent digest assets are read from the draft and checked like
        # the server image's: a swapped, foreign or missing one never publishes.
        agentd = "ghcr.io/jkai-chen/clawbridge-agentd@" + AGENTD_DIGEST
        agent = "ghcr.io/jkai-chen/clawbridge-dispatcher-agent@" + AGENT_DIGEST
        for name, text, says in [
            ("AGENTD-IMAGE-DIGEST.txt", agent + "\n", "does not name an agentd image digest"),
            ("AGENTD-IMAGE-DIGEST.txt", agentd.replace("jkai-chen", "other") + "\n", "does not name an agentd image digest"),
            ("AGENT-IMAGE-DIGEST.txt", agentd + "\n", "does not name a dispatcher agent image digest"),
            ("AGENT-IMAGE-DIGEST.txt", agent.replace("sha256:", "sha512:") + "\n", "does not name a dispatcher agent image digest"),
        ]:
            with self.subTest(name=name, text=text):
                self.reset()
                self.hold()
                (self.root / "remote" / name).write_text(text)
                self.resign_remote()
                calls, result = self.publish_draft()
                self.assertIn(says, result.stderr)
                self.assertNotIn(["release", "edit"], [c[:2] for c in calls])
        for name in ["AGENTD-IMAGE-DIGEST.txt", "AGENT-IMAGE-DIGEST.txt"]:
            with self.subTest(missing=name):
                self.reset()
                self.hold()
                (self.root / "remote" / name).unlink()
                calls, result = self.publish_draft()
                self.assertIn("no AGENTD-IMAGE-DIGEST.txt or AGENT-IMAGE-DIGEST.txt", result.stderr)
                self.assertNotIn(["release", "edit"], [c[:2] for c in calls])

    def test_publish_draft_checks_the_image_the_draft_names(self):
        # A consistent draft (SHA256SUMS and bundle redone) for another image
        # still publishes only that image: the body names the draft's digest,
        # never one from this run. The message reports it.
        self.env["IMAGE_DIGEST"] = "sha256:" + "cd" * 32
        self.env["FIXTURE_IMAGE"] = "ghcr.io/jkai-chen/clawbridge@sha256:" + "cd" * 32
        (self.assets / "IMAGE-DIGEST.txt").write_text(self.env["FIXTURE_IMAGE"] + "\n")
        (self.assets / "SHA256SUMS").write_text("".join(hashlib.sha256(p.read_bytes()).hexdigest() + "  " + p.name + "\n" for p in sorted(self.assets.iterdir()) if p.name != "SHA256SUMS"))
        self.hold()
        _, result = self.publish_draft(ok=True)
        self.assertIn("image " + self.env["FIXTURE_IMAGE"], result.stdout)

    def test_the_manual_marker_is_required_in_the_draft_iff_the_notes_carry_it(self):
        self.retag(TOP + "\n\n**Manual update required.** Google sign-in is gone; sign in again with GitHub.\n\n- a bullet\n\n# ClawBridge v0.0.1\n")
        self.hold()
        self.assertIn("MANUAL-UPDATE-REQUIRED", self.remote())
        self.publish_draft(ok=True)
        for name, change in {
            "missing": lambda m: m.unlink(),
            "changed": lambda m: m.write_text("v0.1.0 needs a manual update (clawbridge self-update); see its release notes.\nsomething else\n"),
        }.items():
            with self.subTest(name):
                self.reset()
                self.hold()
                change(self.root / "remote" / "MANUAL-UPDATE-REQUIRED")
                calls, _ = self.publish_draft()
                self.assertNotIn(["release", "edit"], [c[:2] for c in calls])
                self.assertFalse((self.root / "published").exists())

    def test_publish_draft_outside_its_dispatch_never_contacts_gh(self):
        self.hold()
        # At the tag, so only the context refuses each of these.
        cases = {
            "tag push": dict(GITHUB_EVENT_NAME="push"),
            "a schedule": dict(GITHUB_EVENT_NAME="schedule"),
            "dispatched from main": dict(GITHUB_REF="refs/heads/main"),
            "another repository": dict(GITHUB_REPOSITORY="someone/ClawBridge"),
            "not in Actions": dict(GITHUB_ACTIONS="false"),
        }
        for name, env in cases.items():
            with self.subTest(name):
                calls, result = self.publish_draft(**env)
                self.assertEqual(calls, [])
                self.assertIn("dispatch-from-tag CI context", result.stderr)
        with self.subTest("HEAD past the tag"):
            subprocess.run(["git", "commit", "-q", "--allow-empty", "-m", "after the tag"], cwd=self.repo, env=self.env, check=True, capture_output=True)
            calls, result = self.publish_draft()
            self.assertEqual(calls, [])
            self.assertIn("not the tagged commit", result.stderr)
        subprocess.run(["git", "checkout", "-q", "v0.1.0"], cwd=self.repo, env=self.env, check=True, capture_output=True)
        for version in ["v0.1", "0.1.0", "v0.1.0-rc1", "v01.1.0", "v0.1.0\n"]:
            with self.subTest(version=version):
                calls, _ = self.publish_draft(version=version, GITHUB_REF="refs/tags/" + version)
                self.assertEqual(calls, [])
        with self.subTest("missing argument"):
            result = subprocess.run(["bash", str(ROOT / "scripts/publish-release.sh"), "--publish-draft"], cwd=self.repo, env=dict(self.env, GITHUB_EVENT_NAME="workflow_dispatch"), capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse((self.root / "calls").exists())
        # Back at the tag, the same dispatch publishes.
        self.publish_draft(ok=True)


class ReleaseBodyTests(unittest.TestCase):
    def test_top_section_stops_at_the_next_release_outside_a_fence(self):
        notes = "# Unreleased\n\n```sh\n# a shell comment, not a heading\n```\n- a\n\n# ClawBridge v0.1.0\n- old\n"
        self.assertEqual(release_body.top_section(notes), "# Unreleased\n\n```sh\n# a shell comment, not a heading\n```\n- a\n\n")

    def test_the_body_has_the_link_and_the_image(self):
        body = release_body.release_body("# Unreleased\n- a\n# ClawBridge v0.1.0\n", "v9.9.9", IMAGE)
        self.assertIn("https://github.com/jkai-chen/ClawBridge/blob/v9.9.9/RELEASE-NOTES.md", body)
        self.assertIn("## Server image\n\n`" + IMAGE + "`", body)
        self.assertNotIn("v0.1.0", body)

    def test_the_body_names_the_agent_images_when_given(self):
        agentd = "ghcr.io/jkai-chen/clawbridge-agentd@" + AGENTD_DIGEST
        agent = "ghcr.io/jkai-chen/clawbridge-dispatcher-agent@" + AGENT_DIGEST
        body = release_body.release_body("# Unreleased\n- a\n# ClawBridge v0.1.0\n", "v9.9.9", IMAGE, agentd, agent)
        self.assertIn("\n## Agent images\n", body)
        for ref in (agentd, agent):
            self.assertIn("`" + ref + "`", body)
            self.assertIn("gh attestation verify oci://" + ref + " --repo jkai-chen/ClawBridge", body)
        self.assertLess(body.index("## Server image"), body.index("## Agent images"))
        self.assertNotIn("## Agent images", release_body.release_body("# Unreleased\n- a\n", "v9.9.9", IMAGE))

    def test_manual_line_reads_this_release_only(self):
        self.assertEqual(release_body.manual_line("# Unreleased\n**Manual update required.** why\n# ClawBridge v1\n"), "**Manual update required.** why")
        self.assertIsNone(release_body.manual_line("# Unreleased\n- a\n# ClawBridge v1\n**Manual update required.** old\n"))

    def test_heading_error_names_the_release_and_nothing_else(self):
        self.assertIsNone(release_body.heading_error("# ClawBridge v1.0.0 (released 2 Nov)\n- a\n", "v1.0.0"))
        self.assertIsNone(release_body.heading_error("# ClawBridge v1.0.0 (released 2 Nov)\r\n- a\n", "v1.0.0"))
        for notes in ["# Unreleased\n", "", "\n# ClawBridge v1.0.0 (released 2 Nov)\n", "# ClawBridge v1.0.0\n",
                      "# ClawBridge v1.0.0 (released 2 Nov) and more\n", "# ClawBridge v1.0.0.1 (released 2 Nov)\n",
                      "# ClawBridge v01.0.0 (released 2 Nov)\n", "## ClawBridge v1.0.0 (released 2 Nov)\n"]:
            with self.subTest(notes):
                self.assertIsNotNone(release_body.heading_error(notes, "v1.0.0"))
        self.assertIn("is for v0.5.0, not v1.0.0", release_body.heading_error("# ClawBridge v0.5.0 (released 1 Nov)\n", "v1.0.0"))

    def test_the_repositorys_notes_fit_a_release_body(self):
        # The guard that runs in every PR and main CI: today's RELEASE-NOTES.md
        # makes a body GitHub takes, long before any tag.
        notes = (ROOT / "RELEASE-NOTES.md").read_text(encoding="utf-8")
        release_body.check(release_body.release_body(notes, "v0.0.0", IMAGE))


if __name__ == "__main__":
    unittest.main()

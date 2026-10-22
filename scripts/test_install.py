#!/usr/bin/env python3
"""POSIX installer black-box fixtures; no network. The fixture payload only
records how the installer ran it."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

INSTALLER = Path(__file__).resolve().with_name("install.sh")
STUB = r'''#!PYTHON
import hashlib,json,os,pathlib,sys
name=pathlib.Path(sys.argv[0]).name
args=sys.argv[1:]
root=pathlib.Path(os.environ['FIXTURE'])
if name=='uname':
 print(os.environ.get('TEST_OS','Linux') if args==['-s'] else os.environ.get('TEST_ARCH','x86_64'))
 sys.exit(0)
if name in ('cp','chmod','mv'):
 if os.environ.get('FAIL_WRITE')==name: sys.exit(1)
 os.execv(os.environ['REAL_'+name.upper()], [name]+args)
with (root/'requests').open('a') as f: f.write(json.dumps(args)+'\n')
assert '--proto' in args and args[args.index('--proto')+1]=='=https'
assert '--proto-redir' in args and args[args.index('--proto-redir')+1]=='=https'
assert '--connect-timeout' in args and '--max-time' in args
url=args[-1]; base='https://github.com/jkai-chen/ClawBridge/releases'
mode=os.environ.get('TEST_MODE','ok')
if url==base+'/latest':
 print(base+'/tag/'+os.environ.get('LATEST','v0.1.0'),end='')
 sys.exit(0)
assert url.startswith(base+'/download/v0.1.0/'),url
asset='clawbridge_v0.1.0_'+os.environ.get('ASSET_OS','linux')+'_'+os.environ.get('ASSET_ARCH','amd64')
# Run only as the installed binary, for `skill install`: record that, then
# exit with SKILL_EXIT.
payload=b'#!/bin/sh\nprintf "%s %s\\n" "$0" "$*" >> "$FIXTURE/ran"\nexit "${SKILL_EXIT:-0}"\n'
digest=hashlib.sha256(payload).hexdigest()
entry=digest+'  '+asset+'\n'
output=pathlib.Path(args[args.index('--output')+1])
if url.endswith('/SHA256SUMS'):
 text=entry
 if mode=='duplicate': text+=entry
 if mode=='missing': text=digest+'  other-asset\n'
 if mode=='bad-hash': text='Z'*64+'  '+asset+'\n'
 if mode=='malformed': text=digest+' '+asset+'\n'
 if mode=='hostile': text+='$(touch hostile)\n'
 if mode=='manifest-failure': sys.exit(22)
 output.write_text(text)
else:
 assert url==base+'/download/v0.1.0/'+asset,url
 if mode=='binary-failure': sys.exit(22)
 output.write_bytes(b'' if mode=='empty' else b'wrong' if mode=='mismatch' else payload)
'''


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="clawbridge installer '")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.stubs = self.root / "stubs"
        self.stubs.mkdir()
        self.state = self.root / "home"
        self.state.mkdir()
        self.scratch = self.root / "temporary"
        self.scratch.mkdir()
        self.destination = self.root / "bin with ' spaces"
        self.env = os.environ.copy()
        self.env.update(FIXTURE=str(self.root), HOME=str(self.state), TMPDIR=str(self.scratch))
        for name in ("curl", "uname", "cp", "chmod", "mv"):
            self.env["REAL_" + name.upper()] = shutil.which(name) or ""
            stub = self.stubs / name
            stub.write_text(STUB.replace("#!PYTHON", "#!" + sys.executable))
            stub.chmod(0o755)
        self.env["PATH"] = str(self.stubs) + os.pathsep + self.env["PATH"]

    def run_installer(self, *args, ok=True, **env):
        result = subprocess.run(["/bin/sh", str(INSTALLER), *args], env=dict(self.env, **env), cwd=self.root, capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, ok, result.stdout + result.stderr)
        self.assertEqual(list(self.scratch.iterdir()), [], "download temp leaked")
        self.assertEqual(list(self.state.iterdir()), [], "configuration/profile was changed")
        self.assertFalse((self.root / "hostile").exists())
        if self.destination.exists():
            self.assertFalse(any(p.name.startswith(".clawbridge-install") for p in self.destination.iterdir()))
        return result

    def ran(self):
        p = self.root / "ran"
        return p.read_text().splitlines() if p.exists() else []

    def requests(self):
        p = self.root / "requests"
        return [json.loads(line)[-1] for line in p.read_text().splitlines()] if p.exists() else []

    def test_latest_once_pinned_and_path_instruction(self):
        result = self.run_installer("--bin-dir", str(self.destination))
        requests = self.requests()
        self.assertEqual(len(requests), 3)
        self.assertTrue(requests[0].endswith("/latest"))
        self.assertTrue(all("/download/v0.1.0/" in u for u in requests[1:]))
        self.assertTrue((self.destination / "clawbridge").stat().st_mode & 0o111)
        instruction = result.stdout.split("For this shell, run: ", 1)[1].strip()
        check = subprocess.run(["/bin/sh", "-c", instruction + '\nprintf "%s" "$PATH"'], env=self.env, capture_output=True, text=True, check=True)
        self.assertTrue(check.stdout.startswith(str(self.destination) + ":"))
        # Only the installed binary runs, once, to install the agent skill.
        self.assertEqual(self.ran(), [str(self.destination / "clawbridge") + " skill install"])
        self.assertIn("Installed the ClawBridge agent skill", result.stdout)

    def test_skill_step_is_optional_and_never_fails_the_install(self):
        self.run_installer("--no-skill", "--bin-dir", str(self.destination))
        self.assertEqual(self.ran(), [])
        self.assertTrue((self.destination / "clawbridge").exists())
        result = self.run_installer("--bin-dir", str(self.destination), SKILL_EXIT="1")
        self.assertEqual(len(self.ran()), 1)
        self.assertIn("warning: the agent skill was not installed", result.stderr)
        self.assertIn("Installed v0.1.0", result.stdout)

    def test_explicit_platform_matrix_replaces_existing(self):
        for system, architecture, asset_os, asset_arch in [("Darwin", "x86_64", "darwin", "amd64"), ("Darwin", "arm64", "darwin", "arm64"), ("Linux", "amd64", "linux", "amd64"), ("Linux", "aarch64", "linux", "arm64")]:
            with self.subTest(system=system, arch=architecture):
                self.destination.mkdir(exist_ok=True)
                target = self.destination / "clawbridge"
                target.write_bytes(b"previous")
                self.run_installer("--version", "v0.1.0", "--bin-dir", str(self.destination), TEST_OS=system, TEST_ARCH=architecture, ASSET_OS=asset_os, ASSET_ARCH=asset_arch)
                self.assertNotEqual(target.read_bytes(), b"previous")
        self.assertFalse(any(u.endswith("/latest") for u in self.requests()))

    def test_validation_before_download_or_destination_mutation(self):
        for args, env in [(["--help"], {}), (["--version", "v01.2.3"], {}), (["--version", "v1.2.3-beta"], {}), (["--version", "v1.2.3\nhostile"], {}), (["--bin-dir", "relative"], {}), (["--unknown"], {}), ([], {"TEST_OS": "Plan9"}), ([], {"TEST_ARCH": "riscv64"})]:
            with self.subTest(args=args, env=env):
                self.run_installer(*args, ok=args == ["--help"], **env)
                self.assertEqual(self.requests(), [])
                self.assertFalse(self.destination.exists())

    def test_failures_preserve_existing_binary(self):
        self.destination.mkdir()
        target = self.destination / "clawbridge"
        for mode in ["duplicate", "missing", "bad-hash", "malformed", "hostile", "manifest-failure", "binary-failure", "empty", "mismatch"]:
            with self.subTest(mode=mode):
                target.write_bytes(b"previous")
                self.run_installer("--version", "v0.1.0", "--bin-dir", str(self.destination), ok=False, TEST_MODE=mode)
                self.assertEqual(target.read_bytes(), b"previous")
                self.assertEqual(self.ran(), [], "a failed install ran a binary")
        for operation in ["cp", "chmod", "mv"]:
            with self.subTest(operation=operation):
                self.run_installer("--version", "v0.1.0", "--bin-dir", str(self.destination), ok=False, FAIL_WRITE=operation)
                self.assertEqual(target.read_bytes(), b"previous")
                self.assertEqual(self.ran(), [], "a failed install ran a binary")

    def test_symlink_and_directory_refused_before_network(self):
        self.destination.mkdir()
        victim = self.root / "victim"
        victim.write_bytes(b"preserved")
        target = self.destination / "clawbridge"
        target.symlink_to(victim)
        self.run_installer("--bin-dir", str(self.destination), ok=False)
        self.assertEqual(self.requests(), [])
        self.assertEqual(victim.read_bytes(), b"preserved")
        target.unlink()
        target.mkdir()
        self.run_installer("--bin-dir", str(self.destination), ok=False)
        self.assertEqual(self.requests(), [])

    def test_latest_invalid_never_downloads_payload(self):
        self.run_installer("--bin-dir", str(self.destination), ok=False, LATEST="v0.1.0;touch-hostile")
        self.assertEqual(len(self.requests()), 1)
        self.assertFalse(self.destination.exists())

    def test_slash_suffix_cannot_hide_symlink(self):
        actual = self.root / "actual"
        actual.mkdir()
        link = self.root / "link"
        link.symlink_to(actual, target_is_directory=True)
        for destination in [str(link), str(link) + "/"]:
            self.run_installer("--bin-dir", destination, ok=False)
            self.assertEqual(self.requests(), [])
            self.assertEqual(list(actual.iterdir()), [])


if __name__ == "__main__":
    unittest.main()

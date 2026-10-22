"""Independent installer subprocess probes. All downloads/platforms are fakes."""
import hashlib
import json
import os
from pathlib import Path
import shlex
import stat
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(sys.argv.pop(1)).resolve()
PYTHON = sys.executable

FAKE = r'''
import hashlib,json,os,sys
from pathlib import Path
name=Path(sys.argv[0]).name
args=sys.argv[1:]
if name=='uname':
 print(os.environ['PROBE_OS'] if '-s' in args else os.environ['PROBE_ARCH']);sys.exit(0)
if name in ('mv','chmod'):
 if os.environ.get('PROBE_FAIL_TOOL')==name:sys.exit(71)
 os.execv('/bin/'+name,[name]+args)
with open(os.environ['PROBE_LOG'],'a') as f:f.write(json.dumps(args)+'\n')
urls=[a for a in args if a.startswith('https://') or a.startswith('http://')]
if len(urls)!=1:sys.exit(72)
url=urls[0]
base='https://github.com/jkai-chen/ClawBridge/releases/'
if not url.startswith(base):sys.exit(73)
if url==base+'latest':
 print(os.environ.get('PROBE_LATEST',base+'tag/v1.2.3'),end='');sys.exit(0)
asset=url.rsplit('/',1)[-1]
if os.environ.get('PROBE_DOWNLOAD_FAIL')==asset:sys.exit(22)
payload=Path(os.environ['PROBE_PAYLOAD']).read_bytes()
content=Path(os.environ['PROBE_MANIFEST']).read_bytes() if asset=='SHA256SUMS' else payload
if asset!='SHA256SUMS' and os.environ.get('PROBE_EMPTY')=='1':content=b''
out=None
for i,a in enumerate(args):
 if a in ('-o','--output') and i+1<len(args):out=args[i+1]
 if a.startswith('--output='):out=a.split('=',1)[1]
if out is None:sys.stdout.buffer.write(content)
else:Path(out).write_bytes(content)
'''


class Installer(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="cb-release-probe-")
        self.root = Path(self.temp.name)
        self.home = self.root / "home"
        self.tmp = self.root / "tmp"
        self.bin = self.root / "bin dir with ' quote $(inert) ;"
        self.fake = self.root / "fakebin"
        for p in (self.home, self.tmp, self.fake):
            p.mkdir()
        self.log = self.root / "requests.jsonl"
        self.payload = self.root / "payload"
        # The payload logs every execution. After a verified install the
        # installer may run only the installed binary, once, as `skill install`.
        self.executed = self.root / "EXECUTIONS"
        self.payload.write_text("#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> " + shlex.quote(str(self.executed)) + "\nexit \"${PROBE_SKILL_EXIT:-0}\"\n")
        self.manifest = self.root / "manifest"
        self.env = os.environ.copy()
        for k in list(self.env):
            if k.startswith("CLAWBRIDGE_"):
                del self.env[k]
        self.env.update(HOME=str(self.home), TMPDIR=str(self.tmp), PATH=str(self.fake)+":/usr/bin:/bin", PROBE_OS="Linux", PROBE_ARCH="x86_64", PROBE_LOG=str(self.log), PROBE_PAYLOAD=str(self.payload), PROBE_MANIFEST=str(self.manifest))
        for name in ("curl", "uname", "mv", "chmod"):
            f = self.fake / name
            f.write_text("#!"+PYTHON+"\n"+FAKE)
            f.chmod(0o700)
        self.sentinel = self.home / ".profile"
        self.sentinel.write_text("preserve-profile\n")
        self.asset = "clawbridge_v1.2.3_linux_amd64"
        self.set_manifest()

    def tearDown(self):
        self.temp.cleanup()

    def set_manifest(self, text=None):
        digest = hashlib.sha256(self.payload.read_bytes()).hexdigest()
        self.manifest.write_text(text if text is not None else digest+"  "+self.asset+"\n")

    def call(self, *args, success=True, skill=True):
        before = {str(p.relative_to(self.home)): p.read_bytes() for p in self.home.rglob("*") if p.is_file() and p != self.bin/"clawbridge"}
        result = subprocess.run(["/bin/sh", str(SCRIPT), *args], env=self.env, cwd=self.root, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=20)
        self.assertEqual(result.returncode == 0, success, (result.returncode, result.stdout.decode(errors="replace"), result.stderr.decode(errors="replace")))
        runs = self.executed.read_text().splitlines() if self.executed.exists() else []
        self.executed.unlink(missing_ok=True)
        if success and skill and "--help" not in args and "-h" not in args:
            self.assertEqual(runs, [str(self.bin/"clawbridge")+" skill install"], "installer must run only the installed binary, once, as skill install")
        else:
            self.assertEqual(runs, [], "installer executed a payload without a verified install or with --no-skill")
        after = {str(p.relative_to(self.home)): p.read_bytes() for p in self.home.rglob("*") if p.is_file() and p != self.bin/"clawbridge"}
        self.assertEqual(before, after, "installer changed other HOME files")
        self.assertEqual(self.sentinel.read_text(), "preserve-profile\n")
        self.assertEqual(list(self.tmp.iterdir()), [], "installer leaked private temporary files")
        if self.bin.exists():
            self.assertEqual(sorted(p.name for p in self.bin.iterdir()), ["clawbridge"], "installer leaked destination staging")
        return result

    def urls(self):
        return [a for l in self.log.read_text().splitlines() for a in json.loads(l) if a.startswith("https://") or a.startswith("http://")] if self.log.exists() else []

    def existing(self):
        self.bin.mkdir()
        target = self.bin / "clawbridge"
        target.write_bytes(b"OLD-BINARY")
        return target

    def test_latest_once_pins_all_downloads_and_runs_only_skill_install(self):
        target = self.existing()
        result = self.call("--bin-dir", str(self.bin))
        instruction = next(line for line in result.stdout.decode().splitlines() if line.startswith("For this shell, run: ")).removeprefix("For this shell, run: ")
        self.assertEqual(shlex.split(instruction), ["export", "PATH="+str(self.bin)+":$PATH"])
        for line in self.log.read_text().splitlines():
            argv = json.loads(line)
            for flag, value in [("--proto", "=https"), ("--proto-redir", "=https")]:
                self.assertEqual(argv[argv.index(flag)+1], value)
            for flag in ("--connect-timeout", "--max-time"):
                self.assertGreater(int(argv[argv.index(flag)+1]), 0)
            self.assertIn("--fail", argv)
        self.assertEqual(target.read_bytes(), self.payload.read_bytes())
        self.assertTrue(target.stat().st_mode & stat.S_IXUSR)
        urls = self.urls()
        self.assertEqual(urls.count("https://github.com/jkai-chen/ClawBridge/releases/latest"), 1)
        self.assertEqual(set(urls[1:]), {"https://github.com/jkai-chen/ClawBridge/releases/download/v1.2.3/SHA256SUMS", "https://github.com/jkai-chen/ClawBridge/releases/download/v1.2.3/"+self.asset})

    def test_no_skill_never_executes(self):
        self.call("--version", "v1.2.3", "--bin-dir", str(self.bin), "--no-skill", skill=False)
        self.assertEqual((self.bin/"clawbridge").read_bytes(), self.payload.read_bytes())

    def test_skill_failure_only_warns(self):
        self.env["PROBE_SKILL_EXIT"] = "3"
        result = self.call("--version", "v1.2.3", "--bin-dir", str(self.bin))
        self.assertIn(b"warning: the agent skill was not installed", result.stderr)
        self.assertEqual((self.bin/"clawbridge").read_bytes(), self.payload.read_bytes())

    def test_default_destination_only_installs_binary(self):
        self.bin = self.home / ".local" / "bin"
        self.call("--version", "v1.2.3")
        self.assertEqual((self.bin/"clawbridge").read_bytes(), self.payload.read_bytes())

    def test_platform_mapping_and_explicit_version(self):
        for system, arch, target_os, target_arch in [("Linux", "x86_64", "linux", "amd64"), ("Linux", "amd64", "linux", "amd64"), ("Linux", "aarch64", "linux", "arm64"), ("Linux", "arm64", "linux", "arm64"), ("Darwin", "arm64", "darwin", "arm64"), ("Darwin", "x86_64", "darwin", "amd64"), ("Darwin", "amd64", "darwin", "amd64"), ("Darwin", "aarch64", "darwin", "arm64")]:
            with self.subTest(system=system, arch=arch):
                self.env.update(PROBE_OS=system, PROBE_ARCH=arch)
                self.asset = "clawbridge_v1.2.3_"+target_os+"_"+target_arch
                self.set_manifest()
                self.log.unlink(missing_ok=True)
                self.call("--version", "v1.2.3", "--bin-dir", str(self.bin))
                self.assertFalse(any(u.endswith("/latest") for u in self.urls()))
                self.assertTrue(any(u.endswith("/"+self.asset) for u in self.urls()))

    def test_invalid_arguments_platform_and_latest_preserve(self):
        target = self.existing()
        for args in [("--version", "v01.2.3"), ("--version", "v1.2.3-rc1"), ("--version", "https://evil.test"), ("--version", "v1.2.3\n"), ("--version", "\nv1.2.3"), ("--bin-dir", "relative"), ("--unknown",)]:
            with self.subTest(args=args):
                self.call(*args, success=False)
                self.assertEqual(self.urls(), [])
                self.assertEqual(target.read_bytes(), b"OLD-BINARY")
        self.env.update(PROBE_OS="Plan9")
        self.call("--bin-dir", str(self.bin), success=False)
        self.assertEqual(self.urls(), [])
        self.env.update(PROBE_OS="Linux", PROBE_LATEST="https://attacker.example/releases/tag/v1.2.3")
        self.call("--bin-dir", str(self.bin), success=False)
        self.assertEqual(target.read_bytes(), b"OLD-BINARY")

    def test_strict_manifest_failures_preserve(self):
        target = self.existing()
        digest = hashlib.sha256(self.payload.read_bytes()).hexdigest()
        valid = digest+"  "+self.asset+"\n"
        for manifest in ["", valid+valid, "z"*64+"  "+self.asset+"\n", digest.upper()+"  "+self.asset+"\n", digest+" "+self.asset+"\n", digest+"  not-"+self.asset+"\n", "0"*64+"  "+self.asset+"\n", digest+"  "+self.asset+" $(touch MUST-NOT-EXECUTE)\n"]:
            with self.subTest(manifest=manifest):
                self.set_manifest(manifest)
                self.call("--version", "v1.2.3", "--bin-dir", str(self.bin), success=False)
                self.assertEqual(target.read_bytes(), b"OLD-BINARY")

    def test_download_stage_and_replace_failures_preserve(self):
        target = self.existing()
        for env in [{"PROBE_DOWNLOAD_FAIL":"SHA256SUMS"}, {"PROBE_DOWNLOAD_FAIL":self.asset}, {"PROBE_EMPTY":"1"}, {"PROBE_FAIL_TOOL":"chmod"}, {"PROBE_FAIL_TOOL":"mv"}]:
            with self.subTest(env=env):
                self.env.update(env)
                self.call("--version", "v1.2.3", "--bin-dir", str(self.bin), success=False)
                self.assertEqual(target.read_bytes(), b"OLD-BINARY")
                for k in env:del self.env[k]

    def test_slash_suffixed_directory_symlink_is_refused(self):
        target = self.existing()
        link = self.root / "destination-link"
        link.symlink_to(self.bin, target_is_directory=True)
        for suffix in ("/", "//"):
            with self.subTest(suffix=suffix):
                self.call("--version", "v1.2.3", "--bin-dir", str(link)+suffix, success=False)
                self.assertEqual(target.read_bytes(), b"OLD-BINARY")
                self.assertEqual(self.urls(), [], "symlink destination reached network")

    def test_symlink_directory_and_help_no_mutation(self):
        self.call("--help")
        self.assertEqual(self.urls(), [])
        self.assertFalse(self.bin.exists())
        self.bin.mkdir()
        outside = self.root / "outside"
        outside.write_bytes(b"OUTSIDE")
        (self.bin/"clawbridge").symlink_to(outside)
        self.call("--version", "v1.2.3", "--bin-dir", str(self.bin), success=False)
        self.assertEqual(outside.read_bytes(), b"OUTSIDE")
        (self.bin/"clawbridge").unlink()
        (self.bin/"clawbridge").mkdir()
        self.call("--version", "v1.2.3", "--bin-dir", str(self.bin), success=False)


if __name__ == "__main__":
    unittest.main(verbosity=2)

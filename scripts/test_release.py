#!/usr/bin/env python3
"""Actual five-target builds in a disposable tagged repository; never tags this repo."""
import hashlib
import json
import os
import platform
from pathlib import Path
import shutil
import struct
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent


class ReleaseTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory(prefix="clawbridge-release-test-")
        cls.base = Path(cls.temporary.name)
        cls.repo = cls.base / "source"
        cls.repo.mkdir()
        for name in ["LICENSE", "scripts/build-release.sh", "scripts/install.sh", "scripts/install.ps1", "internal/buildinfo/buildinfo.go"]:
            target = cls.repo / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / name, target)
        (cls.repo / "go.mod").write_text("module clawbridge\n\ngo 1.26.10\n")
        main = cls.repo / "cmd/clawbridge/main.go"
        main.parent.mkdir(parents=True)
        main.write_text('package main\nimport("encoding/json";"os";"clawbridge/internal/buildinfo")\nfunc main(){json.NewEncoder(os.Stdout).Encode(buildinfo.Current())}\n')
        (cls.repo / ".gitignore").write_text(".release-output/\n")
        cls.env = os.environ.copy()
        cls.env.update(GIT_AUTHOR_NAME="Fixture", GIT_AUTHOR_EMAIL="fixture@example.test", GIT_COMMITTER_NAME="Fixture", GIT_COMMITTER_EMAIL="fixture@example.test", GIT_AUTHOR_DATE="2026-10-21T12:00:00Z", GIT_COMMITTER_DATE="2026-10-21T12:00:00Z", GOPROXY="off", GOSUMDB="off", GOOS="plan9", GOARCH="invalid", GOFLAGS="-this-must-not-be-used", GOWORK="off")
        goenv = cls.base / "external-goenv"
        goenv.write_text("GOFLAGS=-modfile=" + str(cls.base / "outside-source.mod") + "\n")
        cls.env["GOENV"] = str(goenv)
        cls.git("init", "-q")
        cls.git("add", ".")
        cls.git("commit", "-qm", "isolated release fixture")
        cls.git("tag", "v0.1.0")
        cls.commit = cls.git("rev-parse", "HEAD").strip()

    @classmethod
    def tearDownClass(cls):
        cls.temporary.cleanup()

    @classmethod
    def git(cls, *args):
        return subprocess.check_output(["git", *args], cwd=cls.repo, env=cls.env, text=True)

    def build(self, version, destination, ok):
        result = subprocess.run(["bash", "scripts/build-release.sh", version, str(destination)], cwd=self.repo, env=self.env, capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, ok, result.stdout + result.stderr)
        return result

    def test_build_exact_assets_metadata_and_deterministic_manifest(self):
        destination = self.base / "release output"
        self.build("v0.1.0", destination, True)
        targets = ["darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64", "windows_amd64.exe", "windows_arm64.exe"]
        names = {"clawbridge_v0.1.0_" + t for t in targets} | {"LICENSE", "install.sh", "install.ps1", "SOURCE-COMMIT.txt", "SHA256SUMS"}
        self.assertEqual({p.name for p in destination.iterdir()}, names)
        self.assertEqual((destination / "SOURCE-COMMIT.txt").read_text(), self.commit + "\n")
        manifest = (destination / "SHA256SUMS").read_text().splitlines()
        self.assertEqual(manifest, sorted(manifest, key=lambda line: line[66:]))
        self.assertEqual({line[66:] for line in manifest}, names - {"SHA256SUMS"})
        for line in manifest:
            digest, name = line.split("  ")
            self.assertEqual(digest, hashlib.sha256((destination / name).read_bytes()).hexdigest())
        for target in targets:
            binary = destination / ("clawbridge_v0.1.0_" + target)
            env = dict(self.env, GOENV="off", GOFLAGS="", GOOS="", GOARCH="")
            metadata = subprocess.check_output(["go", "version", "-m", str(binary)], env=env, text=True)
            system, architecture = target.removesuffix(".exe").split("_")
            for expected in ["CGO_ENABLED=0", "GOOS=" + system, "GOARCH=" + architecture, "-trimpath=true", "vcs.revision=" + self.commit, "vcs.modified=false"]:
                self.assertIn(expected, metadata)
            data = binary.read_bytes()
            for value in ["v0.1.0", self.commit, "2026-10-21T12:00:00Z"]:
                self.assertIn(value.encode(), data)
            native_os = platform.system().lower()
            native_arch = {"x86_64": "amd64", "arm64": "arm64", "aarch64": "arm64"}.get(platform.machine().lower())
            if (system, architecture) == (native_os, native_arch):
                info = json.loads(subprocess.check_output([str(binary)], env=env, text=True))
                self.assertEqual(info, {"version": "v0.1.0", "commit": self.commit, "build_time": "2026-10-21T12:00:00Z"})
            if system == "linux":
                self.assertEqual(data[:6], b"\x7fELF\x02\x01")
                self.assertEqual(struct.unpack("<H", data[18:20])[0], 62 if architecture == "amd64" else 183)
            elif system == "darwin":
                self.assertEqual(struct.unpack("<I", data[4:8])[0], 0x1000007 if architecture == "amd64" else 0x100000C)
            else:
                self.assertEqual(data[:2], b"MZ")
                offset = struct.unpack("<I", data[60:64])[0]
                self.assertEqual(data[offset:offset + 6], b"PE\0\0" + (b"\x64\x86" if architecture == "amd64" else b"\x64\xaa"))
        self.assertEqual((destination / "install.sh").read_bytes(), (ROOT / "scripts/install.sh").read_bytes())
        self.assertEqual((destination / "LICENSE").read_bytes(), (ROOT / "LICENSE").read_bytes())
        self.build("v0.1.0", destination, False)
        self.assertEqual((destination / "SHA256SUMS").read_text().splitlines(), manifest)
        self.assertEqual(self.git("status", "--porcelain"), "")

    def test_invalid_tag_source_and_output_refused(self):
        for version in ["0.1.0", "v01.1.0", "v0.1.0-rc1", "v0.1.0/other", "v9.9.9"]:
            output = self.base / ("invalid-" + str(len(version)) + version.replace("/", "-"))
            self.build(version, output, False)
            self.assertFalse(output.exists())
        output = self.base / "dirty-output"
        dirty = self.repo / "untracked-file"
        dirty.write_text("source dirt")
        try:
            self.build("v0.1.0", output, False)
            self.assertFalse(output.exists())
        finally:
            dirty.unlink()
        self.build("v0.1.0", self.repo / "not-ignored", False)
        self.assertFalse((self.repo / "not-ignored").exists())
        self.build("v0.1.0", self.repo, False)


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3
"""Tests for ci_scope.py: which jobs a pull request's files need."""
import os
import shutil
import subprocess
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "ci_scope.py")
ROOT = os.path.dirname(HERE)
sys.path.insert(0, HERE)
sys.dont_write_bytecode = True
import ci_scope  # noqa: E402


def run(*paths):
    out = subprocess.run([sys.executable, SCRIPT], input="\n".join(paths), capture_output=True, text=True, check=True).stdout
    return dict(line.split("=") for line in out.split())


ALL = {k: "true" for k in ["go", "node", "agentd", "macos", "windows"]}


class Scope(unittest.TestCase):
    def test_docs_only_runs_no_race_and_no_other_platform(self):
        self.assertEqual(run("docs/linear.md", "RELEASE-NOTES.md", "README.md"),
                         {"go": "false", "node": "false", "agentd": "false", "macos": "false", "windows": "false"})

    def test_a_change_outside_every_platform_runs_go_only(self):
        # cmd/clawbridge is Windows' now: its integration tests build it there.
        self.assertEqual(run("cmd/example/main.go"),
                         {"go": "true", "node": "false", "agentd": "false", "macos": "false", "windows": "false"})

    def test_the_skill_is_code(self):
        got = run("skills/clawbridge/SKILL.md")
        self.assertEqual((got["go"], got["windows"]), ("true", "true"))

    def test_a_windows_dependency_runs_windows(self):
        self.assertEqual(run("internal/bus/store.go")["windows"], "true")
        self.assertEqual(run("internal/agentd/launcher.go")["windows"], "false")

    def test_the_cli_runs_windows(self):
        self.assertEqual(run("internal/cli/linear.go")["windows"], "true")
        self.assertEqual(run("internal/notify/private_windows.go")["windows"], "true")
        self.assertEqual(run("internal/bus/store_windows.go")["windows"], "true")
        self.assertEqual(run("internal/ctxwatch/proc_windows_arm64.go")["windows"], "true")
        self.assertEqual(run("cmd/clawbridge/main.go")["windows"], "true")
        self.assertEqual(run("integration/wake_path_test.go")["windows"], "true")

    def test_installers_run_both_platforms(self):
        got = run("scripts/install.ps1")
        self.assertEqual((got["macos"], got["windows"]), ("true", "true"))

    def test_the_agent_and_agentd(self):
        got = run("dispatcher-agent/src/agent.ts")
        self.assertEqual((got["node"], got["agentd"], got["go"]), ("true", "false", "true"))
        self.assertEqual(run("internal/agentd/launcher.go")["agentd"], "true")

    def test_ci_itself_and_modules_run_everything(self):
        for path in [".github/workflows/ci.yml", "scripts/ci_scope.py", "go.mod", "go.sum"]:
            self.assertEqual(run(path), ALL, path)

    def test_an_empty_list_runs_everything(self):
        self.assertEqual(run(), ALL)

    def test_an_unknown_file_counts_as_code(self):
        self.assertEqual(run("Dockerfile")["go"], "true")


def go_deps(goos, *packages, tests=True, goarch=None):
    """The repository directories `go list -deps` names for packages."""
    env = dict(os.environ, GOOS=goos)
    if goarch:
        env["GOARCH"] = goarch
    args = ["go", "list", "-deps"] + (["-test"] if tests else []) + ["-f", "{{.ImportPath}}"] + list(packages)
    out = subprocess.run(args, cwd=ROOT, env=env, capture_output=True, text=True, check=True).stdout
    dirs = set()
    for line in out.split():
        path = line.split(" ")[0].removesuffix(".test")
        if path == "clawbridge" or path.startswith("clawbridge/"):
            dirs.add(path.removeprefix("clawbridge").lstrip("/") or ".")
    return dirs


@unittest.skipUnless(shutil.which("go"), "needs the Go toolchain")
class Dependencies(unittest.TestCase):
    """A job's path rule must cover every package its tests build."""

    def test_the_windows_job_covers_its_dependencies(self):
        # The integration tests also build and run ./cmd/clawbridge there.
        packages = ["./cmd/clawbridge", "./internal/cli", "./internal/notify", "./internal/setup", "./internal/ctxwatch", "./internal/linearconn", "./integration"]
        if os.path.isdir(os.path.join(ROOT, "internal", "selfupdate")):
            packages.append("./internal/selfupdate")
        # Both Windows jobs: x64 and Windows on ARM.
        for goarch in ("amd64", "arm64"):
            missing = sorted(go_deps("windows", *packages, goarch=goarch) - set(ci_scope.WINDOWS_DIRS))
            self.assertEqual(missing, [], f"GOARCH={goarch}: add these to WINDOWS_DIRS in scripts/ci_scope.py")

    def test_the_agentd_image_covers_its_dependencies(self):
        missing = sorted(go_deps("linux", "./cmd/clawbridge-agentd", tests=False) - set(ci_scope.AGENTD_DIRS))
        self.assertEqual(missing, [], "add these to AGENTD_DIRS in scripts/ci_scope.py")


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3
"""Reads a pull request's changed files (one per line) on stdin and prints
which CI jobs it needs, as GitHub step outputs (go=true|false, ...).

A path no rule names is treated as code, so an unexpected file runs more,
never less. CI's own workflow, this script and go.mod/go.sum run everything.
"""
import fnmatch
import sys

# Files that change only documentation: no Go race run, no other platform.
# The skill is embedded in the binary (go:embed), so skills/ is code.
DOCS = ["docs/*", "*.md", "LICENSE"]
CODE = ["skills/*"]

# The Go packages the Windows job's native tests build (cli, notify, setup,
# ctxwatch, linearconn, selfupdate and integration, with their test
# dependencies, for GOOS=windows on amd64 and arm64: the Windows on ARM job
# runs the same tests). test_ci_scope.py fails when `go list
# -deps` names a directory this list doesn't cover, so a new dependency
# can't slip past the Windows job.
WINDOWS_DIRS = [
    "integration", "skills",
    "internal/auth", "internal/buildinfo", "internal/bus", "internal/cli", "internal/clirender",
    "internal/ctxwatch", "internal/devicekey", "internal/devicesign", "internal/dispatch",
    "internal/dispatchermodel", "internal/docpublish", "internal/docseed", "internal/doctor",
    "internal/faketool", "internal/hostobserve", "internal/install", "internal/linearapp",
    "internal/linearboard", "internal/linearconn", "internal/linearkey", "internal/linearpace",
    "internal/linearprovider", "internal/mcpserver", "internal/mirror", "internal/notify",
    "internal/outbox", "internal/playbook", "internal/principal", "internal/remote",
    "internal/runner", "internal/secretenv", "internal/selfupdate", "internal/server",
    "internal/setup", "internal/skill", "internal/transporterr", "internal/tui", "internal/watcher",
]
# The packages agentd's image builds (go list -deps ./cmd/clawbridge-agentd).
AGENTD_DIRS = ["cmd/clawbridge-agentd", "internal/agentd", "internal/transporterr"]
EVERYTHING = [".github/workflows/*", "scripts/ci_scope.py", "scripts/test_ci_scope.py", "go.mod", "go.sum"]
RULES = {
    "node": ["dispatcher-agent/*"],
    "agentd": [d + "/*" for d in AGENTD_DIRS],
    "macos": ["scripts/install*", "scripts/test_install*"],
    "windows": ["scripts/install*", "scripts/test_install*", "*_windows.go", "*_windows_test.go", "*_windows_*.go"]
    + [d + "/*" for d in WINDOWS_DIRS],
}


def matches(path, patterns):
    # fnmatch's * crosses "/", so "docs/*" is the whole tree.
    return any(fnmatch.fnmatchcase(path, p) for p in patterns)


def scope(paths):
    out = {"go": False, **{k: False for k in RULES}}
    for path in paths:
        if matches(path, EVERYTHING):
            return {k: True for k in out}
        for key, patterns in RULES.items():
            if matches(path, patterns):
                out[key] = True
        if matches(path, CODE) or not matches(path, DOCS):
            out["go"] = True
    return out


def main():
    paths = [line.strip() for line in sys.stdin if line.strip()]
    if not paths:
        # Nothing listed is a failed diff, not an empty change: run everything.
        out = {k: True for k in ["go", *RULES]}
    else:
        out = scope(paths)
    for key, value in out.items():
        print(f"{key}={'true' if value else 'false'}")


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Tests for check_pr_text.py. Secrets are assembled from parts so this file
carries none."""
import os
import subprocess
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "check_pr_text.py")


def run(title="", body="", commits=""):
    env = {"PATH": os.environ.get("PATH", ""), "PR_TITLE": title, "PR_BODY": body}
    p = subprocess.run([sys.executable, "-I", SCRIPT], input=commits, env=env, capture_output=True, text=True)
    return p.returncode, p.stdout


A20 = "a1B2c3D4e5F6g7H8i9J0"
HOOKED = {
    "linear key": "lin_api_" + A20,
    "linear oauth": "lin_oauth_" + A20,
    "github token": "ghp_" + A20 + "abcdefghij",
    "github pat": "github_pat_" + A20,
    "sk key": "sk-" + A20,
    "sk-ant key": "sk-ant-" + A20,
    "aws key": "AKIA" + "ABCDEFGHIJKLMNOP",
    "slack token": "xoxb-" + "1234567890-abcdef",
    "private key": "-----BEGIN RSA PRIVATE" + " KEY-----",
    "google key": "AIza" + "A" * 35,
    "launcher token": "cbl_" + A20,
    "agent credential": "cba_" + A20,
    "model token": "cbm_" + A20,
    "intake token": "cbi_" + "0123456789abcdef" * 4,
    "machine path": "/Users/" + "alice/Dev",
    "home path": "/home/" + "bob/work",
    "machine address": "alice@" + "laptop.local",
    "co-author": "Co-Authored-By: Claude <noreply@anthropic.com>",
    "generator": "Generated with [Claude Code](https://claude.com/claude-code)",
    "robot generator": "\U0001F916 Generated with something",
}


class CheckPRText(unittest.TestCase):
    def test_each_rule_hits_in_each_text(self):
        for name, bad in HOOKED.items():
            for where in ("title", "body", "commits"):
                with self.subTest(rule=name, where=where):
                    code, out = run(**{where: "fix: thing\n" + bad + "\nmore"} if where != "title" else {where: bad})
                    self.assertEqual(code, 1, out)
                    self.assertEqual(len([l for l in out.splitlines() if "line " in l]), 1, out)

    def test_secret_is_masked(self):
        for name, bad in HOOKED.items():
            with self.subTest(rule=name):
                _, out = run(body="before " + bad + " after")
                self.assertNotIn(bad, out)
                self.assertNotIn(bad[8:], out)

    def test_every_secret_on_a_line_is_masked(self):
        _, out = run(body="x " + HOOKED["linear key"] + " and " + HOOKED["github token"])
        self.assertNotIn(A20, out)

    def test_line_is_truncated(self):
        _, out = run(body="y" * 300 + " " + HOOKED["sk key"])
        hit = out.splitlines()[0]
        self.assertLessEqual(len(hit.split(": ", 3)[3]), 100)

    def test_clean_text_passes(self):
        clean = "\n".join([
            "fix(server): recover from handler panics",
            "Paths like /Users/you/Dev, /Users/YOU/x, /home/user/x and /home/runner/work are placeholders.",
            "Contact ada@example.test; a host such as host.example.test is fine.",
            "Request 123e4567-e89b-12d3-a456-426614174000 failed.",
            "Launcher tokens start with cbl_ and intake tokens with cbi_.",
            "The risk-assessment-of-this-very-long-hyphenated-word-is-fine and task-force.",
            "```go",
            "token := os.Getenv(\"TOKEN\")",
            "```",
            "Co-Authored-By: Ada Example <ada@example.test>",
            "Generated files are listed in docs.",
            "The rules: a " + "/Users/" + "NAME or " + "/home/" + "NAME path, or a user@" + "host.local address.",
        ])
        code, out = run(title="fix: x", body=clean, commits=clean)
        self.assertEqual((code, out), (0, ""))

    def test_other_users_paths_hit_but_placeholder_prefixes_do_not(self):
        self.assertEqual(run(body="/Users/" + "youthful/x")[0], 1)
        self.assertEqual(run(body="/home/" + "usernames/x")[0], 1)
        self.assertEqual(run(body="/Users/you/x")[0], 0)

    def test_empty_input_passes(self):
        self.assertEqual(run()[0], 0)

    def test_output_names_text_and_rule(self):
        _, out = run(title="ok", body="a\n" + HOOKED["launcher token"])
        self.assertTrue(out.startswith("PR body: ClawBridge token: line 2: "), out)


if __name__ == "__main__":
    unittest.main()

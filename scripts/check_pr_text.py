#!/usr/bin/env python3
"""Fails a pull request whose title, body or commit messages carry a secret, a
machine path or an AI attribution line (G9-3).

Run as `python3 -I scripts/check_pr_text.py`. The title and body come from the
PR_TITLE and PR_BODY environment variables (never interpolated into a shell
line); the commit messages come on stdin (`git log --format=%B BASE..HEAD`).
One line is printed per hit: which text, which rule, and the line with every
match masked and cut to 100 characters. A secret is never printed.
"""
import os
import re
import sys

# NAME placeholders allowed after /Users/ and /home/ (the repo's pre-push hook
# keeps the same lists, plus the literal NAME and the host.local placeholder
# that describe these rules in prose).
ALLOW_USERS = {"YOU", "you", "example", "runneradmin", "Shared", "NAME"}
ALLOW_HOMES = {"a", "env", "example", "me", "owner", "u", "x", "you", "user", "runner", "ubuntu", "NAME"}

# Token prefixes the product mints: cbl_ (launcher), cba_ (agent credential),
# cbm_ (project model token), cbi_ (intake token).
SECRETS = [
    ("Linear key", r"lin_(?:api|oauth)_[A-Za-z0-9]{16,}"),
    ("GitHub token", r"gh[pousr]_[A-Za-z0-9]{30,}"),
    ("GitHub token", r"github_pat_[A-Za-z0-9_]{20,}"),
    ("API key", r"(?<![A-Za-z0-9])sk-(?:ant-)?[A-Za-z0-9_-]{20,}"),
    ("AWS access key", r"AKIA[0-9A-Z]{16}"),
    ("Slack token", r"xox[abprs]-[A-Za-z0-9-]{8,}"),
    ("private key", r"-----BEGIN [A-Z ]*PRIVATE KEY-----"),
    ("Google API key", r"AIza[0-9A-Za-z_-]{35}"),
    ("ClawBridge token", r"cb[liam]_[A-Za-z0-9_-]{20,}"),
]
RULES = [(name, re.compile(rx)) for name, rx in SECRETS] + [
    ("machine path", re.compile(r"/Users/(?!(?:%s)(?![A-Za-z0-9._-]))[A-Za-z0-9._-]+" % "|".join(sorted(ALLOW_USERS)))),
    ("machine path", re.compile(r"/home/(?!(?:%s)(?![A-Za-z0-9._-]))[A-Za-z0-9._-]+" % "|".join(sorted(ALLOW_HOMES)))),
    ("machine address", re.compile(r"[A-Za-z0-9._+-]+@(?!host\.local\b)[A-Za-z0-9-]+\.local\b")),
    ("AI co-author", re.compile(r"^\s*Co-Authored-By:.*(?:claude|anthropic|\bAI\b|\bLLM\b|gpt|copilot|gemini|codex|cursor|deepseek)", re.I)),
    ("AI generator line", re.compile(r"Generated with \[?(?:Claude|Codex|Copilot|Gemini|Cursor)|\U0001F916 Generated with", re.I)),
]
LIMIT = 100


def mask(line):
    for _, rx in RULES:
        line = rx.sub("[masked]", line)
    return line[:LIMIT]


def check(label, text):
    hits = []
    for number, line in enumerate(text.splitlines(), 1):
        for name, rx in RULES:
            if rx.search(line):
                hits.append("%s: %s: line %d: %s" % (label, name, number, mask(line)))
    return hits


def main():
    texts = [
        ("PR title", os.environ.get("PR_TITLE", "")),
        ("PR body", os.environ.get("PR_BODY", "")),
        ("commit messages", sys.stdin.read() if not sys.stdin.isatty() else ""),
    ]
    hits = [h for label, text in texts for h in check(label, text)]
    for h in hits:
        print(h)
    if hits:
        print("Edit the PR text (or reword the commit) to remove the above; this check re-runs on edit.")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

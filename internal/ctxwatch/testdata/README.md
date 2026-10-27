# ctxwatch test fixtures

Every file here is **synthetic**: hand-written to the agent's format, with
made-up IDs, paths and fixed made-up times.

**Rule: never derive a fixture from a real transcript, rollout or hook
payload**, scrubbed or not. Real files carry tool-call IDs, account data,
private schemas and timestamps that a scrub misses. When a format must be
checked against the real thing, look at key names and value types only,
locally, and write the fixture by hand from those.

`TestFixturesAreSynthetic` fails on tool-call IDs, addresses and home paths.

- `claude-code.jsonl`, `codex.jsonl`: transcripts with the line types the
  reader parses, and a few it skips.
- `hooks/*.json`: Claude Code hook input for `SessionStart` (startup, resume,
  compact) and `PreCompact` (manual). Their key names and types were checked
  against Claude Code's real hook input on 2026-10-26; the values are made up.

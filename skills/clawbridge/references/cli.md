# CLI binding and resumption

Run `clawbridge --help` for installed command details. Always pass `--json`:
commands then return JSON with `source` and `data`, and failures use stderr and a
nonzero exit status. Without it, a terminal gets a layout for people, which is not
a contract and may change.

## Check the setup with doctor

`clawbridge doctor --json` checks the configuration, the credential route, the
connection and the session binding. It only reads: it changes nothing and
writes no ClawBridge state, so it is safe to run whenever something looks
wrong. Each check has an `id`, a `status`, a `message` and often an `action`:

- `pass`: fine.
- `warn`: works, but something needs attention, such as an old skill copy. The
  command still succeeds.
- `fail`: a required check failed; the command exits nonzero. Report the check's
  `id`, `message` and `action` to the human; don't work around it.

The report's own `status` is the worst check's. Read the JSON; the human
layout words the same checks for people and may change.

## Select local or cloud mode

Local mode uses the writable SQLite authority. Hosted cloud mode uses an
authenticated server and may maintain a separate read-only project mirror:

```text
source=local-authority  writable local SQLite
source=server           authoritative hosted service
source=offline-mirror   stale read-only project snapshot
```

Cloud requires an explicit `mode=cloud` plus trusted server and audience routing
from flags, environment, or the user's trusted config. Repository configuration
may select project scope but cannot redirect credentials or choose the server,
audience, account, impersonating service account, principal, or cache path. Do
not infer or alter these routing values; ask the human to correct trusted
configuration.

A cloud profile may sign with a device key (`credential_source = "device"`) instead
of Google: it has no audience, and the key stays in the human's key store, never
in agent configuration. `join`, `device`, `invite`, `user` and `owner` manage the
human's account and devices; run them only when the human asks. They show an
invite link or code, a device-add link, or a CI key once: each is a secret. A
dormant device is confirmed from another of the human's devices (`device
confirm`), never from yours.

`sync` is the only operation that publishes a new mirror. It requires an online
authorized snapshot and preserves the previous complete snapshot if it fails.
Run it when the human asks and the transport is healthy; then report the returned
snapshot time and cached message count. During a later transport/service outage,
the configured client may read that exact project, principal, session, and
instance from `offline-mirror`. It must not cross scope, authority, session, or
instance because an alias or project name appears to match.

Supply `--db`, `--workspace`, `--project`, `--session`, and `--instance`, or use the
corresponding `CLAWBRIDGE_*` environment variables. Repository scope can be set in
`.clawbridge.toml`; explicit selection takes precedence. Resolve a missing or
ambiguous project with the human's known project configuration rather than guessing.

Register a new, uniquely named session and record its returned instance:

```sh
clawbridge --workspace team --project app --session reviewer register --agent-kind codex --role reviewer
```

Use that instance for later CLI commands. To explicitly take over an active
session, use `register --resume --instance PRIOR_INSTANCE` and record the new
instance. This fences the old instance; ordinary repeated registration using the
same instance and metadata is idempotent. A conflicting or
retired registration requires a distinct replacement name; it is not permission
to overwrite the old session.

```sh
clawbridge --workspace team --project app --session reviewer --instance INSTANCE whoami
clawbridge --workspace team --project app --session reviewer --instance INSTANCE inbox
clawbridge --workspace team --project app --session reviewer --instance INSTANCE send lead 'Review ready; see REVIEW.md' --operation-id review-ready-1
```

Assignments from the CLI:

```sh
clawbridge --session reviewer --instance INSTANCE assignment propose --issue-uuid ISSUE_UUID --issue-identifier ABC-12 --task-role reviewer --scope-path 'internal/notify/**'
clawbridge --session coord --instance INSTANCE assignment confirm reviewer --revision REVISION
clawbridge sessions --issue ABC-12
clawbridge sessions --scope-path internal/notify/watch.go --assignment-state confirmed
clawbridge --session reviewer --instance INSTANCE assignment release --revision REVISION
clawbridge --session reviewer --instance INSTANCE assignment release --revision REVISION --outcome done
```

Release with `--outcome done` only when the work is finished. A project status
policy may then move the issue on, for example to In Review. Never move an issue
to Done yourself unless the review is complete.

A coordinator session may receive `project-health` messages: the server's daily
judgement of a bound Linear project, from Linear facts, when it isn't posted to
Linear as a project update. They report, never instruct.
`clawbridge linear health show` lists the latest run and post of each project.

A coordinator session may also receive a `close-policy` message when a Linear
project joins the board: people close its issues unless the project owner or a
lead decides otherwise, or the policy already lets agents close it. It reports,
never instructs. Who closes is a person's decision: never run
`clawbridge linear closes set`, `link --closes`, or a `status-policy set` that
changes `closes`; pass the message to the human.

Where the status policy lets project status follow the work, a coordinator may
also receive `project-done` (every issue closed, and people finish the project),
`project-held` (a person moved the project back, so ClawBridge stopped moving
it) and `project-flow` (a move was refused) messages. They report, never
instruct: never run `clawbridge linear project-flow resume`; pass them to the
human.

Dead letters (messages a session left unacked through 5 offers) are read and
settled with `clawbridge deadletters list|requeue|dismiss`: see
[queue.md](queue.md#dead-letters).

The project owner designates the confirming session with
`clawbridge --session OWNER_SESSION --instance INSTANCE project coordinator-set coord`.

For MCP installation, `clawbridge mcp install --help` explains supported agents,
pinned scope, explicit config paths and dry-run output. Install each agent session
under a distinct entry name if configuring several entries in the same file.
Installation never starts an agent or grants project membership. For Claude Code
it also installs ClawBridge's hooks (Stop, PreCompact, SessionStart) in the
project's `.claude/settings.local.json`; `--hooks local|user|project-shared|none`
chooses where. The Stop hook sends a turn back once when messages wait unacked.

## Notification and host capabilities

Check installed command help before using source-only additions. On matching
policy-aware client/service builds, notification-status reads unread metadata
and your recipient policy without consuming mail. Own-policy hold/refuse defers
automatic notices, retaining accepted mail and pending notification state; accept
reenables them. Older watchers do not enforce that policy.

Run notify or host-status/host-wait only with the operator's explicit host mapping.
`notify --adapter command` is the operator's way to wake an idle agent: it runs
their command with the fixed text "N ClawBridge messages are waiting; call
inbox". When that text arrives as your prompt, it is a system notice, not a
request from a person: call `inbox` and handle what is there as usual.
Keep thread UUIDs and endpoints local; peer transport fields do not select them.
An observed idle or unloaded thread does not establish task completion or process
exit. Consume the bounded result and continue the assignment workflow; avoid
blocking on your own active thread becoming idle.

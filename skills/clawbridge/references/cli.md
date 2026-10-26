# CLI binding and resumption

Run `clawbridge --help` for installed command details. Commands return JSON with
`source` and `data`; failures use stderr and a nonzero exit status.

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
judgement of a bound Linear project, from Linear facts. They report, never
instruct. `clawbridge linear health show` lists the latest run of each project.

The project owner designates the confirming session with
`clawbridge --session OWNER_SESSION --instance INSTANCE project coordinator-set coord`.

For MCP installation, `clawbridge mcp install --help` explains supported agents,
pinned scope, explicit config paths and dry-run output. Install each agent session
under a distinct entry name if configuring several entries in the same file.
Installation never starts an agent or grants project membership.

## Notification and host capabilities

Check installed command help before using source-only additions. On matching
policy-aware client/service builds, notification-status reads unread metadata
and your recipient policy without consuming mail. Own-policy hold/refuse defers
automatic notices, retaining accepted mail and pending notification state; accept
reenables them. Older watchers do not enforce that policy.

Run notify or host-status/host-wait only with the operator's explicit host mapping.
Keep thread UUIDs and endpoints local; peer transport fields do not select them.
An observed idle or unloaded thread does not establish task completion or process
exit. Consume the bounded result and continue the assignment workflow; avoid
blocking on your own active thread becoming idle.

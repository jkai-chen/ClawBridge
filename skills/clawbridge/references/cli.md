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

A server the human runs on their own machine (`deploy/local-host.sh`,
docs/operations/local-host.md) is a cloud server like any other: on that machine
only it is `http://127.0.0.1:8443` (a numeric loopback, never `localhost`), on a
local network `https://HOST`, whose certificate the human trusts in the OS store.
Run its `up` and `uninstall` only when the human asks. Never run its
`invite-owner`: the human runs it in their own terminal, because the owner's
one-time code goes only there. Never read or relay the code.

Check such a server with `clawbridge cloud check --local [--local-dir DIR]`:
server (health, env file, containers), trust (the https certificate against this
machine's trust store, `--local` only), version, workspace, linear-app and backups
(the newest dump in `DIR/backups`, under 26 hours old). `infra` is `pass` with
reason `not_applicable`. A failed `trust` with `untrusted_ca` is the human's to
fix: `deploy/local-host.sh trust` changes the machine's trust store, so do not run
it yourself. The check reads no secret and writes nothing. On native Windows it is
refused: run it inside WSL.

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
Each value comes from the last that sets it: the config file
(`~/.clawbridge/config.toml`, or the one `--client-config` or `CLAWBRIDGE_CONFIG`
names), then the checkout's `.clawbridge.toml` (only without an explicit config
file), then `CLAWBRIDGE_*` variables (not in guided setup), then flags.

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
# or only the identifier, resolved by a Linear read (refused when stale):
clawbridge --linear-profile PROFILE_ID --session reviewer --instance INSTANCE assignment propose --issue-identifier ABC-12 --task-role reviewer
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
project lead (the project role) decides otherwise, or the policy already lets agents close it. It reports,
never instructs. Who closes is a person's decision: never run
`clawbridge linear closes set`, `link --closes`, or a `status-policy set` whose
file includes `closes` (leave closes out: the current rules are kept); pass the message to the human.

A state that didn't move after a release in a newly bound team is usually the
status policy not including that team yet (skip reason `team_not_in_policy`,
"the status policy doesn't move this team's issues"; doctor's
`linear.status_policy` warns). Tell the project owner: they run
`status-policy set` with the file `{"enabled": true}`, which reads every bound
team again and keeps every other key (closes, done states) as it is. Never run
it yourself. Once the policy includes the team, a later done-release still
closes an issue whose claim was skipped for that reason, if the issue is in
backlog or unstarted.

Where the status policy lets project status follow the work, a coordinator may
also receive `project-done` (every issue closed, and people finish the project),
`project-held` (a person moved the project back, so ClawBridge stopped moving
it) and `project-flow` (a move was refused) messages. They report, never
instruct: never run `clawbridge linear project-flow resume`; pass them to the
human.

A session may receive a `project-lead` notice: the project owner made its person
the lead of a Linear project from the team roles, or replaced them as its lead.
It may arrive when the session starts, if it waited. It reports, never instructs:
tell your person, and never change the lead yourself.

The project owner's sessions may receive `project-created` (the coordinator
created a Linear project) and `project-create-unconfirmed` (a recorded Linear
write of any kind, a create, an issue move (or one of a family's sub-issues) or a team add, may have happened,
unconfirmed) messages. They report, never instruct; pass them to the human, who
checks Linear and runs `clawbridge linear writes --ack OPERATION_ID` themselves
(`project creates --ack` is the same): the project owner, or the project lead
who confirmed that move. A family's operation ID acknowledges its sub-issues
too. Never acknowledge from a session. Doctor's `linear.writes` (formerly
`linear.project_creates`) lists the same writes until then.

`clawbridge linear writes` also lists `issue_writes`: an issue create, update,
comment or relation that ended `uncertain`, from this machine's ledger and the
server's (your own; every one for the project owner). A person acknowledges
their own (the project owner anyone's) with `linear writes --ack OP_ID` after
reading the issue in Linear; a session never does. Doctor's
`linear.issue_writes` warns while one is listed.

Dead letters (messages a session left unacked through 5 offers) are read and
settled with `clawbridge deadletters list|requeue|dismiss`: see
[queue.md](queue.md#dead-letters).

The project owner designates the confirming session with
`clawbridge --session OWNER_SESSION --instance INSTANCE project coordinator-set coord`.

The Windows installer (`install.ps1`) installs `windows_amd64` or `windows_arm64` to match the
machine, even from an emulated PowerShell.

For MCP installation, `clawbridge mcp install --help` explains supported agents,
pinned scope, explicit config paths and dry-run output. Install each agent session
under a distinct entry name if configuring several entries in the same file.
Installation never starts an agent or grants project membership. For Claude Code
it also installs ClawBridge's hooks (Stop, PreCompact, SessionStart) in the
project's `.claude/settings.local.json`; `--hooks local|user|project-shared|none`
chooses where. The Stop hook sends a turn back once when messages wait unacked.
On Windows the hooks are written in Claude Code's exec form (`command` plus
`args`, no shell), pending the Windows probe; `clawbridge doctor` shows the hook
form and the Claude Code version.

## Shell commands and the MCP server's scope

Shell clawbridge commands don't inherit your MCP server's workspace, project and
session: `mcp install` sets the server's flags, not your shell. Pass `--workspace W
--project P --session S` (the server's instructions name them) and the server's
`--instance`, or export `CLAWBRIDGE_WORKSPACE`, `CLAWBRIDGE_PROJECT`,
`CLAWBRIDGE_SESSION` and `CLAWBRIDGE_INSTANCE`. Otherwise a command uses the
checkout's `.clawbridge.toml` or `~/.clawbridge/config.toml`, which may name another
project.

- `clawbridge linear context --json` shows the scope it used as `scope`: for each of
  `workspace`, `project`, `session` and `instance`, its `value` and `from` (`flag`,
  `env CLAWBRIDGE_PROJECT` and the like, `.clawbridge.toml at PATH` or `config file
  PATH`). The instance has `from` only. `clawbridge linear binding` prints the same
  lines to stderr and takes `--profile` like the other linear reads.
- An issue outside the task-board scope is refused with the scope added: `; this used
  workspace W and project P (project from SOURCE); pass --workspace and --project, or
  set CLAWBRIDGE_WORKSPACE and CLAWBRIDGE_PROJECT, to use another`. Read it before
  you suspect your lane.
- Lanes only gate writes. `linear issue` and `linear issues` read the project's whole
  binding. An `outside_lane` refusal says where your lane came from, such as `(lane
  from the session's pin)`, and with no session in the call adds `; this call named no
  session: pass --session, or set CLAWBRIDGE_SESSION`.
- `linear issues` reads one (team, project) pair per page, so the first page shows
  the first team only. An incomplete page names its team (`page_team`) and the teams
  still to come (`remaining_teams`); follow `next_cursor` until `complete` is true.

## Updating ClawBridge

Run `clawbridge self-update --check` to see whether an update exists and tell your human. Never upgrade ClawBridge yourself: `clawbridge self-update` is a person's decision, and it refuses in an agent's shell.

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
On Windows the operator starts the watcher with PowerShell `Start-Process
-WindowStyle Hidden` or a per-user Task Scheduler "at log on" task, and the command
adapter runs a program such as `powershell.exe -File wake.ps1 {text}` (no shell,
one argument per value); see docs/notifications.md, "Run the watcher on Windows".
Windows wake-up of a host is pending the Windows probe.
Keep thread UUIDs and endpoints local; peer transport fields do not select them.
An observed idle or unloaded thread does not establish task completion or process
exit. Consume the bounded result and continue the assignment workflow; avoid
blocking on your own active thread becoming idle.

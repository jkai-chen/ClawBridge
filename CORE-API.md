# ClawBridge local core API

Status: implemented, cb-engineer. Module `clawbridge`, Go >=1.25, modernc SQLite
v1.39.1. MCP SDK v1.8.0 and TOML v2.4.3 are retained in go.mod/go.sum.

## Application boundary

```go
bus.Open(path string) (*Store, error)
(*Store).Close() error
(*Store).Execute(context.Context, Request) (Result, error)

type Request struct {
    Operation string         `json:"operation"`
    Workspace string         `json:"workspace"`
    Project   string         `json:"project"`
    Session   string         `json:"session"`
    Args      map[string]any `json:"args"`
}
type Result struct {
    Source string `json:"source"`
    Data   any    `json:"data"`
}
```

Every success has `source="local-authority"`. Errors are Go errors; CLI errors
write stderr and exit nonzero. Selectors accept immutable IDs or workspace names,
project slugs, and session names inside their parent scope. Ambiguity fails.
`Open` requires an explicit path and does not create a workspace. Schema version 1
initialization is transactional; newer schemas and unrelated databases are refused.

`(*Store).ResolveProject(ctx, workspace, project, remote string) (workspaceID,
projectID string, err error)` resolves unique existing immutable scope for adapters.
An explicit project selector takes precedence over remote fallback. No resolution
path creates a project. MCP receives resolved immutable IDs before it starts.

## Operation arguments

Unknown arguments, principal overrides and caller timestamps are rejected.
`instance` is required for actor operations and must match the registered instance.
It is optional for discovery/history and omitted-session whoami. If provided for
list/history, it is validated and refreshes the bound actor's heartbeat.

| Operation | Arguments besides common instance |
| --- | --- |
| `workspace_init` | `mode:"local"` required; Workspace selector or `name`; optional `retention_days` (default 90, 1–3650) |
| `project_create` | Workspace required; `slug`; optional `name`, `git_remote` |
| `register_session` | Workspace/project/session required; optional `instance`, `resume` boolean, `role`, `agent_kind`, `model`, `host`, `account`, `notes`, `transport` object |
| `list_sessions` | Optional `status` (active/retired/moved), `role`, `limit`, `after`; no actor required |
| `send` | `to`, `body`, `operation_id`; optional `kind`, `thread_id` |
| `inbox` | Optional `unread_only` boolean (default true), `limit`, `after` |
| `mark_read` | `ids` array of 1–1000 message IDs; atomic, caller deliveries only |
| `reply` | `message_id`, `body`, `operation_id`; optional `kind` |
| `update_session` | At least one of `role`, `model`, `host`, `account`, `notes`, `transport`; own session only |
| `retire_session` | Optional `successor` (active same-project session), `status` (retired default or moved) |
| `history` | Optional `entity`, `key` (immutable entity ID), `limit`, `after`; no actor required |
| `whoami` | No additional args; provided session requires matching instance |
| `message_status` | `message_id`; sender only, delivery metadata only |

Registration generates an instance if omitted. A repeat requires the matching
instance and compatible supplied metadata; normal repeat registration is idempotent
and does not refresh timestamps. Retired/moved names cannot be reused.
`agent_kind`: claude-code, codex, gemini-cli, antigravity, human, other (default).

`resume=true` requires an existing active session and its prior instance. It keeps
the immutable session ID, rotates to a new generated instance, refreshes timestamps,
audits `session/resumed`, and returns the new session object. Subsequent actor calls
with the old instance fail, including in an already-running MCP process. A lost
resume response requires inspecting `sessions`; resume is not silently retried.

`to` accepts a scoped session name/ID, `@all`, or `@role:ROLE`. Broadcast/role
expansion excludes the sender, snapshots active recipients transactionally and
fails if empty. Stale active sessions remain eligible. Cross-project addresses
fail. Retirement validates successor, rejects self/cycles, and never forwards mail.

Kinds: message, reply, decision, handoff, routing-change. Default message for send,
reply for reply. A new root's `thread_id` is its own ID. Explicit send thread_id
must be a root visible to the sender. Reply validates participation, targets the
original sender, preserves the root thread_id and records separate reply_to.

`operation_id` is required (1–128 bytes), scoped to the immutable sender. Exact
send/reply retries return the original message with current delivery metadata;
changed payload is rejected. Recipient role/retirement changes do not re-expand a
successful retry. Retired senders may inspect/replay existing sends but cannot
create new ones. Concurrent duplicate operations create one message/audit event.

## Output shapes

Workspace: `id`, `name`, `mode`, `retention_days`, `owner_principal`, `created_at`.
Project: `id`, `workspace_id`, `slug`, `name`, `git_remote`, `created_at`.
Session: `id`, `project_id`, `name`, `instance`, `owner_principal`, `role`,
`agent_kind`, `model`, `host`, `account`, `transport` object, `notes`, `status`,
`successor` (session ID or null), `last_seen`, `created_at`, `updated_at`,
`presence`, `stale`. Setup/project/register/update/retire return one entity object.

Session presence is derived: active sessions become `presence="stale", stale=true`
after 15 minutes without successful actor use; status stays active. Other presence
values are active/retired/moved. Successful actor use refreshes last_seen in the
operation transaction; errors roll it back. This includes empty inbox, whoami,
status and list/history with an instance. Idempotent registration does not refresh.
Heartbeats are operational metadata, without a separate audit row for each poll.

List/paging results:

```json
{"sessions": [], "next_cursor": 0, "has_more": false}
{"messages": [], "next_cursor": 0, "has_more": false}
{"events": [], "next_cursor": 0, "has_more": false}
```

These correspond to list_sessions/inbox/history. All accept `limit` (1–1000,
default 100) and `after` (nonnegative exclusive sequence cursor, default 0).
Each row has `sequence`; ordering is ascending by immutable sequence. Advance with
next_cursor. It remains usable for polling when has_more=false and is unchanged
for an empty page. Only the returned inbox page is marked delivered, never read.

Send/reply message object:

```json
{"id":"msg_...","sequence":1,"project_id":"prj_...","source_project_id":"prj_...","sender_id":"ses_...","address":"peer","thread_id":"msg_...","reply_to":null,"kind":"message","body":"hello","created_at":"RFC3339Nano","expires_at":"RFC3339Nano","created_by_principal":"local:...","operation_id":"retry-key","deliveries":[{"recipient_id":"ses_...","delivered_at":null,"read_at":null}]}
```

Inbox messages contain the same identity/scope/thread/body/time/principal fields,
plus `sender` name and the caller's delivered_at/read_at. They omit operation_id,
payload hash, and other recipients' metadata. `message_status` returns message
identity/sequence/scope/thread/times, plus deliveries including recipient name and
ID, nullable delivered_at/read_at; body is omitted.

`mark_read` returns `{"deliveries":[{"message_id":"...","recipient_id":"...",
"delivered_at":"...","read_at":"..."}],"marked":1}`. Repeats return marked=0
and preserve original timestamps. A mixed valid/invalid ID set rolls back entirely.

History events: `sequence`, `id`, `workspace_id`, `project_id`, `entity`,
`entity_key`, `action`, `details` object, `changed_at`, `changed_by_principal`,
`changed_by_session` (nullable). History is append-only and contains no message body.

Whoami: `{"workspace":{...},"project":{...},"session":{...}|null,
"principal":"local:...","identity_provenance":"local-os-user-unverified",
"verified":false,"storage_mode":"local","online":true}`.

## CLI

Global flags anywhere before `--`:
`--db PATH --workspace NAME_OR_ID --project SLUG_OR_ID --session NAME_OR_ID
--instance ID --json`. Normal command successes are always JSON, including without
--json. Flags can follow positional command arguments.

```text
setup --local [--name NAME] [--retention-days 90]
project create SLUG [--name NAME] [--git-remote URL]
register [NAME] [--resume] [--role ROLE] [--agent-kind KIND] [--model MODEL] [--host HOST] [--account ACCOUNT] [--notes TEXT] [--transport JSON_OBJECT]
sessions [--status STATUS] [--role ROLE] [--limit N] [--after N]
send TO BODY --operation-id ID [--kind KIND] [--thread-id ID]
inbox [--unread-only=true|false] [--limit N] [--after N]
mark-read ID [ID...]
reply MESSAGE_ID BODY --operation-id ID [--kind KIND]
update [--role ROLE] [--model MODEL] [--host HOST] [--account ACCOUNT] [--notes TEXT] [--transport JSON_OBJECT]
retire [--successor SESSION] [--status retired|moved]
history [--entity ENTITY] [--key KEY] [--limit N] [--after N]
whoami
status MESSAGE_ID
mcp
mcp install ...
```

`register --resume --instance PRIOR_INSTANCE` returns the rotated instance.
Session identity is explicit, never inferred from the active terminal. `--` protects
positional text that looks like a flag, e.g. `send --operation-id ID -- mcp --project`.
`send mcp install --operation-id ID` sends literal body install to recipient mcp.
Installer dispatch only examines the command position.

Environment: CLAWBRIDGE_DB, CLAWBRIDGE_WORKSPACE, CLAWBRIDGE_PROJECT,
CLAWBRIDGE_SESSION, CLAWBRIDGE_INSTANCE, CLAWBRIDGE_MODE, CLAWBRIDGE_CONFIG,
CLAWBRIDGE_HOME. Use explicit temporary paths for isolated tests.

Default state directory: CLAWBRIDGE_HOME or ~/.clawbridge. Setup persists selected
DB/workspace and mode=local to its config.toml, preserving unrelated TOML keys with
an atomic rename. CLAWBRIDGE_CONFIG selects another file (missing is allowed for
first setup) and bypasses home config. Otherwise checkout-root .clawbridge.toml
overlays home config, including in linked worktrees. Recognized TOML keys: db,
workspace, project, session, instance, mode. Relative config DB paths are resolved
against the config directory. Flags > environment > checkout config > home config.

If project is absent, match a registered normalized origin remote. Unknown/multiple
matches fail; no slug is invented. Normalization strips transport/user/trailing
slash/.git, lowercases host, preserves repository path case and explicit port.
Nonlocal configured modes fail before opening a local store. Workspace mode in
SQLite must also be explicitly initialized local; no cloud fallback/write queue.

Wrappers supplied by cb-designer:
`mcpserver.Run(ctx context.Context, dbPath,workspace,project,session string,
instance ...string) error`, `install.Run(args []string) error`.
CLI resolves immutable scope, closes its resolver Store and passes pinned values to
MCP. Installer receives explicit global scope/DB/instance flags before or after its
command; agent configuration is touched only by an explicit installer invocation.

## Limits and local-mode caveats

- OS-user attribution is unverified. Instance fencing is lifecycle protection, not
  secure isolation from another process of that OS user. No cloud auth is claimed.
- Routing roles confer no permissions. Local update/retire only operate on the
  caller's own instance. Peer administration requires later hosted member checks.
- Bodies: nonempty UTF-8, <=65536 bytes, no NUL, common secret patterns rejected
  without echoing content. Pattern detection is not a proof of absence of secrets.
- Max fanout/active sessions per project: 1000. Max args encoded JSON: 80000 bytes.
  Instance/role/agent kind/model/host/account: 256 bytes. Notes/transport: 8192 bytes.
  Session send rate: 120 new messages/minute; retries do not consume another send.
- Application-generated UTC timestamps use fixed nine-digit fractions. Messages
  expire from inbox/reply/status reads using retention_days. No physical database
  purge/compaction, cloud mirror, sync, push, or offline write queue is implemented.
- WAL, foreign keys, one connection per Store, immediate short transactions and
  bounded SQLite busy timeout support concurrent processes. No raw open/close of
  existing SQLite files; this preserves SQLite's POSIX lock lifecycle.

## Verification and resolved review findings

Focused `go test ./internal/bus ./internal/cli` passes. Unit coverage includes
injected delivery failure rollback, mixed-ID mark-read rollback, resume fencing
with preserved deliveries, stale heartbeat/error behavior, schema refusal,
normalization, limits, CLI exchange/read/reply/resume, config precedence/isolation,
ambiguous remote, literal command words and flag-like bodies.

R1 was independently reproduced and fixed by correcting SQLite handle/file
lifecycle; reviewer regression covers long-lived MCP + CLI writes. R2 was fixed by
command-position dispatch; literal recipient mcp/body install has a unit regression.
Reviewer owns integration/**, full/race/vet/cross-build checks and final evidence in
REVIEW.md. The owner subsequently authorized committing and pushing this implementation.

Final independent verification reported in REVIEW.md: `go test ./... -count=1`,
`go test -race ./... -count=1`, `go vet ./...`, and CGO-disabled darwin/arm64,
linux/amd64, windows/amd64 builds all pass after the R1/R2 and lifecycle fixes.
Full installed Claude tool execution remains a separately documented client
conformance limit; see docs/CLIENT-CHECKS.md and REVIEW.md.

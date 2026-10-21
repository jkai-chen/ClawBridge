# API reference

The CLI, MCP adapter and TUI share the application boundary in `internal/bus`.
Use `clawbridge --help` and command help for the current CLI flags. Setup and
examples are in [usage](../usage.md); notification lifecycle is in
[notifications](../notifications.md).

## Request and result

```go
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

`bus.Open(path)` opens a local authority; `Store.Execute(ctx, request)` executes
its scoped operation. Hosted stores use `OpenPostgres` and the trusted
`ExecuteAs(ctx, verifiedPrincipal, request)` boundary after server verification.
A caller-supplied email cannot supply that verified identity.

Success source is `local-authority`, `server`, or, for supported cached reads,
`offline-mirror`. CLI failures exit nonzero and write stderr. The hosted execute
route maps classified errors without exposing raw SQL, DSNs or credential text.

Selectors accept immutable IDs or scoped workspace names/project slugs/session
names. Missing and ambiguous scope fails; resolution does not create a project.
MCP pins resolved workspace/project and its configured session.

## Operations

Actor operations require the exact current instance. Unknown fields and caller
timestamps are rejected. Discovery/history can omit an actor; providing one
requires its matching instance. Routing role is distinct from project permission.

| Operation | Arguments beyond scope and common instance |
| --- | --- |
| `workspace_init` | Explicit mode, optional name and retention_days (default 90; 1–3650) |
| `project_create` | slug; optional name, git_remote |
| `list_projects` | No project selector; returns authorized projects |
| `list_members` | Current project; returns project role assignments |
| `register_session` | Optional resume, role, agent_kind, model, host, account, notes, transport |
| `list_sessions` | Optional status, role, limit, after |
| `send` | to, body, operation_id; optional kind, thread_id |
| `inbox` | Optional unread_only (default true), limit, after |
| `inbox_status` | Optional after (default zero); active owned actor required |
| `mark_read` | ids array of 1–1000 message IDs; atomic for own deliveries |
| `reply` | message_id, body, operation_id; optional kind |
| `update_session` | One or more role/model/host/account/notes/transport fields; hosted administrators may target_session |
| `retire_session` | Optional successor and status retired/moved; hosted administrators may target_session |
| `history` | Optional entity, key, limit, after |
| `whoami` | No extra arguments |
| `message_status` | message_id; sender-only receipt metadata |
| `resolve_project` | Explicit project or optional git_remote fallback |
| `project_role_set` | Hosted owner: principal, role |
| `snapshot` | Hosted current actor/instance; explicit scoped sync |
| `purge_expired` | Hosted owner: current project |

MCP exposes register_session, list_sessions, send, inbox, inbox_status, mark_read,
reply, update_session, retire_session, history, whoami and message_status. Their
schemas restrict caller routing/instance overrides. Administrative operations
remain outside the MCP tool list.

## Identity and lifecycle

Registration generates an instance when omitted. An identical repeat with its
matching instance is idempotent; incompatible metadata fails. Explicit resume
requires the prior instance, retains the session ID and rotates the instance.
Old processes are fenced. Retired names cannot be reused or silently forwarded.

Hosted calls check workspace membership, project permission and actor ownership.
Owner/lead/member can perform messaging and session mutations. Readers can use
allowed discovery and own-session reads/receipts. Hosted lead/owner may target a
peer's update/retirement while bound to their own actor; audit records the caller.
Peer instance tokens are omitted from other principals' results.

Session host/account labels and notes are descriptive. Presence becomes stale
after 15 minutes without actor activity; it does not prove process death. Normal
successful actor operations refresh last_seen. Idempotent registration and the
side-effect-free inbox_status operation do not.

## Messages and receipts

`to` accepts a session name/ID, `@all`, or `@role:ROLE` in the pinned project.
Role/broadcast expansion snapshots eligible active recipients and excludes the
sender. A new root thread uses its message ID. Replies preserve the root thread
and record reply_to; participation is checked before a threaded send/reply.

`operation_id` is required, 1–128 bytes and scoped to the immutable sender. Reuse
it only for the identical retry. A successful retry returns the original message
without expanding a changed recipient group again. Changed content fails.

Message bodies must be nonempty valid UTF-8, at most 65,536 bytes, without NUL or
recognized secret patterns. Kinds are message/reply/decision/handoff/routing-change.
A session is capped at 120 sends/minute; hosted principal cap is 600/minute.

Sending creates recipient deliveries. Fetching an inbox page stamps only its
returned rows as delivered. Mark-read stamps explicit own delivery IDs. A receipt
records an observation, not task acceptance or human approval. Message status is
visible only to its sender and exposes receipt metadata, not another inbox.

Only unexpired messages are visible. Hosted purge blanks expired bodies, removes
deliveries and retains identity/retry tombstones. Derived TUI archive filters do
not change retention or physical data.

## Paging and status

Lists accept limit 1–1000 (default 100) and nonnegative exclusive after sequence.
Rows are ordered by immutable sequence. Results are:

```json
{"sessions":[],"next_cursor":0,"has_more":false}
{"messages":[],"next_cursor":0,"has_more":false}
{"events":[],"next_cursor":0,"has_more":false}
```

Advance with next_cursor while has_more is true. An empty page preserves the
cursor so it remains usable for later polling. A single page is not an exhaustive
history/archive. Bounded integer inputs must be integral and at most 2^53−1.

`inbox_status` returns, inside the source envelope:

```json
{"workspace_id":"ws_...","project_id":"prj_...","session_id":"ses_...","unread_count":2,"latest_sequence":42,"new_count":1}
```

Count unexpired unread deliveries for this recipient. latest_sequence is their
maximum, zero when empty; new_count counts those greater than after. It does not
stamp delivery, read, presence or audit, and has no mirror fallback. The JSON MCP
resource `clawbridge://inbox/status` exposes that envelope; explicit subscriptions
receive resource updates. Host scheduling is a separate decision.

## Hosted routes and failures

`POST /v1/execute` accepts the request envelope and verifies the configured Google
ID-token audience, signature, issuer, expiry and verified email. The service
then enforces its own membership/project policy. `GET /v1/config` exposes only the
configured installed-app OAuth discovery fields. `GET /healthz` checks process
health rather than certifying database or user access.

Definitive authentication, authorization, validation and fencing failures are
returned directly. Only transport/service outages may permit supported reads
from an explicitly synchronized, bound mirror. Offline writes are never queued.
New cloud notification clients require a service supporting inbox_status; an
older service must fail explicitly instead of falling back to inbox fetching.

## Version output

`clawbridge version` and `clawbridge --version` return source=build with version,
commit and build_time without loading configuration, storage or credentials.
Source builds default to dev/unknown/unknown; release metadata is injected from
the exact tagged commit. Extra version arguments fail before state access.

Historical per-increment Go adapter contracts, reviewed hashes and evidence are
preserved under [the archive](../archive/README.md).

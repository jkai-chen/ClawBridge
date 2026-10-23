# API reference

The CLI, MCP adapter and TUI share the application boundary in `internal/bus`.
Use `clawbridge --help` and command help for the current CLI flags. Setup and
examples are in [usage](../usage.md); notification lifecycle is in
[notifications](../notifications.md).

Current source adds [coordination](../design/coordination.md) message metadata,
recipient wake policy, local host observation and [native Linear](../linear.md)
task-board actions after v0.1.0. Dated acceptance evidence is indexed in
[the archive](../archive/README.md); a source build does not upgrade a hosted service.

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
| `list_sessions` | Optional status, role, limit, after; assignment filters issue (UUID or display identifier), scope_path (a repository path a live assignment covers) and assignment_state (proposed/confirmed). Each row carries its live `assignment` or null |
| `send` | to, body, operation_id; optional kind, thread_id, coordination |
| `inbox` | Optional unread_only (default true), limit, after |
| `inbox_status` | Optional after (default zero); active owned actor required |
| `notification_status` | Same metadata plus recipient policy; active owned actor required |
| `set_notification_policy` | policy accept/hold/refuse; active own actor only |
| `mark_read` | ids array of 1–1000 message IDs; atomic for own deliveries |
| `reply` | message_id, body, operation_id; optional kind, coordination |
| `update_session` | One or more role/model/host/account/notes/transport fields; hosted owners and leads may target_session (see below) |
| `retire_session` | Optional successor and status retired/moved; hosted owners and leads may target_session (see below) |
| `history` | Optional entity, key, limit, after |
| `whoami` | No extra arguments; the session includes its live `assignment` and the project its `coordinator_session` |
| `assignment_propose` | Own session only: optional issue_uuid (lowercase UUID), issue_identifier (needs issue_uuid), task_role, scope_repo (git remote), scope_paths (up to 32 patterns); needs an issue or a path. Replaces the session's live assignment and returns overlapping live assignments as `conflicts` |
| `assignment_confirm` | Designated coordinator session: revision, optional target_session, allow_overlap. Refuses overlap with a confirmed assignment unless allow_overlap (audited as conflict_overridden) |
| `assignment_release` | revision; optional target_session for the designated coordinator |
| `coordinator_set` | Owner, or lead for its own and member/reader sessions: target_session, or empty to clear |
| `message_status` | message_id; sender-only receipt metadata |
| `resolve_project` | Explicit project or optional git_remote fallback |
| `taskboard_get` | Shared Linear mapping; optional current actor/instance |
| `taskboard_access` | Own effective project-write capability; optional actor/instance |
| `taskboard_set` | expected_revision, linear_team_ids, linear_default_team_id, linear_project_id (UUID/null); optional linear_workspace_id (UUID/null) |
| `taskboard_clear` | expected_revision; authorized owner/lead; optional repair (project owner, unreadable binding only) |
| `taskboard_get_v2` | Versioned shared mapping; optional current actor/instance |
| `taskboard_set_v2` | expected_revision, mode, linear_team_ids, linear_default_team_id, linear_project_ids; optional nullable linear_workspace_id |
| `taskboard_clear_v2` | expected_revision; authorized owner/lead; optional repair (project owner, unreadable binding only) |
| `project_role_set` | Hosted owner: principal, role |
| `project_role_revoke` | Hosted owner: principal. Deletes the project role, retires that principal's active sessions in the project and removes workspace membership when no project role remains; refuses the workspace owner and self-revoke |
| `snapshot` | Hosted current actor/instance; explicit scoped sync |
| `purge_expired` | Hosted owner: current project |

Peer administration through `target_session` is hosted-only. A project owner may
update or retire any session in the project. A lead may target its own sessions
and sessions owned by member or reader principals, never the owner's, another
lead's or one whose principal holds no project role, and may change the `role`
field only on sessions it owns.

Assignments describe who owns a slice; they are not locks and grant no rights.
Scope patterns are clean repository-relative paths; a pattern covers what it
matches and everything beneath it, and `**` is allowed only as the whole pattern
or a final `/**`. Two live assignments overlap when they share an issue UUID, or
when their repositories are the same or unset and the literal prefixes of their
patterns nest (a conservative test that can report false overlaps). A session
keeps one live assignment. Retiring a session releases it; `status: moved` with a
successor hands it to the successor as a proposal unless the successor already has
one. Retiring or revoking the coordinator session clears the designation. Schema
5 adds `session_assignments` and `projects.coordinator_session`; the offline
mirror does not include assignments.

MCP exposes register_session, list_sessions, send, inbox, inbox_status,
notification_status, set_notification_policy, mark_read, reply, update_session,
retire_session, assignment_propose, assignment_confirm, assignment_release,
history, whoami and message_status. Their
schemas restrict caller routing/instance overrides. Administrative operations
remain outside the MCP tool list.

With an explicitly selected native Linear profile, seven additional local tools
expose scoped `taskboard_binding`, `taskboard_context`, `taskboard_issues`,
`taskboard_issue`, `taskboard_create`, `taskboard_update` and `taskboard_comment`.
These call Linear through the native adapter; their source is `linear`. They
cannot choose another profile/authority/project through tool arguments. Board
administration remains in CLI/TUI. See [Linear setup](../linear.md).

The L2 native adapter adds eleven versioned tools: binding, context, issues,
issue, create, update, comment, labels, members, relation_add and relation_remove,
each named `taskboard_<operation>_v2`. It advertises 35 tools in total; a V1-only
adapter retains 24 and an unconfigured adapter retains 17. Prefer the advertised
V2 family for new board calls. Initialization and listing do not access Linear.
`taskboard_context_v2` includes a `capabilities` object with the negotiated
capability flags and, for any that are off, a `reasons` entry explaining why.
`taskboard_update_v2` requires `expected_label_ids` whenever it sends
`label_ids`: the complete label set last read, refused on mismatch.
`taskboard_issues_v2` accepts `updated_after`, an absolute RFC 3339 time;
rows updated earlier refuse the page.
`taskboard_issue_v2` accepts `resolve_labels: true` to prove label IDs when Linear
returns label names only, and returns `description_sha256`, the SHA-256 of the
description it read. With `comments: true` it also returns `comments` (bodies as
`untrusted_body`), `comments_complete`, `comments_truncated`, `comments_order`,
`comments_walk_direction` and `comments_output_order` (the deprecated
`comments_order_direction` repeats `comments_walk_direction` and will be removed
no earlier than v0.3), plus
`comments_stopped_early` when a `comments_since` read stopped at older pages. A complete read adds `comments_watermark`; pass
it back as `comments_since` for newer or edited comments. An incomplete read
gives `comments_watermark_reason` instead (see [Linear](../linear.md)). `taskboard_update_v2` accepts the optional guards
`expected_title` (with `title`) and `expected_description_sha256` (with
`description`); a different current value is refused and nothing is sent.
If Linear's read omits the description, the read has no `description_sha256`
and a description guard is refused rather than checked against nothing.
`taskboard_update_v2` also accepts `add_label_ids` and `remove_label_ids` (1–20
label UUIDs each, not with `label_ids`) to change single labels; the receipt
lists `labels_added` and `labels_removed`, or reports `observed_unchanged`.
The [L2 contract](../design/linear-migration.md) defines the closed schemas and
scope rules; [L2 review](../archive/reviews/LINEAR-L2-REVIEW.md) records acceptance
progress and native capability limits.

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
side-effect-free inbox_status/notification_status operations do not.

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

Optional coordination metadata carries required issue_id (canonical UUID),
assignment_revision (opaque printable ASCII reference) and intent. Intents are
request/accept/decline/clarify/progress/ready/blocked/handoff. Optional request_id
correlates a visible same-issue/revision message; it is required for
accept/decline/clarify and must match message_id on a correlated reply. Optional
source_commit is a full Git object ID. Unknown/null/empty fields are rejected.
The closed object is capped at 1,024 canonical encoded bytes and participates in
retry identity. Legacy messages omit it and retain their historical retry hash.
Metadata follows message visibility/retention and does not attest assignment,
change ownership, close work or supply human approval. Receiver revision checks
follow the [task-board workflow](../../skills/clawbridge/references/task-board.md).

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

`notification_status` returns the same six fields plus policy accept/hold/refuse
from one authoritative snapshot, with the same no-stamp/no-mirror rules.
`set_notification_policy` returns workspace_id, project_id, session_id and policy.
Only the active owned actor/current instance can set its own policy; this
audited mutation accepts no peer override and preserves policy on resume.
Hold/refuse retain accepted mail and defer automatic watcher/MCP notices without
advancing acknowledged notification state. Returning to accept releases pending
unread arrivals. Direct reads remain available; already emitted hints cannot be
recalled. Matching policy-aware clients and service are required.

## Local host observation

host-status and host-wait are operator commands outside the bus/MCP API. They use
an explicitly selected existing Codex Unix WebSocket endpoint and thread UUID,
without loading ClawBridge configuration or touching authority state. Both return
source=host-observation with outcome matched/timeout/unavailable/unsupported/cancelled
and optional last_observation; only matched exits zero. The observation includes
state idle/active/notLoaded/systemError, recognized active_flags, observed_at and
queue_state=unknown. On host-status, matched means a valid status was observed.

The level-triggered idle/unloaded predicates can match the first observation.
Unloaded means notLoaded, not exited. Disconnect, stale presence and retirement
cannot establish a match. Native Windows transport and an exit predicate are
unsupported. See [notifications](../notifications.md) for invocation and limits.

## Shared Linear mapping

Schema 3 adds project task-board settings independently of personal credentials.
`taskboard_get` returns exactly workspace_id, project_id, configured, revision,
binding and updated_at. The initial revision is zero, with null binding/time.
Configured binding contains provider="linear", linear_team_ids,
linear_default_team_id, nullable linear_project_id and nullable
linear_workspace_id. Workspace metadata is used only when the provider actually
supplies it; stable team/project UUIDs enforce the external scope.

Set replaces the complete mapping using an expected revision; teams are 1–20
unique canonical UUIDs and the default must be in that set. Project is explicitly
a UUID or null for a team board. Clear retains a revision/time tombstone. An
owner/lead may mutate; members/readers may inspect. A supplied actor must be
owned/current, and active for mutation. Reads do not stamp presence/audit and
never fall back to a mirror.

`taskboard_access` returns workspace_id, project_id, nullable session_id,
can_write and project_role without stamps. Native issue writes require both an
allowed ClawBridge role and personal Linear write capability. A supplied actor
must also be current/active. Writes verify this immediately before dispatch.
The private local operation ledger retains accepted/uncertain outcomes against
their original profile, scope and mapping revision; an uncertain external write
is not automatically replayed. The mapping is an administrator selection, not a
claim that the authority itself authenticated to Linear or assigned an agent.

Schema 4 adds a stored mapping version while preserving schema-3 mapping bytes
and revision history. V2 results contain exactly workspace_id, project_id,
configured, revision, mapping_version, binding and updated_at. The response
mapping_version is 2. A configured binding contains provider, mode,
linear_team_ids, linear_default_team_id, linear_project_ids and
linear_workspace_id. Mode is team_board with no projects, or project_set with
1–50 sorted unique project UUIDs. Teams remain bounded to 20. The native adapter
verifies each allowed project/team intersection before exposing an issue or
dispatching a write.

V2 get normalizes a stored L1 mapping without writing it. Representable team
boards and singleton project sets retain the exact L1 wire shape through the
old operations. An authorized L1 get/set/clear on a multi-project mapping fails
with `unsupported_mapping` (HTTP 409) before mutation. It never chooses a first
project. Both versions share the same CAS revision and clear tombstone.

A stored mapping this service cannot read is never used or guessed at: every
get, set and clear of it fails with `binding_unreadable` (HTTP 409). The
message names the stored revision. A mapping that is a JSON object but not a
format this version reads may come from a newer ClawBridge; after a downgrade,
upgrade instead of repairing. The only way out on this version is a clear with
`repair: true`, which is accepted only from the project owner, only at the
stored `expected_revision`, and only while the mapping is unreadable (a repair
of a readable mapping is `invalid`; `repair` is not accepted on set). Its audit
entry records `repaired_unreadable_sha256`, the SHA-256 of the cleared row.

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
Policy-aware clients additionally require notification_status; they do not fall
back to the older policy-free response. Shared task-board operations require the
matching schema-3 service; V2 project-set operations require schema 4. An
old-service refusal is definitive and does not trigger a mirror fallback.
The version-2 coordination and version-3 settings upgrades are described in
[recovery](../operations/backup-and-recovery.md#coordination-schema-upgrade);
the [project-set upgrade](../operations/backup-and-recovery.md#linear-project-set-upgrade)
covers schema 4.

## Version output

`clawbridge version` and `clawbridge --version` return source=build with version,
commit and build_time without loading configuration, storage or credentials.
Source builds default to dev/unknown/unknown; release metadata is injected from
the exact tagged commit. Extra version arguments fail before state access.

## Skill output

`clawbridge skill install|status|uninstall` needs no configuration and returns
source=skill with one entry per target: agent, dir, installed,
installed_version, binary_version and current, plus modified (installed files
changed since), unmanaged (files it did not install), unsafe (links and other
non-regular entries) and corrupt (why the marker is not trusted). Install and
uninstall add changed and refused, and install adds backup when `--force` moved
a folder aside; any refused target also makes the command exit nonzero.

Historical per-increment Go adapter contracts, reviewed hashes and evidence are
preserved under [the archive](../archive/README.md).

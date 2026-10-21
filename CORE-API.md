# ClawBridge local core API

Status: local contract retained; hosted Phase 2 additions are below. Module `clawbridge`, Go >=1.26, modernc SQLite
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
| `list_projects` | Workspace required; no project selector; returns projects visible to the verified principal |
| `list_members` | Workspace and project required; returns project role assignments |
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
List projects returns `{"projects":[...]}`, where each hosted row also has the
caller's effective `role` (owner/lead/member/reader). List members returns
`{"project_id":"prj_...","members":[{"principal","display_name","role",
"granted_by","granted_at"}]}`. The hosted workspace owner is always an effective
project owner. Both reads enforce workspace membership and project scope; local
mode returns projects from the explicitly opened workspace and has no hosted
member list.
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
overlays home config, including in linked worktrees. Repository configuration may
provide only db, workspace, project, session and instance scope. Phase 2 briefly
allowed a repository `mode`; Phase 3 removed that allowance. Relative config DB
paths are resolved against the config directory. Flags > environment > trusted
home/explicit configuration > repository scope. A repository cannot select,
create or switch `mode`, even though trusted home, environment or explicit
configuration can. See [Cloud CLI and trusted
configuration](#cloud-cli-and-trusted-configuration) for the frozen rule.

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

# Phase 2 hosted contract

Status: implemented and accepted by the reviewer. Local behavior
above remains supported. New dependencies: pgx v5.11.0 and Google API v0.305.0;
Go >=1.26 is required (local toolchain 1.27).

## Trusted service API and shared policy

```go
bus.OpenPostgres(ctx context.Context, dsn, bootstrapOwner string) (*Store,error)
(*Store).ExecuteAs(ctx context.Context, verifiedPrincipal string, req Request) (Result,error)
```

ExecuteAs is a trusted server boundary after cryptographic Google verification,
never a client-supplied principal field. It copies the Store's immutable execution
configuration per call and never mutates shared identity. Execute on PG and
ExecuteAs on local SQLite both fail. PG success source is `server`.

Validation, session lifecycle, message addressing, retries, deliveries, expiration
and audit behavior share the SQLite service path. A transaction wrapper binds SQL
placeholders and PG's explicit session sequence. PG migrations are separate,
transactional and versioned in `clawbridge_schema` (version 1), with generated
sequences, constraints and append-only history triggers. Newer versions fail.

Non-snapshot transactions lock principal then workspace with advisory transaction
locks: concurrent retry contenders allocate one message/delivery set/audit event.
Principal send cap is 600/minute, session cap 120/minute; exact retries do not
consume another send. Lock wait is bounded to 5 seconds, statements to 12 seconds,
startup migration to 15 seconds. Caller timestamps never become authority time.

Hosted service errors expose `Code() string`: forbidden, not_found, conflict,
invalid, rate_limited, unavailable. Real query/transport failures retain their cause
for unavailable handling rather than being recast as missing membership. HTTP
mapping is designer-owned and never reflects raw SQL, DSNs or credentials.

## Hosted permissions and added operations

Every project operation checks workspace membership and project authorization
inside its transaction, including discovery/history/resolution/snapshot. Workspace
owner has owner authority; other principals require an explicit project role.
Roles owner/lead/member/reader are independent of session routing role.

Member/lead/owner can register/send/reply/update/retire. Reader can discovery,
history, whoami, existing own inbox/read receipts and sent status. Actor ownership
and matching instance are checked, including discovery with a supplied actor.
Denied operations roll back their heartbeat and all other mutations.

| Operation | Request selectors / Args | Result |
| --- | --- | --- |
| workspace_init | Workspace name (or args.name), mode=server required, optional retention_days | Workspace entity with mode=server |
| project_create | Workspace; args.slug, optional name/git_remote | Project entity; creator gets owner role |
| project_role_set | Workspace/project; args.principal and role, optional bound instance | project_id/principal/role/granted_by/granted_at |
| resolve_project | Workspace/Project selectors; optional args.git_remote | {workspace_id,project_id} |
| snapshot | Bound Workspace/Project/Session and args.instance | Complete scoped snapshot below |
| purge_expired | Workspace/project; optional bound actor/instance | {purged_messages,deleted_deliveries} |

Only the operator-configured bootstrap owner may initialize a **new** workspace.
An existing workspace cannot be adopted by changing that flag. Initialization adds
its owner's workspace membership. Workspace owner may create projects. Workspace
or project owner may set project roles; the grant adds membership and project role
atomically with audit. Routing lead never grants administrative permission.

update_session/retire_session additionally accept args.target_session. Project
lead/owner may explicitly target a same-project peer while bound to their own
active actor. Member/reader/local mode cannot target peers. Target owner_principal
never changes. Audit identifies actual caller and actor, with target entity key;
peer metadata updates do not refresh the peer's last_seen. Own-session and resume
behavior remain unchanged. Hosted output omits other principals' instance tokens.

purge_expired logically blanks expired message bodies, removes their deliveries,
sets purged_at and appends a retention event. Immutable message/retry/thread
identities and hashes remain as tombstones, so old retries never resurrect mail.
Physical pages, backups and cryptographic erasure are outside this cleanup.

## Snapshot shape and privacy

```text
{
  workspace: <workspace entity, mode=server>,
  project: <project entity>,
  session: <owned session entity with current instance and sequence>,
  principal: <authenticated email>, verified: true,
  identity_provenance: "google-id-token", storage_mode: "cloud",
  snapshot_at: <server UTC timestamp>,
  sessions: [routing session rows, each with sequence],
  messages: [only bound session's received unexpired inbox rows],
  sent_messages: [only bound session's unexpired sent status metadata],
  events: [project audit rows]
}
```

Snapshot uses one read-only repeatable-read transaction and fixes expiration at
snapshot_at. It does not heartbeat or stamp delivery/read. Received rows match
inbox fields above, including sender name, body and the caller's nullable delivery/
read fields. Sent rows match message_status fields above, include recipient status
metadata and never a body. Sent statuses/deliveries are collected in bulk. Routing
rows omit non-bound instances. Audit details replace instance/prior_instance with
SHA256 fingerprint fields on write and export, including older schema-1 records.
Caller identity cannot be overridden by snapshot args.

Preflight rejects >10000 rows in **any** dataset without truncation. It also rejects
a conservative aggregate size estimate over 8 MiB or encoded result over 12 MiB
(HTTP/cache transport cap 16 MiB). Failure produces no new snapshot or side effects.
Online paging remains usable when an oversized audit scope cannot sync. Paged
incremental sync and audit retention administration are future work; audit deletion
is not supplied as a workaround for this explicit scale limit.

## Cloud CLI and trusted configuration

Added global flags/env:

| Flag | Environment / trusted TOML key |
| --- | --- |
| --mode local|cloud | CLAWBRIDGE_MODE / trusted mode |
| --server URL | CLAWBRIDGE_SERVER / server |
| --audience AUDIENCE | CLAWBRIDGE_AUDIENCE / audience |
| --account EMAIL | CLAWBRIDGE_ACCOUNT / account |
| --impersonate-service-account EMAIL | CLAWBRIDGE_IMPERSONATE_SERVICE_ACCOUNT / impersonate_service_account |
| --principal EMAIL | CLAWBRIDGE_PRINCIPAL / principal |
| --cache PATH | CLAWBRIDGE_CACHE / cache |

Credential-routing precedence is flags > environment > trusted explicit/home
config. Phase 3 deliberately supersedes the Phase 2 repository-mode allowance:
repository config may overlay db/workspace/project/session/instance scope, but it
cannot select or switch authority mode, or inject
server/audience/account/impersonation/principal/cache. This prevents a checked-out
repository from moving a trusted local route into cloud mode or a trusted cloud
route into local mode. Explicit config bypasses home and repo config. Relative
trusted cache paths resolve against the config directory (flag/env relative cache
paths against the state directory). Cloud routing requires explicit mode=cloud
and trusted server+audience. Unknown modes fail. Cloud CLI never opens the
writable SQLite authority or falls back to local mode. Local defaults and explicit
local initialization remain unchanged.

```text
serve [--listen ADDRESS] --audience AUDIENCE[,AUDIENCE...] --bootstrap-owner EMAIL
      [--oauth-client-id CLIENT_ID] [--oauth-device-client-id CLIENT_ID]
      [--database-url-env NAME]
setup --cloud --mode cloud --workspace NAME [--retention-days DAYS]
project role-set EMAIL ROLE
project role set EMAIL ROLE
update ... [--target-session NAME_OR_ID]
retire ... [--target-session NAME_OR_ID]
purge-expired
sync
```

serve must be the first command token: it delegates all remaining operator flags
to server.Run before reading local config or opening any local DB. Server uses
CLAWBRIDGE_DATABASE_URL by default and requires audience/bootstrap owner; production
Run always uses Google's verifier. `--audience` accepts one exact audience for
backward compatibility or a comma-separated ordered set of exact audiences. The
set supports the separate Desktop browser client ID and TVs/Limited Input device
client ID used by the two OAuth acquisition flows. Entries are trimmed only at
the ends; case and all interior characters are significant. Empty entries,
entries containing whitespace/control characters, duplicate entries, more than
16 entries, and entries longer than 2048 bytes fail before Postgres is opened.
Google's official validator must accept the token's exact `aud` value from the
configured set while retaining its normal signature, issuer, expiry and claim
checks. An unlisted audience fails authentication. The set's order is preserved
and verification tries entries in that order. The exported
`NewGoogleVerifier(ctx, audience)` constructor remains the single-audience
compatibility API; `NewGoogleVerifierForAudiences(ctx, audiences []string)` is the
set-based server API and copies the caller's slice. No skip-auth flag or fake
verifier CLI exists.

`--oauth-client-id` and `--oauth-device-client-id` are optional. When supplied,
each value must exactly match one entry in `--audience`, the two values must be
distinct, and each is limited to 2048 bytes with no whitespace or control
characters. A configured ID requires its matching installed-app credential from
`CLAWBRIDGE_OAUTH_CLIENT_SECRET` or
`CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET`, respectively. The credential is never a
server command-line flag. Validation occurs before Postgres initialization.
Omitting both ID/credential pairs preserves the previous execution API and
returns four empty strings from the public config endpoint.

Google treats secrets for distributed native application clients as application
identifiers rather than confidential server credentials. They are still treated
as sensitive by ClawBridge: never print or log them, pass them through process
argv, place them in an agent MCP entry or expose unrelated routing metadata
alongside them.

The server exposes one public, unauthenticated discovery endpoint:

```text
GET /v1/config
Content-Type: application/json
Cache-Control: no-store

{"browser_client_id":"DESKTOP_CLIENT_ID","browser_client_secret":"DESKTOP_CLIENT_CREDENTIAL","device_client_id":"DEVICE_CLIENT_ID","device_client_secret":"DEVICE_CLIENT_CREDENTIAL"}
```

This is a strict four-field JSON object. Each value is the corresponding exact
configured installed-app OAuth client ID or its matching application credential,
or `""` when the pair was omitted. It never includes the audience set beyond
those client IDs, refresh/access/ID tokens, principal or account,
workspace/project/session scope, server or database addresses, credential
source, cache, ADC path, environment values, health state or session tokens.
`GET` is the only accepted method; other methods return 405. The endpoint does
not invoke Google token verification or the application executor.
`/v1/execute` retains its authenticated boundary and `/healthz` retains its
existing narrow status response.

The server-side discovery API additions are:

```go
type OAuthMetadata struct {
    BrowserClientID     string
    BrowserClientSecret string
    DeviceClientID      string
    DeviceClientSecret  string
}
func NewHandlerWithOAuthClientIDs(
    store Executor,
    verifier Verifier,
    browserClientID, deviceClientID string,
) http.Handler
func NewHandlerWithOAuthMetadata(
    store Executor,
    verifier Verifier,
    metadata OAuthMetadata,
) http.Handler
```

`NewHandler(store, verifier)` remains compatible and uses empty discovery fields.
The Compose deployment routes the API and Caddy through a separate non-internal
egress network because the server fetches Google signing certificates; the API
and Postgres have no public host port and the backend network is internal-only.
setup --cloud sends mode=server, requires the verified bootstrap owner, and persists
trusted routing only after successful initialization. setup --local cannot replace
a configured cloud mode. --session-account selects session metadata in either
mode; local register/update --account retains the Phase 1 metadata behavior, while
cloud --account selects the gcloud initiating identity.

Cloud wiring uses designer APIs:

```go
server.Run(ctx context.Context,args []string) error
remote.New(remote.Config{URL,Audience,Account,ImpersonateServiceAccount,Principal,CachePath})
(*remote.Client).Execute(ctx,request)
(*remote.Client).ResolveProject(ctx,workspace,project,gitRemote)
(*remote.Client).Sync(ctx,workspace,project,session,instance)
mcpserver.RunExecutor(ctx,executor,workspace,project,session,instance)
```

All Config fields are strings. Remote principal binds cache identity; it is not
authentication. MCP is pinned to resolved immutable scope. sync is explicit,
online-only snapshot publication. The designer's remote/mirror packages enforce
endpoint/audience/principal/workspace/project/session/instance binding, atomic
publication, expiry and read-only outage fallback. Rejected HTTP requests never
become cached successes; offline writes, including mark-read/resume, are not queued.

## Verification and coordination

Focused go test ./internal/bus ./internal/cli passes, including shared/local
regressions, trusted/local identity guards, dialect binding, audit fingerprints,
repository credential-routing exclusion, serve bypass/no local DB and cloud
whoami/sync with loopback + synthetic gcloud only. A separate-principal PG role
denial/no-side-effect unit test runs with reviewer disposable DSN.

The reviewer ran actual PG lifecycle and final full/race/vet/cross builds. Independent
real PG17.11 checks passed roles/concurrency/replay, HTTP403 denial semantics,
snapshot body/instance privacy, retention tombstones, fanout rollback, honest row
cap, workspaces/resume/schema and built cloud CLI routing/sync. Exact final evidence
and remaining limits are in PHASE-2-REVIEW.md. Auth tests use synthetic keys/verifiers;
no live Google-issued token or cloud deployment is claimed. No billable resource,
IAM grant or real agent config change was created by verification.

# Phase 3 authentication and routing contract

Status: frozen local/synthetic contract accepted by independent review. Live
Google, real OS-keychain subprocess and deployed behavior remain pending. The
deployment direction is Docker Compose on one Compute Engine VM; Cloud Run/IAM
is no longer the Phase 3 target. Field and package names in this section are the
names exposed by the current Phase 3 patch. Changes after this point require an
explicit contract change and a coordinated test update.

## Credential backends and trusted configuration

Cloud clients can use exactly one of three explicit credential backends:

| `credential_source` | Credential and token behavior | Required settings |
| --- | --- | --- |
| `oauth` | Refresh from the OS keychain and mint a short-lived ID token in memory | `oauth_client_id` plus matching `oauth_client_secret`; `oauth_device_client_id` plus matching `oauth_device_client_secret` for device login; `oauth_keyring_service` |
| `gcloud` | Invoke `gcloud auth print-identity-token` | `account`, `impersonate_service_account`; distinct initiating and impersonation identities |
| `adc` | Read an explicitly selected authorized-user ADC file and impersonate a controlled service account | `credentials_file`, `account`, `impersonate_service_account`; distinct initiating and impersonation identities |

An empty `credential_source` retains the Phase 2 pilot behavior and means
`gcloud`. New cloud setup, doctor and MCP installation flows must write an explicit
source. There is no implicit fallback between backends and the client never uses
the Compute Engine metadata server. Selecting OAuth together with gcloud account,
impersonation or ADC fields is a configuration error. Selecting OAuth client
fields with any other backend is a configuration error.

Trusted TOML keys and matching flags/environment overrides are:

| Trusted key | Flag | Environment |
| --- | --- | --- |
| `credential_source` | `--credential-source oauth|gcloud|adc` | `CLAWBRIDGE_CREDENTIAL_SOURCE` |
| `credentials_file` | `--credentials-file PATH` | `CLAWBRIDGE_CREDENTIALS_FILE` |
| `oauth_client_id` | `--oauth-client-id ID` | `CLAWBRIDGE_OAUTH_CLIENT_ID` |
| `oauth_client_secret` | `--oauth-client-secret SECRET` | `CLAWBRIDGE_OAUTH_CLIENT_SECRET` |
| `oauth_device_client_id` | `--oauth-device-client-id ID` | `CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID` |
| `oauth_device_client_secret` | `--oauth-device-client-secret SECRET` | `CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET` |
| `oauth_keyring_service` | `--oauth-keyring-service NAME` | `CLAWBRIDGE_OAUTH_KEYRING_SERVICE` |

Repository `.clawbridge.toml` cannot select any of these fields, nor `server`,
`audience`, `account`, `impersonate_service_account`, `principal`, `cache` or
`mode`. It may provide database/workspace/project/session/instance scope only.
This prohibition is intentional and frozen for Phase 3: repository values for
those routing fields are ignored in both directions, regardless of whether the
trusted mode is local or cloud. Ignoring includes discarding a repository-only
`mode`: it cannot trigger trusted cloud validation or make a missing trusted
route valid. A trusted local route remains local when a repository says
`mode=cloud`; a trusted cloud route remains cloud, and still requires trusted
server, audience and credentials, when a repository says `mode=local`. If no
trusted route selects cloud, the normal local default remains effective.
Flags override environment, which overrides trusted config. Explicit
`CLAWBRIDGE_CONFIG` bypasses home and repository configuration. Relative trusted
`credentials_file` and `cache` paths resolve against the config directory;
relative flag/environment paths resolve against the ClawBridge state directory.
Cloud routing still requires `mode = "cloud"`, `server` and `audience`.

The OAuth browser client is a Google **Desktop app** OAuth client configured by
`oauth_client_id`. The OAuth device flow uses the separate
`oauth_device_client_id`, whose Google application type must be **TVs and Limited
Input devices**. Both request only `openid email`. Supporting both flows therefore
requires two registered client IDs from the same Google Cloud project and
consent-screen configuration. Local validation requires the applicable explicit
client ID; only Google can verify the client's registered application type, so a
wrong-type device client fails at Google's device-authorization endpoint without
falling back.

## Authentication commands

```text
clawbridge login [--device|--use-gcloud]
clawbridge logout
```

`login` uses the applicable client ID from trusted configuration:
`oauth_client_id` for browser login and `oauth_device_client_id` for `--device`.
An explicitly configured applicable ID wins and skips discovery. A nonempty
`oauth_client_id` equal to `oauth_device_client_id` identifies the active promoted
device client, not an applicable Desktop client. Plain browser `login` in that
state discards the active device pair for acquisition and discovers the Desktop
pair from the trusted server. A distinct explicit Desktop pair still skips
discovery. When the applicable value is absent, login performs discovery against the
trusted cloud `server`; repository configuration cannot supply that server.
Browser login selects `browser_client_id`, while `--device` selects
`device_client_id`.

Browser-after-device discovery must fail closed if discovery fails or the selected
browser ID is still the known device ID. It must never send the promoted device
credential to the browser flow. Acquisition or config-publication failure retains
the previous active device route and keychain state under the existing rollback
contract. Successful browser login promotes the Desktop pair and preserves the
device pair for a later `--device` login. Switching active clients does not revoke
the previous client's credential; `logout` still removes only the active entry.

Discovery requests exactly `GET <trusted-server>/v1/config`. It requires HTTPS
except for numeric `127.0.0.1` or `::1` loopback, refuses every redirect, uses a
bounded timeout and body, and accepts only one JSON object with exactly the four
required string fields `browser_client_id`, `browser_client_secret`,
`device_client_id` and `device_client_secret`. Missing, unknown, duplicate or
non-string fields, malformed JSON, trailing content, non-2xx status and an empty
applicable ID or credential fail before OAuth acquisition. Errors do not echo
response bodies, tokens, credentials or untrusted URLs.

Browser login opens the system browser; `--device` prints the verification URL,
user code and expiry to stderr and polls until approval, denial or expiry. On
success, both flows verify the returned Google ID token, save the refresh token
plus safe principal metadata in the OS keychain, and atomically write
`credential_source = "oauth"`, the verified `principal`, the selected
installed-app ID/credential and matching `audience` to trusted configuration.
Browser login
sets active `oauth_client_id` and `oauth_client_secret` to the browser pair,
preserves an explicitly configured device pair, and fills a missing device pair
from a successful discovery response. Device login keys its credential under
the selected device client ID, retains that value in
`oauth_device_client_id`/`oauth_device_client_secret`, promotes both to active
`oauth_client_id`/`oauth_client_secret`, and sets `audience` to the same ID.
Success reports the actual promoted ID:

```json
{"logged_in":true,"credential_source":"oauth","principal":"human@example.com","oauth_client_id":"CLIENT_ID.apps.googleusercontent.com"}
```

Login refuses a principal that conflicts with an existing trusted `principal`.
It leaves the prior trusted configuration intact if verification, keychain save
or the atomic config update fails. Access tokens, ID tokens, refresh tokens,
authorization codes, PKCE verifiers and client credentials are never included in
command output, installer/MCP argv or doctor output. They may be stored only in
trusted client configuration after an explicit login or configured by the
operator. Keychain staging precedes the atomic trusted-config rename. A
trusted-config failure rolls back a newly created keychain entry or restores a
replaced prior entry and leaves prior config bytes effective. Normal cloud
commands continue to require the complete route written by login; they never
perform discovery or accept a half-promoted audience/client pair. For OAuth,
runtime validation requires `audience == oauth_client_id`.

`--use-gcloud` first attempts the configured gcloud route when `account`,
`audience` and `impersonate_service_account` are complete. If it can mint a token,
the command reports:

```json
{"credential_source":"gcloud","principal":"clawbridge-human@PROJECT.iam.gserviceaccount.com"}
```

and does not change trusted configuration. If gcloud is absent, fails or is
incomplete, login falls back to browser OAuth. A successful fallback writes the
OAuth route as above. `--device` and `--use-gcloud` are mutually exclusive.

`logout` requires `oauth_client_id` and removes only its keyed OS-keychain entry.
It does not revoke Google grants, delete ADC files, change gcloud login, remove
mirror data or alter agent configuration. Success is:

```json
{"logged_out":true,"client_id":"CLIENT_ID.apps.googleusercontent.com"}
```

Logout is idempotent when the keychain entry is already absent. A keychain access
failure exits nonzero.

## Public OAuth package

Package `clawbridge/internal/auth` exposes:

```go
const DefaultKeyringService = "clawbridge-oauth"
var ErrCredentialNotFound = errors.New("ClawBridge OAuth credential not found")
var GoogleEndpoints auth.Endpoints

type Credential struct {
    Version      int
    ClientID     string
    Principal    string
    Subject      string
    RefreshToken string
}
type Store interface {
    Load(context.Context, string) (Credential, error)
    Save(context.Context, string, Credential) error
    Delete(context.Context, string) error
}
type KeyringStore struct { Service string }
type Identity struct { Principal, Subject string }
type VerifyFunc func(context.Context, string /* idToken */, string /* audience */) (Identity, error)
type Endpoints struct {
    AuthorizationURL string
    TokenURL         string
    DeviceAuthURL    string
}
type Config struct {
    ClientID, ClientSecret, ExpectedPrincipal string
    Endpoints                                 Endpoints
    HTTPClient                                *http.Client
    Store                                     Store
    OpenBrowser                               func(context.Context, string) error
    Verify                                    VerifyFunc
}
type DevicePrompt struct {
    VerificationURL string
    UserCode        string
    ExpiresIn       time.Duration
}

func Key(clientID string) string
func BrowserConfig(Config) (Config, error)
func DeviceConfig(Config) (Config, error)
func BrowserLogin(context.Context, Config) (Credential, error)
func DeviceLogin(context.Context, Config, func(DevicePrompt)) (Credential, error)
func NewTokenSource(context.Context, Config) (*TokenSource, error)
func (*TokenSource) Token(context.Context) (string, error)
func Logout(context.Context, Store, string /* clientID */) error
```

`Credential` is persisted as keychain value under `Key(ClientID)`. No access or
ID token is durable. `ExpectedPrincipal`, when set, must match the verified login
identity before any credential is saved. `BrowserConfig` validates the
Desktop-app browser configuration. `DeviceConfig` requires an explicit device
client ID separately from the browser client. `BrowserLogin` binds only to
numeric IPv4 loopback, uses a new random state and PKCE `S256` verifier, accepts
one loopback GET callback with an exact state match, and rejects provider or
callback failure without saving a credential. `DeviceLogin` reports a
`DevicePrompt` once and handles pending, `slow_down`, denial and expiry through
the device protocol. With the present Phase 3 CLI bridge, successful
`login --device` promotes `oauth_device_client_id` to the active
`oauth_client_id` after the credential has been keyed under the device client ID;
this keeps one active keychain-backed runtime client and retains the device client
for later `--device` acquisition. Subsequent plain browser login rediscovers the
Desktop pair when the active client equals that known device client, as specified
above. The trusted-config update is atomic. A failure
after keychain save must clean up the newly created keychain entry and preserve
the prior configuration, so login never reports success with an unusable route.
`NewTokenSource` loads the keychain credential,
validates the refreshed identity against the stored principal and subject, and
accepts only a nonempty, bounded, whitespace-free ID token cached in memory until
its expiry.

## Remote integration

`remote.Config` adds:

```go
const (
    CredentialSourceGcloud = "gcloud"
    CredentialSourceADC    = "adc"
    CredentialSourceOAuth  = "oauth"
)

type Config struct {
    URL, Audience, Account, ImpersonateServiceAccount, Principal, CachePath string
    CredentialSource, CredentialsFile string
    OAuthClientID, OAuthDeviceClientID, OAuthClientSecret string
    OAuthDeviceClientSecret, OAuthKeyringService string
}

type Route struct {
    URL, Audience, Account, ImpersonateServiceAccount, Principal, CachePath string
    CredentialSource, CredentialsFile string
    OAuthClientID, OAuthDeviceClientID, OAuthKeyringService string
}

func (Config) PublicRoute() Route
func ValidateRoute(Config) (Config, error)
```

`remote.New` constructs a token source from this explicit route. `Route` is the
safe, non-secret routing projection; it has no active or device OAuth
credential, refresh token or minted token field. `ValidateRoute` applies the same
exclusivity policy used by the runtime, installer and diagnostics. For `oauth`,
it also requires `Audience == OAuthClientID`.
`NewWithTokenSource` remains the injection boundary for tests. OAuth keychain,
refresh, validation and mint failures must be credential failures: they occur
before HTTP or mirror access and never produce an `offline-mirror` result, cache
write or queued operation. A transport or server service outage may still use a
scope-bound read-only mirror exactly as in Phase 2. All writes, mark-read, resume,
HTTP 401 and HTTP 403 fail without fallback.

Package `clawbridge/internal/remote` also exposes the bounded login discovery
client:

```go
type OAuthDiscovery struct {
    BrowserClientID     string
    BrowserClientSecret string
    DeviceClientID      string
    DeviceClientSecret  string
}

func DiscoverOAuthClient(
    context.Context,
    serverURL string,
    *http.Client,
) (OAuthDiscovery, error)
```

The optional HTTP client can inject transport policy for embedded use and tests;
discovery replaces its redirect policy, enforces a maximum ten-second timeout,
and retains any shorter supplied timeout. The function validates the trusted
server URL itself and returns only the four installed-app discovery fields.

## MCP and installer routing

Cloud `mcp install` emits a pinned stdio entry with only routing and scope values
needed to reconstruct the selected client. Permitted cloud arguments are
`--server`, `--audience`, `--principal`, `--cache`, `--mode`,
`--workspace`, `--project`, `--session`, optional `--instance`, the selected
`--credential-source`, and for OAuth `--oauth-client-id`,
`--oauth-device-client-id` and optional `--oauth-keyring-service`. OAuth
`--oauth-client-secret` is not permitted in argv or environment.

ADC routing may include the absolute `--credentials-file` path and safe account,
impersonation and audience identifiers. gcloud routing may include the explicit
account, impersonation and audience identifiers. Installed argv and process
environment must never contain refresh/access/ID tokens, authorization codes,
PKCE verifiers, OAuth client-secret values, ADC JSON, private keys or database
credentials. The MCP process reads the keychain itself; agents never receive its
contents or minted tokens.

Installer and doctor must derive scope and credential values from the same
trusted route. Doctor reports stable JSON checks rather than raw credentials and
must recognize OAuth as a different credential mode from gcloud/ADC impersonation.
An absent or conflicting entry is a diagnostic failure or warning with no repair
or credential fallback.
Doctor must inspect installed `env` as well as argv: credential-bearing variables
and ClawBridge route overrides cannot be treated as a match for the pinned safe
route. Unrelated benign environment entries are permitted. Invalid environment
shapes fail diagnostics, and reports never echo environment values.

## Setup and TUI scaffold boundaries

At the Phase 3 baseline these packages remained scaffolds, rather than delivered
create/join/TUI CLI commands. The Phase 4 contract below wires existing-service
join and the operator TUI; infrastructure provisioning remains a scaffold. The
callable adapters retain the reviewed plan and display boundaries:

- `setup.Operation.Command` and `Runner.Run` use a complete tokenized command
  starting with `gcloud`. `GCloudRunner` validates that executable token, uses its
  configured executable path, and passes only the remaining tokens as arguments.
  It never invokes a shell or includes the executable again as the first argument.
- TUI output treats peer and other dynamic text as data. Message bodies, names,
  scope labels, diagnostics and drafts cannot emit terminal control sequences,
  including OSC clipboard operations. Render controls as visible escaped text
  before applying trusted styling; preserve ordinary Unicode and intended
  newline/tab layout. Rendering does not rewrite persisted message content.

## Verification boundary

Hermetic tests may inject loopback HTTP authorization/token/device endpoints,
synthetic signers and ID tokens, temporary CLI homes and fake keychain stores.
Those fixtures verify protocol and failure behavior; they are separate from live
Google browser/device consent, Google ID-token issuance, Caddy certificate
issuance, Compose networking and deployed authorization. Final Phase 3 review
must name those live checks separately and must not infer them from synthetic
tests.

## Phase 4: existing-service onboarding contract

Contract published 21 October 2026; implementation and acceptance are tracked in
PHASE-4-PLAN.md and PHASE-4-REVIEW.md. This section extends earlier command
contracts. It does not expose infrastructure `BuildPlan`/`Apply` through CLI/TUI.

### Trusted client-config selection

```text
clawbridge --client-config /absolute/path/config.toml COMMAND ...
```

`--client-config` is a global flag, accepted before or after a command. It names
the trusted ClawBridge client config, not the agent configuration target. Require
an absolute path; reject an empty path or duplicate occurrences. It takes
precedence over `CLAWBRIDGE_CONFIG`, bypasses home/repository configuration, and
allows a missing file for first setup/login/join. Other flags and environment
overlays retain their existing precedence. Repository configuration can never
select this path. Existing `CLAWBRIDGE_CONFIG` semantics remain available.
`mcp install --config PATH` still selects the agent config to edit.

### Join CLI and transaction

```text
clawbridge setup join --server URL --workspace NAME_OR_ID --project SLUG_OR_ID
    [--login|--device] [--cache PATH] [--session NAME_OR_ID --instance ID]
```

Server, workspace and project must be supplied explicitly as global command-line
flags for join; neither repository scope nor inherited defaults replace them.
No positional arguments are accepted after `join`. `--login` forces fresh browser
acquisition, and `--device` forces fresh device acquisition; they are mutually
exclusive. Both use only OAuth. `--mode`, when supplied, must be `cloud`.
For a fresh route, omitted mode/source mean cloud/OAuth for this explicit join
command only. No other command changes its default authority/backend.

A fresh OAuth route with no active client ID acquires browser credentials by
default. A complete existing OAuth route reuses its current keychain credential;
failure to refresh/verify it does not cause fresh interactive acquisition or
another backend. Explain that `login` or explicit `setup join --login|--device`
is needed. Existing explicitly selected gcloud/ADC routes may join without OAuth
acquisition; `--login` and `--device` conflict with those backends. New routes
never infer the legacy empty-source gcloud default. All Phase 3 discovery,
applicable-client pairing, principal verification and secret-redaction rules
apply, including browser-after-device rediscovery.

Before acquisition or authority requests, validate URL, explicit scope, backend,
target config and session/instance pairing. Accept HTTPS, with numeric loopback
HTTP only for local deployments/tests; no URL userinfo/query/fragment or redirect
is accepted. A present target must be valid TOML in a regular nonsymlink file;
reject local mode, conflicting pinned principal or a different existing server.
Use a separate client config to select another authority. Explicit workspace and
project flags authorize scope selection within the same service. A changed scope
clears prior session/instance unless both were supplied explicitly for this join.
Omitted session/instance do not register, resume or adopt any actor.

Acquire fresh credentials through the staged auth transaction, keeping the
candidate route in memory. Resolve workspace/project online and request `whoami`
for the immutable IDs. Success requires `source=server`, `online=true`,
`verified=true`, `storage_mode=cloud`, `identity_provenance=google-id-token`, the
expected verified principal and both exact resolved IDs. No mirror response can
authorize join. Neither `workspace_init`, `project_create`, role changes, sync,
registration nor any infrastructure command may be invoked by join.

Only after those checks, atomically publish the complete trusted cloud route:
mode, server, audience, explicit credential source, verified principal, immutable
workspace/project IDs, cache path and the backend's required fields. OAuth
publishes both applicable installed-app pairs, never refresh/access/ID tokens.
The default cache is `<ClawBridge state directory>/cache.db`; relative explicit
cache paths resolve against that state directory. Preserve unrelated TOML keys,
remove stale fields belonging to other credential backends, use same-directory
temporary file plus atomic rename and private 0600 permissions. Deletion-only
or permission corrections count as changes. Identical private routing is a
byte-level no-op (`changed=false`).

Commit staged keychain credentials only after successful config publication.
Every failure before publication preserves prior config bytes and rolls back
new/replaced staged credentials; cancellation uses a bounded cleanup context
independent of the canceled operation context. Report a failed rollback as a
partial keychain-cleanup failure without claiming full restoration. A reused
credential requires no staged rollback. Authorization failure is never login
success, membership creation or an offline success. Server-side read bookkeeping
may occur, but there is no application mutation requested by join.

Successful stdout is exactly one JSON result (progress/consent goes to stderr):

```json
{"source":"server","data":{"joined":true,"config_path":"/absolute/path/config.toml","workspace":"ws_ID","project":"prj_ID","principal":"human@example.com","credential_source":"oauth","changed":true}}
```

Errors return nonzero with no success JSON and no secret values. Existing
`setup.Join` remains the reusable online validation/publication boundary. Extend
`setup.JoinInput` with `OAuthClientSecret` and `OAuthDeviceClientSecret` string
fields for private publication only; these fields must be excluded from JSON
serialization and diagnostic rendering. CLI coordinates acquisition/rollback;
`Join` does not perform OAuth or call gcloud. Existing `JoinResult` fields remain
unchanged; CLI adds `joined`, `credential_source` and the `source/data` envelope.

### Creation and administration boundary

`setup --local [--name NAME] [--retention-days DAYS]` and
`setup --cloud --workspace NAME [--retention-days DAYS]` retain their previous
argument/output shapes and explicit authority checks. The latter initializes
only the existing hosted service as its verified bootstrap owner. Its remote
operation and local config write cannot form one transaction: if the operation
succeeds but publication fails, report "workspace initialized but config write
failed" and do not claim rollback. Neither command invokes infrastructure apply.
No new `setup create`, automatic resource wizard or TUI infrastructure apply is
delivered in this increment. Help must describe this boundary plainly.

### Installed MCP route and diagnostic selection

`mcp install` now resolves the trusted client route before building an entry.
Global routing flags override inherited values; installer-specific flags retain
their existing meaning. Inherit mode, immutable scope when available, backend,
principal, cache and nonsecret client identifiers from the same resolver used by
runtime. Never infer a local entry from a saved cloud route. Require an explicit
distinct `--session` for installation rather than inheriting an operator actor.
Do not auto-register a session or alter the client route. Existing install result
shape, dry-run, target conflict/backup preservation and idempotence remain.

Cloud entries additionally pin the effective absolute trusted client config as
`--client-config PATH` in argv. This is a nonsecret path, not file contents.
The MCP process reads applicable installed-app credentials from that trusted file;
client-secret values and tokens remain forbidden in installed argv/environment.
A fresh MCP process must reconstruct OAuth routing without inheriting the
installer's environment. Other environment overrides remain rejected by doctor.
Programmatic installer callers can supply `Options.ClientConfig`; when supplied
it must be absolute. Existing callers without that field retain their prior
entry format. `doctor.MCPConfig.ClientConfig` supplies the expected pinned path
and expected argv order must agree with the installer.

```text
clawbridge doctor [--mcp-agent codex|claude-code --mcp-config PATH
    --mcp-session NAME [--mcp-name NAME] [--mcp-instance ID]]
```

The three agent/config/session options are required together; name defaults to
`clawbridge`; instance is optional. The diagnostic selection is independent of
the operator session and never registers/resumes it. Resolve the explicit target
path to absolute, construct expected MCP routing from the trusted route and the
selected session, and use the existing read-only doctor checks. Normal `doctor`
retains its output and unselected-MCP warning. Invalid/extra arguments fail
before storage/network activity; installed secret/environment checks retain A1.

### Terminal UI entry

```text
clawbridge tui [--screen overview|projects|sessions|inbox|cloud|doctor]
```

Global trusted routing/scope flags are accepted. Screen defaults to overview.
No other command-specific arguments or positionals are accepted. Help returns
without initializing configuration, storage, authentication or terminal mode.
Non-terminal stdin/stdout and `TERM=dumb` fail promptly with an actionable CLI
alternative before opening storage or contacting a service. No hidden `/dev/tty`
fallback or process launch is allowed. Interactive stdout carries terminal UI,
not a JSON result. The normal CLI/MCP JSON protocol is unchanged.

Resolve/pin immutable workspace/project once, create the selected local or
remote executor and inject it into `tui.New`; attach doctor with the same route.
Do not register/resume an actor, log in, install an entry or initialize setup.
Actorless read screens are allowed; send/reply/inbox/mark-read require the
explicit matching current session/instance. Inbox delivery/last_seen bookkeeping
retains the existing semantics. Existing keys/actions of the reviewed model are
used: 1–6/tab navigation, r reload, arrows/j/k selection, ? help, q/Ctrl-C exit;
Inbox n composes, p replies, m/Enter mark-read, Escape cancels drafts. Draft
submission is an explicit mutation; failed writes are never queued or retried
by switching authority. Quit/cancellation closes local resources and cancels
in-flight work, restoring the terminal.

Overview, Projects and Sessions expose existing read views, not new project
administration. Cloud is informational and states provisioning unavailable;
no SetupSource/Apply is installed by production CLI. Doctor is read-only.
Dynamic text retains A3 escaping. Transport-only mirror reads keep explicit
source/freshness; credential/401/403 errors never fall back. Startup does not
open a local authority in cloud mode. Interactive first-run forms, project
administration and cloud infrastructure creation remain future increments.

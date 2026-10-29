# Native Linear task-board integration

Status: approved L1 implementation contract, 22 October 2026. Baseline is the
accepted coordination commit `a19e249`. The owner authorized implementation,
independent acceptance and bounded live testing overnight. Orca's existing Linear
authentication may assist tests; the installed product must work without Orca,
Node, a teammate's agent host or a separately registered OAuth application.

A user project's migration requests additional capabilities. The separate
[L2 migration extension](linear-migration.md) records the owner-approved scope
and versioned implementation contract. The deployed L1 contract remains unchanged.

Root owns this contract, guides and skills. Engineer owns production and focused
tests; reviewer owns `integration/**` and the archived `LINEAR-REVIEW.md`; the E2E
tester owns private live evidence and bounded test-team fixtures. Coordinate by
one consolidated Codex queue handoff per milestone, identifying L1; supersede old
pause/freeze notes explicitly. No publication or production deployment is part
of this stage. Preserve prior acceptance evidence and the old v0.2.0 candidate.

## Product boundary

Use Linear's official remote MCP service natively from Go, with the existing
pinned SDK. ClawBridge supplies setup, scoped task-board actions and the agent
workflow; Linear supplies issues, dependencies, projects and workflow states.
No task-claim engine, webhook service or background model launcher is introduced.

There are three distinct bindings:

1. A personal Linear connection profile, with its own native credentials.
2. A shared ClawBridge-project task-board binding: verified Linear team UUIDs,
   default team, and optional Linear project UUID.
3. A coordinator-confirmed issue assignment to a ClawBridge session and revision.

Linear OAuth selects a workspace for a connection. Profiles keep separate
workspace/account authorization contexts. Projects may span multiple teams;
issues belong to a team and optionally a project. Team names/keys are never
project selectors. Globally unique team/project IDs, not labels, enforce scope.
If the official MCP does not expose the workspace's UUID, show it as unavailable
rather than inventing one; an operator label is descriptive. Shared settings may
retain a provider-returned workspace UUID, with its provenance, when available.
Every connection must independently resolve the selected team IDs under its own
authorization before using the binding. Switching profiles does not retarget it.

The dedicated live team is a private test team. Exact private workspace/team
IDs are supplied in the trusted worker briefs. No project is currently visible;
support an explicitly selected **Team board — no Linear project** option. This
avoids creating a project merely to complete setup. A product spanning frontend
and backend repos can use one ClawBridge project/binding, with repository-qualified
issue scopes. Different ClawBridge permissions retain separate bindings.

## Native connection and credentials

Use Streamable HTTP at fixed `https://mcp.linear.app/mcp`; read-only connections
may use the official `/mcp/readonly` endpoint. Discover OAuth metadata only at the
official origin. Current public metadata advertises `/authorize`, `/token`,
`/register`, PKCE S256, token authentication `none`, and refresh tokens. Validate
issuer/resource/endpoint origins before registration or sending credentials;
reject redirects that would carry authorization elsewhere. No arbitrary service
URL or credential destination comes from a repo, board field or peer message.

Implement native OAuth authorization-code + PKCE using SDK auth/oauthex support:
explicit connect, bounded loopback callback on 127.0.0.1, unpredictable state,
matching callback path/method, issuer/state checks, S256 and bounded exchange.
Use public-client DCR; cache its metadata privately with the credential. Request
read by default; explicit write access requests read+write. Offer a no-browser
URL mode for an operator arranging the loopback route on SSH/WSL. Do not claim
Linear has Google's device flow. Cancellation/denial/timeout leaves an existing
profile usable and releases the listener/reader. No implicit reauthorization
from tools, TUI refresh, startup, or an expired/revoked token.

Store access/refresh tokens, client credentials and refresh configuration in a
separate native-keyring namespace. Trusted local files contain opaque profile IDs
and safe display metadata only. Serialize refresh per profile, preserve rotated
refresh tokens atomically, and surface secure-store failures without plaintext
fallback. Credential-changing connect commits only after provider/capability
verification and successful secure storage. Logout deletes only the selected
personal profile credential, preserving shared board settings and other accounts.
Do not revoke borrowed test credentials or scan another app's credential store.

Also support explicit token/API-key import from stdin for an operator with an
existing credential; never accept a token in argv or repository configuration.
Imported tokens do not gain a fabricated refresh flow. This path must be native
and provider-neutral: no production code invokes or discovers Orca. Test harnesses
can inject a credential source/transport; borrowing is confined to private test
code and the owner's authorized team.

Use finite network deadlines, response/result caps and redacted error classes.
No raw OAuth/HTTP errors containing tokens, callback URLs or private request
headers reach output. Authentication failure stops; 429/service errors can be
reported retryable, but a dispatched external write is never blindly replayed.
Local keyring availability is distinct from verified unlocked/write-ready access.

## Shared task-board settings

Add schema version 3 with a small project-keyed taskboard binding/tombstone table.
Keep existing v1->v2 migration and accepted retry behavior. Fresh stores reach v3;
v2->v3 is transactional/idempotent in SQLite and PostgreSQL; refuse newer schemas.
No credentials or personal profile names go to the authority database.

Native issue mutations also respect ClawBridge project permissions: owner, lead
and member may write with their own Linear permission; readers may inspect only.
When an actor is supplied, a mutation requires it to be owned/current/active.
An actorless authorized member/operator may use explicit CLI/TUI actions. Add
no-stamp `taskboard_access` (optional common instance only), returning exactly
workspace_id, project_id, session_id (canonical ID or null), can_write, and
project_role. It performs the same scope/current-actor validation as get; retired
actors and readers get can_write=false. Native writes recheck access immediately
before dispatch and fail before a provider mutation if false. This operation is
read-only and never serves a mirror. No public raw provider token substitutes for
ClawBridge role or instance fencing.

Add bus operations `taskboard_get`, `taskboard_set`, `taskboard_clear`. Get is a
no-stamp authorized project read with no mirror fallback. A supplied actor must
be owned/current; an actorless authorized project read is allowed. Set/clear are
audited project owner/lead actions (local authority owner in local mode); an
actor is optional for setup, but if supplied must be owned/current/active. No
descriptive routing role grants permission. Members/readers can read the binding.

The binding contains configured, revision (0 initially), provider=linear,
linear_team_ids (1–20 unique canonical UUIDs), linear_default_team_id in that set,
and optional linear_project_id (canonical UUID). Keep optional provider-returned
workspace metadata separate from these enforcement keys. Include immutable
ClawBridge workspace_id/project_id and updated_at in results. Get unconfigured
returns configured=false and its current revision. Set replaces the complete
mapping; clear removes the mapping but retains a monotonic revision tombstone.
Require expected_revision on set/clear and reject stale updates atomically.
Validate closed fields/types/nulls/limits before writes. No caller timestamps.

Exact bus arguments: get accepts only common optional instance; set requires
expected_revision, linear_team_ids, linear_default_team_id and linear_project_id
(UUID or explicit null for a team board), with optional linear_workspace_id
(provider-derived UUID or null). Provider is fixed, not a caller argument. Clear
requires expected_revision and optional instance. Revisions use existing bounded
integer rules. Results for all three have exactly workspace_id, project_id,
configured, revision, binding and updated_at. Initial revision=0 has null binding
and updated_at. Binding, when configured, contains exactly provider="linear",
linear_team_ids, linear_default_team_id, linear_project_id and
linear_workspace_id; the latter two are null when unavailable/not selected.
After clear, binding is null and revision/updated_at retain the mutation record.

Before committing, the native client resolves every selected team and the selected
project, checks project/team membership and displays a preview. The authority
records an authorized administrator's selected IDs; it does not attest Linear
membership without that person's provider connection. Native actions revalidate
current team/project access before exposing or mutating external data. A changed
binding requires a fresh context; fail closed on a changed revision during an
action instead of silently changing its destination.

A team board includes issues in its allowed teams regardless of whether those
issues have a Linear project; create leaves project unset. With a selected project,
every allowed team must belong to that project's verified team set; issue reads
require both an allowed team and that exact project.

## CLI and agent interface

Provide native commands under `clawbridge linear`:

- connect/login, status, profiles and logout, with explicit --profile and access.
- teams/projects browsing with real pagination and stable IDs.
- link/unlink for the current ClawBridge project, with expected revision and an
  explicit team-board option when no project is selected.
- issues/issue, create, update and comment, all constrained to the shared binding.

Exact flag spellings and exported Go seams are published by engineer before
reviewer probes. Help/invalid arguments do not read credentials, connect to a
host/provider, initialize storage or alter config. Personal profile selection is
trusted local configuration/flags, not .clawbridge.toml. A read-only provider or
missing write capability gives an actionable failure rather than elevation.

Discover tools and schemas through tools/list. Implement a curated adapter for
teams/projects/issues/statuses/comments, with known safe operation aliases when
the provider uses create/update versus save forms. Never expose arbitrary tool
names/arguments, executable instructions or arbitrary URLs as a passthrough.
Provider descriptions, issue bodies and comments remain untrusted task data.
Bound and validate JSON/text results; reject incomplete/ambiguous scope metadata.
If team/project fields need resolution, resolve their immutable IDs rather than
comparing a potentially duplicated name. Do not infer absence from a partial page.
Provider compatibility may accept descriptive string team/project labels beside
valid flat immutable IDs; labels never supply or override scope. A nested object
ID must agree with any flat ID. Null-versus-present IDs and malformed numeric,
boolean or array scope fields fail closed. A label without a verified immutable
ID remains insufficient. Bounded integer page sizes may target an advertised
integer or number schema; the actual supplied value remains integral and bounded.
Responses may pair a display `id` with an immutable `uuid`; use the UUID for
identity and retain the display ID only as a label. All supplied immutable ID
representations must agree. A display ID alone cannot establish identity. For
issue reads, negotiate a fixed internal field projection when the provider
advertises a compatible string-array schema. Request immutable identity and scope,
including `projectId`, so an unset project is explicitly null. Omitted project
metadata remains unknown; it must not be treated as an unprojected issue.

Read/list/get actions must establish the selected team/project scope before
returning a body. Get includes available parent/subissues/dependency and assignment
context, with truthful pagination/completeness markers. List accepts bounded limit
and opaque scope-bound cursor; no all-workspace search by default. The workflow
uses the issue description's coordinator assignment plus paged list_sessions to
find collaborators. Ambiguous/stale/retired/foreign-scope bindings escalate.

For writes, permit only new issue creation in the default bound team, explicit
issue description/title/state updates and comments on an already verified bound
issue. State IDs must resolve to that issue's team; preserve other properties.
No delete/team-admin/membership/bulk/attachment upload operation is needed. Do not
change human assignees/delegates implicitly. A proposed ownership change still
needs the coordinator's explicit assignment; Linear updates are not atomic claims.

Every write requires a caller operation_id. Use a private per-connection durable
operation ledger: bind provider profile, ClawBridge scope/actor when present,
mapping revision, operation and canonical payload. Record pending before dispatch;
accepted identical retry returns its recorded receipt; changed payload conflicts.
An uncertain/pending outcome refuses automatic replay and directs read-back.
Bound/atomically protect ledger files and redact them from diagnostics. This is
local retry protection, not exactly-once Linear writes across machines/crashes.
Read-only actions may reconnect boundedly; do not reconnect/retry a mutation
after an unknown response. Body/file arguments are bounded and terminal-safe.

Ledger lookup is keyed by canonical ClawBridge authority/project/actor (if any)
and operation_id, independently of profile, revision or operation type. Those
values belong to the stored binding/payload: changing them with the same ID is a
conflict, never a fresh dispatch. Check the mapping revision immediately before
dispatch. A change after dispatch cannot undo an external effect: preserve its
accepted or uncertain outcome against the old revision, surface binding_changed
with any known receipt, and never replay or claim it affected the new binding.

Expose this same native client through additive local ClawBridge MCP task-board
tools, using the CLI-pinned project and explicit personal profile. This lets an
agent use the user's native connection without installing Orca or copying its
tokens. Keep legacy NewServer wrappers; add options/injected interfaces. Tools
read context, list/get issues, create/update/comment and retrieve the binding.
Names and schemas are finalized with early seam publication. No scope/profile
override comes from a tool argument. Administrative link/clear remain CLI/TUI.
No Linear network call occurs merely because tools are listed. Installing a
separate official Linear MCP remains optional for operations outside this adapter.
For an opted-in native connection, MCP initialization also supplies a compact
workflow brief: read the coordinator's issue/session/revision assignment, page
peer discovery, request cooperation through ClawBridge, and reconcile uncertain
writes instead of claiming ownership or replaying them. This makes the basic
workflow available to an agent without an Orca-provided skill installation.

## TUI setup and task view

Add a Linear screen accessible after normal join and from routine navigation.
Offer Connect Linear / Later after join without making Google join depend on
Linear availability. Existing users reach it directly; ordinary startup never
opens a browser or changes credentials. Preserve the saved ClawBridge route.

The flow is: choose/add personal connection (workspace selected in Linear OAuth),
choose allowed team(s), choose default team, select a Linear project or explicit
team board, preview and confirm linking the current ClawBridge project. Show stable
IDs alongside labels where useful. Team/project paging, empty results and scope
errors are explicit. A selected row does not commit a setting. Owner/lead role is
checked at save; readers retain browse access and clear guidance.

After linking, show issues with state, assigned slice/owner context and repository
scope, plus scrollable details and dependencies. Refresh is read-only; no issue
state, assignment, receipt or task claim follows from inspecting it. A native
connection failure does not erase the shared binding. Distinguish local logout
from project unlink. Cancelling an action or leaving the view releases its
request/context; stale responses cannot repaint a new profile/binding generation.
All provider text is sanitized and width/height bounded under resize/tiny-terminal
conditions. Root keeps the skill's dependency/ownership/acceptance workflow lean.

## Independent acceptance and live test boundary

Reviewer may minimally update version-dependent assertions in older integration
probes for schema 3/newer=4, preserving their historical fixtures, migration,
digest, rollback and fencing checks. Previous accepted source/reports stay dated;
their frozen hashes are not claims about the new L1 tree.

Publish interfaces for provider client, secure credential storage, OAuth callback,
clock/wait, HTTP transport, shared binding and ledger before independent probes.
Tests use fake HTTP/MCP providers, memory credentials, actual in-memory MCP clients,
temporary SQLite and disposable PostgreSQL. No production URL override exists;
injected transports are Go test seams. Cover auth/DCR/PKCE/refresh/rollback,
scope/pagination/role fences, old/new migrations, no-stamp gets, concurrency and
uncertain write outcomes, no credentials in files/argv/output, TUI lifecycles and
real CLI/MCP routing. Capture source/module/fixture freezes before full/race/vet
and relevant cross-builds. Prior coordination heavy gates are historical baseline.

Live testing may read metadata and create at most six clearly named
`[ClawBridge test]` issues solely in the live test team. Use no project unless one
is explicitly discovered/created for this test. May comment/update only those
test-owned issues, including a simulated frontend/backend/review dependency and
handoff. Do not alter existing issues, team settings, memberships, credentials,
or real agent execution permissions. Keep a private exact write/read-back log;
archive/cancel only test-owned fixtures if supported, preserving truthful evidence.
No deployment/publication or unrelated remote host testing is authorized here.

One bounded public OAuth-start smoke may discover the fixed official metadata,
register one unauthenticated public client and validate its PKCE authorization URL
before immediate cancellation. Use memory credentials and no browser, consent,
code exchange or automatic retry. This establishes only discovery/DCR/start and
listener cleanup, not an authorized account or authenticated MCP session.

The E2E tester validates native build behavior and owner/peer discovery with fake
or explicitly bound test actors, then independently reviews evidence. Borrowed
Orca access is labeled test-only. Synthetic OAuth success does not establish an
actual owner's browser consent; a test-only authenticated relay does not prove
native live transport. If safe native token handoff is unavailable, still finish
the native path and report that narrow live boundary without claiming it passed.

Sources: [official Linear MCP](https://linear.app/docs/mcp),
[OAuth/PKCE](https://linear.app/developers/oauth-2-0-authentication),
[projects and multi-team membership](https://linear.app/docs/projects).

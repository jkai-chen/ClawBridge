# Linear migration workflow — L2 contract

Status: **approved L2 implementation contract, 22 October 2026**. The owner approved
all five core extensions. This does not
change the approved [L1 contract](linear.md), the deployed schema-3 service, or a
saved board mapping. Requested by a user project's coordinating session on 22 October 2026 in
`CLAWBRIDGE-linear-needs-from-migration.md`. No Linear objects are created by this
design review.

## Required workflow

The Platform and AI workstreams share one Linear team, the live test team. They
coordinate across several Linear projects while retaining separate code and
repository ownership. A Linear team, a workstream and a ClawBridge project remain
different concepts. Labels, assignees and issue text do not grant code access,
release another agent's slice, or establish human approval.

| Migration need | L1 behavior | Approved L2 change |
| --- | --- | --- |
| Several projects under one team | Team board reads all allowed-team issues; a project binding selects one project. | An explicit allowed project set; create/update selects a verified project UUID. |
| Team/domain/kind/source labels | No label writes. | Explicit label-ID selection on create/update; no label creation/deletion. |
| Blocks, blocked by and related | Linked context can be read; no relation writes. | One explicit relation add/remove between two in-scope issues. |
| Parent and sub-issues | Readable context; no parent writes. | Explicit parent UUID on create/update, or explicit clear on update. |
| Human assignment | Existing assignees are preserved. | Explicit assignee UUID/set or clear, with target-team membership verification. |
| Artifact and PR links | Description Markdown already supports URLs. | No additional operation required. |
| 50–100 initial issues | Individual writes with operation IDs. | Keep one operation ID per issue; no batch transaction required. |
| Milestones | No milestone actions. | Optional later increment; use gate issues meanwhile. |
| Separate migration and test work | Team board also includes unprojected test issues. | Select the migration project set; optionally filter by a verified migration label. |

The approved implementation scope is the first five rows. URL attachments,
milestones and a batch API remain optional and excluded from that increment.
Project and label creation remain in Linear's UI; consuming existing IDs does
not authorize new administrative tools.

## Mapping and compatibility

Retain two explicit mapping modes: **team board** and **project set**. Team-board
semantics stay unchanged. A project set contains bounded, unique immutable project
UUIDs plus the existing allowed teams and default team. Verify every project and
its team memberships through the selected personal connection. Require each
selected project to intersect the allowed teams and every selected team to be
represented. An issue must match an allowed team, an allowed project, and that
verified project/team pair. A null or unresolved project is outside a project set.

Do not choose the first project implicitly. With multiple projects, creation
requires an explicit `project_id`; updates preserve the project unless explicitly
changed. Destination projects and effective issue teams must remain in scope.
This also excludes unprojected ClawBridge fixtures from the migration mapping.

The exact L1 six-field/five-field wire shapes cannot silently acquire new fields.
Add a versioned mapping interface and a transactional schema migration, preserving
existing mappings, revisions, tombstones, receipts and operation digests. Existing
clients retain the L1 representation for representable mappings. A client that
cannot represent a project set receives an explicit unsupported-mapping error;
it must never receive a fabricated team board or one arbitrarily selected project.
The addendum below defines the versioned operations; publish test seams before
dependent probes. Composite
pagination must bind the profile, mapping version/revision, project/team position
and filters; empty pages never establish exhaustion.

Every mapping change still requires owner/lead rights, preview, explicit confirm
and the original expected revision. Personal profiles and credentials stay local.

## Mutation rules

- Omitted fields remain unchanged; explicit null clears only fields documented
  as clearable. Validate canonical IDs and current scope before dispatch.
- Labels resolve to the target issue's allowed team. Workspace labels (explicit
  null team) listed for that team are allowed under the same eligibility proof
  as team labels (owner-approved); unscoped and other-team labels are refused.
  Label replacement must be
  explicit; never disguise replacement as an atomic merge or silently overwrite
  unrelated human labels.
- Normalize `blocked by` into a directed `blocks` edge; normalize symmetric
  `related` pairs consistently. Reject self-relations. Both endpoints must be
  authorized before mutation; out-of-scope linked bodies remain undisclosed.
  Add/remove is one edge per operation ID, without a multi-edge atomicity claim.
  Never implement this by replacing complete relation lists from a stale read;
  refuse an unsupported safe edge operation.
- Parent and child must both be in scope. Respect provider hierarchy constraints
  and verify effective team/project context; setting a parent must not silently
  inherit an unapproved destination. Supported rule (owner-approved): a parent
  must share the child's effective team and project, so any inheritance is a
  no-op; explicit parent clear is allowed.
- Assignees must resolve to canonical users with verified membership in the
  target issue's team. No name-only matching, automatic `me`, implicit assignment
  from a label, or assignment inferred from peer/task text. Explicit clearing is
  separate from omission. Human assignment is not an agent claim or approval.
- Preserve all fields outside the explicitly requested mutation. Expose provider
  validation failures and concurrent/partial outcomes honestly; do not promise a
  cross-provider transaction or compare-and-swap the provider does not support.

Extend the existing ledger's canonical payload with every new semantic field,
including mapping version/revision, destination, labels, relation direction,
parent and assignee. Preserve omitted-versus-clear intent. Identical accepted
retries return the known receipt after current authority checks. Changed intent
conflicts; pending/uncertain outcomes require read-back, with no new-ID bypass.
Known receipts survive post-dispatch context changes and persistence failures.

## Native capability gate and acceptance

Discover actual official MCP capabilities before committing to an adapter shape.
Negotiate only curated typed fields/tools; no arbitrary tool, GraphQL, URL or
credential passthrough. If the official interface cannot prove team membership,
scope, relation semantics or the requested operation, fail explicitly and record
the missing capability. Do not use Orca or a broader profile as a runtime fallback.

Use synthetic OAuth/provider/HTTP peers for scope, alias/type/null conflicts,
pagination, concurrent mapping changes, permission revocation, ledger replay and
response-loss tests. Exercise migration/rollback and legacy compatibility in
SQLite and real disposable PostgreSQL, then the affected CLI/TUI/MCP workflows.
Freeze functional bytes before final full/race/vet/cross checks.

Native write consent and a new bounded live-test plan are separate owner actions.
The existing personal read profile and exhausted prior fixture budget do not
authorize new writes, ten project creations or a migration-wide issue import.
Until L2 is accepted, that session can use its authorized Linear tooling for setup
and relations while ClawBridge supplies messaging and L1 task inspection.

## Exact implementation addendum

### Authority and migration

Schema4 transactionally adds
`mapping_version INTEGER NOT NULL DEFAULT 1 CHECK(mapping_version IN (1,2))`
to `taskboard_bindings`. Do not rewrite existing mapping JSON, revisions,
tombstones or historical operation payloads. Preserve existing SQLite/PG locking,
rollback and newer-schema refusal. The service must be upgraded before remote
V2 use; an old-server refusal must never trigger a mirror or credential fallback.

Add `taskboard_get_v2`, `taskboard_set_v2`, `taskboard_clear_v2`. Reuse the existing
no-stamp `taskboard_access` permission result. Get accepts only optional instance;
clear accepts expected_revision and optional instance. Set requires
expected_revision, mode, linear_team_ids, linear_default_team_id and
linear_project_ids; optional instance and nullable linear_workspace_id. Caller
provider, provenance, profile, timestamp and credentials remain forbidden.

V2 results have exactly workspace_id, project_id, configured, revision,
mapping_version, binding, updated_at. The returned mapping_version is 2. Initial
state has revision0, configured=false and null binding/updated_at. A configured
binding has exactly provider, mode, linear_team_ids, linear_default_team_id,
linear_project_ids, linear_workspace_id. Provider is fixed `linear`; mode is
`team_board` or `project_set`. Team board requires an empty project array; project
set requires 1–50 projects. Teams remain 1–20, unique, with the default in the set.
Sort canonical UUID sets; duplicates, wrong types and null arrays reject.

V2 get normalizes stored V1 without writing; V2 set/clear stores version2 and uses
the same CAS revision/tombstone lineage. V1 keeps its exact arguments and closed
six-/five-field results. V2 team boards and singleton project sets can be
downconverted exactly. V1 get/set/clear on a configured multi-project mapping
returns definitive `unsupported_mapping` (HTTP409) after authorization, before
mutation. A cleared tombstone remains representable.

### Native surface and destinations

New binaries use V2 authority for native board commands; old wire operations and
legacy service/MCP entry points remain available. Do not add an L2 installation
opt-in. `linear link` accepts repeated `--linear-project UUID` (up to 50), mutually
exclusive with --team-board. Preview and confirm retain the original revision.

Create/update add --linear-project, repeated --label (up to 100), --parent and
--assignee. Update adds explicit --clear-project, --clear-labels, --clear-parent,
--clear-assignee; set/clear conflicts reject before I/O. A label replace or clear
on update also requires repeated --expect-label UUID or --expect-no-labels. Create alone accepts
--team. Global --project before the command still means the ClawBridge scope;
--linear-project always means the provider destination. One selected project may
be the implicit create destination; multiple projects require an explicit one.
The default team is used only when its verified destination pair is valid. Do
not select another team silently, and do not add update team moves.

Team-board creation remains unprojected. Non-null destination writes require a
project-set mapping. Updates otherwise preserve their current project; explicit
null project is permitted only in team-board mode. Parent and assignee may be
explicit null on create/update; a provider unable to express that intent must
refuse it before dispatch. Omitted values stay absent from provider mutations.

Add `linear labels --profile ID --team UUID`, `linear members --profile ID --team
UUID` and `linear relation add|remove FROM TO --type blocks|blocked-by|related
--profile ID --operation-id ID`. Reads use limit 1–100/cursor; membership discovery
must be scoped to a verified allowed team, never a global name search.

Keep all seven L1 taskboard tools unchanged. A V2-capable configured service adds
eleven tools: taskboard_binding_v2, taskboard_context_v2, taskboard_issues_v2,
taskboard_issue_v2, taskboard_create_v2, taskboard_update_v2,
taskboard_comment_v2, taskboard_labels_v2, taskboard_members_v2,
taskboard_relation_add_v2, taskboard_relation_remove_v2. Default constructors stay
at14 tools; existing V1-only injected adapters need not implement V2. A fully
configured native adapter has32. Listing/initialization performs no Linear I/O.
Publish exact closed schemas and Go seams before dependent reviewer probes.

### Presence, related context and concurrency

Use separate typed V2 inputs and an explicit Present flag. Do not extend legacy
digest structs in place. Label arrays are unique sorted IDs: omission preserves,
present[] clears, null rejects. Replacement must be explicit and cannot silently
remove unrelated human labels; if canonical existing-label context or safe
provider semantics is missing, refuse that update. Supplying labels declares the
complete desired replacement set, including removal of known labels left out;
CLI/tool help must make that consequence clear. Do not turn an unspecified label
change into replacement. An update that supplies labels must also supply
`expected_label_ids`, the complete set the caller last read (unique sorted IDs;
`[]` means none). It is invalid without a label replacement and on create. The
current canonical set must equal it at preflight and again on the fresh read
inside the operation lock; a mismatch inside the lock is a terminal
refused-before-dispatch result for that operation ID. The expectation is part of
the ledger payload digest and never alters the replacement set. No group or
archived label, and no label without supported eligibility proof, may be newly
applied; a workspace label needs the same proof as a team label. On native
Linear, whose label rows carry no team or group field, the proof is the
owner-approved "label eligibility by filter" capability: an inference from
Linear's documented `list_issue_labels` filters, pinned by schema (a string
`team`, and boolean `includeArchived`/`includeGroups` defaulting to false, with
their documented meanings and the documented `retiredAt` rule). Under it the
provider sends both filters as false explicitly; a returned row is eligible for
the queried team unless it carries a non-null `retiredAt` or `archivedAt`, an
absent `retiredAt` means not retired, and a group row contradicts the filter
and fails the page. Such rows are filter-scoped, never given a fabricated team;
a label is applicable only from the filtered list of the issue's own team, and
in issue filtering a label listed for several allowed teams spans them. Any
schema drift disables the capability.

Native `get_issue`/`list_issues` return an issue's labels as names only. On the
label write paths (preflight and the in-lock refresh) and on the opt-in resolve
read, the owner-approved "label membership proof" turns names into IDs: walk the
team's complete label listing (`includeArchived` and `includeGroups` true); each
issue label name must match exactly one canonical label; then `list_issues`
filtered by that label UUID (team, project when set, `includeArchived` true, at
most 4 pages of 250) must return the issue. k names with k distinct confirmed
IDs make the set known. A duplicate or missing name, a non-UUID candidate, an
unconfirmed label or an exceeded bound leave it unknown, so replacement is
refused. The capability pins `list_issues.label` (a string documented as "Label
name or ID"); only UUIDs are sent. Plain reads keep names with `labels_known`
false. Membership must prove the canonical user belongs to
the effective issue team; reject known inactive users and require supported
eligibility proof rather than treating unknown as active.

Expose canonical label, parent and assignee IDs with explicit knownness. Unknown
is not null/absence. Parent traversal is bounded to 64 ancestors; reject self/cycle,
incomplete or inaccessible required ancestry. A parent must share the child's
effective team and project: that structural rule is the destination-preservation
proof, so no provider schema or description is trusted for it. The board verifies
it at preflight and again inside the operation lock, then marks the provider
input; only that board marker lets the provider send a parent, and no wire
field, schema, flag or environment can set it. Cross-team and cross-project
parents are refused. Explicit parent clear is allowed (an owner-approved live
probe showed set and clear preserve team and project). Native Linear returns
`parentId` as the parent's display identifier, not its UUID; issue reads keep it
as a known parent identifier, ancestry reads resolve it with get_issue, cycles
are detected on both forms, and a mutation receipt's parent must match the
request (UUID, or the verified parent's identifier). A parent's `updatedAt` moves
when a child is attached and is never used as a staleness signal. No automatic
hierarchy repair or implicit move.

Add-existing/remove-absent may return `observed_unchanged` without a provider write
only from authoritative edge evidence (complete context is required to prove
absence). The receipt describes an observation, not a provider CAS guarantee.
This remains a mutation-intent request: require current ClawBridge write rights
and a write-capable personal profile even when no provider write is necessary.
An already accepted identical ledger retry retains the existing no-provider-I/O
receipt path.
Otherwise require a genuine one-edge operation. An edge receipt records normalized
action/type/endpoints and an optional real provider edge UUID, never a fabricated
ID. Caller-supplied arbitrary edge IDs are not accepted. Unknown outcome retains
pending and requires read-back; never compensate or blindly repeat.

The observed official `save_issue` schema also provides a safe curated form:
`id` plus one UUID in `blocks`/`relatedTo` appends that edge, and
`removeBlocks`/`removeRelatedTo` removes that endpoint. Negotiate those typed
fields explicitly and send exactly one endpoint. This is an edge operation,
not permission to read and replace the existing relation arrays.

Composite cursor version2 is bounded to8192 bytes and binds personal profile,
canonical ClawBridge scope, mapping version/revision, sorted mapping/filter digest,
verified pair index and provider cursor. Traverse sorted pairs with one provider
page per action, including empty continuations. No unbounded hidden scan or
provider snapshot guarantee: issues moving concurrently may repeat across pages.
Contradictory duplicate identities within a page fail rather than choose a body.

The ledger namespace remains authority/ClawBridge project/actor/operation_id,
independent of operation, profile, mapping revision and payload version. New V2
intent hashes all fields and normalized presence/null semantics. Legacy-only
intent in a representable mapping must retain the exact legacy canonical payload
and receipt path, including when invoked by a new CLI. Test real serialized L1
accepted and pending records; a payload-version field must not invalidate an
otherwise identical historical retry. Any newly supplied L2 intent with that ID
conflicts. V2 issue/comment/edge receipts are discriminated; legacy receipts remain
readable. Existing permission, pending-before-dispatch and known-receipt rules
remain mandatory.

The TUI adds explicit multi-project linking and displays verified L2 context with
unknown values labelled as unavailable. It must preserve its existing source/test
injection compatibility, original-revision confirm, cancellation and resizing.
Agent mutations are provided through the CLI/MCP; a complete new TUI issue editor
is not required by this increment.

### Implementation and verification boundary

The engineer may implement this approved contract and publish additive seams;
the reviewer owns independent probes and a new L2 review record. Minimal current
test version assertions may advance supported schema3->4/newer4->5 while keeping
historical fixtures, digests, rollback tests and reports intact. New schema3
fixtures must be attributed to the actual prior L1 source. Final heavy gates wait
one stable source freeze; changes in reviewer inputs are captured independently.
Real native capabilities and a bounded write-consent/live-test plan remain
separate from synthetic acceptance. Do not mutate migration tasks or broaden the
live board mapping merely to satisfy a probe.

## Owner decision

The owner approved all five core extensions on 22 October 2026: project sets,
labels, relations, parents and explicit assignment. These are flagged additions to
L1. URL attachments, milestones, a batch API and project/label administration are
not included. The installed L1 service remains unchanged until the new contract,
implementation, acceptance and separately authorized rollout are complete.

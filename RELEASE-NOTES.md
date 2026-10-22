# ClawBridge v0.2.0

Adds native Linear task boards, structured coordination and controlled inbox
notifications while keeping normal messaging independent of those integrations.

- Connect Linear through native public-client OAuth/PKCE or explicit token import
  from stdin. Personal credentials stay in the native keyring; no Orca or Node
  runtime is required. Read access is the default and write access is explicit.
- Link a ClawBridge project to verified Linear teams and a set of Linear
  projects. Team boards work without creating a project. Each teammate connects
  their own account; switching accounts does not change the shared mapping.
  Native responses retain display labels while routing by immutable UUIDs;
  issue reads request explicit scope fields before exposing task content.
- Browse setup and issues in the Linear TUI view. Native CLI and pinned local MCP
  tools list/get issues and explicitly create, update or comment within the
  selected board. Inspection never claims a task or changes its assignment.
  Local operation records prevent blind replay of uncertain external writes.
- Versioned task-board tools add explicit destinations, label replacement,
  one-edge dependencies, parents and assignment. Each action requires verified
  scope and provider support. Native label eligibility and parent-preservation
  proof remain limited; unsupported actions fail before dispatch. These fields
  never claim a task, grant permission or substitute for coordinator ownership.
- Optional issue/revision/intent message metadata links requests, acknowledgements
  and progress while preserving legacy retry identity. Coordinator assignments
  remain authoritative; a peer receipt does not confer human approval or close work.
- Recipient notification policy supports accept, hold and refuse. Suppression
  retains unread mail and pending notification progress for a later accept.
- Authority-only inbox metadata leaves delivery/read/presence/audit unchanged.
  Explicit MCP resource subscriptions receive invalidations; the host decides
  whether to start an agent turn.
- The foreground JSON/Codex watcher queues only a fixed inbox-check hint to an
  explicitly selected existing thread. Private checkpoints bind authority,
  recipient, instance, executable, host home and thread. Queue acceptance remains
  separate from processing; crashes or ambiguous outcomes can duplicate hints.
- Bounded `host-status` and `host-wait` observe an explicitly selected existing
  Codex Unix WebSocket endpoint without starting or resuming a thread. Idle and
  unloaded are distinct from task completion, drained queues or process exit.
- Current guides, design contracts and dated review evidence have separate homes
  under `docs/`. The agent skill covers paged ownership discovery, cooperation,
  repository-qualified slices and explicit handoffs.

Schema 3 adds shared board settings after the schema-2 coordination migration;
schema 4 adds versioned project-set mappings while preserving old mapping data.
Back up the existing store before upgrade and deploy matching clients/service.
An older service rejects the new operations; installing a client does not upgrade
that service. Older binaries refuse newer schemas; rollback restores a verified
pre-upgrade backup rather than editing the version marker.

OAuth consent, keyring availability and native host behavior depend on the user's
environment. Automated providers and authenticated test relays establish their
stated test boundaries; they do not substitute for first-login consent or prove
all real provider schemas. No webhook service, background model launcher,
automatic task claim or production deployment is included.

Assets retain five CGO-disabled targets, installers, selected checksums and a full
source marker. Binaries are unsigned; macOS binaries are not notarized. Checksums
verify integrity against the manifest rather than an independent signature.
Native macOS OAuth consent and board reads were exercised against Linear.
New L2 mutation effects remain unverified; local automated acceptance uses
isolated providers and disposable PostgreSQL.
Windows checks cover profile privacy, redirected input and a synthetic keyring
item; account OAuth and notification host transport retain separate platform limits.

See docs/linear.md, docs/notifications.md and docs/reference/api.md for setup and
behavior. Dated acceptance records and their exact limits live under
docs/archive/reviews/.

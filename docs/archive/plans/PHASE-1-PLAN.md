# ClawBridge Phase 1 implementation and review plan

Status: local Phase 1 implemented on 20 October 2026; independent automated acceptance passed.
Installed-client conformance scope and remaining limits are recorded in REVIEW.md
and docs/CLIENT-CHECKS.md. Hosted phases are not implemented.
Source: `/Users/oriixchan/Dev/clawbridge/DESIGN.md`, draft dated 20 October 2026.
This plan supplements that design. Later phase choices and paid deployment still
require their relevant decisions; the owner authorized continuous local Phase 1 work.

## Outcome and scope

Deliver a usable local bus: two agent processes in the same project can register,
discover each other, exchange threaded messages, see delivery and read state, and
retire with a successor. The CLI and stdio MCP expose the same application behavior.
The same session name in a different project cannot expose or receive that project's
messages by accident.

Phase 1 proposes the local-only mode described in sections 8 and 14. Its SQLite
database is the authoritative writable store for an explicitly local workspace.
The future cloud workspace's SQLite mirror remains read-only to application commands.
An unreachable cloud workspace must never silently become a writable local bus.

Cloud deployment, paid resources, the Bubble Tea TUI, push delivery, terminal nudges,
cross-project link administration, importers, and team invitations belong to later
phases. Preserve extension points for them without reporting unimplemented commands
as successful. Cross-project addresses must fail clearly until links are supported.

## Recorded decisions

1. Phase 1 with local-only mode in v1 is approved.
2. Go is approved: one binary, the official Go MCP SDK, and
   `modernc.org/sqlite`. Bubble Tea and Lip Gloss are the proposed Phase 3 TUI stack.
3. Choose licence and repository visibility before publishing or distributing.
   Existing local work does not require inventing a module host or creating a remote.

Google Cloud project, region, unattended-agent service accounts, and deployment
spending decisions are not needed to design this local phase. Retention defaults to
the design's proposed 90 days, is configurable, and must be shown when a workspace
is initialized rather than presented as an owner-approved value.

## Suggested coworker responsibilities

The owner authorized cooperation; these responsibilities are now in progress.

| Coworker | Responsibility | Reviewable output |
| --- | --- | --- |
| `cb-designer` | Maintain behavior contracts and resolve design ambiguities with the owner | This plan and explicit decision records |
| `cb-engineer` | Implement approved local core, CLI, MCP, and installers | Small changes with migration and command documentation |
| `cb-tester-reviewer` | Independently verify scoping, state transitions, and installation behavior | Reproductions and acceptance results with remaining gaps |

All three current terminals share this checkout. Agree on file ownership before
concurrent edits; avoid competing index operations or commits. Keep changes
reviewable on the existing branch until another checkout is explicitly needed.

## Application boundaries

The first working milestone is two sessions registering and exchanging, replying,
and inspecting delivery state through the CLI. MCP and installation follow against
the same application service, then the remaining Phase 1 behavior is verified.

Refinements agreed with the owner: immutable session identity separate from name
and process instance; explicit restart and stale-session behavior; routing roles
never grant permissions; operation IDs make online retries idempotent; separate
thread and reply-to IDs; stable pagination cursors; retirement never silently
forwards mail. Validate cloud identity before the later full TUI, even if the
first deployment is a single Compute Engine VM rather than Cloud Run/Cloud SQL.

Use a single application service for validation, addressing, session permissions,
message delivery, and history. CLI and MCP should translate input and output,
not implement separate policy paths. Keep the store behind that service so the
later server can use Postgres without changing command semantics.

Create an immutable process context containing workspace ID, project ID, session
identity when required, principal, and storage mode. Project selection resolves
before dispatch and remains pinned for an MCP process's lifetime.

Local principals describe the operating-system user and must be labelled as local,
unverified identity. They cannot claim Google verification or security against
another process running under the same OS account. Cloud mode later derives the
principal from the verified token and authorizes each request on the server.

The authoritative local service supplies timestamps. Future cloud writes use the
server clock. Neither path accepts caller-supplied audit timestamps or principal
overrides. A peer message cannot grant permissions or record human approval.

## Storage corrections to the draft schema

Resolve these in the first schema migration rather than leaving enforcement to
individual command handlers:

- Give projects an immutable ID and enforce slug uniqueness within a workspace.
  Every lookup includes workspace scope, even if the initial UI uses one workspace.
- Session references use the composite identity `(project_id, session_name)`.
  Session name alone is never a foreign key or authorization identity.
- Distinguish a member's authorization role from a session's routing role.
  Define an explicit session role field for `@role:lead`; human project membership
  and the session's descriptive `role_text` must not be conflated.
- Record both source and destination project identity for messages, consistent with
  the draft's destination `project_id` and cross-project source field. This permits
  source and recipient session references to remain unambiguous when links arrive.
- Deliveries need `recipient_project_id` alongside `recipient_session`. Enforce a
  unique `(message_id, recipient_project_id, recipient_session)` and the composite
  session reference. Readers always filter by their bound recipient identity.
- Thread references must not allow a caller to read or reply through another
  project's message ID. Validate participation and scope before returning content
  or allocating a delivery. Keep reply ancestry well-defined for later linked
  projects rather than assuming every thread has only one project.
- An individual session is the initial sender. Role and broadcast addresses expand
  transactionally into explicit deliveries for active sessions at send time.
  New sessions do not inherit old broadcast deliveries.
- Append the corresponding audit event in the same transaction as each audited
  mutation. The application exposes no operation to edit or delete history.
- Enable SQLite foreign keys for every connection, bounded contention handling,
  and short transactions. A failed send leaves neither a partial message nor a
  partial set of deliveries.

## Command and MCP behavior

| Operation | Required behavior |
| --- | --- |
| Initialize local workspace | Explicit local mode, workspace identity, retention shown, configuration written without overwriting unrelated settings |
| Resolve project | Explicit project selection, otherwise repository-root config, otherwise normalized git remote; unknown or ambiguous projects require explicit selection or initialization |
| Register session | Unique name within the project; conflicting existing ownership is an error; repeated registration by the same bound identity has documented idempotent behavior |
| List sessions | Current project only, with status and routing-role filters |
| Send | Validate destination, sender, kind, thread, body, and secret patterns before committing; return message ID and delivery state |
| Inbox | Current session only; online/local-authority reads mark delivered, not read; bounded limit and stable chronological ordering |
| Mark read | Only the caller's deliveries; transactionally set delivery if needed and read timestamps; repeat calls preserve the original timestamps |
| Reply | Validate participation, resolve the original sender as recipient, preserve the thread, and return the new message ID |
| Update session | Own session by default; lead/owner authority must be checked before changing another session |
| Retire session | Validate successor in the same project, prohibit self-successor and successor cycles, preserve session identity and history |
| History | Current project only, filterable by entity and key, bounded and ordered |
| Whoami | Workspace, project, session, principal provenance, storage mode, and result source |

Reads and writes report their source explicitly, using a stable machine-readable
field. Proposed source values are `local-authority`, `server`, and `offline-mirror`;
the latter two become usable in Phase 2. Mirror reads must never mark delivery or
read state locally. A failed cloud write returns an actionable error and creates no
queue for replay.

The sender needs an explicit way to inspect delivery/read status for messages it
sent, such as a scoped message-status command/tool. Sending successfully must not
be represented as the recipient having read the message. Such inspection exposes
only the sender's messages and their delivery metadata.

Project fallback matches a registered normalized git remote; it does not invent a
slug or silently create a project. Preserve remote path distinctions and case
where the host requires them. Multiple matching registrations require selection.
Worktree config discovery follows the repository root for the current checkout.

Session identity must be supplied explicitly by CLI configuration/flag and MCP
configuration, never guessed from the active terminal. Commands that need a session
fail with instructions if it is absent. Installer instructions must explain how to
give each agent a distinct name.

## Implementation order

1. Copy the owner's design into the working branch with its provenance, keeping
   unresolved decisions visible. Add basic build/run instructions after Go approval.
2. Implement context resolution, explicit local workspace/project initialization,
   schema migration, storage mode separation, and structured errors.
3. Implement session lifecycle and append-only audit writes through the service.
4. Implement addressing, transactional sends, scoped inboxes, delivery inspection,
   replies, and mark-read semantics.
5. Expose the service through CLI verbs and stdio MCP. MCP protocol traffic is the
   only stdout output; diagnostics go to stderr. Establish actual two-client use.
6. Add Claude Code and Codex MCP installation. Inspect the installed formats and
   official current documentation before writing config. Preview changes, preserve
   unrelated entries, back up existing files, and verify repeat installation.
7. Ship the provider-neutral agent skill and run the acceptance matrix below.
   Skill instructions cover inbox checks, threads, handover, provenance, and offline
   failures. Hooks and terminal adapters remain Phase 4.

## Acceptance matrix for independent review

| Area | Evidence required |
| --- | --- |
| Project scoping | Two projects each contain `lead`; discovery, reads, writes, replies, history, status, and mark-read never cross scope |
| Workspace scoping | The same project slug in two workspaces remains distinct |
| Resolution | Explicit selection precedence, worktree roots, remote URL variants, missing configuration, unknown remote, and ambiguous matches |
| Pinned context | Changing cwd or repository config after MCP startup does not change the bound project |
| Session authority | Own-session mutation succeeds; ordinary members cannot mutate peers or adopt a conflicting session; permitted lead changes are audited |
| Addressing | Individual, role, and broadcast delivery are deterministic; unsupported cross-project addresses fail without writes |
| Transactions | Rejected input or a failed delivery insertion leaves no partial message, delivery set, or success audit event |
| Concurrent use | Two processes register/send/read without lost deliveries or misleading success on database contention |
| Delivery state | Sending, fetching an inbox, and marking read produce distinct observable states; repeats retain original stamps |
| Threads | A reply reaches the correct sender; a foreign message ID cannot leak content or create a reply |
| Retirement | Valid successor is recorded; self, missing, cross-project, and cyclic successors fail |
| Identity and clocks | Caller overrides cannot falsify principal/audit time; local identity is explicitly unverified |
| Input safety | Known example secret patterns are rejected with errors that do not echo the secret; ordinary code examples and bounded text remain usable |
| Storage modes | Local-authority writes work; a cloud/mirror context never falls through to writable local-authority behavior |
| MCP | Claude Code and Codex can discover tools, register distinct sessions, send, fetch, reply, and mark read using the same local bus |
| Installation | Existing unrelated config survives, repeat installation is safe, and invalid config fails without replacement |
| Operator feedback | CLI failures exit nonzero; MCP failures are protocol errors/results as appropriate; every successful result identifies its source |

Tests should use isolated temporary data/config paths and contain no real secrets.
Phase 1 does not need a Postgres container or a GCP project. Phase 2 adds real
Postgres integration and authenticated server tests before cloud use.

## Exit gate

Phase 1 is ready when the documented local workflow works from both CLI and MCP,
the independent review has evidence for the acceptance matrix, and remaining gaps
are stated plainly. Obtain the owner's Phase 2 approval before implementing that
phase. Creating billable resources still requires a separate concrete cost and
region confirmation as specified in the source design.

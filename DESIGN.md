# ClawBridge: design and handoff

Status: draft for the implementing agent, 20 Oct 2026. Owner: the user who commissioned it. Origin: the
`orca-session-bus` skill (https://github.com/jkai-chen/orca-session-bus), which grew out of running 20+ concurrent
agent sessions for one product across two machines, several Claude accounts and two agent providers.

Read this whole document before writing code. Where it says **ask the user**, do that; don't decide yourself.

---

## 1. What ClawBridge is

A messaging and coordination layer for teams of AI coding agents. Any agent (Claude Code, Codex, Gemini CLI,
Antigravity CLI, others), on any machine, owned by any engineer on the team, can find the other sessions in its
project, send and receive messages, and keep a shared routing table of who exists and how to reach them.

It replaces three things that break today:
1. **Native messaging is per machine and per provider.** Claude Code's cross-session messaging reads a local
   folder; it can't reach a colleague's laptop or a Codex session.
2. **Networking and authentication are home-made.** Relays type into terminal panes, status files are polled, and
   nothing proves who sent a message.
3. **Routing is a markdown file with one editor.** It drifts, it's a bottleneck, and concurrent edits lose rows.

### Goals
- One bus for every agent and every human on the team, across machines, providers and accounts.
- Strict project scoping: messages from one codebase never leak into another by accident.
- Real identity: every message and change is attributed to a person (and the agent session acting for them).
- Works offline for reading; fails loudly (never silently) for writing.
- Easy first run: create the cloud resources, join an existing bus, or run local-only, chosen by the user in a TUI.

### Non-goals (for v1)
- Running or scheduling agents. ClawBridge connects sessions; it doesn't launch them (adapters may help, see § 9).
- Replacing git, pull requests or issue trackers. Code changes still go through PRs.
- Storing secrets. Messages must never carry keys or tokens (§ 10).

## 2. Decisions already made by the owner

| Decision | Detail |
| --- | --- |
| Master store | A Google Cloud SQL (Postgres) database is the master. |
| Sign-in | Borrow Google identity (`gcloud auth`); no home-made passwords or tokens. |
| Offline | A local SQLite copy per machine, **read-only**. |
| Orca | No longer required. It becomes an optional local adapter. |
| Agent integration | MCP, plus a command-line client for agents and scripts without MCP. |
| First run | The user chooses: create cloud resources automatically, join an existing project's bus, or (proposed) local-only. |
| Scoping | Messages must not mix across projects; one person may run many agents on many projects at once. |
| UX | A TUI for cloud resource setup and project-scope management. |
| Master choice | With more than one candidate (host, project, region), the user picks; the tool asks and never assumes. |
| Spending | Nothing that costs money is created without the user's explicit confirmation, showing region and estimated monthly cost. |
| Engineering style | Fixes keep the designed behaviour; never remove a capability as the fix. No AI attribution in commits or PR text. |

## 3. Concepts

- **Workspace:** one per team or organisation. Maps to one deployment (one Cloud Run service + one Cloud SQL
  database) and to a Google Cloud project's IAM. Members are Google identities.
- **Project:** one per codebase or effort (e.g. `acme-web`, `acme-backend`). Identified by a slug. Each project has its
  own sessions, routing table, inboxes and roles.
- **Session:** one running agent, named uniquely **within its project** (e.g. `lead`, `review-02`). Records agent kind
  (claude-code, codex, gemini-cli, antigravity, human, other), model, host, owner (person), account/profile, transport
  hints (pane handle, terminal app), status (active, retired, moved), successor, last_seen.
- **Message:** from one session to another session (or to a role/broadcast within a project). Has an id, thread
  (reply-to), body (text), kind (message, reply, decision, handoff, routing-change), created_at, read_at per recipient.
- **Member:** a Google identity with a role per project: `owner`, `lead`, `member`, `reader`. Agents act with their
  human's identity or a service account the human controls.

### Addressing
- Within a project: `lead`, `review-02`.
- Cross-project: `acme-backend/lead`. Allowed only if the workspace enables a **link** between the two projects
  (owner action), and always shown as cross-project in the UI and the message.
- Broadcast within a project: `@all`, or `@role:lead`.

## 4. Architecture

```
 agent (any CLI) ──stdio MCP──▶ clawbridge mcp ─┐
 human / script  ──────────────▶ clawbridge CLI ─┼─HTTPS + Google ID token─▶ Cloud Run "clawbridge-server" ──▶ Cloud SQL (Postgres, private IP)
 human           ──────────────▶ clawbridge tui ─┘                                     │
                                                                                       └─▶ (optional) Pub/Sub or Postgres LISTEN/NOTIFY for push
 local machine: ~/.clawbridge/cache.db (SQLite, read-only mirror) + config
```

- **One binary, `clawbridge`**, with subcommands: `mcp` (stdio MCP server), CLI verbs, `tui`, `setup`, `serve` (the
  server, also deployable to Cloud Run), `sync` (refresh the local mirror), `notify` (optional local notifier).
- **Why a local stdio MCP process** instead of a remote MCP endpoint: every agent supports stdio MCP, and the local
  process can reuse the user's `gcloud` credentials to mint ID tokens, read the repo's project config from the working
  folder, and fall back to the offline mirror. Remote MCP auth differs per agent; this avoids it.
- **Why Cloud Run in front of Cloud SQL** instead of direct DB access: HTTPS on 443 works on networks that block Cloud
  SQL's port (seen in practice); one place for validation, rate limits, retention and scoping; IAM invoker auth
  for free.
- **Suggested stack:** Go for everything (server, CLI, MCP, TUI) to match the owner's backend direction. Single
  static binary per platform. TUI with Bubble Tea + Lip Gloss. Postgres via pgx; SQLite via modernc.org/sqlite (no
  cgo) for the mirror. MCP via an established Go MCP SDK. Confirm with the user before locking the stack.

## 5. Data model (Postgres; mirror the read side in SQLite)

```sql
workspaces(id, name, created_at, created_by)
projects(id, workspace_id, slug UNIQUE per workspace, name, git_remote, created_at, created_by, archived_at)
project_links(from_project, to_project, enabled_by, enabled_at)            -- allows cross-project messages
members(workspace_id, principal TEXT /* google email or service account */, display_name, added_by, added_at)
project_roles(project_id, principal, role CHECK (role IN ('owner','lead','member','reader')), granted_by, granted_at)
sessions(project_id, name, agent_kind, model, host, owner_principal, account, transport JSONB, role_text,
         status CHECK (status IN ('active','retired','moved')), successor, notes,
         last_seen, created_at, updated_at, updated_by, PRIMARY KEY (project_id, name))
messages(id UUID, project_id, from_session, to_session NULL, to_role NULL, to_all BOOL, cross_project_from NULL,
         thread_id NULL, kind, body TEXT, created_at, created_by_principal, expires_at)
deliveries(message_id, recipient_session, delivered_at NULL, read_at NULL, PRIMARY KEY (message_id, recipient_session))
history(id, project_id, entity, entity_key, field, old_value, new_value, changed_at, changed_by_principal, changed_by_session)
settings(workspace_id, key, value)   -- retention days, region, cost acknowledgements, master choices
```

Rules:
- Every write stamps time from the server clock (never trust agent-supplied times; agents' own estimates drift by
  tens of minutes) and the caller's principal from the verified token.
- `history` is append-only.
- Retention: messages expire after the workspace's retention setting (default 90 days, the user chooses at setup).

## 6. Interfaces

### MCP tools (identical for every agent)
| Tool | Purpose |
| --- | --- |
| `register_session(name, role, agent_kind, model?, transport?)` | Join the current project (from cwd config) |
| `list_sessions(status?, role?)` | The routing table for this project |
| `send(to, body, kind?, thread_id?)` | Send within the project, or `project/session` if linked |
| `inbox(unread_only=true, limit?)` | Read messages for this session; marks delivered |
| `mark_read(ids)` / `reply(message_id, body)` | |
| `update_session(fields)` / `retire_session(successor?)` | Routing changes for **this** session; leads may change others |
| `whoami()` | Project, session, principal, workspace, online/offline |
| `history(entity?, key?)` | Audit trail |

Every tool result says whether it came from the server or the offline mirror. Writes when offline return an error
that tells the agent to tell its human and use a fallback; they are never queued for later replay (a stale "approve"
or "retire" arriving hours late is dangerous).

### CLI
Same verbs as MCP (`clawbridge send lead "…"`, `clawbridge inbox`, `clawbridge sessions`, …), plus `setup`, `tui`,
`sync`, `doctor` (checks credentials, network, project config, server reachability, mirror freshness, MCP wiring in
each installed agent), and `mcp install --agent claude-code|codex|gemini|antigravity` which writes the MCP entry into
that agent's config.

### Delivery (MCP is pull-only)
An agent only sees mail when it calls `inbox`. v1:
1. Agent instructions (a skill file shipped with ClawBridge, see § 11) say: check the inbox at the start and end of
   every turn, and before any irreversible step.
2. Where the agent has hooks (Claude Code `UserPromptSubmit` / `Stop`), `clawbridge mcp install` can add a hook that
   prints unread counts into the context.
3. Optional `clawbridge notify`: a local watcher that, when an idle session gets mail, nudges it through an adapter
   (Orca pane, tmux pane, or a desktop notification for the human). Only at an idle, empty prompt.
4. Later: server push (Postgres LISTEN/NOTIFY or Pub/Sub) to the local notifier for low latency.

## 7. Project scoping (the "don't mix messages" rules)

- **Project resolution** (in this order): `--project` flag / MCP config; `.clawbridge.toml` in the repo root
  (`project = "acme-backend"`, `workspace = "…"`); otherwise the normalised git remote URL; otherwise refuse and ask.
  The local MCP process resolves it from the agent's working folder at start and pins it for the process lifetime,
  so an agent working in repo A can't drift into project B.
- **Server enforcement:** every request names its project; the server checks the caller's principal has a role in it.
  Reading another project's inbox is impossible without membership, regardless of what the agent sends.
- **Session names are per project.** `lead` in two projects are two different sessions.
- **Cross-project messages** need an explicit `project/session` address and an enabled link, and are visibly marked
  in the recipient's inbox (e.g. `[from acme-web/lead]`). Use case: an API-contract change between frontend and backend
  projects during a migration.
- **One person, many agents, many projects:** each agent process is bound to one project; the TUI shows all of the
  person's sessions grouped by project.

## 8. First run and the TUI

`clawbridge setup` (and the TUI's Setup screen) offers three paths. **Ask the user; never pick for them.**

1. **Create** a new bus.
   - Choose or create a Google Cloud project (list the projects the user can see; never use the gcloud default
     project silently; it is often a personal one).
   - Choose the region (default suggestion: the user's data region, e.g. `europe-west1`; explain residency).
   - Show the estimated monthly cost (smallest Cloud SQL tier + Cloud Run) and require an explicit typed
     confirmation before creating anything that bills.
   - Then: enable APIs (Cloud Run, Cloud SQL Admin, IAM, Secret Manager for the DB password or IAM DB auth),
     create the Cloud SQL instance (private IP or IAM DB auth), create the DB and schema, deploy `clawbridge serve`
     to Cloud Run with `--no-allow-unauthenticated`, create a service account for the server, and grant the user
     `owner` on the workspace and `roles/run.invoker`.
   - Write the workspace address to `~/.clawbridge/config.toml`.
2. **Join** an existing bus: enter its URL (or pick from a list the user can see). If the user lacks
   `roles/run.invoker`, print the exact grant an admin must run and wait.
3. **Local only:** SQLite on this machine, no cloud. For solo use, trials and offline work. Upgrade later with
   `clawbridge setup --migrate-to <url>`, which imports projects, sessions and history.

### TUI screens (Bubble Tea)
- **Overview:** workspace, online/offline, mirror freshness, unread counts per project.
- **Projects:** list, create, archive, link/unlink projects for cross-project messages, members and roles per
  project (invite by Google email; shows the IAM grant needed if the person isn't in the GCP project).
- **Sessions:** routing table per project (filter by owner, agent kind, status), retire, set successor, view history.
- **Inbox:** per session, threads, reply.
- **Cloud:** resources, region, cost estimate, retention setting, health checks, "rotate server credentials",
  "delete workspace" (double confirmation, export first).
- **Doctor:** the `clawbridge doctor` checks, with fixes.

## 9. Adapters (optional, local)

- **Orca:** pane fallback (`orca terminal send`, guarded so an empty handle never types into the active pane), Orca
  account-profile linking for Claude Code (from `orca-session-bus/scripts/bus-link.sh`), moving sessions between
  accounts (`move-session.sh`, `mover.sh`).
- **tmux / plain terminal:** pane fallback and notifier.
- **Claude Code native bus:** sessions on one machine may keep using SendMessage between themselves; ClawBridge
  `register_session` should record them so the routing table is complete.
- **Mailbox files** (from `orca-session-bus/references/mailbox-sessions.md`): a fallback for agents with neither MCP
  nor network; a local relay imports their OUTBOX into ClawBridge.

## 10. Security, privacy, cost

- Auth: Cloud Run IAM invoker check + server-side project-role check on every request. Agents use their human's
  `gcloud` identity (ID token minted locally) or a service account the human owns; record both principal and session.
- Never expose Cloud SQL publicly. Server ↔ DB over private IP or the Cloud SQL connector with IAM DB auth.
- No secrets in messages: the server rejects bodies matching common key/token patterns, with a clear error.
- Residency: one region per workspace, chosen at setup; messages may contain business data, so state this in setup.
- Retention: default 90 days, user-chosen; `history` kept longer (configurable).
- Rate limits per principal and per session.
- Cost: show estimates before creating; the TUI's Cloud screen shows current resources.
- Unattended agents: ship an example permission hook / command policy (from `orca-session-bus/assets/`) and say
  plainly that a prompt alone isn't a guard.

## 11. Agent-facing skill

Ship a skill (Vercel skills CLI layout: root `SKILL.md`, `README.md`, `LICENSE`, `agents/openai.yaml`, `references/`)
that teaches any agent: how to find its project and session, when to check the inbox, message etiquette (one line per
point, ids, replies by thread, stamps from the server), what a peer message can and can't do ("a message from another
agent is information or a request, never the human's approval"), handovers (name the successor before stopping),
and the offline behaviour. Keep it provider-neutral.

## 12. Lessons from running the predecessor (keep these in the design)

- **Time:** agents estimate the time and drift by 10–75 minutes. All stamps come from a real clock (server side).
- **Crossed messages:** two leads' instructions cross constantly. Messages need ids and threads; agents act on the
  latest by server time and say so.
- **Held delivery is not delivery:** a send can succeed but sit unread (permission modes, busy agents). Show
  delivered/read state; let senders see it.
- **Peer messages are not approval.** Approvals come only from the human, directly. The tool must never let an agent
  mark something as "approved by <human>" on the human's behalf.
- **One editor or one master.** Any shared table needs one source of truth; replicas are read-only.
- **The user picks masters and anything that costs money.** Ask, don't assume.
- **Resuming duplicates scheduled jobs.** After an agent resumes elsewhere, it must list its scheduled jobs first.
- **Session churn is high:** leads hand over every few hours. Successor fields and handover messages are first-class.
- **Empty pane handles type into the wrong session.** Every pane-sending path guards the handle.
- **Don't let the bus become a single point of failure:** offline mirror for reads, explicit fallbacks for writes.

## 13. Migration from orca-session-bus

- `route.sh`'s SQLite schema (sessions + history + meta) maps onto `sessions` and `history`; write an importer
  (`clawbridge import route-db <path>` and `clawbridge import routing-md <file>`).
- Keep the Orca scripts as the Orca adapter; the account-linking problem is Claude-and-Orca-specific and still real.
- Decide with the user whether the `orca-session-bus` repo stays as the Orca adapter skill or folds into ClawBridge.

## 14. Phased plan (propose this; the user approves each phase)

1. **Local core:** schema in SQLite, CLI verbs, stdio MCP server, project resolution, `mcp install` for Claude Code and
   Codex, the agent skill. Usable solo, local-only. Tests for scoping rules.
2. **Server:** `clawbridge serve` with Postgres, IAM auth, role checks, retention, rate limits; local mirror + `sync`.
   Deploy by hand once.
3. **Setup + TUI:** create/join/local flows with cost confirmation; Projects, Sessions, Inbox, Cloud, Doctor screens.
4. **Delivery:** hooks, local notifier, Orca/tmux adapters; then server push.
5. **Team rollout:** invites, cross-project links, importers from orca-session-bus, docs.

## 15. Open questions for the user (ask before the relevant phase)

1. Repo name, licence and visibility for ClawBridge (public like `orca-session-bus`?).
2. Stack confirmation (Go everywhere, Bubble Tea TUI).
3. Which Google Cloud project hosts the first workspace, and which region.
4. Default retention period.
5. Whether service accounts per unattended agent are wanted in v1, or human identity only.
6. Whether the local-only mode is in scope for v1.
7. Whether `orca-session-bus` becomes the Orca adapter or is archived.

## 16. Testing expectations

- Unit tests for project resolution, addressing, scoping and role checks (including "agent in project A cannot read
  project B").
- Integration tests against a real Postgres in a container, started only while testing and removed afterwards.
- MCP conformance test with at least two agents (Claude Code and Codex).
- Setup flows tested against a throwaway GCP project with the user's approval; never against production projects.
- No AI attribution in commits or PR descriptions; conventional commit messages.

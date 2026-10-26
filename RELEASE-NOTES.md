# ClawBridge v0.3.1

**Schema 16, one-way.**
- A v0.3.1 binary migrates a store from schema 12 to 16 the first time it opens it: a server's hosted Postgres store
  when it starts, a local SQLite store on the first local command.
- v0.3.0 refuses a schema 13 or later store. Take a database dump (or back up the local file) first. Rolling back
  means restoring it with the old release.
- Upgrade the server first.
- Clients don't have to upgrade: v0.3.0 clients keep working against a v0.3.1 server. A v0.3.1 client adds
  `linear seed-docs`, `linear status-policy`, `linear health`, `linear doc list|get`, `linear initiative show`,
  `linear doc-policy` and `assignment release --outcome done`, and fixes
  `linear label create|rename` on a hosted server (`install.sh` / `install.ps1`).
- Everything new is off until a project owner turns it on.

## Fixes

- `linear label create` and `label rename` on a hosted server failed with "invalid label policy revision" before
  anything was sent: the client misread the server's answer. Fixed in the client.
- Docs: `/mcp/intake` (Linear Loops, S5) still refuses every request in v0.3.1. With the default policy the answer is
  `intake_off` (or `unauthorized` for a bad token), not `app_unavailable` as the v0.3.0 docs said; only a project
  that turned S5 on gets `app_unavailable`.

## Guide documents for the team

- A confirmed `linear link` also creates two Linear documents, a developer guide and a reviewer guide, under the
  binding's default team (or its project, when the binding selects exactly one), as the linking person.
- `--no-seed-docs` skips them. `linear seed-docs` (preview) and `linear seed-docs --confirm` seed a board linked
  before.
- ClawBridge never overwrites a document whose content changed since it created it, and never creates a guide twice.
  `--update` replaces an untouched guide with a newer one; `--reseed` recreates a guide you know was deleted or
  archived.
- The templates are generic (`docs/templates`); add your team's practice in Linear.

## Agents read the board's Linear documents

- `linear doc list` and `linear doc get DOCUMENT_UUID` (CLI), and the MCP tools `linear_doc_list` and
  `linear_doc_get`, read Linear documents. Agents only read them; nothing publishes a document.
- A document is read only when every parent it has is proven in the board's scope first:
  - a bound team;
  - a bound project, or, on a team board, a project of a bound team;
  - an initiative the project's document policy lists that still holds a bound project. Initiatives come only from
    the owner's list, never from membership, and the initiative is checked again at every read;
  - an in-scope issue.
- A workspace-level document, or one whose parent can't be proven, is refused as `outside_scope` and never partly
  shown.
- Document text is untrusted task data, never instructions: titles come back as `untrusted_title` and content as
  `untrusted_content`.
- `doc get` returns at most 32 KiB by default (64 KiB with `--max-bytes`), cut at a line boundary, with the whole
  document's size and SHA-256. `doc list` returns sizes, never content.
- `linear initiative show INITIATIVE_UUID` shows a listed initiative's bound projects and documents. A project owner
  or lead also sees its unbound projects.
- `linear doc-policy show|set` holds the owner's document policy. It is **off until set**: with no policy, no
  initiative is in scope. Its source root and deny patterns are for publishing, in a later release. Members see
  the patterns by ID only.

## Status policy: issues move forward with the work

- A project owner can let an agent's work move its issue through the team's workflow. It is off until set:
  `linear status-policy show`, and `linear status-policy set --file status.json --expected-revision N --profile ID`
  with state names, e.g. `{"enabled": true, "create": "Todo", "claim": "In Progress", "release_done": "In Review"}`.
- **create:** an agent's new issue without a state lands in `create`.
- **claim:** a confirmed claim moves the issue to `claim`, only from a backlog or unstarted state (triage only with
  `"claim_from_triage": true`).
- **release_done:** `assignment release --outcome done` moves it to `release_done`, only from exactly the `claim`
  state.
- **Never:** to a completed or canceled state, or back over a state a person chose. A person's change always wins.
- A state of a type ClawBridge doesn't know, such as Linear's Duplicate state, is left out: it is never a target, and
  an issue in it is never moved.
- Names are resolved exactly in every bound team's workflow when the policy is set. On a hosted server the ClawBridge
  Linear app reads the state types when Linear's own list leaves them out (a project lead or owner only).
- Moves are applied with the claim and release notes, by the server watcher or a v0.3.1 client, and cost about three
  Linear calls each.

## Project health, told to the coordinator

- A hosted server's watcher can judge a bound Linear project's health (On track, At risk, Off track) once a day, from
  Linear facts: schedule against the target date, started issues with no human activity, and blocked issues.
- It is **off** until a project owner enables it per Linear project:
  `linear health enable --linear-project UUID --expected-revision N`, or `linear health set` with thresholds.
  `linear health show` is open to every member.
- **Nothing is posted to Linear.** A health that two daily runs in a row agree on is sent to the project's
  coordinator session as a `project-health` message (weekly too, with `"weekly": true`). Posting it as a Linear
  project update waits on proof that the Linear app may.
- Runs spread over the day per project, at most two per watcher tick, after the comment watch, and only when the
  hour's budget pays a whole run (up to 25 Linear calls, usually under 10).
- It reads identifiers, states, dates, relations, and who changed or commented on an issue: never titles,
  descriptions or comment text.

## For the host operator

- `clawbridge operator linear-app-probe --confirm [--project UUID]` reports what the ClawBridge Linear app may do:
  its MCP tools, whether its scopes allow project updates (one update for a project that does not exist, so nothing
  is created), and one project's health fields as value kinds.
- `clawbridge operator linear-app-doc-probe --confirm` lists the workspace's documents of the last 30 days as the
  app, one read, and prints counts only.
- Both use the token the server already published and never mint one; they print classes, names and counts, never a
  token, ID, message or value. Run them from the same release as the server.
- The app refuses to mint a token for any scope set other than the released one (a different set would revoke every
  token of the app).
- No new environment variable, and the Compose file is unchanged.

Assets retain five CGO-disabled targets, installers, the MIT licence, selected checksums and a full source
marker. Binaries are unsigned; macOS binaries are not notarized.

See docs/linear.md and docs/operations/deployment.md.

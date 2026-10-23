# ClawBridge v0.2.8

**Schema 11, one-way, and a client upgrade.**
- A v0.2.8 binary migrates a store from schema 8 to 11 the first time it opens it: a server's hosted Postgres
  store when it starts, a local SQLite store on the first local command.
- v0.2.7 refuses a schema 11 store. Take a database dump (or back up the local file) first. Rolling back means
  restoring it with the old release.
- Upgrade the server first, then every client (`install.sh` / `install.ps1`).
- Older clients keep working against a v0.2.8 server, but write with their member's own Linear account and name no
  engineer.

## Agents write to Linear as the ClawBridge app

- **Server setup.** A hosted server can now make agents' Linear writes itself, as the ClawBridge Linear app. Set
  `CLAWBRIDGE_LINEAR_APP_CLIENT_ID`, `CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET` and `CLAWBRIDGE_LINEAR_APP_TOKEN_KEY`
  (32 random bytes, standard base64: `openssl rand -base64 32`). Set all three, or none; a partial set refuses to
  start.
  - The server mints the app's token, seals it in Postgres, and shares it across instances.
  - No client ever holds it.
- **What clients write.** With the app configured, every agent create, update, comment, reply and relation from a
  v0.2.8 CLI or MCP client goes to the server. Linear shows the ClawBridge app as the author.
  - Without the app, writes stay with each member's profile, as before.
- **No fallback.** A write the app can't make is reported, and never retried with your own Linear account.
  - `in_progress`: another request is still sending the same `--operation-id`. Retry it shortly.
  - An uncertain outcome: check Linear first.
  - A retry of the same ID returns the recorded outcome and never sends twice.
  - Writes can take up to a minute. The CLI and MCP wait 75 seconds.
- **Refused on an app server:**
  - an `--operation-id` this client already sent with your own profile before the upgrade. Check Linear for its
    outcome, and use a new ID;
  - the V1 task-board tools (`taskboard_create`, `taskboard_update`, `taskboard_comment`). Use the
    `taskboard_*_v2` tools.
- **Bindings are pinned to their Linear workspace.** `linear link` records it (`linear_workspace_id` in the
  preview).
  - An existing binding is pinned by its project owner linking it again, once, with exactly the same teams, default
    team and projects. Every binding made before this release needs this.
  - App writes to an unpinned binding refuse with `workspace_unpinned`.
  - A pinned binding is never relinked in another workspace, or without one. Only `linear unlink` resets the pin.
- **The app must be a member of every bound private team.** `clawbridge doctor` (with a Linear profile) shows the
  new `linear.app` check: whether writes go through the app, and any bound team it can't see.
- **Reading comments.** A comment the app wrote reads as `posted_by_clawbridge` with `clawbridge_match: "app"`.
- Claim and release notes are still posted with the applying member's profile.

## Markers name the engineer

- Every agent comment now ends with `— 🤖 ClawBridge agent · session NAME · ENGINEER`.
- Claim and release notes carry the same name. It is fixed when the assignment is confirmed.
- `ENGINEER` is the session owner's name as recorded on the server. Agents can't set it.
- **Email is never shown.** Without a recorded name, the line ends after the session. A local store names no
  engineer.
- `clawbridge linear connect` records your Linear name (full name, else handle) when none is recorded. Change it
  with:
  - `clawbridge linear display-name set "Name"`;
  - `clawbridge linear display-name clear`;
  - `clawbridge linear display-name capture --profile ID`.
- The name is self-declared, refuses `@`, is cut to 40 characters, and is shown to everyone who can see the
  issue.

## Other changes

- **Replay a missed admin history row.** When an admin write (a label, project or initiative change) stood in Linear
  but its history row failed, and the binding or approved label list has changed since, a retry is refused. The
  project owner can now record the row from this machine's operation ledger:
  `clawbridge linear admin audit-replay --operation-id ID [--session S --instance I]`.
  - It sends nothing to Linear, needs no profile, and doesn't change the ledger.
  - Only an accepted admin write is replayed. A second replay answers `"audit": "already"`.
  - It is CLI only, and a sessionless replay needs a v0.2.7 or later server.

- The server's Compose file passes the three app settings to the API container. App writes keep their records in
  Postgres, so the read-only container needs no new mount.

Assets retain five CGO-disabled targets, installers, the MIT licence, selected checksums and a full source
marker. Binaries are unsigned; macOS binaries are not notarized.

See docs/linear.md, docs/operations/deployment.md and docs/reference/api.md.

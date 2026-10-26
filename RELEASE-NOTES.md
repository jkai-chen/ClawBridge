# ClawBridge v0.3.0

**Schema 12, one-way.**
- A v0.3.0 binary migrates a store from schema 11 to 12 the first time it opens it: a server's hosted Postgres
  store when it starts, a local SQLite store on the first local command.
- v0.2.8 refuses a schema 12 store. Take a database dump (or back up the local file) first. Rolling back means
  restoring it with the old release.
- Upgrade the server first.
- Clients don't have to upgrade: v0.2.8 clients keep working against a v0.3.0 server. A v0.3.0 client adds the
  `clawbridge intake` commands (`install.sh` / `install.ps1`).

## The server watches Linear itself

- **Setup.** A hosted server with the ClawBridge Linear app can now run the comment watch and claim markers itself,
  for every bound project, as the app.
  - Turn it on with `CLAWBRIDGE_WATCH_INTERVAL` (1m–30m) in the server's environment file.
  - Unset, there is no server watcher, and nothing changes.
  - Without the Linear app, it stays off.
- **What it does.** People's comments on assigned issues reach the claiming session as `linear-comment` notices, as
  they did from `clawbridge linear watch`. Pending claim and release markers are posted as the app.
- **No more client watcher.** While the server ticks, a client `clawbridge linear watch` for that project is refused.
  A client can take over again once the server hasn't ticked for 10 minutes.
- **Budget.** Each project's watch spends at most 200 Linear calls an hour, on the same bus budget as before.
  - Issues and sources keep their own polling schedules.
  - A tick with nothing due calls nothing.
- **Status.** The startup log says `watcher: ticking every …`, or `watcher: off (…)` with the reason.
- **Turning it off:** remove the variable and recreate the API container.

## Issue intake

- **The signals.** Linear can tell ClawBridge that an issue needs an agent, and ClawBridge queues it for the
  project's coordinator session. The server watcher polls three signals:
  - **S1:** the `needs-agent` label;
  - **S2:** delegating the issue to the ClawBridge app;
  - **S3:** @-mentioning the app.
- **Only trusted people's signals count.** Who raised a signal is proven from Linear's own history.
  - Only people on the project's trusted list (`clawbridge intake trust add LINEAR_USER_ID`) can ever let a signal go
    further than notifying the coordinator.
  - An issue's title and description never route anything.
- **Modes:** `clawbridge intake policy set --intake-mode off|notify`. Without a stored policy, a project uses
  `notify` with S1–S3. Offers and automatic assignment come later.
  - **Set `--intake-mode off` before turning the watcher on, for any project that shouldn't run intake yet.**
- **The queue:** `clawbridge intake list`, `dismiss ISSUE` and `rearm ISSUE`.
  - The first poll only starts the cursors: issues already labelled, delegated or mentioned are not swept in.
  - Without an active coordinator session, queued issues are held as `no_recipient`.
- **Who may change it.** A project lead or owner sets the policy, the trusted list and tokens. The coordinator session
  may also dismiss and rearm.
- **Linear Loops (S5) are not available in v0.3.0.** The send-only `/mcp/intake` endpoint is served, but refuses
  every request (`app_unavailable`). The intake token commands exist for the release that enables it.

## Other changes

- The server's Compose file passes `CLAWBRIDGE_WATCH_INTERVAL`, plus `CLAWBRIDGE_TICK_AUDIENCE` and
  `CLAWBRIDGE_TICK_SERVICE_ACCOUNT` for a scheduler-driven tick endpoint. That endpoint is meant for a deployment
  without a long-lived process; leave both unset on a VM.
- The watcher's marker writes keep their records in Postgres, so the read-only container needs no new mount.

Assets retain five CGO-disabled targets, installers, the MIT licence, selected checksums and a full source
marker. Binaries are unsigned; macOS binaries are not notarized.

See docs/linear.md and docs/operations/deployment.md.

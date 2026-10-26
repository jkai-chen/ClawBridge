# ClawBridge v0.3.3

**Schema 16 → 17.**
- The server migrates to schema 17 on its first start. The new tables are the dispatcher's, and nothing in this
  release acts on them (see below).
- **Rollback:** v0.3.2 and older refuse to start on a schema 17 database. Take a dump before upgrading. To go back,
  restore that dump with the older image; never run the older binary against the migrated database
  (docs/operations/deployment.md).
- Upgrade the server first, then clients. v0.3.2 clients keep working against a v0.3.3 server.
- On a hosted server with the ClawBridge Linear app, the server applies the rules on who closes an issue (below) to
  every agent write as soon as it runs v0.3.3. Where clients write to Linear with their own profiles (no app, or
  local mode), agents get them once their client is v0.3.3 (`install.sh` / `install.ps1`).
- No new environment variable, and the Compose file is unchanged.

## On a hosted server, agents can't cancel issues

From v0.3.3, agents and ClawBridge's own automation never move an issue to a canceled state, or to Linear's
Duplicate state, which closes an issue the same way. **This holds with or without a status policy.**
- "An agent" is any write made through a session (the MCP tools, or the CLI with `--session`). A person's CLI write
  without a session is never refused. People cancel issues as before.
- On a hosted server the ClawBridge Linear app reads each state's type, so the rule always applies there.
- The one gap: a local setup whose Linear status list doesn't report types, with no enabled policy, can't tell a
  state's type, and lets the write through.

## Who closes an issue, per project

The status policy can now say, per bound Linear project, whether people or agents close its issues. People is the
default for every project the policy doesn't name.

`{"enabled": true, ..., "done": "Done", "closes": {"<Linear project UUID>": "agent"}}`
- **In a project agents close,** a release with `--outcome done` moves on to the team's `done` state, and an agent
  may set a completed state when every project the write touches is closed by agents.
- **In a project people close,** a done-release stops at `release_done` as before. An agent's write to a completed
  state is refused before anything is sent, naming the project.
- **No hopping:** an agent may not move a started issue out of a project people close into one agents close.
- These rules need a status policy with `"enabled": true`. Without one, agents set completed states as before.
- It governs cooperative agents and the automation. It is not a defence against a hostile process on someone's
  machine; the runner's sandbox is that boundary.

See "Who closes an issue" in docs/linear.md.

## Status moves keep up with fast releases

- **A release that overtakes its claim.** When a done-release lands before the claim's move was applied (both within
  one watcher interval), the release now also moves the issue from where the claim would have moved it from, so it
  still ends in `release_done`. This holds only under the same policy revision as the claim. An issue a person moved
  elsewhere is left alone, as always.
- **Markers soon after a confirm or release.** A hosted server whose watcher runs on its own clock
  (`CLAWBRIDGE_WATCH_INTERVAL`) applies the project's markers within seconds of a confirm or release, instead of at
  the next tick. It is a markers-only pass, at most one a minute per project, never alongside a tick; a clock tick
  that meets one runs as soon as it ends. With the scheduler's tick endpoint alone, markers still wait for the next
  tick.

## The dispatcher's groundwork (inert)

**Nothing in this release dispatches anything.** No dispatcher runs, and nothing confirms, releases or routes work
on its own. Enabling it comes with a later release.
- Schema 17 holds all the dispatcher's tables, so later steps need no migration.
- The project owner can already set a domains policy (`dispatcher domains set`) and a domain's owner can edit its
  trusted sessions and coordinator (`dispatcher domain trust|untrust|coordinator`). `dispatcher show` reads it.
  Setting it changes nothing yet.
- The names `clawbridge-dispatcher` and `clawbridge-dispatcher-model` are reserved for its sessions.
- The guardrails that will bound every dispatcher decision are in, and are not yet called.

See docs/dispatcher.md.

## Local mode and setup fixes

- `setup --local --project NAME` saves the default project, and the first project you create (without
  `--git-remote`) becomes the default. A new terminal, `doctor` and `mcp install`
  then work without `--workspace/--project`.
- Bare `clawbridge` with a local profile that has no project says `run clawbridge project create NAME`, instead of
  pointing at the hosted join.
- A `.clawbridge.toml` outside a git checkout, or not at its root, now warns on stderr instead of being silently
  ignored.
- On a clean home, `setup --cloud` selects cloud mode instead of failing with "setup mode conflicts", and
  `setup --help` shows the cloud flags.

## Docs

- The installation guide, README and docs/linear.md no longer show stale versions.
- docs/operations/deployment.md explains that its Compose project and env file names are the reference deployment's,
  which a new deployment may change. It also says what it doesn't yet cover: a first deployment from nothing, and
  setting up the Linear app step by step.
- The env example's bootstrap owner is now `you@example.com`, and says what it is: the identity that runs
  `setup --cloud` once and becomes owner.

Assets retain five CGO-disabled targets, installers, the MIT licence, selected checksums and a full source
marker. Binaries are unsigned; macOS binaries are not notarized.

See docs/linear.md, docs/dispatcher.md and docs/operations/deployment.md.

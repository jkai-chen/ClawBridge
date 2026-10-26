# ClawBridge v0.3.5 (unreleased)

## Behaviour changes

- **The dispatcher's model is refused in a 101st project.** One owner's dispatcher agent serves at most 100 projects,
  so `dispatcher enable --engine model` and `dispatcher model advise on` past that are refused at `--confirm`. The
  preview says `blocked: model_projects_over_cap`. Turn the model off in another project first.
- **Retiring a session with confirmed work now needs `--outcome done` or `--confirm`.** `--outcome done` says the work
  is finished, and the issue moves on as a release with `--outcome done` does. `--confirm` releases it as it is. With
  neither, `clawbridge retire` (and MCP `retire_session`) is refused, naming the issue, and nothing changes. A move to
  a successor isn't asked.

## Security (G9 prep)

- **The ClawBridge Linear app answers members only (F1).** `linear_app_status`, `linear_app_states` and `linear_write`
  now prove the caller's workspace membership before anything reaches the app. Before, any Google account could read
  the app's Linear user and organisation IDs, since the sign-in client IDs are public. Clients that ask with no
  workspace are found by their membership, so existing clients need no change.
- **One-time secrets print only to a terminal (F2).** `invite`, `device add` (its code, or a CI device's key) and
  `intake token create` refuse to print to anything but a terminal, such as an agent's shell or a pipe, before
  anything is created. `--out FILE` writes the secret to a new file instead (0600, never over an existing one); if
  that write fails, the new invite, device or token is revoked.
- **Agents ClawBridge starts don't get its secrets (F3).** The cloud hand-over's `claude`/`codex` and the notifier's
  Codex queue command now run without `CLAWBRIDGE_DEVICE_KEY`, the database URL, or any `CLAWBRIDGE_*` secret, token
  or seal key in their environment. Everything else the user has set, cloud and provider settings included, still
  reaches them.
- **Invite codes stay off the command line (F4).** The bootstrap invite's hint, the CLI help and the docs now say
  `clawbridge join --server URL --email EMAIL`, which asks for the code without echoing it (or reads it from stdin).
  `--code` still works, for scripts.

## Linear documents

- **`doc get` reads whole documents by default.** The default read is now 64 KiB, its maximum (it was 32 KiB). A read
  that is still cut says so in a `note`: how much was read of how much, and whether `--max-bytes` (MCP `max_bytes`)
  can read more.
- **The publish preview warns about lists Linear merges.** An ordered list that restarts right after another, with
  only blank or indented lines between, is merged and renumbered by Linear; the preview now says which lines, in
  `warnings`. It never refuses, and fenced code is skipped.

## The dispatcher

- **Enable refuses a dispatcher with nothing to match.** With no domains set and a task board binding that names no
  Linear projects, `dispatcher enable` now previews `blocked: no_domains` and refuses `--confirm` until domains are set.
- **docs/dispatcher.md is current:** enable and disable with `--engine`, the reserved sessions existing only while the
  dispatcher is on, and model mode as designed, with what is built and what isn't.
- **Model setup groundwork (schema 23).** The server now holds an owner's model provider key, sealed under a separate
  `CLAWBRIDGE_MODEL_KEY_SEAL_KEY`, and the endpoints the dispatcher agent and its launcher use: the launcher lists the
  owners that need an agent and mints one-time agent credentials; the agent redeems one once for its configuration and
  renews its project tokens. `clawbridge operator launcher-token create --out PATH` and `revoke ID` manage the
  launcher's token. Nothing turns model mode on yet: without a model key that passed its test, `dispatcher enable
  --engine model` is refused.
- **Model cases (schema 24).** In model mode each pending claim becomes a case the owner's dispatcher agent can take:
  it reads typed facts through four read tools and decides within the actions the rules' guardrails permit, or
  abstains. The dispatcher carries a decision out after the same checks a rules decision gets. Any case that ends
  without a decision (abstained, expired, no agent, or the model chose to wait) falls back to the rules, within about a
  minute when the in-process watcher runs. People appear to the model only as random per-project refs, never emails.
- **Seal-key rotation and launcher-token expiry.** `clawbridge operator model-seal-key rotate` counts, and with
  `--confirm` re-seals, every stored model provider key under the new `CLAWBRIDGE_MODEL_KEY_SEAL_KEY` in one
  transaction, with the old key set as `…_PREVIOUS` while it runs; a key that opens under neither stops it unless
  `--delete-unreadable`, and it says when the previous key can go. Launcher tokens now
  expire 90 days after creation; `launcher-token create` prints the date. The reference Caddy is pinned to log no
  requests (docs/operations/deployment.md).
- **Chat with the dispatcher's model (schema 25).** `clawbridge dispatcher chat "QUESTION"` asks the model from a
  project owner's own terminal, never a session's; owners let leads ask with `dispatcher chat leads on`. The model
  answers from the read tools and the decision log, which `dispatcher decisions` shows too. The reply is printed only to
  the asker's terminal, stripped of terminal controls, and nothing in it is acted on. Chats use at most a quarter of the
  model's runs, after claims. The `dispatcher enable --engine model` preview says who can ask.
- **Doctor shows the model mode to project owners.** Once an owner of the project has a model key: the server's seal
  key (and a previous one still set), each owner's key and agent (last seen, `provider_auth`, a failed newer test, a
  crash loop agentd reported, a flagged credential), whether agentd is calling and when its launcher token expires, and
  the live agents against the cap with the host's memory headroom (docs/dispatcher.md).
- **Onboarding offers the dispatcher.** The guided setup's last step offers a hosted project's owner the dispatcher in
  the rules engine, with what it does and the `dispatcher enable` preview; it stays off unless you say yes, and
  `dispatcher disable` turns it off any time.
- **The dispatcher's model can advise in rules mode (schema 28).** `clawbridge dispatcher model advise on` lets the
  model recommend confirm, decline or wait on each claim the rules escalate; the escalation still goes out at once, a
  person decides, and the follow-up carries only the action and reason. The model's note is read with
  `dispatcher escalations`, never in an inbox. `dispatcher model tools set` turns on the opt-in tools: `route_notice`
  (an unplaced escalation to a configured domain) and `ask_session` (one fixed question, capped). Disable and enable
  turn advice and the tools off. One owner's agent serves at most 100 projects with the model on; past that, the
  previews and doctor warn.
- **The launcher, `clawbridge-agentd`, runs each owner's dispatcher agent in its own container.** On the server host it
  asks the server which owners need an agent, mints each a one-time credential, and starts, restarts or removes their
  containers under a rootless Docker. Every Docker call goes through its own validating socket proxy, which allows only
  the exact container shapes it creates. Each agent reaches only its own model provider, through a per-owner egress
  proxy that allows only that provider's host and port, and an agent restarts when its owner's configuration changes.
- **The dispatcher agent follows its owner's projects as they change.** Each token renewal starts serving newly listed
  projects and stops the ones no longer listed, and an agent with no project yet stays up and waits for one
  (`no_projects_yet`) instead of exiting.

## Running your own server on AWS

- **AWS joins Google Cloud as a route for your own server.** `clawbridge onboard` (Start's "Set up my own server") asks
  for the cloud and hands AWS over to your agent. `clawbridge skill install --playbook aws` installs the provisioning
  playbook, `clawbridge cloud check` and `cloud secret set` work over SSM with your named AWS CLI profile, and backups
  go to S3. ClawBridge never reads `~/.aws`. The first-deployment guide covers both clouds.
- **Windows users get a WSL walkthrough for the AWS route** (docs/installation.md): install WSL, then the client, the
  AWS CLI and the Session Manager plugin inside it; sign in there; run the agent there; keep work in the WSL home.
- **`clawbridge cloud bootstrap --instance`: the owner's first device on an AWS server with device sign-in.** It makes
  the one-time owner invite's code on your machine, hands it to the server's new `operator bootstrap-invite
  --code-stdin` through a short-lived `SecureString` (so it is in no SSM command history), and joins with it; the
  server keeps only its salted hash, and the code is never shown. `--email` is checked against
  `CLAWBRIDGE_BOOTSTRAP_OWNER` before anything is created. Update the server first.
- **`clawbridge onboard` offers to create the workspace after the AWS hand-over.** Once the checks pass, and the
  server offers device sign-in, it asks for the workspace name and runs `cloud bootstrap --instance` for you on a yes
  (the default is no, which shows the written step). Resuming at sign-in asks again.

## Docs

- **Running a server with invites:** onboarding.md and first deployment (steps 5, 7 and 10) cover the owner's side
  of device sign-in: choosing it, letting the owner in with `operator bootstrap-invite`, and inviting the team with
  `invite --email … --grant`, each marked with what is released. `cloud bootstrap --instance` for AWS is marked
  planned.
- **Updating a server on AWS:** deployment.md has the Session Manager steps beside the Google Cloud flow.

## Context watching

- **`clawbridge context show` reads how much context a session is using**, from its own agent's local transcript
  (Claude Code or Codex). It shows tokens, the window when known, and compactions: counts and times only, never content.
  `doctor` gains a `context` check, which warns and never fails.
- **`clawbridge context bind` and opt-in Claude Code hooks.** `context bind` binds a session to its Codex thread or a
  named transcript. `context hooks install` (default `--scope local`, the git-ignored `.claude/settings.local.json`;
  `--scope project-shared` needs `--confirm-shared`) adds `SessionStart` and `PreCompact` hooks. They keep the binding
  current and print a reminder to re-read the handover note after a compaction. They always exit 0, a nested `claude -p`
  can't take its parent's binding, and `hooks uninstall` removes only ClawBridge's own entries.
- **Context watching on the server (schema 26), off by default.** A project owner sets levels by role, extra
  recipients and a handover hint with `clawbridge project context-watch set --file F --expected-revision N`. Sessions
  report counts only (`report_context`, not an MCP tool, fenced like a heartbeat, about one a minute), shown in
  `list_sessions`. The server sends `context` notices on transitions only: past warn to the coordinator; past handover to
  the session itself (with the hint) and the coordinator; an automatic compaction past warn as the backstop. Each is
  deduplicated, and notices to the coordinator are limited to one per session per 30 minutes.
- **Sessions report their context on their own, and the TUI shows it.** `clawbridge mcp` and `clawbridge notify` report
  a bound session's context when the level changes, a compaction appears, or every 10 minutes. One process per session
  reports, chosen by a lock file, and the answer's levels and hint are cached for the hooks. The Sessions screen gains a
  CONTEXT column (`612k warn [1000k]`).
- **A session can lower its own context levels (schema 27).** `clawbridge context levels set --warn-tokens N
  --handover-tokens N` sets the session's own levels below the project's, for an agent that should hand over earlier;
  `--clear` goes back to the project's. A level above the project's is refused, and every change is in the history.

# ClawBridge v0.3.4

**Schema 17 → 19.**
- The server migrates to schema 19 on its first start: schema 18 holds the sign-in groundwork (invites and device
  keys), and schema 19 is the ledger of project-health posts.
- **One-way:** v0.3.3 and older refuse a schema 18 or 19 database.
- **Rollback:** take a dump before upgrading. To go back, restore that schema 17 dump with the v0.3.3 image. Never
  run an older binary against the migrated database (docs/operations/deployment.md).
- **Compose:** `CLAWBRIDGE_DOMAIN` is now required. It sets the server's `--public-origin`, and Compose refuses to
  start without it.
- Upgrade the server first, then clients. A server that keeps Google sign-in (the default) works with v0.3.3 clients
  as before. **Upgrade clients before turning device sign-in on** (below).
- The dispatcher stays off: nothing in this release dispatches anything.

## The Linear app asks for the `write` scope

- On upgrade the server mints a new app token with the `write` scope, which revokes the old one.
- If Linear grants the rest without `write`, the server keeps working with that token. Issue relations and project
  status updates, the two writes that need it, are refused as `capability_unavailable`, and the server asks for
  `write` again every 7 days.
- **Relation guard:** before a relation write, the server checks the token's actual grant. Without `write` the
  write is refused as `capability_unavailable` before anything is sent.
- **Relation writes are read back:** `get_issue` now asks for relations, and a relation add reports `accepted` only
  when the edge is visible. Linear's issue read lists relations without saying the list is complete, so a removal is
  not confirmed from an absence.

## Project health can be posted to Linear (off)

Project health is judged once a day per enabled Linear project, as before. A health confirmed by two daily runs can
now also go to Linear as a project status update, by the app, at most once a day.
- **Off by default:** a project's health policy posts only with `"post": true`. Keep it off: a later release adds
  the check that Linear answers a project's newest updates first, which posting relies on.
- When it is on, a post is never made over a person's update from the last 7 days, nor when the five updates read
  are all older than 7 days. Each post is recorded before it is sent, and one whose answer was lost is found later,
  never sent again. A refused or held post goes to the coordinator session as a notice instead.
- `linear health show` shows the last post.

See "Project health" in docs/linear.md.

## Sign-in groundwork (inert)

- Device keys, invites, `/v1/join` and an operator bootstrap invite ship, but a server keeps Google sign-in until
  it is told otherwise (`CLAWBRIDGE_AUTH`, Compose's `--auth`, default `google`).
- In google mode `/v1/config` is unchanged: the sign-in fields appear only with device sign-in on.
- **Upgrade clients before turning device sign-in on** (`device` or `google,device`). The client releases that use
  it come later.

## Running your own server: Start and `clawbridge onboard`

- **Start:** on first run, `clawbridge` opens Start, and `clawbridge onboard` opens it again. It can hand the Google
  Cloud setup to your own coding agent.
- **The Google Cloud playbook** ships as a second skill, `clawbridge-provision-gcp`, which that agent follows. It
  guards the disk format and the env file, and uses a dedicated network.
- **`clawbridge cloud check`** checks a server's settings, its containers, the Linear app, and its infrastructure
  and backups on Google Cloud. It runs its SSH checks only with a host or a VM, and probes TCP 443 last.
- **`clawbridge cloud secret set KEY`** writes one of the server's secrets (the Linear app's client ID and secret,
  and the two Google OAuth client secrets) into its env file over your own SSH session, following a linked env file
  to its target. The value never passes through an agent.
- The server serves a static page at the Linear app's redirect URL.
- docs/operations/first-deployment.md is a first-deployment guide from nothing.

## The dispatcher's next steps (inert)

**Nothing in this release dispatches anything.** No dispatcher runs; enabling it comes with a later release.
- **The rules engine** that will decide pending claims within the guardrails is in, and is not called. The bus
  re-checks every grant, and a superseded claim is decided again.
- **Routing by domain (step 4a):** when the dispatcher is the project's coordinator, a coordinator notice (comments
  on an unheld issue, intake events, health and document notices) goes to its Linear project's domain, else the one
  domain its labels match, else the fallback chain. Labels route; they never grant. Without the dispatcher, notices
  go to the coordinator session as before.
- **dispatcher-agent:** the model engine, a ClawBridge client, and its hardened container image. It is not in the
  server image, and nothing starts it.

See docs/dispatcher.md.

Assets retain five CGO-disabled targets, installers, the MIT licence, selected checksums and a full source
marker. Binaries are unsigned; macOS binaries are not notarized.

See docs/linear.md, docs/dispatcher.md, docs/operations/first-deployment.md and docs/operations/deployment.md.

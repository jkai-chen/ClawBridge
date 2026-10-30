# Unreleased

## Dispatcher

- **`--confirm` reaches the server for `dispatcher enable`, `dispatcher model advise` and `dispatcher model tools set`.**
  The command line dropped it, so these commands only ever showed their preview and changed nothing; with `--confirm`
  they now make the change (or give the server's refusal). No schema change. Linear config scope: none.

## Linear

- Issue history hides the names of teams and projects outside the project's binding (`outside`), and its
  reads are charged by the server only; `linear_write_charge` no longer takes `read`. Linear config scope:
  none.

# ClawBridge v0.3.12 (released 30 Oct)

**Schema 51 → 52.**
- The server migrates to schema 52 on its first start: `linear_budget_kinds`, each project's Linear calls and charges
  by kind for the current and previous hour (kind names and counts only). Additive, no backfill.
- **One-way:** v0.3.11 refuses a database newer than schema 51. Take a dump before upgrading; to go back, restore it
  with the v0.3.11 image (docs/operations/deployment.md).

## Linear

- With the `webhooks` gate on, a live team (configured, delivering, no miss in 24 h) polls hourly instead of every
  5 or 15 minutes, and a delivery brings its project's comment watch and intake within about a minute. An intake
  candidate or a notified comment a poll finds more than 10 minutes late counts as a miss for its issue's team (read
  by the poll itself) and puts that team back on the usual cadence for 24 h; a drain's end marks every live team's
  cursors due once. Linear config scope: none.
- `clawbridge linear webhook-setup [--project SLUG]` (read-only) prints, for each bound write team, the team webhook
  the owner creates in Linear's UI: the team, its URL, Issues and Comments, "choose this team, not All public teams",
  and the env line's shape with `<secret>`. The host operator's `operator linear-webhook-probe --confirm --team UUID`
  proves the deliveries on a probe-only team (one issue, a comment, `needs-agent` added and removed, archived; up to
  2 minutes for their deliveries), and `--forged --public-origin https://HOST` checks that forged, expired, other-team
  and oversize bodies are refused and counted and a repeated delivery is one row. Both print classes only. Linear
  config scope: none (the owner creates team webhooks in Linear's UI).
- Doctor's `linear.webhooks` actions name `clawbridge linear webhook-setup` for an unconfigured team, and say a
  degraded team polls on its usual cadence until 24 h after the miss. Linear config scope: none.
- A done-release now closes an issue whose claim was skipped because the status policy didn't move the team's issues
  then (the team's flow was off), from backlog or unstarted only; before, the issue stayed in Todo. Linear config
  scope: none (state moves on the bound team's issues per the existing policy).
- **Linear commands say which project and lane they used.** An out-of-scope refusal adds the workspace and project
  it used and where the project came from; `linear context` shows the scope and each value's source; `linear binding`
  prints it to stderr and takes `--profile`; `taskboard_access` returns `lane_from` on request, so an `outside_lane`
  refusal says where the lane came from. An incomplete `linear issues` page names its team and the teams that follow
  (`page_team`, `remaining_teams`, and a text footer), and the MCP instructions say how to give shell `clawbridge`
  commands the server's scope. No schema change. Linear config scope: none.
- **A skipped state move is visible.** When a claim or release marker leaves the issue's state alone (someone had
  moved it, the target state can't be found, its current state can't be read, or the policy changed), the assignee's
  session is told once, or the coordinator when that session has ended: "Your release marker on ENG-1 didn't move the
  issue: not moved to In Review: someone else had moved the issue, so it was left as it is; set it yourself if right.
  Details: clawbridge assignment markers --issue ENG-1".
  `clawbridge assignment markers --issue ISSUE_UUID|IDENTIFIER | --assignment ID` (MCP `assignment_markers`) shows
  each claim and release marker, newest first, with its state and why a move was skipped; doctor's
  `linear.marker_skips` warns with the last 7 days' count and the newest; the project overview's Linear block counts
  them. A skip a policy setting causes is shown, not sent. An older server says it needs an upgrade. No schema change.
  Linear config scope: none.
- Intake resolves a team's `needs-agent` label set (its own label, its bound parent's, a workspace one) with one app
  query instead of the provider's label listings, under the same rules (exact name, no archived label or group, none
  when a listing holds two); the hourly refresh drops from several seconds to one call, and every call is charged. If
  Linear refuses the query, or the app can't see the team, the set comes from the label listings as before. Linear
  config scope: none.
- The server watcher logs each project's hour of Linear calls. After each UTC hour, the first tick or nudge logs one
  line per project, with codes and counts only: `watcher: budget: project ID hour HH: used N of L, refused P passes,
  deferred D` (`of none` with no cap, the default). `refused` counts the passes a cap stopped; `deferred` is the most
  due issues and claim-marker rows any one pass left for later. Anyone reading the server's log can see it, with no
  sign-in. Linear config scope: none.
- Each project's hourly Linear calls are split by kind: every charge names the job that made it (`watch.comments`,
  `watch.health`, `app_write.update`, `profile.create`, `issue_move`, …, from a closed set), with its calls and its
  charges, so calls per charge is the cost of one. `project linear-budget show` adds a `By kind` line (the top six),
  the overview's `linear_budget` carries `kinds` and `last_hour`, doctor's `linear.budget` names the top three when the
  app spent more than half the hour, and the watcher's hourly log line ends `, top: KIND N, …`. Names and counts only.
  Schema 51 → 52 (additive, no backfill). Linear config scope: none.
- The document watch lists the documents changed since its cursor with one app query instead of the provider's
  `list_documents`: the same one charged call every 5 minutes at most, about 0.1–0.3 s instead of 1–2 s. A page with
  more behind it still keeps the cursor. If Linear refuses the query, the list goes through the provider for the rest
  of the server's run. Linear config scope: none.
- A burst of more than 50 changed documents is no longer stuck: the document watch reads the oldest 50 changes first
  (whichever way Linear orders the list: it reads which end it got from the page's own order) and moves its cursor
  past them (to a millisecond below the newest it read, so a change in the same millisecond
  isn't skipped), so every change is told, 50 per five-minute poll. More than 50 in one millisecond can't move it,
  and the poll reports `burst_stalled`. Linear config scope: none.
- **Issue history: who changed an issue.** `clawbridge linear issue history ISSUE [--limit N] [--cursor C]` and the
  read-only MCP tool `taskboard_issue_history_v2` show who changed an issue and what, newest first: a person, an
  integration such as GitHub, or ClawBridge's own app, with the state, assignee, labels, team, project, priority,
  parent, archived and trashed changes (anything else reads `other change`). Typed fields only, never a title,
  description or comment text; people are named by their ClawBridge or Linear display name or their ID, never an
  email. The issue is checked in your scope first (UUID or identifier, as `linear issue`), then again by the server.
  It needs the ClawBridge Linear app on the hosted server (`app_unavailable` without it; an older server says to
  upgrade), reads 1–50 entries a page (default 20) with at most 3 Linear queries, and is charged to the caller as a
  read in the project's hourly Linear count. Linear config scope: none.

## Docs

- Linear webhooks as shipped: one deployment section for the owner and operator (a team webhook per bound write team,
  the secrets line, rotation, the gate, the probe, the post-deploy check that each webhook is still enabled, the
  doctor's states, and redacting the secrets from any `docker compose config` render), the route's answers and
  `webhook_status` in the API reference, what webhooks change in intake and the comment watch (and what a moved
  issue's deliveries do) in the Linear guide, and the skill's note that notices can come faster, are still untrusted,
  and never involve an agent with a secret. Linear config scope: none.
- The release process lists what is checked before each cut: the base image digests, and the playbooks' cost tables
  against the providers' current prices (docs/design/releases.md, "Before each cut"). Linear config scope: none.

## Security

- Builds move from Go 1.26.0 to Go 1.26.10, which fixes reachable standard-library vulnerabilities in TLS, X.509,
  HTTP and HTTP/2, `net/textproto` and `html/template`. CI and the release now run `govulncheck` (pinned), and a
  reachable finding fails the release. The PostgreSQL and Caddy images in the compose files are pinned by digest (the
  tag is kept); `docs/operations/deployment.md` says how and when to refresh them. Linear config scope: none.
- A failed device sign-in now returns one answer, `401 device_auth_failed` ("device authentication failed"), whether
  the device ID is unknown, the signature is wrong or the time is off with a wrong signature, so an unsigned caller
  can't learn which device IDs exist. The server's time (`clock_skew` with `server_time`) and the revoked answer
  go only to a request whose signature verifies. Older clients show the 401 with the message; the one corrected
  retry for a skewed clock is unchanged. Linear config scope: none.
- The server reads `X-Forwarded-For` and `X-Forwarded-Proto` only from a trusted proxy: `CLAWBRIDGE_TRUSTED_PROXIES`
  (or `serve --trusted-proxies`), comma-separated IPs, CIDRs or host names; unset trusts none, so a loopback or
  private peer that skips the proxy can no longer pick its rate-limit address or claim https. The compose files trust
  their own Caddy (`caddy-backend`, its name on the internal network only; nothing in local shape A). A server behind its own reverse proxy must set it, such as
  `127.0.0.1`, or every client counts as the proxy's address (and, for a proxy on another address than loopback,
  model and launcher requests are refused as plain http). Linear config scope: none.

## Internals

- One `internal/transporterr` package now classifies a failed request (DNS, TLS, timeout, dial) for the cloud
  checks, the launcher client and OAuth discovery, replacing three copies; each keeps its own answers. Linear config
  scope: none.

## Operations

- **Local host:** `operator bootstrap-invite` (and `deploy/local-host.sh invite-owner`) now revokes every earlier owner
  code of the same workspace that is still pending, in the same transaction and audited, and prints only how many;
  used, expired and other-role invites stay. On macOS, the printed and documented untrust steps first remove the admin
  trust setting (`sudo security remove-trusted-cert -d clawbridge-local-root.crt`), then delete the certificate.
  Linear config scope: none.

## Sign-in

- A device revoked on one server is refused as revoked by every server on its next request, before anything is
  written for it (no nonce, no last-use stamp, no dormancy warning), even while another server still has it cached.
  A change that takes access away (a device revoke, a member's removal, a project role revoke, a retired session, an
  ownership transfer, an account unbind or a narrower domain allow-list) also notifies every server through Postgres
  (`cb_access`, empty payload), which forgets its cached devices and closes the streams that lost access within
  seconds instead of at their next 30 s re-read. No schema change. Linear config scope: none.

# ClawBridge v0.3.11 (released 30 Oct)

v0.3.10 was deployed (server) but its client release failed to publish; v0.3.11 is the same code plus the publish fix.

**Schema 50 → 51.**
- The server migrates to schema 51 on its first start: the Linear webhook deliveries (IDs, kinds, times, changed-field
  names and sizes only, never values or text; kept 7 days), each team's webhook state and hourly counters, and
  `watch_cursors.hinted_at`. Additive, no backfill.
- **One-way:** v0.3.9 and v0.3.9.1 refuse a database newer than schema 50. Take a dump before upgrading; to go back,
  restore it with the v0.3.9.1 image (docs/operations/deployment.md).

## Onboarding

- **Guided setup and `device revoke` fixes.** The doctor step no longer fails with "doctor returned no report" in a real
  terminal; a member's dispatcher step prints "skipped: the project owner sets the dispatcher" instead of a 403; and
  `device revoke` of this device offers to remove its key from the system key store (or names the manual step).
  Linear config scope: none.

## Agents

- **Hooks can find their agent process without a POSIX shell.** ctxwatch now has Windows process primitives (parent
  lookup guarded against reused PIDs, liveness) and derives the agent PID from the hook's parent, skipping a shell. It
  is the groundwork for Windows wake-up, which is pending the Windows probe. No schema change. Linear config scope:
  none.
- **Windows hooks in exec form, and doctor's hook check.** On Windows, `mcp install` writes Claude Code hooks in exec
  form (no POSIX shell). Doctor reports the hook form and the Claude Code version, and warns on Windows until the
  version is probed (pending the Windows probe). No schema change. Linear config scope: none.
- **Windows wake-up adapters are tested on Windows.** The Codex and command wake-up adapters are tested on Windows
  (their tests use Go-built fakes instead of POSIX shell scripts); docs show a Windows watcher (pending the Windows
  probe). No schema change. Linear config scope: none.

## Linear

- **A queued Linear create's settled result carries the new issue's UUID.** `write-status`, the session's `linear-write`
  message and the TUI queue show `issue ENG-88 (<uuid>)`, and `write-status` adds `result_identifier` and `result_id`, so
  an agent can hand the issue on. No schema change. Linear config scope: none.
- **A dispatcher domain's coordinator keeps the coordinator's Linear rights.** While the dispatcher is on, the project's
  coordinator is its system session, so no session could move an issue, add a team to a Linear project or create a
  project as the coordinator. Now each domain's coordinator session (`dispatcher domain coordinator DOMAIN SESSION`)
  does, inside its own lane (same lane and team checks as before), and `taskboard_access` `coordinator_scope` answers
  true for it. Assignments and intake stay with the project's designated coordinator, the dispatcher.
  Each domain's coordinator session also sets a Linear project's status, but only when every team the project carries
  is a write team of its own lane (the server reads the teams at the claim; a cross-lane project, no teams or a partial
  read is `outside_lane`). That status set waits for `serve --project-status-domain-proven`
  (`CLAWBRIDGE_PROJECT_STATUS_DOMAIN_PROVEN=1`, off by default; set it only after the probe shows the app's
  `project.teams` lists a private team the app isn't in): until then it is refused at the begin with
  `project_status_domain_unproven`, and the project owner or a lead sets the status.
  No schema change. Linear config scope: none.
- **Issues take a due date (FR13).** `linear create` and `linear update` take `--due-date YYYY-MM-DD` (a real date from
  2000-01-01 to 2100-12-31), and `linear update` takes `--clear-due-date`; the create and update tools take `due_date`
  (a string, or null to clear it). The write is refused, with nothing sent, for a bad date or a Linear connection that
  can't set one, and the returned issue is checked: another date, or none reported, makes the write uncertain
  (`receipt_mismatch`). An issue move now also settles uncertain when the read-back loses a due date the issue had.
  No schema change. Linear config scope: none (issue data).
- **`linear issue` and `assignment propose` take an issue identifier such as `ENG-88`.** `linear issue ENG-88`
  (with `--resolve-labels`, `--comments`, `--comments-since`; MCP `taskboard_issue_v2`'s `id`) resolves the
  identifier by an authoritative Linear read under the binding, and refuses a stale one ("ENG-88 now resolves to
  OPS-5; use that identifier or the UUID") or one outside the binding. `assignment propose --issue-identifier ENG-88`
  (with `--linear-profile`, no `--issue-uuid`; MCP `assignment_propose` where the server has a Linear connection)
  sends both the UUID and the current identifier; with `--issue-uuid` no Linear read is made. Writes still take the
  UUID. No schema change. Linear config scope: none.
- **Linear projects take a priority, target date and start date (FR13).** `linear project update PROJECT_UUID` takes
  `--priority`, `--target-date YYYY-MM-DD` or `--clear-target-date`, and `--start-date YYYY-MM-DD` or
  `--clear-start-date`, with an `--operation-id`; agents use `taskboard_project_plan_v2`. A call adds a team or sets
  plan fields, never both. Without `--confirm` it is a preview: each field's current and planned value, who confirms and
  `plan_sha256`; it sends nothing. The project owner, a project lead or a team's lead in the team roles confirms in
  their own terminal, the coordinator session inside its lane (while the dispatcher is on, a domain's coordinator too,
  for a project whose teams are all write teams of its lane as the server's own app read shows them, after
  `--project-status-domain-proven`), or a session owned by a team's lead inside a lane holding that team; everyone else
  previews and asks. Refused with nothing sent: a start date after the target date (checked on the dates the project
  would end up with), a date Linear holds at another precision than a day, a connection that can't set the fields, and a
  project that changed since the preview. The confirm is reserved first, sends only the given fields, and is read back:
  `accepted`, `accepted_with_changes` (counted by doctor's `linear.project_plans` until acknowledged), `not_sent` or
  `uncertain` (never sent again). Off until the operator sets `serve --project-plan-proven`
  (`CLAWBRIDGE_PROJECT_PLAN_PROVEN=1`); the preview always works. No schema change. Linear config scope: project.
- Linear webhook deliveries are recorded (inert until the `webhooks` gate is proven). Linear config scope: none.
- `POST /linear/webhook/TEAM_UUID` (each team's webhook URL names its team) verifies Linear's signed deliveries
  (HMAC under that team's secrets, organization, team, a ±60 s window, then dedup on `Linear-Delivery`) for the
  teams in `CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS` (`TEAM_UUID=SECRET`, at most 2 per team; needs the Linear app; without
  it the route is a 404). The new `webhooks` gate (`CLAWBRIDGE_WEBHOOKS_PROVEN`, `serve --webhooks-proven`) is off
  until proven: deliveries are recorded inert and change nothing. Linear config scope: none.
- `clawbridge doctor` has a `linear.webhooks` check (each bound team's webhook state, with one action per problem), and
  the TUI overview a `Webhooks:` line under the intake line; "Proven Linear write gates" is now "Proven Linear gates".
  Linear config scope: none.

## Terminal interface

- **An open TUI sends what waits (FR11).** The TUI, left open, sends the writes waiting in this machine's outbox
  within seconds of the server answering again, as an agent's MCP server does: every 5 s while any wait, and at once
  after a successful call. Its status line's `outbox: N waiting` follows the count. A command line's queued notice now
  says what sends the write: the next `clawbridge` command, or `clawbridge outbox flush`. No schema change. Linear
  config scope: none.

## Operations

- **Every part of a watcher pass gets its time.** A project's share is now split so the comment watch, the steps after
  it and the claim markers each fit, in any share a pass starts with (20 s or more; a project with less left waits for
  the next tick, first in line): the comment watch stops with 15 s left, intake and the later steps run until 9 s are
  left (intake starts a source or page with 5 s), and the markers get the last 9 s, what a marker pass needs to apply a
  row. In v0.3.9.1 the markers got 3 s and stopped as `deadline` without applying anything (a nudge applied them).
  Linear config scope: none.
- **Intake's needs-agent read is one query per team.** S1 read each bound team once per label through Linear's MCP
  connection, with the label listings uncharged; it now reads each team with one app GraphQL query for the whole
  needs-agent set, and every page is charged. A large binding's intake drops from about 14 s to about 1 s. If Linear
  refuses the set's filter, S1 reads one query per label for the rest of that pass. Linear config scope: none.
- **Each release publishes the server image.** It goes to `ghcr.io/jkai-chen/clawbridge:vX.Y.Z` for linux/amd64 and
  linux/arm64, behind the same release approval as the binaries.
  - **Naming:** the release page names it by digest, and `IMAGE-DIGEST.txt` carries the digest.
  - **Verifying:** it has a build provenance attestation (`gh attestation verify oci://…`) and an SBOM.
  - **Using it:** `deploy/compose.image.yaml` pulls exactly that digest (`CLAWBRIDGE_IMAGE`), with no source or build
    on the server. Rolling back means setting the previous release's digest.
  - **From source:** building from source stays as the fallback.
  - **Base images:** both are now pinned by digest.
  - **Version stamping:** the release image reports its tag, commit and commit time in `clawbridge version`.
- **Run the server on your own machine.** `deploy/local-host.sh up --owner EMAIL` builds and starts the server
  for this machine only on `http://127.0.0.1:8443` (device sign-in, no Caddy, no Google settings), writes a private
  env file once, and shows the owner's one-time code in the terminal; `up --lan HOST [--bind IP]` serves
  `https://HOST` through Caddy's internal CA (`trust` exports its root), or a brought mkcert or Tailscale certificate
  with `--cert`/`--key`. `deploy/compose.local.yaml` holds both shapes; `status`, `down`, `backup` and
  `uninstall [--delete-data]` manage them. `deploy/backup-postgres.sh` writes to a local folder with
  `CLAWBRIDGE_BACKUP_DIR` (14 days kept by default) and takes the Compose project and an override file as
  parameters; the bucket path is unchanged. `operator bootstrap-invite --server URL` puts the server's real URL in
  the join command it prints (it said `https://YOUR-DOMAIN`). Guide: docs/operations/local-host.md. No schema
  change. Linear config scope: none.
- **`clawbridge cloud check --local`** checks a server you run on your own machine with `deploy/local-host.sh`
  (`--local-dir DIR`, default `$CLAWBRIDGE_LOCAL_DIR` or `~/.clawbridge-server`; the URL comes from its env file when
  `--server` is left out). It reads the env file's settings and its Compose project's containers on this machine,
  `infra` passes as `not_applicable`, a new `trust` check (`--local` only) fails `untrusted_ca` when this machine
  doesn't trust the https certificate, and `backups` warns when the newest dump in `DIR/backups` is over 26 hours old
  or has no checksum. Remote checks are unchanged. No schema change. Linear config scope: none.
- **Onboarding can host the server on this machine or your LAN.** Start's "Set up my own server" now asks "Where?"
  with a third answer, "This machine or my LAN (no cloud)": just you at `http://127.0.0.1:8443`, or your team at
  `https://HOST` on the LAN. With Claude Code or Codex it hands `deploy/local-host.sh up` to your agent with the new
  `clawbridge-provision-local` playbook (`clawbridge skill install --playbook local`), then `cloud check --local`
  decides, and you make the owner's sign-in code in your own terminal. Refused on native Windows (use WSL). No schema
  change. Linear config scope: none.

## Install and upgrade

- **`clawbridge self-update` (alias `upgrade`) (FR12).** Replaces the running binary with a GitHub release, with the
  installers' checks (the release's `SHA256SUMS` lists the asset once and the download matches it; the new binary
  reports the release's version before it replaces anything), then re-runs `skill install` with the new binary
  (`--no-skill` skips it). `--check` reports the plan and changes nothing, and an agent may run it; `--version vX.Y.Z`
  is the only way to an older release; `--verify-attestation` or `CLAWBRIDGE_VERIFY=required` checks the build
  provenance with `gh`. In an agent's shell it refuses without a terminal, and at one asks the person to type the
  version. It needs no profile or sign-in, and tells you when your hosted server runs an older release than the
  client now does. Windows renames the old binary aside. Running `clawbridge mcp` servers keep the old version until
  their session restarts (docs/installation.md, "Upgrade"). Linear config scope: none.
- **Windows on ARM is supported.** Releases include `clawbridge_<version>_windows_arm64.exe`, and `install.ps1` picks
  it on an ARM64 machine, even from an emulated x64 PowerShell. No schema change; Linear config scope: none.
- **A release publishes with its own notes.** The GitHub release body is now that release's top section of
  RELEASE-NOTES.md, a link to the full notes at its tag, and the server image, instead of the whole file, which had
  grown past GitHub's 125000-character limit and failed v0.3.10's publish. The body is checked at 120000 characters
  before any release is created, and every PR and main CI run checks today's RELEASE-NOTES.md the same way
  (`scripts/release_body.py`). Linear config scope: none.

## Dispatcher

- **A locked-out address costs the model API nothing (G9-2).** After 10 failed authentications in a minute, an address
  (an IPv6 client's /64) or token prefix gets 429 for 10 minutes before its request body is read or its token looked up.
  Before, each refused attempt still cost a body read and a database transaction. A token that authenticated in the last
  10 minutes, such as a running agent's, still gets through, and so does a credential the server has just handed out
  (agentd's mint for a restarted agent, the project tokens its agent redeems), so a dead agent that keeps failing on
  agentd's address can't keep the next one out; any other token new to the server waits out the lockout.
- **A former owner can delete their own model key.** `dispatcher_model_key_delete` needed the project owner role, like
  the rest of model setup, so a key-holder who was no longer an owner couldn't remove their sealed key. Any project role
  may now delete its own key, and only its own; setup, status and doctor stay the owner's.
- **A project owner's `dispatcher show` includes their model setup.** A `model` block shows the project's model use,
  the owner's provider and model, and their key's and agent's state. A new read-only `dispatcher_model_config` returns
  the owner's own active and pending setups, never the key, for key rotation. Other members see neither.
- **`clawbridge dispatcher model key rotate` changes only your key.** It keeps your stored provider, model and caps,
  asks for the new key at the hidden prompt (or `--key-stdin`), and tests it like a new setup; the old key keeps running
  until the new one passes. The disclosure is asked again only if its text changed. In the TUI, `c` on the Dispatcher screen
  does the same.
- **The TUI's Dispatcher screen says how the model is used, and with what.** It shows whether the model decides claims or
  advises the rules in this project, and your provider and model, from the dispatcher's own status.
- **The dispatcher agent reaches the network only through its egress proxy.** It needs `CLAWBRIDGE_EGRESS_PROXY`, sends
  every request (to ClawBridge and to the model provider) through it, and refuses any direct connection or name lookup,
  logging `egress_refused` or `egress_unavailable`.
- **agentd frees a leftover owner network on Docker 29.** Docker 29 answers a disconnect of a container that isn't on
  the network with a 500, so agentd never removed a leftover owner network, and never freed its subnet, every round.
  It now inspects the network and disconnects the egress proxy only when it is on it. CI also installs and probes
  agentd under a real rootless daemon. No schema change. Linear config scope: none.
- **One model agent by default, sized from measured numbers.** `CLAWBRIDGE_DISPATCHER_MODEL_MAX_AGENTS` now defaults to
  1, not 2: agentd's rootless Docker, proxy and egress container take about 260 MiB once, and each agent about 170 MiB,
  so two agents left a host with about 1 GiB available no room. `clawbridge doctor` counts agentd's own cost while it
  isn't running yet, warns when the agents that may still start would leave less than 300 MiB for the server and its
  database, and says how to raise the cap on a bigger host.

## Cloud and onboarding

- **`cloud check infra --ssh HOST` checks a host with no cloud API to ask.**
  - **Fails on:** the platform (64-bit Linux, amd64 or arm64), Docker with Compose v2.24 or later, the env file's mode,
    the data volume, free space, the clock, DNS and 443. Each has its own reason.
  - **Warns on:**
    - a database the owner chose to keep on the root disk (`CLAWBRIDGE_DATA_ON_ROOT_DISK=1`);
    - a domain whose addresses aren't on the host (normal behind NAT).
  - Before, `infra` needed `--vm`.
- **`--ssh` is safer.** It never prompts and never accepts a host key for you. Your `~/.ssh/config` can no longer turn
  host-key checking off or forward your agent, X11 or ports to the server.
  - **New reasons:** a host key ssh can't verify is `host_key_unknown`, and a sudo that wants a password is
    `sudo_needs_password`.
  - **Also covers:** `cloud check`, `cloud secret set` and `cloud bootstrap` alike.
  - **What ssh prints** reaches your terminal with control and bidi characters escaped, and sudo runs in the C locale,
    so its refusal reads the same in any language.
- **`cloud bootstrap --ssh HOST` lets the owner in on a server you reach over SSH.**
  - It makes the one-time code on your computer and sends it only on ssh's stdin to the server's
    `operator bootstrap-invite --code-stdin`.
  - It joins this device with the code, as `--instance` does on AWS.
  - A server that pulls the published image (`CLAWBRIDGE_IMAGE`) bootstraps with `compose.image.yaml`.

# ClawBridge v0.3.9.1 (server hotfix, deployed 29 Oct; no client release or tag)

A server-only hotfix to v0.3.9: no schema change and no client change.

## Operations

- **A large binding gets a larger share of each watcher tick.** A project's share is 20 s plus 2 s for each bound
  team or Linear project past four, up to 40 s, and never more than the tick has left; a project the tick has under
  15 s left for waits, first in line, for the next tick instead of starting a pass the tick would cut. A slow pass's
  log line names its share (`took …ms of its …ms share`), and a tick that defers projects says how many. Linear config
  scope: none.
- **Intake no longer fails a large project's tick.** Intake reads its sources (S1 per team, S2, S3) oldest cursor first
  and starts a source, or its next page, only with 8 s of the project's share left; one it can't start keeps its cursor
  and goes first next tick (`deferred_sources`), instead of the project failing as `deadline`. The calls a cut read made
  are still charged. The steps after the comment watch leave the last 3 s of the share to the claim markers, which a
  slow intake used to starve. A slow pass's log line splits intake by signal (`intake …ms (S1 …ms, S2 …ms, S3 …ms)`).
  Linear config scope: none.

## Linear

- **A family move's preview no longer shows a sub-issue count beside its children.** `sub_issues` is Linear's count of
  one page (at most 10), so a family of 12 showed `sub_issues: 10` next to 12 planned children; a family's preview now
  leaves it out, and its `children` list is the count. No logic change. Linear config scope: none.

# ClawBridge v0.3.9 (released 29 Oct)

**Schema 47 → 50.**
- The server migrates to schema 50 on its first start: 48, the server drain's maintenance row and its instances'
  reports; 49, the shared operation ledger's rows name their ClawBridge project and their person
  (`linear_write_ledger.project_id`, `principal`, `acknowledged_at`, `pending`; rows from before have neither and
  aren't listed); 50, the hourly Linear count's split (people and agent sessions).
- **One-way:** v0.3.8 refuses a database newer than schema 47. Take a dump before upgrading; to go back, restore it
  with the v0.3.8 image (docs/operations/deployment.md).

## Operations

- **Drain the server before a deploy's swap.** `operator maintenance start --reason TEXT --wait 90s --confirm` makes
  every server answer each write with 503 `maintenance` and `Retry-After` (reads still answer), close its streams with
  `event: maintenance` then `bye`, and pause its watcher, sweeper, Linear write outbox and dispatcher model; it prints
  `drained` once every live server has nothing in flight, or names the ones that haven't. The new server's start ends
  it; `maintenance end` and a 15-minute TTL (at most 60) do too, and the workers resume 90 s later, after every live
  session has checked in. `maintenance show` prints the state. On SIGTERM `serve` now waits up to 75 s for in-flight
  requests (was 5 s), and compose gives it 90 s. Older clients see a 503 as today. The first deploy of this release
  can't drain (the running server has no drain); later deploys can. Linear config scope: none.

## Messages survive a server deploy

- **The client keeps what it couldn't send.** On a hosted server, a `send` or `reply` (kind message or reply, without
  coordination), an `ack` and a `mark_read` that can't reach the server, or that a draining server refuses, are
  retried for 15 s, then kept in this machine's outbox (`outbox.db` beside the mirror, mode 0600) and answered
  `outcome: queued_local` (a mark-read, which has no operation ID, is named by its messages). They are sent in order,
  exactly once under their own operation ID, when the server is back: by the agent's MCP server within seconds, before
  any `clawbridge` command or TUI start, or with `clawbridge outbox flush`. Every other write fails fast as before.
  Bounds: 1000 writes and 8 MiB; 24 h, then a dead letter (docs/usage.md, "Messages while the server is away").
- **`clawbridge outbox list [--dead] | flush | retry ID | drop ID --confirm drop`**; doctor's `client.outbox` (warns
  past 10 minutes or half full, fails when full or on a dead letter from the last day); `whoami`'s `client_outbox`;
  the TUI's `outbox: N waiting`. A message whose ack still waits locally isn't handed to the agent again (marked
  `ack_pending_locally`). `CLAWBRIDGE_CLIENT_OUTBOX=off` turns it off.
- **Clients wait out a drain.** `maintenance` is definitive (nothing was sent): the stream reconnects after
  `retry_after` with no error, the lease loop pauses, the MCP server retries a `linear_write` once, and doctor shows
  `server.maintenance` with when it ends (servers mark reads they serve during a drain with
  `ClawBridge-Maintenance-Until`). The server's refusal of a session's earlier instance now names it. No schema change.
  Linear config scope: none.
- **Chaos cases for a deploy.** CI's chaos job also runs the zero-loss cases against real PostgreSQL: a server killed
  after its commit or mid-transaction, a drain then a swap with 100 sends from 3 sessions, a client killed mid-drain,
  two drainers on one profile, acks and replies that wait past their lease, a stream through a maintenance, and a
  Linear write the server queued, held through the drain and sent once by the new server. Each write lands once and in
  order (docs/development.md).

## Joining

- **First-run messages point an invitee the right way.** Bare `clawbridge` with no terminal names the invite link form
  (`clawbridge join`, paste the link) before the code form; `doctor` on a machine with no profile says "No ClawBridge
  profile yet: run `clawbridge join` or `clawbridge`" instead of "Run local setup"; and a join to an unreachable server
  says "cannot reach HOST: check the invite link's address and your network" instead of naming OAuth discovery. Text
  only. Linear config scope: none.

## Linear

- **A board listing no longer fails on another team's issues.** Linear lists another team's issues under a team
  filter: a parent team's filter includes its sub-teams' issues, and a project that holds several teams lists them
  all. Since a binding's projects gained more teams, `clawbridge linear issues` failed on the first such page with
  `scope_mismatch`, and the watcher's needs-agent intake (S1) would have failed the same way for a labelled sub-team
  issue. Such a row is now dropped and counted (`filtered_other_team`), as the board was built to; a row outside the
  asked project is still refused. No schema change. Linear config scope: none.
- **A large project keeps its watcher ticks.** The comment watch polled due issues until the hour's budget ran out,
  never the clock, so with a large budget (or none) and many watched issues it took a project's whole 20 s share of
  the tick: intake, health and the rest were cut, and every tick failed as a bare `failed`. It now stops with 10 s of
  the share left (`comments_stopped: deadline`) and the next tick goes on with the issues left due. Each later step
  stands alone: one that fails is recorded and the next still runs. A share that runs out is recorded as `deadline`,
  and a poll it cut still reports, so its calls are charged and the next tick doesn't redo the same work. Verifying a
  binding whose projects hold several teams no longer reads each such project again: Linear may list its teams in
  another order under each team. The watcher reuses a verified binding for 20 minutes, across ticks, for its reads (a
  relink or a failed check drops it; writes always verify), and a pass that takes 5 s or more logs each step's time.
  No schema change. Linear config scope: none.
- **Doctor's `linear.writes` covers every recorded Linear write.** It names each project create, issue move or team
  add that may have happened unconfirmed (unsettled after 10 minutes, or settled `uncertain`) by kind and state, until
  the owner runs `clawbridge linear writes --ack`. The check was `linear.project_creates`: that ID stays in the
  check's `aliases` for one release, and the project overview gains `linear_writes` (its `project_creates.unconfirmed`
  stays too, until v0.3.10). No schema change. Linear config scope: none.
- **Writes on issues in no project no longer end uncertain.** Linear's answer to a create, update or move of an issue
  in no project (a `team_board` binding's issues) leaves `project` out, which ClawBridge read as missing scope: the
  write landed but came back `uncertain` (`scope_metadata_missing`), and a move settled so for good. The issue is now
  read again by UUID and that read is checked. A move is read back before it settles, also after an error answer:
  `accepted` when the read shows it as planned, `not_sent` only when Linear refused it and the issue is still in its
  team, else `uncertain`. No schema change. Linear config scope: none.
- **Moving a family, and many issues at once.** Linear moves a parent's sub-issues with it, so `linear move ISSUE
  --team TEAM --family` (MCP `family: true`) moves a parent with all of them: one write, on the parent. Without
  `--family`, a parent is refused and named for it; a sub-issue alone is refused as before. Each child is planned as
  a single move would be (state and labels mapped, at most 50 children, none already moved, in another team or
  project, or with children of its own) and read back after, recorded per child on the server; a child that didn't
  follow makes the family `uncertain`, with the move-back listed, and a refusal is `not_sent` only while the parent
  and every child are still in their team. `linear move --file moves.json` (CLI only) previews up to 100 issues,
  children counted, then moves them one by one and stops at the first that isn't `accepted`; the same
  `--operation-id` resumes, and an `uncertain` item holds it until acknowledged (and, if it didn't move, until it is
  moved on its own under a new operation ID). Every confirmed move, single or family, is charged to the project's
  hourly Linear budget with its record, before anything is sent: no room answers a session `rate_limited` with when
  to retry, while a person with no session is charged past the limit and never refused. Agents loop over the single
  move instead. **Off until proven:** family moves also need `serve --issue-family-move-proven`
  (`CLAWBRIDGE_ISSUE_FAMILY_MOVE_PROVEN=1`). No schema change. Linear config scope: none.
- **Which PROVEN gates are on.** The project owner's `clawbridge doctor` reports the server's PROVEN gates as
  `linear.proven_gates` ("Proven gates: issue_subscribers, issue_move (off: project_flow, issue_family_move,
  project_team_add)."), and the TUI's Overview shows them as Proven gates and Not proven. The project overview gains
  `proven_gates` for the owner: the five gate names, each on or off, and nothing else of the server's settings. No
  schema change. Linear config scope: none.
- **Bind a team before any project carries it.** `linear link --team-without-project TEAM` (repeatable) binds a write
  team of a project set that none of its projects carries yet, so `project update --add-team` can add a brand-new team
  without a step in Linear's UI. Until a bound project carries it, the team is inert: no issue of it is read, every
  issue write in it is refused with `team_without_project` (nothing sent), and intake skips it. Doctor
  (`linear.teams_without_project`) and the TUI show "T: no project yet (inert)". Once a project carries it, it works
  from the next read with no relink; its entry shows as stale until the next link drops it. A team not named so is
  still refused at link when no project carries it. Such a binding is version 4: a v0.3.8 client refuses it
  ("mapping_version must be 2 or 3") rather than half use it, so upgrade every client first. Before downgrading the
  server below v0.3.9, relink without --team-without-project. No schema change. Linear config scope: none.
- **A family's acknowledgement covers its sub-issues; a lead acknowledges their own moves.** `linear writes --ack
  OP_ID` on a family move's operation ID acknowledges the parent and every child still unconfirmed (a crash before the
  settle, or a child whose read-back failed), and lists them; a child's own ID still acknowledges it alone. The project
  owner acknowledges any recorded Linear write; a project lead now acknowledges a move they confirmed themselves, from
  their own terminal; never another lead's, a coordinator's, or from a session. History records the IDs only. No schema
  change. Linear config scope: none.
- **`linear project-status` is refused with the command to type, never sent as an issue write.** The two-word
  commands' internal names (`project-status`, `project-update`, `project-creates`, `project-team-adds`,
  `initiative-show`, `view-*`) were accepted as commands and fell through to an issue write, so `linear project-status
  UUID` failed with "write requires operation_id". They now answer with the spaced form (`linear project status
  LINEAR_PROJECT [--suggest]`), and a command the dispatch doesn't know is refused as unknown. The documented forms are
  unchanged. Linear config scope: none.
- **Uncertain issue writes are listed, and acknowledged.** An issue create, update, comment or relation that may have
  reached Linear was held in the operation ledger, but nothing listed it: only the command's own answer said
  `uncertain`. `clawbridge linear writes` now lists them under `issue_writes`, from this machine's ledger (every
  session on it; the first 500 records) and from the server's (your own, or all for the project owner); doctor's
  `linear.issue_writes` warns while one is listed, and the overview counts the server's. `linear writes --ack OP_ID`
  hides one you checked in Linear (not one still being sent); a retry of the same operation ID still sends nothing. A
  ledger record now names its operation ID, its operation and its project, never a title or body; older records are
  counted, with the folder to clear. Linear config scope: none.
- **No ClawBridge limit on Linear calls by default; Linear's own rate limit is respected through the queue.** A
  project no longer has a 600-an-hour cap: every Linear call is still counted per hour (the watcher, project status
  and health, app writes, and now every write made with a person's own profile, by its real calls), and each
  credential's pacer waits out Linear's 429 and its Retry-After; a tick polls at most 100 issues. Nobody's write or
  move is refused for the count: an agent session's and a person's are counted after. A cap stays opt-in (`project
  linear-budget set N`, or the server's `CLAWBRIDGE_LINEAR_BUDGET`, whose old name `CLAWBRIDGE_WATCH_BUDGET` is
  deprecated): **a server whose env still sets `CLAWBRIDGE_WATCH_BUDGET=300` keeps that cap until the line is
  removed**, and `project linear-budget clear` removes a project's. Doctor's new `linear.budget` ("no ClawBridge cap"),
  the project overview's `linear_budget` (`limit` 0, `capped` false, `source`), `project linear-budget show` and the
  Linear screen's header ("(no cap)") split the hour between the app, people and agents, as counts. Schema 50.
  Linear config scope: none.
- **A team bound after the status policy was set is now named.** Doctor's `linear.status_policy` warns, a move into it
  says so, and its skipped state moves record `team_not_in_policy` (before, claims and done-releases there silently left
  the state alone). The project owner includes it with `clawbridge linear status-policy set` and the file
  `{"enabled": true}`. Linear config scope: none (ClawBridge project config only).
- **`status-policy set` keeps every key the file leaves out.** Closes, state names, `project_flow` and
  `project_targets` are no longer reset by a file that doesn't mention them; before, re-running it with an older file
  cleared every agent close rule and done state. A key you include still replaces the current value, and the answer
  lists `kept`. From an agent's shell a file that names agent close rules is refused, and `closes set --agent` warns
  when a team has no done state. Linear config scope: none (ClawBridge project config only).

## Docs

- **The skill and docs as shipped in v0.3.9.** A newly joined agent's first steps (register, doctor, lane and team,
  inbox, then work); the claim message's required `intent`; who closes and that agents never cancel; recovery rows for
  the client outbox, the deploy drain, teams with no project yet, family moves and batch stops; the owner's lanes and
  team-roles steps for a newcomer, invites first, and project role vs team role; Windows (no wake-up: check the inbox
  every turn) and where `mcp install` writes; api.md rows for moves, binding version 4 and `whoami`'s
  `client_outbox`, and the uncertain issue-write list and its acknowledge; a v0.3.9 upgrade note. Docs only. Linear config scope: none.

# ClawBridge v0.3.8 (released 29 Oct)

**Schema 41 → 47.**
- The server migrates to schema 47 on its first start: 42, the agent writes the server queued (the Linear write
  outbox); 43, the team roles; 44, the project lead settings; 45, the project lead notices; 46, the lane pins;
  47, the Linear write reservations (a coordinator's project creates, and later kinds).
- **One-way:** v0.3.7 refuses a database newer than schema 41. Take a dump before upgrading; to go back, restore it
  with the v0.3.7 image (docs/operations/deployment.md).

## The terminal interface

- **Issues and the sessions holding them (U15, U40).** `i` on Sessions opens the selected session's issue on the
  Linear screen; Linear's issue list says which session holds each issue ("held by reviewer"), and `s` on an opened
  issue goes to that session. Projects reads the first 20 projects' summaries on load and any other when it's opened.
  Linear config scope: none.
- **Relinking starts from the board (U33); the Dispatcher speaks plainly (U55).** `l` on a linked board opens the
  link wizard with its teams, default team and projects already chosen. Without the server's model key, the Dispatcher
  screen says the server isn't set up for model keys (operator: see docs/dispatcher.md) instead of naming the
  operator's variable. Linear config scope: none.
- **Doctor in the TUI answers like `clawbridge doctor`.** Each check reads "Workspace: Your workspace can't be
  reached." with the check's own message under it, the same words as the CLI (U60). Linear config scope: none.
- **Linear project status in the TUI.** `p` on the Linear screen reads each linked Linear project's status and
  open issues through the server's app (`g` adds a suggestion, as a hint). A project owner or lead, in a TUI opened
  without a session, sets one after the server's preview and `y`, with a second `y` to close it with issues open.
  The server's rules on who sets it and who closes stay as they are (U61). Linear config scope: one bound project's
  status; nothing at team or workspace scope.
- **A project's threads in the TUI.** On Projects, `t` on a project you own lists its threads (newest activity first,
  who's in each) and Enter reads one: each message, whom it went to and who read it; `]` reads older threads or more
  messages. Read-only, with the rules of `clawbridge project threads`: as you (not in a TUI opened with `--session`
  or run by an agent), from your own device on a hosted server, and recorded in the project's history (U62). Linear
  config scope: none.
- **Shared Linear views in the TUI.** `v` on the Linear screen lists each bound team's shared views, issue and
  project, by name and kind, with the selected one's URL, read with your Linear connection as `linear view list`
  does; workspace views and other kinds are counted, not listed. Read-only: a project owner writes views in their own
  terminal with `clawbridge linear view create|update|delete` (U63). Linear config scope: none.
- **The help fits small windows.** When `?` doesn't fit, the screen's description and the screens' names go first and
  the keys used everywhere run on one line, so every key is still listed; then the keys not usable now become a count
  ("12 more not usable now"), so `q quit` and `1-8` stay in view down to 80x24. A window tall enough shows it all, as
  before. Linear config scope: none.

## Linear

- **Issue subscribers stay in the binding.** A project set's backfill, and every subscribe, now count only issues in
  the binding's own projects; before, another binding's project in the same team was listed too (both read the whole
  team). A team board is unchanged. The live check (`linear-app-probe --subscribe`) no longer needs an empty fixture:
  the creator Linear subscribes is counted, not refused; it refuses only a target already subscribed, and fails if
  anyone else is added or gone. No schema change. Linear config scope: none (reads only).
- **ClawBridge paces its Linear calls per credential** (the server's app, your Linear connection, your API key),
  following Linear's rate-limit headers, so it rarely meets a 429. A call with no room waits (reads up to 20 s, writes
  up to 5 s) instead of failing at once; past that, nothing is sent and the answer says when to retry. The app's reads
  now say `rate_limited` with a retry time, where they said `unavailable` or `query_failed`, and an API key's say how
  long to wait (docs/linear.md, "Linear's rate limits"). Linear config scope: none.
- **Agent writes queue instead of failing** on a hosted server with the ClawBridge app. A write that Linear's rate
  limit or the project's hourly budget holds, or one behind a queued write on the same issue, is queued: the answer
  is `outcome: queued` (not an error), the session gets a `linear-write` message when it ends, and `clawbridge linear
  write-status` lists it. Repeat the same operation_id to ask; never use a new one. Clients older than this release
  are never queued and get `rate_limited` with when to retry (docs/linear.md, "Queued writes"). Linear config scope:
  none.
- A queued write is sent only while the person who asked still may: their session (if any) active, their role
  held, their device live or their Google account still bound. Otherwise it fails as `identity_revoked` and nothing
  is sent. Linear config scope: none.
- **Agent writes made as the ClawBridge app now count against the project's hourly Linear budget**, whose default
  rises from 200 to 600; writes through a person's own Linear profile aren't charged. Its names are now `project
  linear-budget` and `CLAWBRIDGE_LINEAR_BUDGET`; the old ones keep working. An owner may raise a project's budget up
  to the server's `CLAWBRIDGE_LINEAR_BUDGET_MAX` (default 2000), where 600 was the most; the server refuses to start
  when that ceiling is below the default budget. Project status and a forced health run that meet a spent budget now
  say when to retry. Linear config scope: none.
- **Upgrade note:** the 600 now covers agent writes as well as watch, health and status reads. A project that set its
  budget by hand, or a server with `CLAWBRIDGE_WATCH_BUDGET` set, keeps that number, which now has to cover writes
  too; check `clawbridge project linear-budget` after upgrading and raise it if agents write a lot. Linear config
  scope: none.
- **Doctor and the TUI show the Linear budget and the write queue.** A new `linear.outbox` check warns when the
  oldest queued write has waited 15 minutes and fails at an hour, or when a write was given up in the last day;
  `linear.watch` always shows the hour's calls used and the limit; `linear.instances` warns when more than one server
  shares the database, since each paces Linear on its own. The TUI's Linear screen header reads `Linear: 143/600
  this hour · 3 queued (oldest 6 min)`; `o` lists the queue, and the project owner sets the budget with `b`.
  `clawbridge linear write-status --project-wide` lists the project's queue (docs/linear.md, "Queued writes").
  Linear config scope: none.
- **Sub-teams and labels.** A sub-team holds its own copy of each parent team label, under a new ID, and its issues
  may carry either. Intake's `needs-agent` (and the proof, for a write credited through the app, that the label is on
  the issue), the markers' `agent-working` and agent label writes now use the team's own label plus its bound
  parent's. `issues --label` with the parent's label reads a bound sub-team that has no copy. A new doctor check,
  `linear.labels`, shows each bound team's two label sets. Before, a sub-team issue carrying the parent's label was
  never polled, and its marker label never removed. No schema change. Linear config scope: none.
- **A binding can list teams agents only read or comment on (binding version 3).** `linear link` takes
  `--read-team`, `--comment-team` and `--linked-team` beside `--team`, the teams agents write, and `--parent-team`,
  which Linear must confirm is every other listed team's parent. A write to a team that isn't a write team is refused
  before anything is sent (`read_only_team`). A comment team takes agents' new comments only. A linked team is never
  read in full (`linked_only`). Clients older than this release refuse such a binding, so upgrade them before
  relinking with it. No schema change. Linear config scope: none. **Rollback:** an older server reading such a
  binding sees only its write teams, which is more restrictive, never less. Relink without the new flags before
  downgrading clients.
- **Lanes: which write teams each session writes.** `linear link --lane NAME=TEAM,…` splits a binding's write teams
  into lanes, and `--unpinned` sets the lane of a session nobody pinned (`read_only` by default). The project owner
  pins a session or a principal to a lane with `linear lanes pin`, in their own terminal; a session pin binds the
  principal it was set for. A write to a write team outside the session's lane, through a profile or the ClawBridge
  app, is refused before anything is sent (`outside_lane`), and again inside the write lock if the pin changed.
  Relations need the caller's own end in the lane. Writes without a session follow the caller's principal pin, so with
  `read_only` the owner pins their own principal before writing from the CLI. A queued write is checked again at send
  by its session's lane as pinned then. Every listed team stays readable, comment teams take comments from every lane,
  and markers and server-side writes aren't filtered. An agent's `context` shows its lane. Schema 46: the lane pins.
  Linear config scope: none. **Rollback:** relink without `--lane`; every session then writes every write team, as
  before.
- **The coordinator creates Linear projects in its own team.** The project's designated coordinator session may run
  `linear project create NAME --team T --kind K`, through its person's own Linear profile, in its lane's default team
  only (the binding's default team without lanes), with a seeded project kind whose label the project gets in the same
  create. No lead is set (the project owner can set one later with `linear roles apply-leads`). The server reserves
  the create before anything is sent and settles it with the project's ID after; the owner's sessions get a
  `project-created` message and the overview and history show it. A create left unsettled for 10 minutes shows as
  unconfirmed in the overview, `linear project creates` and doctor (`linear.project_creates`), with one message to the
  owner, who acknowledges it with `linear writes --ack` (or `project creates --ack`). Records are kept 90 days. The
  owner's `project create` takes `--kind` too. No cap; archive and delete stay the owner's, in Linear. Schema 47: the
  reservations, one ledger (`linear_write_reservations`) for every Linear write the server records before a client
  makes it, by kind; later kinds join it without a schema change, and doctor names each unconfirmed write's kind, an
  `uncertain` one too. Linear config scope: one project in a bound team, with a label of the project's kind; nothing
  at team or workspace scope. **Rollback:** unset the coordinator designation (or the project kinds); the owner's
  create is unchanged.
- **Moving an issue to another bound team.** `clawbridge linear move ISSUE --team TEAM` (MCP
  `taskboard_issue_move_v2`) previews the move: the state by name, else by its one state of the same type; each
  label by name to the target team's set (its own, its bound parent's, the workspace's); the cycle cleared; the
  estimate kept. It refuses, and lists, a label or state it can't map, a project without the target team, and an
  issue with a parent or sub-issues (Linear moves sub-issues with their parent; families wait for a batch command).
  The project owner or a project lead (the ClawBridge project role), from their own terminal, or the project's
  coordinator session between its own lane's teams, confirms with `--operation-id ID --confirm
  --expected-plan-sha256 SHA`: the server reserves it, one write goes through the person's own Linear profile, and
  the issue is read back by UUID, `accepted` or `uncertain` field by field, never retried. The stored identifier of
  its claims and intake entry follows; the old identifier keeps opening the issue in Linear. **Off until proven:** a
  server confirms no move until its operator sets `serve --issue-move-proven` (`CLAWBRIDGE_ISSUE_MOVE_PROVEN=1`)
  after one Sandbox move through the command reads `accepted`; previews work without it. No schema change: a move is
  recorded with the other Linear writes, as kind `issue_move`. Linear config scope: none (issue writes in bound
  teams). **Rollback:** unset the flag; nothing else changes.
- **Add a bound team to a Linear project from ClawBridge.** `clawbridge linear project update PROJECT --add-team TEAM`
  and the coordinator's `taskboard_project_add_team_v2` add a bound write team to a project of the binding, so an
  issue can move there, without Linear's UI. The project owner, a lead, or the added team's lead in the team roles (an
  active, non-guest user) confirms from their own terminal, or the designated coordinator for its own lane's team, to
  a project that already holds one of its lane's teams. It is previewed, reserved first in the Linear write ledger,
  sent as `addTeams` by UUID only (never a name, never a removal, no other field), and read back before it is
  recorded: `accepted`, `accepted_with_changes` (Linear changed other fields too: named, counted in doctor
  `linear.project_team_adds` and the overview until acknowledged with `linear writes --ack`, never undone), `not_sent`
  (Linear refused it) or `uncertain` (still missing after re-reads at 2 s and 6 s); a lead or members the read leaves
  out are named `unverified`. Off until the operator sets `serve --project-team-add-proven` after the probe. No schema
  change. Linear config scope: project.

## Team roles

- **Step 1: ClawBridge keeps who leads each bound Linear team, and each person's role** (`lead`, `engineer` or
  `reviewer`), because Linear makes every workspace admin a team owner. `clawbridge linear roles
  show|set|person|lead`; the MCP tool `team_roles_get` reads them. The project owner changes them, and a team's lead
  changes engineers and reviewers in their teams (below), in their own terminal, at the revision read, previewed
  first. People are active, non-guest users of the Linear workspace (never a team's membership). History counts only.
  Doctor warns about a team that left the binding or a person who is no longer active. Linear project leads and intake
  use the roles (below), and a team's lead may add their team to a project (under Linear). Schema 43. Linear config
  scope: none (it reads the workspace's users only).
- **A team's lead sets `engineer` and `reviewer` roles** for people who work only in the teams they lead, with the
  same `clawbridge linear roles` commands, preview and revision. The server enforces the scope: a lead never changes
  a lead (their own row included), a team, or a person's ClawBridge account link. The owner links the lead's row to
  their account first (`--link`); only the owner sets leads. History records whether the owner or a lead made the
  change. No schema change. Linear config scope: none (it reads the workspace's users only).
- **Team roles in the TUI (`R` on the Linear screen).** Each bound team's lead, engineers and reviewers by Linear
  name. The project owner adds and removes leads and people, and a team's lead changes engineers and reviewers in
  the teams they lead. Each change is the server's preview first, then `y` at that revision. It's read-only for
  anyone else and in a TUI with a session or run by an agent. Linear config scope: none (ClawBridge only).
- **The "Team roles (ClawBridge)" Linear document: `clawbridge linear roles publish`** writes the team roles as one
  Linear document for people to read, under a bound team or project you name the first time. It shows each team's
  leads, engineers and reviewers by Linear display name, never an email, an ID or a ClawBridge account. The project
  owner publishes it through their own Linear connection, previewed first. It is generated: a publish overwrites an
  edit made in Linear. ClawBridge writes only the document it created, by its stored ID, and `linear doc publish`
  and the roles document never write each other's documents. The project's deny patterns apply. `clawbridge doctor`
  warns when the document was edited in Linear, is behind the roles, or was archived. Nothing publishes it on its
  own yet. No schema change (it uses the document publish ledger). Linear config scope: one document under a bound
  team or project; nothing at workspace scope.
- **A Linear project's lead from the team roles.**
  - **`clawbridge linear roles project-lead on`** (the project owner) lets `linear project create` set a new
    project's lead to its team's one mapped lead. `--lead` picks one when the team has several.
  - **`clawbridge linear roles apply-leads`** previews, then sets, the lead of each bound project that has none. A
    project with another lead changes only when you name it with `--replace`.
  - It all goes through the owner's own Linear connection. It sends only the lead's Linear user ID, re-reads each
    project before writing, and reads the lead back. History records the outcome and member counts, never who.
  - **Linear also makes the lead a member of the project, and clearing or replacing the lead doesn't remove them.**
    ClawBridge never removes members. Linear doesn't notify the person either, so ClawBridge sends a `project-lead`
    notice to their sessions when their lead row is linked to their account. The preview says both.
  - The lead write was proven in a Sandbox probe through the owner's profile. Nothing sets a lead on its own yet.
  - **Schema 44:** the project lead setting.
  - **Linear config scope:** a project's lead in a bound team, and the project membership Linear adds with it.
    Nothing at team or workspace scope.
- **Telling project leads.**
  - **A `project-lead` notice waits for a session.** When ClawBridge sets someone's Linear project lead and they have
    no session open, the notice waits and is delivered once, at their next session start. It waits 30 days at most,
    and a later notice about the same project replaces it.
  - **`--replace` tells the earlier lead** they're no longer the lead. This happens only when ClawBridge set that lead
    itself, by its own record (never the client's word), and they are still a project member. A lead set in Linear
    isn't told.
  - **`project overview` shows the owner "Project leads"**: set and told in the last 7 days, and how many notices are
    waiting. Counts only.
  - **Schema 45:** the lead ClawBridge last set on each Linear project, and the waiting notices. Delivered notices
    are pruned after 90 days. Linear config scope: none.

## Intake trusts by team role

- **A signal from a person whose team role is lead or engineer for the issue's own team is trusted**, like one from
  the trusted list. `clawbridge intake policy set --roles lead` narrows it to leads; a reviewer is never trusted by
  role, and setting `reviewer` (or any other name) is refused. Each such author is also read from Linear, at most
  15 minutes old, and trusted only as an active, non-guest user; a failed read only notifies, and a bot or app
  never counts. Nothing changes until the owner sets the team roles. The poll reports `trust` counts. No schema
  change. Linear config scope: none (it reads one user at a time).

## Docs

- **The skill and docs as shipped in v0.3.8.** The skill tells apart the four meanings of "lead", and its pages agree
  with what shipped: a bound parent's labels, one coordinator per project, the server's write queue, `rate_limited`'s
  retry time and trust by team role, with recovery rows for `outcome: queued`, `rate_limited`, `identity_revoked`,
  `linked_only` and `project_team_add_unproven`. docs/reference/api.md gains the team roles operations and the right
  schema numbers. Docs only. Linear config scope: none.

# ClawBridge v0.3.7 (released 28 Oct)

**Schema 39 → 41.**
- The server migrates to schema 41 on its first start: 40, who asked for each write the ClawBridge app made (intake
  attribution); 41, who is subscribed to agents' issues (a board already linked starts off).
- **One-way:** v0.3.6 refuses a database newer than schema 39. Take a dump before upgrading; to go back, restore it
  with the v0.3.6 image (docs/operations/deployment.md).

## Coordinating a project

- **The clawbridge skill teaches a coordinator how to run the work,** not only the commands
  (`references/coordinator.md`): a brief before any issue, routing, claims, default gates, which calls are the human's,
  session health, milestone comments and holds.
- **A project keeps its own rules in `docs/COORDINATOR.md`,** read by the coordinator as merged on the default branch.
  It may tighten the skill's defaults, never loosen them. docs/guides/coordinator.md explains it, with a template
  (docs/guides/coordinator-playbook-template.md).
- **The coordinator's `whoami` names the playbook** (`coordinator_playbook`: where it is, the `git show` that reads the
  merged copy, and whether it is merged or only in the working tree), read from the checkout the CLI or the MCP server
  runs in; nothing goes to the server. **`clawbridge doctor` gains `coordinator.playbook`** for the coordinator session
  only: it passes when the default branch has `docs/COORDINATOR.md`, and warns when it is missing or not merged.

## Agents' issues notify their people

- **The project owner, and up to 5 people they name, are subscribed to each issue an agent creates or claims** on a
  hosted server, so they get Linear's notifications. Before this, the app authored those issues and nobody was
  subscribed. `clawbridge linear subscribers show|on|off|owner on|off|add|remove`, plus `backfill --since DATE
  [--apply]` for what agents did before.
  - **Issue-level only.** No team, workspace or notification setting changes, nobody is ever unsubscribed, and each
    issue is subscribed once.
  - **The default.** On for boards linked after the upgrade; off for boards already linked, with a doctor hint.
  - **Costs and gates.** One read plus one call per person, from the project's hourly Linear budget. It is only for
    bound teams and their active members. The owner is added by email, which is never logged or shown.
  - **Off until the operator proves it.** The server runs with `serve --issue-subscribers-proven` only after a live
    check that the app may subscribe someone else (docs/operations/deployment.md). It is the one board write the
    app sends as GraphQL.

## Docs

- **The skill's views page, as shipped in v0.3.6:** agents filter issues only by issue views (a project view answers
  `agent_filter: unavailable (uses: kind.project)`), a view whose team left the binding isn't read, and issue rows'
  `priority` counts only with `priority_known`. Docs only.

## Linear

- **A parent team's issue listing no longer fails when Linear returns its sub-teams' issues.** Before, one such row
  refused the whole page ("provider page escaped the selected pair"), so a team that gained sub-teams could no longer
  list issues or poll `needs-agent`. Now the row is shown only under its own bound pair, never twice. A row of a
  team or pair the binding doesn't hold is dropped, and counted as `filtered_other_team` (intake: `other_team`).
  No schema change. Linear config scope: none.
- **An issue's priority on create and update.** `clawbridge linear create|update --priority` and the agents' create
  and update tools take Linear's priority, by name (`none`, `urgent`, `high`, `medium`, `low`) or 0–4. It is sent
  only when Linear's tool takes a priority, and the returned issue is checked like the other fields. No schema
  change, no Linear configuration.
- **`clawbridge linear labels` without `--team` maps every bound team's label names to IDs in one call**, per team
  (each team has its own IDs, so teams are never merged). Group and archived labels are left out, and a name two
  labels share is listed under `duplicates`. `--team` pages one team as before. No schema change, no Linear
  configuration.
- **A trusted person's own label or mention through ClawBridge counts as theirs.** On a hosted server, issue writes
  through ClawBridge are made as the app, so Linear names the app and the signal only notified the coordinator. The
  server now records who asked for each app write that adds a label or posts a comment, and intake credits it to a
  trusted person when it was the only such write in the signal's window and they made it in their own terminal: their
  device key, no session, no agent's environment. Their trusted-list entry must be linked to their ClawBridge
  principal: `intake trust add EMAIL` links a project member found by that email, `--principal EMAIL` names one. An
  agent's write never counts, and one near the person's own makes both uncounted. Linear config: none written.

## Operations

- **`linear-app-probe --allow-member UUID`** (with `--p8` or `--project-status`) lets the empty probe project keep
  exactly one member, the Linear user named by ID; another member, or more than one, is still refused, and the output
  says `member_allowed`. For a probe project that a person can't leave. No schema change. Linear config scope: none.
- **`operator linear-app-probe --rate-headers`** reads once over Linear's GraphQL API and once over its MCP server as
  the app, never mints, writes nothing, and prints only each request's status and Linear's rate-limit headers: which
  limits the MCP server reports, for pacing Linear calls (docs/operations/deployment.md).

# ClawBridge v0.3.6 (released 28 Oct)

**Schema 28 → 39.**
- The server migrates to schema 39 on its first start: 29, the new-device alerts' events and acknowledgements; 30, a
  project's own watch budget; 31, the device an invite was made from; 32, the workspace's device idle limit; 33,
  the Linear team and project names stored with a project's task board binding; 34, the date from which a project's
  threads are readable; 35, the message queue's leases, acks and per-recipient numbering; 36, the message stream's
  tickets; 37, the Linear issue views ClawBridge wrote; 38, project views among them; 39, the project kind setting. The upgrade numbers existing
  deliveries, about 2–4 s per million, and counts what was read as handled.
- **One-way:** v0.3.5 and older refuse a database newer than schema 28. Take a dump before upgrading; to go back,
  restore it with the v0.3.5 image (docs/operations/deployment.md).

## The message queue: the dispatcher, chaos tests, operations (slice 5 of 5)

- **The dispatcher model's `cases/next` is woken when a case opens** (a `cb_case` notification), instead of checking
  every second; it still checks every 5 s. Without the notification connection it checks every second as before.
- **`clawbridge deadletters list|requeue|dismiss`** reads and settles dead letters from the CLI (the session, the
  project's coordinator or a project owner; no bodies; requeue and dismiss are recorded in history).
- **The chaos suite** (`-tags chaos`, real Postgres): API crashes with in-flight transactions aborted, a killed LISTEN
  connection, a stalled network, a consumer killed mid-handle and a compaction cycle run in CI; the 10k-message soak
  with random consumer kills runs nightly and on demand, replayable by its printed seed. docs/operations/deployment.md
  gains "The message queue". No schema change, no Linear configuration.
- **Throughput is limited per workspace.** Writes in one workspace run one at a time, about 16–30 write transactions
  a second for 50 busy sessions in the soak, so one workspace with many busy sessions under churn can fall behind (no
  message is lost or handled twice). Spread a heavy team over workspaces (docs/operations/deployment.md); narrowing it
  is planned.
- **The queue, as shipped, in one place.** The clawbridge skill gains `references/queue.md`: handle, then `ack`
  (or `reply` with `ack`); redelivery and the `msg:<message_id>:<step>` operation IDs; extension; ordering;
  reconnects, restarts and compaction; dead letters. docs/usage.md gains "Messages wait until they're handled". Docs
  only.

## The message queue: agents ack (slice 4 of 5)

- **An agent's MCP server takes the session's messages from the queue** from the agent's first `inbox`, `ack`,
  `reply` or `mark_read` call: it holds the consume stream, or polls `lease` every 2 s in local mode and on a server
  without the stream. `inbox` keeps its shape; a message the server holds shows its `lease`. The new **`ack`** tool
  acks handled messages (the held lease is filled in), and `reply` takes **`ack: true`**, acked in the reply's own
  transaction. While the agent called a tool in the last 10 minutes, its held leases are extended every 5 minutes.
- **`mcp install --agent claude-code` installs Claude Code hooks** in the project's `.claude/settings.local.json` by
  default (`--hooks local|user|project-shared|none`; the report goes to stderr). `context hooks install` adds the
  same three:
  - **Stop** (`clawbridge hook stop`) sends a turn back once per new message while messages wait unacked, with fixed
    text and a count only. It never blocks while Claude Code says `stop_hook_active`, nor on a `hold` or `refuse`
    policy, an older server, an error or no answer in 2 s.
  - **PreCompact** holds the session's notifications and gives back its leases without counting an offer;
    **SessionStart** after the compaction accepts again. Only a hold the compaction set is lifted; one left by an
    agent that died mid-compaction is lifted by the next MCP server start, or after 15 minutes.
  - The hooks act through a private record the MCP server keeps (`<state home>/context/<session>.queue.json`), only
    for their own agent's MCP server and only against the configured server.
- **`set_notification_policy` wakes the session's streams**, so `accept` re-offers at once.
- **`clawbridge doctor` has a `queue` check:** whether the session's MCP server holds the stream, its unacked and dead
  counts, and a `hold` policy (whose messages wait without attempts or dead letters).
- The clawbridge skill gains the protocol: handle, then `ack` (or `reply` with `ack`), with
  `operation_id = msg:<message_id>:<step>`. No Linear configuration is involved.
- **The MCP server's own instructions say how to settle a message** (handle, then `ack`, or `reply` with `ack: true`;
  `mark_read` where `ack` is unavailable; the `msg:<message_id>:<step>` operation IDs). Before, only the skill said
  so, and an agent without it read its messages without acking them, so each came back and ended as a dead letter.

## The message queue: clients follow the stream (slice 3 of 5)

- **`clawbridge notify` is woken by the server** as soon as the session's mail changes, instead of polling every 5 s;
  it still checks once a minute in case a wake was missed. Against an older server, or in local mode, it polls as
  before and says `stream_unavailable` once on stderr.
- **`notify --adapter command -- PROGRAM [ARG]...` wakes an idle agent** by running a command you choose, directly and
  never through a shell. It is only ever given fixed text, `N ClawBridge messages are waiting; call inbox`, in place
  of each `{text}` argument and in `CLAWBRIDGE_NOTICE_TEXT`, never a sender, subject or body. docs/notifications.md
  has recipes for Orca (`orca terminal send`) and tmux (`send-keys`).
- **The TUI's Inbox stays live** on a server with the stream: it reloads when the session's mail changes, while it's
  on screen and nothing is being typed.
- The client reconnects with a fresh single-use ticket each time, backs off 1 s doubling to 60 s with jitter, and
  treats 90 s without a line as a dead connection. A refused ticket (a moved instance, a removed member) ends the
  stream; a full account (429) or an outage is retried.
- **`clawbridge notify` stops its heartbeat and context reporter before it exits**, so no context report is still
  writing to the session's context records after the watcher has stopped.

## The message queue: pushed (slice 2 of 5)

- **The server pushes a session's messages** on `GET /v1/stream` (Server-Sent Events), in place of polling every
  5 s. A consume stream leases and pushes deliveries within a second of their commit, holds 20 unacked at most, and
  is taken over by the session's next consume stream. An observe stream sends only the counts when they change. The
  URL never holds a secret: a single-use `stream_ticket` goes in a header. Every read is authorized again, so a
  revoke, a member's removal or a retired session closes the stream at once. Streams live 30 minutes, an account holds at most 8, and they go through
  Caddy unchanged (docs/reference/api.md, "The stream").
- **The server sweeps** every 15 s: run-out leases are offered again and dead letters made, by one replica at a time.
- Clients use the stream from slice 3; until then nothing changes for them.

## The message queue: acks and dead letters (slice 1 of 5)

- **A message is done when its recipient acks it, not when it is fetched.** New operations: `lease` takes the
  session's next deliveries in order, `ack` marks them handled, `nack` gives one back, and `extend` renews them. A lease
  runs 10 minutes. One that runs out is offered again, and after 5 the delivery is a dead letter. It is kept, and the
  project's coordinator is told the count (never the content). `deadletters_list`, `deadletters_requeue` and
  `deadletters_dismiss` settle them, with history. `reply` and `send` take an `ack`, committed in one transaction, and
  a retried reply still acks. `mark_read` acks. `notification_status` with `queue: true` adds the unacked and dead
  counts.
- **Each recipient's deliveries are numbered 1, 2, 3…** in commit order, with no gaps. Every server path that sends a
  message goes through one writer, which takes the counters in a fixed order so crossing sends can't deadlock.
- **Nothing changes for today's clients:** `inbox` takes no lease and keeps its shape, and `notification_status`
  without `queue` keeps its seven fields. Push (the stream), the CLI and the MCP tools come in the next slices
  (docs/reference/api.md, "The queue").

## Project overview

- **One read for a project's summary (TUI-UX-AUDIT PR C, server half).** `clawbridge project overview` (and the MCP
  tool `project_overview`) shows the project, its coordinator, its sessions with each one's assignment, and its
  Linear board's teams, projects and who closes each. Any project role may read it, and it shows nothing that role
  can't already read. Its counts cover every session, so they match the Sessions tab however many there are; the rows
  stop at 200. Offline it says so instead of failing as a write.
- **`list_sessions` rows say which session is the coordinator** (`coordinator: true`). Older clients ignore it.
- **Linear names are stored at link time (owner's Q5).** A link stores the bound teams' and projects' names, so the
  overview shows names instead of UUIDs. They are display text only: no check reads them, and the audit records
  counts, not names. `clawbridge linear names-refresh` (project owner or lead) picks up a rename without moving the
  binding's revision.

## The project's threads

- **The project owner can read the project's messages, agents' included; each read is in the project's history (Q1).**
  `clawbridge project threads` lists them, newest activity first (names and counts, no text); `clawbridge project
  thread ID` reads one, with who received and read each message. Nothing is marked delivered or read.
  - Only from the owner's own terminal, without a session; on a hosted server, only from a person's signed-in device.
    The CLI refuses when it can tell an agent runs it.
  - Conversations started before the upgrade stay private, even if they continue (`readable_from`).
  - Every read writes one `threads` row to the project's history (no body or name), about 30 a minute at most.

## Your Linear API key (views, phase 3a)

- **`clawbridge linear key set|status|remove --profile ID` (live/LINEAR-VIEWS-PLAN.md §11.3, §13.1).** The project
  owner saves their personal Linear API key for writing Linear views, which Linear's MCP can't. It's read at a hidden
  prompt or from stdin, never argv, and checked before saving: it must read, and belong to the same Linear user as the
  profile's connection. It's kept in the OS key store like the device key (or a 0600 file with `--key-store file`,
  after a warning). On every OS any program running as the owner can read it, so the guards are ClawBridge's:
  - only `linear key` and `linear view` read it; every other run of the binary (`mcp`, `runner`, `serve`, `notify`,
    ...) marks itself unable to, first thing, and the MCP server, runner and server don't link the code;
  - it refuses under an agent's environment (best effort);
  - runner sessions are denied `clawbridge` and `security`;
  - it's sent only with an allow-list of GraphQL operations, never in argv (on macOS it reaches the keychain on the
    `security` tool's stdin), logs or errors.

  `doctor` reports `linear.api_key` from a marker without reading the key. `linear logout` removes it too. No view
  writes yet: those come in the next phase.

## Linear project status

- **Read and set a Linear project's status by hand (owner request).** `clawbridge linear project status
  LINEAR_PROJECT [--suggest]` and `... status set LINEAR_PROJECT --type T --operation-id ID`, and the MCP tools
  `taskboard_project_status_v2` and `taskboard_project_status_set_v2`, run as the hosted server's ClawBridge app. Any
  member or session reads it. Only the project's designated coordinator session, or the project owner or a lead
  without a session, sets it. Completed and canceled follow the project's close rule: where people close its issues,
  no session sets them. A set previews first; its confirm carries the status the preview saw and is refused if the
  status changed since. It is recorded before it is sent, read back by status ID, and never sent twice. With project
  flow on, it holds flow for that project until `project-flow resume`. Each read costs 6 calls of the project's hourly
  Linear budget, each confirmed set 8. No schema change. The set tool is one of the Linear write tools a runner
  launch denies unless it passes `--linear-writes`.

## One invite link

- **An invite is one link (A1, §17.1).** `invite` and `device add` print a link with the code:
  `https://SERVER/join#cbj1.WORKSPACE.EMAIL.CODE`. On the new machine, `clawbridge join` asks for it (unechoed) and
  confirms "Join workspace … on … as …?". Piped, it needs `--yes`. `--out FILE` writes the link.
  - The link is never taken on the command line (`clawbridge join LINK` is refused): a command line is kept in shell
    history and process lists.
  - The code is after the `#`, which a browser never sends; the server's new `/join` page only says to run
    `clawbridge join`. Only `https` links are accepted (`http` on a loopback address, for development).
  - The guided setup's "I have an invite code" step and the `--email` path's code prompt take the link too.
  - The TUI's one-time box shows the link below the code.
- **`device add` is quick and short-lived (§17.2).** Its link lasts 15 minutes by default (`--expires` takes `30m`,
  `2h`, up to `24h`), and works only while the device that made it is still active. That device isn't alerted when
  the link is used; your other devices and the owner's are.

## New-device alerts

- **Your other devices hear of a new one (A1, §17.3).** When a device is added to your account (an invite, `device
  add`, recovery or `device enroll`) or one of yours is revoked, every other device of yours, and the workspace
  owner's, prints a line on stderr after its next command, and the TUI's Account screen shows a banner:
  `New device "Ada's desktop" (dev_9k2…) joined … via device add. Not you? clawbridge device revoke dev_9k2…`
  - It repeats until that device acknowledges it: `clawbridge device ack --all`, or `e` on the Account screen.
    `clawbridge device events` lists them. A command and the banner show the newest five, then "…and N more";
    events older than 90 days aren't shown.
  - A revoke is an alert too, including the owner's `user revoke`, so a stolen device can't quietly revoke your
    others.
  - Not alerted: the device itself, the device that revoked a device, and CI devices.
  - The main owner's coordinator sessions get each event as one `security` message per project, if the owner
    registered them; a coordinator someone else runs gets nothing (A1-D4). It names the user and the device ID, never
    the member-chosen label.
  - The server answers every device-signed request with `pending_device_events`; the client fetches the events at
    most once a minute. Older clients ignore the field.
- **Unused devices go dormant (A1, §17.4).** A human device unused for 90 days is refused until another device of
  its user, or the main owner, confirms it: `clawbridge device confirm dev_…` (`--member EMAIL` for another member's;
  `w` on the Account screen). Nothing it holds is deleted, and any use restarts the clock. CI devices are exempt.
  - Confirming is signed by a live human device of the user (or the main owner's); a Google sign-in or CI key can't.
  - The skill's recovery page says what an agent does on `device_dormant` (tells the human, who confirms it from
    another device) and with a dormancy notice (passes it on).
  - The main owner sets the limit: `clawbridge workspace settings --device-idle-days N` (30–365); a lower limit that
    makes devices dormant at once says how many and needs `--confirm`.
  - The user's other devices and the owner's are warned first, from day 80 (day 60 for the owner's own devices,
    which also reach the owner's coordinator session), and again when it goes dormant. `device list` shows
    `dormant_at`.
  - An owner whose devices are all dormant gets back in with the host operator's `operator bootstrap-invite`.
- **Device labels are printable text.** A label is shown on your other devices, so the server refuses control
  characters, terminal escapes and bidi or other format characters (1–64 printable characters), and the CLI and TUI
  escape any older label when they print it.

## Linear

- **`linear projects --limit` above 50 is refused with a clear message.** Linear refuses a projects page of more
  than 50, which showed as `Linear provider: provider_rejected`. The CLI now says "at most 50; page on with
  `--cursor`", and nothing is sent. More generally, a number outside the bounds a Linear tool's own schema declares is
  refused before the call. Teams, issues, labels and members still take up to 100.

## Security (G9)

- **The session runner's agent gets less of yours (G9-5, G9-6, G9-9, G9-14).**
  - **Environment:** `runner launch` starts claude through a launch script that clears the environment and passes on
    only the terminal's basics, the proxies and claude's own settings, named one by one (its key or sign-in token,
    endpoint, models, config directory). `GITHUB_TOKEN`, `NPM_TOKEN`, `AWS_*`, `SSH_AUTH_SOCK`, `CLAWBRIDGE_*` and a
    parent Claude Code session's variables no longer reach the session, and its sandbox self-test now proves that too.
  - **The clone's origin** is copied without a user or password; a remote helper or unknown transport is refused.
  - **MCP tools:** the agent's task-board write tools (which act as your Linear profile) are denied unless the launch
    passes `--linear-writes`, and `assignment_confirm` always is.
  - **The clone's `.git`** is denied to the file tools and to Bash commands naming it.
- **`runner fetch` checks the clone before git touches it (G9-7, G9-8, G9-14).**
  - **Refused:** a `.git` that isn't a plain directory, borrowed objects (alternates, `commondir`), and a config
    setting `core.hooksPath`, `core.fsmonitor` or an include.
  - **Checked:** every fetched object (`transfer.fsckObjects`).
  - **Scanned:** a merge's own changes, for token shapes, like every other commit's.

## The terminal interface

- **Waiting for a project tells the workspace owner to create the first one.** The server's `whoami` now answers a
  member with no project yet (no project, no session) for the workspace alone, so the TUI's Waiting screen knows
  whose workspace it is: the bootstrap owner is told `clawbridge project create SLUG` instead of to ask themselves,
  and a member is told the owner's address.
- **`clawbridge whoami` works before you have a project,** showing who you are and the workspace (`Project: none`)
  instead of failing to resolve one; a named project or a session must still resolve. The Waiting screen says it's
  checking until its first answer, instead of briefly showing a member's "ask the owner".
- **One set of keys, shown where you look (TUI-UX-AUDIT PR A).** The footer shows the screen's own keys, and `?`
  opens help listing every key for the screen and everywhere; the old one-line notice is gone. Keys never mean two
  things: **Enter opens** the selected row and never changes anything by itself, **Esc goes back one level** (help, an
  editor, an opened row, a wizard step), and `q` asks first while a board link or a model test is under way. Left and
  Right no longer switch screens.
- **New tab order, from the mode.** Overview, Projects, Sessions, Inbox, then Linear (`5`), Dispatcher (`6`, was
  Model), Account (`7`, hosted workspaces only) and Doctor. Cloud shows only where a setup is wired. Under 100 columns
  the tab bar is compact instead of collapsing to one tab. `tui --screen dispatcher` opens it; `model` still works.
- **The header says where you are, once.** One line, `ClawBridge · workspace › project › session`, with the mode and
  an offline badge; the status line sits under the tabs. The `Scope:` and `Source` lines and each screen's subtitle
  are gone from the body (the subtitles are in help).
- **Overview says what needs you (PR D).** A "Needs attention" block lists failing or warning Doctor checks, an
  unlinked Linear board, an ownership offer, unused invites and, for a project owner, the dispatcher's model, each
  with its tab; Enter goes there. Identity reads "Signed in as … (Google, verified)", and a profile with no session is
  told inboxes are per session instead of showing "Unread 0".
- **No project yet? The TUI waits for one.** A hosted profile that joined a workspace with no project shared yet
  opens on "Waiting for a project" instead of failing: it says whom to ask and what they run (the owner, right after a
  bootstrap join, how to create the first project), checks again every 30 s
  (`r` now), and once a project is yours saves it to the profile and reopens on it.
- **Doctor lists failures first.** Passes are folded (`p` shows them), Enter on a failing Linear, Dispatcher or
  device check opens that screen, the last results stay while the checks run again, and it says when they ran.
- **Linear and Account say what they mean, and guard what's shared (PR E1).** Linear names its step ("Link a board ·
  step 2 of 5") and the board in words, with the IDs and revision behind `d`; unlinking the board takes the project's
  slug typed, and logging a connection out asks y/N first; Space (not `c`) switches who closes in the review. Account
  rows are aligned columns with the ID shown for the selected row only, grants by project slug, and an invite is now
  an email, then a role per project from a checklist. An owner or lead grant is typed `invite` first, and an invite
  with no project ticked asks once more.
- **Intake in the TUI (E3b).** `n` on the Linear screen opens the project's intake: the mode and signals (S1 label, S2
  delegate, S3 mention), the trusted Linear users by name, the queue, and a check that each linked team has exactly one
  `needs-agent` label. A project owner or lead switches the mode or a signal, trusts someone picked from the linked
  teams' active members, stops trusting them, and dismisses or rearms an issue, each after a `y`; anyone else reads.
  Overview shows it in a line ("Intake: notify · 1 trusted · 0 queued") that opens it. Needs a server with E3a.
- **Projects and Sessions linked (PR C, TUI half).** Projects lists each project with your role, its sessions
  ("2 of 3 active"), its coordinator and its Linear board, and marks the one this profile works in with `●`; Enter
  opens a project read-only: its coordinator, sessions, and Linear teams and projects by name, with a banner when it
  isn't your profile's project, and `s` and `l` going to your own project's sessions and board. Sessions shows NAME
  (`★` on the coordinator), STATE (retired, moved, stale or presence, in one word), ISSUE, CONTEXT and AGENT, with
  OWNER and HOST from 140 columns; the details use short labels, say when context was reported ("4 minutes ago") and
  show the ID last. Overview counts sessions the same way ("Sessions · 2 active · 1 retired"). Needs a server with
  `project_overview` (PR C server half).
- **Messages: a To and a Message field (U26, U27).** `n` on Sessions writes to the selected session; `n` on Inbox asks
  whom, and Tab offers the project's live sessions (or completes what's typed). The `recipient|message` syntax is
  gone. Sending still needs the TUI opened with a session, and says so otherwise.
- **The Linear board by name (U31, U32).** The Linear screen names the linked teams and projects with the names stored
  at link time ("Board: linked · teams Eng · projects Payments"), says how old they are, and `r` refreshes them from
  Linear for a project owner or lead; a refresh that fails keeps the stored names and says so.
- **Changed keys:** Inbox Enter opens a message, with what it answers and its thread (`m` marks read, as before).
  Account's decline is `d` (was `n`). Linear connects with `+` (was `c`, which now only switches who closes in the
  link review); Esc on Linear goes back one step and keeps the teams and projects you chose, instead of starting over.

- **Every screen says what it's doing (TUI-UX-AUDIT PR B).** While a screen loads it says "Loading sessions…" and the
  layout stays put. A failed load says what failed in words ("The server can't be reached", "You're signed out"), the
  keys that help (`r` reload, the Doctor tab), then the raw error. An empty screen says why, and how to fill it.
- **One status line, with a word for its meaning.** Under the tabs, `OK`, `ERROR`, `NOTE` or `…`, the same words as the
  CLI, so it reads the same without colour. A send, reply or mark-read now says so ("OK  Sent to engineer-backend."),
  and Account and Linear name what changed ("laptop renamed to work-mac") instead of an ID.
- **Times are relative.** Inbox and Account show "4 min ago" or "in 2 days"; an opened message adds the time in UTC.
- **Inbox without a session explains itself.** Inboxes belong to agent sessions; a person's profile gets a note on how to
  read one (`clawbridge tui --session NAME`) instead of an error.
- **The Dispatcher screen keeps its place while it sends and tests.** A key no longer leaves the screen and loses the
  test's answer; Esc stops waiting. The field being typed in stays on screen at 80×24, and the outcome is marked `OK` or
  `FAIL`.
- **The docs describe the TUI and onboarding as shipped.** The header, the Cloud tab and the Dispatcher screen's key in
  the TUI reference; invite links and which release has the client commands in the README, onboarding, first-deployment
  and sign-in guides; the hooks the guided MCP step installs; and the skill's invite link, device-add link and `device
  confirm`. Docs only.

## The dispatcher

- **`clawbridge dispatcher model setup` guides an owner through the model's setup.** It asks for the provider (from
  the list the dispatcher agent can run), the model, the key at a hidden prompt (never an argument or env variable), the
  caps with a required daily token ceiling, and a confirmed disclosure, then waits for the test in your agent and says
  what came of it. `--key-stdin` with flags scripts it. `dispatcher model status` and `dispatcher model key delete
  --confirm` come with it. The server now accepts only providers from that same list.
- **The TUI has the same model setup, on a new Dispatcher screen (`6`).** It shows a project owner their key, their agent
  and the last test, and `s` walks through the same steps in the same order as the CLI. The key field is never drawn,
  and the key is wiped from memory once sent, on Esc, and when you leave the screen or quit.
- **Setup lists the provider's models, with their list prices.** In the CLI and the TUI, the model step shows up to ten
  of the provider's models from the agent's own catalogue, cheapest first, with their price per million tokens and
  context window; a number picks one, and any other model ID can still be typed. The list is generated from the pinned
  agent build, and CI fails if it is out of date.
- **`clawbridge-agentd` has its own image, for the egress proxy, and takes images pinned by ID.** The image is the static
  binary alone (`cmd/clawbridge-agentd/Dockerfile`, about 15 MiB, user 65534), and CI runs its egress proxy with the
  same hardening agentd gives it. `CLAWBRIDGE_AGENT_IMAGE` and `CLAWBRIDGE_EGRESS_IMAGE` now also accept an image ID
  (`sha256:…`), since an image loaded with `docker load` has no repo digest to pin.
- **Installing `clawbridge-agentd` is documented, with its units and a check.** `docs/operations/agentd.md` sets up its
  two users, the rootless Docker, the launcher token and the seal key without printing either, and the way back out.
  `deploy/systemd/agentd/` has the hardened units, and `deploy/agentd-check.sh` checks an install without changing it
  or printing a secret. The launcher token's `--out` steps in deployment.md failed, since the api runs as `nonroot`;
  they now go through a directory it owns.

## The comment watcher

- **A label added just after an issue's create is no longer reported uncertain.** Linear's label-filtered issue list
  can lag a write by a few seconds, so the check after `--add-label` or `--remove-label` sometimes found a label that
  had landed still missing (`receipt_mismatch`). A contradicting check is now asked again after 2 s and after 4 s
  more; only one that holds through both makes the write uncertain.
- **Intake's `needs-agent` signal (S1) now sees every project of a binding.** It read the bound projects one at a time
  and stopped after four pages, so in a project set of more than four projects a label in a later project was never
  seen, and every poll repeated the same partial read. It now reads each bound team once, across all its projects,
  and queues only issues in the binding; one in a project the binding doesn't hold is skipped as `not_in_binding`,
  without reading it. This fix shipped early, over v0.3.5, as the v0.3.5.1 server hotfix (#195).
- **Intake polls on time.** An intake source's next poll was counted from when its read was recorded, a few seconds
  after the tick began, so a tick that started a few seconds earlier skipped it, and a 15-minute source polled every
  30 minutes. The next poll now counts from the read's start, and a tick up to 30 s early still polls it.
- **A project's owner can set its own watch budget.** `clawbridge project watch-budget set N` (1 to 600 Linear calls
  an hour) sets the comment watcher's budget for that project, ahead of the server's `CLAWBRIDGE_WATCH_BUDGET` and a
  client's `linear watch --budget`, from the watcher's next call. `project watch-budget clear` goes back to them, and
  `project watch-budget show` and `doctor` show the budget in force and where it comes from. Each change is recorded in
  the project's history. Schema 30.

## Project kinds

- **A project kind setting (phase 1 of the project-kind labels).** `clawbridge linear kind set --values
  Engineering,Review,Test [--group Type] [--team UUID] --expected-revision N` names a single-select project-label
  group on one bound team, never the workspace, and its values; `kind show` reads it (any member or session). Only
  the project owner sets it, in their own terminal. Schema 39.
- **Seed the kind's labels in Linear (phase 2).** `clawbridge linear kind seed --operation-id ID --profile P` previews,
  then `--expected-plan-sha256 SHA --confirm` makes the group and its values as project labels of the kind's team, or
  adopts the ones already there, with the owner's own Linear profile. A name a workspace project label has is refused
  before anything is sent (Linear refuses that clash), as is any other clash. Each label is recorded before it is
  sent and proven in the team's list; the IDs are then stored.
- **Apply a kind to each project (phase 3).** `clawbridge linear kind apply --file kinds.json --operation-id ID
  --profile P` previews a mapping of up to 50 bound projects to kinds; a confirm carries the preview's plan digest,
  and the first one of a setting is a `--canary` of one project. Each project's labels are proven by ID (each label's
  projects, listed by its ID), and a project whose labels can't be is refused. The write removes only this kind's
  other values and adds the kind, keeping every other label; it is read again before sending and read back after,
  and only the same set is accepted. A change made at the same time is reported, never retried.

## Linear views

- **ClawBridge lists a board's shared Linear views (views phase 2).** `clawbridge linear view list [--team UUID]
  [--type issue|project]` and `clawbridge linear view show ID` list the shared issue and project views of the
  binding's teams, and agents get the read-only MCP tool `taskboard_views_v2`. Private, workspace and other kinds of
  views aren't listed, only counted. A view's filter isn't shown: Linear's MCP doesn't return it. Agents can't create
  or change views; the new skill page (`skills/clawbridge/references/views.md`) has them draft a view set for the
  project owner to apply.
- **The project owner creates and updates shared issue views, and agents filter issues by them (views phase 3b).**
  `clawbridge linear view create --team UUID` and `linear view update ID` (`--name`, `--filter-file`,
  `--operation-id`, `--confirm`) write with the owner's own Linear API key, in their own terminal, without a session.
  - **Always one bound team, never the workspace:** `--team` is required, with no default. The preview's first line
    states the scope ("a shared team view: visible to everyone in that team (N members)"), and history records
    `scope: team:UUID` with the view ID and the filter's digest, never the filter or the name.
  - **Every ID in the filter**, under any key, must be the view's team or the binding's (projects, labels, states,
    members); a name another issue view on the team has is refused.
  - **The confirm holds the preview:** it carries the preview's binding revision, filter digest and, for an update,
    the stored view's `filter_as_of`; anything changed since is refused. An update first reads the view in Linear and
    never overwrites a change someone made there.
  - **Recorded before it's sent, read back after:** only a read-back with the same team, name, sharing and filter is
    accepted and stored for agents; anything else is uncertain and never sent again.
  - **Agents:** `taskboard_issues_v2` takes `view_id` for a view ClawBridge wrote. ClawBridge evaluates the filter on
    the rows (team, project, labels, state, assignee, priority, one `and`), and the answer adds `filter_as_of`,
    `agent_filter` and the rows left out for an unknown field. A view using anything else is
    `agent_filter: unavailable` for agents, while people still see it in Linear. Issue rows now carry `priority`
    (0–4) with `priority_known`.
  - **Delete (phase 3c):** `linear view delete ID [--confirm]`, the owner only, and only a view ClawBridge wrote in
    this project; a view Linear no longer has afterwards is dropped from what agents filter by (history
    `view_deleted`), one still there is `uncertain`. The confirm carries the preview's `filter_as_of`, and a view
    someone changed in Linear since ClawBridge wrote it is never deleted.
  - **View sets and adoption (phase 3d):** `linear view apply --file F [--adopt NAME]... [--confirm]` previews a file
    of up to 50 issue views as creates, updates, unchanged and refused, then writes each as its own operation. A view
    someone made in Linear is taken over only with `--adopt` and only if exactly one has the name; the preview shows
    its current filter, creator and last change, and history records `view_adopted` with the old filter's digest. The
    confirm carries the preview's `plan_sha256`, and a plan that changed since is refused.
  - **Project views (phase 3e):** `view create --kind project` and `"kind": "project"` in a view-set file make a shared
    team view of projects, filtered by project labels: each a live choice of a single-select group of that team or
    the workspace, proven by ID with the owner's key. Agents list project views but don't filter by them yet.
  - Schema 37 stores the views, and schema 38 lets a stored view be a project view.

## The command line

- **Results are readable at a terminal; scripts and agents keep the same JSON.** On a terminal, a result is now laid
  out for people: aligned fields, tables, one block per item when a table would be too wide, and data from outside
  escaped. On a pipe, in an agent's shell (Claude Code, Codex), or with `--json` or `CLAWBRIDGE_OUTPUT=json`, the output
  is exactly the JSON it was, which a test now pins. `--json=false` or `CLAWBRIDGE_OUTPUT=human` asks for the layout
  anywhere. Invite codes and links, CI device keys and intake tokens are printed once, alone on their line. The skill
  tells agents to pass `--json`. Commands get their own layouts in the next releases; until then they show every
  field.
- **Onboarding carries you through instead of stopping you.**
  - **Wrong answers:** a wrong answer is asked again, with the reason, up to three times. The join wizard checks each
    answer as you give it, not after every question.
  - **Opening ClawBridge:** every path (local, joining a server, your own server) ends with "Open ClawBridge now?
    [Y/n]", after the summary.
  - **Invite joins:** with several projects you pick one from a numbered list. With none yet, your profile is saved
    and ClawBridge waits on its Overview until an owner grants you one.
  - **Your own server:** once the workspace exists (made by `cloud bootstrap` on AWS, or by hand and then
    `clawbridge onboard` again), it goes on to your first project.
  - **Optional setup:** the steps count "Optional setup n of 5". The MCP entry and the dispatcher preview read as
    text, not JSON.
  - **Damaged settings:** settings that are incomplete or damaged say what is wrong and how to start again.
  - **Clearer questions:** "Sign in: open a browser here, or show a code to enter on another device". "Google
    Cloud project ID".
- **Only an intake signal made in Linear itself is a trusted person's.** On a hosted server, issue writes through
  ClawBridge, including the owner's own `linear update --add-label`, are made as the ClawBridge app, so Linear's
  history names the app and the signal only notifies the coordinator. docs/linear.md and the skill's intake page now
  say so.
- **The project owner manages intake without a session (E3a).** `clawbridge intake policy|trust|list|dismiss|rearm`
  no longer needs a registered session: the project role decides, as before (any member reads; a lead or owner
  changes; a dismiss without a session needs a lead or owner).
  - `intake trust add|remove` takes a Linear name or email as well as a user ID. It's looked up among the members
    of the project's linked teams through your own Linear connection (`--linear-profile`, or your only one), shown
    ("That is Ada Lovelace <ada@…> (…)"), and changed only with `--confirm`, which says it again. Only active
    members match; a name that matches several people lists them. Names are escaped; emails are never stored, never
    shown to an agent, and `linear members` doesn't show them.
  - The skill has a page on intake (`references/intake.md`): what a coordinator session may do with the queue, and
    that the policy and the trusted list are a person's.

- **`whoami`, `doctor`, `status ID` and the account lists read as answers (O2).** `whoami` says who you are, how that
  was proven, and your workspace, project and session. `doctor` leads with its counts, lists failures first, then
  warnings, then passes, with each action indented under its check. `status ID` shows when a message was sent and who has
  read it. `invite list`, `device list`, `user list` and `owner show` are tables, with a device going dormant marked
  "dormant in N days". Times are relative ("4 min ago", "in 2 days"), with UTC where the exact time matters, through one
  formatter that the TUI uses too. `--json` is unchanged.
- **`doctor` says how each check came out, in plain words (O3).** A check's line now reads as an answer ("Your
  ClawBridge settings can't be used.", "This agent's ClawBridge connection is missing or can't start.", "Your workspace answers."),
  with its details (counts, paths, versions) on the line under it and what to do after `→`. Checks whose message
  already read that way keep it. `--json` is unchanged, check IDs included.

# ClawBridge v0.3.5.1 (server hotfix, deployed 27 Oct; no client release or tag)

A server-only image over v0.3.5, built from `release/v0.3.5-hotfix` (#195). **No schema change** (still 28), so it
deploys over v0.3.5 data and rolls back to the v0.3.5 image. Clients stay on v0.3.5.

- **Intake's `needs-agent` signal (S1) sees every project of a binding** (#182, with #186's test). The fix is described
  under "The comment watcher" above; it is in the next release too.

# ClawBridge v0.3.5 (released 27 Oct)

**Schema 19 → 28.**
- The server migrates to schema 28 on its first start:
  - 20, the session liveness lease;
  - 21 and 22, project flow and its early checks;
  - 23, 24, 25 and 28, the dispatcher's model setup, cases, chat and advice;
  - 26 and 27, context watching and per-session levels.
- **One-way:** v0.3.4 and older refuse a database newer than schema 19 ("Postgres schema is newer than this
  binary").
- **Rollback:** take a dump before upgrading. To go back, restore that schema 19 dump with the v0.3.4 image. Never run
  an older binary against the migrated database (docs/operations/deployment.md).
- Upgrade the server first, then clients. **Upgrade every client before setting `CLAWBRIDGE_AUTH=device`** (below).

## Signing in with invites: the client

- **The client side of device sign-in ships in this release.** The server side shipped in v0.3.4.
  - **Commands:** `clawbridge join` (the invite code is asked for without echoing it), `invite`, `device` (`add`,
    `list`, `label`, `revoke`, `enroll`), `user` and `owner`, and the device checks in `whoami` and `doctor`.
  - **The TUI:** an Account screen (`8`).
  - **The guided setup:** an "I have an invite code" path.
  - **Cloud check:** an `auth_mismatch` check in `clawbridge cloud check server`.
  - A v0.3.4 client can't join a server that takes device keys.
- **Upgrade the clients first, then switch** (docs/sign-in.md, "Turning device sign-in on"):
  1. keep `google`, or set `google,device`;
  2. upgrade every member's client;
  3. give every member a device (`device enroll`, or an invite);
  4. only then set `device`.

  Setting `device` early locks out old clients and Google profiles. Setting `google,device` again undoes it, and
  nothing stored is touched.

## Sessions

- **A liveness lease (schema 20).** `clawbridge mcp` and `clawbridge notify` send a `heartbeat` every 30 s with the
  session's instance, which keeps it alive for 90 s. Session rows carry `liveness`: `working`, `idle`, `asleep` or
  `gone` (docs/usage.md).
  - A heartbeat is not activity and never moves `last_seen`, and the server writes it at most once every 20 s.
  - Only the instance holding the session can renew it.
  - Against an older server, a failed heartbeat just waits for the next beat.

## The comment watcher

- **ClawBridge's own claim and close notes no longer speed up the comment watch.** An issue's comments were polled
  every 5 minutes for an hour after any ClawBridge comment on it, so every claim and release kept its issue on the short
  cadence. That was most of the watcher's fixed Linear calls. Only a session's own comment, which awaits an answer, does
  that now; a marker note leaves the issue on its usual 15-minute or hourly cadence.
- **The marker pass can no longer starve or fail a tick** (#117, the fix the v0.3.4 hotfix image carried).
  - **Markers run last:** in a tick, the claim markers run after the comment watch, intake, health, docs and the
    dispatcher.
  - **Each pass is bounded:** at most 3 rows; none started after 8 s, or with under 6 s left; and its own deadline
    3 s before the tick's.
  - **One proof per pass:** each pass shares one Linear connection and one binding proof.
  - **Refunds:** rows not started give their budget back at once.
  - **Nudges drain what's left:** a tick or nudge that stops with rows left queues a nudge, which repeats a minute
    apart until the queue drains. A queue stopped by the hour's budget waits for the next hour.
  - **Logs:** one line per marker pass, and per failed tick or nudge.
  - **Older servers:** a client against a v0.3.4 server falls back when the server refuses `marker_due`'s new `limit`.
- **The server's watcher budget can be raised.** `CLAWBRIDGE_WATCH_BUDGET` (1 to 600 Linear calls an hour, default
  200) sets the hour's budget of every tick and nudge of the server's own watcher, and of `operator health-run`, for
  every project. Before, only a client `linear watch --budget` could change it. An invalid value refuses to start.

## Project status follows the work (off until proven)

- **Project flow (schemas 21 and 22).**
  - **What it does:** with `"project_flow": true` in an enabled status policy, a hosted server moves each bound
    Linear project to started when its first issue starts, and to completed when all its issues are closed. The
    finish happens only where agents close the project's issues; elsewhere the coordinator is told.
  - **Early checks:** it runs in the daily health slot, and ClawBridge's own claim and done-release moves trigger
    an early check.
  - **Never backwards:** it never moves a project back, and a person's move back holds it until `clawbridge linear
    project-flow resume`.
  - **Existing policies:** they read it as off.
  - **Details:** docs/linear.md, "Project status follows the work".
- **Off until the operator proves it.** The server refuses to turn project flow on until it runs with `serve
  --project-flow-proven` (`CLAWBRIDGE_PROJECT_FLOW_PROVEN=1`), after the deploy probe; until then `status-policy show`
  says `off (not proven on this server)`. The minimal status policy that `link --closes …=agent` offers turns it on
  only on a proven server, and its preview says which.
- **`operator linear-app-probe --project-status --project UUID [--dry-run]`** is that proof (A4). It moves the
  operator's empty probe project, which `CLAWBRIDGE_PROBE_PROJECT` must name, to started, to completed and back, as
  the app with project flow's own call. Each move is read back by status ID. The project must be in no binding, and
  on a team a binding uses. Three accepted moves are the PASS that `--project-flow-proven` needs. It prints IDs and
  types, never names. Details: docs/operations/deployment.md.

## Project health

- **`operator health-run`** forces one health run of a bound Linear project now, instead of at its daily slot. It runs
  on the scheduled run's own path and post rules, under the project's watch lease, and is charged a whole run.
  - **Without `--confirm`:** it says what it would do.
  - **`--force-new`:** acts on this run alone, and marks the post as forced.
  - **`--delete UPDATE_ID`:** deletes a post that a forced run made, and only that.
  - Details: docs/operations/deployment.md.
- **`operator linear-app-probe --health-order --project UUID`** settles, before a project's health posts are turned
  on, that the health judgement's read holds the project's newest update. It makes two reads as the app and writes
  nothing. Only `agree: true` lets `post` go on.


- **The dispatcher's model is refused in a 101st project.** One owner's dispatcher agent serves at most 100 projects,
  so `dispatcher enable --engine model` and `dispatcher model advise on` past that are refused at `--confirm`. The
  preview says `blocked: model_projects_over_cap`. Turn the model off in another project first.
- **Retiring a session with confirmed work now needs `--outcome done` or `--confirm`.** `--outcome done` says the work
  is finished, and the issue moves on as a release with `--outcome done` does. `--confirm` releases it as it is. With
  neither, `clawbridge retire` (and MCP `retire_session`) is refused, naming the issue, and nothing changes. A move to
  a successor isn't asked.

## Security (G9 prep)

- **Releases carry build provenance, and installs refuse downgrades (G9-10).** Every release file is attested by the
  release workflow (`gh attestation verify FILE --repo jkai-chen/ClawBridge`). `CLAWBRIDGE_VERIFY=required` makes the
  installers check it, with the GitHub CLI, before installing. The installers no longer install an older version over
  a newer one unless you pass `--allow-downgrade` (Windows: `-AllowDowngrade`).

- **The ClawBridge Linear app answers members only (F1).** `linear_app_status`, `linear_app_states` and `linear_write`
  now prove the caller's workspace membership before anything reaches the app. Before, any Google account could read
  the app's Linear user and organisation IDs, since the sign-in client IDs are public. Clients that ask with no
  workspace are found by their membership, so existing clients need no change.
- **The dispatcher's launcher and agent credentials are tighter (G9-3, G9-4).** The launcher's routes now refuse plain
  HTTP from another machine, as the agent's already did. The launcher token mints at most 6 agent credentials an hour per
  owner (429 past that), and doctor warns when an owner's agent got more than 3 in a day. An agent's credential now
  expires 24 hours after it is redeemed; the agent stops and agentd starts it again with a fresh one. The launcher token
  serves every owner on the server: treat it like their provider keys.
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
- **Downloads in the provisioning skills are pinned (G9-13).** The AWS skill checks Docker's Compose and Buildx
  plugins against SHA-256 values written in the skill, not checksum files from the same release. The GCP skill checks
  Docker's apt signing key against its published fingerprint before trusting the repository, and stops if it differs.
- **The dispatcher agent's model library is pinned to one fork release (G9-11).** The lock check accepts only the
  `cb-v1.1.0-1` tarballs, by version and integrity, and fails if that tag stops pointing at its commit (`96f054a`).
- **Traffic that never signs in can't lock signed-in users out (G9-1).** `/mcp/intake`'s per-address limit had shared
  `/v1/execute`'s per-principal table, so a flood from rotating IPv6 addresses could fill it and refuse every user with
  429. Intake and the watcher's tick endpoint now each limit addresses in their own bounded table, before any token is
  checked. A full table evicts its oldest entry instead of refusing newcomers, as `/v1/join`'s and failed sign-in
  counters now do too. An IPv6 client counts as its /64.

## Linear documents

- **`doc get` reads whole documents by default.** The default read is now 64 KiB, its maximum (it was 32 KiB). A read
  that is still cut says so in a `note`: how much was read of how much, and whether `--max-bytes` (MCP `max_bytes`)
  can read more.
- **The publish preview warns about lists Linear merges.** An ordered list that restarts right after another, with
  only blank or indented lines between, is merged and renumbered by Linear; the preview now says which lines, in
  `warnings`. It never refuses, and fenced code is skipped.

## The dispatcher

- **The reserved dispatcher sessions exist only while the dispatcher is on** (#99). Since v0.3.4, every watcher tick
  created `clawbridge-dispatcher` and `clawbridge-dispatcher-model` in every project, even with the dispatcher off.
  - **Off:** now nothing is created, and active ones are retired (never deleted), including those v0.3.4 left.
  - **Enable:** brings them back with the same IDs.
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
- **The launcher's egress proxy can reach a server on your LAN or VPN, when you say so.**
  `CLAWBRIDGE_EGRESS_PRIVATE_SERVER=HOST:PORT` (the server's own) lets agents reach a ClawBridge server on a private or
  CGNAT address, never loopback or cloud metadata; by default every address must still be public. A compiled-in table
  also gives the Vertex AI, Bedrock and Azure provider hosts their token endpoint, ready for when the agent offers
  them.
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

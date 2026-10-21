# Phase 4: existing-service onboarding and terminal interface

Status: owner authorized this increment on 21 October 2026 after the Phase 3
deployment, cleanup and unattended-backup checks passed. Baseline is pushed
`e8db318ebdd56785b5a214e9fca41dd69e71fece`. This is the onboarding increment;
the older DESIGN.md phase numbering is a roadmap, not a claim that delivery
hooks or team rollout are included here.

Delivery status: implementation and automated independent acceptance passed.
Full uncached and race suites ran against disposable PostgreSQL 17.11; vet and
CGO-disabled darwin/arm64, linux/amd64 and windows/amd64 builds passed. Reviewer
confirmed unchanged frozen code and removal of both test containers/volumes.
PHASE-4-REVIEW.md owns the exact results and evidence boundaries. The manual
acceptance checklist below remains Pending; this client increment has not been
deployed to the pilot VM.

## Outcome and scope

A new member supplies an existing HTTPS service, workspace and project, signs
in through Google and saves an authorized route without installing gcloud or
editing TOML. That same route works for CLI, installed MCP and the terminal UI.
The administrator can still explicitly initialize an existing deployment, and
local setup remains available. CORE-API.md's Phase 4 section owns exact command
and output semantics.

Deliver `setup join`, a real `tui` entry over the reviewed six-screen model, and
trusted-route inheritance for `mcp install`. Keep existing `setup --local` and
`setup --cloud` as the local creation and hosted administration commands. The
hosted command initializes the already deployed service; it does not create a
VM, database, OAuth application, IAM grant or other infrastructure.

The existing `setup.BuildPlan`/`Apply` provisioner predates the Compose/Caddy,
separate data disk and human OAuth design. It remains disconnected from CLI and
TUI production dispatch. A complete infrastructure create wizard needs a revised
plan and separate resource/cost acceptance; exposing the old scaffold would
provide an incorrect deployment. This increment also does not add project links,
invitations, imports, notification hooks, a session-token exchange or service
account provisioning.

## User journeys

New member, with an address and scope supplied by the workspace administrator:

```sh
clawbridge setup join --server https://SERVICE --workspace TEAM --project CODEBASE
clawbridge whoami
clawbridge mcp install --agent codex --session codex-engineer --dry-run
clawbridge mcp install --agent codex --session codex-engineer
clawbridge doctor
clawbridge tui
```

Use `setup join --device` when consent must happen on another device. Existing
complete OAuth routes reuse their keychain credential; `--login` explicitly
requests fresh browser acquisition. Missing/expired/broken credentials on an
already configured route fail with a login instruction rather than choosing a
different backend. A fresh route acquires browser credentials by default.
Standalone `login`, `login --device`, `login --use-gcloud` and `logout` keep their
Phase 3 contracts. Each agent installation needs a distinct session and entry
name, and installation never registers or resumes that session.

Use an explicit `--client-config /absolute/path/config.toml` to maintain another
trusted route. It selects a trusted client config, whereas `mcp install --config`
continues to select the target agent config. Installed cloud MCP entries pin the
client-config path so fresh agent processes can load their installed-app
credentials without embedding them in argv or environment.

Administrators retain the explicit existing-service bootstrap:

```sh
clawbridge login --mode cloud --credential-source oauth --server https://SERVICE
clawbridge setup --cloud --workspace TEAM --retention-days 90
clawbridge project create CODEBASE
```

Only the verified bootstrap owner can initialize. Joining never calls these
operations, creates a project or grants membership. For local use, choose
`setup --local --name NAME` followed by `project create SLUG`. No authority mode
is selected silently by a new interactive wizard in this increment.

## Join transaction and trust

Join validates explicit service and scope, target config shape and authority
compatibility before OAuth acquisition or network execution. The repository is
never a source of server, credential backend, principal, audience, keychain or
client-config path. Join requires explicit workspace/project selectors, resolves
them online to immutable IDs, and verifies an authoritative online Google-token
`whoami` response for those IDs and the selected principal.

Fresh OAuth acquisition uses the existing staged keychain transaction. Do not
publish a temporary login route before membership authorization. On acquisition,
authorization, cancellation or config-publication failure, preserve prior config
bytes and roll back newly staged/replaced credentials. If rollback itself fails,
report the partial keychain cleanup accurately without exposing values. On
success, atomically write a private trusted cloud route including the applicable
installed-app pairs, then commit the credential transaction. Refresh/access/ID
tokens never enter config, logs, installer results or agent environments.

Reusing a complete route does not stage a replacement credential. All joins are
online-only: mirror reads, a successful login alone, or a principal supplied by
the user cannot establish project membership. No session is registered, resumed
or taken over. Changing project explicitly clears an old session/instance unless
both are explicitly supplied for read-only verification. Rejoining the same
route is an idempotent config no-op. A different server, non-cloud authority or
conflicting pinned principal requires a separate trusted config.

## Terminal interface

`tui` is the operator view of an already configured, resolved project. Validate
terminal input/output and TERM before opening a database or contacting a service.
The CLI assembles the existing local/remote executor and doctor adapters, pins
workspace/project and optional actor identity, and closes/cancels resources on
exit. No actor is registered or resumed by launch. Inbox actions require an
explicit current session instance; actorless views remain usable.

Overview, Projects and Sessions are read views; Inbox exposes the model's
explicit send/reply/mark-read actions; Doctor runs the same diagnostics as CLI.
Cloud shows the selected endpoint and configuration limits, with provisioning
unavailable. It has no apply adapter. The existing safe-rendering and offline
contracts apply to every screen. A TUI launch never performs setup, installs an
MCP entry or changes the credential backend.

## Ownership and gates

- Root: this plan, CORE-API.md, README onboarding guidance, coordination and final
  contract reconciliation. Minimal coding only if ownership is explicitly moved.
- cb-engineer: all implementation files and focused package tests; publish any
  adapter changes, then freeze the implementation with exact fingerprints.
- cb-tester-reviewer: PHASE-4-REVIEW.md, independent integration probes and final
  full/race/vet/CGO-disabled darwin/arm64, linux/amd64 and windows/amd64 checks.

Reviewer writes black-box probes after contract publication, with final heavy
checks after engineer freeze. Review covers denied/nonmember/offline join,
staged-credential rollback, malicious repository input, config publication,
installer fresh-process routing and secret redaction, non-TTY refusal, terminal
controls, explicit TUI mutations and cancellation. Use disposable PostgreSQL for
actual hosted-store coverage and clean up exact test resources.

No deployment or new billed resource is required for this client increment.
Automated browser/device and keyring fixtures are synthetic evidence. A real
clean-machine/no-gcloud journey, a second human member, native platform keychains
and model-driven clients are separate manual acceptance and must stay Pending
until observed. Do not start consent or alter real agent configuration during
automated review. Prior Phase 3 live evidence remains attributed to that code.

## Manual acceptance after automated review

Status: Pending. Use the reviewed binary and a dedicated absolute client-config
path, isolated agent configuration and distinct test sessions. Record the binary
commit, operating system, endpoint and result without recording credential values.
These checks require the owner's participation and do not block publishing the
automatically reviewed client increment as such.

1. On a machine without the SDK installed, verify that `gcloud` is unavailable.
   A restricted PATH on a machine that has the SDK is a separate, narrower claim.
2. With an existing member account, run `setup join` against the pilot's explicit
   service/workspace/project. Complete browser consent; verify `whoami` identifies
   the intended human and immutable project. Inspect config permissions and key
   names only; never print token or client-secret values.
3. Install an MCP entry into the isolated agent configuration, with an explicit
   distinct session. Run the selected `doctor --mcp-*` check, then start a fresh
   client without the installer's environment. Attribute real tool execution
   separately from discovery or synthetic subprocess evidence.
4. Exercise the six TUI screens in a real terminal. Confirm actorless read views,
   explicit actor requirements for Inbox, terminal restoration on quit and an
   informational Cloud screen. Do not send a message without selecting its test
   recipient and content deliberately.
5. With a separate test config/keychain namespace, repeat device join and then
   browser sign-in on that same config. Verify authenticated `whoami` after each,
   fresh-process refresh, and logout's documented keychain/config behavior.
6. Repeat join as a second human already granted membership by the administrator.
   Test a nonmember separately and confirm denied join publishes no route. Joining
   must never grant membership. Retain redacted evidence and remove only the exact
   isolated client entries/configs/credentials created for this acceptance.

Native Windows/Linux credential stores, a second human, a physically SDK-free
machine and live same-config device-to-browser transition remain separate Pending
claims until their respective observations are recorded. This checklist does not
authorize deployment, membership changes or consent during automated review.

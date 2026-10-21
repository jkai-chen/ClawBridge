# ClawBridge

A project-scoped messaging bus for coding agents. ClawBridge supports a local
SQLite mode and a hosted Postgres mode with verified Google identity, plus CLI,
stdio MCP, and an explicitly synchronized read-only offline mirror. The Phase 4
onboarding/TUI increment passed automated independent review; see
[PHASE-4-REVIEW.md](PHASE-4-REVIEW.md). Cross-project links and automated cloud
provisioning remain future work.

## Install release binaries

Release distribution is being prepared for **v0.1.0**. The commands below become
available when that release is published; source builds remain available now.

For macOS or Linux, the installer selects Intel/AMD64 or ARM64, verifies the
release checksum and installs into `~/.local/bin`:

```sh
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  https://github.com/jkai-chen/ClawBridge/releases/latest/download/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
clawbridge --version
clawbridge
```

To pin the installer and binary version:

```sh
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
  https://github.com/jkai-chen/ClawBridge/releases/download/v0.1.0/install.sh \
  | sh -s -- --version v0.1.0
```

Use `--bin-dir /absolute/path` to choose another destination. The installer prints
the PATH step; it does not change shell profiles or PATH itself. If a download or
checksum fails, the existing binary stays in place. An update replaces the binary
while preserving your saved route, database/cache and native credentials.

On Windows x64, download and run the PowerShell installer:

```powershell
Invoke-WebRequest -Uri 'https://github.com/jkai-chen/ClawBridge/releases/latest/download/install.ps1' -OutFile "$env:TEMP\clawbridge-install.ps1"
& "$env:TEMP\clawbridge-install.ps1"
$env:Path = "$env:LOCALAPPDATA\Programs\ClawBridge;$env:Path"
clawbridge --version
clawbridge
```

Use `-Version v0.1.0` for an explicit binary version or `-InstallDir 'C:\absolute\path'`
for a different directory. Default is `%LOCALAPPDATA%\Programs\ClawBridge`.
The PATH command above affects the current session; add the directory to your
user PATH through Windows settings for future terminals. Installer support is
Windows PowerShell 5.1 and PowerShell 7 on Windows x64. Native platform runtime
acceptance is recorded in RELEASE-REVIEW.md.

Installation does not launch ClawBridge. Running `clawbridge` opens your saved
profile or first-run guided setup. Linux/WSL still requires an existing working
Secret Service session for OAuth, as described below. No gcloud installation is
needed for browser/device OAuth. Binaries are unsigned; macOS notarization and
Windows installer packages are separate future distribution options.

You can also download the matching raw binary from the release and verify it
against `SHA256SUMS`. The manifest covers five executables, both installers and
the full `SOURCE-COMMIT.txt`. Checksums verify integrity against that release
manifest; they are not a separate publisher signature.

`clawbridge version` and `clawbridge --version` print JSON with version, commit and
build timestamp without loading app state. Source builds use dev/unknown defaults.

## Build

Requires Go 1.26 or newer. Local mode needs no database server and neither mode
needs a C compiler.

```sh
mkdir -p bin
go build -o bin/clawbridge ./cmd/clawbridge
bin/clawbridge --help
```

## Exchange messages locally

Use an explicit path so this example is separate from any other bus:

```sh
export CLAWBRIDGE_DB="$PWD/demo.db"
export CLAWBRIDGE_WORKSPACE=demo
export CLAWBRIDGE_PROJECT=clawbridge

bin/clawbridge setup --local --name demo
bin/clawbridge project create clawbridge
bin/clawbridge --session engineer --instance engineer-run-1 register --agent-kind codex --role engineer
bin/clawbridge --session reviewer --instance reviewer-run-1 register --agent-kind claude-code --role reviewer
bin/clawbridge --session engineer --instance engineer-run-1 send reviewer 'Ready for review' --operation-id review-request-1
bin/clawbridge --session reviewer --instance reviewer-run-1 inbox
```

Use the returned message ID to reply and mark read:

```sh
bin/clawbridge --session reviewer --instance reviewer-run-1 reply MESSAGE_ID 'Review complete' --operation-id review-reply-1
bin/clawbridge --session reviewer --instance reviewer-run-1 mark-read MESSAGE_ID
bin/clawbridge --session engineer --instance engineer-run-1 status MESSAGE_ID
```

Outputs contain `source: "local-authority"` and a `data` object. Sending commits a
message; inbox fetch stamps delivered; mark-read stamps read. A failed command
exits nonzero. Reuse an operation ID only for the identical send/reply retry.

Local identity is attributed to the OS user and explicitly unverified. Session
instances prevent accidental adoption; they are not authentication against another
process running as that same OS user. A session's routing role grants no powers.

## Project scope

Select scope explicitly with flags or `CLAWBRIDGE_*` environment variables, or
add `.clawbridge.toml` to the repository root:

```toml
workspace = "demo"
project = "clawbridge"
```

Repository config may set only db/workspace/project/session/instance; repository
mode, server, audience, principal, credentials, and cache are always ignored. In
particular, putting `mode = "cloud"` in a checked-out repository cannot redirect
an existing local client to a remote server.

Without a project selector, a registered normalized Git remote can resolve it.
Missing/ambiguous projects fail; a project is never silently created. An MCP
process pins its scope at startup. Session names are unique within each project.

## Join a hosted workspace

In an interactive terminal, start the guided setup with:

```sh
clawbridge
```

On first launch, enter the service URL, workspace and project, choose browser or
device sign-in, then confirm. Future launches open the TUI with the saved route.
Use `clawbridge setup wizard` to explicitly open guided join, or
`clawbridge --client-config /absolute/path/config.toml` for a separate startup
profile. Existing invalid configuration is reported rather than overwritten.
Bare noninteractive invocation prints help; automation uses explicit commands.

This joins an existing deployment; it never provisions resources or grants
membership. Ask the workspace
administrator for the HTTPS address, workspace and project and ensure your Google
account already has access:

```sh
bin/clawbridge setup join --server https://SERVICE \
  --workspace TEAM --project CODEBASE
bin/clawbridge whoami
```

First join opens browser sign-in by default; add `--device` to approve from
another device. A complete existing OAuth route reuses its keychain credential.
Use `--login` for an explicit fresh browser sign-in. A successful join saves the
verified principal and resolved project IDs in private trusted configuration.
Denied membership or an unreachable service leaves the previous route intact.
Joining does not register an agent session.

To keep another trusted route, use the global
`--client-config /absolute/path/config.toml`. Join requires an explicit server,
workspace and project; repository files cannot choose the server or credentials.

The standalone Phase 3 login commands remain supported:

Browser sign-in uses Google OAuth with a loopback callback and PKCE. It needs no
`gcloud` installation:

```sh
bin/clawbridge login
bin/clawbridge doctor
```

Use `bin/clawbridge login --device` on an SSH or remote machine without a usable
browser. Both login commands discover the applicable installed-app OAuth client
from the trusted cloud server's `/v1/config` endpoint by default. A preconfigured
trusted `oauth_client_id` for browser login or `oauth_device_client_id` for
`--device` is optional and overrides discovery together with its matching trusted
installed-app credential. Discovery returns both client IDs and their matching
application credentials; these identifiers are safe for public installed-app
distribution but are still never logged, echoed, or placed in MCP/installer argv.
Trusted configuration must select cloud mode, OAuth as the credential source, and
the cloud server; successful login writes the selected ID/credential, matching
audience and verified principal. Existing pilot installations may continue using
explicit gcloud or ADC impersonation, but new cloud installation flows must pin
one credential source. Sign-in stores only the refresh credential in the OS
keychain; access and identity tokens remain in memory.

`bin/clawbridge logout` removes only the active OAuth keychain entry. It does not
revoke other Google credentials or alter agent configuration. See
[PHASE-3-PLAN.md](PHASE-3-PLAN.md), [CORE-API.md](CORE-API.md), and
[PHASE-3-REVIEW.md](PHASE-3-REVIEW.md) for the exact routing contract and the
remaining live Google acceptance limits.

### Create locally or initialize an existing hosted service

`setup --local` in the local example creates a local workspace. For an already
deployed hosted service, only its configured bootstrap owner can initialize it:

```sh
bin/clawbridge login --mode cloud --credential-source oauth --server https://SERVICE
bin/clawbridge setup --cloud --workspace TEAM --retention-days 90
bin/clawbridge project create CODEBASE
```

These are explicit administrative operations. `setup --cloud` does not create
Compute Engine, PostgreSQL, OAuth clients or IAM grants. The existing Compose
deployment instructions are in [deploy/README.md](deploy/README.md); the older
infrastructure setup scaffold is not exposed as an automatic provisioning path.

## MCP and installation

After a Phase 4 join, MCP installation inherits the saved trusted cloud route.
Choose a distinct session per agent; preview the target before writing it:

```sh
bin/clawbridge mcp install --agent codex --session codex-engineer --dry-run
bin/clawbridge mcp install --agent codex --session codex-engineer
bin/clawbridge doctor --mcp-agent codex --mcp-config /absolute/path/to/codex/config.toml \
  --mcp-session codex-engineer
```

Use `--agent claude-code` and a separate session for Claude Code. Installation
writes configuration only; the agent calls `register_session` after connecting.
Cloud entries pin the trusted client-config path and safe routing identifiers.
They contain no installed-app credentials or user tokens. Keep that trusted
config file and its native keychain available to the user running the MCP process.
`--client-config` selects the ClawBridge route; installer `--config` selects the
agent file to edit. Diagnostics never repair entries automatically.

Start an MCP process with a distinct session name for each agent:

```sh
bin/clawbridge mcp --db "$CLAWBRIDGE_DB" --workspace demo --project clawbridge --session reviewer-mcp
```

Call `register_session` before using the inbox or sending. The MCP process retains
the returned instance. To explicitly take over an existing active session, start
with its recorded `--instance`, then call `register_session` with `resume: true`.
This rotates the instance and fences the previous process. Record the new instance
for later resumption. Retired names stay retired; use a new name for a replacement.
Successful actor calls refresh `last_seen`; presence becomes stale after 15 minutes
without activity. Stale presence does not claim a process is dead.

Preview a Codex entry at an isolated path:

```sh
bin/clawbridge mcp install --agent codex --config /tmp/clawbridge-codex.toml \
  --command "$PWD/bin/clawbridge" --db "$CLAWBRIDGE_DB" \
  --workspace demo --project clawbridge --session codex-mcp --dry-run
```

Remove `--dry-run` to write that target. Without `--config`, Codex uses
`$CODEX_HOME/config.toml` or `~/.codex/config.toml`; Claude Code uses `.mcp.json`
in the current directory. `--agent claude-code` selects Claude's JSON format.
Existing unrelated entries are preserved, conflicting names are rejected, and
existing bytes are backed up before an atomic write. Identical repeats make no
change. Choose distinct `--name` values for multiple entries in one file.

The installer follows the official [Codex MCP configuration](https://developers.openai.com/codex/mcp)
and [Claude Code MCP configuration](https://code.claude.com/docs/en/mcp).
It writes config only when invoked; it never starts agents. The provider-neutral
agent instructions live in [skills/clawbridge](skills/clawbridge/SKILL.md).

## Terminal interface

The Phase 4 terminal entry uses the same saved authority and pinned project:

```sh
bin/clawbridge tui
bin/clawbridge tui --screen doctor
```

Run it in an interactive terminal. Use 1–6 or Tab to switch screens, `?` for
help and `q` or Ctrl-C to quit. Overview, Projects and Sessions show existing
read views. Inbox send/reply/mark-read require an explicitly selected current
session and instance; opening the UI does not register or resume an actor.
Cloud is informational: infrastructure provisioning is unavailable. Use the
guided startup or CLI setup/join commands first. New project administration is
an explicit CLI operation; startup does not create projects or memberships.

Session records include their owner and optional host/local-account labels. The
Sessions view presents this attribution with details for the selected session.
In cloud mode, the owner comes from authenticated Google identity; host and local
account are descriptive client-supplied labels. Supply the existing `--host`
option when registering a session to record its execution host. Older sessions
can show “Not recorded”; a shared host can have sessions from several users.
See [host attribution design](HOST-IDENTITY-DESIGN.md).

Phase 4 automated fixtures and the earlier Phase 3 live Google/macOS evidence
are separate. See [PHASE-4-REVIEW.md](PHASE-4-REVIEW.md) for the current acceptance
status; a new real clean-machine onboarding journey is not yet claimed.

## Behavior and verification

See [CORE-API.md](CORE-API.md) for exact commands, arguments, pagination, and result
shapes. [REVIEW.md](REVIEW.md) records independent acceptance results and limits.
[Installed-client checks](docs/CLIENT-CHECKS.md) distinguish actual Codex exchange
from Claude connection/discovery and from model-driven workflow testing.

```sh
go test ./...
go test -race ./...
go vet ./...
```

The hosted service requires Postgres and validates Google-issued ID tokens without
trusting a caller-supplied email. CLI and MCP clients can sync a project-scoped
snapshot and read it during an outage; writes never fall back to a writable local
bus. See [PHASE-2-PLAN.md](PHASE-2-PLAN.md), [CORE-API.md](CORE-API.md), and
[PHASE-2-REVIEW.md](PHASE-2-REVIEW.md). Cross-project links, resource provisioning,
and notifier adapters remain future work. Deployment choices, including one Compute
Engine VM, are discussed in [docs/DEPLOYMENT-OPTIONS.md](docs/DEPLOYMENT-OPTIONS.md),
with a reviewable template in [deploy/README.md](deploy/README.md).

Repository: [jkai-chen/ClawBridge](https://github.com/jkai-chen/ClawBridge), initially
private. The owner has not selected a distribution licence.

## Linux credential storage

OAuth refresh tokens use Linux Secret Service, not plaintext files. The
ClawBridge process and the unlocked keyring must share the same user D-Bus
session. This also applies to WSL and headless SSH: device sign-in changes the
Google consent flow; it does **not** remove the native credential-store
prerequisite. Windows Credential Manager is not automatically bridged into WSL.

To inspect the current session without starting a service or accessing a token:

```sh
busctl --user --timeout=3 call org.freedesktop.DBus /org/freedesktop/DBus \
  org.freedesktop.DBus NameHasOwner s org.freedesktop.secrets
```

`b true` confirms service presence only, not that the keyring is unlocked or
write-ready. If the bus or service is unavailable, restore the existing desktop
or SSH session in which the keyring was available, and run ClawBridge there.
An already-authorized profile can run `whoami` again from that session; do not
rejoin merely to repair the D-Bus environment. ClawBridge does not start services,
provision a keyring, capture a keyring password, or fall back to plaintext storage.

### TUI sizing and retained archive views

This client increment passed independent automated review; see
[TUI-ARCHIVE-REVIEW.md](TUI-ARCHIVE-REVIEW.md) for checks and evidence limits.
Native Linux resize/archive interaction remains a separate live acceptance step.

The TUI uses the terminal's initial size and resize events, without refreshing
or changing authority. Content scrolls with PageUp/PageDown; arrows or j/k select
rows and keep the selection visible. Headers, tabs, editor/cancel controls and
help share the available screen. Below 24 columns or 9 rows, resize the terminal;
quit and Escape still work. Long selected session attribution, message bodies,
project details and diagnostics remain available through content scrolling.

On Sessions and Inbox, `v` cycles **Current**, **Archive**, **All**. Archive is a
derived display filter, not a delete or retention setting: retired/moved sessions
archive 24 hours after their last update; inbox deliveries archive 24 hours after
that recipient's read stamp. Unread deliveries and active sessions stay Current.
Missing, invalid or future timestamps stay Current. Classification updates locally
once a minute and on entry/refresh/filter changes, without network polling.

Filters apply only to the loaded server page. `]` requests the next bounded page;
`[` revisits a previous page; `r` resets paging and reloads. Empty filtered pages
may still have a next page. The header shows filter/page/remaining-page status.
`mirror*` means incomplete, read-only mirror coverage—not an exhaustive archive.
Browsing does not mark messages read; existing explicit read/send/reply actions,
actor requirements, authorization and expiry rules are unchanged.

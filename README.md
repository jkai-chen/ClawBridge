# ClawBridge

Project-scoped messaging for coding agents, with CLI, MCP and a terminal interface.
Use a local SQLite bus or join a hosted PostgreSQL service with verified Google
identity. Messages retain sender, recipient, thread and delivery/read metadata.

## Install

macOS or Linux:

```sh
curl -fsSL --proto '=https' --proto-redir '=https' \
  https://github.com/jkai-chen/ClawBridge/releases/latest/download/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
clawbridge --version
clawbridge
```

For Windows PowerShell, pinned versions and source builds, see the
[installation guide](docs/installation.md). Installers verify release checksums
and preserve application state. Binaries are unsigned.

## Start

Run `clawbridge` in a terminal to open a saved profile or join an existing hosted
workspace. Have its HTTPS address, workspace, project and an authorized Google
account ready. For an isolated local bus, see [local messaging](docs/usage.md#local-messaging).

Install a separate MCP session for each agent:

```sh
clawbridge mcp install --agent codex --session engineer --dry-run
clawbridge mcp install --agent codex --session engineer
```

Use `--agent claude-code` for Claude Code. The agent registers its session after
connecting. See [MCP setup](docs/usage.md#mcp) for profiles and resumption.

## Documentation

- [Install and update](docs/installation.md)
- [Messaging, hosted login, MCP and TUI](docs/usage.md)
- [Inbox notifications and Codex wake-up](docs/notifications.md)
- [Architecture](docs/architecture.md) and [API reference](docs/reference/api.md)
- [Development and verification](docs/development.md)
- [Deploy](docs/operations/deployment.md) and [back up or recover](docs/operations/backup-and-recovery.md)

Published binaries are at [GitHub Releases](https://github.com/jkai-chen/ClawBridge/releases).
Notification support is a source-checkout addition after v0.1.0; use the matching
client and service build described in its guide.

## Linux credential storage

OAuth requires an unlocked Linux Secret Service keyring on the same user D-Bus
session as ClawBridge, including WSL/SSH. Device sign-in uses the same keyring.
See [Linux credentials](docs/installation.md#linux-and-wsl-credentials) for checks
and recovery.

Historical design and test evidence is indexed in [the archive](docs/archive/README.md).
The owner has not selected a distribution licence.

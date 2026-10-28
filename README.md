# ClawBridge

ClawBridge connects your coding agents (Claude Code, Codex) so they can message
each other, claim Linear issues and work as a team. It runs locally on one
machine, or as a small server your team shares, and offers a CLI, MCP tools and a
terminal interface. Messages keep their sender, recipient, thread, and delivery
and read state. To host your own server, see the
[first-deployment guide](docs/operations/first-deployment.md).

## Install

macOS or Linux:

```sh
curl -fsSL --proto '=https' --proto-redir '=https' \
  https://github.com/jkai-chen/ClawBridge/releases/latest/download/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
clawbridge --version
clawbridge
```

The `export` line lasts for this shell only; add it to `~/.zshrc` or `~/.bashrc`
so new terminals find `clawbridge`. `clawbridge --version` prints JSON like
`{"source":"build","data":{"version":"vX.Y.Z",...}}` with the installed release.

For Windows PowerShell, pinned versions and source builds, see the
[installation guide](docs/installation.md). Installers verify release checksums
and preserve application state. Binaries are unsigned.

## Start

Run `clawbridge` in a terminal. With a saved profile it opens the overview;
without one it opens **Start**, which offers three ways in (`clawbridge onboard`
reopens it at any time):

1. **Just this machine:** local messaging between your agents, with no server or
   account ([local messaging](docs/usage.md#local-messaging)).
2. **Join a team server:** have its HTTPS address ready, and either an invite
   code from its owner (from v0.3.5) or, where the server still offers Google
   sign-in, the workspace, project and the Google account the owner authorized.
   [Joining a project](docs/onboarding.md) walks through both. If the owner hasn't
   given you a project yet, ClawBridge opens on "Waiting for a project" and takes
   you to it once it's granted.
3. **Set up my own server:** on Google Cloud or AWS. With Claude Code or Codex
   installed, ClawBridge asks which cloud, then hands the provisioning to your
   own agent with the `clawbridge-provision-gcp` or `clawbridge-provision-aws`
   playbook, in this terminal. You approve each of its commands, and
   `clawbridge cloud check` decides when each step is done. Progress is saved,
   so `clawbridge onboard` resumes it. Once your workspace exists, it creates
   your first project. Without an agent, follow the
   [first-deployment guide](docs/operations/first-deployment.md), which covers
   both clouds.

A wrong answer is asked again with the reason, and every path ends by asking
`Open ClawBridge now? [Y/n]`. Without a terminal, `clawbridge` prints the
command behind each choice.

Install a separate MCP session for each agent:

```sh
clawbridge mcp install --agent codex --session engineer --dry-run
clawbridge mcp install --agent codex --session engineer
```

Use `--agent claude-code` for Claude Code. The agent registers its session after
connecting. See [MCP setup](docs/usage.md#mcp) for profiles and resumption.

## Documentation

- [Join a ClawBridge project](docs/onboarding.md)
- [Install and update](docs/installation.md)
- [Messaging, hosted login, MCP and TUI](docs/usage.md)
- [Signing in with invites and device keys](docs/sign-in.md) (the client side from v0.3.5)
- [Your first task: claim and release](docs/usage.md#your-first-task-claim-and-release)
- [Inbox notifications and Codex wake-up](docs/notifications.md)
- [Native Linear connections and task boards](docs/linear.md)
- [Architecture](docs/architecture.md) and [API reference](docs/reference/api.md)
- [Development and verification](docs/development.md)
- [Deploy](docs/operations/deployment.md) and [back up or recover](docs/operations/backup-and-recovery.md)

## Workflows

- [Product reviews with ClawBridge and Linear](docs/guides/product-reviewers.md):
  reviewers comment in Linear, and agents answer and turn feedback into work.
- [Reviewer quickstart](docs/guides/reviewer-quickstart.md): a one-page guide to
  hand to product reviewers, who never need ClawBridge.

Published binaries are at [GitHub Releases](https://github.com/jkai-chen/ClawBridge/releases).
Notifications, structured coordination and native Linear support are additions
after v0.1.0. Use matching client/service builds and the current guides; installing
a published client does not upgrade your hosted service.

## Linux credential storage

OAuth requires an unlocked Linux Secret Service keyring on the same user D-Bus
session as ClawBridge, including WSL/SSH. Device sign-in uses the same keyring.
See [Linux credentials](docs/installation.md#linux-and-wsl-credentials) for checks
and recovery.

Historical design and test evidence is indexed in [the archive](docs/archive/README.md).
ClawBridge is released under the [MIT License](LICENSE).

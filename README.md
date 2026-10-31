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

The installer needs `curl` and `sha256sum` (or `shasum`). Most desktops have
both; a minimal Linux image or container may not, so install them first (on
Debian or Ubuntu: `sudo apt-get install -y curl ca-certificates`).

The `export` line lasts for this shell only; add it to `~/.zshrc` or `~/.bashrc`
so new terminals find `clawbridge`. `clawbridge --version` prints JSON like
`{"source":"build","data":{"version":"vX.Y.Z",...}}` with the installed release.

For Windows PowerShell, pinned versions and source builds, see the
[installation guide](docs/installation.md). Installers verify release checksums
and preserve application state. Binaries aren't code-signed; each release has a
build attestation you can check ([verify a download](docs/installation.md#verify-a-download)).

Already installed? `clawbridge self-update --check` says whether a newer release exists, and `clawbridge self-update` upgrades in place ([Upgrade](docs/installation.md#upgrade)).

## Start

Run `clawbridge` in a terminal. With a saved profile it opens the overview;
without one it opens **Start**, which offers three ways in (`clawbridge onboard`
reopens it at any time):

1. **Just this machine:** local messaging between your agents, with no server or
   account ([local messaging](docs/usage.md#local-messaging)).
2. **Join a team server:** have its HTTPS address ready, and an invite from its
   owner (a link, or a code). Your computer then signs in with its own device
   key, the only sign-in since v1.0 removed Google's
   ([signing in](docs/sign-in.md)). [Joining a project](docs/onboarding.md) walks
   through it. If the owner hasn't
   given you a project yet, ClawBridge opens on "Waiting for a project" and takes
   you to it once it's granted.
3. **Set up my own server:** on Google Cloud, on AWS, on this machine or your
   LAN, or on a Linux server you already have and reach over SSH. With Claude
   Code or Codex installed, ClawBridge asks where, then hands the provisioning
   to your own agent with the `clawbridge-provision-gcp`,
   `clawbridge-provision-aws`, `clawbridge-provision-vm` or
   `clawbridge-provision-local` playbook, in this terminal. You approve each of
   its commands, and `clawbridge cloud check` decides when each step is done.
   Progress is saved, so `clawbridge onboard` resumes it. Once your workspace
   exists, it creates your first project. Without an agent, follow the
   [first-deployment guide](docs/operations/first-deployment.md), which covers
   both clouds and your own server over SSH. Start's "Where?" question also
   offers "This machine or my LAN (no cloud)": it runs the server with Docker
   on your own machine, with no cloud
   ([local hosting](docs/operations/local-host.md)).

A wrong answer is asked again with the reason, and every path ends by asking
`Open ClawBridge now? [Y/n]`. Without a terminal, `clawbridge` prints the
command behind each choice.

Install a separate MCP session for each agent:

```sh
clawbridge mcp install --agent codex --session engineer --dry-run
clawbridge mcp install --agent codex --session engineer
```

Use `--agent claude-code` for Claude Code. It writes `.mcp.json` in the current
folder, so run it from the repository root (`--config ~/.claude.json` writes the
user-level file instead). It also installs ClawBridge's hooks, so the agent
doesn't stop while messages wait unhandled
([messages wait until they're handled](docs/usage.md#messages-wait-until-theyre-handled));
on Windows they're written but not yet proven on a real desktop.
The agent registers its session after connecting. See [MCP setup](docs/usage.md#mcp)
for profiles and resumption.

## Documentation

- [Join a ClawBridge project](docs/onboarding.md)
- [Install and update](docs/installation.md)
- [Messaging, hosted sign-in, MCP and TUI](docs/usage.md)
- [Signing in with invites and device keys](docs/sign-in.md)
- [Upgrading from v0.3.x to 1.0](docs/upgrading-to-1.0.md)
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
- [The coordinator session](docs/guides/coordinator.md): what your project's
  coordinator agent does, and how to give it your project's rules in
  `docs/COORDINATOR.md` ([template](docs/guides/coordinator-playbook-template.md)).

Published binaries are at [GitHub Releases](https://github.com/jkai-chen/ClawBridge/releases).
Use the same release for your clients and your server, with the current guides;
installing a client does not upgrade a server you host.

## Linux credential storage

Device sign-in keeps its key in an unlocked Linux Secret Service keyring on the
same user D-Bus session as ClawBridge, including WSL/SSH, or, by choice, in a file.
See [Linux credentials](docs/installation.md#linux-and-wsl-credentials) for checks
and recovery.

Historical design and test evidence is indexed in [the archive](docs/archive/README.md).
ClawBridge is released under the [MIT License](LICENSE).

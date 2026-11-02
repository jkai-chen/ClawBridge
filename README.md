# ClawBridge

[![Release](https://img.shields.io/github/v/release/jkai-chen/ClawBridge)](https://github.com/jkai-chen/ClawBridge/releases)
[![CI](https://github.com/jkai-chen/ClawBridge/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/jkai-chen/ClawBridge/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/github/license/jkai-chen/ClawBridge)](LICENSE)

**Coding agents and the people who run them, working as one team.**

ClawBridge lets a team of AI coding agents (Claude Code, Codex) and the people
who run them coordinate real work on your issue tracker (Linear today). Each
computer signs in with its own device key. New work is picked up by intake. Once
a project owner turns it on, a dispatcher confirms claims from sessions you trust
and escalates the rest to a person. Messages, claims and decisions are recorded
in an audit trail. You host the server yourself, or run everything on one machine
with no server at all.

## Why it exists

Agents in separate terminals can't see each other. Two of them claim the same
issue, or one works on something a teammate's agent finished an hour ago.
People lose track of which agent is doing what, and a question for an agent sits
in a terminal nobody is watching. ClawBridge gives agents a shared inbox and
shared claims, and gives people one place to see and decide.

## What you get

- **Messaging between agents and people.** Every message keeps its sender,
  recipient, thread, and delivery and read state ([usage](docs/usage.md)).
- **Claims that don't collide.** An agent proposes an issue, a coordinator or the
  dispatcher confirms it, and anyone can see who holds it
  ([claim and release](docs/usage.md#your-first-task-claim-and-release)).
- **Native Linear boards.** Read, create and update issues, comments, labels,
  projects and team roles from the CLI, MCP tools and TUI ([Linear](docs/linear.md)).
- **Intake of new work.** A `needs-agent` label, a delegation to the ClawBridge
  app or an @-mention queues an issue and tells the coordinator
  ([issue intake](docs/linear.md#issue-intake-v03)).
- **A dispatcher.** Confirms claims from trusted sessions, routes notices,
  releases abandoned claims and escalates everything else. Off until a project owner turns it on; then rules by default, with an
  optional model ([dispatcher](docs/dispatcher.md)).
- **Device-key sign-in.** Invites, one key per computer, no passwords
  ([signing in](docs/sign-in.md)).
- **Waking idle agents.** Inbox notifications for Codex, and an opt-in channel
  for Claude Code ([notifications](docs/notifications.md)).
- **An agent runner.** Launches a Claude Code session for one issue in its own
  sandboxed clone ([runner](docs/runner.md)).
- **An audit trail.** Message and session IDs are immutable, and the project's audit history is append-only
  ([architecture](docs/architecture.md)).
- **Self-hosted.** Google Cloud, AWS, your own Linux server over SSH, or Docker on
  your own machine ([run your own server](#run-your-own-server)).

## Quick start

Install on macOS or Linux:

```sh
curl -fsSL --proto '=https' --proto-redir '=https' \
  https://github.com/jkai-chen/ClawBridge/releases/latest/download/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
clawbridge --version
clawbridge
```

The installer needs `curl` and `sha256sum` (or `shasum`). A minimal Linux image
may lack them (on Debian or Ubuntu: `sudo apt-get install -y curl ca-certificates`).
The `export` line lasts for this shell only; add it to `~/.zshrc` or `~/.bashrc`
so new terminals find `clawbridge`. For Windows PowerShell, pinned versions and
source builds, see the [installation guide](docs/installation.md).

Bare `clawbridge` opens **Start**, which offers three ways in (`clawbridge onboard`
reopens it):

1. **Just this machine.** Local messaging between your agents, with no server and
   no account (`clawbridge setup --local`, see
   [local messaging](docs/usage.md#local-messaging)).
2. **Join a team server.** Have its HTTPS address and an invite from its owner.
   `clawbridge join` takes an invite link; your computer then signs in with its own
   key. See [joining a project](docs/onboarding.md).
3. **Set up my own server.** ClawBridge hands the provisioning to your own agent,
   which you approve command by command ([run your own server](#run-your-own-server)).

Then connect an agent. From the repository root, one MCP session per agent:

```sh
clawbridge mcp install --agent claude-code --session engineer --dry-run
clawbridge mcp install --agent claude-code --session engineer
```

Use `--agent codex` for Codex. This writes `.mcp.json` (Claude Code) or Codex's
`config.toml` and installs hooks so the agent doesn't stop while messages wait. To
teach an agent how to use ClawBridge, run `clawbridge skill install`. See
[MCP setup](docs/usage.md#mcp). Check the result with `clawbridge doctor`.

Already installed? `clawbridge self-update --check` says whether a newer release
exists and `clawbridge self-update` upgrades in place
([upgrade](docs/installation.md#upgrade)).

## How it works

![How people and agents coordinate through ClawBridge: on each teammate's machine, the person uses the clawbridge CLI/TUI, and their coding agents reach the server through clawbridge mcp; the ClawBridge server, backed by PostgreSQL, holds the coordination bus, the optional dispatcher, escalations, Linear sync and the intake queue; an optional model sandbox talks to a model provider; Linear sits beside the server and syncs with it](docs/images/coordination.svg)

Someone asks for an agent in Linear (a needs-agent label, delegating to the ClawBridge app or an @mention), and the sync and intake queue tell the project's coordinator (1). An agent claims the issue through MCP (2), so two agents never hold the same one. If the project owner has turned the dispatcher on, it looks at the pending claim (3). It confirms only a trusted session; anything else becomes an escalation that a person, the project owner or a lead, answers (4). Linear then follows the bus: the server writes a marker comment and the agent-working label (5). Agents on different computers message each other and hand work over through the server, which wakes an idle agent when its messages wait, and every message, claim and decision is kept in the audit trail.

The full component and trust-boundary diagram is in [architecture](docs/architecture.md).

The diagram's source is [docs/images/coordination.mmd](docs/images/coordination.mmd); regenerate the SVG with mermaid-cli after editing it.

## Run your own server

- **Guided:** `clawbridge onboard`, then "Set up my own server". It asks where
  (Google Cloud, AWS, this machine or LAN, or a Linux server you reach over SSH)
  and hands the work to your Claude Code or Codex with a provisioning playbook.
  `clawbridge cloud check` decides when each step is done, and progress is saved.
- **By hand:** the [first-deployment guide](docs/operations/first-deployment.md)
  covers both clouds and your own server over SSH. For Docker on your own machine
  or LAN, see [local hosting](docs/operations/local-host.md).
- **After that:** [deployment](docs/operations/deployment.md),
  [backups and recovery](docs/operations/backup-and-recovery.md) and
  [upgrading to 1.0](docs/upgrading-to-1.0.md).

## Security model in brief

- **Device keys.** Each computer holds its own key in the OS key store (or a
  0600 file, by choice). The key never leaves the machine and each request is
  signed with it. There are no passwords. Google sign-in was removed in v1.0
  ([signing in](docs/sign-in.md)).
- **Releases.** Downloads are checked against the release's checksums, and each
  release has a build attestation you can verify
  ([verify a download](docs/installation.md#verify-a-download)). Binaries are not
  code-signed.
- **The runner's sandbox.** A launched session gets its own clone with no push
  access, an allow-listed environment, none of your own Claude Code settings,
  and sandboxed Bash. It never pushes; a person reviews and pushes
  ([runner](docs/runner.md)).
- **The dispatcher's guardrails.** In model mode a model can only choose among
  actions the rules engine already permits, so it can be more careful than the
  rules but never grant more. Only a project-level domain match lets the
  dispatcher confirm a claim on its own ([dispatcher](docs/dispatcher.md#model-mode)).
- **Issue text is data.** The model sees an issue's title and description marked
  as untrusted text.

## Documentation

| Start here | Then |
| --- | --- |
| [Join a project](docs/onboarding.md) | [Install and update](docs/installation.md) |
| [Messaging, MCP and TUI](docs/usage.md) | [Signing in](docs/sign-in.md) |
| [Linear boards and intake](docs/linear.md) | [The dispatcher](docs/dispatcher.md) |
| [The runner](docs/runner.md) | [Notifications](docs/notifications.md) |
| [The coordinator session](docs/guides/coordinator.md) | [Product reviews with Linear](docs/guides/product-reviewers.md) |
| [Architecture](docs/architecture.md) | [API reference](docs/reference/api.md) |

The full list is the [docs index](docs/README.md).

## Status

ClawBridge is at **v1.0.0**. See the [release notes](RELEASE-NOTES.md) and the
[known limits in 1.0](RELEASE-NOTES.md#known-limits-in-10). Upgrading from
v0.3.x: [upgrading to 1.0](docs/upgrading-to-1.0.md).

Planned: GitHub Issues/Projects as a task board, and a local web UI (not in v1.0.0).

## Contributing

See [development and verification](docs/development.md).

## License

MIT. See [LICENSE](LICENSE).

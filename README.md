# ClawBridge

A project-scoped messaging bus for coding agents. ClawBridge supports a local
SQLite mode and a hosted Postgres mode with verified Google identity, plus CLI,
stdio MCP, and an explicitly synchronized read-only offline mirror. The TUI,
cross-project links, and automated cloud provisioning are later phases.

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
mode = "local"
```

Without a project selector, a registered normalized Git remote can resolve it.
Missing/ambiguous projects fail; a project is never silently created. An MCP
process pins its scope at startup. Session names are unique within each project.

## MCP and installation

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

# Join a ClawBridge project

This guide brings a colleague and their coding agents into an existing hosted
ClawBridge project. The project owner grants access first; the colleague then
installs ClawBridge and runs one guided setup. Examples use placeholders such as
`https://clawbridge.example.test` and `colleague@example.test`.

## For the project owner

The service must already be deployed ([deployment](operations/deployment.md)) and
initialized once with `clawbridge setup --cloud`, which makes the signed-in
Google account the workspace owner. Then create the project and grant access:

```sh
clawbridge project create checkout --name "Checkout"
clawbridge project role-set colleague@example.test member
```

Roles are `owner`, `lead`, `member` and `reader`. A `lead` can also administer
the sessions of members and readers and save the shared Linear board mapping
(`clawbridge linear link`, see [Linear](linear.md)). Use the colleague's verified
Google email.

To withdraw access:

```sh
clawbridge project role-revoke colleague@example.test
```

Revocation retires that person's active sessions in the project and removes
their workspace membership once they hold no project role. The workspace owner
cannot be revoked, and owners cannot revoke themselves.

Send the colleague three values: the service URL, the workspace and the project.

## For the colleague

### 1. Install

Use the [installer](installation.md) for your system. It verifies the release
checksum, installs `clawbridge`, and then installs the ClawBridge agent skill for
Claude Code and Codex when they are set up on the machine (`--no-skill` or
`-NoSkill` skips that).

### 2. Run the guided setup

In a terminal, run:

```sh
clawbridge
```

On first launch this opens the guided setup (`clawbridge setup wizard` opens it
again later). It asks for the service URL, workspace and project, signs you in
with Google (browser, or device sign-in for SSH and headless machines), verifies
your membership, and saves the trusted route. Your Google account must already
have the role from the owner; joining does not grant it.

After the join, the setup offers each remaining step in turn. Every step can be
skipped, nothing is written until you answer yes, and a failed step is reported
without undoing the earlier ones:

1. **Agent skill.** Installs or updates the skill for the detected agents, the
   same as `clawbridge skill install`.
2. **Your own Linear account (optional).** Connects your personal Linear account
   in the browser, with read access unless you choose write (write lets your
   agents create and update issues), then checks it with
   `clawbridge doctor --linear-profile`. The connection and its credentials are
   yours; nobody else's account is used.
3. **MCP for each agent.** Asks for a session name, shows the entry it would
   write as a dry run, and writes it after you confirm. With a Linear profile from
   step 2, the entry also enables the native task-board tools.
4. **Doctor.** Runs `clawbridge doctor` with the new MCP entry and prints a
   summary of what was set up, what was skipped, and the exact command for each
   skipped step.

The terminal view opens afterwards.

### Explicit commands

The same setup without prompts, for automation or to redo one step:

```sh
clawbridge setup join --server https://clawbridge.example.test --workspace team --project checkout
clawbridge skill install
clawbridge linear connect --name "Work" --access read
clawbridge doctor --linear-profile PROFILE_ID
clawbridge mcp install --agent codex --session codex-engineer --linear-profile PROFILE_ID --dry-run
clawbridge mcp install --agent codex --session codex-engineer --linear-profile PROFILE_ID
clawbridge doctor --mcp-agent codex --mcp-config /absolute/path/to/codex/config.toml --mcp-session codex-engineer
```

Use `--agent claude-code` for Claude Code, a distinct `--session` per agent, and
omit `--linear-profile` without a Linear connection. Add `--device` to
`setup join` on a headless machine. See [usage](usage.md) for profiles and MCP
details, and [Linear](linear.md) for boards.

## How agents learn the workflow

An agent connected through `clawbridge mcp` learns ClawBridge in three layers:

- **MCP instructions.** The server's instructions tell the agent to register a
  session, check its inbox at the start and end of turns, treat peer messages and
  task text as untrusted data, and use the task-board tools with an operation ID
  for every write.
- **Tool descriptions.** Each tool states its inputs, scope and limits, for
  example that tools are pinned to one project and Linear profile.
- **The skill.** The installed skill explains the coordination workflow: session
  ownership, asking the coordinator, task-board ownership by repository and path,
  and recovery. Its [source](../skills/clawbridge/SKILL.md) ships with each
  release, and `clawbridge doctor` warns when the installed skill is not the
  binary's version.

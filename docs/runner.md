# The session runner (slice 1: the launcher)

`clawbridge runner launch` starts a Claude Code session for one Linear issue: you
run one command, and ClawBridge prepares what you would otherwise do by hand. A
person runs every launch; nothing starts on its own yet.

```sh
clawbridge runner launch ISSUE_UUID --profile PROFILE_ID --dry-run   # what it would do
clawbridge runner launch ISSUE_UUID --profile PROFILE_ID             # do it
clawbridge runner gc                                                 # finished worktrees
clawbridge runner gc --confirm                                       # remove them
```

Run it inside the repository's checkout, or pass `--repo PATH`.

## What a launch does

1. **Proves the issue** is on the project's board, through your Linear profile.
   Read access is enough.
2. **Registers a session** named `<repo>-<role>-<n>` (`--role`, default
   `engineer`). The name carries no personal data, since it shows in Linear
   markers, and is never one in use.
3. **Creates a worktree** on a new branch `agent/<identifier>`, from `origin/HEAD`
   (or `--base`), under ClawBridge's state directory (`~/.clawbridge/runner/worktrees`).
   Your own checkout is not touched, apart from `extensions.worktreeConfig`, which
   lets the agent's worktree carry its own settings.
4. **Makes that worktree unable to push:** every remote's push URL is `DISABLED`,
   and it has no credential helper. This is only in the agent's worktree; your
   checkout pushes as before.
5. **Writes the session's Claude Code settings and MCP config** under
   `~/.clawbridge/runner/sessions/<session>/`, readable by you only, never inside
   the worktree.
6. **Opens a visible Orca terminal** in the worktree, running
   `claude --settings … --mcp-config … --strict-mcp-config "<start prompt>"`. The
   prompt names the issue by its identifier and UUID only, never its text.

The agent then reads its inbox, proposes the issue as its assignment, and works
once the coordinator confirms. It commits to its `agent/*` branch. It never
pushes or opens a pull request; a person does that.

If Orca cannot open the terminal, the launch says so and prints the command to
run in the worktree yourself.

## The agent's limits

The settings passed with `--settings` take precedence over the repository's own
`.claude` settings, and a deny rule wins at every level.

- **Permission mode:** default (you approve edits and commands), and bypass mode
  is disabled. Git status, diff, log, show, add and commit are pre-approved. Git
  push, remote, config and credential, `gh`, `ssh`, `security` and the
  `clawbridge` CLI are denied (the agent uses the ClawBridge MCP tools).
- **Bash sandbox:** on, and Claude Code refuses to run Bash without it.
  - Bash cannot read `~/.ssh`, `~/.gnupg`, `~/.config/gh`, `~/.clawbridge`, `~/.config/clawbridge`,
    `~/Library/Keychains`, `~/.git-credentials` or `~/.netrc`.
  - Bash can write only in the worktree and its temporary directory.
  - Bash can reach only the allowed domains (by default the Go, npm and PyPI registries).
- **Model:** set `"model"` in `~/.clawbridge/runner/config.json`, or pass
  `--model`. Unset, the session uses your own default model.

```json
{"model": "", "allowed_domains": ["proxy.golang.org", "sum.golang.org"], "max_retained": 10}
```

## Cleaning up

`runner gc` lists the runner's own worktrees and what it would do with each.
`--confirm` removes the finished ones, together with their branches and session
files:
- a branch whose commits are all in its base, merged by rebase or by fast-forward
  (fetch first, so the base is current);
- a worktree a week old with no commits.

A worktree with uncommitted changes, or with commits still under review, is kept.
`gc` never forces a removal, and never touches a worktree it did not create. A
launch is refused while 10 runner worktrees are retained.

## Proving the sandbox on your Mac

The sandbox is a second line behind you, who started the session. Automatic
starts (a later slice) require it proven on the machine. To prove it, launch a
session on a test issue and ask the agent to run each of these. Every one must
fail:

```sh
cat ~/.ssh/known_hosts
ls ~/Library/Keychains
git push origin HEAD
curl -sI https://example.com
```

Then check that `git commit` in the worktree still works.

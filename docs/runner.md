# The session runner (slice 1: the launcher)

`clawbridge runner launch` starts a Claude Code session for one Linear issue: you
run one command, and ClawBridge prepares what you would otherwise do by hand. A
person runs every launch; nothing starts on its own yet.

```sh
clawbridge runner launch ISSUE_UUID --profile PROFILE_ID --dry-run   # what it would do
clawbridge runner launch ISSUE_UUID --profile PROFILE_ID             # do it
clawbridge runner gc                                                 # finished clones
clawbridge runner gc --confirm                                       # remove them
```

Run it inside the repository's checkout, or pass `--repo PATH`.

## What a launch does

1. **Proves the issue** is on the project's board, through your Linear profile.
   Read access is enough.
2. **Registers a session** named `<repo>-<role>-<n>` (`--role`, default
   `engineer`). The name carries no personal data, since it shows in Linear
   markers, and is never one in use.
3. **Makes the agent its own clone** of your checkout, in the work directory
   (`~/clawbridge-runs/<session>` by default).
   - The clone is on a new branch `agent/<identifier>`, from `origin/HEAD` (or
     `--base`), and has its own object store.
   - The agent's commits stay there. Your repository and its `.git` are never
     written.
   - The clone's `origin` fetches from your upstream. It has no push URL and no
     credential helper.
4. **Writes the session's Claude Code settings and MCP config** under
   `~/.clawbridge/runner/sessions/<session>/`, readable by you only, never inside
   the clone.
5. **Opens a visible Orca terminal** in the clone, running
   `claude --settings … --mcp-config … --strict-mcp-config "<start prompt>"`. The
   prompt names the issue by its identifier and UUID only, never its text.

The agent then reads its inbox, proposes the issue as its assignment, and works
once the coordinator confirms. It commits to its `agent/*` branch. It never
pushes or opens a pull request; a person does that.

When the work is ready, bring it into your checkout with the command the launch
prints, review it there, and push it yourself:

```sh
git fetch ~/clawbridge-runs/<session> agent/<identifier>:agent/<identifier>
```

If Orca cannot open the terminal, the launch says so and prints the command to
run in the clone yourself.

## The agent's limits

The settings passed with `--settings` take precedence over the repository's own
`.claude` settings, and a deny rule wins at every level.

- **Permission mode:** default (you approve edits and commands), and bypass mode
  is disabled. Git status, diff, log, show, add and commit are pre-approved.
  Denied:
  - git push, remote, config and credential; `gh`; `ssh`; `security`; the
    `clawbridge` CLI (the agent uses the ClawBridge MCP tools instead);
  - WebFetch;
  - editing anything in your own checkout.
- **Bash sandbox:** on, and Claude Code refuses to run Bash without it.
  - Bash cannot read `~/.ssh`, `~/.gnupg`, `~/.config/gh`, `~/.clawbridge`,
    `~/.config/clawbridge`, `~/Library/Keychains`, `~/.git-credentials`, `~/.netrc`,
    `~/.aws`, `~/.config/gcloud` or `~/.docker`.
  - Bash writes only in its clone and its temporary directory.
  - Bash reaches only the allowed domains (by default the Go, npm and PyPI
    registries).
- **The file tools:** the sandbox covers Bash only, so Read and Edit are denied
  the same paths.
- **What stops a push:** the clone's missing push URL and the `git push` deny
  rule are only a first line. The agent can edit its own clone's config, or name
  a URL. The real barrier is that the agent has no credentials and no route to
  your git host. The runner refuses an `allowed_domains` entry that reaches a host
  any of your remotes fetches from or pushes to.
- **Model:** set `"model"` in `~/.clawbridge/runner/config.json`, or pass
  `--model`. Unset, the session uses your own default model.

```json
{"model": "", "allowed_domains": ["proxy.golang.org", "sum.golang.org"], "max_retained": 10, "work_dir": "~/clawbridge-runs"}
```

The work directory must be outside every denied path: the sandbox has to be able
to read the agent's own clone.

## Cleaning up

`runner gc` lists the runner's own clones and what it would do with each.
`--confirm` removes the finished ones, together with their session files.
- **Merged:** you fetched the branch, your copy matches the clone's, and its
  commits are all in the base, merged by rebase or by fast-forward. Fetch first,
  so the base is current. Your fetched copy of the branch is deleted too.
- **Idle:** no commits and no changed file in a week.

Everything else is kept: commits you have not fetched, work under review, recent
changes. A launch is refused while 10 runner clones are retained.

`gc` never runs git inside an agent's clone. The clone's configuration and hooks
are the agent's to write, and would run unsandboxed in your shell. `gc` reads the
clone's branch from its files and judges the work in your own repository.

## Proving the sandbox on your Mac

The sandbox is a second line behind you, who started the session. Automatic
starts (a later slice) require it proven on the machine. Launch a session on a
test issue, and check that the agent's ordinary work still works:

```sh
git commit -m test --allow-empty
go test ./...
```

Then ask it to run each of these. Every one must fail:

```sh
cat ~/.ssh/known_hosts
ls ~/Library/Keychains
echo x > <your checkout>/.git/hooks/pre-commit
git push origin HEAD
git push https://github.com/<owner>/<repo>.git HEAD
curl -sI https://example.com
```

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
5. **Opens a visible Orca terminal** in your own checkout, which Orca manages,
   running `cd <clone> && exec claude --setting-sources '' --permission-mode default
   --settings … --mcp-config … --strict-mcp-config "<start prompt>"`. When the
   session ends, the tab closes: your own shell never stays in the clone.
   - The session loads none of your user, project or local Claude Code settings,
     only its own (and managed ones), so your own allows never widen it.
   - The prompt names the issue by its identifier and UUID only, never its text.

Before anything starts, the launch checks that:
- `claude` is 2.1.219 or newer (the sandbox's strict allow-list) and of the
  major version the runner was proven on, 2.1.310;
- managed Claude Code settings, if any, do not allow a host the runner's own
  list does not.

The agent then reads its inbox, proposes the issue as its assignment, and works
once the coordinator confirms. It commits to its `agent/*` branch. It never
pushes or opens a pull request; a person does that.

**Never run git inside a runner clone.** Its configuration and attributes are
the agent's to write, and git there could run commands of its choosing as you.
When the work is ready, bring it into your own checkout, **review the diff**, and
push it yourself:

```sh
clawbridge runner fetch <session>
```

`runner fetch` never runs git inside the clone. The branch first lands under
`refs/clawbridge-runner/<session>`, and every line its commits add, removed
again or not, is scanned for token shapes (GitHub, AWS, Slack, Google, Linear
and Anthropic keys, private keys). If one is found, nothing is fetched and the
report names the file and pattern, never the text. A clean branch becomes
`agent/<identifier>`. The scan is a backstop. You still review every agent diff
before you push it.

If Orca cannot open the terminal, the launch says so and prints the command to
run in the clone yourself.

## The agent's limits

The settings passed with `--settings` take precedence over the repository's own
`.claude` settings, and a deny rule wins at every level.

- **Permission mode:** default (you approve edits and commands). Bypass mode and
  auto mode are both disabled. Git status, diff, log, show, add and commit are pre-approved.
  Denied:
  - git push, remote, config and credential; `gh`; `ssh`; `security`; the
    `clawbridge` CLI (the agent uses the ClawBridge MCP tools instead);
  - WebFetch;
  - editing anything in your own checkout.
- **Bash sandbox:** on, and Claude Code refuses to run Bash without it.
  - Bash reads **nothing in your home directory** except the agent's clone, its
    cache and your Go module cache. Everything else under `~`, every
    credential store included, is denied.
  - Outside `~`, the system and the toolchains stay readable.
  - The clone carries your git name and email itself, since `~/.gitconfig` is not
    readable. Nothing else of that file carries over: includes, aliases and
    credential helpers are left behind on purpose.
  - Bash writes only in its clone, its temporary directory and its cache
    (`<work>/<session>.cache`). Go, npm and XDG caches point there, never at
    yours, and `gc` removes it with the clone.
  - Go modules come read-only from your own module cache
    (`GOPROXY=file://…/cache/download,off`, `GOTOOLCHAIN=local`): inside the
    sandbox, Go cannot verify TLS on macOS. A module you do not have yet is
    reported missing; download it in your own checkout.
  - Bash reaches only the allowed domains (by default the Go, npm and PyPI
    registries). The allow-list is strict: any other host is refused, never
    offered to you to approve. Your git hosts are also denied in WebFetch's
    domain form, which the sandbox's network proxy honors.
- **The file tools:** the sandbox covers Bash only, so Read and Edit are denied
  the known credential locations by name: `~/.ssh`, `~/.gnupg`, `~/.config/gh`,
  `~/.clawbridge`, `~/.config/clawbridge`, `~/Library/Keychains`,
  `~/.git-credentials`, `~/.netrc`, `~/.aws`, `~/.azure`, `~/.config/gcloud`,
  `~/.docker`, `~/.kube`, `~/.npmrc`, `~/.pypirc`, `~/.config/op`, `~/.claude`,
  `~/Library/Application Support` and `~/.cache`. Add more, such as private
  repositories, with `"denied_extra"`.
  - Default mode also asks you before a file tool reads outside the clone.
- **The self-test:** the session's first step is a script the launch wrote. It
  tries to read each of those locations that exists on your Mac, inside the real
  sandbox, and reports READ or DENIED for each. The launch waits for the report
  (three minutes at most). One READ, a malformed report, or none at all closes
  the terminal and retires the session before it does any work.
  - This proves the sandbox on this machine at each start. It still depends on
    the session running its first step as asked; a session that doesn't is
    stopped by the wait.
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

## Proven so far

The first live run, on Claude Code 2.1.310, stopped on a failure, and the
launcher was changed as above:
- Held: reading `~/.ssh` and listing `~/Library/Keychains` from Bash were
  refused by the sandbox; a write to the owner's `.git/hooks` was refused; the
  Read and Write tools were denied `~/.ssh`; a push through `origin` failed; a
  commit worked.
- Failed: `curl https://example.com` and an https push to the git host reached
  the network, because the owner's own Claude Code settings allowed those two
  domains for WebFetch, and Claude Code pre-allows such domains in the sandbox.
  The session now loads none of those settings.
- Also fixed: `go test` could not write Go's build cache; Orca would not open a
  terminal in a directory it does not manage; auto mode could be turned on.

## Proving the sandbox on your Mac

The sandbox is a second line behind you, who started the session. Automatic
starts (a later slice) require it proven on the machine. Launch a session on a
test issue, and check that the agent's ordinary work still works:

```sh
git commit -m test --allow-empty
go test ./...
```

Then ask it to run each of these, and answer **no** to every permission prompt
(a barrier that holds only when a person says no is not a barrier). Every one
must fail:

```sh
cat ~/.ssh/known_hosts
ls ~/Library/Keychains
echo x > <your checkout>/.git/hooks/pre-commit
git push origin HEAD
git push https://github.com/<owner>/<repo>.git HEAD
curl -sI https://example.com
curl -sI https://github.com
```

`curl -sI https://proxy.golang.org` should still work, and switching the
session's permission mode (shift+tab) to auto must be refused.

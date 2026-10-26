# ClawBridge v0.3.2

**No schema change.**
- v0.3.2 uses schema 16, as v0.3.1 does, so nothing migrates. A server can go back to v0.3.1 by swapping the image.
- Upgrade the server first. Clients don't have to upgrade: v0.3.1 clients keep working against a v0.3.2 server.
- A v0.3.2 client adds `clawbridge runner`, `linear doc publish` and the `linear initiative show` fix
  (`install.sh` / `install.ps1`).
- No new environment variable, and the Compose file is unchanged.

## The session runner (optional, Claude Code only)

`clawbridge runner launch ISSUE_UUID --profile ID` starts a Claude Code session for one Linear issue, in its own clone
of your checkout, in a visible terminal. A person runs every launch: nothing starts on its own. `clawbridge runner gc`
removes finished clones. It is optional, and it launches Claude Code only. See docs/runner.md.

**What the session may do:**
- **Permission mode:** default, so you approve edits and commands. Bypass and auto mode are disabled.
  - Pre-approved: git status, diff, log, show, add and commit.
  - Denied: git push, remote, config and credential; `gh`; `ssh`; the `clawbridge` CLI; WebFetch.
- **Bash runs only in Claude Code's sandbox,** and Claude Code refuses to run Bash without it.
  - **Reads:** nothing in your home directory except the clone, its cache and your Go module cache (read-only).
  - **Writes:** only the clone, its temporary directory and its cache.
  - **Network:** only the allowed domains (by default the Go, npm and PyPI registries). Any other host is refused,
    never offered for you to approve.
- **The file tools** (Read, Edit) are outside that sandbox. They are denied your known credential locations by name
  (`~/.ssh`, `~/.config/gh`, `~/.clawbridge`, keychains and others), and default mode asks before they read outside
  the clone.
- **The session loads none of your own Claude Code settings,** only its own and managed ones, so your allow-lists
  never widen it.
- **What stops a push:** the clone has no push URL, and `git push` is denied, but these are a first line only. The
  real barrier is that the agent has no credentials and no route to your git host. The runner refuses an allowed
  domain that reaches one of your remotes.

**The self-test, and its limit:**
- Each session's first step is a script that tries to read every credential location on your machine, plus files
  the launch plants under your home, from inside the sandbox.
- A single successful read, a malformed report or no report within three minutes closes the session before it does
  any work.
- **This catches a misconfigured sandbox. It is not a defence against a hostile agent,** which could write its own
  report. The sandbox itself is the boundary.

**Proven, and not yet proven:**
- Each deny was proven live on macOS with Claude Code 2.1.310.
- Go modules come only from your own module cache, since Go can't verify TLS inside the sandbox; a module you don't
  have yet is reported missing. That path is proven offline, and the first real launch confirms it.
- Launching into a terminal Orca didn't create falls back to printing the command.

## Publish documents from git, one way

`linear doc publish FILE --team|--linear-project|--initiative ID --operation-id ID --profile ID [--confirm]`. It is for
a project owner or lead, on the CLI only (never MCP), under the owner's document policy (`linear doc-policy set`).

**Before any Linear call:**
- the file must sit in the policy's `source_root` repository itself;
- its bytes are read with `git show` at one commit, never from your working tree;
- it must be valid UTF-8 with no @-mention;
- **every line, title and the footer are scanned against the policy's deny patterns.** A hit refuses, naming the
  pattern ID and line only, never the text. Only the owner may publish over a pattern, for that operation ID only,
  and that is recorded in the history;
- the document must fit in 49,000 bytes, unless `--split` (at `## ` headings, on request only).

**In Linear:**
- The parent must be a bound team, a bound project, or a listed initiative.
- A document is created once, read back, and its digest recorded.
- A changed source updates a document only if nobody edited it in Linear. An edit there blocks the next publish, and
  is never overwritten.

## Document-change notices

Once the project owner has set a document policy, a hosted server's watcher tells the project when a Linear document
in its scope changes.
- **What is heard:** a document under a bound team, a bound project, a listed initiative, or an issue a session holds.
  Every parent must be in scope, as for reads. On a team board, project documents are left out.
- **Who hears it:** the coordinator session gets a `linear-doc` message, and so does the session holding the issue,
  for that issue's documents. It names the document, its parent and when it changed, and whether ClawBridge published
  it; never its title or text.
- **An edit to a published document** is read back and compared with what was published, then flagged to the
  coordinator as `published_doc_edited`. The next publish of that file is refused until the edit is in git.
- **Cost:** at most one document call a tick, and at most 12 an hour per project. It runs after the comment watch,
  markers, intake and project health, and only while the hour keeps the comment watch's reserve. The first poll only
  starts the cursor: history is never replayed.

## The watcher's Linear budget is honest about markers

- Claim and release markers are now charged by the binding they must prove: 6 calls plus one per bound team and
  project per marker row, and 7 more when the row moves an issue's state.
- After each row, the server watcher reports what it really spent, and the unused part of the charge goes back to the
  hour.
- Before this, a state move was charged 3 calls but could spend 17 or more on a two-team binding, and nothing was
  reconciled.

## Fixes

- `linear initiative show ID` no longer asks for an operation ID: it is a read.

## For the host operator

- `operator linear-app-probe --confirm --p8 --project UUID [--dry-run]` settles whether the Linear app may post
  project health. It shipped in v0.3.1, and is documented now.
  - **It posts one real status update** to an empty throwaway project (no issues, members or lead), reads it back,
    deletes it, and checks it is gone.
  - It refuses before sending unless the app's tools can create, list and delete one. `--dry-run` stops there and
    prints those tools' fields.
  - It prints classes and the update's ID only, and ends with an `ALERT` naming the ID if it cannot prove the update
    deleted.

Assets retain five CGO-disabled targets, installers, the MIT licence, selected checksums and a full source
marker. Binaries are unsigned; macOS binaries are not notarized.

See docs/runner.md, docs/linear.md and docs/operations/deployment.md.

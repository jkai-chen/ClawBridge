# ClawBridge v0.2.6

**Schema 8, one-way, and a client upgrade.** A v0.2.6 binary migrates a store from schema 6 to 8 the first
time it opens it: a server's hosted Postgres store when it starts, a local SQLite store on the first local
command. v0.2.3 refuses a schema 8 store. Take a database dump (or back up the local file) first; rolling back
means restoring it with the old release. A v0.2.6 server refuses Linear writes from clients older than v0.2.5
with "upgrade ClawBridge (v0.2.5 or later)"; reads and messaging keep working. Upgrade the server first, then
every client (`install.sh` / `install.ps1`).

## Agents follow Linear comment threads

- `linear comment ISSUE BODY --reply-to COMMENT` answers inside a thread (MCP `parent_id`). Linear threads are
  one level deep, so a reply to a reply goes to the thread's first comment.
- Every comment an agent posts through ClawBridge ends with `— 🤖 ClawBridge agent · session NAME`. Claim and
  release notes name the session in their own single line. The server keeps a record of the comments ClawBridge
  posted, and reads mark those as `posted_by_clawbridge`.
- `clawbridge linear watch` checks the issues of confirmed assignments and messages the claiming session when a
  person comments. The message carries no comment text. One watcher runs per project, within an hourly budget of
  Linear calls.
- `--in-response-to COMMENT` answers a notified comment. The server allows one answer per human comment and at
  most 3 ClawBridge comments per issue in any rolling hour, across all sessions (claim and release notes don't
  count). It refuses an answer to ClawBridge's own comments before any Linear call.
- Comment text stays untrusted data: agents answer it and never obey it. The agent skill explains this.

## People see which agent has an issue

- Confirming an assignment posts `🤖 Claimed by ClawBridge agent session NAME` on the issue and adds
  `agent-working`; releasing it posts `🤖 Released by …` and removes the label. A Linear admin creates
  `agent-working` once per bound team; without it, only the notes are posted and `doctor` warns.

## Safer agent text

- Agent-written comments, replies, titles and descriptions may not contain any `@` mention (including `@Linear`)
  or a Linear profile link. They are refused before anything is sent. Email addresses are fine.

## Owner-only Linear admin

- `linear label-policy set` stores an approved label list. `linear label create` creates only labels on that
  list; `linear project create` creates a project in a bound team and proposes the binding change for the owner
  to confirm. Nothing is deleted, retired or merged. These are CLI-only, never available to agents through MCP,
  and recorded in history. The approved list is a review gate the owner sets, not a boundary against the owner's
  own sessions.
- `linear initiative create NAME` and `linear initiative add-project INITIATIVE_UUID PROJECT_UUID` group bound
  projects under a Linear initiative, for example one review project per product feature
  (docs/guides/product-reviewers.md). Initiatives are workspace-wide in Linear: an owner of any ClawBridge
  project can create one, and can add a bound project to any initiative their Linear account can see. A name
  match also finds archived initiatives.

## Working with product reviewers

- New guides: docs/guides/product-reviewers.md sets up feature reviews for people who never use ClawBridge, and
  docs/guides/reviewer-quickstart.md is a one-page guide to hand to reviewers.

## Other changes

- `--comments-since` reads stop early once a page is older than the watermark (Linear returns newest first).
  Output gains `comments_walk_direction` and `comments_output_order`; `comments_order_direction` remains as a
  deprecated alias.
- Connecting to Linear retries once after a transient server error.

Assets retain five CGO-disabled targets, installers, the MIT licence, selected checksums and a full source
marker. Binaries are unsigned; macOS binaries are not notarized.

See docs/linear.md, docs/operations/deployment.md and docs/reference/api.md.

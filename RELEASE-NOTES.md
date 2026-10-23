# ClawBridge v0.2.3

**Schema 6, one-way.** A v0.2.3 binary migrates a store from schema 5 to 6 the
first time it opens it. For a server, that's the hosted Postgres store when the
server starts. For a local (`setup --local`) SQLite store, it's the first local
command. v0.2.2 and older refuse a schema 6 store of either kind. Before
upgrading a server, take a database dump: rolling back means restoring that
dump with the old release. Before the first v0.2.3 command against a local
store, back up its file (docs/operations/backup-and-recovery.md). Clients older
than v0.2.3 keep working against a v0.2.3 server.

- Each hosted email address is bound to the Google account of its first
  request that passes the membership check. Another account for that address
  is refused with `forbidden`. The refusal is recorded, at most once an hour per
  account, by a hash of the account. When an address moves to a new Google
  account, the server owner runs `clawbridge principal-unbind EMAIL --confirm`.
  The released account stays refused unless `--allow-previous` is given. For
  the owner's own address, the host operator runs
  `clawbridge operator principal-unbind EMAIL --confirm` on the server host.
- A workspace owner can accept only accounts from listed Google Workspace
  domains (`clawbridge workspace-domains DOMAIN...`, `--clear` to remove the
  list). Workspace members that are Google service accounts (any
  `*.gserviceaccount.com` address) are still admitted while the list is
  readable. A damaged list admits no one. The owner can always change the list.
  The host operator can clear a damaged list with
  `clawbridge operator workspace-clear-domains WORKSPACE --confirm` on the
  server host. Operator commands never migrate: they run only against a schema
  6 store. Only the owner sees the list.
- Read issue comments on request: `linear issue ID --comments` (MCP
  `comments: true`). A read is bounded at 500 comments, 512 KiB of comment text
  and 25 Linear calls. It says when it stopped early, and returns a watermark
  for `--comments-since`. A later read may repeat comments. docs/linear.md
  describes the one case where a comment deleted during a long first read can
  be skipped. Comment bodies are untrusted data. A plain issue read is
  unchanged.
- A comment write is confirmed from the rows Linear already returned, even when
  the next page cannot be requested. The confirming read stays at 50 per page.
  A response over the 1 MiB cap no longer breaks later calls on that
  connection, and is never re-sent.
- Hosted status, session-list, history, whoami and task-board reads no longer
  wait behind writes in the workspace. Inbox and mark-read still do, because
  they record delivery. For those reads a session's `last_seen` is updated only
  when the workspace is free. While writes keep it busy, `whoami` and
  `sessions` can show an older `last_seen`. The next read or write that finds
  the workspace free records it.
- A project's Linear binding that cannot be read is refused instead of
  guessed. The message says what to do:
  - after a downgrade, upgrade instead;
  - for a corrupt binding, the owner clears it with
    `clawbridge linear unlink --expected-revision N --confirm --repair`, then
    links the project again.

  `doctor` reports it when a Linear profile is selected.
- Sign-in always prints its URL (Google and Linear), so you can open it in
  another browser profile on the same computer. Setup messages are plainer.
  The onboarding guide lists what a colleague needs before joining.
- A Linear command that times out waiting for another process's token refresh
  now names that process and how long it has held the lock, instead of
  reporting a bare deadline.

Assets retain five CGO-disabled targets, installers, the MIT licence, selected
checksums and a full source marker. Binaries are unsigned; macOS binaries are not
notarized.

See docs/operations/deployment.md, docs/linear.md and docs/onboarding.md.

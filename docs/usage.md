# Use ClawBridge

## Local messaging

Start a local workspace and create its first project. Both are saved as your defaults,
so a new terminal needs no flags or environment variables:

```sh
clawbridge setup --local --name demo
clawbridge project create clawbridge
clawbridge --session engineer --instance engineer-run-1 register --agent-kind codex --role engineer
clawbridge --session reviewer --instance reviewer-run-1 register --agent-kind claude-code --role reviewer
clawbridge --session engineer --instance engineer-run-1 send reviewer 'Ready for review' --operation-id review-request-1
clawbridge --session reviewer --instance reviewer-run-1 inbox
```

Use the returned message ID to continue the thread and mark it read:

```sh
clawbridge --session reviewer --instance reviewer-run-1 reply MESSAGE_ID 'Review complete' --operation-id review-reply-1
clawbridge --session reviewer --instance reviewer-run-1 mark-read MESSAGE_ID
clawbridge --session engineer --instance engineer-run-1 status MESSAGE_ID
```

Bare `clawbridge` then opens the TUI for this local workspace, and `clawbridge doctor` passes the project
check. The first project you create (without `--git-remote`) becomes the default; `setup --local --project NAME`
names it up front. A
`.clawbridge.toml` selects a project only at the root of a git checkout. Anywhere else it's ignored, with a
warning. Connect an agent with
`clawbridge mcp install --agent claude-code|codex --session NAME`: the local workspace and project come from
your defaults.

For an isolated local bus, for example in tests, set `CLAWBRIDGE_DB`, `CLAWBRIDGE_WORKSPACE` and
`CLAWBRIDGE_PROJECT` instead.

Results identify `local-authority`. Sending commits a message; inbox fetch stamps
delivery; mark-read stamps reading. Reuse an operation ID only for an identical
send/reply retry. Local identity is OS-user attribution; separate programs under
that OS user are not authenticated from each other.

The accepted [coordination source increment](design/coordination.md)
adds optional task context on send/reply:

```sh
clawbridge --session engineer --instance INSTANCE send reviewer 'Please review the checkout contract' --operation-id contract-review-1 --coordination '{"issue_id":"11111111-2222-3333-4444-555555555555","assignment_revision":"coordinator-7","intent":"request"}'
```

An accept/decline/clarify response references the original request_id; a correlated
reply uses that same ID as its positional message ID. Use the revision confirmed
by the coordinator. Different revisions require reconciliation; message metadata
does not reassign a task or approve work. See the
[task-board workflow](../skills/clawbridge/references/task-board.md) for dependencies
and ownership.

## Join a hosted workspace

Run `clawbridge` in an interactive terminal. On first launch, provide the existing
service's HTTPS address, workspace and project, choose browser or device sign-in,
and confirm. Later launches open the saved profile's TUI. Your Google account
must already have membership; joining does not grant it or create infrastructure.

For explicit commands:

```sh
clawbridge setup join --server https://SERVICE --workspace TEAM --project CODEBASE
clawbridge whoami
```

Browser OAuth uses a loopback callback and PKCE. Add `--device` for headless/SSH
approval from another device. No gcloud installation is needed for either path.
A complete existing OAuth route reuses its keychain credential; `--login` requests
a fresh browser sign-in. A failed join preserves the previous trusted route.
Joining does not register an agent session.

Use `clawbridge setup wizard` to open guided join again. An invalid saved profile
is reported for repair instead of overwritten. Bare noninteractive invocation
prints help; automation uses explicit commands.

`clawbridge login`, `login --device`, `logout` and `doctor` operate on the selected
trusted route. Logout removes that route's OAuth keychain entry. It does not
revoke unrelated Google credentials or change MCP configuration. Linux/WSL
requires the [existing D-Bus/keyring session](installation.md#linux-and-wsl-credentials).

## Join with an invite

The [sign-in guide](sign-in.md) covers invites, device keys, members, ownership,
turning device sign-in on, and troubleshooting in full.

A server that takes device keys (`serve --auth device` or `google,device`) needs
no Google account. An owner invites you and gives you an invite link
(`https://SERVICE/join#cbj1.…`), and you join:

```sh
clawbridge join
Paste the invite link (not shown as you type):
Join workspace TEAM on SERVICE as ada@example.com? [y/N]: y
clawbridge --project CODEBASE whoami      # identity_provenance: device-key
```

`join` asks for the link at a terminal without echoing it, or reads it from stdin
when piped (with `--yes`). It never takes the link on the command line, which is
kept in shell history and shown in other users' process lists. With the code and
the server's URL instead, `clawbridge --server https://SERVICE join --email
EMAIL` asks for the code. In a terminal, bare `clawbridge` (or
`clawbridge setup wizard`) does the same with prompts: choose "I have an invite
code" (offered when the server takes device keys), then give the email and a name
for this device. The code is asked last, unechoed, after you confirm.

- **The key:** `join` makes this device's key, keeps it in the OS key store, and
  writes the profile to sign every request with it. A refused join leaves no key
  and no profile. When the answer is lost, or names another device, the key is
  kept and the error says where; if the invite then shows used, ask an owner to
  revoke that device. If the profile can't be written, the error lists its
  values to set by hand.
- **No OS key store** (a headless Linux machine, a container): add
  `--key-store file`. The key is then a plain 0600 file under
  `~/.clawbridge/device/`, and anyone who can read it as you can act as you.
- **One workspace per device:** a device belongs to the workspace that admitted
  it. To join a second workspace, use another profile (`--client-config`).
- **Already signed in with Google?** `clawbridge device enroll` adds a key to
  this machine, with no invite. It first checks that the server takes device
  keys at the profile's server URL (its public origin), and makes one request
  signed with the new key before the profile switches to it. The profile then
  signs with the key; the Google credential stays until `logout`.

Accounts on a hosted workspace (each command takes `--help`):

- **`invite`:** `invite --email EMAIL --grant PROJECT=ROLE`; `invite list`;
  `invite revoke ID`. The main owner may also issue
  `invite --email EMAIL --recovery`. The code is shown once, on your terminal
  only; `--out FILE` writes it to a new file (0600) instead.
- **`device`:** `device list`, `device revoke ID`, `device label ID LABEL`,
  `device events` and `device ack` (new-device alerts, docs/sign-in.md).
  - `device add` is an invite link for another device of yours, good for 15
    minutes (`--expires` up to `24h`).
  - `device add --ci NAME --grant PROJECT=member --expires 30d` makes a CI
    device. It prints `CLAWBRIDGE_DEVICE_KEY` once, for the CI secret store, on
    your terminal only; `--out FILE` writes it to a new file (0600) instead.
- **`user`:** `user list`; `user revoke EMAIL --confirm`.
- **`owner`:** `owner show`; `owner transfer --to EMAIL`; then the recipient
  runs `owner accept` or `owner decline`, or the owner runs `owner cancel`.

The TUI's Account screen (`8`, or `clawbridge tui --screen account`) does the
same on a hosted workspace:

- your devices, this one marked: `a` adds a device (a one-time code, after you
  type `add`), `l` labels, `x` revokes;
- `i` issues an invite (`EMAIL PROJECT=ROLE ...`), and `x` revokes a pending one;
- for the main owner, every member with their devices: `x` on a member removes
  them, and `t` offers ownership. The recipient sees a banner and answers with `y`
  or `n`; the owner cancels with `c`.

A code shows once, in a box that holds the screen until Enter or Esc, and is
never drawn again. Every revoke, removal and ownership step waits for a typed
word: `revoke`, the member's email, `transfer`, `accept`, `decline` or `cancel`.

`doctor` reports whether the key is readable, a key file's mode, and any clock
correction. A clock more than 5 minutes off the server's is corrected once by the
server's time, and the correction lapses after 24 hours. One more than an hour off
is refused: fix the system clock.

## Profiles and project scope

Use `--client-config /absolute/path/config.toml` to select a separate trusted
profile. The default is `~/.clawbridge/config.toml`, or `CLAWBRIDGE_HOME/config.toml`.
`CLAWBRIDGE_CONFIG` also selects a file. Global routing and scope flags can be
supplied before or after a command, before the positional `--` boundary.

A repository-root `.clawbridge.toml` can provide scope:

```toml
workspace = "demo"
project = "clawbridge"
```

Repository configuration may provide only db/workspace/project/session/instance.
Authority mode, server, audience, credentials, principal and cache come from
trusted routing. A repository cannot switch your local client to a cloud server.

Without a project selector, a registered normalized Git remote can resolve it.
Missing or ambiguous projects fail; they are not silently created. Sessions are
unique within a project. Prefer immutable IDs when naming a specific recipient.

A project can coordinate a product spanning several repositories. Explicitly
select that same project in each checkout; its single registered remote remains
a discovery association. An agent can retain one session across frontend/backend
repos. Qualify task ownership by repository and path, as described in the
[task-board workflow](../skills/clawbridge/references/task-board.md). Separate
project permissions require separate bindings; related issues do not join them.
A running MCP process stays pinned to its resolved project when an agent changes
directories. Start a separate explicit binding to use another project.

## MCP

After joining, install a distinct MCP session for each agent:

```sh
clawbridge mcp install --agent codex --session codex-engineer --dry-run
clawbridge mcp install --agent codex --session codex-engineer
clawbridge doctor --mcp-agent codex --mcp-config /absolute/path/to/codex/config.toml --mcp-session codex-engineer
```

Use `--agent claude-code` for Claude Code. `--config` selects the agent file to
edit; `--client-config` selects the trusted ClawBridge profile. Without an explicit
agent file, Codex uses `$CODEX_HOME/config.toml` or `~/.codex/config.toml`; Claude
Code uses `.mcp.json` in the current directory. Conflicting entries fail; unrelated
entries survive an atomic, backed-up update. Distinct `--name` values allow
multiple scoped entries in one file.

Installation writes configuration and does not start an agent. Cloud entries pin
the trusted client-config path and safe routing metadata. The MCP process needs
that file and the same user's native keychain; tokens are not put in its argv.

For a direct local MCP process:

```sh
clawbridge mcp --db "$CLAWBRIDGE_DB" --workspace demo --project clawbridge --session reviewer-mcp
```

The agent calls `register_session` before actor operations; the process retains
the returned instance. To resume an existing active session, explicitly start
with its recorded `--instance`, then call registration with `resume: true`.
Record the new instance: resumption fences the previous one. Retired names stay
retired. Retiring a session that holds a confirmed assignment asks first: say
`--outcome done` if its work is finished (the issue moves on as a done release
does), or `--confirm` to release it as it is (see docs/linear.md, Done). Stale presence after 15 minutes does not establish that a process died.
On a hosted server, `whoami`, `list_sessions` and `history` that name the session
record its `last_seen` after the read, and only when no write holds the
workspace at that moment. Otherwise the beat is skipped, and `last_seen` can lag
by one poll until the session's next read or write.

The MCP server and `clawbridge notify` also keep a **liveness lease**: every
30 s they send `heartbeat` with the session's instance, which keeps the session
alive for 90 s. A heartbeat is not activity: it never moves `last_seen`, and the
server writes it at most once every 20 s. Only the instance that holds the
session can renew it; a retired or moved session holds no lease. Session rows
then carry `liveness`:

| `liveness` | Meaning |
|---|---|
| `working` | The lease is live and the client says working, or the session made a bus call in the last 15 minutes |
| `idle` | The lease is live and the session has been quiet for 15 minutes |
| `asleep` | The lease expired (laptop asleep, or the process stopped), within `stale_after_hours` of the last call |
| `gone` | Retired or moved, or the lease expired and nothing for `stale_after_hours` (24 h by default) |

A client that never sends `heartbeat` (an older release) keeps the old rule:
`working` while `presence` is `active`, `asleep` once it is `stale`, never
`gone` on time alone. `presence` and `stale` keep their two values. Liveness is
per session, and a session belongs to one workspace's project.

Provider-neutral coordination instructions are in the [ClawBridge skill](../skills/clawbridge/SKILL.md).
For Linear-backed tasks, use its [task-board workflow](../skills/clawbridge/references/task-board.md).
To enable native task-board tools, connect a personal profile as described in the
[Linear guide](linear.md), then pass its opaque ID with `--linear-profile ID`
when starting the MCP process or running `mcp install`. This pins the connection
without putting a credential in the agent configuration.
Automatic inbox hints require the separately configured [notification feature](notifications.md).

## Your first task: claim and release

An **assignment** says which session works on which issue: proposed by the
session, confirmed by the project's **coordinator session**, released when the
work ends. It's a record for the team, not a lock: it doesn't assign the Linear
issue to anyone and grants no rights. The same steps work in local mode and on a
hosted workspace; agents connected through MCP do their half with the tools of
the same names (`assignment_propose`, `assignment_confirm`, `assignment_release`).

**1. Two sessions and a coordinator.** Register a session for yourself (or the
agent that coordinates) and one for the agent that does the work, then make the
first the project's coordinator. On a hosted workspace the project owner does
this, or a lead for their own sessions. An agent installed with `mcp install`
registers its own session when it connects (see [MCP](#mcp)).

```sh
clawbridge --session lead --instance lead-1 register --role coordinator
clawbridge --session eng --instance eng-1 register --role engineer
clawbridge --session lead --instance lead-1 project coordinator-set lead
```

**2. The worker proposes.** It names the issue and, optionally, the paths it
owns:

```sh
clawbridge --session eng --instance eng-1 assignment propose \
  --issue-uuid 0d0d0d0d-0d0d-4d0d-8d0d-0d0d0d0d0d0d --issue-identifier SHOP-1 \
  --task-role engineer --scope-path 'src/**'
```

- **Without a board:** any UUID and label you choose will do.
- **With a Linear board:** use the issue's own UUID and identifier, from `clawbridge
  linear issues --profile PROFILE_ID` (see [Linear](linear.md)).

The answer carries the assignment's `revision`, and any `conflicts` with other
sessions' assignments. The worker sends that revision to the coordinator, for
example with `send lead '…' --operation-id …`.

**3. The coordinator confirms** that revision:

```sh
clawbridge --session lead --instance lead-1 assignment confirm eng --revision REV
clawbridge sessions --assignment-state confirmed      # eng, confirmed, SHOP-1
```

- **With a Linear board,** a confirmed claim gets a claim note and label on the
  issue. Under a status policy, the issue also moves to its `claim` state (see
  [claim markers](linear.md#claim-markers)).
- **On a hosted server** the server applies them. **In local mode** they're applied
  by `clawbridge linear watch`, or straight away when you pass `--linear-profile`.

**4. Release when the work ends.** Read the current revision from `whoami`, then
release with `--outcome done` if the work is finished:

```sh
clawbridge --session eng --instance eng-1 whoami          # session.assignment.revision
clawbridge --session eng --instance eng-1 assignment release --revision REV --outcome done
```

- **Without `--outcome done`,** the release only records that the session
  stopped, and the issue stays where it is.
- **With it,** a status policy may move the issue on, and who closes it follows
  the project's close rule (see
  [the status policy](linear.md#status-policy-project-owner-only)).
- **The coordinator** can release another session's work the same way, naming it:
  `assignment release eng --revision REV`.
- **Retiring a session releases its work too.** From v0.3.5, retiring one that
  holds a confirmed assignment asks first: `retire --outcome done` if the work is
  finished, or `retire --confirm` to release it as it is.

The [task-board workflow](../skills/clawbridge/references/task-board.md) is what
agents follow: scopes, dependencies and handing work over.

## Terminal interface

```sh
clawbridge tui
clawbridge tui --screen doctor
```

Run in an interactive terminal. Use 1–9 or Tab to switch screens, `?` for help,
`r` to reload, and `q` or Ctrl-C to quit. Arrows/j/k select rows. PageUp/PageDown
scroll details, including complete selected immutable IDs. Below 24 columns or
9 rows, resize the terminal; quit and Escape remain available.

Inbox `n` composes, `p` replies, `m`/Enter marks read, and Escape cancels drafts.
These actor operations need an explicit current session/instance. Opening the UI
does not register/resume an actor. Cloud is informational; provisioning and new
project administration use explicit operator workflows.

On Sessions and Inbox, `v` cycles Current/Archive/All. Retired/moved sessions enter
Archive 24 hours after their last update; deliveries enter it 24 hours after that
recipient read them. Active sessions, unread mail and invalid/future timestamps
stay Current. This derived view does not delete or change retention.

Filters apply to the loaded page. `]` fetches the next bounded page, `[` revisits
a prior one, and `r` resets paging. Empty filtered pages can still have later
pages. `mirror*` labels incomplete read-only coverage. Browsing does not mark
mail read; explicit actions retain their normal authority checks.

## Offline reads

`clawbridge sync` explicitly saves a project-scoped read-only cloud snapshot.
During transport/service outages, supported reads may return `offline-mirror`
with its freshness. Authentication/permission failures remain errors. Writes,
registration/resume and notification status stay unavailable; they are never
queued for later replay or redirected to writable local storage.

See the [API reference](reference/api.md) for operations and pagination, and the
[deployment guide](operations/deployment.md) for hosted administration.

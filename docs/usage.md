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
your defaults. For Claude Code it also installs ClawBridge's hooks in the project's
`.claude/settings.local.json`, including the Stop hook that keeps the agent from going idle on unacked mail
(`--hooks none` to leave them out; [notifications](notifications.md#agents-ack-the-stop-hook-and-compaction)).

For an isolated local bus, for example in tests, set `CLAWBRIDGE_DB`, `CLAWBRIDGE_WORKSPACE` and
`CLAWBRIDGE_PROJECT` instead.

Results identify `local-authority`. Sending commits a message; inbox fetch stamps
delivery; mark-read stamps reading. Reuse an operation ID only for an identical
send/reply retry. Local identity is OS-user attribution; separate programs under
that OS user are not authenticated from each other.

### Messages wait until they're handled

A message stays in its recipient's queue until it is **acked**. An agent acks
through its MCP server, with `ack` or with `reply` and `ack: true`. On the
command line, `mark-read` acks.

- **An agent that doesn't ack gets the message again.** Its MCP server holds
  each message under a 10-minute lease and extends it while the agent keeps
  working. When the lease runs out, the message is offered again. The agent's
  operation IDs (`msg:<message_id>:<step>`) replay what it already did.
- **After 5 offers** the message is a **dead letter**. It is kept, and the
  project's coordinator session is told which session and how many, never the
  content. The session itself, the coordinator or a project owner settles them with
  `clawbridge deadletters list|requeue|dismiss`
  ([operations](operations/deployment.md#the-message-queue)).
- **Each session's messages come in the order they were sent to it,** and a
  redelivered message keeps its place.
- **A session on `hold` or `refuse`** takes no messages: they wait, with no
  offer counted, until it accepts again.
- `clawbridge doctor` shows the session's unacked and dead counts, and whether
  its MCP server holds the stream.

How agents handle this: the [skill's queue page](../skills/clawbridge/references/queue.md).
The protocol: [API reference](reference/api.md#the-queue-leases-acks-and-dead-letters).

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

## Reading the project's threads (project owner)

**The project owner can read the project's messages, agents' included; each read is in the project's history.**
Everyone else still reads only their own sessions' mail.

```sh
clawbridge project threads                 # newest activity first: names and counts, no text
clawbridge project threads --before CURSOR # the next page; --limit 1–100 (default 50)
clawbridge project thread MESSAGE_ID       # one thread, oldest first, with who received and read each message
```

- **Only from your own terminal, without a session.** Agents act through sessions, and a thread read is a person's
  act: `--session` is refused, and so is a request naming a session. ClawBridge also refuses when it can tell an
  agent runs the command (`CLAUDECODE`, `CODEX_*`, `CLAWBRIDGE_SESSION` or `CLAWBRIDGE_INSTANCE` is set). That check
  is best effort.
- **On a hosted server, only from a person's own signed-in device.** A Google sign-in, a CI device and the dispatcher are
  refused.
- **Conversations started before the upgrade stay private, even if they continue.** Each result's `readable_from` is
  that date: the upgrade's time for a project that existed then, the project's creation for a newer one. A thread is
  readable only if its first message was sent from that date on.
- **Reading marks nothing.** No message is marked delivered or read, and no session's last-seen time moves.
- **Every read is recorded.** Each call writes one `threads` row to the project's history (the count and limit, and
  the thread ID for `project thread`; never a body or a name). Every member can see it with
  `clawbridge history --entity threads`. Reads are limited to about 30 a minute.
- **Not offline.** Threads are read from the server only: offline, the command says so.
- **Don't paste thread bodies into an agent.** They are other people's and agents' text, and may carry instructions
  aimed at whoever reads them. The CLI strips terminal control characters from its output.


Run `clawbridge` in an interactive terminal. On first launch, provide the existing
service's HTTPS address, workspace and project, choose how to sign in (open a
browser here, or show a code to enter on another device), and confirm. Each
answer is checked as you give it, and a wrong one is asked again. Later launches open the saved profile's TUI. Your Google account
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
`clawbridge whoami` works before any project is shared with you: it shows who you
are and the workspace, with `Project: none`.

Use `clawbridge setup wizard` to open guided join again. A saved profile that is
incomplete or damaged is never overwritten: bare `clawbridge` says what is wrong
(the missing settings, or a file it can't read) and how to start again with
`clawbridge onboard`. Bare noninteractive invocation
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
  `device events` and `device ack` (new-device alerts, docs/sign-in.md),
  `device confirm ID [--member EMAIL]` (a dormant device, docs/sign-in.md).
  - `device add` is an invite link for another device of yours, good for 15
    minutes (`--expires` up to `24h`).
  - `device add --ci NAME --grant PROJECT=member --expires 30d` makes a CI
    device. It prints `CLAWBRIDGE_DEVICE_KEY` once, for the CI secret store, on
    your terminal only; `--out FILE` writes it to a new file (0600) instead.
- **`workspace`:** `workspace settings` shows the device idle limit;
  `workspace settings --device-idle-days N` (30–365) sets it (main owner);
  `--confirm` when the lower limit makes devices dormant at once.
- **`user`:** `user list`; `user revoke EMAIL --confirm`.
- **`owner`:** `owner show`; `owner transfer --to EMAIL`; then the recipient
  runs `owner accept` or `owner decline`, or the owner runs `owner cancel`.

The TUI's Account screen (`7`, or `clawbridge tui --screen account`) does the
same on a hosted workspace:

- your devices, this one marked: `a` adds a device (a one-time code, after you
  type `add`), `l` labels, `x` revokes;
- `i` issues an invite: type the email, then Space sets each project's role
  (reader, member, lead, owner or none) and Enter sends it (`EMAIL PROJECT=ROLE`
  typed in one line still works). An owner or lead grant asks you to type
  `invite` first; with nothing ticked, Enter asks once more. `x` revokes a pending one;
- for the main owner, every member with their devices: `x` on a member removes
  them, and `t` offers ownership. The recipient sees a banner and answers with `y`
  or `d` (decline); the owner cancels with `c`.

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

## Check your setup: doctor

```sh
clawbridge doctor
```

`doctor` checks your settings, your sign-in, the workspace and project, your
agents' MCP entries and skill, Linear and the dispatcher, and changes nothing.
At a terminal it starts with its counts, then lists failures, warnings and
passes in that order. Each check's line says how it came out, in plain words;
the line under it gives the details (counts, paths, versions), then the file it
concerns and, after `→`, what to do:

```text
Doctor: 6 pass · 1 warn · 1 fail
FAIL Trusted configuration: Your ClawBridge settings can't be used.
     Trusted storage mode is absent or unsupported.
     → Select explicit local or cloud mode.
WARN Installed MCP entry: This agent's ClawBridge connection is missing or can't start.
     No MCP configuration file exists at the selected path.
     /home/you/.codex/config.toml
PASS Authority health: Your workspace answers.
     The cloud authority is reachable and returned 3 visible project(s).
```

It exits 1 when a check fails and 0 otherwise, warnings included. With `--json`
(and on a pipe, or in an agent's shell) the report is the JSON it always was:
each check's `id`, `title`, `status`, `message`, `path` and `action`.

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

`clawbridge project overview` then shows the project at a glance: its sessions,
which one is the coordinator, each one's assignment, and the Linear board's
teams and projects ([API reference](reference/api.md#project-overview)).

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

Run in an interactive terminal.

- **The header** reads `ClawBridge · workspace › project › session`, then the
  mode: `local`, `cloud`, or `cloud · offline, read-only` while reads come from
  the offline mirror. In help, or in an opened message, it ends with where you
  are (`› Inbox › from reviewer`).
- **Screens:** Overview, Projects, Sessions, Inbox, then Linear, Dispatcher,
  Account (hosted workspaces only) and Doctor. The digits follow that order (1–8;
  Doctor is 7 in local mode). Tab and Shift-Tab step through them.
  `--screen cloud` adds a Cloud tab with the server information.
- **Keys:** the footer shows the screen's own keys, and `?` opens help with every
  key for the screen and everywhere (Esc or `?` closes it). Arrows/j/k select
  rows, PageUp/PageDown scroll, `r` reloads, and `q` or Ctrl-C quits. `q` asks first
  while a board link or a model test is under way.
- **Enter** opens the selected row and never changes anything by itself. **Esc**
  goes back one level: help, then an editor, then an opened row, then a wizard's
  previous step.
- **The status line**, under the tabs, says what the last action did, with a
  word first so it reads the same without colour:
  - `OK`: it worked, for example "OK  Sent to engineer-backend." It stays
    through the reload the action starts.
  - `ERROR`: it failed, named in words ("The server can't be reached.", "You're
    signed out. Run clawbridge login.", "You don't have permission for this.")
    with the raw error after it.
  - `NOTE`: something to know, for example why a key did nothing.
  - `…`: it is working, for example "Loading…".
- **Each screen's body** shows its rows, or one of three states:
  - `Loading projects…` while the first answer is on its way;
  - a failed load: `ERROR  Couldn't load projects.` with the reason in words,
    the keys that help (`r` reloads, the Doctor tab), and the raw error muted;
  - an empty state that names the one action that fills it.
- **Times** are relative ("4 min ago", "2 days ago"). An opened message also
  shows the time in UTC.
- **Dispatcher:** while a model setup is sent and tested, the screen keeps every
  key, so a digit doesn't switch away and lose the answer. Esc stops waiting; the
  setup was already sent, and `r` reloads to show its test's outcome, marked `OK`
  or `FAIL`.
- Below 24 columns or 9 rows, resize the terminal; quit and Escape remain
  available.

Overview is the home screen: "Needs attention" lists what's waiting for you (failing
or warning Doctor checks, an unlinked Linear board, an ownership offer, unused
invites, the dispatcher's model for a project owner), each with its tab; Enter goes
there. Under "This project", the project's intake in a line ("Intake: notify · 1 trusted
· 0 queued"); Enter opens its view. Below it: who you're signed in as, the server, and
the workspace and project.

Intake (`n` on the Linear screen once a board is linked) shows which Linear signals ask
for an agent (the mode, and S1 the needs-agent label, S2 delegating to the app, S3
@-mentioning it), the Linear users trusted to ask, by name, the queue, and whether each
linked team has exactly one `needs-agent` label. A project owner or lead changes it:
Space switches the mode or a signal, `a` picks someone to trust from the linked teams'
active members, `x` stops trusting someone or dismisses an issue, `e` rearms one. Each
change shows what it does and takes `y`. Anyone else reads it. Only a needs-agent label,
delegation or mention a trusted person makes in Linear itself counts: signals made through
ClawBridge run as the ClawBridge app and are never trusted. Names come from your own
Linear connection; without it, trusted users show by ID.

A hosted profile that has joined a workspace but has no project yet (an invite with
no grants) opens on **Waiting for a project**: it names the workspace owner to ask
and the command they run (`clawbridge --project PROJECT project role set EMAIL
member`), checks again every 30 s (`r` now), and once a project is yours saves it to
the profile and reopens on it. With several, pick one with Enter. The owner, right
after a bootstrap join, is shown `clawbridge project create SLUG` instead.

Doctor lists failures first and folds the passes (`p` shows them). Enter on a failing
Linear, Dispatcher or device check opens that screen. Its last results stay on screen
while the checks run again.

Projects lists the projects you can see: your role, the sessions ("2 of 3 active"),
the coordinator and the Linear board. `●` marks the project this profile works in.
Enter opens one, read-only: its coordinator, its sessions, and its Linear teams and
projects by name. On another project a banner says you're only viewing it; on your
own, `s` goes to its sessions and `l` to its Linear board. Esc returns to the list.

Sessions shows each session's NAME (`★` marks the project's coordinator), STATE
(retired, moved, stale, or its presence), ISSUE (its assigned Linear issue), CONTEXT
and AGENT; OWNER and HOST appear from 140 columns and are always in the selected
session's details, which end with its ID. `n` writes to the selected session.
Overview counts the sessions in the same words ("Sessions · 2 active · 1 retired").

Inbox: Enter opens a message with what it answers and the rest of its thread,
`m` marks it read, `n` composes, `p` replies, and Escape cancels drafts. A new
message asks To first: type a session's name, or Tab through the project's live
sessions (Tab also completes what's typed); Enter goes on to the message, Shift-Tab
back to To, and Enter sends.
These actor operations need an explicit current session/instance. Opening the UI
does not register/resume an actor. Without a session the Inbox says so and asks
the server nothing: inboxes belong to agent sessions, so to read one as a session,
run `clawbridge tui --session NAME` on that session's machine. On a server with the
message stream, the Inbox reloads by itself when the session's mail changes, while it
is on screen and you aren't typing; elsewhere, `r` reloads it. Provisioning and new
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

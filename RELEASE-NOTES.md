# ClawBridge v0.3.6 (unreleased)

**Schema 28 → 39.**
- The server migrates to schema 39 on its first start: 29, the new-device alerts' events and acknowledgements; 30, a
  project's own watch budget; 31, the device an invite was made from; 32, the workspace's device idle limit; 33,
  the Linear team and project names stored with a project's task board binding; 34, the date from which a project's
  threads are readable; 35, the message queue's leases, acks and per-recipient numbering; 36, the message stream's
  tickets; 37, the Linear issue views ClawBridge wrote; 38, project views among them; 39, the project kind setting. The upgrade numbers existing
  deliveries, about 2–4 s per million, and counts what was read as handled.
- **One-way:** v0.3.5 and older refuse a database newer than schema 28. Take a dump before upgrading; to go back,
  restore it with the v0.3.5 image (docs/operations/deployment.md).

## The message queue: the dispatcher, chaos tests, operations (slice 5 of 5)

- **The dispatcher model's `cases/next` is woken when a case opens** (a `cb_case` notification), instead of checking
  every second; it still checks every 5 s. Without the notification connection it checks every second as before.
- **`clawbridge deadletters list|requeue|dismiss`** reads and settles dead letters from the CLI (the session, the
  project's coordinator or a project owner; no bodies; requeue and dismiss are recorded in history).
- **The chaos suite** (`-tags chaos`, real Postgres): API crashes with in-flight transactions aborted, a killed LISTEN
  connection, a stalled network, a consumer killed mid-handle and a compaction cycle run in CI; the 10k-message soak
  with random consumer kills runs nightly and on demand, replayable by its printed seed. docs/operations/deployment.md
  gains "The message queue". No schema change, no Linear configuration.

## The message queue: agents ack (slice 4 of 5)

- **An agent's MCP server takes the session's messages from the queue** from the agent's first `inbox`, `ack`,
  `reply` or `mark_read` call: it holds the consume stream, or polls `lease` every 2 s in local mode and on a server
  without the stream. `inbox` keeps its shape; a message the server holds shows its `lease`. The new **`ack`** tool
  acks handled messages (the held lease is filled in), and `reply` takes **`ack: true`**, acked in the reply's own
  transaction. While the agent called a tool in the last 10 minutes, its held leases are extended every 5 minutes.
- **`mcp install --agent claude-code` installs Claude Code hooks** in the project's `.claude/settings.local.json` by
  default (`--hooks local|user|project-shared|none`; the report goes to stderr). `context hooks install` adds the
  same three:
  - **Stop** (`clawbridge hook stop`) sends a turn back once per new message while messages wait unacked, with fixed
    text and a count only. It never blocks while Claude Code says `stop_hook_active`, nor on a `hold` or `refuse`
    policy, an older server, an error or no answer in 2 s.
  - **PreCompact** holds the session's notifications and gives back its leases without counting an offer;
    **SessionStart** after the compaction accepts again. Only a hold the compaction set is lifted; one left by an
    agent that died mid-compaction is lifted by the next MCP server start, or after 15 minutes.
  - The hooks act through a private record the MCP server keeps (`<state home>/context/<session>.queue.json`), only
    for their own agent's MCP server and only against the configured server.
- **`set_notification_policy` wakes the session's streams**, so `accept` re-offers at once.
- **`clawbridge doctor` has a `queue` check:** whether the session's MCP server holds the stream, its unacked and dead
  counts, and a `hold` policy (whose messages wait without attempts or dead letters).
- The clawbridge skill gains the protocol: handle, then `ack` (or `reply` with `ack`), with
  `operation_id = msg:<message_id>:<step>`. No Linear configuration is involved.

## The message queue: clients follow the stream (slice 3 of 5)

- **`clawbridge notify` is woken by the server** as soon as the session's mail changes, instead of polling every 5 s;
  it still checks once a minute in case a wake was missed. Against an older server, or in local mode, it polls as
  before and says `stream_unavailable` once on stderr.
- **`notify --adapter command -- PROGRAM [ARG]...` wakes an idle agent** by running a command you choose, directly and
  never through a shell. It is only ever given fixed text, `N ClawBridge messages are waiting; call inbox`, in place
  of each `{text}` argument and in `CLAWBRIDGE_NOTICE_TEXT`, never a sender, subject or body. docs/notifications.md
  has recipes for Orca (`orca terminal send`) and tmux (`send-keys`).
- **The TUI's Inbox stays live** on a server with the stream: it reloads when the session's mail changes, while it's
  on screen and nothing is being typed.
- The client reconnects with a fresh single-use ticket each time, backs off 1 s doubling to 60 s with jitter, and
  treats 90 s without a line as a dead connection. A refused ticket (a moved instance, a removed member) ends the
  stream; a full account (429) or an outage is retried.

## The message queue: pushed (slice 2 of 5)

- **The server pushes a session's messages** on `GET /v1/stream` (Server-Sent Events), in place of polling every
  5 s. A consume stream leases and pushes deliveries within a second of their commit, holds 20 unacked at most, and
  is taken over by the session's next consume stream. An observe stream sends only the counts when they change. The
  URL never holds a secret: a single-use `stream_ticket` goes in a header. Every read is authorized again, so a
  revoke, a member's removal or a retired session closes the stream at once. Streams live 30 minutes, an account holds at most 8, and they go through
  Caddy unchanged (docs/reference/api.md, "The stream").
- **The server sweeps** every 15 s: run-out leases are offered again and dead letters made, by one replica at a time.
- Clients use the stream from slice 3; until then nothing changes for them.

## The message queue: acks and dead letters (slice 1 of 5)

- **A message is done when its recipient acks it, not when it is fetched.** New operations: `lease` takes the
  session's next deliveries in order, `ack` marks them handled, `nack` gives one back, and `extend` renews them. A lease
  runs 10 minutes. One that runs out is offered again, and after 5 the delivery is a dead letter. It is kept, and the
  project's coordinator is told the count (never the content). `deadletters_list`, `deadletters_requeue` and
  `deadletters_dismiss` settle them, with history. `reply` and `send` take an `ack`, committed in one transaction, and
  a retried reply still acks. `mark_read` acks. `notification_status` with `queue: true` adds the unacked and dead
  counts.
- **Each recipient's deliveries are numbered 1, 2, 3…** in commit order, with no gaps. Every server path that sends a
  message goes through one writer, which takes the counters in a fixed order so crossing sends can't deadlock.
- **Nothing changes for today's clients:** `inbox` takes no lease and keeps its shape, and `notification_status`
  without `queue` keeps its seven fields. Push (the stream), the CLI and the MCP tools come in the next slices
  (docs/reference/api.md, "The queue").

## Project overview

- **One read for a project's summary (TUI-UX-AUDIT PR C, server half).** `clawbridge project overview` (and the MCP
  tool `project_overview`) shows the project, its coordinator, its sessions with each one's assignment, and its
  Linear board's teams, projects and who closes each. Any project role may read it, and it shows nothing that role
  can't already read. Its counts cover every session, so they match the Sessions tab however many there are; the rows
  stop at 200. Offline it says so instead of failing as a write.
- **`list_sessions` rows say which session is the coordinator** (`coordinator: true`). Older clients ignore it.
- **Linear names are stored at link time (owner's Q5).** A link stores the bound teams' and projects' names, so the
  overview shows names instead of UUIDs. They are display text only: no check reads them, and the audit records
  counts, not names. `clawbridge linear names-refresh` (project owner or lead) picks up a rename without moving the
  binding's revision.

## The project's threads

- **The project owner can read the project's messages, agents' included; each read is in the project's history (Q1).**
  `clawbridge project threads` lists them, newest activity first (names and counts, no text); `clawbridge project
  thread ID` reads one, with who received and read each message. Nothing is marked delivered or read.
  - Only from the owner's own terminal, without a session; on a hosted server, only from a person's signed-in device.
    The CLI refuses when it can tell an agent runs it.
  - Conversations started before the upgrade stay private, even if they continue (`readable_from`).
  - Every read writes one `threads` row to the project's history (no body or name), about 30 a minute at most.

## Your Linear API key (views, phase 3a)

- **`clawbridge linear key set|status|remove --profile ID` (live/LINEAR-VIEWS-PLAN.md §11.3, §13.1).** The project
  owner saves their personal Linear API key for writing Linear views, which Linear's MCP can't. It's read at a hidden
  prompt or from stdin, never argv, and checked before saving: it must read, and belong to the same Linear user as the
  profile's connection. It's kept in the OS key store like the device key (or a 0600 file with `--key-store file`,
  after a warning). On every OS any program running as the owner can read it, so the guards are ClawBridge's:
  - only `linear key` and `linear view` read it; every other run of the binary (`mcp`, `runner`, `serve`, `notify`,
    ...) marks itself unable to, first thing, and the MCP server, runner and server don't link the code;
  - it refuses under an agent's environment (best effort);
  - runner sessions are denied `clawbridge` and `security`;
  - it's sent only with an allow-list of GraphQL operations, never in argv (on macOS it reaches the keychain on the
    `security` tool's stdin), logs or errors.

  `doctor` reports `linear.api_key` from a marker without reading the key. `linear logout` removes it too. No view
  writes yet: those come in the next phase.

## Linear project status

- **Read and set a Linear project's status by hand (owner request).** `clawbridge linear project status
  LINEAR_PROJECT [--suggest]` and `... status set LINEAR_PROJECT --type T --operation-id ID`, and the MCP tools
  `taskboard_project_status_v2` and `taskboard_project_status_set_v2`, run as the hosted server's ClawBridge app. Any
  member or session reads it. Only the project's designated coordinator session, or the project owner or a lead
  without a session, sets it. Completed and canceled follow the project's close rule: where people close its issues,
  no session sets them. A set previews first; its confirm carries the status the preview saw and is refused if the
  status changed since. It is recorded before it is sent, read back by status ID, and never sent twice. With project
  flow on, it holds flow for that project until `project-flow resume`. Each read costs 6 calls of the project's hourly
  Linear budget, each confirmed set 8. No schema change. The set tool is one of the Linear write tools a runner
  launch denies unless it passes `--linear-writes`.

## One invite link

- **An invite is one link (A1, §17.1).** `invite` and `device add` print a link with the code:
  `https://SERVER/join#cbj1.WORKSPACE.EMAIL.CODE`. On the new machine, `clawbridge join` asks for it (unechoed) and
  confirms "Join workspace … on … as …?". Piped, it needs `--yes`. `--out FILE` writes the link.
  - The link is never taken on the command line (`clawbridge join LINK` is refused): a command line is kept in shell
    history and process lists.
  - The code is after the `#`, which a browser never sends; the server's new `/join` page only says to run
    `clawbridge join`. Only `https` links are accepted (`http` on a loopback address, for development).
  - The guided setup's "I have an invite code" step and the `--email` path's code prompt take the link too.
  - The TUI's one-time box shows the link below the code.
- **`device add` is quick and short-lived (§17.2).** Its link lasts 15 minutes by default (`--expires` takes `30m`,
  `2h`, up to `24h`), and works only while the device that made it is still active. That device isn't alerted when
  the link is used; your other devices and the owner's are.

## New-device alerts

- **Your other devices hear of a new one (A1, §17.3).** When a device is added to your account (an invite, `device
  add`, recovery or `device enroll`) or one of yours is revoked, every other device of yours, and the workspace
  owner's, prints a line on stderr after its next command, and the TUI's Account screen shows a banner:
  `New device "Ada's desktop" (dev_9k2…) joined … via device add. Not you? clawbridge device revoke dev_9k2…`
  - It repeats until that device acknowledges it: `clawbridge device ack --all`, or `e` on the Account screen.
    `clawbridge device events` lists them. A command and the banner show the newest five, then "…and N more";
    events older than 90 days aren't shown.
  - A revoke is an alert too, including the owner's `user revoke`, so a stolen device can't quietly revoke your
    others.
  - Not alerted: the device itself, the device that revoked a device, and CI devices.
  - The main owner's coordinator sessions get each event as one `security` message per project, if the owner
    registered them; a coordinator someone else runs gets nothing (A1-D4). It names the user and the device ID, never
    the member-chosen label.
  - The server answers every device-signed request with `pending_device_events`; the client fetches the events at
    most once a minute. Older clients ignore the field.
- **Unused devices go dormant (A1, §17.4).** A human device unused for 90 days is refused until another device of
  its user, or the main owner, confirms it: `clawbridge device confirm dev_…` (`--member EMAIL` for another member's;
  `w` on the Account screen). Nothing it holds is deleted, and any use restarts the clock. CI devices are exempt.
  - Confirming is signed by a live human device of the user (or the main owner's); a Google sign-in or CI key can't.
  - The skill's recovery page says what an agent does on `device_dormant` (tells the human, who confirms it from
    another device) and with a dormancy notice (passes it on).
  - The main owner sets the limit: `clawbridge workspace settings --device-idle-days N` (30–365); a lower limit that
    makes devices dormant at once says how many and needs `--confirm`.
  - The user's other devices and the owner's are warned first, from day 80 (day 60 for the owner's own devices,
    which also reach the owner's coordinator session), and again when it goes dormant. `device list` shows
    `dormant_at`.
  - An owner whose devices are all dormant gets back in with the host operator's `operator bootstrap-invite`.
- **Device labels are printable text.** A label is shown on your other devices, so the server refuses control
  characters, terminal escapes and bidi or other format characters (1–64 printable characters), and the CLI and TUI
  escape any older label when they print it.

## Security (G9)

- **The session runner's agent gets less of yours (G9-5, G9-6, G9-9, G9-14).**
  - **Environment:** `runner launch` starts claude through a launch script that clears the environment and passes on
    only the terminal's basics, the proxies and claude's own settings, named one by one (its key or sign-in token,
    endpoint, models, config directory). `GITHUB_TOKEN`, `NPM_TOKEN`, `AWS_*`, `SSH_AUTH_SOCK`, `CLAWBRIDGE_*` and a
    parent Claude Code session's variables no longer reach the session, and its sandbox self-test now proves that too.
  - **The clone's origin** is copied without a user or password; a remote helper or unknown transport is refused.
  - **MCP tools:** the agent's task-board write tools (which act as your Linear profile) are denied unless the launch
    passes `--linear-writes`, and `assignment_confirm` always is.
  - **The clone's `.git`** is denied to the file tools and to Bash commands naming it.
- **`runner fetch` checks the clone before git touches it (G9-7, G9-8, G9-14).**
  - **Refused:** a `.git` that isn't a plain directory, borrowed objects (alternates, `commondir`), and a config
    setting `core.hooksPath`, `core.fsmonitor` or an include.
  - **Checked:** every fetched object (`transfer.fsckObjects`).
  - **Scanned:** a merge's own changes, for token shapes, like every other commit's.

## The terminal interface

- **One set of keys, shown where you look (TUI-UX-AUDIT PR A).** The footer shows the screen's own keys, and `?`
  opens help listing every key for the screen and everywhere; the old one-line notice is gone. Keys never mean two
  things: **Enter opens** the selected row and never changes anything by itself, **Esc goes back one level** (help, an
  editor, an opened row, a wizard step), and `q` asks first while a board link or a model test is under way. Left and
  Right no longer switch screens.
- **New tab order, from the mode.** Overview, Projects, Sessions, Inbox, then Linear (`5`), Dispatcher (`6`, was
  Model), Account (`7`, hosted workspaces only) and Doctor. Cloud shows only where a setup is wired. Under 100 columns
  the tab bar is compact instead of collapsing to one tab. `tui --screen dispatcher` opens it; `model` still works.
- **The header says where you are, once.** One line, `ClawBridge · workspace › project › session`, with the mode and
  an offline badge; the status line sits under the tabs. The `Scope:` and `Source` lines and each screen's subtitle
  are gone from the body (the subtitles are in help).
- **Changed keys:** Inbox Enter opens a message, with what it answers and its thread (`m` marks read, as before).
  Account's decline is `d` (was `n`). Linear connects with `+` (was `c`, which now only switches who closes in the
  link review); Esc on Linear goes back one step and keeps the teams and projects you chose, instead of starting over.

- **Every screen says what it's doing (TUI-UX-AUDIT PR B).** While a screen loads it says "Loading sessions…" and the
  layout stays put. A failed load says what failed in words ("The server can't be reached", "You're signed out"), the
  keys that help (`r` reload, the Doctor tab), then the raw error. An empty screen says why, and how to fill it.
- **One status line, with a word for its meaning.** Under the tabs, `OK`, `ERROR`, `NOTE` or `…`, the same words as the
  CLI, so it reads the same without colour. A send, reply or mark-read now says so ("OK  Sent to engineer-backend."),
  and Account and Linear name what changed ("laptop renamed to work-mac") instead of an ID.
- **Times are relative.** Inbox and Account show "4 min ago" or "in 2 days"; an opened message adds the time in UTC.
- **Inbox without a session explains itself.** Inboxes belong to agent sessions; a person's profile gets a note on how to
  read one (`clawbridge tui --session NAME`) instead of an error.
- **The Dispatcher screen keeps its place while it sends and tests.** A key no longer leaves the screen and loses the
  test's answer; Esc stops waiting. The field being typed in stays on screen at 80×24, and the outcome is marked `OK` or
  `FAIL`.

## The dispatcher

- **`clawbridge dispatcher model setup` guides an owner through the model's setup.** It asks for the provider (from
  the list the dispatcher agent can run), the model, the key at a hidden prompt (never an argument or env variable), the
  caps with a required daily token ceiling, and a confirmed disclosure, then waits for the test in your agent and says
  what came of it. `--key-stdin` with flags scripts it. `dispatcher model status` and `dispatcher model key delete
  --confirm` come with it. The server now accepts only providers from that same list.
- **The TUI has the same model setup, on a new Dispatcher screen (`6`).** It shows a project owner their key, their agent
  and the last test, and `s` walks through the same steps in the same order as the CLI. The key field is never drawn,
  and the key is wiped from memory once sent, on Esc, and when you leave the screen or quit.
- **Setup lists the provider's models, with their list prices.** In the CLI and the TUI, the model step shows up to ten
  of the provider's models from the agent's own catalogue, cheapest first, with their price per million tokens and
  context window; a number picks one, and any other model ID can still be typed. The list is generated from the pinned
  agent build, and CI fails if it is out of date.
- **`clawbridge-agentd` has its own image, for the egress proxy, and takes images pinned by ID.** The image is the static
  binary alone (`cmd/clawbridge-agentd/Dockerfile`, about 15 MiB, user 65534), and CI runs its egress proxy with the
  same hardening agentd gives it. `CLAWBRIDGE_AGENT_IMAGE` and `CLAWBRIDGE_EGRESS_IMAGE` now also accept an image ID
  (`sha256:…`), since an image loaded with `docker load` has no repo digest to pin.
- **Installing `clawbridge-agentd` is documented, with its units and a check.** `docs/operations/agentd.md` sets up its
  two users, the rootless Docker, the launcher token and the seal key without printing either, and the way back out.
  `deploy/systemd/agentd/` has the hardened units, and `deploy/agentd-check.sh` checks an install without changing it
  or printing a secret. The launcher token's `--out` steps in deployment.md failed, since the api runs as `nonroot`;
  they now go through a directory it owns.

## The comment watcher

- **Intake's `needs-agent` signal (S1) now sees every project of a binding.** It read the bound projects one at a time
  and stopped after four pages, so in a project set of more than four projects a label in a later project was never
  seen, and every poll repeated the same partial read. It now reads each bound team once, across all its projects,
  and queues only issues in the binding; one in a project the binding doesn't hold is skipped as `not_in_binding`,
  without reading it.
- **A project's owner can set its own watch budget.** `clawbridge project watch-budget set N` (1 to 600 Linear calls
  an hour) sets the comment watcher's budget for that project, ahead of the server's `CLAWBRIDGE_WATCH_BUDGET` and a
  client's `linear watch --budget`, from the watcher's next call. `project watch-budget clear` goes back to them, and
  `project watch-budget show` and `doctor` show the budget in force and where it comes from. Each change is recorded in
  the project's history. Schema 30.

## Project kinds

- **A project kind setting (phase 1 of the project-kind labels).** `clawbridge linear kind set --values
  Engineering,Review,Test [--group Type] [--team UUID] --expected-revision N` names a single-select project-label
  group on one bound team, never the workspace, and its values; `kind show` reads it (any member or session). Only
  the project owner sets it, in their own terminal. Schema 39.
- **Seed the kind's labels in Linear (phase 2).** `clawbridge linear kind seed --operation-id ID --profile P` previews,
  then `--expected-plan-sha256 SHA --confirm` makes the group and its values as project labels of the kind's team, or
  adopts the ones already there, with the owner's own Linear profile. A name a workspace project label has is refused
  before anything is sent (Linear refuses that clash), as is any other clash. Each label is recorded before it is
  sent and proven in the team's list; the IDs are then stored. Applying a kind to projects comes next.

## Linear views

- **ClawBridge lists a board's shared Linear views (views phase 2).** `clawbridge linear view list [--team UUID]
  [--type issue|project]` and `clawbridge linear view show ID` list the shared issue and project views of the
  binding's teams, and agents get the read-only MCP tool `taskboard_views_v2`. Private, workspace and other kinds of
  views aren't listed, only counted. A view's filter isn't shown: Linear's MCP doesn't return it. Agents can't create
  or change views; the new skill page (`skills/clawbridge/references/views.md`) has them draft a view set for the
  project owner to apply.
- **The project owner creates and updates shared issue views, and agents filter issues by them (views phase 3b).**
  `clawbridge linear view create --team UUID` and `linear view update ID` (`--name`, `--filter-file`,
  `--operation-id`, `--confirm`) write with the owner's own Linear API key, in their own terminal, without a session.
  - **Always one bound team, never the workspace:** `--team` is required, with no default. The preview's first line
    states the scope ("a shared team view: visible to everyone in that team (N members)"), and history records
    `scope: team:UUID` with the view ID and the filter's digest, never the filter or the name.
  - **Every ID in the filter**, under any key, must be the view's team or the binding's (projects, labels, states,
    members); a name another issue view on the team has is refused.
  - **The confirm holds the preview:** it carries the preview's binding revision, filter digest and, for an update,
    the stored view's `filter_as_of`; anything changed since is refused. An update first reads the view in Linear and
    never overwrites a change someone made there.
  - **Recorded before it's sent, read back after:** only a read-back with the same team, name, sharing and filter is
    accepted and stored for agents; anything else is uncertain and never sent again.
  - **Agents:** `taskboard_issues_v2` takes `view_id` for a view ClawBridge wrote. ClawBridge evaluates the filter on
    the rows (team, project, labels, state, assignee, priority, one `and`), and the answer adds `filter_as_of`,
    `agent_filter` and the rows left out for an unknown field. A view using anything else is
    `agent_filter: unavailable` for agents, while people still see it in Linear. Issue rows now carry `priority`
    (0–4) with `priority_known`.
  - **Delete (phase 3c):** `linear view delete ID [--confirm]`, the owner only, and only a view ClawBridge wrote in
    this project; a view Linear no longer has afterwards is dropped from what agents filter by (history
    `view_deleted`), one still there is `uncertain`. The confirm carries the preview's `filter_as_of`, and a view
    someone changed in Linear since ClawBridge wrote it is never deleted.
  - **View sets and adoption (phase 3d):** `linear view apply --file F [--adopt NAME]... [--confirm]` previews a file
    of up to 50 issue views as creates, updates, unchanged and refused, then writes each as its own operation. A view
    someone made in Linear is taken over only with `--adopt` and only if exactly one has the name; the preview shows
    its current filter, creator and last change, and history records `view_adopted` with the old filter's digest. The
    confirm carries the preview's `plan_sha256`, and a plan that changed since is refused.
  - **Project views (phase 3e):** `view create --kind project` and `"kind": "project"` in a view-set file make a shared
    team view of projects, filtered by project labels: each a live choice of a single-select group of that team or
    the workspace, proven by ID with the owner's key. Agents list project views but don't filter by them yet.
  - Schema 37 stores the views, and schema 38 lets a stored view be a project view.

## The command line

- **Results are readable at a terminal; scripts and agents keep the same JSON.** On a terminal, a result is now laid
  out for people: aligned fields, tables, one block per item when a table would be too wide, and data from outside
  escaped. On a pipe, in an agent's shell (Claude Code, Codex), or with `--json` or `CLAWBRIDGE_OUTPUT=json`, the output
  is exactly the JSON it was, which a test now pins. `--json=false` or `CLAWBRIDGE_OUTPUT=human` asks for the layout
  anywhere. Invite codes and links, CI device keys and intake tokens are printed once, alone on their line. The skill
  tells agents to pass `--json`. Commands get their own layouts in the next releases; until then they show every
  field.
- **The project owner manages intake without a session (E3a).** `clawbridge intake policy|trust|list|dismiss|rearm`
  no longer needs a registered session: the project role decides, as before (any member reads; a lead or owner
  changes; a dismiss without a session needs a lead or owner).
  - `intake trust add|remove` takes a Linear name or email as well as a user ID. It's looked up among the members
    of the project's linked teams through your own Linear connection (`--linear-profile`, or your only one), shown
    ("That is Ada Lovelace <ada@…> (…)"), and changed only with `--confirm`, which says it again. Only active
    members match; a name that matches several people lists them. Names are escaped; emails are never stored, never
    shown to an agent, and `linear members` doesn't show them.
  - The skill has a page on intake (`references/intake.md`): what a coordinator session may do with the queue, and
    that the policy and the trusted list are a person's.

- **`whoami`, `doctor`, `status ID` and the account lists read as answers (O2).** `whoami` says who you are, how that
  was proven, and your workspace, project and session. `doctor` leads with its counts, lists failures first, then
  warnings, then passes, with each action indented under its check. `status ID` shows when a message was sent and who has
  read it. `invite list`, `device list`, `user list` and `owner show` are tables, with a device going dormant marked
  "dormant in N days". Times are relative ("4 min ago", "in 2 days"), with UTC where the exact time matters, through one
  formatter that the TUI uses too. `--json` is unchanged.
- **`doctor` says how each check came out, in plain words (O3).** A check's line now reads as an answer ("Your
  ClawBridge settings can't be used.", "This agent's ClawBridge connection is missing or can't start.", "Your workspace answers."),
  with its details (counts, paths, versions) on the line under it and what to do after `→`. Checks whose message
  already read that way keep it. `--json` is unchanged, check IDs included.

# ClawBridge v0.3.5 (unreleased)

**Schema 19 → 28.**
- The server migrates to schema 28 on its first start:
  - 20, the session liveness lease;
  - 21 and 22, project flow and its early checks;
  - 23, 24, 25 and 28, the dispatcher's model setup, cases, chat and advice;
  - 26 and 27, context watching and per-session levels.
- **One-way:** v0.3.4 and older refuse a database newer than schema 19 ("Postgres schema is newer than this
  binary").
- **Rollback:** take a dump before upgrading. To go back, restore that schema 19 dump with the v0.3.4 image. Never run
  an older binary against the migrated database (docs/operations/deployment.md).
- Upgrade the server first, then clients. **Upgrade every client before setting `CLAWBRIDGE_AUTH=device`** (below).

## Signing in with invites: the client

- **The client side of device sign-in ships in this release.** The server side shipped in v0.3.4.
  - **Commands:** `clawbridge join` (the invite code is asked for without echoing it), `invite`, `device` (`add`,
    `list`, `label`, `revoke`, `enroll`), `user` and `owner`, and the device checks in `whoami` and `doctor`.
  - **The TUI:** an Account screen (`8`).
  - **The guided setup:** an "I have an invite code" path.
  - **Cloud check:** an `auth_mismatch` check in `clawbridge cloud check server`.
  - A v0.3.4 client can't join a server that takes device keys.
- **Upgrade the clients first, then switch** (docs/sign-in.md, "Turning device sign-in on"):
  1. keep `google`, or set `google,device`;
  2. upgrade every member's client;
  3. give every member a device (`device enroll`, or an invite);
  4. only then set `device`.

  Setting `device` early locks out old clients and Google profiles. Setting `google,device` again undoes it, and
  nothing stored is touched.

## Sessions

- **A liveness lease (schema 20).** `clawbridge mcp` and `clawbridge notify` send a `heartbeat` every 30 s with the
  session's instance, which keeps it alive for 90 s. Session rows carry `liveness`: `working`, `idle`, `asleep` or
  `gone` (docs/usage.md).
  - A heartbeat is not activity and never moves `last_seen`, and the server writes it at most once every 20 s.
  - Only the instance holding the session can renew it.
  - Against an older server, a failed heartbeat just waits for the next beat.

## The comment watcher

- **ClawBridge's own claim and close notes no longer speed up the comment watch.** An issue's comments were polled
  every 5 minutes for an hour after any ClawBridge comment on it, so every claim and release kept its issue on the short
  cadence. That was most of the watcher's fixed Linear calls. Only a session's own comment, which awaits an answer, does
  that now; a marker note leaves the issue on its usual 15-minute or hourly cadence.
- **The marker pass can no longer starve or fail a tick** (#117, the fix the v0.3.4 hotfix image carried).
  - **Markers run last:** in a tick, the claim markers run after the comment watch, intake, health, docs and the
    dispatcher.
  - **Each pass is bounded:** at most 3 rows; none started after 8 s, or with under 6 s left; and its own deadline
    3 s before the tick's.
  - **One proof per pass:** each pass shares one Linear connection and one binding proof.
  - **Refunds:** rows not started give their budget back at once.
  - **Nudges drain what's left:** a tick or nudge that stops with rows left queues a nudge, which repeats a minute
    apart until the queue drains. A queue stopped by the hour's budget waits for the next hour.
  - **Logs:** one line per marker pass, and per failed tick or nudge.
  - **Older servers:** a client against a v0.3.4 server falls back when the server refuses `marker_due`'s new `limit`.
- **The server's watcher budget can be raised.** `CLAWBRIDGE_WATCH_BUDGET` (1 to 600 Linear calls an hour, default
  200) sets the hour's budget of every tick and nudge of the server's own watcher, and of `operator health-run`, for
  every project. Before, only a client `linear watch --budget` could change it. An invalid value refuses to start.

## Project status follows the work (off until proven)

- **Project flow (schemas 21 and 22).**
  - **What it does:** with `"project_flow": true` in an enabled status policy, a hosted server moves each bound
    Linear project to started when its first issue starts, and to completed when all its issues are closed. The
    finish happens only where agents close the project's issues; elsewhere the coordinator is told.
  - **Early checks:** it runs in the daily health slot, and ClawBridge's own claim and done-release moves trigger
    an early check.
  - **Never backwards:** it never moves a project back, and a person's move back holds it until `clawbridge linear
    project-flow resume`.
  - **Existing policies:** they read it as off.
  - **Details:** docs/linear.md, "Project status follows the work".
- **Off until the operator proves it.** The server refuses to turn project flow on until it runs with `serve
  --project-flow-proven` (`CLAWBRIDGE_PROJECT_FLOW_PROVEN=1`), after the deploy probe; until then `status-policy show`
  says `off (not proven on this server)`. The minimal status policy that `link --closes …=agent` offers turns it on
  only on a proven server, and its preview says which.

## Project health

- **`operator health-run`** forces one health run of a bound Linear project now, instead of at its daily slot. It runs
  on the scheduled run's own path and post rules, under the project's watch lease, and is charged a whole run.
  - **Without `--confirm`:** it says what it would do.
  - **`--force-new`:** acts on this run alone, and marks the post as forced.
  - **`--delete UPDATE_ID`:** deletes a post that a forced run made, and only that.
  - Details: docs/operations/deployment.md.
- **`operator linear-app-probe --health-order --project UUID`** settles, before a project's health posts are turned
  on, that the health judgement's read holds the project's newest update. It makes two reads as the app and writes
  nothing. Only `agree: true` lets `post` go on.


- **The dispatcher's model is refused in a 101st project.** One owner's dispatcher agent serves at most 100 projects,
  so `dispatcher enable --engine model` and `dispatcher model advise on` past that are refused at `--confirm`. The
  preview says `blocked: model_projects_over_cap`. Turn the model off in another project first.
- **Retiring a session with confirmed work now needs `--outcome done` or `--confirm`.** `--outcome done` says the work
  is finished, and the issue moves on as a release with `--outcome done` does. `--confirm` releases it as it is. With
  neither, `clawbridge retire` (and MCP `retire_session`) is refused, naming the issue, and nothing changes. A move to
  a successor isn't asked.

## Security (G9 prep)

- **Releases carry build provenance, and installs refuse downgrades (G9-10).** Every release file is attested by the
  release workflow (`gh attestation verify FILE --repo jkai-chen/ClawBridge`). `CLAWBRIDGE_VERIFY=required` makes the
  installers check it, with the GitHub CLI, before installing. The installers no longer install an older version over
  a newer one unless you pass `--allow-downgrade` (Windows: `-AllowDowngrade`).

- **The ClawBridge Linear app answers members only (F1).** `linear_app_status`, `linear_app_states` and `linear_write`
  now prove the caller's workspace membership before anything reaches the app. Before, any Google account could read
  the app's Linear user and organisation IDs, since the sign-in client IDs are public. Clients that ask with no
  workspace are found by their membership, so existing clients need no change.
- **The dispatcher's launcher and agent credentials are tighter (G9-3, G9-4).** The launcher's routes now refuse plain
  HTTP from another machine, as the agent's already did. The launcher token mints at most 6 agent credentials an hour per
  owner (429 past that), and doctor warns when an owner's agent got more than 3 in a day. An agent's credential now
  expires 24 hours after it is redeemed; the agent stops and agentd starts it again with a fresh one. The launcher token
  serves every owner on the server: treat it like their provider keys.
- **One-time secrets print only to a terminal (F2).** `invite`, `device add` (its code, or a CI device's key) and
  `intake token create` refuse to print to anything but a terminal, such as an agent's shell or a pipe, before
  anything is created. `--out FILE` writes the secret to a new file instead (0600, never over an existing one); if
  that write fails, the new invite, device or token is revoked.
- **Agents ClawBridge starts don't get its secrets (F3).** The cloud hand-over's `claude`/`codex` and the notifier's
  Codex queue command now run without `CLAWBRIDGE_DEVICE_KEY`, the database URL, or any `CLAWBRIDGE_*` secret, token
  or seal key in their environment. Everything else the user has set, cloud and provider settings included, still
  reaches them.
- **Invite codes stay off the command line (F4).** The bootstrap invite's hint, the CLI help and the docs now say
  `clawbridge join --server URL --email EMAIL`, which asks for the code without echoing it (or reads it from stdin).
  `--code` still works, for scripts.
- **Downloads in the provisioning skills are pinned (G9-13).** The AWS skill checks Docker's Compose and Buildx
  plugins against SHA-256 values written in the skill, not checksum files from the same release. The GCP skill checks
  Docker's apt signing key against its published fingerprint before trusting the repository, and stops if it differs.
- **The dispatcher agent's model library is pinned to one fork release (G9-11).** The lock check accepts only the
  `cb-v1.1.0-1` tarballs, by version and integrity, and fails if that tag stops pointing at its commit (`96f054a`).
- **Traffic that never signs in can't lock signed-in users out (G9-1).** `/mcp/intake`'s per-address limit had shared
  `/v1/execute`'s per-principal table, so a flood from rotating IPv6 addresses could fill it and refuse every user with
  429. Intake and the watcher's tick endpoint now each limit addresses in their own bounded table, before any token is
  checked. A full table evicts its oldest entry instead of refusing newcomers, as `/v1/join`'s and failed sign-in
  counters now do too. An IPv6 client counts as its /64.

## Linear documents

- **`doc get` reads whole documents by default.** The default read is now 64 KiB, its maximum (it was 32 KiB). A read
  that is still cut says so in a `note`: how much was read of how much, and whether `--max-bytes` (MCP `max_bytes`)
  can read more.
- **The publish preview warns about lists Linear merges.** An ordered list that restarts right after another, with
  only blank or indented lines between, is merged and renumbered by Linear; the preview now says which lines, in
  `warnings`. It never refuses, and fenced code is skipped.

## The dispatcher

- **The reserved dispatcher sessions exist only while the dispatcher is on** (#99). Since v0.3.4, every watcher tick
  created `clawbridge-dispatcher` and `clawbridge-dispatcher-model` in every project, even with the dispatcher off.
  - **Off:** now nothing is created, and active ones are retired (never deleted), including those v0.3.4 left.
  - **Enable:** brings them back with the same IDs.
- **Enable refuses a dispatcher with nothing to match.** With no domains set and a task board binding that names no
  Linear projects, `dispatcher enable` now previews `blocked: no_domains` and refuses `--confirm` until domains are set.
- **docs/dispatcher.md is current:** enable and disable with `--engine`, the reserved sessions existing only while the
  dispatcher is on, and model mode as designed, with what is built and what isn't.
- **Model setup groundwork (schema 23).** The server now holds an owner's model provider key, sealed under a separate
  `CLAWBRIDGE_MODEL_KEY_SEAL_KEY`, and the endpoints the dispatcher agent and its launcher use: the launcher lists the
  owners that need an agent and mints one-time agent credentials; the agent redeems one once for its configuration and
  renews its project tokens. `clawbridge operator launcher-token create --out PATH` and `revoke ID` manage the
  launcher's token. Nothing turns model mode on yet: without a model key that passed its test, `dispatcher enable
  --engine model` is refused.
- **Model cases (schema 24).** In model mode each pending claim becomes a case the owner's dispatcher agent can take:
  it reads typed facts through four read tools and decides within the actions the rules' guardrails permit, or
  abstains. The dispatcher carries a decision out after the same checks a rules decision gets. Any case that ends
  without a decision (abstained, expired, no agent, or the model chose to wait) falls back to the rules, within about a
  minute when the in-process watcher runs. People appear to the model only as random per-project refs, never emails.
- **Seal-key rotation and launcher-token expiry.** `clawbridge operator model-seal-key rotate` counts, and with
  `--confirm` re-seals, every stored model provider key under the new `CLAWBRIDGE_MODEL_KEY_SEAL_KEY` in one
  transaction, with the old key set as `…_PREVIOUS` while it runs; a key that opens under neither stops it unless
  `--delete-unreadable`, and it says when the previous key can go. Launcher tokens now
  expire 90 days after creation; `launcher-token create` prints the date. The reference Caddy is pinned to log no
  requests (docs/operations/deployment.md).
- **Chat with the dispatcher's model (schema 25).** `clawbridge dispatcher chat "QUESTION"` asks the model from a
  project owner's own terminal, never a session's; owners let leads ask with `dispatcher chat leads on`. The model
  answers from the read tools and the decision log, which `dispatcher decisions` shows too. The reply is printed only to
  the asker's terminal, stripped of terminal controls, and nothing in it is acted on. Chats use at most a quarter of the
  model's runs, after claims. The `dispatcher enable --engine model` preview says who can ask.
- **Doctor shows the model mode to project owners.** Once an owner of the project has a model key: the server's seal
  key (and a previous one still set), each owner's key and agent (last seen, `provider_auth`, a failed newer test, a
  crash loop agentd reported, a flagged credential), whether agentd is calling and when its launcher token expires, and
  the live agents against the cap with the host's memory headroom (docs/dispatcher.md).
- **Onboarding offers the dispatcher.** The guided setup's last step offers a hosted project's owner the dispatcher in
  the rules engine, with what it does and the `dispatcher enable` preview; it stays off unless you say yes, and
  `dispatcher disable` turns it off any time.
- **The dispatcher's model can advise in rules mode (schema 28).** `clawbridge dispatcher model advise on` lets the
  model recommend confirm, decline or wait on each claim the rules escalate; the escalation still goes out at once, a
  person decides, and the follow-up carries only the action and reason. The model's note is read with
  `dispatcher escalations`, never in an inbox. `dispatcher model tools set` turns on the opt-in tools: `route_notice`
  (an unplaced escalation to a configured domain) and `ask_session` (one fixed question, capped). Disable and enable
  turn advice and the tools off. One owner's agent serves at most 100 projects with the model on; past that, the
  previews and doctor warn.
- **The launcher, `clawbridge-agentd`, runs each owner's dispatcher agent in its own container.** On the server host it
  asks the server which owners need an agent, mints each a one-time credential, and starts, restarts or removes their
  containers under a rootless Docker. Every Docker call goes through its own validating socket proxy, which allows only
  the exact container shapes it creates. Each agent reaches only its own model provider, through a per-owner egress
  proxy that allows only that provider's host and port, and an agent restarts when its owner's configuration changes.
- **The launcher's egress proxy can reach a server on your LAN or VPN, when you say so.**
  `CLAWBRIDGE_EGRESS_PRIVATE_SERVER=HOST:PORT` (the server's own) lets agents reach a ClawBridge server on a private or
  CGNAT address, never loopback or cloud metadata; by default every address must still be public. A compiled-in table
  also gives the Vertex AI, Bedrock and Azure provider hosts their token endpoint, ready for when the agent offers
  them.
- **The dispatcher agent follows its owner's projects as they change.** Each token renewal starts serving newly listed
  projects and stops the ones no longer listed, and an agent with no project yet stays up and waits for one
  (`no_projects_yet`) instead of exiting.

## Running your own server on AWS

- **AWS joins Google Cloud as a route for your own server.** `clawbridge onboard` (Start's "Set up my own server") asks
  for the cloud and hands AWS over to your agent. `clawbridge skill install --playbook aws` installs the provisioning
  playbook, `clawbridge cloud check` and `cloud secret set` work over SSM with your named AWS CLI profile, and backups
  go to S3. ClawBridge never reads `~/.aws`. The first-deployment guide covers both clouds.
- **Windows users get a WSL walkthrough for the AWS route** (docs/installation.md): install WSL, then the client, the
  AWS CLI and the Session Manager plugin inside it; sign in there; run the agent there; keep work in the WSL home.
- **`clawbridge cloud bootstrap --instance`: the owner's first device on an AWS server with device sign-in.** It makes
  the one-time owner invite's code on your machine, hands it to the server's new `operator bootstrap-invite
  --code-stdin` through a short-lived `SecureString` (so it is in no SSM command history), and joins with it; the
  server keeps only its salted hash, and the code is never shown. `--email` is checked against
  `CLAWBRIDGE_BOOTSTRAP_OWNER` before anything is created. Update the server first.
- **`clawbridge onboard` offers to create the workspace after the AWS hand-over.** Once the checks pass, and the
  server offers device sign-in, it asks for the workspace name and runs `cloud bootstrap --instance` for you on a yes
  (the default is no, which shows the written step). Resuming at sign-in asks again.

## Docs

- **Running a server with invites:** onboarding.md and first deployment (steps 5, 7 and 10) cover the owner's side
  of device sign-in: choosing it, letting the owner in with `operator bootstrap-invite`, and inviting the team with
  `invite --email … --grant`, each marked with what is released. `cloud bootstrap --instance` for AWS is marked
  planned.
- **Updating a server on AWS:** deployment.md has the Session Manager steps beside the Google Cloud flow.

## Context watching

- **`clawbridge context show` reads how much context a session is using**, from its own agent's local transcript
  (Claude Code or Codex). It shows tokens, the window when known, and compactions: counts and times only, never content.
  `doctor` gains a `context` check, which warns and never fails.
- **`clawbridge context bind` and opt-in Claude Code hooks.** `context bind` binds a session to its Codex thread or a
  named transcript. `context hooks install` (default `--scope local`, the git-ignored `.claude/settings.local.json`;
  `--scope project-shared` needs `--confirm-shared`) adds `SessionStart` and `PreCompact` hooks. They keep the binding
  current and print a reminder to re-read the handover note after a compaction. They always exit 0, a nested `claude -p`
  can't take its parent's binding, and `hooks uninstall` removes only ClawBridge's own entries.
- **Context watching on the server (schema 26), off by default.** A project owner sets levels by role, extra
  recipients and a handover hint with `clawbridge project context-watch set --file F --expected-revision N`. Sessions
  report counts only (`report_context`, not an MCP tool, fenced like a heartbeat, about one a minute), shown in
  `list_sessions`. The server sends `context` notices on transitions only: past warn to the coordinator; past handover to
  the session itself (with the hint) and the coordinator; an automatic compaction past warn as the backstop. Each is
  deduplicated, and notices to the coordinator are limited to one per session per 30 minutes.
- **Sessions report their context on their own, and the TUI shows it.** `clawbridge mcp` and `clawbridge notify` report
  a bound session's context when the level changes, a compaction appears, or every 10 minutes. One process per session
  reports, chosen by a lock file, and the answer's levels and hint are cached for the hooks. The Sessions screen gains a
  CONTEXT column (`612k warn [1000k]`).
- **A session can lower its own context levels (schema 27).** `clawbridge context levels set --warn-tokens N
  --handover-tokens N` sets the session's own levels below the project's, for an agent that should hand over earlier;
  `--clear` goes back to the project's. A level above the project's is refused, and every change is in the history.

# ClawBridge v0.3.4

**Schema 17 → 19.**
- The server migrates to schema 19 on its first start: schema 18 holds the sign-in groundwork (invites and device
  keys), and schema 19 is the ledger of project-health posts.
- **One-way:** v0.3.3 and older refuse a schema 18 or 19 database.
- **Rollback:** take a dump before upgrading. To go back, restore that schema 17 dump with the v0.3.3 image. Never
  run an older binary against the migrated database (docs/operations/deployment.md).
- **Compose:** `CLAWBRIDGE_DOMAIN` is now required. It sets the server's `--public-origin`, and Compose refuses to
  start without it.
- Upgrade the server first, then clients. A server that keeps Google sign-in (the default) works with v0.3.3 clients
  as before. **Upgrade clients before turning device sign-in on** (below).
- The dispatcher stays off: nothing in this release dispatches anything.

## The Linear app asks for the `write` scope

- On upgrade the server mints a new app token with the `write` scope, which revokes the old one.
- If Linear grants the rest without `write`, the server keeps working with that token. Issue relations and project
  status updates, the two writes that need it, are refused as `capability_unavailable`, and the server asks for
  `write` again every 7 days.
- **Relation guard:** before a relation write, the server checks the token's actual grant. Without `write` the
  write is refused as `capability_unavailable` before anything is sent.
- **Relation writes are read back:** `get_issue` now asks for relations, and a relation add reports `accepted` only
  when the edge is visible. Linear's issue read lists relations without saying the list is complete, so a removal is
  not confirmed from an absence.

## Project health can be posted to Linear (off)

Project health is judged once a day per enabled Linear project, as before. A health confirmed by two daily runs can
now also go to Linear as a project status update, by the app, at most once a day.
- **Off by default:** a project's health policy posts only with `"post": true`. Keep it off: a later release adds
  the check that Linear answers a project's newest updates first, which posting relies on.
- When it is on, a post is never made over a person's update from the last 7 days, nor when the five updates read
  are all older than 7 days. Each post is recorded before it is sent, and one whose answer was lost is found later,
  never sent again. A refused or held post goes to the coordinator session as a notice instead.
- `linear health show` shows the last post.

See "Project health" in docs/linear.md.

## Sign-in groundwork (inert)

- Device keys, invites, `/v1/join` and an operator bootstrap invite ship, but a server keeps Google sign-in until
  it is told otherwise (`CLAWBRIDGE_AUTH`, Compose's `--auth`, default `google`).
- In google mode `/v1/config` is unchanged: the sign-in fields appear only with device sign-in on.
- **Upgrade clients before turning device sign-in on** (`device` or `google,device`). The client releases that use
  it come later.

## Running your own server: Start and `clawbridge onboard`

- **Start:** on first run, `clawbridge` opens Start, and `clawbridge onboard` opens it again. It can hand the Google
  Cloud setup to your own coding agent.
- **The Google Cloud playbook** ships as a second skill, `clawbridge-provision-gcp`, which that agent follows. It
  guards the disk format and the env file, and uses a dedicated network.
- **`clawbridge cloud check`** checks a server's settings, its containers, the Linear app, and its infrastructure
  and backups on Google Cloud. It runs its SSH checks only with a host or a VM, and probes TCP 443 last.
- **`clawbridge cloud secret set KEY`** writes one of the server's secrets (the Linear app's client ID and secret,
  and the two Google OAuth client secrets) into its env file over your own SSH session, following a linked env file
  to its target. The value never passes through an agent.
- The server serves a static page at the Linear app's redirect URL.
- docs/operations/first-deployment.md is a first-deployment guide from nothing.

## The dispatcher's next steps (inert)

**Nothing in this release dispatches anything.** No dispatcher runs; enabling it comes with a later release.
- **The rules engine** that will decide pending claims within the guardrails is in, and is not called. The bus
  re-checks every grant, and a superseded claim is decided again.
- **Routing by domain (step 4a):** when the dispatcher is the project's coordinator, a coordinator notice (comments
  on an unheld issue, intake events, health and document notices) goes to its Linear project's domain, else the one
  domain its labels match, else the fallback chain. Labels route; they never grant. Without the dispatcher, notices
  go to the coordinator session as before.
- **dispatcher-agent:** the model engine, a ClawBridge client, and its hardened container image. It is not in the
  server image, and nothing starts it.

See docs/dispatcher.md.

Assets retain five CGO-disabled targets, installers, the MIT licence, selected checksums and a full source
marker. Binaries are unsigned; macOS binaries are not notarized.

See docs/linear.md, docs/dispatcher.md, docs/operations/first-deployment.md and docs/operations/deployment.md.

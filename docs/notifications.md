# Inbox notifications and agent wake-up

This source-checkout feature follows v0.1.0. Build the updated client; cloud use
also requires the matching updated service. Published v0.1.0 binaries and services
do not implement `inbox_status` or the watcher. Updating a local client does not
upgrade your hosted service.

Recipient wake policy and local host observation are implemented source additions
described by the [coordination contract](design/coordination.md).
Use matching policy-aware client and service builds; earlier notification-only
candidate evidence remains dated in the archive.

## Choose the delivery path

- A subscribed MCP client receives changes to `clawbridge://inbox/status`. Its
  host decides whether to display that notice or start an agent turn.
- An idle Claude Code session can be woken through ClawBridge's
  [channel plugin](#wake-an-idle-agent-through-a-channel-claude-code), which
  starts a turn carrying only a fixed notice and counts.
- A foreground watcher emits JSON events for a host integration, queues a
  fixed inbox-check notice into an explicitly selected existing Codex thread, or
  runs a command you choose with a fixed notice (for example, to type it into an
  idle agent's terminal).

On a server with the message stream, the watcher is woken by the server as soon
as the session's mail changes, and still checks once a minute in case a wake was
missed. Against an older server, or in local mode, it polls at a five-second
default interval and says `stream_unavailable` once on stderr. Each watcher, like
each open TUI and each agent, holds one of the 8 streams an account may have open
on a server; past that, a new one waits and retries. The agent fetches
actual message content with `inbox` and acks it after processing. A
notification or accepted queue request is not a delivery/read receipt.

## Inspect without consuming mail

Select your existing trusted route and current session/instance:

```sh
clawbridge --session reviewer --instance INSTANCE inbox-status
```

The source envelope includes immutable workspace/project/session IDs, total unread
count, latest unread sequence and count newer than `--after N`. Only unexpired
unread recipient deliveries count. This read leaves presence, delivery, read and
audit metadata unchanged. Offline mirrors cannot supply notification status.

## Foreground JSON watcher

Use a private state directory and an absolute checkpoint path:

```sh
clawbridge --session reviewer --instance INSTANCE notify --adapter json --state /absolute/private/reviewer-notify.json
```

Events are newline-delimited JSON on stdout; diagnostics go to stderr. Add
`--once` for one check/possible event, or `--interval 10s` to change the interval
within 1–300 seconds. JSON mode does not launch a host process.

The first run emits one event for existing unread mail. Later events coalesce
new arrivals; unchanged unread messages do not produce repeating reminders.
The private checkpoint survives restart and is tied to the exact authority,
recipient and instance. Only one watcher may use a checkpoint at a time.

## Codex queue adapter

Choose the existing Codex thread UUID that is configured for this same ClawBridge
recipient. The watcher uses that explicit mapping; it cannot prove the thread's
MCP configuration or discover the intended thread from a session name.

```sh
clawbridge --session reviewer --instance INSTANCE notify --adapter codex --thread THREAD_UUID --state /absolute/private/reviewer-codex-notify.json
```

The selected Codex CLI must expose `queue --thread --message`. You can select an
absolute executable with `--codex-command /absolute/path/to/codex`. The adapter
keeps the existing Codex account, model and execution permissions. It queues a
fixed metadata-only notice without including peer text or typing into a terminal.
A busy agent handles it through the host queue; actual wake/processing depends on
that host. No additional agent or background service is installed.

## Command adapter: wake an idle terminal

An agent that is idle in a terminal (Claude Code between turns, for example) has
nothing running that could notice mail. The command adapter runs a command you
choose whenever new mail arrives, such as one that types into that terminal:

```sh
clawbridge --session reviewer --instance INSTANCE notify --adapter command --state /absolute/private/reviewer-command-notify.json -- PROGRAM [ARG]...
```

Everything after `--` is the command, run directly, never through a shell. It is
only ever given fixed text: `N ClawBridge messages are waiting; call inbox` (or
`1 ClawBridge message is waiting; call inbox`). Each `{text}` argument is replaced
by it, and it is also in `CLAWBRIDGE_NOTICE_TEXT`, with the count in
`CLAWBRIDGE_NOTICE_COUNT`. Stdin gets the event's JSON: the type, IDs, counts and
sequence. No sender, subject or body ever reaches the command, so nobody who can
send to the session can type into its terminal. The command must finish within
10 seconds; if it fails, the checkpoint isn't advanced and the next change tries
again. ClawBridge's own secrets are removed from its environment.

In an [Orca](https://github.com/stablyai/orca) terminal (use the terminal's
handle):

```sh
clawbridge --session reviewer --instance INSTANCE notify --adapter command --state /absolute/private/reviewer-command-notify.json -- orca terminal send --terminal TERMINAL_HANDLE --text {text} --enter
```

In tmux:

```sh
clawbridge --session reviewer --instance INSTANCE notify --adapter command --state /absolute/private/reviewer-command-notify.json -- tmux send-keys -t SESSION:WINDOW.PANE {text} Enter
```

Typing into a terminal while the agent is busy queues the line as its next
prompt in most hosts; check yours before you rely on it.

## Wake an idle agent through a channel (Claude Code)

An idle Claude Code session can start a turn by itself when ClawBridge
messages wait for it, with no watcher typing into its terminal. ClawBridge's
Claude Code plugin runs a small server, `clawbridge channel serve`, that
pushes a notice into the session through Claude Code's
[channels](https://code.claude.com/docs/en/channels). The turn it starts
carries only fixed text and counts:

```text
<channel source="plugin:clawbridge:wake" kind="wake" count="2" seq="41">
2 ClawBridge messages are waiting; call inbox
</channel>
```

(`1 ClawBridge message is waiting; call inbox` for one.) No sender, subject or
body ever reaches it: the agent reads its messages with `inbox`, as always. It is
off by default, per session, and the [command adapter](#command-adapter-wake-an-idle-terminal)
keeps running beside it as the fallback.

**Requirements.**

- Claude Code with channels, a research preview.
- A claude.ai or Console login. Bedrock, Vertex and Foundry don't deliver
  channel events, so those sessions stay on the notifier.
- A custom `ANTHROPIC_BASE_URL` is untested: the channel stays `unconfirmed`
  until a push is proven, so the notifier keeps typing.
- macOS or Linux. On Windows the channel stays `unconfirmed` (it can't read the
  agent's command line), and the hooks are pending the Windows probe.

### Set it up

1. **Install the plugin**, once per person, in a plain terminal:

   ```sh
   clawbridge channel setup
   ```

   It shows the two commands still missing, asks, and runs them only on yes. It
   refuses to run in an agent's pane (where `CLAWBRIDGE_SESSION` or
   `CLAWBRIDGE_INSTANCE` is set), or with `--session`. You can run the commands
   yourself instead:

   ```sh
   claude plugin marketplace add jkai-chen/ClawBridge
   claude plugin install clawbridge@clawbridge --scope user
   ```

2. **Allowlist the plugin, once per machine, as an administrator.** Claude Code
   takes a channel only from a plugin on its allowlist, and a user setting can't
   add one: it takes a managed-settings file. `clawbridge channel allow` shows the
   file and the warnings below; with `--write`, run as root, it writes it:

   ```sh
   clawbridge channel allow
   sudo clawbridge channel allow --write
   ```

   If `sudo` can't find `clawbridge` on its own `PATH`, give the path:
   `sudo "$(command -v clawbridge)" channel allow --write`. On Windows, run
   `clawbridge channel allow --write` in a terminal run as administrator. ClawBridge
   never raises its own privileges. `--write` needs a person at a terminal: it
   restates that the file is machine-wide and writes only after you type `yes`
   (`--force` doesn't skip that), and without a terminal it writes nothing.
   Agents must never run it.
   The file is `managed-settings.d/50-clawbridge-channels.json` in Claude Code's
   managed-settings folder:

   | OS | File |
   | --- | --- |
   | macOS | `/Library/Application Support/ClaudeCode/managed-settings.d/50-clawbridge-channels.json` |
   | Linux and WSL | `/etc/claude-code/managed-settings.d/50-clawbridge-channels.json` |
   | Windows | `C:\Program Files\ClaudeCode\managed-settings.d\50-clawbridge-channels.json` |

   ```json
   {
     "channelsEnabled": true,
     "allowedChannelPlugins": [
       {
         "marketplace": "clawbridge",
         "plugin": "clawbridge"
       },
       {
         "marketplace": "claude-plugins-official",
         "plugin": "discord"
       },
       {
         "marketplace": "claude-plugins-official",
         "plugin": "telegram"
       },
       {
         "marketplace": "claude-plugins-official",
         "plugin": "fakechat"
       },
       {
         "marketplace": "claude-plugins-official",
         "plugin": "imessage"
       }
     ]
   }
   ```

   Before you write it:

   - **It is machine-wide:** it applies to every Claude Code user on this machine.
   - **It replaces Anthropic's default channel list** unless another managed
     list exists (the lists from all managed documents add up). So it also lists
     Anthropic's four official channel plugins, which would stop working on this
     machine otherwise.
   - **Anthropic may change its default list later,** and this file won't follow:
     if Anthropic adds an official channel plugin, add it to the file yourself
     (or with a newer ClawBridge's `channel allow --write --force`), or it stays
     blocked on this machine.
   - **To undo it,** delete the file (`sudo rm` it, or as administrator on
     Windows). Rerunning `--write` changes nothing when the file is already in
     place; a different file there is shown as a diff and replaced only with
     `--force`.
   - **MDM or claude.ai organisation settings may take precedence over the
     file.** On a machine managed that way, or on claude.ai Team or Enterprise,
     an admin lists the plugin (`"marketplace": "clawbridge", "plugin":
     "clawbridge"`) and enables channels there instead. ClawBridge can't see
     those settings, so `mcp install` and `doctor` may still name this step.

3. **Ask for the channel when you install the session's MCP entry:**

   ```sh
   clawbridge mcp install --agent claude-code --session api-worker-2 --channel
   ```

   `--channel` adds one more hook, a UserPromptSubmit hook (`clawbridge hook
   turn`), to the Stop, PreCompact and SessionStart hooks. It doesn't change the
   MCP entry. It then lists, on stderr, the setup steps still missing and the
   launch line; it reads Claude Code's settings but never edits them, and it
   never runs `claude`. It is refused for `--agent codex` (Codex has no
   channels; it keeps the notify adapter) and with `--hooks none`.

4. **Launch Claude Code with the channel,** the session's name and instance
   exported in that shell:

   ```sh
   CLAWBRIDGE_SESSION=api-worker-2 CLAWBRIDGE_INSTANCE=api-worker-2-pane claude --channels plugin:clawbridge@clawbridge
   ```

   Add the session's usual flags. The plugin's server takes the session from
   `CLAWBRIDGE_SESSION`; without it, it answers Claude Code and does nothing.
   Two sessions in one folder each get their own pane with their own exported
   pair. The plugin's server is started in every Claude Code session where the
   plugin is enabled, but pushes only where `--channels` names it.

### How it decides to push

The channel server reads the session's counts the way the Stop hook does,
through the session's own MCP server record on this machine: the observe stream
on a hosted server, otherwise a poll every 30 s. Until that MCP server knows its
instance (from `--instance`, or after `register_session`), there is no record and
the channel is `inactive`. It pushes when all of these hold:

- messages are unacked, the policy is `accept`, and no compaction hold is set;
- the newest message is newer than the last push and than what the Stop hook
  already told the agent;
- **the agent is idle.** The turn hook marks a turn's start and the Stop hook
  its end, so nothing is pushed into a running turn: the Stop hook covers that
  turn's end. A turn started over 10 minutes ago without an end counts as idle,
  because Claude Code runs no Stop hook when you press Esc. Another tool's
  blocking Stop hook may make a turn that continues read as ended;
- a burst of messages is coalesced (3 s) into one push, and at most one push
  goes out a minute.

**Confirmation.** Claude Code never acknowledges a push. A push counts as
delivered only when the session's own channel turn carrying the same `seq` is
seen (the turn hook reads only that number from it). No such turn within 120 s
puts the channel in `fallback`; a later confirmed push makes it `active` again.
A typed notice can't confirm it. An `active` channel whose server can't read the
counts for over 60 s (its database or hosted server unreachable) can't push
either, so it goes to `fallback: counts unreadable for over 60 s`; once it reads
them again, it needs a newly confirmed push to be `active`.

**The notifier keeps running.** Keep the [command
adapter](#command-adapter-wake-an-idle-terminal) (or the Codex adapter) running
for the session as before. It skips typing only while the channel is confirmed
`active` (a fresh record from a live channel server), and then never marks the
skipped batch handled: when the channel falls back, it types that batch. Until
the first confirmed push, a batch may get both a channel turn and a typed line;
both say "call inbox". When a push is silently dropped, the wake can take about 4
minutes (120 s to fall back, the 60 s between pushes, and the notifier's minute
re-check). After a turn you interrupt with Esc, the wake can take up to 10
minutes from that turn's start: the turn counts as running until then, so the
channel doesn't push, and while the channel is `active` the notifier doesn't
type either.

**Sessions started with `--mcp-config FILE --strict-mcp-config`.** Claude Code
2.1.314 doesn't start a plugin's servers under `--strict-mcp-config` (checked
with `scripts/e2e/channel-wake.sh --strict`): such a session has no channel and
stays on the notifier. To give it a channel, drop `--strict-mcp-config`; its
`--mcp-config` entry still brings the ClawBridge tools, and the plugin brings the
channel. A server named only in `--mcp-config` can't be a channel itself.

**Limits.**

- **One channel record per session name.** A second instance of the same
  session may be silenced by the first one's confirmed channel: its notifier
  skips typing too.
- **A batch the Stop hook already announced gets no idle wake** while the
  channel is `active`: the channel doesn't push a message the Stop hook already
  told the agent about, and the notifier stands down. If the agent goes on
  without reading its inbox, those messages wait for a newer one (or for the
  channel to leave `active`).
- **Removing only the turn hook** takes an uninstall, then an install without
  `--channel`: a plain reinstall keeps the hook.
- **Going back to an older ClawBridge:** first run `clawbridge context hooks
  uninstall` (with the same `--scope`) with this version, then install the hooks
  again with the older one. An older binary doesn't know the turn hook: it can't
  remove it, and every prompt then shows a hook error (the prompt still goes
  through). To undo the whole channel setup, also run `claude plugin uninstall
  clawbridge@clawbridge`, `claude plugin marketplace remove clawbridge`, and
  delete the managed-settings file (step 2), and launch Claude Code without
  `--channels`.
- **Codex has no channels;** its sessions keep the notify adapters.

**Check it.** `clawbridge --session NAME doctor` shows a `mcp.channel` line with
exactly one of these:

| State | Meaning |
| --- | --- |
| `active` | A push was confirmed by the session's own channel turn; the notifier stands down |
| `unconfirmed` | The channel may work, but no push is confirmed yet; the notifier still types until one is |
| `inactive: REASON` | It can't work: for example not launched with `--channels`, the plugin not installed, not allowlisted, a third-party provider, no MCP server record, or a stale record. A warning when the session asked for a channel (its turn hook is installed), a pass otherwise |
| `fallback: REASON` | A push went unanswered for 120 s, or the channel server couldn't read the session's counts for over 60 s; the notifier types again. A warning |

Every line with a record also says whether the notifier stands down ("The
notifier stands down while the channel is active.") or keeps typing hints (until
a push is confirmed, while `unconfirmed`). If the session was launched with
`--dangerously-load-development-channels` instead of `--channels` (a flag for
local development only), the line is a warning, whatever the state, and the
action is to relaunch with `--channels plugin:clawbridge@clawbridge` after the
admin step.

Its action names the next step: the plugin install, the admin allow step, or a
relaunch with `--channels` and `CLAWBRIDGE_SESSION` exported. It shows no prompt
or message. The record is `<session>.channel.json` under `~/.clawbridge/context`
(`$CLAWBRIDGE_HOME/context`).

## Run the watcher on Windows

The watcher is one foreground process with no shell, so it runs the same way on
Windows. Whether a given host (a terminal, an agent app) actually wakes from the
notice on Windows is pending the Windows probe; the adapters' own behaviour (the
argv they run, the checkpoint) is tested on Windows in CI. Use absolute Windows
paths and keep the state file under your own profile (for example
`%LOCALAPPDATA%\ClawBridge`), which only you can read.

Start it hidden, for your user only, in PowerShell (no administrator rights).
`Start-Process` does not quote the elements of `-ArgumentList`, so wrap every
value that can hold a space (your profile path, for one) in quotes yourself:

```powershell
$state = Join-Path $env:LOCALAPPDATA 'ClawBridge\reviewer-codex-notify.json'
New-Item -ItemType Directory -Force -Path (Split-Path $state) | Out-Null
Start-Process -WindowStyle Hidden -FilePath clawbridge -ArgumentList @(
  '--session','reviewer','--instance','INSTANCE',
  'notify','--adapter','codex','--thread','THREAD_UUID','--state',"`"$state`"")
```

For the command adapter, give the program and its arguments after `--`. The
watcher passes each of them to the program as one argument, so a PowerShell
script receives `{text}` as one value:

```powershell
$state = Join-Path $env:LOCALAPPDATA 'ClawBridge\reviewer-command-notify.json'
Start-Process -WindowStyle Hidden -FilePath clawbridge -ArgumentList @(
  '--session','reviewer','--instance','INSTANCE',
  'notify','--adapter','command','--state',"`"$state`"",
  '--','powershell.exe','-NoProfile','-File',"`"$env:LOCALAPPDATA\ClawBridge\wake.ps1`"",'{text}')
```

`wake.ps1` is yours: it receives the fixed notice as its first argument (and in
`CLAWBRIDGE_NOTICE_TEXT`) and decides how to reach the agent. Stop the watcher
from Task Manager, or stop just this one by its command line (other `clawbridge`
processes, such as an agent's `clawbridge mcp`, keep running):

```powershell
Get-CimInstance Win32_Process -Filter "Name='clawbridge.exe'" |
  Where-Object { $_.CommandLine -like '*--session reviewer *notify*' } |
  ForEach-Object { Stop-Process -Id $_.ProcessId }
```

To start it at log on instead, register a task for the current user, which needs
no administrator rights:

```powershell
$state = Join-Path $env:LOCALAPPDATA 'ClawBridge\reviewer-codex-notify.json'
$action = New-ScheduledTaskAction -Execute 'clawbridge' -Argument "--session reviewer --instance INSTANCE notify --adapter codex --thread THREAD_UUID --state `"$state`""
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$settings = New-ScheduledTaskSettingsSet -Hidden
Register-ScheduledTask -TaskName 'ClawBridge reviewer watcher' -Action $action -Trigger $trigger -Settings $settings -User $env:USERNAME
```

Use the full path to `clawbridge.exe` as `-Execute` if it is not on the PATH of
the logon session. A task registered this way runs only while you are logged on;
`-Hidden` hides it in Task Scheduler, and a console window may still show at log
on (start it through `Start-Process -WindowStyle Hidden` if that matters).

## Agents: ack, the Stop hook and compaction

An agent's MCP server takes the session's messages from the queue once the
agent first calls `inbox` (or `ack`, `reply`, `mark_read`): on a server with
the stream it holds the session's consume stream, otherwise it polls. Messages
it holds show a `lease` in `inbox`. The agent handles each one, then calls
`ack`, or answers with `reply` and `ack: true`, which acks in the same step. A
message that isn't acked is offered again when its lease runs out, and after 5
offers it is a dead letter the coordinator is told about. While the agent keeps
working, the MCP server extends its leases, so a long turn doesn't use up
offers.

`clawbridge mcp install --agent claude-code` also installs Claude Code hooks
in the project's `.claude/settings.local.json` (`--hooks none` leaves them out;
`clawbridge context hooks install` adds them later):

- **Stop:** a turn that ends with unacked messages is sent back once with
  `N ClawBridge messages are waiting: call inbox, handle them, then ack`. It
  doesn't block again for the same messages, nor when the server is old, slow
  or down, nor while the session's policy is `hold` or `refuse`.
- **PreCompact and SessionStart:** before a compaction the session's
  notifications are held and its leases given back without counting an offer;
  after it they are accepted again and everything is offered anew. A hold that
  you set yourself is never lifted. If the agent dies mid-compaction, the next
  start lifts the hold; until then its messages wait, and `clawbridge doctor`
  shows the hold.

**Relaunching Claude Code by hand.** The hooks name the session from
`CLAWBRIDGE_SESSION` in Claude Code's own environment; without it they bind
nothing, and `doctor` says the session is "not bound to a transcript". When you
start or resume Claude Code yourself (`claude --resume …`) rather than from the
launcher that set it, export it first, in that shell:

```sh
export CLAWBRIDGE_SESSION=api-worker-2 CLAWBRIDGE_INSTANCE=api-worker-2-pane
claude --resume
```

Any session start binds once it is set: a fresh start, a resume, `/clear` or
`/compact`. The hook command also needs `clawbridge` on that shell's `PATH`.

**Hook forms.** On macOS and Linux each hook is a POSIX shell command. On Windows
there is no POSIX shell, so `mcp install` writes each hook in Claude Code's exec
form: an entry with `command` (the absolute path of `clawbridge.exe`) and `args`
(for example `hook stop --from-hook --hook-tag clawbridge-context-hook-v1`),
which Claude Code runs with no shell. The tag only marks the entry as
ClawBridge's, so a reinstall replaces it and an uninstall removes it, in either
form. A shared settings file holds the form of the OS that last installed it, so a
team on mixed systems should install the hooks per machine (local or user scope). The `project-shared` scope writes `clawbridge` instead of a path, so a
shared hook runs whatever `clawbridge` the project folder or PATH resolves, the
same trust boundary as any committed hook. The hooks on Windows are pending the
Windows probe: they are written, but waking an agent through them is not yet
confirmed.

The Stop hook covers a busy agent. An idle one still needs the command adapter
above, or a [channel](#wake-an-idle-agent-through-a-channel-claude-code), to be
woken.

`clawbridge doctor` shows whether the session's MCP server holds the stream,
and the session's unacked and dead counts. It reads the counts as the
session's instance from its MCP server's record on this machine, or from
`--instance` without one; when the server refuses, the queue line names the
reason (`invalid`: that instance isn't the session's current one, so pass the
current `--instance` from its `whoami` or restart its MCP server; `forbidden`:
the session is another person's) instead of a bare "rejected". Its "Claude Code hooks" line says
which form is installed (`exec` or `shell`) and the Claude Code version
(`claude --version`, run with a short timeout; an unknown version never fails
doctor). On Windows with the exec form it warns that the hooks are pending the
Windows probe, until the version is on the list of probed versions; to help the
probe, report your Claude Code version and whether a Stop hook held back a stop
while mail was waiting. Details: [API reference, "Agents and
the queue"](reference/api.md#agents-and-the-queue).

## Choose when to receive automatic notices

With matching policy-aware client and service builds, the recipient controls its
own wake policy:

```sh
clawbridge --session reviewer --instance INSTANCE notification-policy --policy hold
clawbridge --session reviewer --instance INSTANCE notification-status
clawbridge --session reviewer --instance INSTANCE notification-policy --policy accept
```

Accept enables notices. Hold defers them; refuse disables them until explicitly
changed. Both keep accepted mail unread and retain pending notification state.
Returning to accept notices pending unread arrivals once. Inbox/resource reads
still work while notices are suppressed, and the policy survives explicit resume.
A change applies on the next poll; it cannot recall an already queued hint.
Older watchers do not honor policy, so coordinate the upgrade rather than mixing
policy-aware and older notifier processes.

## Observe a Codex thread or wait once

Select the existing local Unix WebSocket control endpoint yourself:

```sh
clawbridge host-status --adapter codex --endpoint unix:///absolute/control.sock --thread THREAD_UUID
clawbridge host-wait --adapter codex --endpoint unix:///absolute/control.sock --thread THREAD_UUID --for idle --timeout 30s
```

These commands request host metadata without turn history. They do not start or
resume an agent, change settings, emit conversation text or touch ClawBridge authority. host-wait returns
one result, immediately when already matching or after bounded polling. Only
outcome=matched succeeds. Use unloaded to observe notLoaded; actual process exit
is unsupported. An observed idle thread does not prove its queue is empty or its
task complete. Disconnect is unavailable, and native Windows transport is not
supported by this adapter. Do not wait for your own active thread to become idle.
The caller consumes the result; a match does not itself queue a model turn.

## Recovery

Ctrl-C stops polling and child processes. Temporary transport/host failures back
off; authorization, fencing, incompatible state and capability failures stop.
The watcher does not advance its checkpoint after a failed hint. A crash after
host acceptance but before saving state can duplicate a hint; stable event IDs
help identify it. The agent's durable inbox/read state remains the processing record.

A changed authority, recipient, instance, executable, Codex home or thread needs
its own explicitly chosen binding/checkpoint. Inspect a mismatched/corrupt state
or an abandoned lock before manual recovery. A dead watcher does not remove the
inbox messages, and the normal inbox check remains available.

The [API reference](reference/api.md) defines status fields. The
[notification contract](design/notifications.md) records detailed lifecycle and
checkpoint behavior; [review evidence](archive/reviews/NOTIFICATION-REVIEW.md)
separates fixtures from native host observations.

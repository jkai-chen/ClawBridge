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
above to be woken.

`clawbridge doctor` shows whether the session's MCP server holds the stream,
and the session's unacked and dead counts. Its "Claude Code hooks" line says
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

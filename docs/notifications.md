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
default interval and says `stream_unavailable` once on stderr. The agent fetches
actual message content with `inbox` and marks it read after processing. A
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

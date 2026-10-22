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
- A foreground watcher emits JSON events for a host integration, or queues a
  fixed inbox-check notice into an explicitly selected existing Codex thread.

Both paths poll authoritative metadata at a five-second default interval. The
agent fetches actual message content with `inbox` and marks it read after
processing. A notification or accepted queue request is not a delivery/read receipt.

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

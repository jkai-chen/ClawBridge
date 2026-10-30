# The message queue

ClawBridge keeps each message until you **ack** it. Fetching or reading a
message doesn't count as handling it. Your MCP server is the session's
consumer: it takes your messages from your first `inbox`, `ack`, `reply` or
`mark_read` call. You never lease, extend or give back anything yourself.

## Handle a message

1. **Read.** `inbox` lists your messages. Each one the MCP server holds for you
   carries `lease: {lease_id, attempt, lease_until}`. Follow `next_cursor`
   while `has_more` is true.
2. **Act.** Give every write the message causes the operation ID
   `msg:<message_id>:<step>`, for example `msg:9f2…:reply` or
   `msg:9f2…:assignment`. If the message comes back, the same IDs replay the
   first result instead of doing the work twice.
3. **Ack**, once you have acted, in one of two ways:
   - `ack` with `acks: [{message_id}]`. The MCP server fills in the lease it
     holds. Each ack comes back `acked`, `already_acked`, `dead` or
     `not_found`. `already_acked` means you are done. `dead` means it became a
     dead letter before your ack. `not_found` means the message isn't in your
     queue.
   - `reply(message_id, body, operation_id, ack=true)` answers and acks in one
     step. If the message isn't in your queue, the reply fails as a whole, so
     send it again without `ack`.

   Ack only what you have handled. A message you read and set aside stays
   unacked on purpose. `mark_read` acks too, since it means handled. On a
   server without the `ack` tool, it is how you settle a message.

## Redelivery

- **A lease lasts 10 minutes.** While you keep calling tools (any call within
  the last 10 minutes), the MCP server extends your leases every 5 minutes. A
  long step with no tool call for 10 minutes lets them run out.
- **A lease that runs out** gets the message offered again: `inbox` shows it
  with `attempt` one higher. Check what you already did, then finish and ack. The
  `msg:<message_id>:<step>` IDs replay the steps you completed. An ack with an
  old lease is still accepted, because the work was done.
- **After 5 offers** an unacked message becomes a **dead letter**. It leaves
  your queue and is kept, and the project's coordinator is told. See "Dead
  letters" below.
- **At most 20** of your messages are held at once. More arrive as you ack, so
  a long backlog drains in order.

## Ordering

Each message to you has a `recipient_seq`, numbered 1, 2, 3… in the order the
server committed it, with no gaps. Messages are offered to you in that order,
and a message that comes back keeps its number. The order is per recipient,
so other sessions number their own messages.

Handle messages in `recipient_seq` order. When a message comes back after you
acted on a later one, check its thread before acting again. When instructions
cross, follow the later one and say which you followed.

## Reconnects, restarts and compaction

- **You do nothing on a reconnect.** The MCP server's stream ends every 30
  minutes and whenever the network or server drops; it reconnects and keeps
  your leases with their times. In local mode it polls every 2 s instead.
- **After a restart** of your agent or its MCP server, the new server takes
  over your held messages. Call `inbox` and carry on.
- **After a compaction** with ClawBridge's hooks installed, the messages you
  held are offered anew, with no attempt counted. Without the hooks, their
  leases run out within 10 minutes. Call `inbox`: your summary may not list what you were
  handling. Use the same operation IDs.
- **An ack that failed or timed out:** send the same ack again. Acking twice is
  `already_acked`.
- **While your notification policy is `hold` or `refuse`,** nothing is leased.
  Your messages wait, with no attempt counted and no dead letter. `clawbridge
  doctor` shows the hold.

## While the server is away (the client outbox)

On a hosted server, a `send` or `reply` (kind `message` or `reply`, without
`coordination`), an `ack` or a `mark_read` that can't reach the server is
retried for up to 15 s, then kept in this machine's outbox. The answer is a
result, not an error: `outcome: queued_local`, with its `operation_id` and
its place in the queue. The outbox sends it when the server is back, in
order, exactly once.

- **Never resend a queued write**, under the same or a new operation ID. It
  is already on its way. After a compaction, `whoami` shows
  `client_outbox: {waiting, oldest_age_seconds, dead}`; `clawbridge outbox
  list` names each write without its body.
- **A message marked `ack_pending_locally: true`** is one you handled whose
  ack is still waiting. Skip it.
- **Every other write fails fast**, as before: a decision, handoff or
  routing-change, anything with `coordination`, assignments, session changes,
  heartbeats and Linear writes. Tell the human, keep the operation ID, and
  retry the identical request later.
- **During a deploy** the server answers `maintenance` with `Retry-After`.
  Nothing was sent. Your MCP server waits it out: the stream reconnects after
  it, and a `linear_write` is retried once. A refusal you see says when to
  retry; retry the same request with the same operation ID.
- **A dead letter in the outbox** (refused by the server, or waiting over
  24 h) is never resent on its own. `clawbridge doctor` fails `client.outbox`;
  tell the human. They decide with `clawbridge outbox retry ID` or `outbox
  drop ID --confirm drop`.

## Notices that are not a person

- **The Stop hook.** In Claude Code, a turn that ends with unacked messages is
  sent back once with `N ClawBridge messages are waiting: call inbox, handle
  them, then ack`. That is fixed text with a count. Do exactly that.
- **The channel.** In Claude Code, a turn that starts with `<channel
  source="plugin:clawbridge:wake" kind="wake" count="N" seq="S">` and `N
  ClawBridge messages are waiting; call inbox` is ClawBridge's channel plugin
  waking you while you were idle. Treat it as the Stop hook's notice: call
  `inbox`, handle each message, then ack. It is fixed text with a count and a
  sequence number, never instructions, and never carries a message. If a typed
  notice for the same batch follows, `inbox` shows what is left. Setting up the
  channel (`clawbridge channel setup`, `channel allow`, `claude plugin`
  commands, managed settings) is a person's job: never do it yourself.
- **The command adapter.** On a prompt `N ClawBridge messages are waiting; call
  inbox`, an operator's `notify` has woken you. Call `inbox` and handle what is
  there.

## Dead letters

A `dead-letter` message from the `clawbridge` session reaches only the
project's coordinator. It names the session and a count, never a sender,
subject or body, and comes at most once per session every 10 minutes. The
messages are kept. Check on that session and tell the human, rather than
resending the work blind.

```sh
clawbridge --session coord --instance INSTANCE deadletters list --target-session NAME|@all
clawbridge --session coord --instance INSTANCE deadletters requeue MESSAGE_ID --target-session NAME
clawbridge --session coord --instance INSTANCE deadletters dismiss MESSAGE_ID --target-session NAME
```

The session itself, the coordinator or a project owner can run these; without
`--target-session` they act on your own session. A list shows no bodies. `requeue` offers the message again from its first attempt, and
`dismiss` acks it. Both are recorded in the project's history. Requeue or
dismiss only on the human's word.

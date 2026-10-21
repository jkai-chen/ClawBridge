# Coordination messages, wake policy and host observation

Status: implemented and independently accepted in source, 21 October 2026.
[Scoped review](../archive/reviews/COORDINATION-REVIEW.md) records actual PostgreSQL
full/race, vet, cross-builds and targeted transport evidence. This increment starts
from `59d68b57eef62938b9fdf99ae49105b16785df70`; it is not part of the prepared
v0.2.0 candidate or its accepted notification evidence. Root owns this contract,
guides and skills; the engineer owns production and focused tests; the reviewer
owns independent integration probes and `COORDINATION-REVIEW.md` under the review
archive. Use Codex queue for coordination. Source preparation does not authorize
publication, deployment, live host operations or Linear mutations.

## Work and repository boundaries

A ClawBridge workspace/project is the communication and permission boundary. A
product can use one project across frontend/backend repositories: each checkout
explicitly selects the same project, and an assignment qualifies its owned paths
with the repository identity. One worker can keep one session across those repos;
separate independently owned slices can use separate sessions. Project identity
does not attest an agent's current working directory.

The existing single Git remote is a discovery association. This increment keeps
that API: multiple checkouts use explicit project selection. Automatic matching
of several registered remotes is a later additive change. Repositories requiring
different project permissions retain separate actor bindings. Linked Linear
issues do not authorize cross-project messaging or automatic forwarding.

Linear supplies assignments, dependencies and acceptance criteria through existing
tools. A coordinator assigns each slice and an opaque assignment revision. Agents
check prerequisites before starting and record implementation-ready, independent
acceptance and deployment separately. Board read-modify-write is not an atomic
task claim. No Linear connector, new task engine or automatic ownership transfer
is introduced here.

## Optional coordination metadata

`send` and `reply` accept an optional `coordination` object, independent of `kind`:

```json
{"issue_id":"11111111-2222-3333-4444-555555555555","assignment_revision":"coordinator-7","intent":"request","source_commit":"59d68b57eef62938b9fdf99ae49105b16785df70"}
```

The closed object has these fields:

| Field | Rule |
| --- | --- |
| issue_id | Required canonical lowercase UUID; the issue within the explicitly assigned Linear workspace |
| assignment_revision | Required nonempty opaque reference, 1–128 printable ASCII bytes; no secret patterns |
| intent | Required: request, accept, decline, clarify, progress, ready, blocked or handoff |
| request_id | Optional ClawBridge message ID, 1–128 ASCII identifier characters `[A-Za-z0-9._-]`; required for accept/decline/clarify |
| source_commit | Optional complete lowercase hexadecimal Git object ID, 40 or 64 characters |

Encoded canonical metadata is at most 1,024 bytes. Reject null, unknown fields,
wrong types, empty optional fields and invalid values atomically. Absence retains
the existing payload and behavior; an empty object is invalid. CLI exposes
`--coordination JSON` on send/reply, and MCP exposes the typed object. Bodies and
existing kinds remain mandatory/validated as before; no intent is inferred from
body text or kind. All semantic metadata participates in the retry digest. When
metadata is absent, preserve the exact historical digest so pre-migration retries
still return their original message. Key order does not alter the digest.

Persist and return the object on message/inbox/retry and sender-only message_status
results, including snapshot sent_messages and the corresponding mirror results;
omit it for legacy messages. Keep participant context out of broadly readable
audit details. It follows the existing message's
project/participant/retention rules. Expired purge clears metadata with body text,
retaining the original retry tombstone. Do not copy metadata into host hints.

Whenever request_id is supplied, require an unexpired referenced message visible
to the sender in the same project, whose metadata has the same issue/revision.
The referenced message may have any valid intent; this is a context link. A
correlated `reply` also requires request_id to equal its message_id. This verifies
conversation context, not permission to approve work. Uncorrelated progress/ready
can describe the assigned issue without referencing a particular message.

Receivers compare assignment_revision for equality against the last assignment
confirmed by their coordinator/Linear record. Revisions are opaque, not sortable.
A different revision is unconfirmed context: preserve the message, ask the
coordinator, and exclude its progress/ready claim from the current completion
decision. An old message can still contain an unresolved question. No automatic
read, wake suppression, reassignment, closure or approval follows from metadata.
The server does not query or attest Linear, and a higher-looking peer revision
cannot supersede an assignment. `accept` agrees to the bounded request; `ready`
offers an artifact for review. Neither is human approval or independent acceptance.
For a multi-repository artifact, include repository/full-commit pairs in the body;
source_commit alone does not identify which repository it belongs to.

## Recipient wake policy

Add `set_notification_policy` with required policy `accept`, `hold` or `refuse`.
Only the active owned actor with its exact current instance can change its own
policy; no target_session override, administrator peer override or sender control.
The setter is an ordinary audited actor mutation. Readers with an owned active
actor may set their own policy. Default is accept. Policy persists across resume;
the new instance can explicitly change it. Retirement/move ends the actor's
notification eligibility under the existing rules.
The setter returns workspace_id, project_id, session_id and policy inside the
ordinary source envelope. Repeating the same policy is successful; it does not
grant any other session permission or rotate its instance.

Add a side-effect-free `notification_status` operation, sharing inbox_status's
scope, authorization, read-only transaction and no-mirror behavior. Its exact data
fields are the existing six inbox_status fields plus `policy`. The original
inbox_status six-field response remains unchanged. Both require active owned
actors. Expose the new read tool and setter in MCP; CLI names are
`notification-status` and `notification-policy --policy VALUE`.

The foreground watcher and MCP resource poll notification_status so policy and
counts are one authority snapshot. The resource content remains the original
six-field inbox_status envelope. `accept` enables the existing notices. `hold`
means temporarily defer; `refuse` means disabled until explicitly changed. Both
suppress automatic JSON/Codex hints and MCP invalidations while retaining accepted
mail and unread state. Direct inbox/status/resource reads remain available.
Neither suppressed mode advances the watcher's acknowledged cursor nor the MCP
last-notified count/sequence. Returning to accept therefore notifies pending
unread arrivals once, including after restart. Already acknowledged, unchanged
mail does not repeat. Read/expiry still never lowers the checkpoint.

Policy cannot recall an already emitted hint; a change takes effect on the next
authoritative poll. There is no atomic transaction spanning the database and host
queue. Keep the original private checkpoint/binding, retry and uncertain-acceptance
rules. A suppressed --once returns success without a hint; a bounded diagnostic
may state wake_suppressed, never mail delivered/read. Avoid per-poll diagnostic
spam. Matching new clients/service are required: old notification binaries do
not honor this policy, and unsupported servers fail rather than using old status
or a mirror. No claim of retroactive enforcement is made.

## Observed host state and one-shot wait

Add local operator commands, independent of ClawBridge authority/configuration:

```text
clawbridge host-status --adapter codex --endpoint unix:///absolute/control.sock --thread THREAD_UUID [--timeout 5s]
clawbridge host-wait --adapter codex --endpoint unix:///absolute/control.sock --thread THREAD_UUID --for idle|unloaded [--timeout 30s] [--interval 1s]
```

The explicit endpoint is an existing local Unix WebSocket control socket, selected
by the operator. Support this transport on Unix only; native Windows is explicitly
unsupported in this increment. Reject other schemes, userinfo, query, fragments,
relative paths and invalid UUIDs before connecting. Normalize the socket path;
do not scan for endpoints, read credentials, launch a daemon, start/resume a thread,
change settings or create a new agent. Repository/session/message fields cannot
select the endpoint. Help and option validation have no host/authority effects.

Use the installed Codex 0.163 schema: initialize/initialized then metadata-only
thread/read with includeTurns=false. Restrict outbound methods to those three;
never answer host command/approval requests. Read only the requested thread's
identity/status. Discard unrelated notifications/content without logging them.
Use the existing x/net WebSocket implementation rather than a handwritten framing
stack; promoting the existing module to a direct requirement is permitted, without
changing its version. Bound frames/responses to 1 MiB and unrelated messages to
128 per request. Validate request IDs, requested thread ID and the status enum;
cap fixed diagnostics and exclude raw host text, homes, paths and conversation
content from error output. Close the connection on cancellation.

Supported observed states are idle, active, notLoaded and systemError. Return
only the recognized active flags waitingOnApproval/waitingOnUserInput; reject
malformed state/unknown flags as unsupported metadata. Output source is
host-observation, not server/local-authority. Data includes adapter, thread_id,
state, active_flags, observed_at (UTC RFC3339), and queue_state=unknown. No host
thread is automatically associated with a ClawBridge session. Operator mapping
is separate, as in the existing notify command.

host-status performs one observation and uses the same terminal envelope/exit
rules as host-wait. Its matched outcome means the status was observed, not that
a particular lifecycle predicate matched. A recognized systemError returns
unavailable with that last valid observation. host-wait is a finite level-triggered wait:
an already matching first observation succeeds immediately, otherwise poll the
same thread on one connection until a match/deadline. `idle` means observed host
status only, not queue drained, task accepted/completed or future inactivity.
`unloaded` matches notLoaded and does not mean process exited. An `exit` predicate
is rejected explicitly: the available evidence cannot prove it. Disconnect,
thread-not-found, malformed/unsupported response and systemError cannot count as
idle/unloaded success. Do not reconnect silently across possible host replacement.

Timeouts are 1–3,600 seconds; poll intervals 250ms–30s. Per-handshake/RPC deadline
is at most five seconds and the remaining overall deadline. Keep the observer
connection for one command only; release timers/socket on every outcome. The wait
emits exactly one source envelope with outcome matched, timeout, unavailable,
unsupported or cancelled, and last valid observation when available. A match
exits zero, other outcomes nonzero. Ordinary option errors go to stderr. No
observation is emitted as a bare object: data contains outcome and optional
last_observation, with the fields defined above. No
automatic message or model wake occurs on match; the caller consumes the result.
Do not block an agent waiting for its own active thread to become idle. No PID,
heartbeat, retirement or transport error substitutes for a host observation.

Discovery therefore has distinct evidence: list_sessions identifies registered
mailboxes and active routing eligibility; an explicit host-status can observe a
locally bound thread; other host availability is unknown. Neither stale presence
nor missing host access releases task ownership. This increment does not publish
private host endpoints/thread bindings through peer discovery or add MCP host
tools accepting arbitrary endpoint arguments.

## Storage and acceptance

Use transactional schema version 2 migrations for existing SQLite and PostgreSQL
stores: optional message metadata plus per-session policy, preserving IDs, retry
hashes, deliveries, audit and all current settings. Fresh stores reach version 2;
reopening is idempotent; newer schemas are refused. Existing binaries that refuse
version 2 cannot serve the upgraded store. Document backup, coordinated service
upgrade and restore-based rollback; do not run a live migration here.

Engineer publishes additive exported test seams for metadata, status and injected
host observation/clock/transport, runs focused checks, then supplies an exact
source/module freeze. Reviewer owns independent probes for migrations/old retries,
metadata validation/correlation/privacy, authorization/fencing, no-stamp status,
policy backlog/restart/uncertain outcomes, real in-memory MCP lifecycle, and fake
Unix WebSocket metadata/wait/error/cancellation limits. Test production CLI parsing
without host/authority effects. No fake signal may establish actual process exit.

After stable freeze, run uncached full/race/vet, real disposable PostgreSQL full
and race, relevant CGO0 cross-builds and manifest stability. Record actual skips
and remove only owned containers/volumes/outputs. Native host observations and
Windows runtime remain separately attributed from protocol fixtures. No duplicate
release gates or live SSH/model/queue/Linear operations are needed for this source
increment. Root reconciles current guides/skills and links after acceptance.

## Inspiration and scope

[Claude cross-session messaging](https://code.claude.com/docs/en/cross-session-messaging)
offers native host delivery, inbound controls and bounded idle notices.
[Agent teams](https://code.claude.com/docs/en/agent-teams) separates task dependencies,
mail and completion gates. Here those ideas map to durable ClawBridge mail,
explicit host observations and the existing Linear workflow. Revision comparison
is our extension; this contract does not claim Claude's native runtime guarantees.

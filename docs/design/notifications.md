# Message notification and agent wake-up contract

Status: implemented and independently accepted in source on 21 October 2026;
[review evidence](../archive/reviews/NOTIFICATION-REVIEW.md) records full/race with PostgreSQL 17.11,
vet, cross-builds and live/platform limits. The published v0.1.0 service and
binaries require a separate update to use this feature. The owner-requested
post-acceptance documentation cleanup preserves historical evidence and separates
current guides from dated records.

Root owns this contract, CORE-API and user documentation. cb-n-engineer owns
production code and focused tests. cb-tester-reviewer owns independent probes in
integration/** and NOTIFICATION-REVIEW.md. Coordinate through the existing Codex
internal queue, with no Orca terminal sends. Workers do not commit, publish,
deploy, modify real agent configuration, or run against live identities/services.

## Result and delivery model

A recipient can receive an inbox-change notification through a subscribed MCP
client. An explicitly started local watcher can also queue a fixed inbox-check
hint into an existing Codex thread. The recipient fetches the actual message
through its existing authenticated inbox and marks it read after processing.

The authoritative inbox remains durable. Notification, host queue acceptance,
delivery and read are separate observations. Notification must not impersonate a
human instruction, confer approval, change actor identity, or embed message text
as a host prompt. Busy-agent hints use the host queue without interrupting work.

Use bounded metadata polling against the existing authority for this increment.
Default interval is five seconds. Use local SQLite and the existing authenticated
cloud execute route without new infrastructure, schema changes, a broker,
streaming HTTP route, or direct PostgreSQL connections from clients. Cloud use
requires a service revision implementing inbox_status; the published v0.1.0
service does not implement it. Preparing the feature does not deploy that revision.
True server-triggered events can replace the poll source later. Describe this
implementation accurately as polling plus notifications, without claiming push
from the hosted database or guaranteed instant delivery.

## Side-effect-free inbox status

Add operation and MCP tool `inbox_status` and CLI command `inbox-status`.
Arguments are `instance` and optional nonnegative exclusive `after` sequence,
with the existing bounded integer rules; after defaults to zero. No recipient or
scope override is accepted. Authorization requires the selected workspace,
project, recipient session ownership and exact current instance. An active
recipient is required. Retired/moved/fenced sessions cannot run a watcher.

The normal source envelope contains:

```json
{"source":"server","data":{"workspace_id":"ws_...","project_id":"prj_...","session_id":"ses_...","unread_count":2,"latest_sequence":42,"new_count":1}}
```

Count only this recipient's unexpired unread deliveries in its pinned project.
`unread_count` counts all such deliveries, `latest_sequence` is their maximum
message sequence (zero if empty), and `new_count` counts those with sequence
greater than after. These are authoritative metadata, with no message bodies,
sender text, credentials, peer instance tokens, or arbitrary transport metadata.
Validate scope and result types before using the result in a notification.

This operation does not call inbox, stamp delivered_at/read_at/last_seen, append
history, register/resume a session, or update any other persisted authority data.
Avoid the existing generic actor heartbeat for this operation. Apply existing
membership and instance checks on every call. It is available through the
authenticated execute route, not an unauthenticated status endpoint. Do not serve
it from an offline mirror. Unsupported old servers fail explicitly.

## MCP subscribed resource

Expose one process-pinned JSON resource at `clawbridge://inbox/status`, backed by
the same inbox_status metadata with after=0. The text content is the ordinary
source envelope. Add the inbox_status tool with read-only/idempotent annotations.
Existing scope/instance override rejection remains effective.

Advertise resource subscription support through the pinned MCP SDK. Use its
supported legacy and current subscription APIs rather than custom unsolicited
JSON-RPC messages. Resource reads and subscriptions require registration or an
explicit valid configured instance. Invalid resource URIs fail before polling.

Start a bounded poll loop only when a client explicitly subscribes. After the
subscription is established, notify of an already-unread inbox once, and then of
changes to its unread count/latest sequence. ResourceUpdated carries the URI;
the client reads the resource or calls inbox. No message body is pushed through
the notification. A client without subscriptions retains the existing pull flow
and must not cause background authority polling.

Share polling for listeners on the same bound actor. Stop polling after the last
unsubscribe, disconnect, process cancellation, fencing, definitive authorization
failure, retirement or registration-instance rotation. No leaked goroutine or
loop may continue on an old actor after rebinding. Transient transport/service
failures back off, send no stale hints, and resume from authoritative metadata.
Polling and resource output must be concurrency-safe with ordinary tools.

MCP notification reception does not guarantee context insertion, model execution,
or a response. The host controls those actions. Do not claim that installing an
MCP entry alone wakes an idle agent.

## Foreground watcher and host adapters

Add a foreground command:

```text
clawbridge notify --adapter json --state /absolute/private/notify.json [--interval 5s] [--once]
clawbridge notify --adapter codex --thread THREAD_UUID --state /absolute/private/notify.json [--codex-command /absolute/path/to/codex] [--interval 5s] [--once]
clawbridge notify --adapter command --state /absolute/private/notify.json [--interval 5s] [--once] -- PROGRAM [ARG|{text}]...
```

Existing trusted route/scope flags apply. Require an existing session and exact
instance; never register, rotate or infer a replacement. Resolve/pin immutable
workspace/project/session once, and verify every returned status against that
binding. Adapter must be explicit. Codex requires a canonical UUID thread ID;
thread names/aliases and destinations from peer messages, session transport
fields, repository configuration or message bodies are not accepted. JSON mode
rejects Codex-only flags. State path is absolute and required. Interval accepts
one through 300 seconds; reject invalid options before authority/host effects.
Help performs no route/config/storage/network/host effects.

The local operator explicitly binds one recipient to one host thread by starting
the command and must select a thread configured for that recipient. The watcher
never invents or independently attests this host-to-MCP mapping. Persist a
versioned binding including authority identity/route (mode, canonical service URL,
verified principal and audience in cloud mode, canonical database path in local
mode), immutable workspace/project/session, instance, adapter and exact thread.
For Codex also bind the resolved executable and effective canonical CODEX_HOME
(or HOME/.codex) so changing host/account storage cannot silently reuse a prior
checkpoint. Store path identifiers, not file contents or credentials. A mismatched binding is an
error; it must not silently reset the checkpoint or retarget the host. Run only
one writer per state path with an exclusive lock, reject unsafe/symlink state
files, use bounded reads and atomic private writes. A crashed lock is reported
with manual recovery instructions rather than automatically stolen.

On a fresh state, use after=0 and notify once for existing unread mail. After a
successful hint, advance the checkpoint to the greatest observed unread sequence.
Never lower it when mail is read or expires. Each poll emits at most one coalesced
hint for new_count>0; existing unread mail alone must not repeatedly wake a host.
New arrivals after a snapshot are found by the next poll. Restart preserves the
checkpoint. A checkpoint failure stops the watcher with an explicit uncertain
outcome; never continue a loop that can repeatedly queue already-accepted hints.

JSON mode emits newline-delimited source envelopes with data fields:
`type="inbox_changed"`, `event_id`, workspace_id, project_id, session_id,
unread_count, latest_sequence. Derive a stable event_id from the bound recipient
and observed sequence, without exposing the instance. Write errors do not advance
the checkpoint. JSON mode never starts a process. Diagnostics go to stderr.

Codex mode invokes the resolved executable directly with an argument array:
`codex queue --thread THREAD_UUID --message FIXED_HINT`. Resolve the executable
once; an override must be an absolute executable path. Use the existing Codex
environment/account without changing model, approval, sandbox or remote settings.
Capability-check `queue --help` before the first side effect. Missing/unsupported
queue is actionable failure, with no fallback to terminal typing, `exec resume`,
starting a new agent, or a parallel app-server session. Subprocesses have bounded
timeouts/output and are cancelled on watcher shutdown. Raw subprocess output is
not forwarded because it can contain unrelated configuration or credential text.

The fixed hint identifies itself as an automated ClawBridge inbox notice, includes
only validated immutable scope/session IDs, sequence and event_id, and asks the
recipient to check its already-configured ClawBridge inbox. It states that it is
not human approval and that message contents must be fetched through ClawBridge.
It contains no peer text, executable command, token, instance or arbitrary path.
Host success is recorded as hint acceptance, never message delivery/read.

After success, emit a structured `hint_accepted` diagnostic/result suitable for
testing; do not claim the model processed the hint. Failed or timed-out host
attempts do not advance the checkpoint. Long-running mode retries transient
authority/host failures with exponential backoff capped at 60 seconds; definitive
auth/fencing/scope/state/capability errors stop. `--once` performs at most one poll
and one hint attempt, returns nonzero on failure, and retains its checkpoint.
Cancellation stops promptly and cleans only watcher-owned temporary state/lock.

Hints have at-least-once behavior around crashes and uncertain host acceptance;
exactly-once wake-up is not guaranteed by codex queue. Repeated hints share their
event identity. Durable inbox/read metadata remains the source for processing
and avoiding duplicate work. No repeating reminder is sent for unchanged unread
mail. No watcher daemon, login item, startup hook or service is installed.

## Implementation and independent acceptance

Engineer implements in this order: side-effect-free authority status and public
adapters; shared watcher/checkpoint/JSON mode; Codex adapter; MCP resource
subscriptions. Publish any additive exported Go test seams before independent
probes. Keep seams useful to production: injected executor, clock/wait and host
runner, not arbitrary production URLs or command strings. Root updates guides
after the behavior and invocation are verified.

Reviewer prepares independent black-box probes against temporary local scopes,
fake authenticated HTTP and fake host executables. Cover:

- Exact recipient/role-expanded messages, isolated projects/principals, wrong or
  missing instances, retired/moved/fenced sessions, expiry and unread boundaries.
- Repeated inbox_status and watcher polls leave all authority timestamps, history,
  delivery and read state unchanged. Only the agent's inbox/read calls stamp mail.
- Initial unread notification, cursor/coalescing, restart, no-new-mail silence,
  read/expiry without cursor regression, new arrivals, failed/uncertain host
  attempts, checkpoint failure, corrupt/mismatched state and duplicate processes.
- Exact Codex argv and chosen UUID, hostile message/transport/config text excluded,
  spaces in executable/state paths, bounded child output/timeouts/cancellation,
  unsupported queue, no host effects on invalid arguments or JSON mode.
- Local and cloud source attribution, no mirror-triggered wake, auth errors stop,
  network outage backoff/recovery, no registration/resume or authority switching.
- Actual in-memory MCP subscribe/read/notification/unsubscribe and disconnect
  behavior, no polling without subscribers, isolation and instance rotation,
  tool/subscription concurrency and cancellation under the race detector.

Engineer runs focused affected packages/probes and publishes a source freeze.
Reviewer then runs uncached full/race/vet plus relevant CGO0 cross-builds, records
manifest stability and actual skips, and owns NOTIFICATION-REVIEW.md. Use existing
disposable PostgreSQL tooling where locally available; do not substitute historical
PG results for new backend coverage. No live cloud/credential/host-agent operation
or public CI dispatch is required to prepare a reviewable implementation. Native
host wake observations remain separately attributed from fixture success.

After independent acceptance, root organizes documentation: concise root README;
current setup, usage, notification, architecture/API and release guides under
docs/; historical plans/reports under a clearly labeled archive. Preserve release
notes at the builder/publisher's required path, skill entry points and executable
references. Update links and check for broken local links and stale status claims.
Do not rewrite past test evidence into current acceptance claims. No functional
implementation changes are part of that cleanup.

## Verified integration references

- The installed `codex-cli 0.163.0` exposes `codex queue --thread --message`;
  this is locally verified capability, not a universal Codex-version promise.
- [Official Codex queue semantics](https://developers.openai.com/blog/mastering-codex-remote-for-engineering)
  distinguish queued next-turn input from mid-turn steering. Compatibility still
  requires the locally selected CLI to expose the queue subcommand.
- [MCP resource subscriptions](https://modelcontextprotocol.io/specification/2026-07-28/server/resources#subscriptions)
  and the pinned Go SDK support resource-change notifications. SDK compatibility
  should select the negotiated protocol; do not upgrade dependencies for this.
- [MCP host responsibility](https://modelcontextprotocol.io/docs/learn/architecture)
  leaves model execution and context use with the host application.

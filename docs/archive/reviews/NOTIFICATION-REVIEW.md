# Notification / wake delivery independent review

Status: **PASS — independent frozen local/synthetic acceptance.** Read NOTIFICATION-DESIGN.md and CORE-API.md's “Inbox notification extension” on 21 October 2026. This report records feasibility, acceptance criteria and focused synthetic evidence. Independent focused probes are now authorized and started; no production edits, live queue messages, real credential/config operations or old release reruns have been performed. Engineer owns production code; reviewer owns this report and future integration probes.

## Scope and evidence boundaries

Authoritative inbox_status metadata feeds MCP resource invalidations and an explicitly foreground JSON/Codex watcher. A notification, queue acceptance, model turn, inbox delivery and message read are distinct observations. Fake protocol/host fixtures do not prove actual client wake behavior. Message bodies, labels and transport metadata are untrusted and cannot select a thread, executable, authority or permission.

The published contract resolves the initial policy questions: active owned current-instance recipients only; status has no persisted authority mutations; no offline mirror; MCP initially notifies unread backlog then changes to count/latest sequence; foreground watchers emit only when new_count>0 and retain a monotonic checkpoint. Queue timeout/crash can duplicate a hint; stable event IDs do not provide sink deduplication or exactly-once wake.

Cloud notification requires an upgraded service implementing inbox_status. The deployed v0.1.0 service does not support it. Unsupported operation must fail explicitly; an ordinary inbox fetch is never a compatibility fallback. Deployment remains separately authorized.

## Acceptance matrix

Automated gates below passed on the final combined freeze; detailed commands/results and explicit live/platform limits are recorded in the final acceptance section. Historical pre-freeze observations and holds are retained below for attribution.

| Gate | Independent acceptance criteria |
| --- | --- |
| True no-stamp read | Through Store.Execute/ExecuteAs, repeated empty/nonempty inbox_status calls preserve delivered_at, read_at, last_seen, updated_at, session status/instance and complete audit history. Compare raw disposable database state before/after requests, avoiding observer calls that themselves heartbeat. No registration, resume, retention cleanup or other persisted authority mutation. |
| Recipient authorization | Validate workspace/project membership, owner principal and exact active instance on every call. Missing/wrong instance, retired/moved/fenced actors fail; routing lead confers no rights. Isolate identical lead/session names across projects and identical project slugs across workspaces; test multiple principals concurrently. |
| Exact status | Ordinary local-authority/server envelope; only workspace_id/project_id/session_id/unread_count/latest_sequence/new_count data. after defaults to zero, is exclusive and bounded/nonnegative. Count only bound recipient unread, unexpired deliveries using one authority-time snapshot. Maximum is zero when empty. No bodies, peer instances, transport, credentials or caller timestamps. |
| Aggregate boundaries | Direct and role-expanded deliveries, unread/read, delivery-vs-read distinction, expiry boundary and concurrent sends/reads produce coherent counts. Count/max can decrease while the durable checkpoint cannot. Invalid count/sequence/scope/source results fail before producing hints. |
| Public adapters and compatibility | CLI inbox-status, MCP tool annotations and authenticated HTTP whitelist expose the operation. Body scope override rejection retained. Existing inbox still stamps delivery and explicit read still stamps read; other delivery/history/presence contracts remain intact. Old servers fail explicitly. |
| Authority-only behavior | mirror never serves status or drives hints. Credential failures and 401/403/fencing stop rather than use stale data. Transport outage backs off without stale notices, recovery reads authority. Local/cloud attribution and immutable trusted routing remain honest. |
| MCP shape and subscription ordering | Exact JSON resource clawbridge://inbox/status with after=0, normal source envelope and negotiated SDK subscription support. Reads/subscriptions require valid captured instance; invalid URI fails before polling. Since SubscribeHandler precedes SDK subscription registration, initial backlog notice must occur only after registration, not be lost inside the callback. Test actual in-memory protocol delivery, not callback invocation alone. |
| MCP lifetime and generation | Zero subscribers means zero background authority calls. Initial unread notification once, then count/latest tuple changes; notices contain only URI. Shared actor poller, duplicate subscribe/unsubscribe, multiple connections and last-listener removal behave correctly. Disconnect/cancel/retirement/auth failure/registration rotation stop polling; delayed old-generation results never publish. Race tools against subscription lifecycle and resume. |
| CLI validation and help | Explicit adapter, absolute required state path, interval 1–300s, canonical UUID for Codex and absolute executable override. JSON rejects Codex-only flags. Reject invalid inputs before authority/host effects; help performs no config/state/network/host work. Existing route resolves once; never register/rotate/infer successor. |
| Binding and private state | Versioned state binds canonical authority/URL/principal/audience or DB path, immutable scope, instance, adapter/thread, resolved Codex executable and effective canonical CODEX_HOME. No credentials/content. Mismatch/corruption/unknown version/unsafe symlink refuses without replacement. Bound reads, exclusive lock, atomic private save, concurrent writers and manual stale-lock recovery; preserve unrelated files. |
| Coalescing and restart | Fresh after=0 emits once for existing unread backlog. At most one hint per snapshot for new_count>0. Advance only after successful sink acceptance; no checkpoint regression on read/expiry and no repeat reminder for unchanged unread. New arrival during dispatch/save remains discoverable next poll. Restart retains checkpoint and event identity. |
| Output and checkpoint failures | JSON partial/write failure and definite host failure do not checkpoint. Host timeout has uncertain acceptance and possible duplicate with same event ID. Accepted hint plus checkpoint-save failure stops with explicit uncertain outcome, rather than repeatedly submitting. Test save/rename/permission failures with isolated fixtures. |
| JSON sink | NDJSON normal source envelope with inbox_changed/event_id and validated scope/count/sequence fields. No instance in event ID/output. JSON mode starts no child. Errors go to stderr; bounded sanitized diagnostics never contain body/token/SQL/DSN. |
| Codex host boundary | Resolve executable once; capability-check queue --help; exact direct argv queue --thread UUID --message FIXED_HINT, no shell. Bind effective host storage to prevent checkpoint reuse across account/home changes. Fixed automated hint uses only validated IDs/sequence/event ID, denies human approval and directs durable inbox fetch. No peer body, command, path, token or instance. Use fake executable only; never real-send. |
| Host failure and lifecycle | Unsupported queue, missing/blocked/nonzero/noisy child, cancellation and timeout remain bounded and never expose raw subprocess output. Exit success means hint_accepted only. At most one poll/attempt in --once; loop exponential retry capped at 60s, definitive failures stop. Cancel cleans only owned lock/temp files. No service/daemon/PATH/config/credential install. |
| Final gates | After published additive seams and explicit engineer freeze: focused black-box/local/fake-HTTP/fake-host/raw MCP tests, uncached full/race/vet/relevant CGO0 cross builds, actual disposable PostgreSQL coverage for the new backend path and cleanup evidence. Record stable source fingerprints and actual skips; historical PG coverage is not new notification evidence. |

## Read-only feasibility observations

Existing inbox mutates delivery.delivered_at and audit history; generic execute actor paths call touchReadActor before dispatch. inbox_status therefore needs a separate authorized path before that touch and must never reuse inbox/whoami as a polling shortcut. Existing PostgreSQL snapshot handling offers a read-only transaction pattern. A no-stamp request guarantee is distinct from bus.Open startup initialization/migrations; tests must take their before-state after opening/initializing a disposable store.

The strict server operation/argument whitelist and mirror's separate readable-operation whitelist require deliberate treatment. Supporting arbitrary operation names in the remote transport alone does not make an old server compatible. Published CORE/design now explicitly state upgrade requirements and no inbox-stamping fallback; no rollout contradiction remains in the reread.

Pinned MCP SDK v1.8.0 supports resources and subscription callbacks, but its SubscribeHandler runs before it records the subscription. Publishing an immediate notice inside that callback risks losing initial backlog notification. Binding mutex/generation safety also needs to cover in-flight polls when register/resume rotates the captured instance. The contract requires post-establishment notification and no old-generation publication; independent protocol probes must test both.

The inspected Codex queue interface has explicit thread/message arguments but no idempotency key or durable receipt lookup. A subprocess exit/queue receipt cannot establish actual turn start or mail delivery/read. The published at-least-once/uncertain-outcome wording is appropriate. A peer's session.transport, message body or ambient latest thread is never a valid adapter binding. The operator selects a thread already configured for the recipient; the watcher neither discovers nor attests its MCP mapping. Resolved executable and effective canonical CODEX_HOME (or HOME/.codex) are explicit checkpoint-binding components, so changing host storage/executable must fail as a mismatch rather than reuse acknowledged progress.

No schema blocker identified: recipient delivery/message aggregates can supply count/MAX under the existing authorization boundary. This is feasibility only, not implementation acceptance. Exact exported production test seams, poll/child timeout bounds and checkpoint schema remain to be inspected as engineer publishes them. Independent probes are authorized after contract publication; final heavy gates wait explicit freeze.

## Pre-freeze independent probes

Added integration/notification_status_test.go and integration/notification_remote_test.go using only public Store/remote/mirror APIs and disposable fixture database inspection. Raw before/after comparison covers every scoped domain row, including history and presence, without observer heartbeat calls. Fixture-only SQL establishes old presence, expired mail and read state; no production timestamp injection. The raw observer resolves the workspace name to its immutable ID and requires captured session rows, preventing an empty comparison from passing vacuously. This latest rerun supersedes the initial probe-development result.

`go test ./integration -run '^TestNotification' -count=1 -v` — **PASS 0.887s**, pre-freeze:

- SQLite empty/nonempty repeated status and invalid request calls leave scoped persisted rows unchanged. Exact field set, unread/exclusive-after counts, other recipient/project/workspace isolation, expiry/read filtering, role-expanded recipient deliveries, exact canonical scope IDs, resumed-instance and retired/moved rejection pass.
- A populated mirror is independently proved readable for inbox first; status refuses mirror output for HTTP 400/401/403/500/502/503/504, credential failure and transport outage. Mirror bytes remain unchanged.
- PostgreSQL no-write aggregate and principal/role/scope/incarnation probes are present but **SKIP: test DSN absent**. This focused result is not actual PG acceptance.

At this initial status checkpoint watcher/MCP seams were still awaited. Subsequent seam-based evidence and pre-freeze observations follow; no heavy checks or live/host-agent operations.

## Watcher pre-freeze observations (implementation in progress)

Public seams announced by engineer: notify.Run/Config/Binding/Status/RunnerFunc and injected Wait/timeouts; NewServerWithOptions for in-memory MCP. Initial independent watcher subset passes: canonical-ID restart pinning, coalescing/no reminders after read/expiry, output-failure retry event identity, binding/corrupt/oversized/symlink/stale-lock refusals, one-writer exclusion and exponential backoff capped at 60s. `go test ./integration -run '^TestNotification(JSON|Output|Binding|Backoff|Concurrent)' -count=1 -v` PASS 0.788s.

### N-O1 — host binding missing (Corrected; pre-freeze retest Pass)

Owner: engineer. Exact repro: `go test ./integration -run '^TestNotificationCodexCheckpointHostBinding$' -count=1 -v`. Initialize private checkpoint through an injected fake host, then change only the absolute executable, or effective CODEX_HOME. Both second runs currently succeed and call authority/host again (calls=2, hosts=2), instead of rejecting a binding mismatch before those effects. Published design explicitly binds both host identifiers; existing instance/thread/scope fields alone are insufficient. No real Codex execution in this probe.

### N-O2 — null numeric metadata accepted (Corrected; pre-freeze retest Pass)

Owner: engineer. Exact repro: `go test ./integration -run '^TestNotificationStatusRejectsMalformedMetadata$' -count=1 -v`. ParseStatus accepts unread_count:null, latest_sequence:null or new_count:null when the other counts are zero; Go decoding silently retains zero. Contract requires validated numeric result types. All three malformed payloads have the exact six keys, so field-count validation does not reject them. Fractional/inconsistent counts, hostile ID text and offline-mirror source are also probed and rejected.

Combined `go test ./integration -run '^TestNotification(Codex|Accepted|StatusRejects)' -count=1 -v` FAIL 0.501s on N-O1/N-O2 only. Exact direct fake-host argv/UUID, fixed non-approval hint, no raw host output, stable ambiguous retry and no repeat submission after checkpoint pass. Accepted output followed by checkpoint replacement failure stops permanently with uncertain outcome and one emission. These are in-progress observations; final acceptance remains open pending engineer correction/readiness and explicit freeze.

### Subsequent focused protocol, CLI and lifecycle evidence

- `go test ./integration -run '^TestNotification(MCP|StatusRejects)' -count=1 -v` — **PASS 0.460s**. Actual in-memory SDK protocols 2025-11-25 and 2026-07-28 deliver initial unread and read-to-zero updates; discovery/read alone never start polling; unsubscribe/disconnect/resume cancel the old poller. Two clients share a poller and retain independent notice baselines; the later subscription does not repeat unchanged backlog to the first listener. N-O2's three null numeric regressions now reject after engineer correction. Native host/model execution is not exercised.
- MCP probe development initially assumed a first-interval delay and synchronous latest-protocol subscription error, and counted an already-published backlog notice as post-stop. These were fixture assumptions, not promoted findings. Current probes permit immediate post-registration publication, consume legitimate initial notices before testing shutdown and test both negotiated protocols. Invalid resource checks require no authority access; legacy subscribe and resource reads also require explicit error.
- `go test ./integration -run '^TestNotificationCLI' -count=1 -v` — **PASS 4.128s**. Actual isolated binary: help plus eight invalid-option cases run despite a malformed active config, preserve the sentinel and create no state/DB; inbox-status and exclusive after return exact metadata.
- `go test ./integration -run '^TestNotification(PollAndHostCancellation|CodexFakeSubprocess)' -count=1 -v` — **PASS 0.652s**. Injected polling/host cancellation returns promptly and removes owned locks; actual POSIX fake executable receives exact queue/UUID/message argv through default runner, while raw child output is suppressed and acceptance diagnostic is truthful. The fake executable never invokes Codex or another agent. Native Windows fake subprocess case explicitly skips its POSIX fixture.

At the historical checkpoint above N-O1 was Open; its later corrected retest is recorded below. Actual disposable PG, frozen combined focused/full/race/vet/cross and stable fingerprints remain Pending. No notification cloud rollout, real queue send, credential/native client or model-wake evidence claimed.

Latest expanded MCP-only rerun after adding explicit invalid-resource read rejection: `go test ./integration -run '^TestNotificationMCP' -count=1 -v` **PASS 0.454s**. This verifies the new read case rather than attributing it to the earlier protocol run. `git diff --check` passes.

## Task-board skill wording review

Read skills/clawbridge/SKILL.md's conditional Linear pointer and references/task-board.md independently. **Pass for the requested design-only wording scope:** coordinator-maintained canonical issue/session/scope assignment is authoritative; descriptive notes and role labels do not grant ownership or project rights. Candidate matching escalates ambiguous ownership rather than claiming a lock. Absent/contradictory/retired/moved bindings return to the coordinator; stale presence does not release ownership, and successors require explicit reassignment. Resumption rereads assignment and follows existing instance-fencing rules. Cross-project board links do not extend bus scope.

The reference distinguishes human assignee, delegate and runtime session, keeps host UUID/instance/executable routes out of the team assignment block, and treats send/read receipts as observations rather than acceptance. It uses connected Linear schemas and existing user authorization without promising a connector, atomic claims, new permissions or automatic installation. No wording blocker found. This is static workflow review only: no external Linear operations, implementation edits or additional backend tests. Notification implementation gates remain unchanged.

## Actual error-boundary regression (pre-freeze)

Added integration/notification_error_boundary_test.go. `go test ./integration -run '^TestNotificationRealDefinitiveErrors' -count=1 -v` — **PASS 0.961s** across 14 watcher/MCP subcases. Each begins with an authorized active authority, then revokes/fences between polls. Local cases use actual Store.Execute errors after resume, retirement and move. Remote cases use actual remote.Client error routes for injected token-source failure, malformed token output, HTTP 401 and malformed authoritative JSON; they do not inject notify.Permanent. No real credential lookup/Google request occurs.

Long-running watcher performs exactly one initial poll, one ordinary interval wait and one failing poll, then returns a permanent error without a retry wait. In-memory MCP performs subscription validation, initial poll and failing poll only; real returned errors classify as definitive, with no retry wait or hint observed. Protocol checks include a bounded 40ms negative retry observation; source review also confirms the permanent-error branch exits immediately.

Current source intentionally classifies local inbox_status errors in Store.Execute and remote credentialError implements Code()=unauthenticated. Remote status also classifies invalid non-transport metadata errors. The earlier risk is **not reproduced on these current in-progress bytes**. Initial MCP fixture incorrectly required the poller's Wait context to be cancelled on return; corrected it to observe real classification and absence of retry directly. That initial fixture failure is not a production finding.

N-O1 correction independently retested with `go test ./integration -run '^TestNotificationCodex' -count=1 -v` — **PASS 0.385s**. Changing executable or CODEX_HOME now refuses checkpoint reuse before authority/host calls; fixed argv/ambiguous retry/default fake subprocess still pass. N-O1 and N-O2 are corrected in pre-freeze evidence; final corrected frozen-tree acceptance remains Pending, including real disposable PG and final combined/race/vet/cross gates. `git diff --check` passes.

### N-O3 — missing/null checkpoint cursor silently resets (Corrected; pre-freeze retest Pass)

Owner: engineer. `go test ./integration -run '^TestNotificationCorruptCheckpointCursorRefused$' -count=1 -v` — **FAIL 0.543s** for missing/null cursor, while fractional/negative cursor cases pass. Repro: create a valid private checkpoint after an accepted sequence-7 hint, remove its after field or set it to JSON null, then restart unchanged. Run succeeds, polls with after=0, repeats the accepted hint and overwrites the damaged state. The typed decoder defaults a missing/null int64 to zero. Published contract says corrupt state must be rejected instead of silently resetting acknowledgement progress. Expected: refuse before authority/sink effects and preserve damaged bytes for inspection. This fixture uses only fake authority/JSON output and private test files. No production edits or real host operations.

### N-O4 — MCP inactive subscriptions accumulate session waiters (Corrected; pre-freeze retest Pass)

Owner: engineer. `go test ./integration -run '^TestNotificationMCPResubscribeDoesNotAccumulateWaiters$' -count=1 -v` — **FAIL 0.580s**. Through public MCP client APIs on one persistent in-memory legacy connection, subscribe/unsubscribe 20 times to the status resource. A public runtime goroutine-profile observation shows 20 retained inboxResource.subscribe session-wait observers relative to baseline. Polling itself stops, but each new subscription starts another req.Session.Wait goroutine that survives until disconnect. A single shared session-lifetime disconnect observer is allowed by the probe; unbounded accumulation per inactive subscription is not. Expected: lifecycle cleanup or one observer per connection rather than per reactivation. No leaked authority poll or live client operation is claimed.

N-O3 correction independently retested after persisted-state validation changed: `go test ./integration -run '^TestNotificationCorruptCheckpointCursorRefused$' -count=1 -v` — **PASS 0.448s**. Missing/null/fractional/negative cursors all refuse before authority/output and preserve damaged bytes. N-O4 remains Open pending observer-lifecycle correction; N-O1/N-O2/N-O3 are corrected pre-freeze. No duplicate N-O1 or heavy run performed for this update.

## Initial freeze reconciliation — HOLD

Explicit handoff freeze directory: /var/folders/h6/qh_thy6s4_n4ccxfqhc9gp080000gn/T/clawbridge-notification-freeze-l2ez1vn9. The three supplied artifact SHA256 values verify exactly (owned.sha256 b41af75d3a278437c9927347e5d7c857c833a4d1d8c05302b3777deb4c431260; go-module.sha256 abdf6d8098d20098682982e7fc23f7372ac7a5afa029c12a3df08524f3676b1b; evidence.json 13dd01f832a2bdcd46c54881142064990c18507c663ce4a7787ffbcfeb04e50f). However the current tree differs from its 19-file owned manifest in internal/mcpserver/server.go, internal/mcpserver/inbox_resource_test.go, internal/notify/checkpoint.go and internal/notify/watch_test.go. The complete 111-file manifest additionally predates current reviewer notification_mcp_test.go and notification_watch_test.go. These include later focused fixes/probes; do not attribute a final acceptance run to stale frozen bytes.

Observed HEAD: `27953b73fb9b35109d5edb642b3dd6d5c96a2b39`. Independent expected/current fingerprints and before-manifest are saved in `/var/folders/h6/qh_thy6s4_n4ccxfqhc9gp080000gn/T/cb-notification-final-osfycase` (pointer /tmp/cb-notification-final-path).

Historical N-O4 pre-correction rerun (the then-current tree) `go test ./integration -run '^TestNotificationMCPResubscribeDoesNotAccumulateWaiters$' -count=1 -v` **FAIL 0.317s**, retaining 20 waiter goroutines across 20 cycles. Engineer/root notified; revised freeze requested after correction. Docker info confirms Engine 29.8.2 available. No heavy acceptance or disposable PG lifecycle started while source/freeze differs. No production edits.

## Latest corrected lifecycle/mandatory-state retest

`go test ./integration -run '^TestNotification(MCPResubscribeDoesNotAccumulateWaiters|MCPIdempotentRegistrationPreservesSubscription|CorruptCheckpointCursorRefused|CheckpointMandatoryFields)$' -count=1 -v` — **PASS 0.524s** on the current corrected tree. N-O4 now retains at most one disconnect observer across 20 reactivations. N-O3 rejects missing/null version, binding, scope and after (all eight mandatory-field cases), as well as fractional/negative cursors, before authority/output and without overwriting damaged bytes.

Root's same-instance registration concern is **not reproduced on current bytes**: actual in-memory MCP subscription survives register_session returning its unchanged instance, and the client receives a subsequent metadata-change notification. Current handler checks changed instance before stopping subscriptions/advancing generation. No historical failing implementation claim is made for this new probe.

N-O1 through N-O4 are corrected in independent focused evidence. Initial freeze mismatch remains unresolved until engineer publishes current manifests including the final fixes and new reviewer probes. Heavy/actual PG gates have not started; no production/live changes by reviewer.

Revised N-O3-only freeze 6u3s7lt_ artifacts also verify their supplied SHA256 values, but still predate final MCP corrections: current internal/mcpserver/server.go, inbox_resource.go and inbox_resource_test.go differ from its owned list; complete manifest additionally predates the two latest reviewer probes. Expected/current hashes saved in the independent evidence directory as refreeze-check.json. No defect reopened: current corrected probes passed, but exact final manifest reconciliation is still Pending. Heavy gates remain held rather than attributing tests to superseded bytes.

## Final independent frozen acceptance — PASS

Accepted engineer freeze: `/var/folders/h6/qh_thy6s4_n4ccxfqhc9gp080000gn/T/clawbridge-notification-refreeze-9apbrqm4`, superseding both earlier freeze directories. Its evidence/manifest artifact digests match the handoff; **all 19 engineer-owned file hashes match current source**. Its complete 111-file manifest predates only the reviewer's final two additive probe updates. Reviewer captured a complete current 111-file manifest before gates and verified byte-for-byte equality afterward; no implementation mismatch or additional Go/module file.

HEAD during acceptance: `27953b73fb9b35109d5edb642b3dd6d5c96a2b39` plus the frozen uncommitted notification implementation/tests. Independent before/after manifest SHA256: `cebf1141de208dd61e75b154fd9eba38659a23a5a1205eb0aead6a3dfbc1c083`. Evidence directory: `/var/folders/h6/qh_thy6s4_n4ccxfqhc9gp080000gn/T/cb-notification-accepted-freeze-u_vcjnmy`; pointer `/tmp/cb-notification-final-path`. Logs: full.jsonl, race.jsonl, postgres-resources.md, vet/cross logs, cross-metadata.json and summary.json.

### Exact gates

| Command | Result |
| --- | --- |
| `bash integration/run-postgres.sh go test ./... -count=1 -json` | Pass; integration 83.917s; entire runner 102.730s. |
| `bash integration/run-postgres.sh go test -race ./... -count=1 -json` | Pass; integration 91.711s; entire runner 111.023s; no race report. |
| `go vet ./...` | Pass, 0.609s. |
| `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o TEMP/clawbridge-darwin-arm64 ./cmd/clawbridge` | Pass, 2.169s. |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o TEMP/clawbridge-linux-amd64 ./cmd/clawbridge` | Pass, 1.959s. |
| `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o TEMP/clawbridge-windows-amd64.exe ./cmd/clawbridge` | Pass, 2.107s. |
| `git diff --check` and complete before/after source comparison | Pass; all 111 current Go/module fingerprints stable. |

Cross-binary metadata independently confirms the intended OS/architecture and CGO_ENABLED=0. Binaries were not executed; all three temporary build outputs were removed afterward. Full and race each execute 108 named notification tests/subtests across integration and affected packages; both new PostgreSQL notification cases explicitly **Pass**, not Skip. Retained Phase 1–4, release and other integration regressions also pass as part of the required full suites; no separate historical release gate was rerun.

### Actual disposable PostgreSQL and cleanup

Docker Engine 29.8.2; both isolated runs use PostgreSQL **17.11** from postgres:17-alpine, random test-only password passed through environment, loopback-only random host port and a fresh anonymous volume. Only the runner's own resources were created/removed. No DSN/credential is checked in or printed in this report. Full and race ran sequentially, each cleaned before the next fixture.

| Run | Exact container ID | Loopback port | Exact anonymous volume |
| --- | --- | --- | --- |
| Full | `575c388be47b511725a93d9e31c439c6d218fbedc3d2cd0036f01cc51c5a805f` | `127.0.0.1:32803` | `1955f0bc414b2c3c9ef584590653ffad0ff4d33696a163a8f930fa396de828b3` |
| Race | `1e22a474adee669d7913d031551319d15c7e81b4eb96b1a4debd42ed3b26e48a` | `127.0.0.1:32804` | `bf5ae4c4b0c80d8fb34773e87c91844133d39abe32415a7b53a9545fba11185b` |

Runner EXIT traps report each container/volume removed. Independent `docker container inspect ID` and `docker volume inspect NAME` fail for each exact resource after its run, confirming all four are absent. New real-PG evidence covers no persisted writes including presence/history/delivery/read rows, scoped counts/expiry/after, roles/principals/membership revocation, project isolation and incarnation fencing. Historical PG results are not substituted.

### Manual review and finding closure

Read the complete new notify package, status dispatch/SQL and changed bus/CLI/remote/MCP integration. Reviewed immutable binding, source attribution, read-only authorization transaction path, checkpoint save ordering/permissions/locking/cleanup, fixed argv/hint boundary, transient versus definitive errors and subscription activation/generation/lifetime. No remaining reproduced implementation defect on accepted bytes.

- N-O1: resolved executable and canonical effective Codex storage are bound; restart mismatch rejects before authority/host.
- N-O2: malformed/null numeric metadata rejects before notification.
- N-O3: all mandatory persisted fields are non-null/required; missing/null cursor cannot rewind/replay; damaged bytes preserved.
- N-O4: one disconnect observer per session; repeated subscriptions do not accumulate waiters.
- Root's same-instance registration concern was not reproduced in the reviewer's corrected-tree probe; unchanged instance preserves subsequent notices, while explicit rotation stops old subscriptions. Engineer's earlier failing own test is separately attributed, not presented as a reviewer-observed failure.

The independent actual Store/remote error-boundary regressions, fake subprocess argv/output tests and real in-memory SDK tests on both protocol versions are included in both final passing suites. Queue exit success is called hint acceptance only; ambiguous failure/crash can duplicate stable event IDs. First selector resolution uses inbox_status, without bootstrap heartbeat calls.

### Explicit skips and remaining live/platform limits

Both final suites skip exactly the environment-gated `TestDeploymentLiveHTTPSContract`, `TestLiveGoogleDiscoveredClientsExchangeWithMatchingSecrets` and `TestLiveDeviceLoginDiscoveryReachesPollingNoGcloud`; no live fixture was requested/provided for these runs. cmd/clawbridge, internal/buildinfo and internal/mirror report no package-local test files; mirror behavior is exercised through integration. No real PostgreSQL notification test was skipped.

No real Codex thread message, model wake, native MCP host subscription, OS credential-store operation, consent, Google authentication or service rollout is claimed. Native Windows notification ACL/replace/runtime behavior is not established by cross-compilation or historical release PowerShell CI; it remains separately pending if required for delivery. Actual client resource-to-context/turn behavior is host-controlled. Operator-selected Codex thread-to-MCP scope is not independently attested by the watcher. The old v0.1.0 hosted service still needs a separately authorized upgrade for inbox_status; no deployment performed. Root's documentation cleanup remains a post-acceptance task; reviewer moved no owner files and made no implementation/staging/commit/publication changes.

Post-acceptance reconciliation: engineer's reconciled snapshot `/var/folders/h6/qh_thy6s4_n4ccxfqhc9gp080000gn/T/clawbridge-notification-reconciled-awiub9mw` verifies all supplied artifact digests; its 19 owned and 111 complete file hashes match current source exactly. Parsed complete set equals the independently saved before/after manifests used for the already-passing final gates. Evidence saved as reconciled-equivalence.json. This resolves the two newer reviewer-probe snapshot differences without production changes or duplicate tests. Final PASS and live/platform limits unchanged.

### N-O4 historical failure versus accepted hashes

Reconciled the supplied 18:59:15.638592–18:59:16.830353 UTC probe.json (directory clawbridge-notification-waiter-reconcile-1ikn8qki): engineer's unchanged before/after source `7b7c0d1fb12e17cba82300746757b1526675e070b91cd56d77d20b938b0ccfec` and reviewer probe `0ff97e65a1d3d1a8e1467785cb785a23a201dcb2027bb5c19e1385054a1825b6` pass 0.325s. These exactly equal the current files and independent final before/after acceptance manifests.

The earlier 0.317s failure is **historical, not a failure of those accepted hashes**. Snapshot captured immediately before that repro at cb-notification-final-osfycase/manifest-before.json records source `884815a8df6c82730b25357d0db800fa1875082e5884e507f9819a23e762a0a4` and earlier probe `69e13e92bf23dc95159569afdcade8d16ba58d06f148f54c6d20f533981ed93b`. That source still created an observer per reactivation. Current source retains the observed session entry across unsubscribe and removes it on disconnect. Reviewer's subsequent corrected lifecycle selection passed 0.524s; final PostgreSQL full/race suites also pass the exact same current probe. This reconciliation required reads/report clarification only, with no new tests, heavy operations or source changes. N-O4 remains closed and final scoped PASS unchanged.

### Subsequent root-authorized real-host pilot — 21 October 2026

**Pass for an isolated, operator-prepared local Codex CLI mapping on macOS arm64.** This supplements the dated automated acceptance above. Root built the pilot from commit `9e7ad14f8f76c29a1cdce7d307533986b242e8f5`; independent review verified the exact committed source archive, all 111 accepted Go/module files, binary hash and native CGO0 metadata without rebuilding or executing it.

The existing recipient had completed its bootstrap turn before the notice. Queue acceptance at 20:57:54 UTC is a separate observation from the exact event appearing in its next turn at 20:58:02 UTC. Narrow host transcript verification, saved command results and read-only checks of the two known durable message records agree: the agent fetched the nonce-bearing request, replied on its exact message/thread, and marked it read; the sender then received and marked the matching reply read.

Both status/queue watcher runs leave all six saved authority tables identical, including presence/history/delivery/read. Initial checkpoint 1 is unchanged through the silent immediate restart and silent post-read restart. Fuller independent reconciliation is retained privately as independent-live-review.json in the pilot evidence directory. Private host routing identifiers are omitted here.

This demonstrates actual queue-triggered processing through a retained explicit CLI mapping. It does not establish native MCP-resource wake, automatic thread-to-MCP mapping, another host/platform, Windows notification behavior, Google/cloud execution, hosted rollout or exactly-once delivery. Earlier no-live claims describe their original checkpoint and remain historical. Reviewer sent no pilot messages, fetched no application inbox, accessed no credentials and ran no new tests.

### Local v0.2.0 candidate — PREPARED / UNPUBLISHED

Independent static review passes for exact source `25c43c65cbc5567a54f3a9c187481f3690637602`: all nine expected local assets, strict eight-payload checksum manifest, full source marker, tagged installers and separate tagged notes match. All five binaries have the correct format/architecture, CGO0/trimpath, clean VCS revision and embedded version/commit/build-time values. The clean clone's annotated tag is temporary only; no v0.2.0 tag exists in the real local repository. All 111 accepted Go/module files are unchanged; every regular service-source archive member matches the committed tree.

Root-operated macOS arm64 `version` and `--version` smoke records return the exact candidate metadata and report preserved active poisoned config. Reviewer confirmed the saved outputs and current malformed fixture state, without executing the application or independently observing its original environment. Fuller evidence: independent-candidate-review.json in the candidate temp directory. No rebuild, download or tests performed.

This is local preparation, not public CI/release/install-route acceptance or deployment. Native Windows notification runtime, native MCP-resource-to-model behavior and cloud notification remain unverified. Linear execution awaits owner-selected team/issue; no board operations performed. Prior automated and real-host evidence retains its separate scope.

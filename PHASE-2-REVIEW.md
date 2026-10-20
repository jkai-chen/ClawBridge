# Phase 2 independent review

Status: **Fail — one open shipped-documentation defect; Phase 2 implementation
checks pass.** Reviewer: cb-tester-reviewer. Final checkpoint: 20 October 2026.
Baseline main 4c66800. Phase 1 REVIEW.md remains historical and unchanged.

Reviewer owns `integration/**`, this report, the disposable Postgres lifecycle and
final full/race/vet/cross-build checks. All five defects independently found in
Phase 2 were fixed by their owners and independently reproduced as passing.
No implementation files, real user configurations or credentials were edited or
used. No cloud resource, IAM grant, deployment, paid service or agent model request
was made.

## Coordination / checkpoints

Initial COORDINATION.md and CORE-API.md described Phase 1; PHASE-2-PLAN.md,
the final shared contract and owner checkpoints were read as they appeared.
Docker server 29.8.2 was available. Existing container
`69e6bdeeb1373090b7dcc8fdd66b3f0a1d046d559d83796506ef4000ae68e07e`
(`fj-db-test-6703-770266`) was unrelated, was not modified, and remains running.
Every reviewer PG run used its own fresh `postgres:17-alpine` container, random
loopback port, anonymous volume and environment-only random test password. The
cleanup-trapped runner recorded and removed those exact resources. Passwords and
DSNs were never checked in or written to this report.

## Checks

| Check | State | Evidence |
| --- | --- | --- |
| Real Postgres integration | Pass | Actual PostgreSQL 17.11; required-env runner; no PG test skipped |
| HTTP auth/application route tests | Pass | Application routes with fake identity separately cover request boundaries; official Google library plus synthetic signing keys cover verifier failure modes |
| Remote + server roundtrip | Pass | Real HTTP handler + real PG + scoped sync/outage/denial; synthetic token/verifier fixtures |
| Mirror sync/offline reads | Pass | Exact binding, atomic publication, freshness, read-only file proof, no delivery/read stamps or queued writes |
| Full test/race/vet/cross builds | Pass | Final uncached full and race suites, vet, and three CGO-disabled builds all pass |
| Cleanup | Pass | Both final runner containers and their anonymous volumes confirmed absent |

## Acceptance matrix

| Area | State | Required evidence |
| --- | --- | --- |
| Bootstrap | Pass | Server mode explicit; only configured bootstrap owner can initialize |
| Authorization | Pass | owner/lead/member/reader and no-member checks; routing role grants no authority; denied changes have no audit effect |
| Session identity | Pass | Principal + instance ownership; resume fences old incarnations; explicit peer mutation audits the actual actor |
| Isolation | Pass | Duplicate names/slugs across projects/workspaces, foreign message IDs and concurrent principal state remain isolated |
| Transactions/replay | Pass | Concurrent identical sends produce one logical send; mismatched replay refused; injected fanout failure rolls back fully |
| Snapshot privacy | Pass | Only bound session's received bodies and own sent metadata; no other inbox/body; no delivery/read stamping |
| HTTP Google identity | Pass | Missing/bad/wrong-audience/wrong-issuer/expired/unsigned and malformed tokens fail without execution; production path cannot disable verification |
| HTTP input/error limits | Pass | Principal overrides rejected; request body and execution time bounded; response does not reflect tokens, SQL, DSNs or backend secrets |
| Remote token/URL boundary | Pass | Explicit account/audience/impersonation invocation; HTTPS or loopback HTTP only; redirects not followed; client email is not authority |
| Offline fallback | Pass | Only matching server/scope/principal/session/instance reads; freshness and `offline-mirror` source; writes refused; 401/403 never fall back |
| Sync atomicity | Pass | Invalid or mismatched sync preserves the previous complete mirror and cannot mix authority or scope |
| Stable pagination/state | Pass | Stable cursors avoid skips/duplicates; delivered and read are distinct; `thread_id` and immediate `reply_to` are distinct |
| Packaged agent guidance | Fail | Current skill still labels cloud/offline mirror as future Phase 1 work |
| Local compatibility | Pass | All Phase 1 tests remain in the final full and race runs |

## Reproduced defects

P2-R1 through P2-R5 below are retained as historical exact reproductions. Each
was reproduced on the current source, reported to its owner, fixed without removing
the affected capability, and independently retested. No reproduced defect remains
open.

Postgres test run: container `22513629c88310f31bbe055aff267a2a982ba270df7c57e76fca7b469be71ab0`; loopback `127.0.0.1:32838`; anonymous volume `217ef9d518a19defd71fa1e460c02e378221ef6d5fdbd85e560f21ad50f75eb2`.

Postgres test run: container `d597fb37c7a0fb46bc1bab3e9a9d08fa15c77c8f485bd70d90ec3344aff9aabf`; loopback `127.0.0.1:32839`; anonymous volume `16e22cb68cb8be7ef5754e15607ae8039a80c19804548f405ad0e83adfb2dded`.
Cleanup: container `d597fb37c7a0fb46bc1bab3e9a9d08fa15c77c8f485bd70d90ec3344aff9aabf` removed with anonymous volume.

## Checkpoint 2

PHASE-2-PLAN.md and CORE-API Phase 2 additions now read. Three independent PG
scenarios written for bootstrap/roles/peer audit, snapshot privacy/project IDs,
and concurrent mixed-principal sends plus identical retries. Server tests separate
fake application-route checks from the production GoogleVerifier + official library
using synthetic RSA-signed JWT/JWKS fixtures (certificate fetch intercepted locally;
no Google-issued credentials and no live Google-auth claim).

First actual runner smoke succeeded and container/volume were removed; subsequent
`docker volume inspect` confirms the recorded anonymous volume is absent. Second
runner attempted PG tests but compilation stopped at missing pgx puddle go.sum;
cleanup trap still removed its container and volume. This is **not** counted as a
successful real PG test run. Dependencies remain engineer-owned.

Postgres test run: container `435c34610036e7e7e307206343cc615b39ec194f9f3719d2cc9effc2392ade4b`; loopback `127.0.0.1:32840`; anonymous volume `e1837f319eba63c0cf76f0976b6b80d817daa4f56a03ba1095ce3e0e04c5462d`.

## P2-R1 — Fail: malformed ES256 token panics production verifier

Owner: cb-designer (server.GoogleVerifier).
Exact repro: `go test ./integration -run TestServerGoogleVerifierSyntheticCrypto -count=1 -v`.

Production NewGoogleVerifier uses the official Google validator. Test supplies
locally generated RSA and P-256 public keys through an intercepted certificate
fetch at the library's expected Google JWKS URLs. Valid RSA-signed fixture passes;
wrong audience/issuer/expiry/unverified-email/missing-subject and none/HS256 fail.
An ES256 header with the matching P-256 key ID but a one-byte decoded signature
panics: `runtime error: slice bounds out of range [:0] with capacity 1`.

Expected: reject every malformed token normally, HTTP 401, no application execution.
This is cryptographic-library/synthetic-key evidence, not live Google authentication.
Please defensively validate malformed signatures or otherwise ensure verifier errors
cannot panic the handler. Reviewer retains exact regression; no implementation edits.
Cleanup: container `435c34610036e7e7e307206343cc615b39ec194f9f3719d2cc9effc2392ade4b` removed with anonymous volume.

Postgres test run: container `9659bdf168969cdc5f3bedcc9df1f5d878b14a1251022cd062089bb7d8583903`; loopback `127.0.0.1:32841`; anonymous volume `3f50bb53afe9eb8349aaea2d7da54f26b843386e8dccce7ae29339b6675fa868`.
Cleanup: container `9659bdf168969cdc5f3bedcc9df1f5d878b14a1251022cd062089bb7d8583903` removed with anonymous volume.

Postgres test run: container `116ea35f52765cf2240d480144cfae3bb5c5d742e7d2b0fe0db5ea880589e78d`; loopback `127.0.0.1:32842`; anonymous volume `851ffe06b7d1108c63ba8e322fb04a05ec8667cd681052004104c6cff87132ff`.
Cleanup: container `116ea35f52765cf2240d480144cfae3bb5c5d742e7d2b0fe0db5ea880589e78d` removed with anonymous volume.

## Checkpoint 3 — real PostgreSQL tests Pass

Real PostgreSQL **17.11** now exercised through the disposable 17-alpine image:
`integration/run-postgres.sh go test ./integration -run TestPostgres -count=1 -v`
PASS (2.074s). All three Store scenarios ran; none skipped. Bootstrap/role/reader/
peer-actor audit, snapshot privacy/no stamps, cross-project IDs, 32 mixed-principal
concurrent sends and eight identical retry contenders passed.

One earlier OpenPostgres-unavailable attempt was a runner readiness defect: Unix
socket pg_isready saw the image's temporary initialization server before final TCP
startup. Runner now waits on 127.0.0.1 TCP. This was corrected in reviewer-owned
runner, not reported as an implementation defect. All attempted-run containers and
anonymous volumes have been cleaned up; original unrelated container remains.

## P2-R2 — Fail: authorization denials lose forbidden HTTP semantics

Owners: cb-engineer (coded core authorization errors) + cb-designer (HTTP mapping).
Exact repro: `integration/run-postgres.sh go test ./integration -run TestPostgresHTTPRoundtripAndDenial -count=1 -v`.
Real HTTP server + real PG successfully send/fetch a message. Reader send and
nonmember send both return HTTP 400 / code invalid; expected HTTP 403 / forbidden.
No audit changes occur, so core rejects writes correctly. Plain core membership/
permission errors are not classified by server.serviceError. Preserve explicit
authorization-denial semantics so remote clients never mistake rejection for outage.
Fake verifier in this test only supplies test identities; this is not live auth.

Postgres test run: container `665c135edd1a9b6f686d8451ee52f790239ebb1b12efd215621368d8fa72c1a0`; loopback `127.0.0.1:32843`; anonymous volume `d62140afd1249f4aa92582a94c01729a723cb34f5b548b40ae02042810b55a56`.
Cleanup: container `665c135edd1a9b6f686d8451ee52f790239ebb1b12efd215621368d8fa72c1a0` removed with anonymous volume.

## Checkpoint 4 / P2-R1 resolved

`go test ./integration -run TestServer -count=1 -v` now Pass. Designer prevalidates
JWT algorithm/key/signature size and handles dependency panics as verification
failure. Valid synthetic RSA fixture and all negative claim/signature cases pass.
This remains official-library + synthetic-key validation, not live Google auth.

## P2-R3 — Fail: peer instance tokens survive in exported audit events

Owner: cb-engineer (hosted history/snapshot redaction).
Exact repro: `integration/run-postgres.sh go test ./integration -run TestPostgresSnapshotPrivacyAndScope -count=1 -v`.
Bob's authorized snapshot correctly includes only Bob's received bodies and own
sent metadata, but its events contain owner/alice/lead/reader registration instance
tokens (`*-instance`). The output contract says hosted outputs redact other
principals' instance tokens. Routing maps redact instance correctly; history event
details do not have owner_principal and escape the recursive redactor.
Expected: project audit remains useful without revealing other principals' session
incarnations. Do not remove the history capability as a fix. Ownership checks still
prevent use of a peer's instance under Bob's principal; no impersonation claim is
made here. Snapshot body privacy and no-stamp checks continue to pass.

Postgres test run: container `832ab0d599950fa1c858bda27e3f8074e6fa66c60a9d1481b24743fd57d8a529`; loopback `127.0.0.1:32844`; anonymous volume `785f801d158cf4993c9c8d48ca915499af072f51437dc26680c7f32ed2e9fd21`.
Cleanup: container `832ab0d599950fa1c858bda27e3f8074e6fa66c60a9d1481b24743fd57d8a529` removed with anonymous volume.

## Checkpoint 5 — remote HTTP/PG sync/outage checks

TestPostgresRemoteSyncOfflineAndDenial Pass against real PG + actual local HTTP
server (in-process synthetic verifier/token source). Sync exports only Bob's inbox;
o delivered/read stamps; offline discovery/history/whoami/inbox/status return
source offline-mirror + freshness. HTTP401/403 never fall back. Invalid snapshot
sync preserves prior bytes, all seven authority/scope/session/instance mismatches
refuse, and online recovery stamps delivery only. URL/redirect boundary test Pass:
nonloopback HTTP/userinfo/query URLs rejected, authenticated307 not followed,
unsupported human/self-impersonation flow refused, response secrets not echoed.
No real gcloud credentials or Google network calls used.

## P2-R4 — Fail: offline alias resolves the wrong project

Owner: cb-designer (remote selections / mirror publication).
Exact repro: `integration/run-postgres.sh go test ./integration -run TestPostgresRemoteResolutionCannotMixAliases -count=1 -v`.
One remote client resolves authorized projects first and second, then syncs only
second. After HTTP server shutdown, ResolveProject(first) returns second's immutable
project ID. Client stores every historical selection in the latest snapshot without
tracking which immutable scope it resolved to. Expected first's matching complete
snapshot or an error; never another project's scope. This can misroute an offline
process and violates pinned scope/cache separation. Keep resolution capability;
associate each alias only with its actual immutable project.

## P2-R5 — Fail: offline sent status ignores expiration

Owner: cb-designer (mirror.Offline message_status).
Exact repro: `go test ./integration -run TestMirrorExpiredSentStatus -count=1 -v`.
Synthetic complete snapshot acquired two hours ago contains sent metadata whose
expiry was one hour ago (valid/unexpired when acquired). Offline message_status
still succeeds for it. Expected expiry behavior matching authoritative status /
inbox, which reject expired entries. Mirror inbox already filters expiration;
sent status lookup does not. No expired body exposure claimed, only metadata and
behavioral mismatch. TestMirrorReadOnlyBindingAndAtomicPublication passes, proving
read-only file bytes, no writes, failed-publication preservation and exact binding.

Postgres test run: container `d570fbfb3d6b041a846eedfa7d0fb04582a507115aa6c938839b1a5646136129`; loopback `127.0.0.1:32845`; anonymous volume `5cf841a8d28e4bf88c8c76d4ee2b559973ea45bd55c2aefb77fa901abed4f184`.
Cleanup: container `d570fbfb3d6b041a846eedfa7d0fb04582a507115aa6c938839b1a5646136129` removed with anonymous volume.

## Checkpoint 6 — all five reported regressions independently Pass

Exact grouped rerun with real PostgreSQL17.11:
`integration/run-postgres.sh go test ./integration -run 'Test(Postgres|MirrorExpired)' -count=1 -v`
PASS (4.273s), covering all five owner fixes plus the main remote flow.
P2-R2 now forbidden403; P2-R3 no peer instance tokens in snapshots; P2-R4 aliases
remain bound to the resolved immutable scope; P2-R5 expires offline sent metadata.
P2-R1 crypto/negative-token suite also independently passed after its fix.

Retention/rollback/cap tests use direct SQL solely to construct expired messages,
a scoped delivery-insert failure trigger and 10001 audit rows inside the disposable
reviewer DB. API assertions prove expired inbox/snapshot/status exclusion, owner-only
purge with retained retry tombstone/no resurrection, entire fanout rollback including
audit, safe retry after trigger removal, and explicit 10000-row snapshot refusal.
The trigger/function and container/volume are cleaned up. This is logical retention
verification, not a claim that old disk pages/backups are cryptographically erased.

Postgres test run: container `97bd442042f88040995a32fe12aede4677eecea719dd2db8f580cc8f1690fdcb`; loopback `127.0.0.1:32846`; anonymous volume `9c149b62c4d68a1639f3ba73c658c2c56dcdc38096940b93c563cf1b80cf55fb`.
Cleanup: container `97bd442042f88040995a32fe12aede4677eecea719dd2db8f580cc8f1690fdcb` removed with anonymous volume.

## Checkpoint 7 — cloud CLI and remaining lifecycle focus Pass

`integration/run-postgres.sh go test ./integration -run 'TestPostgres(Workspace|Rejects|CloudCLI)' -count=1 -v`
PASS (6.513s). Actual built cloud CLI uses a temp trusted home config, a test-only
fake gcloud executable and real HTTP/PG server. Malicious repository server/
audience/account/impersonation/principal/cache fields cannot redirect credentials.
CLI sync works, outage inbox is offline-mirror, valid offline send fails, no local
writable authority DB is created, and cloud installer pins all required routing.
No real gcloud command, account, token or config was used.

Two workspaces with the same project/session names remain isolated for inbox and
foreign status/read/reply/thread IDs. Authorized ambiguity refuses; a principal
with access to one workspace resolves only that one. PG resume fences all old
actor operations while preserving immutable send/retry identity. A separate temp
Postgres schema with version99 is refused without creating application tables.

## P2-R6 — Fail: packaged skill contradicts hosted Phase 2 behavior

Owner: cb-designer (`skills/clawbridge/SKILL.md`).
Exact repro: `rg -n 'Phase 1 is local-only|unreachable future cloud workspace'
skills/clawbridge/SKILL.md`.

The shipped agent skill currently says "Phase 1 is local-only; cloud identity and
offline mirrors are later features" and later calls an unreachable cloud workspace
"future". Phase 2 now implements an explicitly configured hosted mode and scoped,
read-only offline mirror, and README, CORE-API and deployment docs describe them.
The stale skill can cause an installed agent to refuse or avoid the supported
hosted mode. Expected: current guidance identifies both modes, requires explicit
trusted cloud configuration, permits only scoped mirror reads on transport outage,
and says writes, mark-read, resume and 401/403 failures never fall back or queue.
The existing local-authority explanation and same-OS-user limitation should remain.
No implementation defect or failed test is implied by this documentation finding.

## Final checks — Pass

Engineer declares Phase2 CODE FROZEN in QUESTIONS.md; designer reports no further
architecture changes after R4/R5 fixes. All focused regressions passed. Reviewer
ran cleanup-trapped real-PG final runs: uncached `go test ./... -count=1` and
`go test -race ./... -count=1` both Pass across all packages. `go vet ./...` Pass.
CGO-disabled builds for darwin/arm64, linux/amd64 and windows/amd64 Pass. Both
runners removed their containers and anonymous volumes. No final check or test
database remains running.

Postgres test run: container `f098dad75a1b1d4b9fea69c8fbcc22ebd2e3e64603c50036d127a3b29b979351`; loopback `127.0.0.1:32848`; anonymous volume `14219005457bfd82ee716eb1fbe3e33e4bf1316f4e868a2d9c5d2b97b58dd0d9`.
Cleanup: container `f098dad75a1b1d4b9fea69c8fbcc22ebd2e3e64603c50036d127a3b29b979351` removed with anonymous volume.

Final test run: container `c0b521459f3f1c80e02533d30e105e214abee148e2d433dd179cde3a13a862ad`; loopback `127.0.0.1:32849`; anonymous volume `8abc7ee6b2c56ec39bc23e90bc684cdbf4a7f441a0086e1b80a3999600e78ceb`.
Cleanup: container `c0b521459f3f1c80e02533d30e105e214abee148e2d433dd179cde3a13a862ad` removed with anonymous volume.
Final race run: container `a68a69da50ad97220eef3ae6b932c79b3bf63b8454a823d75eb5c4cfb6a2105d`; loopback `127.0.0.1:32850`; anonymous volume `ec3e065ebacf60bac05fcd514f7b965d7ba96ea54297ca0f8a6e96446f57873a`.
Cleanup: container `a68a69da50ad97220eef3ae6b932c79b3bf63b8454a823d75eb5c4cfb6a2105d` removed with anonymous volume.

## Verification limits

Google authentication evidence uses the official verifier with intercepted
synthetic keys. No live Google-issued token, IAP request, VM deployment, backup
restore or billed cloud resource was exercised. The single-VM Compose template is
reviewable but not deployed. The installed Claude client connected and discovered
all tools; Claude tool execution and model-driven conformance remain unclaimed.

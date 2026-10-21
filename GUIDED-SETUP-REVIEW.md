# Guided startup independent review

Status: **Automated acceptance PASS on final refreeze; no open reproduced defect. Guided live/native acceptance remains Pending**.

Reviewer owns this document and integration probes only. No implementation edits,
commit/staging, deployment, consent, native keyring access or target-machine
operations. Engineer owns implementation and focused package tests; final checks
will be coordinated after an explicit freeze. Existing Phase 4 automated and
owner-attributed live evidence does not automatically establish guided startup.

## Contract and acceptance matrix

Read GUIDED-SETUP-DESIGN.md and CORE-API.md guided extension. No contradiction
identified at initial read. A running Linux Secret Service is availability
evidence only; unlocked/write-ready behavior and actual credential operations
remain separate. Interactive values must be sanitized without changing routing
values. Guided output is human-readable; explicit join JSON remains compatible.

| Gate | Required evidence | Status |
| --- | --- | --- |
| Entry/help | Bare non-TTY and help print usage without input, storage or network; explicit non-TTY wizard fails with setup-join guidance | Pass: actual-binary nonterminal/help probes |
| Implicit flags | Only unique absolute client-config allowed without command; other globals including --json fail rather than selecting authority | Pass: actual-binary rejection + parser inspection |
| Target selection | Explicit path > environment selector > state-home default; repository cannot select target/authority | Pass: selector inspection + retained explicit-profile regressions |
| Existing target | Existing valid route opens TUI; malformed/inaccessible/symlink/incomplete target fails unchanged, never first-run replacement | Pass: PTY malformed/incomplete/symlink + injected valid-route handoff |
| Missing target | Terminal path prompts URL, workspace, project, browser/device in order with no pilot defaults | Pass: PTY prompt/summary, injected device join and browser-default source review |
| Authority isolation | Fresh wizard ignores inherited server/backend/principal/actor routing from repository/environment; existing target validation/pinning retained | Pass: hostile env/repo probes + injected resolver/transaction |
| Confirmation/cancellation | Sanitized summary and default-no confirmation; decline/EOF/cancel preserves route/credentials with no acquisition or writes | Pass: PTY decline/EOF/SIGINT + injected cleanup failure |
| Keyring availability | Before consent on Linux only: reachable local session bus + Secret Service owner; other platforms use actual native credential operation; safe missing-service guidance | Pass: Linux synthetic socket/sentinel and hook ordering; native live Pending |
| Safe guidance | No keyring-password prompt, service startup, package install, plaintext backend or obsolete provisioning | Pass: static audit, diagnostics tests and README review |
| OAuth/join transaction | Only existing discovery/staged OAuth join; exact authority/scope verification, bounded rollback and truthful cleanup failures | Pass: injected staged join + retained rollback/HTTP regressions |
| Join-to-TUI | Successful join emits brief success then opens TUI; later TUI failure explicitly preserves completed join rather than claiming rollback | Pass: injected close-before-handoff and preserved successful join |
| Redaction | Terminal controls escaped; no token/app-secret/unsafe response body output; explicit join automation unchanged | Pass: synthetic summary/native-error/URL/config tests |
| Compatibility/final | Entrypoint/auth boundary focused review, uncached suites/race/vet/cross and disposable PG as justified after freeze, stable fingerprints | Pass: full + PG race, vet, CGO0 three targets; final wording delta focused race |
| Live | Actual terminal/native keyring/device/browser guided flow on target | Pending; no synthetic-to-live inference |

## Initial checkpoint (21 October 2026)

Implementation entry points were not yet present at initial inspection. Existing
public CLI Run/subprocess boundary is suitable for nonterminal/help/config probes;
interactive success/rollback/preflight requires either an isolated PTY or the
engineer's injected adapter tests. Reviewer will inspect injection coverage
independently and add external probes where possible without touching production.
No new test or live operation run at this initial checkpoint.

Reviewer added `TestGuidedStartupNonterminalBoundaries` in integration with eleven
isolated subprocess cases: bare startup, missing/invalid selected config, three
help forms, wizard help/nonterminal failure, and commandless mode/scope/JSON.
The fixture supplies hostile inherited route/actor and malformed repo metadata,
requires no state/config mutation or authority requests, and bounds execution.
Executed after dispatch appeared: Pass; see focused checkpoint below.

**G-O1 — pre-freeze static observation, engineer notified:** initial
`internal/auth/availability.go` used `dbus.ConnectSessionBus`. The pinned
godbus v5.2.2 implementation calls `getSessionBusAddress(true)`, then on Linux
`getSessionBusPlatformAddress()` executes `dbus-launch` if no address is found.
This contradicts the no-service-startup boundary. No bus call or service startup
was executed by reviewer. Expected: a non-autostart connection path, with focused
sentinel coverage showing no dbus-launch attempt. Await implementation/retest;
not promoted as a frozen defect.

## Focused pre-freeze checkpoint

- `go test ./integration -run '^TestGuidedStartupNonterminalBoundaries$' -count=1`:
  Pass, 2.070s (eleven isolated actual-binary cases). Initial reviewer help
  assertion incorrectly required the application name; corrected to accept the
  standard `Usage of setup wizard` flag help. No implementation defect.
- `go test ./internal/cli ./internal/auth -run '^TestGuided|^TestAvailability' -count=1 -v`:
  Pass, CLI 0.308s/auth 0.194s. Independently read injected preflight-before-
  acquisition, transaction, handoff and preserved-route-on-TUI-failure tests.
- `go test ./integration -run '^TestGuidedStartup' -count=1 -v`:
  Pass, 4.065s. Adds five actual-binary PTY cases: default-no decline, invalid
  credential-bearing URL redaction, existing malformed/incomplete config and
  symlink rejection. All retain config and avoid state creation/acquisition.
  Python3 provides PTY only; test skips explicitly if unavailable. Terminal echo
  is disabled to distinguish application output from terminal input echo.

**G-O1 revised:** concrete address Dial replaces ConnectSessionBus; static
no-autolaunch concern corrected. Dependency context closes an established
connection, including blocked auth. Availability sentinel coverage still needs
strengthening: its `touch` executable is absent from its deliberately restricted
PATH; use shell builtin redirection to detect launch reliably. Engineer notified.

**Lifecycle observations sent to engineer, pre-freeze:** cancellation test cancels
immediately without synchronizing on actual blocked Read, so may test only an
already-canceled context. Production ignores `Cancel()` false and then waits
unconditionally for read completion; pinned cancelreader Windows implementation
explicitly permits false for blocked GetOverlappedResult. Native Windows behavior
has not been exercised. Also godbus Dial constructs transport before applying
WithContext: its TCP transport uses net.Dial, outside the stated three-second
bound. Review local address restriction or bounded transport before freeze.
These are open observations, not promoted frozen implementation defects.

One-byte reads avoid wizard read-ahead; injected handoff verifies unread TUI
bytes and reader closure before acquisition. Final lifecycle verdict remains
Pending revised tests/implementation and explicit freeze. Heavy suites and
native guided/keyring/consent operations have not run.

## Frozen lifecycle checkpoint (13:27 declaration)

HEAD `cdc1b0db3ea969bc2554a64f8eb5dfafd04a341b`, dirty guided increment.
Captured 81 Go/module fingerprints before tests. Reader cancellation now waits
for Read-entry in tests, closes on Cancel=false, and has a one-second bounded
cleanup error. Linux probe now uses context-aware net.Dialer to a concrete local
Unix socket plus bounded auth; rejects TCP/autolaunch/multiple addresses. Sentinel
uses builtin redirection. These correct the prior observations; native platform
success is not inferred. Contract doc clarification matches Linux-only preflight.

**G-O2 Open — frozen static handoff defect; engineer/root notified.**
`internal/cli/wizard.go` closeInput ignores input.Close() error and marks closed.
After confirmation it unconditionally continues into keyring, join and TUI.
The pinned Windows reader Close can fail console restoration or handle cleanup.
Exact injected repro requested: a reader supplies valid confirmed answers but
Close returns `errors.New("synthetic close failure")`; acquisition/TUI must remain
uncalled and config unchanged. Current code continues. Expected fail-closed,
truthful terminal cleanup error. Not a native-Windows runtime claim. PG-backed
full suite already started; subsequent heavy checks held for corrected refreeze.

README current content independently reviewed: guided default-no existing-service
join and selected profile, invalid-config preservation, automation guidance,
Linux same-session service-only versus unlock/write readiness, no service startup
or plaintext fallback. SHA256 `f539468002423a4b614656636518a965e4fa447a93f4d56015f8b7b2de0a0f2f`.
This is documentation evidence separate from implementation fingerprints.

## G-O2 corrected refreeze (13:29)

Independent hashes match engineer's new declaration:

```text
efe1cc3cb63fcff4bc1810cd78a9f1ab02672c6eea4724a611412e0d0e9e7582  internal/cli/wizard.go
2810f1f14f733b10c923d94f10d98300eeb4aceb27dde7ab3f9c443ba6b38827  internal/cli/wizard_test.go
```

Static review confirms Close failure is retained, sanitized and returned before
keyring/join/TUI; deferred cleanup failure also propagates. Exact injected y/n
regression leaves target absent and hooks forbid acquisition. **G-O2 resolved**
by independent focused rerun, not a claim of native Windows operation.

`go test ./internal/cli ./internal/auth -run 'TestGuided|TestAvailability' -count=1 -v`:
Pass (CLI 0.325s/auth 0.265s). Includes actual-read-entry cancellation, Cancel=false
close-before-wait, no read-ahead, transaction reuse, truthful postjoin TUI error,
preflight-first ordering, corrected autolaunch sentinel, local-only bus addressing
and bounded stalled authentication against an isolated fake Unix socket.

`go test ./integration -run '^TestGuidedStartup' -count=1 -v`: Pass, 3.910s,
now 18 cases. Added actual-binary EOF and SIGINT after the first prompt; neither
creates config/state or requires another input line. This focused run overlapped
the engineer's narrow correction; final full suite below covers stable refreeze.

First PG-backed full suite on the initial freeze passed (integration 47.684s),
then exact fixture cleanup ran. That pass is historical and superseded by the
corrected-tree run. Its fixture ID/port/volume are retained below for cleanup
audit. No test credentials were written into the repository.

## Final automated acceptance — 21 October 2026

**PASS.** No open reproduced defect remains in this guided increment. G-O1
(non-autostart/bounded Linux availability), cancellation/handoff observations,
G-O2 (ignored reader-close error) and G-O3 (cross-platform availability wording)
are corrected and independently reviewed. No production code edited by reviewer.

G-O3 final delta: the universal message now requires native credential storage
without claiming a macOS/Windows pre-consent probe. Replacing only that string
and removing only its new test assertion reconstructs the exact prior SHA256s.
All other 79 Go/module files match the PG full/race acceptance manifest. Because
this delta changes wording only, final focused race and vet/cross checks cover
it; unchanged expensive PG suites were not repeated.

Commands and results (all uncached tests use `-count=1`):

| Command | Result |
| --- | --- |
| `CLAWBRIDGE_TEST_RESOURCE_LOG=<temporary evidence file> integration/run-postgres.sh go test ./... -count=1` | Pass on G-O2 corrected refreeze; integration 46.907s, CLI 2.117s, auth 2.353s; PostgreSQL 17.11 exercised, no PG skip substituted |
| `CLAWBRIDGE_TEST_RESOURCE_LOG=<temporary evidence file> integration/run-postgres.sh go test -race ./... -count=1` | Pass on same refreeze; integration 54.282s, CLI 3.427s, auth 3.622s; no race report; PostgreSQL 17.11 exercised |
| `go test -race ./internal/cli ./internal/auth ./integration -run 'TestGuided\|TestAvailability' -count=1` | Pass on final G-O3 wording refreeze; CLI 1.598s/auth 1.344s/integration 6.348s, including all 18 actual-binary startup probes |
| `go vet ./...` | Pass; repeated after final wording change |
| `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o <temporary artifact> ./cmd/clawbridge` | Pass |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o <temporary artifact> ./cmd/clawbridge` | Pass |
| `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o <temporary artifact> ./cmd/clawbridge` | Pass |
| `git diff --check` | Pass |

The exact evidence directory used by the runner is
`/var/folders/h6/qh_thy6s4_n4ccxfqhc9gp080000gn/T/cb-guided-final-owd3_2g4`;
its resource file is `resources.md`. Logs and fingerprint manifests there contain
no checked-in credentials. The runner passes a random test-only DSN through the
environment and traps cleanup. Cross-build binaries were removed after success.

Final code fingerprints:

```text
03b53a987e62b7cc9ab025919af53557dc5329f594b5f9d379a8a40bc13af4de  internal/auth/availability.go
7fb6b3860526f14efa59b89af99c1e02414141a1063f25ddeec909b5b9af6d5d  internal/auth/availability_test.go
564a32c925d115dfda113fde14a692c2fb98896724ecd55353603fdcd6bccffe  internal/auth/oauth.go
40163427a7eb04cc749364093c63ec2a61186cff85a26b2bf3769add7802c936  internal/cli/cli.go
9e2ea1655eda34642d00493fe272630f860249c7875f0dfa9e17928e02dd4812  internal/cli/config.go
209a0f850c1130a58783cbb0748a58e2591a2d036f0f638abcc40f815c436bf4  internal/cli/wizard.go
468a740b06dc7712116e5149075fd7e9b3171abb624ea2fe02d46642faea23df  internal/cli/wizard_test.go
```

Final 81-file Go/module manifest stable after all checks; manifest SHA256
`0bd748f5c983512dde27a4cff79703e693f8da9bda53ae7560ef9f366888af0e`.
README remains independently reviewed at the documentation hash above. Owner/root
contract-only clarifications do not change implementation acceptance bytes.

### Disposable resource audit

Docker Engine 29.8.2, `postgres:17-alpine`, actual `psql (PostgreSQL) 17.11`.
All bindings are numeric loopback random ports. First row is the historical
initial-freeze full run; second/third are corrected-tree full/race respectively.

| Container ID | Loopback port | Anonymous volume |
| --- | --- | --- |
| `4bce4d4b77669c11c24da2deb339e972070fae5650b477e7812e19bf6dc28345` | 32796 | `57e443185e0afc3ecd1910978d8cd3d6961d1508301e457540cbdecc6da65ce2` |
| `442a86d18b2e4b9f0b264984c5853f77f401502a5b353656a5ccdc14f557f4ed` | 32797 | `2d341b1cba1090a12db9d871c5ecc9404c2bcd490d7bed3b364d3131493e6c29` |
| `ef18b0597f0f38a608525703455c3af681f767303563369c80807fdd31a33c79` | 32798 | `080879a9cdf708ad6ccda8ec1312c2a90a252b999b1af461359eabc140e10d4c` |

Runner cleanup removed each exact container with its anonymous volume.
Independent subsequent `docker container inspect ID` and `docker volume inspect
NAME` confirmed all six absent. No existing container/image/volume was removed.

### Limits and remaining live gate

Automated terminal probes use local PTYs; auth/join success, rollback and keyring
ordering use synthetic injected credentials. A stalled fake Unix bus tests
bounded failure, not a real Linux Secret Service success. No guided Google
consent, native Linux/macOS/Windows credential operation, user config mutation,
installer execution, deployment or live endpoint operation was performed here.
Native Windows cancellation/console restoration remains a platform live limit.
Prior owner-reported WSL onboarding and replacement alignment Pass remain in
PHASE-4-LIVE-ACCEPTANCE.md; neither establishes live guided startup. Guided live
acceptance, other previously pending lifecycle/MCP gates and release distribution
remain separate work. No staging, commit, push or consent by reviewer.

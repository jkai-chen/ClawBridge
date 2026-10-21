# Responsive TUI / archive independent review

Status: **Automated PASS on corrected A1 refreeze; Linux reuse/resize/archive completion is owner-reported PASS, as attributed below.**
Reviewer owns this report and integration probes only. Contract: TUI-ARCHIVE-DESIGN.md and CORE-API.md responsive/archive additive adapter API. No live operations or credential access; no implementation edits.

| Gate | Acceptance / independent probe | Status |
| --- | --- | --- |
| Whole screen | All six screens fit terminal cell width and row height, including borders, scope, tabs, notices, input and help. Tiny terminals have bounded hint and usable quit/cancel. Wide terminals are not capped at 140. | Pass |
| Unicode and controls | CJK, combining marks, emoji, long unbroken values, ANSI/OSC/bidi controls remain safe and bounded without modifying stored text. | Pass |
| Resize lifecycle | Repeated WindowSizeMsg makes no authority calls; preserves immutable actor/scope, draft, confirmation, selected ID and inspectable details. Selection remains visible; scrolling reaches long bodies/diagnostics. | Pass |
| Session archive | Only retired/moved records with valid updated_at at least 24h old; equality archives; active/stale, missing/bad/future timestamps remain Current. | Pass |
| Recipient inbox | Classification uses that delivery's read_at, not created_at/delivered_at; unread and recently read remain Current; exact boundary and malformed/future cases. | Pass |
| Clock / filter | One injected time per pass; refresh/view entry/minute tick reclassify without extra authority polling; raw records retained; Current/Archive/All preserve selected ID where present. | Pass |
| Paging | Scoped limit=100, exact server cursor, next/visited previous, refresh reset, empty filtered page retains next navigation; no unbounded fetch/global coverage claim. | Pass |
| Input / actions | v/[ /] inert during editing/confirmation; authorized actions unchanged; browsing does not mark read or introduce mutations. Draft survives resize/scroll. | Pass |
| Offline | Explicit scoped read-only mirror source/freshness and incomplete coverage; no credential/401/403 fallback or queued writes. | Pass |
| Compatibility | No Store/HTTP/CLI/MCP/schema/auth contract change. Focused/full/race/vet/cross checks after freeze; unchanged PostgreSQL suites not repeated without cause. | Pass |
| Native live | Owner reports completion of the exact bad2d9e Linux reuse/resize/archive guide. No per-command transcript or independent native observation supplied. | Owner-reported Pass |

Contract inspection: exact additive API is published: Config.Now, ArchiveFilter, SetFilter/CycleFilter, NextPage/PreviousPage/Scroll, Snapshot paging/filter/scroll fields and ArchiveTickMsg. No contract contradiction identified on initial read. The initial in-progress renderer still had the old width cap; frozen implementation removes it, as verified by code review and expanded sizing probes.

Evidence boundaries: prior owner bundle reuse and Projects/Sessions alignment evidence remain in PHASE-4-LIVE-ACCEPTANCE.md. They do not prove guided consent or this new resize/archive increment. Final corrected-freeze automated results are recorded below.

## Pre-freeze focused evidence

Command: `go test ./integration -run '^TestTUIArchive' -count=1 -v`.
Boundary/exclusions (Sessions and Inbox), retained filters/local tick, and scoped empty-page next/back probes **Pass**. Requests use published `after` (not an invented request cursor field), limit=100 and unchanged workspace/project/session/instance. These synthetic results are not final frozen acceptance.

Two reproducible **observations, not promoted frozen defects** in the in-progress renderer:

- O1: `go test ./integration -run '^TestTUIArchiveDiagnosticTailInspectable$' -count=1 -v`: fake executor returns 80 repetitions of diagnostic context followed by DIAGNOSTIC-END. At 40×12, scrolling through 180 offsets never exposes the tail. Error remains in truncated header outside scrollable content. Expected: full sanitized diagnostics inspectable without authority calls.
- O2: `go test ./integration -run '^TestTUIArchiveSmallEditorRetainsCancelAndDraft$' -count=1 -v`: loaded Inbox source=server, BeginSend, draft peer|DRAFT-PRESERVED, WindowSizeMsg 24×9. Output consumes five header/status rows plus three frame rows and partial editor; Esc cancel/footer absent. Expected: reserve editor/cancel before final screen fitting, preserve draft and cell/row bounds.

Engineer owns correction; independent probes remain strict. At this pre-freeze checkpoint, reviewer had changed no implementation files, run no heavy gates, and claimed no live/native behavior.

## Expanded independent pre-freeze checkpoint

Latest command: `go test ./integration -run '^TestTUIArchive' -count=1 -v` — **Pass**, integration 0.729s. O1 diagnostic-tail and O2 24×9 editor/cancel exact regressions now pass; observations closed against current bytes, subject to frozen retest.

Additional public-model probes in integration/tui_archive_layout_test.go pass:

- All six loaded screens at widths 8,12,23,24,32,40,80,141,215 and heights 1,4,8,9,12,24,50 (378 screen/size combinations). Whole-screen cell/row bounds, sanitized Unicode/control text, no peer instance exposure, immutable selection/scope/identity, tiny-view quit and no executor/doctor/planner activity during resize.
- Selected row 35 remains visible across shrink/grow on Projects/Sessions/Inbox; full owner/body tails and Cloud plan/Doctor diagnostic tails remain inspectable by content scrolling. Assertions remove presentation borders/whitespace so wrapping a marker is not misreported as data loss.
- Draft survives tiny/large/repeated resize. v/]/[ type into draft without changing filter/page or starting commands; public NextPage/CycleFilter are also inert during edit. Explicit submit preserves exact Unicode body and pinned actor/instance. Confirmation survives 24×9 resize and cancels without Apply.
- Refresh/reordering and filter changes preserve selected immutable session ID; disappeared item clamps safely. Requests retain actor/scope.
- Offline mirror view labels incomplete coverage, retains source, and refuses send without authority activity.

These remain synthetic pre-freeze passes, not a native Linux/live resize claim or final acceptance. No new implementation finding reproduced. Full/race/vet/cross gates remain Pending explicit freeze; unchanged PostgreSQL reruns are not justified by this TUI-only scope.

## Frozen review finding A1 — selected immutable IDs inaccessible

**Closed on corrected refreeze; original finding below retained for history. Owner engineer.** Root's review clarification requires selected detail IDs/owners inspectable through scrolling. Exact repro: `go test ./integration -run '^TestTUIArchiveSelectedImmutableIDInspectable$' -count=1 -v`. Projects passes; Sessions and Inbox fail. Fake authorized selected record has a long immutable ID ending ID-END; at 40×12 no scroll offset exposes that ID. Session selected details omit ID entirely; Inbox shows only a 36-cell ID fallback (and shows created_at instead of ID when populated). Expected: full sanitized selected IDs inspectable in scrollable details without altering stored identity, authorization or reply target. No implementation edit by reviewer.

Recovered initial full run failed only obsolete reviewer attribution assertions at widths 8/12/20, where contract now requires resize hint. Reviewer fixture now verifies tiny hint and inspects complete attribution through scrolling at usable size. Combined focused attribution/archive run Pass 1.314s. Subsequent uncached full JSON run passed integration 41.377s before additional A1 regression was introduced. Final acceptance remains open for A1 retest; ongoing race/check results are interim evidence until correction/freeze.

## Interim frozen acceptance evidence (21 October)

Base HEAD `0340a8ad898a0c0e8bc8132b9ab16eec1572e48c`, shared dirty acceptance tree. Engineer freeze observed 14:20; all five production/test TUI hashes independently measured below. Three supplied hashes visible in recovered terminal match exactly; coordinator reports engineer confirmed all five. Whole Go/module manifest-before.json was captured before gates; subsequent comparison showed only reviewer attribution fixture changed, no production drift.

```
ddf53d4409cfee809d66aeb055332451518dbb8f2d13bf60146aa7830c942c7a  internal/tui/model.go
540ca6429540f176771486d6e66817bc475ad69eb5e8b3ae25e922c599800a07  internal/tui/view.go
b4ee8a4fb38d49fefd1d23b7c47f17df1dcaa94c05e84d68fa17eef02d9e7d2f  internal/tui/archive.go
316e1fa25dbb4e82b89a1d58d42794d3ce1adb87eeccb964d51070278493e370  internal/tui/layout.go
8850655e4826356da63e174ed02e71f2109706a636210d477afcdcd4eb73024e  internal/tui/archive_layout_test.go
```

| Command | Interim result |
| --- | --- |
| `go test ./integration -run '^TestTUIArchive' -count=1 -v` | Pass 0.863s before A1 probe added; includes original reply target retained when tick removes its row, and failed next/previous page cursor/data rollback plus retry |
| `go test ./integration -run '^(TestTUIArchive|TestPhase4SessionsDisplay)' -count=1` | Pass 1.314s after reviewer attribution fixture update, before A1 probe added |
| `go test ./... -count=1 -json` | Pass after fixture reconciliation, integration 41.377s; before A1 probe added |
| `go test -race ./... -count=1 -json` | Fail only A1 Sessions/Inbox regression, integration 54.847s; no race diagnostic |
| `go vet ./...` | Pass exit 0 |
| `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o <temp>/clawbridge-darwin-arm64 ./cmd/clawbridge` | Pass; output deleted |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o <temp>/clawbridge-linux-amd64 ./cmd/clawbridge` | Pass; output deleted |
| `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o <temp>/clawbridge-windows-amd64.exe ./cmd/clawbridge` | Pass; output deleted |

Evidence directory: `/var/folders/h6/qh_thy6s4_n4ccxfqhc9gp080000gn/T/cb-tui-final-4vg8x44v`. Initial `full.jsonl` retains the reviewer fixture drift failure. `full-final.jsonl`, `race-final.jsonl`, `build-vet-results.json` and per-build/vet logs retain interim results; these are superseded only after corrected refreeze, not represented as final Pass.

Actual ordinary-run skips: 12 real PostgreSQL-dependent tests (11 integration plus internal/bus TestPostgresRoleDenialHasNoSideEffects), and three explicit live HTTPS/Google/device tests without opt-in fixtures. cmd/clawbridge and internal/mirror report no test files. No current PostgreSQL lifecycle or live operation was performed. Prior independent PG baseline is GUIDED-SETUP-REVIEW.md: PostgreSQL 17.11 full 46.907s and race 54.282s passed; exact fixture/container/volume cleanup independently recorded there. Backend/schema/auth/CLI implementation unchanged in this increment, so this historical baseline is retained without claiming fresh PG execution.

Root acknowledged A1 and assigned correction to recovered engineer; no contract relaxation. Final full/race/vet/cross evidence awaits revised freeze. Native Linux resize/archive behavior remains Pending owner observation.

## Final corrected-freeze verdict — Automated PASS

Engineer explicitly refroze A1 after adding full sanitized Session ID to selected session details and Message ID to selected Inbox details. Manual review confirms these are display-only additions using terminalText; stored identity and pinned reply target remain unchanged. Independent A1 and all expanded integration probes pass. O1/O2 remain closed. No unresolved implementation defect found in this scope.

All five supplied revised SHA256 values independently match:

```
ddf53d4409cfee809d66aeb055332451518dbb8f2d13bf60146aa7830c942c7a  internal/tui/model.go
dead7f9369e3844b1699ff8c76f4a1a504bb5135e3bbe6915bafbedbb676ff83  internal/tui/view.go
b4ee8a4fb38d49fefd1d23b7c47f17df1dcaa94c05e84d68fa17eef02d9e7d2f  internal/tui/archive.go
316e1fa25dbb4e82b89a1d58d42794d3ce1adb87eeccb964d51070278493e370  internal/tui/layout.go
f6455329b40f4b0177b5525a1b79f2b5d3a6beeb6cfa4fdd777c90774f9ca8ff  internal/tui/archive_layout_test.go
```

| Final independent command | Result |
| --- | --- |
| `go test ./integration -run '^(TestTUIArchive|TestPhase4SessionsDisplay)' -count=1 -v` | Pass, integration 1.081s, includes exact A1, reply-target tick, failed-page rollback, all six screens/378 sizes and prior attribution |
| `go test ./... -count=1 -json` | Pass, integration 41.396s, TUI 0.328s |
| `go test -race ./... -count=1 -json` | Pass, integration 51.171s, TUI 2.861s; no race report |
| `go vet ./...` | Pass exit 0 |
| `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o <temp>/a1-darwin-arm64 ./cmd/clawbridge` | Pass exit 0; binary removed |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o <temp>/a1-linux-amd64 ./cmd/clawbridge` | Pass exit 0; binary removed |
| `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o <temp>/a1-windows-amd64.exe ./cmd/clawbridge` | Pass exit 0; binary removed |
| `git diff --check` | Pass |

Final evidence in the directory above: a1-focused.log, a1-full.jsonl, a1-race.jsonl, a1-vet.log, three a1-OS-ARCH.log files and a1-check-results.json. All 86 Go/module files remained identical before/after these final checks; a1-manifest-before.json and a1-manifest-after.json SHA256 both `7098ea7fc8e7885ffc397988af5c5e45d07360a8ad0ba9da5eb494e4d575d866`. No backend/schema/CLI/auth implementation delta since the earlier accepted baseline; no PG lifecycle repeated.

Actual final full and race skips are each exactly 12 PostgreSQL tests plus three opt-in live tests. PG: internal/bus TestPostgresRoleDenialHasNoSideEffects; integration TestPostgresCloudCLITrustedRoutingAndOffline, TestPostgresProjectAndMemberViewsAreScoped, TestPostgresRetentionRollbackAndHonestCap, TestPostgresRejectsNewerSchema, TestPostgresBootstrapAndAuthorization, TestPostgresSnapshotPrivacyAndScope, TestPostgresConcurrentPrincipalsAndReplay, TestPostgresWorkspaceIsolationAndResume, TestPostgresRemoteSyncOfflineAndDenial, TestPostgresRemoteResolutionCannotMixAliases, TestPostgresHTTPRoundtripAndDenial. Live: TestDeploymentLiveHTTPSContract, TestLiveGoogleDiscoveredClientsExchangeWithMatchingSecrets, TestLiveDeviceLoginDiscoveryReachesPollingNoGcloud. cmd/clawbridge and internal/mirror have no test files. Prior actual PG 17.11 full/race and resource cleanup remain historical evidence in GUIDED-SETUP-REVIEW.md, not fresh execution for this increment.

Manual code/contract review covers complete screen budgeting and sanitized content, local one-minute reclassification with one time per pass, retained raw page records, strict retired/moved and per-recipient read stamps, exact scoped bounded after requests, visited cursor rollback on failed next/previous/refresh, immutable selected-ID preservation, pinned reply-target lifecycle, offline read-only coverage labels and input/confirmation isolation. README and CORE additive API match these semantics. No deployment, native terminal/keychain/Google operation or real user-config access by reviewer. This synthetic automated Pass does not establish native Linux resizing/archive interaction; that remains the owner live gate. Earlier owner reuse/alignment evidence stays narrowly attributed in PHASE-4-LIVE-ACCEPTANCE.md, with no inferred guided consent.

## Exact-commit Linux reuse bundle — independent static PASS

Artifact: `/Users/oriixchan/Downloads/clawbridge-responsive-linux-amd64-bad2d9e.tar.gz`.
Source commit: `bad2d9e61f5781ebb710dd33d4d52bf7ee19d9ec`.
Archive SHA256: `9a948a1c734d61ca92ff55555c29aca475f882afa616a75267474738015f7267`; supplied companion file matches the exact filename/digest.

Independently verified six regular files plus their one top-level directory, with no symlinks, hardlinks, absolute/traversal paths or extra payloads. Archive bytes equal the supplied inspected directory. SHA256SUMS contains exactly five distinct payload entries and all five hashes match; manifest SHA256 `ca7cc42bee761f8f4ca2ae415fcfbb86c3099c0742cf0ad78a2d15b377efc42d`.

| Payload | Independently matched SHA256 |
| --- | --- |
| BUILD-INFO.txt | `6c5cb560e476d37dc159bb48bbf75be95dc2f057ff50f44996c39b933f6db3c6` |
| README.txt | `c8375f8464aae4f061675a70b6a0d566cb72f83be9c01820c9d3c9d10336f78f` |
| SOURCE-COMMIT.txt | `7b5f03053624ae9c52c101ea8d92062b0ee585153a2474d1c7fc3c568b93f269` |
| clawbridge | `a6c2254253c065868915a27e9aaeb7975bd694b924bad2ae5d5c2f4b9cc99fa9` |
| reuse-existing.sh | `60fe691d2c7a33d098a6aadf8d2a6fc11d3d2defa960c7c14d5a026948a45d36` |

SOURCE-COMMIT.txt matches the full published commit. Independent `git show COMMIT:PATH` reconciliation matches all 86 accepted Go/module hashes. Independent `git archive --format=tar COMMIT` digest matches the recorded source archive SHA256 `77a12bb11a3d7d42bbd5cb3ae4197b4ea1950dece3e8cf7ffd2ffe480e54da06`. BUILD-INFO records the engineer's cached, network-disabled build command from extracted source. This verifies recorded source attribution and binary metadata, not an independently reproduced binary build.

Static ELF parsing and `file` confirm ELF64 little-endian x86-64, machine 62, static executable with no program interpreter. `go version -m` reads Go 1.27.0, CGO_ENABLED=0, GOOS=linux, GOARCH=amd64, GOAMD64=v1 and trimpath=true; no VCS revision is embedded, consistent with archive extraction. `sh -n reuse-existing.sh` passes. Byte comparison against guided-0340a8a archive (verified SHA256 `5ad70285fb74b991e322f0180e79c0bbc49afbfeb54f40a130b44146cfa07917`) proves the reviewed launcher is unchanged.

Instructions preserve original profile/cache/state and native namespace `clawbridge-phase4-linux-8fae89c143150f33`, require the original unlocked D-Bus/Secret Service session, and explicitly limit reuse to preflight → whoami → tui. No fresh guided launcher/config/credentials are included. Preflight is correctly described as checking service presence rather than unlock/write readiness; failure guidance stops before rejoin. Inherited join/logout actions exist in the unchanged launcher but are explicitly outside this test. Safe owner sequence after archive/manifest/source-marker verification is `sh ./reuse-existing.sh preflight`, `sh ./reuse-existing.sh whoami`, `sh ./reuse-existing.sh tui` from the new extraction directory in that existing shell. README accurately describes resize/filter/paging/ID inspection and forbids send/reply/read/config changes for this observation.

No bundled configuration or credential artifact found among the exact file set. Narrow private-key/installed-app-secret/Google token pattern scan matched no filenames; this is not an exhaustive secret detector. App and launcher were not executed; no rebuild, credential access, target mutation, consent, network/live request or heavy test repeat. Prior bundles preserved. Evidence: bundle-static-results.json and bundle-go-metadata.txt in the final evidence directory above. `git diff --check` passes. No static artifact blocker; native Linux responsive/archive owner observation remains Pending.

## Owner-reported Linux responsive/archive completion

On 21 October root relayed the owner's report that the exact bad2d9e step-by-step Linux reuse/resize/archive guide was "all done". Record **Pass — owner-reported completion** of that guide using the reviewed reuse-only bundle, superseding the Pending owner-observation status at packaging. This is an aggregate completion report; no per-command transcript, exact resize dimensions, screenshots or independent reviewer observation of the native run was supplied.

This closes only the narrow reported existing-profile Linux reuse/resize/archive observation. It does not establish new guided/browser/device consent, forced-expiry refresh, logout/keychain removal, terminal restoration/cancellation, installed MCP/model behavior, second-human access or any additional lifecycle/platform acceptance. Reviewer performed no target or credential operations. Automated/model and static packaging evidence above remain separately attributed.

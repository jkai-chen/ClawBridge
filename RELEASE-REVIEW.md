# Release distribution independent review

Status: **Final local automated/static PASS on revised frozen source. O1/R2 closed. Native Windows PS5.1/7, actual public CI and release/install delivery remain Pending root-controlled execution.** Reviewer owns this document and integration probes. Root owns contracts/history audit/external publication; engineer owns production. No real repository tag, release, visibility change, deployment, credential/config mutation or hosted CI execution by reviewer.

Contract read in full: RELEASE-DESIGN.md (21 October 2026). CORE release extension was absent on initial read and subsequently published; its version/assets/installers/public-only CI semantics match the design. Exact CLI/script contract is sufficient to prepare probes; new production scripts/APIs were not yet present. Native PowerShell (`pwsh`/`powershell`) is unavailable locally. Native Windows runtime acceptance remains Pending actual Windows CI; cross-builds cannot substitute for it.

| Gate | Independent acceptance / probe | Status |
| --- | --- | --- |
| Version boundary | Actual binary `version` and `--version` exact source=build JSON; dev/unknown defaults, injected exact tag/full commit/UTC build timestamp; malformed/inaccessible route/config, poisoned env and nonTTY cannot trigger authority/keychain/network/filesystem activity. Extra arguments reject early. | Pass |
| Tag/build validation | Canonical vMAJOR.MINOR.PATCH only, exact matching tag at clean source; reject dirty tracked/nonignored input, wrong/missing tag, unsafe/ambiguous output, caller GOOS/GOARCH interference. Use isolated local clone/tag only. | Pass |
| Asset completeness | Exactly five raw target executables, two installers, SOURCE-COMMIT.txt and SHA256SUMS; metadata injected from tagged commit/time; deterministic manifest covers every other exact basename once with lowercase hash/two spaces. No archive execution or target binary execution by builder. | Pass; foreign runtime unclaimed |
| Platform mapping | POSIX Darwin/Linux x86_64/amd64 and arm64/aarch64; Windows native x64 including 32-bit process; unsupported OS/arch reject before downloads/destination mutation. | POSIX Pass; Windows runtime Pending CI |
| Latest pinning | Latest discovery exactly once; strict validated stable tag reused for every download. Explicit version performs no discovery. Invalid tags/ref/URL/option/path reject early. | POSIX Pass; Windows static/parser Pass, native Pending CI |
| Transport | Fixed HTTPS jkai-chen/ClawBridge routes, bounded connect/total timeout and HTTP failure; asset HTTPS redirect allowed, downgrade refused. Local tests stub commands/transport, no production URL override. | Static/fake-transport Pass; public delivery Pending |
| Strict checksum | Exactly one selected basename entry with valid lowercase SHA256/two spaces; absent/duplicate/malformed/hostile lines reject; no substring/path collision; empty binary/mismatch/download failure reject. | POSIX Pass; Windows native Pending CI |
| Atomic preservation | Private temporary download, verification before same-filesystem staging/replacement; existing binary preserved on all failures; write/rename/in-use failures visible; symlink/directory/reparse destinations refused. | POSIX Pass; Windows native Pending CI |
| Paths and cleanup | Spaces and shell/PowerShell metacharacters inert; absolute destinations validated; only installer-owned temporary artifacts removed; no leftovers on success/failure. | POSIX Pass; Windows native Pending CI |
| No side effects | No execution of downloaded binary/text; no eval/source, sudo/prerequisite install, config/cache/native credentials/profile/PATH/services/db/process changes; quoted PATH guidance only. --help no network/mutation. | POSIX/version Pass; Windows native Pending CI |
| CI security | Public-only explicit job gates, main/PR/manual triggers, pinned action SHA, readonly tokens/no secrets to forks, Go from go.mod, standard runners/timeouts/retention. Actual PG full/race via existing runner plus vet/installer and native Windows/macOS gates. | Static/actionlint Pass; actual public CI Pending |
| Release publication | Strict tag trigger, exact tagged full history, required gates at same commit, separate contents:write publishing, draft/upload/verify/publish exact asset set, refuse existing release/overwrite; no arbitrary dispatch publish. | Static/fake-gh Pass; actual release Pending |
| Documentation | Dev defaults, fixed/pinned/latest install distinction, checksum integrity vs publisher signature, no signing/notarization claim, no prerequisite or native runtime overclaim. | Static Pass; root publication/history audit separately owned |
| Final local gates | Explicit engineer freeze, stable fingerprints; independent focused/full/race/vet/cross for changed boundary. Unchanged backend retains historical PG baseline locally; actual public CI PG execution separately attributed. | Pass; historical PG baseline retained |

## Probe approach and evidence boundaries

Installer subprocesses will run with temporary HOME/LOCALAPPDATA, destinations and fake curl/uname/hash/file tools as appropriate. Fixtures record requested URLs, mutations and unexpected target execution. Existing binary/config/profile sentinels must survive failures. A disposable Git clone will receive test tags and clean/dirty/output cases; the real checkout receives no tag. Production scripts and workflows remain read-only to reviewer.

Owner's bad2d9e reuse/resize/archive "all done" is recorded narrowly in PHASE-4-LIVE-ACCEPTANCE.md and TUI-ARCHIVE-REVIEW.md. It proves no release/public install, new consent, credential lifecycle, MCP or native Windows behavior. No actual CI/release result may be inferred from YAML inspection or local mocks.

Initial contract review found no contradiction requiring implementation changes. Shared CORE extension and scripts are awaited; exact observations will be sent via Codex internal queue as code lands. Heavy gates wait explicit engineer freeze.


## Probe preparation checkpoint

integration/release_version_test.go prepares real subprocess checks for both default and injected buildinfo, exact version JSON, extra-argument rejection, malformed repo/home state, no file mutation and zero requests to a local fake authority. It uses isolated HOME/state only, and performs no auth/keychain operation. Not run yet: engineer buildinfo/version dispatch is not present at this checkpoint. Native PowerShell remains unavailable; installer/build/workflow probes await their implementation. No implementation failure is inferred before it lands.

## First implementation checkpoint — independent focused Pass

Engineer buildinfo/version dispatch and scripts/install.sh, install.ps1, build-release.sh have landed; no freeze received. `go test ./integration -run '^TestRelease(VersionHasNoRuntimeDependencies|POSIXInstallerIndependent)$' -count=1 -v` **Pass**, integration 8.879s. Default and injected real version subprocesses pass with poisoned isolated state, no file mutation/authority request. First six fake-transport POSIX cases pass. Expanded `python3 integration/release_fixtures/install_posix_probe.py scripts/install.sh` **Pass**, seven test methods in 6.281s (multiple platform/invalid/manifest/failure subcases).

Independent POSIX probes cover default destination and quoted paths with spaces/single quote/shell metacharacters; strict latest-once pin/explicit no-discovery; every documented uname alias; absent/duplicate/malformed/uppercase/mismatched/hostile selected manifest, empty/download/chmod/mv failures with old binary preserved; symlink/directory refusal; help inactivity; no downloaded target execution; no other HOME-file writes; private-temp/stage cleanup. Curl arguments require fail-on-HTTP, HTTPS-only initial/redirect protocols and positive connect/overall bounds. Shell-quoted PATH guidance is parsed as data, never executed. Runtime is macOS /bin/sh with fake Linux/Darwin platform and transport; it does not establish native Linux or actual GitHub delivery.

Static initial code review: version early dispatch precedes config resolution. POSIX installer verifies complete manifest before private same-filesystem staging; Windows uses manual bounded HTTPS redirects, ACL-restricted temp/stage and File.Replace/File.Move. Native PS5.1/7 behavior is still Pending actual Windows execution. Build script/workflow acceptance is pending independent isolated fixture and final freeze. No implementation defect reproduced so far; production bytes remain engineer-owned/in progress.

Independent build-script probe is now prepared in integration/release_fixtures/build_probe.py with a Go integration wrapper. It clones locally without hardlinks, snapshots frozen source into that disposable clone, creates only fixture-local commit/tags with hooks/signing disabled, and checks exact tag/dirty/output refusal plus five real target assets, metadata and complete manifest. It never tags/stages/commits the real checkout. The builder executes no targets; the reviewer fixture runs only host-native version after the build, with isolated state, as clarified below. Its actual five-target build is intentionally unrun until freeze to avoid duplicating engineer's ongoing build work. The upcoming full/race gates will execute it against frozen source.

## Native Windows fixture / documentation checkpoint

README release section statically matches actual installer flags/defaults, pre-release availability caveat, moving/latest vs pinned POSIX commands, explicit PATH step, no installer auto-launch/sign-in, source-build defaults and unsigned/checksum-integrity limits. Actual public delivery is not yet demonstrated. Root owns documentation edits/publication history review.

Independent native fixture prepared at integration/release_fixtures/install_windows_probe.ps1; **unrun** locally because neither native PowerShell runtime is available. It dot-sources the production installer, fakes transport/platform selection for installation cases, and exercises actual native ACL/staging/replacement, file-in-use preservation and junction rejection. It checks latest-once/exact pin, strict manifest/download/empty/mismatch rejection, hostile path/text inactivity, invalid-before-fetch, temporary cleanup and unchanged PATH/config sentinel; original production architecture function is also checked with 32-bit-process/x64-host environment mapping. This mapping is a fixture, not an actual 32-bit PowerShell process claim.

Engineer received exact required native invocations: `powershell -NoProfile -File integration/release_fixtures/install_windows_probe.ps1` and `pwsh -NoProfile -File integration/release_fixtures/install_windows_probe.ps1`, alongside engineer's scripts/test_install.ps1. Workflow edits remain engineer-owned. Existing native CI steps and all public-only gates were statically observed; runtime Pass awaits actual public Windows CI. No execution policy weakening or real user Credential Manager/config access is part of these fixtures.

## Pre-freeze destination/source-override observations

O1 **Open pre-freeze observation, engineer owner**: `python3 integration/release_fixtures/install_posix_probe.py scripts/install.sh Installer.test_slash_suffixed_directory_symlink_is_refused` fails. Fixture has an existing binary in a real directory and a symlink to that directory; installer receives `--version v1.2.3 --bin-dir <destination-link>/`. Actual exit 0 and stdout reports Installed v1.2.3 at `<destination-link>//clawbridge`, rather than refusing the destination before downloads/replacement. Plain `-L "$install_bin_dir"` follows the slash-suffixed link. Expected: canonical handling must still refuse a symlink destination, preserve the original target and avoid transport. This is in-progress code, not a promoted frozen defect. install.sh observed SHA256 `ce3d6f9f0bb02f44ee229f8659067d00cccbfe6d462c06ae4c364d2ed1c45710`.

Source-override concern: current scripts/build-release.sh explicitly exports `GOFLAGS='' GOWORK=off`. Independent `python3 integration/release_fixtures/build_override_probe.py .` **Pass**, 3.898s. An isolated cloned/tagged miniature source uses real build-release.sh and external GOFLAGS overlay, GOFLAGS modfile and GOWORK dependency variants. Each successful sanitized build emits all five targets with no external marker; no target binary executes, no real-repo tag/commit is made. This is focused source-isolation coverage using synthetic minimal application code, not the pending actual application release asset acceptance. Builder observed SHA256 `9dcc815a6726df1f316dddab67aefe6ae3e453b3da85b0a7e73ca7cc035a18f4`. No current builder source-override defect reproduced. Final frozen retest still required.

O1 follow-up: engineer normalized trailing slash(es) before destination checks. Independent full POSIX fixture suite now **Pass**, eight methods in 5.328s; focused `/` and `//` symlink suffix regression additionally **Pass**, 0.179s, preserving old target and making no download. Leading/trailing newline explicit version arguments also reject before transport. O1 closed against current pre-freeze bytes, subject to frozen retest; no frozen defect promoted. Native Windows fixture also now includes trailing-newline version input; actual result remains Pending Windows CI.

## Build metadata assertion correction

Go 1.27 trimpath build metadata may omit literal -X linker flags. Independent build probe now checks cross-target OS/architecture/CGO/trimpath and matching clean VCS commit when present, exact embedded version/commit/timestamp bytes in all five binaries, exact source marker/assets/manifest, and actual host-native fixture `version` JSON for injected tag/full commit/commit timestamp. It no longer infers missing injection from omitted linker-flag text. Host-native version executes only in poisoned isolated state; no target is executed by build-release.sh, and foreign-platform runtime version remains unclaimed. Existing independent real-subprocess default/injected buildinfo probe already passed. Full five-target builder fixture still awaits explicit freeze.

## Frozen finding R2 — GOENV bypasses empty GOFLAGS source isolation

**Closed on revised frozen source; original blocking finding retained below for history. Engineer owner.** All 13 supplied frozen owned hashes independently match. Frozen builder SHA256 `9dcc815a6726df1f316dddab67aefe6ae3e453b3da85b0a7e73ca7cc035a18f4` exports GOFLAGS='' and GOWORK=off, but Go treats empty GOFLAGS as permitting its GOENV-file default.

Exact repro: `python3 integration/release_fixtures/build_override_probe.py .` (or Go wrapper `go test ./integration -run '^TestReleaseBuilderRejectsExternalSourceOverrides$' -count=1 -v`). Direct overlay/modfile/workspace subcases pass; added goenv-modfile fails in 5.085s. Isolated cloned/tagged miniature module sets GOENV to an external file containing `GOFLAGS=-modfile=<external override.mod>`. Override modfile points the fixture dependency at external source with a distinctive marker. Actual frozen build-release.sh returns 0 and all five target executables contain that external-source marker, violating clean exact-tag provenance. No generated executable is executed and no real checkout/tag changes occur. External fixture files are disposed by the test.

Read-only control: with a temporary GOENV file defining GOFLAGS, `GOFLAGS='' GOENV=<fixture> go env GOFLAGS` returns the file's modfile setting, confirming the environment fallback. Expected: refuse or fully isolate caller Go environment settings so external source cannot enter a release while clean/tag checks and source marker claim tagged bytes. Disabling caller GOENV (for example GOENV=off) is one owner implementation option; reviewer makes no production change.

At this checkpoint final heavy gates were paused until engineer correction and exact refreeze; the corrected final results below supersede that hold. Native/public CI/release gates remain Pending. Evidence directory allocated for final review: `/var/folders/h6/qh_thy6s4_n4ccxfqhc9gp080000gn/T/cb-release-final-ffz0ryj2`.

## Final revised-freeze independent verdict — local PASS

Acceptance base HEAD `a56bea6d4f82ea875c5fbe6e663357fa692f2213` with shared uncommitted release source. Initial freeze hashes were captured before R2 discovery. The supplied freeze directory was subsequently updated to revised source; root explicitly authorized current matching source/manifest acceptance. Both original and revised fingerprints remain in this review's evidence directory. All 13 revised owned fingerprints and all 91 Go/module hashes match supplied manifests and remained identical across final gates. No backend/schema/dependency implementation delta; no current PG lifecycle was run.

R2 independently closes: revised builder sets GOENV=off as well as empty GOFLAGS and GOWORK=off. Full independent source-override probe now passes overlay, modfile, workspace and GOENV-modfile cases; no external marker can enter successful output. O1 slash-suffixed symlink refusal also passes for `/` and `//`, preserving the existing target without downloads. No unresolved local implementation finding remains.

| Final command | Independent result |
| --- | --- |
| `go test ./integration -run '^TestRelease' -count=1 -v` | Pass, integration 27.974s: actual application five-target release builder 13.00s; source overrides 4.29s; POSIX 5.49s; version 4.58s |
| `python3 scripts/test_install.py` | Pass, seven methods 4.694s, native macOS shell with fake transport/platform mappings |
| `python3 scripts/test_release.py` | Pass, two methods 1.893s; synthetic tagged fixture, actual target builds/native fixture version |
| `python3 scripts/test_publish.py` | Pass, six methods 1.752s; fake gh only, no GitHub mutations |
| `go test ./... -count=1 -json` | Pass, integration 66.027s; CLI 4.516s |
| `go test -race ./... -count=1 -json` | Pass, integration 74.130s; CLI 5.926s; no race report |
| `go vet ./...` | Pass exit 0 |
| `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o <temp>/cross-darwin-arm64 ./cmd/clawbridge` | Pass; temporary output removed |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o <temp>/cross-linux-amd64 ./cmd/clawbridge` | Pass; temporary output removed |
| `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o <temp>/cross-windows-amd64.exe ./cmd/clawbridge` | Pass; temporary output removed |
| `bash -n scripts/build-release.sh scripts/publish-release.sh`; `sh -n scripts/install.sh` | Pass |
| `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7 .github/workflows/ci.yml .github/workflows/release.yml` | Pass, no findings. Initial offline attempt could not resolve Go deprecation metadata; rerun with normal public Go module lookup passed. No workflow execution occurred |
| Portable PowerShell 7.4.6 Parser.ParseFile, macOS/Unix | Pass for install.ps1, test_install.ps1 and independent install_windows_probe.ps1; syntax only, no native installer execution |
| `git diff --check` | Pass |

The independent actual-app tagged clone emits exactly five raw executables plus install.sh/install.ps1/SOURCE-COMMIT.txt/SHA256SUMS; all eight manifest entries match exact payload hashes. Every cross binary has expected target/CGO0/trimpath metadata, matching clean VCS attribution when present and embedded exact tag/full commit/UTC timestamp values. Host-native actual `version` JSON confirms active injected metadata with poisoned isolated state. Literal -X metadata is not required; foreign-platform runtime output is unclaimed. Builder itself executes no targets; only reviewer fixture host-native version is executed. Tags/commits/staging occurred only inside disposable fixtures, never the real repository.

Revised owned SHA256:

```
5dbb40819daf39c48823186af8f2dbfd821e2a64279b7834f5f06b30dab08d38  .github/workflows/ci.yml
e08b00c06fb595afe1551d8cba2150716e316033ea8ea1d3595531943fba2911  .github/workflows/release.yml
8dfbde56367c5a5a10b9e7dde25b663d45f145b52dda26424ae51a5043715fdf  internal/buildinfo/buildinfo.go
07746547e92bd573a5dca9107d29ed476ffb787dc9307d3a51e4117b0d59ba1a  internal/cli/cli.go
2b4852fc00a6aa0a05ffab3d77e8fdcb513c670b31b68ac76c3e693202c41abc  internal/cli/version_test.go
fc705b0ff586585755d8e2e138ab9a658359217a5091e12c976ecf927429c4a5  scripts/build-release.sh
965a3f34c7499d466a9664b6affa11308d0cde8390b1cddf7e65b5bc148f2f45  scripts/install.ps1
1b25bd77484fc6fa49ad0aadcff98b8215d342713f4970c42374ae2ff9a5cd47  scripts/install.sh
f0d911c4dd2671f387cfc38fc3f7fe7973ba4b695afbe3004c04bf742c1b72db  scripts/publish-release.sh
c3196b5a69c75665bcb965533f0d446dacd5bc97c285fa2c476498381c65df5c  scripts/test_install.ps1
1beaf027c12316f1184a8b104a0c74d536d49cb0f0942809529dd710f3763a62  scripts/test_install.py
b46e9b98c7c5addddf1a3b602d1c08c10212883044b820c729dbfa124c5ae8ea  scripts/test_publish.py
35f7fbcbf60c0aba2dd881583310e0fe8ee5545f1c8a5faaf20a9e92e3bd335e  scripts/test_release.py
```

Go/module manifest: 91 files, SHA256 `dcd4b8053d8e59a1e03af36e5877023794cfd2b6c58d272d68498f6ddc284d1c`, stable before/after. Complete final Go/owned/reviewer-fixture manifest: 105 files, SHA256 `af474d90cfb7098d9a45d438dd643b64f0d4feefb750b5b06bed72a49f3f2145` (final-review-manifest.json).
Evidence: `/var/folders/h6/qh_thy6s4_n4ccxfqhc9gp080000gn/T/cb-release-final-ffz0ryj2`, including original/revised manifests, R2 observation, revised-focused.log, fixture logs, final-full.jsonl/final-race.jsonl/final-results.json, actionlint logs, PowerShell parse log and no-drift result. Temporary cross outputs and disposable fixture clones/outputs were removed. No worker tag/release/visibility/deployment/live or credential operation.

### Manual/security review and actual remaining gates

Manual review covers early version dispatch before configuration; canonical tags; clean exact-tag builder and caller source-override isolation; commit-time metadata; exact asset/manifest set; fixed HTTPS/pinned downloads with byte/time/redirect bounds; strict digests; private staging/atomic replacement/preservation; symlink/reparse handling; shell/PowerShell hostile-text treatment; no payload execution or automatic app/config/PATH/native-credential mutation. Windows production code and both native fixture suites were read and parsed, including native ACL/file-in-use/junction cases, but actual Windows behavior remains untested locally.

CI YAML and publisher code review: every job is public-gated; official action uses are pinned to full commit SHAs; ordinary/read-only and publishing write permissions are separated; checkout does not persist credentials; Go follows go.mod; full/race invoke real disposable PG; macOS POSIX and native Windows PS5.1/7 include meaningful fixtures and independent Windows probes. Strict tag-only publishing requires checked commit/source marker/assets; notes come from tagged RELEASE-NOTES.md. Existing releases/drafts or API failure stop creation; failed upload/download/extra/mismatched assets never publish; only a complete downloaded exact-byte-verified draft is published. Static/fake-gh results are not actual GitHub execution. Root retains final source/history audit and all external visibility/tag/release operations.

Both final ordinary full and race runs explicitly skip 12 PostgreSQL tests plus three opt-in live HTTPS/Google/device tests. cmd/clawbridge, internal/buildinfo and internal/mirror have no test files. No native PowerShell installer fixture executes in those local Go runs. Prior actual PG17.11 baseline and exact container/volume cleanup are historical GUIDED-SETUP-REVIEW.md evidence (full 46.907s, race 54.282s); unchanged backend/dependencies do not justify repeating that lifecycle locally. Public Linux CI must actually exercise PG before its result is called Pass.

Still Pending: root publication/history completion and visibility transition; actual public Linux PG/full/race/vet and macOS/native Windows PS5.1/7 CI results; actual tag/release draft/upload/publish; isolated public latest/pinned installer+version smoke. Source signing/notarization and foreign-platform app/OAuth/native-keychain runtime are unclaimed. Owner's prior Linux reuse/resize/archive completion remains narrowly attributed and establishes no release-delivery or new consent/lifecycle/MCP acceptance. Local Pass is not a claim that v0.1.0 is published or installation from GitHub already works.

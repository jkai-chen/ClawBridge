# Release distribution contract

Delivery: v0.1.0 is published at source dd34322. Local independent review,
actual public CI/release and isolated public installer/version smoke passed;
PUBLICATION-REVIEW.md and RELEASE-REVIEW.md own results and evidence limits.

Owner requested GitHub release builds, a curl-based macOS/Linux installer and a
PowerShell Windows installer, then public repository visibility after completion.
The owner reported "all done" after the reviewed bad2d9e Linux reuse/resize/archive
guide. Record that as owner-reported completion, separately from independent
automated checks and unobserved authentication/lifecycle journeys.

Root owns this contract, CORE-API, user documentation, final publication/history
review and external release operations. cb-n-engineer owns implementation and
focused tests; cb-tester-reviewer owns independent integration probes and
RELEASE-REVIEW.md. Use Codex internal queued messages. No worker tags, releases,
visibility changes, real configuration/credential operations or cloud deployment.

## Version and asset contract

Initial stable release target is v0.1.0. Tags have exact canonical form
vMAJOR.MINOR.PATCH (decimal components, no leading zeroes except 0). No arbitrary
ref, prerelease syntax or URL is accepted as an installer version in this slice.
No tag is created before final independent review and root publication audit.

Add internal/buildinfo with exported string variables Version (default dev),
Commit (default unknown), BuildTime (default unknown), injected with go -ldflags.
`clawbridge version` and `clawbridge --version` emit JSON:

```json
{"source":"build","data":{"version":"v0.1.0","commit":"FULL_SHA","build_time":"RFC3339_UTC"}}
```

These exact invocations require no route, database, keychain, configuration,
network, project or interactive terminal. Extra version arguments fail before
those resources are used. Existing command/result contracts remain unchanged.
Document ordinary source-build dev/unknown defaults. Version metadata is display
information, not an authentication or signature claim.

Publish raw executables rather than extracting downloaded archives:

- clawbridge_v0.1.0_darwin_amd64
- clawbridge_v0.1.0_darwin_arm64
- clawbridge_v0.1.0_linux_amd64
- clawbridge_v0.1.0_linux_arm64
- clawbridge_v0.1.0_windows_amd64.exe
- LICENSE (the MIT licence), install.sh, install.ps1, SOURCE-COMMIT.txt, SHA256SUMS

Replace v0.1.0 with the exact release tag for later versions. SHA256SUMS contains
one lowercase SHA256 and two spaces plus the exact basename for every other
published payload. SOURCE-COMMIT.txt contains the full tagged commit plus newline.
Do not include credentials, configuration, cached database, test launchers or
machine-specific state in release assets. macOS signing/notarization and installer
packages are outside this increment; do not imply those properties.

## Release build and workflows

Provide scripts/build-release.sh taking VERSION and an output directory. Validate
the tag syntax; require an exact matching Git tag at a clean source commit for
release builds. Build all five targets with CGO_ENABLED=0 and -trimpath, injecting
the exact tag/full commit/commit timestamp (UTC RFC3339). Version and commit must
not be read from mutable working-tree files. Ignore caller GOOS/GOARCH overrides
when selecting each target. Refuse a dirty tracked/nonignored source set and
ambiguous pre-existing output payloads. Put outputs outside the source checkout
or into an explicitly ignored release-output directory, so repeat validation
does not count generated assets as source. Copy installers and source marker;
generate the complete deterministic checksum manifest. Do not run target binaries
in the build script. Script itself does not create/push tags or releases.

Provide GitHub Actions CI for public push main, pull_request and manual dispatch:
read-only contents permissions, standard ubuntu-latest runner, pinned action
commit SHAs (not floating tags), Go from go.mod, bounded timeout. Run actual
PostgreSQL full/race via existing integration/run-postgres.sh, plus vet and
installer tests. Add native PowerShell tests on standard windows-latest and
POSIX installer tests on standard macos-latest as needed. Explicitly gate jobs on
repository public status to avoid private hosted-runner consumption before the
authorized visibility transition. Fork PRs receive no write token/secrets.

Provide tag-push release workflow (v*.*.* plus strict validation), also gated to
public repository. Checkout exact tag/full history and build the tagged tree.
Run required full/race/vet/installer gates at that commit before publishing.
Separate read-only testing/build permissions from the publish job's contents:write.
Publish only the exact reviewed asset set. Create a draft first, upload/verify all
assets, then publish; refuse to overwrite an existing release or replace assets
silently. A failed/incomplete upload may leave a clearly identified draft for
root recovery, never a partial published release. No arbitrary workflow-dispatch
ref can publish. Avoid long-lived credentials or third-party release actions;
GitHub's job token and gh CLI are sufficient. Use short artifact retention.

Root performs local acceptance while private, reviews the final tree and all
reachable Git history for private artifacts, commits/pushes the accepted code,
then makes the repository public under the owner's existing authorization.
Dispatch public CI, observe its actual result, then create/push v0.1.0 at that
accepted commit. Observe release workflow and verify published assets before
claiming a working public install. Do not claim public CI execution from static
workflow inspection, or native runtime acceptance from cross-compilation.

## POSIX installer

Ship scripts/install.sh (copied as release asset install.sh). It works under
POSIX sh with existing curl, uname, mktemp and sha256sum or shasum. It never
installs prerequisites or invokes sudo. Usage: `sh install.sh [--version vX.Y.Z]
[--bin-dir ABSOLUTE_PATH]`; --help performs no network/filesystem mutation.
Default destination is $HOME/.local/bin/clawbridge. Validate options, absolute
destination and supported platform before fetching or creating destination state.
Support Darwin/Linux x86_64/amd64 and arm64/aarch64; reject others explicitly.

Without --version, discover the current stable GitHub release once, then pin its
strict validated tag for all subsequent manifest/binary downloads. A versioned
invocation skips latest discovery. All production URLs are fixed HTTPS routes
under jkai-chen/ClawBridge; no arbitrary mirror/base URL, token or command-string
input. HTTPS redirects for release asset delivery are allowed, HTTP downgrade
is not. Use bounded connection/overall timeouts and fail on HTTP errors.
Tests stub transports/platform calls; production needs no test URL override.

Require exactly one valid SHA256SUMS entry for the selected exact basename;
reject absent, duplicate, malformed hash/entry, empty binary, mismatch or failed
downloads. Treat downloaded text as data; never eval/source/execute it. Download
to private temporary storage, verify first, then stage and atomically replace
only the binary in its destination filesystem. Set executable permissions.
Refuse destination symlinks/directories and preserve an existing binary on any
failure before replacement. Clean only installer-owned temporary files. Paths
with spaces must work. No mutation of ClawBridge config/cache/credentials, shell
profiles, global PATH, services, databases or running processes. Print installed
version/path and a shell-quoted PATH instruction when necessary. Do not execute
the installed binary, open a browser or initiate setup/sign-in automatically.

After the first release exists, document both a simple latest installer URL and
a pinned v0.1.0 URL using curl --fail over HTTPS piped to sh, with the optional
explicit --version argument. Clearly distinguish the moving convenience entry
from the pinned binary release. Checksum establishes artifact integrity against
the fetched release manifest; it is not a separate publisher signature.

## Windows installer

Ship scripts/install.ps1 (copied as release asset install.ps1), supporting Windows
PowerShell 5.1 and PowerShell 7 on Windows x64. Parameters: -Version vX.Y.Z and
-InstallDir ABSOLUTE_PATH. Default destination is
$env:LOCALAPPDATA\Programs\ClawBridge\clawbridge.exe. Reject unsupported native
OS/architecture, invalid paths/tag/unknown parameters before network or mutation.
Detect native x64 even under a 32-bit PowerShell process; do not claim ARM support.

The same latest-once/exact-tag, fixed HTTPS repository, bounded download, strict
single checksum entry, no remote code execution, private staging, same-volume
rename replacement, existing-binary preservation, cleanup and no-auto-launch
rules apply. Use Get-FileHash SHA256 and .NET/native file primitives compatible
with both shells. Refuse destination reparse points/symlinks/directories. A file
whose replacement Windows refuses must stay intact, failing before success is
reported; a running binary that can only be renamed is replaced, and its renamed
copy is removed best effort. Append the install directory to the per-user PATH
(HKCU\Environment\Path) once by default, preserving the value's kind and
unexpanded %VARIABLES%; -NoModifyPath opts out and prints the manual step. Never
change the system PATH or a PowerShell profile.
Existing configuration and Credential Manager entries stay untouched.

## Independent acceptance and publication

Test version output against malformed/inaccessible route input and isolated
state; test built version metadata through a real local subprocess. Installer
fixtures cover platform mapping, one latest discovery and tag pinning, explicit
version, paths with spaces, existing-binary success and preservation on malformed
or duplicate manifest/mismatch/download/write failures, symlink refusal, no target
execution, hostile payload text, no extra config/credential/profile writes and
temporary cleanup. Run POSIX tests on Linux/macOS and PowerShell tests on native
Windows in CI; explicitly attribute unavailable local runtimes as Pending until
actually run. Meaningful release tests assert all asset names, metadata and
manifest coverage at an isolated tagged clean clone; no tag on the real repo.

After engineer freeze, reviewer owns final focused/full/race/vet/cross checks
appropriate to the version-command and packaging delta. No backend changes are
planned; retain historical PG evidence locally unless a concern justifies a new
fixture lifecycle. Public CI itself runs actual PG gates. Root reviews all source
and reachable Git history, publishes only after review, and verifies actual
release/installer smoke results using an isolated binary directory. First-release
smokes may run version only, with no app configuration/credential/sign-in access.

Official references checked 21 October 2026: GitHub's
[release API](https://docs.github.com/en/rest/releases/releases),
[release management](https://docs.github.com/en/repositories/releasing-projects-on-github/managing-releases-in-a-repository),
and [Actions billing](https://docs.github.com/en/billing/concepts/product-billing/github-actions).
Standard hosted runners are free for public repositories; no claim is made about
other billed features or account-specific allowances.

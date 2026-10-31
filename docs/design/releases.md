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
- clawbridge_v0.1.0_windows_arm64.exe
- LICENSE (the MIT licence), install.sh, install.ps1, SOURCE-COMMIT.txt, SHA256SUMS
- PROVENANCE.sigstore.json: the Sigstore bundle of the assets' build-provenance
  attestation, outside SHA256SUMS (it attests SHA256SUMS too). A client verifies
  an asset with it and no API call or token (FR24 automatic updates).
- MANUAL-UPDATE-REQUIRED, only when the release's notes carry the marker (below):
  automatic updates never install that release.

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

The release workflows:

- `.github/workflows/release.yml`, on a `v*.*.*` tag push: checks, build, then the
  publish job (image, attestations, `scripts/publish-release.sh VERSION DIR`).
- `.github/workflows/publish-draft.yml`, dispatched by hand with input `version`:
  publishes a draft that release.yml held, with no rebuild and no upload. It runs
  under the same `release` environment, checks out the tag, requires its commit on
  `main`, and runs `scripts/publish-release.sh --publish-draft VERSION`. That
  refuses anything but a dispatch from the tag itself (`--ref VERSION`) at the
  tagged commit, then anything but a draft of exactly that tag, then validates the
  draft's own assets as the tag-push run did: the asset set, SHA256SUMS,
  SOURCE-COMMIT.txt, the bundle's subjects, an `IMAGE-DIGEST.txt` of exactly
  `ghcr.io/jkai-chen/clawbridge@sha256:<64 hex>` (and `AGENTD-IMAGE-DIGEST.txt` and
  `AGENT-IMAGE-DIGEST.txt` of exactly their image's, which the body then names),
  and `MANUAL-UPDATE-REQUIRED`
  present (and as the tagged notes make it) exactly when those notes carry the
  marker. Then the bundle's signature: `gh attestation verify SHA256SUMS --bundle
  PROVENANCE.sigstore.json` must show release.yml's signature for this tag
  (`--signer-workflow`, `--source-ref refs/tags/VERSION`), at the tagged commit
  (`--source-digest`), SLSA v1 provenance, on a GitHub-hosted runner
  (`--deny-self-hosted-runners`). SHA256SUMS is one of the attested subjects and
  covers every other asset, so a draft edited by anyone with contents:write, even
  with a consistent SHA256SUMS and a forged bundle, is refused. Only then does it
  decide Latest and publish, as a tag push does, setting the body again from the
  tagged notes and the draft's image as the tag-push run wrote it, so an edited
  draft body is not published. A dispatch can publish only a draft that a
  tag-push run made; it never builds or uploads.

The repository variable `RELEASE_HOLD_DRAFT` (Settings → Secrets and variables →
Actions → Variables, or `gh variable set`) is passed to the publish step of
release.yml. `true` ends the run at the verified draft with `held as draft:
<tag>; image <image>; publish with the publish-draft workflow`; unset, empty or
`false` publishes in the same run, as before. Any other value fails the run before
anything is created. The image is pushed and attested either way; a draft is not
`releases/latest`, so installers and `self-update` do not see a held release.

Before anything builds, the release workflow requires the tagged commit to be an
ancestor of `main` (`git merge-base --is-ancestor` after fetching `main`), and
the publish job checks it again just before publishing. A tag on a commit that
never reached `main` fails without a draft. Publication marks the release as
Latest only when its version is higher than every published stable release;
drafts, prereleases and noncanonical tags are ignored. So a backport such as
v0.1.4 after v0.2.0 is published without moving `releases/latest`, which the
installers follow. If the release list cannot be read, the verified draft stays
unpublished.

The publish job runs in the `release` GitHub Environment so publication can wait
for a human approval. This needs a one-time repository setting by the owner:
Settings → Environments → New environment `release` → Required reviewers (the
owner), and under Deployment branches and tags, allow only tags matching
`v*.*.*`. Until that environment exists with reviewers, GitHub creates it
unprotected on first use and the job runs without waiting.

These checks guard against mistakes, not against someone with write access. A
tag push runs the workflow file as it is at the tagged commit, so whoever can
push a `v*` tag can point it at a commit whose workflow drops the `on-main` job
or the `environment:` line, and publish without approval. The boundary is who
may create `v*` tags, enforced by a repository tag ruleset (owner, one-time):
Settings → Rules → Rulesets → New ruleset → New tag ruleset:

- Ruleset name `release-tags`, Enforcement status **Active**.
- Bypass list: **Repository admin** only (the owner), with "Always allow".
- Target tags: Include by pattern `v*`.
- Rules: **Restrict creations**, **Restrict updates**, **Restrict deletions**
  and **Block force pushes**.

With that ruleset only the owner can create, move or delete a release tag, so the
workflow file a release runs is always one the owner chose to tag.

Root performs local acceptance while private, reviews the final tree and all
reachable Git history for private artifacts, commits/pushes the accepted code,
then makes the repository public under the owner's existing authorization.
Dispatch public CI, observe its actual result, then create/push v0.1.0 at that
accepted commit. Observe release workflow and verify published assets before
claiming a working public install. Do not claim public CI execution from static
workflow inspection, or native runtime acceptance from cross-compilation.

## The server image

Each release also publishes the server image, `ghcr.io/jkai-chen/clawbridge:vX.Y.Z`. It is a multi-arch index for
linux/amd64 and linux/arm64, built from the repository's `Dockerfile` in the same `publish` job, behind the same
`release` approval.

- **Permissions:** only that job has `packages: write`, `id-token: write` and `attestations: write`. It logs in with
  its own `GITHUB_TOKEN`; there is no other credential.
- **Order:**
  1. The job refuses if any release for the tag exists, draft or published, before it logs in or pushes. So the tag
     never moves under a release.
  2. It builds and pushes.
  3. It attests the image (`actions/attest-build-provenance`, with the bundle pushed next to the image).
  4. It writes `IMAGE-DIGEST.txt` (`ghcr.io/jkai-chen/clawbridge@sha256:…`) into the assets and SHA256SUMS, and
     attests every asset.
  5. `publish-release.sh` refuses to run without that digest. It appends a "Server image" section with the digest
     and its `gh attestation verify` command to the release body.
- **Re-runs:** builds aren't byte-reproducible, so a re-run after a failure before the draft pushes a new digest,
  which is the one the release names. The first digest stays orphaned, with its attestation.
- **The agent images.** The same job, after the server image and the same release check, builds, pushes and attests
  two more images the same way (two platforms, one tag per release, never `latest`, BuildKit's SBOM, no build args):

  | Image | Built from | Digest asset |
  | --- | --- | --- |
  | `ghcr.io/jkai-chen/clawbridge-agentd:vX.Y.Z`: agentd's image, which is also the egress container's | `cmd/clawbridge-agentd/Dockerfile`, context the repository root | `AGENTD-IMAGE-DIGEST.txt` |
  | `ghcr.io/jkai-chen/clawbridge-dispatcher-agent:vX.Y.Z`: the dispatcher agent | `dispatcher-agent/` | `AGENT-IMAGE-DIGEST.txt` |

  Each digest asset holds one line, `ghcr.io/jkai-chen/<name>@sha256:…`, is in SHA256SUMS and is attested with the
  other assets. `publish-release.sh` refuses to run without both digests (`AGENTD_IMAGE_DIGEST`,
  `AGENT_IMAGE_DIGEST`) and checks each asset names exactly its pushed digest, before the draft and again after
  download. The release body names them too, in an "Agent images" section after the server image's, each with its
  `gh attestation verify` command.
  The agent's arm64 image runs its npm and RUN steps under QEMU, so the job's timeout is 60 minutes; agentd's
  image cross-compiles, as the server's does.
- **Verifying an image.** Take the digest from the release's asset (`IMAGE-DIGEST.txt`, `AGENTD-IMAGE-DIGEST.txt` or
  `AGENT-IMAGE-DIGEST.txt`), and on any machine with `gh`:

  ```sh
  gh attestation verify oci://ghcr.io/jkai-chen/clawbridge-agentd@sha256:<digest> --repo jkai-chen/ClawBridge \
    --signer-workflow jkai-chen/ClawBridge/.github/workflows/release.yml
  ```

  The same command, with the other name, verifies the server or the agent image. A deploy pins each by that digest
  (operations/agentd.md for the two agent images).
- **Tags and notes:**
  - Each release has exactly one tag. There is never a `latest`, and nothing is retagged or deleted.
  - The release body is not the whole RELEASE-NOTES.md (GitHub refuses a body of 125000 characters or more): it is the
    release's own top section, up to the next top-level `# ` heading, then a link to the full notes at the tag, then the
    server image. `scripts/release_body.py` builds it, and refuses one over 120000 characters before any release
    exists. Every PR and main CI run checks today's notes the same way (`scripts/test_publish.py`), so an oversize
    section fails long before a tag.
  - **The top heading names the release.** At the tag, the notes' first line must be exactly
    `# ClawBridge VERSION (released DATE)` for the version being released. `release_body.py` refuses anything else,
    such as `# Unreleased`, another version's heading, or a heading with no `(released …)`, so a tag on notes the
    cut didn't rename stops before anything is created. Both the tag-push run and `publish-draft` build the body
    this way. CI checks only the size, since main's notes start with `# Unreleased` between releases.
  - **Manual-update marker.** A release whose client change needs a person's
    attention, such as a sign-in method going away, starts a line of its top
    section with `**Manual update required.**`, followed by the reason. Only a
    line start counts, outside code fences and in this release's section only.
    `publish-release.sh` then adds the asset `MANUAL-UPDATE-REQUIRED`, which
    holds the version and that line, and automatic updates (`self-update --auto`)
    skip the release, while a manual `self-update` still installs it. Leave the
    marker out otherwise; it holds every auto-updating client back.
  - **Provenance bundle.** The assets' attestation bundle (attest-build-provenance's
    `bundle-path`) is published as `PROVENANCE.sigstore.json`. Before any release
    exists, `publish-release.sh` checks that the bundle is a signed in-toto SLSA
    v1 statement whose subjects are exactly the assets in SHA256SUMS plus
    SHA256SUMS, by digest. It checks the uploaded bundle the same way after
    download.
  - **Dry run in CI.** `scripts/test_publish.py` runs `publish-release.sh` on a fake
    tag with a fake `gh` in every PR and main CI run. It asserts the exact uploaded
    asset list (with and without the marker), the bundle's subjects, and the
    body's size, so the one-shot publish path is exercised before a tag.
  - RELEASE-NOTES.md never carries the digests: the tagged notes are part of what is built. The release body and
    the three digest assets do.
- **What the image is:**
  - Its binary is stamped with the tag, the commit and the commit's time, the same as the release binaries.
  - It is still its own build of that commit, not the binaries' bytes.
  - It carries BuildKit's SBOM.
- **Signing:** provenance is the Sigstore keyless attestation alone; there is no separate cosign signature.
  `gh attestation verify oci://… --repo jkai-chen/ClawBridge --signer-workflow
  jkai-chen/ClawBridge/.github/workflows/release.yml` checks it, and `cosign verify-attestation
  --new-bundle-format` verifies the same bundle.
- **Base images:**
  - Both bases are pinned by their multi-arch index digest.
  - Before each cut, run `bash scripts/check-image.sh bases --strict`. It fails when a base has moved; refresh the
    digest (`docker buildx imagetools inspect <tag>` prints the index digest) in its own PR.
  - `bases` also checks the backup script's `aws_image` (deploy/backup-postgres.sh): pinned by a multi-arch index digest,
    like the bases. Its version is chosen on purpose, so it isn't compared with a tag; bump it by hand.
  - CI runs `check-image.sh bases` and `arch` on every PR that changes code. `arch` builds both platforms and checks each binary and
    each base's packages.
- **The package's visibility (owner, one-time, after the first push):**
  - The first push creates the package private, so its tag exists before anyone can pull it. That is harmless, since
    the digest is attested, but it means a failed release can leave a private `vX.Y.Z` image behind.
  - Then: github.com → your profile → Packages → `clawbridge` → Package settings → Danger Zone → Change visibility
    → Public.
  - **Maintainer note:** `clawbridge-agentd` and `clawbridge-dispatcher-agent` are new packages from the first
    release that pushes them: set each public and check it is linked to `jkai-chen/ClawBridge` once, the same way.
  - Check that "Repository source" shows `jkai-chen/ClawBridge`. The image's `org.opencontainers.image.source`
    label links it.
  - Until it is public, deployments use the build-from-source path, or log the host in with `docker login ghcr.io`
    and a token that has only `read:packages`.

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
PowerShell 5.1 and PowerShell 7 on Windows x64 and ARM64. Parameters: -Version vX.Y.Z and
-InstallDir ABSOLUTE_PATH. Default destination is
$env:LOCALAPPDATA\Programs\ClawBridge\clawbridge.exe. Reject unsupported native
OS/architecture, invalid paths/tag/unknown parameters before network or mutation.
Detect the native architecture (x64 or ARM64) even under a 32-bit or emulated PowerShell process,
through IsWow64Process2 with the environment variables as fallback, and pick the matching asset.

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

## Before each cut

Two checks, each a separate PR when it changes anything, merged before the tag:

- **Base images:** `bash scripts/check-image.sh bases --strict` (see [The server image](#the-server-image)).
- **Cost tables:** the playbooks' monthly estimates (`skills/clawbridge-provision-*/references/cost.md`, and the
  summary in [first-deployment.md §2](../operations/first-deployment.md#2-estimate-the-cost)) are checked against
  the providers' current list prices for the regions and sizes they name. A row that moved by more than about 10%, or
  a size the provider retired, is updated in all of them so they agree. The PR says which prices were checked and
  on what date; a table nobody could check says so in its PR rather than changing silently.

The cut commit itself renames the notes' `# Unreleased` heading to `# ClawBridge vX.Y.Z (released D Mon)`; the tag
goes on that commit, or the release run stops (see "The top heading names the release" above).

## Releasing v1.0 (hold, soak, publish)

v1.0.0 is tagged once, held as a draft, soaked on its own image, and published
after the gate review. Nothing is rebuilt or restamped between the soak and the
publication: **the published bytes are the soaked bytes**, binaries and image
alike, and the binaries and the image report v1.0.0 from the start.

Sunday, at the freeze:

1. Set the hold: `gh variable set RELEASE_HOLD_DRAFT --body true`.
2. Tag v1.0.0 at the freeze commit, whose notes start `# ClawBridge v1.0.0 (released …)`, and push the tag.
3. Approve the `release` environment when the run asks.
4. The run ends with `held as draft: v1.0.0; image ghcr.io/jkai-chen/clawbridge@sha256:…;
   publish with the publish-draft workflow`. The draft is not public and not Latest.
5. The tester reads the image from the draft's `IMAGE-DIGEST.txt`
   (`gh release download v1.0.0 --pattern IMAGE-DIGEST.txt`) and, before deploying it,
   verifies the image's attestation:

   ```sh
   gh attestation verify oci://ghcr.io/jkai-chen/clawbridge@sha256:<digest> --repo jkai-chen/ClawBridge \
     --signer-workflow jkai-chen/ClawBridge/.github/workflows/release.yml \
     --source-ref refs/tags/v1.0.0 --predicate-type https://slsa.dev/provenance/v1
   ```

   If agentd's images are deployed too (they stay stopped through the soak), read
   `AGENTD-IMAGE-DIGEST.txt` and `AGENT-IMAGE-DIGEST.txt` the same way and verify each
   before loading it, with the same flags:

   ```sh
   gh attestation verify oci://ghcr.io/jkai-chen/clawbridge-agentd@sha256:<digest> --repo jkai-chen/ClawBridge \
     --signer-workflow jkai-chen/ClawBridge/.github/workflows/release.yml \
     --source-ref refs/tags/v1.0.0 --predicate-type https://slsa.dev/provenance/v1
   gh attestation verify oci://ghcr.io/jkai-chen/clawbridge-dispatcher-agent@sha256:<digest> --repo jkai-chen/ClawBridge \
     --signer-workflow jkai-chen/ClawBridge/.github/workflows/release.yml \
     --source-ref refs/tags/v1.0.0 --predicate-type https://slsa.dev/provenance/v1
   ```

   Then the tester deploys that digest, and the 24 h soak starts.

Monday, after the gate review:

1. Dispatch publish-draft from the tag: `gh workflow run publish-draft.yml --ref
   v1.0.0 -f version=v1.0.0`.
2. Approve the `release` environment.
3. The run validates the draft again and publishes it; v1.0.0 becomes Latest.
4. **Then unset the hold:** `gh variable delete RELEASE_HOLD_DRAFT`, so the next tag
   publishes in its own run again.

If the soak fails: fix it, then cut **v1.0.1** the same way, with the hold still
set (preferred). Delete the v1.0.0 tag, its draft and its GHCR tag only if v1.0.0
was never published; a published release is never deleted or replaced.

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

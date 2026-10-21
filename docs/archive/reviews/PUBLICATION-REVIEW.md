# Public repository and first-release review

Status: **Public repository, successful hosted CI/release, and actual public
installer/version smoke Pass.** Root owns this review and
external publication; workers must not change visibility, push tags or releases.
The owner authorized public visibility after completed work, with a final tree
and Git-history review first. No additional license has been selected or added.

## Initial history inspection

Baseline a56bea6, 21 October 2026: enumerated all 16 reachable commits and 285
named Git objects; inspected all 220 named blobs. Filename checks found no
tracked environment/credential files, private keys, database snapshots, dumps,
logs or transcript artifacts. Content checks covered private-key markers, Google
access/refresh tokens and installed-app credential forms, GitHub token forms,
cloud access-key IDs, OpenAI key forms and service-account private-key fields.

One private-key marker matched integration/store_test.go:106. Manual review
confirms TestSecretRejectionAndAtomicFailure uses literal synthetic-data and
constructed repeated-character token examples to verify rejection/redaction.
It contains no working key. No other content-pattern hits. Counts and locations
are retained in the local review evidence, without copying potential credential
values to logs. Pattern matching is a screening aid, not an exhaustive secret
detector; root also reviews final changes before visibility transition.
The same sensitive-marker scan of all 16 commit metadata/message objects found
no matches. There were no existing release tags at initial inspection.
Historical blob inspection also checked NUL bytes and executable/archive/SQLite
magic signatures; no compiled binaries, archives or database candidates were
found among the named blobs.

Existing operational review/runbook documents contain non-secret pilot service
addresses, cloud-resource names, attributed owner identity, local evidence paths
and deployment fingerprints. These are distinct from credentials and are part
of the existing versioned publication set. No real environment files, keychain
material, OAuth tokens or generated acceptance bundles are included.

## Final gates

- Release implementation and independent local acceptance: Pass RELEASE-REVIEW.
- Native PowerShell/macOS installer checks: Pass public CI 39647665688.
- Final source/asset set and pushed reachable-history screening: Pass.
- Commit/push accepted source while private: Pass dd3432242b9656bd4bad0aa8dfc9d963c5f38628.
- Root public visibility transition, under existing owner authorization: complete.
- Actual public CI result: Pass 39647665688; release workflow/assets: Pass 39648840179.
- Isolated install/version smoke against published assets: Pass, root Darwin/arm64.

Final working-tree screening at the revised implementation freeze covered 149
publication files and all 126 distinct historical tree paths. It again found
only the synthetic private-key test marker; no binary/NUL-bearing source file,
credential/private-artifact filename or other credential-pattern hit. Evidence
is retained locally. Final commit/history rescan remains below the publication
gate, after independent acceptance.

All four workflow action SHAs independently match their official actions/* tag
refs (checkout v4.2.2, setup-go v5.5.0, upload-artifact v4.6.2 and download-artifact
v4.3.0). Repository metadata confirms root has admin/push access, Actions is
enabled, and the active GitHub authorization includes repo/workflow scopes;
credential values were not printed. These checks establish available controls,
not actual hosted workflow execution or acceptance.

Root pushed dd34322, independently rechecked all 105 accepted review hashes and
rescanned all 17 reachable commits / 249 blobs. Only the known synthetic test-key
marker remained; no other sensitive-marker or binary candidate appeared. The
private push CI run 39647502521 completed Skipped, consistent with the public-only
job gates. Root then changed repository visibility to public using the owner's
existing explicit authorization and verified isPrivate=false. A first dispatch
returned HTTP 500 during the transition and created no run; after inspecting run
history, one retry succeeded. Actual public CI
[39647665688](https://github.com/jkai-chen/ClawBridge/actions/runs/39647665688)
targets the exact accepted commit. Its outcome is pending, not inferred from
static checks. No release tag exists yet.

The public CI run subsequently completed Success for Linux, macOS and Windows.
Linux executed full and race suites with real disposable PostgreSQL plus vet and
installer/build/publication fixtures; Windows ran both engineer and independent
fixtures under Windows PowerShell 5.1 and PowerShell 7. Root rechecked all 105
accepted fingerprints, then created/pushed annotated v0.1.0 at the same exact
commit dd34322. Release run
[39648840179](https://github.com/jkai-chen/ClawBridge/actions/runs/39648840179)
is now executing the tag gates/build/draft verification transaction. No release
delivery or installer smoke Pass is claimed before its actual completion.

## Published v0.1.0 and actual installer smoke

[Release v0.1.0](https://github.com/jkai-chen/ClawBridge/releases/tag/v0.1.0)
is published (draft=false, prerelease=false). Release run 39648840179 completed
Success for Linux/Windows/macOS checks, exact-tag build and publishing. Nine
assets are present: five raw target executables, install.sh/install.ps1,
SOURCE-COMMIT.txt and SHA256SUMS. The production publisher downloaded all uploaded
assets while the release was a draft, checked exact set/source marker/manifest
coverage and hashes, compared bytes, then published. RELEASE-REVIEW.md owns
independent hosted/asset evidence and its exact boundaries.

Root then used public HTTPS downloads, without a GitHub token, to verify the
installer and source marker against the manifest and install to two temporary
destinations: explicit v0.1.0 into a path with spaces, and latest into an isolated
HOME's default .local/bin. Both selected Darwin/arm64 binaries matched the
release digest. Both `version` and `--version` returned source=build,
version=v0.1.0, commit=dd3432242b9656bd4bad0aa8dfc9d963c5f38628 and
build_time=2026-10-21T17:51:13Z. A malformed config sentinel remained unchanged;
installer temporary storage was empty after completion. Temporary roots were
removed. App startup/sign-in/credential operations were not run. Native Windows
installer primitives are covered by actual CI fixtures; this public network/app
smoke is specifically Darwin/arm64, not Windows/Linux app/OAuth certification.
The preserved sentinel was under .config/clawbridge, whereas the active default
config is .clawbridge/config.toml. It proves preservation of that file, not an
active malformed-config bypass in this smoke. Independent pre-release version
probes covered poisoned active state; no repeat test is required for this
nonblocking attribution limit.

The moving latest installer URL was separately fetched and SHA256 matched the
tagged installer: 1b25bd77484fc6fa49ad0aadcff98b8215d342713f4970c42374ae2ff9a5cd47.
Evidence: local clawbridge-public-install-smoke-result.json and
clawbridge-latest-bootstrap-result.json; root smoke harness retained locally.
No existing user application configuration, cache/database, native credential or
cloud service was changed by these release checks.

Reviewer independently downloaded all nine public assets and verified every
downloaded hash against GitHub digest metadata, all eight payload hashes against
the manifest, source/installer bytes and release notes against the tag, and all
five static target/build metadata sets. Manifest SHA256:
f05dcc8bb9a868bf9152ac279f4ea0486fd2007bb6277a0b0805cf3e0aa95205.
This final external verification is recorded in RELEASE-REVIEW.md; no executable
was run or rebuilt by reviewer.

Final publication reconciliation changes only documentation; the accepted 105
source/script/probe fingerprints and the v0.1.0 tag remain unchanged. Its commit
uses `[skip ci]` to avoid repeating unchanged checks, following
[GitHub's documented skip instruction](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/skip-workflow-runs).

Standard GitHub-hosted runners for public repositories are free according to
[GitHub's Actions billing documentation](https://docs.github.com/en/billing/concepts/product-billing/github-actions).
Workflows are designed to skip private repositories before runner allocation.
This is not a claim about account-specific quotas or unrelated billed features.

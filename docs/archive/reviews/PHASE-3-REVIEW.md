# Phase 3 independent acceptance review

Status: **Pass for the explicitly frozen automated/local and deployed non-interactive Phase 3 acceptance scope. No implementation defect was reproduced. Interactive browser/device consent, real-Google keychain login/logout, reboot/lifecycle recovery, and scheduled backup/restore jobs remain Pending by the owner-requested pause.**
Reviewer: cb-tester-reviewer. Final local checkpoint: 20 October 2026, 22:39 IST.

The owner reopened the client-integration gate after the Compose/public-config
checkpoint. The earlier synthetic discovery/login revision and its local full
gates passed, but the live Google client exchange exposed a blocking
compatibility defect: the deployed public contract supplied client IDs without
the secrets Google required for the token endpoints. The owner then selected
Option A and froze a replacement four-field installed-app credential contract.
cb-engineer subsequently froze the replacement. The final local, PostgreSQL and
non-interactive deployed evidence below supersedes the interim Open state.
Synthetic, live Google, real keychain, deployed Compose/Caddy and owner-supplied
evidence remain labeled separately.

### R-live — four-field public credential fix

The owner selected Option A for LIVE-1 and froze this replacement public
discovery contract:

```json
{
  "browser_client_id": "...",
  "browser_client_secret": "...",
  "device_client_id": "...",
  "device_client_secret": "..."
}
```

These are distributed native-app application credentials. They may be returned
by the public discovery endpoint so first-time login works, but every CLI,
doctor, installer and MCP path must avoid logging or echoing their values.
Browser login must pair `browser_client_id` with `browser_client_secret`. Device
login must pair `device_client_id` with `device_client_secret`, then atomically
promote the device credential into active `oauth_client_secret`. The server reads
the two installed-app secret values from its process environment; they must not
appear in the server command or process argv.

Reviewer-owned integration expectations now require the exact four-field shape
and exact name-to-value pairing. The generic discovery client preserves an
omitted unrelated pair as two empty strings; the selected/applicable pair must
be non-empty before acquisition. The deployed live fixture advertises both flows,
so its live probe requires all four values to be non-empty. The live Google probe
sends each discovered secret only with its matching client ID and requests no
consent.
Compose checks require the two API environment variables and reject their presence
in command argv or other services. Routing checks include distinct synthetic
browser/device credential sentinels and reject them from installer JSON, MCP argv
and environment. Synthetic public-config/401 and Compose tests pass on the current
partial working edit.

Before the frozen-state redeployment, the live endpoint returned the old two
fields and the updated probe failed before contacting Google. That evidence is
preserved as the historical pre-fix result:

```sh
CLAWBRIDGE_TEST_PUBLIC_URL=https://34-142-66-102.sslip.io \
CLAWBRIDGE_TEST_LIVE_GOOGLE_OAUTH=1 \
go test ./integration \
  -run '^TestLiveGoogleDiscoveredClientsExchangeWithMatchingSecrets$' \
  -count=1 -v
```

```text
public config has 2 fields, want exactly four public OAuth app credential fields
```

The engineer subsequently froze the four-field implementation. Independent
post-deployment probes now pass: the public endpoint has the exact four fields,
each matching secret reaches its corresponding Google token endpoint, and a
sanitized no-gcloud device login reaches recurring polling. The owner has
deliberately paused before interactive consent and real-keychain lifecycle work.

| R-live gate | Current state | Evidence / next independent check |
| --- | --- | --- |
| Exact public shape and name/value pairing | Pass | Synthetic tests require exact four-field pairing. The deployed endpoint independently parsed as exactly four non-empty string fields with matching Google client-ID shape, without printing any value. Composition with the matching-secret token probes establishes production pairing |
| Browser secret use and promotion | Pass at synthetic/auth boundary; live consent Pending | The browser token exchange uses `browser_client_secret`; successful synthetic login atomically persists active ID, matching audience and matching secret while preserving an explicit device pair. Interactive browser consent was not started by owner direction |
| Device secret use and promotion | Pass; live consent Pending | Synthetic acquisition/polling and runtime refresh use the matching device secret; successful login atomically promotes ID and secret and leaves no stale Desktop secret. The deployed no-gcloud probe reaches recurring Google polling. Approval and completed device login were deliberately not started |
| Keychain/config rollback and logout | Pass synthetic; real keychain lifecycle Pending | New-entry cleanup, replacement restoration, promoted-device-key logout and repeated logout pass with isolated synthetic keychains. A real Google-refresh-token keychain round trip remains in the paused live checklist |
| Secret redaction | Pass | Distinct browser/device sentinel scans cover failing CLI/provider output, installer result, doctor/MCP artifacts, serialized MCP argv and explicit environment. Reviewer assertion failures no longer print response values. Compose requires secrets in API environment and rejects them from argv and unrelated services |
| Server secret input | Pass | Compose renders `CLAWBRIDGE_OAUTH_CLIENT_SECRET` and `CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET` only in API environment; server argv contains IDs but no secret. Half-configured pairs fail before Postgres |
| Live public config | Pass | Independent live test and root evidence against the frozen deployment found exactly four non-empty keys: `browser_client_id`, `browser_client_secret`, `device_client_id`, `device_client_secret`; values were not printed |
| Live token-client compatibility | Pass | Independent dummy device-code and authorization-code exchanges passed only with each discovered field paired to its matching client ID, reaching Google's expected grant rejection rather than missing/mismatched-secret failure |
| No-gcloud device acquisition | Pass through polling | With sanitized `PATH`, isolated home/config, and no `gcloud`, the built CLI discovered the four values, printed the code prompt, entered polling, did not change trusted config, and was canceled before consent |
| Preserved HTTPS/host/401/service contracts | Live Pass | TLS 1.3 certificate verification, exact hostname, `/healthz`, unauth `/v1/execute` 401, Caddy-only TCP 443, internal DB/API composition, public config and the post-redeploy gcloud whoami evidence all pass |
| Final local gates | Pass | Full, race, vet, three cross-builds, two disposable PostgreSQL suites, focused discovery/OAuth/routing/installer/doctor/Compose/backup tests and deployment docs/script consistency all pass on the frozen implementation |

Initial pre-freeze focused reviewer evidence:

```sh
go test ./integration \
  -run '^(TestOAuthBrowserPKCEStateFailureAndSingleUse|TestOAuthDeviceFlowPendingSlowDownDenialAndExpiry|TestOAuthDeviceClientKeyPromotionRuntimeAndLogoutStayCoherent|TestOAuthExpectedPrincipalIsRejectedBeforeCredentialSave|TestOAuthStagedCredentialsRollbackNewAndReplacedEntries)$' \
  -count=1 -v
```

Result: `PASS` in 14.443s. The browser exchange asserted its matching secret;
device acquisition retained the device client ID; every device token poll and
runtime refresh asserted the matching device secret; principal rejection and
new/replaced transaction rollback retained their no-save/fail-closed behavior.
The test helper uses distinct synthetic browser and device secret values.

```sh
go test ./integration \
  -run '^(TestPhase3CommandOutputsAndTempArtifactsDoNotLeakSecrets|TestOAuthInstallerUsesSafeRouteAndDoctorAcceptsIt|TestOAuthInstallerRejectsSecretAndCrossBackendFields|TestInstalledOAuthEntryHasOnlyExplicitEnvironment)$' \
  -count=1 -v
```

Result: `PASS` in 2.438s. The secret scan now includes both distinct synthetic
installed-app credential values and checks CLI output, installer JSON, doctor/MCP
artifacts, serialized argv and installed environment.

```sh
go test ./integration \
  -run '^(TestPublicOAuthConfigContract|TestDeploymentPublicConfigAndUnauthenticatedAPIContract|TestPhase3DiscoveryPublicConfigContainsExactlyFourCredentials|TestComposePublicHTTPSAndPersistenceContract)$' \
  -count=1 -v
```

Result: `PASS` in 1.351s. This covers exact four-field synthetic output and
pairing, no-auth/no-execution behavior, the existing 401 fixture, and the
Compose environment/argv/network contract. It is not a substitute for the
deployed endpoint update.

The strict discovery transport rerun passes:

```sh
go test ./integration \
  -run '^TestPhase3OAuthDiscoveryTransportContract$' \
  -count=1 -v
```

Result: `PASS` in 0.643s. It rejects missing, unknown, duplicate, non-string,
non-2xx, redirect, oversized, timed-out and trailing responses, preserves
omitted unrelated pairs, and confirms the browser/device CLI aborts before
acquisition when the applicable pair is empty or incomplete.

The combined focused discovery, rollback, redaction and installer/doctor rerun
also passed after reviewer-owned failure output was scrubbed of all credential
values:

```sh
go test ./integration \
  -run '^(TestPhase3OAuthDiscoveryTransportContract|TestPhase3OAuthDiscoveryCLIFailClosed|TestPhase3OAuthDeviceExplicitIDBypassesDiscovery|TestPhase3OAuthDiscoveryRejectsInsecureLoopbackBeforeRequest|TestPhase3LoginHelpSubprocess|TestPhase3DiscoveryPublicConfigContainsExactlyFourCredentials|TestPublicOAuthConfigContract|TestDeploymentPublicConfigAndUnauthenticatedAPIContract|TestOAuthDeviceClientKeyPromotionRuntimeAndLogoutStayCoherent|TestOAuthStagedCredentialsRollbackNewAndReplacedEntries)$' \
  -count=1 -v
```

Result: `PASS` in 7.675s. A separate rerun of the installer, doctor and
cross-backend rejection probes passed in 2.545s. These remain pre-freeze evidence
until the engineer publishes the replacement implementation freeze.

### LIVE-1 — historical pre-fix blocking Google client compatibility defect

Against the owner-published HTTPS endpoint
`https://34-142-66-102.sslip.io`, a built CLI from the frozen tree with an
isolated temporary `HOME`, temporary trusted config and a sanitized `PATH` where
`gcloud` was absent performed discovery and reached Google successfully:

```sh
path_without_gcloud="$tmp/bin:/usr/bin:/bin"
env -i PATH="$path_without_gcloud" HOME="$tmp/home" /bin/sh \
  -c 'command -v gcloud || printf "absent\n"'

CLAWBRIDGE_HOME="$tmp/home" CLAWBRIDGE_CONFIG="$tmp/config.toml" \
PATH="$path_without_gcloud" "$tmp/clawbridge" login --device
```

The trusted config contained only the cloud route and OAuth backend:

```toml
mode = "cloud"
server = "https://34-142-66-102.sslip.io"
cache = "cache.db"
credential_source = "oauth"
```

`gcloud` resolved as absent. The command discovered `device_client_id`, printed:

```text
Open https://www.google.com/device and enter code [REDACTED] (expires in 30m0s).
```

and then exited 1 before consent:

```text
clawbridge: Google device sign-in failed, expired or was declined; start a new login
```

The config remained byte-for-byte at its original 104 bytes, and the isolated
home contained no stored files. The sanitized raw Google token-endpoint probes
use the public IDs from the deployed `/v1/config` with dummy grant values:

```sh
# No client_secret parameter.
curl -X POST https://oauth2.googleapis.com/token \
  -d client_id="$DEVICE_CLIENT_ID" \
  -d grant_type=urn:ietf:params:oauth:grant-type:device_code \
  -d device_code=clawbridge-review-dummy-device-code
# {"error":"invalid_request","error_description":"Missing required parameter: client_secret"}

# No client_secret parameter.
curl -X POST https://oauth2.googleapis.com/token \
  -d client_id="$BROWSER_CLIENT_ID" \
  -d code=clawbridge-review-dummy-authorization-code \
  -d code_verifier="$(printf 'a%.0s' {1..64})" \
  -d grant_type=authorization_code \
  -d redirect_uri=http://127.0.0.1:53682/callback
# {"error":"invalid_request","error_description":"client_secret is missing."}
```

This is a live-provider failure, not a synthetic-fixture failure. The frozen
public contract exposes exactly two client IDs and no secret; the current device
path deliberately clears the Desktop client secret, and no separate device
client secret exists. Thus discovery cannot supply the material Google currently
requires from these deployed clients. Google's current device-flow documentation
lists `client_secret` in the token poll, while the live endpoint is authoritative
for this deployment's actual client configuration.

The reviewer added an opt-in black-box regression at
`integration/phase3_live_google_oauth_test.go`. Its ordinary test execution
skips without live opt-in; the exact live invocation currently fails both
subtests:

```sh
CLAWBRIDGE_TEST_PUBLIC_URL=https://34-142-66-102.sslip.io \
CLAWBRIDGE_TEST_LIVE_GOOGLE_OAUTH=1 \
go test ./integration \
  -run '^TestLiveGoogleDiscoveredClientsSupportSecretlessTokenExchange$' \
  -count=1 -v
```

```text
--- FAIL: .../device-code-poll
    Google rejects the discovered client without a secret: Missing required parameter: client_secret
--- FAIL: .../desktop-authorization-code-exchange
    Google rejects the discovered client without a secret: client_secret is missing.
```

At that historical checkpoint the two-field/no-secret contract made a successful
live exchange impossible for this Google client configuration. The owner
superseded that contract with Option A above. The failure and its exact probes
remain evidence for why the gate was reopened; they do not authorize a
secretless final implementation and are not the current acceptance contract.

Current focused evidence uses temporary homes, loopback HTTP fixtures, a fake
browser opener, synthetic OAuth responses and a memory credential store. It makes
no Google request and does not access the user's OS keychain. The exact commands
are recorded below. Their passing result establishes synthetic protocol behavior;
it does not establish live Google consent, browser/device client registration,
real OS-keychain integration at the binary boundary or deployed Caddy behavior.
The pre-freeze checkpoint is `HEAD` `9f3f04be1cec` plus the shared dirty
discovery/login revision.

Historical reproduction from before the client fix, retained for traceability:

```sh
tmp=$(mktemp -d /tmp/clawbridge-login-discovery.XXXXXX)
go build -o "$tmp/clawbridge" ./cmd/clawbridge
cat > "$tmp/config.toml" <<'EOF'
mode = "cloud"
server = "https://clawbridge.example.test"
audience = "browser.apps.googleusercontent.com,device.apps.googleusercontent.com"
cache = "cache.db"
credential_source = "oauth"
EOF
CLAWBRIDGE_HOME="$tmp/home" CLAWBRIDGE_CONFIG="$tmp/config.toml" \
  "$tmp/clawbridge" login
CLAWBRIDGE_HOME="$tmp/home" CLAWBRIDGE_CONFIG="$tmp/config.toml" \
  "$tmp/clawbridge" login --device
```

At that earlier revision both commands exited 1 before acquisition with:

```text
clawbridge: OAuth credential source requires oauth_client_id or oauth_device_client_id in trusted configuration
```

The current login resolver permits discovery for login while normal cloud
commands still require the complete trusted route. Source review and focused
command tests also show that successful device login promotes the acquired
device ID to active `oauth_client_id` and `audience`, retains it as
`oauth_device_client_id`, clears the Desktop secret and logs out the promoted
key. Browser login writes the selected browser ID to both active
`oauth_client_id` and `audience` while preserving the separately configured
device ID and supported Desktop secret.

### Reopened discovery/login gate

| Contract gate | State | Independent evidence |
| --- | --- | --- |
| Discovery selection and precedence | Pass on frozen tree | Browser login chooses `browser_client_id` only when trusted `oauth_client_id` is absent. Device login chooses `device_client_id` only when trusted `oauth_device_client_id` is absent. The built binary proves browser bypass reaches the fake browser opener with zero discovery requests, and device bypass reaches the synthetic acquisition proxy with zero discovery requests |
| Exact transport and URL policy | Pass on frozen tree | Integration probes require exactly `GET /v1/config`, `Accept: application/json`, no `Authorization`, numeric-loopback HTTP only or HTTPS otherwise, and no path/query/fragment/userinfo. Redirect statuses 301, 302, 303, 307 and 308 are refused without contacting the target. A 75 ms injected timeout and the 16 KiB body cap pass |
| Strict discovery document | Pass on frozen tree | Empty, array, missing, unknown, duplicate, non-string and trailing JSON are rejected. Both client IDs are required as strings, while either unrelated ID may be empty so long as the applicable flow's ID is non-empty |
| Failure before acquisition and redaction | Pass on frozen tree | A built binary with a temporary home rejects empty applicable IDs, malformed/duplicate JSON, non-2xx, redirect and insecure non-loopback URLs before the fake browser opener runs. Trusted config bytes stay unchanged, no local database appears, redirect targets receive zero requests, and synthetic response secrets and unsafe URLs are absent from output |
| Successful login promotion | Pass at the command-hook boundary; binary keychain limit recorded | Browser success writes the discovered browser client to active `oauth_client_id` and `audience`, retaining an explicit device ID and Desktop secret. Device success keeps the device ID in `oauth_device_client_id`, promotes it to both active values, clears the Desktop secret and reports the promoted active ID. The binary has no external fake-keyring injection seam |
| Keychain/config transaction and logout | Pass at synthetic store and command-hook boundaries | Browser and device config-write failures emit no success, preserve trusted config bytes and either delete a new staged key or restore the replaced credential. Device logout removes the promoted active key and a second logout remains idempotent. Real OS-keychain disk state is not exercised |
| Public config secret boundary | Pass on frozen tree | The unauthenticated response has exactly `browser_client_id` and `device_client_id`, does not invoke verification or execution, and contains no client secret or other routing/credential data |
| `login --help` | Pass on frozen tree | The built binary exits zero, emits normal help on stderr and performs no configuration, discovery or acquisition work |

Focused no-Google commands and results on the current tree:

```sh
go test ./integration \
  -run 'TestPhase3OAuthDiscoveryTransportContract|TestPhase3OAuthDiscoveryCLIFailClosed|TestPhase3OAuthDeviceExplicitIDBypassesDiscovery|TestPhase3OAuthDiscoveryRejectsInsecureNonLoopbackBeforeRequest|TestPhase3LoginHelpSubprocess|TestPhase3DiscoveryPublicConfigContainsOnlyClientIDs' \
  -count=1 -v
```

Result: `PASS` in 6.357s, including 27 transport subtests, 11 CLI
fail-closed/bypass subtests, browser/device explicit-ID bypass processes,
insecure URL rejection, help and public-config checks.

```sh
go test ./internal/cli \
  -run 'Test(BrowserLoginDiscoversClientAndPromotesAudience|DeviceLoginDiscoversClientAndClearsDesktopSecret|ExplicitOAuthClientIDSkipsDiscovery|LoginDiscoveryFailuresStopBeforeAcquisition|LoginDiscoveryConfigWriteFailureRollsBackPromotedCredential|LoginHelpReturnsNormally)' \
  -count=1 -v
```

Result: `PASS`. Explicit-ID bypass is exercised for browser and device;
discovery success and config-write rollback are exercised for browser and device
through package-private provider/keyring hooks.

```sh
go test ./internal/remote -run TestOAuthDiscovery -count=1 -v
```

Result: `PASS` for strict JSON, content type, redirect, oversized body, URL policy,
redaction and timeout bounds.

```sh
go test ./integration \
  -run 'TestOAuthBrowserPKCEStateFailureAndSingleUse|TestOAuthDeviceFlowPendingSlowDownDenialAndExpiry|TestOAuthRuntimeRefreshesAndCachesOnlyIdentityToken|TestOAuthExpectedPrincipalIsRejectedBeforeCredentialSave|TestPhase3CredentialFailureStopsBeforeTransportAndMirror|TestOAuthStagedCredentialsRollbackNewAndReplacedEntries|TestOAuthDeviceClientKeyPromotionRuntimeAndLogoutStayCoherent|TestPhase3CredentialSourcesAreExclusiveAndRepositoryCannotInject' \
  -count=1 -v
```

Result: `PASS`. This retains the synthetic PKCE, device polling, runtime mint,
credential-failure, transaction, device-key promotion/logout, backend-exclusivity
and repository-isolation regressions.

```sh
go test ./integration \
  -run 'Test(OAuthInstallerUsesSafeRouteAndDoctorAcceptsIt|OAuthInstallerRejectsSecretAndCrossBackendFields|InstalledOAuthEntryHasOnlyExplicitEnvironment|Phase3CommandOutputsAndTempArtifactsDoNotLeakSecrets|DoctorCLIValidatesTrustedRouteReadOnly)' \
  -count=1 -v
```

Result: `PASS`; the OAuth installer fixture was corrected in reviewer-owned
integration code so its configured `audience` equals the active browser client,
matching the frozen route validation. Installer/doctor argv, environment and
redaction checks remain intact.

The repository authority fixture correction was then applied only in
reviewer-owned `integration/**`. Every active/trusted OAuth route used by the
route and repository tests now has `audience == oauth_client_id`; malicious
repository route fields remain present only to prove that they are ignored
before route validation or authority selection. The corrected focused route
file passed:

```sh
go test ./integration \
  -run 'Test(OAuthInstallerUsesSafeRouteAndDoctorAcceptsIt|OAuthInstallerRejectsSecretAndCrossBackendFields|InstalledOAuthEntryHasOnlyExplicitEnvironment|Phase3CommandOutputsAndTempArtifactsDoNotLeakSecrets)$' \
  -count=1 -v
```

Result: `PASS` in 1.999s.

The combined routing/backend-exclusivity and discovery suite also passed:

```sh
go test ./integration \
  -run 'Test(Phase3CredentialSourcesAreExclusiveAndRepositoryCannotInject|Phase3OAuthDiscoveryTransportContract|Phase3OAuthDiscoveryCLIFailClosed|Phase3OAuthDeviceExplicitIDBypassesDiscovery|Phase3OAuthDiscoveryRejectsInsecureNonLoopbackBeforeRequest|Phase3LoginHelpSubprocess|Phase3DiscoveryPublicConfigContainsOnlyClientIDs)$' \
  -count=1 -v
```

Result: `PASS` in 8.006s, including all discovery transport/CLI subtests,
browser/device explicit-ID bypass, public config and backend-exclusivity cases.
The repository three-direction process probe was run separately because Go
splits `-run` expressions on `/`:

```sh
go test ./integration \
  -run '^TestProcessIntegration$/^Resolution$' \
  -count=1 -v
```

Result: `PASS` in 2.417s. It retains repository-only `mode=cloud` falling back
to the trusted local default, trusted local surviving malicious repository
routing, trusted cloud surviving repository `mode=local`, explicit/config/env
precedence and fail-closed unsupported-mode coverage.

The earlier owner-document contradiction is resolved. The engineer and owner
corrected `PHASE-3-PLAN.md` and `README.md` to state that browser and device login
discover the applicable installed-app client when the trusted value is absent,
while an explicit applicable ID overrides discovery. The corrected wording now
matches `CORE-API.md` and `COORDINATION.md`.

Deployment direction update, 20 October 2026: the owner selected Docker Compose
on one Compute Engine VM for the hosted deployment. Cloud Run and Cloud SQL are
historical design alternatives and are no longer acceptance criteria. The frozen
review gate covers Caddy automatic HTTPS and outbound ACME/Google access, a public unauthenticated
`GET /v1/config` exposing only the browser/device OAuth client IDs, startup
validation that both IDs belong to the server's accepted audience set, preserved
single-audience behavior when the public-ID flags are unset, Caddy as the sole
public TCP 443 ingress, internal-only API/database networking, a configurable
host path for PostgreSQL data on the separate persistent disk, and documentation
that the reverse proxy replaces the IAP requirement for users. The synthetic
Compose/public-config portion passed before deployment. The final section records
the later live VM, Caddy certificate, HTTPS, public-config and unauthenticated-401
results.

That paragraph records the earlier ID-only checkpoint. The owner later replaced
its `/v1/config` shape with the four-field R-live contract; the Caddy, audience,
network and startup-validation requirements remain in force.

The owner has frozen the Phase 3 scope. Minimum delivery is OAuth login and
logout, keychain-backed runtime minting, explicit `oauth`/`gcloud`/`adc`
exclusivity, safe MCP routing and installation, and the real `doctor` CLI route.
Setup create/join automation and the TUI are optional scaffolds unless the
engineer's freeze includes them. A successful Phase 3 join is login plus a
trusted route. The engineer declared implementation freeze at the final
acceptance checkpoint. The frozen focused suite, uncached full and race suites,
`go vet`, disposable PostgreSQL, and all three cross-builds pass. Live Google and
deployed non-interactive behavior passed separately; interactive consent and
real-keychain lifecycle remain paused.

The owner subsequently froze a backward-compatible multi-audience server fix:
`serve --audience` accepts one exact audience or an ordered comma-separated set,
and `NewGoogleVerifierForAudiences` verifies against that copied set. The frozen
set, full, race, vet, cross-build and disposable PostgreSQL gates below were
rerun for that revision.

No implementation or frozen owner document was edited by the reviewer. All
reviewer changes remain uncommitted and unstaged. Synthetic loopback, fake
keychain and loopback HTTP evidence below is not evidence of live Google, IAM or
deployed Compose/Caddy behavior.

## Acceptance matrix

> The matrix in this section began at the pre-R-live frozen checkpoint. Its
> passes remain regression evidence. R-live above records the final four-field
> rerun and supersedes the shape, device-secret, promotion, redaction and
> non-interactive live-provider rows that were pending here.

| Gate | State | Independent evidence required |
| --- | --- | --- |
| Browser loopback PKCE | Pass, frozen synthetic rerun | `TestOAuthBrowserPKCEStateFailureAndSingleUse` passed after freeze. It proved numeric `127.0.0.1` loopback, independent random state, exact state refusal, `S256` challenge/verifier equality, one token exchange, one-shot callback closure and no durable access/ID token or code |
| Device authorization | Pass, frozen synthetic rerun | `TestOAuthDeviceFlowPendingSlowDownDenialAndExpiry` passed after freeze. It proved one prompt, pending polling, a five-second `slow_down` increase followed by approval, terminal denial, terminal expiry and no credential saved on failure |
| Device-client identity coherence | Pass for auth/staged flow and package-private CLI hook; binary injection limit recorded | `TestOAuthDeviceClientKeyPromotionRuntimeAndLogoutStayCoherent` passed: device acquisition used `oauth_device_client_id` without a client secret, `StageCredential` keyed it under that ID, runtime refresh and logout used the promoted active key, and repeated logout stayed idempotent. Engineer-owned `TestDeviceLoginPromotesDeviceClientAndOmitsDesktopSecret` covers the command boundary and verifies the original Desktop secret is absent after promotion. The binary still exposes no fake-keyring injection seam |
| Keychain-only refresh token | Pass, synthetic store and package-private CLI hooks | Browser and device fixtures persisted only version/client/principal/subject/refresh fields. Runtime mint performed no keychain save. No external fake-keyring seam exists for a full login/logout binary subprocess, so that specific limit remains explicit |
| Trusted principal checked before save | Pass, frozen synthetic rerun | `TestOAuthExpectedPrincipalIsRejectedBeforeCredentialSave` passed for browser and device: mismatched verified identity returned an error, made zero `Save` calls, stored zero credentials and did not echo token material. Current `internal/cli` passes trusted `config.Principal` as `ExpectedPrincipal`; command-level transaction behavior is covered through package-private hooks |
| OAuth runtime refresh and mint | Pass, frozen synthetic rerun | `TestOAuthRuntimeRefreshesAndCachesOnlyIdentityToken` passed. One keychain load and one refresh produced a cached in-memory ID token; no access or ID token became durable |
| Credential failure blocks mirror | Pass; P3-D1 fixed | The original defect was reproducible: both credential-read and refresh/mint failures returned a cached `offline-mirror` inbox. The frozen `TestPhase3CredentialFailureStopsBeforeTransportAndMirror` passes for both cases, requires an error rather than a result, verifies unchanged mirror bytes and checks no queued write |
| Login transaction atomicity | Pass at staged credential and CLI hook boundaries | The frozen contract requires cleanup or restoration if trusted-config write fails after keychain save. `TestOAuthStagedCredentialsRollbackNewAndReplacedEntries` passed for browser and device. Engineer-owned `internal/cli` tests cover new-entry deletion, replaced-entry restoration, no success output and fail-closed prior routing at the command boundary. No external fake-keyring seam is exposed for a full binary login transaction; this limit is recorded rather than overstated as subprocess coverage |
| Backend exclusivity | Pass, frozen synthetic rerun | `TestPhase3CredentialSourcesAreExclusiveAndRepositoryCannotInject` passed for OAuth plus gcloud fields, gcloud plus OAuth fields, ADC without an explicit file, and an unknown `metadata` source. Each command failed without echoing the configured routing sentinels. Runtime tests cover empty-source gcloud compatibility, OAuth, ADC, and unsafe ADC construction |
| Trusted versus repository config | Pass for the three frozen directions; synthetic | Repository files may supply only `db`, `workspace`, `project`, `session` and `instance`. Focused process probes prove: (1) trusted local survives malicious repository `mode=cloud` and routing fields; (2) trusted cloud survives repository `mode=local` and still demands the trusted server/audience/pinned credential; (3) with no trusted route, repository-only `mode=cloud` creates no cloud route and permitted scope resolves through normal local default. Repository fields are ignored in both directions and were not echoed |
| Project-list representation compatibility | Pass, frozen rerun | `TestDoctorAcceptsEquivalentProjectListRepresentations` passes for both `[]any{map[string]any{...}}` and Store-native `[]map[string]any{{...}}`; both yield `authority.health=pass` |
| Login/logout semantics | Pass at auth and command-hook boundaries; binary injection limit recorded | Auth package and staged transactions cover principal mismatch, keychain durability, logout idempotence and rollback. Focused four-case `TestUseGcloud*` passes for successful gcloud check, browser fallback, separate profile and invalid mixed profile. Engineer-owned command tests cover device promotion, logout key selection, no misleading success and fail-closed rollback. Current provider/keyring seams are package-private, so no clean external fake-keyring binary probe exists |
| MCP safe routing | Pass, frozen synthetic rerun | `TestOAuthInstallerUsesSafeRouteAndDoctorAcceptsIt` asserts the exact OAuth argv, including safe scope, server/audience, source, active browser client, separate device client, keyring service, principal, cache and instance. It omits client secret, account, impersonation, credentials file and local DB. `TestInstalledOAuthEntryHasOnlyExplicitEnvironment` confirms no environment secret fields. The secret scan additionally checks installer result and argv |
| Installer/doctor consistency | Pass for public route and real doctor CLI | Installer and doctor use `remote.ValidateRoute` and emit the same OAuth argv. Doctor accepts the installer-produced isolated entry with `mcp.claude-code.clawbridge=pass`. `TestDoctorCLIValidatesTrustedRouteReadOnly` executes the built `doctor` command with an isolated synthetic gcloud route and loopback authority, requiring pass statuses for config, authority, credential selection and resolution, warn-only mirror/MCP absence, exactly two read calls, unchanged config bytes and no token disclosure |
| Secret logging | Pass for focused command paths | `TestPhase3CommandOutputsAndTempArtifactsDoNotLeakSecrets` checks failing `whoami`, real `doctor` and MCP startup routes for client-secret, unrelated-config and ADC sentinels; asserts installer result/argv/environment omit OAuth client-secret fields; and verifies a failed ADC selection leaves all regular files byte-identical. `TestDoctorCLIValidatesTrustedRouteReadOnly` separately proves the minted gcloud token and unrelated config secret are absent from output. OAuth login binary subprocess coverage remains limited by the package-private keyring seam |
| Existing local/hosted behavior | Pass | The retained local, MCP, installer, remote, mirror and PostgreSQL regressions passed. This includes workspace/project isolation, replay, retirement and successors, delivery/read state, cursor pagination, session resume, HTTP auth/denial, snapshot privacy, concurrency, retention rollback and offline replay prevention |
| OAuth discovery/login integration | **Pass for frozen local, synthetic and non-interactive live scope; consent Pending** | The frozen contract requires applicable-ID discovery only when that trusted value is absent, exact trusted-server transport, strict JSON, fail-before-acquisition semantics, audience/client promotion, transaction rollback and idempotent device-key logout. Focused local probes exercise those behaviors. The deployed endpoint, matching Google dummy exchanges and no-gcloud device polling pass independently without consent |
| Live Google browser/device | **Pass for non-interactive compatibility; interactive consent Pending** | The deployed public fixture supplies the exact four fields; matching browser/device credentials reach their corresponding Google endpoints; and device login reaches polling. Browser approval, device approval, real refresh-token keychain persistence and completed logout remain owner-paused |
| Multi-audience serve contract | Pass, frozen synthetic rerun | The production verifier accepted synthetic signed tokens for either configured browser or device audience and rejected an unlisted audience while retaining issuer, expiry, verified-email and signature checks. A built-binary probe rejected empty, empty-entry, duplicate, interior-whitespace, control-character, overlong and excessive sets before PostgreSQL and confirmed the valid ordered set reached the database boundary without reading a poisoned local config or echoing the test DSN |

### Compose and public-config gate

Status: **Historical Pass for the earlier ID-only server/Compose slice. The final
four-field post-redeployment result is recorded in the final frozen acceptance
section below.**

The reviewer added `TestPublicOAuthConfigContract` under `integration/**` as the
handler gate. Before the implementation update it failed exactly as expected:

```text
public config status=404 body={"error":{"code":"not_found","message":"endpoint not found"}}
```

Reproduction:

```sh
go test ./integration \
  -run 'TestServeValidatesAudienceSetBeforePostgres|TestPublicOAuthConfigContract' \
  -count=1 -v
```

At the original checkpoint the focused endpoint probes required exactly
`browser_client_id` and `device_client_id`. The owner replaced that contract with
the four fields in R-live above. The reviewer now requires an unauthenticated
`GET /v1/config`, a strict JSON object containing exactly
`browser_client_id`, `browser_client_secret`, `device_client_id` and
`device_client_secret`, `application/json`, `Cache-Control: no-store`, no unknown
fields or trailing JSON, and 405 for write methods. The shape/pairing probes pass
on the final frozen implementation; the deployed endpoint also passed strict
non-empty independent validation after redeployment.

The acceptance matrix for this historical slice is:

| Gate | Required evidence |
| --- | --- |
| Public config exposure | Direct request with no `Authorization` header returns 200; exact JSON contains the four frozen browser/device installed-app ID and secret fields, with no token, principal, account, impersonation, cache, server URL, database URL, credential source, ADC path, other secret or environment data |
| Public config method and path | Only `GET /v1/config` is public; other methods return 405; `/healthz` retains its existing narrow behavior; `/v1/execute` still requires exactly one bearer token |
| Public IDs match accepted audiences | Startup rejects a browser or device ID absent from the parsed `--audience` set before PostgreSQL is opened; exact case remains significant; success requires both IDs in the set |
| Optional public-ID flags | With either or both flags unset, the prior single/set audience behavior remains intact and no empty public field is invented |
| Invalid public flags | Empty, whitespace, duplicate, control, oversized and cross-field-invalid values fail closed with generic diagnostics that do not echo client IDs or database credentials |
| Caddy automatic HTTPS | Compose renders a Caddy service with persistent certificate/data and configuration volumes, an explicit public hostname/email configuration path, and HTTPS enabled without a checked-in private key or certificate |
| Caddy and API egress | Caddy and API are attached to a non-internal network that can reach ACME, Google signing-certificate endpoints and other required outbound services; internal-only attachments must not be their sole network |
| Public ingress | Only Caddy publishes a host port. It publishes TCP 443 and does not require TCP 80; TLS-ALPN-01 on 443 is sufficient for the owner's inbound-443-only policy |
| Backend isolation | PostgreSQL attaches only to the internal `backend` network. API and Caddy attach to `backend` and `egress`; API publishes no host port. "Private API" means no direct public exposure, not zero outbound access |
| PostgreSQL persistence | `CLAWBRIDGE_POSTGRES_DATA_PATH` renders as the host bind source for `/var/lib/postgresql/data` when set to an absolute path, allowing the separate persistent disk mount; an unset value retains the named-volume default |
| Container policy | API remains nonroot, read-only, capability-free and `no-new-privileges`; database storage uses the configured host path or named-volume default; restart and health-check behavior is explicit |
| Configuration validation | `docker compose config` renders with a temporary nonsecret fixture, rejects missing required deployment values, and does not expose a rendered production password or token |
| Documentation | Deployment docs state that browser/device users reach the service through the HTTPS reverse proxy and no longer need IAP for normal user access; operator SSH/IAP and firewall access remain separate operational details |
| Historical managed option | Cloud Run and Cloud SQL are described only as superseded historical alternatives; no Phase 3 acceptance gate requires Cloud Run, Cloud SQL, an IAP assertion or a load balancer |

The owner froze the Compose network direction: DB attaches only to the internal
`backend` network; API and Caddy each attach to `backend` and the non-internal
`egress` network; only Caddy publishes a port, and that port is exactly TCP 443.
The API needs outbound access for Google token-signing certificates, so private
API placement means no host port and no direct public exposure rather than zero
egress. A temporary fixture rendered with
`CLAWBRIDGE_POSTGRES_DATA_PATH=/srv/clawbridge/postgres-data` produces a host
`bind` mount at `/var/lib/postgresql/data` and no named PostgreSQL volume. The
independent focused command is:

```sh
go test ./integration \
  -run TestComposePublicHTTPSAndPersistenceContract \
  -count=1 -v
```

The test requires this exact topology, `--disable-redirects`, the Caddy `/data`
and `/config` volumes, rejects any host publication of the API or database, and
rerenders without `CLAWBRIDGE_POSTGRES_DATA_PATH` to prove the named-volume
default remains available. The exact normalized-Compose reproduction is:

```sh
env POSTGRES_PASSWORD=review-random-password \
  CLAWBRIDGE_AUDIENCE=browser-client.apps.googleusercontent.com,device-client.apps.googleusercontent.com \
  CLAWBRIDGE_BOOTSTRAP_OWNER=owner@example.test \
  CLAWBRIDGE_OAUTH_CLIENT_ID=browser-client.apps.googleusercontent.com \
  CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID=device-client.apps.googleusercontent.com \
  CLAWBRIDGE_DOMAIN=clawbridge.example.test \
  CLAWBRIDGE_POSTGRES_DATA_PATH=/srv/clawbridge/postgres-data \
  docker compose -f deploy/compose.yaml config --format json
```

The rendered result must show DB networks as exactly `[backend]`; API and Caddy
networks as exactly `[backend, egress]`; no API or DB host ports; and only Caddy
TCP 443. The earlier PostgreSQL failure that expected API backend-only was
reviewer test drift against the now-frozen direction, not an implementation
defect. Its cleanup was recorded: container
`63815cb2436527f05bbbe2f826e96b8fb2b7638f1fa7e93210adcbf051989ece`
on loopback `127.0.0.1:32774` and anonymous volume
`bf7fd6d29ad3fc56f5591924eb025a4ba3ea2be94d1f2250f289594b7f67492e`
were removed by the cleanup-trapped runner; the volume was confirmed absent.

The combined focused command is:

```sh
go test ./integration \
  -run 'TestPublicOAuthConfigContract|TestComposePublicHTTPSAndPersistenceContract|TestServeValidatesAudienceSetBeforePostgres' \
  -count=1 -v
```

At the historical ID-only checkpoint the server implementation validated both
optional IDs before database open, returned empty strings when omitted, rejected
cross-role duplicates and unlisted IDs, and did not invoke the verifier or
executor for `/v1/config`. Under R-live the same no-auth/no-execution invariants
must remain true while the endpoint returns only the four approved installed-app
credential fields. The reviewer
strengthened the public-config probe with verifier and executor call counters.
Successful discovery and each rejected write method require both counters to
remain zero. The corrected Compose regression and the strengthened discovery
regression passed before the final freeze. The final frozen rerun is recorded at
the end of this report.

The final focused result on the frozen revision was:

```text
TestComposePublicHTTPSAndPersistenceContract                  PASS  0.38s
TestServeValidatesAudienceSetBeforePostgres                   PASS  1.78s
  empty                                                       PASS  0.48s
  empty-entry                                                 PASS  0.02s
  duplicate-after-trim                                        PASS  0.02s
  interior-whitespace                                         PASS  0.02s
  control-character                                           PASS  0.02s
  too-long                                                    PASS  0.01s
  too-many                                                    PASS  0.01s
TestPublicOAuthConfigContract                                 PASS  0.00s
```

The rendered Compose service/network contract is exactly:

```text
db       networks=[backend]          host ports=[]
api      networks=[backend,egress]   host ports=[]
caddy    networks=[backend,egress]   host ports=[TCP 443]
backend  internal=true
egress   internal=false
```

Caddy renders `--disable-redirects`, uses `/data` and `/config` persistence, and
does not publish TCP 80. The API image runs as distroless `nonroot`, with a
read-only filesystem, `no-new-privileges`, and all capabilities dropped. The
configured absolute PostgreSQL data path renders as a host bind; removing the
setting restores the named-volume default.

An earlier documentation draft described the loopback-only API and IAP-mediated
loopback tunnel rather than the Caddy HTTPS user path, and its environment
example omitted `CLAWBRIDGE_DOMAIN`, both OAuth client IDs and the configurable
persistent-disk data path. The current draft corrects those points: it states
that only Caddy publishes TCP 443, TLS-ALPN does not require port 80, Caddy uses
a separate non-internal egress network, normal clients no longer need an IAP
tunnel to the API, and `CLAWBRIDGE_POSTGRES_DATA_PATH` can identify the attached
persistent disk. The current-state documentation check is:

```sh
rg -n 'loopback|IAP|CLAWBRIDGE_DOMAIN|CLAWBRIDGE_OAUTH_CLIENT_ID|CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID|CLAWBRIDGE_POSTGRES_DATA_PATH' \
  deploy/README.md
```

The remaining loopback/IAP references describe historical pilot access or
operator administration, not the normal user path. The configuration-string bind
syntax passed on Docker Compose v5.5.0. At final review the reviewer will also
check that the deployment docs require an absolute path to the separately mounted
persistent disk and warn that Docker may create a root-owned directory when the
path is absent. Relative values are not accepted as evidence of the separate-disk
layout.

The new deployment direction does not alter the multi-audience verifier result.
Cloud Run and Cloud SQL references elsewhere in this report are historical design
context. They are not acceptance criteria for the Compose-on-Compute-Engine
deployment and are not evidence for or against it.
| Per-user service-account provisioning and IAM expiry | Future work, outside Phase 3 | Automatic provisioning, deterministic duplicate-name collision handling and IAM Condition expiry are future setup/deployment work. They require separate contracts, tests and operator-approved live IAM verification and are not claims of the Phase 3 scaffold |
| Optional setup and TUI scaffolds | Scope, outside minimum unless frozen in | `PHASE-3-PLAN.md` and `COORDINATION.md` make create/join automation and TUI optional. Existing focused tests are useful evidence, but delivery is not required for Phase 3 minimum acceptance unless the engineer's freeze claims it |

Reviewer test drift was corrected in `integration/resolution_test.go` after the
repository-mode rule became final. The Resolution scenario now covers both
repository directions without relying on the superseded Phase 2 overlay rule:

1. With no trusted route, repository-only `mode=cloud` plus a deliberately unsafe
   non-loopback HTTP server still resolves permitted scope through the normal
   trusted local default.
2. With trusted local mode, repository cloud mode and every routing field are
   ignored, while local `whoami` and setup remain effective.
3. The trusted-cloud route surviving a repository `mode=local` is covered in
   `TestPhase3CredentialSourcesAreExclusiveAndRepositoryCannotInject`, where it
   still fails closed on the trusted OAuth keychain route.

Explicit flag and environment precedence checks remain. Unsupported authority
mode is now exercised through an invalid trusted-home configuration, which still
fails before local or mirror fallback. Exact focused command:

```sh
go test ./integration -run TestProcessIntegration/Resolution -count=1 -v
```

Result: `PASS`.

## Black-box scenario coverage

The focused scenarios avoid real Google credentials and use isolated temp homes,
explicit config paths, loopback HTTP fixtures, injected/fake OS-keyring
implementations and test-only subprocess environments. The frozen run repeated
the scenarios that can be reached through public boundaries.

1. Build the CLI. Launch a synthetic loopback authorization flow and capture the
   authorization URL. Assert `redirect_uri` is numeric loopback, `response_type=code`,
   `code_challenge_method=S256`, a fresh state and offline access. Refuse wrong,
   missing and replayed state; verify the token exchange receives the matching
   verifier. Confirm only a test keychain entry receives `refresh_token`; success
   output and config contain no token material.
2. Drive synthetic device responses through pending, `slow_down`, denial and expiry.
   Assert the prompt appears once, polling respects provider intervals and deadline,
   failure exits nonzero without a stored credential, and success caches only the
   refresh credential.
3. With a stored synthetic refresh credential, serve one token-mint response and
   one HTTP authority request. Assert the minted ID token stays in memory, refresh
   occurs only when required and no access-token duplicate is persisted. Then fail
   each of keychain read, refresh, malformed ID token and transport independently.
   Only transport may read a matching scoped mirror; credential failures must not.
4. Run a table of trusted/repository configuration combinations. Prove repository
   route/credential fields cannot override trusted values and that `oauth`, `gcloud`
   and `adc` never fall through to another backend. Include `--use-gcloud`, missing
   gcloud, gcloud success, OAuth fallback and both flags together using temporary
   executable fixtures.
5. Inspect installed Claude/Codex MCP entries and started MCP process argv/environment
   in a temporary configuration. Assert safe route and scope pinning, no secret
   values, consistent scope with doctor, and no real user config access.
6. Run doctor and login/logout through the built binary with missing, malformed,
   denied, mismatch and success fixtures. Capture stdout/stderr and compare exact
   config/cache/keychain bytes before and after refusal. Search every artifact for all
   synthetic secrets and emitted command/DSN values.
7. Parse-safe scan subprocess diagnostics rather than relying only on the first line.
   Build MCP and CLI with logging enabled where supported; require no token, refresh
   token, authorization code, verifier, client secret, credential-file content, ADC
   JSON, DSN/password or SQL echo.
8. Final frozen run: uncached `go test ./... -count=1` and `go test -race ./... -count=1`
   through `integration/run-postgres.sh`, followed by `go vet ./...`, CGO-disabled
   darwin/arm64, linux/amd64 and windows/amd64 builds. The exact results are
   recorded below.

## Historical pre-R-live frozen gate results

The final verification used Go 1.27.0 on darwin/arm64 and Docker Engine 29.8.2.
Every PostgreSQL run used the current `postgres:17-alpine` image and reported
PostgreSQL 17.11. `integration/run-postgres.sh` generated a random test password,
published the database on a random loopback port, created an anonymous volume and
passed the DSN only through `CLAWBRIDGE_TEST_POSTGRES_DSN`. No credential was
written to the repository.

These rows are retained as the pre-R-live regression record. The final four-field
implementation and deployment results are recorded at the end of this report.

| Command | Result | Evidence |
| --- | --- | --- |
| `go test ./... -count=1` without the PostgreSQL environment | Pass | All packages passed; real PostgreSQL tests skip when the DSN is absent |
| `go test -race ./... -count=1` without the PostgreSQL environment | Pass | All packages passed under the race detector |
| `go vet ./...` | Pass | No diagnostics |
| `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./cmd/clawbridge` | Pass | Mach-O 64-bit executable arm64 |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/clawbridge` | Pass | Statically linked ELF 64-bit x86-64 executable |
| `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./cmd/clawbridge` | Pass | PE32+ console executable for Windows x86-64 |
| `integration/run-postgres.sh go test ./... -count=1` | Pass | All packages passed with `CLAWBRIDGE_REQUIRE_POSTGRES=1`; integration completed in 27.654s |
| `integration/run-postgres.sh go test -race ./... -count=1` | Pass | All packages passed with the race detector and real PostgreSQL; integration completed in 30.887s |

The first frozen PostgreSQL invocation exposed a stale reviewer fixture rather
than a product failure. `TestPostgresCloudCLITrustedRoutingAndOffline` called
cloud MCP installation without the frozen required `--credential-source` flag.
The installer correctly returned `cloud installation requires an explicit
credential source`. The reviewer changed only `integration/cloud_cli_test.go` to
pass and assert `--credential-source gcloud`; the corrected fixture passes in the
full suite and in the retained PostgreSQL set. The original PostgreSQL container
and volume were removed by the runner before the rerun.

The disposable resource ledger for that pre-R-live checkpoint was:

| Purpose | Container | Loopback | Anonymous volume | Cleanup |
| --- | --- | --- | --- | --- |
| Initial full run that found the stale fixture | `78facda6f30cd747745d12b1e8dd91715148cc371183e56d014d6115922f28a0` | `127.0.0.1:32768` | `bd1ffa629177ebed0364d5cf267257ed045b0088f381ba1f017c21716a6355ae` | Container and volume removed |
| Corrected full suite | `21061fb27bf8566f7c1e6561381dda1c0d3ae09799369717fcbc1e703b4f4801` | `127.0.0.1:32769` | `4454d1cdff8a50fba19c16a129a88e34c782eb4389fffe3e448c6de5a4f83e4a` | Container and volume removed |
| Full race suite | `2d55517c4a223087433e5a91e82413852e738db128de89f040ab70001d2588ee` | `127.0.0.1:32770` | `24e1536c19fa6143168cd57bcc8f616623aa8fd0597e4be2452d65a12d516458` | Container and volume removed |
| Retained PostgreSQL scenarios | `25b1767e589d43a9a984f9c46cab0e466af42efb295962325358a0d78db2151c` | `127.0.0.1:32771` | `ed3c5ada03b0cef68c49a815d9fcc6ad8bb7dcd28db045bb222bc2d4c1abb27f` | Container and volume removed |

Post-run checks found no container with the reviewer prefix and confirmed every
recorded volume absent. Existing Docker containers were not used or removed.

The final focused batch repeated browser and device OAuth protocol handling,
runtime minting, principal refusal, transaction rollback, device-key promotion,
all four `UseGcloud*` cases, all three repository directions, P3-D1, installer
and doctor argv/environment agreement, project-list shapes, secret redaction and
the real `doctor` binary route. The retained PostgreSQL batch separately exercised
HTTP roundtrip and denial, snapshot privacy, concurrent principals and replay,
workspace isolation and resume, project/member views, retention rollback and
honest caps, schema rejection, scoped offline fallback, alias isolation, cloud CLI
installer routing and synthetic crypto claims. All passed.

The README documentation check confirms `.clawbridge.toml` examples contain scope
only, explain that repository mode and routing fields are ignored, and document
browser login, device login, doctor and logout. It also states that only the
refresh credential is stored and live Google acceptance remains outstanding.
`git diff --check` is clean.

## Current review findings

At the pre-R-live checkpoint no implementation defect remained promoted on that
then-frozen tree. R-live later exposed a real client-compatibility blocker; the
frozen four-field replacement fixes it and its final local, PostgreSQL and
non-interactive live probes pass. No implementation defect remains promoted.
P3-D1 retains its historical finding and is fixed in the frozen rerun. The OAuth protocol,
runtime-mint, backend-exclusivity and repository-isolation probes are independent
hermetic evidence. The repository-isolation probe covers trusted local versus
malicious repository cloud mode, trusted cloud versus malicious repository local
mode, and repository-only cloud mode with no trusted route. Full, race,
cross-build and real PostgreSQL evidence now support the local and synthetic
acceptance status above.

The frozen coordination checkpoint defines the minimum Phase 3 coding slice as
OAuth join/runtime, safe routing and the real `doctor` CLI route. Setup and TUI
remain scaffolds. Their absence from complete CLI delivery is reported as scope,
not as a defect.

The owner has made the repository configuration boundary final. Repository
`.clawbridge.toml` may overlay only `db`, `workspace`, `project`, `session` and
`instance`. It may not select or switch `mode`, `server`, `audience`,
credentials, OAuth clients, keyring service, `account`,
`impersonate_service_account`, `principal` or `cache`. Trusted local routing must
survive a repository `mode=cloud`; trusted cloud routing must survive a
repository `mode=local` and continue to require the trusted server, audience and
pinned credential. This decision supersedes the older Phase 2 repository-mode
overlay allowance. The passing direction probes verify the final contract and are
not defects.

Two earlier CLI observations were corrected. Login now passes
trusted `config.Principal` as `auth.Config.ExpectedPrincipal`, so auth rejects a
mismatch before keychain save. Device login clears the Desktop client secret
before acquisition and clears it again when promoting the device client ID to
active `oauth_client_id`. Package-private CLI tests now cover the transaction,
device-key and logout boundaries. No external fake-keyring seam exists for a
full binary login/logout subprocess.

An earlier installer/doctor mismatch was corrected. `install.Options`, the
installer route, `doctor.Config`, `doctor.MCPConfig` and expected-entry
construction now carry `credential_source` and OAuth routing fields through the
shared `remote.ValidateRoute` policy. `checkAuthority` now accepts both `[]any`
and Store-native `[]map[string]any` project lists. No implementation file was
changed by the reviewer. No Python CLI exists in this checkout, so the initial
Python-specific request could not be performed; the equivalent Go CLI wiring was
reviewed instead.

Historical project-shape reproduction before its fix:

```sh
go test ./integration -run TestDoctorAcceptsEquivalentProjectListRepresentations -count=1
```

At the old revision, the interface-slice case passed and the typed-slice case
failed with `authority.health=fail` and
`The authority returned an unexpected result type or nonauthoritative source.`
The fixture returned the same one-row project list as `[]any` and as
`[]map[string]any`; only the typed Store-native slice was rejected. The current
focused test passes both cases.

Historical credential-fallback reproduction and current regression:

```sh
go test ./integration -run TestPhase3CredentialFailureStopsBeforeTransportAndMirror -count=1 -v
```

For both `credential-read` and `refresh-or-mint`, an injected token source
returning `errors.New("synthetic-...-secret")` produced
`result.Source == "offline-mirror"` instead of an error. A valid scoped snapshot
was placed at the configured cache path first. `internal/remote/client.go:374`
treats every non-`credentialError` token-source failure as a transport outage;
`NewWithTokenSource` accepts arbitrary public `TokenSource` implementations that
cannot construct the private typed error. The test makes the mirror file available
only to prove prohibited fallback; it verifies the file remains byte-identical.
At that recorded source revision the test failed on both subtests. The current
source wraps arbitrary token-source failures as credential errors. The focused
regression passed on the frozen tree, confirmed unchanged mirror bytes and
confirmed no queued write.

The engineer declared the implementation frozen after these contract decisions
were recorded. The reviewer then reran the targeted OAuth/routing/doctor set,
disposable PostgreSQL full and race suites, `go vet`, and all three CGO-disabled
cross-builds. All peer and reviewer changes remain uncommitted and unstaged.

## Handoff and verification limits

Interactive Google browser sign-in, real device approval, real-refresh-token
keychain lifecycle, lifecycle/reboot
recovery and live scheduled-backup/restore jobs have not been exercised and
remain Pending by the owner-requested pause. The owner supplied deployed
Compose/Caddy and gcloud-token evidence; the reviewer independently repeated the
public HTTPS, discovery, unauthenticated-401, Google matching-client and
no-gcloud device-polling probes. The reviewer will not claim interactive consent
or real keychain behavior from local HTTP fixtures, SDK simulation, fake
keyrings or fake `gcloud` executables.

At this checkpoint Phase 3 passes for the frozen automated/local and deployed
non-interactive contract, with interactive OAuth, keychain lifecycle, reboot and
scheduled backup/restore rows explicitly Pending. The optional setup create/join automation and TUI
remain scaffolds under the owner's frozen scope; their focused model tests are
supporting evidence, not a claim of complete CLI delivery. Automatic per-user
service-account provisioning, deterministic duplicate-name collision handling,
IAM Condition expiry and restore verification remain future setup/deployment
work. None is promoted as a Phase 3 minimum defect.

The root may use this report for the Phase 3 handoff after preserving the live
limits above. No commit, stage, push, deployment or real agent-configuration
mutation was performed by the reviewer. In particular, this report does not claim
that expired identities, disabled identities or ambiguous names have been
exercised against Google IAM.

## Frozen multi-audience server review

Review date: 20 October 2026. Source target: shared dirty checkout at HEAD
`9f3f04be1cecdeab5e620f38446308894255476e` plus the engineer's frozen uncommitted
multi-audience changes. The reviewer did not change the implementation or owner
contract files.

The contract and security review found no defect. `parseAudiences` trims only
surrounding whitespace and rejects an empty input, empty entries, exact duplicates,
entries over 2048 bytes, more than 16 entries, and entries containing Unicode
whitespace or control characters. It runs before database-URL lookup, Google
verifier construction and `bus.OpenPostgres`. `normalizeAudiences` applies the
same rules to the public set API and copies the accepted slice. Ordering and case
are retained.

`NewGoogleVerifier` continues to treat its argument as one exact audience, so a
comma remains an ordinary character for backward-compatible callers.
`NewGoogleVerifierForAudiences` uses the production Google ID-token validator for
each configured audience, then independently checks the exact `aud`, Google
issuer, expiry, verified email and subject. A token for an unlisted audience is
rejected. Validation errors remain generic and do not echo an audience, token,
database URL or SQL. The existing algorithm, key-ID and signature-length
prevalidation and dependency-panic recovery remain in the verification path.
Cloud client routing remains a single `audience` string, matching the single
audience minted into each request token.

The acceptance matrix for this fix is:

| Case | Status | Independent evidence |
| --- | --- | --- |
| Single exact audience remains compatible | Pass | Production verifier accepted a correctly signed single-audience token; the constructor and CLI retain their original single-value form |
| Ordered comma-separated browser and device audiences | Pass | Both configured audiences verified through the official validator path; parser and integration tests confirm ordering and exact values |
| Trimming only at set boundaries | Pass | Surrounding whitespace accepted; interior whitespace rejected |
| Empty, empty-entry and duplicate sets | Pass | Constructor and built-CLI probes reject each case |
| Control, overlong and excessive entries | Pass | Constructor and built-CLI probes reject each case at the stated limits |
| Invalid set fails before PostgreSQL or trusted config | Pass | Built binary was run with a sentinel unreachable database URL and malformed `CLAWBRIDGE_CONFIG`; it failed at audience validation without reading either |
| Unlisted audience rejected | Pass | Synthetic signed token for an unlisted client failed production verifier |
| Wrong issuer, expired token, unverified email and invalid signature | Pass | Existing production-verifier crypto regression remains passing |
| Invalid input not reflected | Pass | Subprocess diagnostics contain neither the malformed audience nor the test DSN/password |
| New set is copied, caller mutation cannot alter trust | Pass | Constructor test mutates its input after construction and verification retains the original set |

The focused independent command was:

```sh
go test ./internal/server ./internal/cli ./integration \
  -run 'TestParseAudiences|TestNewGoogleVerifier|TestServeRejectsInvalidAudience|TestServeAcceptsOrderedAudience|TestServeDoesNotReadConfig|TestServerGoogleVerifierSyntheticCrypto' \
  -count=1 -v
```

It passed. The reviewer added
`integration/multi_audience_test.go`, a built-binary probe that covers malformed
sets and the pre-PostgreSQL/config boundary without adding an implementation
seam. Its focused invocation also passed:

```sh
go test ./integration \
  -run TestServeValidatesAudienceSetBeforePostgres \
  -count=1 -v
```

The frozen full gates were then rerun on the final tree:

| Command | Result |
| --- | --- |
| `go test ./... -count=1` | Pass; integration 28.759s, every package passed |
| `go test -race ./... -count=1` | Pass; integration 28.454s, every package passed |
| `go vet ./...` | Pass; no diagnostics |
| `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o ... ./cmd/clawbridge` | Pass; Mach-O 64-bit executable arm64 |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ... ./cmd/clawbridge` | Pass; statically linked ELF 64-bit x86-64 executable |
| `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o ... ./cmd/clawbridge` | Pass; PE32+ console executable x86-64 |
| `CLAWBRIDGE_TEST_RESOURCE_LOG=... integration/run-postgres.sh go test ./... -count=1` | Pass; real PostgreSQL 17.11, integration 30.266s |
| `CLAWBRIDGE_TEST_RESOURCE_LOG=... integration/run-postgres.sh go test -race ./... -count=1` | Pass; real PostgreSQL 17.11 with race detector, integration 37.104s |
| `git diff --check` | Pass; no whitespace errors |

Docker Engine 29.8.2 was healthy before the disposable runs. The runner used
the `postgres:17-alpine` image, a random test-only password, random loopback
ports, anonymous volumes and cleanup traps. The exact ledger was:

| Purpose | Container | Loopback | Anonymous volume | Cleanup |
| --- | --- | --- | --- | --- |
| Full suite | `8a4fc3e962d54e26bd3175d7db8bcf4a40c56407e5107e2d01e338badcb7ac3d` | `127.0.0.1:32772` | `7811b47a730adafcb9232dd0694f0d55542de7167f292532f2b0356e5d6f0693` | Container removed; volume confirmed absent |
| Full race suite | `5c428252fc5d5b680233607c7edc343e88cc5b846d2275224efb1872716868d5` | `127.0.0.1:32773` | `c7d9936b1969aceb2335e6c81381afc20e576d20d540c7f173e771014e650e87` | Container removed; volume confirmed absent |

Post-run checks found no matching reviewer container. Both recorded anonymous
volumes returned `ABSENT` from `docker volume inspect`. The six existing Docker
containers were not touched.

The documentation check found the README scope example limited to
`.clawbridge.toml` database and scope fields, with explicit prose that repository
routing and mode fields are ignored. The README also includes browser/device
login, doctor and logout, while preserving the live-Google limitation.
`COORDINATION.md`, `CORE-API.md` and `PHASE-3-PLAN.md` describe the
multi-audience limits, validation behavior, single-client cloud routing and
exact-audience verification without claiming a live Google run. The later
discovery contract initially exposed a documentation mismatch in
`PHASE-3-PLAN.md` and `README.md`. The engineer corrected both documents to
describe applicable-ID discovery and explicit-ID override before declaring the
tree frozen; they now agree with `CORE-API.md` and `COORDINATION.md`.

The multi-audience result is **Pass for the frozen local and synthetic contract**.
It was established with synthetic signed tokens and local HTTP/PostgreSQL
fixtures. It is not evidence of a live Google-issued browser or device token,
Google consent, IAM or deployed Compose/Caddy behavior. Those rows were Pending at
that checkpoint; the final section records the later non-interactive live passes,
while interactive consent remains Pending.
No implementation defect is promoted, and no implementation, owner contract,
real agent configuration or cloud resource was changed.

## Historical Compose/public-config owner gate before R-live deployment

Review date: 20 October 2026. Acceptance target: shared dirty checkout at HEAD
`9f3f04be1cecdeab5e620f38446308894255476e` plus the frozen uncommitted
Compose/public-config and OAuth changes. Go was 1.27.0 on darwin/arm64, Docker
Engine was 29.8.2, Docker Compose was v5.5.0, and the disposable image was
`postgres:17-alpine` at
`sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24`,
reporting PostgreSQL 17.11.

The reviewer reran every owner gate after strengthening the public-config probe
to require that both successful discovery and rejected write methods leave the
Google verifier and application executor call counts at zero. No implementation
or owner document was edited. This table records the checkpoint before the later
explicit freeze and four-field deployment; the final result is at the end of this
report.

| Command | Result | Exact evidence |
| --- | --- | --- |
| `go test ./... -count=1` | Pass | Every package passed without the PostgreSQL DSN; integration completed in 23.963s |
| `go test -race ./... -count=1` | Pass | Every package passed under the race detector; integration completed in 27.150s |
| `go vet ./...` | Pass | No diagnostics |
| `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /dev/null ./cmd/clawbridge` | Pass | Exit 0 |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /dev/null ./cmd/clawbridge` | Pass | Exit 0 |
| `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /dev/null ./cmd/clawbridge` | Pass | Exit 0 |
| `CLAWBRIDGE_TEST_RESOURCE_LOG=/tmp/clawbridge-compose-final-pg-full-20261020.log integration/run-postgres.sh go test ./... -count=1` | Pass | All packages passed with real PostgreSQL; integration completed in 30.240s |
| `CLAWBRIDGE_TEST_RESOURCE_LOG=/tmp/clawbridge-compose-final-pg-race-20261020.log integration/run-postgres.sh go test -race ./... -count=1` | Pass | All packages passed with real PostgreSQL and the race detector; integration completed in 35.349s |
| `go test ./integration -run 'TestPublicOAuthConfigContract\|TestComposePublicHTTPSAndPersistenceContract\|TestServeValidatesAudienceSetBeforePostgres' -count=1 -v` | Pass | All focused tests and seven invalid-audience subtests passed |
| `git diff --check` | Pass | No whitespace errors |

The exact disposable PostgreSQL ledger is:

| Suite | Container | Loopback | Anonymous volume | Cleanup proof |
| --- | --- | --- | --- | --- |
| Full | `eb18178a0d13e5034b11e68d99d737294b78f68751d7bc7063210c0b3c6eebd0` | `127.0.0.1:32775` | `a6493985c608d3b4966006e4ea427ebd8290f6df11219421dd9af3fc1964569a` | Runner removed the container and attached volume; both inspected `ABSENT` afterward |
| Race | `d141ae8b6dd53ab2a571995e749cd230af96a8bad5fa22a47cd68a8e864240a0` | `127.0.0.1:32776` | `4db40abac622f6fe3d183af2911f8d0bc94f3eecb74f462cc5e306e24908c2cf` | Runner removed the container and attached volume; both inspected `ABSENT` afterward |

The runner generated a random password, exposed PostgreSQL only on random
loopback ports, used anonymous volumes and cleanup traps, and supplied the DSN
only through `CLAWBRIDGE_TEST_POSTGRES_DSN`. No database credential entered the
repository. Post-run inspection found no matching reviewer container or volume;
the eight pre-existing containers and two pre-existing volumes were untouched.

At this earlier checkpoint the public configuration response was an
unauthenticated strict two-field object. The frozen R-live contract requires an
unauthenticated strict four-field installed-app credential object with no other
sensitive routing or credential fields. Both versions require
`Cache-Control: no-store`, invoke neither authentication nor execution, and
return 405 for tested non-GET methods. The pre-final synthetic shape,
pairing, no-auth/no-execution and Compose environment probes passed before the
final freeze. The final post-redeployment live probe passed. Invalid OAuth client IDs and
invalid audience sets fail before PostgreSQL initialization. The built-binary
probe confirms malformed audiences are not reflected and database credentials
are not exposed.

The normalized Compose contract attaches DB only to the internal `backend`
network; attaches API and Caddy to both `backend` and non-internal `egress`; gives
API and DB no host ports; and publishes only TCP 443 on Caddy. Caddy disables
HTTP redirects, persists `/data` and `/config`, and does not require port 80.
The API's egress attachment is intentional for Google token-signing certificate
access. The API image is nonroot with a read-only filesystem, all capabilities
dropped and `no-new-privileges`; PostgreSQL uses the configured host bind when
`CLAWBRIDGE_POSTGRES_DATA_PATH` is absolute and the named volume when unset.

The current deployment documentation consistently states that only Caddy is
public, DB stays on the internal backend, and both Caddy and API use egress for
ACME/OCSP or Google signing-certificate access. It documents the Caddy HTTPS
user path replacing normal-user IAP access, TCP-443-only TLS-ALPN behavior, the
separate persistent disk setting and its ownership requirement. Cloud Run and
Cloud SQL remain historical alternatives and are not acceptance criteria.

This historical checkpoint was Pass for the frozen server/Compose/public-config
slice. The later engineer freeze, full rerun and non-interactive live probes are
recorded in the final section below. Interactive browser/device authorization and
real-keychain lifecycle remain Pending by owner direction; no real agent
configuration was touched.

## Final frozen acceptance after four-field deployment

Review date: 20 October 2026, 22:39 IST. cb-engineer explicitly declared the
implementation frozen. Acceptance target:

- Repository: `/Users/oriixchan/orca/workspaces/ClawBridge/auk`
- Branch and baseline: `main`, `HEAD 9f3f04be1cecdeab5e620f38446308894255476e`
- Initial freeze fingerprint when the heavy gates started, excluding this
  evolving report:
  `sha256:1b4c5aa18b7f98d5f372e0f48d1fcf81a8813badcbd7cfe9c2fe30b55dd8bcdb`
- Final source-plus-reviewer-tests fingerprint, excluding this evolving report:
  `sha256:fccd5497cb7efb63b02f67488f132e2bd55773805ff1aa21ca76853ae4e2b205`
- Frozen implementation/owner-doc/deployment fingerprint, excluding
  `integration/**` and this report:
  `sha256:99c95f31202afa48d7a1181c5bd9b8d454a8250a1d0dbf25f5206666cdcea9f0`
- Toolchain: Go 1.27.0 darwin/arm64; Docker Engine 29.8.2; Docker Compose v5.5.0
- Disposable image: `postgres:17-alpine`, image
  `sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24`,
  reporting PostgreSQL 17.11

The full/race/vet/cross-build/PostgreSQL sequence began immediately after the
freeze. A later reviewer-only correction replaced the live HTTP fixture's
synthetic-value assertion with exact-key/non-empty live validation so the probe
could run without exposing production values. No implementation, owner contract
or deployment file changed. The focused synthetic, live-shape and live-client
probes were rerun after that reviewer-only edit.

### Heavy gate results

| Command | Result | Exact evidence |
| --- | --- | --- |
| `go test ./... -count=1` | **Pass** | Every package passed; integration completed in 33.764s, process wall time 36.92s |
| `go test -race ./... -count=1` | **Pass** | Every package passed; integration completed in 36.787s, wall time 40.58s |
| `go vet ./...` | **Pass** | No diagnostics, wall time 0.65s |
| `env CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o /tmp/clawbridge-cross.Cw4Bpf/clawbridge-darwin-arm64 ./cmd/clawbridge` | **Pass** | Mach-O 64-bit executable arm64, 3.99s |
| `env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/clawbridge-cross.Cw4Bpf/clawbridge-linux-amd64 ./cmd/clawbridge` | **Pass** | Statically linked ELF 64-bit x86-64 executable, 23.24s |
| `env CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o /tmp/clawbridge-cross.Cw4Bpf/clawbridge-windows-amd64.exe ./cmd/clawbridge` | **Pass** | PE32+ console executable x86-64, 22.05s |
| `bash integration/run-postgres.sh go test ./... -count=1` | **Pass** | PostgreSQL 17.11; integration 40.278s; total 59.04s |
| `bash integration/run-postgres.sh go test -race ./... -count=1` | **Pass** | PostgreSQL 17.11 with race detector; integration 42.486s; total 62.02s |
| Focused four-field/OAuth/routing/doctor/Compose/backup acceptance block | **Pass** | `go test ./integration`, 24 named tests, integration 26.864s, wall time 29.63s |
| Deployment docs/script consistency check | **Pass** | `sh -n`; all 9 rendered Compose variables documented; 3 systemd files present and checked for their documented paths/schedule; 30-day `postgres/daily/` lifecycle valid. ShellCheck is unavailable in the review environment |
| `git diff --check` | **Pass** | No whitespace errors |

The cross-build directory was deleted after `file` inspection and verified
absent.

The focused block covered exact four-field discovery and pairing, missing,
unknown, duplicate, non-string, trailing, redirect, timeout, oversized and
non-2xx rejection; PKCE/state; device pending/slow-down/denial/expiry; runtime
mint; principal refusal before save; rollback for new and replaced keychain
entries; promoted device key and idempotent logout; credential-source
exclusivity; trusted/repository isolation; OAuth installer route; MCP environment;
CLI/doctor/installer secret redaction; project-list representations; Compose
topology; unauth 401; and fail-closed backup behavior.

The Compose gate rendered DB only on internal `backend`, API and Caddy on
`backend` plus non-internal `egress`, no API/DB host ports, and only Caddy TCP 443.
It required Caddy `/data` and `/config` volumes, `--disable-redirects`, the
configured persistent Postgres data path, and the two OAuth secrets only in API
environment. No secret appeared in argv, MCP environment or installer output.

### Disposable PostgreSQL ledger and cleanup

| Suite | Container | Loopback | Anonymous volume | Cleanup proof |
| --- | --- | --- | --- | --- |
| Full | `08cd62acea14d38d94470df75f4cfa7b60999b1e0f0e8e7528cc63090e045ca0` | `127.0.0.1:32780` | `04a9506a562eb632d9281f7ebdb9c49e3863203ae5c9896ab2b3029aaecac40c` | Runner removed both; independent `docker inspect` returned absent |
| Race | `e5417ac7cbdad711cbc4d2807164d4fcc213312f84f01c6a694aa5f4f93c0799` | `127.0.0.1:32781` | `361a47f2644a50fc8e8cd467ab92cabba675c34ac752323545aeaa2665c57e6c` | Runner removed both; independent `docker inspect` returned absent |

The runner generated independent random passwords, published each database only
on loopback, used an anonymous volume, supplied
`CLAWBRIDGE_TEST_POSTGRES_DSN` only through the test environment, and cleaned on
exit. The reviewer confirmed all two container IDs and two volume IDs absent
after the runs. No unrelated container or volume was touched.

### Deployed non-interactive live evidence

Endpoint: `https://34-142-66-102.sslip.io`. The reviewer independently ran:

```sh
curl --fail --silent --show-error --max-time 10 \
  https://34-142-66-102.sslip.io/healthz
```

Result: HTTP 200, `{"status":"ok"}`.

```sh
CLAWBRIDGE_TEST_PUBLIC_URL=https://34-142-66-102.sslip.io \
go test ./integration -run '^TestDeploymentLiveHTTPSContract$' -count=1 -v
```

Result: **Pass**, 0.794s. `/v1/config` was one exact four-field JSON object with
four non-empty strings, no trailing content, and matching Google client-ID shape.
`Content-Type` was JSON and `Cache-Control` was `no-store`; no credential value
was printed. Unauthenticated `POST /v1/execute` returned 401
with `error.code=unauthenticated` and without invoking an accepted identity or
executor.

```sh
openssl s_client -connect 34-142-66-102.sslip.io:443 \
  -servername 34-142-66-102.sslip.io -tls1_3
```

Result: TLS 1.3 with `TLS_AES_128_GCM_SHA256`; peer
`CN=34-142-66-102.sslip.io`; Let's Encrypt issuer; verify return code 0.

An independent TCP probe of the public endpoint found only 443 open:

```text
80 closed
443 open
5432 closed
8080 closed
```

```sh
CLAWBRIDGE_TEST_PUBLIC_URL=https://34-142-66-102.sslip.io \
CLAWBRIDGE_TEST_LIVE_GOOGLE_OAUTH=1 \
go test ./integration \
  -run '^(TestLiveGoogleDiscoveredClientsExchangeWithMatchingSecrets|TestLiveDeviceLoginDiscoveryReachesPollingNoGcloud)$' \
  -count=1 -v
```

Result: **Pass**, 8.309s. The device-code poll and Desktop authorization-code
dummy grant each reached Google's expected grant failure with their matching
discovered secret; neither reported a missing or mismatched client secret. The
isolated built CLI, with temporary home/config and a `PATH` containing no
`gcloud`, discovered the four values, printed the code prompt, reached recurring
polling, left trusted config byte-identical, exposed no discovered credential,
and was canceled before consent.

Owner-supplied deployed evidence, retained separately because the reviewer did
not repeat the gcloud call during the pause: API/Caddy were recreated, healthy
Postgres stayed running; gcloud compatibility `whoami` returned
`source=server`, `storage_mode=cloud`, `verified=true`, workspace `pilot`,
project `clawbridge`, and the expected service-account principal/subject.

### Final result and paused live checklist

**PASS** means the explicitly frozen Phase 3 automated, local, disposable
PostgreSQL, Compose/backup, deployed public-config, HTTPS, 401, Google
matching-client and no-gcloud device-polling acceptance passed. It does not claim
interactive consent or real keychain/lifecycle behavior.

No implementation defect remains reproduced. Optional setup create/join
automation and TUI remain scaffolds under the frozen scope. Automatic per-user
service-account provisioning, duplicate-name collision policy and IAM Condition
expiry remain future setup/deployment work. Cloud Run and Cloud SQL are historical
alternatives, not acceptance criteria.

The owner deliberately paused before these checks. On resume:

1. Complete browser consent in an isolated fixture and verify PKCE state, code
   callback, Google ID-token verification, keychain-only refresh-token save,
   active browser ID/secret/audience/config transaction and no credential echo.
2. Complete device consent and verify the stored key uses the promoted device
   client ID, its matching secret is active, runtime refresh/mint succeeds,
   repeated logout removes the key, and failed config publication restores prior
   credential/config state.
3. Run a real-Google runtime refresh after restart, with no gcloud fallback, and
   prove credential failure blocks mirror fallback.
4. Reboot the VM/stack and verify Postgres persistence, session incarnation
   behavior, Caddy certificate continuity, `/healthz`, public discovery,
   authenticated read and authenticated write.
5. Run the actual scheduled backup job, verify the same-timestamp custom dump and
   checksum pair, then restore into a disposable Postgres target and compare
   scoped state without touching production.
6. Recheck that no token, refresh token, code, verifier, client secret,
   environment content, DSN/password or credential-file content appears in
   logs, argv, rendered config, MCP entries or agent configuration.

All reviewer changes are uncommitted and unstaged. The reviewer did not commit,
stage, push, deploy, mutate real agent configuration, or create paid resources.

## Resumed checkpoint: current-stage evidence and deferred full audit

Owner resumed on 20 October 2026 after 22:49 Europe/Dublin. The prior scoped
PASS above remains unchanged; it is not acceptance of the pending live lifecycle
checks or a completed implementation-wide manual audit. HEAD remains
`9f3f04be1cecdeab5e620f38446308894255476e`, with the shared worktree dirty.

The engineer owns the current backup timer/upload/isolated-restore and
mount-guard/reboot validation. Root owns design and the browser/device/real
OS-keychain consent fixture. No new completion evidence for those checks was
available in the coordination/report checkpoint read by the reviewer. They
remain **Pending**. Reviewer preparation has not started the full audit: an
explicit root **STAGE COMPLETE** is required first, even though the owner's
after-18:00 timing condition is satisfied.

### Current-stage independent verification checklist

| Evidence to receive | Independent reviewer check | Status |
| --- | --- | --- |
| Browser and device consent fixture | Distinguish actual Google consent and OS-keychain subprocess execution from injected stores; verify matching ID/secret/audience promotion, identity, restart refresh, logout and rollback without exposing credentials | Pending root evidence |
| Backup service/timer and upload | Check exact unit invocation/result, timer activation/next run, same-timestamp dump/checksum pair, checksum verification and failure visibility; fake-runner results alone do not prove scheduled uploads | Pending engineer evidence |
| Isolated restore | Verify disposable target identity, dump integrity, restore result and scoped state comparison; confirm cleanup and no production-target writes | Pending engineer evidence |
| Mount guard and reboot | Verify missing-mount refusal, actual reboot identity/times, correct mounted disk/data path, retained DB state, Caddy continuity and authenticated read/write | Pending engineer evidence |
| Secret safety | Review sanitized evidence for logs/argv/MCP/config/artifacts; never copy token, code, verifier, refresh token, credential values or DSN into this report | Pending current-stage evidence |

### Prepared full implementation audit scope — not yet executed

After root explicitly declares **STAGE COMPLETE**, manually trace shipped entry
points and trust boundaries against DESIGN/CORE-API/phase contracts, then add
targeted black-box regressions only where a concrete gap warrants them. Existing
heavy-suite passes will be reused for unchanged code; new checks will be selected
for changed code, reproduced failures or unresolved audit concerns.

| Surface | Planned manual/security/correctness review |
| --- | --- |
| Local/PG store and authorization | Transaction boundaries, schema/dialect parity, owner/role/session-instance checks, workspace/project isolation, replay mismatch and concurrent exactly-once sends, retirement/successor cycles, TTL, cursor/thread/reply semantics and delivered/read state |
| Server/cloud transport | Verified identity provenance, exact audiences, public discovery schema, bounded requests/timeouts, typed denials, cancellation, concurrent principals, redirection/TLS and error redaction |
| OAuth and runtime credentials | PKCE/state, device timing/error handling, client/secret/audience pairing, ExpectedPrincipal, keychain namespace, atomic save/config rollback, refresh/mint failure classification and oauth/gcloud/adc exclusivity |
| Mirror | Complete scoped snapshot publication, principal/session/instance binding, expiry/pagination, permitted outage reads only, no authentication-failure fallback or offline state mutation |
| CLI/config/MCP | Explicit/home/env/repository precedence, repository scope-only rule, pinned immutable MCP actor context, subprocess transport/lifecycle, cancellation, safe argv/env/output and compatibility |
| Installer/doctor | Consistent reconstructed routes, required-status/exit semantics, secret omission, unrelated-config preservation, idempotence, invalid-config refusal and atomic writes |
| Setup/TUI | Inspect every shipped reachable path, confirmation/failure behavior and cloud/local boundaries; distinguish delivered behavior from explicitly scoped scaffolds and future provisioning |
| Deployment/operations | Compose isolation/egress/443, environment-only credentials, Caddy persistence, persistent-disk mount guards, backup shell/unit/timer/lifecycle/restore failure paths and documentation consistency |
| Cross-cutting | Races and shared mutable state, rollback/partial failure, context/resource cleanup, dependency/API compatibility, audit actor truth, historical Phase 1/2 regression coverage and evidence limits |

No implementation edits, heavy reruns, deployment actions, new agents, staging
or commits were performed for this preparation. The full audit and all unfinished
live gates remain open; final docs/commit/push are not claimed complete.

### Current-stage operational evidence update — owner report received

Owner reports that the engineer completed an actual backup-service dump/checksum
upload, downloaded the pair, and restored it into an isolated Postgres target.
Reported table counts are workspaces/projects/sessions/messages = **1/1/4/3**.
Reboot is underway. These are owner-supplied results, not yet independently
reconciled with the engineer's sanitized command/result ledger. The historical
pause checklist above is superseded only to this extent; the independent
operational gates remain open.

The reviewer read `deploy/README.md`'s service, upload and restore procedures.
The reported operations align with its intended logical-backup path. Final
reconciliation still requires the object timestamp and checksum verification,
service result and timer state, restore schema version, isolated container/network
identity and cleanup, plus known-state comparison beyond counts. A manually
started service proves that execution, not that a scheduled timer has fired.
Counts alone do not establish message contents or complete recovery. Actual
reboot completion, mount guard behavior, persistent state and endpoint recovery
remain Pending engineer evidence.

Root's documentation-only identity correction is retained: a verified
impersonated service-account ID token establishes that service-account principal;
it does **not** independently establish the initiating human. Earlier
service-account `whoami` evidence must be read with that limit. Real browser/device
consent and OS-keychain lifecycle remain Pending. No **STAGE COMPLETE** has been
received, so the prepared full implementation audit has not started and unchanged
heavy checks have not been rerun.
## Full implementation audit — STAGE COMPLETE received

The owner explicitly authorized this audit after completing the operational and
end-user stage. Review target: HEAD
`9f3f04be1cecdeab5e620f38446308894255476e` plus the shared dirty worktree.
No implementation edits, staging, commits or deployment are authorized here.

The reviewer reread `PHASE-3-LIVE-ACCEPTANCE.md` and the corrected operational
sections of `deploy/README.md`. The earlier documentation mismatch is **Closed**:
the current runbook records completed browser/device consent and native macOS
keychain lifecycle, distinguishes historical recovery counts `1/1/4/3` from
post-acceptance `1/1/5/4`, and retains the appropriate evidence limits. These are
reconciled root/engineer execution records, not reviewer-repeated live consent,
reboot or restore. The previous Pending stage entries are historical checkpoints.
Actual manually invoked backup uploads, downloaded checksum/isolated restore,
mounted reboot and private-namespace missing-mount predicate are supported.
Unattended timer firing, physical disk loss, native Linux/Windows keychains and
physical removal of gcloud remain unclaimed. Synthetic rollback tests do not
become destructive live-Google rollback evidence.

### Audit finding A1 — Closed after independent retest: doctor MCP environment

Owner: engineer (`internal/doctor`). Reproduction:

```sh
go test ./integration -run '^TestAuditDoctorRejectsSecretBearingMCPEnvironment$' -count=1 -v
```

Result: **Fail**, `doctor passed a secret-bearing MCP env as matching the safe
OAuth route`. The reviewer-owned test installs a valid OAuth entry into a
temporary Claude Code JSON file, adds a synthetic
`env.CLAWBRIDGE_OAUTH_CLIENT_SECRET`, and invokes `doctor.Run` with the same
expected pinned route. Check `mcp.claude-code.clawbridge` incorrectly returns
Pass. No secret value is printed; report redaction itself passes.

`internal/doctor/doctor.go` `entryMatches` compares argv/type but ignores `env`.
The CORE-API MCP routing contract forbids OAuth client-secret values in installed
argv/environment and requires conflicting installed routes to produce a
diagnostic failure or warning. Impact is a false assurance in installed-route
diagnostics; this test does not claim that the installer generates secrets or
that a peer can edit another user's configuration. This describes the original
reproduction; the engineer subsequently fixed environment inspection.

Independent retest after engineer refreeze: **Pass** for the exact A1 probe and
the three browser-after-device subprocess cases in
`TestPhase3OAuthDiscoveryCLIFailClosed`. Browser acquisition uses the rediscovered
Desktop client; failed discovery and a rediscovered known device ID stop before
acquisition and leave the trusted configuration unchanged. A distinct explicit
Desktop pair retains the discovery bypass. These use a synthetic HTTP server and
browser denial callback, not Google consent or native keychain writes. Earlier
separate-config live evidence does not establish a live device-to-browser switch.

### Audit finding A2 — Closed after independent retest: setup gcloud command

Owner: engineer (`internal/setup/setup.go`, `GCloudRunner.Run` / `Apply`).

```sh
go test ./integration -run '^TestAuditSetupRunnerUsesReviewedCommandWithoutDuplicateExecutable$' -count=1 -v
```

Independent result: **Fail**, `first argument="gcloud", want projects`.
`BuildPlan` commands start with `gcloud`; `Apply` passes the full command to
`GCloudRunner`, which independently chooses the executable and passes the full
slice as arguments. Thus the real adapter would execute
`gcloud gcloud projects describe ...`. The probe uses only a temporary fake
executable recording its first argument; no Google command/resource is touched.
This is an existing scaffold adapter defect, not a claim that optional setup
create/join automation must be delivered in the minimum CLI slice.

### Audit finding A3 — Closed after independent retest: TUI terminal controls

Owner: engineer (`internal/tui/view.go`, `Render` / `clip`).

```sh
go test ./integration -run '^TestAuditTUIEscapesPeerTerminalControls$' -count=1 -v
```

Independent result: **Fail**, `TUI emitted a peer-controlled OSC clipboard sequence`.
An isolated local store delivers a peer message containing an OSC 52 sequence;
the Inbox model's rendered string retains the escape bytes. `clip` truncates
runes but does not remove terminal control sequences. A real terminal renderer
could interpret peer text as clipboard/title/control instructions. The probe
inspects bytes in memory and deliberately never prints or executes the sequence.
This concerns the implemented TUI scaffold renderer; absent CLI wiring is not
promoted as a scope defect. A2/A3 were sent to root and engineer for disposition.

### Full audit coverage and current verification boundary

Manual review covers the shipped packages and entry point, not only their diff:

| Surface | Reviewed security/correctness boundaries | Audit status |
| --- | --- | --- |
| SQLite/PG bus | Transaction/rollback, immutable request identity, role versus routing role, session incarnation, peer changes/audit actor, operation replay, message participation, pagination, retention, snapshot isolation/caps | Reviewed; retained automated/real-PG regressions apply |
| HTTP/server | Exact audience set, official signature validation and malformed-token guard, identity attribution, public four-field installed-app discovery, startup validation, body/time bounds, sanitized errors | Reviewed; live and synthetic evidence remain separately identified |
| OAuth/remote/mirror | Loopback state/PKCE, device polling, matching client/credential, principal precheck, staged keychain/config rollback, active-entry logout, backend exclusivity, definitive credential failure, no redirect credential forwarding, exact mirror binding/atomic publication/expiry | Reviewed; browser-after-device newly covered synthetically |
| CLI/config/install/MCP/doctor | Trusted routing precedence, repository scope-only projection in both directions, pinned process/instance, safe argv, installer lock/atomic update/unrelated preservation, diagnostic redaction and env inspection | Reviewed; A1 independently closed |
| Setup/TUI scaffolds | Plan/confirmation fence, executable tokenization/no shell, read-only navigation, pinned scope, offline writes rejected, stale asynchronous result handling, terminal rendering of peer data | Reviewed; A2/A3 independently closed after corrected freeze |
| Deployment | Caddy-only 443, API egress/no host ports, DB internal-only, persistent data mount guard, private env, backup readiness/dump/archive/checksum/upload failure handling and cleanup | Reviewed; actual runbook evidence reconciled; physical disk loss and unattended timer firing unclaimed |

Additional independent commands after A1/browser-transition refreeze:

```sh
go test ./integration -run '^(TestAuditDoctor|TestPhase3OAuthDiscoveryCLIFailClosed)' -count=1
go test ./internal/cli -run 'TestBrowserAfterDevice|TestLogin.*(Failure|RollsBack)|TestDeviceLoginPromotes|TestExplicitOAuthClient' -count=1
git diff --check
```

All **Pass**. Expanded A1 black-box diagnostics cover benign env, credential and
routing variables, malformed maps, non-string values and inherited-variable
lists. No environment value is reflected. The earlier combined discovery/scaffold
command failed solely on the two A2/A3 probes; all discovery subtests passed.
New TUI regression coverage spans overview, project/session names, inbox,
diagnostics and setup text with C0/C1/OSC/non-SGR CSI controls and Unicode.
The initial broader scanner incorrectly counted trusted SGR styling and Unicode
format characters as terminal commands. It was corrected to allow trusted SGR
and preserve joined Unicode emoji; no new implementation defect was promoted
from those reviewer-fixture assumptions.

Engineer declared A2/A3 **CODE REFREEZE**. Independent command
`go test ./integration -run '^TestAudit(Setup|TUI)' -count=1 -v` now **Pass**,
including both exact original reproductions, all six broader dynamic surfaces,
visible escaping, Unicode/emoji preservation and unchanged stored message data.
Manual reread confirms the gcloud adapter validates the first token and executes
only tail arguments without a shell. Renderer escapes dynamic text before trusted
styling, including drafts/diagnostics/scope and message fields. **A2/A3 closed**.

Final audit remains **Open** only for justified final acceptance on the changed
implementation. Existing earlier scoped passes remain historical. Current
acceptance HEAD is `9f3f04be1cecdeab5e620f38446308894255476e` plus the shared
dirty tree. Frozen implementation SHA-256 fingerprints:

```text
37a6e0f423877b9d419d12d6c8242818da1e337e7c9fdd5c3d57177590a51e52 internal/setup/setup.go
77f64b7920d7495e591050493ef0e8149c3bc41f242c1d32c6f5a919de47fcd5 internal/tui/view.go
e7431788286589a34b64e8df716c34ec2353afbb96a9a52b72d7197c09013ee6 internal/doctor/doctor.go
34e02cfc2cdf2c35421524c12a25a689a8f76710e660c81c8b31bb4227b05b65 internal/cli/cli.go
```

Final full suite is running through the disposable-PG runner, so PostgreSQL cases
execute rather than skip. Full race follows sequentially with its own disposable
fixture; vet/cross builds follow. No implementation, deployment or real user
configuration has been modified by the reviewer.


Postgres test run: container `4fae82aab56952021e4d0fec14aacd733a83015258c34f3d0c45830f2f1299f4`; loopback `127.0.0.1:32782`; anonymous volume `2b2ef7b19aabbd8cd152cb96beae418aa639ab0d86d2e0a42867fc6603952ccc`.
Cleanup: container `4fae82aab56952021e4d0fec14aacd733a83015258c34f3d0c45830f2f1299f4` removed with anonymous volume.

Postgres test run: container `fa07784193045b7dc339319f2cb31f2fbacbd42d7f0a02d179f447399e1033fd`; loopback `127.0.0.1:32783`; anonymous volume `89e07a44360a835de4471cbd8318d0a20a72add5f503c48d7ffa74a60851e9c1`.
Cleanup: container `fa07784193045b7dc339319f2cb31f2fbacbd42d7f0a02d179f447399e1033fd` removed with anonymous volume.

### Final independent full-audit result — PASS, 20 October 2026 22:10 UTC

The owner-requested manual implementation audit and justified post-fix gates
are complete against the above HEAD/dirty-tree fingerprints. **No unresolved
reproduced implementation finding remains.** A1, A2 and A3 are independently
closed; the browser-after-device transition meets its published synthetic
contract. This final status supersedes earlier Open/Pending checkpoints for these
audit findings, without rewriting their original reproductions.

| Independent command | Result |
| --- | --- |
| `CLAWBRIDGE_TEST_RESOURCE_LOG="$PWD/PHASE-3-REVIEW.md" bash integration/run-postgres.sh go test ./... -count=1` | **Pass**; all packages, integration 36.321s; actual PostgreSQL 17.11 |
| `CLAWBRIDGE_TEST_RESOURCE_LOG="$PWD/PHASE-3-REVIEW.md" bash integration/run-postgres.sh go test -race ./... -count=1` | **Pass**; all packages, integration 41.344s; fresh actual PostgreSQL 17.11; no race reported |
| `go vet ./...` | **Pass**, exit 0 |
| `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o <temporary-output> ./cmd/clawbridge` | **Pass** |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o <temporary-output> ./cmd/clawbridge` | **Pass** |
| `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o <temporary-output> ./cmd/clawbridge` | **Pass** |
| `git diff --check` | **Pass**, exit 0 |

The full suites retain Phase 1/2 isolation, authorization, concurrent-send,
HTTP/PG/mirror, routing/config, installer/MCP/doctor, discovery/pairing/redaction,
rollback and Compose/backup fake-failure regressions. PostgreSQL was explicitly
enabled by the runner, not skipped. Both fixtures used loopback-only random
ports, random environment-only test passwords and disposable anonymous volumes.
The resource records immediately above identify each full/race fixture. A final
independent inspection with Docker healthy confirmed both exact container IDs and
both exact volume IDs absent. Cross-build artifacts were confined to a temporary
directory and removed. Frozen implementation fingerprints were unchanged at the
final check. No staging, commit, push, deployment or real agent config edit was
performed by the reviewer.

Live limits remain explicit. Root/engineer evidence reconciles actual separate
browser/device Google consents, macOS native keychain/fresh-process refresh,
post-logout failure despite mirror, actual backup upload/checksum/isolated
restore, mounted reboot and missing-mount predicate. The new device-to-browser
transition is independently proven with synthetic discovery/browser-denial and
injected acquisition/credential tests, **not a new real Google consent switch**.
Windows/Linux native keychains, physically uninstalled gcloud, unattended timer
firing, physical disk-loss recovery and live model-driven Claude/Codex behavior
remain unclaimed. Setup automation/TUI remain callable scaffolds; automatic
per-user service-account provisioning, duplicate-name collision handling and IAM
Condition expiry remain future work. Cloud Run references describe historical
plans; the accepted deployment direction is Compose/Caddy on Compute Engine.


### Independent exact-SHA pilot postdeploy check — PASS, 2026-10-20 22:27 UTC

Reviewed deployment: `f913ffa80443a47bf0faa62b55936a14802c570e`, endpoint
`https://34-142-66-102.sslip.io`. This is a focused read-only postdeploy check;
no heavy suites, new Google consent or keychain lifecycle was repeated.

- `CLAWBRIDGE_TEST_PUBLIC_URL=https://34-142-66-102.sslip.io go test ./integration -run '^TestDeploymentLiveHTTPSContract$' -count=1 -v`:
  **Pass**, integration 0.552s. Actual HTTPS returned exactly the four required
  nonempty string fields, no extra fields/trailing content, JSON content type and
  `Cache-Control: no-store`; unauthenticated `POST /v1/execute` returned **401**
  with `error.code=unauthenticated`. Installed-app credential values were not
  printed or recorded.
- Independent Python standard-library TLS socket and HTTPS health read:
  **Pass**, TLS 1.3 with CA verification and hostname checking enabled;
  `GET /healthz` **200**, expected `{"status":"ok"}`.
- Independent read-only `gcloud compute ssh instance-claw-bridge-dev --project
  claw-bridge-dev --zone europe-west2-a --tunnel-through-iap --command ...`
  inspected only the deployment marker, source/archive hashes and API image
  identity/state. `/opt/clawbridge/repo/DEPLOYED_SHA` equals the exact requested
  commit. All four final PASS fingerprints (`internal/setup/setup.go`,
  `internal/tui/view.go`, `internal/doctor/doctor.go`, `internal/cli/cli.go`)
  match the values recorded above.
- Remote `/tmp/clawbridge-f913ffa80443.tar.gz` SHA-256 matches independently
  generated local `git archive --format=tar.gz f913ffa80443a47bf0faa62b55936a14802c570e | shasum -a 256`:
  `95cbed36b52ef2e5f6e51385cfddf551ef9959e7e81733a58fb3fc9cbe0aea52`.
  Running API image: `sha256:211e529908ce9a46667e84923a36094e0286e61f4b093f6126e00b2c80e1bfb2`.

The marker alone is operator evidence; independent archive and source comparisons
provide the additional deployment correspondence checked here. These checks do
not assert image reproducibility or a new end-user OAuth transition. Prior live
and operational limits remain as documented above. No implementation/deployment
changes, staging or commits were performed by the reviewer.


### Independent boot-disk cleanup verification — PASS, 20 October 2026 22:36 UTC

Owner-authorized targets were reviewed before deletion: inactive Docker builder
cache (no active build), downloaded APT packages, and the exact checksum-verified
`/tmp/clawbridge-f913ffa80443.tar.gz` transfer archive. No objection. Root
performed those three agreed operations after engineer messaging became blocked;
the reviewer performed only read-only VM/HTTPS checks and this report edit.

Independent IAP SSH baseline at 22:32 UTC and after-state at 22:34:47 UTC:

| Check | Independent result |
| --- | --- |
| Boot filesystem | **Pass**: 97% used / 329 MB free -> 38% used / 5.7 GB free |
| Persistent data filesystem | **Pass**: `/dev/sdb`, 49 MB used / 1%, unchanged; mount predicate succeeds |
| Containers | **Pass**: all five exact IDs unchanged; Phase 3 API/Caddy running, both databases healthy, Phase 2 API remains stopped |
| Images and volumes | **Pass**: all seven exact image IDs retained, including dangling rollback images; all four volume names retained, including the anonymous volume |
| Data and recovery | **Pass**: DB/Caddy bind/volume paths unchanged; all four repo/current/recovery directories remain; SQL counts `1/1/5/4` |
| Deployed implementation | **Pass**: marker remains `f913ffa80443a47bf0faa62b55936a14802c570e`; all four final PASS source fingerprints match |
| Private environment | **Pass**: mode 0600 and stable environment-file symlink intact; no contents printed |
| Backup and mount readiness | **Pass**: Docker active, timer active/enabled with next 02:30 UTC deadline; last backup `Result=success`, `ExecMainStatus=0`; loaded Docker mount guard remains present; executable installed runner matches repository runner SHA-256; Compose file present |
| Backup objects | **Pass**: read-only `sudo gcloud storage ls` confirms all four documented dump/checksum pairs remain (`215100Z`, `215427Z`, `222251Z`, `222706Z` on 20 October) |
| Disposable targets | **Pass**: APT archives now 16 KB; the previously validated exact transfer archive is absent as intended |

Focused command:
`CLAWBRIDGE_TEST_PUBLIC_URL=https://34-142-66-102.sslip.io go test ./integration -run '^TestDeploymentLiveHTTPSContract$' -count=1 -v`
**Pass**, integration 0.530s: exact four nonempty config fields, no-store/JSON,
unauthenticated execute **401** / `unauthenticated`. Independent Python TLS/health
read also **Pass**: verified TLS 1.3 with hostname checking, `/healthz` **200** and
expected JSON. No installed-app credential values were printed or recorded.

Reread root's `deploy/README.md` cleanup section: consistent with independent
observations and correctly attributes removal to root. The deleted transfer copy
was validated before deletion; future verification uses current sources, marker
and image, not the now-removed temporary archive. No heavy suite, service restart,
new backup execution, restore, reboot or consent was performed by the reviewer.
Prior platform/operational limits remain unchanged.


### Independent first unattended daily backup observation — PASS, 21 October 2026 06:08 UTC

Read-only checks against the existing pilot independently confirm the first
unattended 02:30 UTC timer execution. No manual service invocation or operational
change was made for this observation. Local baseline was clean pushed
`74075991ffca32c8b21b33d9a5780f88fcba1bc5`.

Using `gcloud compute ssh instance-claw-bridge-dev --project claw-bridge-dev
--zone europe-west2-a --tunnel-through-iap --command ...`, independently inspected
`systemctl show`, `systemctl list-timers`, the bounded 02:20–03:00 UTC service/timer
journal, and GCS object metadata/content reads:

| Evidence | Independent result |
| --- | --- |
| Schedule | **Pass**: daily 02:30 UTC; timer active/enabled; last trigger 2026-10-21 02:30:08 UTC; next deadline 2026-10-22 02:30 UTC |
| Execution | **Pass**: service started 02:30:08 UTC, exited 02:30:12 UTC, `Result=success`, `ExecMainStatus=0`; invocation `bd553e56e0514a88939024b1409e3cfe` |
| Journal | **Pass**: records start, dump/checksum uploads and successful completion in that same interval |
| New dump | **Pass**: `gs://clawbridge-backups-claw-bridge-dev/postgres/daily/20261021T023008Z.dump`, 28,047 bytes, generation `1792549810853037` |
| Companion checksum | **Pass**: same prefix, `20261021T023008Z.sha256`, 88 bytes, generation `1792549812402822` |
| Stored-content verification | **Pass**: independently streamed `gcloud storage cat` dump into `sha256sum` under `bash -o pipefail`; compared with the companion checksum and obtained `e1fe8d9702d737a1fea900639a26972945863751dfb1d3b91e9f25c7089a1f9d` |

No dump copy was written locally, and no dump contents or credentials were
printed. Reread engineer's `deploy/README.md` diff: timestamps, object generations,
sizes and digest match these independent observations. This supersedes earlier
unattended-firing Pending limits for this single observed execution. It does not
prove missed-schedule catch-up or restore of this new dump; prior isolated restore
evidence and other platform/operational limits remain separate. No heavy tests,
new consent, scheduling, deployment, staging or commit was performed.

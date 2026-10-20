# Phase 3: authentication, setup, diagnostics and TUI

Status: owner authorized Phase 3 on 20 October 2026. Independent review passed
the frozen local and synthetic scope. Live acceptance exposed a server verifier
gap because browser and device OAuth clients issue ID tokens with different
audiences; the owner authorized the minimal multi-audience server fix. Live
Google consent and native-keychain lifecycle checks passed on macOS. The
single-VM Compose HTTPS deployment and public OAuth-client discovery endpoint
are deployed on the approved pilot at `https://34-142-66-102.sslip.io`.
Operational acceptance and the subsequent full implementation audit passed.
Reviewer independently closed A1–A3 and verified the final full/race suites on
real PostgreSQL, vet and cross builds; see PHASE-3-REVIEW.md. Exact end-user
evidence and limits are in PHASE-3-LIVE-ACCEPTANCE.md; backup, restore and reboot
evidence is in deploy/README.md. The reviewed implementation is committed and
pushed as `f913ffa80443a47bf0faa62b55936a14802c570e` and deployed to the pilot.
Postdeployment evidence and the retained recovery source are recorded in the
runbook and reviewer report.

Implementation checkpoint: preserve the reviewed Phase 2 behavior. The minimum
Phase 3 coding slice makes browser and device OAuth the supported join paths,
removes the runtime dependency on the `gcloud` executable for normal users, and
keeps `gcloud` and ADC as explicit operator alternatives. Doctor, setup and TUI
work began as contracts/scaffolds; the minimum accepted slice includes the real
doctor route while create/join setup automation and the TUI remain scaffolds. The
live pilot now runs the approved Phase 3 deployment; retained Phase 2 data and
the previous checkout provide recovery options during acceptance.

For Phase 3 review, "join" means `clawbridge login` followed by an explicit
trusted cloud route. It does not claim that the optional `setup` package or TUI
has been wired into the CLI. `clawbridge doctor` is in the minimum slice because
it validates the same trusted route used by login, MCP and runtime requests.
Create/join automation and TUI delivery require a separate freeze statement and
their own acceptance evidence.

## Outcome

Make the existing local and hosted authorities usable from one operator interface
without weakening verified cloud identity, project scoping or offline safety.

Deliver:

1. `clawbridge doctor` checks trusted configuration, local store or cloud server,
   Google credential selection, project resolution, mirror freshness and installed
   MCP entries. It reports actionable failures and never prints tokens or secrets.
2. Create, join and local setup flows. Create mode produces a reviewable resource
   and cost plan, requires an exact typed confirmation, and only then invokes the
   configured GCP operations. Join mode authenticates against an existing service
   and writes trusted routing after successful authorization. Local mode remains
   unchanged.
3. A Bubble Tea TUI with Overview, Projects, Sessions, Inbox, Cloud and Doctor
   views. It reads through the same Executor boundary as CLI/MCP. Mutations require
   explicit user action and preserve existing scope and instance rules.
4. Pilot recovery hardening: a daily snapshot schedule attached to the VM disk,
   documented restore/rollback steps and an actual restore verification.

## Client authentication without a full gcloud installation

The hosted service already accepts a Google ID token for an explicit audience.
Phase 2 clients shell out to:

```text
gcloud auth print-identity-token
  --account HUMAN
  --audiences AUDIENCE
  --impersonate-service-account CLIENT_SA
  --include-email
```

That makes normal CLI and MCP use depend on the `gcloud` executable even though
the Go Google SDK can perform the token exchange. Phase 3 supports three explicit,
mutually exclusive data-plane credential backends:

```toml
# Trusted home or explicit CLAWBRIDGE_CONFIG only.
credential_source = "oauth"   # default after interactive login
# Optional explicit IDs override installed-app discovery.
# oauth_client_id = "DESKTOP_CLIENT_ID.apps.googleusercontent.com"
# oauth_device_client_id = "DEVICE_CLIENT_ID.apps.googleusercontent.com"
# oauth_client_secret and oauth_device_client_secret are the matching
# installed-app application credentials. Normally login writes them from
# trusted discovery; explicit trusted configuration can override them.
# oauth_keyring_service = "clawbridge-oauth" is optional.

# Existing pilot compatibility; an empty source still means gcloud.
# credential_source = "gcloud"

# Explicit operator alternative after a one-time ADC bootstrap:
# credential_source = "adc"
# credentials_file = "/absolute/path/to/application_default_credentials.json"
# account = "human@example.com"
# impersonate_service_account = "clawbridge-human@PROJECT.iam.gserviceaccount.com"
```

The OAuth path implements Google's installed-app loopback flow and limited-input
device flow:

- `clawbridge login` binds an ephemeral `127.0.0.1` callback, generates a new
  PKCE verifier with an `S256` challenge, uses cryptographically random state,
  opens the system browser and accepts exactly one matching loopback callback.
  It requests `openid email` with offline access and no broad Cloud scope.
- `clawbridge login --device` prints the verification URL and short code, then
  polls Google for approval. It is for SSH, remote desktop and other machines
  without a usable browser. Browser login uses a trusted explicit
  `oauth_client_id` when configured and otherwise discovers `browser_client_id`;
  device login similarly prefers an explicit `oauth_device_client_id` and
  otherwise discovers `device_client_id`. Each explicit ID uses its matching
  trusted installed-app credential; discovery supplies both IDs and matching
  credentials in one strict response.
- After device promotion, plain browser login treats an active client equal to
  the known device client as inapplicable for Desktop acquisition. It rediscovers
  the Desktop pair from the trusted server, refuses failed/invalid discovery or
  a browser ID equal to the known device ID, and never sends the promoted device
  credential into the browser flow. Successful browser login preserves the device
  pair; failure preserves the prior route and credential state. A distinct
  explicit Desktop pair continues to bypass discovery.
- Login validates the returned Google ID token before persistence, including
  signature, issuer, audience and a verified email. It stores only the refresh
  token and safe principal metadata in the native OS keychain. Access tokens,
  ID tokens and PKCE/device secrets remain in memory and are never handed to an
  agent.
- The CLI and MCP process load the keychain credential, refresh and mint a
  short-lived ID token on demand, and cache that ID token only in memory.
- `clawbridge login --use-gcloud` first tries the existing gcloud route when its
  explicit account, audience and impersonation values are complete. If gcloud
  is unavailable or cannot mint a token, login falls back to browser OAuth. A
  successful browser fallback writes `credential_source = "oauth"` to trusted
  configuration. A successful gcloud check leaves the gcloud route unchanged.
- `clawbridge logout` removes only the keychain entry identified by the
  configured OAuth client ID. It does not run `gcloud auth revoke`, delete an ADC
  file or alter an agent configuration.

Google's Desktop-app client supports browser loopback OAuth. Device
authorization requires an OAuth client whose application type is **TVs and
Limited Input devices**; it is a separate client from the Desktop-app browser
client. The trusted keys are `oauth_client_id` for Desktop/app browser and
`oauth_device_client_id` for the device client. Both are optional: an explicit
trusted value wins and skips discovery, while an absent applicable value makes
login query the trusted server's `/v1/config` discovery endpoint. A deployment
that wants both flows must register both separate Google client types. An empty
discovered applicable ID or credential fails closed rather than sending a
Desktop-app client to the device endpoint.

Because those Google clients issue ID tokens with different `aud` values,
`clawbridge serve --audience` accepts a comma-separated ordered set as well as
the original single value. The set contains exact client IDs or a custom exact
audience accepted by the same production validator. Surrounding whitespace is
trimmed, while interior whitespace/control characters, empty entries, duplicate
entries, more than 16 entries, and entries longer than 2048 bytes are rejected
before Postgres initialization. Every token still passes Google's official
signature, issuer, expiry and claim validation with its exact configured
audience; an unlisted audience fails authentication. Cloud client route
`audience` remains one string because it names the single audience minted into
the request token.

The single-VM deployment uses Docker Compose with Caddy terminating HTTPS in
front of the API. Caddy is the only service with host ports, publishing TCP 443;
the API and Postgres remain on an internal-only network. Caddy also joins a
separate non-internal egress network for ACME, OCSP and Google reachability; the
API joins that egress network for Google signing-certificate fetches.
TLS-ALPN validation over TCP 443 supports an ingress policy that does not expose
port 80. The Compose API command passes both optional OAuth client IDs through
the `serve` flags below. The corresponding installed-app credentials are passed
only through `CLAWBRIDGE_OAUTH_CLIENT_SECRET` and
`CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET` in the API environment; they are never
server argv. Caddy's certificate and configuration state and Postgres data use
persistent volumes; the Postgres source can be replaced with a separately
mounted disk without editing the Compose source.

`serve` gains:

```text
--oauth-client-id DESKTOP_CLIENT_ID
--oauth-device-client-id DEVICE_CLIENT_ID
```

Each supplied ID must exactly appear in the audience set and the two must be
distinct. A configured ID requires its matching environment credential; omitting
both ID/credential pairs retains previous behavior. The public unauthenticated
`GET /v1/config` endpoint returns exactly the four-field installed-app bundle
`{"browser_client_id":"...","browser_client_secret":"...","device_client_id":"...","device_client_secret":"..."}`,
with empty strings for omitted pairs. These native-client credentials are safe
for distribution once the application is public but remain redacted from logs,
errors, server argv, MCP/installer argv and doctor output. The endpoint exposes
no token state, principal, scope, storage address, unrelated credential route or
health detail. This is installed-app OAuth discovery only; ClawBridge
session-token exchange remains an owner decision and is not implemented.

There is no implicit fallback between data-plane backends and no request to the
Compute Engine metadata server. Repository config may set only
db/workspace/project/session/instance; repository mode, server, audience,
principal, credentials, and cache are always ignored. In particular, a checked-out
repository cannot move a trusted local configuration into cloud mode or move a
trusted cloud configuration into local mode. The trusted explicit/home mode
remains effective. This prohibition is intentional and frozen for Phase 3;
repository routing fields are ignored in both directions rather than conditionally
overlaid. A repository-only `mode` neither creates a cloud route nor causes
trusted cloud validation to run. A trusted cloud route must still provide its own
server, audience and credentials; a repository `mode` cannot supply or remove
them.
Flags and environment variables are useful for isolated tests and explicit
operator overrides; the trusted configuration remains the durable source. New
cloud setup and installation flows must pin an explicit source. Empty source is
accepted only for backwards compatibility with the existing pilot.

OAuth, keychain, refresh, token-validation and token-mint failures are credential
failures. They fail closed before transport and must not fall through to gcloud,
ADC, another account, another project or the offline mirror. Only a transport or
service outage may permit a scoped read-only mirror read under the Phase 2 rules.
Writes, mark-read, resume, 401 and 403 never fall back or queue.

Safe agent routing may include the backend name, audience, expected principal,
server and cache path, plus the OAuth client ID and optional keyring service. It
must never include a refresh token, access token, ID token, PKCE verifier,
authorization code, either installed-app credential, ADC file contents or
service-account key. Installed MCP entries must resolve the same trusted scope
and credential settings as the CLI and doctor.

The ADC backend accepts only an explicitly selected `authorized_user` file whose
refresh credential belongs to the selected initiating human. It uses Google's
impersonation SDK to mint a short-lived, audience-bound ID token for the required
distinct service account, with the email claim present. Minted tokens stay in
memory. ADC read, refresh and token-mint failures are credential failures: they
must not fall back to an offline mirror, gcloud, another account or another
project. Cached mirror reads remain limited to transport/service outages as in
Phase 2.

ADC is a credential format, not a password. Operators can obtain it in three ways:

1. Run `gcloud auth application-default login` once, then stop using `gcloud` for
   ClawBridge messaging. The runtime reads the resulting JSON directly.
2. Import an authorized-user ADC file through a trusted, access-restricted path
   from a machine where login already occurred.
3. Use browser or device OAuth with the keychain-backed runtime instead. ADC is
   not needed for the normal join path.

Cloud resource provisioning is separate from data-plane authentication.
`gcloud`, a Terraform provider or direct Cloud APIs can be used as an explicit
operator backend. Removing the runtime gcloud dependency does not claim that a
new user can create GCP resources without provisioning credentials.

## Per-user service accounts and expiry

Creating a dedicated identity on first initialization is an optional team
deployment capability. The normal OAuth client can carry the verified human
directly to the hosted service. A controlled service account can still be useful
for impersonation and workload attribution. Audit records identify the principal
verified from the request token: a human for direct OAuth, or the service account
for impersonation. A local account setting is not independent proof of the
initiating human.

This Phase 3 slice does not automatically provision per-user service accounts.
The rules in this section are requirements for a later setup/deployment
increment, whose plan and acceptance must explicitly cover them. The existing
single-VM template and focused `setup` scaffold must not be described as having
shipped collision cleanup, IAM expiry or per-user identity provisioning.

Use one service account per human, for example:

```text
clawbridge-<normalized-human-id>@PROJECT.iam.gserviceaccount.com
```

The initiating human receives `roles/iam.serviceAccountTokenCreator` on that
identity. The client can mint one-hour ID tokens from a short-lived access
credential and impersonate the service account. Recording a separately verified
initiating human would require an additional authenticated delegation design;
the current service-account token alone does not establish that identity.
Do not use service-account key files in the default path; they are long-lived
bearer secrets.

Google service accounts do not have a built-in expiry date. Implement time-bound
access with an IAM Condition on the initiating human's impersonation grant, for
example an `expires=<RFC3339 timestamp>` condition. The service account resource
may remain after access expires so audit history and owned data stay attributable.
Disable or delete it only through a separately confirmed administration flow.
Renewal, extension and revocation are explicit owner actions, show the identity
and new expiry, and are audited.

Identity creation must be idempotent and fail closed:

- Derive a deterministic, normalized service-account ID from the verified human
  principal; reject collisions instead of adopting an unrelated account.
- Before creation, describe/list the deterministic ID and verify its project,
  display metadata and expected ownership. A name that already exists with
  different metadata or bindings stops the plan; it is never silently reused.
- If the intended account exists after an uncertain response, re-read and verify
  it before treating creation as successful.
- The final plan shows project, service account, initiating account, audience,
  roles, IAM condition and expiry.
- Expired, disabled or ambiguous identities fail authorization. The client does
  not switch to a default account, shared service account or offline write path.

Bootstrap ownership remains an explicit trusted operation. A user who cannot
create or manage the identity must join through an existing administrator and
report the precise missing IAM binding.

Automatic provisioning must fail closed. It cannot use a default gcloud project,
create resources when cost confirmation is absent, or change existing resources
without showing the exact plan. The existing manual pilot remains the reference
deployment.

## Initial implementation ownership

- cb-engineer: `internal/doctor/**`, `internal/setup/**` and their focused tests;
  publish exact exported APIs before implementation.
- cb-designer: `internal/tui/**` and focused tests; publish exact exported APIs
  before implementation. Do not edit CLI or core files.
- cb-tester-reviewer: independent integration tests, `integration/**` and
  `PHASE-3-REVIEW.md`; no implementation edits.
- root: `internal/cli/**`, command dispatch, package integration, live GCP backup
  and restore verification, deployment documentation and final checks.

For resumed acceptance, root focuses on design, coordination and interactive
OAuth evidence; cb-engineer owns engineering and operational recovery checks;
cb-tester-reviewer owns independent verification and the full audit after root's
explicit stage-completion checkpoint. See COORDINATION.md for current gates.

## Contracts

- `setup` planning is deterministic and testable with an injected command runner.
  Apply requires both `--apply` and the exact confirmation phrase derived from the
  displayed plan. Join is online-only and never initializes or adopts a workspace.
- `doctor` returns stable machine-readable statuses (`pass`, `warn`, `fail`) and
  exits nonzero only for failed required checks.
- The TUI never opens a writable local store in cloud mode and never performs an
  offline write. Cloud setup confirmation is shown verbatim before apply.
- Existing Phase 1/2 behavior and exact CORE-API contracts remain valid.

## Verification

- Unit and integration coverage for create plan/confirmation refusal, join
  authorization, diagnostic redaction, local/cloud TUI model behavior and MCP
  installation checks.
- Real disposable Postgres and existing live VM checks retained.
- Full `go test ./...`, race, vet and CGO-disabled darwin/arm64, linux/amd64 and
  windows/amd64 builds.
- Create a snapshot, inspect its status, restore into a separate disposable disk
  and verify the ClawBridge exchange row. Remove the disposable restore instance
  and disk; retain the production snapshot schedule.

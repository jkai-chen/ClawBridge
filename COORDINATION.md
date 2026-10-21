# ClawBridge active coordination

## Current phase: Phase 3 Compose/OAuth-discovery surface implemented; review and live checks pending

The owner resumed the work on 20 October 2026 and directed the existing
cb-engineer and cb-tester-reviewer coworkers to continue. Root owns the Phase 3
plan and contracts; cb-engineer implements and tests owned source; the reviewer
owns independent integration coverage, defect reports and final heavy checks.

The tree is dirty and uncommitted. Do not deploy, stage, commit, alter real agent
configuration or create billed cloud resources from this checkpoint.

Frozen Phase 3 contract decisions:

- Browser OAuth is the normal join path; `--device` supports headless machines.
- `oauth_client_id` identifies the Desktop-app browser client.
  `oauth_device_client_id` identifies the separate TVs and Limited Input devices
  client. Only `openid email` is requested.
- The OS keychain stores only refresh credential and safe identity metadata.
  Access and ID tokens stay in memory. CLI/MCP refresh internally; agents never
  receive tokens.
- `oauth`, `gcloud` and `adc` are exclusive credential backends. An empty source
  is legacy gcloud compatibility for the pilot. New flows pin a source. Credential
  failures never use the mirror; only transport/service outages may serve scoped
  read-only mirror reads.
- Trusted configuration owns authority mode and routing. Repository
  `.clawbridge.toml` may select database/workspace/project/session/instance scope
  only; it cannot select or switch `mode`, credentials, server, audience,
  principal or cache.
- Successful device login keys the refresh credential under the device client ID
  and promotes that client ID to active `oauth_client_id`. The trusted config
  update is atomic; a post-save write failure must clean up the new credential
  and preserve fail-closed prior configuration.
- Optional per-human service accounts use deterministic collision-checked names.
  Access expires through an IAM Condition on the human's Token Creator grant;
  the human principal remains in every audit record. Per-user provisioning,
  collision handling and IAM expiry automation are future setup/deployment work,
  not delivered by the current Phase 3 authentication slice.

Repository-mode ambiguity is closed by owner decision: Phase 3 supersedes the
older Phase 2 repository `mode` overlay allowance. Both peers have the updated
probe or implementation requirement. No implementation defect is currently
promoted. The rule is intentional and frozen: repository routing fields,
including `mode`, are ignored in both directions; only database/workspace/project/
session/instance scope may come from repository configuration. A
repository-only `mode` cannot create a cloud route or trigger its validation; a
trusted cloud route still requires trusted server, audience and credentials.
The engineer declared the Phase 3 acceptance tree frozen. The reviewer completed
full uncached tests, race, vet, cross builds and disposable PostgreSQL lifecycle
against that unchanged implementation tree. `PHASE-3-REVIEW.md` records Pass for
the local and synthetic scope. Live Google consent, real OS-keychain behavior at
the binary boundary, Caddy certificate issuance and deployed behavior remain
explicitly pending.

Live acceptance then exposed one concrete server contract gap: browser and
device OAuth use different Google clients, so their ID tokens carry different
audiences while `serve` accepted only one. The owner authorized a backward-
compatible fix. `serve --audience` now accepts a comma-separated ordered set of
exact audiences, rejects empty/duplicate/malformed entries before opening
Postgres, and still uses Google's official validator for each exact configured
audience. Single-audience invocation and the `NewGoogleVerifier` API remain
compatible; `NewGoogleVerifierForAudiences` is the set-based server API. Cloud
client routing continues to use one `audience` string.

The owner fixed the deployment direction to Docker Compose on the existing
Compute Engine VM. `deploy/compose.yaml` now publishes only Caddy on TCP 443 and
keeps the API and Postgres on an internal-only backend network. Caddy joins a
separate non-internal egress network for ACME/OCSP/Google access and retains
certificate state in named volumes; the API also uses that egress network for
Google signing certificates. The operator can bind Postgres to a
separately mounted persistent disk through `CLAWBRIDGE_POSTGRES_DATA_PATH`. The
live data path is `/var/lib/clawbridge-postgres/data`, leaving the ext4 mount
root's `lost+found` outside database storage. Backup tooling uses the standard
`COMPOSE_PROJECT_NAME=clawbridge-phase3`.

The API now supports optional `--oauth-client-id` and
`--oauth-device-client-id` flags. Each must exactly match an audience and the
pair must be distinct; omission preserves prior behavior. A configured ID
requires its matching installed-app credential from
`CLAWBRIDGE_OAUTH_CLIENT_SECRET` or
`CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET`; credentials are passed only through the
private environment, never server argv. Google treats distributed native-client
credentials as application identifiers, while ClawBridge still never logs,
prints or places them in agent configuration. Unauthenticated `GET /v1/config`
returns the strict four-field bundle
`{"browser_client_id":"...","browser_client_secret":"...","device_client_id":"...","device_client_secret":"..."}`.
It exposes no token, principal, scope, storage, unrelated credential route or
health data. This is not a ClawBridge session-token exchange; that remains an
owner decision.

The follow-up login discovery contract is implemented in the shared client
slice. Browser login and `login --device` use an explicitly configured applicable
trusted ID and skip discovery when present. Otherwise they query exactly
`GET <trusted-server>/v1/config`, with HTTPS except numeric loopback, redirects
refused, bounded timeout/body and strict JSON containing exactly
`browser_client_id`, `browser_client_secret`, `device_client_id` and
`device_client_secret`, all strings, with unknown, duplicate or trailing fields
rejected. Discovery failures stop before OAuth acquisition and never trigger
mirror fallback. Browser login promotes the browser ID, credential and audience
while preserving an explicit device pair. Device login retains the selected
device ID and credential, promotes the same credential to active
`oauth_client_id`/`oauth_client_secret`, sets the matching audience, and remains
the key removed by idempotent logout. Config-write failure rolls back new or
replaced keychain state and leaves prior config bytes unchanged. Normal cloud
commands still refuse an incomplete route. `clawbridge login --help` returns
normal help.

Minimum Phase 3 delivery is OAuth login/logout, keychain-backed runtime minting,
explicit `oauth`/`gcloud`/`adc` routing, safe MCP installation and the `doctor`
route check. The optional `setup` create/join automation and TUI remain scaffolds
unless the engineer explicitly includes them in the freeze and the reviewer
accepts their contracts. A successful `login` plus trusted route satisfies the
Phase 3 join outcome; no half-wired `setup --cloud` path is claimed.

After repository creation/push, the owner said to go ahead. Phase 2 is now active;
see PHASE-2-PLAN.md for exact shared APIs and ownership. Engineer owns PG/bus/CLI
and dependencies; designer owns server/remote/mirror and adapters; reviewer owns
real temporary Postgres and PHASE-2-REVIEW.md plus final heavy checks. Preserve all
Phase 1 behavior. No resources/IAM grants or paid deployment, real config changes,
worker commits/pushes, or extra agents. Repository baseline is main at 4c66800.

Required new dependencies (engineer owns go.mod/sum): pgx/v5 and Google idtoken.
Server reads CLAWBRIDGE_DATABASE_URL by default, with --database-url-env override,
--listen (loopback default), --audience and --bootstrap-owner required. Auth never
trusts an email header or disables verification for production. Remote Config and
snapshot shape are fixed in PHASE-2-PLAN.md; publish any change before adapters/tests.

The owner approved Go and the refined local Phase 1 plan, then explicitly asked
us to continue and cooperate. No further phase/stack confirmation is needed for
this work. No billable resources, publication, or real agent configuration changes
are part of this implementation turn.

## Ownership

- cb-engineer: go.mod/go.sum, cmd/clawbridge, internal/bus, internal/cli,
  CORE-API.md, QUESTIONS.md. Owns fixes in these implementation files.
- cb-designer: DESIGN.md copy, PHASE-1-PLAN.md, COORDINATION.md, README.md,
  internal/mcpserver, internal/install, skills/clawbridge, docs.
- cb-tester-reviewer: integration/**, REVIEW.md. Owns full test/race/vet and
  cross-build verification after implementation is ready. Reports implementation
  failures with exact repros; owners fix them.

We share one checkout. During implementation, staging and committing were reserved
for the designer after the owner's repository/push instruction; preserve peer files.
Use natural checkpoints to read CORE-API.md, QUESTIONS.md, REVIEW.md, and this file.
The designer reads reports and coordinates fixes throughout implementation.

## Shared adapter contract

Module is `clawbridge`. Core API and arguments are in CORE-API.md (already present).
MCP adapter: `mcpserver.Run(ctx, dbPath, workspace, project, session string,
instance ...string) error`. Variadic instance supports both the original call and
CLI `--instance`; engineer should pass the resolved instance as the final argument.
Installer: `install.Run(args []string) error`. Installation requires --agent,
--workspace, --project, --session and accepts --db, --config, --command, --name,
--instance, --dry-run. It builds an explicitly pinned local MCP entry. Configuration
writes happen only when the installer is invoked, never during normal build/tests.

Designer needs the official `github.com/modelcontextprotocol/go-sdk` and
`github.com/pelletier/go-toml/v2` dependencies; engineer owns dependency updates.
MCP tools expose normal named arguments, with scope/instance held by the process.
Successful registration binds its returned instance. Calls cannot supply another
scope or actor. Explicit installer names allow multiple configured agent sessions.

## Final local implementation checkpoint

- Design source copied into this branch; refined Phase 1 plan and exact core API shipped.
- Engineer implemented the SQLite authority, CLI, project resolution, session
  lifecycle/resume fencing, pagination, transactional messages and audit history.
- Designer implemented stdio MCP, safe config installers, README and the packaged
  agent skill. Skill validation and the README workflow smoke pass.
- Reviewer completed final uncached full tests, race tests, vet, and CGO-disabled
  builds for darwin/arm64, linux/amd64 and windows/amd64. All pass. R1 (database
  file lifecycle) and R2 (literal message dispatch) are fixed and regression tested.
- `bin/clawbridge` is built locally and ignored by Git. No full verification
  processes remain running. The owner subsequently authorized committing the
  implementation and pushing a personal repository, selecting `jkai-chen` as owner.
- Installed Codex actual MCP tool exchange passed; installed Claude connection and
  11-tool discovery passed. Both used temporary isolated configs and no model
  requests. Full live Claude tool execution and model-driven two-agent workflows
  remain untested; precise limits are in REVIEW.md and docs/CLIENT-CHECKS.md.

Current ownership remains with the existing coworkers; no additional agents were
started for implementation. Installer code writes real settings only when invoked
explicitly. No hosted resources, paid deployment, licence choice, or publication
was made. The completed hosted transport and identity work is summarized below.

## Deployment steering

Owner suggested backend and Postgres on one Compute Engine VM to reduce baseline
cost. This is a candidate for the hosted phase, not permission to create a VM.
Keep the server portable. Investigate IAP tunnel plus application identity, or
HTTPS IAP with a load balancer. IAP TCP access alone does not establish per-request
application identity. Details go in docs/DEPLOYMENT-OPTIONS.md. This does not block
the local Phase 1 implementation.

## Phase 2 completion

Hosted Postgres, verified Google identity, CLI/MCP remote execution and explicit
read-only mirror sync are implemented. The reviewer reproduced and retested six
defects, then completed uncached real-PostgreSQL full and race suites, vet, and
CGO-disabled Darwin/arm64, Linux/amd64 and Windows/amd64 builds successfully.
Exact evidence and limits are in PHASE-2-REVIEW.md. No cloud resource, IAM grant,
deployment or real agent configuration change was made. The single-VM Compose
layout remains a reviewable template pending explicit owner choices and approval
for billed resources.

## Phase 3 resumed operational acceptance and full review

Owner resumed work on 20 October 2026 after the explicit pause. Existing
`cb-engineer` and `cb-tester-reviewer` sessions retain their ownership; no new
workers are needed. The frozen code is deployed at
`https://34-142-66-102.sslip.io` on the approved Compute Engine pilot. The final
section of PHASE-3-REVIEW.md records automated and noninteractive scoped passes.

- Engineer owns installing/exercising the existing backup timer, dump/checksum
  restore drill, mount guard, planned reboot and deployment evidence/docs.
- Designer owns the isolated browser/device consent and keychain lifecycle
  acceptance, design consistency, and the stage-completion decision.
- Reviewer verifies the current acceptance evidence and owns integration tests
  and PHASE-3-REVIEW.md. After the designer explicitly marks this stage complete,
  the reviewer performs a full implementation audit covering security,
  correctness, compatibility and failure recovery across the shipped code.
- The requested review time is after about 18:00 Europe/Dublin. At resumption,
  that time has already passed on 20 October; the remaining gate is stage
  completion. Do not describe a future scheduled automation as installed.

Interactive Google consent and real keychain lifecycle, operational backup and
reboot evidence remain pending until actually exercised. Preserve all dirty
peer changes. No staging, commit or push occurs before final reconciliation and
review; retain existing recovery data and do not create new billed resources or
IAM grants under this operational checklist.

### Stage-complete checkpoint

Root declared **STAGE COMPLETE** at approximately 21:57 UTC on 20 October.
Actual browser/device Google consent, native macOS keychain persistence,
fresh-process refresh, whoami, doctor, write/inbox/sync and repeated logout passed;
post-logout credential failures did not return a cached result. Sanitized evidence
and platform limits are in PHASE-3-LIVE-ACCEPTANCE.md. Engineer completed backup
upload/checksum, isolated restore, mounted reboot recovery and missing-mount
predicate checks; deploy/README.md records exact evidence and remaining limits.

The full implementation audit is authorized now. Reviewer owns manual review and
targeted independent probes, engineer owns fixes to reproduced findings, and root
owns design/contract reconciliation. Unattended timer firing and physical disk
loss remain explicitly unclaimed future operational observations. No final audit
pass, staging, commit or push is implied by stage completion.

### Full-audit corrections

Reviewer independently closed three reproduced findings: A1 doctor ignored
installed MCP environment credentials/route overrides; A2 the setup scaffold
adapter duplicated the executable token; A3 the TUI scaffold emitted peer
terminal controls. Engineer supplied all implementation fixes, and reviewer
retained independent regressions, including broader dynamic TUI surfaces.
Setup/TUI remain scaffolds; these fixes do not add CLI wiring.

The browser-after-device contract is now explicit in CORE-API.md: a promoted
known device client is not an applicable Desktop client for plain login, which
rediscovers the Desktop pair and preserves the previous device route on failure.
Independent synthetic transition probes passed; the earlier separate-config
live Google runs do not establish a live transition. Reviewer completed the
manual audit with no additional promoted finding. Final uncached full and race
suites passed with real disposable PostgreSQL 17.11, as did vet and CGO-disabled
darwin/arm64, linux/amd64 and windows/amd64 builds. Both fixtures and anonymous
volumes were confirmed removed. PHASE-3-REVIEW.md owns the final evidence and
limits. This audit checkpoint preceded the publication and deployment below.

### Phase 3 publication and pilot deployment

The owner authorized publication and deployment after the independent audit.
Reviewer inspected the publication file set without finding apparent live
credentials or private artifacts; all four recorded implementation fingerprints
matched. Root committed and pushed the reviewed Phase 3 implementation as
`f913ffa80443a47bf0faa62b55936a14802c570e` to `jkai-chen/ClawBridge` main.

Engineer deployed an archive of that exact commit to the approved Compute Engine
pilot, retaining the previous source directory and preserving private environment,
database disk, Caddy volumes and Phase 2 recovery containers. HTTPS health,
four-field discovery, unauthenticated execute rejection, authenticated whoami and
required doctor checks passed. The deployment runbook and reviewer report own
exact artifact, backup and independent postdeployment evidence. Prior live Google
consent evidence remains separate from the synthetic same-config transition
tests; no new consent or broader platform claims are implied by this deployment.

### Boot-disk maintenance and session limits

The owner authorized boot-disk cleanup after deployment. Engineer owns the
inventory and removal of disposable caches and temporary transfer artifacts;
reviewer independently checks targets and retained resources, then service and
storage health. Preserve containers, database/Caddy volumes, current and rollback
images, recovery sources, private environment and backup artifacts. Root owns
documentation reconciliation and publication of the resulting evidence.

The owner explicitly directs all three sessions to wait for the reset and
continue if a ChatGPT usage limit is reached. Preserve progress and ownership
across the wait; a usage-limit interruption does not cancel the authorized task.

Engineer inventoried cleanup targets and reviewer independently approved them.
An Orca queued-question state blocked follow-up prompts to engineer; root
therefore performed only the agreed cache/package/exact-transfer-archive cleanup.
Boot-disk usage fell from 97% (329 MB free) to 38% (5.7 GB free), while all five
containers, seven images and four volumes remained. No restart or disk expansion
was needed. The deployment runbook records commands and retained-resource checks;
the reviewer report records independent post-cleanup verification.

### Phase 4: onboarding and operator TUI

Owner authorized the next increment on 21 October 2026. Baseline `e8db318` is
pushed; prior deployment/cleanup and first unattended backup evidence are closed.
Root published PHASE-4-PLAN.md and the exact Phase 4 section in CORE-API.md.
Scope: transactional SDK-free `setup join`, explicit trusted `--client-config`,
MCP installation inheriting/pinning that route, explicit MCP doctor selection,
and a `tui` entry for the existing six-screen operator model. Existing local
creation and hosted bootstrap initialization remain explicit commands. The old
infrastructure provisioner stays disconnected; no new cloud resources, pilot
deployment or real-user config/consent operations are part of implementation.

Root owns plan/contract/README and coordination; cb-engineer owns all production
implementation and focused tests; cb-tester-reviewer owns integration probes,
PHASE-4-REVIEW.md and heavy checks after exact engineer freeze. Fresh no-gcloud
live onboarding and a second human user remain separate manual acceptance gates.

Implementation and automated independent acceptance passed. Reviewer retested
empty immutable-ID rejection, diagnostic redaction, actorless TUI guards and
complete OAuth-pair preflight; transaction tests cover rollback failures and
uncanceled cleanup. CLI preflight, pinned installer routing and a fresh-process
MCP/selected doctor exchange use synthetic credentials. Full uncached and race
suites passed with disposable PostgreSQL 17.11, alongside vet and three CGO0
cross-builds. Both exact containers/volumes are absent and all 73 frozen Go/module
fingerprints remain unchanged. PHASE-4-REVIEW.md owns exact results. Root handles
documentation/publication only; no Phase 4 pilot deployment occurred. The manual
checklist is in PHASE-4-PLAN.md; live consent, platform keychains, a physically
SDK-free machine and second-human acceptance remain Pending.

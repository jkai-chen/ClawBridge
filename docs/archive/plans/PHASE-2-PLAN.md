# Phase 2: hosted service, clients and read-only mirror

Status: owner authorized continuing after the Phase 1 repository push. Implement
and verify locally; no cloud resources or IAM grants are authorized by this plan.

## Deliverable

Run the same messaging application against Postgres behind an authenticated HTTP
service. CLI/MCP can use that service, explicitly sync a scoped SQLite mirror,
and read the mirror during transport outages. Preserve the complete local mode.
Target deployment is portable to a single Compute Engine VM with private Postgres;
no deployment is performed during this implementation.

## Ownership and contracts

- cb-engineer: shared bus Postgres adapter, schema and role checks, CLI wiring,
  dependencies, CORE-API.md and QUESTIONS.md.
- cb-designer: internal/server, internal/remote, internal/mirror, MCP/install
  adapters, docs and this plan.
- cb-tester-reviewer: integration tests, PHASE-2-REVIEW.md, temporary Postgres
  Docker lifecycle and full/race/vet/cross-build verification.

Shared APIs:

```go
bus.OpenPostgres(ctx context.Context, dsn, bootstrapOwner string) (*bus.Store,error)
(*bus.Store).ExecuteAs(ctx context.Context, principal string, req bus.Request) (bus.Result,error)
server.Run(ctx context.Context, args []string) error
server.NewHandler(store server.Executor, verifier server.Verifier) http.Handler
// Executor.ExecuteAs(ctx, verifiedPrincipal, request); no caller principal field.
// Verifier.Verify(ctx, bearerToken) (server.Identity,error)
// Identity {Principal, Subject string}
remote.New(remote.Config) (*remote.Client,error)
// Config string fields: URL, Audience, Account, ImpersonateServiceAccount,
// Principal, CachePath. Explicit Principal binds mirror identity, never authenticates.
(*remote.Client).Execute(ctx context.Context, req bus.Request) (bus.Result,error)
(*remote.Client).ResolveProject(ctx context.Context,w,p,gitRemote string)(string,string,error)
(*remote.Client).Sync(ctx context.Context,w,p,session,instance string)(bus.Result,error)
mcpserver.RunExecutor(ctx context.Context, executor mcpserver.Executor,w,p,session,instance string) error
```

HTTP protocol: authenticated POST /v1/execute with the existing Request/Result
JSON. Unknown fields, malformed bodies and unsupported methods fail. Errors are
`{"error":{"code":"...","message":"..."}}` without tokens/DSNs or raw SQL.
Known service errors may implement `Code() string` for forbidden/not_found/conflict/
invalid/rate_limited/unavailable; unknown errors receive a generic rejection.
Production Run always creates a Google verifier; there is no bypass flag.

## Identity and permissions

Require cryptographically verified Google-issued ID tokens with the configured
nonempty audience, expected issuer, expiry, subject and verified email. Derive
the principal from that verified email, never a client header/body. Record the
token subject where available in identity results. Test route policies with
injected verifiers but label that evidence as synthetic, not live Google auth.

The gcloud client requests an audience-bound token. If using impersonation it
passes the explicitly selected service-account email and includes email in the
token. The verified/audited principal is that service account, not the initiating
human; distinct humans need distinct controlled identities to retain attribution.
Generic user `gcloud` development tokens are not a production login substitute.
IAM identity choices and grants remain deployment decisions, not automatic setup.

Bootstrap a new workspace only for an explicitly configured bootstrap owner.
New projects grant their creator owner. Project roles owner/lead/member/reader
are independent of session routing roles. Every request including discovery,
history, resolution and snapshots enforces membership. Session instance/owner
checks remain. Only lead/owner may explicitly target a peer session for changes.
Project owners manage role grants through a server-authorized operation.
Rate-limit verified principals and sending sessions. Keep audit and mutations in
one transaction. Independent concurrent callers cannot share mutable identity.

## Snapshot and mirror

The `snapshot` operation returns one consistent authorized view:

```text
workspace, project, session, principal, verified:true,
identity_provenance:google-id-token, storage_mode:cloud, snapshot_at,
sessions:[routing rows], messages:[only this session's unexpired inbox],
sent_messages:[this session's sent status metadata], events:[project history]
```

It must not stamp messages delivered/read or export another user's inbox/body.
More than 10000 rows in any dataset fails explicitly rather than truncating.
`resolve_project` returns immutable workspace_id/project_id after authorization.
Local snapshot publication is atomic, bound to endpoint, audience, principal,
project, session and instance; a failed sync keeps the previous complete snapshot.
SQLite mirror reads open in read-only mode. Only sync may replace cached data.

Offline operations: list_sessions, inbox, history, whoami and message_status for
the bound identity, with filtering/pagination and mirror freshness. No delivery
or read stamps change. Writes including mark_read/resume fail loudly and are
never queued. Definitive 401/403 and all other rejected requests never fall back
to cache. Only an unavailable transport/service permits read fallback.
No mirror data becomes the writable local authority during an outage.

## Client and CLI

Keep endpoint and credential routing in trusted home/explicit config, flags or
environment. Repository config may select scope and mode but cannot redirect
credentials to an arbitrary server or choose a token audience/account. HTTPS is
required except loopback HTTP for local testing/IAP-forwarded connections.
Do not follow authenticated redirects. Apply finite network/request/body limits.

Add serve, explicit cloud config/flags and sync. Wire CLI and MCP to the same
remote Executor, with process scope pinned. Installers can emit explicitly cloud
entries without changing existing user config during tests.

## Verification and deployment gate

Use a real disposable Postgres container, remove it and its volume afterwards,
and retain all Phase 1 checks. Exercise separate principals/roles, foreign IDs,
concurrent retry dedup, rollback/audit, snapshots, HTTP round trips, sync/outage
reads, forbidden writes, no fallback on authorization denial, and provenance.
Test expiry/retention and schema version rejection. Run final tests/race/vet and
cross builds. Record actual versus simulated Google-auth evidence distinctly.

Deployment still needs the owner's selected GCP project/region, concrete cost
estimate including disks/backups/networking, identity/IAM setup and explicit
confirmation before creating billed resources. Do not infer a master or region.

# Core decisions / integration notes

cb-engineer: using a generated immutable session ID and separate registration `instance` (caller provided or generated). Actor operations require the matching instance; please carry CLI `--instance` / MCP tool argument through. This avoids silently adopting a live/retired session name. Local identity remains unverified OS-user attribution, not a security boundary against that user.

No administrative session actions are exposed: updates/retire are own-session only; role routing cannot grant authority. If a wider local admin workflow is desired, it should be separately designed rather than inferred from `role=lead`.

R1 update: reproduced exact reviewer CLI/MCP failure, fixed redundant resolver/MCP handle overlap and raw open/close of existing SQLite file. Exact reproduction now passes. Please retest and retain this regression. No dependency version change was needed.

MCP list/history instance mismatch is also resolved: common instance accepted by those read operations.

Resume/presence contract ready: register_session accepts resume=true plus prior instance; rotates instance, keeps session ID, fences old instance, audits resumed. CLI register --resume wired. Designer: please add optional Resume bool to MCP register input. Core now derives presence/stale on session outputs (15-minute threshold) and refreshes last_seen on all successful actor uses. Details in CORE-API.md.

Engineer unit suites now pass: `go test ./internal/bus ./internal/cli`. Core changes for resume/presence are complete and ready for independent lifecycle regression checks. Full checks remain reviewer's ownership. CORE-API.md contains exact final API, paging, resume and presence behavior.

R2 fixed in CLI dispatch: globals are parsed first and only rest[0:2] == mcp/install invokes installer. Message words no longer affect dispatch. Installer still receives all explicit scope/DB/instance flags; global --json is consumed consistently. Added focused unit regression for literal recipient mcp/body install. Reviewer may retest exact reproduction and refresh owned checks.

Final engineer checkpoint: final CORE-API.md consolidated; all owned source is gofmt-clean. Reviewer reports full tests, race, vet and three CGO-disabled cross builds PASS after R1/R2 + resume/presence. No open core decision or defect remains. Implementation complete in owned files; no staging/commits, paid resources, or real agent config changes.

Phase2 checkpoint: shared bus PG API compiles and local unit suite passes. Reviewer may run real PG focus now. Core implements mode=server bootstrap, workspace/project membership and role grants, scoped resolve_project, readonly repeatable-read snapshot with 10000 honest cap, role-only peer target, principal/session rate limits, retention purge tombstones. Exact contract appended CORE-API.md. Dependencies current compatible pgx v5.11.0 / Google API v0.305.0 landed; require Go >=1.26 (installed1.27). CLI cloud wiring next; wrappers remain designer ownership.

CLI cloud integration now written in internal/cli: direct serve dispatch, credential-routing flags/env with repo exclusion, remote Execute/ResolveProject/Sync and MCP RunExecutor, setup --cloud, project role-set, purge-expired and peer --target-session. Waiting only for designer wrapper package files to build CLI; core checks continue independently. Bus hosted errors now expose Code() so server can distinguish forbidden/conflict/rate-limit/unavailable without returning SQL text.

P2-R2 fix: ExecuteAs now wraps policy errors with Code(): permission/membership/ownership/instance denials forbidden, retry/identity conflicts conflict, rate-limits rate_limited, SQL/transient failures unavailable. Existing server serviceError already consumes Code(). Reviewer may retest HTTP403 regression. Added bus PG role-denial test using separate principals; runs only with reviewer disposable DSN and verifies no success audit change plus reader snapshot provenance. Shared dialect and trusted/local identity guard unit tests pass locally.

P2-R3 fix ready: PG audit writes and exported history details now replace instance/prior_instance tokens with deterministic SHA256 fingerprints recursively. Legacy schema-1 audit rows are sanitized when read as well, covering history and snapshot; actual caller/principal/session audit identity and rotation distinction retained. Local audit behavior unchanged. Reviewer may rerun snapshot privacy exact regression.

Core/CLI focused suites pass with wrappers landed. Added CLI unit checks for repo credential-routing injection refusal, serve bypass of malformed config/no local DB, and remote whoami+explicit snapshot sync using loopback/synthetic gcloud fixture only (no real credentials/network). Snapshot now bulk-collects sent status deliveries at one expiration time and rejects excessive row/byte budgets before text materialization. Please include latest bus PG unit test in reviewer env run as well as integration. No current core blocker.

Phase2 ENGINEER CODE FROZEN: internal/bus/**, internal/cli/**, cmd/**, go.mod/go.sum complete. Latest focused `go test ./internal/bus ./internal/cli` PASS after database-failure classification refinement. No further source/dependency changes planned unless reviewer reports a defect. Reviewer may claim full/race/vet/cross checks in the single cleanup-trapped PG runner. Last source includes role/instance/code handling, audit fingerprints, bulk readonly snapshot with row/byte caps, retention tombstones, heartbeat truth for peer updates, bounded PG waits, and credential-safe remote CLI. Only CORE-API.md documentation consolidation remains, with no code change. No commits/pushes/real config changes.

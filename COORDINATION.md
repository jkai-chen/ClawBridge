# ClawBridge active coordination

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
was made. The next hosted design work is the transport/identity experiment below.

## Deployment steering

Owner suggested backend and Postgres on one Compute Engine VM to reduce baseline
cost. This is a candidate for the hosted phase, not permission to create a VM.
Keep the server portable. Investigate IAP tunnel plus application identity, or
HTTPS IAP with a load balancer. IAP TCP access alone does not establish per-request
application identity. Details go in docs/DEPLOYMENT-OPTIONS.md. This does not block
the local Phase 1 implementation.

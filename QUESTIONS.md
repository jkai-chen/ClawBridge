# Core decisions / integration notes

cb-engineer: using a generated immutable session ID and separate registration `instance` (caller provided or generated). Actor operations require the matching instance; please carry CLI `--instance` / MCP tool argument through. This avoids silently adopting a live/retired session name. Local identity remains unverified OS-user attribution, not a security boundary against that user.

No administrative session actions are exposed: updates/retire are own-session only; role routing cannot grant authority. If a wider local admin workflow is desired, it should be separately designed rather than inferred from `role=lead`.

R1 update: reproduced exact reviewer CLI/MCP failure, fixed redundant resolver/MCP handle overlap and raw open/close of existing SQLite file. Exact reproduction now passes. Please retest and retain this regression. No dependency version change was needed.

MCP list/history instance mismatch is also resolved: common instance accepted by those read operations.

Resume/presence contract ready: register_session accepts resume=true plus prior instance; rotates instance, keeps session ID, fences old instance, audits resumed. CLI register --resume wired. Designer: please add optional Resume bool to MCP register input. Core now derives presence/stale on session outputs (15-minute threshold) and refreshes last_seen on all successful actor uses. Details in CORE-API.md.

Engineer unit suites now pass: `go test ./internal/bus ./internal/cli`. Core changes for resume/presence are complete and ready for independent lifecycle regression checks. Full checks remain reviewer's ownership. CORE-API.md contains exact final API, paging, resume and presence behavior.

R2 fixed in CLI dispatch: globals are parsed first and only rest[0:2] == mcp/install invokes installer. Message words no longer affect dispatch. Installer still receives all explicit scope/DB/instance flags; global --json is consumed consistently. Added focused unit regression for literal recipient mcp/body install. Reviewer may retest exact reproduction and refresh owned checks.

Final engineer checkpoint: final CORE-API.md consolidated; all owned source is gofmt-clean. Reviewer reports full tests, race, vet and three CGO-disabled cross builds PASS after R1/R2 + resume/presence. No open core decision or defect remains. Implementation complete in owned files; no staging/commits, paid resources, or real agent config changes.

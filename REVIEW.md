# Phase 1 independent integration review

**Pass — local Phase 1 automated acceptance complete. No open reproduced implementation defects.**
Reviewer: cb-tester-reviewer. Final checkpoint: 20 October 2026. Read the original
DESIGN.md, approved PHASE-1-PLAN.md, current CORE-API.md, QUESTIONS.md and
COORDINATION.md throughout implementation. Owner approval came from the current
human instructions; older proposed-status text did not block authorized work.

Reviewer edited only `integration/**` and this report. Engineer fixed core/CLI;
designer supplied MCP/install/docs. No commits, staging, resets, paid resources or
real agent configuration changes. All tests use isolated temporary databases,
repositories and explicit temporary configuration paths. Linked-worktree testing
uses an unborn branch, with no commits or index operations.

## Final verification

Reviewer owns and completed all full checks after R1/R2 fixes, session resume /
presence changes, MCP/install refinements, and the final frozen MCP annotation
change (resume makes register non-idempotent; session mutations are destructive):

| Check | State | Evidence |
| --- | --- | --- |
| `go test ./... -count=1` | Pass | integration 2.545s; bus 0.242s; CLI 0.827s; install 0.103s; MCP 0.144s |
| `go test -race ./... -count=1` | Pass | integration 5.300s; bus 1.652s; CLI 2.470s; install 1.141s; MCP 1.336s |
| `go vet ./...` | Pass | No findings |
| CGO-disabled darwin/arm64 build | Pass | `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o TEMP/clawbridge ./cmd/clawbridge` |
| CGO-disabled linux/amd64 build | Pass | Same command with GOOS=linux GOARCH=amd64 |
| CGO-disabled windows/amd64 build | Pass | Same command with GOOS=windows GOARCH=amd64 |

Cross-build artifacts were temporary and removed. Non-host binaries were compiled,
not executed. No full heavy checks remain running or claimed by reviewer.

## Independent acceptance evidence

The suite has eleven top-level Store tests plus TestProcessIntegration with CLI,
long-lived Store, two MCP-process, installer and resolution scenarios. Tests use
`bus.Open` / `Store.Execute`; CLI/MCP scenarios run the actual compiled executable.

| Scenario | State | Evidence in integration/** |
| --- | --- | --- |
| Same `lead` in two projects and same slug in two workspaces | Pass | TestScopedMessageIDsAndReplay: foreign status/mark_read/reply/thread fail; inbox/history do not reveal sentinel IDs or content |
| Routing role grants no authority | Pass | TestRegistrationIdentityAndCallerOverrides: routing lead cannot supply a peer mutation target; API exposes own-session mutation only |
| Immutable MCP scope | Pass | MCPStdioPinnedScope: process starts from explicit temp config, config changes to another project, whoami remains pinned; per-call project/instance injection fails |
| Safe operation replay | Pass | Sequential replay returns the same message; body/address/kind/thread changes fail; eight independent handles concurrently retry one logical send and create one delivery/one sent audit event |
| Authority identity and time | Pass | Caller principal/time fields rejected; created_at falls within observed authority clock bounds; results source=local-authority and whoami says local-os-user-unverified / verified=false |
| Concurrent sends | Pass | 48 messages from six independent Store handles; returned IDs and unique bodies exactly match all inbox deliveries; long-lived handles also read external CLI writes |
| Delivery/read distinction | Pass | Send leaves both null; inbox stamps delivery only; mark_read preserves delivery and sets read; repeats retain stamps; mixed foreign mark_read rolls back entirely |
| Role/all snapshot expansion | Pass | Only active matching recipients at send time; new registration inherits no prior mail; replay does not re-expand changed membership |
| Thread ancestry | Pass | Root thread_id=self, replies preserve root thread_id and immediate reply_to; non-root thread IDs and nonparticipant references rejected |
| Stable cursor pages | Pass | Inbox/list/history bounded traversal; no skips/duplicates during insert/read changes; empty polling retains cursor; unreturned inbox page remains undelivered |
| Retirement/successors | Pass | Valid handover recorded; self/missing/foreign/cyclic successor attempts rejected; new sends to/from retired session fail; no forwarding |
| Atomic rejected operations | Pass | Rejected sends do not alter message audit; mixed-scope mark_read leaves earlier valid delivery unread |
| Secret rejection | Pass | Synthetic private-key/GitHub/AWS/OpenAI patterns rejected without echo; ordinary os.Getenv example accepted; malformed/oversize bodies rejected |
| Explicit local mode | Pass | Store requires local initialization; CLI cloud/offline-mirror config refuses operations and setup; no writable local fallback or queue |
| CLI/operator behavior | Pass | Two distinct CLI sessions exchange/read/reply/status; errors nonzero; literal mcp/install words and flag-like body work; successful application results identify source |
| MCP subprocess protocol | Pass | Initialize, discover all 11 tools, register/list/whoami/send/inbox/mark_read/reply/status/update/retire; valid JSON-RPC stdout; visible tool errors |
| Installation | Pass | Both Claude JSON/Codex TOML subprocess installs preserve unrelated entries, dry-run leaves bytes unchanged, repeat is byte-idempotent, original bytes backed up, invalid config untouched |
| Crash/restart incarnation | Pass | Resume rotates instance but preserves immutable session/delivery identity and replay key; old instance fenced; reopening with known instance works; retired name cannot resume; one resume audit event |
| MCP replacement process | Pass | New MCP subprocess resumes known prior instance; old still-open process fails actor calls; new process retains captured instance and can inbox/retire |
| Resolution | Pass | Flags > environment > checkout config > registered normalized remote; repository/worktree subdirectories; host/transport variants; path case preserved; missing/ambiguous matches refuse |

Engineer-owned unit tests additionally cover injected delivery-insert failure
rollback, stale presence and successful heartbeat/failure rollback, transactional
audit behavior, unsupported/newer database schema handling, and CLI resume/config
preservation. These passed in the full and race suites above; they are distinguished
from the independent black-box integration cases.

## Reproduced defects and owner fixes

### R1 — Resolved / retested Pass: MCP SQLite lifecycle corruption

Original exact repro:
`go test ./integration -run 'TestProcessIntegration/(CLIExchange|MCPStdioPinnedScope)' -count=1 -v`

Observed twice on fresh temp databases: MCP sent to bob; CLI inbox and reply
succeeded; next long-lived MCP inbox returned `SQLite unavailable (no write queued):
database disk image is malformed (11)` as a tool error.

Engineer reproduced and fixed overlapping resolver/MCP Store lifetimes and raw
opening/closing an existing SQLite file, which interfered with POSIX locks.
The original subprocess scenario now passes, including in the final full/race
runs. StoreAcrossCLIProcesses and two simultaneous MCP processes retain regression
coverage. No dependency downgrade or disabled messaging capability used as a fix.

### R2 — Resolved / retested Pass: message words became CLI dispatch

Original exact repro:
`go test ./integration -run 'TestProcessIntegration/CLILiteralMessageArguments' -count=1 -v`

After registering alice and mcp in isolated w/p, this normal send failed:
`clawbridge --db TEMP/bus.db --workspace w --project p --session alice --instance alice-instance send mcp install --operation-id literal-command-words`

Observed stderr: `clawbridge: invalid global flags before mcp install`. Expected:
a message to session mcp with literal body install. CLI scanned argument words
before identifying the actual command. Engineer fixed dispatch after global flag
parsing. Exact regression and explicit `--` flag-like-body case now pass, including
final full/race checks. Installer subprocess scenarios remain Pass.

Earlier contract gaps resolved during implementation: inbox/list/history gained
stable cursor pagination; core list/history accepts the adapter's common instance;
SQLite dependency sums were completed by engineer. No implementation files were
edited by reviewer.

## Installed-client evidence and remaining limits

**Separate manual evidence, performed by cb-designer:** reviewed
[docs/CLIENT-CHECKS.md](docs/CLIENT-CHECKS.md). This supplements the independent raw
JSON-RPC suite and is not silently run by `go test`.

| Client check | State | Proven scope / limit |
| --- | --- | --- |
| Installed Codex CLI 0.163.0 app-server | Reported Pass | Actual MCP connection and register/whoami/list/send tool calls; CLI peer replied and Codex MCP inbox received it. No model turn started. |
| Installed Claude Code 2.1.304 | Reported Pass (connection/discovery) | Actual temporary-config connection and SDK-control discovery of all 11 tools. Claude tool execution not exercised. |
| Complete live Claude+Codex tool workflow | Pending | Claude register/send/reply/read through its actual client still needs evidence. |
| Model-driven cooperation | Pending | No model requests submitted; autonomous tool choice and persistent interactive chat behavior not tested. |

Designer reports all installed-client config/database paths were temporary; real
agent configs/credentials were unchanged. Reviewer did not independently rerun
those manual installed-client checks and does not claim live conformance from SDK
simulation. Pure local identity is not security against another process of the
same OS user. Hosted authentication/cloud, paid deployment and Postgres are later
phases and were not exercised or provisioned.

Review work is complete for the implemented local Phase 1 automated scope. The
remaining installed-client conformance gap is explicit; no open code defect or
unfinished full check is hidden behind a Pass.

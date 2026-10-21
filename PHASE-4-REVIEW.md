# Phase 4 independent review

Status: **PASS — frozen local/synthetic automated acceptance; live/manual gates Pending**.

Baseline: clean pushed `e8db318ebdd56785b5a214e9fca41dd69e71fece`.
Reviewer owns this report, `integration/**`, independent acceptance and final
full/race/vet/cross-build gates. Engineer owns implementation; root owns the plan
and public contract. No implementation edits, staging, commits, deployment,
live consent, VM operations or real user configuration changes are authorized
for this review stage.

## Scope and evidence boundary

Owner's target is SDK-free onboarding to an existing service, explicit admin
initialization, and a TUI entry. Existing infrastructure `BuildPlan`/`Apply`
scaffolds must not become a production provisioning path. PHASE-4-PLAN.md and
CORE-API.md's Phase 4 section (line 867 onward) are published. Their explicit
join/config/transaction, MCP/doctor and terminal-entry semantics govern this review.
Pre-freeze observations are not promoted implementation defects. Synthetic fixtures
are separate from live clean-machine, Google consent and native-keychain evidence.

## Acceptance matrix

The statuses below distinguish automated evidence from remaining manual acceptance.
Fixtures will use isolated homes/configs/stores, loopback HTTP where permitted,
synthetic identity/keyring adapters and subprocesses with sentinel executables.

| ID | Acceptance scenario | Intended independent evidence | Status |
| --- | --- | --- | --- |
| P4-J1 | Existing-service join selects an explicit trusted service and scope, with no cloud provisioning | CLI subprocess against isolated synthetic service; capture bounded requests and resulting trusted config | Pass synthetic CLI/HTTP; live Pending |
| P4-J2 | Normal OAuth join is SDK-free; no implicit gcloud/ADC fallback | PATH without gcloud plus fail-on-call sentinel; credential-source and child-process assertions | Pass injected OAuth/route isolation; physically SDK-free live Pending |
| P4-J3 | Browser/device discovery and applicable credential pairing retain Phase 3 guarantees | Existing synthetic OAuth/discovery regressions plus new join entry-path checks | Pass synthetic/injected; live Pending |
| P4-J4 | Online identity and project authorization are required before join success; cached/offline data cannot authorize joining | Rejected identity/membership, outage, credential-failure and mirror fixtures | Pass independent strict identity/scope/HTTP probes |
| P4-J5 | Join does not silently initialize a workspace, grant roles or modify unrelated server state | Request audit asserting no admin/provisioning operations; before/after isolated store state | Pass independent request audit |
| P4-J6 | Trusted config and keychain writes have the documented transaction boundary and failure behavior | Failure injection for acquisition, credential stage, online validation and config publication; prior bytes/credential assertions | Pass injected transaction and independent publication probes |
| P4-J7 | Repeated join and interrupted join have explicit, safe lifecycle semantics | Repeat/cancel probes and registration/session-incarnation assertions where delivered | Pass repeat/cancel; live Pending |
| P4-J8 | Installer/MCP/doctor reconstruct the joined route without embedded secrets | Inspect isolated installed argv/env and doctor results; reject conflicting routing/credential values | Pass dry-run OAuth and fresh-process synthetic MCP/doctor; live OAuth client Pending |
| P4-C1 | Repository config remains scope-only in all authority directions | Retained local/cloud/repo-only subprocess regressions; join-specific precedence checks | Pass retained suite |
| P4-C2 | Explicit config/environment/CLI precedence remains deterministic and fails closed | Isolated conflicting routes/backends, invalid inputs and redaction checks | Pass focused preflight/config probes and retained suite |
| P4-A1 | Admin initialization is a distinct explicit action with required inputs and verified bootstrap authority | Successful owner and denied non-owner synthetic server/store cases; exact operation audit | Pass retained explicit bootstrap authorization regressions |
| P4-A2 | Admin failure does not publish a successful route or perform unrequested actions | Authorization/transport/config-write failure probes; distinguish server mutation from local publication where atomicity cannot span both | Retained tests Pass; no new live admin operation |
| P4-A3 | No onboarding/admin/TUI entry invokes obsolete infrastructure apply or provisions IAM/resources | Subprocess sentinel runner; inspect production dispatch paths and request log | Pass manual production dispatch review; no Apply adapter |
| P4-T1 | TUI is reachable through its documented entry with the same pinned route and scope | CLI dispatch probe; injected model/executor coverage; pseudo-terminal smoke only if needed | Pass injected CLI route/model; real terminal Pending |
| P4-T2 | Non-terminal/TERM=dumb inputs fail promptly; help stays noninteractive | Bounded subprocess probes with redirected streams and isolated environment | Pass independent subprocess help/nonterminal probes |
| P4-T3 | TUI presents only delivered actions; writes/admin operations require documented explicit intent | Independent model/action and executor request assertions; cancellation/quit checks | Pass actor/draft/action probes |
| P4-T4 | TUI retains terminal-control escaping, ordinary Unicode and unchanged stored data | Retained A3 dynamic-surface regressions through delivered entry/model | Pass retained escaping regressions |
| P4-T5 | TUI handles credential/offline/stale async results without authority changes or silent write queues | Controlled executor/model failure/cancellation fixtures and race verification | Pass independent offline/stale/cancellation probes; race Pass |
| P4-S1 | Errors/help/diagnostics/drafts never disclose tokens, secrets, unsafe response bodies or credential-bearing URLs | Sentinel-secret assertions across new entry points and failure paths | Pass sentinel diagnostics and installed route probes |
| P4-R1 | Existing local/cloud/auth/store/mirror/MCP/install/doctor behavior remains compatible | Retained suites and focused regressions justified by changed dispatch/config paths | PG-backed full Pass; race Pass |
| P4-G1 | Final uncached full and race suites, vet, three CGO-disabled cross builds pass on one frozen target | Reviewer-owned exact commands/results, HEAD/fingerprints and disposable resource cleanup; retain actual PG coverage where applicable | Full/vet/cross Pass; PG-backed race Pass |
| P4-D1 | README/help/plan/contract accurately distinguish shipped onboarding/TUI from future provisioning | Manual docs/dispatch reconciliation; synthetic/live evidence clearly separated | Pass manual README/plan/contract/dispatch reconciliation; live claims remain Pending |

## Checkpoints and findings

Initial inspection only: existing setup infrastructure adapters and callable TUI
remain Phase 3 scaffolds at the baseline. Their existence is not a Phase 4 defect.
No live end-user onboarding, native-keychain, deployed-service, interactive TUI or
new platform acceptance is claimed by this preparation checkpoint.

Published-contract details retained: join requires explicit server/workspace/project,
validates nonsymlink target and actor pair before acquisition/network, authorizes
only from exact online server identity/scope, stages fresh OAuth, publishes private
config atomically, then commits credentials. Failure/cancellation must preserve prior
bytes and report rollback failures truthfully. Config deletions and permission fixes
count as changes; identical private config is a byte-level no-op.

`--client-config` is absolute, unique, bypasses home/repository config, and is distinct
from install's agent `--config`. Cloud MCP pins it without credential values and
requires an explicit distinct session. Doctor's explicit MCP selection is read-only.
TUI help and nonterminal rejection precede storage/auth/network; production Cloud
screen is informational and has no infrastructure apply adapter.

Manual checklist in PHASE-4-PLAN.md is acknowledged: physically SDK-free machine,
live member/nonmember and second human, native keychains, real terminal and client
execution remain Pending. No such operations are authorized by this focused review.
Waiting for engineer's explicit freeze before final heavy verification.

### Published-contract focused checkpoint (21 October 2026, pre-freeze)

Only reviewer files changed. Commands run against the actively edited shared tree:

- `go test ./integration -run '^TestPhase4Join' -count=1 -v`: Fail in
  `TestPhase4JoinRequiresAuthoritativeIdentityAndScope/empty-resolved-workspace`
  and `/empty-resolved-project`. All other identity/scope rejection cases pass;
  all six target-preflight cases pass; stale-backend deletion, permission-only
  correction, 0600 publication and byte-identical repeat join pass.
- `go test ./integration -run '^TestPhase4CLIRejectsBeforeAuthorityOrStorage$' -count=1`:
  Pass (`clawbridge/integration`, 2.180s). Thirteen isolated subprocess cases cover
  absolute/nonempty/unique client-config, three explicit-scope requirements despite
  inherited scope, conflicting browser/device acquisition, actor-pair validation,
  positional rejection, join/TUI help, nonterminal TUI and partial doctor selection.
  No synthetic service requests or state-directory creation observed. PATH contains
  only the disposable directory; no Google acquisition or native keychain used.
  Initial test expected “absolute” for an empty path; implementation correctly said
  “nonempty”; reviewer assertion corrected before the passing rerun.
- `go test ./integration -run '^TestPhase4JoinInputDiagnosticsExcludeAppSecrets$' -count=1 -v`:
  Fail for `%v`, `%+v`, `%#v`; JSON serialization passes. Test diagnostics suppress
  values and use synthetic sentinels only.
- `git diff --check`: Pass at this checkpoint.

**P4-O1 — pre-freeze observation, pending implementation/retest:**
`go test ./integration -run '^TestPhase4JoinRequiresAuthoritativeIdentityAndScope/empty-resolved-' -count=1 -v`
reproduces accepted empty immutable scope. Executor resolves one ID to empty and
returns the same empty ID in an otherwise verified online server whoami. Join
publishes instead of rejecting. Expected: reject missing resolved identity/scope
and preserve original trusted config. Owner: engineer (`internal/setup`).

**P4-O2 — pre-freeze observation, pending implementation/retest:**
`go test ./integration -run '^TestPhase4JoinInputDiagnosticsExcludeAppSecrets$' -count=1 -v`
reproduces JoinInput app-secret exposure through ordinary Go diagnostic rendering.
Expected: public secret fields excluded from JSON and diagnostic rendering per
CORE-API. This demonstrates a rendering hazard, not an observed production log
or live credential disclosure. Owner: engineer (`internal/setup`).

Neither observation is promoted as a frozen implementation defect. Heavy gates,
remaining transaction/MCP/doctor/TUI probes and all manual gates remain Pending.

Additional focused probe:
`go test ./integration -run '^TestPhase4CLIInstallerPinsTrustedOAuthWithoutSecrets$' -count=1 -v`
Pass (`clawbridge/integration`, 2.857s). Actual CLI dry-run inherits the trusted
OAuth/cloud immutable scope, pins absolute client-config, bypasses malformed
repo/env-selected configs, rejects inherited operator session, requires explicit
agent session, and excludes both app-secret sentinels and local DB/operator
instance from output. Trusted bytes unchanged; no target agent file written.
This is route construction evidence, not real MCP tool execution or keychain use.


### Revised freeze and independent focused retest (21 October 2026)

Engineer explicitly re-froze after P4-O4. Acceptance target is the dirty shared
worktree at HEAD `e8db318ebdd56785b5a214e9fca41dd69e71fece`, not HEAD alone.
The two supplied setup SHA-256 values match exactly. Reviewer additionally records
all Go/module source bytes below for final stability comparison.

P4-O1 and P4-O2: **Closed / Pass**. Empty resolved IDs now fail closed and
JoinInput JSON/ordinary diagnostic formatting suppresses both app secrets.

P4-O3: **Closed / Pass**. Exact repro:
`go test ./integration -run '^TestPhase4TUIActorlessActionsDoNotReachAuthority$' -count=1 -v`.
Previously actorless send/reply/mark-read reached the executor; Inbox already
blocked. Revised guards prevent all four from reaching authority.

P4-O4: **Closed / Pass**. Exact repro:
`go test ./integration -run '^TestPhase4JoinPublishesCompleteOAuthPair$' -count=1 -v`.
Previously reusable Join accepted missing active app secret and published a route;
CLI-only validation did not protect the public boundary. Revised Join rejects
before resolution/execution/publication. Independent optional-device-pair probe
accepts absent/complete pairs and rejects ID-only/secret-only before authority.

Commands and results on revised target:

- `go test ./integration -run '^TestPhase4Join' -count=1 -v`: Pass, 0.313s.
- `go test ./integration -run '^TestPhase4TUI|^TestTUICloudOffline' -count=1 -v`:
  Pass, 0.383s. Draft edit/cancel has no write; explicit submit writes once;
  denied write has no automatic retry; stale-screen result is ignored; in-flight
  reads receive cancellation; offline writes are not queued.
- `go test ./internal/cli -run '^TestPhase4' -count=1 -v`: Pass, 0.410s.
  Reviewed injected transaction cases cover browser/device stage/commit/denial,
  replacement restoration after cancellation/publication failure, eight
  new/replaced x successful/failed cleanup x construction/resolution failure
  combinations, a bounded non-canceled cleanup context, truthful partial cleanup
  errors without secret echo, and reused credentials with no acquisition/rollback.
- `go test ./integration -run '^TestPhase4|^TestTUICloudOffline' -count=1 -v`:
  Pass, 7.473s, including the optional-device-pair probe.

New subprocess evidence: actual CLI join against isolated synthetic loopback HTTP
passes authorized publication and rejects 403/offline-source/wrong-principal
without mutation/body echo. Only resolve_project/whoami occur, no inherited actor
is adopted, scope changes clear actor, relative cache anchors to config, unrelated
TOML survives, and repeat join is byte-identical changed=false. Fresh installed
MCP executable/argv run in a separate subprocess and selected doctor reconstructs
the pinned immutable route and agent actor without tokens in the agent entry or
local storage. These HTTP/MCP fixtures use a fake gcloud executable and synthetic
tokens; they do not prove OAuth/native-keychain or live Google behavior. Injected
OAuth transaction tests are likewise synthetic.

Final heavy gates are authorized and now running sequentially with disposable
Postgres. Live/manual clean-machine onboarding, second-human authorization,
platform keychains, interactive terminal/client acceptance and pilot deployment
remain **Pending**, not inferred from these probes.

Frozen source manifest (SHA-256):

```text
8bccd0c65435079d71b5f5953cb53849aa0cac8263f6e6f1c3b9a2aaa5b229b2  cmd/clawbridge/main.go
329a4f5910527621a432ce97451e940ddf073eb47d6f3e28ac94b1b36e4d98a7  go.mod
192cf3cdf421703c7ac096b30431657fbfd7dc70bf0d69f85a8f2d3ba2737f73  go.sum
8c0f9e2f90d4d6f1f287c9f3a2d7c0a61d9c7e8a0cd16b07b35039e5f4c6ec48  integration/audit_doctor_test.go
1b0e0cd1cd21b0cdc37bb8ebf358b47000e2da7477a05f108a3057773330806a  integration/audit_scaffold_test.go
3204f206386088fe292bc7c2a5c23861170c51c1d7aea357b0d4bb3553347744  integration/cloud_cli_test.go
8010d55a135e8bf259fe338546de4f3cd06776c70745922e397980f183eda6c6  integration/compose_contract_test.go
812046a987946ceeb46fe82c218f226c7d21385acd6a3d64ca419b750522b8b4  integration/deployment_probe_test.go
0db055f6cb1e8462f0b823c0342d4370442713d2e5532da632db6c378c1bb9b0  integration/doctor_cli_test.go
9415cbadec61958e5eb5ac6720037890ae32a2b7461da4f43f9757cd403807d8  integration/doctor_probe_test.go
d8652e75acd8f03aed6b439d137cb42400af6bce4a58a854f54e77ef1e7d6664  integration/helpers_test.go
cae6d809704e8bd67116cc573c502f57a48198bb174c2df4451409d3695635aa  integration/mirror_test.go
52e5afe697175d9cac826a8b2f7400c1d95be824ab3e3f128b70c4199a3c1ca8  integration/multi_audience_test.go
ca1ab5a9bf8bc7af338f75fcf438318c1aa9af950aff44dc1ecf1a2b1deff2fb  integration/phase3_discovery_test.go
4bd17d8a59c3487dbab00315be9be4d5e3e2442e01ee03eea8c804a6c092b4ea  integration/phase3_live_google_oauth_test.go
47d18066c13046b3d38b3dce0b07fa855e69c2577bad83d222eb81098db18b62  integration/phase3_oauth_test.go
ad1a190ea5ac1c35d46ccdf8a63080435aff8355c8720875f6c34f19c40fdff2  integration/phase3_routing_test.go
31762f8b83e3d96e38883d95e25de479579f558304130f10ea1323b2f344d805  integration/phase4_cli_test.go
74626a1f43e650e5f40a939b808b7b420e83af3b21101de638bdaa7a85bf4708  integration/phase4_join_http_test.go
787ee01bed59bfecca8941cf4b5837c7a4487319c15ea741c2e8a137c6a1d489  integration/phase4_join_test.go
1c62de2417d2b4843d95224ba1665ea7201ecb40e2a6abaee2cae3a61d53c627  integration/phase4_mcp_test.go
955c68d44a81b4c7fb5e4c1709a49ca942bc185909bf768b3ff00d292100dd96  integration/phase4_tui_test.go
469c1a96ae1dc7a121a9fd14ecddfbb8f73e2755dbd8857cf284529cb33bf40c  integration/postgres_project_views_test.go
21071827f330101785182ac04ca1ae9df0ba448b0edffe3ffc1f7d2bcec0dce4  integration/postgres_retention_test.go
6583e4e8f259f39decde5019aa6a72ed4187f2251e5272f94ae273115067be72  integration/postgres_test.go
c434933f623f44879ad14efef4b366b0263d2d00e49e18bb846e60aa9cdac40c  integration/process_test.go
d4a492507bde3a7d23166d883a96d5fd4af34da5948732af5ee2c8fad7437ff2  integration/remote_test.go
b8a78785706122ceb92ff985c05358b27d045dc614220ab17cd6c6f370a8260a  integration/resolution_test.go
ee8af0332c073d54fe0e8f466a6db344ad5fedc1a31892b851a032e268aefc2e  integration/server_test.go
8d461dca8e36dc46be07814775964c7baf21b288056f50be475772f24c56d614  integration/setup_test.go
8a776d43949d339e9ffc9ec2e726f98218e7006cadd5d96ca7c1b79d21995f66  integration/state_test.go
5d4af64b1959f3d3cb900aa60ee3177663699f551f40fdef538507790b43a43a  integration/store_test.go
659e796176f0112e61f2c05bf831053354cf4ac23ead21fc28908c2153e8a560  integration/tui_test.go
d6a25e7658ee68077f9dec926162f74466800a59fb35d633fa4453a3bbda4884  internal/auth/oauth.go
f8301a8d37cc28321b3676d2684cbc1b55594f31dde61e4f22db8109a8a04dcb  internal/auth/oauth_test.go
e08221b5bf3150a4b4df9dd596d33bc97e77a29b75125abc4a14edc8d62b3f6d  internal/auth/token.go
7cdea38dc70e1ade804f6441f26dc69db6c2bed1353e2550df700ee6657ea4c6  internal/bus/dialect.go
56c5edb37e2998d2719639e5e65e1d059c8290afdfca2fa93543358785e012b3  internal/bus/errors.go
cce1b22dedb8c83172795a17faebc46cb5d4999ca77fd9b2d64924dc1f302696  internal/bus/hosted.go
48753bc9ad659a8a5b5db624766507079601b039938ce35449996ca604760dd5  internal/bus/hosted_test.go
878797a77944b0cea8f294c380cf40270caaa372fc134421662ca3a86c0d79fa  internal/bus/messages.go
9b06744a7820db2fbb4ae439058089a86a3d3739bd4bf9ed7ba72647bd1e2c35  internal/bus/postgres.go
4a0ea578d04fe37ac570dfeae5de7b4b3dd468203af6a4f6a2e804a808218d47  internal/bus/sessions.go
b493b0ab9f1fd80116321793cb60e831bf770d540abe9d74600f5454ebe68bd1  internal/bus/store.go
1832a5f67957fcfba0977c889aadbfb937f197a90c7a22586cfac419f740c97d  internal/bus/store_test.go
f527c7a276c3f72278e3a9cdbeff594260ed606891251f9474c66ccf925ffe28  internal/cli/adapters.go
a56cd7ccdf78e4bcc1bd343b4f902cdba4d20cd2419d42e39bafcd943da1f6b1  internal/cli/cli.go
8b04da16ac8ea3ea13f7dd5fc2af84e59b13bdd2fc8df3c4e6b1ab946d7607c7  internal/cli/cli_test.go
e2164370eec98bc886b216c41bdf4dd58659812269f1a43d2d8a182020d66bb6  internal/cli/config.go
f716cd47dd202bea930a5723d1a82dbfd0d81199a2e8929b118c747438762527  internal/cli/join.go
92356378a907bf4d813282db4e96614eed6966dd2c9f0a7878b8cf6111f26657  internal/cli/login_test.go
fdcfeae10420103736727f805fd84eb54517f429622f896780fa6e8d95cd7552  internal/cli/oauth.go
57a2b89ef223e1cb0dd2ec5a332dccc8438fbc69ce9aa3a58ae4fbf9c71d186e  internal/cli/phase4_test.go
b5a70e25f1c6a6632e2c045a6e4630dbb1480b8c08e0ae8e8ad5b55b50162e60  internal/doctor/doctor.go
011a65476f41c2e1fea5b61ae839bd5b1af7bb9323e7290744183478bc2c7146  internal/doctor/doctor_test.go
b49a785172bcdfeafb9832b0613030fe5401f4459c87ce2578eafb456bd0b85c  internal/install/install.go
7cd60bee0108acf133a4cefe9a9a141b92c7f82baff5b17408301bc5b390de81  internal/install/install_test.go
d5410dddc582133624b7f4d94c0e5ee9bc6ec98fa6064485236874f13d8f9c10  internal/mcpserver/server.go
52127d6748c863cfb6d447b59e42c08f95938027f99388bf868e9cbcdbdfab11  internal/mcpserver/server_test.go
4ef5a9620c5579c8edf6cc647badf1eb40571fb7902524ba49deeb6c4ff09aed  internal/mirror/mirror.go
34c322d2fc3d5a27f28607c8c7866475560276588750c715f1f8cdcce57db76e  internal/remote/client.go
f03faa68c76f59d47b404e214dba1e5327c92c8adcbbd3c40466b029b587cde7  internal/remote/client_test.go
eb29770a229094f3b2dd8aba846fb45d0cefa179cddc6649336323e6059b53ae  internal/remote/discovery.go
260c628978c28eb77594806f5d7b330dd944f3defc59b2afd69cee9eafa58400  internal/remote/discovery_test.go
c509f9e60a0838f172eba1c0f44ca301319a0341ea38d15a762c59a950d5a633  internal/server/server.go
51e7dad127f39ac60efe5ad5c207f9017a1433ca4571f3b0a3fa9cc941aebdd0  internal/server/server_test.go
31fe4433b912302e102927c9c68c0cf79add417394d13dc91f999e82b0c42614  internal/setup/setup.go
983c48a52c02b87375a6b9b227cdf559e941fce3aaec8ecc74d6231cf3131c32  internal/setup/setup_test.go
56fdead378387980a692792e7406ebe5a1478d59163ea6564bbbebb5677e7f0e  internal/tui/model.go
23d294696d17ff7bf771f6873ab4e119a6b5a66837bbeb141baf5127eb8b7b8b  internal/tui/model_test.go
65bf484a13d4a811e0a7437ee04430632c08a8fea74578ef430bb558cf117098  internal/tui/run.go
03324eb622d82d45e7a692d4e3a83a3f4ac510d7785effc4a89247f1790580b2  internal/tui/view.go
0f1decde36f125e74e9563c474fc1c53c8132187770dce67fee96305ce6031bc  internal/tui/view_test.go
```

Postgres test run: container `bd638d0afd13ffc1739cd63b6f5d36e56ffe4548aef194447f4b49b4d0b068b6`; loopback `127.0.0.1:32786`; anonymous volume `005d46d84e973242f37e0246310fa41d66693e8c172414e2feefce6fd3e94ef8`.

Preliminary final-gate results: `go vet ./...` Pass (exit 0).
`CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o TEMP ./cmd/clawbridge`,
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o TEMP ./cmd/clawbridge`, and
`CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o TEMP ./cmd/clawbridge`
all Pass (exit 0). Outputs were isolated in an automatically removed temporary
directory. All five engineer-supplied revised hashes match exactly, including CLI
transaction tests and TUI model/tests. `git diff --check` Pass before heavy gates.
Cleanup: container `bd638d0afd13ffc1739cd63b6f5d36e56ffe4548aef194447f4b49b4d0b068b6` removed with anonymous volume.

Postgres test run: container `2419d4147d7a1c9d5f45fe92a0b89d3c6aad46a2e6cb2722ebe6d718d9293008`; loopback `127.0.0.1:32787`; anonymous volume `298db4e4e670ef19ad32a7cff33a0eedf889647082ad5402c52024b5592ee30d`.
Cleanup: container `2419d4147d7a1c9d5f45fe92a0b89d3c6aad46a2e6cb2722ebe6d718d9293008` removed with anonymous volume.

Manual dispatch/docs reconciliation: README's join/config/MCP/doctor/TUI guidance
matches the published boundary. Production `runTUI` installs the selected executor
and doctor, informational Cloud text, no SetupSource; resources close and child
context cancels on exit. Explicit cloud setup requests only server workspace_init;
its existing post-operation config failure reports initialized-but-write-failed,
not remote rollback. Retained real-PG bootstrap owner/nonowner tests pass in the
full gate; this is not a newly exercised live administration operation.

`CLAWBRIDGE_TEST_RESOURCE_LOG="$PWD/PHASE-4-REVIEW.md" integration/run-postgres.sh go test ./... -count=1`:
**Pass**, integration 42.774s; all packages passed. Real PostgreSQL 17.11 ran
with required-PG environment, not an env-absent skip. Runner cleanup and independent
Docker inspection confirm full-suite container and anonymous volume both absent.
No existing containers, images or volumes were removed. Random test-only DSN was
passed only through environment and was not recorded.


### Final automated gate result — PASS (21 October 2026)

`CLAWBRIDGE_TEST_RESOURCE_LOG="$PWD/PHASE-4-REVIEW.md" integration/run-postgres.sh go test -race ./... -count=1`:
**Pass**, integration 47.917s; all packages passed with no race report. Actual
PostgreSQL 17.11 used a separate fresh loopback fixture. Runner cleanup plus
independent `docker container inspect` / `docker volume inspect` confirmed both
race resources absent. Exact IDs/ports/volumes and runner cleanup are recorded
above for both runs. No production DB, native credential store or agent config
was used.

Final independent SHA-256 comparison: **all 73 Go/module files unchanged** from
the frozen manifest through focused/full/race/vet/cross completion. Revised
engineer fingerprints match. Final `git diff --check`: **Pass**. Cross-build
outputs were removed; both PG fixture containers and anonymous volumes removed.
Reviewer edited only integration probes and this report, with no staging/commit.

No unresolved reproduced automated defect remains: P4-O1/O2/O3/O4 are closed by
independent focused retests and retained by the passing full/race gates. This is
a scoped automated PASS, not complete live Phase 4 acceptance. Remaining gates:
physically SDK-free fresh-machine browser/device join; live member/nonmember and
second human; actual OS keychain transaction/lifecycle; fresh installed real agent
with OAuth; real interactive TUI/terminal restoration; platform-specific behavior;
and any separately authorized pilot deployment. Earlier Phase 3 live evidence
does not substitute for these new Phase 4 journeys. No such live/manual operation
was started during this review.

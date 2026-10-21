# Phase 4 live acceptance

Status: **Partial — owner-reported Linux device onboarding succeeds; focused TUI correction automated Pass; replacement live UI retest and other live gates Pending**.

## Attribution and target

Evidence below was reported by the owner through root on 21 October 2026.
The reviewer statically inspected the distributed launcher and archive but did
not observe the target terminal, Google consent or Secret Service directly.
Synthetic automated evidence remains in PHASE-4-REVIEW.md and is separate.

Target: separate Ubuntu 22.04.5 LTS WSL2 Linux x86_64 environment, headless/SSH.
Owner preflight reported gcloud unavailable. This is a reported separate-machine
observation, not the reviewer's restricted-PATH simulation or independent proof
that no SDK files exist anywhere on the machine. Initial session bus/Secret
Service was unavailable; the procedure required an available unlocked Linux
Secret Service and prohibited plaintext fallback. No Windows Credential Manager
bridge or unattended keyring setup is implied.

Distributed source: `4a39235114b00873c39d1a0d67e9ee2b13bcd0e0`.
Archive: `clawbridge-phase4-linux-amd64-4a39235.tar.gz`.
SHA-256: `da98f6c8d27e2e1172a9d644dd8604a72f8154ec58a336f0e7b01e4702450496`.
Reviewer verified the prepared archive hash, all four manifest entries, regular
archive members, isolated absolute client config/cache, unique keyring service,
private state directory, device-only flow and explicit bus/service preflight.
This establishes artifact/procedure integrity, not native execution by itself.

## Reported live results

| Gate | Result and evidence boundary |
| --- | --- |
| Device join to existing service | **Owner-reported Pass**. Real device consent and join completed against `https://34-142-66-102.sslip.io`, workspace `pilot`, project `clawbridge`. |
| Fresh-process authenticated identity | **Owner-reported Pass**. Subsequent whoami returned `source=server`, online/verified, `identity_provenance=google-id-token`, principal `jiakai@greentally.ai` and intended scope. Supports credential reuse across processes; no claim that a forced token-expiry refresh was separately observed. |
| Doctor | **Owner-reported Pass for four required checks**. Mirror-incomplete and unselected-MCP warnings expected. This does not establish an installed or model-driven MCP exchange. |
| Linux credential storage | **Owner-reported functioning native path** through successful device join and later process. Locked/unavailable-store rollback, secret persistence inspection, explicit expiry refresh and logout remain separately unobserved here. No plaintext fallback is accepted. |
| Real terminal TUI | **Partial**. Owner reports all six screens usable except alignment on tabs 2/3 (Projects/Sessions). No blanket UI/manual Pass. Terminal restoration, cancellation and mutation behavior are not separately evidenced by this general screen report. |

## Live finding: Projects/Sessions alignment

Status: **Automated correction Pass — owner replacement-build retest Pending**.
Reproduction reported by owner: launch the supplied Linux TUI in the target
interactive terminal, open tabs 2 and 3; table alignment is incorrect. The report
does not yet include terminal dimensions, exact row data or screenshot, so no
more specific root cause is asserted. Engineer owns implementation. Reviewer
will inspect the frozen fix and run justified focused regression coverage;
owner confirmation on the new artifact remains a separate live UI gate.

### Frozen table/attribution review (21 October 2026)

Engineer freeze at 12:58; reviewer verified all three exact SHA-256 values before
and after focused checks:

```text
d6e19ec8c79b68f5d4c2e0993b8761e0a5882aff6d7e43d39a0843cdafbaaa94  internal/tui/model.go
8033488311d66fab516afea646486548461c295d9ef329b959dbce7104bfe287  internal/tui/view.go
a0e5876e6dd875cb1aa5538dd6ebe8737913188bef2621ab4776ddf4d2a087c1  internal/tui/table_test.go
```

Manual review against HOST-IDENTITY-DESIGN.md: shared display-cell column sizing
and selection gutters replace byte/rune printf padding. Narrow tables drop
trailing columns; selected-session details retain complete sanitized owner/host
values with wrapping. Existing owner_principal/host/account fields alone are
decoded. Host and account details are explicitly self-reported; account is not
verified identity. Local display explicitly disclaims Google verification.
Missing host is “Not recorded”; no host is inferred or collected. Peer instance
is not mapped into SessionRow. No API/schema/authority/provisioning change.

Independent integration probe drives the public Model through list_sessions and
rendering in local/cloud modes at widths 8, 12, 20, 32, 56, 80 and 140. It checks
full wrapped long email/host values, CJK/combining/ZWJ emoji, escaped escape/OSC/
carriage-return/bidi controls, account qualification, original metadata unchanged,
no peer-instance output, and no request beyond the single list_sessions read.
This is automated rendering evidence, not observation of the owner's terminal.

Commands/results:

- `go test ./internal/tui ./integration -run 'TestTable|TestSessionsAttribution|TestPhase4TUI|TestTUI.*(Control|Escape|Offline|Dynamic)' -count=1 -v`:
  Pass (tui 0.326s, integration 0.419s). Includes column/gutter/display-cell and
  missing/full-attribution tests plus retained actor/draft/offline/cancellation.
- `go test ./integration -run '^TestPhase4SessionsDisplay' -count=1 -v`:
  Pass, 0.416s. Stronger assertion retaining the complete sanitized host then
  rerun with `-count=1`: Pass, 0.432s.
- `go test -race ./internal/tui ./integration -run 'TestTable|TestSessionsAttribution|TestPhase4SessionsDisplay|TestPhase4TUI|TestTUICloudOffline' -count=1`:
  Pass (tui 1.264s, integration 1.619s), no races reported. The subsequent change
  only strengthens the reviewer's full-host assertion; production bytes stable.
- `git diff --check`: Pass.

Verdict: **focused automated Pass, no reproduced defect in this frozen scope**.
No full/PG/heavy reruns were needed for the presentation-only change. Owner
retest of Projects/Sessions alignment in the replacement Linux artifact is
**owner-reported Pass**, as attributed below; the original build remains historical.

## Remaining acceptance

- Same-config device-to-browser transition, requiring an explicit browser-capable
  display/tunnel procedure on this headless target; not attempted by this package.
- Repeated logout and exact isolated native credential removal; post-logout
  credential failure despite any mirror; forced-expiry refresh lifecycle.
- Real installed MCP client, fresh process with OAuth, selected doctor and actual
  tool execution; model-driven behavior attributed separately.
- Second human member and nonmember denied join, without implicit membership grants.
- Explicit terminal restoration/cancellation and any intended actor actions;
  replacement Projects/Sessions alignment is owner-reported Pass below.
- Other native platforms, physically SDK-free observations beyond this reported
  target, and any separately authorized deployment/release distribution.

No credentials, consent codes, secret config contents or tokens are recorded.
Reviewer performed no live requests, consent or target operations. Automated
rendering tests are attributed separately above. No schema/user-host-session implementation or
acceptance is implied by the prospective design work.

## Owner screenshot attribution checkpoint

Owner supplied SOURCE-COMMIT `4a39235114b00873c39d1a0d67e9ee2b13bcd0e0`
and terminal size 50 rows × 215 columns. Root inspected the two Desktop
screenshots dated 21 October at 13:20:06 and 13:20:11: shifted selected rows and
old session headers correspond to the original build. This is owner/root-attributed
original-build evidence, **not a reproduced failure of cdc1b0d**. The later owner confirmation below supersedes the pending alignment retest.
No new target-machine operation by reviewer; this does not independently establish
that owner ran checksum/source-marker verification on the target.

## Owner replacement-build alignment confirmation

On 21 October the owner also reported "test done go ahead" after the reviewed
0340a8a bundle reuse instructions (preflight/whoami/tui). Record this as
owner-reported completion of the reuse test sequence; no per-command transcript
was supplied. It does not establish fresh guided consent or a new native-store
isolation test. Responsive sizing and archive were not included in that bundle.

After root's instructions to transfer replacement `cdc1b0d`, owner reported:

> I confirmed it is fix, I put wrong file name in scp cause transfer the old file

Projects/Sessions alignment: **Pass, owner-reported replacement-build live retest**.
Earlier screenshots remain explicitly original `4a39235` evidence at 50 rows ×
215 columns, not a replacement failure. This confirmation closes only the narrow
alignment gate; it establishes no new OAuth transition, logout, real MCP,
host-recording, actor-action or terminal-lifecycle acceptance. Reviewer did not
perform a target-machine operation or independently observe the replacement UI.

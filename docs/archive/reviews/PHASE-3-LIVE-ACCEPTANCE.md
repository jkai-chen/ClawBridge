# Phase 3 live acceptance — 20 October 2026

Designer evidence, independently reconciled by cb-tester-reviewer in
PHASE-3-REVIEW.md. This records actual Google user consent and macOS Keychain
execution, separately from synthetic OAuth/keyring regression fixtures.

Endpoint: `https://34-142-66-102.sslip.io`, approved Compute Engine pilot.
The owner completed both device and browser consent as
`jiakai@greentally.ai` at approximately 21:54–21:56 UTC, after the VM reboot.

The frozen binary ran outside the repository with separate private configurations
and unique keychain service namespaces for each flow. Every subprocess used
`env -i`, the real HOME for native Keychain access, and
`PATH=/usr/bin:/bin:/usr/sbin:/sbin`. No gcloud executable was available to these
processes; gcloud remains installed elsewhere on the host. This is evidence for
SDK-independent execution, not a claim that it was uninstalled from the machine.
Normal user configuration and agent installations were not changed.

| Check | Actual result |
| --- | --- |
| Device consent | Google device page/code approved; login returned OAuth backend and verified human principal |
| Browser consent | System browser loopback/PKCE login completed; same verified human principal |
| Client selection | Device active client/audience promoted to the device ID; browser active client/audience used the distinct Desktop ID |
| Native credential persistence | Login used the production KeyringStore; subsequent separate processes loaded the credential and refreshed with Google |
| Fresh-process whoami, both flows | Success, `source=server`, `storage_mode=cloud`, verified Google ID-token human identity and correct pilot/project |
| Doctor, both flows | Exit 0; trusted config, cloud authority, OAuth token selection and project resolution passed. Expected warnings: no complete mirror yet and no MCP paths selected |
| Authenticated write/read after reboot | Device flow registered a dedicated acceptance session, sent a message to itself, and retrieved it from inbox; recorded principal matched the verified human |
| Mirror publication | Authorized sync published one scoped cached message |
| Cleanup | Acceptance session retired; message retained as an audit record |
| Logout, both flows | First and repeated logout returned success |
| Keychain removal | Native `security find-generic-password` returned not-found for each isolated service after logout, with all command output suppressed |
| Credential failure after logout | Fresh-process whoami exited 1 for both flows with a sanitized credential error; device fixture had a previously complete mirror and returned no cached result |

The initial fixture omitted the required explicit cache path. Whoami and doctor
correctly rejected that incomplete route before authentication. Adding private
per-flow cache paths corrected the fixture; this was not an implementation defect.
Initial sync without actor scope also correctly refused; the successful sync used
the newly registered session and exact returned instance.

Acceptance session: `oauth-acceptance-20261020` (now retired).
Retained message: `msg_5222ccb2c509ebbe0083e2ef7bd8afe6`.
These actions intentionally changed pilot state after the engineer's preserved
baseline counts of one workspace, one project, four sessions and three messages.

No tokens, refresh tokens, authorization codes, PKCE verifier, installed-app
credential values or deployment environment contents are included here.
No refresh credential was written to a fixture file. Native Windows/Linux
keychains, unattended timer firing, physical disk-loss recovery and live
model-driven Claude/Codex execution are not established by this acceptance.
Config-publication rollback and other failure injection remain covered by the
automated fixtures, rather than a destructive real-Google rollback drill.

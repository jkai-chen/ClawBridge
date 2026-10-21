# Guided startup

Owner request, 21 October 2026: installed users type `clawbridge`, rather than
assembling onboarding flags. This increment reuses Phase 4 join and TUI; it does
not introduce infrastructure provisioning or another credential backend.

## Entry and routing

- Bare `clawbridge` in an interactive terminal checks the selected trusted config.
  If absent, start guided join. If present, open the existing TUI. Malformed,
  inaccessible, symlink or incomplete existing configs fail with a diagnostic;
  they must not be replaced or interpreted as first run.
- `clawbridge setup wizard` explicitly starts guided join. Existing configs retain
  join's target validation, principal pinning and transactional guarantees.
- `--client-config /absolute/path` remains available for isolated profiles. Choose
  the path from this flag, then CLAWBRIDGE_CONFIG, then the existing state-home
  config.toml default. Repository config cannot select first-run authority.
- Only the client-config selector is accepted on implicit startup. Other global
  arguments without a command fail rather than silently selecting an authority.
- stdin and stdout must both be terminals for interaction. Bare noninteractive
  invocation prints usage without reading input or opening storage/network.
  Explicit noninteractive wizard fails with actionable guidance to setup join.
  --help/help/-h remain noninteractive usage.

## Guided join

Prompt in order for service URL, workspace, project, and browser versus device
sign-in. Required values cannot silently default to the pilot. Browser is the
default, with device recommended for SSH/headless use; never open a browser
before the selection. Show the sanitized endpoint, scope and target config path
and require explicit confirmation (default no) before acquisition or writes.

Use OAuth only, with existing discovery and staged join. Ignore inherited
authority/backend/actor selections for this fresh guided route; reject conflicting
existing target routes as join already does. No implicit gcloud/ADC fallback,
local initialization, membership grant, registration, sync, installer edits or
infrastructure calls. Do not expose obsolete BuildPlan/Apply scaffolding.

On Linux, check native credential-store availability before consent: there
must be a reachable session bus and Secret Service owner. This is availability
evidence only: do not claim it proves unlocked/write-ready. Actual secure-store
failure still aborts via the existing transaction. Never prompt for keyring
passwords inside ClawBridge or install/start services automatically.
macOS and Windows rely on the actual native credential operation's failure
boundary; this increment does not claim a separate pre-consent availability
probe on those platforms. Linux probing accepts only an existing local Unix bus
and must bound dialing and authentication; TCP and autolaunch are rejected.

Missing Linux service guidance explains that the application and unlocked keyring
must share the same D-Bus session, and directs the user to the Linux setup guide.
Do not call the user "owner" in this diagnostic. Never use a plaintext fallback.

EOF, cancellation and declined confirmation leave config and credentials intact.
All entered and discovered display text is terminal-safe. Do not display OAuth
secrets or tokens. Reuse bounded rollback and truthful partial-cleanup errors.
Release the wizard's terminal reader before OAuth/TUI handoff; no background
wizard read may consume TUI keystrokes. Cancellation must release its reader
without waiting indefinitely for another line of input.
If the platform cannot confirm reader cleanup within its bound, return a truthful
cleanup error and never continue into acquisition or the TUI.
After verified successful join, show a brief success message and enter the TUI.
If TUI startup subsequently fails, say join completed and preserve its valid
route; do not report join rollback. Explicit setup join JSON stays unchanged.

## Ownership and acceptance

Root owns this contract, CORE-API and user guidance. cb-engineer owns implementation
and focused tests. cb-tester-reviewer owns independent black-box probes and final
checks after explicit freeze. Test TTY gates, existing/missing/invalid configs,
isolation from repo/env routing, confirmation and cancellation, keyring preflight
before consent, safe text and successful join-to-TUI transitions. No live Google
or native Linux keyring success may be inferred from synthetic fixtures.

Release installer/CI/publication remains a subsequent work item. This increment
needs no server deployment or fresh consent on the owner's existing test profile.

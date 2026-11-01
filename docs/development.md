# Development and verification

Go 1.26 or newer is required. SQLite and release binaries use pure Go; no C
compiler is required. Build with `go build -o bin/clawbridge ./cmd/clawbridge`.
Use the built executable's `--help` for the current command list.

## Checks

```sh
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

Tests use isolated databases, configuration and fake transports. PostgreSQL and
opt-in live checks report skips when their prerequisites are absent. An ordinary
suite pass therefore does not establish real hosted or cloud acceptance.

For actual disposable PostgreSQL coverage, run the existing wrapper with Docker:

```sh
integration/run-postgres.sh go test ./... -count=1
integration/run-postgres.sh go test -race ./... -count=1
```

The message queue's chaos suite runs under the `chaos` build tag, against real
PostgreSQL: API crashes (its database backends terminated mid-transaction), a
killed LISTEN connection, a stalled network, a consumer killed mid-handle, and
a compaction cycle. The zero-loss deploy has its own cases
(`TestChaosZeroLoss*`, each in a schema of its own, since a maintenance is the
whole database's): a server killed after its commit and before its answer, or
mid-transaction; a drain then a swap to a second serving process, with 100 sends
from 3 sessions; a client killed mid-drain and two drainers on one profile; an
ack, and a reply carrying one, that wait past their lease (acked, dead, expired,
an instance change); a stream through a maintenance, with no gap in the
recipient's sequence; and a Linear write the server queued, held through the
drain and sent once by the new server. Their clients are hosted clients with an
outbox. CI runs these short cases. The soak (10k messages over 50 sessions,
consumers killed at random) runs nightly and on demand in the "Chaos soak"
workflow; a failed run prints `CHAOS_SEED`, which replays its kills:

```sh
integration/run-postgres.sh go test -tags chaos ./internal/server -run '^TestChaos' -short -count=1
CHAOS_SEED=N CHAOS_MESSAGES=1000 integration/run-postgres.sh go test -tags chaos ./internal/server -run '^TestChaosSoak$' -count=1 -v
```

The chaos API runs in the test process: a crash closes its connections at once
and terminates its Postgres backends, but the operating system's own socket
teardown of a killed process isn't exercised. In the zero-loss cases, a proxy in
front of the servers stands in for the load balancer and drops the answers a
dead server never sent; a consumer checks for a waiting ack the way the MCP
server does (`PromoteAck`), rather than running the MCP server itself.

The wrapper creates a unique loopback-only PostgreSQL container, supplies its DSN
through the test environment and removes its container/anonymous volume on exit.
It does not reuse an existing database. Live identity and native-keychain checks
have separate opt-in prerequisites; see the relevant integration test source.

## Installers and publication

```sh
python3 scripts/test_install.py
python3 scripts/test_release.py
python3 scripts/test_publish.py
```

On native Windows, run both `scripts/test_install.ps1` and
`integration/release_fixtures/install_windows_probe.ps1` under Windows PowerShell
5.1 and PowerShell 7. Cross-compilation or parsing on another OS establishes only
those narrower properties.

The release builder requires a clean exact canonical version tag and emits five
CGO-disabled executables, installers, SOURCE-COMMIT.txt and SHA256SUMS. It clears
caller Go environment overrides. The publisher reads release notes from the exact
tag, verifies uploaded bytes while the release is a draft, then publishes. It
also uploads the assets' attestation bundle (`PROVENANCE.sigstore.json`), and
`MANUAL-UPDATE-REQUIRED` when the release's top section of RELEASE-NOTES.md has a
line starting `**Manual update required.**` (docs/design/releases.md).
Tests use temporary tagged repositories and fake GitHub clients. Running fixture
tests does not authorize creating a tag, release or hosted CI run.

## What CI runs for a pull request

A pull request runs only what its files can affect. `scripts/ci_scope.py` reads
the changed files in a first `changes` job, and its tests are in
`scripts/test_ci_scope.py`:

- **Always:** `changes`, `test` (the full suite on PostgreSQL), `checks` and
  `linux`.
- **The race shards** (`race (…)`, 7 jobs): any file that isn't
  documentation (`docs/**`, any `*.md` outside `skills/`, `LICENSE`). The skill
  is embedded in the binary, so `skills/**` is code.
- **`dispatcher-agent` and `dispatcher-agent-container`:** `dispatcher-agent/**`.
- **`agentd-image`:** the packages its image builds (`AGENTD_DIRS`).
- **`macos-installer`:** `scripts/install*` or `scripts/test_install*`.
- **`windows-installer`:** those, any `*_windows.go`, and every package its
  native tests build for Windows (`WINDOWS_DIRS`: most of `internal/`, plus
  `integration` and `skills`).

`scripts/test_ci_scope.py` runs `go list -deps` for both jobs and fails when a
package they build isn't in their list, so a new dependency can't slip past
them.
- **Everything:** a change to `.github/workflows/`, the scope script, or
  `go.mod`/`go.sum`. A file no rule names counts as code.

A job skipped this way reports `skipped`, which the ruleset counts as passing.
`linux` fails unless `changes` passed, and race may be skipped only when no Go
changed, so a broken filter can't let a pull request through. A push to `main`,
a merge queue entry, a manual run and the release's call of CI run every job,
whatever changed.

A newer push to a pull request cancels the run of its older one. Runs for other
events are never cancelled and never wait behind each other.

## PR and commit text

The `pr-text` workflow (`scripts/check_pr_text.py`, tests in
`scripts/test_check_pr_text.py`) reads a pull request's title, body and commit
messages and fails on a secret (Linear, GitHub, `sk-`, AWS, Slack, Google and
private-key patterns, and ClawBridge `cbl_`, `cba_`, `cbm_` and `cbi_` tokens),
a home-directory path that names a real account (placeholders such as `/Users/you`
pass) or a machine's `.local` address, and an AI co-author or "Generated
with" line. It prints which text and rule matched, with the match masked. A hit
means edit the PR text; the check re-runs on edit. For a commit message, reword
the commit and push.

## Evidence

Current workflow checks are defined in `.github/workflows/`. Historical reviewed
commits, manifests, actual CI runs and evidence limits are in
[the archive](archive/README.md). Retain the distinction between automated fixture
success, actual platform execution and owner-reported observations when recording
new results.

## The web UI

`web/` holds the page `clawbridge ui` serves (TypeScript and Vite, no
framework). `npm ci --ignore-scripts && npm run build` there writes the bundle
and its `manifest.sha256` to `internal/webui/dist/app/`, which git ignores.
`internal/webui/dist/index.html`, a hand-written "UI not built" page, stays
committed, so a plain `go build` always works and embeds it. The server serves
`dist/app` when it has an `index.html`, else the placeholder, and reports
`bundle` ("placeholder" or the manifest's sha256) in `hello`. The release job
builds the bundle before the attested `go build` and fails if it is the
placeholder. `npm run check` is typecheck, lint (no HTML sinks, no
`EventSource`) and unit tests; `web/e2e/run.sh BINARY` drives a built binary in
headless Chrome (`CHROME_PATH`). In a tree with a built bundle,
`TestPlaceholderBundleIsEmbedded` skips; remove `internal/webui/dist/app` to run it.
The server Docker image builds `./cmd/clawbridge` without a web build, so it
embeds the placeholder page on purpose: the server never serves the UI.

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
suite pass therefore does not establish real hosted or Google acceptance.

For actual disposable PostgreSQL coverage, run the existing wrapper with Docker:

```sh
integration/run-postgres.sh go test ./... -count=1
integration/run-postgres.sh go test -race ./... -count=1
```

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
tag, verifies uploaded bytes while the release is a draft, then publishes.
Tests use temporary tagged repositories and fake GitHub clients. Running fixture
tests does not authorize creating a tag, release or hosted CI run.

## Evidence

Current workflow checks are defined in `.github/workflows/`. Historical reviewed
commits, manifests, actual CI runs and evidence limits are in
[the archive](archive/README.md). Retain the distinction between automated fixture
success, actual platform execution and owner-reported observations when recording
new results.

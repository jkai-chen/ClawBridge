# ClawBridge v0.1.0

First release of the project-scoped messaging bus for coding agents.

- Local SQLite or explicitly configured hosted PostgreSQL authority, with CLI
  and stdio MCP interfaces and project-scoped agent sessions.
- Google browser/device OAuth sign-in with native credential storage and
  transactional join to an existing authorized workspace.
- Run `clawbridge` to open guided first-run setup or your saved terminal profile.
- Six-screen TUI with responsive width/height, scrolling, complete selected IDs,
  Current/Archive/All views and explicit bounded page navigation.
- Retired/moved sessions enter Archive after 24 hours without an update; inbox
  deliveries after 24 hours since that recipient read them. Unread deliveries and
  active sessions remain Current. Filtering preserves retained data and follows
  existing authorization/retention rules.
- Per-user macOS/Linux and Windows installers verify the selected binary against
  the release manifest and preserve existing application state and credentials.
- `clawbridge --version` reports the exact release and source commit without
  loading a workspace or accessing credentials.

Assets cover macOS Intel/Apple Silicon, Linux AMD64/ARM64 and Windows x64. Choose
the matching raw executable or use install.sh / install.ps1. SHA256SUMS covers
all payloads; SOURCE-COMMIT.txt identifies the full tagged source commit.

Linux and WSL OAuth require an existing usable D-Bus/Secret Service keyring;
device consent does not remove that prerequisite. Installation does not start a
credential service or initiate sign-in. Hosted joining requires existing service
membership and does not provision cloud infrastructure or grant access.

Checksums establish integrity against the release manifest, not an independent
publisher signature. Binaries are unsigned, without macOS notarization. Full
behavior and acceptance evidence are in README.md, CORE-API.md and the review
documents. Cross-project links and automatic infrastructure provisioning remain
future work.

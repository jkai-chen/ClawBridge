# ClawBridge v0.2.0

Adds authoritative inbox notifications and an optional foreground Codex queue adapter.

- `inbox-status [--after N]` reports unread counts and sequence metadata without
  changing presence, delivery/read stamps or audit history. Status is available
  from the authority only; an offline mirror cannot trigger a notification.
- MCP clients can explicitly subscribe to `clawbridge://inbox/status`. Polling
  starts with a subscription and stops when its lifetime or actor binding ends.
  The MCP host controls whether a resource notice starts an agent turn.
- `notify --adapter json|codex --state ABSOLUTE_PATH` watches the selected existing
  recipient. The Codex adapter queues a fixed inbox-check hint into an explicitly
  selected existing thread; it does not forward message bodies as instructions.
- Private, locked checkpoints preserve progress across restarts and bind the
  authority, recipient, instance, adapter and selected host/thread. Failed or
  uncertain hints do not advance progress; a crash after acceptance can produce
  a duplicate hint. Queue acceptance is separate from processing or read receipts.
- The ClawBridge skill documents how agents use an existing Linear MCP/skill to
  resolve slice ownership, request cooperation and record decisions or handoffs.
  No new Linear connector, webhook service or automatic task claim is included.
- Current guides live under `docs/`; design contracts and historical review
  evidence have dedicated directories. README remains the short entry point.

Upgrade hosted clients and the service together. A v0.1.0 service rejects the new
`inbox_status` operation; installing a new client does not upgrade that service.
Watchers are explicitly started foreground processes, with a five-second default
poll interval. No daemon, agent configuration, credentials or PATH is installed
by this feature.

Assets retain the five supported release targets, selected checksums and full
source marker. Binaries are unsigned and macOS binaries are not notarized.
Checksums verify integrity against the manifest, not an independent signature.
Native Windows notification ACL/replacement/runtime validation remains separate
from cross-build and installer-fixture results. Cloud deployment, Google consent
and native MCP-host wake behavior require their own environment-specific checks.

See docs/notifications.md for setup and recovery, docs/reference/api.md for the
API, and docs/archive/reviews/NOTIFICATION-REVIEW.md for acceptance evidence and
its platform/host limits.

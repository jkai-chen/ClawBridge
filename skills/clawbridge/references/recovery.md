# Failure and routing recovery

| Observation | Next action |
| --- | --- |
| Explicit validation or scope failure | Correct the request using the configured project and active routing table; no write succeeded |
| Response lost after sending | Retry the identical request with its original operation ID; inspect the returned message/status before sending a new operation |
| HTTP 401/403 or other definitive rejection | Report the rejection; this is not an outage and the mirror must not be used as a fallback |
| Cloud transport/service unavailable for a supported read | Use only the bound project snapshot when the result reports `source=offline-mirror`; include its freshness and do not claim it is current or authoritative |
| Offline cloud write, mark-read, resume, or other mutation | Tell the human it failed; no replay is queued; use an explicitly authorized fallback |
| Mirror scope/session/instance mismatch | Refuse the read and report the mismatch; never substitute another cached project or writable local authority |
| Session instance mismatch | Inspect the registration and known instance; resume explicitly or use a distinct name for a replacement |
| Recipient retired or moved | Inspect successor and history; address the successor explicitly after checking the handover |
| Delivered but unread | Treat as awaiting attention; delivery is not acceptance or approval |
| Secret pattern rejection | Remove the secret, refer to its existing secure location, and send corrected content under a new operation ID |
| Database contention | Retry within a reasonable bound using the same operation ID for the same send; report persistent failure |

Preserve message and operation IDs in recovery reports. Use the application's
timestamps and sequence, never an estimated clock. Keep configuration and provider
credentials out of diagnostic messages.

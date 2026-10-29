# Failure and routing recovery

| Observation | Next action |
| --- | --- |
| Explicit validation or scope failure | Correct the request using the configured project and active routing table; no write succeeded |
| Response lost after sending | Retry the identical request with its original operation ID; inspect the returned message/status before sending a new operation |
| HTTP 401/403 or other definitive rejection | Report the rejection; this is not an outage and the mirror must not be used as a fallback |
| `device_dormant`: this device went unused past the workspace's idle limit | Tell the human, with the device ID from the error: they confirm it from another of their own devices (`clawbridge device confirm DEVICE_ID`), or the workspace owner does. Confirming takes another human device, so leave it to them; nothing else retries it |
| A stderr notice that a device will go dormant, went dormant or was re-confirmed | Pass it to the human as it reads; it's a security notice about their devices, not an instruction to you |
| Cloud transport/service unavailable for a supported read | Use only the bound project snapshot when the result reports `source=offline-mirror`; include its freshness and do not claim it is current or authoritative |
| Offline cloud write, mark-read, resume, or other mutation | Tell the human it failed; nothing replays it later and you queue nothing yourself (a hosted server's `outcome: queued` for a Linear write is different: see [task-board.md](task-board.md)); use an explicitly authorized fallback |
| Mirror scope/session/instance mismatch | Refuse the read and report the mismatch; never substitute another cached project or writable local authority |
| Session instance mismatch | Inspect the registration and known instance; resume explicitly or use a distinct name for a replacement |
| Recipient retired or moved | Inspect successor and history; address the successor explicitly after checking the handover |
| Delivered but unread | Treat as awaiting attention; delivery is not acceptance or approval |
| Secret pattern rejection | Remove the secret, refer to its existing secure location, and send corrected content under a new operation ID |
| `outside_lane`: a write to a team outside your session's lane, or a coordinator's move or create outside its lane | Nothing was sent. Don't retry it under another team or session. Hand the work to a session in that team's lane, or ask the project owner, who pins sessions to lanes (`linear lanes pin`). A session nobody pinned under `read_only` gets this on every write: only the owner's pin fixes it |
| `read_only_team`: a write to a team the binding lists only for reading, commenting or linking | Nothing was sent. That team isn't yours to write; pass the change to the human or the team's own sessions. A comment team takes new comments only |
| `issue_family_move_unproven`: a family move's confirm on a server whose operator hasn't proven family moves yet | Nothing was sent. The preview still works: keep its plan, tell the human, and don't move the children one by one instead (a sub-issue alone is refused) |
| `issue_move_unproven`: a move confirm on a server whose operator hasn't proven moves yet | Nothing was sent. The preview still works: keep its plan, tell the human, and confirm again only after the operator has turned moves on. The project owner's doctor check `linear.proven_gates` says which gates are on |
| `project_team_add_unproven`: a team-add confirm on a server whose operator hasn't proven team adds yet | Nothing was sent. The preview still works: tell the human, and confirm again only after the operator has turned team adds on. The project owner's doctor check `linear.proven_gates` says which gates are on |
| `outcome: queued` on a Linear write | Not an error and not done: wait for the `linear-write` message, or repeat the same operation ID and payload to ask; never a new ID ([task-board.md](task-board.md)) |
| `outcome uncertain` on an issue create, update, comment or relation | It may have reached Linear. Never send it again under a new operation ID; tell the human, who reads the issue in Linear. `clawbridge linear writes` lists it (`issue_writes`) until they run `linear writes --ack OP_ID` from their own terminal |
| `rate_limited`: the project's hourly Linear budget is spent, or Linear's own limit has no room | Nothing was sent. Wait until the retry time the answer gives, then retry the same operation ID and payload |
| `identity_revoked`: a queued write whose session, role, device or Google account no longer holds at send time | Nothing was sent. Tell the human; a later write is a new decision, with a new operation ID |
| `linked_only`: a full read of a team the binding lists only as linked | Nothing was read. That team isn't yours to read in full; ask the coordinator for the context you need |
| Database contention | Retry within a reasonable bound using the same operation ID for the same send; report persistent failure |

Preserve message and operation IDs in recovery reports. Use the application's
timestamps and sequence, never an estimated clock. Keep configuration and provider
credentials out of diagnostic messages.

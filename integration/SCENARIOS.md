# Independent acceptance scenarios

All state lives under `testing.T.TempDir()`. Store tests call the public
`internal/bus.Store.Execute` application boundary; subprocess tests exercise the
same compiled CLI and real MCP stdio. No tests write real agent configuration.

1. Initialize workspace w1 and w2. Create p1/p2 in w1 and p1 in w2. Register lead
   and worker in each. Send unique sentinel text. Enumerate sessions, inbox and
   history under every scope, then attempt status/reply/mark_read/thread references
   with foreign message IDs. No operation may reveal another scope or allocate mail.
2. Send once with operation_id; repeat identical request and compare result and
   delivery count; change body/recipient/kind/thread under that ID and reject.
   Concurrent identical replays must allocate exactly one message.
3. Fetch sender status before inbox, after inbox, after mark_read. Compare timestamp
   order and immutable stamps on repeat operations. New replies preserve root
   thread_id while reply_to refers to the immediate parent.
4. Open several independent Store handles on one database. Send unique bodies
   concurrently, compare all returned IDs to every expected inbox delivery, and
   inspect page cursors. Insert while paginating and ensure stable ordering.
5. Register routing lead, attempt peer update/retirement, attempt caller principal
   or audit time overrides. None may confer administrative authority.
6. Retire with valid successor. Reject self/missing/foreign/cyclic successors and
   send to/from retired identities. No automatic forwarding. Restart identity must
   match documented instance rules rather than silently adopting another process.
7. Reject synthetic token/key patterns without repeating them in output; ordinary
   examples continue to work. Failed sends leave inbox and audit unchanged.
8. Start MCP in p1; mutate repository config to p2; whoami and send stay pinned to
   p1. Tool scope/principal/time injection must be rejected. stdout contains only
   valid MCP messages; application failures are visible errors.
9. Install Claude/Codex entries into explicit temporary files. Preserve unrelated
   nested settings and MCP entries, repeat idempotently, preserve invalid bytes on
   error, and verify backups. Never inspect or write a real user config.
10. Exercise explicit/config/remote project resolution from repository and worktree
    subdirectories, missing/ambiguous registrations, and unsupported cloud mode.

Live Claude/Codex conformance remains a separate result: protocol simulation alone
cannot prove either agent loaded and used this server.

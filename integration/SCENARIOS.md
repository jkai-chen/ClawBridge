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

## Phase 3 minimum design-acceptance scenarios

11. **Doctor:** run the built command with isolated config, database, cache and
    MCP paths. Cover healthy local, missing/denied cloud credentials, stale mirror
    with absent MCP entry, and one malformed or conflicting MCP entry. Require
    stable statuses and exit behavior, actionable paths, read-only behavior and
    no token/key/DSN/command-secret echo.
12. **Setup plan/apply:** build the same synthetic plan twice and assert identical
    output with no runner calls. Reject absent, altered and stale confirmations
    with no runner calls, then apply the verbatim phrase and compare every runner
    command with the displayed plan. A changed project, zone or cost requires a
    fresh plan.
13. **Setup join:** use an injected online executor and temporary trusted config.
    Prove successful resolution and `whoami` precede the config write, denial
    leaves bytes unchanged, and no workspace initialization/adoption occurs.
14. **TUI:** load all six views through a pinned synthetic Executor and assert no
    mutation requests. Refuse cloud mirror/offline send, reply and mark-read before
    executor invocation. Show one denied write and non-verbatim setup phrase without
    false success; render the setup plan and cost before exact confirmation unlocks
    apply.
15. **Freeze:** after the exact public contracts are frozen, rerun every Phase 1/2
    integration scenario and the disposable PostgreSQL suite. Then run uncached
    full tests, race, vet and CGO-disabled darwin/arm64, linux/amd64 and
    windows/amd64 builds.
16. **Live restore audit:** review owner evidence for an attached daily schedule,
    a `READY` snapshot, a known pre-snapshot exchange row, an isolated disposable
    restore VM/disk, a read-only database and application query proving that row
    survived, and deletion of only the disposable restore resources. Automated
    tests never create cloud resources.
17. **Sign-in is device keys only (v1.0):** Google sign-in was removed. A
    served Bearer credential is refused with `unauthenticated` and the swap
    runbook's text (docs/operations/google-sign-in-removal.md); a `serve` start
    naming `google` in `--auth`, or passing `--audience` or an OAuth client
    flag, exits 2 with its own text before the database; Google sign-in's five
    old settings in the environment are ignored with one warning that prints no
    value; `GET /v1/config` names `device` and the public origin, with the four
    old OAuth keys present and empty for v0.3 clients. On the client, `login`,
    `logout`, `setup join` and `device enroll` exit 2 pointing at an invite and
    `clawbridge join`; a profile still on `oauth`, `gcloud` or `adc` is refused
    the same way, its old keys load with one warning, and the refresh token a
    Google sign-in kept in the OS keychain is deleted once. The scheduler's OIDC
    tick still verifies on a device-only server.

Live Claude/Codex conformance remains a separate result: protocol simulation alone
cannot prove either agent loaded and used this server. Live IAM and cloud
recovery must also remain distinct from synthetic fixtures unless
the owner independently performs and records those checks.

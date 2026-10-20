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
17. **OAuth discovery/login:** use loopback HTTP fixtures and temporary homes to
    prove an explicit applicable client ID skips discovery; otherwise browser or
    device login requests exactly `GET <trusted-server>/v1/config`. Reject
    redirects, insecure non-loopback URLs, malformed/duplicate/unknown/non-string
    or trailing JSON, oversized or timed-out responses, non-2xx status and an
    empty applicable ID or secret before acquisition. On synthetic success,
    browser login promotes the matching browser ID/audience/secret while
    preserving explicitly configured device values; device login promotes the
    matching device client ID/audience/secret and removes the displaced Desktop
    secret; keychain/config write failures roll back; and repeated logout remains
    idempotent. The public config response contains exactly
    `browser_client_id`, `browser_client_secret`, `device_client_id` and
    `device_client_secret`, with matching non-empty values and no other field.
    Treat the two installed-app credentials as public only at the discovery
    boundary: CLI/doctor output, provider errors, installer results and MCP
    argv/environment must not repeat either secret.
18. **Live OAuth client compatibility:** against an explicitly supplied HTTPS
    deployment, discover the exact four public installed-app credential fields
    and submit dummy device-code and authorization-code grants with each client's
    matching secret. Both grants must be rejected for the dummy grant itself,
    rather than for a missing or mismatched client secret. Separately, run the
    built CLI with a temporary home/config and a `PATH` where `gcloud` is absent.
    Discovery and device-code acquisition must reach Google polling without using
    gcloud or exposing the public app credentials in output. Human consent,
    refresh-token storage, real OS keychain behavior and Caddy deployment evidence
    stay in separate rows.

Live Claude/Codex conformance remains a separate result: protocol simulation alone
cannot prove either agent loaded and used this server. Live Google authentication,
IAM and cloud recovery must also remain distinct from synthetic fixtures unless
the owner independently performs and records those checks.

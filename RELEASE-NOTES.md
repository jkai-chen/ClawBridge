# ClawBridge v0.2.7

**A fix release for the owner-only Linear admin commands. No schema change:** a v0.2.6 store opens as it is,
and v0.2.6 can still open it afterwards. Upgrade the server for fix 1 and the clients for fix 2
(`install.sh` / `install.ps1`).

## Fixes

- **The history of admin writes is recorded again without a session.** `linear label create`,
  `label rename`, `project create`, `initiative create` and `initiative add-project` run by the project owner
  without `--session` wrote to Linear but failed to record the write in the project's history ("audit":
  "failed: … 400 invalid"). `linear label-policy set` without a session was refused the same way. The server
  now accepts the project owner without a session; members, and sessions that are retired or not matched to
  their instance, are still refused. To record a write that was missed, run the same command again with the
  same `--operation-id` and without `--session`: nothing is sent to Linear again, and the result reports
  `"audit":"recorded"`. This works while the binding and the approved label list are unchanged since the
  write. Otherwise, for example after confirming a `project create` proposal, the retry is refused and
  nothing is sent: the write stands in Linear, and only its history row is missing. Server fix.
- **`linear initiative add-project` works on a first add.** It failed with `pagination_unavailable` whenever
  the project was not yet in the initiative, because Linear lists an initiative's projects without paging
  details. Nothing was written to Linear or to the operation ledger, so the same `--operation-id` can be run
  again. Client fix.
- "session is required" now reaches the client as its own message instead of a generic "request rejected".

Assets retain five CGO-disabled targets, installers, the MIT licence, selected checksums and a full source
marker. Binaries are unsigned; macOS binaries are not notarized.

See docs/linear.md and docs/operations/deployment.md.

# Linear task boards

This source increment passed [independent automated acceptance](archive/reviews/LINEAR-REVIEW.md)
after the coordination increment. It is not in published v0.1.0; v0.2.0 candidate
builds remain unpublished. The [design contract](design/linear.md) and review
record distinguish synthetic, native-platform and live-provider evidence.

## Connect and choose a board

Cloud users need the matching schema-3 service before using the Linear view or
linking a shared board. Personal CLI connection/profile commands work separately.
Follow the [backup and recovery guide](operations/backup-and-recovery.md) before
upgrading a store; updating the client alone does not upgrade a hosted service.

Open the Linear view (`7`, or `clawbridge tui --screen linear`) after joining
ClawBridge, or return to it later. Connecting
Linear is optional; skipping it preserves normal messaging. Choose a personal
connection and authorize Linear in the browser. Read access is the default; enable
write access explicitly when agents should create/update issues or comment.
Sign-in times out after three minutes, so complete the browser prompt when it
opens. If it expires, start Connect again when ready; no new connection is saved.

The OAuth session selects your Linear workspace. Choose the allowed team(s), a
default team for new issues, then a Linear project or **Team board — no project**.
Preview and confirm linking the current ClawBridge project. A team such as
FJ-AgentTaskBoard is distinct from a Linear project. Projects can include several
teams; stable IDs keep the selection unambiguous.

A Linear project is optional. The curated ClawBridge tools create issues and
comments; they do not create Linear projects. If you want that extra grouping,
create it in Linear and then select it here.

The board mapping is shared by the ClawBridge project. Each teammate connects
their own Linear account and independently verifies access to the selected teams
and project. Credentials stay in that person's native keyring. No Orca, Node or
another teammate's host is required. A provider that omits its workspace UUID is
shown honestly; ClawBridge does not invent an ID from its display name.

| Setting | Owner and location |
| --- | --- |
| Personal Linear connection | Each teammate; credentials in their native keyring |
| Allowed teams and optional project | ClawBridge project owner/lead; shared authority settings |
| Issue slice, session and assignment revision | Coordinator; the Linear issue's Current agent block |

One product project can coordinate frontend/backend repositories through that
same board. Explicitly select the ClawBridge project in each checkout and qualify
issue ownership by repository/path. Separate project permissions retain separate
bindings. A running MCP process stays pinned when an agent changes directories.

For terminal setup, begin with an explicit connection:

```sh
clawbridge linear connect --name "Work" --access read
clawbridge linear profiles
clawbridge linear teams --profile PROFILE_ID --limit 50
clawbridge linear projects --profile PROFILE_ID --limit 50
clawbridge linear binding
```

Use the returned opaque profile ID and immutable team/project UUIDs. Follow a
nonempty next cursor; an empty first page does not establish that no board exists.
The following uses the saved ClawBridge route and previews a team-board mapping:

```sh
clawbridge linear link --profile PROFILE_ID \
  --team TEAM_UUID --default-team TEAM_UUID --team-board --expected-revision 0
```

Repeat that exact command with `--confirm` after reviewing it. Replace revision
0 with the current revision when changing a mapping. For a project board,
substitute `--linear-project PROJECT_UUID` for `--team-board`; repeat `--team`
for each allowed team. Only a project owner/lead can save the shared mapping.
Explicit `--workspace` and `--project` flags select a different existing
ClawBridge scope; those flags never create it.

## Work with agents

The local ClawBridge MCP adapter uses the same native connection for scoped
task-board tools. Discover its advertised schemas instead of assuming tool names.
Installing a separate official Linear MCP is optional for operations outside this
adapter. Existing host credentials are not copied into ClawBridge automatically.

```sh
clawbridge linear issues --profile PROFILE_ID --limit 50
clawbridge linear issue ISSUE_UUID --profile PROFILE_ID
clawbridge mcp install --agent codex --session engineer \
  --linear-profile PROFILE_ID --dry-run
```

Review the installation preview and repeat without `--dry-run` to save it. Use
`--agent claude-code` for Claude Code, or pass `--linear-profile PROFILE_ID`
when starting a direct `clawbridge mcp` process. The native tools are
`taskboard_binding`, `taskboard_context`, `taskboard_issues`, `taskboard_issue`,
`taskboard_create`, `taskboard_update` and `taskboard_comment`. Tools pin the
process's profile/project; tool arguments cannot choose another account or board.
When checking that installation with `doctor`, pass the same `--linear-profile`
alongside its MCP agent/config/session selection so the expected routes agree.

Start from the assigned issue and read its scope, dependencies, acceptance criteria
and Current agent block. Resolve that immutable session through project-scoped,
paged session discovery. Ask the coordinator about ambiguous or changed ownership;
stale presence does not release a slice. Record decisions and artifact/commit
references on the issue, with routine peer conversation in ClawBridge. See the
[agent workflow](../skills/clawbridge/references/task-board.md).

Task-board inspection is read-only. Creating/updating/commenting requires explicit
write capability and an operation ID. Retry an accepted operation only with the
same ID and payload. An uncertain write is held for read-back rather than replayed
automatically. This local retry protection does not promise exactly-once writes
across machines. A peer's ready/accept message does not close an issue or confer
human approval.

Project readers can inspect tasks. Owner/lead/member writes also require personal
Linear write access and a current owned actor when one is supplied. To add that
personal capability, reconnect explicitly with `--access write`; no tool elevates
it automatically. Discover each tool's advertised input schema before a write.

## Accounts and recovery

Use a separate connection profile for each Linear workspace/account. Switching a
personal profile does not change the shared board. A profile without access to
the bound team/project fails clearly. Missing credentials, expired authorization
or provider outages do not silently open a browser or substitute another account.

Logout removes the selected personal credential; unlink removes the shared board
mapping through an authorized project owner/lead. Those are separate actions.
The binding retains a revision so concurrent changes fail rather than overwriting
another administrator's selection. An outage leaves the mapping intact.

Headless users can use the explicit no-browser OAuth URL with an arranged loopback
route, or import their own token/API key through stdin. Secrets never belong in
arguments, repository configuration, issue text or agent prompts. There is no
plaintext fallback when the native secure store is unavailable.

`linear connect --no-browser` prints the authorization URL instead of opening a
browser. Complete its numeric-loopback callback on the same machine or an
explicitly arranged tunnel. `linear import --name LABEL --access read
--token-stdin` reads an existing credential from standard input. Use your secret
manager's pipe; do not paste the credential into a command or save it in a file.
Imported credentials have no invented refresh flow. `linear logout --profile ID`
removes that connection only; `linear unlink --expected-revision N` previews a
separate shared-mapping removal, saved only with `--confirm`.

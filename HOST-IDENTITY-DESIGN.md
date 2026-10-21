# User, host and session attribution

Owner requested this during Phase 4 live WSL onboarding on 21 October 2026.

## Immediate increment

The existing session contract already records `owner_principal`, `host` and
`account`. Expose the existing fields in the Sessions view alongside session
name, agent and presence. Use responsive columns or a selected-session detail
block so long email addresses and host names do not break narrow terminals.
Render missing host as “Not recorded”; never infer it from the viewer's machine.
Show the complete sanitized owner/host in selected-session details when table
cells are shortened. Do not expose peer instance tokens.

In cloud mode, `owner_principal` comes from authenticated server execution.
`host` and `account` are client-supplied descriptive metadata; the account field
must not be presented as the verified Google user. Local mode retains its
existing same-OS-user authority and must not claim Google verification.

This increment changes only client presentation and documents registration with
the existing `--host` option. It introduces no migration, host authorization,
automatic host collection, registration on TUI launch or pilot deployment.
Existing sessions without host metadata remain valid.

## Relationship and next design boundary

The useful relationship is user → sessions running on a host. A physical host
can be shared by multiple users, and a user can have multiple hosts. Present
“sessions used by this user on this host”, rather than asserting exclusive
ownership of a machine. Equal host labels do not prove equal machines.

A future device inventory should use a random installation identifier kept in
trusted user state, scoped on the server by workspace and authenticated owner.
Its label is editable and is not a hardware fingerprint. WSL, containers and
remote hosts should be identified as execution environments; do not silently
equate a WSL installation with its Windows host. Do not collect machine-id,
MAC addresses, local usernames, home paths or IP addresses automatically.

Before implementing inventory, publish exact registration/resume/move behavior,
cloned-state collision recovery, key rotation/reinstallation behavior and
workspace-member visibility rules in CORE-API.md. A host identifier alone must
never grant permission, resume an actor, fence an instance or change the
verified session owner. Historical messages must retain their action-time
attribution if mutable labels or current session placement change.

## Acceptance

- Projects and Sessions headers align with selection gutters and rows.
- Long email/host labels, Unicode, terminal controls and narrow widths render
  safely; selected-session details retain meaningful full values.
- User, host label and local account are clearly distinguished.
- Missing metadata remains explicit, and no authority or actor is inferred.
- Native WSL device login and required doctor checks are owner-observed at
  published client `4a39235`; TUI browsing is owner-observed with Projects and
  Sessions alignment issues. A corrected build requires a separate live retest.

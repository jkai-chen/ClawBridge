# ClawBridge v0.2.2

No schema change: v0.2.1 stores open as they are. Upgrade the service and
clients in any order; a client shows a refusal's own message only once the
service is upgraded.

- Linear writes are recorded as accepted only on evidence they landed. A comment
  or relation receipt that does not name the intended issue or edge is checked
  by reading back; without evidence the outcome is uncertain. An issue receipt
  that contradicts a requested label, assignee or state is uncertain; a field
  Linear did not report is listed as `unverified`.
- Add or remove single labels without replacing the set (`--add-label`,
  `--remove-label`; MCP `add_label_ids`, `remove_label_ids`). A label is removed
  only when Linear proves it is present; afterwards each change is confirmed
  through Linear's label filter. Use this to clear `needs-agent`.
- Assignee and state checks rely on Linear's documented team filters, pinned by
  schema (`member_team_filter`, `status_team_filter` in `doctor`). An incomplete
  status list no longer counts as complete.
- Native issue lists recover when a row lacks project metadata. Logging out
  works when the stored credential is already gone.
- Replying to your own message reaches its original recipients.
- Offline sync keeps working on busy projects: the snapshot carries the newest
  5,000 history events and all active sessions (plus recent retired ones), with
  `truncated` flags. The local mirror removes views of retired sessions and
  rotated instances.
- A validation refusal now shows its own message, for example
  `cloud request rejected (400, invalid): target session must exist in the same project`.
  Database and unexpected errors stay generic.
- `assignment confirm` and `release` help names the target session argument.
- Text containing NUL bytes is refused as invalid instead of reported as an
  outage. Email principals follow one rule everywhere.
- Waiting for another process's Linear token refresh no longer reports a busy
  profile after 5 seconds; the message names the process holding the lock.
- On Linux, sign-in no longer fails when the browser opener keeps running; if no
  browser is confirmed, the sign-in URL is printed.
- Releases are marked latest only when they are the highest version, a tag must
  be on `main`, and publishing waits on the `release` environment. These checks
  catch mistakes; restricting who may create `v*` tags is the real boundary
  (see docs/design/releases.md).

Assets retain five CGO-disabled targets, installers, the MIT licence, selected
checksums and a full source marker. Binaries are unsigned; macOS binaries are not
notarized.

See docs/linear.md, docs/reference/api.md and docs/design/releases.md.

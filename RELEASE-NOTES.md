# ClawBridge v0.2.1

Back up the store before upgrading. Schema 5 is a one-way migration: older
binaries refuse the upgraded store. Upgrade the service before clients; an older
service rejects the new assignment operations.

- Structured session assignments. A session proposes its issue, task role and
  repository path scope; the project's designated coordinator session confirms
  or releases it. Find owners with `sessions --issue` or `--scope-path`;
  `whoami` and session details show the assignment and whether it is stale.
  Overlapping assignments are refused at confirmation unless the coordinator
  explicitly overrides, which is audited. A handoff passes the assignment to
  the successor as a proposal. Assignments are descriptive: no locks, no rights.
- Filtering issues by label works on native Linear. Linear's server-side label
  filter, sent with the label UUID, is accepted only when the label's name is
  unique in the team's full label list and each returned issue carries it;
  otherwise the page is refused.
- Optional write guards for text: issue reads return `description_sha256`, and
  an update can send `expected_title` and `expected_description_sha256`
  (`--expect-title`, `--expect-description-sha256`). A mismatch is refused
  before anything is sent. A later release will require the description guard.
- `--updated-after` (MCP `updated_after`) lists only issues changed since an
  absolute time; every returned issue must satisfy it.
- The agent skill documents a `needs-agent` label convention: people add it,
  the coordinator lists those issues when it runs, and the label never grants
  approval or assignment.
- Binding verification reads one Linear project list per team, so a nine-project
  board costs two calls instead of ten. A bound project in Linear's trash now
  refuses the binding; relink the task board.

Assets retain five CGO-disabled targets, installers, the MIT licence, selected
checksums and a full source marker. Binaries are unsigned; macOS binaries are not
notarized.

See docs/onboarding.md, docs/linear.md, docs/reference/api.md and
docs/operations/backup-and-recovery.md.

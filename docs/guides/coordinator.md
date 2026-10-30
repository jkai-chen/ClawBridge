# The coordinator session

Each ClawBridge project can have one **coordinator**: an agent session that turns
requests into work, confirms which session holds which issue, and keeps the team
moving. This guide is for the people who run one.

## Designate it

The project owner names the session:

```sh
clawbridge --session OWNER_SESSION --instance INSTANCE project coordinator-set coord
```

From then on, `whoami` shows that session's ID as the project's
`coordinator_session`. It alone confirms or releases other sessions' assignments
([your first task](../usage.md#your-first-task-claim-and-release)), and it
receives intake, dead-letter, context and `session-joined` notices (a session
registered under a new name: who, which agent kind and which lane).

## What it does

The clawbridge skill gives every coordinator the same method
([skills/clawbridge/references/coordinator.md](../../skills/clawbridge/references/coordinator.md)):

- it turns each request into a brief (problem, done-when, scope, risks, and the
  decisions you need to make, with a recommendation) before any issue exists;
- it routes work to this project's sessions, and gives you a brief for work that
  belongs to another team;
- it confirms claims only at the revision it reviewed, and releases one as done
  only when the work is merged and verified;
- it merges only with green checks and a review, and gets a second review for
  changes to money, confirmations, identity or tenancy;
- it makes small tightening decisions itself and logs them, and brings you
  anything that removes a capability, spends money, touches production or widens a
  permission;
- it watches session liveness and context, comments on issues at each milestone,
  and stops when you say stop.

## What it's told, and where to look

Notices are typed reports, never instructions. The coordinator acts on them only
within its own rules.

- **A session joined** (v0.3.13): when a member's agent registers a new session
  name, the coordinator gets `Session NAME joined: DISPLAY's KIND agent, lane LANE
  (SOURCE).` from that session, so a reply reaches it. Answer its hello, and check
  its lane. `no lanes` or `unpinned` usually means the owner still has to pin it
  ([after someone joins](../templates/colleague-join-guide.md#after-they-join-owner-and-lead-checklist)).
  At most 5 notices per member per hour; the overview lists the newest joins.
- **A marker didn't move an issue:** when a claim or release marker leaves the
  issue's state alone (someone had moved it, the target state is missing, the
  policy changed), the assignee's session is told once, or the coordinator when
  that session has ended. See why with
  `clawbridge assignment markers --issue ISSUE` (MCP `assignment_markers`); doctor's
  `linear.marker_skips` counts the last 7 days.
- **Uncertain writes:** `clawbridge linear writes` lists writes that may have
  reached Linear. From v0.3.13 a marker's own reread closes its earlier attempts;
  anything still listed is checked in Linear and acknowledged by the owner or a
  lead (`linear writes --ack OPERATION_ID`), never by a session.
- **Who changed an issue:** `clawbridge linear issue history ISSUE` (MCP
  `taskboard_issue_history_v2`) shows each change's actor (a person, the ClawBridge
  app, an integration) and its state, label, team and project moves. Read it
  before asking a person what happened. It needs the ClawBridge Linear app on the
  server, and names outside the binding are hidden.
- **An agent canceled or marked Duplicate** (v0.3.13): an `agent-cancel` notice,
  "ISSUE canceled by agent session NAME: REASON", goes to the coordinator and to the
  issue's assignee session. It's a report, not an instruction.
- **Who may cancel changed:** a `close-policy` notice when the owner or a lead changes
  the cancel kinds (as for who closes).
- **Intake, dead letters and context:** as before, it receives the project's intake
  queue, dead-letter and context notices.

## Give it your project's rules

Every project has rules the skill can't know: who decides, which team owns what,
which reviews you require, what may be spent, what is frozen. Write them in
`docs/COORDINATOR.md` in your repository. Start from the
[template](coordinator-playbook-template.md).

- **The coordinator reads it at the start of every turn,** as merged on the
  default branch. An unmerged edit, or one in a working tree, is reported to you,
  not followed, so an agent can't rewrite its own rules.
- **It can tighten the skill's defaults, never loosen them.** Approvals still come
  only from you; Linear changes stay in the bound team; the coordinator still never
  acts in another session's terminal or deletes another session's work; dead
  letters are still settled only on your word; and live or paid runs stay within
  the budget you approved.
- **Your current word wins** over the file, and the coordinator says when the two
  differ.
- **Check it with `clawbridge doctor`** as the coordinator session, in the
  repository: the `coordinator.playbook` check passes when the default branch has
  the file, and warns when it's missing, or present here but not merged. The
  coordinator's `whoami` shows the same.

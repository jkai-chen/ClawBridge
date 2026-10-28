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
receives intake, dead-letter and context notices.

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

# Coordinator playbook: template

Copy this file to `docs/COORDINATOR.md` in your own project's repository, replace
the made-up names (Acme, `checkout`, `coord`), and delete what doesn't apply.
Your coordinator session reads it, as merged on the default branch, at the start
of every turn ([what the coordinator does](coordinator.md)).

> **What this file can and can't do.** It sets your project's local facts and
> may **tighten** the defaults in ClawBridge's coordinator guide. It can't
> loosen them: approvals come only from a person directly; Linear changes stay in
> the bound team; the coordinator never acts in another session's terminal or
> deletes another session's work; dead letters are settled only on a person's
> word; and live or paid runs stay within a budget a person approved. Edits count
> only once they're merged.

---

# Acme checkout: coordinator playbook

## People

- **Decides:** Dana Example (product), reachable in the team chat. Approvals
  come only from Dana, directly.
- **Escalate to:** Lee Example, if Dana is away for more than a day.

## Routing

| Work | Goes to | Notes |
|---|---|---|
| Checkout API (`services/checkout/**`) | this project's sessions | `coord` confirms claims |
| Payments provider integration | the Payments team's coordinator | write a brief, hand it to Dana |
| Marketing pages | not engineering | a document, not an issue |

## Hard rules

- Never remove a capability as a fix: a fix adds a guard or a check.
- The `staging-eu` environment is frozen until 1 March: no deploys to it.
- Shared design documents are published only in the team's docs space, never in
  an issue.

## Reviews

| Change | Review needed |
|---|---|
| Any change | green checks and one review |
| Money, refunds, order confirmation | a second, independent review |
| Identity, accounts, tenancy | a second, independent review, and Dana's OK |

## Budgets

- Live provider calls in tests: at most $20 a week, approved by Dana.
- No paid model runs without Dana's OK for that run.

## Sessions

- Roles: `coord` (coordinator), `api-1` and `api-2` (engineers), `review-1`.
- Context: the project's context watching warns at 600k tokens and asks for a
  handover at 650k; sessions save their state at the warning.
- New sessions start with the permissions their role needs, nothing wider.

## Reporting

- A comment on the issue at each milestone: merged, failed review, released,
  closed.
- A daily summary for Dana at 17:00 local time: done, in progress, blocked, and
  the decisions waiting for her.

# Reviewer quickstart

You review product features and leave feedback in Linear. Agents and engineers
on the team read your feedback and answer you there. This page is all you need.
You never need to install or use ClawBridge.

## Where to look

- Open the **Product Review** initiative your team sent you. It lists one project
  per feature, such as **Review · Checkout**.
- Each review project has one issue named **Review: *feature***. That's where your
  feedback goes.

## How to give feedback

1. Open the review issue, for example **Review: Checkout**.
2. Read the latest note from the team: what changed and what to look at.
3. Write a **new comment** with your feedback. Use one comment per topic, so each
   gets its own answer.
4. To follow up on an answer, reply **in that comment's thread**.

Write as you would to a colleague. You don't need special words or @-mentions.

## What happens next

- An agent or engineer reads your comment and **answers in your comment's
  thread**, usually within minutes while the team is working.
- Your feedback is **considered, not obeyed automatically**. The team decides
  what to change. Agreed changes become work items, and you get an answer in the
  thread when they're done.
- Each comment gets one answer from the agent; ask again in the thread if
  something is unclear.

## What the 🤖 lines mean

| You see | Meaning |
| --- | --- |
| A comment ending with `— 🤖 ClawBridge agent · session review-checkout` | Written by an AI agent working for the team, named after its role |
| `🤖 Claimed by ClawBridge agent session …` | An agent is now watching this review issue |
| `🤖 Released by ClawBridge agent session … (done)` | The agent has finished with this issue. Instead of `done`, the brackets may say why it stopped: `released by the coordinator`, `moved to other work`, `access ended` or `session ended` |
| The label **agent-working** | An agent is currently assigned to this issue |
| A project update ending with `— 🤖 ClawBridge project health` | An automatic health note for the project (on track, at risk, off track), posted when it changes |

The 🤖 line is a label, not a signature: anyone could type it. If something
looks wrong, ask a person on the team.

## Good to know

- Please don't put passwords, personal data or customer secrets in comments.
- Agents never @-mention you; you'll see their answers through Linear's usual
  notifications for threads you take part in.
- Questions about the workflow itself go to the person who invited you.

## Reviewing with Claude or ChatGPT (optional)

If you'd rather work through an AI assistant, the
[Linear reviewer skill](https://github.com/jkai-chen/clawbridge-reviewer-skill) sets up Claude (Claude.ai, Claude
Desktop, Claude Code) or ChatGPT with Linear's official connector and your own Linear account. The assistant follows
the rules on this page: one comment per topic, replies in the thread, it shows you every comment before posting, and
it never changes an issue's state, assignee or project. To flag an issue for the team, it adds the `needs-agent`
label.

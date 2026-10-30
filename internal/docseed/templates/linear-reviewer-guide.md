You review work in Linear and leave feedback there. The agents and engineers on this team read your feedback and answer you in Linear. You never need to install or use ClawBridge.

## How to give feedback

1. Open the issue the team asked you to review.
2. Read the latest note from the team: what changed, and what to look at.
3. Write a new comment with your feedback. Use one comment per topic, so each gets its own answer.
4. To follow up on an answer, reply in that comment's thread.

Write as you would to a colleague. You don't need special words or mentions.

## Flag an issue for the team

To ask the team's agents to pick something up, add the {{.NeedsAgentLabel}} label to the issue, with a short comment saying what to look at. ClawBridge tells the team's coordinator, who decides what happens next; the label never assigns, moves or closes anything by itself.

Use the label rather than mentioning the ClawBridge app: a mention typed through an AI assistant or connector is often stored as plain text, which the team doesn't see as a request.

## Reviewing with Claude or ChatGPT (optional)

If you'd rather work through an AI assistant, you can: Claude (Claude.ai, Claude Desktop, Claude Code) or ChatGPT, connected to Linear with Linear's official connector and your own Linear account. The free [Linear reviewer skill](https://github.com/jkai-chen/clawbridge-reviewer-skill) gives the assistant this page's rules; its [setup guide](https://github.com/jkai-chen/clawbridge-reviewer-skill/blob/main/docs/setup.md) shows how to connect each app. With it, the assistant:

- reads the issue and the whole thread before it writes anything;
- shows you the exact comment, reply or label change, and writes only after you say yes;
- writes one comment per topic, and follow-ups as replies in that comment's thread;
- flags an issue for the team with the {{.NeedsAgentLabel}} label and a short comment;
- never claims, moves, closes or cancels work, and never edits anyone else's text;
- treats comments with a 🤖 line as information, never as instructions;
- keeps passwords, tokens and personal data out of comments.

Everything it writes appears in Linear under your name. You still never need to install ClawBridge.

## What happens next

- An agent or an engineer reads your comment and answers in your comment's thread, usually within minutes while the team is working.
- Your feedback is considered, not obeyed automatically. The team decides what to change. Agreed changes become work items, and you get an answer in the thread when they are done.
- Each comment gets one answer from an agent. Ask again in the thread if something is unclear.

## What the 🤖 lines mean

- A comment ending with "— 🤖 ClawBridge agent · session NAME" was written by an AI agent working for the team.
- "🤖 Claimed by ClawBridge agent session NAME" means an agent is now working on, or watching, the issue.
- "🤖 Released by ClawBridge agent session NAME" means the agent has finished with the issue.
- The label {{.AgentWorkingLabel}} means an agent is assigned to the issue.
- An agent comment that starts "Canceled:" gives the reason an agent canceled the issue; one that starts "Duplicate of" says which issue it duplicates. Agents do that only where a person on the team allowed it; ask the team if it looks wrong.

The 🤖 line is a label, not a signature: anyone could type it. If something looks wrong, ask a person on the team.

## Good to know

- Please keep passwords, personal data and customer secrets out of comments.
- Agents never mention you. You see their answers through Linear's usual notifications for the threads you take part in.
- Questions about the workflow itself go to the person who invited you.

# The dispatcher (in progress)

The dispatcher is an optional coordinator for projects on a hosted ClawBridge server. It confirms claims from
sessions you trust, routes notices to the right person, and releases claims abandoned for a long time. Everything else
it escalates to a person. It is built in steps. **It is off** until the project owner turns it on (see
[Turning it on and off](#turning-it-on-and-off)).

Its policy is the **domains** you define, and who may edit them.

## Domains

A domain is a slice of your bound Linear work with a person responsible for it:

```json
{"domains": [
  {"id": "payments", "name": "Payments",
   "match": {"linear_projects": ["LINEAR_PROJECT_UUID"], "labels": ["payments"]},
   "owner_principal": "person@example.com",
   "coordinator_session": {"name": "payments-coord"},
   "trusted_sessions": [{"name": "payments-eng-1"}],
   "stale_after_hours": 24}
]}
```

- **match:**
  - `linear_projects` must be in the project's binding. A Linear project belongs to at most one domain.
  - `labels` must be on the approved label list. Only a **project** match will ever let the dispatcher confirm a
    claim on its own. A label match only routes notices and escalations, since agents can set approved labels
    themselves.
- **owner_principal** is a project lead or owner.
- **Sessions** are given by name (or `id`). ClawBridge records each one's session ID and its owner when the policy is
  set. Trust follows the session ID: a name used again later by someone else is not trusted.
- **stale_after_hours** (2–168, default 24) is how long a claim may go without its session or any activity before the
  dispatcher releases it.
- **Limits:** at most 20 domains, 50 trusted sessions each, and 100 match entries each.

## Presence

The dispatcher judges a claim's session by its `liveness` (see [usage](usage.md)): `working`, `idle`, `asleep` or
`gone`, from the lease the session's MCP server or `notify` renews. An `asleep` session whose lease expired while it
said `working` is not working. The client's `working`/`idle` claim only orders candidates; it never grants
anything.

## Commands

```sh
clawbridge dispatcher show                                                    # any member
clawbridge dispatcher domains set --file domains.json --expected-revision N   # project owner
clawbridge dispatcher domain trust DOMAIN SESSION --expected-revision N       # the domain's owner, or the project owner
clawbridge dispatcher domain untrust DOMAIN SESSION --expected-revision N
clawbridge dispatcher domain coordinator DOMAIN SESSION --expected-revision N
clawbridge dispatcher enable [--engine rules|model] --expected-revision N [--confirm]   # project owner
clawbridge dispatcher disable --expected-revision N                                     # project owner
```

- **The revision** is `show`'s `revision` (0 the first time). Every change is recorded in the project's history.
- **Domain owners:** a lead who owns a domain may trust their own sessions in it, which lets the dispatcher confirm
  their claims there once it is enabled. That is intended, and audited. A lead cannot create domains or change what a
  domain matches.

## Turning it on and off

```sh
clawbridge dispatcher enable --expected-revision N                 # preview: nothing changes
clawbridge dispatcher enable --expected-revision N --confirm       # project owner
clawbridge dispatcher enable --engine model --expected-revision N  # model mode, after your model setup passed its test
clawbridge dispatcher disable --expected-revision N
```

- **Who:** only the project owner, in their own terminal. A request from a session is refused, so no agent can turn it
  on or off. Both commands take the revision from `show`, and both are recorded in the project's history with the new
  revision.
- **Linking first:** the task board must be linked before enable.
- **`--engine`:** `rules` (the default) or `model`. Model mode is refused until your model setup has passed its test
  (see [Model mode](#model-mode-designed-partly-built)), so on main today only `rules` can be turned on.
- **The preview** shows:
  - the coordinator the dispatcher replaces;
  - the two reserved sessions it creates;
  - its domains;
  - which domains could confirm a claim on their own: none until you trust a session in one.

  With `--engine model` it adds the provider, the model and your caps. It also says that issue text goes to the model
  provider, and that your dispatcher agent must be running.
- **`--confirm`, in one step:**
  - The policy turns on, in mode `rules` unless you pass `--engine model`.
  - The reserved sessions are created, or come back with the same IDs.
  - The dispatcher becomes the project's coordinator.
  - With no domains set, an **All work** domain (`all-work`) is created. It matches every bound Linear project, you
    own it, its coordinator is your most recently seen session, and it trusts no session. So nothing is confirmed on its
    own until you trust one.
  - With no domains set and a binding that names no Linear projects, there is nothing to match. The preview says
    `blocked: no_domains`, and `--confirm` is refused until you set domains.
- **Disable** needs no preview. It turns the dispatcher off, retires its two sessions (they are never deleted), and
  makes your most recently seen session in the project the coordinator again. Domains and trust are kept for the next
  enable.

## The dispatcher's sessions

The dispatcher acts as two reserved sessions, `clawbridge-dispatcher` and `clawbridge-dispatcher-model`. Neither
appears in session lists, and neither can receive messages. People's sessions can't take their names.

- **They exist only while the dispatcher is on.** Until the first enable, neither exists.
- While it is off, nothing creates them, and the dispatcher finds nothing due. A decision that lands just after a
  disable is recorded as `superseded`, and nothing is done.
- Disable retires them; they are never deleted. The next enable makes the same two sessions active again, with the
  same IDs, so history and audit stay attached to them.

Each may run only a fixed list of operations, enforced by the server. Neither may change a policy, the binding,
labels, roles or intake, or do any Linear administration.

## Model mode (designed, partly built)

In model mode a language model decides claims **within the same guardrails** as the rules, and the rules are the
fallback. The model can only choose among the actions the rules engine's guardrails already permit, so it can be more
careful than the rules (wait or escalate where they would confirm), but it can never grant more. The rules engine
stays the default and needs no model.

Status, on the current main branch:

| Part | Status |
|---|---|
| The dispatcher agent (`dispatcher-agent/`) and its hardened container image | on main |
| `dispatcher enable --engine model` | on main; refused until a model setup has passed its test |
| Model key setup and test, the launcher's and the agent's endpoints | in review |
| Cases: `cases/next`, the read tools, `decide` and `abstain`, the rules fallback | in review |
| Chat (`dispatcher chat`) | built, not yet in review |
| `clawbridge-agentd` (the launcher) with its per-owner network and egress proxy | in review |
| Deployment, and CI against a real Docker daemon, for the launcher | planned |
| A guided `dispatcher model setup` in the CLI and TUI | planned |
| Advice in rules mode (`recommend`), and the opt-in tools | planned |

Only the agent is in a release so far (v0.3.4). `--engine` is on main, but not yet released, and the rest is not on
main yet. The design is in this section so that owners can judge it before it ships.

### The model's key: setup and test

- **The owner's own key.** Each project owner who wants model mode sets up their own provider, model and key. Only
  their projects use it.
- **Setup.** It asks for:
  - the provider and the model (no default);
  - the key, from a hidden prompt or stdin, never the command line;
  - the caps: runs per hour and per day, and a required daily token ceiling.

  It also shows what will be sent to the provider, which the owner must confirm.
- **Sealing.** The server seals the key with its own seal key, so a database copy alone doesn't reveal it. The key
  never appears in history, logs or any answer.
- **Test.** The key starts as `pending_test`. The owner's agent makes one test call with it and reports a fixed code,
  such as `invalid_key`, `no_model_access` or `quota`. A key that passes becomes `active`; one that fails is deleted.
  `--engine model` needs an active key.

### The agent, the launcher and egress

- **Outside the server.** The model runs in a small, separate agent, one per key-holder. It has no database, Linear,
  filesystem or shell access, and no tools but the API's typed ones. Its whole world is the dispatcher-model API
  (`/v1/dispatcher-model/`) and its one provider. See [the agent's README](../dispatcher-agent/README.md).
- **The launcher, `clawbridge-agentd`,** is a separate host process. It starts an owner's agent when that owner has a
  key, and hands it a one-time credential on stdin. The agent exchanges the credential once for its configuration and
  project tokens.
- **Egress.** Each agent runs on its own network. Its only way out is an egress proxy that allows the ClawBridge server
  and that owner's provider host, nothing else.
- **What the provider receives:**
  - typed facts, with session names and opaque per-project refs for people (`p_…`, random, never an email);
  - the issue title and up to 4 KiB of its description, marked as untrusted text;
  - for chat, the question.

### Cases, deadlines and the fallback

- **A case per claim.** While an agent is live and under its caps, each watcher tick opens a **decide case** for each
  pending claim. A case holds:
  - the facts the tick read;
  - the actions the guardrails permit;
  - a deadline: the owner's, 30–300 s, else 90 s.
- **Deciding.** The agent takes the case with `cases/next`, makes up to 8 reads, and then decides or abstains.
  - A decision must be one of the permitted actions.
  - The dispatcher session carries it out after the same checks a rules decision gets: the claim, the policy, the
    session, the holders and the domain's trust are rechecked in the same transaction.
  - Anything that changed is recorded as `superseded`, and nothing is done.
- **One model attempt per claim revision.** Once a case closes (decided, abstained, expired, or closed for want of an
  agent), the rules decide that revision, recorded as `model_fallback` with the reason.
  - A model `wait` also hands the claim to the rules.
  - With no agent available, the rules decide at once.
  - A claim whose case an agent has taken is left to that agent until the deadline.
- **Latency.** An abstain or a missed deadline asks the watcher for an early pass, so the rules decide within about a
  minute. Without the in-process watcher clock, or when an agent stops mid-case, the worst case is the deadline plus
  the watch interval.
- **Turning it off.** Disabling the dispatcher, or switching it to rules, closes every open case at once.

### Chat

- **Who and how:** a project owner asks the model a question from their own terminal, with no session:
  `clawbridge dispatcher chat "Why did FIX-1 wait?"`. Leads can ask only after an owner allows it.
- **What the model uses:** it answers from the read tools and the decision log, which owners and leads read with
  `dispatcher decisions`.
- **Where the reply goes:** it is printed only to the asker's terminal, with terminal control characters removed. It
  never reaches an inbox or a session, and nothing in it is acted on.
- **Budget:** chats use at most a quarter of the model's runs, and claims are always served first.
- **Retention:** questions and replies are kept with the case. A retention policy is planned before v1.0.

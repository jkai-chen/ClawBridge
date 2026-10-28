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
clawbridge dispatcher decisions [--since-hours 24] [--limit 50]               # project owner or lead
clawbridge dispatcher chat "QUESTION"                                          # model mode; see Chat
clawbridge dispatcher chat show [CASE]
clawbridge dispatcher chat leads on|off --expected-revision N                 # project owner
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
  provider, who can ask the model questions (`chat`: `owners`, or `owners and leads`), that their questions go to
  the provider too, and that your dispatcher agent must be running.
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
| Model key setup and test, the launcher's and the agent's endpoints | on main |
| Cases: `cases/next`, the read tools, `decide` and `abstain`, the rules fallback | on main |
| `doctor`'s model checks | on main |
| Chat (`dispatcher chat`) | on main |
| `clawbridge-agentd` (the launcher) with its per-owner network and egress proxy | on main |
| Deployment, and CI against a real Docker daemon, for the launcher | planned |
| A guided `dispatcher model setup` in the CLI | on main |
| The same setup in the TUI (the Model screen, `9`) | on main |
| Advice in rules mode (`recommend`), and the opt-in tools | on main |

Only the agent is in a release so far (v0.3.4). The rows marked on main are not released yet, and the rest is not on
main yet. The design is in this section so that owners can judge it before it ships.

### Setting up your model

```sh
clawbridge dispatcher model setup             # guided, in your own terminal
clawbridge dispatcher model status            # your key, your agent and the test, as doctor sees them
clawbridge dispatcher model key delete --confirm
```

Setup asks, in this order:
1. **The provider,** from the list the dispatcher agent can run: Anthropic, OpenAI, Google Gemini, Mistral, DeepSeek,
   Groq, Together, OpenRouter, or an OpenAI-compatible endpoint at a base URL you give.
2. **The model ID,** as the provider names it. There is no default. Setup lists up to ten of the provider's models from
   the agent's own catalogue, cheapest first by list price, with their context window. A number picks one, and any
   other model ID can be typed. Floating aliases (OpenRouter's `~…-latest`) and routers aren't accepted: the dispatcher
   runs one pinned model, the one its setup test checked. A model without tool use fails the test.
3. **Your key,** at a hidden prompt.
   - It is never an argument or an environment variable. An argument that looks like a key stops setup before
     anything is sent.
   - It is sent once, sealed on the server, and kept by the CLI only in memory.
4. **Your caps:** runs per hour and per day for each project (10 and 50 by default), input and output tokens per run
   (20,000 and 2,000), and your daily token ceiling across your projects. The ceiling has no default: it is your bill.
5. **The disclosure** of what is sent to the provider, which you confirm by typing `yes`.

Then your agent makes one test call with the key, and setup prints the result: either the model is set up, or a fixed
reason (the key was refused, no access to the model, quota, no tool use, the provider unreachable, …). Nothing is
enabled by setup. Model mode is turned on per project with `dispatcher enable --engine model`, or the model becomes
the rules' advisor with `dispatcher model advise on`.

In a script, pass every answer as a flag and the key on stdin:

```sh
clawbridge dispatcher model setup --provider anthropic --model MODEL --daily-tokens 500000 \
  [--caps runs_per_hour=10,runs_per_day=50] --confirm-egress --key-stdin < key-file
```

`--key-stdin` reads a pipe or a file, never a terminal, where the key would show as you type it. Setup exits 0 when
the model is set up, 3 when the test hasn't finished within a minute (check with `dispatcher model status`), and 1 when
it failed or stopped. A model ID that looks like a key is refused, since the model is stored and audited in the clear.

In the TUI, the Dispatcher screen (`6`, or `clawbridge tui --screen dispatcher`) shows a project owner their key, their agent and
the last test, and `s` runs the same setup, step by step, through the same code as the CLI. The key field shows only
whether something was typed, never the key or its length. Esc cancels at any step before the confirmation, and the key
is wiped from memory once sent, on Esc, and when you leave the screen or quit. Members who aren't owners see only that
an owner sets the model up.

To change the key, provider or model, run setup again. The new configuration is tested in a fresh agent, and the old
one keeps running until the new test passes.

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
- **Waking.** On a hosted server, `cases/next` is woken when a case opens, rather than checking every second; it
  still checks every 5 s. Without the server's notification connection it checks every second.
- **Timeouts.** An agent's `cases/next` waits at most 30 s for a case. The server's write timeout (80 s) outlasts it,
  and so does the bundled Caddy, which sets no response timeout on the proxy.
- **Turning it off.** Disabling the dispatcher, or switching it to rules, closes every open case at once.

### Chat

A project owner can ask the model a question from their own terminal, with no session:
`clawbridge dispatcher chat "Why did FIX-1 wait?"`.
- **Who can ask:** project owners. Leads can ask only after an owner runs `dispatcher chat leads on`, which is off by
  default.
- **When:**
  - The dispatcher must be on in model mode, with an agent running.
  - An asker has at most one chat open; a second ask names the open one.
- **Waiting:** the command waits for the reply, up to the case deadline: the owner's, else 120 s, and never over
  300 s. A late reply is read with `dispatcher chat show [CASE]`, which shows only your own chats.
- **What the model sees:**
  - your question, sent to the **model provider** set up for the dispatcher;
  - the read tools;
  - the decision log (`dispatcher decisions` shows the same records).
  It answers with a reply of at most 2,000 characters, or abstains.
- **The budget:** chats use at most a quarter of the model's runs per hour and per day, rounded down, so caps under 4
  runs an hour or a day leave no runs for chat. Past the share, an ask is refused until the window moves, and claims
  are always served before chats. So while the board is busy, a chat waits behind each open claim's case, each
  up to its deadline, and can end with no answer in time: read a late reply with `dispatcher chat show`.
- **Model output in `dispatcher decisions`:** the log's explanations are printed stripped the same way as replies.
- **The reply is model output, not checked.**
  - It is printed only to the asker's terminal, with terminal control characters and bidirectional overrides removed.
  - It never reaches an inbox or a session, and nothing in it is acted on.
  - To act, use the CLI's own commands.
- **Retention:** questions and replies are kept with the case, like other cases. There is no purge yet; a retention
  policy is planned before v1.0.

### Advice in rules mode, and the opt-in tools

In rules mode the model can **advise** instead of decide. The rules decide every claim as usual, and the model only
adds a recommendation to the claims they escalate.
- **Turning it on** (project owner, in your own terminal, with no session):
  - `clawbridge dispatcher model advise on --expected-revision N` shows what it means: the model and caps of your
    tested key, that issue text goes to your model provider, and that a dispatcher agent must run.
  - Add `--confirm` to make the change. `advise off` turns it off and closes any case still open.
  - Advice is for rules mode only: in model mode the model already decides.
  - The preview also lists the opt-in tools already set, since they come back on with advice.
  - `dispatcher disable` and every `dispatcher enable` turn advice and the opt-in tools off. The enable preview lists
    what it turns off (`turns_off`). Each comes back only through its own preview.
- **What happens:**
  - An escalation goes out at once, exactly as without the model.
  - With an agent running and within its caps, the claim then gets one advise case. It is opened in the check that
    escalates, and only then: an escalation that misses it (no agent, caps reached, an error) gets no advice. The model recommends confirm,
    decline or wait, with a reason code and a note of at most 280 characters.
  - The people the escalation reached get a follow-up with the **action and reason code only**. A person still decides.
  - The note is model output, so it never goes to an inbox. Owners and leads read it with
    `clawbridge dispatcher escalations`, stripped of terminal controls. It lists the escalations whose claim is still
    pending; `--all` includes the ones a person already decided.
  - A recommendation that arrives after a person decided is kept but not sent.
- **The opt-in tools** act without a person, so each is off until the owner turns it on with
  `clawbridge dispatcher model tools set [route_notice] [ask_session] --expected-revision N [--confirm]` (no tools turns
  them all off). They work in advise and model-mode cases, never in a chat.
  - `route_notice` sends an escalation the rules couldn't place (no domain, or more than one) to one domain you
    configured, in the escalation's own fixed text, once per case. Each route is audited.
  - `ask_session` sends a session in the case one fixed question about its claim: which domain it belongs to, whether
    it is still working, or what its scope is. The model writes none of the words. At most one per case, one per
    session, issue and question a day, and 20 per project a day, and none once a person has decided the claim.
- **At most 100 projects per owner.** One owner's agent serves at most 100 projects with the model on, in model mode
  or advising. `enable --engine model` and `advise on` refuse a 101st: the preview says `blocked:
  model_projects_over_cap`, and `--confirm` is refused until you turn the model off in another project. You can still
  pass 100 by gaining projects, for example through a new owner role. Then the first 100 by project ID are served, the
  rest get no model, and doctor warns.

### What doctor shows

`clawbridge doctor`, run with your Linear profile (as for the task board's checks), shows a project owner the model
mode, once an owner of the project has a model key or model mode is on. Members see none of it. It reads only, and
never the key:

- **The seal key** (`dispatcher.model.seal`): fails when the server has none, so no stored key opens; warns while
  `CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS` is still set, which means a rotation isn't finished. That warning is shown
  even before anyone sets the model up.
- **The keys** (`dispatcher.model.key`): each owner's key, active or pending its test, with its revision. It fails when
  model mode is on but no owner has a tested key, and warns when a newer setup's test failed (the active key keeps
  running).
- **The agents** (`dispatcher.model.agent`): each owner's agent, running or offline, and when it last called. It fails
  when the provider refuses the key (`provider_auth`) or agentd reported the agent crash-looping in the last hour, and
  warns when a credential was presented twice or with a wrong binding secret in the last week, when the agent was given a
  new credential more than 3 times in 24 hours (agentd restarting it, or someone else holding the launcher token; one a
  day is its credential's renewal), or when model mode is on and no agent has called for 5 minutes.
- **agentd** (`dispatcher.model.launcher`): whether it called in the last 5 minutes, and when its launcher token
  expires; it warns 14 days ahead.
- **Projects per owner** (`dispatcher.model.projects`): shown only when an owner of this project has the model on in
  more than 100 projects. It warns with the count, and says whether this project is among the 100 served.
- **Capacity** (`dispatcher.model.capacity`): the server's live agents against the cap, warning at the cap only when an
  owner of this project with an active key has no agent running, and when the agents that may still start (160 MiB
  each) need more than the host's available memory.

The seal key, agentd and capacity checks are about the whole server, so every project owner sees the same: whether the
seal keys are set, when agentd last called and when its token expires, how many agents run, and the host's available
memory. They are counts and times, never a key or another project's names.

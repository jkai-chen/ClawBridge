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
  - With no domains set and a binding that names no Linear projects, there is nothing to match: the preview says
    `blocked: no_domains`, and enable is refused until you set domains.
- **Disable** needs no preview. It turns the dispatcher off, retires its two sessions (they are never deleted), and
  makes your most recently seen session in the project the coordinator again. Domains and trust are kept for the next
  enable.

## The dispatcher's sessions

The dispatcher acts as two reserved sessions, `clawbridge-dispatcher` and `clawbridge-dispatcher-model`. Neither
appears in session lists, and neither can receive messages. People's sessions can't take their names.

Each may run only a fixed list of operations, enforced by the server. Neither may change a policy, the binding,
labels, roles or intake, or do any Linear administration.

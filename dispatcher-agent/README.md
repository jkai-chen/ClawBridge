# The dispatcher agent (model engine)

The dispatcher's optional **model engine**: a small Node service that decides dispatcher cases with a language model.
It is a ClawBridge **client** and nothing more. It connects as the project's restricted `dispatcher-model` session,
and the server decides every grant: the guardrails, the role's allow-list, the caps and the audit stay in ClawBridge.
Any program that speaks the same API can replace it.

It is built on two packages from [clawbridge-pi](https://github.com/jkai-chen/clawbridge-pi), a stripped-down fork
of [pi](https://github.com/earendil-works/pi) (MIT): `pi-ai` (model providers) and `pi-agent-core` (the tool loop).
The fork removes everything else, including pi's coding tools, CLI, logins and telemetry.

This release adds the agent and its tests. Nothing runs it yet: the server endpoints, the container and the launcher
come in later steps (see "Status").

## What it does

1. **Starts** with three inputs only:
   - the server URL, in `CLAWBRIDGE_SERVER_URL` (HTTPS; plain HTTP only for localhost);
   - the egress proxy, in `CLAWBRIDGE_EGRESS_PROXY` (`http://HOST:PORT`). It is required: unset, malformed or
     unreachable, the agent refuses to start and says why;
   - a one-time agent credential (`cba_…`) on **stdin**, read once.
2. **Fetches its configuration** from the server (`agent/config`): provider, model, key, project tokens and per-run
   limits. The key lives only in memory.
   - **New config:** for a config that is still `pending_test`, it first runs the **setup test**: one request with
     one no-op `ping` tool. It reports the result to `agent/test-result` as a fixed code (`invalid_key`,
     `no_model_access`, `quota`, `rate_limited`, `no_tool_use`, `egress_blocked`, `provider_error` or `timeout`),
     and exits if the test failed.
   - **Status:** it reports `ok` to `agent/status`, so that `doctor` can tell a working agent from an offline one.
3. **For each project**, checks `hello`. It refuses to serve a project whose provider or model differs from its own.
   It then pulls cases one at a time (`cases/next`).
4. **Decides each case** with a fresh agent and the case's typed tools only:
   - reads for every kind;
   - one outcome (`recommend`, `decide`, `reply` or `abstain`);
   - opt-in tools only when `hello` lists them, and never in chat.
5. **Falls back to `abstain(uncertain)`**, so the server's rules decide instead, when:
   - the deadline nears (5 s before it);
   - the per-run token cap is reached;
   - the model calls an unknown tool;
   - an outcome is refused;
   - the provider fails;
   - the model stops without an outcome.

   A provider `401`/`403` stops all case polling. The agent then reports `provider_auth` to `agent/status` every
   5 minutes until it is shut down.
6. **Renews** its project tokens with the binding secret, and never fetches the key again. The projects it serves
   follow each renewal, which lists the owner's enabled projects. A newly enabled one gets a loop (renewal is at most
   5 minutes apart), and a disabled one loses its loop. With none yet, as right after an owner's setup test, it stays
   up and renews every minute.
   - A dropped project's running case stops at once, and sends no outcome: its token is gone, and the server falls
     back to rules at the deadline. A project listed again starts only once its old loop has ended.
   - A list of more than 100 projects, or any answer over 1 MiB, is refused.
   - A loop that crashed is started again by the next renewal. One that ended on a provider mismatch is not, while
     the project stays listed.

## What it never does

- **Environment variables:** it doesn't read provider keys from the environment. It **refuses to start** if any
  variable that pi-ai would read is set (`src/env-guard.ts`; a test fails when the pinned pi-ai reads a new one).
- **Logs:** it logs no prompt, model text, issue text, key, token or header. Log lines are JSON with IDs, tool names,
  codes, counts and durations only. Provider errors are reduced to a status and a fixed type.
- **Outside access:** it has no database, Linear, filesystem or shell access, and makes no call except to the
  ClawBridge server and the one provider host.
- **Direct connections:** every request goes through the egress proxy (`src/egress.ts`):
  - Node's `fetch`, which the ClawBridge client and the provider SDKs use, is undici's `fetch` on a `ProxyAgent`. That
    includes their retries. Every request, http or https, is a CONNECT tunnel.
  - Every other connection or DNS lookup fails at once with `egress refused`: `node:http`, `node:https`, `node:net`,
    `dns` and its resolvers, and libraries built on them. There is no direct-dial fallback.
  - A refused destination reports `egress_refused`, a proxy that can't be reached `egress_unavailable`. A reset or a
    timeout on an open tunnel is an ordinary `network` failure, because the request may have arrived. An outcome is
    retried after either `network` or `egress_unavailable`. A provider call failing either way is the setup test's
    `egress_blocked`.
  - **At startup** the proxy may not have this agent's policy yet, since it rereads it every 2 seconds. The first
    config call is tried up to 6 times, a second apart, while the proxy refuses it or can't be reached. Such a call
    never reached the server, so the one-time credential is still unused.
  - **What it doesn't cover:** this fence lives in the agent's own process. It can't see `worker_threads`, UDP
    (`node:dgram`) or a `child_process`. The real boundary is the container: its only route out is the egress proxy.
- **Memory between cases:** none.

## The contract

`api/dispatcher-model/v1.schema.json` is the dispatcher-model API, v1. It holds every request, response and error
code, as JSON Schema 2020-12, with one `$defs` entry per operation and an `x-operations` table.
- **Source:** it is **generated** from `src/schema.ts`, so to change the contract, edit that file and run
  `npm run schema`. CI fails when the two differ.
- **The server:** it embeds the same file and validates against it.

## Development

```sh
cd dispatcher-agent
npm ci --ignore-scripts
npm run check          # tsc
npm run check:schema   # v1.schema.json matches src/schema.ts
npm run check:lock     # pi from the fork release, 14-day package age, allowed licences
npm test               # node:test against a fake ClawBridge and a fake provider; no network, no keys
```

- **Node:** Node 22.20, the version CI and the image test, runs the TypeScript directly, with no build step.
  `engines` pins it: the egress proxy's undici must match the one Node bundles.
- **Tests:** they never call a real provider. They run in CI with no keys present.

## The container

`Dockerfile` builds the image from `node:22.20.0-slim` (pinned by digest).
- **Contents:** production dependencies (`npm ci --omit=dev --ignore-scripts`), the agent's source and a generated
  `THIRD-PARTY-NOTICES`.
- **Removed:** npm, npx and corepack.
- **User:** it runs as the non-root `node` user.

Run it only with these flags. `clawbridge-agentd` sets them; until it exists, so does whoever runs the agent by hand:

```sh
printf '%s\n' "$AGENT_CREDENTIAL" | docker run -i --rm --name clawbridge-agent-OWNER_REF \
  --read-only --tmpfs /tmp:rw,noexec,nosuid,size=8m \
  --cap-drop ALL --security-opt no-new-privileges --pids-limit 64 \
  --memory 160m --memory-swap 160m --user node \
  -e CLAWBRIDGE_SERVER_URL=https://clawbridge.example.com \
  -e CLAWBRIDGE_EGRESS_PROXY=http://clawbridge-egress:3128 \
  clawbridge-dispatcher-agent
```

- **Environment:** exactly two variables, the server URL and the egress proxy. The credential goes on stdin, never in
  env or argv. The agent refuses to start if a provider-key, ambient proxy (`HTTP_PROXY` and the like) or Node
  TLS/preload variable is set.
- **Memory:** 160 MiB with no swap. The heap is capped at 96 MiB by the image's entry point.
- **Network:** no inbound port. The per-owner network and the egress proxy, which allows only the ClawBridge host and
  that owner's provider host, come with `clawbridge-agentd`. Its container is `clawbridge-egress`, on port 3128.

`scripts/container-test.sh` (run in CI) builds the image and runs 1,000 cases through it under exactly these flags. The
fakes it runs against share only a loopback network with it. It checks:
- every outcome, against the schema;
- no OOM, reporting the cgroup's memory peak (91 MiB on the last run);
- every request went through a fake egress proxy, tunnelled to the server and the provider only;
- no connection or DNS lookup beyond loopback, with a negative control;
- the hardening itself: non-root, read-only root, no capabilities, no npm.

## Moving the pin

The two pi packages are pinned to the fork's release tarballs by URL and `integrity` in `package-lock.json`. An
`overrides` entry keeps a single `pi-ai`. To move the pin:
1. **The fork:** a new `cb-v…` tag is released on the fork.
2. **The URLs:** change both URLs in `package.json`, run `npm install --ignore-scripts`, and check that the lockfile's
   integrity matches the release's `SHA256SUMS` build.
3. **The PR:** the PR is labelled as a pin move.

Every other package must be at least 14 days old (`.npmrc` and `check:lock`).

## Status

Done:
- the agent, its tests and the contract;
- the hardened container image, with its memory, egress and hardening test;
- `clawbridge-agentd`'s validating Docker proxy, fixed container spec and reconciler (`cmd/clawbridge-agentd`).

Next:
- the server endpoints, which `clawbridge-agentd run` also needs;
- the per-owner egress proxy, and deploying the launcher under a rootless daemon;
- guided model setup in the CLI and TUI.

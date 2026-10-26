# clawbridge-agentd

The launcher for the dispatcher agent (`dispatcher-agent/`). It runs one agent container per owner who turns the
model on, and gives each one its one-time agent credential on stdin. It never sees a provider key.

Its Docker access is root-equivalent, so it is kept small, and it is fenced in by two layers:

1. **A separate rootless Docker daemon.** The agents run under a rootless `dockerd` owned by an unprivileged user
   (`cbagent`), used only for this. agentd never talks to the root daemon that runs the api and Postgres. It
   refuses `/var/run/docker.sock`, `/run/docker.sock` and any root-owned socket.
2. **A validating socket proxy** (`clawbridge-agentd proxy`), run as `cbagent` in front of that daemon. It forwards
   only these calls:
   - **Agent containers:** create, attach to stdin, start, stop, remove and inspect, on containers named
     `clawbridge-agent-<owner ref>`.
   - **The egress container:** the same on `clawbridge-egress`, except attach.
   - **Networks:** create, inspect and remove on `cbnet-<owner ref>`, and connect or disconnect `clawbridge-egress`
     there. The egress network `clawbridge-egress-net` can only be created.
   - **Lists:** containers and networks.

   A create passes only when its body is exactly that container's or network's fixed spec, and the proxy forwards
   its own encoding of it. An owner network must sit on a /28 of `172.30.0.0/16`. Privileged mode, binds (other
   than the egress container's read-only policy directory), mounts, volumes, devices, added capabilities, another
   network, extra env, another image digest or a missing limit are all refused. So is any other endpoint, query
   parameter or path shape.

## The egress proxy

`clawbridge-agentd egress --listen IP:PORT --policy FILE` is the one process on every owner's internal network
(design §5.3). It holds no secrets and does no TLS interception.

- **CONNECT only.** Plain proxying gets 405, and anything else gets 400.
- **Per source network.** Each owner's subnet may reach exactly its own `host:port` entries: the ClawBridge server and
  that owner's provider, two at most. Another owner's provider, another host and another port are all refused.
- **No rebinding.** The proxy resolves a name itself and refuses the CONNECT if any address in the answer is not
  public (loopback, private, link-local such as cloud metadata, CGNAT). It then dials the address it checked. An IP
  literal passes only when it is listed itself, and must be public.
- **The policy** is a JSON file agentd writes from the server's per-owner config:

  ```json
  {"networks": [{"owner_ref": "p_…", "source": "172.30.1.0/24",
                 "allow": ["bridge.example.com:443", "api.anthropic.com:443"]}]}
  ```

  It is reread every 2 seconds. A change closes the open tunnels it no longer allows. An unreadable or invalid file
  refuses all egress until it is valid again.
- **Limits:** 32 tunnels per network, 10 minutes idle, 10 seconds for the request and the dial.
- **Logs:** the owner ref, the target, the outcome and the byte counts. Never the tunnel's bytes.

## How the agents reach the outside

- **Owner networks.** Each owner's agent sits alone on its internal network `cbnet-<owner ref>`, on a fixed /28 of
  `172.30.0.0/16` that agentd allocates (the first free one; up to 4,096 owners). The network has no route out. A
  subnet an owner gave up waits a minute before another owner gets it, so none of the old owner's tunnels or policy
  can match the new agent.
  - **The pool and your VPC:** `172.30.0.0/16` sits inside common VPC ranges. Under the rootless daemon the networks
    live in the daemon's own namespace, so they can't clash with the host's routes. A rootful install, which this
    design doesn't use, would have to check for overlap with the host's networks first.
- **The egress container.** `clawbridge-egress` runs `clawbridge-agentd egress` from the agentd image, by digest:
  - as `nobody`, read-only, with no capabilities, no env and 32 MiB;
  - on `clawbridge-egress-net` (`172.31.255.0/28`), the one network with a route out;
  - its only host path is the policy directory, read-only.

  agentd connects it to every owner network, where agents reach it as `clawbridge-egress:3128`. It reconnects it
  when the container was recreated.
- **The policy.** agentd writes the policy from each owner's subnet, the server's `host:port` and that owner's
  provider `host:port` from the server. It writes it before an owner's agent starts, and only when it changed. The
  egress proxy picks it up within 2 seconds.
- **No egress, no agents.** If the egress container can't be kept running, no agent starts that round.

## The fixed spec

The spec is compiled in. The server says which owner refs should run, never how.
- **Image:** pinned by digest (`CLAWBRIDGE_AGENT_IMAGE=name@sha256:…`).
- **Filesystem:** read-only root, with a tmpfs `/tmp` (`noexec,nosuid`, 8 MiB).
- **Privileges:** the image's non-root `node` user, no capabilities, `no-new-privileges`.
- **Limits:** 64 pids, 160 MiB of memory with no swap.
- **Network:** the owner's own internal network `cbnet-<owner ref>`, which has no route out.
- **Environment and input:** two env variables, `CLAWBRIDGE_SERVER_URL` and
  `CLAWBRIDGE_EGRESS_PROXY=http://clawbridge-egress:3128`. stdin is open once, for the credential.
- **Label:** one, `clawbridge.config_revision`, the owner's config revision it was started for (a positive number,
  the one value agentd chooses). When the server's revision for that owner changes, after a rotated key or a new
  model, agentd replaces the container with a fresh one and a fresh credential (§11.6). That is not counted as a
  crash, and agentd still sees the revision after it restarts itself.

## Commands

```sh
clawbridge-agentd proxy --listen /run/clawbridge-agentd/docker.sock --upstream /run/user/UID/docker.sock
clawbridge-agentd check
clawbridge-agentd run
```

- **`proxy`:** reads `CLAWBRIDGE_SERVER_URL`, `CLAWBRIDGE_AGENT_IMAGE`, `CLAWBRIDGE_EGRESS_IMAGE` and
  `CLAWBRIDGE_EGRESS_POLICY_DIR`, which make up the fixed specs. It holds no secrets, and refuses to
  start with `CLAWBRIDGE_LAUNCHER_TOKEN` in its environment. Its socket is mode 0660, so agentd connects through
  the socket's group.
- **`check` and `run`:** read agentd's root-only (0600) env file:
  - `CLAWBRIDGE_SERVER_URL`;
  - `CLAWBRIDGE_AGENT_IMAGE` and `CLAWBRIDGE_EGRESS_IMAGE`;
  - `CLAWBRIDGE_EGRESS_POLICY_DIR`, a directory agentd writes and the egress container reads;
  - `CLAWBRIDGE_LAUNCHER_TOKEN` (`cbl_…`), which is never logged;
  - `CLAWBRIDGE_AGENTD_DOCKER`, the proxy's socket.
- **No root:** neither process runs as root.

**What `run` does:** every 30 seconds it compares the server's wanted owner refs with the running
`clawbridge-agent-*` containers:
- It starts the missing ones: network, create, attach, start, then a fresh credential on stdin.
- It stops and removes the rest.
- It never runs more than the server's `dispatcher_model_max_agents`, counted from Docker's own list.
- It restarts a crashed agent at most 3 times an hour, then reports `agent_crashloop`.
- It never touches a container with another name.
- Logs are JSON lines: owner refs, actions, Docker calls and status codes. Never a credential.

## Status

Done:
- the proxy and its boundary tests;
- the reconciler and the Docker calls, tested through the real proxy against a fake daemon;
- the configuration checks;
- the egress proxy and its tests (fake DNS and upstream).

Next:
- **The server's launcher endpoints:** the wanted set, minting the one-time credential, and the report. Until they
  exist, `run` exits saying so.
- **The agent's proxy setting:** the agent must send its requests through `http://clawbridge-egress:3128`, set from
  config rather than ambient env. That change is on the agent's side.
- **An agentd image** that ships the binary for the egress container.
- **Deploy:** the `cbagent` user, the rootless daemon and the systemd units. The deploy also loads the pinned
  image into that daemon, because the proxy allows no pull.
- **Real-daemon tests:** in CI, against a real rootless daemon. A create through the proxy is refused. With the
  proxy bypassed, a container still can't read a root-owned file on the host.
- **Overhead:** measure the fixed memory overhead, for `doctor`.

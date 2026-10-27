# Installing clawbridge-agentd

`clawbridge-agentd` starts each owner's dispatcher agent in its own container, when an owner sets up the model and
turns it on (`docs/dispatcher.md`). It is optional: without it, the rules decide every claim. What it does and why it
is fenced in the way it is: [cmd/clawbridge-agentd/README.md](../../cmd/clawbridge-agentd/README.md).

This page installs it beside the reference deployment ([deployment.md](deployment.md)): one Linux host (Debian 12 or
similar, with systemd and cgroup v2) whose root Docker daemon runs the Compose stack. **The root daemon and the stack
are not changed,** apart from the api getting its seal key. Nothing new listens on the network.

## What is added

| Piece | Runs as | What it can reach |
|---|---|---|
| a rootless `dockerd` (cbagent's user unit) | `cbagent` | its own socket, `/run/user/<uid>/docker.sock` |
| `clawbridge-agentd-proxy.service` | `cbagent` | that daemon; it listens on `/run/clawbridge-agentd/docker.sock` (0660, group `cbagent-sock`) |
| `clawbridge-agentd.service` | `cbagentd` | the proxy's socket (through `cbagent-sock`), the server, the policy directory |
| the egress and agent containers | under the rootless daemon | each agent its own network; only the egress proxy has a route out |

**Two users, not one:** agentd must reach Docker only through the proxy, which checks every create against the fixed
spec. As the daemon's own user it could open the daemon's socket and skip the proxy. Neither user is root, and neither
is in the `docker` group.

The repository's files: `deploy/systemd/agentd/` (the two units, `delegate.conf`, example env files) and
`deploy/agentd-check.sh` (a read-only check of the install).

## Before you start

- **Memory,** as measured under a real rootless daemon (the `agentd-rootless` CI job): agentd's fixed cost is about
  260 MiB once (the rootless dockerd about 100, containerd about 60, the rest small), and each agent about 170 MiB (its
  160 MiB limit and its container's shim). Set `CLAWBRIDGE_DISPATCHER_MODEL_MAX_AGENTS` (deployment.md) so that
  `MemAvailable`, with the stack running, still leaves 300 MiB after agentd and the agents that may start. On a host
  with about 1 GiB available, that is **1**, the default.
- **Disk.** At least 2 GiB free under `/home` for the rootless daemon's images and logs.
- **The server's address** must be public HTTPS: the agents reach it through the egress proxy, which allows public
  addresses only, unless it is listed with `CLAWBRIDGE_EGRESS_PRIVATE_SERVER` (the README).
- **The images,** built from the release's commit on another machine:
  - the agent: `docker build -t clawbridge-dispatcher-agent dispatcher-agent`;
  - agentd's (for the egress proxy): `docker build -f cmd/clawbridge-agentd/Dockerfile -t clawbridge-agentd .`;
  - `docker save` each, copy the tarballs to the host, and compare their SHA-256 there.
- **The binary:** `CGO_ENABLED=0 go build -trimpath -o clawbridge-agentd ./cmd/clawbridge-agentd`, copied and checked
  the same way.

## 1. Users, packages and the rootless daemon

```sh
sudo apt-get install docker-ce-rootless-extras uidmap dbus-user-session slirp4netns   # same repo and version as docker-ce
sudo groupadd --system cbagent-sock
sudo useradd --system --create-home --shell /usr/sbin/nologin cbagent
sudo useradd --system --no-create-home --shell /usr/sbin/nologin --groups cbagent-sock cbagentd
grep '^cbagent:' /etc/subuid /etc/subgid      # one range each; add 65536 IDs that overlap no other user's if missing
sudo loginctl enable-linger cbagent           # its user manager, and its dockerd, start at boot
```

**cgroup delegation,** for cbagent's user manager only, so the agents' CPU limit can be set:

```sh
uid=$(id -u cbagent)
sudo install -d -m 0755 "/etc/systemd/system/user@$uid.service.d"
sudo install -m 0644 deploy/systemd/agentd/delegate.conf "/etc/systemd/system/user@$uid.service.d/delegate.conf"
sudo systemctl daemon-reload && sudo systemctl restart "user@$uid.service"
cat "/sys/fs/cgroup/user.slice/user-$uid.slice/user@$uid.service/cgroup.controllers"   # must list cpu memory pids
```

**The rootless daemon,** installed as cbagent with only cbagent's own environment (a caller's `XDG_CONFIG_HOME`
would put cbagent's unit in the caller's home):

```sh
home=$(getent passwd cbagent | cut -d: -f6)
cbuser() { sudo -H -u cbagent env -i HOME="$home" USER=cbagent LOGNAME=cbagent XDG_RUNTIME_DIR="/run/user/$uid" \
  XDG_CONFIG_HOME="$home/.config" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
  PATH=/usr/local/bin:/usr/bin:/usr/sbin:/bin "$@"; }
cbuser systemctl --user show-environment | grep '^XDG_'   # must name cbagent's own directories
cbuser dockerd-rootless-setuptool.sh install
```

If `show-environment` names another user's `XDG_RUNTIME_DIR` (a host that sets it in `/etc/environment` does that to
every user manager), set cbagent's first:
`cbuser systemctl --user set-environment XDG_RUNTIME_DIR=/run/user/$uid XDG_CONFIG_HOME=$home/.config`. The CI job
`agentd-rootless` installs it exactly this way.

Do not pass `--skip-iptables`: the owner networks and the egress network rely on the daemon's own iptables. In
`~cbagent/.config/docker/daemon.json`, set `default-address-pools` outside `172.30.0.0/16` and `172.31.255.0/28`,
which agentd allocates, for example `[{"base":"10.213.0.0/16","size":24}]`, then restart cbagent's `docker` unit.

**Load the images** into that daemon directly (the proxy allows no pull or load), and note their IDs. Every Docker
command for cbagent's daemon names it, as `cbdocker` below; a bare `docker` is the root daemon's:

```sh
cbdocker() { sudo -u cbagent DOCKER_HOST="unix:///run/user/$(id -u cbagent)/docker.sock" docker "$@"; }
cbdocker load -i clawbridge-dispatcher-agent.tar
cbdocker load -i clawbridge-agentd.tar
cbdocker image inspect --format '{{.Id}}' clawbridge-dispatcher-agent clawbridge-agentd
```

Pin them by these IDs (`sha256:…`): an image loaded this way has no repo digest. Delete the tarballs afterwards.

## 2. The binary, the env files and the units

```sh
sudo install -d -m 0755 /usr/local/libexec/clawbridge
sudo install -o root -g root -m 0755 clawbridge-agentd /usr/local/libexec/clawbridge/clawbridge-agentd
sudo install -d -o root -g root -m 0700 /etc/clawbridge-agentd
sudo install -d -o cbagentd -g cbagentd -m 0755 /var/lib/clawbridge-agentd/policy
```

- `/etc/clawbridge-agentd/proxy.env`, root **0644**, from `proxy.env.example`: the server URL, both image IDs and the
  policy directory. No secret.
- `/etc/clawbridge-agentd/agentd.env`, root **0600**, from `agentd.env.example`: the same four values, plus the
  proxy's socket. The two files must agree, or the proxy refuses agentd's creates.
- `/etc/clawbridge-agentd/launcher.env`: written by the next section, never by hand.

systemd reads the env files as root and passes the values on; agentd itself can't open them.

```sh
sudo install -m 0644 deploy/systemd/agentd/clawbridge-agentd-proxy.service deploy/systemd/agentd/clawbridge-agentd.service /etc/systemd/system/
sudo systemctl daemon-reload
```

## 3. The launcher token

`PROJECT` is the Compose project name the stack runs under (deployment.md). The api image runs as `nonroot` (uid
65532). It writes the token into a one-run directory it owns, and root moves the
file into place, so the token is never printed or copied:

```sh
sudo install -d -o 65532 -g 65532 -m 0700 /etc/clawbridge-agentd/mint
sudo docker compose --env-file /etc/clawbridge/clawbridge.env --project-name "$PROJECT" \
  -f /opt/clawbridge/repo/deploy/compose.yaml \
  run --rm --no-deps -v /etc/clawbridge-agentd/mint:/out api operator launcher-token create --out /out/launcher.env
sudo test ! -e /etc/clawbridge-agentd/launcher.env
sudo mv /etc/clawbridge-agentd/mint/launcher.env /etc/clawbridge-agentd/launcher.env
sudo chown root:root /etc/clawbridge-agentd/launcher.env
sudo chmod 0600 /etc/clawbridge-agentd/launcher.env
sudo rmdir /etc/clawbridge-agentd/mint
```

- The create prints the token's ID (`lt_…`) and expiry date: keep them.
- If `test ! -e` stops you, a token is already in place. Revoke the one you just made by its ID, and remove
  `/etc/clawbridge-agentd/mint/launcher.env` by name.

## 4. The seal key

The api needs `CLAWBRIDGE_MODEL_KEY_SEAL_KEY` for model setup. It goes in the **api's** env file, never agentd's.
Generate it on the host, as root, straight into that file, so it is never printed, typed, or in any command line:

```sh
sudo sh -c 'set -e; umask 077; f=$(readlink -f /etc/clawbridge/clawbridge.env);
  if grep -q "^CLAWBRIDGE_MODEL_KEY_SEAL_KEY=" "$f"; then echo "a seal key line exists (even empty); not changed"; exit 1; fi;
  cp -p "$f" "$f.pre-seal";
  [ -z "$(tail -c 1 "$f")" ] || printf "\n" >> "$f";
  printf "CLAWBRIDGE_MODEL_KEY_SEAL_KEY=%s\n" "$(openssl rand -base64 32)" >> "$f"'
sudo sh -c 'f=$(readlink -f /etc/clawbridge/clawbridge.env);
  n=$(grep -c "^CLAWBRIDGE_MODEL_KEY_SEAL_KEY=" "$f");
  b=$(sed -n "s/^CLAWBRIDGE_MODEL_KEY_SEAL_KEY=//p" "$f" | base64 -d 2>/dev/null | wc -c);
  echo "lines=$n bytes=$b"'                                    # must print lines=1 bytes=32
```

- An existing line, even an empty one, stops it: delete that line by hand first.
- `$f.pre-seal` holds every other secret of that file. It is root 0600; delete it once you keep the seal key.
- Never run `cat`, `grep` without `-c`, or `docker compose config` on that file: each would print the key.
- Then recreate the api: `docker compose … up -d --no-deps api`. `clawbridge doctor`, as a project owner, shows the
  seal key set.

There is no copy of the seal key in the database backups. If it is lost, the stored provider keys can't be opened,
and owners run `dispatcher model setup` again.

## 5. Start, and check

```sh
sudo sh deploy/agentd-check.sh                         # files, users and groups; changes nothing
sudo systemctl enable --now clawbridge-agentd-proxy clawbridge-agentd
sudo sh deploy/agentd-check.sh --running               # also the units, sockets, cgroups and memory
journalctl -u clawbridge-agentd -n 20                  # a round every 30 seconds
```

The check prints only `PASS` and `FAIL` lines, never a file's contents, and exits 1 if anything fails.

## Removing it

In this order, by name. The root daemon and the stack are never touched.

1. **Stop new agents:** `operator launcher-token revoke lt_…`. This does not stop running agents: each keeps renewing
   its project tokens with its credential for up to 24 hours. Steps 2 and 3 stop them.
2. `sudo systemctl disable --now clawbridge-agentd clawbridge-agentd-proxy`
3. In cbagent's daemon, through `cbdocker` (§1), never a bare `docker`: `cbdocker rm -f -v clawbridge-agent-<ref>` for
   each agent, `cbdocker rm -f -v clawbridge-egress`, then `cbdocker network rm cbnet-<ref>` for each and
   `cbdocker network rm clawbridge-egress-net`.
4. Owners turn the model off (`dispatcher disable`, or `dispatcher model advise off`); the rules decide again.
5. Only if model setup should go too: restore `clawbridge.env.pre-seal` and recreate the api. Stored provider keys
   can then no longer be opened.
6. To remove everything: `systemctl --user -M cbagent@ disable --now docker`, `loginctl disable-linger cbagent`, remove
   the unit files and the `delegate.conf` directory, `daemon-reload`, `userdel -r cbagent`, `userdel cbagentd`,
   `groupdel cbagent-sock`, and remove `/etc/clawbridge-agentd`, `/var/lib/clawbridge-agentd` and the binary.

There is no schema change in any of this, so no database restore is ever part of removing it.

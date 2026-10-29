#!/usr/bin/env bash
# agentd's real-daemon CI job, part 1: installs agentd on the runner as
# docs/operations/agentd.md does (two users, cbagent's rootless dockerd with
# cgroup delegation, the images loaded by ID, the units and env files), with
# integration/agentd-rootless/fakelauncher as the server. CI only: it
# creates users and changes the host. Run from the repository root, as root.
set -euo pipefail
[ "${GITHUB_ACTIONS:-}" = true ] && [ "$(id -u)" = 0 ] || { echo "CI only, as root"; exit 2; }

repo=$PWD
work=/var/tmp/agentd-ci
mkdir -p "$work"
step() { echo "::group::$*"; }
end() { echo "::endgroup::"; }

step "packages"
# Ubuntu 24.04 lets AppArmor stop unprivileged user namespaces, which
# rootlesskit needs.
sysctl -w kernel.apparmor_restrict_unprivileged_userns=0 || true
apt-get update -qq
apt-get install -y -qq uidmap dbus-user-session slirp4netns
docker version --format 'root daemon {{.Server.Version}}'
# docker-ce-rootless-extras where Docker's apt repo is configured (as on
# the deploy host); otherwise Docker's static build of the same files for
# the runner's 29.4.2, checked against its pinned SHA-256.
if ! command -v dockerd-rootless-setuptool.sh >/dev/null && ! apt-get install -y -qq docker-ce-rootless-extras; then
  extras=docker-rootless-extras-29.4.2.tgz
  curl -sSfL --proto '=https' -o "$work/$extras" "https://download.docker.com/linux/static/stable/x86_64/$extras"
  echo "02dd6a42fe8824af169be7d94e9a01618e8d70a5712e7c273f1e6a605e145042  $work/$extras" | sha256sum -c -
  tar -xzf "$work/$extras" -C "$work"
  install -m 0755 "$work"/docker-rootless-extras/* /usr/local/bin/
  # The setuptool runs the docker CLI from its own directory.
  [ -e /usr/local/bin/docker ] || ln -s "$(command -v docker)" /usr/local/bin/docker
  rm -rf "$work/$extras" "$work/docker-rootless-extras"
fi
end

step "users and cgroup delegation (agentd.md §1)"
groupadd --system cbagent-sock
useradd --system --create-home --shell /usr/sbin/nologin cbagent
useradd --system --no-create-home --shell /usr/sbin/nologin --groups cbagent-sock cbagentd
uid=$(id -u cbagent)
grep -q '^cbagent:' /etc/subuid || echo "cbagent:300000:65536" >> /etc/subuid
grep -q '^cbagent:' /etc/subgid || echo "cbagent:300000:65536" >> /etc/subgid
install -d -m 0755 "/etc/systemd/system/user@$uid.service.d"
install -m 0644 deploy/systemd/agentd/delegate.conf "/etc/systemd/system/user@$uid.service.d/delegate.conf"
systemctl daemon-reload
loginctl enable-linger cbagent
for _ in $(seq 30); do [ -S "/run/user/$uid/bus" ] && break; sleep 1; done
cat "/sys/fs/cgroup/user.slice/user-$uid.slice/user@$uid.service/cgroup.controllers"
end

step "cbagent's rootless dockerd"
install -d -o cbagent -g cbagent -m 0700 ~cbagent/.config ~cbagent/.config/docker
printf '{"default-address-pools":[{"base":"10.213.0.0/16","size":24}]}\n' > ~cbagent/.config/docker/daemon.json
chown cbagent:cbagent ~cbagent/.config/docker/daemon.json
home=$(getent passwd cbagent | cut -d: -f6)
ls -ld "$home" "$home/.config" "/run/user/$uid"
cd "$home"
# The runner's own XDG settings reach every user manager here (CI only):
# give cbagent's its own before its dockerd starts.
grep -rn 'XDG_' /etc/environment /etc/security/pam_env.conf /etc/environment.d 2>/dev/null || true
cbuser() { sudo -H -u cbagent env -i HOME="$home" USER=cbagent LOGNAME=cbagent XDG_RUNTIME_DIR="/run/user/$uid" \
  XDG_CONFIG_HOME="$home/.config" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
  PATH=/usr/local/bin:/usr/bin:/usr/sbin:/bin "$@"; }
cbuser systemctl --user show-environment | grep '^XDG_' || true
cbuser systemctl --user set-environment XDG_RUNTIME_DIR="/run/user/$uid" XDG_CONFIG_HOME="$home/.config"
# Only cbagent's own environment: a caller's XDG_CONFIG_HOME (the runner
# sets one) would put cbagent's user unit in the caller's home.
if ! sudo -H -u cbagent env -i HOME="$home" USER=cbagent LOGNAME=cbagent XDG_RUNTIME_DIR="/run/user/$uid" \
  XDG_CONFIG_HOME="$home/.config" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
  PATH=/usr/local/bin:/usr/bin:/usr/sbin:/bin bash -x "$(command -v dockerd-rootless-setuptool.sh)" install > "$work/setuptool.log" 2>&1; then
  tail -60 "$work/setuptool.log"
  exit 1
fi
tail -5 "$work/setuptool.log"
cd "$repo"
for _ in $(seq 30); do [ -S "/run/user/$uid/docker.sock" ] && break; sleep 1; done
end

cbdocker() { sudo -u cbagent DOCKER_HOST="unix:///run/user/$uid/docker.sock" docker "$@"; }

step "images, built by the root daemon, loaded into cbagent's by ID"
docker build -q -t clawbridge-dispatcher-agent dispatcher-agent >/dev/null
docker build -q -f cmd/clawbridge-agentd/Dockerfile -t clawbridge-agentd . >/dev/null
docker save -o "$work/agent.tar" clawbridge-dispatcher-agent
docker save -o "$work/agentd.tar" clawbridge-agentd
chmod 0644 "$work"/*.tar
cbdocker load -q -i "$work/agent.tar"
cbdocker load -q -i "$work/agentd.tar"
rm -f "$work"/*.tar
agent_id=$(cbdocker image inspect --format '{{.Id}}' clawbridge-dispatcher-agent)
egress_id=$(cbdocker image inspect --format '{{.Id}}' clawbridge-agentd)
# The probes' own image, pulled by cbagent's daemon directly.
cbdocker pull -q busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
end

step "the fake server and its CA"
CGO_ENABLED=0 go build -trimpath -o "$work/fakelauncher" ./integration/agentd-rootless/fakelauncher
token="cbl_$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=')"
echo "127.0.0.1 bridge.test" >> /etc/hosts
FAKE_LAUNCHER_TOKEN=$token nohup "$work/fakelauncher" -ca "$work/ca.pem" > "$work/fakelauncher.log" 2>&1 &
for _ in $(seq 20); do [ -s "$work/ca.pem" ] && break; sleep 0.5; done
cp "$work/ca.pem" /usr/local/share/ca-certificates/agentd-ci-fake.crt
update-ca-certificates >/dev/null
end

step "binary, env files and units (agentd.md §2, §3)"
CGO_ENABLED=0 go build -trimpath -o "$work/clawbridge-agentd" ./cmd/clawbridge-agentd
install -d -m 0755 /usr/local/libexec/clawbridge
install -o root -g root -m 0755 "$work/clawbridge-agentd" /usr/local/libexec/clawbridge/clawbridge-agentd
install -d -o root -g root -m 0700 /etc/clawbridge-agentd
install -d -o cbagentd -g cbagentd -m 0755 /var/lib/clawbridge-agentd/policy
spec="CLAWBRIDGE_SERVER_URL=https://bridge.test:8443
CLAWBRIDGE_AGENT_IMAGE=$agent_id
CLAWBRIDGE_EGRESS_IMAGE=$egress_id
CLAWBRIDGE_EGRESS_POLICY_DIR=/var/lib/clawbridge-agentd/policy"
printf '%s\n' "$spec" > /etc/clawbridge-agentd/proxy.env
chmod 0644 /etc/clawbridge-agentd/proxy.env
(umask 077; printf '%s\nCLAWBRIDGE_AGENTD_DOCKER=/run/clawbridge-agentd/docker.sock\n' "$spec" > /etc/clawbridge-agentd/agentd.env)
(umask 077; printf 'CLAWBRIDGE_LAUNCHER_TOKEN=%s\n' "$token" > /etc/clawbridge-agentd/launcher.env)
install -m 0644 deploy/systemd/agentd/clawbridge-agentd-proxy.service deploy/systemd/agentd/clawbridge-agentd.service /etc/systemd/system/
systemctl daemon-reload
sh deploy/agentd-check.sh
systemctl enable --now clawbridge-agentd-proxy clawbridge-agentd
end

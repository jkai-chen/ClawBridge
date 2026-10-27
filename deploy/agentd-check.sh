#!/bin/sh
# Checks a clawbridge-agentd install (docs/operations/agentd.md) and changes
# nothing. Run as root on the server host: it reads root's env files, but
# prints only PASS or FAIL lines, never a file's contents. It exits 1 if any
# check fails.
#
#   sudo sh deploy/agentd-check.sh            # the files, users and groups
#   sudo sh deploy/agentd-check.sh --running  # also the units, sockets and memory
set -u

etc=/etc/clawbridge-agentd
bin=/usr/local/libexec/clawbridge/clawbridge-agentd
policy=/var/lib/clawbridge-agentd/policy
proxy_socket=/run/clawbridge-agentd/docker.sock
running=false
[ "${1:-}" = --running ] && running=true

failed=0
pass() { echo "PASS  $1"; }
fail() { echo "FAIL  $1"; failed=1; }
check() { # description, then a command that succeeds when it holds
  what=$1
  shift
  if "$@" >/dev/null 2>&1; then pass "$what"; else fail "$what"; fi
}
# mode owner group, as stat prints them, or nothing for a missing path.
perms() { stat -c '%a %U %G' "$1" 2>/dev/null; }
is() { [ "$(perms "$1")" = "$2" ]; }
in_group() { id -nG "$1" 2>/dev/null | tr ' ' '\n' | grep -qx "$2"; }

[ "$(id -u)" = 0 ] || { echo "run as root: the env files are root's"; exit 2; }

# Users and groups: agentd reaches Docker only through the proxy's group,
# and neither user can reach the root daemon.
check "user cbagent exists" id cbagent
check "user cbagentd exists" id cbagentd
check "group cbagent-sock exists" getent group cbagent-sock
check "cbagentd is in cbagent-sock" in_group cbagentd cbagent-sock
check "cbagent and cbagentd are two users" test "$(id -u cbagent 2>/dev/null)" != "$(id -u cbagentd 2>/dev/null)"
for user in cbagent cbagentd; do
  if in_group "$user" docker; then fail "$user is not in the docker group"; else pass "$user is not in the docker group"; fi
done

uid=$(id -u cbagent 2>/dev/null || echo none)
check "cbagent lingers (its dockerd starts at boot)" test -e "/var/lib/systemd/linger/cbagent"
check "delegate.conf for cbagent's user manager only" test -f "/etc/systemd/system/user@$uid.service.d/delegate.conf"
if [ -f /etc/systemd/system/user@.service.d/delegate.conf ]; then
  fail "no delegate.conf for every user (user@.service.d)"
else
  pass "no delegate.conf for every user (user@.service.d)"
fi

# The files: the binary, the env files and the policy directory.
check "the binary is root 0755" is "$bin" "755 root root"
check "$etc is root 0700" is "$etc" "700 root root"
check "proxy.env is root 0644" is "$etc/proxy.env" "644 root root"
check "agentd.env is root 0600" is "$etc/agentd.env" "600 root root"
check "launcher.env is root 0600" is "$etc/launcher.env" "600 root root"
if grep -q '^CLAWBRIDGE_LAUNCHER_TOKEN=' "$etc/proxy.env" "$etc/agentd.env" 2>/dev/null; then
  fail "the launcher token is only in launcher.env"
else
  pass "the launcher token is only in launcher.env"
fi
check "launcher.env holds one launcher token" test "$(grep -c '^CLAWBRIDGE_LAUNCHER_TOKEN=cbl_' "$etc/launcher.env" 2>/dev/null)" = 1
check "no mint directory is left" test ! -e "$etc/mint"
# The seal key opens every owner's provider key: it is the api's only.
check "no seal key in agentd's files" test "$(cat "$etc"/* 2>/dev/null | grep -c '^CLAWBRIDGE_MODEL_KEY_SEAL_KEY')" = 0
check "the policy directory is cbagentd 0755" is "$policy" "755 cbagentd cbagentd"
# The proxy and agentd must build the same specs.
for key in CLAWBRIDGE_SERVER_URL CLAWBRIDGE_AGENT_IMAGE CLAWBRIDGE_EGRESS_IMAGE CLAWBRIDGE_EGRESS_POLICY_DIR; do
  a=$(grep "^$key=" "$etc/proxy.env" 2>/dev/null)
  b=$(grep "^$key=" "$etc/agentd.env" 2>/dev/null)
  if [ -n "$a" ] && [ "$a" = "$b" ]; then pass "$key is the same for the proxy and agentd"; else fail "$key is the same for the proxy and agentd"; fi
done

# Room for the images and logs in cbagent's daemon.
home=$(getent passwd cbagent | cut -d: -f6)
free=$(df -Pk "${home:-/}" 2>/dev/null | awk 'NR==2 {print $4}')
check "at least 2 GiB free for cbagent's daemon" test "${free:-0}" -ge 2097152

if $running; then
  check "clawbridge-agentd-proxy is active" systemctl is-active clawbridge-agentd-proxy
  check "clawbridge-agentd is active" systemctl is-active clawbridge-agentd
  check "cbagent's dockerd is active" systemctl --user -M cbagent@ is-active docker
  check "the proxy socket is 0660 cbagent:cbagent-sock" is "$proxy_socket" "660 cbagent cbagent-sock"
  check "the daemon's socket is cbagent's" test "$(stat -c %U "/run/user/$uid/docker.sock" 2>/dev/null)" = cbagent
  # agentd's only way to Docker is the proxy.
  if su -s /bin/sh -c "test -r /run/user/$uid/docker.sock || test -w /run/user/$uid/docker.sock" cbagentd 2>/dev/null; then
    fail "cbagentd can't open the daemon's own socket"
  else
    pass "cbagentd can't open the daemon's own socket"
  fi
  cgroup="/sys/fs/cgroup/user.slice/user-$uid.slice/user@$uid.service/cgroup.controllers"
  for c in cpu memory pids; do
    check "cbagent's user manager has the $c controller" grep -qw "$c" "$cgroup"
  done
  # One agent (160 MiB) must still leave 300 MiB (cap 1).
  avail=$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo)
  check "MemAvailable leaves 300 MiB after one agent" test "${avail:-0}" -ge $(( (160 + 300) * 1024 ))
fi

if [ "$failed" = 0 ]; then echo "agentd-check: all passed"; else echo "agentd-check: FAILED"; fi
exit "$failed"

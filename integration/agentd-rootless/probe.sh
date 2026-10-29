#!/usr/bin/env bash
# agentd's real-daemon CI job, part 2: after setup.sh, checks what a fake
# daemon can't show (cmd/clawbridge-agentd/README.md, "Real-daemon tests"):
# agentd's own round against the fake server, the containers' limits as
# the kernel applies them, the proxy's boundary, the rootless layer with the
# proxy bypassed, and the fixed memory overhead. CI only, as root.
set -uo pipefail
[ "${GITHUB_ACTIONS:-}" = true ] && [ "$(id -u)" = 0 ] || { echo "CI only, as root"; exit 2; }

work=/var/tmp/agentd-ci
uid=$(id -u cbagent)
sock=/run/user/$uid/docker.sock
proxy=/run/clawbridge-agentd/docker.sock
owner=p_ciowner0001
busybox=busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
cbdocker() { sudo -u cbagent DOCKER_HOST="unix://$sock" docker "$@"; }
failed=0
pass() { echo "PASS  $1"; }
fail() { echo "FAIL  $1"; failed=1; }
expect() { # description, expected, actual
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (expected $2, got $3)"; fi
}

echo "::group::agentd's round against the fake server"
for _ in $(seq 90); do
  grep -q '"call":"credential","owner_ref":"'$owner'","status":200' "$work/fakelauncher.log" &&
    cbdocker inspect clawbridge-egress >/dev/null 2>&1 && break
  sleep 2
done
grep -q '"call":"wanted","status":200' "$work/fakelauncher.log" && pass "agentd asked for the wanted set" || fail "agentd asked for the wanted set"
grep -q '"call":"credential","owner_ref":"'$owner'","status":200' "$work/fakelauncher.log" && pass "agentd minted the owner's credential" || fail "agentd minted the owner's credential"
if grep -q '"status":401' "$work/fakelauncher.log"; then fail "the launcher token was accepted"; else pass "the launcher token was accepted"; fi
echo "::endgroup::"

echo "::group::the egress container, as the kernel sees it"
expect "egress is running" true "$(cbdocker inspect -f '{{.State.Running}}' clawbridge-egress 2>/dev/null)"
expect "egress runs as 65534" 65534:65534 "$(cbdocker inspect -f '{{.Config.User}}' clawbridge-egress 2>/dev/null)"
expect "egress has a read-only root" true "$(cbdocker inspect -f '{{.HostConfig.ReadonlyRootfs}}' clawbridge-egress 2>/dev/null)"
expect "egress has 32 MiB" 33554432 "$(cbdocker inspect -f '{{.HostConfig.Memory}}' clawbridge-egress 2>/dev/null)"
pid=$(cbdocker inspect -f '{{.State.Pid}}' clawbridge-egress 2>/dev/null)
cg=/sys/fs/cgroup$(cut -d: -f3 "/proc/$pid/cgroup" 2>/dev/null)
expect "egress's cgroup memory.max" 33554432 "$(cat "$cg/memory.max" 2>/dev/null)"
expect "egress's cgroup has a CPU quota" yes "$(awk '{print ($1 != "max") ? "yes" : "no"}' "$cg/cpu.max" 2>/dev/null)"
expect "egress's capabilities" 0000000000000000 "$(awk '/^CapEff/ {print $2}' "/proc/$pid/status" 2>/dev/null)"
echo "::endgroup::"

echo "::group::the owner's network and agent"
expect "the owner's network is internal" true "$(cbdocker network inspect -f '{{.Internal}}' cbnet-$owner 2>/dev/null)"
subnet=$(cbdocker network inspect -f '{{range .IPAM.Config}}{{.Subnet}}{{end}}' cbnet-$owner 2>/dev/null)
case "$subnet" in 172.30.*/28) pass "the owner's network is a /28 of 172.30.0.0/16 ($subnet)" ;; *) fail "the owner's network is a /28 of 172.30.0.0/16 ($subnet)" ;; esac
expect "egress is on the owner's network" yes "$(cbdocker network inspect -f '{{range .Containers}}{{.Name}} {{end}}' cbnet-$owner 2>/dev/null | grep -qw clawbridge-egress && echo yes || echo no)"
# The agent can't reach its server here (bridge.test is only in the
# runner's /etc/hosts), so it may be between restarts: catch it created.
spec=""
for _ in $(seq 30); do
  spec=$(cbdocker inspect -f '{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}} {{.HostConfig.NanoCpus}} {{.HostConfig.PidsLimit}} {{.HostConfig.ReadonlyRootfs}} {{.HostConfig.NetworkMode}} {{.Config.User}}' clawbridge-agent-$owner 2>/dev/null) && [ -n "$spec" ] && break
  sleep 1
done
expect "the agent's limits and network" "167772160 167772160 500000000 64 true cbnet-$owner node" "$spec"
echo "::endgroup::"

echo "::group::the proxy's boundary, as cbagentd"
code() { sudo -u cbagentd curl -s -o /dev/null -w '%{http_code}' --max-time 10 --unix-socket "$@"; }
agent_id=$(cbdocker image inspect -f '{{.Id}}' clawbridge-dispatcher-agent)
expect "a privileged create through the proxy" 403 \
  "$(code "$proxy" -X POST -H 'Content-Type: application/json' -d '{"Image":"'"$agent_id"'","HostConfig":{"Privileged":true}}' "http://d/containers/create?name=clawbridge-agent-p_cievil0001")"
expect "a host bind through the proxy" 403 \
  "$(code "$proxy" -X POST -H 'Content-Type: application/json' -d '{"Image":"'"$agent_id"'","HostConfig":{"Binds":["/:/host"]}}' "http://d/containers/create?name=clawbridge-agent-p_cievil0001")"
expect "an exec through the proxy" 403 "$(code "$proxy" -X POST -H 'Content-Type: application/json' -d '{"Cmd":["sh"]}' "http://d/containers/clawbridge-egress/exec")"
expect "/info through the proxy" 403 "$(code "$proxy" "http://d/info")"
expect "cbagentd on the daemon's own socket" 000 "$(code "$sock" "http://d/_ping")"
echo "::endgroup::"

echo "::group::the rootless layer, with the proxy bypassed (as cbagent)"
install -d -m 0755 /var/lib/agentd-ci
echo "root-only-agentd-ci-secret" > /var/lib/agentd-ci/secret
chmod 0600 /var/lib/agentd-ci/secret
out=$(cbdocker run --rm -v /var/lib/agentd-ci/secret:/s:ro "$busybox" cat /s 2>&1)
case "$out" in *root-only-agentd-ci-secret*) fail "a container can't read a root-only host file" ;; *) pass "a container can't read a root-only host file" ;; esac
cbdocker network create --internal --subnet 172.30.255.0/28 cbnet-ciprobe >/dev/null
if cbdocker run --rm --network cbnet-ciprobe "$busybox" nslookup -timeout=5 example.com >/dev/null 2>&1; then
  fail "an internal network's DNS doesn't resolve outside names"
else
  pass "an internal network's DNS doesn't resolve outside names"
fi
python3 -m http.server 18080 --bind 0.0.0.0 >/dev/null 2>&1 &
web=$!
sleep 1
gw=$(cbdocker network inspect -f '{{range .IPAM.Config}}{{.Gateway}}{{end}}' cbnet-ciprobe)
if cbdocker run --rm --network cbnet-ciprobe "$busybox" wget -T 5 -qO- "http://$gw:18080/" >/dev/null 2>&1; then
  fail "the internal network's gateway exposes no host service"
else
  pass "the internal network's gateway exposes no host service"
fi
if cbdocker run --rm --network clawbridge-egress-net "$busybox" wget -T 5 -qO- "http://10.0.2.2:18080/" >/dev/null 2>&1; then
  fail "the egress network doesn't reach the host's loopback"
else
  pass "the egress network doesn't reach the host's loopback"
fi
kill "$web"
cbdocker network rm cbnet-ciprobe >/dev/null
echo "::endgroup::"

echo "::group::a leftover owner network without egress"
# Docker answers a disconnect of a container not on a network with a 500:
# agentd must remove such a network without one, in its next round.
cbdocker network create --internal --subnet 172.30.1.0/28 cbnet-p_ciorphan01 >/dev/null
gone=no
for _ in $(seq 40); do
  cbdocker network inspect cbnet-p_ciorphan01 >/dev/null 2>&1 || { gone=yes; break; }
  sleep 2
done
expect "agentd removes a leftover network the egress proxy isn't on" yes "$gone"
if journalctl -u clawbridge-agentd --no-pager | grep -q '"ref":"p_ciorphan01".*action failed'; then
  fail "no failed action for the leftover network"
else
  pass "no failed action for the leftover network"
fi
rm -rf /var/lib/agentd-ci
echo "::endgroup::"

echo "::group::the install check, and no secret in the logs"
if sh deploy/agentd-check.sh --running; then pass "agentd-check --running"; else fail "agentd-check --running"; fi
expect "no token or credential in agentd's and the proxy's logs" 0 \
  "$(journalctl -u clawbridge-agentd -u clawbridge-agentd-proxy --no-pager | grep -c -E 'cbl_|cba_')"
echo "::endgroup::"

echo "::group::the fixed memory overhead"
summary=${GITHUB_STEP_SUMMARY:-/dev/null}
{
  echo "### agentd's fixed memory overhead (RSS, MiB)"
  echo
  echo "| process | user | MiB |"
  echo "|---|---|---|"
  total=0
  while read -r rss user comm; do
    echo "| $comm | $user | $((rss / 1024)) |"
    total=$((total + rss))
  done < <(ps -eo rss=,user=,comm= | awk '$2 == "cbagent" || $2 == "cbagentd"')
  egress=$(cat "$cg/memory.current" 2>/dev/null || echo 0)
  echo "| egress container (memory.current) | | $((egress / 1048576)) |"
  echo "| **total** | | **$(( (total * 1024 + egress) / 1048576 ))** |"
} | tee -a "$summary"
echo "::endgroup::"

exit "$failed"

#!/usr/bin/env bash
# The container test (design §5, §8): builds the image and runs the agent in it with the production hardening, against
# fakes that share only a loopback network with it. Checks, over CASES cases (default 1000):
# - every case gets its outcome, and every message matches v1.schema.json;
# - memory: no OOM under --memory 160m, and the cgroup's peak is reported;
# - egress: a preload records any non-loopback connect or DNS lookup (none allowed), with a negative control;
# - hardening: non-root, read-only root, no capabilities, no package manager.
# Removes its containers by name on exit. Needs docker and a prior `npm ci` (the fakes use the dev dependencies).
set -euo pipefail

DIR=$(cd "$(dirname "$0")/.." && pwd)
CASES=${CASES:-1000}
IMAGE=${IMAGE:-clawbridge-dispatcher-agent:test}
NODE_IMAGE=$(sed -n 's/^FROM \(node:[^ ]*\) AS deps$/\1/p' "$DIR/Dockerfile")
FAKES=cb-agent-test-fakes-$$
AGENT=cb-agent-test-agent-$$
CREDENTIAL=cba_Q7x2Lk9PzR4mWv8NcT3jYb # the fake ClawBridge's test credential
LOG=$(mktemp)

cleanup() {
	docker rm -f -v "$AGENT" "$FAKES" >/dev/null 2>&1 || true
	rm -f "$LOG"
}
trap cleanup EXIT
fail() {
	echo "FAIL: $*" >&2
	echo "--- agent log (tail)" >&2
	tail -n 20 "$LOG" >&2 || true
	exit 1
}

docker build -q -t "$IMAGE" "$DIR" >/dev/null

# The fakes have no network at all; the agent joins their namespace, so loopback is the only thing it can reach.
docker run -d --name "$FAKES" --network none -v "$DIR:/src:ro" -w /src "$NODE_IMAGE" \
	node --disable-warning=ExperimentalWarning test/container/serve-fakes.ts "$CASES" >/dev/null
for _ in $(seq 1 30); do
	docker logs "$FAKES" 2>&1 | grep -q '"ready":true' && break
	sleep 1
done
docker logs "$FAKES" 2>&1 | grep -q '"ready":true' || fail "fakes did not start"

# The production flags (design §5.3), with the egress guard preloaded by --import.
printf '%s\n' "$CREDENTIAL" | docker run -i --name "$AGENT" --network "container:$FAKES" \
	--read-only --tmpfs /tmp:rw,noexec,nosuid,size=8m --cap-drop ALL --security-opt no-new-privileges \
	--pids-limit 64 --memory 160m --memory-swap 160m --user node \
	-v "$DIR/test/container/egress-guard.mjs:/guard/egress-guard.mjs:ro" \
	-e CLAWBRIDGE_SERVER_URL=http://127.0.0.1:18080 \
	--entrypoint node "$IMAGE" \
	--max-old-space-size=96 --disable-warning=ExperimentalWarning --import /guard/egress-guard.mjs src/main.ts \
	>"$LOG" 2>&1 &
RUN_PID=$!
for _ in $(seq 1 30); do
	[ "$(docker inspect -f '{{.State.Status}}' "$AGENT" 2>/dev/null || true)" = running ] && break
	kill -0 "$RUN_PID" 2>/dev/null || break
	sleep 1
done

done_line=""
for _ in $(seq 1 300); do
	done_line=$(docker logs "$FAKES" 2>&1 | grep '"done":true' || true)
	[ -n "$done_line" ] && break
	[ "$(docker inspect -f '{{.State.Running}}' "$AGENT" 2>/dev/null || echo false)" = true ] || fail "the agent exited early"
	sleep 1
done
[ -n "$done_line" ] || fail "not every case got an outcome within 300 s"

peak=$(docker exec "$AGENT" cat /sys/fs/cgroup/memory.peak)
oom=$(docker exec "$AGENT" sh -c "grep '^oom_kill ' /sys/fs/cgroup/memory.events | cut -d' ' -f2")
hardening=$(docker exec "$AGENT" sh -c '
	id -u
	touch /app/x 2>/dev/null && echo writable || echo read-only
	grep ^CapEff /proc/self/status | tr -d "\t" | cut -d: -f2
	command -v npm >/dev/null && echo has-npm || echo no-npm')
control=$(docker exec "$AGENT" node --import /guard/egress-guard.mjs \
	-e 'fetch("https://example.com").catch(() => {})' 2>&1 || true)

docker stop -t 10 "$AGENT" >/dev/null
wait "$RUN_PID" || true
exit_code=$(docker inspect -f '{{.State.ExitCode}}' "$AGENT")
oom_killed=$(docker inspect -f '{{.State.OOMKilled}}' "$AGENT")
egress=$(grep -o '{"egress_attempts":\[[^]]*\]}' "$LOG" | tail -n 1)

echo "outcomes: $done_line"
echo "memory.peak: $((peak / 1048576)) MiB (limit 160 MiB), oom_kill: $oom, OOMKilled: $oom_killed, exit: $exit_code"
echo "hardening: $(echo "$hardening" | tr '\n' ' ')"
echo "egress: $egress; negative control: $control"

echo "$done_line" | grep -q "\"outcomes\":$CASES,\"decide\":$CASES,\"violations\":0," || fail "outcomes or schema violations"
[ "$oom" = 0 ] && [ "$oom_killed" = false ] || fail "the agent ran out of memory"
[ "$exit_code" = 0 ] || fail "the agent exited with $exit_code on SIGTERM"
[ "$egress" = '{"egress_attempts":[]}' ] || fail "egress beyond loopback: $egress"
echo "$control" | grep -q 'example\.com' || fail "the egress guard's negative control was not recorded"
[ "$hardening" = "$(printf '1000\nread-only\n0000000000000000\nno-npm')" ] || fail "hardening: $hardening"
echo "container test passed"

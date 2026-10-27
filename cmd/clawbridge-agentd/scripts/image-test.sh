#!/usr/bin/env bash
# Builds the agentd image and runs its egress proxy as the fixed spec does
# (read-only, no capabilities, user 65534, 32 MiB, the policy read-only),
# then checks it answers and refuses a source on no owner's network.
# Run from the repository root. Removes what it made, by name: the test
# image and container. Meant for CI: run locally, it leaves the golang base
# and the Go module download in Docker's build cache, which this script
# does not prune.
set -euo pipefail

tag=clawbridge-agentd-image-test
name=clawbridge-agentd-image-test
policy=$(mktemp -d)
cleanup() {
  docker rm -f -v "$name" >/dev/null 2>&1 || true
  docker rmi "$tag" >/dev/null 2>&1 || true
  rm -rf "$policy"
}
trap cleanup EXIT

docker build -q -f cmd/clawbridge-agentd/Dockerfile -t "$tag" . >/dev/null
id=$(docker image inspect "$tag" --format '{{.Id}}')
user=$(docker image inspect "$tag" --format '{{.Config.User}}')
entry=$(docker image inspect "$tag" --format '{{json .Config.Entrypoint}}')
size=$(docker image inspect "$tag" --format '{{.Size}}')
[ "$user" = "65534:65534" ] || { echo "image user is $user, not 65534:65534"; exit 1; }
[ "$entry" = '["/clawbridge-agentd"]' ] || { echo "entrypoint is $entry"; exit 1; }
[ "$size" -lt $((40 << 20)) ] || { echo "image is $size bytes, over 40 MiB"; exit 1; }

printf '{"networks":[]}' > "$policy/egress.json"
chmod 0755 "$policy"
chmod 0644 "$policy/egress.json"
# By image ID, as the deploy pins it.
docker run -d --name "$name" --read-only --cap-drop ALL --security-opt no-new-privileges \
  --memory 32m --memory-swap 32m --pids-limit 32 --user 65534:65534 \
  -v "$policy:/policy:ro" -p 127.0.0.1:13128:3128 \
  "$id" egress --listen 0.0.0.0:3128 --policy /policy/egress.json >/dev/null

for _ in $(seq 20); do
  docker logs "$name" 2>&1 | grep -q 'egress proxy listening' && break
  sleep 0.5
done
[ "$(docker inspect "$name" --format '{{.State.Running}}')" = true ] || { docker logs "$name"; exit 1; }
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -p -x http://127.0.0.1:13128 https://example.invalid/ 2>/dev/null || true)
docker logs "$name" 2>&1 | grep -q 'the source is on no owner'"'"'s network' || { echo "no refusal logged (CONNECT answered $code)"; docker logs "$name"; exit 1; }
echo "agentd image: $id, $((size >> 20)) MiB, user $user; egress refuses an unknown source"
docker stats --no-stream --format 'egress memory: {{.MemUsage}}' "$name"

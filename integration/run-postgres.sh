#!/usr/bin/env bash
# Explicit production-store integration runner. Never reuses another container.
set -euo pipefail
command -v docker >/dev/null || { echo 'Postgres verification requested but Docker is unavailable' >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo 'Postgres verification requested but Docker daemon is unavailable' >&2; exit 1; }
review_root="$(cd "$(dirname "$0")/.." && pwd)"
review_container=''
review_volume=''
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [[ -n "$review_container" ]]; then
    if docker rm -f -v "$review_container" >/dev/null; then
      printf 'Postgres cleanup: removed container %s and attached anonymous volume\n' "$review_container"
      if [[ -n "${CLAWBRIDGE_TEST_RESOURCE_LOG:-}" ]]; then
        printf 'Cleanup: container `%s` removed with anonymous volume.\n' "$review_container" >> "$CLAWBRIDGE_TEST_RESOURCE_LOG"
      fi
      if [[ -n "$review_volume" ]] && docker volume inspect "$review_volume" >/dev/null 2>&1; then
        echo "ERROR: reviewer volume still exists: $review_volume" >&2
        status=1
      fi
    else
      echo "ERROR: failed to remove reviewer container $review_container" >&2
      status=1
    fi
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
review_password="$(python3 -c 'import secrets; print(secrets.token_hex(24))')"
# Docker receives the password through its environment, never an interpolated CLI argument.
export POSTGRES_PASSWORD="$review_password"
review_container="$(docker run -d --name "clawbridge-review-$(python3 -c 'import secrets;print(secrets.token_hex(6))')" -e POSTGRES_PASSWORD -e POSTGRES_USER=clawbridge_test -e POSTGRES_DB=clawbridge_test -p 127.0.0.1::5432 postgres:17-alpine)"
review_port="$(docker inspect -f '{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}' "$review_container")"
review_volume="$(docker inspect -f '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}{{end}}{{end}}' "$review_container")"
printf 'Reviewer Postgres container=%s loopback_port=%s anonymous_volume=%s\n' "$review_container" "$review_port" "$review_volume"
if [[ -n "${CLAWBRIDGE_TEST_RESOURCE_LOG:-}" ]]; then
  printf '\nPostgres test run: container `%s`; loopback `127.0.0.1:%s`; anonymous volume `%s`.\n' "$review_container" "$review_port" "$review_volume" >> "$CLAWBRIDGE_TEST_RESOURCE_LOG"
fi
review_ready=false
for ((review_attempt=0;review_attempt<60;review_attempt++)); do
  if docker exec "$review_container" pg_isready -h 127.0.0.1 -U clawbridge_test -d clawbridge_test >/dev/null 2>&1; then review_ready=true; break; fi
  sleep 1
done
[[ "$review_ready" == true ]] || { echo 'Disposable Postgres did not become ready' >&2; exit 1; }
docker exec "$review_container" psql --version
export CLAWBRIDGE_REQUIRE_POSTGRES=1
export CLAWBRIDGE_TEST_POSTGRES_DSN="postgres://clawbridge_test:${review_password}@127.0.0.1:${review_port}/clawbridge_test?sslmode=disable"
unset POSTGRES_PASSWORD review_password
cd "$review_root"
if [[ $# == 0 ]]; then set -- go test ./... -count=1; fi
"$@"

#!/usr/bin/env bash
# Headless-browser check of the built UI: starts `clawbridge ui --no-open` on
# a throwaway local profile, opens its link in headless Chrome and asserts
# the page works under the server's real headers (headless.mjs).
#   usage: web/e2e/run.sh PATH_TO_CLAWBRIDGE   (CHROME_PATH names the browser)
set -euo pipefail
[[ $# == 1 ]] || { echo "usage: run.sh CLAWBRIDGE_BINARY" >&2; exit 2; }
bin=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
pid=
cleanup() {
  [[ -z $pid ]] || kill "$pid" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
mkdir -p "$work/home"
# A clean environment: no agent marker, no caller profile.
run() { env -i PATH="$PATH" HOME="$work/home" CLAWBRIDGE_HOME="$work/home" "$@"; }
run "$bin" setup --local --workspace w >/dev/null
run "$bin" project create p >/dev/null
# exec: $! is the server itself, so the trap stops it, not a wrapper shell.
( exec env -i PATH="$PATH" HOME="$work/home" CLAWBRIDGE_HOME="$work/home" "$bin" ui --no-open >"$work/ui.out" 2>"$work/ui.err" ) &
pid=$!
link=
for _ in $(seq 100); do
  link=$(grep -o 'http://127.0.0.1:[0-9]*/?t=[0-9a-f]*' "$work/ui.out" | head -1 || true)
  [[ -z $link ]] || break
  kill -0 "$pid" 2>/dev/null || { echo "clawbridge ui exited:" >&2; cat "$work/ui.out" "$work/ui.err" >&2; exit 1; }
  sleep 0.2
done
[[ -n $link ]] || { echo "no link printed" >&2; cat "$work/ui.out" "$work/ui.err" >&2; exit 1; }
node "$here/headless.mjs" "$link"

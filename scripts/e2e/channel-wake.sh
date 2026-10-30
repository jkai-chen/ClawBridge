#!/usr/bin/env bash
# FR26 end-to-end check: an idle Claude Code session wakes through
# ClawBridge's channel plugin. Run by hand only, never in CI: it needs a
# Claude Code login (claude.ai or Console), and it starts ONE throwaway
# Claude Code session under expect, which costs a few model turns.
#
#   scripts/e2e/channel-wake.sh [--with-allowlist] [--strict]
#
#   (default)         launch with --dangerously-load-development-channels
#                     plugin:clawbridge@clawbridge: it skips only the
#                     allowlist check, for a machine without the admin step
#   --with-allowlist  launch with --channels plugin:clawbridge@clawbridge;
#                     needs `sudo clawbridge channel allow --write` done
#   --strict          then launch again with --mcp-config FILE
#                     --strict-mcp-config and report whether the plugin's
#                     wake server starts there (docs/notifications.md's
#                     FR26-STRICT-RESULT line)
#
# Everything ClawBridge writes lives in a scratch folder (its own database,
# CLAWBRIDGE_HOME and binary); the plugin is added from this checkout at
# project scope in that folder. At the end the session is exited (and
# killed by its PID if still alive), the plugin and marketplace this run
# added are removed, and the scratch folder and the session's transcript
# folder are deleted by exact path. Claude Code's own record that the
# scratch folder was trusted (in its global config) is left; the summary
# names it.
#
# Overrides (environment): WAKE_LIMIT (seconds for the wake, default 40:
# a local database is polled every 30 s), E2E_TRUST_KEYS (default
# Down,Enter), E2E_MCP_KEYS (default Up,Up,Enter), E2E_DEV_KEYS (default
# Enter), E2E_READY_PATTERN (a regexp for the idle prompt, default
# "shortcuts|Try"). The dialog patterns are single words: Claude Code
# draws its text with cursor moves between words.
set -euo pipefail

die() { echo "channel-wake: $*" >&2; exit 2; }

MODE=dev STRICT=0
for arg in "$@"; do
  case $arg in
    --with-allowlist) MODE=allowlist ;;
    --strict) STRICT=1 ;;
    -h|--help) awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"; exit 0 ;;
    *) die "unknown argument $arg (see --help)" ;;
  esac
done

for tool in claude expect go python3; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is not on PATH"
done

REPO=$(cd "$(dirname "$0")/../.." && pwd -P)
[ -f "$REPO/.claude-plugin/marketplace.json" ] || die "no marketplace at $REPO"
SESSION=wake-e2e
INSTANCE=wake-e2e-1
SENDER=wake-e2e-sender
WAKE_LIMIT=${WAKE_LIMIT:-40}
PLUGIN=plugin:clawbridge@clawbridge
CLAUDE_CONFIG=${CLAUDE_CONFIG_DIR:-$HOME/.claude}

if [ "$MODE" = allowlist ]; then
  CHANNEL_FLAG=(--channels "$PLUGIN")
  case $(uname -s) in
    Darwin) dropin="/Library/Application Support/ClaudeCode/managed-settings.d/50-clawbridge-channels.json" ;;
    *) dropin=/etc/claude-code/managed-settings.d/50-clawbridge-channels.json ;;
  esac
  [ -f "$dropin" ] || echo "channel-wake: warning: $dropin is missing; the push will be dropped unless MDM or the org allowlists the plugin" >&2
else
  CHANNEL_FLAG=(--dangerously-load-development-channels "$PLUGIN")
fi

# A clawbridge marketplace already known to Claude Code is someone's real
# setup: this run would shadow it and its cleanup would remove it.
had_market=$(python3 -I - "$CLAUDE_CONFIG/plugins/known_marketplaces.json" <<'EOF'
import json, sys
try:
    print("yes" if "clawbridge" in json.load(open(sys.argv[1])) else "no")
except (OSError, ValueError):
    print("no")
EOF
)
[ "$had_market" = no ] || die "Claude Code already knows a clawbridge marketplace; run this where it doesn't (or remove it first)"

# Scratch: the Claude Code project folder, ClawBridge's state, its database
# and binary. Every path below is under it.
SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/clawbridge-channel-e2e.XXXXXX")
SCRATCH=$(cd "$SCRATCH" && pwd -P)
case $(basename "$SCRATCH") in clawbridge-channel-e2e.*) ;; *) die "unexpected scratch path $SCRATCH" ;; esac
WORK=$SCRATCH/work
# Claude Code's folder for this scratch folder's sessions (its transcripts).
PROJECT_DIR=$CLAUDE_CONFIG/projects/$(printf '%s' "$WORK" | sed 's/[^A-Za-z0-9]/-/g')
STATE=$SCRATCH/state
CTX=$STATE/context
DB=$SCRATCH/bus.db
BIN=$SCRATCH/bin
mkdir -p "$WORK" "$STATE" "$BIN"

# Only this run's ClawBridge settings reach the commands and the session.
while IFS='=' read -r name _; do
  case $name in CLAWBRIDGE_*) unset "$name" ;; esac
done < <(env)
export CLAWBRIDGE_HOME=$STATE
export PATH=$BIN:$PATH

CLAUDE_PID= EXPECT_PID= TRANSCRIPT_DIR= added_market=0
had_cache=no
[ -e "$CLAUDE_CONFIG/plugins/cache/clawbridge" ] && had_cache=yes
RESULTS=() FAILED=0 STRICT_RESULT="not run"
pass() { RESULTS+=("PASS  $*"); echo "channel-wake: PASS $*"; }
fail() { RESULTS+=("FAIL  $*"); FAILED=1; echo "channel-wake: FAIL $*" >&2; }
info() { RESULTS+=("INFO  $*"); echo "channel-wake: $*"; }

# json FILE EXPR: a Python expression over the parsed file `d`, or empty.
json() {
  python3 -I - "$1" "$2" <<'EOF' 2>/dev/null || true
import json, sys
try:
    d = json.load(open(sys.argv[1]))
    v = eval(sys.argv[2], {}, {"d": d})
    print("" if v is None else v)
except Exception:
    pass
EOF
}

# since FILE KEY T0: whether the RFC3339 time at KEY in FILE is at or after
# the Unix time T0.
since() {
  python3 -I - "$1" "$2" "$3" <<'EOF' 2>/dev/null || echo no
import json, re, sys
from datetime import datetime
try:
    v = json.load(open(sys.argv[1])).get(sys.argv[2], "")
    v = re.sub(r"\.(\d{1,6})\d*", r".\1", v).replace("Z", "+00:00")
    print("yes" if datetime.fromisoformat(v).timestamp() >= float(sys.argv[3]) else "no")
except Exception:
    print("no")
EOF
}

now() { python3 -I -c 'import time; print(time.time())'; }

alive() { [ -n "${1:-}" ] && kill -0 "$1" 2>/dev/null; }

# stop_session: /exit through expect, then the PIDs, by exact number only.
stop_session() {
  [ -n "${READY_DIR:-}" ] && touch "$READY_DIR/exit"
  local i
  for i in $(seq 1 30); do
    alive "$EXPECT_PID" || alive "$CLAUDE_PID" || break
    sleep 1
  done
  for pid in $CLAUDE_PID $EXPECT_PID; do
    if alive "$pid"; then
      kill "$pid" 2>/dev/null || true
      sleep 2
      alive "$pid" && kill -9 "$pid" 2>/dev/null || true
    fi
  done
  [ -n "$EXPECT_PID" ] && wait "$EXPECT_PID" 2>/dev/null || true
  CLAUDE_PID= EXPECT_PID=
}

cleanup() {
  local status=$?
  set +e
  stop_session
  if [ "$added_market" = 1 ]; then
    (cd "$WORK" && claude plugin uninstall clawbridge@clawbridge --scope project >/dev/null 2>&1) || echo "channel-wake: left: the plugin's project install in the scratch folder (removed with it)" >&2
    (cd "$WORK" && claude plugin marketplace remove clawbridge >/dev/null 2>&1) || echo "channel-wake: left: the clawbridge marketplace; remove it with: claude plugin marketplace remove clawbridge" >&2
    # The plugin's cache folder, when this run made it.
    if [ "$had_cache" = no ] && [ -d "$CLAUDE_CONFIG/plugins/cache/clawbridge" ]; then
      rm -rf -- "$CLAUDE_CONFIG/plugins/cache/clawbridge"
    fi
  fi
  # The transcript folder is Claude Code's for exactly this scratch folder.
  if [ -d "$PROJECT_DIR" ]; then
    rm -rf -- "$PROJECT_DIR"
  fi
  if [ -n "$TRANSCRIPT_DIR" ] && [ "$TRANSCRIPT_DIR" != "$PROJECT_DIR" ] && [ -d "$TRANSCRIPT_DIR" ]; then
    echo "channel-wake: left: transcript folder $TRANSCRIPT_DIR (not the expected $PROJECT_DIR)" >&2
  fi
  rm -rf -- "$SCRATCH"
  echo "channel-wake: left: Claude Code's trust record for $WORK in its global config"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

echo "channel-wake: scratch $SCRATCH, mode $MODE"
if [ -n "${ANTHROPIC_BASE_URL:-}" ]; then
  info "ANTHROPIC_BASE_URL is set: a custom endpoint may never get channel events (the result then says so, not the code)"
fi
(cd "$REPO" && go build -o "$BIN/clawbridge" ./cmd/clawbridge) || die "build failed"

cb() { clawbridge --db "$DB" --workspace w --project p "$@" >/dev/null; }
cb setup --local
cb project create p
cb --session "$SESSION" --instance "$INSTANCE" register --agent-kind claude-code
cb --session "$SENDER" --instance "$SENDER-1" register
(cd "$WORK" && clawbridge --db "$DB" --workspace w --project p --session "$SESSION" --instance "$INSTANCE" \
  mcp install --agent claude-code --channel >/dev/null 2>"$SCRATCH/install.err") || { cat "$SCRATCH/install.err" >&2; die "mcp install --channel failed"; }
grep -q 'hook turn' "$WORK/.claude/settings.local.json" || die "mcp install --channel wrote no turn hook"

# The plugin, from this checkout, at project scope in the scratch folder.
added_market=1
(cd "$WORK" && claude plugin marketplace add "$REPO" --scope project) || die "claude plugin marketplace add failed"
(cd "$WORK" && claude plugin install clawbridge@clawbridge --scope project) || die "claude plugin install failed"

# The expect driver: answers the dialogs once each, says when the prompt
# is idle, and exits the session when told.
cat >"$SCRATCH/drive.exp" <<'EOF'
log_user 0
log_file -a $env(E2E_LOG)
set dir $env(E2E_DIR)
proc keys {spec} {
  foreach k [split $spec ","] {
    switch -- $k {
      Up {send "\033\[A"}
      Down {send "\033\[B"}
      Enter {send "\r"}
    }
    after 400
  }
}
spawn -noecho {*}$argv
set f [open "$dir/pid" w]; puts $f [exp_pid]; close $f
array set done {trust 0 mcp 0 dev 0}
set ready 0
set deadline [expr {[clock seconds] + 90}]
while {!$ready && [clock seconds] < $deadline} {
  expect {
    -timeout 2
    -nocase -re {trust} {
      if {!$done(trust)} { set done(trust) 1; after 800; keys $env(E2E_TRUST_KEYS) }
    }
    -re {Continue} {
      if {!$done(mcp)} { set done(mcp) 1; after 800; keys $env(E2E_MCP_KEYS) }
    }
    -nocase -re {development} {
      if {!$done(dev)} { set done(dev) 1; after 800; keys $env(E2E_DEV_KEYS) }
    }
    -re $env(E2E_READY_PATTERN) { set ready 1 }
    timeout {}
    eof { exit 3 }
  }
}
set f [open "$dir/ready" w]; puts $f $ready; close $f
while {![file exists "$dir/exit"]} {
  expect {
    -timeout 1
    -re {.+} {}
    timeout {}
    eof { exit 0 }
  }
}
send "/exit"
after 600
send "\r"
expect { -timeout 20 eof {} timeout {} }
exit 0
EOF

# launch NAME ARGS...: one Claude Code session, the session exported as the
# docs say, MCP tools of the clawbridge entry allowed so the agent can call
# inbox unattended.
launch() {
  local name=$1; shift
  READY_DIR=$SCRATCH/$name
  mkdir -p "$READY_DIR"
  (
    cd "$WORK" || exit 1
    export CLAWBRIDGE_SESSION=$SESSION CLAWBRIDGE_INSTANCE=$INSTANCE
    export E2E_LOG=$SCRATCH/$name.log E2E_DIR=$READY_DIR
    export E2E_TRUST_KEYS=${E2E_TRUST_KEYS:-Down,Enter} E2E_MCP_KEYS=${E2E_MCP_KEYS:-Up,Up,Enter}
    export E2E_DEV_KEYS=${E2E_DEV_KEYS:-Enter} E2E_READY_PATTERN=${E2E_READY_PATTERN:-shortcuts|Try}
    exec expect -f "$SCRATCH/drive.exp" claude "${CHANNEL_FLAG[@]}" --allowedTools mcp__clawbridge "$@"
  ) &
  EXPECT_PID=$!
  local i
  for i in $(seq 1 20); do [ -s "$READY_DIR/pid" ] && break; sleep 0.5; done
  CLAUDE_PID=$(cat "$READY_DIR/pid" 2>/dev/null || true)
  [ -n "$CLAUDE_PID" ] || { fail "$name: Claude Code did not start"; return 1; }
  for i in $(seq 1 100); do [ -s "$READY_DIR/ready" ] && break; alive "$EXPECT_PID" || break; sleep 1; done
  if [ "$(cat "$READY_DIR/ready" 2>/dev/null)" != 1 ]; then
    info "$name: the idle prompt was not seen; going on"
  fi
}

# show_log NAME: the end of a session's screen log, for a failure.
show_log() {
  [ -f "$SCRATCH/$1.log" ] || return 0
  echo "---- last screen output ($1) ----" >&2
  python3 -I - "$SCRATCH/$1.log" <<'EOF' >&2 || true
import re, sys
text = open(sys.argv[1], errors="replace").read()
text = re.sub(r"\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[\]P].*?(\x07|\x1b\\)|\x1b.", "", text)
print("\n".join([l for l in text.splitlines() if l.strip()][-40:]))
EOF
}

# --- The main run -----------------------------------------------------------
launch main || exit 1

for i in $(seq 1 30); do [ -f "$CTX/$SESSION.channel.json" ] && break; sleep 1; done
if [ ! -f "$CTX/$SESSION.channel.json" ]; then
  fail "the plugin's wake server wrote no channel record (is the plugin enabled? the session exported?)"
  show_log main; exit 1
fi
server_pid=$(json "$CTX/$SESSION.channel.json" 'd["pid"]')
parent_pid=$(json "$CTX/$SESSION.channel.json" 'd["parent_pid"]')
if [ "$parent_pid" = "$CLAUDE_PID" ]; then
  pass "the wake server (pid $server_pid) is a direct child of claude (pid $CLAUDE_PID)"
else
  fail "the wake server's parent is $parent_pid, not claude's pid $CLAUDE_PID"
fi
for i in $(seq 1 30); do [ -f "$CTX/$SESSION.queue.json" ] && break; sleep 1; done
[ -f "$CTX/$SESSION.queue.json" ] || fail "the session's ClawBridge MCP server wrote no queue record"
# The wake server looks for a missing queue record again only every 30 s,
# so it may still say inactive for a while after the record appears.
for i in $(seq 1 40); do
  state=$(json "$CTX/$SESSION.channel.json" 'd["state"]')
  [ "$state" = unconfirmed ] && break
  sleep 1
done
reason=$(json "$CTX/$SESSION.channel.json" 'd.get("reason","")')
info "channel before the message: $state ($reason)"
[ "$state" = unconfirmed ] || { fail "the channel is $state before any push: $reason"; show_log main; exit 1; }
TRANSCRIPT=$(json "$CTX/$SESSION.json" 'd["transcript"]')
[ -n "$TRANSCRIPT" ] && TRANSCRIPT_DIR=$(dirname "$TRANSCRIPT")
[ -n "$TRANSCRIPT" ] || info "no transcript bound yet (the SessionStart hook)"

t0=$(now)
cb --session "$SENDER" --instance "$SENDER-1" send "$SESSION" "FR26 e2e probe: please ack this message; no reply needed" --operation-id "e2e-probe-$$"
echo "channel-wake: message sent; waiting up to ${WAKE_LIMIT}s for the wake"

woke=no
for i in $(seq 1 "$WAKE_LIMIT"); do
  if [ "$(json "$CTX/$SESSION.turn.json" 'd.get("channel_seq",0) > 0')" = True ] && [ "$(since "$CTX/$SESSION.turn.json" channel_at "$t0")" = yes ]; then
    woke=yes; break
  fi
  sleep 1
done
waited=$(python3 -I -c "import time; print(round(time.time() - $t0))")
if [ "$woke" = yes ]; then
  pass "a channel turn started ${waited}s after the send (seq $(json "$CTX/$SESSION.turn.json" 'd["channel_seq"]'))"
else
  fail "no channel turn within ${WAKE_LIMIT}s"
fi

# The transcript's channel user message: exactly the fixed notice in our tag.
# Without the SessionStart hook's binding, the newest transcript in Claude
# Code's folder for the scratch folder (one session runs there).
if [ -z "$TRANSCRIPT" ]; then
  TRANSCRIPT=$(json "$CTX/$SESSION.json" 'd["transcript"]')
  [ -n "$TRANSCRIPT" ] && TRANSCRIPT_DIR=$(dirname "$TRANSCRIPT")
fi
if [ -z "$TRANSCRIPT" ] && [ -d "$PROJECT_DIR" ]; then
  TRANSCRIPT=$(ls -t "$PROJECT_DIR"/*.jsonl 2>/dev/null | head -1 || true)
  [ -n "$TRANSCRIPT" ] && info "transcript found in Claude Code's folder for the scratch folder (no binding)"
fi
check_transcript() {
  python3 -I - "$TRANSCRIPT" <<'EOF'
import json, re, sys
tag = re.compile(r'\A\s*<channel source="plugin:clawbridge:wake"((?: (?:kind="wake"|count="1"|seq="\d+")){3})>\s*'
                 r'1 ClawBridge message is waiting; call inbox\s*</channel>\s*\Z')
def texts(content):
    if isinstance(content, str):
        yield content
    elif isinstance(content, list):
        for part in content:
            if isinstance(part, dict) and isinstance(part.get("text"), str):
                yield part["text"]
seen, exact, tools = [], False, []
for line in open(sys.argv[1], errors="replace"):
    try:
        entry = json.loads(line)
    except ValueError:
        continue
    msg = entry.get("message") or {}
    if entry.get("type") == "user":
        for t in texts(msg.get("content")):
            if "plugin:clawbridge:wake" in t:
                seen.append(t)
                exact = exact or bool(tag.match(t))
    if entry.get("type") == "assistant" and isinstance(msg.get("content"), list):
        for part in msg["content"]:
            if isinstance(part, dict) and part.get("type") == "tool_use":
                tools.append(part.get("name", ""))
print(json.dumps({"seen": seen[:3], "exact": exact, "tools": tools}))
EOF
}
inbox=no exact=no
for i in $(seq 1 120); do
  [ -n "$TRANSCRIPT" ] && [ -f "$TRANSCRIPT" ] || { sleep 1; continue; }
  report=$(check_transcript 2>/dev/null || echo '{}')
  exact=$(printf '%s' "$report" | python3 -I -c 'import json,sys; print("yes" if json.load(sys.stdin).get("exact") else "no")')
  inbox=$(printf '%s' "$report" | python3 -I -c 'import json,sys; print("yes" if any(t.endswith("__inbox") for t in json.load(sys.stdin).get("tools", [])) else "no")')
  [ "$exact" = yes ] && [ "$inbox" = yes ] && break
  sleep 1
done
if [ -n "${report:-}" ]; then
  echo "channel-wake: channel turn as the transcript holds it (fixed text only):"
  printf '%s' "$report" | python3 -I -c 'import json,sys; [print("  " + repr(t)) for t in json.load(sys.stdin).get("seen", [])]'
  printf '%s' "$report" | python3 -I -c 'import json,sys; t=json.load(sys.stdin).get("tools", []); print("  tools called:", ", ".join(t) or "none")'
fi
[ "$exact" = yes ] && pass "the user message is exactly the fixed notice in a <channel source=\"plugin:clawbridge:wake\"> tag" \
  || fail "no transcript user message is exactly the fixed notice in our channel tag"
[ "$inbox" = yes ] && pass "the session called the inbox tool" || fail "the session did not call the inbox tool"

active=no
for i in $(seq 1 60); do
  [ "$(json "$CTX/$SESSION.channel.json" 'd["state"]')" = active ] && { active=yes; break; }
  sleep 1
done
if [ "$active" = yes ]; then
  pass "channel.json reached active (confirmed at $(json "$CTX/$SESSION.channel.json" 'd.get("confirmed_at","")'))"
else
  fail "channel.json is $(json "$CTX/$SESSION.channel.json" 'd["state"]') ($(json "$CTX/$SESSION.channel.json" 'd.get("reason","")')), not active"
fi
[ "$FAILED" = 0 ] || show_log main
stop_session

# --- --strict: does the plugin's server start under --strict-mcp-config? ---
if [ "$STRICT" = 1 ]; then
  (cd "$WORK" && clawbridge --db "$DB" --workspace w --project p --session "$SESSION" --instance "$INSTANCE" \
    mcp install --agent claude-code --config "$SCRATCH/strict.mcp.json" --hooks none >/dev/null 2>&1) || die "mcp install --config failed"
  rm -f -- "$CTX/$SESSION.channel.json"
  if launch strict --mcp-config "$SCRATCH/strict.mcp.json" --strict-mcp-config; then
    # Only a record written by a child of this claude counts: the first
    # session's server may write its stopped record after the rm.
    started=no
    for i in $(seq 1 30); do
      [ "$(json "$CTX/$SESSION.channel.json" 'd["parent_pid"]')" = "$CLAUDE_PID" ] && { started=yes; break; }
      sleep 1
    done
    if [ "$started" = yes ]; then
      STRICT_RESULT="the plugin's wake server started under --strict-mcp-config (pid $(json "$CTX/$SESSION.channel.json" 'd["pid"]'), parent $(json "$CTX/$SESSION.channel.json" 'd["parent_pid"]'), claude $CLAUDE_PID; state $(json "$CTX/$SESSION.channel.json" 'd["state"]'))"
    else
      STRICT_RESULT="the plugin's wake server did NOT start under --strict-mcp-config (no channel record from a child of claude pid $CLAUDE_PID in 30 s)"
      show_log strict
    fi
    info "strict: $STRICT_RESULT"
    stop_session
  fi
fi

echo
echo "==== FR26 channel wake e2e ($MODE) ===="
printf '%s\n' "${RESULTS[@]}"
[ "$STRICT" = 1 ] && echo "STRICT  $STRICT_RESULT"
if [ "$FAILED" = 0 ]; then echo "RESULT  PASS"; else echo "RESULT  FAIL"; fi
exit "$FAILED"

#!/usr/bin/env bash
#
# demo-lib.sh — shared machinery for the credibility demos.
#
# Everything here drives the REAL hook. Nothing simulates a decision. A demo
# that printed "HIT" because a script decided to print "HIT" would be worth
# less than no demo at all, so every verdict in these scripts is read back out
# of the daemon's own log.jsonl after the fact, never inferred from the shape
# of the rewritten command.
#
# HP_ENABLE is set as a per-invocation prefix on `hindsight hook` and never
# exported. The guard exists because the hook intercepts shell commands in
# whatever repo it is installed in; exporting it in a development shell points
# it at the session you would use to fix a bug in it. A prefix on one child
# process has the same effect for the demo and none of the risk.

# shellcheck shell=bash
# Every HS_* result variable is set here and read by the calling script. That
# crosses a source boundary the linter cannot follow, hence the blanket
# disable rather than a directive on each assignment.
# shellcheck disable=SC2034

HS_BIN="${HINDSIGHT_BIN:-hindsight}"

if [ -t 1 ]; then
	C_RESET=$'\033[0m'; C_DIM=$'\033[2m'; C_BOLD=$'\033[1m'
	C_GREEN=$'\033[32m'; C_RED=$'\033[31m'; C_YELLOW=$'\033[33m'; C_BLUE=$'\033[36m'
else
	C_RESET=""; C_DIM=""; C_BOLD=""; C_GREEN=""; C_RED=""; C_YELLOW=""; C_BLUE=""
fi

hs_hr() { printf '%s\n' "$(printf '─%.0s' $(seq 1 "${1:-74}"))"; }
hs_title() { printf '\n%s%s%s\n' "$C_BOLD" "$*" "$C_RESET"; hs_hr; }
hs_note() { printf '%s%s%s\n' "$C_DIM" "$*" "$C_RESET"; }
hs_die() { printf '%serror:%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; exit 1; }

# hs_colour_decision <decision> — colour a verdict the same way everywhere.
hs_colour_decision() {
	case "$1" in
	HIT) printf '%s%-11s%s' "$C_GREEN" "$1" "$C_RESET" ;;
	MISS) printf '%s%-11s%s' "$C_YELLOW" "$1" "$C_RESET" ;;
	LEASE_WAIT) printf '%s%-11s%s' "$C_BLUE" "$1" "$C_RESET" ;;
	REFUSED) printf '%s%-11s%s' "$C_RED" "$1" "$C_RESET" ;;
	*) printf '%s%-11s%s' "$C_DIM" "$1" "$C_RESET" ;;
	esac
}

hs_require() {
	command -v "$HS_BIN" >/dev/null 2>&1 ||
		hs_die "'$HS_BIN' is not on PATH. Build it: go build -o ~/.local/bin/hindsight ./cmd/hindsight"
	command -v python3 >/dev/null 2>&1 || hs_die "python3 is required"
}

# hs_port_free <port> — never use lsof in a loop. The pattern matches this
# script's own command line, and the usual cleanup idiom that follows kills the
# session running the demo.
hs_port_free() {
	python3 -c "import socket,sys;s=socket.socket();s.settimeout(.3);sys.exit(0 if s.connect_ex(('127.0.0.1',$1)) else 1)"
}

HS_DAEMON_PID=""
HS_DAEMON_URL=""
HS_HOME=""

# hs_daemon_start <home> <port>
hs_daemon_start() {
	local home="$1" port="$2"
	hs_port_free "$port" || hs_die "port $port is already in use; pass --port to pick another"
	rm -rf "$home"
	mkdir -p "$home"
	HS_HOME="$home"
	HS_DAEMON_URL="http://127.0.0.1:$port"
	HP_HOME="$home" "$HS_BIN" daemon --addr "127.0.0.1:$port" >"$home/daemon.log" 2>&1 &
	HS_DAEMON_PID=$!
	local i
	for i in $(seq 1 40); do
		curl -s -o /dev/null --max-time 1 "$HS_DAEMON_URL/healthz" && return 0
		sleep 0.25
	done
	hs_die "daemon did not come up on $port (see $home/daemon.log)"
}

# Kill by recorded pid only. `pkill -f "hindsight daemon"` matches this script.
hs_daemon_stop() {
	[ -n "$HS_DAEMON_PID" ] && kill "$HS_DAEMON_PID" 2>/dev/null
	HS_DAEMON_PID=""
}

# hs_key <dir> — prints "<tree> <env-fp>".
hs_key() {
	(cd "$1" && "$HS_BIN" key . 2>/dev/null |
		awk '/^tree /{t=$2} /^env-fp /{e=$2} END{print t" "e}')
}

# hs_worktree <repo> <ref> <path> — a detached checkout of tracked files only.
#
# Retries, because a target repo may have another fleet running against it and
# `git worktree add` contends for the main repo's index lock. A demo that dies
# on someone else's concurrent run is a demo that dies on stage.
hs_worktree() {
	local repo="$1" ref="$2" path="$3" i err
	git -C "$repo" worktree remove --force "$path" >/dev/null 2>&1
	rm -rf "$path"
	for i in 1 2 3; do
		if err=$(git -C "$repo" worktree add --detach --force "$path" "$ref" 2>&1); then
			return 0
		fi
		git -C "$repo" worktree prune >/dev/null 2>&1
		sleep 1
	done
	hs_die "could not create worktree $path at $ref: $err"
}

hs_worktree_rm() {
	local repo="$1" path="$2"
	git -C "$repo" worktree remove --force "$path" >/dev/null 2>&1 || rm -rf "$path"
}

# hs_hook <cwd> <agent> <cmd>
#
# Hands the command to the hook exactly as a PreToolUse harness would and
# prints whatever the hook wants run instead. Exit 9 means the hook emitted no
# decision, which is the only way to say "pass this through untouched" to
# Codex: it rejects "ask", rejects allow without updatedInput, and sets
# deny_unknown_fields, so silence is the pass-through verb.
hs_hook() {
	local cwd="$1" agent="$2" cmd="$3"
	python3 -c 'import json,sys; print(json.dumps({
        "tool_name":"Bash","tool_input":{"command":sys.argv[1]},
        "cwd":sys.argv[2],"session_id":"demo"}))' "$cmd" "$cwd" |
		HP_ENABLE=1 HP_DAEMON="$HS_DAEMON_URL" HP_HOME="$HS_HOME" HP_AGENT="$agent" \
			"$HS_BIN" hook 2>/dev/null |
		python3 -c 'import json,sys
raw = sys.stdin.read().strip()
if not raw:
    sys.exit(9)
try:
    print(json.loads(raw)["hookSpecificOutput"]["updatedInput"]["command"])
except Exception:
    sys.exit(9)'
}

# hs_log_after <n> <cmd> — the daemon appends log.jsonl, so a record can land a
# beat after the command returns. Poll rather than sleep a fixed amount.
#
# Prints one TSV line: decision  duration_ms  servable  tree_before  tree_after
#                      envfp_before  envfp_after  source_agent  exit_code
hs_log_after() {
	local skip="$1" cmd="$2" i
	for i in $(seq 1 40); do
		local got
		got=$(python3 - "$HS_HOME/log.jsonl" "$skip" "$cmd" <<'PY'
import json, sys
path, skip, cmd = sys.argv[1], int(sys.argv[2]), sys.argv[3]
try:
    lines = open(path, encoding="utf-8").read().splitlines()
except OSError:
    sys.exit(1)
for ln in lines[skip:]:
    ln = ln.strip()
    if not ln:
        continue
    try:
        r = json.loads(ln)
    except ValueError:
        continue
    if r.get("cmd") != cmd or r.get("decision") == "VERIFY":
        continue
    print("\t".join(str(r.get(k, "")) for k in (
        "decision", "duration_ms", "servable", "tree_before", "tree_after",
        "env_fp_before", "env_fp_after", "source_agent", "exit_code")))
    sys.exit(0)
sys.exit(1)
PY
		) && { printf '%s\n' "$got"; return 0; }
		sleep 0.25
	done
	return 1
}

hs_log_lines() {
	[ -f "$HS_HOME/log.jsonl" ] && wc -l <"$HS_HOME/log.jsonl" | tr -d ' ' || echo 0
}

# hs_run <cwd> <agent> <cmd> <stdout-file>
#
# The whole interception path for one command: ask the hook, run what it says,
# capture the bytes, then read the verdict back out of the daemon's log.
#
# Sets HS_DECISION, HS_DURATION_MS, HS_SERVABLE, HS_TREE_BEFORE, HS_TREE_AFTER,
# HS_ENVFP_BEFORE, HS_ENVFP_AFTER, HS_SOURCE_AGENT, HS_RC.
hs_run() {
	local cwd="$1" agent="$2" cmd="$3" outfile="$4"
	local before rewritten rc row
	before=$(hs_log_lines)

	rewritten=$(hs_hook "$cwd" "$agent" "$cmd")
	if [ $? -eq 9 ]; then
		# The hook stayed silent: not intercepted at all.
		(cd "$cwd" && bash -c "$cmd") >"$outfile" 2>"${outfile}.err"
		HS_RC=$?
		HS_DECISION="PASSTHROUGH"; HS_DURATION_MS=0; HS_SERVABLE=""
		HS_TREE_BEFORE=""; HS_TREE_AFTER=""; HS_ENVFP_BEFORE=""; HS_ENVFP_AFTER=""
		HS_SOURCE_AGENT=""
		return 0
	fi

	(cd "$cwd" && bash -c "$rewritten") >"$outfile" 2>"${outfile}.err"
	rc=$?
	HS_RC=$rc

	if row=$(hs_log_after "$before" "$cmd"); then
		IFS=$'\t' read -r HS_DECISION HS_DURATION_MS HS_SERVABLE \
			HS_TREE_BEFORE HS_TREE_AFTER HS_ENVFP_BEFORE HS_ENVFP_AFTER \
			HS_SOURCE_AGENT _ <<<"$row"
	else
		HS_DECISION="UNLOGGED"; HS_DURATION_MS=0; HS_SERVABLE=""
		HS_TREE_BEFORE=""; HS_TREE_AFTER=""; HS_ENVFP_BEFORE=""; HS_ENVFP_AFTER=""
		HS_SOURCE_AGENT=""
	fi
	return 0
}

# hs_row <label> <decision> <ms> <note> — one aligned line, readable at the
# back of a room. Terse columns beat a wall of JSON.
hs_row() {
	printf '  %-7s %s %7sms  %s\n' "$1" "$(hs_colour_decision "$2")" "$3" "${4:-}"
}

hs_short() { printf '%.12s' "${1:-—}"; }

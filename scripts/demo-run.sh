#!/usr/bin/env bash
#
# demo-run.sh — run the baseline and cached arms back to back and store the
# evidence. This is the thing that goes on screen.
#
# The one non-obvious rule it enforces: EACH ARM GETS A FRESH CACHE HOME.
# Both arms record. If they share $HP_HOME and baseline runs first, the cached
# arm replays baseline's records off disk and reports a ~100% hit rate that
# proves nothing about a cold fan-out. A fresh home per arm forces the cached
# arm to earn every hit from its own peers, which is the phenomenon we claim.
#
# Usage:
#   bash scripts/demo-run.sh                              # sympy, both arms
#   bash scripts/demo-run.sh --target express
#   bash scripts/demo-run.sh --target fasthack-go
#   bash scripts/demo-run.sh --repo ~/src/sympy           # dir override
#   bash scripts/demo-run.sh --target sympy --arm cached  # one arm
#   bash scripts/demo-run.sh --target sympy --live        # real coding agents
#
# Ports default to 7778 (baseline) and 7777 (cached, the viewer's default).
# Override both when someone else is already fanning out on this machine:
#   bash scripts/demo-run.sh --target express --baseline-port 7822 --cached-port 7821
#
# Do NOT pipe this through `tail`. An agent's grandchild process inherits the
# pipe, so if one wedges — and one did — `tail` waits on it long after this
# script has exited, and the run looks hung when it finished minutes ago.
# Redirect to a file instead: `... > run.log 2>&1` and read the file.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/demo-targets/_load.sh
. "$HERE/scripts/demo-targets/_load.sh"

TARGET="sympy"
REPO=""
AGENTS=5
ARM="both"
LIVE=0
KEEP_DAEMON=0
BASELINE_PORT=7778
CACHED_PORT=7777 # the viewer defaults here; keep the cached arm on it

STAMP="$(date +%Y%m%d-%H%M%S)"

while [ $# -gt 0 ]; do
	case "$1" in
	--target) TARGET="$2"; shift 2 ;;
	--repo) REPO="$2"; shift 2 ;;
	--agents) AGENTS="$2"; shift 2 ;;
	--arm) ARM="$2"; shift 2 ;;
	--live) LIVE=1; shift ;;
	--keep-daemon) KEEP_DAEMON=1; shift ;;
	--baseline-port) BASELINE_PORT="$2"; shift 2 ;;
	--cached-port) CACHED_PORT="$2"; shift 2 ;;
	--list) printf 'Demo targets:\n'; list_targets; exit 0 ;;
	-h | --help) sed -n '2,28p' "$0"; exit 0 ;;
	*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

load_target "$TARGET" || exit 2
# --repo overrides the profile's directory and keeps the original
# `demo-run.sh --repo ~/src/sympy` invocation working unchanged.
[ -n "$REPO" ] && TARGET_DIR="$REPO"
REPO="$TARGET_DIR"

EVIDENCE="$HERE/demo-runs/$STAMP-$TARGET_NAME"

is_git_repo "$REPO" || { echo "demo-run: $REPO is not a git repository. Run demo-setup.sh first." >&2; exit 2; }
command -v hindsight >/dev/null 2>&1 || {
	echo "demo-run: 'hindsight' is not on PATH. Run scripts/demo-setup.sh first." >&2
	exit 2
}

printf 'demo-run: target %s — %s\n' "$TARGET_NAME" "$TARGET_DESC"
printf 'demo-run: repo   %s\n' "$REPO"

# Apply the target's environment (venv activation, and so on) before anything
# reads it. fleet.sh passes the environment through to the agents, so this is
# also how the five worktrees end up sharing one interpreter.
[ -n "$TARGET_SHELL_ENV" ] && eval "$TARGET_SHELL_ENV"

# replay-agent.sh runs this inside each worktree before the command list.
# `git worktree add` materialises tracked files only, so a target whose
# runtime dependencies are gitignored has to put them back.
export HS_WORKTREE_PREP="$TARGET_WORKTREE_PREP"

# A busy port is the difference between "the demo shows 0%" and "the daemon
# never started". Check it up front, in Python — an `lsof` loop matches its own
# command line and takes the shell down with it.
port_busy() {
	python3 -c "import socket,sys
s=socket.socket(); s.settimeout(0.3)
sys.exit(0 if s.connect_ex(('127.0.0.1',$1))==0 else 1)"
}
for p in $([ "$ARM" = cached ] || echo "$BASELINE_PORT") $([ "$ARM" = baseline ] || echo "$CACHED_PORT"); do
	if port_busy "$p"; then
		echo "demo-run: port $p is already in use — another fleet is probably running." >&2
		echo "  Pick free ports: --baseline-port 7822 --cached-port 7821" >&2
		exit 2
	fi
done

# The live arm drives real coding agents. Pick a driver unless the caller has
# already chosen one, so `--live` works without anyone having to know the flag
# set each CLI needs.
#
# Both drivers need more than fleet.sh's default `codex exec
# --dangerously-bypass-hook-trust`: that flag only lets the hook run, and the
# agent still has to be allowed to execute the commands it is being asked to
# execute. Reasoning effort is pinned low because the demo prompt names the
# commands outright; there is nothing to deliberate about, and the default
# effort spends about ninety seconds per agent doing so.
#
# SC2016/SC2089/SC2090 are wrong here and an array is not available: this is a
# command TEMPLATE crossing a process boundary. fleet.sh evals it with the
# prompt as $1, so "$1" and the quotes around it must survive as literal text,
# and bash cannot export an array to a child process.
# shellcheck disable=SC2016,SC2089,SC2090
if [ "$LIVE" = 1 ]; then
	if [ "$TARGET_LIVE" != 1 ]; then
		echo "demo-run: target '$TARGET_NAME' does not support --live." >&2
		echo "  It has no committed PreToolUse hook, so the live arm would measure nothing." >&2
		exit 2
	fi
	if [ -z "${FLEET_AGENT_CMD:-}" ]; then
		if command -v codex >/dev/null 2>&1; then
			FLEET_AGENT_CMD='codex exec --dangerously-bypass-hook-trust --dangerously-bypass-approvals-and-sandbox -c model_reasoning_effort="low" "$1"'
		elif command -v claude >/dev/null 2>&1; then
			FLEET_AGENT_CMD='claude -p "$1" --permission-mode bypassPermissions'
		else
			echo "demo-run: --live needs 'codex' or 'claude' on PATH." >&2
			echo "  Set FLEET_AGENT_CMD to drive a different CLI; it receives the prompt as \$1." >&2
			exit 2
		fi
		export FLEET_AGENT_CMD
		printf 'demo-run: live driver  %s\n' "$FLEET_AGENT_CMD"
	fi
fi

# The live arm needs the PreToolUse hook installed in the target repo, and it
# must be COMMITTED there: agents run in `git worktree add` checkouts, which
# materialise tracked files only. An untracked .codex/hooks.json in the main
# clone never reaches a single agent, and the run silently measures nothing.
if [ "$LIVE" = 1 ] && [ ! -f "$REPO/.codex/hooks.json" ]; then
	echo "demo-run: no PreToolUse hook in $REPO — the live arm would measure nothing." >&2
	echo "  Install and commit it:" >&2
	echo "    (cd $REPO && hindsight init && git add .codex .claude && git commit -m 'hindsight hook')" >&2
	exit 2
fi

DAEMON_PIDS=()
cleanup() {
	[ "$KEEP_DAEMON" = 1 ] && return 0
	local p
	for p in ${DAEMON_PIDS[@]+"${DAEMON_PIDS[@]}"}; do
		# Kill by recorded pid. Never `pkill -f "hindsight daemon"`: that
		# pattern matches this script's own command line and kills the shell.
		kill "$p" 2>/dev/null || true
	done
}
trap cleanup EXIT INT TERM

mkdir -p "$EVIDENCE"

# run_arm <baseline|cached> <port>
run_arm() {
	local mode="$1" port="$2"
	local home="/tmp/hs-demo-$TARGET_NAME-$mode-$STAMP"
	local url="http://127.0.0.1:$port"
	local out="/tmp/fleet-$TARGET_NAME-$mode-$STAMP"
	local dest="$EVIDENCE/$mode"

	rm -rf "$home"
	mkdir -p "$home" "$dest"

	printf '\n\n########## %s arm  (fresh cache, port %s) ##########\n\n' "$(echo "$mode" | tr '[:lower:]' '[:upper:]')" "$port"

	HP_HOME="$home" hindsight daemon --addr "127.0.0.1:$port" >"$dest/daemon.log" 2>&1 &
	DAEMON_PIDS[${#DAEMON_PIDS[@]}]=$!
	sleep 1.5
	if ! curl -s -o /dev/null --max-time 2 "$url/healthz"; then
		echo "demo-run: daemon failed to come up on $port — is the port already taken?" >&2
		return 1
	fi

	if [ "$LIVE" = 1 ]; then
		# A live agent that finishes takes about 25s. Seven minutes is enough
		# headroom for a slow model and short enough that one wedged agent
		# does not hold the demo hostage.
		bash "$HERE/scripts/fleet.sh" --repo "$REPO" --prompt "$HERE/scripts/demo-prompt.md" \
			--agents "$AGENTS" --mode "$mode" --out "$out" --hp-home "$home" --daemon "$url" \
			--timeout 420
	else
		# fleet.sh's 900s default is meant for real models. A deterministic
		# arm that should finish in ten seconds needs a much tighter bound,
		# or one wedged agent — seen once, an express mocha that never
		# exited — turns a thirty-second demo into a fifteen-minute stall.
		FLEET_AGENT_CMD="bash $HERE/scripts/replay-agent.sh $TARGET_CMDS" \
			bash "$HERE/scripts/fleet.sh" --repo "$REPO" --prompt "deterministic replay" \
			--agents "$AGENTS" --mode "$mode" --out "$out" --hp-home "$home" --daemon "$url" \
			--timeout "$TARGET_TIMEOUT"
	fi

	# Evidence outlives the run. summary.json is the machine-readable claim;
	# log.jsonl is every decision the hook made, which is what makes the claim
	# checkable rather than asserted.
	cp "$out/summary.txt" "$dest/" 2>/dev/null
	cp "$out/summary.json" "$dest/" 2>/dev/null
	cp "$home/log.jsonl" "$dest/" 2>/dev/null
	cp -r "$out/logs" "$dest/agent-logs" 2>/dev/null
}

# A record of what produced these numbers, next to the numbers. A demo-runs
# directory whose target nobody can identify is not evidence.
cat >"$EVIDENCE/target.txt" <<EOF
target      $TARGET_NAME
desc        $TARGET_DESC
repo        $TARGET_DIR
ref         $(git -C "$REPO" rev-parse --short HEAD 2>/dev/null)
commands    $TARGET_CMDS
agents      $AGENTS
live        $LIVE
hindsight   $(git -C "$HERE" rev-parse --short HEAD 2>/dev/null)
when        $STAMP
EOF
sed -e 's/^/            /' "$TARGET_CMDS" | grep -v '^ *#' | grep -v '^ *$' >>"$EVIDENCE/target.txt"

case "$ARM" in
baseline) run_arm baseline "$BASELINE_PORT" ;;
cached) run_arm cached "$CACHED_PORT" ;;
both)
	run_arm baseline "$BASELINE_PORT"
	run_arm cached "$CACHED_PORT"
	;;
*) echo "demo-run: --arm must be baseline, cached or both" >&2; exit 2 ;;
esac

if [ -f "$EVIDENCE/baseline/summary.json" ] && [ -f "$EVIDENCE/cached/summary.json" ]; then
	python3 - "$EVIDENCE" "$TARGET_NAME" <<'PY'
import json, os, sys

ev, target = sys.argv[1], sys.argv[2]
b = json.load(open(os.path.join(ev, "baseline", "summary.json")))
c = json.load(open(os.path.join(ev, "cached", "summary.json")))

def row(label, bv, cv):
    print("  %-34s %14s %14s" % (label, bv, cv))

print("\n" + "=" * 66)
print("  RESULT   baseline vs cached   [%s]" % target)
print("=" * 66)
row("", "baseline", "cached")
row("commands agents asked for", b["demand"], c["demand"])
row("  executed", b["executed"], c["executed"])
row("  served", b["served"], c["served"])
row("    coalesced in flight", b["lease_waits"], c["lease_waits"])
row("execution-seconds spent", "%.1fs" % (b["executed_ms"] / 1000), "%.1fs" % (c["executed_ms"] / 1000))
row("execution-seconds deleted", "%.1fs" % (b["deleted_ms"] / 1000), "%.1fs" % (c["deleted_ms"] / 1000))
row("hit rate", "%.1f%%" % b["hit_rate_pct"], "%.1f%%" % c["hit_rate_pct"])
row("wall clock", "%.1fs" % (b["fleet_wall_ms"] / 1000), "%.1fs" % (c["fleet_wall_ms"] / 1000))

total = c["counterfactual_ms"]
pct = (100.0 * c["deleted_ms"] / total) if total else 0.0
print("\n  %.0f%% of execution-seconds deleted." % pct)

# Wall clock is reported but never claimed: at this scale the bottleneck is
# model latency, so deleted execution-seconds do not become elapsed time.
if c["fleet_wall_ms"] >= b["fleet_wall_ms"]:
    print("  Wall clock did NOT improve. Say so before anyone asks.")

if c["verified_false"]:
    print("\n  *** %d DIVERGENT served result(s). Do not quote this run. ***" % c["verified_false"])
elif c["verified_true"]:
    print("  %d served result(s) shadow-verified against a real re-execution." % c["verified_true"])

print("\n  evidence: %s" % ev)
print("=" * 66 + "\n")
PY
fi

printf 'Stored: %s\n' "$EVIDENCE"

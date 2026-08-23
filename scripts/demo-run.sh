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
#   bash scripts/demo-run.sh --repo ~/src/sympy               # both arms, deterministic
#   bash scripts/demo-run.sh --repo ~/src/sympy --arm cached  # one arm
#   bash scripts/demo-run.sh --repo ~/src/sympy --live        # real coding agents
set -uo pipefail

REPO=""
AGENTS=5
ARM="both"
LIVE=0
KEEP_DAEMON=0
BASELINE_PORT=7778
CACHED_PORT=7777 # the viewer defaults here; keep the cached arm on it

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STAMP="$(date +%Y%m%d-%H%M%S)"
EVIDENCE="$HERE/demo-runs/$STAMP"

while [ $# -gt 0 ]; do
	case "$1" in
	--repo) REPO="$2"; shift 2 ;;
	--agents) AGENTS="$2"; shift 2 ;;
	--arm) ARM="$2"; shift 2 ;;
	--live) LIVE=1; shift ;;
	--keep-daemon) KEEP_DAEMON=1; shift ;;
	-h | --help) sed -n '2,18p' "$0"; exit 0 ;;
	*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

[ -n "$REPO" ] || { echo "demo-run: --repo is required" >&2; exit 2; }
command -v hindsight >/dev/null 2>&1 || {
	echo "demo-run: 'hindsight' is not on PATH. Run scripts/demo-setup.sh first." >&2
	exit 2
}

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
	local home="/tmp/hs-demo-$mode-$STAMP"
	local url="http://127.0.0.1:$port"
	local out="/tmp/fleet-$mode-$STAMP"
	local dest="$EVIDENCE/$mode"

	rm -rf "$home"
	mkdir -p "$home" "$dest"

	printf '\n\n########## %s arm  (fresh cache, port %s) ##########\n\n' "$(echo "$mode" | tr '[:lower:]' '[:upper:]')" "$port"

	HP_HOME="$home" hindsight daemon --addr "127.0.0.1:$port" >"$dest/daemon.log" 2>&1 &
	DAEMON_PIDS[${#DAEMON_PIDS[@]}]=$!
	sleep 1.5
	if ! curl -s -o /dev/null --max-time 2 "$url/healthz"; then
		echo "demo-run: daemon failed to come up on $port — is the port already taken?" >&2
		echo "  check with: lsof -ti tcp:$port" >&2
		return 1
	fi

	if [ "$LIVE" = 1 ]; then
		bash "$HERE/scripts/fleet.sh" --repo "$REPO" --prompt "$HERE/scripts/demo-prompt.md" \
			--agents "$AGENTS" --mode "$mode" --out "$out" --hp-home "$home" --daemon "$url"
	else
		FLEET_AGENT_CMD="bash $HERE/scripts/replay-agent.sh $HERE/scripts/demo-cmds.txt" \
			bash "$HERE/scripts/fleet.sh" --repo "$REPO" --prompt "deterministic replay" \
			--agents "$AGENTS" --mode "$mode" --out "$out" --hp-home "$home" --daemon "$url"
	fi

	# Evidence outlives the run. summary.json is the machine-readable claim;
	# log.jsonl is every decision the hook made, which is what makes the claim
	# checkable rather than asserted.
	cp "$out/summary.txt" "$dest/" 2>/dev/null
	cp "$out/summary.json" "$dest/" 2>/dev/null
	cp "$home/log.jsonl" "$dest/" 2>/dev/null
	cp -r "$out/logs" "$dest/agent-logs" 2>/dev/null
}

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
	python3 - "$EVIDENCE" <<'PY'
import json, os, sys

ev = sys.argv[1]
b = json.load(open(os.path.join(ev, "baseline", "summary.json")))
c = json.load(open(os.path.join(ev, "cached", "summary.json")))

def row(label, bv, cv):
    print("  %-34s %14s %14s" % (label, bv, cv))

print("\n" + "=" * 66)
print("  RESULT   baseline vs cached")
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

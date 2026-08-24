#!/usr/bin/env bash
# benchmark.sh — the same task, three ways, side by side.
#
# Two arms is the obvious design and it hides the interesting result. On a cold
# fan-out Hindsight deletes execution-seconds and does not move wall clock:
# single-flight collapses N executions into one, but the other agents block for
# that one execution, so the critical path is unchanged. Measured on SQLAlchemy
# that was 1083s of execution down to 244s, and 243s of elapsed time up to 260s.
#
# The arm where wall clock actually moves is a warm cache, because a peer has
# already finished and the result returns immediately instead of being waited
# on. So:
#
#   1  baseline     hook records, serves nothing        the control
#   2  cold cache   serving on, empty cache             CPU saved, clock flat
#   3  warm cache   serving on, arm 2's cache reused    clock actually moves
#
# Arm 3 is the honest version of "faster". Arms 1 and 2 are what makes it
# believable, because they show the case where it isn't.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FLEET="$HERE/fleet.sh"
HINDSIGHT="${HINDSIGHT_BIN:-hindsight}"

REPO=""; PROMPT=""; AGENTS=5; OUT="/tmp/hs-bench-$(date +%s)"; PORT=7860; TIMEOUT=900
KEEP=0

usage() {
	cat <<'EOF'
benchmark.sh — run one task three ways and print the comparison.

USAGE
  benchmark.sh --repo <path> --prompt <file-or-string> [options]

OPTIONS
  --agents <N>      Agents per arm.                          [5]
  --out <dir>       Where arm outputs and caches go.  [/tmp/hs-bench-<ts>]
  --port <N>        First of two consecutive daemon ports.    [7860]
  --timeout <secs>  Per-agent wall-clock kill.                [900]
  --keep            Leave caches and worktrees in place.
  -h, --help

Set FLEET_AGENT_CMD to choose the harness, e.g.
  export FLEET_AGENT_CMD='claude --print --permission-mode bypassPermissions "$1"'
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--repo) REPO="$2"; shift 2 ;;
	--prompt) PROMPT="$2"; shift 2 ;;
	--agents) AGENTS="$2"; shift 2 ;;
	--out) OUT="$2"; shift 2 ;;
	--port) PORT="$2"; shift 2 ;;
	--timeout) TIMEOUT="$2"; shift 2 ;;
	--keep) KEEP=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) echo "benchmark.sh: unknown option $1" >&2; usage; exit 2 ;;
	esac
done

[ -n "$REPO" ] || { echo "benchmark.sh: --repo is required" >&2; exit 2; }
[ -n "$PROMPT" ] || { echo "benchmark.sh: --prompt is required" >&2; exit 2; }
command -v python3 >/dev/null || { echo "benchmark.sh: needs python3" >&2; exit 2; }

mkdir -p "$OUT"
DAEMONS=()

cleanup() {
	trap - EXIT INT TERM
	for pid in ${DAEMONS[@]+"${DAEMONS[@]}"}; do kill "$pid" 2>/dev/null || true; done
	if [ "$KEEP" = 0 ]; then rm -rf "$OUT/cache-baseline" "$OUT/cache-cold" 2>/dev/null || true; fi
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

# Each arm gets its own daemon and its own cache, so nothing leaks between
# them. Arm 3 is the exception and that is the entire point of arm 3.
start_daemon() {
	local port="$1" home="$2"
	"$HINDSIGHT" daemon --addr "127.0.0.1:$port" --home "$home" \
		>"$OUT/daemon-$port.log" 2>&1 &
	DAEMONS[${#DAEMONS[@]}]=$!
	for _ in $(seq 1 40); do
		if curl -fsS -m 1 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then return 0; fi
		sleep 0.25
	done
	echo "benchmark.sh: daemon on $port never became healthy" >&2
	return 1
}

reset_repo() {
	git -C "$REPO" checkout -q -- . 2>/dev/null || true
	git -C "$REPO" clean -qfd 2>/dev/null || true
}

run_arm() {
	local name="$1" mode="$2" port="$3" home="$4"
	echo
	echo "──────────────────────────────────────────────────────────────"
	printf '  arm: %s   (%s, %s cache)\n' "$name" "$mode" \
		"$([ -s "$home/log.jsonl" ] && echo warm || echo cold)"
	echo "──────────────────────────────────────────────────────────────"
	reset_repo
	"$FLEET" --repo "$REPO" --prompt "$PROMPT" --agents "$AGENTS" --mode "$mode" \
		--daemon "http://127.0.0.1:$port" --hp-home "$home" \
		--out "$OUT/$name" --timeout "$TIMEOUT" >"$OUT/$name.log" 2>&1 ||
		echo "  (arm exited nonzero; see $OUT/$name.log)"
	tail -1 "$OUT/$name.log" >/dev/null 2>&1 || true
}

P1=$PORT; P2=$((PORT + 1))
start_daemon "$P1" "$OUT/cache-baseline"
start_daemon "$P2" "$OUT/cache-cold"

run_arm baseline baseline "$P1" "$OUT/cache-baseline"
run_arm cold     cached   "$P2" "$OUT/cache-cold"

# Arm 3 reuses arm 2's cache. Same daemon, so the in-memory index already holds
# everything arm 2 recorded — which is exactly the situation a second developer,
# a rerun, or an imported bundle finds itself in.
run_arm warm cached "$P2" "$OUT/cache-cold"

reset_repo

python3 - "$OUT" <<'PY'
import json, os, sys

out = sys.argv[1]
arms = [
    ("baseline", "no serving", "the control"),
    ("cold",     "empty cache", "CPU saved, clock flat"),
    ("warm",     "arm 2's cache", "the case that moves the clock"),
]

rows = []
for name, cache, note in arms:
    path = os.path.join(out, name, "summary.json")
    if not os.path.exists(path):
        rows.append((name, cache, note, None)); continue
    rows.append((name, cache, note, json.load(open(path))))

def s(ms): return f"{(ms or 0)/1000:.1f}s"

print()
print("=" * 78)
print("  same task, three ways")
print("=" * 78)
print(f"  {'arm':10s} {'cache':15s} {'wall':>9s} {'exec-sec':>10s} {'served':>8s} {'hit rate':>9s}")
print("  " + "-" * 74)
base_wall = None
for name, cache, note, d in rows:
    if d is None:
        print(f"  {name:10s} {cache:15s} {'—':>9s}   (arm did not produce a summary)")
        continue
    wall = d.get("fleet_wall_ms") or 0
    if name == "baseline": base_wall = wall
    print(f"  {name:10s} {cache:15s} {s(wall):>9s} {s(d.get('executed_ms')):>10s}"
          f" {d.get('served', 0):>8d} {d.get('hit_rate_pct', 0):>8.1f}%")

print()
for name, _, note, d in rows:
    if d is None: continue
    wall = d.get("fleet_wall_ms") or 0
    delta = ""
    if base_wall and name != "baseline":
        pct = 100 * (base_wall - wall) / base_wall
        delta = f"  wall clock {'-' if pct >= 0 else '+'}{abs(pct):.0f}% vs baseline"
    print(f"  {name:10s} {note}{delta}")

print()
print("  Execution-seconds and wall clock are different quantities. A lease wait")
print("  turns execution time into blocked time at par, so a cold fan-out saves")
print("  CPU and not elapsed time. Only a warm cache returns a result nobody has")
print("  to wait for.")
print()
print(f"  full output: {out}")
PY

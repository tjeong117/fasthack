#!/usr/bin/env bash
#
# demo-live.sh — the recordable demo. Narrates a fan-out while it happens.
#
# The summary table that demo-run.sh prints at the end is a claim. This script
# exists because a claim is not evidence: it streams every decision the hook
# makes as it makes it, shows the key components behind the first served
# result so the audience can see the match is mechanical, and carries a running
# total of deleted execution-seconds that climbs on screen.
#
# Nothing here decides anything. Every line is read back out of the daemon's
# own log.jsonl after the fact, so the display cannot flatter the mechanism.
#
# Usage:
#   bash scripts/demo-live.sh                        # 5 agents, cached, sympy
#   bash scripts/demo-live.sh --agents 3
#   bash scripts/demo-live.sh --mode baseline        # the control arm
#   bash scripts/demo-live.sh --ref hindsight-demo-bug
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

REPO="$HOME/src/sympy"
AGENTS=5
MODE="cached"
PORT=7831
REF=""
CMDS=""
KEEP=0

while [ $# -gt 0 ]; do
	case "$1" in
	--repo) REPO="$2"; shift 2 ;;
	--agents) AGENTS="$2"; shift 2 ;;
	--mode) MODE="$2"; shift 2 ;;
	--port) PORT="$2"; shift 2 ;;
	--ref) REF="$2"; shift 2 ;;
	--cmds) CMDS="$2"; shift 2 ;;
	--keep) KEEP=1; shift ;;
	-h | --help) sed -n '2,25p' "$0"; exit 0 ;;
	*) echo "demo-live: unknown argument: $1" >&2; exit 2 ;;
	esac
done

command -v hindsight >/dev/null 2>&1 ||
	{ echo "demo-live: 'hindsight' is not on PATH. Run scripts/demo-setup.sh first." >&2; exit 2; }

# The command list moved into per-target profiles part-way through the demo
# harness's life. Accept either location so this script keeps working on both
# sides of that change rather than breaking on whichever branch it lands in.
if [ -z "$CMDS" ]; then
	for candidate in "$HERE/scripts/demo-cmds.txt" "$HERE/scripts/demo-targets/sympy.cmds"; do
		[ -r "$candidate" ] && { CMDS="$candidate"; break; }
	done
fi
[ -n "$CMDS" ] && [ -r "$CMDS" ] ||
	{ echo "demo-live: no command list found (tried scripts/demo-cmds.txt and scripts/demo-targets/sympy.cmds)" >&2; exit 2; }
[ -d "$REPO/.git" ] || { echo "demo-live: $REPO is not a git repository" >&2; exit 2; }
[ -z "$REF" ] && REF="$(git -C "$REPO" rev-parse --abbrev-ref HEAD)"

# Never lsof in a loop: the pattern matches this script's own command line, and
# the cleanup that usually follows kills the session running the demo.
python3 -c "import socket,sys;s=socket.socket();s.settimeout(.3);sys.exit(0 if s.connect_ex(('127.0.0.1',$PORT)) else 1)" ||
	{ echo "demo-live: port $PORT is in use. Pass --port." >&2; exit 2; }

STAMP="$(date +%Y%m%d-%H%M%S)"
HOME_DIR="/tmp/hs-live-$MODE-$STAMP"
OUT="/tmp/fleet-live-$MODE-$STAMP"
URL="http://127.0.0.1:$PORT"
EVIDENCE="$HERE/demo-runs/live-$STAMP"

DAEMON_PID=""
TAILER_PID=""
cleanup() {
	# Kill by recorded pid. `pkill -f "hindsight daemon"` matches this very
	# script and silently kills the shell running the demo.
	[ -n "$TAILER_PID" ] && kill "$TAILER_PID" 2>/dev/null
	[ -n "$DAEMON_PID" ] && kill "$DAEMON_PID" 2>/dev/null
	[ "$KEEP" = 0 ] && rm -rf "$OUT" 2>/dev/null
	return 0
}
trap cleanup EXIT INT TERM

mkdir -p "$HOME_DIR" "$EVIDENCE"

# ---------------------------------------------------------------- header ----
python3 - "$MODE" "$AGENTS" "$REPO" "$REF" "$URL" "$HOME_DIR" "$OUT" "$CMDS" <<'PY'
import subprocess, sys

mode, agents, repo, ref, url, home, out, cmds = sys.argv[1:9]
tty = sys.stdout.isatty()
B = "\033[1m" if tty else ""
D = "\033[2m" if tty else ""
R = "\033[0m" if tty else ""
C = "\033[36m" if tty else ""

try:
    sha = subprocess.run(["git", "-C", repo, "rev-parse", "--short", ref],
                         capture_output=True, text=True).stdout.strip()
except Exception:
    sha = "?"

W = 96
print()
print(B + "═" * W + R)
label = "CACHED ARM — the hook may serve" if mode == "cached" \
        else "BASELINE ARM — the hook records but is forbidden to serve"
print(B + "  %s" % label + R)
print(B + "═" * W + R)
print("  %-14s %s agents, one detached git worktree each" % ("fan-out", agents))
print("  %-14s %s @ %s (%s)" % ("repo", repo, ref, sha))
print("  %-14s %s/a1 … a%s" % ("worktrees", out, agents))
print("  %-14s %s" % ("daemon", url))
print("  %-14s %s  %s(fresh — no records carried in from any earlier run)%s"
      % ("cache home", home, D, R))
print()
print("  every agent runs these, in order:")
n = 0
for line in open(cmds, encoding="utf-8"):
    line = line.strip()
    if not line or line.startswith("#"):
        continue
    n += 1
    print("    %s%d.%s %s%s%s" % (D, n, R, C, line, R))
print()
print("  %s%d agents × %d commands = %d commands demanded.%s"
      % (D, int(agents), n, int(agents) * n, R))
if mode == "cached":
    print("  %sOnly distinct (command, tree, environment) states can cost anything.%s" % (D, R))
print(B + "─" * W + R)
print("  %-4s %-9s %-44s %-13s %-11s %s" % ("agt", "decision", "command", "effect", "peer", "Σ deleted"))
print(B + "─" * W + R)
PY

# ---------------------------------------------------------------- daemon ----
HP_HOME="$HOME_DIR" hindsight daemon --addr "127.0.0.1:$PORT" >"$HOME_DIR/daemon.log" 2>&1 &
DAEMON_PID=$!
for _ in $(seq 1 40); do
	curl -s -o /dev/null --max-time 1 "$URL/healthz" && break
	sleep 0.25
done
curl -s -o /dev/null --max-time 1 "$URL/healthz" ||
	{ echo "demo-live: daemon did not come up on $PORT (see $HOME_DIR/daemon.log)" >&2; exit 1; }

# ---------------------------------------------------------------- tailer ----
# Follows the daemon's log and renders one aligned line per decision. Reading
# the log rather than the fleet's stdout matters: the log is the artefact the
# claim is checked against later, so the screen and the evidence cannot drift.
python3 - "$HOME_DIR/log.jsonl" "$HOME_DIR/done.flag" <<'PY' &
import json, os, sys, time

log_path, done_flag = sys.argv[1], sys.argv[2]
tty = sys.stdout.isatty()


def c(code):
    return code if tty else ""


R = c("\033[0m"); D = c("\033[2m"); B = c("\033[1m")
GREEN = c("\033[32m"); YELLOW = c("\033[33m"); BLUE = c("\033[36m"); GREY = c("\033[90m")


def short_cmd(cmd, width=44):
    """Middle-elide. The head and the tail are what identify a test command."""
    if len(cmd) <= width:
        return cmd
    keep = width - 1
    head = keep // 2
    tail = keep - head
    return cmd[:head] + "…" + cmd[-tail:]


def secs(ms):
    return "%.2fs" % (ms / 1000.0)


deleted_ms = 0
executed_ms = 0
served = executed = waits = 0
explained = False
pos = 0
buf = ""

while True:
    finished = os.path.exists(done_flag)
    try:
        with open(log_path, encoding="utf-8") as fh:
            fh.seek(pos)
            chunk = fh.read()
            pos = fh.tell()
    except OSError:
        chunk = ""
    buf += chunk
    lines, buf = buf.split("\n")[:-1], buf.split("\n")[-1]

    for ln in lines:
        ln = ln.strip()
        if not ln:
            continue
        try:
            r = json.loads(ln)
        except ValueError:
            continue
        dec = r.get("decision") or ""
        if dec == "VERIFY":
            continue
        agent = r.get("agent") or "?"
        cmd = r.get("cmd") or ""
        dur = int(r.get("duration_ms") or 0)
        src = r.get("source_agent") or ""

        if dec == "HIT":
            served += 1
            deleted_ms += dur
            tag = GREEN + "%-9s" % "SERVED" + R
            effect = GREEN + "saved %s" % secs(dur) + R
            peer = "from %s" % src if src else ""
        elif dec == "LEASE_WAIT":
            # A coalesced wait avoided an execution exactly as a hit did: the
            # agent blocked on a peer instead of duplicating the work.
            served += 1
            waits += 1
            deleted_ms += dur
            tag = BLUE + "%-9s" % "WAIT" + R
            effect = BLUE + "saved %s" % secs(dur) + R
            peer = "onto %s" % src if src else ""
        elif dec == "MISS":
            executed += 1
            executed_ms += dur
            tag = YELLOW + "%-9s" % "EXECUTED" + R
            effect = "ran   %s" % secs(dur)
            peer = "recorded" if r.get("servable") is True else GREY + "unservable" + R
        else:
            executed += 1
            executed_ms += dur
            tag = GREY + "%-9s" % (dec or "PASS") + R
            effect = "ran   %s" % secs(dur)
            peer = ""

        print("  %-4s %s %-44s %-13s %-11s %s%6.1fs%s"
              % (agent, tag, short_cmd(cmd), effect, peer,
                 B, deleted_ms / 1000.0, R))
        sys.stdout.flush()

        # The anti-"trust me" moment: print the key components behind the first
        # avoided execution, so the audience sees a mechanical match rather
        # than a model deciding something looked similar enough.
        if not explained and dec in ("HIT", "LEASE_WAIT"):
            explained = True
            key = r.get("key") or ""
            print()
            print("    %s┌ why that replay was legal %s" % (D, "─" * 52 + R))
            print("    %s│%s key   %s" % (D, R, key[:46] + "…"))
            print("    %s│%s tree  %s   env  %s   cwd  %s"
                  % (D, R, (r.get("tree_before") or "")[:12] + "…",
                     (r.get("env_fp_before") or "")[:12] + "…", r.get("cwd_rel") or "."))
            print("    %s│%s cmd   %s" % (D, R, cmd))
            print("    %s│%s %sidentical tree + identical environment + identical command"
                  "  →  replay, not prediction%s" % (D, R, B, R))
            print("    %s└%s" % (D, "─" * 78 + R))
            print()
            sys.stdout.flush()

    if finished and not lines and not chunk:
        break
    time.sleep(0.15)

with open(done_flag + ".stats", "w", encoding="utf-8") as fh:
    json.dump({"served": served, "executed": executed, "waits": waits,
               "deleted_ms": deleted_ms, "executed_ms": executed_ms}, fh)
PY
TAILER_PID=$!

# ------------------------------------------------------------------ fleet ----
FLEET_START=$(python3 -c 'import time; print(time.time())')
FLEET_AGENT_CMD="bash $HERE/scripts/replay-agent.sh $CMDS" \
	bash "$HERE/scripts/fleet.sh" --repo "$REPO" --prompt "deterministic replay" \
	--agents "$AGENTS" --mode "$MODE" --out "$OUT" --hp-home "$HOME_DIR" \
	--daemon "$URL" --ref "$REF" --no-verify >"$HOME_DIR/fleet.log" 2>&1
FLEET_RC=$?
FLEET_END=$(python3 -c 'import time; print(time.time())')

# Let the tailer drain whatever landed in the last beat, then let it finish.
sleep 1
touch "$HOME_DIR/done.flag"
for _ in $(seq 1 40); do
	kill -0 "$TAILER_PID" 2>/dev/null || break
	sleep 0.25
done
kill "$TAILER_PID" 2>/dev/null
TAILER_PID=""

if [ "$FLEET_RC" != 0 ]; then
	echo
	echo "demo-live: fleet exited $FLEET_RC — tail of $HOME_DIR/fleet.log:" >&2
	tail -n 15 "$HOME_DIR/fleet.log" >&2
fi

cp "$HOME_DIR/log.jsonl" "$EVIDENCE/" 2>/dev/null
cp "$OUT/summary.json" "$EVIDENCE/" 2>/dev/null
cp "$OUT/summary.txt" "$EVIDENCE/" 2>/dev/null

# ----------------------------------------------------------------- result ----
python3 - "$HOME_DIR/log.jsonl" "$MODE" "$AGENTS" "$FLEET_START" "$FLEET_END" "$EVIDENCE" <<'PY'
import json, sys

log_path, mode, agents, t0, t1, evidence = sys.argv[1:7]
tty = sys.stdout.isatty()
B = "\033[1m" if tty else ""
D = "\033[2m" if tty else ""
R = "\033[0m" if tty else ""
G = "\033[32m" if tty else ""

served = executed = waits = 0
served_ms = executed_ms = 0
states = set()
try:
    rows = [json.loads(l) for l in open(log_path, encoding="utf-8") if l.strip()]
except OSError:
    rows = []

for r in rows:
    dec = r.get("decision")
    if dec == "VERIFY":
        continue
    dur = int(r.get("duration_ms") or 0)
    if dec in ("HIT", "LEASE_WAIT"):
        served += 1
        served_ms += dur
        if dec == "LEASE_WAIT":
            waits += 1
    elif dec:
        executed += 1
        executed_ms += dur
    states.add(r.get("key"))

demand = served + executed
counterfactual = executed_ms + served_ms
pct = (100.0 * served_ms / counterfactual) if counterfactual else 0.0
wall = float(t1) - float(t0)

W = 96
print()
print(B + "═" * W + R)
print(B + "  RESULT" + R)
print(B + "═" * W + R)


def row(label, value, extra=""):
    print("  %-34s %12s  %s" % (label, value, extra))


row("commands demanded", demand)
row("  executed", executed, "%sdistinct (command, tree, environment) states%s" % (D, R))
row("  served without executing", served)
row("    of which coalesced in flight", waits, "%sblocked on a peer already running it%s" % (D, R))
print()
row("execution-seconds  before", "%.1fs" % (counterfactual / 1000.0),
    "%sif every agent executed everything%s" % (D, R))
row("execution-seconds  after", "%.1fs" % (executed_ms / 1000.0))
print("  %-34s %s%12s%s  %s(%.0f%% deleted)%s"
      % ("execution-seconds deleted", G + B, "%.1fs" % (served_ms / 1000.0), R, D, pct, R))
print()
row("fleet wall clock", "%.1fs" % wall)
print("  %sWall clock is reported, never claimed: at this scale the bottleneck is agent"
      % D)
print("  latency, so deleted execution-seconds do not all become elapsed time.%s" % R)
print()
if mode == "cached":
    print("  %sWhy:%s %d commands were demanded but only %d distinct"
          % (B, R, demand, executed))
    print("  (command, tree hash, environment fingerprint) states existed among them.")
    print("  The other %d were the same command at the same state, so the cache replayed"
          % served)
    print("  bytes that had already been produced. Nothing was predicted.")
else:
    print("  %sControl arm.%s The hook recorded every result and was forbidden to serve any,"
          % (B, R))
    print("  so this is what the same work costs with no sharing at all.")
print()
print("  %sevidence  %s%s" % (D, evidence, R))
print(B + "═" * W + R)
print()
PY

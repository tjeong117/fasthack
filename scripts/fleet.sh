#!/usr/bin/env bash
#
# fleet.sh — cold fan-out harness for Hindsight.
#
# Fans N coding agents out onto one task in N git worktrees of a target repo,
# runs them all simultaneously, and reports the execution-seconds Hindsight
# deleted by serving one agent's recorded result to its peers.
#
# Written for bash 3.2 (the /bin/bash that ships with macOS): no associative
# arrays, no `wait -n`, no `mapfile`, no `${var,,}`.
#
# Requires: git, and a POSIX shell. python3 is used for the log summary and
# millisecond timing; both degrade gracefully if it is missing. jq is never
# required.

set -euo pipefail

PROG="$(basename "$0")"

# ---------------------------------------------------------------------------
# Defaults
# ---------------------------------------------------------------------------

REPO=""
PROMPT_ARG=""
AGENTS=5
MODE="cached"
DAEMON="${HP_DAEMON:-http://127.0.0.1:7777}"
OUT=""
REF="HEAD"
KEEP=0
# Verification runs by default in the cached arm. It is the answer to "how do
# you know it isn't lying to you", and a default-off credibility check is a
# credibility check that never runs.
VERIFY=1
VERIFY_LIMIT=25
# Fraction of served results re-executed in the background, in the state they
# were recorded in. This has to happen during the run: afterwards every agent
# has edited its worktree and no recorded state still exists to check against.
VERIFY_RATE=1.0
HINDSIGHT="${HINDSIGHT_BIN:-hindsight}"
DRY_RUN=0
AGENT_TIMEOUT=900
HP_HOME_OPT="${HP_HOME:-}"

# Populated during setup; read by the exit trap.
WORKTREES=()
AGENT_IDS=()
REPO_TOP=""
CLEANED=0

usage() {
	cat <<'EOF'
fleet.sh — fan N coding agents onto one task in N git worktrees, then report
           how many execution-seconds Hindsight deleted.

USAGE
  fleet.sh --repo <path> --prompt <file-or-string> [options]
  fleet.sh <N> <baseline|cached>          # compatibility form, see AGENTS.md

REQUIRED
  --repo <path>            Target git repository to fan out over.
  --prompt <file|string>   Task for every agent. If the value names a readable
                           file the file's contents are used, otherwise the
                           value is used literally.

OPTIONS
  --agents <N>             Number of agents and worktrees.      [5]
  --mode <baseline|cached> Control arm or treatment arm.        [cached]
  --daemon <url>           Hindsight daemon.   [$HP_DAEMON or 127.0.0.1:7777]
  --out <dir>              Scratch dir for worktrees.  [./.fleet/<timestamp>]
                           Must not be inside --repo; see the guard below.
  --ref <rev>              Revision every worktree starts from.  [HEAD]
  --hp-home <dir>          Force $HP_HOME for the agents and for the summary.
  --timeout <secs>         Per-agent wall-clock kill (0 disables).     [900]
  --keep                   Do not remove the worktrees on exit.
  --no-verify              Skip shadow re-execution of served results.
  --verify-limit <N>       How many served results to re-execute.        [25]
  --dry-run                Create the worktrees, print exactly what would be
                           launched, then exit without invoking any model.
  -h, --help               This text.

MODES
  baseline   HP_SERVE=0 — hook runs, records, serves nothing.
  cached     HP_SERVE=1 — hook runs, records, and serves peer results.

  Both modes run with the hook ENABLED. See the block comment at the mode
  handling below for why the control arm is not "hooks off".

OUTPUT
  <out>/logs/aN.log      full stdout+stderr of each agent
  <out>/summary.txt      the printed summary
  <out>/summary.json     the same numbers, machine-readable

EXAMPLES
  fleet.sh --repo ~/src/flask --prompt task.md --agents 5 --mode baseline
  fleet.sh --repo ~/src/flask --prompt task.md --agents 5 --mode cached
  fleet.sh --repo ~/src/flask --prompt "fix the parser bug" --dry-run
EOF
}

die() {
	printf '%s: error: %s\n' "$PROG" "$*" >&2
	exit 2
}

warn() {
	printf '%s: warning: %s\n' "$PROG" "$*" >&2
}

rule() {
	printf '%s\n' '---------------------------------------------------------------------'
}

# ---------------------------------------------------------------------------
# THE AGENT INVOCATION
#
# This is the single place that knows which coding-agent CLI we drive. To run
# the fleet on Claude Code instead of Codex, replace the one command below and
# update LAUNCH_DESC so --dry-run keeps telling the truth. Nothing else in this
# script needs to change.
#
# When this is called: cwd is already the agent's worktree, HP_* are already
# exported, and stdout/stderr are already redirected to the agent's log.
# ---------------------------------------------------------------------------

launch_agent() {
	local prompt="$1"

	# FLEET_AGENT_CMD overrides the driver. It receives the prompt as $1 and
	# runs in the agent's worktree with HP_* already exported. This exists so
	# the baseline/cached comparison can be driven by a deterministic command
	# sequence: a real model rerun introduces enough variance between the two
	# arms to swamp the effect being measured, which makes it a poor control.
	if [ -n "${FLEET_AGENT_CMD:-}" ]; then
		sh -c "$FLEET_AGENT_CMD" _ "$prompt"
		return
	fi

	codex exec --dangerously-bypass-hook-trust "$prompt"

	# Claude Code equivalent:
	# claude -p "$prompt" --permission-mode bypassPermissions
}

# Human-readable form of the above, printed by --dry-run. Keep in sync.
LAUNCH_DESC='codex exec --dangerously-bypass-hook-trust "<prompt>"'
if [ -n "${FLEET_AGENT_CMD:-}" ]; then
	LAUNCH_DESC="sh -c '$FLEET_AGENT_CMD' _ \"<prompt>\""
fi

# The binary that launch_agent needs on PATH, for the preflight check.
LAUNCH_BIN='codex'
if [ -n "${FLEET_AGENT_CMD:-}" ]; then
	LAUNCH_BIN='sh'
fi

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------

POSITIONAL=()

need() {
	# need <flag> <argc-remaining>
	[ "$2" -ge 2 ] || die "$1 requires a value"
}

while [ $# -gt 0 ]; do
	case "$1" in
	--repo=*) REPO="${1#*=}"; shift ;;
	--repo) need "$1" $#; REPO="$2"; shift 2 ;;
	--prompt=*) PROMPT_ARG="${1#*=}"; shift ;;
	--prompt) need "$1" $#; PROMPT_ARG="$2"; shift 2 ;;
	--agents=*) AGENTS="${1#*=}"; shift ;;
	--agents) need "$1" $#; AGENTS="$2"; shift 2 ;;
	--mode=*) MODE="${1#*=}"; shift ;;
	--mode) need "$1" $#; MODE="$2"; shift 2 ;;
	--daemon=*) DAEMON="${1#*=}"; shift ;;
	--daemon) need "$1" $#; DAEMON="$2"; shift 2 ;;
	--out=*) OUT="${1#*=}"; shift ;;
	--out) need "$1" $#; OUT="$2"; shift 2 ;;
	--ref=*) REF="${1#*=}"; shift ;;
	--ref) need "$1" $#; REF="$2"; shift 2 ;;
	--hp-home=*) HP_HOME_OPT="${1#*=}"; shift ;;
	--hp-home) need "$1" $#; HP_HOME_OPT="$2"; shift 2 ;;
	--timeout=*) AGENT_TIMEOUT="${1#*=}"; shift ;;
	--timeout) need "$1" $#; AGENT_TIMEOUT="$2"; shift 2 ;;
	--keep) KEEP=1; shift ;;
	--no-verify) VERIFY=0; VERIFY_RATE=0; shift ;;
	--verify-rate) VERIFY_RATE="$2"; shift 2 ;;
	--verify-limit) VERIFY_LIMIT="$2"; shift 2 ;;
	--dry-run) DRY_RUN=1; shift ;;
	-h | --help) usage; exit 0 ;;
	--) shift; while [ $# -gt 0 ]; do POSITIONAL[${#POSITIONAL[@]}]="$1"; shift; done ;;
	-*) die "unknown option: $1 (try --help)" ;;
	*) POSITIONAL[${#POSITIONAL[@]}]="$1"; shift ;;
	esac
done

# Compatibility form documented in AGENTS.md: `fleet.sh 5 baseline`. The repo
# defaults to whichever git repo the caller is standing in.
if [ "${#POSITIONAL[@]}" -eq 2 ] && [ -z "$REPO" ]; then
	case "${POSITIONAL[0]}" in
	'' | *[!0-9]*) die "unexpected arguments: ${POSITIONAL[*]}" ;;
	esac
	AGENTS="${POSITIONAL[0]}"
	MODE="${POSITIONAL[1]}"
	REPO="$(git rev-parse --show-toplevel 2>/dev/null || true)"
	[ -n "$REPO" ] || die "positional form needs to be run inside a git repo, or pass --repo"
elif [ "${#POSITIONAL[@]}" -gt 0 ]; then
	die "unexpected arguments: ${POSITIONAL[*]}"
fi

# ---------------------------------------------------------------------------
# Validation
# ---------------------------------------------------------------------------

[ -n "$REPO" ] || { usage >&2; die "--repo is required"; }

case "$AGENTS" in
'' | *[!0-9]*) die "--agents must be a positive integer, got '$AGENTS'" ;;
esac
[ "$AGENTS" -ge 1 ] || die "--agents must be at least 1"

case "$AGENT_TIMEOUT" in
'' | *[!0-9]*) die "--timeout must be a non-negative integer, got '$AGENT_TIMEOUT'" ;;
esac

case "$MODE" in
baseline | cached) : ;;
*) die "--mode must be 'baseline' or 'cached', got '$MODE'" ;;
esac

[ -d "$REPO" ] || die "--repo is not a directory: $REPO"
git -C "$REPO" rev-parse --git-dir >/dev/null 2>&1 || die "--repo is not a git repository: $REPO"
REPO_TOP="$(cd "$REPO" && pwd -P)"
REPO_TOP="$(git -C "$REPO_TOP" rev-parse --show-toplevel)"
REPO_TOP="$(cd "$REPO_TOP" && pwd -P)"
git -C "$REPO_TOP" rev-parse --verify --quiet "$REF^{commit}" >/dev/null 2>&1 ||
	die "--ref '$REF' does not resolve to a commit in $REPO_TOP"
REF_SHA="$(git -C "$REPO_TOP" rev-parse --short "$REF^{commit}")"

# Prompt: a readable file wins, otherwise the value is the prompt.
if [ -z "$PROMPT_ARG" ]; then
	usage >&2
	die "--prompt is required"
fi
if [ -f "$PROMPT_ARG" ] && [ -r "$PROMPT_ARG" ]; then
	PROMPT_TEXT="$(cat "$PROMPT_ARG")"
	PROMPT_SRC="file:$PROMPT_ARG"
else
	PROMPT_TEXT="$PROMPT_ARG"
	PROMPT_SRC="literal"
fi
[ -n "$PROMPT_TEXT" ] || die "prompt is empty ($PROMPT_SRC)"

HAVE_PY=0
if command -v python3 >/dev/null 2>&1; then HAVE_PY=1; fi

now_ms() {
	if [ "$HAVE_PY" = 1 ]; then
		python3 -c 'import time; print(int(time.time()*1000))'
	else
		printf '%s000\n' "$(date +%s)"
	fi
}

now_epoch() {
	if [ "$HAVE_PY" = 1 ]; then
		python3 -c 'import time; print(repr(time.time()))'
	else
		date +%s
	fi
}

# ---------------------------------------------------------------------------
# Scratch directory
#
# The default lives beside the *invoking* directory, not inside --repo. Putting
# worktrees inside the target repo would add untracked files to the very tree
# whose hash keys the cache, so every agent would compute a different key and
# the demo would silently produce zero hits. That is a correctness bug, not an
# inconvenience, so it is a hard error rather than a warning.
# ---------------------------------------------------------------------------

STAMP="$(date +%Y%m%d-%H%M%S)"
if [ -z "$OUT" ]; then
	OUT="$PWD/.fleet/$STAMP"
fi

# Canonicalise before creating anything, so a rejected --out does not leave a
# stray directory inside the repo it was rejected for being inside of.
canon() {
	if [ "$HAVE_PY" = 1 ]; then
		python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$1"
	else
		mkdir -p "$1" && (cd "$1" && pwd -P)
	fi
}
OUT="$(canon "$OUT")"

case "$OUT/" in
"$REPO_TOP"/*)
	die "refusing to put worktrees inside the target repo.
    out:  $OUT
    repo: $REPO_TOP
  Untracked worktrees inside the tree change the tree hash that keys the
  cache. Pass --out with a directory outside $REPO_TOP."
	;;
esac

mkdir -p "$OUT" "$OUT/logs" "$OUT/.status"

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------

cleanup() {
	local rc=$?
	trap - EXIT INT TERM
	[ "$CLEANED" = 0 ] || return 0
	CLEANED=1

	if [ "$KEEP" = 1 ]; then
		printf '\n%s: --keep set, leaving worktrees in %s\n' "$PROG" "$OUT" >&2
		return "$rc"
	fi

	local wt
	for wt in ${WORKTREES[@]+"${WORKTREES[@]}"}; do
		[ -e "$wt" ] || continue
		git -C "$REPO_TOP" worktree remove --force "$wt" >/dev/null 2>&1 || rm -rf "$wt"
	done
	git -C "$REPO_TOP" worktree prune >/dev/null 2>&1 || true

	# Logs and the summary are evidence; they outlive the worktrees. The
	# status files are internal scratch and do not.
	rm -rf "$OUT/.status" >/dev/null 2>&1 || true
	rmdir "$OUT/logs" >/dev/null 2>&1 || true
	rmdir "$OUT" >/dev/null 2>&1 || true
	return "$rc"
}
trap 'cleanup' EXIT
trap 'cleanup || true; exit 130' INT
trap 'cleanup || true; exit 143' TERM

# ---------------------------------------------------------------------------
# Preflight — every one of these is a warning, never a failure. The hook fails
# open by design, so a fleet run is still a valid measurement with no daemon
# and no binary; it just measures the uncached world.
# ---------------------------------------------------------------------------

HINDSIGHT_OK=1
DAEMON_OK=1

if ! command -v hindsight >/dev/null 2>&1; then
	HINDSIGHT_OK=0
	warn "'hindsight' is not on PATH — the hook cannot record or serve.
           Agents will run normally and the cache summary will be empty."
fi

if command -v curl >/dev/null 2>&1; then
	if ! curl -s -o /dev/null --max-time 2 "$DAEMON/healthz" 2>/dev/null; then
		DAEMON_OK=0
		warn "daemon unreachable at $DAEMON — the hook will fail open.
           Start one with:  HP_DAEMON=$DAEMON hindsight daemon"
	fi
else
	DAEMON_OK=0
	warn "curl not found; skipping the daemon reachability check."
fi

if [ "$DRY_RUN" = 0 ] && ! command -v "$LAUNCH_BIN" >/dev/null 2>&1; then
	warn "'$LAUNCH_BIN' is not on PATH — every agent will fail immediately.
           Edit launch_agent() to drive a different CLI, or use --dry-run."
fi

if [ "$HAVE_PY" = 0 ]; then
	warn "python3 not found; timings fall back to whole seconds and the log
           summary will be skipped."
fi

# ---------------------------------------------------------------------------
# Mode
#
# THE TWO ARMS DIFFER ONLY IN WHETHER CACHE HITS ARE SERVED, NEVER IN WHETHER
# MEASUREMENT HAPPENS. Both run with HP_ENABLE=1, so both carry identical
# instrumentation and identical hook overhead; the single variable between them
# is HP_SERVE. A control arm measured differently from the treatment arm is not
# a control arm — it is a second, incomparable experiment, and any speedup it
# appears to show is partly just the difference in measurement apparatus.
# ---------------------------------------------------------------------------

if [ "$MODE" = "cached" ]; then
	HP_SERVE_VAL=1
else
	HP_SERVE_VAL=0
fi

# ---------------------------------------------------------------------------
# Worktrees
# ---------------------------------------------------------------------------

printf '\n'
rule
printf '  hindsight fleet  ·  mode=%s  agents=%s  ref=%s\n' "$MODE" "$AGENTS" "$REF_SHA"
printf '  repo   %s\n' "$REPO_TOP"
printf '  out    %s\n' "$OUT"
printf '  prompt %s (%s bytes)\n' "$PROMPT_SRC" "${#PROMPT_TEXT}"
rule

i=1
while [ "$i" -le "$AGENTS" ]; do
	id="a$i"
	wt="$OUT/$id"
	# Detached HEAD: this is a measurement harness, so we care about execution
	# seconds, not about the branches the agents leave behind. Detaching also
	# makes worktree names collision-free across repeat runs.
	git -C "$REPO_TOP" worktree add --detach "$wt" "$REF" >/dev/null 2>&1 ||
		die "failed to create worktree $wt from $REF"
	AGENT_IDS[${#AGENT_IDS[@]}]="$id"
	WORKTREES[${#WORKTREES[@]}]="$wt"
	i=$((i + 1))
done
printf '  created %s worktrees\n' "$AGENTS"

# ---------------------------------------------------------------------------
# Dry run
# ---------------------------------------------------------------------------

PROMPT_FIRSTLINE="$(printf '%s' "$PROMPT_TEXT" | head -n 1 | cut -c1-72)"

if [ "$DRY_RUN" = 1 ]; then
	printf '\n  DRY RUN — nothing is launched. Each agent would run, simultaneously:\n\n'
	idx=0
	while [ "$idx" -lt "${#AGENT_IDS[@]}" ]; do
		id="${AGENT_IDS[$idx]}"
		wt="${WORKTREES[$idx]}"
		printf '  %s\n' "$id"
		printf '    cwd   %s\n' "$wt"
		printf '    env   HP_AGENT=%s HP_ENABLE=1 HP_SERVE=%s HP_DAEMON=%s\n' \
			"$id" "$HP_SERVE_VAL" "$DAEMON"
		if [ -n "$HP_HOME_OPT" ]; then
			printf '          HP_HOME=%s\n' "$HP_HOME_OPT"
		fi
		printf '    exec  %s\n' "$LAUNCH_DESC"
		printf '    log   %s\n' "$OUT/logs/$id.log"
		idx=$((idx + 1))
	done
	printf '\n    prompt (%s, first line)  %s\n' "$PROMPT_SRC" "$PROMPT_FIRSTLINE"
	printf '    per-agent timeout        %ss\n' "$AGENT_TIMEOUT"
	printf '    hindsight on PATH        %s\n' "$([ "$HINDSIGHT_OK" = 1 ] && echo yes || echo 'no (hook fails open)')"
	printf '    daemon reachable         %s\n' "$([ "$DAEMON_OK" = 1 ] && echo yes || echo 'no (hook fails open)')"
	printf '\n'
	rule
	exit 0
fi

# ---------------------------------------------------------------------------
# Launch — all agents at once. This is a COLD fan-out: the whole phenomenon
# Hindsight exploits is that simultaneously-launched agents issue identical
# commands in their first few steps. Staggering the launch would manufacture
# cache hits that a real fan-out would have to earn through the lease.
# ---------------------------------------------------------------------------

run_one() {
	local id="$1" wt="$2"
	local log="$OUT/logs/$id.log"
	local start end child watchdog rc

	start="$(now_ms)"
	(
		cd "$wt" || exit 127
		export HP_AGENT="$id"
		export HP_ENABLE=1
		export HP_SERVE="$HP_SERVE_VAL"
		export HP_VERIFY_RATE="$VERIFY_RATE"
		export HP_DAEMON="$DAEMON"
		if [ -n "$HP_HOME_OPT" ]; then export HP_HOME="$HP_HOME_OPT"; fi
		launch_agent "$PROMPT_TEXT"
	) >"$log" 2>&1 </dev/null &
	child=$!

	watchdog=""
	if [ "$AGENT_TIMEOUT" -gt 0 ]; then
		(
			sleep "$AGENT_TIMEOUT"
			kill -TERM "$child" 2>/dev/null || true
			sleep 5
			kill -KILL "$child" 2>/dev/null || true
		) >/dev/null 2>&1 &
		watchdog=$!
	fi

	rc=0
	wait "$child" || rc=$?

	if [ -n "$watchdog" ]; then
		kill "$watchdog" 2>/dev/null || true
		wait "$watchdog" 2>/dev/null || true
	fi

	end="$(now_ms)"
	printf '%s\t%s\t%s\n' "$id" "$rc" "$((end - start))" >"$OUT/.status/$id"
}

printf '\n  launching %s agents simultaneously (mode=%s, HP_SERVE=%s)\n\n' \
	"$AGENTS" "$MODE" "$HP_SERVE_VAL"

RUN_START_EPOCH="$(now_epoch)"
FLEET_START_MS="$(now_ms)"

PIDS=()
idx=0
while [ "$idx" -lt "${#AGENT_IDS[@]}" ]; do
	run_one "${AGENT_IDS[$idx]}" "${WORKTREES[$idx]}" &
	PIDS[${#PIDS[@]}]=$!
	idx=$((idx + 1))
done

for p in ${PIDS[@]+"${PIDS[@]}"}; do
	wait "$p" || true
done

FLEET_END_MS="$(now_ms)"
FLEET_WALL_MS=$((FLEET_END_MS - FLEET_START_MS))

# ---------------------------------------------------------------------------
# $HP_HOME resolution
#
# The daemon is the single writer of log.jsonl and it owns $HP_HOME. If the
# caller did not pin one we guess the documented default and then, failing
# that, fall back to the most recently written log under ~/.hindsight. We
# always print which file the numbers came from, because a summary that
# silently reads the wrong run is worse than no summary.
# ---------------------------------------------------------------------------

resolve_log() {
	local candidate

	if [ -n "$HP_HOME_OPT" ]; then
		printf '%s/log.jsonl\n' "${HP_HOME_OPT%/}"
		return 0
	fi

	local base="$HOME/.hindsight"
	local name
	name="$(basename "$REPO_TOP")"

	if [ "$HAVE_PY" = 1 ]; then
		local hashed
		hashed="$(python3 -c 'import hashlib,sys; print(hashlib.sha256(sys.argv[1].encode()).hexdigest()[:12])' "$REPO_TOP")"
		for candidate in "$base/$name-$hashed" "$base/$hashed" "$base/$name"; do
			if [ -f "$candidate/log.jsonl" ]; then
				printf '%s/log.jsonl\n' "$candidate"
				return 0
			fi
		done
	fi

	# Last resort: newest log.jsonl anywhere under ~/.hindsight. These paths are
	# repo-ids the daemon generates, so `ls -t` is safe here and `find` has no
	# portable mtime sort.
	# shellcheck disable=SC2012
	candidate="$(ls -t "$base"/*/log.jsonl 2>/dev/null | head -n 1 || true)"
	if [ -n "$candidate" ]; then
		printf '%s\n' "$candidate"
		return 0
	fi

	printf '%s/<repo-id>/log.jsonl\n' "$base"
	return 1
}

LOG_PATH="$(resolve_log || true)"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

{
	printf '\n'
	rule
	printf '  fleet complete  ·  mode=%s  agents=%s  ref=%s\n' "$MODE" "$AGENTS" "$REF_SHA"
	rule
	printf '\n  %-8s %-10s %12s\n' "agent" "status" "wall"
	idx=0
	SLOWEST_MS=0
	while [ "$idx" -lt "${#AGENT_IDS[@]}" ]; do
		id="${AGENT_IDS[$idx]}"
		sf="$OUT/.status/$id"
		if [ -f "$sf" ]; then
			arc="$(cut -f2 "$sf")"
			ams="$(cut -f3 "$sf")"
		else
			arc="?"
			ams=0
		fi
		if [ "$arc" = "0" ]; then astat="ok"; else astat="exit $arc"; fi
		if [ "$ams" -gt "$SLOWEST_MS" ]; then SLOWEST_MS="$ams"; fi
		printf '  %-8s %-10s %11ss\n' "$id" "$astat" "$(printf '%d.%01d' $((ams / 1000)) $(((ams % 1000) / 100)))"
		idx=$((idx + 1))
	done
	printf '\n  %-38s %11ss\n' "total wall clock (fan-out)" \
		"$(printf '%d.%01d' $((FLEET_WALL_MS / 1000)) $(((FLEET_WALL_MS % 1000) / 100)))"
	printf '  %-38s %11ss\n' "slowest agent" \
		"$(printf '%d.%01d' $((SLOWEST_MS / 1000)) $(((SLOWEST_MS % 1000) / 100)))"
	printf '\n'
} | tee "$OUT/summary.txt"

# Shadow verification, before the worktrees are removed.
#
# A served result is only worth anything if we can show it matches a real
# re-execution, and that check is only meaningful in the worktree and state it
# was recorded in — which exists right now and will not in a moment. Running it
# afterwards is why the first real fan-out reported "verified 0" while serving
# 29 results: the mechanism was built and never fired.
if [ "$MODE" = "cached" ] && [ "$VERIFY" = 1 ] && [ ${#WORKTREES[@]} -gt 0 ]; then
	{
		printf '\n  shadow verification\n'
		# Verify in a pristine worktree at the starting revision, not in an
		# agent's. By now every agent has edited its own tree, so almost every
		# recorded state has moved and would simply be skipped — which reports
		# a near-empty verification and proves nothing. The states worth
		# checking are the ones all the agents shared before they diverged,
		# and this is where those still exist.
		verify_wt="$OUT/verify"
		if git -C "$REPO_TOP" worktree add --detach "$verify_wt" "$REF" >/dev/null 2>&1; then
			WORKTREES[${#WORKTREES[@]}]="$verify_wt"
		else
			verify_wt="${WORKTREES[0]}"
			printf '  (could not create a pristine worktree; verifying in %s)\n' "$verify_wt"
		fi
		if [ -d "$verify_wt" ]; then
			(
				cd "$verify_wt" || exit 1
				export HP_ENABLE=1 HP_DAEMON="$DAEMON"
				if [ -n "$HP_HOME_OPT" ]; then export HP_HOME="$HP_HOME_OPT"; fi
				"$HINDSIGHT" verify --limit "$VERIFY_LIMIT" --quiet 2>&1 | sed 's/^/  /'
			) || printf '  (divergence found — see above)\n'
		else
			printf '  skipped: no worktree left to verify in\n'
		fi
	} | tee -a "$OUT/summary.txt"
fi

if [ "$HAVE_PY" = 1 ] && [ -f "$LOG_PATH" ]; then
	python3 - "$LOG_PATH" "$RUN_START_EPOCH" "$OUT/summary.json" "$MODE" "$AGENTS" "$FLEET_WALL_MS" <<'PY' | tee -a "$OUT/summary.txt"
import json
import sys

log_path, start_s, json_out, mode, agents, fleet_wall_ms = sys.argv[1:7]
start = float(start_s)

EXECUTED = ("MISS", "PASSTHROUGH")

records, malformed = [], 0
with open(log_path, "r", errors="replace") as fh:
    for line in fh:
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except ValueError:
            malformed += 1
            continue
        if isinstance(obj, dict):
            records.append(obj)


def ts_of(r):
    try:
        return float(r.get("ts") or 0.0)
    except (TypeError, ValueError):
        return 0.0


windowed = [r for r in records if ts_of(r) >= start - 5.0]
scoped = "this run"
if not windowed and records:
    # Either the daemon writes a clock we do not recognise, or this run logged
    # nothing. Say so rather than quietly reporting an empty fleet.
    windowed = records
    scoped = "WHOLE FILE - no records inside this run's time window"

served = executed = waits = 0
served_ms = executed_ms = wait_ms = 0
verified_true = verified_false = 0
per_agent = {}


def count_verdict(r):
    """Tally one shadow-verification verdict. Returns True if it was one."""
    global verified_true, verified_false
    if r.get("verified") is True:
        verified_true += 1
        return True
    if r.get("verified") is False:
        verified_false += 1
        return True
    return False


for r in windowed:
    dec = str(r.get("decision") or "")
    agent = str(r.get("agent") or "?")
    try:
        dur = int(r.get("duration_ms") or 0)
    except (TypeError, ValueError):
        dur = 0

    # Shadow verification emits its own terminal records under a synthetic
    # agent, rather than annotating the HIT it re-executed. Reading `verified`
    # off HIT records alone reported verified_true=0/verified_false=0 for runs
    # whose log held nine divergences -- a machine-readable clean bill of
    # health that the log contradicted. Count the VERIFY records, and keep the
    # verifier out of per_agent: it is an auditor, not a fleet member, and a
    # sixth all-zero lane in a five-agent run reads as a bug.
    if dec == "VERIFY":
        count_verdict(r)
        continue

    slot = per_agent.setdefault(agent, {"hit": 0, "exec": 0, "wait": 0, "deleted_ms": 0, "exec_ms": 0})
    if dec == "HIT":
        served += 1
        served_ms += dur
        slot["hit"] += 1
        slot["deleted_ms"] += dur
        # Still honoured for the inline form, where a verdict rides the HIT.
        count_verdict(r)
    elif dec in EXECUTED:
        executed += 1
        executed_ms += dur
        slot["exec"] += 1
        slot["exec_ms"] += dur
    elif dec == "LEASE_WAIT":
        # A lease wait avoided an execution exactly as a hit did: the agent
        # blocked on a peer instead of duplicating the work, and never ran the
        # command. Counting it only as "waiting" understates what was deleted
        # and stops the arithmetic from reconciling against the baseline arm.
        served += 1
        served_ms += dur
        waits += 1
        wait_ms += dur
        slot["wait"] += 1
        slot["deleted_ms"] += dur

demand = served + executed
hit_rate = (100.0 * served / demand) if demand else 0.0
counterfactual_ms = executed_ms + served_ms

out = []
w = out.append
w("  cache log  %s" % log_path)
w("  scope      %s   (%d of %d lines)" % (scoped, len(windowed), len(records)))
if malformed:
    w("  malformed  %d line(s) skipped" % malformed)
w("")
w("  %-38s %11d" % ("commands agents asked for", demand))
w("  %-38s %11d" % ("  executed  (MISS + PASSTHROUGH)", executed))
w("  %-38s %11d" % ("  served    (HIT + LEASE_WAIT)", served))
w("  %-38s %11d" % ("    of which coalesced in flight", waits))
w("")
w("  %-38s %10.1fs" % ("execution-seconds spent", executed_ms / 1000.0))
w("  %-38s %10.1fs" % ("execution-seconds DELETED", served_ms / 1000.0))
if waits:
    # Not the time these agents spent blocked: the daemon discards that and
    # stamps a LEASE_WAIT record with the execution time it deleted instead.
    # This is the share of the deleted total that the lease earned rather than
    # the index -- work that no cache could have served because no peer had
    # finished it yet.
    w("  %-38s %10.1fs" % ("  of which won by in-flight leases", wait_ms / 1000.0))
w("  %-38s %10.1fs" % ("if every agent executed everything", counterfactual_ms / 1000.0))
w("  %-38s %10.1f%%" % ("hit rate", hit_rate))
verified_checked = verified_true + verified_false
if verified_checked:
    w("  %-38s %11d" % ("shadow-verified served results", verified_true))
    w("  %-38s %11d" % ("  re-executed and checked", verified_checked))
if verified_false:
    w("")
    w("  *** %d DIVERGENT served result(s) — investigate before quoting ***" % verified_false)
if served == 0 and mode == "cached":
    w("")
    w("  note: zero hits. Check that the daemon is up and that HP_SERVE=1")
    w("        reached the agents.")
w("")
w("  %-8s %6s %6s %6s %12s" % ("agent", "hit", "exec", "wait", "deleted"))
for agent in sorted(per_agent):
    s = per_agent[agent]
    w("  %-8s %6d %6d %6d %11.1fs" % (agent, s["hit"], s["exec"], s["wait"], s["deleted_ms"] / 1000.0))
w("")

print("\n".join(out))

with open(json_out, "w") as fh:
    json.dump(
        {
            "mode": mode,
            "agents": int(agents),
            "log": log_path,
            "scope": scoped,
            "fleet_wall_ms": int(fleet_wall_ms),
            "demand": demand,
            "executed": executed,
            "served": served,
            "lease_waits": waits,
            "executed_ms": executed_ms,
            "deleted_ms": served_ms,
            "counterfactual_ms": counterfactual_ms,
            "wait_ms": wait_ms,
            "hit_rate_pct": round(hit_rate, 2),
            "verified_true": verified_true,
            "verified_false": verified_false,
            "verified_checked": verified_checked,
            "per_agent": per_agent,
        },
        fh,
        indent=2,
        sort_keys=True,
    )
    fh.write("\n")
PY
else
	{
		printf '  cache log  %s\n' "$LOG_PATH"
		if [ "$HAVE_PY" = 0 ]; then
			printf '  skipped: python3 is required for the log summary.\n'
		else
			printf '  skipped: no log at that path yet.\n'
			printf '           Set HP_HOME or pass --hp-home to point at the daemon'\''s home.\n'
		fi
		printf '\n'
	} | tee -a "$OUT/summary.txt"
fi

{
	rule
	printf '  agent logs   %s/logs\n' "$OUT"
	printf '  summary      %s/summary.txt\n' "$OUT"
	rule
	printf '\n'
} | tee -a "$OUT/summary.txt"

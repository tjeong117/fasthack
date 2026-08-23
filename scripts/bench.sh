#!/usr/bin/env bash
#
# End-to-end hook latency.
#
# The Go benchmarks in internal/hp/perf_test.go measure the pieces. This
# measures what an agent actually waits for: a real PreToolUse payload piped
# into a freshly spawned `hindsight hook`, against a real daemon, in a real git
# worktree. That total is the tax on every command the cache fails to serve, so
# it is the number the break-even threshold is computed from.
#
# Five paths are timed separately, because they cost very different things:
#
#   disabled     HP_ENABLE unset. Binary spawn and nothing else, which is the
#                floor nobody can get under.
#   passthrough  The classifier rejects the command before any git work.
#   known-fast   The duration memo has seen this command run under the floor
#                three times and bails before hashing anything.
#   miss         The full key path plus a daemon round trip, ending in a
#                rewrite to `hindsight record`. A unique command each
#                iteration, so no two share a key.
#   hit          The same, ending in a rewrite that replays a recorded result.
#   daemon-down  The full key path, then a refused connection and a
#                passthrough. This is what every user pays who has not started
#                the daemon, in exchange for nothing.
#
# The command used for the hit path has to clear the minimum duration floor, or
# nothing it produces is servable and there is no hit path to measure. Sorting a
# generated data file and taking the last line takes about 750 ms and prints one
# line, which clears the floor without risking output truncation.
#
# Usage: bash scripts/bench.sh [--iterations N] [--large N] [--with-go]
#
# Everything it creates lives under one temp directory and is removed on exit,
# including on failure. The daemon it starts is killed the same way.

set -euo pipefail

ITERATIONS=50
LARGE_FILES=20000
SMALL_FILES=100
WITH_GO=0

while [ $# -gt 0 ]; do
	case "$1" in
	--iterations)
		ITERATIONS="$2"
		shift 2
		;;
	--large)
		LARGE_FILES="$2"
		shift 2
		;;
	--with-go)
		WITH_GO=1
		shift
		;;
	-h | --help)
		echo "usage: bash scripts/bench.sh [--iterations N] [--large N] [--with-go]"
		exit 0
		;;
	*)
		echo "bench.sh: unknown argument $1" >&2
		exit 2
		;;
	esac
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK=""
DAEMON_PID=""

cleanup() {
	status=$?
	if [ -n "$DAEMON_PID" ]; then
		kill "$DAEMON_PID" 2>/dev/null || true
		wait "$DAEMON_PID" 2>/dev/null || true
	fi
	if [ -n "$WORK" ] && [ -d "$WORK" ]; then
		# The fixtures are git repositories full of generated files and the
		# cache home is full of blobs. Both are large and neither is wanted.
		rm -rf "$WORK"
	fi
	exit $status
}
trap cleanup EXIT INT TERM

need() {
	command -v "$1" >/dev/null 2>&1 || {
		echo "bench.sh: $1 is required" >&2
		exit 1
	}
}
need go
need git
need perl
need curl
perl -MTime::HiRes -MJSON::PP -e1 2>/dev/null || {
	echo "bench.sh: perl needs the core modules Time::HiRes and JSON::PP" >&2
	exit 1
}

# An inherited GIT_DIR, GIT_INDEX_FILE or GIT_WORK_TREE points every git call
# in here at somebody else's repository, which fails confusingly a hundred
# lines later. They are never wanted in a benchmark.
unset GIT_DIR GIT_INDEX_FILE GIT_WORK_TREE GIT_OBJECT_DIRECTORY

WORK="$(mktemp -d "${TMPDIR:-/tmp}/hindsight-bench-XXXXXX")"
BIN="$WORK/hindsight"
DRIVER="$WORK/driver.pl"
export HP_HOME="$WORK/home"
export HP_AGENT="bench"
mkdir -p "$HP_HOME"

echo "hindsight end-to-end hook latency"
echo "  work dir    $WORK"
echo "  iterations  $ITERATIONS per path"
echo

# ---------------------------------------------------------------------------
# The timing driver
#
# Written in perl rather than bash because bash 3.2, which is what macOS ships,
# has no sub-second clock. It forks and execs the hook directly with stdin
# redirected, so the measured window contains the hook and nothing else. Note
# that a real harness runs hooks under "$SHELL -lc", which adds a login shell
# spawn on top of every number here.
# ---------------------------------------------------------------------------

cat >"$DRIVER" <<'PERL'
use strict;
use warnings;
use Time::HiRes qw(time);
use JSON::PP;

my $json = JSON::PP->new->canonical;

sub payload_for {
    my ($cwd, $cmd) = @_;
    return $json->encode({
        session_id => 'bench',
        cwd        => $cwd,
        tool_name  => 'Bash',
        tool_input => { command => $cmd },
    });
}

# run_hook forks and execs the hook with stdin bound to a payload file. The
# clock starts immediately before the fork and stops after the child is
# reaped, so it measures exactly what the harness waits for.
sub run_hook {
    my ($bin, $payload_file, $out_file) = @_;
    my $t0  = time;
    my $pid = fork();
    die "fork: $!" unless defined $pid;
    if ($pid == 0) {
        open(STDIN,  '<', $payload_file) or exit 127;
        open(STDOUT, '>', $out_file // '/dev/null') or exit 127;
        open(STDERR, '>', '/dev/null') or exit 127;
        exec($bin, 'hook') or exit 127;
    }
    waitpid($pid, 0);
    return (time - $t0) * 1000;
}

sub write_payload {
    my ($file, $body) = @_;
    open(my $fh, '>', $file) or die "open $file: $!";
    print $fh $body;
    close $fh;
}

# rewritten returns the command the hook told the harness to run instead, or
# the empty string when the hook emitted nothing (which is a passthrough).
sub rewritten {
    my ($file) = @_;
    open(my $fh, '<', $file) or return '';
    local $/;
    my $raw = <$fh>;
    close $fh;
    return '' unless defined $raw && $raw =~ /\S/;
    my $resp = eval { $json->decode($raw) };
    return '' unless $resp && ref $resp eq 'HASH';
    my $hso = $resp->{hookSpecificOutput} or return '';
    return $hso->{updatedInput}{command} // '';
}

sub percentile {
    my ($sorted, $p) = @_;
    my $i = int($p * (scalar(@$sorted) - 1) + 0.5);
    return $sorted->[$i];
}

# run_shell times a command the way the harness would run it, under sh -c.
# Used for the two costs that are not the hook itself: the `hindsight record`
# wrapper a miss is rewritten into, and the replay a hit is rewritten into.
sub run_shell {
    my ($cwd, $cmd) = @_;
    my $t0  = time;
    my $pid = fork();
    die "fork: $!" unless defined $pid;
    if ($pid == 0) {
        chdir($cwd) or exit 127;
        open(STDIN,  '<', '/dev/null') or exit 127;
        open(STDOUT, '>', '/dev/null') or exit 127;
        open(STDERR, '>', '/dev/null') or exit 127;
        exec('/bin/sh', '-c', $cmd) or exit 127;
    }
    waitpid($pid, 0);
    return (time - $t0) * 1000;
}

sub report {
    my (@times) = @_;
    my @sorted = sort { $a <=> $b } @times;
    my $sum = 0;
    $sum += $_ for @sorted;
    printf("%.3f %.3f %.3f %.3f %.3f %d\n",
        percentile(\@sorted, 0.50),
        percentile(\@sorted, 0.95),
        $sorted[0],
        $sorted[-1],
        $sum / scalar(@sorted),
        scalar(@sorted));
}

my $mode = shift @ARGV or die "usage: driver.pl <rewrite|hook|shell> ...\n";

if ($mode eq 'rewrite') {
    # One run, printing the rewritten command. Used to seed the cache, to
    # recover the exact `hindsight record` line a miss produces, and to confirm
    # that a path labelled "hit" really is one.
    my ($bin, $cwd, $cmd, $scratch) = @ARGV;
    write_payload("$scratch/payload", payload_for($cwd, $cmd));
    run_hook($bin, "$scratch/payload", "$scratch/out");
    print rewritten("$scratch/out"), "\n";
    exit 0;
}

if ($mode eq 'shell') {
    my ($cwd, $cmd, $n) = @ARGV;
    $n ||= 50;
    run_shell($cwd, $cmd) for 1 .. 3;
    my @times;
    push @times, run_shell($cwd, $cmd) for 1 .. $n;
    report(@times);
    exit 0;
}

die "unknown mode $mode\n" unless $mode eq 'hook';

my ($bin, $cwd, $cmd, $scratch, $n, $unique) = @ARGV;
$n      ||= 50;
$unique ||= 0;

# Warm up. The first spawn of a binary pays for dyld and the page cache, which
# a real session pays exactly once and would otherwise skew a 50-sample median.
for my $i (1 .. 3) {
    write_payload("$scratch/payload", payload_for($cwd, $unique ? "$cmd warmup$i" : $cmd));
    run_hook($bin, "$scratch/payload", undef);
}

my @times;
for my $i (1 .. $n) {
    my $this = $unique ? "$cmd iteration$i" : $cmd;
    # The payload is written before the clock starts; it is our cost, not the
    # hook's.
    write_payload("$scratch/payload", payload_for($cwd, $this));
    push @times, run_hook($bin, "$scratch/payload", undef);
}
report(@times);
PERL

# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

# make_repo builds a git repository of $2 small files under realistic nesting.
# awk writes them in one process because forking a shell per file would take
# longer than every measurement in this script combined.
make_repo() {
	repo="$1"
	count="$2"
	mkdir -p "$repo"
	(
		cd "$repo"
		git init -q
		git config user.email "bench@hindsight.invalid"
		git config user.name "hindsight bench"
		git config commit.gpgsign false
		git config gc.auto 0
		printf '.venv/\nnode_modules/\n' >.gitignore

		awk -v n="$count" 'BEGIN {
			for (i = 0; i < n; i++) {
				leaf = int(i / 20)
				printf("src/pkg%03d/mod%02d\n", int(leaf / 25), leaf % 25)
			}
		}' | sort -u | tr '\n' '\0' | xargs -0 mkdir -p

		awk -v n="$count" 'BEGIN {
			for (i = 0; i < n; i++) {
				leaf = int(i / 20)
				f = sprintf("src/pkg%03d/mod%02d/file%05d.go", int(leaf / 25), leaf % 25, i)
				printf("// %s\npackage fixture\n\n", f) > f
				for (j = 0; j < 12; j++) printf("func Fixture%02d() int { return %d }\n", j, j * 7) > f
				close(f)
			}
		}'

		# Something for a genuinely slow read to chew on. Committed rather than
		# gitignored, so the hit path being measured does not quietly depend on
		# reading a file the key cannot see.
		awk 'BEGIN {
			srand(7)
			for (i = 0; i < 400000; i++)
				printf("line %08d payloadpayloadpayload\n", int(rand() * 99999999))
		}' >bench-data.txt

		git add -A
		git commit -q -m fixture
	)
}

echo "building hindsight"
go build -o "$BIN" "$ROOT/cmd/hindsight"

SMALL_REPO="$WORK/small"
LARGE_REPO="$WORK/large"
echo "generating fixtures ($SMALL_FILES files, $LARGE_FILES files)"
make_repo "$SMALL_REPO" "$SMALL_FILES"
make_repo "$LARGE_REPO" "$LARGE_FILES"

# ---------------------------------------------------------------------------
# Daemon
# ---------------------------------------------------------------------------

# free_port asks the kernel for an unused one rather than guessing, because
# 7777 and 7778 are routinely already held by a teammate's daemon.
free_port() {
	perl -MIO::Socket::INET -e '
		my $s = IO::Socket::INET->new(LocalAddr => "127.0.0.1", Listen => 1, Proto => "tcp")
			or die "no free port: $!";
		print $s->sockport, "\n";
		$s->close;
	'
}

PORT="$(free_port)"
export HP_DAEMON="http://127.0.0.1:$PORT"
"$BIN" daemon --addr "127.0.0.1:$PORT" --home "$HP_HOME" >"$WORK/daemon.log" 2>&1 &
DAEMON_PID=$!

for _ in $(seq 1 50); do
	if curl -fsS --max-time 1 "$HP_DAEMON/healthz" >/dev/null 2>&1; then
		break
	fi
	sleep 0.1
done
if ! curl -fsS --max-time 1 "$HP_DAEMON/healthz" >/dev/null 2>&1; then
	echo "bench.sh: daemon did not come up on port $PORT" >&2
	cat "$WORK/daemon.log" >&2
	exit 1
fi
echo "daemon up on port $PORT"
echo

# ---------------------------------------------------------------------------
# Measurement
# ---------------------------------------------------------------------------

RESULTS="$WORK/results.txt"
SCRATCH="$WORK/scratch"
: >"$RESULTS"
mkdir -p "$SCRATCH"

# time_hook runs one timing sweep of the hook itself.
time_hook() {
	local label="$1" repo_name="$2" repo="$3" cmd="$4" unique="$5" stats
	stats="$(perl "$DRIVER" hook "$BIN" "$repo" "$cmd" "$SCRATCH" "$ITERATIONS" "$unique")"
	echo "$repo_name $label $stats" >>"$RESULTS"
	# shellcheck disable=SC2086
	set -- $stats
	printf '  %-13s %-12s median %8.2f ms   p95 %8.2f ms   min %7.2f   max %7.2f\n' \
		"$repo_name" "$label" "$1" "$2" "$3" "$4"
}

# time_shell runs one timing sweep of a command under sh -c, which is how the
# harness runs whatever the hook rewrote it into.
time_shell() {
	local label="$1" repo_name="$2" repo="$3" cmd="$4" stats
	stats="$(perl "$DRIVER" shell "$repo" "$cmd" "$ITERATIONS")"
	echo "$repo_name $label $stats" >>"$RESULTS"
	# shellcheck disable=SC2086
	set -- $stats
	printf '  %-13s %-12s median %8.2f ms   p95 %8.2f ms   min %7.2f   max %7.2f\n' \
		"$repo_name" "$label" "$1" "$2" "$3" "$4"
}

rewrite_of() {
	perl "$DRIVER" rewrite "$BIN" "$1" "$2" "$SCRATCH"
}

# seed_hit runs the whole miss path once and then executes the command the hook
# handed back, which is what a harness would do. That lands a servable record,
# so the next lookup for the same key is a hit.
seed_hit() {
	local repo="$1" cmd="$2" wrapped
	wrapped="$(rewrite_of "$repo" "$cmd")"
	if [ -z "$wrapped" ]; then
		echo "bench.sh: hook did not rewrite '$cmd'; cannot seed a hit" >&2
		exit 1
	fi
	(cd "$repo" && sh -c "$wrapped") >/dev/null 2>&1 || true

	# Confirm it really is a hit now. A served rewrite replays two blobs; a
	# miss rewrites to "hindsight record". Reporting a miss as a hit would be
	# the single most misleading thing this script could do.
	wrapped="$(rewrite_of "$repo" "$cmd")"
	case "$wrapped" in
	cat\ *) printf '%s' "$wrapped" ;;
	*)
		echo "bench.sh: expected a served replay, got: $wrapped" >&2
		exit 1
		;;
	esac
}

# seed_fast runs a cheap command through the full record path until the
# duration memo has enough samples to start skipping it.
seed_fast() {
	local repo="$1" cmd="$2" wrapped i
	for _ in 1 2 3; do
		wrapped="$(rewrite_of "$repo" "$cmd")"
		[ -n "$wrapped" ] || break
		(cd "$repo" && sh -c "$wrapped") >/dev/null 2>&1 || true
	done
	if [ -n "$(rewrite_of "$repo" "$cmd")" ]; then
		echo "bench.sh: '$cmd' did not become known-fast; is HP_MIN_DURATION_MS 0?" >&2
		exit 1
	fi
}

PASSTHROUGH_CMD="curl -s https://example.invalid/health"
# Not `echo`: the alwaysCheap list short-circuits that head with no
# observations at all, so it would measure the wrong mechanism. `wc` has to be
# learned by the memo, which is the path worth timing.
FAST_CMD="wc -l .gitignore"
# Must clear the duration floor or nothing it produces is servable and there is
# no hit path to measure. Sorting the data file takes about 750 ms; piping to
# tail keeps the recorded output one line, well under the 8 MB cap above which
# a record is refused as truncated.
HIT_CMD="sort bench-data.txt | tail -1"
BARE_CMD="grep -cF payload bench-data.txt"

# The harness surcharge, which is nobody's fault and everybody's cost.
#
# Hooks execute under a login shell. Every number below is what happens after
# that shell has started, so the difference between these two rows is added to
# all of them in a real session, on every command, including the ones that pass
# straight through. It is entirely a property of the user's dotfiles.
echo "harness floor"
time_shell "shell-floor" "harness" "$LARGE_REPO" "true"
time_shell "login-shell" "harness" "$LARGE_REPO" "${SHELL:-/bin/sh} -lc true"
echo

for pair in "small:$SMALL_REPO" "large:$LARGE_REPO"; do
	name="${pair%%:*}"
	repo="${pair#*:}"
	echo "$name repo"

	# In production each repository gets its own HP_HOME and therefore its own
	# duration memo. Here one daemon serves both fixtures, so they share one,
	# and what the small sweep taught the memo would make the large sweep skip
	# the very commands it is trying to time. Start each sweep clean.
	rm -f "$HP_HOME/fastpath.json"

	# The kill switch, which is the floor: spawn the binary, read one
	# environment variable, exit.
	export HP_ENABLE=""
	time_hook "disabled" "$name" "$repo" "echo floor" 1
	export HP_ENABLE=1

	time_hook "passthrough" "$name" "$repo" "$PASSTHROUGH_CMD" 0

	seed_fast "$repo" "$FAST_CMD"
	time_hook "known-fast" "$name" "$repo" "$FAST_CMD" 0

	# Not an `echo`: alwaysCheap would short-circuit it and this would measure
	# the bail-out path twice. The hook never executes the command, so its real
	# cost is irrelevant — only that the classifier says SERVE and the memo has
	# never seen this exact string.
	time_hook "miss" "$name" "$repo" "grep -rn hindsight-bench-miss src" 1

	replay="$(seed_hit "$repo" "$HIT_CMD")"
	time_hook "hit" "$name" "$repo" "$HIT_CMD" 0

	# Fail-open. Point at a port nothing is listening on rather than killing
	# the daemon, so the rest of the sweep still has one.
	DEAD_PORT="$(free_port)"
	SAVED_DAEMON="$HP_DAEMON"
	export HP_DAEMON="http://127.0.0.1:$DEAD_PORT"
	time_hook "daemon-down" "$name" "$repo" "grep -rn hindsight-bench-down src" 1
	export HP_DAEMON="$SAVED_DAEMON"

	# The other two halves of the round trip, which the hook does not pay but
	# the agent waits for all the same.
	#
	#   bare     the command on its own, so the wrapper's cost can be isolated
	#   record   what a miss is rewritten into: the command plus a second full
	#            state computation to close the purity gate
	#   replay   what a hit is rewritten into: two cats and an exit
	time_shell "bare" "$name" "$repo" "$BARE_CMD"
	record_cmd="$(rewrite_of "$repo" "$BARE_CMD")"
	case "$record_cmd" in
	*record*) time_shell "record" "$name" "$repo" "$record_cmd" ;;
	*) echo "bench.sh: expected a record wrapper, got: $record_cmd" >&2 ;;
	esac
	time_shell "replay" "$name" "$repo" "$replay"
	echo
done

# ---------------------------------------------------------------------------
# Break-even
# ---------------------------------------------------------------------------

# What a command costs with and without the cache.
#
#   without           T
#   with, on a miss   hook_miss + T + (record - bare)
#   with, on a hit    hook_hit + replay
#
# Over many intercepted commands at hit rate p, the cache is worth having only
# when the average command is slower than
#
#   T > (hook_hit + replay) + ((1 - p) / p) * (hook_miss + record_overhead)
#
# The first term is what a served command costs. The second is the tax on
# everything that missed, amortised over the commands that did hit: at p = 0.5
# each hit carries one miss, and at p = 0.1 each hit carries nine.
echo "break-even"
awk '
	{ med[$1 " " $2] = $3 }
	function threshold(hh, rp, hm, ro, p) { return hh + rp + ((1 - p) / p) * (hm + ro) }
	END {
		printf("  %-7s %8s %8s %8s %8s   %9s %9s %9s\n",
			"repo", "hk_hit", "hk_miss", "replay", "record+",
			"p=1.00", "p=0.53", "p=0.075")
		split("small large", repos, " ")
		for (i = 1; i <= 2; i++) {
			r = repos[i]
			hh = med[r " hit"]; hm = med[r " miss"]
			rp = med[r " replay"]; ro = med[r " record"] - med[r " bare"]
			if (hh == "" || hm == "") continue
			printf("  %-7s %8.1f %8.1f %8.1f %8.1f   %7.0f ms %7.0f ms %7.0f ms\n",
				r, hh, hm, rp, ro,
				threshold(hh, rp, hm, ro, 1.0),
				threshold(hh, rp, hm, ro, 0.533),
				threshold(hh, rp, hm, ro, 0.075))
		}
		print ""
		print "  Commands faster than the threshold cost more to cache than they save."
		print "  p=1.00   floor: every command hits forever. Nothing beats this."
		print "  p=0.53   the hit rate measured on a real five-agent fan-out."
		print "  p=0.075  the hit rate the replayed SWE-bench corpus gives when"
		print "           keyed on (state, command) the way Hindsight really keys."
	}
' "$RESULTS"
echo

# ---------------------------------------------------------------------------
# Does the shipping classifier serve things below break-even?
# ---------------------------------------------------------------------------

# The threshold is only interesting next to what the classifier actually lets
# through. The policy comes from `hindsight key --cmd` rather than from a list
# in this script, so it cannot drift from what really ships.
#
# Two lines matter. The measured floor is what serving genuinely costs on this
# repository. The shipping floor is the constant `hindsight` compares against,
# and whether the two agree is the question.
FLOOR="$(awk '
	$1 == "large" && $2 == "hit" { h = $3 }
	$1 == "large" && $2 == "replay" { r = $3 }
	END { printf("%.1f", h + r) }
' "$RESULTS")"
# Read the shipping default out of the source rather than restating it here,
# because a benchmark that quotes a stale constant is worse than one that does
# not mention it at all.
SHIP_FLOOR="${HP_MIN_DURATION_MS:-}"
if [ -z "$SHIP_FLOOR" ]; then
	SHIP_FLOOR="$(awk '/^const DefaultMinDurationMS/ { print $NF }' \
		"$ROOT/internal/hp/fastpath.go" 2>/dev/null)"
fi
[ -n "$SHIP_FLOOR" ] || SHIP_FLOOR=0

echo "what SERVE-eligible commands actually cost (large repo)"
echo "  measured floor ${FLOOR} ms (hit path + replay)   shipping floor ${SHIP_FLOOR} ms"
printf '  %-32s %-11s %9s   %s\n' "command" "classifier" "median ms" "verdict"
while IFS= read -r probe; do
	[ -n "$probe" ] || continue
	# stdin is the heredoc below; anything spawned in here must not eat it.
	policy="$(cd "$LARGE_REPO" && "$BIN" key --cmd "$probe" 2>/dev/null </dev/null |
		awk '$1 == "policy" { print $2 }')"
	[ -n "$policy" ] || policy="?"
	med="$(perl "$DRIVER" shell "$LARGE_REPO" "$probe" "$ITERATIONS" </dev/null | awk '{ print $1 }')"
	verdict="$(awk -v m="$med" -v f="$FLOOR" -v s="$SHIP_FLOOR" -v p="$policy" 'BEGIN {
		if (p != "SERVE")     { print "not served by the classifier" }
		else if (m < s && m < f) { print "refused by the duration floor; correctly" }
		else if (m < s)       { print "refused by the duration floor; would have paid" }
		else if (m < f)       { print "PAST THE SHIPPING FLOOR, STILL A LOSS" }
		else                  { print "worth serving" }
	}')"
	printf '  %-32s %-11s %9s   %s\n' "$probe" "$policy" "$med" "$verdict"
done <<'PROBES'
echo hello
pwd
basename /usr/local/bin/hindsight
cat .gitignore
ls src
wc -l .gitignore
head -n 5 .gitignore
git status --porcelain
git log --oneline -20
git diff --stat HEAD
find src -name file00001.go
grep -c payload bench-data.txt
find src -type f
grep -rl Fixture00 src
grep -rn Fixture07 src
sort bench-data.txt
PROBES
echo
echo "  The classifier column is the shipping policy. A SERVE command whose own"
echo "  execution is below the measured floor is made slower by the cache on"
echo "  every hit; the duration memo is what has to catch it, and it only"
echo "  catches what falls under the shipping floor."
echo

if [ "$WITH_GO" = "1" ]; then
	echo "go benchmarks (fixtures under $WORK/fixtures, removed on exit)"
	HP_PERF_FIXTURES="$WORK/fixtures" go test "$ROOT/internal/hp/" \
		-run XXX -bench . -benchtime 10x -count 1 -timeout 60m
fi

echo "done"

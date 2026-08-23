# What Hindsight costs

Hindsight is a cache, so the only question that matters is whether the work it
deletes is larger than the work it adds. This document measures the work it
adds. Every number traces to a named benchmark that anyone can rerun; the
commands are in [Reproducing](#reproducing).

## Findings

**The tree hash is not sub-linear.** It looks sub-linear because a fixed ~13 ms
of git process spawn dominates below about two thousand files. Subtract that
and the cost is linear at **2.9 µs per tracked file** from 5,000 files upward.
A 50,000-file repository hashes warm in 159 ms and that is affordable, but it
is affordable because the constant is small, not because the curve bends.

**92% of a small-repo interception is process creation.** On a 100-file
repository the hook spends 32.1 ms of a 34.9 ms interception starting five
processes — itself plus four gits — and 2.7 ms actually hashing. Everything
written in Go (classify, key derivation, JSON, the daemon round trip) totals
0.09 ms, or a quarter of one percent.

**The marginal cost of attempting to cache is 35 ms on a small repository and
114 ms on a 20,000-file one.** The single figure of 42 ms this project has been
quoting is right for a small repository and 3× low for a large one. Because
that marginal cost is what the duration floor is derived from, no single
constant can be correct across repository sizes.

**`hindsight record` replays the entire cache log on every miss.** It calls
`OpenStore`, which scans and parses `log.jsonl` to rebuild an in-memory index,
solely so it can write two blobs — and writing a blob needs only the path
helper the hook already uses. That scan costs 59 ms at 10,000 records and
616 ms at 100,000. It is on the critical path of every uncached command and it
grows without bound, which means **the break-even threshold is not a constant:
it degrades as the cache fills.** This is the most consequential thing in this
report.

**Caching reads is net negative by two orders of magnitude.** Reads are 48.8%
of commands in the measured corpus and show 5.4% cross-agent reuse. Attempting
to cache a typical 7 ms read costs 76 ms on a small repository and 241 ms on a
large one, to save 0.4 ms. That is 202:1 against, and 639:1 on the large
repository. The duration floor exists precisely to prevent this, and this is
the number that justifies it.

**The fail-open path costs as much as a cache hit and returns nothing.** A user
who installs the hook and never starts the daemon pays 39 ms per SERVE-eligible
command on a small repository and 126 ms on a large one, because the hook
computes the whole key before it discovers the daemon is not there.

**500 ms is inside the defensible range but is not what the arithmetic
produces**, and the reasoning recorded next to it has its safety margin
pointing the wrong way. Details in [The break-even threshold](#the-break-even-threshold).

## Machine and method

| | |
|---|---|
| Host | Apple M2 Max (Mac14,5), 8 performance + 4 efficiency cores, 32 GB |
| OS | macOS 26.5 (build 25F5058e), Darwin 25.5.0, arm64 |
| Go | go1.24.2 darwin/arm64 |
| git | 2.46.0 |
| Filesystem | APFS, local SSD |

**This is a laptop, and it was not idle.** Other agents were working in the
same repository throughout. CPU idle was 48.9% when the Go sweep started and
80.3% when it ended; a five-agent Claude Code fleet run finished minutes
before it began. A quieter confirmation sweep taken afterwards came in 10–20%
faster across the whole key path than the authoritative run. Treat every
absolute number here as carrying roughly ±20%, and prefer the ratios and the
shapes of the curves, which are stable. Where it matters, minimum as well as
median is reported: the minimum of many samples is the least contaminated
estimate available on a noisy host.

Two harnesses produced these numbers.

`internal/hp/perf_test.go` measures components in-process against synthetic git
repositories with realistic nesting — about twenty files per leaf directory in
a two-level package tree, which matters because git writes one tree object per
directory. Fixtures are generated once and cached between runs, so the page
cache is warm, which is the realistic condition: an agent's worktree is not
cold. The authoritative run pins the iteration count, because the large
fixtures take longer than the default benchtime and would otherwise be sampled
once.

`scripts/bench.sh` measures what an agent actually waits for: a real PreToolUse
payload piped into a freshly spawned `hindsight hook`, against a real daemon,
in a real git worktree, 50 iterations per path. Its two fixtures are a
100-file repository and a 20,000-file one; the large fixture also carries a
400,000-line data file so there is something genuinely slow to read, which is
why its tree hash is larger than the 20,000-file Go fixture's.

Go benchmark figures below are the **median of 5 runs of 10 iterations**
(`-benchtime 10x -count 5`) unless noted. End-to-end figures are the **median
of 50** with the minimum in parentheses.

## Tree hash versus repository size

This is the number that decides whether Hindsight is usable on a real codebase.
`BenchmarkTreeHashWarm` and `BenchmarkTreeHashCold`.

| Files | Warm (ms) | range | Cold (ms) | range | Cold ÷ warm |
|---|---|---|---|---|---|
| 100 | **15.6** | 14.7–16.8 | **21.4** | 20.9–22.3 | 1.4× |
| 1,000 | **17.9** | 17.6–18.6 | **70.2** | 61.2–72.9 | 3.9× |
| 5,000 | **28.0** | 27.4–28.5 | **313.9** | 307–384 | 11.2× |
| 20,000 | **70.1** | 68.7–72.8 | **2,380** | 2,292–2,596 | 34× |
| 50,000 | **158.7** | 153–176 | **4,599** | 4,500–5,914 | 29× |

Warm means the persistent side index is populated, which is the state the hook
is in for every command after the first. Cold deletes the side index before
each iteration, which is what a throwaway `mktemp` index would cost on *every*
command. The cold column is what justifies the persistent index existing at
all, and it is paid once per worktree.

**The sub-linearity is an artefact of the fixed cost.** Every tree hash starts
two git processes (`git add -A`, then `git write-tree`), and
`BenchmarkSpawnFloor/git_rev_parse` puts a git process that discovers and reads
a repository at 6.5 ms. Subtracting 13.0 ms of unavoidable spawn:

| Files | Warm minus spawn | Per tracked file |
|---|---|---|
| 100 | 2.7 ms | 26.8 µs |
| 1,000 | 4.9 ms | 4.9 µs |
| 5,000 | 15.1 ms | 3.0 µs |
| 20,000 | 57.1 ms | 2.9 µs |
| 50,000 | 145.7 ms | 2.9 µs |

The marginal cost is flat at **2.9 µs per file above 5,000 files**. The curve
is linear there; what changes between 100 files and 5,000 is only how much of
the fixed 13 ms each file has to share. This still supports the practical
conclusion — 159 ms on a 50,000-file monorepo is affordable against a test
suite that takes minutes — but the reason is a small constant, not a bending
curve, and a 500,000-file repository should be expected to cost about 1.5 s
rather than something sub-linear.

Against the figures this project had been quoting: warm was reported as
17/20/33/79/175 ms and measures 15.6/17.9/28.0/70.1/158.7, consistently 8–15%
lower and directionally confirmed. Cold at 100 files was reported as 21 ms and
measures 21.4 ms exactly. **Cold at 50,000 files was reported as 7.2 s and
measures 4.6 s** — the old figure is 57% too high and should be retired.

## Scaling with changed files

Repository size fixed at 20,000 files; the count of files rewritten with
content git has never seen is varied. `BenchmarkTreeHashDirty`.

| Files changed | Tree hash (ms) | Marginal per changed file |
|---|---|---|
| 0 | **71.3** | — |
| 1 | **84.7** | — |
| 10 | **81.3** | — |
| 100 | **135.8** | 0.61 ms |
| 1,000 | **618.6** | 0.54 ms |
| 10,000 | **5,887** | 0.59 ms |

`git add -A` has to stat every tracked path whatever happened, which is the
71 ms floor and matches `BenchmarkTreeHashWarm/20k_files` as it should. On top
of that each changed file costs a consistent **0.57 ms**. That is far too slow
to be hashing 700 bytes; it is the loose object write — compress, create,
write, rename, one filesystem round trip per changed file.

This is what makes the warm case fast, and it is worth being precise about why:
not because re-hashing is cheap, but because agents change few files between
commands. Ten changed files is free. A hundred costs 65 ms over the floor. A
thousand costs 547 ms, and at that point the tree hash alone exceeds the
duration floor the cache uses to decide what is worth caching at all.

This benchmark rebuilds its fixture rather than reusing the cached one, because
it is the only benchmark here that dirties what it measures. That turned out
not to matter — five runs against fresh fixtures agreed with a run against a
fixture carrying half a million leftover loose objects to within 5% — but a
number that depends on how many times the suite has been run is not
reproducible even when it happens to be stable.

## The full per-command overhead

The hook pays this once per intercepted command. **On a miss, `hindsight record`
pays the state computation and the process spawn a second time**, because the
purity gate has to recompute the tree hash and environment fingerprint after
execution to decide whether the result was servable at all.

### Components

| Component | Cost | Benchmark |
|---|---|---|
| `hindsight` process startup | 6.64 ms | `ProcessStartup/hindsight_disabled` |
| Duration memo load, empty | 0.015 ms | `FastpathLoad/0_entries` |
| `Classify` on a realistic mix | 0.0013 ms | `ClassifyMix` |
| `NormalizeCommand` | 0.0001 ms | `NormalizeCommand` |
| `NewWorkspace`, 2 × `git rev-parse` | 12.45 ms | `NewWorkspace` |
| Tree hash warm, 100 files | 15.64 ms | `TreeHashWarm/100_files` |
| Environment fingerprint, no deps | 0.013 ms | `EnvFingerprint/none` |
| `Key` (sha256) | 0.0004 ms | `KeyOnly` |
| Daemon lookup, miss | 0.056 ms | `DaemonLookupMiss` |
| Payload parse + response encode | 0.0024 ms | `HookEnvelope` |
| **Total** | **34.86 ms** | |

Measured end-to-end for the same case: **42.01 ms median, 34.33 ms minimum**.
The decomposition accounts for essentially all of the best case; the 7.7 ms
between minimum and median is host noise.

The proportions are the finding:

| | Small repo (100 files) | Share |
|---|---|---|
| Process creation (1 hook + 4 git) | 32.05 ms | **92%** |
| Actual hashing and filesystem work | 2.68 ms | 7.7% |
| All Go logic (classify, key, JSON, HTTP) | 0.088 ms | 0.25% |

### End-to-end, both repositories

`scripts/bench.sh`, median of 50 with minimum in parentheses.

| Path | 100 files | 20,000 files |
|---|---|---|
| Harness floor, `sh -c true` | 4.65 (3.67) | — |
| Harness floor, `$SHELL -lc true` | 29.91 (27.25) | — |
| Hook disabled (kill switch) | 6.37 (5.70) | 6.52 (6.03) |
| Passthrough (classifier refuses) | 6.24 (5.69) | 6.32 (5.88) |
| Known-fast (memo bails before hashing) | 6.78 (5.80) | 6.58 (6.09) |
| **Miss** (full key path + daemon + rewrite) | **42.01** (34.33) | **120.82** (100.84) |
| **Hit** (full key path + replay rewrite) | **37.69** (33.13) | **108.66** (98.68) |
| Daemon down (full key path, then fail open) | 39.18 (33.49) | 125.58 (98.18) |
| Bare command, unwrapped | 72.23 (69.89) | 70.45 (68.23) |
| `hindsight record` wrapper | 115.62 (109.00) | 204.64 (171.93) |
| Replay of a served result | 8.85 (8.18) | 10.13 (7.90) |

The `record` row minus the `bare` row is the second payment: **43.4 ms on the
small repository and 134.2 ms on the large one**, added to every miss.

One number here belongs to nobody and is paid by everybody. Hooks execute under
a login shell, and `$SHELL -lc true` costs 29.9 ms against 4.7 ms for `sh -c
true`. That **25.3 ms is added to every command in a real session**, including
commands that pass straight through, and it is entirely a property of the
user's dotfiles. It cancels out of the marginal analysis below, because it is
paid on both branches, but it dwarfs the hook's own 6.6 ms startup and it is
the first thing to look at if per-command latency is the complaint.

## The break-even threshold

The decision the duration floor makes is not "is Hindsight worth installing".
It is narrower: *given that the hook has already started and classified this
command, is it worth doing the state computation, or should we get out of the
way?* So the right comparison is full interception against the fastpath, not
against no hook at all.

Let

- `M` = marginal cost of attempting = hook on the full path − hook on the fastpath
- `R` = record overhead = `hindsight record` − the bare command
- `P` = replay cost on a hit
- `T` = the command's own execution time
- `p` = hit rate

| | 100 files | 20,000 files |
|---|---|---|
| `M` | **35.2 ms** | **114.2 ms** |
| `R` | 43.4 ms | 134.2 ms |
| `P` | 8.9 ms | 10.1 ms |

Attempting beats passing through when `p·T > M + p·P + (1−p)·R`, so

```
T*(p) = [ M + p·P + (1 − p)·R ] / p
```

| Hit rate | Where it comes from | T\* small | T\* large |
|---|---|---|---|
| 100% | unattainable floor | 44 ms | 124 ms |
| 53.3% | measured five-agent homogeneous fan-out | **113 ms** | **342 ms** |
| 16.9% | corpus reuse across the opening three commands | 431 ms | 1,346 ms |
| 7.5% | corpus avoidable, keyed on (state, command) | 1,014 ms | 3,188 ms |
| 5.4% | corpus cross-agent reuse for reads | 1,421 ms | 4,477 ms |
| 3.6% | corpus cross-agent reuse overall | 2,149 ms | 6,777 ms |

`scripts/bench.sh` computes the same threshold from a different starting point
— it compares against running with no hook at all rather than against the
fastpath — and lands within 8% at every hit rate it reports (342 ms against my
342 ms at p = 0.53; 3,264 ms against my 3,188 ms at p = 0.075). Two framings
agreeing is the best evidence available that neither is arithmetic error.

### Does the arithmetic support `DefaultMinDurationMS = 500`?

Partly. The *existence* of a floor is strongly supported. The *value* is not
derivable from these numbers, and the justification recorded beside it in
`internal/hp/fastpath.go` has three problems.

**The stated derivation omits its largest term.** The comment computes
`hit_rate × duration > 42 ms` and concludes that 5.4% reuse needs a command of
roughly 800 ms. That drops `R`, the record overhead, which is paid on `1 − p`
of attempts and is therefore the *dominant* term when `p` is small. Including
it, the 5.4% threshold is **1,421 ms on a small repository and 4,477 ms on a
large one**, not 800 ms. The comment understates its own threshold by between
1.8× and 5.6×.

**The safety margin points the wrong way.** The comment says 500 ms is
"deliberately conservative against the 800 ms the arithmetic suggests."
Lowering a floor admits *more* commands, which is the permissive direction, not
the conservative one. Everything between 500 ms and 1,421 ms is admitted and,
at the 5.4% hit rate the same paragraph cites, loses money. The accompanying
argument — that hit rates are much higher during the opening lockstep of a
fan-out — is legitimate and probably correct, but it is an argument for
optimism, and it should be labelled as one.

**One constant cannot be right across repository sizes.** Solving `T*(p) = 500`
for the hit rate at which 500 ms exactly breaks even:

| Repository | 500 ms breaks even at |
|---|---|
| 100 files | **14.7%** hit rate |
| 20,000 files | **39.8%** hit rate |

So 500 ms is the correct floor if you expect roughly 15% hits on a small
repository or roughly 40% on a large one. Neither is the 5.4% the comment cites
nor the 53.3% the fan-out measured. It is a defensible middle, and it is a
guess.

**And it decays.** Because `hindsight record` rebuilds the store index on every
miss (`StoreOpen`, below), `R` grows with the corpus:

| Corpus size | `R` small | T\* at p = 0.533 |
|---|---|---|
| empty | 43.4 ms | 113 ms |
| 10,000 records | 102.5 ms | 165 ms |
| 100,000 records | 659.3 ms | **653 ms** |

At a 100,000-record cache the break-even is 653 ms even at the optimistic 53%
hit rate, so the 500 ms floor becomes definitively too low **purely because the
cache filled up**. Every threshold in this section was measured against an
essentially empty cache and is therefore a lower bound.

## Where the cache is net negative: reads

Reads are **48.8% of all commands** in the measured corpus and show **5.4%
cross-agent reuse**. From the `bench.sh` probe table, typical read durations on
the large fixture: `cat .gitignore` 7.6 ms, `ls src` 7.2 ms, `wc -l` 6.7 ms,
`head -n 5` 6.2 ms, `git log --oneline -20` 10.8 ms. Call it 7 ms.

Net cost of attempting to cache one such read, relative to passing it through
(`M + p·P + (1−p)·R − p·T`):

| | Cost of attempting | Saving | Ratio |
|---|---|---|---|
| 100 files | **+76.4 ms** | 0.38 ms | **202:1 against** |
| 20,000 files | **+241.4 ms** | 0.38 ms | **639:1 against** |

Per hundred commands an agent runs, 48.8 are reads. Attempting to cache all of
them costs **3.7 seconds** of pure overhead on a small repository and **11.8
seconds** on a large one, and deletes 18 milliseconds of work. There is no
configuration of the cache that fixes this, because the overhead is not the
problem — the hit rate is. A cache that cost nothing to consult would still
only save 18 ms.

This is the strongest argument in the codebase for the duration floor, and it
should be quoted as such. Two caveats on the mechanism that implements it:

- **The memo has to learn, and learning is not free.** `fastpathSamples = 2`,
  so each distinct normalized command string costs two full interceptions
  before it is skipped: **157 ms on a small repository, 497 ms on a large one**,
  per distinct string. The memo keys on the exact command string, so
  `cat a.py` and `cat b.py` are separate learners. In a session with many
  distinct read strings the memo may never amortize. The `alwaysCheap` list
  (`echo`, `pwd`, `true`, `false`, `:`, `basename`, `dirname`, `printf`) is
  what avoids this for the most common cases, and the classifier's passthrough
  of `ls -l`, `stat`, `du` and `df` removes more.
- **The memo itself is re-read every command.** The hook is a fresh process, so
  there is no warm map to inherit. `BenchmarkFastpathLoad`: 0.015 ms empty,
  0.074 ms at 100 entries, 0.59 ms at 1,000, **5.83 ms at 10,000**. At ten
  thousand entries consulting the memo nearly doubles the cost of the fastpath
  it is meant to make cheap — still a good trade against 35 ms, but another
  term that grows with session length. The lookup itself is free once loaded
  (`FastpathKnownFast`: 12–24 ns).

## Where the overhead is irreducible

`BenchmarkProcessStartup` separates the floor into layers. The last row is the
real binary on its kill-switch path; the row above is a synthetic Go program
that links the same dependency set and does nothing. They agree to within 1 ms,
which is what makes the decomposition credible.

| | Cost | Increment | Reducible? |
|---|---|---|---|
| `os_true` — fork, exec, exit of a C binary | 2.02 ms | — | **No.** The kernel's price for any out-of-process hook. |
| `go_minimal` — empty Go program | 3.05 ms | +1.03 ms | **No**, given a Go binary. Runtime init. |
| `go_hooklike` — links `net/http`, `encoding/json`, `os/exec`, `crypto/sha256`, `flag` | 5.63 ms | +2.58 ms | **Yes.** Image size and package init. |
| `hindsight_disabled` — the real binary | 6.64 ms | +1.01 ms | Partly. |

So of the 6.64 ms floor, **3.05 ms is genuinely irreducible** and 3.59 ms — 54%
of it — is the dependency set. Replacing `net/http` with a raw unix-socket
client is the single largest available win here, and it is worth roughly
2.5 ms per command on every command that is not passed through.

The larger reducible item is not the hook binary at all, it is git:

| | Cost | Reducible? |
|---|---|---|
| `NewWorkspace` — 2 × `git rev-parse` | 12.45 ms | **Yes, entirely.** |
| `git add -A` + `git write-tree` spawn | 12.96 ms | **No**, not with git as the hasher. |
| Actual hashing work, 100 files | 2.68 ms | No. |

`NewWorkspace` resolves the worktree root and the git dir — two facts that
cannot change during a session — with two separate git invocations. Both are
available from one call (`git rev-parse --show-toplevel --absolute-git-dir`),
which halves it to 6.5 ms, and both could be memoized in `$HP_HOME` keyed by
cwd, which removes it entirely.

Adding up what is genuinely available: 12.45 ms from caching workspace
resolution, 2.58 ms from a leaner binary. A small-repo interception would fall
from 34.9 ms to **19.9 ms, a 43% reduction**, moving the p = 0.533 threshold
from 113 ms to about 78 ms. Real, and not transformative: two git spawns and
one process launch remain, and they are 15 ms.

**The daemon is not a bottleneck and should not be optimized.** Serial round
trip is 0.056 ms on a miss and 0.181 ms on a hit — the hit is more expensive
because the daemon appends a record to the log and broadcasts it before
replying. Under concurrency (`DaemonLookupConcurrent`; note the labels are
multipliers of `GOMAXPROCS`, so on this twelve-core host they are 12, 60 and
240 in-flight lookups) per-operation cost is 0.016 ms, 0.009 ms and 0.010 ms
respectively — roughly 100,000 lookups per second with no degradation. The
single mutex and single log file are not straining at fleet scale.

**The classifier is free.** `ClassifyMix` is **1.35 µs** on a 35-command mix
spanning every branch. This project had been quoting 14 µs, which is **10×
too high**; that figure should be retired. `ClassifyLength` is flat at 33–36
ns/byte from 1 KB to 256 KB, so there is nothing quadratic waiting for a pasted
file list, and the worst 10 KB shape (`10KB_chain`, which stresses the chain
rule) is 456 µs — still a hundredth of an interception.

**The environment fingerprint is cheap even when realistic.**
`BenchmarkEnvFingerprint`: nothing detected 0.013 ms, Python with 300 installed
distributions 0.41 ms, Node with 500 top-level entries 0.78 ms, both 1.30 ms.
The realistic Node case — 1,250 hoisted top-level entries, 25 scopes, and two
1 MB lockfiles which the fingerprint hashes whole — is **2.65 ms**, confirming
the 3.3 ms this project had claimed. That is paid twice per intercepted
command, so about 5 ms on a real Node project, and it is the price of the one
guarantee that keeps the cache from being wrong.

### The record path scans the whole log

`BenchmarkStoreOpen` rebuilds the in-memory index from `log.jsonl`:

| Records | Log size | Time |
|---|---|---|
| 0 | — | 0.16 ms |
| 1,000 | 0.4 MB | 6.03 ms |
| 10,000 | 4 MB | **59.25 ms** |
| 100,000 | 40 MB | **616.10 ms** |

About 6.1 µs per record, linear. The daemon paying this once at startup is
exactly what the append-only design intends. But `cmd/hindsight/record.go`
calls `hp.OpenStore(hp.Home(ws.Root))` on **every miss, in a fresh process**,
and the only thing it does with the result is call `PutBlob` twice.
`BenchmarkStorePutBlob` puts a 64 KB blob in 0.21 ms. The index is built and
thrown away.

`hp.StorePaths()` already exists for exactly this reason and the hook already
uses it to resolve blob paths without replaying the log. The record path does
not, and the cost is unbounded in the size of the corpus the cache is
accumulating.

## The fail-open path

Every failure path in the hook is a passthrough, which is the right design.
The question is what it costs.

| | 100 files | 20,000 files |
|---|---|---|
| Miss, daemon up | 42.01 ms | 120.82 ms |
| **Daemon down** | **39.18 ms** | **125.58 ms** |

They are the same. The hook resolves the worktree, hashes the tree,
fingerprints the environment and derives the key *before* it tries to contact
the daemon, so a refused connection arrives after all the work is already done
and every millisecond of it is discarded.

This matters more than the numbers suggest, because it is the **default state
after `hindsight init`**. The hook is installed and armed; the daemon is a
separate `doctor --ensure-daemon` step. Between those two moments the user pays
39 ms per SERVE-eligible command on a small repository and 126 ms on a large
one, in exchange for nothing whatsoever, and nothing tells them.

A single loopback connect is microseconds — the whole successful lookup is
0.056 ms. Probing the daemon before doing any hashing, or caching a
"daemon down" flag in `$HP_HOME` with a short TTL, would reduce this to about
the known-fast path, 7 ms. That is a **32 ms saving per command on a small
repository and 119 ms on a large one**, for every user who has not started a
daemon yet.

## Reproducing

```bash
# Component benchmarks. The iteration count is pinned because the large
# fixtures take longer than the default benchtime and would be sampled once.
go test ./internal/hp/ -run XXX -bench . -benchtime 10x -count 5 -timeout 90m

# Microbenchmarks, which need a real benchtime to be meaningful.
go test ./internal/hp/ -run XXX -count 5 \
  -bench 'Classify|KeyOnly|Normalize|HookEnvelope|Fastpath|Daemon|Store'

# End-to-end hook latency, break-even table, and what the classifier
# actually admits.
bash scripts/bench.sh --iterations 50
```

Fixtures are cached in `$TMPDIR/hindsight-perf-v1` and a plain run leaves about
a hundred thousand files behind on purpose, so the page cache is warm on the
next run. Set `HP_PERF_FIXTURES` to relocate them; `scripts/bench.sh --with-go`
points it at its own temp directory and cleans up.

The definition-of-done command, `go test ./internal/hp/ -bench . -run XXX`,
runs everything at the default benchtime. It is correct but slow, and its large
fixtures are sampled too few times to trust individually.

## Recommendations

Ordered by how much they change the conclusions above.

1. **Stop replaying the log in the record path.** Move `PutBlob` onto `Paths`
   and have `cmd/hindsight/record.go` use `hp.StorePaths(home)` instead of
   `hp.OpenStore(home)`. This removes 59 ms per miss at a 10,000-record corpus
   and 616 ms at 100,000, and it is the only cost in the system that grows
   without bound. **Until it is fixed the break-even threshold is not a
   constant, and therefore no choice of `DefaultMinDurationMS` can be correct
   for long.** Two files, both Tom's.

2. **Derive the duration floor instead of hardcoding it.** `hindsight doctor`
   already measures the warm tree hash. The marginal cost `M` is dominated by
   it, and it varies 3.2× between a 100-file and a 20,000-file repository, so a
   floor computed at `doctor` time from the measured tree hash — and from the
   observed hit rate once the daemon has one — would be right on both. If the
   constant stays, correct the comment: 500 ms is optimistic relative to the
   arithmetic, not conservative, and the arithmetic it cites omits the record
   overhead.

3. **Check the daemon before hashing.** Saves 32 ms per command on a small
   repository and 119 ms on a large one for every user who has installed the
   hook but not started a daemon, which is everyone for some interval and some
   people forever.

4. **Resolve the workspace once.** `NewWorkspace` costs 12.45 ms — 36% of a
   small-repo interception — to establish two facts that cannot change during
   a session. One `git rev-parse` call returns both, and memoizing the answer
   in `$HP_HOME` removes it entirely.

5. **Do not optimize the daemon, the classifier, or the key derivation.**
   Together they are 0.09 ms of a 34.9 ms interception. The daemon sustains
   100,000 lookups/sec at 240-way concurrency. Any effort spent here is spent
   on a quarter of one percent.

6. **Publish the read result.** That caching reads is 202:1 against on a small
   repository and 639:1 on a large one is the most useful number in this
   document. It is the entire justification for the duration floor, it is
   unflattering, and a reader who finds it in the documentation will trust the
   rest of the numbers more than one who works it out themselves.

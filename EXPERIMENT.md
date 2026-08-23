# Hindsight on a real repository with a slow test suite

One experiment, two arms, five Claude Code agents each, run on 2026-08-23 on an
M-series Mac (12 cores, 34 GB). The question was whether the 4% figure from the
sealed corpus or the 53.3% figure from the toy fan-out is closer to the truth on
a real codebase, and whether deleted execution-seconds ever become elapsed time.

**Short answer.** Execution-seconds fell 77.5%, from 1083.5s to 244.0s, on a
suite that takes 70–105 seconds a run. Wall clock did not move: 243.0s baseline
against 260.3s cached. It did not move for a reason that is structural rather
than incidental, and that reason is the most useful thing this run produced.

Two findings outrank the headline numbers:

1. **Tier-1 diff-disjoint scoping serves wrong answers on this repository.** It
   promotes `pytest -q test/orm` across an edit to `lib/sqlalchemy/sql/util.py`
   because the changed path is disjoint from the literal path argument. It is
   not disjoint — `test/orm` imports the library. Demonstrated below with the
   exact command the agents ran: the cache offers "25 failed, 10893 passed" to a
   worktree whose true result is "10918 passed". This is the shipping lookup
   path on the build both arms ran. A guard landed in `scope.go` while the
   experiment was in progress and closes it, at the cost of making Tier-1 refuse
   every test-suite command permanently.
2. **The environment fingerprint is worktree-dependent for the two most common
   ways of creating a Python virtualenv.** `uv venv` and `python -m venv .venv`
   both write the worktree path or directory name into `pyvenv.cfg`, which the
   fingerprint hashes verbatim. Five worktrees therefore get five different
   fingerprints and the hit rate is structurally zero. I had to work around this
   in the task prompt to measure anything at all.

Everything below states its denominator.

---

## 1. Target repository

| | |
|---|---|
| Repository | [`sqlalchemy/sqlalchemy`](https://github.com/sqlalchemy/sqlalchemy) |
| Upstream commit | `f35da7e2b` "Merge 'Accept any whitespace in SQLite constraint reflection patterns' into main" |
| Experiment ref | `311a357a4` (upstream + one bug commit + one hook-install commit) |
| Files in the tree | **931** (929 upstream tracked files, plus `.codex/hooks.json` and `.claude/settings.json`) |
| Clone location | `/tmp/sa-target` |
| Install | `uv venv -p 3.12 .venv && DISABLE_SQLALCHEMY_CEXT=1 uv pip install -e . pytest pytest-xdist typing_extensions greenlet` — 11–16s per worktree |
| Suite | `.venv/bin/pytest -q test/orm` — 10,918 tests, 142 skipped |

It was chosen over `fastapi` (36s suite, and a pre-existing unrelated failure),
`pydantic` (now a monorepo containing the `pydantic-core` Rust source, so the
install is a cargo build) and `flask`/`requests` (suites under 30s). SQLAlchemy
also leaves a **byte-clean worktree** after both install and test, which is a
precondition for the purity gate to pass at all; I checked this before
committing to it.

At 931 files it is slightly under the 1,000-file floor in the brief. I accepted
that in exchange for the suite time, which the brief calls the single most
important property.

### Suite timing, standalone

| Condition | pytest-reported | wall |
|---|---|---|
| Idle machine, warm `__pycache__`, no bug | 68.8s | ~74s |
| Idle machine, warm `__pycache__`, with the bug | 70.9–71.9s | ~76s |
| First run in a fresh worktree (cold bytecode) | ~90s | ~97s |
| Five agents running it concurrently (baseline arm) | ~96s | 103.7–105.1s |

The cold-bytecode penalty is real and worth noting: every worktree pays ~20
extra seconds on its first run compiling SQLAlchemy to `.pyc`. Those files are
gitignored, so the tree hash does not see them and the purity gate is unaffected.

### `hindsight doctor` on this repo

Run with `HP_HOME=/tmp/hs-doctor`, no daemon up yet.

```
hindsight doctor  /private/tmp/sa-target

OK    git worktree        /private/tmp/sa-target
                          side index   /private/tmp/sa-target/.git/hp-index
OK    tree hash           bd8a69429a20  22ms warm (1340ms cold), 931 files
OK    env fingerprint     7c48ba031de5883406d54ffe26c1a6de  complete, 0ms
                          detected     python
                          registered   python, go, jvm, node, ruby, rust
OK    ecosystem coverage  pyproject.toml -> python
OK    cache home          /tmp/hs-doctor  (HP_HOME)
OK    codex hook          .codex/hooks.json  installed   timeout 10s   matcher Bash
OK    claude hook         .claude/settings.json  installed   timeout 10s   matcher Bash
OK    kill switch         HP_ENABLE unset: the hook is inert
OK    classifier          SERVE for reads, PASSTHROUGH for non-hermetic commands
WARN  daemon              http://127.0.0.1:7777  unreachable
OK    cache               empty; nothing recorded yet
OK    gitignore           ignores .venv

1 warning. Cacheable, with the caveats above.
```

**Tree-hash cost, measured here for the first time at this size:** 1340 ms to
build the side index from empty, 22 ms warm, and 17–18 ms on a second `doctor`
invocation once the index file exists. So on a 931-file repository the persistent
side index buys a 60–75× speedup on every command after the first, and each
fresh worktree pays the 1.3 s cold build exactly once.

The environment fingerprint costs 0 ms (below the reported resolution) and
reports `complete`, so nothing was blocked from serving on that account.

---

## 2. The introduced bug

**I introduced it.** It is not a real SQLAlchemy defect. Commit `1c2b3d200`,
one line, in `lib/sqlalchemy/sql/util.py` at line 1477, inside `_make_slice`,
which converts Python slice semantics into SQL `LIMIT`/`OFFSET`:

```python
-        limit_clause = _offset_or_limit_clause(stop - start)
+        limit_clause = _offset_or_limit_clause(stop - start + 1)
```

Every `Query[start:stop]`, `Query.slice()` and `Select.slice()` therefore emits a
`LIMIT` one larger than requested and fetches one extra row.

Blast radius on `pytest -q test/orm`: **25 failed, 10893 passed, 142 skipped**.
Failures are concentrated in `test_query.py::SliceTest`, `test_dynamic.py`,
`test_generative.py` and the polymorphic inheritance tests, so the traceback
points at the right area without naming the line.

All ten agents across both arms found it and produced the byte-identical correct
fix. Nobody refactored, nobody touched `test/`, and every agent exited 0.

**Fixture flaw, mine:** I committed the bug with the message "Introduce
off-by-one in `_make_slice` (experiment fixture)", which is visible to
`git log`. One agent per arm found the answer that way rather than by debugging
(`a3` in baseline ran `git show 1c2b3d200`; `a2` in cached ran
`git log -p --follow -1 1c2b3d200 -- lib/sqlalchemy/sql/util.py`). This shortened
those two agents' search but does not affect the cache measurement, because the
expensive commands — install and the two suite runs — happened either way. A
future fixture should squash the bug into the base commit.

---

## 3. Execution-seconds — the primary metric

Denominator: all hook-visible commands in one five-agent fan-out per arm.

| | baseline | cached |
|---|---|---|
| hook-visible commands | 20 | 21 |
| executed (MISS + PASSTHROUGH) | 20 | 12 |
| served (HIT + LEASE_WAIT) | 0 | 9 |
| — of which coalesced in flight | 0 | 8 |
| **execution-seconds spent** | **1083.5s** | **244.0s** |
| execution-seconds deleted | 0.0s | 667.2s |
| cached arm's own counterfactual | — | 911.2s |
| hit rate | 0.0% | **42.9%** |

**Execution-seconds fell by 839.5s, or 77.5%, against the measured baseline.**

The cached arm's internal counterfactual (911.2s) is 15.9% below the measured
baseline (1083.5s), and the gap has the sign the design doc predicts: the
baseline ran five pytest processes concurrently and each was slowed by the
others (103.7–105.1s), while the cached arm ran one at a time and recorded an
uncontended duration (96.9s). Pricing avoided work at uncontended speed
understates what was avoided, so 667.2s is a lower bound on deleted CPU.

The 21st command in the cached arm is one extra `git log` that `a2` chose to run;
it is not an artefact of caching.

---

## 4. Wall clock — the open question

| | baseline | cached | change |
|---|---|---|---|
| fleet wall clock | 243.0s | 260.3s | **+7.1%** |
| slowest agent | 242.9s | 260.2s | +7.1% |
| mean agent | 242.7s | 239.4s | −1.4% |
| total agent-seconds | 1213.5s | 1196.8s | −1.4% |

**Wall clock did not improve.** It got slightly worse on the metric that matters
for a fan-out (the slowest agent, since you wait for all of them), and moved by
essentially nothing on the mean.

This is *not* the earlier explanation. Model latency did not dominate here — it
was 10.7% of elapsed time in the baseline arm, against 89.3% spent executing.
This is exactly the regime the design doc said would be needed, and the wall
clock still did not move.

### Per-agent time accounting

`busy` is execution plus time blocked on a peer's lease. `model` is the
remainder of elapsed time: the agent thinking, plus harness startup.

**Baseline**

| agent | wall | executing | blocked | model | busy % |
|---|---|---|---|---|---|
| a1 | 242.6s | 216.8s | 0.0s | 25.8s | 89.4% |
| a2 | 243.0s | 217.1s | 0.0s | 25.9s | 89.4% |
| a3 | 242.2s | 215.3s | 0.0s | 26.9s | 88.9% |
| a4 | 242.9s | 217.6s | 0.0s | 25.3s | 89.6% |
| a5 | 242.9s | 216.7s | 0.0s | 26.1s | 89.2% |
| **total** | **1213.5s** | **1083.5s** | **0.0s** | **130.0s** | **89.3%** |

**Cached**

| agent | wall | executing | blocked | model | busy % |
|---|---|---|---|---|---|
| a1 | 241.5s | 16.7s | 166.7s | 58.0s | 76.0% |
| a2 | 260.3s | 16.1s | 166.7s | 77.4s | 70.3% |
| a3 | 238.8s | 16.8s | 166.7s | 55.3s | 76.9% |
| a4 | 214.9s | 183.3s | 0.0s | 31.5s | 85.3% |
| a5 | 241.4s | 11.0s | 166.7s | 63.7s | 73.6% |
| **total** | **1196.8s** | **244.0s** | **666.9s** | **285.9s** | **76.1%** |

### Why the wall clock did not move

**Eight of the nine served results were in-flight lease coalescing, and a lease
wait costs the waiter exactly as long as the execution it avoids.** The waiter
blocks from its lookup until the holder publishes. It saves the CPU; it saves no
elapsed time. The deleted execution-seconds did not vanish — they reappeared as
666.9 seconds of blocked waiting, and the total agent-seconds are unchanged
(1213.5 → 1196.8).

The decision trace makes this plain. `t` is the completion time of each record,
relative to the first install completing.

```
t+0.0..2.1   a2,a4,a1,a3,a5  MISS  RECORD_ONLY  11-16s each   uv venv && uv pip install ...
t+99.7       a4              MISS  SERVE        96,921 ms     .venv/bin/pytest -q test/orm 2>&1 | tail -100
t+99.7       a5,a1,a2,a3     LEASE_WAIT  from a4  96,921 ms   (same command; blocked, did not run)
t+105.2      a4              MISS  SERVE           371 ms     grep -rn "_make_slice" lib/
t+116.3      a3              MISS  SERVE           332 ms     grep -rn "_make_slice" lib/ | grep -v test
t+117.2      a1              MISS  RECORD_ONLY     331 ms     grep -rn "_make_slice" lib/ 2>/dev/null
t+117.3      a2              HIT   SERVE   from a4 371 ms     grep -rn "_make_slice" lib/
t+118.3      a5              MISS  SERVE           339 ms     grep -rn "_make_slice" lib/ | grep -v "\.pyc"
t+133.7      a2              MISS  SERVE            14 ms     git log -p --follow -1 1c2b3d200 -- ...
t+184.6      a4              MISS  SERVE        69,795 ms     .venv/bin/pytest -q test/orm 2>&1 | tail -20
t+184.6      a2,a3,a1,a5     LEASE_WAIT  from a4  69,795 ms   (same command; blocked, did not run)
```

`a4` took the lease on both suite runs and paid for the whole fleet: 183.3s of
execution and the shortest elapsed time of any agent (214.9s). The other four
paid 166.7s of blocking each and finished no earlier than the baseline agents.

The only wall-clock benefit available from coalescing is second-order — removing
contention makes the one execution faster than five concurrent ones. That is
real but small here: 104.6s → 96.9s on round one and 96.6s → 69.8s on round two,
about 35s per agent. It was absorbed by run-to-run variance in model latency,
which was 156s higher in aggregate in the cached arm. `a2` alone accounts for a
third of that by choosing to go read git history.

A plain index HIT is different in kind: it returns in milliseconds and *does*
convert to elapsed time. There was exactly one in this run, and it was a 371 ms
grep. **Plain hits require temporal separation between agents, which is exactly
what a cold simultaneous fan-out does not have.** Every agent reaches the same
expensive command within seconds of every other, so nobody has finished it yet
and the lease is the only mechanism available.

One caveat on the accounting: a `LEASE_WAIT` record is credited the *holder's
full execution duration*, not the time the waiter was actually blocked. A waiter
that joined partway through is over-credited. That does not affect the deleted
execution-seconds figure — the command genuinely did not run — but it means the
`blocked` column above is an upper bound and the `model` column a lower bound.
The true model latency of the cached waiters was higher than shown, which makes
the wall-clock result slightly worse, not better.

---

## 5. Hit rate and its breakdown

Denominator: 21 hook-visible commands in the cached arm.

| | count | share |
|---|---|---|
| served | 9 | **42.9%** |
| — plain HIT (index) | 1 | 4.8% |
| — LEASE_WAIT (in-flight coalescing) | 8 | 38.1% |
| executed | 12 | 57.1% |

Deleted seconds by mechanism: 666.9s of the 667.2s came from lease coalescing.
The single index hit was worth 0.371s.

That distribution is the whole story of this run. **The index contributed 0.06%
of the deleted seconds; single-flight contributed 99.94%.** On a cold
homogeneous fan-out the index has nothing to offer, because no peer has finished
anything yet.

---

## 6. Tier-0 versus Tier-1

**Tier-1 fired zero times.** All nine served results were Tier 0, exact tree
matches. `grep '"tier":1'` over both arms' logs returns nothing, and both
daemons report `"tier1": 0`.

The reason is visible in the trace: Tier-1 requires a candidate record with the
same normalized command, same working directory and same environment
fingerprint, recorded at a *different* tree. That combination never occurred.
Every agent that issued a given command string issued it at a tree identical to
its peers' — before the fix all five were at `bd8a6942`, and after the fix all
five were at `20c1b6a7`, because all five made the same one-line edit. Where
command strings differed (the greps), there was no candidate to promote.

So on this workload Tier-0 already captured everything Tier-1 could have, and
Tier-1's premise — that agents diverge and stop sharing trees — did not hold.

### Tier-1 serves a wrong answer on this repository

Zero firings is not the interesting result. I tested whether it *would* fire,
using the same repo, the same two trees and the same environment, against a copy
of the baseline store so the experiment logs stayed untouched.

The two trees differ in exactly one path:

```
$ git diff-tree -r --name-only --no-commit-id bd8a6942 20c1b6a7
lib/sqlalchemy/sql/util.py
```

Recording `.venv/bin/pytest -q test/orm 2>&1 | tail -100` at the pre-fix tree and
then looking it up from a worktree at the post-fix tree:

```
HIT tier=1  from=a2  saved=105138ms
  all 1 changed paths are disjoint from the literal path arguments [test/orm]
```

The blob it offers to serve ends:

```
=========== 25 failed, 10893 passed, 142 skipped in 96.03s (0:01:36) ===========
```

The true result at that tree is `10918 passed, 142 skipped`. **An agent that had
just fixed the bug would be told, from cache, that its fix did not work.**

I reproduced this three ways to rule out an accident of the command shape:

| recorded command | at post-fix tree | served | truth |
|---|---|---|---|
| `.venv/bin/pytest -q test/orm/test_query.py -k slice` | `HIT tier=1` | `3 failed, 5 passed` | `8 passed` |
| `.venv/bin/pytest -q test/orm/test_query.py -k slice 2>&1 \| tail -5` | `HIT tier=1` | `3 failed, 5 passed` | `8 passed` |
| `.venv/bin/pytest -q test/orm 2>&1 \| tail -100` | `HIT tier=1` | `25 failed, 10893 passed` | `10918 passed` |

The pipe does not protect it; the chain rule splits the segments but the pytest
segment still carries a literal path argument and still promotes.

The flaw is in the premise, not the implementation. `scope.go` is careful about
everything it says it is careful about — it refuses on globs, variables,
absolute paths, toolchain manifests, and unclassifiable tokens, and it treats
ambiguous tokens as paths so that adding one can only block a promotion. But for
an interpreted language, **a test file's literal path argument is not its
dependency set.** `pytest test/orm` reads all of `lib/sqlalchemy`. Disjointness
in the filesystem says nothing about disjointness in the import graph, and the
design doc's own framing — "the difference between these two states provably
cannot affect this command" — is simply false for this class of command.

This matters more than the hit-rate numbers because test suites are, by the
project's own value analysis, 8.1% of hits and 55.6% of the deleted seconds.
Tier-1 is unsound precisely where the value is. It did not bite in this run only
because the agents happened to change `tail -100` to `tail -20` between rounds,
which changed the normalized command and left no candidate. That is luck, not a
guard.

### This has since been fixed, and the fix has a cost worth naming

Everything above was measured against the binary I built at 14:56 from commit
`2a3c1db`. While the experiment was running, `internal/hp/scope.go` gained a
`scopeSelfContainedHead` guard (working tree at 15:23, nine minutes after my last
measurement) that refuses promotion for any command that "reads more than its
arguments … follows a dependency graph the command line does not name".

I rebuilt from that source and re-ran the same two probes against the same store
and the same pair of trees. Both now return **MISS**:

```
recorded .venv/bin/pytest -q test/orm 2>&1 | tail -100  at bd8a6942
looked up at 20c1b6a7:
  2a3c1db build   HIT tier=1  -> would serve "25 failed, 10893 passed"
  patched build   MISS
```

So the hole is closed. I did not re-run the fan-out against the patched build,
so every number in this document is from the `2a3c1db` build; the only thing
that changes is that Tier-1 would refuse rather than promote, and Tier-1
promoted nothing in either arm anyway.

The cost is worth stating, because it is not free. `pytest` is not
self-contained and never will be, so **Tier-1 now refuses the entire test-suite
command class by construction.** The zero in the table above is no longer an
accident of this workload — it is the permanent answer for the commands that
carry 55.6% of the value. Tier-1's stated purpose was to recover the reuse that
exact-tree matching loses once agents diverge; after this fix it cannot do that
for test suites, only for `cat`, `head`, `wc`, `diff` and similar. That is the
correct trade — a wrong answer is worse than a miss — but the design doc's claim
that Tier-1 is "where the value is" no longer holds and should be rewritten.

---

## 7. Verification — two divergences

`fleet.sh`'s built-in post-run verification reported:

```
served 5 / verified 0 / 0 divergent  (5 skipped: workspace no longer in the recorded state)
```

That is a false all-clear, and the mechanism is worth recording: `fleet.sh`
creates its verification worktree with `git worktree add --detach <ref>` and no
dependency install, so its environment fingerprint is "python detected, not
installed" and every record is skipped as out-of-state. The check cannot fire on
any repository that needs an install, which is every repository it is aimed at.

I therefore ran verification myself, in two worktrees that genuinely reproduced
the recorded states: `/tmp/pf/w1` at the pre-fix tree with the venv installed,
and the cached arm's `a1` worktree at the post-fix tree. Both matched the
recorded `tree_before` and `env_fp_before` exactly.

| state | servable | verified | **divergent** |
|---|---|---|---|
| pre-fix (`bd8a6942`) | 5 | 3 | **1** |
| post-fix (`20c1b6a7`) | 4 | 0 | **1** |
| daemon total | 5 | 3 | **2** |

```
OK        grep -rn "_make_slice" lib/                    byte-identical
OK        grep -rn "_make_slice" lib/ | grep -v test     byte-identical
OK        grep -rn "_make_slice" lib/ | grep -v "\.pyc"  byte-identical
DIVERGED  .venv/bin/pytest -q test/orm 2>&1 | tail -100  stdout diverged after normalization
DIVERGED  .venv/bin/pytest -q test/orm 2>&1 | tail -20   stdout diverged after normalization
```

**Both pytest records diverged. Those two records are 100% of the deleted
execution-seconds.** Hindsight's own credibility mechanism cannot confirm the
only two results that carried any value in this run, and it evicted both.

The difference is one line in each, and the cause is the same in both. The
diff on normalized output, round one:

```
-=========== 25 failed, 10893 passed, 142 skipped in {{DUR}} (0:01:29) ===========
+=========== 25 failed, 10893 passed, 142 skipped in {{DUR}} (0:01:06) ===========
```

pytest prints its elapsed time twice: as `89.98s`, which `normDurRe` catches and
replaces with `{{DUR}}`, and again as `(0:01:29)` in `H:MM:SS` form, which no
pattern in `norm.go` matches. Round two is identical in shape: recorded
`in 65.07s (0:01:05)`, re-run `in 68.80s (0:01:08)`.

I am not going to call that harmless, because the honest description is that it
cuts both ways:

- No test outcome differs. The counts — `25 failed, 10893 passed, 142 skipped`
  and `10918 passed, 142 skipped` — are identical between the recording and the
  re-execution. The served output is a faithful replay of a real run.
- It is nevertheless a genuine difference in what the agent reads, produced by
  the same category of gap that the `ls -la` finding came from: the normalizer's
  list will always be incomplete, and this is the mechanism that says where.
- And it is load-bearing operationally. Divergence triggers eviction. Any run
  with in-run verification enabled would evict the suite record the first time a
  hit was verified, and every subsequent hit on it would disappear.

The last point interacts with a change I made to the harness, described below.

---

## 8. Where the hits actually were

Denominator: five agents per step, in the cached arm.

| step | command class | distinct command strings | reused | note |
|---|---|---|---|---|
| 1 | install | 1 | **0 / 5** | correctly unservable — the venv changes the env fingerprint |
| 2 | suite run, pre-fix | 1 | **4 / 5** | 1 executed, 4 coalesced |
| 3 | source search | 4 | **1 / 5** | see below |
| 4 | suite run, post-fix | 1 | **4 / 5** | 1 executed, 4 coalesced |

**Reuse did not decay across steps.** It was 80% at step 2 and 80% again at step
4. That contradicts the corpus prediction that reuse concentrates in the opening
commands and falls away — 16.9% across the first three, 1.0% after step 50 — and
it contradicts it for the reason the design doc already flagged as a caveat: all
five agents produced a byte-identical patch, so all five converged on the
identical post-fix tree `20c1b6a7` with the identical environment fingerprint,
and immediately shared the post-fix cache entry. A bug with several valid fixes
would have left them at five different trees and step 4 would have yielded
nothing.

**The real limiter was command-string variation, not state divergence.** At step
3 all five agents wanted the same thing and wrote it four different ways:

```
grep -rn "_make_slice" lib/                    a4, a2   <- the only shared string
grep -rn "_make_slice" lib/ | grep -v test     a3
grep -rn "_make_slice" lib/ 2>/dev/null        a1
grep -rn "_make_slice" lib/ | grep -v "\.pyc"  a5
```

The same variance nearly killed the suite hits. The task prompt pinned the exact
test command, and all five agents still appended their own output filter —
`2>&1 | tail -100` in round one, and in round two `tail -20` for four of them
and `tail -30` for the fifth in the baseline arm. Round one shared a string by
luck. Had the round-one filters differed the way the greps did, the hit rate
would have collapsed to near zero.

That is worth stating plainly as a limit on the whole result: **this measurement
is a best case.** The prompt told agents to use one exact install command and one
exact test command. A prompt that left the commands free would produce fewer
identical strings and a lower hit rate, and I did not measure that.

---

## 9. Overhead

Measured on this repository against a scratch daemon, so the experiment logs
stayed clean:

| component | cost |
|---|---|
| hook, warm side index (2 state hashes + daemon round trip) | 62–65 ms (first call 105 ms) |
| `hindsight record` wrapper, over raw execution (2 more state hashes + POST) | ~37 ms |
| **total per intercepted command** | **~100 ms** |
| cold side-index build, once per fresh worktree | ~1.3 s |

Cached arm: 21 intercepted commands × 0.1s + 5 worktrees × 1.3s ≈ **8.6
agent-seconds of 1196.8**, or **0.7%**. Per agent that is about 1.7s of a 240s
run. In fleet wall-clock terms the cold index builds happen in parallel, so the
visible cost is roughly 1.3s of 260.3s, or 0.5%.

Overhead is negligible here and would not be on a fast command — 100 ms to cache
a 3 ms `echo` is why the duration fastpath exists. It fired correctly during the
smoke test, marking `wc -l lib/sqlalchemy/sql/util.py` (6 ms) as
`below the duration floor: caching costs more than it saves`.

---

## Two setup problems I had to work around, and one harness change

### The environment fingerprint is worktree-dependent

Before running anything I checked the precondition that makes sharing possible:
do two worktrees with identical trees produce identical keys after installing?
They did not.

```
w1  tree bd8a6942…  env-fp 1f5f02b68111aae7868085d21ce3c113
w2  tree bd8a6942…  env-fp 30e8279bd76b8e2f83fbc545e1022042
```

Identical trees, different fingerprints, therefore different keys, therefore
zero hits. The cause is `pyvenv.cfg`, which `envfp.go` hashes byte-for-byte:

| how the venv was created | `pyvenv.cfg` across two worktrees |
|---|---|
| `uv venv` (no path argument) | **differs** — `prompt = w1` vs `prompt = w2` |
| `python3 -m venv .venv` | **differs** — `command = … -m venv /private/tmp/pf/w1/.venv` |
| `uv venv .venv` (explicit path) | identical |
| `uv venv --prompt <fixed>` | identical |

The two most common ways to create a Python virtualenv both leak per-worktree
identity into the fingerprint. The design doc is explicit that environment
variable *values* are "scrubbed of the worktree root and home directory before
hashing" for exactly this reason; the same scrubbing is not applied to
`pyvenv.cfg`.

The practical consequence: **on a default `uv venv` or `python -m venv` fan-out,
Hindsight's hit rate is structurally zero**, and it would look like the agents
diverged rather than like a fingerprint bug. I worked around it by pinning
`uv venv -p 3.12 .venv` in the task prompt and confirming the two worktrees then
produced byte-identical keys. Every number in this document depends on that
workaround.

### Claude Code does not enforce the hook's declared timeout

`hindsight init` writes `"timeout": 10` into `.claude/settings.json`. The lease
waits in this run blocked the hook for **96.9s and 69.8s**. That is the behaviour
the experiment needed — without it, coalescing on a 70-second suite would be
impossible — but it means the declared timeout is not a bound, and a wedged
daemon would hang an agent for the client's 10-minute HTTP timeout or the
daemon's 15-minute lease timeout rather than for 10 seconds. Worth knowing before
someone relies on it.

### I ran both arms with `--verify-rate 0`

`fleet.sh` defaults `HP_VERIFY_RATE=1.0`, which re-executes every served result
in the background, in-state. On a 4-second suite that is cheap. On a 70–100
second suite it returns *exactly the execution the hit deleted*, as background
CPU, in the treatment arm only — the baseline arm has no hits, so nothing fires
there. Leaving it on would have made the wall-clock comparison measure the cost
of verification rather than the effect of caching, and the brief calls wall
clock the open question.

Note also that a lease-coalesced result is returned to the hook as
`Decision: HIT` (only the daemon's log distinguishes `LEASE_WAIT`), so all eight
coalesced results would have spawned background re-executions: four concurrent
90-second pytest runs after round one and four more after round two.

So both arms ran with `HP_VERIFY_RATE=0`, identically, and I verified afterwards
in properly-installed worktrees instead — which produced a stricter check than
sampling would have, and is where the two divergences came from. This is a
deviation from the harness default and it is the only one; everything else was
`fleet.sh` as shipped. It makes the cached arm look *better* on wall clock than
the shipped configuration would, and it is the reason the run reported no
divergence at the time.

### Everything else

Both arms used their own daemon, own port and own empty `HP_HOME`
(`127.0.0.1:7811` / `/tmp/hs-real-base`, `127.0.0.1:7812` / `/tmp/hs-real-cached`),
both started genuinely cold, both ran the hook and recorded identically, and the
target repo was reset with `git checkout -- . && git clean -fd` between them. All
ten agents exited 0. Neither arm was re-run. Every daemon was killed afterwards.

**Binary provenance.** Built at 14:56 from commit `2a3c1db` with the exact
command given, then copied to `/tmp/hsbin/hindsight`. The hook points at the
copy, because `doctor`'s installed-hook check and `fleet.sh`'s preflight both
match on a binary literally named `hindsight` and reported false negatives
against `hindsight-bin`. That copy is `sha256:485c5e62a29c…` and its mtime is
still 14:58, so **both arms ran the identical binary.** Note that
`/tmp/hindsight-bin` itself was rebuilt by someone else at 15:08, during the
baseline arm, and now differs (`sha256:da0eec0c0548…`). It was referenced only by
`HINDSIGHT_BIN`, which `fleet.sh` uses for its own post-run verification call —
the call that skipped all five records and measured nothing. No measurement in
this document depends on it.

The repository moved during the experiment: `HEAD` was `2a3c1db` when I built and
`df3ce5f` afterwards, with further uncommitted work in `scope.go`, `perf_test.go`
and `bench.sh`. `EXPERIMENT.md` is the only file I created or modified.

---

## Verdict

**Does this repository's evidence support the claim that Hindsight deletes
meaningful execution time on a real codebase?** Yes, and by a wide margin.
1083.5 execution-seconds became 244.0, a 77.5% reduction, on a suite that takes
70–105 seconds a run, measured against a control that carried identical
instrumentation. Four of five agents skipped both suite runs entirely. That
number is real, and the 4% corpus figure is clearly a floor for a homogeneous
fan-out rather than an estimate of one.

**Does it move wall clock?** No. 243.0s to 260.3s, and the reason is structural
rather than a matter of scale. The previous explanation — model latency dominates
— is now dead: execution was 89.3% of elapsed time in the baseline arm and the
wall clock still did not move. The real reason is that **on a cold simultaneous
fan-out, essentially all of the value arrives through the in-flight lease, and a
lease wait converts execution time into blocked time at par.** The waiter saves
the CPU and saves no seconds. Only a plain index hit converts to elapsed time,
and a plain index hit requires that some peer already finished — which is exactly
what does not happen when five agents start together. There was one plain hit in
this run and it was worth 371 milliseconds.

That reframes the product rather than refuting it. What Hindsight demonstrably
buys on a cold fan-out is **CPU, energy, and machine capacity** — the ability to
run five agents on the hardware of one and a bit — not latency. The latency win
should exist for a *warm* cache, where a second fleet inherits a finished first
fleet's results and gets plain hits, and for staggered rather than simultaneous
starts. Neither was tested here. Until one of them is, "we do not claim a
wall-clock speedup" should stay in the document, and the reason given for it
should be changed, because the old reason is now known to be wrong.

**Is the value confined to the first few commands before agents diverge?** No,
and that is the one result that came out better than predicted. Reuse was 80% at
step 2 and 80% again at step 4, because all five agents produced a byte-identical
patch and converged on the same tree. But that is a property of a bug with one
obvious fix, and it is the caveat the design doc already carries. What actually
limited reuse was not divergence of state but divergence of *typing*: five agents
wanted the same grep and wrote it four ways.

**Two things should be settled before any of this is quoted.**

Tier-1 diff-disjoint scoping was unsound for interpreted languages and live in
the lookup path: on the build both arms ran, it will serve a stale failing test
result to an agent that has already fixed the bug, and it was off by luck rather
than by design. A `scopeSelfContainedHead` guard landed in the working tree
during the experiment and closes it — I rebuilt and confirmed both probes now
MISS. What remains is a documentation problem: with that guard, Tier-1 refuses
every test-suite command permanently, so it can no longer recover the reuse the
design doc says it exists to recover. "Where the value is" is now the one place
it cannot go.

The `pyvenv.cfg` leak silently reduces the hit rate to zero on the default way
of setting up a Python project, and it is still open. It is a small change in
`envfp.go` — scrub the worktree root out of `pyvenv.cfg` the way environment
variable values are already scrubbed, and drop `prompt` — and until it is made,
every real-world Python hit rate will read zero for the wrong reason and look
like agent divergence.

---

## Raw artifacts

Left in place for inspection; all outside this repository.

| path | contents |
|---|---|
| `/tmp/sa-target` | the target clone at ref `311a357a4`, bug and hook committed |
| `/tmp/sa-task.md` | the task prompt every agent received |
| `/tmp/hs-real-base/log.jsonl` | baseline cache log, 20 records |
| `/tmp/hs-real-cached/log.jsonl` | cached cache log, 21 records + 5 `VERIFY` records I appended afterwards |
| `/tmp/rr-base/`, `/tmp/rr-cached/` | `summary.txt`, `summary.json`, per-agent logs, and the kept worktrees |
| `/tmp/pf/w1` | pre-fix worktree with the venv installed, used for verification and the Tier-1 probes |
| `/tmp/doctor-cold.txt` | first `doctor` run |

The cached log's 26 lines are 12 `MISS` + 8 `LEASE_WAIT` + 1 `HIT` + 5 `VERIFY`.
Only the first 21 are the fan-out; the summary numbers were computed before
verification ran.

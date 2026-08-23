# Does Tier-2 fire on a live fan-out, and does it move wall clock?

Four five-agent fan-outs run on 2026-08-23 on an M-series Mac (12 cores, 34 GB):
two scenarios × two arms. The question is the one `EXPERIMENT.md` left open.
That run deleted 77.5% of execution-seconds and moved wall clock by nothing,
because 8 of its 9 hits were in-flight lease waits and a lease wait converts
execution time into blocked time at par. Only an index hit against a peer's
*completed* result returns immediately and becomes elapsed time. Tier-2
dependency scoping is the mechanism that is supposed to produce those index
hits. It had never been measured on a live fan-out.

**Short answer. Yes, and yes — on a scenario built for it.** Tier-2 fired ten
times across two cached arms, every one of them an index hit, and fleet wall
clock fell 18.5% and 20.1% against measured baselines carrying identical
instrumentation. It is the first wall-clock improvement this project has
measured. It is also the first result here whose headline depends on a target
repository I constructed, and section 8 says exactly how much of it I built.

Three findings outrank the headline:

1. **Tier-0 fired zero times in both cached arms, including the one where all
   five agents converged on a byte-identical tree.** A Tier-2 hit does not
   create a servable record at the new tree, so "convergent fixes give you
   Tier-0 sharing for free" — the story in `design_doc.md` principle 3 and
   `EXPERIMENT.md` §8 — only holds if some agent actually *executes* at the
   converged tree. Here Tier-2 got there first and nobody did.
2. **Single-flight has a wall-clock cost nobody has priced.** The lease
   releases all its waiters at the same instant, and the synchronized fleet
   then takes about 10 seconds longer to issue its next model turn than a
   staggered one does. Measured within one arm: the lease holder resumed 4.6 s
   after its gate finished; the four waiters took 12.9–16.0 s; baseline agents,
   who never blocked, took 2.4–2.9 s. That is 40.6 and 41.7 agent-seconds in the
   two runs — reproducing to within 1.1 s — and it takes back 9.4 of the 24.7
   seconds per agent that Tier-2 delivered.
3. **The daemon never records why Tier-2 refused.** `emitDecision` returns
   early on a `MISS`, so a refusal is invisible in `log.jsonl`. The scope reason
   is written down only when it promotes, which means the distribution of
   refusal reasons — the one output that would tell you which guard is too
   strict — cannot be recovered from a shipped run at all. I mapped the guard
   surface out of band instead (§6).

Everything below states its denominator.

---

## 1. Preflight — the two silent zeros

Both were checked before any fan-out, because each one would have made the run
measure nothing while looking healthy.

**The environment fingerprint matches across worktrees.** Two worktrees of the
target, each installed with `uv venv .venv -p 3.12 && uv pip install -q pytest`:

```
w1  tree 0ba9e605b34a…  env-fp d6492ff4c64f9413688abcf7948f5e2f
w2  tree 0ba9e605b34a…  env-fp d6492ff4c64f9413688abcf7948f5e2f
    key hs-v1:fe7b466c4898bada…  (identical)
```

`pyvenv.cfg` was byte-identical between the two. The `pyvenvVolatileKeys` fix
in `envfp.go` holds for `uv venv <path> -p <version>`. Note this does not
retire the `EXPERIMENT.md` finding: that fix drops `command` and `prompt` from
the hash, and I also passed an explicit path, so I did not re-test the bare
`uv venv` / `python -m venv` cases that originally leaked.

**The pytest plugin arms and captures.** One manual `hindsight record` of the
gate command produced a `read_set` in the log:

```json
{"method":"python/sys.modules","tool":"pytest","processes":1,"policy":"SERVE",
 "test_globs":["*_test.py","test_*.py"],
 "paths":["src/__init__.py","src/core.py","tests/__init__.py","tests/test_core.py"]}
```

**A Tier-2 dry run before committing to the fan-out.** Recording the gate at the
pristine tree and looking it up from a worktree that had fixed `src/parsing.py`:

```
HIT tier=2  from=probe  duration_ms=24264
  all 1 changed paths are absent from the 4-path read set observed for this
  command by python/sys.modules, the diff adds nothing it would newly read,
  and every changed path is of a kind that method reports
```

Re-executing the gate at that diverged tree gave the same result the cache
offered (`11 passed`), differing only in the pytest duration string.

**`hindsight doctor` on the target.** Kill-switch, daemon and cache lines
elided; this invocation was made from a shell that had `HP_ENABLE=1` set from
the overhead probe, with the cached arm's daemon still up.

```
hindsight doctor  /private/tmp/t2repo

OK    git worktree        /private/tmp/t2repo
                          side index   /private/tmp/t2repo/.git/hp-index
OK    tree hash           0ba9e605b34a  13ms warm (17ms cold), 19 files
OK    env fingerprint     d6492ff4c64f9413688abcf7948f5e2f  complete, 0ms
                          detected     python
                          registered   python, go, jvm, node, ruby, rust
OK    ecosystem coverage  pyproject.toml -> python
OK    codex hook          .codex/hooks.json  installed   timeout 10s  matcher Bash
OK    claude hook         .claude/settings.json  installed  timeout 10s  matcher Bash
OK    classifier          SERVE for reads, PASSTHROUGH for non-hermetic commands
OK    gitignore           ignores .venv

All checks passed. This workspace is cacheable.
```

Tree hash is 13 ms warm on a 19-file repo, against 22 ms on SQLAlchemy's 931.
The cold side-index build is 17 ms rather than SQLAlchemy's 1340 ms, so unlike
that experiment there is no per-worktree cold-start cost worth naming. The
whole hook round trip measured **6 ms median** here (7 samples, warm index,
daemon up), against 62–65 ms on SQLAlchemy. Overhead is not a factor in any
number below.

One doctor caveat reproduced from `EXPERIMENT.md`: the installed-hook check
matches on a binary literally named `hindsight`, and reported a false negative
against `/tmp/hs`. I copied the binary to `/tmp/hsbin/hindsight` and reran
`hindsight init`. Both hook configs point at that copy.

---

## 2. The scenario

A repository I wrote, `gridworks`, at `/tmp/t2repo`, ref `652586d`, 19 files.

```
src/core.py       61-bit rolling checksum. Nothing else may change it.
src/pricing.py    line-item discounts          seeded bug: floor instead of round-half-up
src/routing.py    least-cost paths             seeded bug: returns cost instead of hop count
src/ledger.py     double-entry balances        seeded bug: credit adds instead of subtracts
src/parsing.py    human page ranges            seeded bug: range(lo,hi) excludes the endpoint
src/retry.py      backoff schedules            seeded bug: delay + factor instead of delay * factor
tests/test_core.py       the shared gate — 11 tests, ~25 s, imports only src.core
tests/test_<module>.py   one fast unit file per module, under a second
```

The five service modules do not import each other. `tests/test_core.py`
re-derives eight published golden digests from scratch — 96 million rounds of a
Python mixer loop — which is why it is slow. It is CPU-bound and deterministic,
and its output is two lines: a progress bar and `11 passed in 24.05s`.

Every agent runs the same two pinned commands:

```
uv venv .venv -p 3.12 && uv pip install -q pytest
.venv/bin/pytest -q tests/test_core.py          # before its edit, and after
```

**Scenario A — divergent.** Each agent is told to fix one module, chosen by the
last path component of its working directory (`a1`→pricing … `a5`→retry). Five
agents, five different one-line edits, five different trees, and a gate whose
read set contains none of them.

**Scenario B — convergent.** Every agent fixes `src/parsing.py`. This is the
shape `EXPERIMENT.md` measured: one bug, one obvious fix, five agents
converging on an identical tree.

Both prompts forbid changing the pinned command strings, editing anything
outside the assigned file, and creating new files. All twenty agents obeyed
exactly: in every arm, every agent's `git status --porcelain` showed exactly
one modified file and nothing else, and every agent exited 0.

How contrived this is, stated plainly, is §8. The short version: **this is not
"Tier-2 fired in a natural fan-out." It is "Tier-2 fired in a fan-out
constructed so that it could."**

---

## 3. Tier breakdown of every hit

Counted from `"decision"` and `"tier"` in each arm's `log.jsonl`.

| | div. baseline | div. cached | conv. baseline | conv. cached |
|---|---|---|---|---|
| hook-visible commands | 25 | 25 | 20 | 17 |
| executed (MISS + PASSTHROUGH) | 25 | 16 | 20 | 8 |
| served (HIT + LEASE_WAIT) | 0 | 9 | 0 | 9 |
| — `LEASE_WAIT` | 0 | **4** | 0 | **4** |
| — index `HIT`, tier 0 | 0 | **0** | 0 | **0** |
| — index `HIT`, tier 1 | 0 | **0** | 0 | **0** |
| — index `HIT`, tier 2 | 0 | **5** | 0 | **5** |
| hit rate | 0.0% | 36.0% | 0.0% | 52.9% |

Ten tier-2 hits, zero tier-0, zero tier-1. Every hit carried the same reason:

```
tier-2: all 1 changed paths are absent from the 4-path read set observed for
this command by python/sys.modules, the diff adds nothing it would newly read,
and every changed path is of a kind that method reports
```

Of the five tier-2 hits in each arm, four were cross-agent and one was an agent
reusing its own earlier record.

The command counts differ between the convergent arms (20 vs 17) because the
duration fastpath learned that `pytest -q tests/test_parsing.py` costs 200 ms
and stopped intercepting it, which happens sooner when five agents share one
command string. Those unrecorded commands are worth about 0.2 s each.

### Tier-0 fired zero times, and that is the surprise

In the convergent arm all five agents produced the byte-identical fix and
reached tree `89060b8e`. Under the `EXPERIMENT.md` story they should have
shared a Tier-0 entry there. They did not, and the reason is structural: the
only agent that *executed* the gate did so at the pre-fix tree `0ba9e605`, and
`emitScopedHit` writes a `HIT` record that is not servable, so no servable
record ever existed at `89060b8e`. Tier-2 promoted all five against the pre-fix
record instead — and, because `tryScope` runs before `waitForPeer`, none of
them even took a lease.

So the round-two sharing that `EXPERIMENT.md` attributed to convergent fixes
does not require convergent fixes at all. It requires a peer's completed
record, at *any* provably-irrelevant tree. That is a strictly weaker condition,
and it is Tier-2's whole point.

---

## 4. Wall clock and execution-seconds

Denominator: all hook-visible commands in one five-agent fan-out per arm.
Baselines run the hook and record identically; the only variable is `HP_SERVE`.

### Scenario A — divergent

| | baseline | cached | change |
|---|---|---|---|
| **fleet wall clock** | **80.4s** | **65.5s** | **−18.5%** |
| slowest agent | 80.3s | 65.4s | −18.6% |
| mean agent | 75.5s | 61.2s | −18.9% |
| total agent-seconds | 377.7s | 306.1s | −19.0% |
| **execution-seconds** | **248.0s** | **29.1s** | **−88.3%** |
| execution-seconds deleted | 0.0s | 240.0s | |
| — won by in-flight leases | 0.0s | 106.7s | |
| — won by the index (all tier 2) | 0.0s | **133.3s** | |
| cached arm's own counterfactual | — | 269.1s | |

### Scenario B — convergent

| | baseline | cached | change |
|---|---|---|---|
| **fleet wall clock** | **77.1s** | **61.6s** | **−20.1%** |
| slowest agent | 77.1s | 61.6s | −20.1% |
| mean agent | 73.5s | 57.0s | −22.4% |
| total agent-seconds | 367.5s | 285.2s | −22.4% |
| **execution-seconds** | **245.9s** | **25.6s** | **−89.6%** |
| execution-seconds deleted | 0.0s | 216.9s | |
| — won by in-flight leases | 0.0s | 96.4s | |
| — won by the index (all tier 2) | 0.0s | **120.5s** | |
| cached arm's own counterfactual | — | 242.5s | |

The divergent arm's internal counterfactual (269.1s) sits 8.5% *above* the
measured baseline (248.0s), the opposite sign to `EXPERIMENT.md`. There is no
CPU contention to recover here — five single-threaded pytest processes on
twelve cores do not slow each other — so the cached arm's one uncontended gate
run (26.7s) was simply a slower draw than the baseline's five (24.1–25.0s).
Pricing avoided work at that rate therefore overstates it slightly, and the
88.3% figure above is measured against the baseline, not the counterfactual.
The convergent pair reconciles to within 1.4%.

### Per-agent time accounting, divergent

`busy` is execution plus time blocked on a peer's lease. `model` is the
remainder: the agent thinking, plus harness startup.

**Baseline**

| agent | wall | executing | blocked | model | busy % |
|---|---|---|---|---|---|
| a1 | 75.1s | 48.9s | 0.0s | 26.2s | 65.1% |
| a2 | 80.3s | 51.1s | 0.0s | 29.2s | 63.6% |
| a3 | 74.1s | 49.4s | 0.0s | 24.7s | 66.7% |
| a4 | 74.6s | 49.3s | 0.0s | 25.3s | 66.1% |
| a5 | 73.6s | 49.3s | 0.0s | 24.4s | 66.9% |
| **total** | **377.7s** | **248.0s** | **0.0s** | **129.7s** | **65.7%** |

**Cached**

| agent | wall | executing | blocked | model | busy % |
|---|---|---|---|---|---|
| a1 | 61.5s | 0.5s | 26.7s | 34.4s | 44.1% |
| a2 | 64.6s | 0.4s | 26.7s | 37.5s | 41.9% |
| a3 | 54.6s | 27.1s | 0.0s | 27.5s | 49.7% |
| a4 | 65.4s | 0.6s | 26.7s | 38.2s | 41.7% |
| a5 | 59.9s | 0.5s | 26.7s | 32.8s | 45.3% |
| **total** | **306.1s** | **29.1s** | **106.7s** | **170.3s** | **44.4%** |

**Total agent-seconds fell, 377.7 → 306.1.** In `EXPERIMENT.md` they were flat
(1213.5 → 1196.8), which was the clearest statement that nothing had actually
been saved in elapsed terms. That is the difference an index hit makes.

The same caveat applies as there: a `LEASE_WAIT` record is credited the
holder's full duration rather than the waiter's true block time, so `blocked`
is an upper bound and `model` a lower bound.

---

## 5. The number that matters

**Ten Tier-2 hits. Each returned from the index immediately, and each deleted
the full duration of the served command from the agent's critical path.**

| | divergent | convergent |
|---|---|---|
| tier-2 index hits | 5 | 5 |
| served command | `.venv/bin/pytest -q tests/test_core.py` | same |
| recorded duration of that command | 26.665s | 24.101s |
| **elapsed seconds removed from each agent** | **26.7s** | **24.1s** |
| **elapsed seconds removed in total** | **133.3s** | **120.5s** |
| observed mean per-agent wall reduction | 14.3s | 16.5s |
| observed fleet wall reduction | 14.9s | 15.5s |

The gap between the 26.7 s a tier-2 hit removed and the 14.3 s of wall clock
that actually appeared is not measurement slop, and it is the second finding of
this document.

### Where the other half went: the lease synchronizes the fleet

The per-agent timelines, as record-completion times relative to the first
record. `LEAS` is a lease wait, `HIT` is a tier-2 index hit.

```
divergent BASELINE
  a1  0.1 install   26.3 gate      29.2 pricing   39.3 pricing   66.5 gate     exit 75.1
  a4  0.1 install   26.6 gate      29.2 parsing   38.5 parsing   65.3 gate     exit 74.6

divergent CACHED
  a3  0.0 install   28.9 gate/MISS 33.5 ledger    44.3 ledger    46.5 gate/HIT exit 54.6
  a1  0.0 install   28.9 gate/LEAS 41.8 pricing   51.0 pricing   53.4 gate/HIT exit 61.5
  a4  0.2 install   28.9 gate/LEAS 45.0 parsing   55.0 parsing   57.2 gate/HIT exit 65.4
```

Look at the gap between the gate finishing and the next command:

| | gap after round-one gate |
|---|---|
| baseline, all five agents | 2.4 – 2.9s (mean 2.6s) |
| cached, `a3` — held the lease, executed, never blocked | 4.6s |
| cached, `a1 a2 a4 a5` — released from the lease together | 12.9 – 16.0s |

The four waiters are unblocked at the same instant and then all issue their
next model turn simultaneously. That burst is about 10 seconds slower per agent
than the naturally staggered baseline. `a3`, inside the same arm, on the same
machine, under the same conditions, but never blocked, pays 4.6 s.

Aggregated, it is the entire model-time difference between the arms: 129.7s →
170.3s, **+40.6s** in the divergent pair and **+41.7s** in the convergent pair.
Two independent runs agreeing to within 1.1 s is not run-to-run variance in API
latency, and the direction tracks the arm rather than the clock — the four runs
were interleaved in time and both baselines came out low, both cached arms high.

Since `blocked` is credited generously, reported `model` is a lower bound, so
the true cost is at least this large.

The whole per-agent wall-clock budget then reconciles, divergent scenario:

| | per agent |
|---|---|
| round-two gate the agent did not have to run (baseline mean) | **+24.7s** |
| round-one gate was slower in the cached arm (26.7s uncontended vs 24.5s mean) | −2.2s |
| lease-release penalty (mean post-gate gap 12.0s vs 2.6s) | −9.4s |
| **predicted** | **13.0s** |
| **observed mean per-agent wall reduction** | **14.3s** |

A 1.3-second residual on a 75-second run is as close as this kind of accounting
gets. **Tier-2 delivered 24.7 seconds per agent and single-flight took 9.4 of
them back.**

**This is a cost of single-flight, not of Tier-2**, and it did not show up in
`EXPERIMENT.md` because there was no index hit there to measure it against. It
means the two mechanisms interact: the lease deletes CPU and then charges some
of the wall clock back at the moment it releases. Staggering lease releases by
a second or two would be a cheap thing to try.

---

## 6. Why Tier-2 refused, when it did

**Tier-2 refused zero times in these four runs**, and that is not as
informative as it sounds: the only serve-eligible candidate that ever existed
was the gate record, and the module tests fall below the duration floor so they
never became candidates at all.

More usefully: **the daemon does not log refusals.** `emitDecision` returns
early for a `MISS`, so a `ScopeDecision` that refused is never written to
`log.jsonl`. The scope reason is recorded only on a promotion. There is no way
to answer "which guard is too strict" from a shipped run, which is a gap worth
closing given that the answer is the most actionable thing the mechanism could
tell you.

So I mapped the guard surface out of band, against the real read set captured
in the divergent cached run and real tree hashes computed in a real worktree of
the target, by calling `ScopeMatchObserved` directly. This is the same source at
the same commit with one extra `cmd/t2probe` file, built outside the repository.

| change to the tree | verdict | guard |
|---|---|---|
| modify `src/retry.py` (module outside the read set) | **PROMOTE** | — |
| modify `src/retry.py` **and** `src/parsing.py` (two peers at once) | **PROMOTE** | — |
| add `src/newmod.py` (new module deep in a package) | **PROMOTE** | — |
| add `NOTES.md` at the repo root | **PROMOTE** | — |
| modify `src/retry.py` and add `NOTES.md` | **PROMOTE** | — |
| modify `src/core.py` (module inside the read set) | REFUSE | in the observed read set |
| modify `tests/test_core.py` | REFUSE | in the observed read set |
| **modify `README.md`** | **REFUSE** | not a kind of file this method reports |
| modify `.gitignore` | REFUSE | configures the toolchain |
| modify `pyproject.toml` | REFUSE | configures the toolchain |
| add `scratch.py` at the repo root | REFUSE | new top-level module on the import path |
| add `tests/test_new.py` | REFUSE | matches the project's discovery glob |
| add `tests/fixtures/data.json` | REFUSE | under a directory tests were collected from |
| add `conftest.py` | REFUSE | python finds it by name, not by reference |
| modify `src/retry.py` and add `scratch.py` | REFUSE | the addition poisons the whole diff |

Every refusal is correct and every one of them is cheap to justify. The one to
worry about is `README.md`:

> changed path `README.md` is not a kind of file this capture method reports
> (`python/sys.modules` sees every in-repo Python module the run imported …),
> so its absence from the read set proves nothing

**`sys.modules` can only speak about `.py`, `.so` and `.pyd`, so modifying any
other file kind refuses permanently.** A markdown note, a JSON fixture, a YAML
config, a `.txt` — all of them refuse, and coding agents touch such files
constantly. In these runs no agent did, because the prompt forbade it. That
prohibition is doing real work and I put it there.

The asymmetry between adding a markdown file and modifying one is worth noting
because it is easy to get
backwards: *adding* `NOTES.md` promotes, because an added file has no previous
contents for the recorded run to have read, and `firstUnsafeAddition` already
answered what its existence does to the next run. *Modifying* `README.md`
refuses, because the recorded run might have `open()`ed it. That is the right
call in both directions and it is not obvious.

---

## 7. Were read sets captured, and for what fraction?

Denominator: commands that actually executed. A served command never runs, so
it cannot produce a read set.

| arm | executed | of which pytest | pytest commands with a read set | installs with one |
|---|---|---|---|---|
| divergent baseline | 25 | 20 | **20 / 20 (100%)** | 0 / 5 |
| divergent cached | 16 | 11 | **11 / 11 (100%)** | 0 / 5 |
| convergent baseline | 20 | 15 | **15 / 15 (100%)** | 0 / 5 |
| convergent cached | 8 | 3 | **3 / 3 (100%)** | 0 / 5 |

**49 of 49 executed pytest commands carried a read set.** Zero of the 20 `uv
venv && uv pip install` commands did, which is correct — that is not a Python
test session, and it is `RECORD_ONLY` anyway.

Against the whole log the figure reads 80%, 44%, 75% and 18%; those are the
wrong denominators and are quoted only so nobody recomputes them and thinks
capture is flaky. The capture path is armed and complete for every command it
is meant to cover.

Every one of the 49 sets was captured with `processes: 1` and carried the
project's own discovery globs, read out of its configuration rather than
assumed. Across all 22 executions of the gate command in all four arms the set
was identical — `src/__init__.py`, `src/core.py`, `tests/__init__.py`,
`tests/test_core.py` — which is the stability the promotion argument rests on.
The module tests produced their own sets, each containing the module under
test: `pytest -q tests/test_pricing.py`, for instance, reported
`src/pricing.py`. That is the read set correctly reporting the import edge that
Tier-1's literal-argument matching could never see.

---

## 8. Verification, and how contrived this is

### The tier-2 promotions were sound

All five tier-2 hits in the divergent arm served output recorded at the
pre-fix tree `0ba9e605` into worktrees at five different post-fix trees. I
re-executed the gate in each of those five worktrees, in place, and diffed
against the served blob:

```
served    ...........          [100%]   11 passed in 25.89s
a1 truth  ...........          [100%]   11 passed in 24.98s
a2 truth  ...........          [100%]   11 passed in 24.84s
a3 truth  ...........          [100%]   11 passed in 24.60s
a4 truth  ...........          [100%]   11 passed in 24.79s
a5 truth  ...........          [100%]   11 passed in 24.66s
```

The only difference in any of the five is the duration, which `normDurRe`
(`\b\d+\.\d+\s?(s|ms|sec|seconds)\b`) replaces with `{{DUR}}`. Byte-identical
after normalization, not byte-identical raw. Because the gate stays under a
minute, pytest never prints the `(0:01:29)` form that caused both divergences
in `EXPERIMENT.md` §7, so that gap is dodged here rather than fixed.

`fleet.sh`'s own post-run verification reported `served 1 / verified 0 / 0
divergent (1 skipped: workspace no longer in the recorded state)` in both
cached arms. That is the `EXPERIMENT.md` §7 finding reproducing exactly: the
verification worktree is created with `git worktree add --detach` and no
dependency install, so its fingerprint is "python detected, not installed" and
every record is skipped. The check still cannot fire on any repository that
needs an install.

All four arms ran with `--verify-rate 0`, identically, for the same reason
`EXPERIMENT.md` gives: in-run shadow verification re-executes served results in
the background, which on a 25-second gate returns exactly the execution the hit
deleted, in the treatment arm only. Leaving it on would have measured the cost
of verification instead of the effect of caching. This is the only deviation
from the shipped harness defaults.

### How contrived the scenario is

Six things I constructed, in descending order of how much they matter.

**1. The shared expensive command does not import the code under repair. This
is the whole result and it is the least natural thing about the repo.** I built
a gate whose read set is four files and had agents edit five other files. In
SQLAlchemy, `pytest -q test/orm` imports all of `lib/sqlalchemy`, so the file
every agent edits *is* in the read set, and Tier-2 refuses — correctly, and
permanently. My own guard map shows this: "modify `src/core.py` (module inside
the read set) → REFUSE". Whether real repositories have expensive commands that
are import-disjoint from the code being changed is the question that decides
whether Tier-2 is worth anything in production, and **this experiment does not
answer it.** It answers only that when such a command exists, the machinery
finds it, proves it, and converts it to elapsed time.

**2. Agents were told which module to take.** Divergence was assigned, by
mapping the worktree's directory name to a module in the prompt. That is a
work-division swarm, not the N-independent-attempts-at-one-bug fan-out that
`EXPERIMENT.md` measured and that motivates this project. Scenario B is the
honest comparison, and it is reassuring: Tier-2 fired there too, five times,
for a different reason (§3).

**3. Both commands were pinned character-for-character, and the prompt says
so twice.** `EXPERIMENT.md` §8 showed command-string variance, not state
divergence, is what actually kills hit rates — five agents wanted the same grep
and wrote it four ways. Every agent here obeyed. A prompt that left the
commands free would produce a lower hit rate and I did not measure that. This
is a best case in exactly the way that one was.

**4. The gate is synthetic CPU burn, not a real test suite.** 96 million rounds
of an arithmetic loop, chosen to take about 25 seconds deterministically. It is
honest about what it is — a slow, pure, import-bounded command — but it is not
evidence that a real suite behaves this way.

**5. The prompt forbids creating new files, and that rule is load-bearing.**
Per the guard map, a new `.py` at the repo root, anything matching the test
glob, and anything under a directory tests were collected from each refuse the
whole promotion. A new `.md` or a new module deep in a package does not. So the
prohibition mattered for some file kinds and not others, and I did not measure
what a fan-out that ignored it would have produced.

**6. The repository is 19 files with no root `conftest.py` and one import
root.** A larger repo with a root conftest would add its directory to
`importRoots`, and a repo with fixture directories under `tests/` would refuse
additions there. Both make Tier-2 stricter, not looser.

What is *not* contrived: all four arms ran the same binary with the same hook
and the same instrumentation, the baselines are measured rather than assumed, all
twenty agents followed the prompt exactly, no arm was re-run, and the two
scenarios were designed before any of them was run rather than selected
afterwards.

---

## Verdict

**Does Tier-2 fire on a live fan-out?** Yes. Ten times in two five-agent runs,
with no misfires and no wrong answers. Every one of the ten was an index hit
against a peer's completed result — tier 0 fired zero times and tier 1 fired
zero times, in both arms. The mechanism works end to end on a real fan-out with
real agents: the pytest plugin arms itself, captures a read set for 100% of the
commands it covers, the daemon finds the candidate by everything-except-the-tree,
git diffs the two trees, the guards clear, and the result comes back
immediately and is correct when re-executed.

**Does it move wall clock?** Yes, for the first time in this project. Fleet
wall clock fell 80.4s → 65.5s (−18.5%) and 77.1s → 61.6s (−20.1%) against
measured baselines. Total agent-seconds fell 19.0% and 22.4%, where in
`EXPERIMENT.md` they were flat. The claim that only an index hit converts
deleted CPU into deleted seconds is now measured rather than argued, and the
converse is measured too: the four lease waits in each arm deleted 106.7s and
96.4s of CPU and produced no elapsed time at all.

**What is the honest scope of that?** Narrow. The conversion happened because I
built a repository in which the expensive shared command is import-disjoint
from the code five agents were editing. That is the precondition, it is stated
in the code as the read-set disjointness test, and it is the thing to go and
look for in real repositories before quoting any of this. `EXPERIMENT.md`'s
target is a counter-example: on SQLAlchemy the gate imports the library, the
edit lands inside the read set, and Tier-2 refuses. Nothing here contradicts
that; the two runs bound the answer from opposite sides.

**Two things this run turned up that were not the question.**

Tier-0's role has been overstated. Five agents produced a byte-identical fix,
converged on one tree, and still got Tier-2 rather than Tier-0, because a
served hit does not repopulate the index at the new tree. "Convergent fixes
share downstream entries for free" needs the qualifier "if somebody executed
there", and in a cache that is working well, increasingly nobody does.

Single-flight charges wall clock back at release. The lease unblocks its
waiters simultaneously and the synchronized fleet is about 10 seconds per agent
slower to take its next model turn than a staggered one — 40.6s and 41.7s
across the two runs, reproducing to within 1.1s, and controlled within a single
arm by the lease holder, who paid 4.6s where its waiters paid 12.9–16.0s. It
hands back 9.4 of the 24.7 seconds per agent that Tier-2 delivered. It should be
measured again before anyone believes it, and if it holds, jittering lease
releases is a one-line experiment.

---

## Raw artifacts

All outside this repository; nothing in it was modified except this file.

| path | contents |
|---|---|
| `/tmp/t2repo` | the target repo at ref `652586d`, bugs and hook committed |
| `/tmp/t2-task-divergent.md`, `/tmp/t2-task-convergent.md` | the two prompts |
| `/tmp/t2-base/log.jsonl` | divergent baseline, 25 records |
| `/tmp/t2-cached/log.jsonl` | divergent cached, 25 records |
| `/tmp/t2-convbase/log.jsonl` | convergent baseline, 20 records |
| `/tmp/t2-conv/log.jsonl` | convergent cached, 17 records |
| `/tmp/t2-out-{base,cached,convbase,conv}` | `summary.txt`, `summary.json`, agent logs, kept worktrees |
| `/tmp/t2-precheck` | the preflight store: read-set probe and Tier-2 dry run |
| `/tmp/hs-probe-src/cmd/t2probe/main.go` | the guard-map probe, built outside this repo |
| `/tmp/t2-truth-a{1..5}.txt` | fresh gate output at each diverged tree, for §8 |

Each arm used its own daemon, its own port (7842, 7841, 7844, 7843) and its own
empty `HP_HOME`. All four started cold. All twenty agents exited 0. No arm was
re-run.

**Binary provenance.** Built from commit `a44ad42` ("Update benchmarks
writeup"), working tree clean, as `/tmp/hs` and copied to
`/tmp/hsbin/hindsight`, `sha256:96660bcd9ca7b94c24659a7c7886e92941f9f9bf2f56238f680271bc7804d659`.
Both hook configs and `HINDSIGHT_BIN` point at that copy. The guard-map probe
is the same source at the same commit with one added file, built out of tree.
Agents were driven by
`claude --print --permission-mode bypassPermissions`.

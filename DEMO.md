# Hindsight — demo plan

The claim, in one sentence:

> **When you fan out N coding agents on one task, Hindsight makes them pay once, in total, for the work all N would otherwise do independently — and a served result is a verified replay, so the failure mode is a cache miss, never a wrong answer.**

Everything here has been **run end to end on Arnav's machine**. The numbers below are ours, measured, reproducible with two commands. Contracts are in [AGENTS.md](AGENTS.md), the product writeup in [design_doc.md](design_doc.md), costs in [BENCHMARKS.md](BENCHMARKS.md), open problems in [ISSUES_FOR_TOM.md](ISSUES_FOR_TOM.md).

---

## 0. Run of show — lead with the mechanism, not the table

One command. Takes about 17 seconds.

```bash
export PATH="/opt/homebrew/bin:$HOME/.local/bin:$PATH"
source ~/src/sympy/.venv/bin/activate
bash scripts/demo-live.sh --agents 5 --port 7831
```

The table in §1 is the *claim*. This is the claim being earned on screen. `demo-live.sh`
prints the configuration up front — how many agents, the exact commands each will run, the
worktree paths, the daemon, and that the cache is fresh — then streams one line per decision
as the hook makes it, with a running total of deleted execution-seconds:

```
  agt  decision  command                                      effect        peer        Σ deleted
────────────────────────────────────────────────────────────────────────────────────────────────
  a1   EXECUTED  python -m pytest -q -…ore/tests/test_expr.py ran   3.00s   recorded       0.0s
  a2   WAIT      python -m pytest -q -…ore/tests/test_expr.py saved 3.00s   onto a1        3.0s

    ┌ why that replay was legal ────────────────────────────────────────────────────
    │ key   hs-v1:4b6b3fc0809ce105ce5ea75cd5a7b75653ce75cb…
    │ tree  89b863fc1ff5…   env  c44ee644caeb…   cwd  .
    │ cmd   python -m pytest -q -p no:cacheprovider sympy/core/tests/test_expr.py
    │ identical tree + identical environment + identical command  →  replay, not prediction
    └──────────────────────────────────────────────────────────────────────────────

  a5   WAIT      python -m pytest -q -…ore/tests/test_expr.py saved 3.00s   onto a1        6.0s
  a1   EXECUTED  python -m pytest -q -…ore/tests/test_arit.py ran   3.01s   recorded      12.0s
  …
  a2   WAIT      python -m pytest -q -…/tests/test_numbers.py saved 1.69s   onto a1       30.8s
```

Two things on that screen answer the "trust me bro" objection directly. The **key block**
prints the tree hash, environment fingerprint, cwd and command behind the first avoided
execution, so the audience sees a mechanical match rather than a model deciding two things
looked similar enough. And the **Σ deleted** column climbs in front of them rather than
appearing as a single number at the end.

The closing block states the reason in words:

```
  commands demanded                            15
    executed                                    3  distinct (command, tree, environment) states
    served without executing                   12
      of which coalesced in flight             12  blocked on a peer already running it

  execution-seconds  before                 38.4s  if every agent executed everything
  execution-seconds  after                   7.7s
  execution-seconds deleted                 30.8s  (80% deleted)

  fleet wall clock                          10.4s
  Wall clock is reported, never claimed: at this scale the bottleneck is agent
  latency, so deleted execution-seconds do not all become elapsed time.
```

Every line on screen is read back out of the daemon's own `log.jsonl` *after* the decision,
never inferred from what the script just did — so the screen and the evidence file in
`demo-runs/live-<stamp>/` are the same artefact and cannot drift apart.

`--mode baseline` runs the control arm, where the hook records everything and is forbidden
to serve any of it.

### An injected bug is available but not yet wired into this run

`~/src/sympy` has a branch `hindsight-demo-bug` (commit `54bfba2`) carrying a real,
deterministic regression: in `sympy/core/mod.py`, `Mod(p, 2)` returns `S.One` for even `p`
and `S.Zero` for odd `p` — the two return values are swapped. It fails exactly one test with
a legible message and leaves the other two demo commands green:

```
        # symbolic with known parity
        n = Symbol('n', even=True)
>       assert Mod(n, 2) == 0
E       assert 1 == 0
E        +  where 1 = Mod(n, 2)
FAILED sympy/core/tests/test_arit.py::test_Mod - assert 1 == 0
1 failed, 100 passed, 2 xfailed
```

It was chosen for **convergence**: the only fix that satisfies the test is swapping the two
constants back, which returns the worktree to git tree `89b863fc…` — byte-identical to
`master`. Agents that converge on an identical tree share every downstream cache entry, which
is what would produce round-two hits *after* the fix. Run the fan-out against it with
`--ref hindsight-demo-bug`. The fix-and-re-test phase is **not** scripted yet, so today this
only shows a failing test being replayed faithfully (exit code included), not the round-two
story. Do not claim round-two sharing from this run.

---

## 1. The measured result — our demo number

Five agents, five git worktrees of **SymPy**, launched simultaneously, each running the same three test commands. Both arms run the hook and record identically; **the only variable is whether hits are served.** Each arm gets a *fresh* cache, so the cached arm earns every hit from its own peers.

| | baseline | cached |
|---|---|---|
| commands agents asked for | 15 | 15 |
| executed | 15 | **3** |
| served | 0 | **12** |
| of which coalesced in flight | 0 | **12** |
| execution-seconds spent | 40.5s | **8.9s** |
| execution-seconds deleted | 0.0s | **35.6s** |
| hit rate | 0% | **80.0%** |
| wall clock | 8.9s | 9.9s |

**80% of execution-seconds deleted.** Reproduced twice independently (29.9s and 35.6s deleted, 80.0% both times).

What makes this stronger than a warm-cache number: **all 12 hits were won by in-flight leases, not read off disk.** This is a cold fan-out — no peer had finished when the others asked. A naive cache scores zero here. That is the single hardest case and the one that actually happens when you launch five agents at once.

And the honest half, which we say before anyone asks: **wall clock did not improve** (8.9s → 9.9s). Deleted execution-seconds are not elapsed time when the agents run in parallel anyway. We claim the first and not the second.

---

## 2. The target repo: SymPy, and why not Apache Beam

**Decision: `github.com/sympy/sympy`, shallow clone.** Chosen by testing candidates, not by reputation.

| | sympy | flask | apache/beam |
|---|---|---|---|
| tracked files | 2,087 | 236 | ~20,000 |
| runtime deps | 1 (`mpmath`) | 6 | dozens, multi-language |
| install time | seconds (`uv`) | seconds | **minutes**, Gradle + gRPC compile |
| per-file test target | **2.5–2.9s** | ~0.3s ✗ | minutes |
| full-suite target | 13.9s | 1.5s | very long |
| tree clean after tests | yes | yes | build artifacts |
| verdict | **use this** | too fast | demo-killer |

Three reasons this is the right call:

**The 500 ms floor decides everything.** `DefaultMinDurationMS = 500` in `internal/hp/fastpath.go`: anything faster is never servable, and after a few observations the fastpath memo stops intercepting it at all. Flask's per-file tests run ~0.3s — **below the floor, so they would show literally nothing.** SymPy's run 2.5–2.9s, comfortably clear.

**Apache Beam would fail on stage.** It is the recognisable name, but it is Gradle plus a multi-language install that takes minutes per worktree, times five worktrees, with native gRPC compilation. It also leaves build artifacts, which fails the purity gate. Every second of setup risk buys nothing the demo needs.

**SymPy is still a real repo.** 2,087 files, a household name in scientific Python. Nobody watching thinks it is a toy, and it installs in seconds because it is pure Python with one dependency.

If someone asks whether this works on a monorepo, the answer is measured and already in `BENCHMARKS.md`: **50,000 files hash in 156 ms warm**, linear at 2.9 µs per file with no cliff. We do not need to demo Beam to answer the Beam question.

---

## 3. Everything is scripted

Three new files. `scripts/` is our lane per AGENTS.md; none of Tom's files are touched.

| File | Does |
|---|---|
| `scripts/demo-setup.sh` | builds `hindsight`, clones sympy, makes the venv, **verifies every command clears the 500 ms floor**, runs `doctor` |
| `scripts/demo-cmds.txt` | the three commands every agent runs |
| `scripts/demo-prompt.md` | the prompt for the live-agent arm |
| `scripts/demo-run.sh` | runs both arms with fresh caches, stores evidence, prints the comparison |

Two commands, start to finish:

```bash
export PATH="/opt/homebrew/bin:$HOME/.local/bin:$PATH"
bash scripts/demo-setup.sh                     # once, ~2 min
source ~/src/sympy/.venv/bin/activate          # must be active
bash scripts/demo-run.sh --repo ~/src/sympy    # both arms, ~35s
```

`demo-setup.sh` refuses to pass if any command is under the floor, which is the failure that would otherwise only show up on stage as a counter stuck at zero.

**Evidence is stored per run**, which is what makes this checkable rather than asserted:

```
demo-runs/<timestamp>/
  baseline/  summary.txt  summary.json  log.jsonl  daemon.log  agent-logs/a1..a5.log
  cached/    summary.txt  summary.json  log.jsonl  daemon.log  agent-logs/a1..a5.log
```

`log.jsonl` is every decision the hook made, one JSON object per line. Run the demo repeatedly and the directory becomes a time series showing it was not a fluke. **Commit the good ones.**

### The rule that makes the comparison honest

Each arm gets a **fresh `$HP_HOME`**. Both arms record. If they share a cache and baseline runs first, the cached arm just replays baseline's records off disk and reports a meaningless ~100%. `demo-run.sh` enforces this; if you run `fleet.sh` by hand, pass a different `--hp-home` per arm or the number is worthless.

---

## 4. Screen layout

```
┌────────────────────────────┬────────────────────────────┐
│ TERMINAL (large)           │ VIEWER  web/viewer.html    │
│ demo-run.sh output:        │ live over SSE from :7777   │
│ baseline then cached,      │                            │
│ ending in the comparison   │ execution-seconds DELETED  │
│ table                      │ per-agent lanes a1..a5     │
│                            │ HIT / MISS / LEASE_WAIT    │
└────────────────────────────┴────────────────────────────┘
```

The cached arm runs on **port 7777**, which is the viewer's default, so it lights up with no configuration. The baseline arm uses 7778 deliberately — during baseline the viewer shows lanes filling with `MISS` and a counter pinned at zero, which is exactly the "before" picture.

---

## 5. Run of show — 3 minutes

### Beat 1 — The problem (20s, no commands)

> Launch five agents on one bug in five worktrees. Each installs the same dependencies, runs the same failing test to reproduce it, reads the same files. Nothing tells agent five that agent two already paid for all of it. The redundancy is structural: agents given the same task start from the same state, so they start with the same moves, and they only diverge as they learn. The opening stretch is the most redundant *and* the one where all five are doing it simultaneously.

### Beat 2 — Can this workspace even be cached? (20s)

```bash
hindsight doctor
```

> Eight checks before it caches anything. Tree hash 32 ms warm. Dependency fingerprint complete. Cache lives outside the worktree, because a cache inside the tree would change the hash that keys it. And the hook is inert unless armed — this repo installs a hook into its own config, so an armed hook would intercept the session we are demoing from.

### Beat 3 — Baseline: five agents, nothing shared (40s)

```bash
bash scripts/demo-run.sh --repo ~/src/sympy
```

While baseline runs, viewer on the right:

> Five agents, five worktrees, same three test commands. Every lane is a miss. Fifteen commands demanded, fifteen executed, **40.5 execution-seconds**, and the deleted counter never moves. This is the world today.

### Beat 4 — Cached: the same run, hits served (40s) ← **the demo**

Same script continues into the cached arm.

> Identical setup, identical instrumentation, one variable: hits are served. **Three commands executed. Twelve served.** All twelve coalesced in flight — the daemon holds a lease per key, the first caller executes, the rest block and are handed its result. Nothing was read from a warm disk cache; this is a cold fan-out and every hit had to be earned.
>
> **40.5 execution-seconds down to 8.9. Eighty percent deleted.**

Then, unprompted:

> Wall clock did not improve — 8.9 to 9.9 seconds. The agents were already running in parallel, so deleted execution-seconds do not become elapsed time. Those are different quantities and we only claim the first. It converts to wall clock when the suite takes minutes and the machine is contended, and we have not measured that, so we do not claim it.

### Beat 5 — Why you can trust it (30s)

> A cache that serves a wrong answer to a coding agent is worse than no cache. So: a record is servable only if the tree hash **and** the dependency fingerprint are byte-identical before and after it ran. Measured, not declared — which is why it catches `tsc` emitting `.js`, `cargo test` writing `target/`, and `uv sync` mutating a virtualenv the tree hash cannot see.
>
> On top of that, `hindsight verify` re-executes what the cache would serve and diffs it, evicting anything that disagrees. That mechanism already caught a real bug: on the first live fleet run it flagged `ls -la`, which mutates nothing and is not random, so nothing about it looked suspicious. Only re-executing it surfaced the problem. The deny-list will always be incomplete; this is the thing that tells you where.

**Do not run `verify` live right now.** It currently reports false divergences — see [ISSUES_FOR_TOM.md](ISSUES_FOR_TOM.md) issue 1. Describe the mechanism; show it once the normalization bug is fixed.

### Beat 6 — Close (15s)

> It is a cache, not a model. Nothing predicts anything. Default is passthrough, so a classification bug costs a hit and never correctness. It deletes duplicated work and can prove the work it deleted was duplicated.

### Beat 7 — Claude-Mem, only if time (30s, talk it)

> Hindsight shares **exact, verified observations** between agents running **now**. Claude-Mem shares **approximate, generative memory** across sessions that already **ended**. Different axes: exact/now versus approximate/later. Hindsight's cache is cold in a fresh session; Claude-Mem is what makes the *agent* warm when the *cache* is not.

Say explicitly: **Claude-Mem is never in the serve path.** One-way, advisory, a sidecar tailing `log.jsonl`. The moment it were in the serve path, the failure mode would stop being a cache miss. Design in [CMEM_LANE.md](CMEM_LANE.md).

---

## 6. Live agents — the optional upgrade

`demo-run.sh --live` swaps the deterministic replay for real coding agents. Both `claude` (2.1.159) and `codex` (0.149.0) are installed here; `fleet.sh` drives `codex exec --dangerously-bypass-hook-trust` by default, with the Claude line commented one above it.

**Recommendation: record it, do not run it live.** It is 30+ seconds per arm of watching models think, it needs network, and hit rate depends on whether five agents happen to phrase commands identically. `scripts/demo-prompt.md` pins the three commands verbatim to keep keys aligned — which is legitimate (dispatching a fleet, you give them the repro command) but must be disclosed.

Tom's recorded five-agent Claude Code run stays the headline credibility number: **77% of execution-seconds deleted, 53.3% hit rate, 15 hook-visible commands.** Ours is the reproducible one; his is the real-model one. Show both, label which is which.

---

## 7. Preflight — T-30

```bash
export PATH="/opt/homebrew/bin:$HOME/.local/bin:$PATH"
cd ~/Desktop/Hackathon/fasthack && git pull --rebase
go build ./...
VIRTUAL_ENV="" go test ./...           # see the note below before you panic
bash scripts/demo-setup.sh             # ends with "Setup complete."
source ~/src/sympy/.venv/bin/activate
bash scripts/demo-run.sh --repo ~/src/sympy   # full rehearsal
```

**Run the tests with `VIRTUAL_ENV=""`.** With a venv active, `TestEnvFingerprintDistinguishesVenvs` fails, because the env fingerprint reads the ambient `$VIRTUAL_ENV` instead of each worktree's own. That is a real bug ([issue 0](ISSUES_FOR_TOM.md)) and it is Tom's to fix, but it does not affect the demo: all five worktrees genuinely share one venv, so collapsing them to one fingerprint is the correct answer here. Do not let a red suite five minutes before the slot convince you something broke.

- [ ] `demo-setup.sh` prints `ok` for all three commands (none under 500 ms).
- [ ] Rehearsal shows ~80% hit rate. If it shows 0%, the daemon or `HP_SERVE` did not reach the agents.
- [ ] Ports 7777 and 7778 free. Check with Python, **not `lsof`** (see below).
- [ ] `web/viewer.html` open; confirm it animates on the embedded fixture with the daemon down.
- [ ] Venv **activated** in the demo shell — `which python` must be under `~/src/sympy/.venv`.
- [ ] `echo "[$HP_ENABLE]"` prints `[]`.
- [ ] A good `demo-runs/<stamp>/` committed as the fallback.
- [ ] Recording of the live arm on disk, playable offline.

```bash
# port check that does not kill your shell
python3 -c "
import socket
for p in (7777,7778):
    s=socket.socket(); s.settimeout(0.3)
    try: s.connect(('127.0.0.1',p)); print(p,'IN USE')
    except OSError: print(p,'free')
    finally: s.close()"
```

---

## 8. Failure playbook

Every row here was hit for real while building this.

| Symptom | Cause | Fix |
|---|---|---|
| hit rate 0% in cached arm | daemon down, or `HP_SERVE` never reached agents | `curl 127.0.0.1:7777/healthz`; fleet prints a warning it is easy to scroll past |
| `NO-DECISION`, nothing intercepted | command under the 500 ms floor, or fastpath memo learned it is cheap | slower command, or `HP_MIN_DURATION_MS=100` and disclose it |
| `servable=False` everywhere | duration floor, or the command dirtied the tree | read `reason` in `log.jsonl`; it names which |
| cached arm shows ~100% | both arms shared one `$HP_HOME` | fresh cache per arm; `demo-run.sh` does this |
| every lookup misses, no reason given | stale daemon holding the port, mismatched store | kill by PID, restart, re-check `/healthz` |
| concurrent agents all passthrough | agents sharing one worktree, side-index contention | separate worktrees — correct fail-open behavior |
| `verify` reports divergence | known normalization bug, issue 1 | do not run it live yet |
| **shell dies with no output** | `pkill -f "hindsight daemon"` or an `lsof` loop matched its own command line | kill by recorded PID; check ports with Python |
| pytest can't import sympy | venv not activated in the demo shell | `source ~/src/sympy/.venv/bin/activate` |

---

## 9. Q&A prep

**"Isn't this ccache / sccache / Bazel?"** Same instinct, different domain. Those key on declared inputs for a known compiler. We key on git's hash of the whole live worktree plus a dependency fingerprint, for arbitrary shell commands, with purity measured after the fact rather than declared. And they were not built for N processes racing on identical state, which is what single-flight handles — 12 of our 12 hits.

**"What if the cache is wrong?"** Default is passthrough; anything uncertain executes normally. The key contains everything that can change the output or the command is not served. And `verify` re-executes and evicts on disagreement — it already caught a real one, `ls -la`.

**"Why not share the fix between agents?"** An edit is a decision, not an observation. Agent two inheriting agent one's patch collapses five independent searches into one and destroys the reason to fan out. We share consequences, never the route.

**"Why sympy and not a huge repo?"** Because the honest constraint is the 500 ms floor, and we needed commands that genuinely take seconds. Scale is answered by measurement instead: 50,000 files hash in 156 ms warm, linear, no cliff.

**"80% seems too good."** It is a deterministic arm with pinned commands, which we disclose. The real-model run is 53.3%. The replayed SWE-bench corpus, keyed the way we actually key, gives 7.5% avoidable and 3.6% cross-agent — but that corpus mixes different models from a pre-built container, so it is a floor, not an estimate. We do not have a principled interpolation between 3.6% and 53%, and we do not pretend to.

**"So agents finish faster?"** No, and we measured it: 8.9s → 9.9s. It deletes execution-seconds. Those become elapsed time only when execution dominates wall clock, which is long suites on a contended machine — not measured, not claimed.

---

## 10. What we do not claim

- Not a wall-clock speedup.
- Not a correctness improvement; it does not stop an agent making a mistake.
- Not validated at scale: one repo, one task, one machine.
- Not a full command surface: agents use native Read/Edit tools, so only ~3 Bash commands per agent reach the hook.
- Not useful below 500 ms — a deliberate refusal, not a gap.
- Not a route-sharing system.

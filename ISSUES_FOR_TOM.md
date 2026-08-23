# Issues + open questions for Tom

Found while building the demo (see [DEMO.md](DEMO.md)). Everything below is reproduced on Arnav's machine at `main`, with evidence committed under `demo-runs/`.

**Good news first:** the whole thing works. Five agents on SymPy, cold cache, **80% hit rate, 40.5s → 8.9s execution-seconds, 35.6s deleted, all 12 hits won by in-flight leases.** Reproduced twice. The two issues below are about *verification and reporting*, not about the cache.

---

## The text-Tom version

> Ran a full 5-agent baseline-vs-cached fan-out on sympy. Cache works great — 80% hit rate, 40.5s of execution-seconds down to 8.9s, all 12 hits from in-flight leases on a cold cache. Three problems though, and the first one is the scary one.
>
> **0. `go test ./...` fails whenever a Python venv is active in your shell, and it's the env-fingerprint collision test that fails.** `TestEnvFingerprintDistinguishesVenvs` — two worktrees with different installed packages getting the same fingerprint. With `VIRTUAL_ENV` set it FAILS, with `VIRTUAL_ENV=""` it PASSES, same commit, no Go code changed. Looks like the fingerprint reads the ambient `$VIRTUAL_ENV` rather than the worktree's own `.venv`. That's the one hole the README says could make us serve a wrong answer, and it's live in exactly the situation we ship into — anyone caching a Python repo has a venv activated. Doesn't corrupt our demo (all five worktrees really do share one venv there, so collapsing them is accidentally correct) but the mechanism is wrong.
>
> **1. Shadow verification is false-positiving hard — 9 of 12 records "diverged" and it's evicting good cache entries.** I think it's `cmd/hindsight/verify.go:89-92`: you normalize both the fresh output and the recorded blob with `ws.Root`, but the recorded blob was produced in a *different* worktree, so its absolute paths never match and survive normalization while the fresh ones get replaced. Guaranteed mismatch on any output containing a path. Evidence: the one test file whose output has no absolute paths verified 3/3; the two with pytest warning blocks (which print absolute paths) failed every time. Cleanest fix is probably to normalize at record time and store that alongside the raw blob, so each side gets scrubbed with its own root.
>
> **2. `summary.json` reports `verified_true: 0, verified_false: 0` while the log has 3 true and 9 false.** That's my file (`fleet.sh`) — the counter only reads `verified` on `HIT` records but verification writes separate `VERIFY` records, so they fall through. I'll fix it. Flagging because right now the machine-readable summary gives a clean bill of health that the log contradicts, and we should not quote it until it's fixed.
>
> Also: 7 of the 12 `VERIFY` records have an empty `cmd` field — might be related to #1, might be separate.
>
> Not running `verify` in the demo until #1 is fixed. Everything else is ready.

---

## Issue 0 — Env fingerprint reads the ambient `$VIRTUAL_ENV`, not the worktree's

**Severity: highest.** This is the invariant the README singles out: *"Two worktrees with byte-identical trees and different installed packages must not share a key — that is the single hole that could make Hindsight wrong."* The regression test for exactly that hole is currently red.

### The experiment

Same commit, same machine, same code, one variable:

```bash
# VIRTUAL_ENV=/Users/arnavarora/src/sympy/.venv
go test ./internal/hp/ -run TestEnvFingerprintDistinguishesVenvs -count=1
# FAIL: different installed packages produced the same env fingerprint

VIRTUAL_ENV="" go test ./internal/hp/ -run TestEnvFingerprintDistinguishesVenvs -count=1
# ok
```

`TestEnvFingerprintDistinguishesVenvs` builds two repos with identical trees and deliberately different `.venv/lib/python3.12/site-packages` contents (one has an extra `numpy-1.26.0.dist-info`) and asserts the fingerprints differ. With a venv active in the shell, they don't.

### Why I'm confident it isn't a recent regression

- `internal/hp/key.go` has not changed in the last four commits.
- The only files changed since `df3ce5f` are `MY_TASKS.md`, `web/fixture.jsonl`, `web/viewer.html` — no Go code.
- The test fails at `df3ce5f` too, in a clean detached worktree. It passed for me earlier in the same session, **before I activated a venv.**

So it is environment-dependent, and `VIRTUAL_ENV` is the variable.

### Why it matters more than a red test

Every realistic use of this tool on a Python repo happens with a venv activated. Our own demo requires `source ~/src/sympy/.venv/bin/activate` before `demo-run.sh`, and `fleet.sh` exports the parent environment into all five agents — so during our own measured runs the fingerprint is very likely describing the shared venv rather than each worktree.

**It does not invalidate our numbers.** All five worktrees genuinely do share one venv, so collapsing them to one fingerprint is accidentally the correct answer there. But the mechanism is wrong: give two agents different virtualenvs and they would collide, which is precisely the wrong-answer case the fingerprint exists to prevent.

### What I think the fix is

The fingerprint should describe the *workspace being keyed*, not the ambient shell. Resolve the venv from the worktree root (`<root>/.venv`, `pyvenv.cfg`, or whatever the ecosystem plugin declares) and ignore `$VIRTUAL_ENV`; or if the ambient venv is genuinely the one in use, include its *resolved path* in the fingerprint so two agents pointing at different venvs cannot collide. `key.go` is yours — I have not touched it.

Worth a test that pins the behaviour with `VIRTUAL_ENV` set to something unrelated, since that is the normal case and it is currently the untested one.

---

## Issue 1 — Shadow verification false-positives and evicts valid entries

**Severity: high.** It undermines the trust story, which is the strongest part of the pitch, and it actively deletes good cache entries.

### What happens

From `demo-runs/20260823-154831/cached/`:

| | count |
|---|---|
| `VERIFY` records, `verified=true` | 3 |
| `VERIFY` records, `verified=false` | **9** |
| post-run `hindsight verify` | `served 2 / verified 1 / 1 divergent` |
| evicted | `CACHE_MISMATCH: 1 divergent record(s) evicted` |

**75% false-divergence rate.** The breakdown is the tell:

| test file | output contains absolute paths? | verified |
|---|---|---|
| `test_expr.py` | no (no warnings) | **3/3 pass** |
| `test_arit.py` | yes (1 warning block) | fail |
| `test_numbers.py` | yes (2 warning blocks) | fail |

### Root cause

`cmd/hindsight/verify.go:88-95`:

```go
rawMatch := bytes.Equal(res.Stdout, wantOut) && bytes.Equal(res.Stderr, wantErr)
gotOutN  := hp.Normalize(res.Stdout, ws.Root, home)   // fresh output, verifier's root
wantOutN := hp.Normalize(wantOut,    ws.Root, home)   // RECORDED blob, ALSO verifier's root
```

Both blobs are scrubbed with `ws.Root`, the **verifier's** worktree. But `wantOut` was produced in some *other* worktree — agent `a1`'s. `Normalize` does a literal `bytes.ReplaceAll` of `root`, so:

- fresh output `/private/tmp/fleet-cached-.../verify/sympy/...` → `{{ROOT}}/sympy/...` ✓
- recorded blob `/private/tmp/fleet-cached-.../a1/sympy/...` → **unchanged**, `a1` never matches `verify` ✗

Any output containing an absolute worktree path is guaranteed to mismatch. `fleet.sh` deliberately verifies in a *pristine* worktree (`$OUT/verify`), which is correct for state reasons but guarantees the root differs from every recorded one.

### Reproduction

```bash
cd ~/src/sympy && source .venv/bin/activate
git worktree add -q --detach /tmp/wtA HEAD && git worktree add -q --detach /tmp/wtB HEAD
CMD='python -m pytest -q -p no:cacheprovider sympy/core/tests/test_numbers.py'
(cd /tmp/wtA && eval "$CMD" >/tmp/outA.txt 2>&1)
(cd /tmp/wtB && eval "$CMD" >/tmp/outB.txt 2>&1)
diff /tmp/outA.txt /tmp/outB.txt
```

```
5c5
<   /private/tmp/wtA/sympy/core/tests/test_numbers.py:56: PytestUnknownMarkWarning: ...
>   /private/tmp/wtB/sympy/core/tests/test_numbers.py:56: PytestUnknownMarkWarning: ...
```

Two correct runs, differing only in the worktree path. Exactly what `Normalize` exists to erase, and exactly what it cannot erase when both sides are given the same root.

### Suggested fixes, in order of preference

1. **Normalize at record time and store the scrubbed form next to the raw blob.** Serving still replays raw bytes; verification compares fresh-normalized against stored-normalized. Each side is scrubbed with the root it was actually produced under. Touches `record.go`/`store.go` (yours).
2. **Store the producing root on the record** and pass it: `hp.Normalize(wantOut, rec.Root, home)`. Smaller, but grows the record.
3. **Stopgap in `norm.go` (mine):** a regex collapsing worktree-shaped absolute paths without needing to know the root. Fragile — I'd rather not ship it as the real fix, but I can land it in 20 minutes if we want `verify` on screen and you're busy.

Tell me which and I'll do the `norm.go` half.

### Until it's fixed

`DEMO.md` says describe the verification mechanism but **don't run it live.** The mechanism is genuinely a strong part of the story — it already caught the real `ls -la` bug — so it's worth fixing rather than dropping.

---

## Issue 2 — `summary.json` always reports zero verifications *(mine, I'll fix)*

**Severity: high**, because it is a wrong claim in the machine-readable artifact.

Same run: the log holds 3 `verified=true` and 9 `verified=false`, and `summary.txt` prints `CACHE_MISMATCH: 1 divergent record(s) evicted`. Meanwhile:

```json
{"served": 12, "hit_rate_pct": 80.0, "verified_true": 0, "verified_false": 0}
```

Cause is in my file, `scripts/fleet.sh`, in the summary block around lines 693-726. The loop reads `verified` only inside the `HIT` branch:

```python
if dec == "HIT":
    ...
    if r.get("verified") is True:  verified_true += 1
    elif r.get("verified") is False: verified_false += 1
elif dec in EXECUTED: ...
elif dec == "LEASE_WAIT": ...
```

Verification emits records with `decision == "VERIFY"`, which match none of those branches and are silently dropped. It also means the `*** N DIVERGENT ***` warning can never fire, which is the guard that is supposed to stop us quoting a bad run.

Mine to fix — noting it here so you know the JSON is currently not trustworthy on this axis.

---

## Issue 3 — `VERIFY` records with an empty `cmd`

**Severity: low**, possibly a symptom of issue 1.

7 of the 12 `VERIFY` records have `cmd: ""`. The other 5 carry the command. Makes the log hard to read and breaks any grouping by command. Might just be the eviction path writing a partial record.

---

## Smaller findings worth knowing

These are all correct behaviour, but each cost real time to diagnose, so they belong in someone's head:

- **The 500 ms floor is the single biggest demo constraint.** `DefaultMinDurationMS = 500`. My first three attempts at a hit showed nothing: a 220 ms `grep` is marked `servable=False` with reason *"below the duration floor"*, and after ~3 observations the fastpath memo stops intercepting it entirely so the hook emits no decision at all. This is right, but it means any demo built on `ls`/`git status`/small greps shows a counter pinned at zero. `scripts/demo-setup.sh` now hard-fails if any demo command is under the floor.
- **Both arms must use separate `$HP_HOME`s.** Both arms record. Share a cache with baseline first and the cached arm replays baseline's records off disk for a meaningless ~100%. `demo-run.sh` enforces a fresh home per arm. Worth a line in `fleet.sh --help`, since the positional form makes it easy to get wrong.
- **Concurrent agents in the *same* worktree all degrade to passthrough.** Five hooks in one worktree contend on the single side index, `git add -A` interleaves, the key can't be derived, and fail-open correctly kicks in — 4 of 5 produced no decision. Correct, and `fleet.sh` never does this, but it silently shows zero if anyone hand-rolls a test.
- **`pkill -f "hindsight daemon"` kills your own shell** — the pattern matches the invoking command line. Killed my session twice. So does an `lsof -ti tcp:$p` loop, for reasons I never pinned down. Kill by recorded PID; check ports with a Python socket connect.
- **A stale daemon holding the port fails silently and looks exactly like a broken cache** — records and lookups go to mismatched stores and every lookup misses with no error anywhere.

---

## Questions I need answers to

1. **Which fix for issue 1, and are you taking it?** If you're deep in something else I'll ship the `norm.go` stopgap so `verify` is demoable.
2. **Which machine drives the demo?** Mine now builds, serves, verifies, and has `claude` 2.1.159 + `codex` 0.149.0 + `uv`. You have the recorded five-agent run and the API keys. I'd rather rehearse on one and keep the recording on both.
3. **Do we show your live five-agent Claude Code run, or my deterministic sympy run, or both?** My instinct: lead with mine because it's reproducible in 35 seconds on demand, and cite yours (77% deleted, 53.3% hit rate) as the real-model number. Both labelled for what they are.
4. **Is the poisoned-blob `CACHE_MISMATCH` path a single command?** It's the strongest trust beat if it is — but it's blocked behind issue 1 either way.
5. **Do you want `demo-runs/` committed?** It's the time-series evidence that this isn't a fluke. Adds a few hundred KB per run.

---

## Remaining coding work

### Mine (teammate lane — `policy.go`, `norm.go`, `scripts/`, `web/viewer.html`)

| # | Task | Why | Size |
|---|---|---|---|
| 1 | Fix `summary.json` verification accounting (issue 2) | Wrong claim in the artifact we quote | 20m |
| 2 | Surface divergence/eviction in the headline comparison output | Right now you have to read `summary.txt` to notice | 15m |
| 3 | `norm.go` stopgap path scrub + tests, *if Tom wants it* | Unblocks `verify` on screen | 20m |
| 4 | Rehearse `demo-run.sh --live` once with real agents | Only the deterministic arm is proven | 20m |
| 5 | Confirm `policy.go` classifies `python -m pytest ...` as `SERVE` | Works empirically; want the test pinned | 10m |

**Done — the viewer is verified live end to end.** This was the risk I was most worried about, because the viewer silently falls back to the fixture when it cannot reach the daemon and *looks identical from the outside* (its own source comment says so). Confirmed on a real 3-agent run against the daemon on 7777:

- `/events` returns `Access-Control-Allow-Origin: *` and `Content-Type: text/event-stream`, so a `file://` page with a null origin can subscribe.
- The stream delivered **29 events during one run**: 9 `decision` (3 `MISS`, 6 `LEASE_WAIT`), 15 `stats`, 5 `verify`.
- `/agents` answers for the fleet map.

So the counter, the lanes and the fleet map all animate from real data. No configuration needed as long as the cached arm is on 7777.

### Tom's lane

| # | Task | Why |
|---|---|---|
| 1 | **Issue 0 — env fingerprint reads `$VIRTUAL_ENV`** | The correctness invariant; its regression test is red |
| 2 | Issue 1 — verification normalization root | Blocks the trust beat; evicting valid entries |
| 3 | Issue 3 — empty `cmd` on `VERIFY` records | Log readability |
| 4 | Confirm eviction can't cascade | If false divergence evicts aggressively, a long run could empty its own cache |

### Not doing

Claude-Mem stays talk-only unless everything above is green — it's a bolt-on and it must never touch the serve path. Design is in [CMEM_LANE.md](CMEM_LANE.md).

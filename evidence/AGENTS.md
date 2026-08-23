# Evidence track — agent brief

Scoped instructions for work inside `evidence/`. The root `AGENTS.md` still applies; this file adds to it.

## Context

Hindsight is a build cache for coding agents. When you fan out N agents on one task in N git worktrees, each one independently installs the same dependencies, runs the same failing test, and reads the same files. Hindsight intercepts every shell command with a PreToolUse hook, keys it on git's own Merkle hash of the workspace plus an environment fingerprint, and if a peer already ran that exact command at that exact state, returns the recorded result instead of executing it. Nothing is predicted. A served result is a verified replay, so the failure mode is a cache miss, never a wrong answer.

The pitch is a subtraction: execution-seconds deleted, work paid for once and provably not paid for again.

**This track owns whether anyone believes that.** The engineering track can build a perfect cache and still lose to the first question from the audience, which will be some version of "how much overlap is actually there, and how do you know?" Everything below is about answering that with numbers someone else could regenerate.

## Files you own

- Everything under `evidence/`.
- Nothing else. Do not edit any `.go` file, `AGENTS.md` at the repo root, `PLAN.md`, `design_doc.md`, `scripts/`, or `web/`. Other agents are actively writing those and will clobber you or be clobbered.
- `seed/` is **read-only**. Those are rescued artifacts and their line counts are cited in documents. Read them, copy them into `evidence/` if you want to modify them, never edit in place.

Write your prose conclusions to `evidence/claims.md`. That file gets folded into `design_doc.md` at the end by someone else, which is how we avoid both of us editing the deliverable at once.

Python is fine here. Standard library plus whatever is already installed; if you need a package, prefer writing it yourself over adding a dependency.

## Verified facts — do not re-derive these

Someone already checked all of this against disk. Trust it and build on it.

- **The corpus** is at `/Users/tomjeong/hacker/skunk-works/notes/sealed-corpus/replay-A`. It holds 265 sharded JSON records under `records/`, one per replayed agent attempt, across 31 distinct SWE-bench instances, of which 25 have two or more agents. Fan-out widths are exactly 3, 4, 5, 6, 9, 10, and 26.
- **It is about a third of a planned run.** `plan-manifest.json` describes 736 candidates, 158 tasks, 1,464 trajectories, 75 units. On disk: 265 records, 28 units, 249 `complete`, 16 `replay_error`. The manifest's own invariant does not hold against the contents. Say "a partial run" in any writeup, never "the corpus."
- **The published overlap numbers reproduce exactly.** Re-running `seed/vn2.py` gives 17.7% at 1 command, 26.5% at 3, 24.3% at 5, 19.2% at 10, 11.0% after step 10, 13.7% overall, 10.4% for expensive commands.
- **11,687 is a deduplicated count.** `vn2.py` builds a `set()` per agent. Raw is 12,806 across multi-agent tasks, 13,149 across all records.
- **The value table reproduces exactly** and is a model, not a measurement. `seed/value.py:5` hardcodes `COST={'install':45.0,'suite':90.0,'testfile':8.0,'build':25.0,'lint':6.0,'read':0.05,'other':0.3}`. Hit counts are real; every second in that table is arithmetic over assumed costs.
- **ρ = −0.252, p = 8.3e-77 has no producing artifact.** It appears in four notes under `skunk-works/notes/benchmark-questions/`, all pointing at `oneirology-taskyield/YIELD_AUDIT.md`, which does not exist. No script computes it. It has been cut from our documents. Do not reintroduce it.

### Record shape

Each record is `{candidate_id, evidence{attempt_count, docker_image, final_diff, steps[]}, summary{...}, source{...}, task{...}, status, unit_id}`.

A step looks like:

```json
{"n":0,"cmd":"ls -la /testbed","rc_stored":0,"rc_replay":0,
 "state_sha256":"44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
 "delta":{},"diff_lines":0,"n_files":0,"mutated":false}
```

`summary` carries `instance_id`, `steps`, `mutating_steps`, `rc_agree`, and `wall_s` (whole-trajectory wall clock, e.g. 165.063).

**There is no per-command duration anywhere in the corpus.** Only per-trajectory `wall_s`. This is load-bearing and you should confirm it yourself, because it means the per-class seconds in the value table can never be measured from this corpus — they have to come from the live fleet run or stay explicitly labelled as modelled.

## Tasks, in priority order

### 1. State-keyed overlap — the real number

This is the highest-value thing available and nobody has done it.

Every published overlap figure matches on the **command string alone**. But Hindsight does not key on the command, it keys on `(workspace state, command)`. The run sheet admits this gap in its own caveats. So 26.5% is an upper bound on a proxy, not the thing we actually serve on, and a sharp listener will notice.

The corpus contains `state_sha256` per step. If that is a cumulative workspace-state hash, you can compute the true statistic: how often do two independent agents run the same command *at the same state*?

**First, establish what `state_sha256` actually hashes.** Note that step 0 above has `delta: {}` and `state_sha256: 44136fa3...`, which is precisely `sha256("{}")`. So it appears to hash the `delta` object rather than the workspace tree. Determine whether `delta` is cumulative (a running diff from the base commit, which makes `state_sha256` a legitimate state key) or per-step (which makes it nearly useless for this purpose, and you should say so and stop). Verify by walking the steps of a single trajectory and checking whether `delta` and `diff_lines` grow monotonically.

If it is cumulative, produce the state-keyed overlap decay curve alongside the command-only one, using the same held-out-per-agent methodology `vn2.py` uses (each agent's set compared against the union of its peers). The honest expectation is that state-keyed overlap is **lower** than command-only overlap. Report it anyway. A smaller number we can defend is worth more than a larger one we cannot, and the gap between the two curves is itself the most interesting finding in the whole dataset.

Output: `evidence/overlap.py` (regenerates everything from scratch), `evidence/overlap.json` (raw numbers), `evidence/overlap.md` (the table plus methodology).

### 2. What is an "agent" in this corpus?

`source.metadata.submission` on the sample record reads `20260213_mini-v2.0.0a0_claude-4-5-sonnet`. If the 3-26 attempts on a given instance are **different submissions** — different models or scaffolds — then this is cross-*system* overlap, not N runs of one agent fanned out.

That distinction changes what the number is allowed to claim. Cross-system overlap is arguably a stronger result, since it means the redundancy survives changing the model. But our demo is N instances of one agent, so if the corpus measures something different we have to say so.

Count distinct `submission` values per instance and report the answer plainly in `evidence/claims.md`. Do this before anyone puts 26.5% on a slide.

### 3. Settle the durations question

Confirm no per-step timing exists. Then write the honest note: hit counts are measured, per-class seconds are modelled with the constants above, and the live counter in the demo uses real measured durations from the fleet run instead. Regenerate the value table from `seed/value.py` with the modelled label attached, into `evidence/value.md`.

If you find any timing signal in the corpus that I missed, that changes the plan — say so immediately and loudly.

### 4. Second corpus — gated

`experiential-labs/wmo-terminal-tasks-traces` on HuggingFace. Apache-2.0, 6.01 MB, real terminal-bench agent runs where every transition is a real tool call paired with the true environment observation. An independent corpus reproducing our overlap decay would be a serious credibility win.

**Spend five minutes on the gate before anything else:** our statistic is cross-agent overlap *within* a task. If this corpus is one agent per task, it cannot reproduce it, and you should document that in two sentences and move on rather than forcing a different statistic and pretending it is the same one.

Two traps already found. Their own dataset card's download snippet says `wmh-terminal-tasks-traces` while the repo is `wmo-`. And the HF dataset viewer is broken with a `CastError` because the repo mixes router-optimizer JSON (`winner_spec`, `scores`) in with the traces, so `load_dataset` fails — use `hf_hub_download` to fetch the single `traces.otel.jsonl`.

### 5. Transition-corpus emission spec

Hindsight records `(tree_before, cmd, tree_after, rc, duration)` for every command every agent runs. That is a transition corpus, generated for free as a byproduct of the cache. It is the phase-2 payload and it is worth writing down now while it is cheap to change the schema.

The intended consumer is `/Users/tomjeong/hacker/skunk-works/tools/yc_demo/train_transition_world_model.py`. Read it and write `evidence/transition-spec.md` covering what an adapter would have to do. Four constraints already found:

- Input is a directory of pretty-printed JSON, one file per trajectory — not JSONL.
- `_validate_dataset_binding` (line ~500) hard-fails unless the record's `source` matches a pinned dataset identity: dataset name, revision, fingerprint, row count, and row-index bounds, for two datasets. Constants are at lines 59-66. A new corpus requires forking those constants.
- A tree hash cannot reconstruct a delta. The `any_named_path_movement` and line-bucket labels still need `git diff --numstat HEAD` and `git status --porcelain=v1 -uall`. The tree hash is an additional finer-grained bit, not a replacement for the numstat map.
- No trainer consumes stdout; there is no output field in the step schema. Emitting stdout serves the cache, not the trainer.

Also record the leakage rule we are adopting from Experiential Labs (Apache-2.0, with attribution): only a real action with a subsequently observed response becomes a retrieval transition; generated predictions, simulator rollouts, teacher data and judgments cannot enter the index. It governs this corpus too.

## Rules for how you report

The negatives are the credibility on this project. A number that shrinks under scrutiny but survives it beats a number that impresses and then collapses when someone opens the script.

- Every figure in `evidence/` must be regenerable by running one committed script against a path on disk. If it cannot be regenerated, it does not go in.
- State the denominator every time. "26.5%" alone is meaningless; "26.5% of 729 command slots across 25 multi-agent tasks, deduplicated per agent" is a claim.
- Never mix the deduplicated and raw counts on the same table. Pick one, label it.
- If a task above turns out to rest on a false premise, write down that it did and why. That is a result, not a failure, and it is worth more than a forced number.

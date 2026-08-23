# Hindsight — a build cache for coding agents

Hindsight makes N parallel coding agents pay once, in total, for work that all N would otherwise do independently. It is a cache, not a model. Nothing in it predicts anything.

## Problem

Fan out five coding agents on one task, in five git worktrees, and each one independently installs the same dependencies, runs the same failing test to reproduce the bug, and reads the same files. Nothing tells agent five that agent two already paid for it. In a large repo with a slow test suite, that duplicated work is the dominant cost of running agents in parallel.

The redundancy is structural rather than incidental: agents given the same task start from the same state and therefore start with the same moves, and they only diverge as they learn. Measured on a corpus of replayed SWE-bench attempts, cross-agent command overlap is 26.5% across the first three commands and decays to 11.0% after step ten. The opening — the most redundant stretch — is also the stretch where every agent is doing it simultaneously.

## What it does

A PreToolUse hook intercepts every shell command an agent runs. It keys the command on git's own Merkle hash of the live worktree plus a fingerprint of the environment, asks a shared daemon whether that exact command has already been run at that exact state, and if so rewrites the command to replay the recorded result instead of executing it.

A served result is a verified replay of something that really happened. The failure mode is a cache miss, never a wrong answer.

The pitch is a subtraction: execution-seconds deleted, work paid for once and provably not paid for again.

## Principles

1. **Default is PASSTHROUGH.** Anything unmatched, unparseable, or uncertain runs normally. A classification bug costs a hit, never correctness.
2. **The key must dominate the output.** Anything that can change what a command prints is in the key, or the command is not served.
3. **Share the map, not the route.** Agents inherit what is *here* — what the tests say, what the files contain. They never inherit each other's fix. Deduplicating mutations would collapse the very search you paid for five agents to run.
4. **Effects, not calls.** Cache what the environment returned. Never cache what a model produced.
5. **Abstention over guessing.** Missing artifact, unknown command, stale environment: all misses.
6. **Verify what you serve.** Re-execute served results, diff, evict on divergence, keep the counter visible.

Adopted from Experiential Labs (Apache-2.0, with attribution), because they state it better than we would: *only a real action with a subsequently observed response becomes a retrieval transition; generated predictions, simulator rollouts, teacher data and judgments cannot enter this index.*

## Frozen contracts

Duplicated in `AGENTS.md`, which is what the coding agents read.

### Policy

Exactly three values. The word "deny" is never used: in Codex, `permissionDecision: "deny"` blocks the tool call outright, and an agent that cannot run `curl` is broken.

```go
type Policy int

const (
    PASSTHROUGH Policy = iota // run normally, record nothing
    RECORD_ONLY               // run normally, never serve, record for the corpus
    SERVE                     // eligible to be served, subject to the purity gate
)

func Classify(cmd string) (Policy, string)
func Normalize(b []byte, root, home string) []byte
```

`Classify` is a pure function of the command string. It never consults the filesystem, the daemon, or the clock.

### Key

```
key = sha256(hs-v1 \0 tree \0 env_fp \0 cwd_rel \0 cmd_norm)
```

### Log record

One JSON object per line in `$HP_HOME/log.jsonl`, appended only by the daemon.

```json
{"v":1,"ts":0.0,"agent":"a3","cmd":"","cmd_norm":"","cwd_rel":"",
 "tree_before":"","env_fp_before":"","tree_after":"","env_fp_after":"",
 "key":"hs-v1:","policy":"SERVE","reason":"","decision":"HIT",
 "servable":false,"exit_code":0,"duration_ms":0,
 "stdout_blob":"sha256:","stderr_blob":"sha256:","source_agent":"","verified":null}
```

`decision` is one of `HIT`, `MISS`, `LEASE_WAIT`, `PASSTHROUGH`, `VERIFY`.

## Key derivation

**Tree.** `git add -A` into a side index, then `git write-tree`. This is git's own content hash of the live worktree, including uncommitted and untracked work. Measured at 36 ms warm on this repo. The persistent index is what makes that number possible; a throwaway index costs 220 ms to 1.7 s.

The side index must live in the per-worktree git dir, resolved with `git rev-parse --absolute-git-dir`. Hardcoding `.git/hp-index` is wrong twice: in a linked worktree `.git` is a file rather than a directory, and if worktrees shared one index file their concurrent `git add -A` calls would interleave and produce a tree describing no real state. A corrupt tree is a wrong key, and a wrong key is a wrong answer.

Computing a key never touches `.git/index`, so `git status` is unaffected. There is a test for this.

**Environment fingerprint.** The tree hash structurally cannot see gitignored files, and the most important thing it cannot see is the virtualenv. Two worktrees with byte-identical trees and different installed packages would otherwise share a key, and the cache would serve one environment's output into the other. That is the single hole that can make Hindsight wrong, so the fingerprint covers interpreter version, architecture, `pyvenv.cfg`, and the sorted list of installed distribution directories. It is a readdir, about 1 ms — deliberately not `pip freeze`, which would cost 300 ms on every command.

Environment variables are an allowlist, never the whole environment, and their *values* are scrubbed of the worktree root and home directory before hashing. Harnesses inject per-session variables that differ for every agent by construction; hashing those raw would give every agent a distinct key and reduce sharing to zero.

**Working directory** is included, relative to the worktree root. The same command means different things in different directories, and the absolute path differs per worktree and must not enter the key.

## The purity gate

This is the central design decision, and it replaces a static table of command classes.

Record the state before and after every command. A record is servable if and only if both the tree hash and the environment fingerprint are unchanged:

```
servable  ⟺  tree_after == tree_before  ∧  env_fp_after == env_fp_before
```

Purity is measured, not declared. That matters because the static approach gets a predictable set of cases wrong:

- `tsc` emits `.js` by default. It looks like a read and is not.
- `cargo test` writes `target/`; `go test -c` writes a binary.
- `uv sync` and `pip install` mutate a virtualenv that the tree hash cannot see at all — but the environment fingerprint can, so installs fall to unservable automatically rather than because someone remembered to list them.

Cost is two extra hashes, about 40 ms per command.

The classifier is therefore reduced to two jobs it genuinely cannot delegate:

1. **The non-hermeticity list.** `date`, `curl`, `$RANDOM`, `git push`, `uuidgen`, `hostname`, command substitution, piping into a shell. These are pure *by state* and still wrong to serve, because state hashing is blind to nondeterminism. This list is correctness, not polish, and it is never dropped for time.
2. A cheap pre-filter so we do not record noise.

Chain rule: split on `&&`, `||`, `;`, `|`, and the strictest segment wins. `ls && curl example.com` is PASSTHROUGH.

## Single-flight

A cold fan-out gets nothing from a naive cache. Five agents launched at once run the same first command simultaneously, so none of them has a peer's result to serve yet and all five pay in full — precisely across the opening stretch where overlap is highest.

The daemon therefore keeps an in-flight lease per key. The first caller to miss takes the lease and is told to execute. Subsequent callers for the same key block until the holder publishes its result, then are served it. If the holder dies, the lease times out and the waiters fall through and execute rather than hanging.

Measured on a five-worktree cold fan-out where all five agents run the same expensive command simultaneously: **one execution, four served.**

## Architecture

```
cmd/hindsight/         hook | key | record | daemon | verify | stats | init
internal/hp/
  key.go       tree hash, env fingerprint, key derivation
  policy.go    the three-value classifier and the non-hermeticity list
  norm.go      output normalization for verification
  store.go     append-only JSONL log, content-addressed blobs, memory index
  hook.go      PreToolUse payload parsing and response envelopes
  record.go    process-group-bounded execution with separate stream capture
  daemon.go    lookup, record, verify, leases, SSE, stats
  client.go    fail-soft daemon client
web/viewer.html        live counter, two clocks, verification badge
scripts/fleet.sh       N worktrees, N agents, baseline and cached arms
```

Storage is an append-only JSONL log plus content-addressed blob files, with an in-memory index rebuilt by scanning the log at startup. No SQLite: no cgo, trivially inspectable during a demo, and about forty lines instead of two hundred. Blobs are written with an atomic rename, so concurrent agents producing identical output is safe.

The cache root lives outside the workspace. Blobs written inside the tree would change the tree hash that keys the cache, which is a feedback loop that poisons every subsequent key.

Dependencies: the Go standard library, and nothing else.

## Harness integration

The hook is byte-compatible with neither harness by accident, so it targets the strict intersection. Codex's own source says it "intentionally diverges from Claude's public hook docs," and it applies `deny_unknown_fields` to the wire structs. Concretely, Codex rejects `permissionDecision: "ask"`, rejects `allow` without `updatedInput`, and rejects `updatedInput` without `allow`. There is no plain approve verb, so **the only way to let a command run untouched is to emit nothing at all.**

Other things that each cost an hour if unknown:

- Codex's default hook timeout is 600 seconds. We set it explicitly to 10.
- Exactly one JSON object may reach stdout. Output *before* it makes the hook a silent no-op; output *after* it fails loudly. All diagnostics go to stderr.
- Headless runs need `codex exec --dangerously-bypass-hook-trust`.
- Hooks fail open, run unsandboxed, and execute under a login shell.
- Codex file edits arrive as `apply_patch`, not `Bash`, so a Bash-matcher hook never sees them. This is harmless here: the key is state-based, so mutations we never observed are still reflected in the next tree hash.

## Failure modes and guards

| Failure | Guard |
|---|---|
| Hook breaks the dev loop it is being built in | `HP_ENABLE` kill switch, default off; only `fleet.sh` sets it |
| Malformed payload, dead daemon, timeout | Every path passes through; verified for all three |
| Two hooks race on one worktree's index | Bounded retry on index lock rather than silent passthrough |
| Command dirties the tree | Purity gate marks it unservable |
| Command mutates a gitignored venv | Environment fingerprint catches what the tree hash cannot |
| Truncated or timed-out output | Never marked servable |
| Served result is subtly wrong | Shadow verification, normalized diff, auto-eviction, loud counter |
| Blob torn by a concurrent writer | Content-addressed, write-then-rename |
| Slow viewer | Broadcast drops rather than blocking the cache |

Shadow verification runs agent-side, because the daemon has no worktree and a replay only means anything in the state it was recorded in. Records whose workspace has moved on are skipped rather than judged. The diff is on *normalized* output: test runners print durations, temp paths and per-worktree absolute paths that legitimately differ between two correct runs, so a raw byte-diff would report divergence on nearly every hit and the counter would be worthless. Both verdicts are reported, so a byte-identical match stays visible.

Verified against a deliberately poisoned blob: detected, reported as `CACHE_MISMATCH`, evicted, nonzero exit.

## Measured — real agents

Five Claude Code agents, five git worktrees, launched simultaneously on one repo with a genuine failing test suite, told to install dependencies, reproduce the failure, fix it, and re-test. Both arms run the hook and record identically; the only variable is whether hits are served.

| | baseline | cached |
|---|---|---|
| hook-visible commands | 15 | 15 |
| executed | 15 | 7 |
| served | 0 | 8 (5 coalesced in flight) |
| execution-seconds | 50.1s | 11.7s |
| hit rate | 0% | 53.3% |
| wall clock | 30.5s | 31.8s |

**77% of execution-seconds deleted.** The cached arm's counterfactual is 47.3s against a measured baseline of 50.1s, a 5.6% gap.

The decision trace is the clearest statement of what the system does:

```
a1..a5  MISS        ~0.5s each  RECORD_ONLY  uv sync --extra dev
a2      MISS         4634ms     SERVE        uv run pytest -q
a1,a4,a5 LEASE_WAIT  4634ms     from a2      uv run pytest -q
a3      HIT          4634ms     from a2      uv run pytest -q
a2      MISS         4278ms     SERVE        uv run pytest -q   (post-fix)
a1,a5   LEASE_WAIT   4278ms     from a2      uv run pytest -q
a3,a4   HIT          4278ms     from a2      uv run pytest -q
```

Both test rounds collapse from five executions to one. Installs correctly never enter the lease path and run in parallel.

The second round is worth dwelling on: all five agents independently produced a byte-identical fix, which means a byte-identical tree, which means they share the post-fix cache entry automatically. Nobody copied anybody's patch. This is principle 3 working as intended — the consequences of an edit are shared once two agents have independently agreed on it, while the edit itself is never transferred.

### Four things this experiment does not show

- **Wall clock did not improve** (30.5s to 31.8s). At this scale the bottleneck is model latency, not execution, so deleted execution-seconds do not become elapsed time. The counter measures the right thing and it is not a speedup claim. Execution only dominates wall clock when the suite is minutes rather than seconds.
- **The sample is 15 commands.** Modern agents use native Read and Edit tools for file work, so only three Bash commands per agent reach the hook. That is favourable — what remains is almost entirely the expensive class — but it is a small n.
- **Round-two sharing depends on convergent fixes.** This bug has one obvious correct patch. A bug with several valid fixes would leave agents at different trees and the post-fix hits would disappear.
- **The install pool is still invisible.** `uv` resolves from a warm global cache in under a second here. On a cold cache or with pip, install is the largest single pool, and it is also the case single-flight handles best.

## Measured — synthetic control

The same harness driven by a fixed command script rather than a model, which removes run-to-run variance so the arms are exactly comparable.

| | baseline | cached |
|---|---|---|
| commands demanded | 35 | 35 |
| executed | 35 | 6 |
| served | 0 | 29 (24 coalesced in flight) |
| execution-seconds | 34.1s | 3.0s |
| hit rate | 0% | 82.9% |

Two caveats, both of which make the headline number *conservative* rather than flattering:

The cached arm's internal counterfactual is 26.7s, against a measured baseline of 34.1s. The gap is CPU contention: the baseline runs five expensive commands concurrently and each one is slowed by the others, while the cached arm executes one at a time and therefore records an uncontended duration. Pricing avoided work at uncontended speed understates what was actually avoided, so the counterfactual is a lower bound.

On a lighter workload where contention is negligible, the two reconcile closely: 25.0s counterfactual against a 25.3s measured baseline, a 1.2% gap. That agreement between two independent measurements is the number worth trusting.

### What shadow verification found

On the first real fleet run, verification flagged `ls -la` as divergent. That was not a harness artifact. Long listings print mtimes, sizes and ownership, and git's tree hash deliberately covers none of them, so two worktrees with byte-identical trees legitimately produce different output. The key did not dominate the output, which is a principle 2 violation.

It is worth being precise about why nothing else would have caught it. The command mutates nothing, so the purity gate sees identical state before and after and is satisfied. It is not non-deterministic in the `date` or `$RANDOM` sense, so nothing about it looks suspicious. Only re-executing it and comparing surfaced the problem. `ls -l`, `stat`, `du` and `df` are now passthrough; plain `ls` prints names only and remains serveable.

This is the argument for keeping verification in the product rather than treating it as a test: the deny-list will always be incomplete, and this is the mechanism that tells you where.

## Roadmap: other tool classes

The hook mechanism generalizes, but the interesting constraint is not "which tools" — it is **what state determines the output**. Shell commands are keyed on workspace state. Everything else answers that question differently.

**Code edits: deliberately refused.** Caching `apply_patch` or `Edit` would violate principle 3 head-on. An edit is a decision, not an observation — model output, not an environment response — and letting agent two inherit agent one's patch collapses five independent searches into one, destroying the reason to fan out at all. It also has nothing to capture: edits cost milliseconds, and the value is concentrated in slow things.

We get the useful half for free anyway. Because the key is state-based rather than history-based, an unobserved edit simply appears in the next tree hash; that is why the Bash-only matcher is sufficient even though Codex routes edits through `apply_patch`. And if two agents independently converge on an identical tree, they immediately share every downstream cache entry — the consequences of an edit are shared once two agents have agreed on it, without the patch itself ever being copied.

**Read-only MCP tools: the most promising extension.** Schema listings, doc searches, SELECT queries and issue fetches are slow, network-bound and heavily repeated across a fleet. Since value tracks latency, this is plausibly the second-largest pool after test suites.

The obstacle is that the determining state is remote. `list_tables` depends on a database schema the tree hash knows nothing about, so principle 2 fails by default. An MCP read is a verified replay only when the server exposes a version token — schema hash, ETag, snapshot id — that can enter the key. Where none exists, the honest fallback is session-scoped memoization: key on `(server, tool, args)` with no state component and label the guarantee as "identical within this run" rather than claiming verified replay. Weaker, clearly stated, still turns five agents' doc searches into one.

**Mutating MCP tools: passthrough forever.** Migrations, deploys, Slack posts. Note that `execute_sql` cannot be classified from the tool name at all; distinguishing a SELECT requires parsing the statement. Same shape as `sed -n` versus `sed -i`, same answer: parse conservatively, default to passthrough.

Concretely, the refactor this implies is small: `Classify(cmd string) (Policy, string)` becomes `Classify(tool string, input) (Policy, KeyScope, string)`, where the scope names which state must enter the key — workspace, session, or unservable.

## Dependency scoping — designed, not built

Whole-tree keying is coarse. If agent one edits `src/auth.py`, agent two's `pytest tests/test_billing.py` misses even though nothing it reads changed.

- **Tier 0 — exact tree match.** Always sound. This is what ships.
- **Tier 1 — diff-disjoint.** Treat a tree-key match as a candidate, then run `git diff-tree --name-only <recorded> <current>`; if the changed paths are disjoint from the command's literal path arguments, promote to a hit. Cheap, because both trees are real git objects in a shared store. Designed, not built.
- **Tier 2 — observed read sets.** Record which files a command actually opened and key on those. Designed, not built.

A wrong scope is a wrong answer, so Tier 1 applies only to literal path arguments and Tier 2 only to genuinely observed reads. Never an inferred-but-unobserved scope.

This is worth distinguishing from similarity matching. The projection is not "these two states look alike," it is "the difference between these two states provably cannot affect this command." That is how Bazel dodges the same trade-off, and it is why dependency scoping is not the same gamble as fuzzy state matching.

For Tier 2 the hard part is observation, not modelling. On macOS `fs_usage` needs root and `DYLD_INSERT_LIBRARIES` is blocked by SIP. But for the command classes that carry almost all the value, the language toolchain already knows: `coverage.py` for Python, `go list -deps`, `jest --findRelatedTests`, the cargo unit graph. Four adapters, not a kernel problem.

## Phase 2

Every command every agent runs produces `(tree_before, cmd, tree_after, rc, duration)`. That is a transition corpus, generated for free as a byproduct of caching. It is the thing a transition world model would need, and the cache is the data engine that produces it.

Two constraints already established for any consumer: a tree hash cannot reconstruct a delta, so line-bucket and path-movement labels still need `git diff --numstat` and `git status --porcelain`; and no trainer consumes stdout, so emitting output serves the cache, not the model.

The leakage rule quoted above governs this corpus too.

## What we do not claim

The negatives are the credibility.

- **We do not claim "N× faster" without the measured baseline arm.** The control runs with the hook enabled and serving disabled, so both arms carry identical instrumentation and the only variable is the serve decision. A control measured differently from the treatment is not a control.
- **We do not claim it prevents mistakes.** It deletes duplicated work. That is all.
- **We do not predict anything.** There is no model in the system, and that is the cleanest defence available.
- **The multi-developer case is untested.** The corpus is independent attempts at one task.
- **The corpus is a partial run.** 265 records against a manifest describing 736, roughly a third.
- **11,687 is a deduplicated command count.** Raw is 12,806. Pick one, say which, never mix them.
- **The per-class seconds in the value table are modelled, not measured.** Hit counts are real; the seconds come from hardcoded per-class costs. The live counter uses real durations from the fleet run instead.
- **We do not claim a wall-clock speedup.** The measured fan-out deleted 77% of execution-seconds and moved wall clock by nothing, because model latency dominated. Those are different quantities and we only claim the first.
- **Published overlap figures match on the command string alone**, and they overstate what we can serve. Keyed the way Hindsight actually keys, on `(state, command)`, the same corpus gives 7.5% avoidable, of which only 3.6% is cross-agent — the rest is agents repeating themselves. Against 16.6% for command-string matching. The honest decay is 16.9% across the opening three commands, falling to 1.0% after step 50.
- **That corpus measures a different population than we target.** All 25 tasks mix different models (haiku, sonnet, opus) rather than N instances of one agent, and it starts from a pre-built container so setup is nearly absent. Both make 3.6% a floor rather than an estimate, which the 53.3% measured on a real homogeneous fan-out bears out. But the gap between those two numbers is large and we do not have a principled interpolation between them.
- **Hindsight shares observations, never decisions.** Route-sharing between agents is deliberately out of scope. The transition corpus is what would make it approachable later; this is the foundation for that idea, not a retreat from it.

## Prior art

Experiential Labs open-sourced trace-keyed retrieval for agent environments in June and now ship a model gateway instead. Theirs replays one agent's environment offline so evals are cheap, and it *predicts* the environment response. Hindsight shares state across N agents live, keyed on git's own tree hash, and it *quotes*.

Their most useful contribution to this document is a negative result they made structural: they built a world-model fidelity metric and then forbade it, in code, from gating any decision — `optimizer.py:422` raises rather than let a fidelity cell into an evaluation plan. That is a revealed preference from a team motivated to conclude the opposite, and it is the strongest available argument for quoting instead of generating.

We do not describe this as turning your codebase into a world model. It is a build cache.

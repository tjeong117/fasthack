# Hindsight

A build cache for coding agents. When you fan out N agents on one task, Hindsight makes them pay once, in total, for the work all N would otherwise do independently.

It is a cache, not a model. Nothing in it predicts anything: a served result is a replay of a command that really ran, so the failure mode is a cache miss, never a wrong answer. It replays the recorded bytes exactly; when re-checking a served result against a fresh execution we compare *normalized* output, because test runners print durations and temp paths that legitimately differ between two correct runs.

## The problem

Launch five agents on one bug in five git worktrees. Each one installs the same dependencies, runs the same failing test to reproduce it, and reads the same files. Nothing tells agent five that agent two already paid for all of it.

The redundancy is structural, not accidental. Agents given the same task start from the same state, so they start with the same moves, and they only diverge as they learn. The opening stretch is both the most redundant and the one where every agent is doing it simultaneously.

## Quickstart

```bash
go build ./cmd/hindsight

./hindsight doctor                    # can this workspace be cached safely?
./hindsight init                      # install the PreToolUse hook
./hindsight doctor --ensure-daemon    # start the shared daemon

HP_ENABLE=1 claude          # or: HP_ENABLE=1 codex exec --dangerously-bypass-hook-trust
```

`doctor` is the first thing to run and the first thing to run again when something looks wrong. It checks that the tree hash works and how much it costs, that every dependency ecosystem in the repo is covered by the fingerprint, that the cache lives outside the worktree, and that the hook is installed and points at a binary that exists. It exits non-zero if any of that fails, and `--json` gives the same answer to a script.

**The hook is inert unless `HP_ENABLE=1` is set.** That is deliberate: this repo installs a hook into its own config, so an armed hook would intercept the very session you use to work on the hook.

| Variable | Default | Meaning |
|---|---|---|
| `HP_ENABLE` | unset | `1` arms the hook. Nothing happens without it. |
| `HP_SERVE` | `1` | `0` records everything and serves nothing — the control arm. |
| `HP_DAEMON` | `http://127.0.0.1:7777` | Shared daemon. |
| `HP_HOME` | `~/.hindsight/<repo-id>` | Cache root. Must be outside the worktree. |
| `HP_AGENT` | `local` | Agent id, used for provenance on served results. |
| `HP_DEBUG` | unset | `1` sends diagnostics to stderr. Never to stdout. |

## How it works

A PreToolUse hook intercepts every shell command an agent is about to run, derives a key, and asks the daemon whether that exact command has already been run at that exact state. If it has, the hook rewrites the command to replay the recorded output instead of executing it.

**The key.**

```
key = sha256(hs-v1 \0 tree \0 env_fp \0 cwd_rel \0 cmd_norm)
```

`tree` is git's own Merkle hash of the live worktree — `git add -A` into a side index, then `git write-tree` — so it covers uncommitted and untracked work. It is about 36 ms warm on this repo, and the persistent side index is what makes that true; a throwaway index costs 220 ms to 1.7 s. The side index lives in the per-worktree git dir, because worktrees sharing one index would interleave their `git add` calls and produce a tree describing no real state.

`env_fp` covers what the tree hash structurally cannot see: installed dependencies, which every ecosystem hides in a gitignored directory. Two worktrees with byte-identical trees and different installed packages must not share a key — that is the single hole that could make Hindsight wrong. The fingerprint is a directory listing, about 1 ms, deliberately not `pip freeze`. If an ecosystem is in use but its state cannot be established, the workspace is marked incomplete and nothing is served at all.

**The purity gate.** State is recorded before and after every command, and a record is servable only if both halves are unchanged:

```
servable  ⟺  tree_after == tree_before  ∧  env_fp_after == env_fp_before
```

Purity is measured, not declared, which is why this catches things a static table of "read" commands gets wrong: `tsc` emits `.js`, `cargo test` writes `target/`, and `uv sync` mutates a virtualenv the tree hash cannot see but the fingerprint can. Installs fall out as unservable automatically, rather than because somebody remembered to list them.

**Single-flight.** A cold fan-out gets nothing from a naive cache: five agents launched at once run the same first command simultaneously and all five pay. The daemon keeps an in-flight lease per key, so the first caller executes and the rest block and are served its result. If the holder dies the lease expires and the waiters execute normally.

## What it refuses to cache

| Policy | Meaning |
|---|---|
| `SERVE` | Eligible to be replayed, subject to the purity gate. |
| `RECORD_ONLY` | Runs normally, recorded for the corpus, never served. |
| `PASSTHROUGH` | Runs normally, records nothing. The default for anything unrecognized. |

There is no "deny". In Codex, `permissionDecision: "deny"` blocks the tool call outright, and an agent that cannot run `curl` is broken.

Four things are refused on purpose:

- **Non-hermetic commands.** `date`, `curl`, `$RANDOM`, `git push`, `uuidgen`, `hostname`, command substitution, piping into a shell. These are pure *by state* and still wrong to serve, because state hashing is blind to nondeterminism. This list is correctness, not polish.
- **File metadata.** `ls -l`, `stat`, `du`, `df` print mtimes, sizes and ownership that git's tree hash deliberately does not cover, so two identical trees legitimately disagree. Plain `ls` prints names only and stays servable.
- **Anything that dirties the workspace**, caught by measurement rather than by a list.
- **Code edits.** An edit is a decision, not an observation. Letting agent two inherit agent one's patch collapses five independent searches into one and destroys the reason to fan out. Consequences are shared, never the patch: if two agents independently converge on an identical tree, they share every downstream cache entry for free.

Chains split on `&&`, `||`, `;` and `|`, and the strictest segment wins, so `ls && curl example.com` is passthrough.

## How do I know it isn't lying to me?

Because you can make it prove it. `hindsight verify` re-executes commands the cache is willing to serve and diffs the result against what would have been served, evicting anything that disagrees.

The diff is on *normalized* output. Test runners print durations, temp paths and per-worktree absolute paths that legitimately differ between two correct runs, so a raw byte-diff would report a divergence on nearly every hit and the counter would be worthless. Both verdicts are reported, so a byte-identical match stays visible.

This is a product feature rather than a test because it finds real bugs. On the first live fleet run it flagged `ls -la` as divergent — a command that mutates nothing, so the purity gate was satisfied, and which is not random, so nothing about it looked suspicious. Only re-executing it and comparing surfaced the problem. The deny-list will always be incomplete; this is the mechanism that tells you where.

## Principles, held as invariants

1. **Default is PASSTHROUGH.** Anything unmatched, unparseable, or uncertain runs normally. A classification bug costs a hit, never correctness.
2. **The key must dominate the output.** Anything that can change what a command prints is in the key, or the command is not served.
3. **Share the map, not the route.** Agents inherit what is *here*. They never inherit each other's fix.
4. **Effects, not calls.** Cache what the environment returned. Never cache what a model produced.
5. **Abstention over guessing.** Missing artifact, unknown command, stale environment: all misses.
6. **Verify what you serve.** Re-execute, diff, evict on divergence, keep the counter visible.

The leakage rule that principle 4 encodes is adopted from Experiential Labs (Apache-2.0, with attribution), because they state it better than we would: *only a real action with a subsequently observed response becomes a retrieval transition; generated predictions, simulator rollouts, teacher data and judgments cannot enter this index.*

## Measured

Five Claude Code agents, five git worktrees, one repo with a genuine failing test suite, launched simultaneously and told to install dependencies, reproduce the failure, fix it, and re-test. Both arms run the hook and record identically; the only variable is whether hits are served.

| | baseline | cached |
|---|---|---|
| hook-visible commands | 15 | 15 |
| executed | 15 | 7 |
| served | 0 | 8 (5 coalesced in flight) |
| execution-seconds | 50.1s | 11.7s |
| hit rate | 0% | 53.3% |
| wall clock | 30.5s | 31.8s |

**77% of execution-seconds deleted.** Read that number with three caveats, all of which are ours:

- **Wall clock did not improve** — 30.5s to 31.8s. At this scale the bottleneck is model latency, not execution, so deleted execution-seconds did not become elapsed time. These are different quantities and we only claim the first. Execution dominates wall clock when the suite takes minutes, not seconds.
- **The sample is 15 commands, one task, one machine.** Modern agents use native Read and Edit tools for file work, so only about three Bash commands per agent reach the hook at all. What remains is almost entirely the expensive class, which is favourable, but n is small and one run is not a distribution.
- **Round-two sharing depended on convergent fixes.** All five agents produced a byte-identical patch, so they shared the post-fix cache entry. A bug with several valid fixes would leave them at different trees and those hits would disappear.

We do not claim a speedup, and we do not claim it prevents mistakes. It deletes duplicated work. `design_doc.md` has the full set of things we do not claim, and it is worth reading before quoting any number here.

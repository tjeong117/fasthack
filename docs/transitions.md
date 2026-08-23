# `hindsight transitions` — the transition corpus

Hindsight is a build cache. To decide whether a command is safe to serve, it
already measures the workspace state before the command runs and again after it
exits. That means every recorded execution carries
`(tree_before, cmd, tree_after, exit_code, duration_ms)` whether anyone wants it
or not — a real state transition, observed rather than predicted, produced as a
byproduct of caching.

`hindsight transitions` writes that out. It generates nothing. It selects the subset
of `log.jsonl` that is honestly a transition, drops the rest, and says exactly
what it dropped and why.

```bash
hindsight transitions --stats                              # summary, export nothing
hindsight transitions > transitions.jsonl                  # one transition per line
hindsight transitions --format json --out corpus.json      # metadata header + array
hindsight transitions --mutating-only --stats
```

## The leakage rule

Adopted from Experiential Labs (Apache-2.0), with attribution, and already
invariant 6 in `AGENTS.md`:

> only a real action with a subsequently observed response becomes a retrieval
> transition; generated predictions, simulator rollouts, teacher data and
> judgments cannot enter this index.

**It is enforced in code, not in this document.** `hp.TransitionFrom` admits a
record only when `decision == "MISS"`. That is the single path on which
`hindsight record` actually forked the command, captured its exit code and
duration, and recomputed the tree hash and environment fingerprint afterwards.
Every other decision is excluded:

| Decision | Why it is not a transition |
|---|---|
| `MISS` | **Kept.** The command ran and both states were measured. |
| `HIT` | A replay served from the cache. Nothing executed; the fields are quoted from a peer's earlier observation. |
| `LEASE_WAIT` | Served from a peer's in-flight execution. Same quotation, arrived by a different route. |
| `PASSTHROUGH` | Ran unintercepted, so no state was measured around it. |
| `VERIFY` | A verdict *about* a served record, written by the verifier, not an execution. |
| anything else | Excluded by default, following invariant 1. |

The reason `HIT` is the dangerous one is that it does not look dangerous. A
served hit carries the same command, the same exit code and the same duration
as the execution it was copied from, because that is precisely what a cache
does. If it leaked into the corpus, the file would contain duplicated rows
presented as independent evidence and nothing downstream could tell. On a real
fleet run the cached arm serves more commands than it executes, so this is not
a marginal filter — it is most of the log.

Records that reach the log with no observed after-state are dropped too, under
`no observed after-state`. The action was real but its response was never
measured, which fails the rule and leaves a trainer with a row that has no
successor state.

## Accounting

Every scanned line either becomes a transition or is counted under a reason it
did not. `hp.ScanTransitions` refuses to return a scan where
`scanned != exported + excluded`, so the header can be checked against itself:

```
  records scanned       20
  transitions exported  12
  records excluded      8

  4  decision HIT: a replay served from cache, not an execution
  3  decision VERIFY: a verdict about a served record, not an execution
  1  decision LEASE_WAIT: served from a peer's in-flight execution, not an execution
```

Torn log lines are counted as `malformed` rather than skipped, because a line
the store's index silently ignores is still a real gap in the corpus.

## Schema

Row identity and version: `schema: "hindsight.transition/v1"`. One JSON object
per line in `jsonl`; the same objects in a `transitions` array under `json`.

| Field | Type | Meaning |
|---|---|---|
| `schema` | string | `hindsight.transition/v1`. Version is part of the identifier. |
| `ts` | float | Unix seconds when the record was written. |
| `agent` | string | Which agent observed it (`a1`…`a5` in a fleet run, `local` otherwise). |
| `tree_before` | string | `git write-tree` of the live worktree before the command, via a per-worktree side index. Covers uncommitted and untracked work. |
| `env_fp_before` | string | Fingerprint of interpreter version, architecture and installed distributions before the command. Covers what the tree hash structurally cannot see, above all the virtualenv. |
| `cwd_rel` | string | Working directory relative to the worktree root. Part of the state: the same command means different things in different directories. |
| `cmd` | string | The command as issued, verbatim. |
| `cmd_norm` | string | The normalized form the cache key is derived from. |
| `tree_after` | string | Tree hash measured once the command exited. |
| `env_fp_after` | string | Environment fingerprint measured once the command exited. |
| `exit_code` | int | Observed exit status. |
| `duration_ms` | int | Wall-clock milliseconds, monotonic, process-group bounded. |
| `mutated` | bool | `tree_after != tree_before ∨ env_fp_after != env_fp_before`. |
| `tree_mutated` | bool | The tracked/untracked workspace moved. |
| `env_mutated` | bool | The environment moved. |
| `servable` | bool | The cache's own verdict, passed through unmodified. |
| `policy` | string | `SERVE`, `RECORD_ONLY` or `PASSTHROUGH` — what the classifier said. |
| `reason` | string | The classifier's human reason. Omitted when empty. |
| `key` | string | The cache key, so a row can be audited back to its entry. |
| `stdout_blob`, `stderr_blob` | string | Content hashes. For the cache, not for a model — see limitations. |

### `mutated` is measured, never declared

This is the single most useful label in the file and it costs nothing, because
the purity gate had to compute it anyway. It is derived by comparing the two
states, which is what makes it catch the cases a static command table gets
wrong: `tsc` emits `.js`, `cargo test` writes `target/`, and `go build -o
build.out` puts a binary in the worktree. None of those announce themselves.

`tree_mutated` and `env_mutated` are kept separate because they answer
different questions. `uv sync` and `pip install` leave the tree hash
byte-identical — `.venv` is gitignored, so git cannot see it — and move only
the environment fingerprint. Comparing trees alone would label the most
expensive class of command in the corpus as a no-op.

`servable` implies `!mutated`, but not the reverse. A non-mutating command can
still be unservable because the classifier called it non-hermetic (`date`,
`curl`, `git push`), because its output was truncated or timed out, or because
it ran below the duration floor where caching costs more than it saves.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--home <dir>` | `$HP_HOME`, else `~/.hindsight/<repo-id>/` | Cache root. Resolved exactly as `hindsight daemon` resolves it, then made absolute. |
| `--out <path>` | stdout | Output file. Written to a temporary path and renamed, so a failed export leaves no half-written corpus. |
| `--format jsonl\|json` | `jsonl` | `jsonl` is one transition per line and no header. `json` is a single object with a metadata header and an array. |
| `--include-nonmutating` | `true` | Include transitions that changed nothing. |
| `--mutating-only` | `false` | Only transitions that moved the tree or the environment. Shorthand for `--include-nonmutating=false`. |
| `--stats` | `false` | Print the summary to stderr and export nothing. |

Asking for both populations and then only one of them (`--mutating-only
--include-nonmutating=true`) is refused rather than resolved by precedence.

The two filters partition the corpus, and rows a filter drops are still counted
under `filtered out: …`, so the header keeps adding up. The library also has
`FilterNonMutatingOnly`, which the CLI does not expose because only the two
flags above were specified.

The summary always goes to stderr, including when the corpus is going to
stdout, so piping the data somewhere cannot hide what was left out of it.

## Provenance header (`--format json`)

A bare array of transitions with no statement of where it came from is worth
much less than the same array with one. The `json` format therefore leads with
a `meta` object carrying the schema, the export time, the absolute cache path
and log path, the filter, the full scanned/exported/excluded counts with the
reason breakdown, the leakage rule quoted verbatim with its attribution, a
statement of where in the code it is enforced, and the limitations below.

`jsonl` has no header by design. A header line would be a row that is not a
transition, which is exactly the kind of thing this corpus exists to not
contain. Use `--format json` when provenance has to travel with the data.

## Limitations

Stated plainly, because they are the parts that decide whether this is usable.

**A tree hash is an identity, not a delta.** It tells you the state changed and
uniquely names both states. It cannot tell you which files moved, or by how
many lines. Any consumer wanting path-movement or line-bucket labels still
needs `git diff --numstat` and `git status --porcelain=v1 -uall` between the
two trees. Because both trees are real git objects in a shared object store,
that is cheap and always possible after the fact — but it is a second pass, and
the tree hash is an additional finer-grained bit rather than a replacement for
the numstat map.

**No trainer consumes stdout.** `stdout_blob` and `stderr_blob` are content
hashes into the cache's blob store, kept so a row can be audited against what
was actually served. The bytes are not in the corpus and there is no head in
any consumer that would read them. Emitting command output serves the cache,
not a model.

**The corpus is tiny.** It is only as large as the runs that produced it, and
today those runs are demos. Concretely, at the time of writing:

- The cache root for this repo, `~/.hindsight/<repo-id>/`, has **no
  `log.jsonl` at all** — nothing has been recorded into it yet.
- The end-to-end run used to validate this exporter — three agents plus a
  two-agent race over six commands in a clone of this repo — produced **20
  records, of which 12 were transitions** (6 non-mutating, 6 mutating) and 8
  were excluded.
- The largest real run described in `design_doc.md` is five Claude Code agents
  over five worktrees, and it saw **15 hook-visible commands**, of which 7
  executed in the cached arm. The synthetic control saw 35 demanded and 6
  executed.

Numbers in the low tens are not a dataset. The mechanism is what is being
demonstrated here; the volume is a function of how many fleet runs anyone
bothers to do.

**Rows are per repository.** The cache root is keyed by repo, so a corpus
spanning repositories has to be concatenated deliberately and labelled as such.

**Modern agents route most file work around the hook.** Reads and edits go
through native `Read`/`Edit` tools, and Codex file edits arrive as
`apply_patch` rather than `Bash`. The corpus therefore covers shell commands
only. This is fine for the cache — an unobserved edit still shows up in the
next tree hash — but it means the transitions are biased toward the expensive
tail (test suites, builds, installs) rather than being a representative sample
of everything an agent does.

**Non-mutating rows are over-represented relative to what agents do**, because
the cache's duration floor and classifier keep cheap and non-hermetic commands
out of the log in the first place.

## The intended downstream consumer

The transition world model this corpus was imagined for is
`train_transition_world_model.py` in a separate repository
(`skunk-works/tools/yc_demo/`). It will not read this file as it stands, and
being honest about that is more useful than implying an adapter is trivial.

- **It hard-validates a pinned dataset identity.** `_validate_dataset_binding`
  (line 500) raises `TrainingError` unless every record's `source` and `task`
  match constants frozen at lines 59–66 — dataset name, revision, fingerprint
  and row count for
  `Kwai-Klear/SWE-smith-mini_swe_agent_plus-trajectories-66k` and
  `SWE-bench/SWE-smith` — plus a `row_index` inside `TRAJECTORY_ROWS` and
  sha256-shaped digests. A Hindsight corpus matches none of that. **Accepting
  it requires forking those constants**, which is deliberate on their part:
  the check exists so a model cannot silently be trained on a dataset other
  than the one it claims.
- **The input shape is different.** It walks a directory tree with
  `rglob("*.json")` and expects one pretty-printed JSON object per trajectory,
  each with `status`, `candidate_id` and a `steps` array, verified against an
  index of per-file sha256 digests. Not JSONL, and not flat.
- **Its step schema wants a delta we do not have.** Each step must carry
  `delta` (a path-to-movement map), `diff_lines` and `n_files`, and its heads
  are `return_code_success`, `observed_diffstat_state_changed`,
  `next_observed_file_count`, `any_named_path_movement`, `added_line_delta`
  and `deleted_line_delta`. Everything after the first two needs the numstat
  pass described above; the tree hash alone cannot produce them.
- **It has no head for duration.** `duration_ms` is the one field Hindsight
  measures that the SWE-smith corpus does not have at all — that corpus records
  a single `wall_s` per whole trajectory and no per-command timing. So the
  most distinctive column here is currently unconsumed, which is an argument
  for a new head rather than a defect in the export.

An adapter is therefore: fork the identity constants, group rows by `agent`
into trajectory files, run `git diff --numstat` between each row's
`tree_before` and `tree_after` to synthesize `delta`, `diff_lines` and
`n_files`, and emit pretty-printed JSON with a digest index. That is real work,
and none of it is blocked — but none of it is done.

## Why this is not a world model

The cache does not predict. It quotes. This corpus is what a transition model
would need in order to be trained, and emitting it is not the same as building
one. `design_doc.md` records the reason for the caution: Experiential Labs
built a world-model fidelity metric and then forbade it, in code, from gating
any decision (`optimizer.py:422` raises rather than let a fidelity cell into an
evaluation plan). That is a revealed preference from a team motivated to
conclude the opposite, and it is the strongest available argument for quoting
instead of generating.

Exporting the data is cheap and reversible. Depending on a model trained from
it is neither.

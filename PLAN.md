# Hindsight — finish plan

The frozen contract and file ownership live in [AGENTS.md](AGENTS.md). The current product and measured results live in [design_doc.md](design_doc.md). Arnav's delegated lane is [MY_TASKS.md](MY_TASKS.md).

## Current HEAD — August 23, 2026

This finish plan was rebased onto remote `main` at `9e1fdb9` (`add build cache and doctor support`).

The core implementation is built. Tom has already landed:

- hook, key, record, store, daemon, single-flight, verification, stats, and init;
- policy and normalization integration plus regression fixes;
- baseline/cached fleet orchestration;
- fixture and live-event viewer behavior;
- a synthetic controlled run;
- a real five-agent Claude Code run;
- cheap-command gating, Tier-1 scoping, multi-language fingerprints, replay,
  build-cache adapters, and doctor diagnostics.

Measured real-fleet result at this commit:

| | baseline | cached |
|---|---:|---:|
| Hook-visible commands | 15 | 15 |
| Executed | 15 | 7 |
| Served | 0 | 8 |
| Execution-seconds | 50.1s | 11.7s |
| Hit rate | 0% | 53.3% |
| Wall clock | 30.5s | 31.8s |

The supported claim is **77% of execution-seconds deleted**, not a wall-clock speedup.

## Tom's ownership assignment

The latest `AGENTS.md` assigns the Teammate these paths:

1. `internal/hp/policy.go` and `internal/hp/policy_test.go`
2. `internal/hp/norm.go` and `internal/hp/norm_test.go`
3. `scripts/fleet.sh`
4. `web/viewer.html`

The build plan also assigns the Teammate the evidence pass. Work under `evidence/` must follow [evidence/AGENTS.md](evidence/AGENTS.md).

Tom retains the Go execution core: key, store, hook, record, daemon, CLI, leases, and verification. Do not edit those files to make integration easier; report a failing case to Tom.

## What remains

### P0 — make the published evidence reproducible

The public design document now contains important numbers whose producing artifacts are not yet committed under `evidence/`. That is the highest-priority gap.

Arnav owns producing:

- `evidence/overlap.py`
- `evidence/overlap.json`
- `evidence/overlap.md`
- `evidence/value.md`
- `evidence/claims.md`
- a small fleet-results note or machine-readable summary referencing the real run

The evidence must reproduce or correct these current claims:

- 7.5% state-keyed avoidable work in the corpus;
- 3.6% state-keyed cross-agent reuse;
- 16.9% across the opening three commands;
- 1.0% after step 50;
- all 25 multi-agent tasks mix different model submissions;
- 11,687 commands is deduplicated and 12,806 is raw;
- per-class seconds are modeled, while live fleet seconds are measured.

If the available corpus cannot regenerate a number, remove or qualify the claim instead of reverse-engineering a script to match it.

### P1 — make Arnav's owned demo surfaces stage-ready

1. **Fleet invocation.** Decide with Tom whether the unsupported short form (`scripts/fleet.sh 5 baseline`) should be fixed or removed from the shared docs. The canonical full invocation must include an explicit repo, prompt, output directory outside the target tree, mode, and agent count.
2. **Fleet reproducibility.** Record the exact command, target revision, cache directory policy, and output locations used for the real baseline/cached run. A judge should be able to rerun it.
3. **Viewer live check.** Connect `web/viewer.html` to a daemon run and confirm decisions, provenance, deleted seconds, verification, and divergence state without relying on the embedded fixture.
4. **Demo capture.** Save a screenshot or fallback recording showing the measured run, source-agent provenance, and zero unexplained divergence.
5. **Regression-only code changes.** Modify policy, normalization, fleet, or viewer code only when one of the checks above demonstrates a concrete failure.

### P2 — shared finalization

1. Prove Codex interception in the exact permission profile used for judging; the real measured run used Claude Code.
2. Update shared docs only after Tom agrees on baseline wording and the canonical fleet command.
3. Re-run build, tests, shell syntax checks, and one smoke fleet.
4. Rehearse the three-minute pitch twice.
5. Keep the live claim precise: execution-seconds were deleted; measured wall clock did not improve at this workload size.

## Completion gates

- [x] Work rebased onto remote `main` at `9e1fdb9`.
- [x] Go build passes.
- [x] Go tests pass with loopback binding available.
- [x] Policy and normalization tests exist.
- [x] Fleet dry-run creates and cleans detached worktrees.
- [x] Synthetic baseline/cached experiment recorded.
- [x] Real five-agent baseline/cached experiment recorded.
- [x] Installs excluded from the lease path.
- [ ] Evidence scripts and outputs committed.
- [ ] Real fleet command and raw summary provenance documented.
- [ ] Live viewer checked against the daemon.
- [ ] Codex interception verified in the judging permission profile.
- [ ] Shared fleet command and baseline wording reconciled with `AGENTS.md`.
- [ ] Final screenshots/video and rehearsal complete.

## Cut line

No Claude-Mem, second corpus, new dependency, environment restore, cross-machine transport, or subtree caching until every P0 and P1 item is complete.

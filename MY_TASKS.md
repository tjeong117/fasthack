# MY_TASKS — Arnav / Teammate

This file delegates only the work Tom assigns to the **Teammate** in the latest [AGENTS.md](AGENTS.md) and [PLAN.md](PLAN.md).

## Git state

- Branch: `main`
- Local HEAD: `c7e0843`
- Remote HEAD: `c7e0843`
- `git pull --rebase --autostash origin main`: already up to date

Do not create or checkout another branch. The project contract says both teammates work directly on `main` with disjoint file ownership.

## What Tom assigns to you

| Assigned path/work | Your responsibility |
|---|---|
| `internal/hp/policy.go` + tests | Conservative command classification and chain behavior |
| `internal/hp/norm.go` + tests | Stable output normalization for verification |
| `scripts/fleet.sh` | Baseline/cached fleet harness and measured summaries |
| `web/viewer.html` | Live counters, provenance, verification, and demo presentation |
| Evidence pass from `PLAN.md` | Reproducible overlap/value claims and screenshots |

Tom owns `internal/hp/key.go`, `store.go`, `hook.go`, `record.go`, `daemon.go`, and `cmd/hindsight/main.go`. Do not edit those files. If your test exposes a core problem, hand Tom the exact reproducer.

## Already complete — do not redo

- Policy and normalization are implemented and tested.
- Metadata-producing commands and descriptor-dup redirects have regression coverage.
- Installs are excluded from the single-flight serve path.
- Fleet baseline/cached modes work.
- The viewer supports fixtures and live daemon events.
- A controlled synthetic experiment has been measured.
- A real five-agent experiment has been measured: 53.3% hit rate and 77% of execution-seconds deleted.

Your work starts after implementation, at reproducibility and stage readiness.

## Task 1 — commit the evidence package

Follow [evidence/AGENTS.md](evidence/AGENTS.md). Work inside `evidence/` only for this task.

Create:

- `evidence/overlap.py`
- `evidence/overlap.json`
- `evidence/overlap.md`
- `evidence/value.md`
- `evidence/claims.md`

Required checks:

1. Determine and document what `state_sha256` hashes.
2. Reproduce or correct the state-keyed figures now quoted in `design_doc.md`: 7.5% avoidable, 3.6% cross-agent, 16.9% in the first three commands, and 1.0% after step 50.
3. Count submission identities per task and verify the claim that all 25 multi-agent tasks mix model submissions.
4. Regenerate the value table and label its seconds **modeled**.
5. Keep counts explicit: 11,687 deduplicated; 12,806 raw across multi-agent tasks.
6. Keep the unsupported correlation statistic deleted.

Acceptance:

- Every published figure comes from one committed command and one documented corpus path.
- Every table states its denominator and whether it is measured or modeled.
- If a claim does not regenerate, the evidence file says so and the public claim is queued for correction.

## Task 2 — document the real fleet run

Tom has committed the headline results to `design_doc.md`, but the rerun recipe and raw-result provenance still need a durable home.

Within your fleet/evidence lane, record:

- target repository and revision;
- exact prompt or deterministic workload;
- exact baseline and cached commands;
- agent count and harness;
- cache directories and cold-start procedure;
- raw `summary.json` values or their preserved location;
- why baseline and cached arms differ only by serving;
- the measured limitation that wall clock did not improve.

Do not manufacture missing artifacts. If Tom's raw logs are not in this workspace, ask him for the paths or files and mark the task waiting.

Acceptance: another teammate can reproduce the experiment without guessing hidden flags or environment variables.

## Task 3 — finish the fleet interface

The current shared docs still advertise `scripts/fleet.sh 5 baseline`, but that short form requires a prompt and a safe output directory that it does not supply.

Coordinate one decision with Tom:

- either make the short form truly self-contained; or
- remove it and document the full invocation as canonical.

The full invocation must make these explicit:

- target repo;
- prompt/workload;
- agent count;
- `baseline` or `cached` mode;
- output directory outside the target repo;
- cache directory when reproducibility matters.

Only edit `scripts/fleet.sh` if the chosen interface requires it. `AGENTS.md` is shared, so change its examples only after Tom agrees.

Acceptance: the documented baseline and cached commands both reach dry-run and create/clean their worktrees.

## Task 4 — validate the live viewer

Use a real daemon event stream, not the embedded fixture.

Verify:

- HIT, MISS, LEASE_WAIT, and PASSTHROUGH display correctly;
- each hit identifies `source_agent`;
- execution-seconds deleted use the recorded source duration;
- verified/divergent counts update without double-counting;
- a divergence is visually loud;
- reconnecting mid-run catches up correctly;
- the viewer never blocks daemon work.

Capture one screenshot or fallback recording for the submission.

Acceptance: the viewer can tell the measured five-agent story without manual fixture edits.

## Task 5 — owned-file regression pass

Do not add features. Run the existing verification and change your files only for a reproduced failure.

```sh
GOCACHE=/private/tmp/fasthack-go-cache \
GOTMPDIR=/private/tmp/fasthack-go-tmp \
go test ./...

GOCACHE=/private/tmp/fasthack-go-cache \
GOTMPDIR=/private/tmp/fasthack-go-tmp \
go build ./cmd/hindsight

bash -n scripts/fleet.sh scripts/replay-agent.sh
git diff --check
```

If the failure is in Tom's core, stop at the reproducer and hand it off. Do not cross ownership to fix it yourself.

## Shared final checks — coordinate, do not silently own

- Prove Codex command rewriting in the exact judging permission profile. The measured real run used Claude Code.
- Reconcile `AGENTS.md` saying “hooks off” with the implemented baseline using equal instrumentation and serving disabled.
- Update `design_doc.md` only if evidence regeneration changes a number.
- Rehearse the three-minute demo twice and keep the claim to execution-seconds deleted, not wall-clock speedup.

## Do not work on

- Tom's key/store/hook/record/daemon/CLI files;
- Claude-Mem;
- environment restoration or install caching;
- subtree or cross-machine caching;
- another external corpus before the current evidence is committed;
- expanding policy or normalization without a real failing case;
- new dependencies.

## Immediate order

1. Ask Tom for the raw five-agent run artifacts and exact rerun command.
2. Build the reproducible evidence package.
3. Reconcile and dry-run the canonical fleet command.
4. Validate and capture the live viewer.
5. Run the regression suite, then coordinate the shared docs and rehearsal.

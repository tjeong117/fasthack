# MY_TASKS — Arnav / Teammate

This is the local execution queue for the files assigned to the **Teammate** in
[AGENTS.md](AGENTS.md) and [PLAN.md](PLAN.md). Exactly one task is active. Finish
its acceptance checks before promoting the next backlog item.

## Git state

- Work from `main`, which tracks `origin/main`.
- Pull with rebase immediately before each push.
- Preserve the local untracked notes `CMEM_LANE.md` and `research_notes.md`.
- Commit only files named by the active task.

## Assigned ownership

| Path/work | Responsibility | Current state |
|---|---|---|
| `internal/hp/policy.go` + tests | Conservative classification and chain behavior | Complete; regression fixes only |
| `internal/hp/norm.go` + tests | Stable output normalization for verification | Complete; regression fixes only |
| `scripts/fleet.sh` | Baseline/cached harness and measured summaries | Complete; QA and provenance remain |
| `web/viewer.html` | Live counters, provenance, verification, and presentation | Active |
| Evidence pass | Regenerable overlap/value claims and screenshots | Generator complete; private outputs pending |

Do not edit Tom's key, store, hook, record, daemon, or CLI implementation. If
work in this queue exposes a core issue, record the exact reproducer and hand it
to Tom.

## Completed — Task 1: wire fleet state into the viewer

Completed on `main`. The viewer polls `/agents`, consumes fleet SSE snapshots,
renders state/command/peer context, handles nullable empty Go slices, and keeps
fleet data separate from execution counters. Fixture and live-empty-daemon QA
passed at desktop and compact projector widths.

### Allowed files

- `web/viewer.html`
- `web/fixture.jsonl`
- `MY_TASKS.md` only to record completion and promote Task 2

### Inputs already available

- `GET /agents` returns `FleetView` with `agents`, `clusters`, `converged`,
  `fully_apart`, and `assessment`.
- Calling `/agents` also broadcasts an SSE event shaped as
  `{"type":"fleet","fleet":{...}}`.
- Each agent includes its tree, latest command, command/served/executed counts,
  peers at the same tree, in-flight command, and last-seen timestamp.

### Implementation checklist

1. Fetch `/agents` immediately after a live SSE connection opens, then every
   second while that connection remains active.
2. Accept fleet SSE events and keep the latest snapshot separate from the
   decision counters.
3. Add one compact fleet assessment above the existing lanes.
4. Enrich each lane with its short tree, current command, and same-state peers;
   use fleet snapshot totals after a late connection.
5. Escape every daemon-provided string before inserting it into HTML.
6. Stop the poller on teardown, reconnect, or fixture playback.
7. Treat `/agents` failure as non-fatal; do not abandon a working SSE stream.
8. Add fixture fleet snapshots that demonstrate exploring and convergence in
   both `web/fixture.jsonl` and the standalone embedded fixture.

### Acceptance

- Exploring, converged, fully-apart, one-agent, and missing-tree snapshots all
  render without breaking the decision feed.
- Late connection populates lanes immediately and reconnecting leaves exactly
  one fleet poller.
- Fixture playback, pause/resume, looping, projector layout, and reduced motion
  still work.
- Fixture totals remain 528.4 seconds executed, 588.6 seconds deleted, and
  1117.0 seconds counterfactual.
- Existing PASSTHROUGH, provenance, verification, and divergence accounting
  does not regress.

### Blockers

None. This task changes no backend contract.

## Active — Task 2: live viewer QA and capture

### Allowed files

- Viewer/fixture presentation files only for a reproduced presentation bug
- One screenshot and one fallback recording in an agreed evidence/demo location
- `MY_TASKS.md` only to record completion and promote Task 3

### Required input

- A representative live five-agent daemon run, or Tom's preserved event log

### Implementation checklist

1. Connect the viewer to the representative live stream.
2. Verify HIT, MISS, LEASE_WAIT, PASSTHROUGH, source provenance, verification,
   divergence, fleet snapshots, and late reconnect.
3. Confirm the live totals reconcile with the preserved summary.
4. Capture a projector-readable screenshot and a fallback recording.
5. Change viewer code only for a reproduced presentation or accounting bug.

### Acceptance

The measured five-agent story can be presented without editing the fixture or
explaining around a broken state, and both captured artifacts remain usable if
the live daemon or agents fail on stage.

### Blocker

Waiting for a representative live daemon run or Tom's preserved log.

## Backlog — promote one only after the active task passes

### Task 3: generate the final evidence outputs

**Allowed files:** `evidence/` and `MY_TASKS.md`.

Run `python3 evidence/overlap.py` against the sealed replay corpus and review
the generated `overlap.json`, `overlap.md`, `value.md`, and `claims.md`. Confirm
7.5% avoidable, 3.6% cross-agent, 16.9% at steps 0–2, and 1.0% at step 50+ come
from the shipping Go replay. Keep every denominator and measured/modeled label.

**Acceptance:** one committed command regenerates every published evidence
artifact without `n/a`, `not_tested`, or a flattering alternate method.

**Blocker:** Tom must run the command at, or provide access to, the private
sealed-corpus path.

### Task 4: preserve fleet-run provenance and demo commands

**Allowed files:** evidence/demo documentation, `scripts/fleet.sh` only if a
reproduced interface bug requires it, and `MY_TASKS.md`.

Document the target revision, prompt/workload, exact baseline and cached
commands, agent count, output/cache directories, cold-start procedure, raw
summary locations, expected measurements, and recovery commands. State that
wall clock did not improve.

**Acceptance:** another teammate can reproduce both arms without guessing a
flag or environment variable.

**Blocker:** Tom must supply any raw paths or commands that are not committed.

### Task 5: Codex verification and owned-file regression

**Allowed files:** assigned paths only, and only for reproduced failures.

Prove Codex command rewriting under the judging permission profile, then run:

```sh
GOCACHE=/private/tmp/fasthack-go-cache \
GOTMPDIR=/private/tmp/fasthack-go-tmp \
go test ./...

GOCACHE=/private/tmp/fasthack-go-cache \
GOTMPDIR=/private/tmp/fasthack-go-tmp \
go build ./cmd/hindsight

bash -n scripts/fleet.sh scripts/replay-agent.sh
python3 -m py_compile evidence/overlap.py
git diff --check
```

**Acceptance:** the judging-profile Codex hook is demonstrated and the full
regression pass is green. Core failures are handed to Tom with a reproducer.

## Out of scope until the queue is complete

- Claude-Mem integration
- API/MCP response caching
- cross-machine or subtree caching
- environment restoration or install replay
- new dependencies
- decorative viewer redesigns

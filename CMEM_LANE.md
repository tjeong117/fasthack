# Claude-Mem lane — advisory warm boot

Scoping doc for the one honest Claude-Mem integration. **Nothing here is built yet, and nothing here may be built until every P0 and P1 item in [PLAN.md](PLAN.md) is done.** This file exists so the design is settled on paper while it is free to change.

Not named `README.md` on purpose — the repo front door is shared/Tom's call, and this is one optional side lane, not the project.

## The one-sentence claim

Hindsight's cache is **exact but ephemeral**: it dies with the fleet and knows nothing about yesterday's run. Claude-Mem is **approximate but durable**. So Hindsight writes observations into Claude-Mem during a run, and tomorrow's fleet boots with *hints* about what was expensive — as planning context, never as served results.

```
Hindsight  ──(metadata only, fire-and-forget)──▶  Claude-Mem     [durable, approximate]
Claude-Mem ──(text hints, at boot only)───────▶  agent prompt   [advisory, labelled]
Claude-Mem ─╳─────────────────────────────────▶  serve path     [NEVER]
```

## Why this is the only lane that isn't futile

Two things people expect Claude-Mem to do here, that it must not do:

1. **It cannot be the cache.** Hindsight's single defensible property is "no model in the system — we quote verified executions." Claude-Mem AI-compresses observations and retrieves them by approximate vector search. Putting lossy, generative recall inside a "provably never wrong" serve path doesn't add a feature, it deletes the guarantee. A judge watching for 60 seconds will not maintain two mental buckets labelled "exact" and "advisory"; one wrong hint and the whole thing is downgraded.
2. **It cannot do live cross-agent sharing.** Claude-Mem's clock is Stop → SessionStart: knowledge from a session that *ended* into a session that *starts later*. In a concurrent fan-out, agent 2 started twenty minutes ago; the knowledge arrives never. Hindsight's **single-flight lease already does live sharing** — agent 1 executes, agents 2–5 block and serve from its result while it is still running. That need is met.

What is genuinely unmet: **cross-run memory.** That is the lane.

> Do **not** argue this from `ρ = −0.252, p = 8.3e-77`. That statistic has no producing artifact and is cut from all our docs (see `evidence/AGENTS.md`). The two reasons above stand on their own.

## Verified environment (probed Aug 23, 2026, 14:45 PT — this machine)

The reliability objection I previously raised (auto-installing Bun/uv/Chroma on venue wifi) **does not apply here.** Everything is already installed and running.

| Fact | Value |
|---|---|
| Claude-Mem version | **13.15.3** |
| Runtime | `worker` (legacy local worker) → use **`/api/*`** routes, *not* `/v1/*` |
| Worker | **alive**, `http://127.0.0.1:37777/healthz` → `{"status":"ok"}` |
| Worker port | **37777** — overridden in settings; *not* the documented `37700+(uid%100)`=37701 |
| Data dir | `~/.claude-mem/` |
| SQLite | `~/.claude-mem/claude-mem.db` (3.1 MB, WAL) |
| Observations | **324**, FTS5 index `observations_fts` |
| Chroma | enabled, local, `127.0.0.1:8000`, `chroma-mcp` pid running |
| Runtimes present | node, npm, npx, bun, uv, sqlite3 — all on PATH |
| Project key for this repo | **`fasthack`**, with **49 observations** already stored |
| Already observing this repo | **yes** — rows include "Full Hindsight build plan reviewed from PLAN.md", "Five-agent fan-out experiment results recorded in commit c7e0843", "Three key correctness fixes in policy engine", and "Claude-Mem integration lane designed and documented in CMEM_LANE.md" (it observed this doc being written) |

Two caveats found in the same probe:

- `~/.claude-mem/CAPTURE_BROKEN` exists, written 21:45Z: `[bun-runner] empty stdin payload received — issue #2188`. But `observer-health.json` shows `consecutiveFailures: 0` with a recent `lastSuccessAt`, and observations are landing. Treat it as a known upstream flake, not a blocker — **but re-check the worker is up right before demoing.**
- `~/.claude-mem/settings.json` contains a live Pro/OpenRouter token and cloud-sync credentials. **Never commit that file, never paste it into a slide, never echo it in a terminal on the projector.**

## Ownership — how to build this without touching Tom's core

Per [AGENTS.md](AGENTS.md), Tom owns `internal/hp/*.go` and `cmd/hindsight/*`. The obvious implementation (make `daemon.go` POST to Claude-Mem) is **wrong twice**: it edits files we don't own, and it puts a network call on the daemon's path.

**Do this instead: a sidecar that tails the log.**

`$HP_HOME/log.jsonl` is already append-only and the daemon is its only writer. A separate process can follow it and never be noticed.

- Zero diff to Tom's Go code.
- `go.mod` stays frozen (stdlib-only rule untouched) — the bridge is Python 3, which `fleet.sh` already depends on for its summary.
- Physically cannot block, slow, or corrupt the hook, the daemon, or the serve path. If it crashes, the demo doesn't notice.
- Lives in `scripts/`, which is the Teammate's lane.

```
scripts/cmem-bridge.py    tail $HP_HOME/log.jsonl  ──▶ POST /api/sessions/observations
scripts/cmem-hints.py     read-only SQLite FTS     ──▶ hints.md  (prepended to the prompt)
```

## Write path — `scripts/cmem-bridge.py`

Follows the log and emits observations. **Rollup by default, not one per command** — 15 commands should not become 15 memories; that is how you poison a memory store with noise.

What it emits per fleet run:

1. **One run rollup.** Agents, hit rate, executed vs served, execution-seconds deleted, wall clock, mode (baseline/cached).
2. **One entry per expensive or notable command class** — the suite, the install, anything that fell to `PASSTHROUGH` with a reason worth remembering next time.

Mapped onto Claude-Mem's actual `observations` columns:

| Column | Value |
|---|---|
| `type` | `discovery` (a fact about the repo, not a code change) |
| `title` | `Hindsight fleet: <repo>@<ref> — 53% hit rate, 77% exec-seconds deleted` |
| `narrative` | the honest paragraph, including what did *not* improve |
| `facts` | JSON list, e.g. `["uv run pytest -q costs ~4.6s", "uv sync is not servable: env fingerprint changes", "5 agents coalesced to 1 execution on the suite"]` |
| `concepts` | `pattern`, `trade-off`, `gotcha` |
| `project` | **`fasthack`** (verified — this is the existing key, don't invent a new one) |

### Hard rules for the write path

1. **Metadata only. Never blob contents.** `stdout_blob` / `stderr_blob` are the *servable payload*. Copying command output into an approximate, AI-compressed store is exactly the contamination we're avoiding. Send the digest reference at most, never the bytes.
2. **Scrub before sending.** Reuse `Normalize` semantics — strip the worktree root and `$HOME` out of commands and reasons. Worktree paths differ per agent and leak local layout.
3. **Fire-and-forget.** 2-second timeout, catch everything, always `exit 0`. Mirrors Claude-Mem's own graceful-degradation contract (transport error → exit 0, never block).
4. **Never send secrets.** Skip any command containing a token-shaped string. Don't read `settings.json`.
5. **Opt-in.** Runs only when `HP_CMEM=1`. Default off, exactly like `HP_ENABLE`.

## Read path — `scripts/cmem-hints.py`

At fleet boot, produce a short hint block. **Query SQLite read-only, not the HTTP semantic endpoint.**

That is the deliberate call: a read-only FTS5 query is deterministic, instant, needs no model call, no OpenRouter token, no network, and works even if the worker is down. The semantic endpoint adds a model in the loop for something that only needs "what did we learn about this repo."

```sql
-- read-only: sqlite3 'file:...claude-mem.db?mode=ro&immutable=0'
SELECT o.created_at, o.type, o.title, o.narrative
FROM observations_fts f
JOIN observations o ON o.id = f.rowid
WHERE observations_fts MATCH ?          -- e.g. 'hindsight OR pytest OR "uv sync"'
ORDER BY o.created_at_epoch DESC
LIMIT 20;
```

Output goes to `hints.md`, and the operator prepends it to the `--prompt` file `fleet.sh` already takes. **Every line is labelled:**

```markdown
## Prior-run hints  [advisory · unverified · from Claude-Mem · NOT cache results]
- The test suite `uv run pytest -q` took ~4.6s on the last run.
- `uv sync` is never cache-served here (it changes the env fingerprint). Budget ~40s.
- Last fleet: 5 agents, 53% hit rate. The suite coalesced 5 executions into 1.
```

The label is not decoration. It is the sentence that keeps the "never wrong" claim intact: these are hints, and the cache is a separate thing that quotes.

## Build order (post-cut-line, ~60–90 min total)

| Step | Work | Time |
|---|---|---|
| ~~1~~ | ~~Resolve the `project` key~~ — **done: `fasthack`, 49 observations already present.** | ✔ |
| 2 | `cmem-hints.py` — read-only query → `hints.md`, filtered to `project='fasthack'`. **Read path first**: it works against 49 existing observations, so it demos immediately with zero writes. | 20 min |
| 3 | `cmem-bridge.py` — tail `log.jsonl`, build the rollup, POST to `/api/sessions/observations`. Verify one row lands via sqlite. | 30 min |
| 4 | Wire `HP_CMEM=1` into `fleet.sh` (Teammate-owned) as an opt-in post-run step. | 10 min |
| 5 | Screenshot: hints block at boot + the new observation in the cmem web viewer. | 10 min |

Step 2 before step 3 is deliberate — the read path is already demonstrable today, and it's the half a judge actually sees.

## Prize-track mapping ($1,000 Memory Prize, judged separately)

| Track | How this hits it |
|---|---|
| **Build an integration** | Claude-Mem wired into a *parallel agent fleet harness* — a surface its sequential lifecycle hooks don't reach. Strongest fit. |
| **Fire on what it sees** | The bridge reacts to observations as they land in the append-only log, in real time. |
| **Warm boot** | Tomorrow's fleet opens with cost/structure context instead of rediscovering it. |
| **Memory as a speed play** | Recall cuts turns spent re-learning what the last fleet already measured. |

Honest framing for the judges, which doubles as the answer to "why isn't memory in your cache?":

> Two kinds of memory matter. Semantic — "what did I learn about this repo" — is approximate, durable, and Claude-Mem is genuinely good at it. Exact-episodic — "did anyone run *this* command at *this* state" — is verified, quoted, abstain-on-miss, and that's Hindsight. We never let approximate recall decide what we serve. But our exact memory is ephemeral, so we feed Claude-Mem and tomorrow's fleet boots warm. Right memory, right job.

## Zero-dependency fallback

If Claude-Mem is uncooperative at showtime, the same idea in ~20 lines: a per-repo `FINDINGS.md` the fleet appends to on stop and reads at start. It captures most of the value, cannot fail, and is honest about being a text file. Keep it in your pocket; do not present it as the memory integration unless asked.

## Explicit non-goals

Do not, under any circumstances:

- let a Claude-Mem result become a servable record, or influence `Classify`, the key, the purity gate, or a lease;
- write stdout/stderr bytes, blob contents, secrets, or `settings.json` into Claude-Mem;
- put a Claude-Mem call anywhere on the hook or daemon path;
- edit `internal/hp/*.go` or `cmd/hindsight/*` for this;
- add a dependency to `go.mod`;
- describe this on stage as making the cache smarter. It makes the *next run* better informed. Those are different claims.

## Pre-demo checklist

- [ ] `curl -s http://127.0.0.1:37777/healthz` returns `{"status":"ok"}`
- [ ] `CAPTURE_BROKEN` re-checked; worker restarted if capture is actually dead
- [ ] `hints.md` regenerates in under a second with the worker stopped (proves the read path is worker-independent)
- [ ] bridge killed mid-run → fleet output and counters completely unaffected
- [ ] no token appears in any committed file, screenshot, or terminal on screen
- [ ] every hint line carries the `advisory · unverified` label

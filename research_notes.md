# Hindsight — Research Notes and Compute-Savings Bank

> Internal source notes, refreshed August 23, 2026 (2:00 PM). This is not the implementation contract. Fast-moving product details, ports, endpoints, and benchmark claims must be re-checked against primary sources before they go into a public submission.

The build contract is [AGENTS.md](AGENTS.md), the schedule is [PLAN.md](PLAN.md), the reconciled public explanation is [design_doc.md](design_doc.md), and the personal task breakdown is [MY_TASKS.md](MY_TASKS.md).

## Repo reality check (read this first)

As of this refresh the repository contains **only documentation** — `AGENTS.md`, `PLAN.md`, `design_doc.md`, `research_notes.md`, and `hello.txt`. There is **no `go.mod`, no `cmd/`, no `internal/hp/`, no `scripts/`, no `web/`, and not one `.go` file**. Every schedule and "confirmed" claim below should be read as a plan, not a status. The single highest-value action right now is to stop editing prose and land buildable stubs.

Two claims that earlier drafts treated as settled but are not:

- **Interception is unverified.** The Codex `PreToolUse` rewrite path (`allow` + `updatedInput`) is the entire mechanism, and there is an open report ([codex#32544](https://github.com/openai/codex/issues/32544)) that a rewrite can *display* in the UI while the original command still runs under a managed permission profile. Until this is tested in the exact stage environment it is a **go/no-go**, not a feature.
- **`ρ = −0.252, p = 8.3e-77` does not exist.** It appears in old notes but traces to files that were never present and no script computes it. Do not use it in the doc, the pitch, or Q&A. The arguments that matter (below) do not need it.

## Executive summary

The research considered two connected opportunities:

1. A command-result cache keyed by live repository and environment state — the hackathon core.
2. A memory layer that could carry setup knowledge across sessions while leaving large artifacts in Hindsight's own content-addressed store — an optional, out-of-core extension that maps directly onto the Claude-Mem sponsor prize.

Single-flight request coalescing is the best complexity-to-demo-value addition because it converts concurrent identical cache misses into one real execution, and it is *also* Hindsight's real-time cross-agent sharing story. The correctness crux is conservative cacheability classification plus measured before/after state.

The memory layer must never be treated as a source of servable command results. Only observed executions belong in Hindsight's index. Claude-Mem's role is strictly a one-way, advisory, cross-session warm-boot lane (detailed below).

## Ranked compute-savings bank

### 1. Result-only command cache — current core
**Saves:** repeat test, build, type-check, lint, reproduction, and inspection time.
**Complexity:** medium. Needs a sound state key, exact stdout/stderr/exit capture, conservative policy, and a servability gate.
**Correctness risk:** medium-to-high if inputs are omitted. Mitigation: repo tree hash + env fingerprint + relative cwd + normalized command + non-hermeticity exclusions + before/after measurement.
**Environment restore:** none. Only the recorded result is replayed.

### 2. Single-flight request coalescing — current core
**Saves:** converts multiple simultaneous executions of the same cacheable command into one execution plus waiting readers. This is also the *real-time* cross-agent sharing layer.
**Complexity:** low-to-medium. One lease per key; waiters re-check after completion; expired/failed leases fall through.
**Correctness risk:** errors are shared with all waiters, so provenance and failure behavior must be explicit. Coalescing never overrides the purity gate.

### 3. Dependency or environment restoration — deferred
**Saves:** dependency install and setup time, often tens of seconds to minutes.
**Complexity:** high — filesystem deltas, artifact transport, safe restoration, compatibility checks, cleanup.
**Correctness risk:** high. `.venv` can embed absolute paths; native artifacts vary by arch/interpreter/OS. Deliberately out of scope.

### 4. Snapshot / microVM reuse — positioning only
**Saves:** cold environment startup and broad setup.
**Complexity:** infrastructure-heavy, unsuitable for a four-hour build.
**Positioning:** snapshot systems reduce *per-agent startup*; Hindsight targets *duplicate commands across already-independent attempts*. We do not compete there.

### 5. Declared action caching — precedent
Build/compiler caches (Bazel, Turborepo, Nx, ccache, sccache) prove the value of hashing inputs and storing results, but their boundary is a *declared* action graph or compiler invocation. Hindsight's boundary is the arbitrary shell command at the agent tool boundary — zero declaration, which requires conservative abstention.

### 6. Affected-test selection — complementary
Runs only tests likely affected by a diff. Reduces the cost of a *miss*; Hindsight removes repeated *executions* of the selected command. Complementary, not competing. Needs a full-suite fallback to stay safe.

### 7. Memory-based warm boot — deferred to the Claude-Mem lane
Optimizes reasoning context and cross-session recall, not command execution. This is exactly where Claude-Mem fits, and it must be framed separately from Hindsight's measured execution savings (full section below).

### 8. Hash and lookup overhead
Caching loses value when hashing + daemon round-trips + storage cost more than the command. Later optimizations: skip known-cheap classes, reuse a tree hash while watched state is unchanged, incremental file-state index, batch daemon events, measure overhead explicitly instead of guessing a universal time threshold.

### 9. Determinism and cacheability
The static classifier does only what measurement cannot: exclude clock/network/randomness/host identity/remote side effects; reject ambiguous parsing and mixed unsafe chains; avoid recording low-value noise. Before/after state measurement is the final gate for local mutations.

### 10. Hit-rate realism
Best case is parallel fan-out on one task before agents edit. Other plausible hit sources: repeated reproduction runs, unchanged CI-style commands, commands scoped to unchanged regions. Hit rate falls as worktrees diverge; the full-tree key accepts that loss in exchange for a clean correctness story. Multi-developer, cross-machine, and subtree-aware reuse remain unproven.

---

## Claude-Mem: what it actually is (verified against primary docs)

Sourced from `github.com/thedotmack/claude-mem` `docs/` on Aug 23, 2026. Apache-2.0. Built for Claude Code; also supports OpenCode and Antigravity CLI.

**One-line model:** an *approximate, generative, cross-session* memory system. It captures tool-usage observations via lifecycle hooks, uses the Claude Agent SDK to AI-compress them into semantic summaries, stores them in SQLite + a Chroma vector DB, and injects recalled context into future sessions.

### Runtime shape
- **Local worker daemon** (Express), per-user port **`37700 + (uid % 100)`**. Prints a web-viewer URL on startup with a real-time memory stream (SSE).
- **Storage:** SQLite (`claude-mem.db`) for structured data; ChromaDB (`chroma.sqlite3`) for vector embeddings.
- **Server Beta (v13):** Postgres + Valkey/BullMQ + API-key auth, for deployable use. Overkill for a hackathon; note it exists but do not run it on stage.
- **Dependencies:** auto-installs **Bun** and **uv** if missing. This is the venue-wifi demo risk — see below.

### Five lifecycle hooks (the clock that matters)
| Event | What it does | Timeout |
|---|---|---|
| SessionStart | start worker + inject context | 60s |
| UserPromptSubmit | register session + start SDK agent + semantic injection | 60s |
| PostToolUse | capture tool usage → enqueue | 120s |
| Summary (Stop) | request session summary | 120s |
| SessionEnd | end session + drain | 30s |

**The critical property for us:** injection is **sequential** — memory is compressed on Stop and injected on SessionStart. It carries knowledge from a session that *ended* into a session that *starts later*. In a concurrent fan-out there is no "next session" to inject into, so Claude-Mem cannot do live cross-agent sharing. Hindsight's **lease already does** live sharing. This is why Claude-Mem is redundant in our core and only useful across *runs*.

### HTTP surfaces (for an integration)
- **Legacy worker routes** under `/api`: `/api/sessions/init`, `/api/sessions/observations`, `/api/context/semantic`, `/api/sessions/summarize`, `/api/sessions/complete`. `broadcastObservation()` emits over **SSE** to the viewer/UI.
- **Server Beta REST** under `/v1`: `POST /v1/events` (+ `/batch`, with `generate=false` / `wait=true` flags), `POST /v1/memories`, `POST /v1/search`, `POST /v1/context`, and a read-only remote MCP at `/v1/mcp`.
- **MCP search tools** (the 3-layer, token-efficient pattern): `search` (compact index, ~50–100 tok/result) → `timeline` (chronological context) → `get_observations` (full detail, batch IDs only). Remote MCP exposes `search` / `context` / `recent`.

### Failure behavior (why an integration is safe)
Claude-Mem's own hooks fail open: transport errors (ECONNREFUSED, timeout, 5xx) → **exit 0**, never blocking Claude Code. Any write path *we* build into it must do the same and, additionally, must be **fire-and-forget off the serve path** so a dead Claude-Mem worker cannot slow or corrupt a Hindsight hit.

## Why Claude-Mem is futile *in the core* — the two real reasons

1. **It poisons Hindsight's one defensible property.** Hindsight's whole pitch is "no model in the system — we quote verified executions, never generate." Claude-Mem is a model: lossy AI compression + approximate vector retrieval. Putting it inside a "provably never wrong" serve path deletes the guarantee. A judge watching for 60 seconds will not hold two mental buckets ("this part exact, that part advisory"); one wrong hint downgrades the whole thing.
2. **It is redundant with the lease.** The only real-time cross-agent sharing a fan-out needs is already provided by the single-flight lease: agent 1 executes, agents 2–5 block and serve from agent 1's result *while agent 1 is still running*. Claude-Mem's sequential clock cannot do this at all.

Do **not** argue this from `ρ = −0.252`; that statistic is fabricated (see top of file). The two reasons above stand on their own.

## Where Claude-Mem is genuinely useful — the one honest lane

Hindsight's cache is **ephemeral**: it dies when the fleet ends and remembers nothing of yesterday's fleet. That gap is exactly Claude-Mem's strength (durable cross-session semantic recall). So the honest integration is a **one-way, advisory, cross-session warm-boot lane**:

- **Write (during a run):** the Hindsight daemon fires observations into Claude-Mem in real time — "this command was expensive," "this suite took 90s at tree X," "this repro reproduced the bug" — via `POST /api/sessions/observations` (local) or `POST /v1/events` (server), fire-and-forget, exit-0 on failure.
- **Read (next run):** tomorrow's fleet queries `search` / `context` (or `/api/context/semantic`) at SessionStart to boot with **advisory hints** about what was expensive or already learned.

**Hard boundary:** this lane is strictly one-way and **cannot touch the serve path or the "never wrong" guarantee.** Claude-Mem hints are planning context, never servable results. Right memory, right job.

## Claude-Mem sponsor prize — the seven tracks, mapped to what we can build

Separate **$1,000 Memory Prize**, judged independently of overall 1st–3rd. We can win both. Everything scores extra for "something someone would actually use." Ranked by fit for a Hindsight-adjacent build:

1. **Build an integration (strongest fit).** Wire Claude-Mem into an agent *fleet harness* where it doesn't live yet. Hindsight's daemon becomes the writer; `fleet.sh` becomes the consumer at boot. This is a genuinely novel surface (concurrent fan-out) that Claude-Mem's sequential hooks don't cover.
2. **Fire on what it sees.** Hook actions off observations as they land in real time — the Hindsight daemon writes an observation on every `record`, and/or subscribes to Claude-Mem's SSE stream to react to its own emitted memories.
3. **Warm boot.** Tomorrow's fleet opens with instant context ("suite is ~90s; agent-2 already found the repro at tree X") instead of burning its first N turns rediscovering the repo. Directly = our ephemeral-cache gap, filled.
4. **Memory as a speed play.** Recall to cut tokens / turns / wall-clock. This is literally Hindsight's thesis for *exact* memory; the Claude-Mem lane extends it to *semantic* memory across runs. Clean framing for the pitch.
5. **Give the skills a face.** Wrap the CLI/MCP search tools in UI. Our `web/viewer.html` could add a "warm-boot hints" panel that renders Claude-Mem `search`/`timeline` results next to Hindsight's exact hits — a visible contrast of "exact vs advisory."
6. **Build on the timeline.** Use the `timeline` MCP tool + mem-search skill as the retrieval layer for the warm-boot panel.
7. **Ingest anything, look for anything.** Feed Hindsight's `log.jsonl` (commands, durations, exit codes, per-class value) into Claude-Mem as observations — logs are one of the listed ingestion sources.

### Side projects we could build with Claude-Mem (bigger than a lane)
Only if the core is done and stable. Each is self-contained enough to demo on its own for the $1k:

- **`fleetmem` — warm-boot advisor for agent fleets.** A ~20-line writer + a SessionStart reader that turns "what did the last fleet learn about this repo" into a boot-time hint block. The honest, minimal version of the whole memory story. (Compare: a plain per-task `FINDINGS.md` captures most of the value with zero retrieval — use that as the fallback if Bun/uv install fails on venue wifi.)
- **Memory viewer face.** A focused UI over `search`/`timeline`/`get_observations` that makes memory steerable/shareable — pin, promote, or mute observations before they inject.
- **Real-time observation reactor.** Subscribe to the SSE stream; when an observation matching a pattern lands (e.g. "test suite failed"), fire an action (open an issue, ping a channel, tag the fleet).
- **Log/transcript ingester.** A tool that ingests CI logs, agent transcripts, or `log.jsonl` and makes them queryable via the existing search skill.

### Demo-reliability warning (do not skip)
Claude-Mem auto-installs **Bun**, **uv**, and Chroma, and SessionStart hooks are synchronous with 60s timeouts. Five worktrees × five launches on venue wifi mid-hackathon is five chances of a visible failure on the projector, in a demo whose whole aesthetic is "nothing goes wrong." Mitigations: (a) keep Claude-Mem 100% off the critical path so its failure is invisible to the core demo; (b) pre-install and warm the worker before going on stage; (c) keep the `FINDINGS.md` markdown fallback ready as the zero-dependency version of the same idea.

## Four-hour build recommendation

**Hour 0–1 — interception go/no-go, then result path.** First prove the `PreToolUse` rewrite works in the *actual* stage environment (codex#32544). If it fails, switch to a verified execution boundary before building anything else. Then: compute repo + env state, capture stdout/stderr/exit/duration separately, store one real observation, serve it to a peer.
**Hour 1–2 — daemon and single-flight.** Lookup, record, lease. Daemon is the single log writer. Verify exactly one execution for concurrent identical lookups.
**Hour 2–3 — policy and fail-open correctness.** Three policy values, non-hermeticity exclusions, chain rule. Test tree-changing / env-changing commands. Test malformed input, dead daemon, timeout.
**Hour 3–4 — evidence and presentation.** Baseline vs cached fleet with **measured** durations. Provenance + verification in the viewer. Rehearse the three-minute narrative; record a fallback demo.

**Claude-Mem work is strictly post-cut-line dessert for the $1k**, and only if it is stable.

## Pivot thresholds
- If interception is unreliable on stage, stop and fix the boundary before adding features.
- If cross-agent hits are low, emphasize concurrent single-flight savings.
- If state hashing dominates short commands, narrow the candidate set and show the measured break-even point.
- If the viewer is incomplete, keep the append-only event log and present a simple generated summary.
- Do not pivot into environment restoration during the hackathon.
- If Bun/uv/Chroma won't install cleanly, drop the Claude-Mem lane to the `FINDINGS.md` markdown fallback and keep moving.

## Claims requiring source verification / careful labels
- Internal corpus overlap is **measured**; time-by-command-class is **modeled** unless produced by the live baseline arm.
- Adjacent agent-memory studies are not direct evidence of identical-command cache hit rate.
- Multi-developer reuse is a hypothesis, not a demonstrated result.
- `11,687` is the **deduplicated** command count; `12,806` is the **raw** count. Label which; never mix on a slide.
- `ρ = −0.252` stays **excluded** — no surviving script reproduces it.
- Any Claude-Mem port/endpoint/tool name above is version-sensitive; re-verify against the installed version before building.

## Research conclusion

The defensible hackathon product is the narrow one: replay only real command observations whose relevant local state was measured unchanged. Single-flight makes the cold parallel demo valuable *and* is the live cross-agent sharing story; baseline durations make the savings credible; provenance and optional shadow verification make the result explainable. Claude-Mem is a clean, honest, **one-way** warm-boot extension for the separate prize — never a component of the serve path.

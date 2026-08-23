# Hindsight — design doc

*Skeleton with the frozen contracts. Prose sections are filled in at the end of the build with measured numbers.*

## Problem

*(to be written)*

## Principles

1. Default is PASSTHROUGH.
2. The key must dominate the output.
3. Share the map, not the route.
4. Effects, not calls.
5. Abstention over guessing.
6. Verify what you serve.

## Frozen contracts

These are pinned at minute 12 and do not change without both authors agreeing out loud. They are duplicated in `AGENTS.md`, which is what the coding agents read.

### Policy

Exactly three values. The word "deny" is never used: in Codex, `permissionDecision: "deny"` blocks the tool call outright, and an agent that cannot run `curl` is broken.

```go
type Policy int

const (
    PASSTHROUGH Policy = iota // run normally, do not record
    RECORD_ONLY               // run normally, record for the transition corpus
    SERVE                     // eligible to be served, subject to the purity gate
)

func Classify(cmd string) (Policy, string) // policy + human-readable reason
func Normalize(b []byte, root, home string) []byte
```

`Classify` is a pure function of the command string. It never consults the filesystem, the daemon, or the clock.

### Key

```
key = sha256(hs-v1 \0 tree \0 env_fp \0 cwd_rel \0 cmd_norm)
```

`tree` is git's own Merkle hash of the live worktree, computed against a per-worktree side index so `.git/index` is never touched. `env_fp` covers what the tree hash structurally cannot see, which is everything gitignored that changes output — principally the virtualenv.

### Log record

One JSON object per line in `$HP_HOME/log.jsonl`, appended only by the daemon. Agents never write to it directly.

```json
{"v":1,"ts":0.0,"agent":"a3","cmd":"","cmd_norm":"","cwd_rel":"",
 "tree_before":"","env_fp_before":"","tree_after":"","env_fp_after":"",
 "key":"hs-v1:","policy":"SERVE","reason":"","decision":"HIT",
 "servable":false,"exit_code":0,"duration_ms":0,
 "stdout_blob":"sha256:","stderr_blob":"sha256:","source_agent":"","verified":null}
```

`decision` is one of `HIT`, `MISS`, `LEASE_WAIT`, `PASSTHROUGH`.

### The purity gate

A record is servable if and only if:

```
tree_after == tree_before && env_fp_after == env_fp_before
```

Measured, not declared.

## Command policy

*(to be written)*

## Key derivation

*(to be written)*

## Dependency scoping tiers

*(to be written — Tier 1 and Tier 2 are design-only for this build)*

## Architecture and storage

*(to be written)*

## Failure modes and guards

*(to be written)*

## Phase 2

*(to be written)*

## What we do not claim

*(to be written — see PLAN.md for the list; the negatives are the credibility)*

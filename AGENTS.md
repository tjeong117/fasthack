# Hindsight — agent contract

A build cache for coding agents. A PreToolUse hook keys every shell command on git's tree hash plus an environment fingerprint; if a peer already ran that exact command at that exact state, the command is rewritten to return the recorded result. Nothing is predicted. A served result is a verified replay, so the failure mode is a cache miss, never a wrong answer.

Build order and schedule live in `PLAN.md`. The public writeup lives in `design_doc.md`. This file is only the things that must not drift.

## Read this first: the hook can break your own session

This repo installs a PreToolUse hook into `.codex/hooks.json` and `.claude/settings.json`. That hook intercepts shell commands **in this repo**, which includes the ones your own agent is running to develop the hook. A bug in it breaks the loop you would use to fix the bug.

The hook therefore exits silently with empty output unless `HP_ENABLE=1` is set. Only `scripts/fleet.sh` sets it. Never export it in a development shell, and never remove the guard.

## Ownership

Do not edit files you do not own. If you need something from the other column, use its frozen signature and stub it — do not create the file.

| Path | Owner |
|---|---|
| `internal/hp/key.go`, `store.go`, `hook.go`, `record.go`, `daemon.go` | Tom |
| `cmd/hindsight/main.go` | Tom |
| `internal/hp/policy.go`, `internal/hp/norm.go` (+ their tests) | Teammate |
| `scripts/fleet.sh`, `web/viewer.html` | Teammate |
| `AGENTS.md`, `PLAN.md`, `design_doc.md`, `go.mod` | Shared — see below |

`go.mod` is frozen after the first commit. Dependencies are **stdlib only**; there is no reason for it to change, and two agents running `go mod tidy` is a guaranteed conflict. If you think you need a dependency, you are solving the wrong problem.

## Frozen contracts

Changing any of these requires telling the other person out loud. Do not change them in a commit and hope.

Policy values — exactly three. The word "deny" is never used, because in Codex `permissionDecision: "deny"` blocks the tool call outright and an agent that cannot run `curl` is broken.

```go
type Policy int // SERVE | RECORD_ONLY | PASSTHROUGH

func Classify(cmd string) (Policy, string) // returns policy + human reason
func Normalize(s string, root, home string) string
```

Log line, one JSON object per line, appended only by the daemon:

```json
{"v":1,"ts":0.0,"agent":"a3","cmd":"","cmd_norm":"","cwd_rel":"",
 "tree_before":"","env_fp_before":"","tree_after":"","env_fp_after":"",
 "key":"hs-v1:","policy":"SERVE","reason":"","decision":"HIT",
 "servable":false,"exit_code":0,"duration_ms":0,
 "stdout_blob":"sha256:","stderr_blob":"sha256:","source_agent":"","verified":null}
```

`decision` is one of `HIT`, `MISS`, `LEASE_WAIT`, `PASSTHROUGH`.

## Invariants — breaking these produces wrong answers, not slow ones

1. **Default is PASSTHROUGH.** Anything unmatched, unparseable, or uncertain runs normally. A classification bug must cost a hit, never correctness.
2. **A record is servable only if `tree_after == tree_before` and `env_fp_after == env_fp_before`.** This is the gate. It is measured, not declared. It is what catches `tsc` (emits `.js`), `cargo test` (writes `target/`), and `uv sync` (invisible to the tree hash, visible to the env fingerprint).
3. **The non-hermeticity deny-list is irreducible.** `date`, `curl`, `$RANDOM`, `git push`, `uuidgen`, `hostname` are pure by state and still wrong to serve. State hashing cannot see them. This list is correctness, not polish.
4. **The side index is per-worktree.** Resolve it with `git rev-parse --git-dir`, never hardcode `.git/hp-index`. Five worktrees sharing one index file corrupt each other's trees and produce wrong keys.
5. **`$HP_HOME` lives outside the tree**, default `~/.hindsight/<repo-id>/`. Cache writes inside the workspace change the hash that keys the cache.
6. **Only real observed results enter the index.** Generated predictions, simulator rollouts, teacher data and judgments never become servable records. (Rule adopted from Experiential Labs, Apache-2.0.)
7. **stdout and stderr stay separate.** Serve as `cat out; cat err >&2; exit rc`.
8. **Chain rule:** split on `&&`, `;`, `|`. The strictest segment wins.

## Hook facts that cost an hour each if unknown

- Codex is **not** byte-compatible with Claude Code. `codex-rs/hooks/src/schema.rs:788` says so explicitly. Codex rejects `"ask"`, rejects `allow` without `updatedInput`, rejects `updatedInput` without `allow`, and applies `deny_unknown_fields`. There is no plain approve verb — to pass a command through untouched, emit **no decision**.
- Codex's default hook timeout is **600 seconds**. Set it explicitly in the config or a stalled daemon hangs a tool call for ten minutes before failing open.
- Print exactly one JSON object on stdout. Output *before* the JSON silently no-ops the hook; output *after* it fails loudly.
- Headless runs need `codex exec --dangerously-bypass-hook-trust`.
- Hooks fail open, run unsandboxed, and execute under a login shell (`$SHELL -lc`).
- Codex file edits arrive as `apply_patch`, not `Bash`, so a Bash-matcher hook never sees them. This is fine: the key is state-based, so unobserved mutations are still caught by the tree hash.

## Commands

```bash
go build ./cmd/hindsight        # must stay green on main at all times
go test ./...
HP_DAEMON=http://127.0.0.1:7777 hindsight daemon
scripts/fleet.sh 5 baseline     # hooks off, control arm
scripts/fleet.sh 5 cached       # hooks on
```

Use port 7778 if the other person already holds 7777.

## Git

Two people, disjoint files, four hours. Both push straight to `main`; pull requests are pure overhead here.

- `git pull --rebase` before every push. Files are disjoint, so this should never conflict. If it does, you edited someone else's file.
- Commit small and often. `main` must always build.
- Land the empty shell early. A stub that returns `PASSTHROUGH` for everything, committed at minute 20, is worth more than a finished file at minute 150, because it moves integration to when it is cheap.

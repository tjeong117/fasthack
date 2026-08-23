# Cross-worktree verification reports divergences that are not divergences

**Status:** diagnosed, reproduced in a test, **not fixed** — the fix is one line
in `cmd/hindsight/verify.go`, which is not ours to edit.

**Owner of the fix:** Tom.

---

## Symptom

In `demo-runs/20260823-154831/cached/`, shadow verification checked twelve
served results and reported **nine divergent**. None of the nine were real. The
three commands split cleanly along one axis:

| command | absolute paths in output | verdict |
|---|---|---|
| `pytest -q sympy/core/tests/test_expr.py` | none | verified |
| `pytest -q sympy/core/tests/test_arit.py` | 1 warning block | **diverged** |
| `pytest -q sympy/core/tests/test_numbers.py` | 2 warning blocks | **diverged** |

Any command whose output prints an absolute path diverges. Any command whose
output does not, verifies.

## Cause

`cmd/hindsight/verify.go:89-92` normalizes both sides with the same root:

```go
gotOutN  := hp.Normalize(res.Stdout, ws.Root, home)  // fresh   — correct root
wantOutN := hp.Normalize(wantOut,    ws.Root, home)  // recorded — WRONG root
```

`ws.Root` is the verifier's worktree. `wantOut` was produced in a peer agent's
worktree. Its paths therefore never match the string being substituted, and the
two sides come out shaped differently rather than merely differing:

```
recorded, produced in .../a3, normalized against .../verify
  → /private{{TMP}}:1699: PytestUnknownMarkWarning ...

fresh, produced in .../verify, normalized against .../verify
  → {{ROOT}}/sympy/core/tests/test_arit.py:1699: PytestUnknownMarkWarning ...
```

The fresh output loses its prefix to `{{ROOT}}`. The recorded output falls
through to the temp-dir pattern in `internal/hp/norm.go`, which swallows the
whole path tail and leaves behind the `/private` the pattern is not anchored to.
Two correct runs, two different strings.

Verified by hand against the blobs still on disk from that run:

```
/private/tmp/fleet-cached-20260823-154831/a3/sympy/core/tests/test_arit.py:1699
/private/tmp/fleet-cached-20260823-154831/verify/sympy/core/tests/test_arit.py:1699
```

## Why this is not fixable in `norm.go`

`Normalize(b, root, home)` accepts exactly one root, and the signature is a
frozen contract. The recorded blob's producing root is not recoverable from the
bytes — the only thing distinguishing `.../a3/sympy/...` from
`.../verify/sympy/...` is one path segment that Normalize has no way to know is
a worktree name rather than a real directory.

The tempting heuristic — collapse any path sharing `filepath.Dir(root)` plus one
segment — was considered and rejected. It makes `<parent>/X/foo` and
`<parent>/Y/foo` compare **equal**, which is right for peer worktrees and wrong
for any two sibling directories that are not. That trades a false divergence for
a false agreement, and a verifier that can report a clean bill of health it has
not earned is worse than one that cries wolf. The project's stated stance is
that a classification bug must cost a hit, never correctness; the verification
analogue is that it must cost a false alarm, never a false pass.

## Proposed fix

Record the root each result was produced under, and normalize each blob against
its own root.

**1. `internal/hp/store.go` — add one field to `Record`:**

```go
 	SourceAgent string  `json:"source_agent"`
+	Root        string  `json:"root"`   // absolute worktree root the result was produced in
 	Verified    *bool   `json:"verified"`
```

**2. Populate it wherever a `Record` is built from a real execution** (in
`cmd/hindsight/record.go`, alongside `Agent:` / `Cmd:`):

```go
Root: ws.Root,
```

**3. `cmd/hindsight/verify.go:91-92` — use it:**

```go
-wantOutN := hp.Normalize(wantOut, ws.Root, home)
-wantErrN := hp.Normalize(wantErr, ws.Root, home)
+wantOutN := hp.Normalize(wantOut, recordRoot(rec, ws.Root), home)
+wantErrN := hp.Normalize(wantErr, recordRoot(rec, ws.Root), home)
```

where `recordRoot` falls back to `ws.Root` for records written before the field
existed, so old caches degrade to today's behaviour rather than panicking:

```go
func recordRoot(rec *hp.Record, fallback string) string {
	if rec.Root != "" {
		return rec.Root
	}
	return fallback
}
```

### Two things to be aware of before doing this

- **The log line schema in `AGENTS.md` is frozen.** Adding `root` changes it.
  That is the out-loud conversation the contract asks for; this document is one
  half of it.
- **`root` is an absolute local path in a machine-readable artifact.** The
  blobs already contain the same paths verbatim, so it leaks nothing new, but if
  the log is ever meant to be shareable the field should be scrubbed at export
  rather than at write time — scrubbing at write time would defeat its purpose.

## Test coverage already landed

`internal/hp/norm_test.go`:

- `TestNormalizeCrossWorktreeRoots` — reproduces the exact failure with the real
  strings, asserts the two sides do **not** agree under today's caller, asserts
  *why* (one becomes `{{ROOT}}/...`, the other `/private{{TMP}}`), and asserts
  that handing each blob its own root makes them agree. That last assertion is
  the proposed fix, proven at the unit level.
- `TestNormalizeCrossWorktreeRelativeOutputIsUnaffected` — the control: output
  with no absolute paths normalizes identically under any root, which is why
  `test_expr.py` was the one command that verified clean.

Both are green today. When the caller is fixed, `TestNormalizeCrossWorktreeRoots`
will fail loudly on its first assertion with a message telling you to flip it to
assert equality. That is deliberate.

## Until it is fixed

`verified_false` in `summary.json` is now accurate (it previously read 0 — see
the accounting fix in `scripts/fleet.sh`), but on a multi-worktree fleet run the
number it reports is dominated by this artifact. **Do not quote a divergence
count from a fleet run as evidence of cache unsoundness** until the roots are
matched up. Divergences from a single-worktree run are trustworthy.

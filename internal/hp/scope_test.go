package hp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run against real git repos, real commits and real tree objects
// rather than a mock, because the whole of Tier 1 rests on git's own answer to
// "what differs between these two trees". A fake diff would test our opinion
// of git instead of git.

func scopeWrite(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func scopeRemove(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

// scopeTree stages the worktree and returns git's own hash of it, which is
// exactly what Workspace.TreeHash produces in production.
func scopeTree(t *testing.T, root string) string {
	t.Helper()
	mustRun(t, root, "git", "add", "-A")
	cmd := exec.Command("git", "write-tree")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git write-tree in %s: %v", root, err)
	}
	tree := strings.TrimSpace(string(out))
	if tree == "" {
		t.Fatal("git write-tree returned nothing")
	}
	return tree
}

// scopeRepo seeds a repo, snapshots it as the recorded tree, applies mutate,
// and snapshots the result as the current tree. The worktree is left in the
// current state, which is what an agent's disk looks like at lookup time.
func scopeRepo(t *testing.T, seed map[string]string, mutate func(root string)) (root, recorded, current string) {
	t.Helper()
	root = newRepo(t)
	for rel, body := range seed {
		scopeWrite(t, root, rel, body)
	}
	recorded = scopeTree(t, root)
	mutate(root)
	current = scopeTree(t, root)
	if recorded == current {
		t.Fatalf("fixture did not move the tree; there is nothing for tier 1 to decide")
	}
	return root, recorded, current
}

// scopeCheck asserts the promotion verdict and the invariant that every
// decision explains itself well enough for a human reading a log to act on.
func scopeCheck(t *testing.T, d ScopeDecision, want bool) {
	t.Helper()
	if strings.TrimSpace(d.Reason) == "" {
		t.Fatal("every ScopeDecision must carry a reason")
	}
	if d.Promoted != want {
		t.Fatalf("promoted = %v, want %v\n  reason:  %s\n  changed: %v\n  scope:   %v",
			d.Promoted, want, d.Reason, d.ChangedPaths, d.ScopePaths)
	}
}

func scopeEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The motivating case, and the only reason Tier 1 exists: a peer edited a file
// this command does not name, so the recorded result is still the right answer.
func TestScopePromotesDisjointPeerEdit(t *testing.T) {
	seed := map[string]string{
		"src/auth.py":           "def login(): pass\n",
		"src/billing.py":        "def total(): return 1\n",
		"tests/test_billing.py": "def test_total(): assert True\n",
	}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "src/auth.py", "def login(): return 42\n")
	})

	d := ScopeMatch(root, recorded, current, "wc -l tests/test_billing.py")
	scopeCheck(t, d, true)
	if !scopeEqual(d.ChangedPaths, []string{"src/auth.py"}) {
		t.Fatalf("changed paths = %v, want [src/auth.py]", d.ChangedPaths)
	}
	if !scopeEqual(d.ScopePaths, []string{"tests/test_billing.py"}) {
		t.Fatalf("scope paths = %v, want [tests/test_billing.py]", d.ScopePaths)
	}
}

func TestScopeRefusesWhenTheScopedFileChanged(t *testing.T) {
	seed := map[string]string{
		"src/auth.py":           "def login(): pass\n",
		"tests/test_billing.py": "def test_total(): assert True\n",
	}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "tests/test_billing.py", "def test_total(): assert False\n")
	})

	scopeCheck(t, ScopeMatch(root, recorded, current, "wc -l tests/test_billing.py"), false)
}

// A change inside a scoped directory is inside the scope. Getting this
// backwards is the difference between a cache and a corruption.
func TestScopeRefusesChangeInsideScopedDirectory(t *testing.T) {
	seed := map[string]string{
		"src/auth.py":           "def login(): pass\n",
		"tests/test_billing.py": "def test_total(): assert True\n",
	}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "tests/test_billing.py", "def test_total(): assert False\n")
	})

	for _, cmd := range []string{"wc -l tests", "wc -l tests/", "wc -l ./tests"} {
		d := ScopeMatch(root, recorded, current, cmd)
		if d.Promoted {
			t.Fatalf("%q promoted despite a change inside its scope: %s", cmd, d.Reason)
		}
		scopeCheck(t, d, false)
	}
}

// src2/x.py does not live under src. Naive strings.HasPrefix says it does, and
// a repo with a src2 would then never see a hit.
func TestScopeSrcIsNotAPrefixOfSrc2(t *testing.T) {
	seed := map[string]string{
		"src/app.py":    "x = 1\n",
		"src2/other.py": "y = 1\n",
	}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "src2/other.py", "y = 2\n")
	})

	d := ScopeMatch(root, recorded, current, "wc -l src")
	scopeCheck(t, d, true)
	if !scopeEqual(d.ScopePaths, []string{"src"}) {
		t.Fatalf("scope paths = %v, want [src]", d.ScopePaths)
	}
}

func TestScopePathsConflictUsesSegmentBoundaries(t *testing.T) {
	cases := []struct {
		scope, changed string
		conflict       bool
	}{
		{"tests/test_billing.py", "tests/test_billing.py", true},
		{"tests", "tests/test_billing.py", true},
		{"tests/", "tests/test_billing.py", true},
		{"./tests", "tests/test_billing.py", true},
		{"tests/a/b.py", "tests", true}, // scope sits inside a changed directory
		{"tests/test_billing.py", "src/auth.py", false},
		{"src", "src2/x.py", false},
		{"src2/x.py", "src", false},
		{"src", "srcx", false},
		{".", "anything/at/all.py", true},
	}
	for _, c := range cases {
		if got := scopePathsConflict(c.scope, c.changed); got != c.conflict {
			t.Errorf("scopePathsConflict(%q, %q) = %v, want %v", c.scope, c.changed, got, c.conflict)
		}
	}
}

// A command with no path argument reads the whole tree by definition.
func TestScopeRefusesUnscopedCommand(t *testing.T) {
	seed := map[string]string{"src/auth.py": "def login(): pass\n"}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "src/auth.py", "def login(): return 42\n")
	})

	for _, cmd := range []string{"wc", "wc -l -q", "wc -l -q --tb=short", "wc", "cat"} {
		d := ScopeMatch(root, recorded, current, cmd)
		scopeCheck(t, d, false)
		if !strings.Contains(d.Reason, "no literal path arguments") {
			t.Fatalf("%q: reason = %q, want the unscoped-command reason", cmd, d.Reason)
		}
	}
}

// A glob's match set is a function of the tree, and the tree is what moved.
func TestScopeRefusesGlob(t *testing.T) {
	seed := map[string]string{
		"src/auth.py":           "def login(): pass\n",
		"tests/test_billing.py": "def test_total(): assert True\n",
	}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "src/auth.py", "def login(): return 42\n")
	})

	for _, cmd := range []string{
		"wc -l tests/*.py",
		"wc -l tests/test_?.py",
		"wc -l tests/test_[ab].py",
		"wc -l tests/{a,b}.py",
		"wc -l 'tests/*.py'",
	} {
		d := ScopeMatch(root, recorded, current, cmd)
		scopeCheck(t, d, false)
		if !strings.Contains(d.Reason, "glob") {
			t.Fatalf("%q: reason = %q, want a glob refusal", cmd, d.Reason)
		}
	}
}

// git reports on the whole tree and index no matter what you ask it about.
func TestScopeRefusesWholeTreeCommands(t *testing.T) {
	seed := map[string]string{
		"src/auth.py":           "def login(): pass\n",
		"tests/test_billing.py": "def test_total(): assert True\n",
	}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "src/auth.py", "def login(): return 42\n")
	})

	for _, cmd := range []string{
		"git status --porcelain",
		"git diff tests/test_billing.py",
		"git log --oneline tests/test_billing.py",
		"find tests -name test_billing.py",
		"ls tests",
		"tree tests",
		"make test",
		"go test ./tests",
		"npm test tests/test_billing.py",
		"tsc tests/test_billing.py",
		"mypy tests/test_billing.py",
		// The refuse-list is checked past the head, because a wrapper hides
		// the real command one token in.
		"uv run make test",
		"timeout 60 find tests -name x",
		"bash -c 'pytest tests/test_billing.py'",
	} {
		scopeCheck(t, ScopeMatch(root, recorded, current, cmd), false)
	}
}

func TestScopeStripsPytestNodeID(t *testing.T) {
	seed := map[string]string{
		"src/auth.py":     "def login(): pass\n",
		"tests/test_x.py": "def test_one(): assert True\n",
		"tests/test_y.py": "def test_two(): assert True\n",
	}

	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "src/auth.py", "def login(): return 42\n")
	})
	d := ScopeMatch(root, recorded, current, "wc -l tests/test_x.py::test_one")
	scopeCheck(t, d, true)
	if !scopeEqual(d.ScopePaths, []string{"tests/test_x.py"}) {
		t.Fatalf("scope paths = %v, want [tests/test_x.py]; the node id must reduce to its file", d.ScopePaths)
	}

	d = ScopeMatch(root, recorded, current, "wc -l tests/test_x.py::TestClass::test_method")
	scopeCheck(t, d, true)
	if !scopeEqual(d.ScopePaths, []string{"tests/test_x.py"}) {
		t.Fatalf("scope paths = %v, want [tests/test_x.py]", d.ScopePaths)
	}

	// The file the node id names is still the scope, so changing it refuses.
	root2, recorded2, current2 := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "tests/test_x.py", "def test_one(): assert False\n")
	})
	scopeCheck(t, ScopeMatch(root2, recorded2, current2, "wc -l tests/test_x.py::test_one"), false)
}

// A deletion is a change. diff-tree names deleted paths, which is what we want.
func TestScopeRefusesDeletedFileInScope(t *testing.T) {
	seed := map[string]string{
		"src/auth.py":        "def login(): pass\n",
		"tests/test_gone.py": "def test_gone(): assert True\n",
	}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeRemove(t, root, "tests/test_gone.py")
	})

	d := ScopeMatch(root, recorded, current, "wc -l tests/test_gone.py")
	scopeCheck(t, d, false)
	if !scopeEqual(d.ChangedPaths, []string{"tests/test_gone.py"}) {
		t.Fatalf("changed paths = %v, want the deleted file", d.ChangedPaths)
	}
}

// A deleted file with no extension and no slash is the case that would slip
// through a pure looks-like-a-path test: nothing on disk to find, no suffix to
// recognize. It must still refuse rather than be dismissed as a bare word.
func TestScopeRefusesDeletedExtensionlessArgument(t *testing.T) {
	seed := map[string]string{
		"LICENSE":     "MIT\n",
		"src/auth.py": "def login(): pass\n",
	}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeRemove(t, root, "LICENSE")
	})

	d := ScopeMatch(root, recorded, current, "cat LICENSE")
	scopeCheck(t, d, false)
	if !strings.Contains(d.Reason, "LICENSE") {
		t.Fatalf("reason = %q, want it to name the argument it could not classify", d.Reason)
	}
}

// Tier 0 already handles this. Reaching Tier 1 with equal trees means a caller
// bug, and promoting would hide it.
func TestScopeRefusesIdenticalTrees(t *testing.T) {
	root := newRepo(t)
	tree := scopeTree(t, root)

	d := ScopeMatch(root, tree, tree, "wc -l tests/test_billing.py")
	scopeCheck(t, d, false)
	if !strings.Contains(d.Reason, "identical trees") {
		t.Fatalf("reason = %q, want the identical-trees guard", d.Reason)
	}
}

func TestScopeRefusesGarbageTreeHash(t *testing.T) {
	seed := map[string]string{"tests/test_billing.py": "def test_total(): assert True\n"}
	root, _, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "src/auth.py", "def login(): pass\n")
	})

	cases := []string{
		"not-a-tree",
		"",
		"--output=/tmp/pwned",
		"HEAD^{tree}",
		strings.Repeat("a", 40), // well-formed and absent from the object store
		strings.Repeat("b", 64),
	}
	for _, bad := range cases {
		scopeCheck(t, ScopeMatch(root, bad, current, "wc -l tests/test_billing.py"), false)
		scopeCheck(t, ScopeMatch(root, current, bad, "wc -l tests/test_billing.py"), false)
	}
	// A root that is not a git repo at all must fail soft too.
	scopeCheck(t, ScopeMatch(t.TempDir(), current, strings.Repeat("c", 40),
		"wc -l tests/test_billing.py"), false)
	scopeCheck(t, ScopeMatch("", current, strings.Repeat("c", 40),
		"wc -l tests/test_billing.py"), false)
}

// A rename must surface both the old and the new path. With rename detection
// on, --name-only prints only the destination, which would hide the deletion
// of the scoped file and promote a command whose input no longer exists.
func TestScopeSeesBothSidesOfARename(t *testing.T) {
	seed := map[string]string{
		"src/auth.py":     "def login(): pass\n",
		"tests/test_a.py": "def test_a(): assert True\n",
	}
	root := newRepo(t)
	for rel, body := range seed {
		scopeWrite(t, root, rel, body)
	}
	mustRun(t, root, "git", "config", "diff.renames", "true")
	recorded := scopeTree(t, root)

	mustRun(t, root, "git", "mv", "tests/test_a.py", "tests/test_b.py")
	current := scopeTree(t, root)

	scopeCheck(t, ScopeMatch(root, recorded, current, "wc -l tests/test_a.py"), false)
}

// A change to a file that configures the toolchain cannot be proven irrelevant
// to anything, and segment comparison would happily call it disjoint.
func TestScopeRefusesToolchainConfigChange(t *testing.T) {
	seed := map[string]string{
		"tests/test_billing.py": "def test_total(): assert True\n",
		"src/auth.py":           "def login(): pass\n",
	}

	for _, cfg := range []string{"tests/conftest.py", "pyproject.toml", ".gitignore", "Makefile"} {
		root, recorded, current := scopeRepo(t, seed, func(root string) {
			scopeWrite(t, root, cfg, "# changed\n")
		})
		d := ScopeMatch(root, recorded, current, "wc -l tests/test_billing.py")
		scopeCheck(t, d, false)
		if !strings.Contains(d.Reason, cfg) {
			t.Fatalf("%s: reason = %q, want it to name the config file", cfg, d.Reason)
		}
	}
}

// Quoting is the tokenizer's job, and we reuse the one the classifier uses so
// the two cannot disagree about what a command is.
func TestScopeRespectsQuoting(t *testing.T) {
	seed := map[string]string{
		"src/auth.py":           "def login(): pass\n",
		"tests/test billing.py": "def test_total(): assert True\n",
	}

	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "src/auth.py", "def login(): return 42\n")
	})
	d := ScopeMatch(root, recorded, current, `wc -l "tests/test billing.py"`)
	scopeCheck(t, d, true)
	if !scopeEqual(d.ScopePaths, []string{"tests/test billing.py"}) {
		t.Fatalf("scope paths = %v, want the quoted path intact", d.ScopePaths)
	}

	root2, recorded2, current2 := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "tests/test billing.py", "def test_total(): assert False\n")
	})
	scopeCheck(t, ScopeMatch(root2, recorded2, current2, `wc -l 'tests/test billing.py'`), false)
	scopeCheck(t, ScopeMatch(root2, recorded2, current2, "wc -l tests/test\\ billing.py"), false)
}

// The chain rule from AGENTS.md: the scope is the union of every segment, and
// a refusal in any segment refuses the whole command.
func TestScopeAppliesTheChainRule(t *testing.T) {
	seed := map[string]string{
		"src/app.py":            "x = 1\n",
		"tests/test_billing.py": "def test_total(): assert True\n",
		"docs/notes.md":         "hello\n",
	}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "docs/notes.md", "goodbye\n")
	})

	d := ScopeMatch(root, recorded, current, "wc -l tests/test_billing.py && wc -l src")
	scopeCheck(t, d, true)
	if !scopeEqual(d.ScopePaths, []string{"src", "tests/test_billing.py"}) {
		t.Fatalf("scope paths = %v, want the union of both segments", d.ScopePaths)
	}

	scopeCheck(t, ScopeMatch(root, recorded, current,
		"wc -l tests/test_billing.py && git status"), false)

	// A filter downstream of a pipe reads the pipe, so the pipeline stays
	// bounded by the segment that does read the tree.
	scopeCheck(t, ScopeMatch(root, recorded, current,
		"wc -l tests/test_billing.py | head -5"), true)
	scopeCheck(t, ScopeMatch(root, recorded, current,
		"wc -l tests/test_billing.py | grep -c PASS"), true)

	// But an unscoped segment anywhere reads the whole tree, and the union of
	// the other segments' paths must not paper over it.
	for _, cmd := range []string{
		"wc -l tests/test_billing.py; pytest",
		"cat src/app.py && pytest",
		"wc -l tests/test_billing.py | python -c 'import sys'",
		"head -5 | pytest tests/test_billing.py",
	} {
		scopeCheck(t, ScopeMatch(root, recorded, current, cmd), false)
	}
}

// Anything the shell would rewrite before the command sees it is not a path we
// can read off the string.
func TestScopeRefusesUnresolvableArguments(t *testing.T) {
	seed := map[string]string{
		"src/auth.py":           "def login(): pass\n",
		"tests/test_billing.py": "def test_total(): assert True\n",
	}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "src/auth.py", "def login(): return 42\n")
	})

	for _, cmd := range []string{
		"wc -l $DIR/test_billing.py",
		"wc -l ~/tests/test_billing.py",
		"wc -l $(ls tests)",
		"wc -l /tmp/elsewhere/test_billing.py",
		"wc -l " + filepath.Join(root, "tests", "test_billing.py"),
		"wc -l ../sibling/tests/test_billing.py",
		"wc -l tests/test_billing.py > out.txt",
		"wc -l --rootdir=tests/other tests/test_billing.py",
		"wc -l .",
	} {
		scopeCheck(t, ScopeMatch(root, recorded, current, cmd), false)
	}

	// 2>&1 duplicates a descriptor rather than writing a file, so it is not a
	// redirection in the sense that matters and must not cost the hit.
	scopeCheck(t, ScopeMatch(root, recorded, current, "wc -l tests/test_billing.py 2>&1"), true)
}

// Path arguments are relative to where the command ran. The frozen ScopeMatch
// signature has no working directory, so it assumes the repo root; the guard
// that keeps the assumption from becoming a wrong answer is that a path naming
// nothing in either tree refuses.
func TestScopeResolvesPathsAgainstTheWorkingDirectory(t *testing.T) {
	seed := map[string]string{
		"src/auth.py":         "def login(): pass\n",
		"sub/tests/test_a.py": "def test_a(): assert True\n",
	}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "src/auth.py", "def login(): return 42\n")
	})

	scopeCheck(t, ScopeMatchAt(root, "sub", recorded, current, "wc -l tests/test_a.py"), true)

	d := ScopeMatch(root, recorded, current, "wc -l tests/test_a.py")
	scopeCheck(t, d, false)
	if !strings.Contains(d.Reason, "names nothing in either tree") {
		t.Fatalf("reason = %q, want the unresolvable-path guard", d.Reason)
	}

	scopeCheck(t, ScopeMatchAt(root, "../outside", recorded, current, "wc -l tests/test_a.py"), false)
	scopeCheck(t, ScopeMatchAt(root, "/etc", recorded, current, "wc -l tests/test_a.py"), false)
}

// Flags are never scoped, but they must not cost the hit either. Only a flag
// that appears to carry a path refuses, because what a tool does with a path
// flag is a per-tool question we decline to answer.
func TestScopeIgnoresOrdinaryFlags(t *testing.T) {
	seed := map[string]string{
		"src/auth.py":           "def login(): pass\n",
		"tests/test_billing.py": "def test_total(): assert True\n",
	}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "src/auth.py", "def login(): return 42\n")
	})

	for _, cmd := range []string{
		"wc -l -q tests/test_billing.py",
		"wc -l --tb=short -q tests/test_billing.py",
		"wc -l -x --maxfail=1 tests/test_billing.py",
		"wc -l -k not_slow tests/test_billing.py",
		"wc -l -- tests/test_billing.py",
	} {
		d := ScopeMatch(root, recorded, current, cmd)
		scopeCheck(t, d, true)
		if !scopeEqual(d.ScopePaths, []string{"tests/test_billing.py"}) {
			t.Fatalf("%q: scope paths = %v, want [tests/test_billing.py]", cmd, d.ScopePaths)
		}
	}
}

// A pattern is not a path. grep TODO src/ must scope src/ and ignore TODO,
// otherwise no grep would ever promote.
func TestScopeIgnoresPatternsButScopesDirectories(t *testing.T) {
	seed := map[string]string{
		"src/auth.py":   "def login(): pass\n",
		"docs/notes.md": "hello\n",
	}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "docs/notes.md", "goodbye\n")
	})

	d := ScopeMatch(root, recorded, current, "wc -l TODO src/")
	scopeCheck(t, d, true)
	if !scopeEqual(d.ScopePaths, []string{"src"}) {
		t.Fatalf("scope paths = %v, want [src]; TODO is a pattern, not a path", d.ScopePaths)
	}
}

// Whatever the verdict, the log line has to be actionable.
func TestScopeAlwaysExplainsItself(t *testing.T) {
	seed := map[string]string{
		"src/auth.py":           "def login(): pass\n",
		"tests/test_billing.py": "def test_total(): assert True\n",
	}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "src/auth.py", "def login(): return 42\n")
	})

	for _, cmd := range []string{
		"", "   ", "wc", "wc -l tests/test_billing.py", "git status",
		"wc -l 'unterminated", "FOO=1", "FOO=1 pytest tests/test_billing.py",
		"&& pytest", "|", "wc -l tests/*.py", "./", "-q",
	} {
		d := ScopeMatch(root, recorded, current, cmd)
		if strings.TrimSpace(d.Reason) == "" {
			t.Fatalf("%q produced a decision with no reason", cmd)
		}
	}
}

// Determinism matters: the same inputs must not sometimes serve and sometimes
// miss, or the failure would be unreproducible.
func TestScopeIsDeterministic(t *testing.T) {
	seed := map[string]string{
		"src/auth.py":           "def login(): pass\n",
		"tests/test_billing.py": "def test_total(): assert True\n",
	}
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "src/auth.py", "def login(): return 42\n")
	})

	first := ScopeMatch(root, recorded, current, "wc -l tests/test_billing.py")
	for i := 0; i < 5; i++ {
		got := ScopeMatch(root, recorded, current, "wc -l tests/test_billing.py")
		if got.Promoted != first.Promoted || got.Reason != first.Reason ||
			!scopeEqual(got.ChangedPaths, first.ChangedPaths) ||
			!scopeEqual(got.ScopePaths, first.ScopePaths) {
			t.Fatalf("run %d disagreed with the first: %+v vs %+v", i, got, first)
		}
	}
}

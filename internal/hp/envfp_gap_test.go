package hp

import (
	"os"
	"path/filepath"
	"testing"
)

// writeNodeModules fakes an installed JS dependency tree. Like .venv it is
// gitignored, so the tree hash cannot see it.
func writeNodeModules(t *testing.T, root string, pkgs ...string) {
	t.Helper()
	for _, p := range pkgs {
		dir := filepath.Join(root, "node_modules", p)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "package.json"),
			[]byte(`{"name":"`+p+`","version":"1.0.0"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestEnvFingerprintSeesNodeModules is the same test as the Python one, for a
// JS project.
//
// The Python case is covered because the fingerprint reads pyvenv.cfg and
// site-packages. Nothing covers node_modules, go module cache, Cargo
// registry, or bundler. For any non-Python repo the tree hash cannot see the
// installed dependencies and neither can the fingerprint, so two worktrees
// with identical trees and different dependencies share a key — and the cache
// serves one project's output into the other.
//
// That is not a missed hit. It is the wrong-answer failure mode the whole
// design exists to prevent.
func TestEnvFingerprintSeesNodeModules(t *testing.T) {
	a, b := newRepo(t), newRepo(t)
	for _, root := range []string{a, b} {
		if err := os.WriteFile(filepath.Join(root, ".gitignore"),
			[]byte(".venv/\nnode_modules/\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "package.json"),
			[]byte(`{"name":"app","dependencies":{"left-pad":"1.3.0"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		mustRun(t, root, "git", "add", "-A")
		mustRun(t, root, "git", "commit", "-q", "-m", "js")
	}
	writeNodeModules(t, a, "left-pad")
	writeNodeModules(t, b, "left-pad", "lodash", "express")

	wsA, err := NewWorkspace(a)
	if err != nil {
		t.Fatal(err)
	}
	wsB, err := NewWorkspace(b)
	if err != nil {
		t.Fatal(err)
	}
	stA, err := wsA.State()
	if err != nil {
		t.Fatal(err)
	}
	stB, err := wsB.State()
	if err != nil {
		t.Fatal(err)
	}

	if stA.Tree != stB.Tree {
		t.Fatalf("precondition failed: trees should be identical, got %s vs %s", stA.Tree, stB.Tree)
	}
	if stA.EnvFP == stB.EnvFP {
		t.Fatal("different node_modules produced the same env fingerprint; " +
			"a cache built on this would serve one project's output into the other")
	}
}

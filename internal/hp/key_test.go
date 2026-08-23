package hp

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func mustRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v in %s: %v\n%s", args, dir, err, out)
	}
}

// newRepo builds a git repo that gitignores .venv, which is what makes the
// virtualenv invisible to the tree hash.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustRun(t, dir, "git", "init", "-q")
	mustRun(t, dir, "git", "config", "user.email", "t@example.com")
	mustRun(t, dir, "git", "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".venv/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-q", "-m", "init")
	return dir
}

// writeVenv fakes an installed package set.
func writeVenv(t *testing.T, root string, dists ...string) {
	t.Helper()
	sp := filepath.Join(root, ".venv", "lib", "python3.12", "site-packages")
	if err := os.MkdirAll(sp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".venv", "pyvenv.cfg"), []byte("version = 3.12.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range dists {
		if err := os.MkdirAll(filepath.Join(sp, d+".dist-info"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// TestEnvFingerprintDistinguishesVenvs is the regression test for the one hole
// that can make Hindsight serve a wrong answer.
//
// Two worktrees with byte-identical git trees but different installed packages
// must not share a cache key. The tree hash cannot see the difference, because
// .venv is gitignored, so the whole burden falls on the env fingerprint.
func TestEnvFingerprintDistinguishesVenvs(t *testing.T) {
	a, b := newRepo(t), newRepo(t)
	writeVenv(t, a, "requests-2.31.0", "urllib3-2.0.0")
	writeVenv(t, b, "requests-2.31.0", "urllib3-2.0.0", "numpy-1.26.0")

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
		t.Fatal("different installed packages produced the same env fingerprint; " +
			"a cache built on this would serve one venv's output to the other")
	}
	if Key(stA, ".", "pytest -q") == Key(stB, ".", "pytest -q") {
		t.Fatal("identical keys despite different environments")
	}
}

// TestEnvFingerprintStableForSameVenv guards the opposite failure: if the
// fingerprint moves when nothing meaningful changed, sharing drops to zero and
// the cache is useless.
func TestEnvFingerprintStableForSameVenv(t *testing.T) {
	root := newRepo(t)
	writeVenv(t, root, "requests-2.31.0")
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	first := ws.EnvFingerprint()
	for i := 0; i < 5; i++ {
		if got := ws.EnvFingerprint(); got != first {
			t.Fatalf("fingerprint unstable: %s != %s", got, first)
		}
	}
}

// TestConcurrentWorktreeKeys checks that agents in separate worktrees compute
// keys simultaneously without corrupting each other.
//
// Each worktree must resolve its own git dir and therefore its own side index.
// If they shared one index file, concurrent "git add -A" would interleave and
// produce trees that describe no real state, which is a wrong key and
// therefore a wrong answer.
func TestConcurrentWorktreeKeys(t *testing.T) {
	root := newRepo(t)
	base := t.TempDir()
	const n = 5

	var trees [n]string
	for i := 0; i < n; i++ {
		wt := filepath.Join(base, "a"+string(rune('1'+i)))
		mustRun(t, root, "git", "worktree", "add", "-q", "-f", wt, "-b", "agent"+string(rune('1'+i)), "HEAD")
		trees[i] = wt
	}

	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	indexes := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ws, err := NewWorkspace(trees[i])
			if err != nil {
				errs[i] = err
				return
			}
			indexes[i] = ws.IndexPath
			st, err := ws.State()
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = st.Tree
		}(i)
	}
	wg.Wait()

	seenIndex := map[string]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("worktree %d failed: %v", i, errs[i])
		}
		if seenIndex[indexes[i]] {
			t.Fatalf("worktree %d shares a side index with a peer: %s", i, indexes[i])
		}
		seenIndex[indexes[i]] = true
		if results[i] != results[0] {
			t.Fatalf("worktree %d computed %s, worktree 0 computed %s; identical content must hash identically",
				i, results[i], results[0])
		}
	}
}

// TestSideIndexLeavesRealIndexAlone: computing a key must not disturb the
// user's git state.
func TestSideIndexLeavesRealIndexAlone(t *testing.T) {
	root := newRepo(t)
	if err := os.WriteFile(filepath.Join(root, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	status := func() string {
		cmd := exec.Command("git", "status", "--porcelain")
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}

	before := status()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.TreeHash(); err != nil {
		t.Fatal(err)
	}
	if after := status(); after != before {
		t.Fatalf("git status changed after hashing:\nbefore: %q\nafter:  %q", before, after)
	}
}

func TestKeyIncludesCwdAndCommand(t *testing.T) {
	st := State{Tree: "t", EnvFP: "e"}
	if Key(st, ".", "pytest") == Key(st, "sub", "pytest") {
		t.Fatal("cwd must be part of the key; the same command means different things elsewhere")
	}
	if Key(st, ".", "pytest") == Key(st, ".", "pytest -q") {
		t.Fatal("command must be part of the key")
	}
	// Key hashes what it is given; normalization is the caller's job, so two
	// spellings only share a key once both have gone through NormalizeCommand.
	if Key(st, ".", NormalizeCommand("pytest  -q")) != Key(st, ".", NormalizeCommand("pytest   -q")) {
		t.Fatal("normalized spellings of one command must share a key")
	}
	if Key(st, ".", "pytest  -q") == Key(st, ".", "pytest -q") {
		t.Fatal("Key must not silently normalize; callers rely on it hashing exactly what it is given")
	}
}

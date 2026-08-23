package hp

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// treeAfterWriting commits a file and returns the resulting tree hash, so
// tests operate on real git objects rather than invented strings.
func treeAfterWriting(t *testing.T, root, rel, content string) string {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, root, "git", "add", "-A")
	mustRun(t, root, "git", "commit", "-q", "-m", "change "+rel)
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := ws.TreeHash()
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// TestTier1PromotesThroughTheDaemon is the end-to-end version of the claim
// that matters most.
//
// Exact-tree matching collapses the moment agents start editing: measured
// cross-agent reuse falls from 16.9% over the first three commands to 1.0%
// after step fifty, purely because no two agents share a tree any more. Tier-1
// is the only mechanism that recovers those hits, so it has to work through
// the real lookup path, not just as a unit.
func TestTier1PromotesThroughTheDaemon(t *testing.T) {
	root := newRepo(t)
	treeA := treeAfterWriting(t, root, "tests/test_billing.py", "def test_billing(): pass\n")

	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(store)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	const cmd = "pytest tests/test_billing.py"
	blob, err := store.PutBlob([]byte("1 passed\n"))
	if err != nil {
		t.Fatal(err)
	}

	// Agent 1 runs the suite at tree A and records it.
	post(t, ts.URL+"/record", &Record{
		Agent: "a1", Key: "hs-v1:treeA", Cmd: cmd, CmdNorm: NormalizeCommand(cmd),
		CwdRel: ".", TreeBefore: treeA, EnvFPBefore: "env1",
		Decision: DecisionMiss, Servable: true, ExitCode: 0, DurationMS: 9000,
		StdoutBlob: blob, StderrBlob: blob,
	}, nil)

	// A peer edits something the command does not read.
	treeB := treeAfterWriting(t, root, "src/auth.py", "def login(): pass\n")
	if treeA == treeB {
		t.Fatal("precondition failed: the edit should have moved the tree")
	}

	var resp LookupResp
	post(t, ts.URL+"/lookup", LookupReq{
		Key: "hs-v1:treeB", Agent: "a2", Cmd: cmd, CmdNorm: NormalizeCommand(cmd),
		CwdRel: ".", Tree: treeB, EnvFP: "env1", Policy: "SERVE",
		Serve: true, RepoRoot: root,
	}, &resp)

	if resp.Decision != DecisionHit {
		t.Fatalf("expected a tier-1 hit after a disjoint peer edit, got %s", resp.Decision)
	}
	if resp.Tier != 1 {
		t.Fatalf("expected tier 1, got %d", resp.Tier)
	}
	if resp.SourceAgent != "a1" {
		t.Fatalf("served from %q, want a1", resp.SourceAgent)
	}
	if resp.ScopeReason == "" {
		t.Fatal("a tier-1 promotion must explain itself")
	}
	if got := srv.stats().Tier1; got != 1 {
		t.Fatalf("tier-1 hits should be counted separately, got %d", got)
	}
}

// TestTier1RefusesWhenTheScopedFileChanged: the same setup, but the peer edits
// the very file the command reads. Promoting here would serve a stale test
// result for code that changed, which is the wrong answer this whole design
// exists to prevent.
func TestTier1RefusesWhenTheScopedFileChanged(t *testing.T) {
	root := newRepo(t)
	treeA := treeAfterWriting(t, root, "tests/test_billing.py", "def test_billing(): pass\n")

	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(store)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	const cmd = "pytest tests/test_billing.py"
	blob, _ := store.PutBlob([]byte("1 passed\n"))
	post(t, ts.URL+"/record", &Record{
		Agent: "a1", Key: "hs-v1:treeA", Cmd: cmd, CmdNorm: NormalizeCommand(cmd),
		CwdRel: ".", TreeBefore: treeA, EnvFPBefore: "env1",
		Decision: DecisionMiss, Servable: true, DurationMS: 9000,
		StdoutBlob: blob, StderrBlob: blob,
	}, nil)

	treeB := treeAfterWriting(t, root, "tests/test_billing.py", "def test_billing(): assert False\n")

	var resp LookupResp
	post(t, ts.URL+"/lookup", LookupReq{
		Key: "hs-v1:treeB", Agent: "a2", Cmd: cmd, CmdNorm: NormalizeCommand(cmd),
		CwdRel: ".", Tree: treeB, EnvFP: "env1", Policy: "SERVE",
		Serve: true, RepoRoot: root,
	}, &resp)

	if resp.Decision == DecisionHit {
		t.Fatal("promoted a hit even though the scoped file itself changed; " +
			"this would serve a stale result for code that moved")
	}
}

// TestTier1RespectsEnvironment: a different environment must not be promoted,
// no matter how disjoint the file changes are. The env fingerprint is not part
// of what disjointness can prove anything about.
func TestTier1RespectsEnvironment(t *testing.T) {
	root := newRepo(t)
	treeA := treeAfterWriting(t, root, "tests/test_billing.py", "def test_billing(): pass\n")

	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(store)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	const cmd = "pytest tests/test_billing.py"
	blob, _ := store.PutBlob([]byte("1 passed\n"))
	post(t, ts.URL+"/record", &Record{
		Agent: "a1", Key: "hs-v1:treeA", Cmd: cmd, CmdNorm: NormalizeCommand(cmd),
		CwdRel: ".", TreeBefore: treeA, EnvFPBefore: "env1",
		Decision: DecisionMiss, Servable: true, DurationMS: 9000,
		StdoutBlob: blob, StderrBlob: blob,
	}, nil)

	treeB := treeAfterWriting(t, root, "src/auth.py", "def login(): pass\n")

	var resp LookupResp
	post(t, ts.URL+"/lookup", LookupReq{
		Key: "hs-v1:treeB", Agent: "a2", Cmd: cmd, CmdNorm: NormalizeCommand(cmd),
		CwdRel: ".", Tree: treeB, EnvFP: "env2-different-packages", Policy: "SERVE",
		Serve: true, RepoRoot: root,
	}, &resp)

	if resp.Decision == DecisionHit {
		t.Fatal("promoted across different environments; disjoint file changes " +
			"say nothing about different installed dependencies")
	}
}

// TestTier1EvictionAlsoClearsCandidates: a record evicted by shadow
// verification must not come back through the Tier-1 path.
func TestTier1EvictionAlsoClearsCandidates(t *testing.T) {
	root := newRepo(t)
	treeA := treeAfterWriting(t, root, "tests/test_billing.py", "def test_billing(): pass\n")

	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(store)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	const cmd = "pytest tests/test_billing.py"
	blob, _ := store.PutBlob([]byte("1 passed\n"))
	post(t, ts.URL+"/record", &Record{
		Agent: "a1", Key: "hs-v1:treeA", Cmd: cmd, CmdNorm: NormalizeCommand(cmd),
		CwdRel: ".", TreeBefore: treeA, EnvFPBefore: "env1",
		Decision: DecisionMiss, Servable: true, DurationMS: 9000,
		StdoutBlob: blob, StderrBlob: blob,
	}, nil)

	post(t, ts.URL+"/verify", VerifyVerdict{
		Key: "hs-v1:treeA", OK: false, Detail: "stdout diverged",
	}, nil)

	treeB := treeAfterWriting(t, root, "src/auth.py", "def login(): pass\n")

	var resp LookupResp
	post(t, ts.URL+"/lookup", LookupReq{
		Key: "hs-v1:treeB", Agent: "a2", Cmd: cmd, CmdNorm: NormalizeCommand(cmd),
		CwdRel: ".", Tree: treeB, EnvFP: "env1", Policy: "SERVE",
		Serve: true, RepoRoot: root,
	}, &resp)

	if resp.Decision == DecisionHit {
		t.Fatal("an evicted record came back through tier-1; eviction must " +
			"clear both indexes or shadow verification is decorative")
	}
}

package hp

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

// TestEnvFingerprintIgnoresWorktreeIdentity is the regression test for the bug
// that made Hindsight structurally useless on Python fan-outs.
//
// `python -m venv` writes `command = ... -m venv /abs/path/a1/.venv` into
// pyvenv.cfg, and `uv venv` writes `prompt = a1`. Both name the worktree. We
// hashed the file verbatim, so five worktrees of one project produced five
// different fingerprints for byte-identical environments, every key was
// unique, and the hit rate was zero by construction.
//
// It was invisible because it looks exactly like agents diverging. A real
// five-agent run on SQLAlchemy is what surfaced it.
func TestEnvFingerprintIgnoresWorktreeIdentity(t *testing.T) {
	const shared = "home = /opt/homebrew/opt/python@3.14/bin\n" +
		"include-system-site-packages = false\n" +
		"version = 3.14.6\n" +
		"executable = /opt/homebrew/Cellar/python@3.14/3.14.6/bin/python3.14\n"

	fp := func(cfg string) string {
		venv := t.TempDir()
		if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		hashPyvenvCfg(venv, h)
		return string(h.Sum(nil))
	}

	a1 := fp(shared + "command = /usr/bin/python3 -m venv /tmp/fleet/a1/.venv\nprompt = a1\n")
	a2 := fp(shared + "command = /usr/bin/python3 -m venv /tmp/fleet/a2/.venv\nprompt = a2\n")
	if a1 != a2 {
		t.Fatal("two worktrees of one project produced different fingerprints for " +
			"identical environments; every key would be unique and nothing could ever hit")
	}

	// The parts that genuinely change behaviour must still move the hash.
	if fp(shared) == fp("home = /usr/bin\nversion = 3.11.0\n") {
		t.Fatal("a different interpreter must produce a different fingerprint")
	}
	if a1 == fp(shared+"include-system-site-packages = true\n") {
		t.Fatal("system site-packages visibility changes what imports resolve and must be in the key")
	}
}

// TestEnvFingerprintIsFieldOrderIndependent: the order a tool happens to write
// its config in is not a property of the environment.
func TestEnvFingerprintIsFieldOrderIndependent(t *testing.T) {
	fp := func(cfg string) string {
		venv := t.TempDir()
		if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		hashPyvenvCfg(venv, h)
		return string(h.Sum(nil))
	}
	forward := fp("home = /a\nversion = 3.12.0\ninclude-system-site-packages = false\n")
	reverse := fp("include-system-site-packages = false\nversion = 3.12.0\nhome = /a\n")
	if forward != reverse {
		t.Fatal("field order changed the fingerprint")
	}
}

// TestEnvFingerprintMatchesAcrossRealWorktrees builds two actual virtualenvs
// at different paths and asserts they agree. The unit tests above pin the
// parsing; this pins the thing we actually care about.
func TestEnvFingerprintMatchesAcrossRealWorktrees(t *testing.T) {
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("no system python3")
	}
	// newRepo already gitignores .venv, which is the precondition that makes
	// the venv invisible to the tree hash and the fingerprint's whole job.
	a, b := newRepo(t), newRepo(t)
	for _, root := range []string{a, b} {
		mustRun(t, root, "/usr/bin/python3", "-m", "venv", filepath.Join(root, ".venv"))
	}

	wsA, err := NewWorkspace(a)
	if err != nil {
		t.Fatal(err)
	}
	wsB, err := NewWorkspace(b)
	if err != nil {
		t.Fatal(err)
	}
	fpA, okA := wsA.EnvFingerprint()
	fpB, okB := wsB.EnvFingerprint()
	if !okA || !okB {
		t.Fatalf("fingerprints incomplete: %v %v", okA, okB)
	}
	if fpA != fpB {
		t.Fatalf("two real venvs at different paths disagree (%s vs %s); "+
			"a fan-out would get no hits at all", fpA, fpB)
	}
}

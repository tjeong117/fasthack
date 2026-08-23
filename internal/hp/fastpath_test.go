package hp

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFastpathNeedsRepeatedEvidence: one lucky fast run must not permanently
// stop a normally-expensive command from being cached.
func TestFastpathNeedsRepeatedEvidence(t *testing.T) {
	f := LoadFastpath(t.TempDir())
	f.Observe("pytest -q", 2)
	if f.KnownFast("pytest -q", 50) {
		t.Fatal("one observation should not be enough to disable caching")
	}
	f.Observe("pytest -q", 2)
	f.Observe("pytest -q", 2)
	if !f.KnownFast("pytest -q", 50) {
		t.Fatal("three consistent fast observations should mark it not worth intercepting")
	}
}

// TestFastpathTakesTheWorstCase: a command that is *ever* slow keeps being
// cached. Abstaining from the memo only wastes overhead; wrongly abstaining
// from the cache wastes real execution.
func TestFastpathTakesTheWorstCase(t *testing.T) {
	f := LoadFastpath(t.TempDir())
	for i := 0; i < 5; i++ {
		f.Observe("make", 3)
	}
	if !f.KnownFast("make", 50) {
		t.Fatal("consistently fast command should be memoized")
	}
	f.Observe("make", 9000) // a real build, nothing cached
	if f.KnownFast("make", 50) {
		t.Fatal("a single expensive run must put the command back in play")
	}
}

func TestFastpathRoundTrips(t *testing.T) {
	home := t.TempDir()
	f := LoadFastpath(home)
	for i := 0; i < 3; i++ {
		f.Observe("echo hi", 1)
	}
	f.Save()

	if _, err := os.Stat(filepath.Join(home, "fastpath.json")); err != nil {
		t.Fatalf("memo not written: %v", err)
	}
	if !LoadFastpath(home).KnownFast("echo hi", 50) {
		t.Fatal("memo did not survive a reload")
	}
}

func TestFastpathCorruptFileIsEmptyNotFatal(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "fastpath.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if LoadFastpath(home).KnownFast("echo hi", 50) {
		t.Fatal("a corrupt memo must behave as an empty one")
	}
}

// TestFastpathDisabled: a zero floor turns the whole mechanism off, so the
// behaviour can always be recovered by configuration.
func TestFastpathDisabled(t *testing.T) {
	f := LoadFastpath(t.TempDir())
	for i := 0; i < 5; i++ {
		f.Observe("echo hi", 1)
	}
	if f.KnownFast("echo hi", 0) {
		t.Fatal("a floor of zero must disable the fastpath")
	}
}

func TestHomeForCwdFindsRepoWithoutGit(t *testing.T) {
	t.Setenv("HP_HOME", "")
	root := newRepo(t)
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := HomeForCwd(nested), Home(root); got != want {
		t.Fatalf("HomeForCwd walked to %s, want %s", got, want)
	}
}

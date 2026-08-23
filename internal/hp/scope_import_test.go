package hp

import "testing"

// TestScopeAcrossAnImportEdge documents the soundness boundary of Tier-1.
//
// Tier-1 promotes when the changed paths are disjoint from the command's
// literal path arguments. For a command that reads only its arguments — cat,
// wc, md5sum — that is a proof. For a command that follows an import graph it
// is not: `pytest tests/test_billing.py` reads `src/billing.py` too, and that
// path appears nowhere in the command.
//
// So a peer editing src/billing.py leaves the literal argument untouched, the
// disjointness check passes, and a stale "1 passed" is served for code that
// changed. That is not a missed hit, it is a wrong answer, and it is the one
// outcome the whole design exists to prevent.
func TestScopeAcrossAnImportEdge(t *testing.T) {
	seed := map[string]string{
		"src/billing.py": "def total(): return 1\n",
		"tests/test_billing.py": "from src.billing import total\n" +
			"def test_total(): assert total() == 1\n",
	}
	// A peer changes the module under test. The test file itself is untouched.
	root, recorded, current := scopeRepo(t, seed, func(root string) {
		scopeWrite(t, root, "src/billing.py", "def total(): return 999\n")
	})

	d := ScopeMatch(root, recorded, current, "pytest tests/test_billing.py")
	if d.Promoted {
		t.Fatalf("promoted across an import edge: changed=%v scope=%v reason=%q\n\n"+
			"The recorded run passed against total()==1. The current code returns 999, "+
			"so the test now fails. Serving the recorded pass is a wrong answer.",
			d.ChangedPaths, d.ScopePaths, d.Reason)
	}
}

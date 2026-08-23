package hp

// Tier-1 dependency scoping: diff-disjoint promotion.
//
// Tier 0 is exact tree match, which is always sound and is what ships today.
// Its weakness is coarseness: if a peer edits src/auth.py, our
// `pytest tests/test_billing.py` misses even though nothing it reads changed.
// The corpus says this is where nearly all the value leaks — cross-agent reuse
// runs at 16.9% across the first three commands and 1.0% after step 50,
// because agents diverge and never share a tree again.
//
// Tier 1 takes a tree-key miss as a *candidate*, asks git which paths differ
// between the recorded tree and the current one, and promotes to a hit when
// those paths are provably irrelevant to the command.
//
// The distinction from similarity matching matters and is the whole reason
// this is safe: we are not claiming two states look alike, we are proving the
// difference cannot affect this command. A wrong scope is a wrong answer, so
// promotion is allowed only from literal path arguments actually present in
// the command — never from an inferred or guessed dependency set.

// ScopeDecision explains a Tier-1 outcome, for logging and for the viewer.
type ScopeDecision struct {
	Promoted     bool
	Reason       string
	ChangedPaths []string
	ScopePaths   []string
}

// ScopeMatch reports whether a candidate record recorded at recordedTree can
// be promoted to a hit for a command running at currentTree.
//
// STUB: never promotes, which degrades exactly to Tier 0.
func ScopeMatch(repoRoot, recordedTree, currentTree, cmd string) ScopeDecision {
	return ScopeDecision{Promoted: false, Reason: "tier-1 scoping not enabled"}
}

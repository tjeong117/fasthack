package hp

import (
	"encoding/json"
	"strings"
	"testing"
)

// trRecord builds a complete MISS record in memory. Tests start from a valid
// transition and break exactly one thing, so a failure names the thing.
func trRecord(mut func(*Record)) *Record {
	r := &Record{
		V: 1, TS: 1750000000, Agent: "a1",
		Cmd: "pytest -q", CmdNorm: "pytest -q", CwdRel: ".",
		TreeBefore: "tree-before", EnvFPBefore: "env-before",
		TreeAfter: "tree-before", EnvFPAfter: "env-before",
		Key: "hs-v1:abc", Policy: "SERVE", Decision: DecisionMiss,
		Servable: true, ExitCode: 0, DurationMS: 4634,
		StdoutBlob: "sha256:aa", StderrBlob: "sha256:bb",
	}
	if mut != nil {
		mut(r)
	}
	return r
}

func trLog(t *testing.T, recs ...*Record) string {
	t.Helper()
	var b strings.Builder
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestTransitionAdmitsOnlyRealExecutions is the leakage rule as a test.
//
// This is the one property of the corpus that cannot be recovered later. A
// served HIT looks identical to a real execution in every field a trainer
// would read — same command, same exit code, same duration — and it is a
// quotation of someone else's observation rather than an observation. If it
// leaks in, the corpus silently contains duplicated rows presented as
// independent evidence, and nothing downstream can tell.
func TestTransitionAdmitsOnlyRealExecutions(t *testing.T) {
	cases := []struct {
		decision string
		keep     bool
		reason   string
	}{
		{DecisionMiss, true, ""},
		{DecisionHit, false, ExcludeHit},
		{DecisionLeaseWait, false, ExcludeLeaseWait},
		{DecisionPassthrough, false, ExcludePassthrough},
		{"VERIFY", false, ExcludeVerify},
	}
	for _, c := range cases {
		t.Run(c.decision, func(t *testing.T) {
			rec := trRecord(func(r *Record) { r.Decision = c.decision })
			tr, reason, ok := TransitionFrom(rec)
			if ok != c.keep {
				t.Fatalf("decision %s: kept=%v, want %v (reason %q)", c.decision, ok, c.keep, reason)
			}
			if !c.keep {
				if reason != c.reason {
					t.Errorf("decision %s: reason = %q, want %q", c.decision, reason, c.reason)
				}
				if tr != (Transition{}) {
					t.Errorf("decision %s: excluded record still produced a transition", c.decision)
				}
				return
			}
			if tr.Schema != TransitionSchema {
				t.Errorf("schema = %q, want %q", tr.Schema, TransitionSchema)
			}
			if tr.Cmd != rec.Cmd || tr.CmdNorm != rec.CmdNorm || tr.CwdRel != rec.CwdRel {
				t.Errorf("action fields did not survive conversion: %+v", tr)
			}
			if tr.ExitCode != rec.ExitCode || tr.DurationMS != rec.DurationMS {
				t.Errorf("observed response did not survive conversion: %+v", tr)
			}
			if tr.Agent != rec.Agent || tr.Servable != rec.Servable {
				t.Errorf("agent or servable did not survive conversion: %+v", tr)
			}
		})
	}
}

// TestTransitionUnknownDecisionIsExcluded holds the default to exclusion. A
// decision this exporter has never heard of is not evidence that a command
// ran.
func TestTransitionUnknownDecisionIsExcluded(t *testing.T) {
	for _, d := range []string{"", "SHADOW", "miss", "MISS_MAYBE"} {
		_, reason, ok := TransitionFrom(trRecord(func(r *Record) { r.Decision = d }))
		if ok {
			t.Fatalf("decision %q was admitted; default must be exclusion", d)
		}
		if !strings.HasPrefix(reason, ExcludeUnknown) {
			t.Errorf("decision %q: reason = %q, want the unrecognised-decision reason", d, reason)
		}
	}
}

// TestTransitionMutatedIsMeasured checks the label that carries most of the
// value in the file. It is derived from the two states, never declared, which
// is the same argument as the purity gate: tsc emits .js and uv sync mutates a
// gitignored virtualenv, and neither announces it.
func TestTransitionMutatedIsMeasured(t *testing.T) {
	cases := []struct {
		name                     string
		treeAfter, envAfter      string
		mutated, treeMut, envMut bool
	}{
		{"nothing moved", "tree-before", "env-before", false, false, false},
		{"tree moved", "tree-after", "env-before", true, true, false},
		{"env moved only", "tree-before", "env-after", true, false, true},
		{"both moved", "tree-after", "env-after", true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, reason, ok := TransitionFrom(trRecord(func(r *Record) {
				r.TreeAfter, r.EnvFPAfter = c.treeAfter, c.envAfter
			}))
			if !ok {
				t.Fatalf("record was excluded: %s", reason)
			}
			if tr.Mutated != c.mutated {
				t.Errorf("Mutated = %v, want %v", tr.Mutated, c.mutated)
			}
			if tr.TreeMutated != c.treeMut {
				t.Errorf("TreeMutated = %v, want %v", tr.TreeMutated, c.treeMut)
			}
			if tr.EnvMutated != c.envMut {
				t.Errorf("EnvMutated = %v, want %v", tr.EnvMutated, c.envMut)
			}
		})
	}
}

// TestTransitionEnvOnlyMutationIsNotMissed states the install case on its own.
//
// A `uv sync` leaves the tree hash byte-identical because .venv is gitignored.
// Comparing trees alone would label the single most expensive class of command
// in the corpus as a no-op, so the environment fingerprint has to be part of
// the comparison.
func TestTransitionEnvOnlyMutationIsNotMissed(t *testing.T) {
	tr, _, ok := TransitionFrom(trRecord(func(r *Record) {
		r.Cmd, r.CmdNorm = "uv sync --extra dev", "uv sync --extra dev"
		r.EnvFPAfter = "env-after-install"
		r.Servable = false
	}))
	if !ok {
		t.Fatal("an install should still be a transition; it is just not a servable one")
	}
	if !tr.Mutated {
		t.Fatal("an install that moved only the env fingerprint was labelled non-mutating")
	}
	if tr.TreeMutated {
		t.Error("the tree did not move, so TreeMutated must be false")
	}
	if tr.Servable {
		t.Error("Servable must pass through from the record, not be re-derived")
	}
}

// TestTransitionMissingAfterStateIsDropped covers the record hindsight record
// writes when it could not recompute the state after the command.
//
// The action is real but its response was never observed, so under the leakage
// rule it is not a transition, and to a trainer it is a row with no successor
// state. It must be counted, not skipped, and it must not panic.
func TestTransitionMissingAfterStateIsDropped(t *testing.T) {
	cases := map[string]func(*Record){
		"no tree after":   func(r *Record) { r.TreeAfter = "" },
		"no env after":    func(r *Record) { r.EnvFPAfter = "" },
		"no state at all": func(r *Record) { r.TreeBefore, r.EnvFPBefore, r.TreeAfter, r.EnvFPAfter = "", "", "", "" },
		"zero record":     func(r *Record) { *r = Record{Decision: DecisionMiss} },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			_, reason, ok := TransitionFrom(trRecord(mut))
			if ok {
				t.Fatal("a record with no observed after-state was admitted")
			}
			if reason != ExcludeNoAfter {
				t.Errorf("reason = %q, want %q", reason, ExcludeNoAfter)
			}
		})
	}

	if _, reason, ok := TransitionFrom(nil); ok || reason != ExcludeMalformed {
		t.Errorf("a nil record gave (%q, %v), want the malformed reason and exclusion", reason, ok)
	}
}

// TestTransitionScanBalances is the accounting invariant. Every scanned record
// either becomes a row or is counted under a reason it did not, so a reader
// can verify from the header alone that nothing vanished quietly.
func TestTransitionScanBalances(t *testing.T) {
	log := trLog(t,
		trRecord(nil),
		trRecord(func(r *Record) { r.Decision = DecisionHit }),
		trRecord(func(r *Record) { r.Decision = DecisionHit }),
		trRecord(func(r *Record) { r.Decision = DecisionLeaseWait }),
		trRecord(func(r *Record) { r.Decision = DecisionPassthrough }),
		trRecord(func(r *Record) { r.Decision = "VERIFY" }),
		trRecord(func(r *Record) { r.TreeAfter = "" }),
		trRecord(func(r *Record) { r.TreeAfter = "tree-after" }),
	)
	// A torn line and a blank one, both of which a real log can contain.
	log += "{\"v\":1,\"decision\":\"MI\n\n"

	transitions, stats, err := ScanTransitions(strings.NewReader(log), FilterAll)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Balanced() {
		t.Fatalf("scanned %d != exported %d + excluded %d",
			stats.Scanned, stats.Exported, stats.ExcludedTotal())
	}
	if stats.Scanned != 9 {
		t.Errorf("Scanned = %d, want 9 (8 records plus one torn line; the blank line is not a record)", stats.Scanned)
	}
	if stats.Exported != 2 || len(transitions) != 2 {
		t.Fatalf("exported %d transitions (stats say %d), want 2", len(transitions), stats.Exported)
	}
	want := map[string]int{
		ExcludeHit:         2,
		ExcludeLeaseWait:   1,
		ExcludePassthrough: 1,
		ExcludeVerify:      1,
		ExcludeNoAfter:     1,
		ExcludeMalformed:   1,
	}
	for reason, n := range want {
		if stats.Excluded[reason] != n {
			t.Errorf("excluded[%q] = %d, want %d", reason, stats.Excluded[reason], n)
		}
	}
	if len(stats.Excluded) != len(want) {
		t.Errorf("got %d exclusion reasons, want %d: %v", len(stats.Excluded), len(want), stats.Excluded)
	}
	if !transitions[1].Mutated || transitions[0].Mutated {
		t.Error("scan did not preserve the mutation labels")
	}
}

// TestTransitionFiltersSplitThePopulation checks that the two filters partition
// the corpus rather than overlapping or leaking, and that a filtered row is
// still counted so the header keeps adding up.
func TestTransitionFiltersSplitThePopulation(t *testing.T) {
	log := trLog(t,
		trRecord(nil),
		trRecord(func(r *Record) { r.TreeAfter = "tree-after" }),
		trRecord(func(r *Record) { r.EnvFPAfter = "env-after" }),
		trRecord(func(r *Record) { r.Decision = DecisionHit }),
	)

	all, allStats, err := ScanTransitions(strings.NewReader(log), FilterAll)
	if err != nil {
		t.Fatal(err)
	}
	mut, mutStats, err := ScanTransitions(strings.NewReader(log), FilterMutatingOnly)
	if err != nil {
		t.Fatal(err)
	}
	pure, pureStats, err := ScanTransitions(strings.NewReader(log), FilterNonMutatingOnly)
	if err != nil {
		t.Fatal(err)
	}

	if len(all) != 3 || len(mut) != 2 || len(pure) != 1 {
		t.Fatalf("all=%d mutating=%d non-mutating=%d, want 3/2/1", len(all), len(mut), len(pure))
	}
	if len(mut)+len(pure) != len(all) {
		t.Error("the two filters do not partition the corpus")
	}
	for _, s := range []ExportStats{allStats, mutStats, pureStats} {
		if !s.Balanced() {
			t.Errorf("filtered scan does not balance: %+v", s)
		}
		if s.Scanned != 4 {
			t.Errorf("Scanned = %d, want 4; a filter must not change what was scanned", s.Scanned)
		}
	}
	if mutStats.Excluded[ExcludeNonMutating] != 1 {
		t.Errorf("--mutating-only dropped a row without counting it: %v", mutStats.Excluded)
	}
	if pureStats.Excluded[ExcludeMutating] != 2 {
		t.Errorf("non-mutating filter dropped rows without counting them: %v", pureStats.Excluded)
	}
}

// TestTransitionManifestCarriesProvenance checks that the json header states
// where the corpus came from and what rule produced it. A bare array of
// transitions is worth much less than the same array with a provenance
// statement, and the rule has to appear verbatim so a reader is not trusting a
// paraphrase.
func TestTransitionManifestCarriesProvenance(t *testing.T) {
	log := trLog(t, trRecord(nil), trRecord(func(r *Record) { r.Decision = DecisionHit }))
	transitions, stats, err := ScanTransitions(strings.NewReader(log), FilterAll)
	if err != nil {
		t.Fatal(err)
	}
	doc := ExportDocument{
		Meta:        NewExportManifest("/cache/repo", "/cache/repo/log.jsonl", "2026-08-23T00:00:00Z", FilterAll, stats),
		Transitions: transitions,
	}
	blob, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	var back ExportDocument
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if back.Meta.LeakageRule != LeakageRule {
		t.Error("the leakage rule is not quoted verbatim in the header")
	}
	if !strings.Contains(back.Meta.LeakageRuleSource, "Experiential Labs") ||
		!strings.Contains(back.Meta.LeakageRuleSource, "Apache-2.0") {
		t.Errorf("attribution missing from the header: %q", back.Meta.LeakageRuleSource)
	}
	if back.Meta.Scanned != 2 || back.Meta.Exported != 1 || back.Meta.Excluded != 1 {
		t.Errorf("header counts = %d scanned / %d exported / %d excluded, want 2/1/1",
			back.Meta.Scanned, back.Meta.Exported, back.Meta.Excluded)
	}
	if back.Meta.Scanned != back.Meta.Exported+back.Meta.Excluded {
		t.Error("the header's own counts do not add up")
	}
	if back.Meta.Reasons[ExcludeHit] != 1 {
		t.Errorf("header does not say why the record was dropped: %v", back.Meta.Reasons)
	}
	if back.Meta.Source == "" || back.Meta.SourceLog == "" || back.Meta.ExportedAt == "" {
		t.Error("header is missing source or export time")
	}
	if len(back.Meta.Limitations) == 0 {
		t.Error("header states no limitations")
	}
}

package hp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// referenceCorpus is the sealed corpus the published figures were measured on.
// Every assertion against it is skipped when it is absent, so the suite passes
// on a machine that has never seen it.
const referenceCorpus = "/Users/tomjeong/hacker/skunk-works/notes/sealed-corpus/replay-A"

func step(state, cmd string) CorpusStep { return CorpusStep{State: state, Cmd: cmd} }

// attempt builds one record in the corpus's documented shape. Step indices are
// assigned in order, which is what the real harness does.
func attempt(instance, submission string, steps ...CorpusStep) map[string]any {
	for i := range steps {
		steps[i].N = i
	}
	return map[string]any{
		"status":   "complete",
		"summary":  map[string]any{"instance_id": instance},
		"source":   map[string]any{"metadata": map[string]any{"submission": submission}},
		"evidence": map[string]any{"steps": steps},
	}
}

func writeRecord(t *testing.T, dir, name string, rec any) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), b, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func writeRaw(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func mustLoad(t *testing.T, dir string) ([]Trajectory, CorpusLoad) {
	t.Helper()
	trajs, load, err := LoadCorpusReport(dir)
	if err != nil {
		t.Fatalf("LoadCorpusReport: %v", err)
	}
	return trajs, load
}

// A corpus is a pile of files written by a long-running harness. One bad file
// costs a record, never the measurement, and the loader has to say how many it
// dropped or the denominator is unknowable.
func TestLoadCorpusSkipsBadRecordsWithoutFailing(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, "good", attempt("inst-1", "m1", step("s0", "ls")))
	writeRaw(t, dir, "truncated", `{"status":"complete","evidence":{"steps":[`)
	writeRaw(t, dir, "garbage", "not json at all")
	writeRecord(t, dir, "incomplete", map[string]any{
		"status":   "timeout",
		"summary":  map[string]any{"instance_id": "inst-2"},
		"evidence": map[string]any{"steps": []CorpusStep{step("s0", "ls")}},
	})
	writeRecord(t, dir, "nosteps", map[string]any{
		"status":  "complete",
		"summary": map[string]any{"instance_id": "inst-3"},
	})
	writeRecord(t, dir, "noinstance", map[string]any{
		"status":   "complete",
		"evidence": map[string]any{"steps": []CorpusStep{step("s0", "ls")}},
	})
	// Steps whose shape does not match are malformed, not silently empty.
	writeRaw(t, dir, "wrongtype", `{"status":"complete","summary":{"instance_id":"i"},"evidence":{"steps":[{"n":"zero"}]}}`)

	trajs, load := mustLoad(t, dir)

	if len(trajs) != 1 {
		t.Fatalf("loaded %d trajectories, want 1", len(trajs))
	}
	if trajs[0].InstanceID != "inst-1" || trajs[0].Submission != "m1" {
		t.Errorf("loaded %+v, want instance inst-1 submission m1", trajs[0])
	}
	want := CorpusLoad{Files: 7, Loaded: 1, Incomplete: 1, NoSteps: 1, NoInstance: 1, Malformed: 3}
	if load != want {
		t.Errorf("load counts = %+v, want %+v", load, want)
	}
	if load.Skipped() != 6 {
		t.Errorf("Skipped() = %d, want 6", load.Skipped())
	}
}

// The instance id moved between emitted shapes. Reading only one of them would
// silently produce a corpus with no multi-agent tasks in it, which looks like
// a real answer.
func TestLoadCorpusFindsInstanceIDInEitherPlace(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, "nested", map[string]any{
		"status": "complete",
		"source": map[string]any{"metadata": map[string]any{"submission": "m"}},
		"evidence": map[string]any{
			"summary": map[string]any{"instance_id": "from-evidence"},
			"steps":   []CorpusStep{step("s0", "ls")},
		},
	})
	trajs, _ := mustLoad(t, dir)
	if len(trajs) != 1 || trajs[0].InstanceID != "from-evidence" {
		t.Fatalf("got %+v, want one trajectory with instance from-evidence", trajs)
	}
}

// The shipped corpus nests records under records/; a synthetic one usually
// does not. Both are corpora.
func TestLoadCorpusAcceptsRecordsSubdirectory(t *testing.T) {
	dir := t.TempDir()
	recs := filepath.Join(dir, "records", "ab")
	if err := os.MkdirAll(recs, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRecord(t, recs, "one", attempt("inst-1", "m1", step("s0", "ls")))
	// A sibling of records/ that is not a record must not be counted.
	writeRecord(t, dir, "plan-manifest", map[string]any{"unrelated": true})

	trajs, load := mustLoad(t, dir)
	if len(trajs) != 1 {
		t.Fatalf("loaded %d, want 1", len(trajs))
	}
	if load.Files != 1 {
		t.Errorf("walked %d files, want 1 (records/ only)", load.Files)
	}
}

func TestLoadCorpusRejectsMissingDirectory(t *testing.T) {
	if _, _, err := LoadCorpusReport(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("want an error for a corpus directory that does not exist")
	}
}

// A task only one agent attempted cannot say anything about sharing between
// agents. Leaving it in the denominator alone would deflate every rate.
func TestReplayExcludesSingleAttemptInstances(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, "a", attempt("shared", "m1", step("s0", "ls"), step("s0", "cat f")))
	writeRecord(t, dir, "b", attempt("shared", "m2", step("s0", "ls")))
	writeRecord(t, dir, "c", attempt("lonely", "m3", step("s0", "ls"), step("s0", "ls")))

	trajs, _ := mustLoad(t, dir)
	if len(trajs) != 3 {
		t.Fatalf("loaded %d, want 3", len(trajs))
	}
	if got := len(MultiAgentInstances(trajs)); got != 1 {
		t.Fatalf("multi-agent instances = %d, want 1", got)
	}

	rep := Replay(trajs, KeyState)
	if rep.Instances != 1 || rep.Attempts != 2 {
		t.Errorf("instances=%d attempts=%d, want 1 and 2", rep.Instances, rep.Attempts)
	}
	// The lonely task's two commands are outside the denominator entirely, and
	// so is the repeat inside it.
	if rep.Overall.Commands != 3 {
		t.Errorf("commands = %d, want 3 (the lonely attempt excluded)", rep.Overall.Commands)
	}
	if rep.Overall.Avoidable != 1 || rep.Overall.CrossAgent != 1 {
		t.Errorf("avoidable=%d cross=%d, want 1 and 1", rep.Overall.Avoidable, rep.Overall.CrossAgent)
	}
}

// Self-reuse and cross-agent reuse are different claims. Only the second is an
// argument for fanning out, so conflating them overstates the whole story.
func TestReplaySeparatesSelfReuseFromCrossAgent(t *testing.T) {
	dir := t.TempDir()
	// "solo": two agents that each repeat only themselves, and never overlap.
	writeRecord(t, dir, "solo-a", attempt("solo", "m1", step("s0", "cmd A"), step("s0", "cmd A")))
	writeRecord(t, dir, "solo-b", attempt("solo", "m2", step("s0", "cmd B"), step("s0", "cmd B")))
	// "peer": two agents that run the same command at the same state.
	writeRecord(t, dir, "peer-a", attempt("peer", "m1", step("s0", "cmd X")))
	writeRecord(t, dir, "peer-b", attempt("peer", "m2", step("s0", "cmd X")))

	rep := Replay(mustLoadTrajs(t, dir), KeyState)
	if rep.Overall.Commands != 6 {
		t.Fatalf("commands = %d, want 6", rep.Overall.Commands)
	}
	if rep.Overall.Avoidable != 3 {
		t.Errorf("avoidable = %d, want 3", rep.Overall.Avoidable)
	}
	if rep.Overall.SelfReuse != 2 {
		t.Errorf("self-reuse = %d, want 2 (one per solo agent)", rep.Overall.SelfReuse)
	}
	if rep.Overall.CrossAgent != 1 {
		t.Errorf("cross-agent = %d, want 1 (the peer pair)", rep.Overall.CrossAgent)
	}
}

// The state component is the entire difference between what is published on
// command strings and what a state-keyed cache can actually serve. The same
// command at a moved workspace is a miss.
func TestReplayStateKeyingRefusesWhatCommandKeyingAccepts(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, "a", attempt("inst", "m1", step("s0", "pytest -q")))
	writeRecord(t, dir, "b", attempt("inst", "m2", step("s1", "pytest -q")))
	trajs := mustLoadTrajs(t, dir)

	if got := Replay(trajs, KeyState).Overall.Avoidable; got != 0 {
		t.Errorf("state-keyed avoidable = %d, want 0: the workspaces differ", got)
	}
	byCmd := Replay(trajs, KeyCommand).Overall
	if byCmd.Avoidable != 1 || byCmd.CrossAgent != 1 {
		t.Errorf("command-keyed = %+v, want 1 avoidable and 1 cross-agent", byCmd)
	}
}

// Whitespace is not meaning. Two spellings of one command share a key because
// NormalizeCommand says so, not because this file has its own opinion.
func TestReplayNormalizesCommandSpelling(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, "a", attempt("inst", "m1", step("s0", "pytest  -q")))
	writeRecord(t, dir, "b", attempt("inst", "m2", step("s0", "pytest -q")))
	if got := Replay(mustLoadTrajs(t, dir), KeyState).Overall.Avoidable; got != 1 {
		t.Errorf("avoidable = %d, want 1: the two spellings normalize identically", got)
	}
}

// The headline count is a property of the multiset of keys, not of any model
// of how the agents interleave. This is what lets the number be quoted without
// also quoting an interleaving assumption.
func TestReplayCountIsOrderIndependent(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, "a", attempt("inst", "m3",
		step("s0", "ls"), step("s0", "cat f"), step("s1", "ls"), step("s1", "ls")))
	writeRecord(t, dir, "b", attempt("inst", "m1",
		step("s0", "ls"), step("s1", "ls"), step("s1", "grep x")))
	writeRecord(t, dir, "c", attempt("inst", "m2",
		step("s0", "cat f"), step("s0", "ls"), step("s2", "ls")))
	trajs := mustLoadTrajs(t, dir)

	// Total minus distinct keys, computed here rather than by the code under
	// test, is the definition the count has to satisfy.
	distinct := map[string]bool{}
	total := 0
	for _, tr := range trajs {
		for _, s := range tr.Steps {
			total++
			distinct[s.State+"\x00"+NormalizeCommand(s.Cmd)] = true
		}
	}
	want := total - len(distinct)

	base := Replay(trajs, KeyState)
	if base.Overall.Commands != total {
		t.Fatalf("commands = %d, want %d", base.Overall.Commands, total)
	}
	if base.Overall.Avoidable != want {
		t.Errorf("avoidable = %d, want total-distinct = %d", base.Overall.Avoidable, want)
	}
	if base.Overall.SelfReuse+base.Overall.CrossAgent != base.Overall.Avoidable {
		t.Errorf("self %d + cross %d != avoidable %d",
			base.Overall.SelfReuse, base.Overall.CrossAgent, base.Overall.Avoidable)
	}

	// Every permutation of the input has to produce the same report, so the
	// order files happen to be read in never reaches the answer.
	for _, perm := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}, {2, 0, 1}} {
		shuffled := []Trajectory{trajs[perm[0]], trajs[perm[1]], trajs[perm[2]]}
		got := Replay(shuffled, KeyState)
		if got.Overall != base.Overall {
			t.Errorf("permutation %v gave %+v, want %+v", perm, got.Overall, base.Overall)
		}
	}
}

// Every command is classified by the shipping classifier, so the policy rows
// partition the corpus exactly. A command counted twice or not at all would
// make the "under our policy" figure quietly wrong.
func TestReplayPolicyRowsPartitionTheCorpus(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, "a", attempt("inst", "m1",
		step("s0", "cat f"), step("s0", "rm -rf build"), step("s0", "curl https://example.com")))
	writeRecord(t, dir, "b", attempt("inst", "m2",
		step("s0", "cat f"), step("s0", "rm -rf build"), step("s0", "curl https://example.com")))

	rep := Replay(mustLoadTrajs(t, dir), KeyState)
	var sum Tally
	for _, p := range rep.ByPolicy {
		sum.Commands += p.Commands
		sum.Avoidable += p.Avoidable
		sum.CrossAgent += p.CrossAgent
	}
	if sum.Commands != rep.Overall.Commands || sum.Avoidable != rep.Overall.Avoidable {
		t.Errorf("policy rows sum to %+v, want %+v", sum, rep.Overall)
	}
	var classes Tally
	for _, c := range rep.ByClass {
		classes.Commands += c.Commands
		classes.Avoidable += c.Avoidable
	}
	if classes.Commands != rep.Overall.Commands || classes.Avoidable != rep.Overall.Avoidable {
		t.Errorf("class rows sum to %+v, want %+v", classes, rep.Overall)
	}
	// Only SERVE is servable, so "under our policy" is strictly the SERVE row.
	for _, p := range rep.ByPolicy {
		if p.Policy == SERVE.String() && p.Tally != rep.UnderPolicy {
			t.Errorf("UnderPolicy = %+v, want the SERVE row %+v", rep.UnderPolicy, p.Tally)
		}
	}
	if rep.UnderPolicy.Avoidable > rep.Overall.Avoidable {
		t.Error("what our policy can serve must never exceed the overlap that exists")
	}
}

// The class table exists to answer "cheap reads or expensive suites". A chain
// that opens with cd is the common case in real traces, and labelling it by
// the strictest segment would file every test run under "unrecognized".
func TestCommandClassNamesTheExpensiveSegment(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{"cat pkg/thing.py", "read"},
		{"grep -rn foo src | head -20", "read"},
		{"cd /testbed && python -m pytest tests/", "build"},
		{"pytest -q", "build"},
		{"cat setup.py && pytest -q", "build"},
		{"pip install -e .", "install"},
		{"rm -rf build", "mutation"},
		{"ls -la /testbed", "metadata"},
		{"curl https://example.com", "non-hermetic"},
	}
	for _, c := range cases {
		if got := CommandClass(c.cmd); got != c.want {
			t.Errorf("CommandClass(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

func TestParseKeying(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Keying
	}{{"", KeyState}, {"state", KeyState}, {"command", KeyCommand}, {"cmd", KeyCommand}} {
		got, err := ParseKeying(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseKeying(%q) = %v, %v; want %v, nil", c.in, got, err, c.want)
		}
	}
	if _, err := ParseKeying("tree"); err == nil {
		t.Error("want an error for an unknown keying")
	}
}

func TestReplayEmptyCorpusSaysNothing(t *testing.T) {
	rep := Replay(nil, KeyState)
	if rep.Instances != 0 || rep.Overall.Commands != 0 {
		t.Errorf("empty corpus gave %+v", rep)
	}
	if rep.Overall.ReuseRate() != 0 {
		t.Error("an empty corpus must not divide by zero")
	}
}

// The published figures. This is the check that the tool measures the corpus
// rather than measuring itself, so the numbers are spelled out rather than
// recomputed by the test.
func TestReplayReproducesReferenceCorpus(t *testing.T) {
	dir := referenceCorpus
	if v := os.Getenv("HP_REPLAY_CORPUS"); v != "" {
		dir = v
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("reference corpus not present at %s", dir)
	}
	trajs, load, err := LoadCorpusReport(dir)
	if err != nil {
		t.Fatalf("LoadCorpusReport: %v", err)
	}
	if load.Files != 265 || load.Loaded != 249 || load.Incomplete != 16 {
		t.Errorf("load = %+v, want 265 files, 249 loaded, 16 incomplete", load)
	}

	state := Replay(trajs, KeyState)
	if state.Instances != 25 {
		t.Errorf("multi-agent instances = %d, want 25", state.Instances)
	}
	if state.Attempts != 243 {
		t.Errorf("attempts = %d, want 243", state.Attempts)
	}
	if state.Overall.Commands != 12806 {
		t.Errorf("commands = %d, want 12806", state.Overall.Commands)
	}
	if state.Overall.Avoidable != 959 {
		t.Errorf("state-keyed avoidable = %d, want 959", state.Overall.Avoidable)
	}
	if state.Overall.SelfReuse+state.Overall.CrossAgent != 959 {
		t.Errorf("self %d + cross %d != 959", state.Overall.SelfReuse, state.Overall.CrossAgent)
	}
	// The reference analysis split the same 959 as 496 self / 463 cross. It
	// attributed four commands differently at the one place the split is a
	// judgement call: an agent repeating itself at a key a peer had already
	// run. Both splits are 3.6% cross-agent of 12806, which is the published
	// figure. Pinned exactly here so a change to the attribution rule fails
	// out loud rather than moving a number nobody is watching.
	if state.Overall.SelfReuse != 492 || state.Overall.CrossAgent != 467 {
		t.Errorf("split = %d self / %d cross, want 492 / 467 (reference 496 / 463)",
			state.Overall.SelfReuse, state.Overall.CrossAgent)
	}

	byCmd := Replay(trajs, KeyCommand)
	if byCmd.Overall.Avoidable != 2132 {
		t.Errorf("command-keyed avoidable = %d, want 2132", byCmd.Overall.Avoidable)
	}
	if byCmd.Overall.Avoidable <= state.Overall.Avoidable {
		t.Error("command-string keying is an upper bound and must exceed state keying")
	}

	// The decay is the shape of the claim: the opening is where agents overlap
	// and also where they are all running at once.
	want := map[string]float64{"0-2": 16.9, "3-9": 9.5, "10-19": 3.3, "20-49": 1.5, "50+": 1.0}
	for _, s := range state.ByStep {
		got := s.CrossRate()
		if diff := got - want[s.Steps]; diff > 0.15 || diff < -0.15 {
			t.Errorf("steps %s cross rate = %.1f%%, want about %.1f%%", s.Steps, got, want[s.Steps])
		}
	}
	if state.ByStep[0].CrossRate() <= state.ByStep[len(state.ByStep)-1].CrossRate() {
		t.Error("cross-agent reuse must decay across the trajectory")
	}
}

func mustLoadTrajs(t *testing.T, dir string) []Trajectory {
	t.Helper()
	trajs, err := LoadCorpus(dir)
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	return trajs
}

package hp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests that matter here are the negative ones.
//
// Speculation is only defensible because a bad guess costs a CPU cycle rather
// than a wrong answer, and that is a claim about what the code refuses to do.
// So most of what follows asserts absence: no suggestion for a command the
// classifier will not serve, none for a key already in the index, none for one
// an agent is executing right now, none cheap enough that warming it costs
// more than it saves. And the last test asserts the thing the whole design
// rests on — that a speculative record is served under exactly the same rules
// as an agent's, because it went through exactly the same path.

const specTestEnvFP = "envfp-for-tests"

func specServer(t *testing.T) *Server {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(store)
}

// observe appends the record a real execution would have produced. Servability
// follows the policy, as it does in the record path, so a test never has to
// declare it by hand.
func observe(t *testing.T, s *Server, agent, tree, envFP, cmd string, durMS int64) *Record {
	t.Helper()
	policy, reason := Classify(cmd)
	norm := NormalizeCommand(cmd)
	rec := &Record{
		V: 1, TS: float64(time.Now().UnixMilli()) / 1000,
		Agent: agent, Cmd: cmd, CmdNorm: norm, CwdRel: ".",
		TreeBefore: tree, EnvFPBefore: envFP,
		TreeAfter: tree, EnvFPAfter: envFP,
		Key:      Key(State{Tree: tree, EnvFP: envFP}, ".", norm),
		Policy:   policy.String(),
		Reason:   reason,
		Decision: DecisionMiss,
		Servable: policy == SERVE,
		// A record with no blobs is never servable in production; give the
		// synthetic ones something so they behave like the real thing.
		StdoutBlob: "sha256:out", StderrBlob: "sha256:err",
		DurationMS: durMS,
	}
	if err := s.store.Append(rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func saw(t *testing.T, s *Server, agent, tree, cmd string, durMS int64) *Record {
	t.Helper()
	return observe(t, s, agent, tree, specTestEnvFP, cmd, durMS)
}

func suggestionFor(sugs []Suggestion, cmdNorm, tree string) *Suggestion {
	for i := range sugs {
		if sugs[i].CmdNorm == cmdNorm && (tree == "" || sugs[i].Tree == tree) {
			return &sugs[i]
		}
	}
	return nil
}

// mustNotServe is a precondition guard. If the classifier ever changes its mind
// about one of these, the test should say so directly rather than quietly stop
// testing anything.
func mustNotServe(t *testing.T, cmd string) {
	t.Helper()
	if p, _ := Classify(cmd); p == SERVE {
		t.Fatalf("precondition: %q is expected not to be SERVE", cmd)
	}
}

// TestSpeculateIgnoresNonServeCommands. Warming a command the cache will never
// serve is pure waste, and for the non-hermeticity list it is worse than
// waste: those commands are wrong to serve at any state, so a speculative
// result of one is a trap sitting in the index waiting for a classifier bug.
func TestSpeculateIgnoresNonServeCommands(t *testing.T) {
	t.Setenv("HP_MIN_DURATION_MS", "500")
	s := specServer(t)

	const curl = "curl https://example.com/data.json"
	const install = "pip install -r requirements.txt"
	mustNotServe(t, curl)
	mustNotServe(t, install)

	for _, tr := range []string{"tree-1", "tree-2"} {
		saw(t, s, "a1", tr, curl, 3000)
		saw(t, s, "a2", tr, install, 4000)
	}
	// A positive control, so an empty result cannot pass this test.
	saw(t, s, "a3", "tree-3", "pytest -q", 2000)
	saw(t, s, "a4", "tree-4", "pytest -q", 2100)

	sugs := s.Suggestions(50)
	if suggestionFor(sugs, "pytest -q", "") == nil {
		t.Fatalf("expected the servable command to be suggested somewhere; got %d suggestions", len(sugs))
	}
	for _, g := range sugs {
		if p, _ := Classify(g.Cmd); p != SERVE {
			t.Fatalf("suggested %q, which classifies as %s", g.Cmd, p)
		}
		if g.Policy != SERVE.String() {
			t.Fatalf("suggestion carries policy %s", g.Policy)
		}
	}
}

// TestSpeculateIgnoresWhatIsAlreadyCached. The result is already there; paying
// to compute it again is the one case where speculation has a guaranteed
// return of zero.
func TestSpeculateIgnoresWhatIsAlreadyCached(t *testing.T) {
	t.Setenv("HP_MIN_DURATION_MS", "500")
	s := specServer(t)

	for _, tr := range []string{"tree-1", "tree-2", "tree-3"} {
		saw(t, s, "a1", tr, "pytest -q", 2000)
	}
	saw(t, s, "a2", "tree-4", "make build", 1500)

	cached := Key(State{Tree: "tree-3", EnvFP: specTestEnvFP}, ".", "pytest -q")
	if _, ok := s.store.Lookup(cached); !ok {
		t.Fatal("precondition: tree-3 should be in the servable index")
	}

	sugs := s.Suggestions(50)
	for _, g := range sugs {
		if g.Key == cached {
			t.Fatalf("suggested a key that is already in the index: %s", g.Reason)
		}
		if _, ok := s.store.Lookup(g.Key); ok {
			t.Fatalf("suggested %q at %s, which is already cached", g.CmdNorm, g.Tree)
		}
	}
	if suggestionFor(sugs, "pytest -q", "tree-4") == nil {
		t.Fatal("tree-4 has never run pytest and is where the suggestion belongs")
	}
}

// TestSpeculateIgnoresCommandsBelowTheDurationFloor. The floor exists because
// interception costs about 42ms whether or not the answer was waiting, so a
// command below it cannot pay for its own cache entry. Precomputing one cannot
// either — it just moves the same losing trade into the background.
func TestSpeculateIgnoresCommandsBelowTheDurationFloor(t *testing.T) {
	t.Setenv("HP_MIN_DURATION_MS", "500")
	s := specServer(t)

	const cheap = "git status"
	if p, _ := Classify(cheap); p != SERVE {
		t.Fatalf("precondition: %q should be SERVE", cheap)
	}
	for _, tr := range []string{"tree-1", "tree-2"} {
		saw(t, s, "a1", tr, "pytest -q", 2000)
		saw(t, s, "a2", tr, cheap, 40)
	}
	saw(t, s, "a3", "tree-3", "make build", 1200)

	sugs := s.Suggestions(50)
	if suggestionFor(sugs, "pytest -q", "tree-3") == nil {
		t.Fatal("the expensive command is the whole point and should be suggested")
	}
	for _, g := range sugs {
		if g.CmdNorm == cheap {
			t.Fatalf("suggested %q, which runs in 40ms", cheap)
		}
		if g.DurationMS < MinDurationMS() {
			t.Fatalf("suggested %q at %dms, below the %dms floor", g.CmdNorm, g.DurationMS, MinDurationMS())
		}
	}
}

// TestSpeculateRanksByExpectedSaving. A warmer has one CPU and a queue, so the
// order is the whole product: p(somebody asks) times what they would have
// paid.
func TestSpeculateRanksByExpectedSaving(t *testing.T) {
	t.Setenv("HP_MIN_DURATION_MS", "500")
	s := specServer(t)

	// Slow and moderately common against quick and near-universal. Expected
	// value should prefer the first even though the second is likelier.
	saw(t, s, "a1", "tree-1", "pytest -q", 4000)
	saw(t, s, "a2", "tree-2", "pytest -q", 4000)
	for _, tr := range []string{"tree-1", "tree-2", "tree-3"} {
		saw(t, s, "a3", tr, "mypy .", 1000)
	}
	saw(t, s, "a4", "tree-4", "make build", 900)

	sugs := s.Suggestions(50)
	if len(sugs) < 2 {
		t.Fatalf("expected several suggestions, got %d", len(sugs))
	}
	for i := 1; i < len(sugs); i++ {
		if sugs[i-1].ExpectedMS < sugs[i].ExpectedMS {
			t.Fatalf("suggestions are not ordered by expected saving: %.0f then %.0f",
				sugs[i-1].ExpectedMS, sugs[i].ExpectedMS)
		}
	}
	if sugs[0].CmdNorm != "pytest -q" {
		t.Fatalf("the most valuable candidate is the 4s suite, got %q (%.0fms)",
			sugs[0].CmdNorm, sugs[0].ExpectedMS)
	}
	// Compared at the same state, so the only difference is the command.
	slow := suggestionFor(sugs, "pytest -q", "tree-4")
	quick := suggestionFor(sugs, "mypy .", "tree-4")
	if slow == nil || quick == nil {
		t.Fatalf("both candidates should be offered at tree-4: %+v", sugs)
	}
	if quick.P <= slow.P {
		t.Fatalf("mypy ran at more states and should carry the higher p (%.2f vs %.2f)", quick.P, slow.P)
	}
	if slow.ExpectedMS <= quick.ExpectedMS {
		t.Fatalf("the slower command should still win on expected value: %.0f vs %.0f",
			slow.ExpectedMS, quick.ExpectedMS)
	}
}

// TestSpeculateIsSilentOnAColdLog. A cache with nothing in it has nothing to
// learn from, and the right amount of guessing to do from no evidence is none.
func TestSpeculateIsSilentOnAColdLog(t *testing.T) {
	t.Setenv("HP_MIN_DURATION_MS", "500")

	s := specServer(t)
	if got := s.Suggestions(10); len(got) != 0 {
		t.Fatalf("an empty log should suggest nothing, got %v", got)
	}

	// One agent, one expensive command, seen exactly once. An anecdote is not
	// a pattern, and the log cannot tell the difference between a command that
	// will recur and one that was typed by accident.
	saw(t, s, "a1", "tree-1", "pytest -q", 9000)
	if got := s.Suggestions(10); len(got) != 0 {
		t.Fatalf("one observation is not evidence, got %v", got)
	}

	// Several agents, all doing different things once each: still nothing that
	// repeats, so still nothing to predict.
	saw(t, s, "a2", "tree-2", "mypy .", 8000)
	saw(t, s, "a3", "tree-3", "make build", 7000)
	if got := s.Suggestions(10); len(got) != 0 {
		t.Fatalf("distinct one-off commands are not a pattern, got %v", got)
	}
}

// TestSpeculateSkipsKeysUnderLease. An agent is running this command right
// now. Running it alongside them is the one form of speculation that is
// strictly worse than doing nothing: it cannot arrive first and it steals CPU
// from the run that will.
func TestSpeculateSkipsKeysUnderLease(t *testing.T) {
	t.Setenv("HP_MIN_DURATION_MS", "500")
	s := specServer(t)

	saw(t, s, "a1", "tree-1", "pytest -q", 3000)
	saw(t, s, "a2", "tree-2", "pytest -q", 3000)
	saw(t, s, "a3", "tree-3", "make build", 1000)

	target := suggestionFor(s.Suggestions(50), "pytest -q", "tree-3")
	if target == nil {
		t.Fatal("precondition: pytest at tree-3 should be suggested")
	}

	s.acquire(target.Key, "a3")
	if got := suggestionFor(s.Suggestions(50), "pytest -q", "tree-3"); got != nil {
		t.Fatal("suggested a key an agent holds the lease on")
	}

	s.release(target.Key)
	if got := suggestionFor(s.Suggestions(50), "pytest -q", "tree-3"); got == nil {
		t.Fatal("the lease is gone and the candidate should be back")
	}

	// A key already shown to be unservable is equally pointless: whatever the
	// command does, the purity gate will refuse the result again.
	s.mu.Lock()
	s.unservable[target.Key] = true
	s.mu.Unlock()
	if got := suggestionFor(s.Suggestions(50), "pytest -q", "tree-3"); got != nil {
		t.Fatal("suggested a key already known to be unservable")
	}
}

// TestSpeculateMaterializesATreeWithoutACommit is the mechanism the whole
// executor rests on.
//
// The states worth warming are uncommitted working directories — that is what
// a fan-out spends its time in — so they exist only as tree objects and `git
// checkout` will not take one. read-tree plus checkout-index will, and this
// asserts the round trip is exact rather than approximately right, because an
// approximate tree is a different key and a different key is a wasted suite.
func TestSpeculateMaterializesATreeWithoutACommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	repo := t.TempDir()
	initRepo(t, repo)

	ws, err := NewWorkspace(repo)
	if err != nil {
		t.Fatal(err)
	}

	// State one: an uncommitted edit plus an untracked file, which is the
	// normal shape of an agent mid-task and is nowhere in the object graph as
	// a commit.
	writeFile(t, repo, "src/app.py", "print('edited, never committed')\n")
	writeFile(t, repo, "src/new.py", "print('untracked')\n")
	treeA, err := ws.TreeHash()
	if err != nil {
		t.Fatal(err)
	}

	// State two: the untracked file is gone again, so materializing A then B
	// has to remove a file rather than only overwrite one.
	if err := os.Remove(filepath.Join(repo, "src", "new.py")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repo, "src/app.py", "print('edited again')\n")
	treeB, err := ws.TreeHash()
	if err != nil {
		t.Fatal(err)
	}
	if treeA == treeB {
		t.Fatal("precondition: the two states should hash differently")
	}

	sc, err := NewScratch(repo, filepath.Join(t.TempDir(), "scratch"))
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Remove()

	rehash := func() string {
		t.Helper()
		sws, err := NewWorkspace(sc.Dir)
		if err != nil {
			t.Fatal(err)
		}
		// The side index must be the scratch worktree's own, or two worktrees
		// interleave their `git add -A` and produce a tree describing no real
		// state.
		if !strings.HasPrefix(sws.IndexPath, sws.GitDir) {
			t.Fatalf("scratch index %s is not inside its own git dir %s", sws.IndexPath, sws.GitDir)
		}
		got, err := sws.TreeHash()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if err := sc.Checkout(treeA); err != nil {
		t.Fatal(err)
	}
	if got := rehash(); got != treeA {
		t.Fatalf("materialized tree is %s, asked for %s", got, treeA)
	}
	if _, err := os.Stat(filepath.Join(sc.Dir, "src", "new.py")); err != nil {
		t.Fatalf("an untracked file present when the tree was hashed is part of the tree: %v", err)
	}
	fi, err := os.Stat(filepath.Join(sc.Dir, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o100 == 0 {
		t.Fatal("the executable bit is part of the tree and must survive the round trip")
	}

	// Reuse: the same scratch has to be able to reach a second state, which
	// means removing what the first one left behind.
	if err := sc.Checkout(treeB); err != nil {
		t.Fatal(err)
	}
	if got := rehash(); got != treeB {
		t.Fatalf("second materialization is %s, asked for %s", got, treeB)
	}
	if _, err := os.Stat(filepath.Join(sc.Dir, "src", "new.py")); !os.IsNotExist(err) {
		t.Fatal("a file absent from the second tree was left behind by the first")
	}
}

// TestSpeculateGrantsNoPrivilegeToItsOwnRecords is the safety rule stated as a
// test.
//
// A speculative result enters through /record like anything else, and its
// servability is decided by the flag the record path computed from the purity
// gate. Nothing anywhere consults the agent id when deciding whether to serve.
// If that ever stops being true, this fails.
func TestSpeculateGrantsNoPrivilegeToItsOwnRecords(t *testing.T) {
	s := specServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	post := func(path string, v any) *http.Response {
		t.Helper()
		b, _ := json.Marshal(v)
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	lookup := func(key, agent string) LookupResp {
		t.Helper()
		resp := post("/lookup", LookupReq{Key: key, Agent: agent, Cmd: "pytest -q",
			CmdNorm: "pytest -q", CwdRel: ".", Tree: "t", EnvFP: "e",
			Policy: SERVE.String(), Serve: true})
		defer resp.Body.Close()
		var out LookupResp
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	base := Record{
		V: 1, Cmd: "pytest -q", CmdNorm: "pytest -q", CwdRel: ".",
		TreeBefore: "t", EnvFPBefore: "e", TreeAfter: "t", EnvFPAfter: "e",
		Policy: SERVE.String(), Decision: DecisionMiss, DurationMS: 4000,
		StdoutBlob: "sha256:out", StderrBlob: "sha256:err",
	}

	// An impure speculative result. The gate refused it, so it is not
	// servable, and being speculative buys it nothing.
	dirty := base
	dirty.Agent, dirty.Key, dirty.Servable = SpeculatorAgent, "k-dirty", false
	dirty.TreeAfter = "t-moved"
	post("/record", dirty).Body.Close()
	if got := lookup("k-dirty", "a1"); got.Decision != DecisionMiss {
		t.Fatalf("an unservable speculative record must never be served, got %s", got.Decision)
	}
	if _, ok := s.store.Lookup("k-dirty"); ok {
		t.Fatal("an unservable speculative record must not enter the servable index")
	}

	// A pure one, and the same record produced by an agent. Both must be
	// served identically, because the only difference between them is
	// provenance.
	spec := base
	spec.Agent, spec.Key, spec.Servable = SpeculatorAgent, "k-spec", true
	post("/record", spec).Body.Close()

	human := base
	human.Agent, human.Key, human.Servable = "a2", "k-human", true
	post("/record", human).Body.Close()

	fromSpec, fromHuman := lookup("k-spec", "a1"), lookup("k-human", "a1")
	if fromSpec.Decision != DecisionHit || fromHuman.Decision != DecisionHit {
		t.Fatalf("both should be hits, got %s and %s", fromSpec.Decision, fromHuman.Decision)
	}
	if fromSpec.SourceAgent != SpeculatorAgent {
		t.Fatalf("provenance should survive, got %q", fromSpec.SourceAgent)
	}
	fromSpec.SourceAgent, fromHuman.SourceAgent = "", ""
	if fromSpec != fromHuman {
		t.Fatalf("a speculative hit must be indistinguishable from an agent's:\n %+v\n %+v",
			fromSpec, fromHuman)
	}

	st := s.SpecStats()
	if st.Produced != 2 || st.Servable != 1 {
		t.Fatalf("ledger should show 2 produced and 1 servable, got %+v", st)
	}
	if st.Used != 1 || st.HitRate != 0.5 {
		t.Fatalf("one of two speculative results was used, so the hit rate is 0.5: %+v", st)
	}
}

// TestSpeculateEndToEnd is the whole loop: a daemon with a seeded log, a real
// `hindsight warm --once` against a real repository, a real recorded result,
// and an agent lookup that then hits it.
//
// It also measures the thing worth measuring — what the agent would have paid
// without speculation, against what it pays with it.
func TestSpeculateEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and runs a real command")
	}
	for _, bin := range []string{"git", "go", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("needs %s", bin)
		}
	}
	t.Setenv("HP_MIN_DURATION_MS", "500")
	// Pinned so the fingerprint does not pick up whatever virtualenv the
	// developer running the tests happens to be inside.
	t.Setenv("VIRTUAL_ENV", "")

	const slow = "python3 slow.py"
	if p, _ := Classify(slow); p != SERVE {
		t.Fatalf("precondition: %q should be SERVE", slow)
	}

	home := t.TempDir()
	repo := t.TempDir()
	initRepo(t, repo)
	writeFile(t, repo, "slow.py", "import time\ntime.sleep(1.2)\nprint('42 passed')\n")
	// Uncommitted, so the state being warmed exists only as a tree object.
	writeFile(t, repo, "src/app.py", "print('mid-task edit')\n")

	ws, err := NewWorkspace(repo)
	if err != nil {
		t.Fatal(err)
	}
	state, err := ws.State()
	if err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(home)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(store)
	mux := http.NewServeMux()
	mux.Handle("/suggest", s.SuggestHandler())
	mux.Handle("/", s.Handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("HP_DAEMON", srv.URL)

	// Two peers ran the suite at earlier states, and a third has just arrived
	// at the state we can actually reach. This is the convergence the design
	// doc describes: agents start from the same place and make the same moves.
	observe(t, s, "a1", "tree-earlier-one", state.EnvFP, slow, 1400)
	observe(t, s, "a2", "tree-earlier-two", state.EnvFP, slow, 1500)
	observe(t, s, "a3", state.Tree, state.EnvFP, "ls", 4)

	key := Key(state, ".", NormalizeCommand(slow))
	sugs := s.Suggestions(10)
	top := suggestionFor(sugs, NormalizeCommand(slow), state.Tree)
	if top == nil {
		t.Fatalf("the daemon should want this warmed; suggestions were %+v", sugs)
	}
	if top.Key != key {
		t.Fatalf("suggested key %s, an agent here would compute %s", top.Key, key)
	}
	t.Logf("suggestion: %s at %s — %s (expected saving %.0fms)",
		top.CmdNorm, top.Tree[:8], top.Reason, top.ExpectedMS)

	bin := buildHindsight(t)
	warm := exec.Command(bin, "warm", "--once", "-v",
		"--daemon", srv.URL, "--repo", repo, "--home", home)
	warmStart := time.Now()
	out, err := warm.CombinedOutput()
	warmTook := time.Since(warmStart)
	t.Logf("hindsight warm --once (%.1fs):\n%s", warmTook.Seconds(), out)
	if err != nil {
		t.Fatalf("warm failed: %v", err)
	}

	rec, ok := store.Lookup(key)
	if !ok {
		t.Fatal("speculation produced nothing servable for the key an agent will ask for")
	}
	if rec.Agent != SpeculatorAgent {
		t.Fatalf("record came from %q, expected the speculator", rec.Agent)
	}
	if !rec.Servable || rec.TreeAfter != rec.TreeBefore || rec.EnvFPAfter != rec.EnvFPBefore {
		t.Fatalf("the record should have passed the ordinary purity gate: %+v", rec)
	}
	if rec.Decision != DecisionMiss {
		t.Fatalf("a speculative execution is an ordinary miss-then-execute, got %s", rec.Decision)
	}

	// An agent arrives at the state the peers converged on and asks.
	client := NewClient()
	askedAt := time.Now()
	resp, err := client.Lookup(LookupReq{
		Key: key, Agent: "a4", Cmd: slow, CmdNorm: NormalizeCommand(slow),
		CwdRel: ".", Tree: state.Tree, EnvFP: state.EnvFP,
		Policy: SERVE.String(), Serve: true, RepoRoot: repo,
	})
	served := time.Since(askedAt)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != DecisionHit {
		t.Fatalf("expected a hit, got %s", resp.Decision)
	}
	if resp.SourceAgent != SpeculatorAgent {
		t.Fatalf("hit should be attributed to the speculator, got %q", resp.SourceAgent)
	}

	// The counterfactual, measured rather than modelled: the same command, in
	// the same repository, actually run.
	controlStart := time.Now()
	control := Run(slow, repo, time.Minute)
	controlTook := time.Since(controlStart)
	if control.ExitCode != 0 {
		t.Fatalf("control run failed: %s", control.Stderr)
	}

	spec := s.SpecStats()
	if spec.Produced != 1 || spec.Used != 1 || spec.HitRate != 1 {
		t.Fatalf("one result produced and one used: %+v", spec)
	}
	t.Logf("lookup answered in %s; the same command really takes %s (speculator measured %dms)",
		served.Round(time.Microsecond), controlTook.Round(time.Millisecond), rec.DurationMS)
	t.Logf("speculation ledger: produced=%d servable=%d used=%d hit_rate=%.0f%% spent=%.1fs deleted=%.1fs net=%+.1fs",
		spec.Produced, spec.Servable, spec.Used, spec.HitRate*100,
		spec.SecondsSpent, spec.SecondsDeleted, spec.NetSeconds)

	ask := func(agent string, st State) LookupResp {
		t.Helper()
		r, err := client.Lookup(LookupReq{
			Key: Key(st, ".", NormalizeCommand(slow)), Agent: agent, Cmd: slow,
			CmdNorm: NormalizeCommand(slow), CwdRel: ".", Tree: st.Tree, EnvFP: st.EnvFP,
			Policy: SERVE.String(), Serve: true, RepoRoot: repo,
		})
		if err != nil {
			t.Fatal(err)
		}
		return *r
	}

	// One warmed result covers more than the one state it was computed at:
	// an edit the command provably cannot read is promoted by Tier-1 scoping.
	// That is an existing mechanism rather than something speculation relaxed,
	// but it materially raises what a single warm run is worth.
	writeFile(t, repo, "src/app.py", "print('an edit this command cannot read')\n")
	moved, err := ws.State()
	if err != nil {
		t.Fatal(err)
	}
	if scoped := ask("a5", moved); scoped.Decision == DecisionHit {
		if scoped.Tier != 1 {
			t.Fatalf("a different tree can only be served through scoping, got tier %d", scoped.Tier)
		}
		t.Logf("a disjoint edit still hits, at tier 1: %s", scoped.ScopeReason)
	}

	// Change what the command actually reads and the hit is gone. Speculation
	// adds hits; it never invents them.
	writeFile(t, repo, "slow.py", "import time\ntime.sleep(1.2)\nprint('43 passed')\n")
	after, err := ws.State()
	if err != nil {
		t.Fatal(err)
	}
	if cold := ask("a6", after); cold.Decision != DecisionMiss {
		t.Fatalf("the command's own source changed, so this must miss; got %s", cold.Decision)
	}
}

// TestSpeculateRefusesAnEnvironmentItCannotReproduce is the honest half of the
// executor.
//
// A tree object cannot carry a virtualenv, because a virtualenv is gitignored
// and gitignored is exactly what a tree hash cannot see. So a scratch worktree
// materialized from a tree is missing precisely the thing the environment
// fingerprint exists to cover, its fingerprint differs from the agents', and
// its key is one nobody will ever ask for. Warming it would be a test suite
// spent on a slot in the index that is unreachable by construction.
//
// The warmer therefore checks before it runs and declines. --env-link is the
// only way through and it is opt-in, because it shares a live agent's
// dependency directory rather than copying it.
func TestSpeculateRefusesAnEnvironmentItCannotReproduce(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and runs a real command")
	}
	for _, bin := range []string{"git", "go", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("needs %s", bin)
		}
	}
	t.Setenv("HP_MIN_DURATION_MS", "500")
	t.Setenv("VIRTUAL_ENV", "")

	const slow = "python3 slow.py"
	home := t.TempDir()
	repo := t.TempDir()
	initRepo(t, repo)
	writeFile(t, repo, "slow.py", "import time\ntime.sleep(0.8)\nprint('ok')\n")
	writeFile(t, repo, "pyproject.toml", "[project]\nname = \"demo\"\n")
	// A virtualenv of the shape the fingerprint reads: gitignored, so it is
	// invisible to the tree and cannot travel with it.
	writeFile(t, repo, ".venv/pyvenv.cfg", "version = 3.11.0\n")
	writeFile(t, repo, ".venv/lib/python3.11/site-packages/pytest-8.0.0.dist-info/METADATA", "x\n")

	ws, err := NewWorkspace(repo)
	if err != nil {
		t.Fatal(err)
	}
	state, err := ws.State()
	if err != nil {
		t.Fatal(err)
	}
	if !state.EnvComplete {
		t.Fatal("precondition: the donor environment should be readable")
	}

	store, err := OpenStore(home)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(store)
	mux := http.NewServeMux()
	mux.Handle("/suggest", s.SuggestHandler())
	mux.Handle("/", s.Handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	observe(t, s, "a1", "tree-earlier-one", state.EnvFP, slow, 900)
	observe(t, s, "a2", "tree-earlier-two", state.EnvFP, slow, 950)
	observe(t, s, "a3", state.Tree, state.EnvFP, "ls", 4)

	key := Key(state, ".", NormalizeCommand(slow))
	if suggestionFor(s.Suggestions(10), NormalizeCommand(slow), state.Tree) == nil {
		t.Fatal("precondition: the daemon should want this warmed")
	}

	bin := buildHindsight(t)
	run := func(extra ...string) string {
		t.Helper()
		args := append([]string{"warm", "--once", "-v",
			"--daemon", srv.URL, "--repo", repo, "--home", home}, extra...)
		out, err := exec.Command(bin, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("warm: %v\n%s", err, out)
		}
		return string(out)
	}

	out := run()
	if _, ok := store.Lookup(key); ok {
		t.Fatal("warmed a state whose environment it could not reproduce")
	}
	if !strings.Contains(out, "environment fingerprint") {
		t.Fatalf("the refusal should say why:\n%s", out)
	}
	t.Logf("refused, correctly:\n%s", out)

	// Told explicitly where the environment lives, the fingerprint matches and
	// the same suggestion goes through.
	out = run("--env-link", ".venv")
	rec, ok := store.Lookup(key)
	if !ok {
		t.Fatalf("with the environment linked this should have warmed:\n%s", out)
	}
	if rec.Agent != SpeculatorAgent || !rec.Servable {
		t.Fatalf("expected a servable speculative record, got %+v", rec)
	}
	t.Logf("with --env-link .venv: warmed %s in %dms", rec.CmdNorm, rec.DurationMS)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func initRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "warm@test"},
		{"config", "user.name", "warm"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	writeFile(t, dir, ".gitignore", "__pycache__/\n.venv/\n")
	writeFile(t, dir, "src/app.py", "print('hello')\n")
	writeFile(t, dir, "run.sh", "#!/bin/sh\necho hi\n")
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildHindsight builds the CLI for the end-to-end test.
//
// `warm` is dispatched from cmd/hindsight/main.go, which this feature does not
// own — that one line is for whoever owns the file. Until it lands, build
// against an overlaid copy of main.go so the end-to-end path can still be
// exercised. `go build -overlay` is a first-class flag and writes nothing into
// the repository; once the dispatch is wired the overlay drops out.
func buildHindsight(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "hindsight")
	args := []string{"build", "-o", bin}

	mainPath, err := filepath.Abs(filepath.Join("..", "..", "cmd", "hindsight", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `case "warm":`) {
		const anchor = "\tcase \"record\":"
		patched := strings.Replace(string(src), anchor,
			"\tcase \"warm\":\n\t\terr = cmdWarm(os.Args[2:])\n"+anchor, 1)
		if patched == string(src) {
			t.Fatal("could not find the dispatch switch in main.go to overlay")
		}
		overlaid := filepath.Join(dir, "main_with_warm.go")
		if err := os.WriteFile(overlaid, []byte(patched), 0o644); err != nil {
			t.Fatal(err)
		}
		spec, err := json.Marshal(map[string]any{"Replace": map[string]string{mainPath: overlaid}})
		if err != nil {
			t.Fatal(err)
		}
		ov := filepath.Join(dir, "overlay.json")
		if err := os.WriteFile(ov, spec, 0o644); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-overlay", ov)
		t.Log("main.go does not dispatch `warm` yet; building through an overlay")
	}

	args = append(args, "github.com/tjeong117/fasthack/cmd/hindsight")
	if out, err := exec.Command("go", args...).CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

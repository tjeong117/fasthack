package hp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Speculative pre-execution: compute a result before anybody asks for it.
//
// Single-flight already turns five simultaneous demands into one execution and
// four waits. The four still wait the full duration of the first, because
// nothing exists until somebody asks. Speculation is what turns that wait into
// a hit — the work is done while the agents are still thinking.
//
// The safety rule is absolute and it is a rule about *scope*, not about
// confidence:
//
//	Predict what to compute. Never predict what to answer.
//
// A wrong guess about what to precompute wastes a background CPU cycle. A
// wrong guess about what to serve is a wrong answer, and a cache that can give
// a wrong answer has no reason to exist. Those two costs differ by so many
// orders of magnitude that the only defensible design is one where the second
// is structurally impossible rather than merely unlikely.
//
// So nothing in this file decides anything about serving. It emits
// Suggestions, which are hints about *where to spend CPU*. A warmer takes a
// suggestion, materializes the tree for real, runs the command for real, and
// hands the result to `hindsight record`, which is the same program path an
// agent's own execution takes: same purity gate, same servability test, same
// index. The only trace speculation leaves on a record is the agent id, which
// is provenance for accounting and is never consulted by the serve path.
//
// The consequence worth stating plainly: if the model in here is garbage, the
// cache is not wrong, it is only slower and warmer. That is the entire
// justification for the feature existing.

// SpeculatorAgent is the agent id every speculatively produced record carries.
//
// It is provenance, never privilege. Nothing in the lookup path branches on
// it. It exists so that the cost of speculation can be measured after the fact
// (see SpecStats) and so the fleet map does not mistake a warmer for a member
// of the fan-out.
const SpeculatorAgent = "speculator"

// specMinObservations is how much evidence a command needs before it is worth
// guessing at. One observation is an anecdote: an agent typed something once,
// at one state, and may never do it again. Two is the weakest signal that can
// still be called a pattern, and since the cost of being wrong is a wasted
// cycle rather than a wrong answer, there is no reason to demand more.
const specMinObservations = 2

// specRecentSites bounds how far back "a state an agent is at or recently
// left" reaches. Old states are not wrong to warm, they are just unlikely to
// be revisited: agents move forward. Eight keeps the candidate set small
// enough that Suggestions stays cheap enough to call on a timer.
const specRecentSites = 8

// specMaxP caps the estimate short of certainty. Nothing observed from a log
// justifies claiming a command will definitely be asked for.
const specMaxP = 0.99

// defaultSuggestLimit is what /suggest returns when the caller does not say.
const defaultSuggestLimit = 10

// Suggestion is one (state, command) pair worth computing before it is asked
// for. It is an instruction to spend CPU, and explicitly not a claim about
// what the answer will be.
type Suggestion struct {
	Tree   string `json:"tree"`
	EnvFP  string `json:"env_fp"`
	CwdRel string `json:"cwd_rel"`
	// Cmd is a literal spelling observed in the log; CmdNorm is what enters
	// the key. The warmer executes Cmd and must key on CmdNorm, exactly as the
	// hook does.
	Cmd     string `json:"cmd"`
	CmdNorm string `json:"cmd_norm"`
	// Key is what an agent arriving at this state would compute. The warmer
	// recomputes it locally and refuses the suggestion if it disagrees.
	Key    string `json:"key"`
	Policy string `json:"policy"`
	// P is the estimated probability that some agent asks for this key.
	P float64 `json:"p"`
	// DurationMS is the slowest recorded execution of this command, which is
	// what a hit would delete.
	DurationMS int64 `json:"duration_ms"`
	// ExpectedMS is P * DurationMS: the ranking quantity.
	ExpectedMS float64 `json:"expected_ms"`
	Reason     string  `json:"reason"`
}

// SuggestResp is the /suggest payload.
//
// It carries the running speculation accounting alongside the suggestions,
// so a warmer can report its own hit rate without a second round trip, and so
// the number is visible to anyone curious enough to curl the endpoint. If
// speculation is a CPU heater, this is where it says so.
type SuggestResp struct {
	Suggestions []Suggestion `json:"suggestions"`
	Spec        SpecStats    `json:"spec"`
	// Inflight is the number of leases held right now, so a warmer can back
	// off while real agents are executing.
	Inflight int `json:"inflight"`
}

// SpecStats is the honest cost accounting for speculation.
//
// The number that decides whether this feature deserves to exist is HitRate:
// the fraction of speculatively computed results that some agent later
// actually asked for. Everything else is context for it.
type SpecStats struct {
	// Produced is the number of distinct keys the speculator executed.
	Produced int `json:"produced"`
	// Servable is how many of those survived the purity gate. The gap between
	// Produced and Servable is work that could never have been served no
	// matter who asked.
	Servable int `json:"servable"`
	// Used is how many distinct speculative results were later served.
	Used int `json:"used"`
	// Serves counts every serve, including several agents sharing one result.
	Serves  int     `json:"serves"`
	HitRate float64 `json:"hit_rate"`
	// SecondsSpent is CPU the speculator burned; SecondsDeleted is execution
	// time agents did not pay because of it. Net is the difference, and it is
	// allowed to be negative.
	SecondsSpent   float64 `json:"seconds_spent"`
	SecondsDeleted float64 `json:"seconds_deleted"`
	NetSeconds     float64 `json:"net_seconds"`
}

// cmdEvidence is everything the log says about one normalized command.
type cmdEvidence struct {
	cmd     string
	cmdNorm string
	cwdRel  string
	// maxMS is the slowest observation, matching the convention in
	// fastpath.go: a command that is *ever* expensive is worth treating as
	// expensive, because the cost of being wrong is only misranking.
	maxMS   int64
	demands int
	trees   map[string]bool
}

// specSite is one (tree, environment) position in the fan-out. It is the unit
// a suggestion is made about, because those two together are what the key
// binds — a tree alone does not identify a state a command can be run at.
type specSite struct {
	tree   string
	envFP  string
	agents map[string]bool
	seq    int
}

// learnFromLog builds the model: what commands exist, how expensive they are,
// how widely they are run, and where the fleet currently stands.
//
// Deliberately a frequency count and nothing more. It has to be explainable in
// one sentence — "this command was run at three of the four states we have
// seen, and two agents are sitting at this one" — because a model whose
// mistakes cannot be read off the reason string is a model nobody will trust
// enough to leave running.
func learnFromLog(recs []*Record) (cmds []*cmdEvidence, sites []*specSite, trees int, attempted map[string]bool) {
	byCmd := map[string]*cmdEvidence{}
	bySite := map[string]*specSite{}
	pos := map[string]string{}
	treeSet := map[string]bool{}
	attempted = map[string]bool{}
	seq := 0

	for _, r := range recs {
		// Every key anyone has already tried, whoever they were. A key that
		// has been attempted is not worth speculating on: it either landed in
		// the index, failed the purity gate, or was evicted for diverging, and
		// in all three cases repeating it buys nothing.
		if r.Key != "" && r.Decision != "" && r.Decision != "VERIFY" {
			attempted[r.Key] = true
		}

		// The model is built from what real agents demanded. Speculator
		// records must not feed it, or the warmer ends up citing its own past
		// guesses as evidence for the next one.
		if r.Agent == "" || r.Agent == SpeculatorAgent || r.Agent == "verifier" {
			continue
		}
		switch r.Decision {
		case DecisionHit, DecisionLeaseWait, DecisionMiss:
		default:
			continue
		}
		if r.CmdNorm == "" || r.TreeBefore == "" {
			continue
		}
		treeSet[r.TreeBefore] = true
		seq++

		e := byCmd[r.CmdNorm]
		if e == nil {
			e = &cmdEvidence{cmdNorm: r.CmdNorm, trees: map[string]bool{}}
			byCmd[r.CmdNorm] = e
		}
		if r.Cmd != "" {
			e.cmd = r.Cmd
		}
		e.cwdRel = r.CwdRel
		e.demands++
		e.trees[r.TreeBefore] = true
		if r.DurationMS > e.maxMS {
			e.maxMS = r.DurationMS
		}

		id := r.TreeBefore + "\x00" + r.EnvFPBefore
		st := bySite[id]
		if st == nil {
			st = &specSite{tree: r.TreeBefore, envFP: r.EnvFPBefore, agents: map[string]bool{}}
			bySite[id] = st
		}
		st.seq = seq
		// An agent is at exactly one state: the last one it was seen at. Log
		// order is the only ordering available, and it is the right one.
		if prev, ok := pos[r.Agent]; ok && prev != id {
			delete(bySite[prev].agents, r.Agent)
		}
		pos[r.Agent] = id
		st.agents[r.Agent] = true
	}

	for _, e := range byCmd {
		if e.cmd == "" {
			e.cmd = e.cmdNorm
		}
		cmds = append(cmds, e)
	}
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].cmdNorm < cmds[j].cmdNorm })

	for _, st := range bySite {
		sites = append(sites, st)
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].seq > sites[j].seq })
	if len(sites) > specRecentSites {
		sites = sites[:specRecentSites]
	}
	return cmds, sites, len(treeSet), attempted
}

// Suggestions ranks (state, command) pairs by expected saving.
//
// Expected saving is p(some agent asks) * recorded duration. Both factors come
// out of the log: frequency for the first, prior executions of the same
// command for the second. Nothing here consults a model of what the output
// will be, because nothing here needs to — the warmer is going to run the real
// command and find out.
func (s *Server) Suggestions(limit int) []Suggestion {
	if limit <= 0 {
		limit = defaultSuggestLimit
	}
	cmds, sites, trees, attempted := learnFromLog(s.store.Records())
	if trees == 0 {
		return nil
	}
	floor := MinDurationMS()

	var out []Suggestion
	for _, site := range sites {
		for _, e := range cmds {
			if e.demands < specMinObservations {
				continue
			}
			// Same floor the cache itself uses. A command too cheap to be
			// worth caching is too cheap to be worth precomputing, and the
			// arithmetic is not close: interception costs about 42ms whether
			// the result was waiting or not.
			if floor > 0 && e.maxMS < floor {
				continue
			}
			// Re-classified here rather than trusted from the log. Policy is a
			// pure function of the command string and it is the one thing that
			// must never be stale: a RECORD_ONLY command warmed as if it were
			// SERVE would be wasted work at best, and the deny-list exists
			// because some of those commands are wrong to serve at any state.
			policy, reason := Classify(e.cmd)
			if policy != SERVE {
				continue
			}
			key := Key(State{Tree: site.tree, EnvFP: site.envFP}, e.cwdRel, e.cmdNorm)
			if attempted[key] {
				continue
			}
			if _, cached := s.store.Lookup(key); cached {
				continue
			}
			// Under lease means an agent is executing it right now.
			// Duplicating that is the one form of speculation that is
			// unambiguously worse than doing nothing.
			if s.speculationBlocked(key) {
				continue
			}

			base := float64(len(e.trees)) / float64(trees)
			// One more chance than there are agents present, because the
			// interesting case is an agent that has not arrived yet: a state
			// nobody is at is still worth warming if peers are converging.
			p := 1 - math.Pow(1-base, float64(len(site.agents)+1))
			if p > specMaxP {
				p = specMaxP
			}
			out = append(out, Suggestion{
				Tree: site.tree, EnvFP: site.envFP, CwdRel: e.cwdRel,
				Cmd: e.cmd, CmdNorm: e.cmdNorm, Key: key,
				Policy:     policy.String(),
				P:          p,
				DurationMS: e.maxMS,
				ExpectedMS: p * float64(e.maxMS),
				Reason: fmt.Sprintf("run at %d of %d observed states, %s at this one; %s (%s)",
					len(e.trees), trees, plural(len(site.agents), "agent"),
					msText(e.maxMS), reason),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].ExpectedMS != out[j].ExpectedMS {
			return out[i].ExpectedMS > out[j].ExpectedMS
		}
		return out[i].Key < out[j].Key // deterministic ties
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func msText(ms int64) string {
	if ms >= 1000 {
		return strconv.FormatFloat(float64(ms)/1000, 'f', 1, 64) + "s to run"
	}
	return strconv.FormatInt(ms, 10) + "ms to run"
}

// speculationBlocked reports whether a key is off limits: somebody is
// executing it, or it has already been shown to be unservable.
func (s *Server) speculationBlocked(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, held := s.leases[key]; held {
		return true
	}
	return s.unservable[key]
}

// SpecStats reconstructs the speculation ledger from the log.
//
// Produced counts what the speculator executed; Used counts how many of those
// results an agent later asked for. If Used stays near zero while Produced
// climbs, speculation is a CPU heater and this is the number that says so.
//
// One caveat on Used: a Tier-1 hit is logged under the *requester's* key
// rather than the source record's, so a single speculative result promoted to
// several different trees is counted once per requester. HitRate is clamped at
// 1 for that reason.
func (s *Server) SpecStats() SpecStats {
	var st SpecStats
	produced := map[string]bool{}
	servable := map[string]bool{}
	used := map[string]bool{}

	for _, r := range s.store.Records() {
		if r.Key == "" {
			continue
		}
		if r.Agent == SpeculatorAgent {
			if r.Decision == DecisionMiss {
				produced[r.Key] = true
				st.SecondsSpent += float64(r.DurationMS) / 1000
				if r.Servable {
					servable[r.Key] = true
				}
			}
			continue
		}
		switch r.Decision {
		case DecisionHit, DecisionLeaseWait:
			if r.SourceAgent == SpeculatorAgent {
				used[r.Key] = true
				st.Serves++
				st.SecondsDeleted += float64(r.DurationMS) / 1000
			}
		}
	}

	st.Produced, st.Servable, st.Used = len(produced), len(servable), len(used)
	if st.Produced > 0 {
		st.HitRate = math.Min(1, float64(st.Used)/float64(st.Produced))
	}
	st.NetSeconds = st.SecondsDeleted - st.SecondsSpent
	return st
}

// handleSuggest is the /suggest route. Register it in daemon.go's Handler with
//
//	mux.HandleFunc("/suggest", s.handleSuggest)
func (s *Server) handleSuggest(w http.ResponseWriter, r *http.Request) {
	limit := defaultSuggestLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	s.mu.Lock()
	inflight := len(s.leases)
	s.mu.Unlock()
	writeJSON(w, SuggestResp{
		Suggestions: s.Suggestions(limit),
		Spec:        s.SpecStats(),
		Inflight:    inflight,
	})
}

// SuggestHandler exposes /suggest as a composable handler, for callers that
// build their own mux around a Server rather than using Handler().
func (s *Server) SuggestHandler() http.Handler { return http.HandlerFunc(s.handleSuggest) }

// FetchSuggestions asks a daemon what is worth warming. Used by the warmer,
// which lives beside a worktree and therefore in a different process.
func FetchSuggestions(daemon string, limit int) (*SuggestResp, error) {
	resp, err := http.Get(strings.TrimRight(daemon, "/") + "/suggest?limit=" + strconv.Itoa(limit))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s/suggest: %s (is the /suggest route registered?)", daemon, resp.Status)
	}
	var out SuggestResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Reaching an arbitrary tree
// ---------------------------------------------------------------------------

// The daemon cannot execute anything: it has no worktree, and the states worth
// warming are uncommitted working directories that exist only as tree objects.
// So the warmer has to get to a tree, and `git checkout` will not take one —
// it wants a commit-ish.
//
// read-tree loads a bare tree object into an index and checkout-index writes
// that index out to disk. Between them they materialize a state nobody ever
// committed, which is exactly the state a fan-out spends its time in. Measured
// here: the round trip is exact, including the executable bit and files that
// were untracked when the tree was hashed.
//
// The check that this worked is not a check we have to write. `hindsight
// record` re-hashes the worktree after the command and refuses to mark the
// record servable unless the state is unchanged, so a materialization that
// drifted produces an unservable record rather than a wrong one. The warmer
// still verifies up front, because discovering it after paying for a test
// suite is a waste.

// Scratch is a throwaway worktree the warmer uses to reach arbitrary trees.
//
// It is a real linked worktree, which matters for one specific reason: a
// linked worktree gets its own git dir, and therefore its own hp-index side
// index. Sharing an index with the agent's worktree would let two concurrent
// `git add -A` calls interleave and produce a tree describing no real state.
type Scratch struct {
	Dir      string
	repoRoot string
	// excludes are paths `git clean` must not touch between checkouts,
	// because they are linked dependency directories rather than debris.
	excludes []string
	// current is the tree this worktree is known to hold. Reaching a tree is
	// not cheap — measured at 1.6s on a 5,000-file repository, since
	// checkout-index rewrites every file whether it changed or not — and
	// several suggestions landing on one state is the normal case, because
	// convergence is what makes a state worth warming in the first place.
	current string
}

// NewScratch registers a linked worktree at dir without checking anything out.
//
// It must live outside the agent's workspace: a directory tree created inside
// it would change the tree hash that keys the cache.
func NewScratch(repoRoot, dir string) (*Scratch, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	// A warmer killed mid-pass leaves a registration behind, and git refuses
	// to reuse the path until it is pruned.
	_, _ = runGit(repoRoot, "worktree", "prune")
	if _, err := runGit(repoRoot, "worktree", "add", "--detach", "--no-checkout", dir, "HEAD"); err != nil {
		return nil, fmt.Errorf("scratch worktree: %w", err)
	}
	return &Scratch{Dir: dir, repoRoot: repoRoot}, nil
}

// Checkout materializes an arbitrary tree object into the scratch worktree.
//
// A no-op if the worktree already holds this tree and nothing has been run
// since. Getting that wrong would cost a wasted execution rather than a wrong
// answer — the caller re-hashes before it trusts the state, and `hindsight
// record` re-hashes again afterwards — but Dirty exists so it does not happen.
func (sc *Scratch) Checkout(tree string) error {
	if sc.current == tree {
		return nil
	}
	sc.current = ""
	if _, err := runGit(sc.Dir, "read-tree", tree); err != nil {
		return fmt.Errorf("read-tree %s: %w", tree, err)
	}
	// checkout-index writes files and never removes them, so whatever the
	// previous tree left behind — and whatever the previous command wrote —
	// has to go first, or the re-hash will not match and the state is not the
	// one we were asked for. No -x, so a linked dependency directory survives.
	clean := []string{"clean", "-fdq"}
	for _, e := range sc.excludes {
		clean = append(clean, "-e", e)
	}
	if _, err := runGit(sc.Dir, clean...); err != nil {
		return fmt.Errorf("clean: %w", err)
	}
	if _, err := runGit(sc.Dir, "checkout-index", "-a", "-f"); err != nil {
		return fmt.Errorf("checkout-index: %w", err)
	}
	sc.current = tree
	return nil
}

// Dirty forgets which tree the worktree holds. Call it before running anything
// in there, because a command may write and the next Checkout must not assume
// it can skip the work.
func (sc *Scratch) Dirty() { sc.current = "" }

// Link makes a gitignored dependency directory from a donor worktree visible
// in the scratch, so the environment fingerprint can match.
//
// This is the only way a scratch worktree ever reproduces a virtualenv, and it
// is opt-in for a reason: the donor's contents are shared, not copied, so a
// command that writes into them writes into a live agent's environment. Use it
// for read-only suites and nothing else.
//
// The directory itself is real and only its contents are symlinked, which is
// not fussiness. A gitignore pattern written `.venv/` matches directories, and
// git classifies a symlink-to-a-directory as a symlink, so linking the whole
// thing leaves it unignored, `git add -A` picks it up, and the tree hash of
// the scratch no longer matches the tree we were asked to reach. Mirroring one
// level down keeps the repository's own ignore rules working, and has the
// happy side effect that anything the command creates at the top level of the
// directory lands here rather than in the donor.
func (sc *Scratch) Link(donorRoot, name string) error {
	src := filepath.Join(donorRoot, name)
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("donor has no readable %s: %w", name, err)
	}
	dst := filepath.Join(sc.Dir, name)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Symlink(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	sc.excludes = append(sc.excludes, name)
	return nil
}

// Remove unregisters and deletes the scratch worktree.
func (sc *Scratch) Remove() error {
	_, err := runGit(sc.repoRoot, "worktree", "remove", "--force", sc.Dir)
	if err != nil {
		// Best effort: an unremovable worktree is debris, not a failure worth
		// aborting a warm pass over. prune will collect it later.
		_ = os.RemoveAll(sc.Dir)
		_, _ = runGit(sc.repoRoot, "worktree", "prune")
	}
	return err
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = cleanGitEnv()
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err,
			strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// cleanGitEnv strips the variables that would silently redirect git at the
// wrong repository or the wrong index. A caller mid-tree-hash has
// GIT_INDEX_FILE set, and inheriting it here would make read-tree overwrite
// somebody's side index.
func cleanGitEnv() []string {
	drop := map[string]bool{"GIT_INDEX_FILE": true, "GIT_DIR": true, "GIT_WORK_TREE": true}
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && drop[kv[:i]] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

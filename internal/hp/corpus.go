package hp

// Replaying a recorded corpus answers "what would Hindsight have saved here".
// The answer is only worth having if it comes from the code that ships, so
// every decision below that could be re-derived — is this command servable,
// do these two spellings mean the same thing — defers to Classify and
// NormalizeCommand instead of reimplementing them. A second implementation
// would measure the second implementation.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CorpusStep is one recorded command from a replayed agent attempt.
//
// State is the corpus's cumulative hash of the workspace: it changes exactly
// when the workspace changes and is stable across steps that do not touch it.
// That makes it the corpus analogue of our tree hash, and (State, Cmd) the
// corpus analogue of a Hindsight key.
type CorpusStep struct {
	N        int    `json:"n"`
	Cmd      string `json:"cmd"`
	State    string `json:"state_sha256"`
	RCStored int    `json:"rc_stored"`
	RCReplay int    `json:"rc_replay"`
	Mutated  bool   `json:"mutated"`
}

// Trajectory is one agent's attempt at one task.
type Trajectory struct {
	Path       string
	InstanceID string
	Submission string
	Steps      []CorpusStep
}

// CorpusLoad counts what the loader refused, so a run can state its own
// denominator. A corpus is a pile of files written by a long-running harness;
// one truncated write is a skipped record, never a dead measurement.
type CorpusLoad struct {
	Files      int `json:"files"`
	Loaded     int `json:"loaded"`
	Incomplete int `json:"skipped_incomplete"`
	NoSteps    int `json:"skipped_no_steps"`
	NoInstance int `json:"skipped_no_instance"`
	Malformed  int `json:"skipped_malformed"`
}

// Skipped is every record the loader declined to use.
func (l CorpusLoad) Skipped() int {
	return l.Incomplete + l.NoSteps + l.NoInstance + l.Malformed
}

// LoadCorpus reads every record under dir, tolerating anything it cannot
// parse. dir may be the corpus root or the records/ directory inside it.
func LoadCorpus(dir string) ([]Trajectory, error) {
	trajs, _, err := LoadCorpusReport(dir)
	return trajs, err
}

// LoadCorpusReport is LoadCorpus plus the tally of what was skipped and why.
func LoadCorpusReport(dir string) ([]Trajectory, CorpusLoad, error) {
	var load CorpusLoad
	root := dir
	if fi, err := os.Stat(filepath.Join(dir, "records")); err == nil && fi.IsDir() {
		root = filepath.Join(dir, "records")
	}
	fi, err := os.Stat(root)
	if err != nil {
		return nil, load, err
	}
	if !fi.IsDir() {
		return nil, load, fmt.Errorf("corpus: %s is not a directory", root)
	}

	var trajs []Trajectory
	// WalkDir visits lexically, so the loaded order is reproducible.
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree loses records, not the run.
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		load.Files++
		t, verdict := parseCorpusRecord(p)
		switch verdict {
		case recOK:
			load.Loaded++
			trajs = append(trajs, t)
		case recIncomplete:
			load.Incomplete++
		case recNoSteps:
			load.NoSteps++
		case recNoInstance:
			load.NoInstance++
		default:
			load.Malformed++
		}
		return nil
	})
	if err != nil {
		return nil, load, err
	}
	return trajs, load, nil
}

type recVerdict int

const (
	recOK recVerdict = iota
	recIncomplete
	recNoSteps
	recNoInstance
	recMalformed
)

// corpusSummary sits at the top level in some emitted shapes and under
// evidence in others. Both are read rather than picking one and being brittle.
type corpusSummary struct {
	InstanceID string  `json:"instance_id"`
	Steps      int     `json:"steps"`
	WallS      float64 `json:"wall_s"`
}

type corpusRecord struct {
	Status  string        `json:"status"`
	Summary corpusSummary `json:"summary"`
	Source  struct {
		InstanceID string `json:"instance_id"`
		Metadata   struct {
			Submission string `json:"submission"`
			InstanceID string `json:"instance_id"`
		} `json:"metadata"`
	} `json:"source"`
	Evidence struct {
		Summary corpusSummary `json:"summary"`
		Steps   []CorpusStep  `json:"steps"`
	} `json:"evidence"`
}

func parseCorpusRecord(path string) (Trajectory, recVerdict) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Trajectory{}, recMalformed
	}
	var rec corpusRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return Trajectory{}, recMalformed
	}
	if rec.Status != "complete" {
		return Trajectory{}, recIncomplete
	}
	if len(rec.Evidence.Steps) == 0 {
		return Trajectory{}, recNoSteps
	}
	id := firstNonEmpty(rec.Summary.InstanceID, rec.Evidence.Summary.InstanceID,
		rec.Source.InstanceID, rec.Source.Metadata.InstanceID)
	if id == "" {
		return Trajectory{}, recNoInstance
	}
	return Trajectory{
		Path:       path,
		InstanceID: id,
		Submission: firstNonEmpty(rec.Source.Metadata.Submission, "unknown"),
		Steps:      rec.Evidence.Steps,
	}, recOK
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// InstanceGroup is every recorded attempt at one task.
type InstanceGroup struct {
	InstanceID string
	Attempts   []Trajectory
}

// MultiAgentInstances returns the tasks more than one agent attempted, which
// are the only tasks that can say anything about sharing between agents.
// Single-attempt tasks leave the numerator and the denominator together;
// keeping them in the denominator alone would quietly deflate every rate.
func MultiAgentInstances(trajs []Trajectory) []InstanceGroup {
	byID := map[string][]Trajectory{}
	for _, t := range trajs {
		byID[t.InstanceID] = append(byID[t.InstanceID], t)
	}
	out := make([]InstanceGroup, 0, len(byID))
	for id, attempts := range byID {
		if len(attempts) < 2 {
			continue
		}
		// The totals do not depend on this order. The self/cross split and the
		// step table do, because they turn on who got somewhere first, so it
		// is fixed here rather than left to map iteration.
		sort.Slice(attempts, func(i, j int) bool {
			if attempts[i].Submission != attempts[j].Submission {
				return attempts[i].Submission < attempts[j].Submission
			}
			return attempts[i].Path < attempts[j].Path
		})
		out = append(out, InstanceGroup{InstanceID: id, Attempts: attempts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out
}

// Keying selects which key the replay simulates.
type Keying int

const (
	// KeyState is (workspace state, command): the corpus analogue of how
	// Hindsight actually keys, and the only one of the two we can defend.
	KeyState Keying = iota
	// KeyCommand is the command string alone. It ignores the workspace, so it
	// counts a second `pytest` as avoidable even after the tree moved
	// underneath it. It is an upper bound, reported for contrast.
	KeyCommand
)

func (k Keying) String() string {
	if k == KeyCommand {
		return "command"
	}
	return "state"
}

// ParseKeying maps the --key flag onto a Keying.
func ParseKeying(s string) (Keying, error) {
	switch s {
	case "", "state":
		return KeyState, nil
	case "command", "cmd":
		return KeyCommand, nil
	}
	return KeyState, fmt.Errorf("unknown keying %q (want \"state\" or \"command\")", s)
}

// Tally is one row of the report: how many commands landed in this slice, and
// how many of them a cache would not have had to run.
type Tally struct {
	Commands   int `json:"commands"`
	Avoidable  int `json:"avoidable"`
	SelfReuse  int `json:"self_reuse"`
	CrossAgent int `json:"cross_agent"`
}

// ReuseRate is avoidable commands as a share of this row's commands.
func (t Tally) ReuseRate() float64 { return pct(t.Avoidable, t.Commands) }

// CrossRate is cross-agent reuse as a share of this row's commands.
func (t Tally) CrossRate() float64 { return pct(t.CrossAgent, t.Commands) }

func pct(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return 100 * float64(n) / float64(d)
}

type reuseKind int

const (
	firstRun reuseKind = iota
	selfReuse
	crossAgent
)

func (t *Tally) add(k reuseKind) {
	t.Commands++
	switch k {
	case selfReuse:
		t.Avoidable++
		t.SelfReuse++
	case crossAgent:
		t.Avoidable++
		t.CrossAgent++
	}
}

// PolicyTally is one policy's slice of the corpus.
type PolicyTally struct {
	Policy string `json:"policy"`
	Tally
}

// ClassTally is one command class's slice of the corpus.
type ClassTally struct {
	Class string `json:"class"`
	Tally
}

// StepTally is one band of step indices.
type StepTally struct {
	Steps string `json:"steps"`
	Tally
}

// ReplayReport is the whole answer, in the order a reader wants it.
type ReplayReport struct {
	Keying      string        `json:"keying"`
	Load        CorpusLoad    `json:"load"`
	Instances   int           `json:"instances"`
	Attempts    int           `json:"attempts"`
	FanOut      []int         `json:"fan_out"`
	Overall     Tally         `json:"overall"`
	UnderPolicy Tally         `json:"under_policy"`
	ByPolicy    []PolicyTally `json:"by_policy"`
	ByClass     []ClassTally  `json:"by_class"`
	ByStep      []StepTally   `json:"by_step,omitempty"`
}

// stepBuckets band the step index. The opening is split finely because that is
// where the overlap is, and it is also where every agent in a fan-out is
// running simultaneously.
var stepBuckets = []struct {
	label  string
	lo, hi int
}{
	{"0-2", 0, 2},
	{"3-9", 3, 9},
	{"10-19", 10, 19},
	{"20-49", 20, 49},
	{"50+", 50, 1 << 30},
}

func stepBucket(n int) int {
	for i, b := range stepBuckets {
		if n >= b.lo && n <= b.hi {
			return i
		}
	}
	return len(stepBuckets) - 1
}

// sighting is what the cache knows about one key so far.
type sighting struct {
	first int  // attempt that ran it first
	multi bool // more than one attempt has run it
}

// Replay walks every step of every attempt of every multi-agent task and asks,
// for each command, whether an identical command at an identical state had
// already been run.
//
// The headline count is order-independent: it is the total minus the number of
// distinct keys, so no model of how the agents interleave is needed to produce
// it. Telling self-reuse apart from cross-agent reuse does need one, because
// it turns on who got there first. Attempts are therefore walked interleaved
// by step index — agent three's tenth command is contemporaneous with agent
// one's tenth, not with its own hundredth — with ties broken by the attempt
// order MultiAgentInstances fixed. Conflating the two would be the expensive
// mistake here: only cross-agent reuse is an argument for fanning out.
func Replay(trajs []Trajectory, k Keying) ReplayReport {
	rep := ReplayReport{Keying: k.String()}
	policies := []Policy{SERVE, RECORD_ONLY, PASSTHROUGH}
	byPolicy := map[Policy]*Tally{}
	for _, p := range policies {
		byPolicy[p] = &Tally{}
	}
	byClass := map[string]*Tally{}
	byStep := make([]Tally, len(stepBuckets))
	verdicts := map[string]cmdVerdict{}

	for _, g := range MultiAgentInstances(trajs) {
		rep.Instances++
		rep.Attempts += len(g.Attempts)
		rep.FanOut = append(rep.FanOut, len(g.Attempts))

		evs := interleave(g, k)
		seen := map[string]*sighting{}
		for _, e := range evs {
			kind := firstRun
			if s, ok := seen[e.key]; ok {
				// A peer's result is the interesting one, so it wins the tie:
				// if any other attempt has run this key, an agent repeating
				// itself here was still beaten to it by a peer.
				if s.multi || s.first != e.attempt {
					kind = crossAgent
				} else {
					kind = selfReuse
				}
				if s.first != e.attempt {
					s.multi = true
				}
			} else {
				seen[e.key] = &sighting{first: e.attempt}
			}

			v := classifyOnce(verdicts, e.cmd)
			rep.Overall.add(kind)
			byPolicy[v.policy].add(kind)
			if byClass[v.class] == nil {
				byClass[v.class] = &Tally{}
			}
			byClass[v.class].add(kind)
			byStep[stepBucket(e.n)].add(kind)
		}
	}

	rep.UnderPolicy = *byPolicy[SERVE]
	for _, p := range policies {
		rep.ByPolicy = append(rep.ByPolicy, PolicyTally{Policy: p.String(), Tally: *byPolicy[p]})
	}
	for _, c := range sortedClasses(byClass) {
		rep.ByClass = append(rep.ByClass, ClassTally{Class: c, Tally: *byClass[c]})
	}
	for i, b := range stepBuckets {
		rep.ByStep = append(rep.ByStep, StepTally{Steps: b.label, Tally: byStep[i]})
	}
	return rep
}

// event is one recorded command, placed on the instance's shared clock.
type event struct {
	attempt int
	n       int
	cmd     string
	key     string
}

func interleave(g InstanceGroup, k Keying) []event {
	var evs []event
	for i, t := range g.Attempts {
		for _, st := range t.Steps {
			evs = append(evs, event{attempt: i, n: st.N, cmd: st.Cmd, key: replayKey(k, st)})
		}
	}
	sort.SliceStable(evs, func(a, b int) bool {
		if evs[a].n != evs[b].n {
			return evs[a].n < evs[b].n
		}
		return evs[a].attempt < evs[b].attempt
	})
	return evs
}

func replayKey(k Keying, st CorpusStep) string {
	cmd := NormalizeCommand(st.Cmd)
	if k == KeyCommand {
		return cmd
	}
	return st.State + "\x00" + cmd
}

// cmdVerdict is what the shipping classifier says about one command string.
type cmdVerdict struct {
	policy Policy
	class  string
}

// Classify is pure, so classifying each distinct string once is free accuracy
// rather than a shortcut.
func classifyOnce(cache map[string]cmdVerdict, cmd string) cmdVerdict {
	if v, ok := cache[cmd]; ok {
		return v
	}
	p, _ := Classify(cmd)
	v := cmdVerdict{policy: p, class: CommandClass(cmd)}
	cache[cmd] = v
	return v
}

// classOrder ranks command classes by how much running one costs. The class
// table exists to answer "are the avoidable commands cheap reads or expensive
// suites", so where a chain contains both, the expensive one names it.
var classOrder = []string{"build", "install", "mutation", "non-hermetic", "metadata", "read", "unrecognized", "other"}

func classRank(c string) int {
	for i, name := range classOrder {
		if name == c {
			return i
		}
	}
	return len(classOrder)
}

func sortedClasses(m map[string]*Tally) []string {
	out := make([]string, 0, len(m))
	for c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := classRank(out[i]), classRank(out[j]); ri != rj {
			return ri < rj
		}
		return out[i] < out[j]
	})
	return out
}

// CommandClass labels a command by what running it costs.
//
// This is deliberately not the same question as Classify's: the policy of
// `cd /testbed && python -m pytest` is PASSTHROUGH, because the chain rule
// takes the strictest segment and `cd` is unrecognized, but the *cost* of that
// command is a test suite. 37% of the commands in the reference corpus open
// with `cd`, so labelling by the strictest segment would file almost every
// expensive command under "unrecognized" and the table would answer nothing.
//
// Each segment is still put through the real Classify — the label follows the
// classifier rather than a second opinion about what a command does — and the
// most expensive segment names the chain.
func CommandClass(cmd string) string {
	best := ""
	for _, seg := range chainSegments(cmd) {
		if strings.TrimSpace(seg) == "" {
			continue
		}
		_, reason := Classify(seg)
		c := classOfReason(reason)
		if best == "" || classRank(c) < classRank(best) {
			best = c
		}
	}
	if best == "" {
		return "other"
	}
	return best
}

// classOfReason reads Classify's reason rather than re-deriving anything, so
// the classes follow the classifier. An unrecognized reason lands in "other",
// which costs a tidy table and never a count.
func classOfReason(reason string) string {
	r := reason
	if rest, ok := strings.CutPrefix(r, "chain: strictest segment is "); ok {
		if i := strings.Index(rest, " ("); i >= 0 && strings.HasSuffix(rest, ")") {
			r = rest[i+2 : len(rest)-1]
		}
	}
	// "uv run -> build: pytest" describes the inner command.
	if i := strings.LastIndex(r, "-> "); i >= 0 {
		r = r[i+3:]
	}
	switch {
	case strings.HasPrefix(r, "read:"):
		return "read"
	case strings.HasPrefix(r, "build:"):
		return "build"
	case strings.HasPrefix(r, "install:"):
		return "install"
	case strings.HasPrefix(r, "mutation:"):
		return "mutation"
	case strings.HasPrefix(r, "non-hermetic:"):
		return "non-hermetic"
	case strings.HasPrefix(r, "prints file metadata"), strings.HasPrefix(r, "ls long format"):
		return "metadata"
	case strings.HasPrefix(r, "unrecognized"), strings.HasPrefix(r, "unparseable"),
		strings.HasPrefix(r, "empty command"), strings.HasPrefix(r, "no command after"):
		return "unrecognized"
	}
	return "other"
}

// chainSegments splits a command on &&, ||, ; and | without splitting inside
// quotes. This is for labelling only: Classify applies its own chain rule to
// whatever it is handed, so a segment this gets wrong is still classified
// correctly, it is just filed under a coarser name.
func chainSegments(cmd string) []string {
	var segs []string
	var b strings.Builder
	var quote rune
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case quote != 0:
			b.WriteRune(c)
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
			b.WriteRune(c)
		case c == '&' && i+1 < len(rs) && rs[i+1] == '&',
			c == '|' && i+1 < len(rs) && rs[i+1] == '|':
			i++
			segs = append(segs, b.String())
			b.Reset()
		case c == '|', c == ';', c == '\n':
			segs = append(segs, b.String())
			b.Reset()
		default:
			b.WriteRune(c)
		}
	}
	return append(segs, b.String())
}

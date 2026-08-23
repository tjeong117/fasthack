package hp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// A transition corpus, produced as a byproduct of caching.
//
// Every command the hook intercepts already yields (tree_before, cmd,
// tree_after, exit_code, duration_ms), measured on both sides. That is a real
// state transition, observed rather than predicted, and it is the shape a
// transition world model would need. Nothing here generates data; it selects
// the subset of the existing log that is honestly a transition and drops the
// rest, loudly.

// TransitionSchema identifies the row format. It is both the identity and the
// version: a corpus that leaves this repo has to say what it is without
// reference to anything in it.
const TransitionSchema = "hindsight.transition/v1"

// LeakageRule is invariant 6, quoted verbatim so a consumer never has to take
// the exporter's word for what was enforced.
const LeakageRule = "only a real action with a subsequently observed response becomes a retrieval transition; " +
	"generated predictions, simulator rollouts, teacher data and judgments cannot enter this index."

// LeakageRuleSource is the attribution the rule ships with.
const LeakageRuleSource = "Experiential Labs (Apache-2.0), adopted with attribution"

// LeakageRuleEnforcement states where the rule lives in code, because a rule
// enforced only in prose is not enforced.
const LeakageRuleEnforcement = "hp.TransitionFrom admits a record only when decision == MISS, the single path on " +
	"which hindsight record actually executed the command and measured the state on both sides of it. " +
	"HIT, LEASE_WAIT and PASSTHROUGH describe a served or unintercepted command, and VERIFY is a verdict " +
	"about a served record rather than an execution; all four are excluded and counted."

// The daemon writes VERIFY records (internal/hp/daemon.go) without a constant
// in store.go. Naming it here rather than adding to a file this package's
// owner froze.
const decisionVerifyRecord = "VERIFY"

// Transition is one observed state transition: the world before, the action,
// the world after, and what the environment returned.
//
// It deliberately does not carry stdout or stderr. The blob references are
// kept because provenance is cheap and a reader may want to audit a row
// against the cache, but no trainer consumes command output, so the bytes stay
// where they are.
type Transition struct {
	Schema string  `json:"schema"`
	TS     float64 `json:"ts"`
	Agent  string  `json:"agent"`

	// Before: the state the command was issued from. CwdRel is part of the
	// state, not decoration — the same command means different things in
	// different directories, which is why it is in the cache key too.
	TreeBefore  string `json:"tree_before"`
	EnvFPBefore string `json:"env_fp_before"`
	CwdRel      string `json:"cwd_rel"`

	// Action. CmdNorm is the form the key is derived from; Cmd is kept
	// verbatim because a normalizer change should not silently rewrite
	// history in an exported corpus.
	Cmd     string `json:"cmd"`
	CmdNorm string `json:"cmd_norm"`

	// After: the state measured once the command exited.
	TreeAfter  string `json:"tree_after"`
	EnvFPAfter string `json:"env_fp_after"`

	// Observed response.
	ExitCode   int   `json:"exit_code"`
	DurationMS int64 `json:"duration_ms"`

	// Labels, all derived from the two states rather than declared.
	// Mutated is the disjunction; the two halves are kept separate because
	// they answer different questions. A tree move is an edit to tracked or
	// untracked files; an environment move is almost always an install into a
	// gitignored virtualenv, which the tree hash structurally cannot see.
	Mutated     bool `json:"mutated"`
	TreeMutated bool `json:"tree_mutated"`
	EnvMutated  bool `json:"env_mutated"`

	// Servable is the cache's own verdict, passed through unmodified.
	// Servable implies !Mutated, but not the reverse: a non-mutating command
	// can still be unservable because the classifier called it non-hermetic,
	// its output was truncated, or it ran below the duration floor.
	Servable bool `json:"servable"`

	// Provenance. Policy and Reason are what the classifier said; Key ties
	// the row back to the cache entry it came from.
	Policy string `json:"policy"`
	Reason string `json:"reason,omitempty"`
	Key    string `json:"key,omitempty"`

	// For the cache, not for a model. See the note on Transition.
	StdoutBlob string `json:"stdout_blob,omitempty"`
	StderrBlob string `json:"stderr_blob,omitempty"`
}

// Exclusion reasons. Fixed strings so the counts in two exports are
// comparable, and phrased so the reason reads as an argument rather than a
// code.
const (
	ExcludeHit         = "decision HIT: a replay served from cache, not an execution"
	ExcludeLeaseWait   = "decision LEASE_WAIT: served from a peer's in-flight execution, not an execution"
	ExcludePassthrough = "decision PASSTHROUGH: ran unintercepted, so no state was observed around it"
	ExcludeVerify      = "decision VERIFY: a verdict about a served record, not an execution"
	ExcludeUnknown     = "decision unrecognised: excluded by default"
	ExcludeNoAfter     = "no observed after-state: the transition was never completed"
	ExcludeMalformed   = "malformed: log line did not parse as a record"
	ExcludeNonMutating = "filtered out: non-mutating (--mutating-only)"
	ExcludeMutating    = "filtered out: mutating (--include-nonmutating=false)"
)

// TransitionFrom converts one record. The second return is the reason it was
// rejected; ok is false whenever that reason is non-empty.
//
// This is where the leakage rule is mechanical. Only decision == MISS is
// admitted, because that is the only path on which the command really ran:
// hindsight record executes it, captures the exit code and duration, and
// recomputes the tree hash and environment fingerprint afterwards. Every other
// decision describes something the environment was never asked.
func TransitionFrom(r *Record) (Transition, string, bool) {
	if r == nil {
		return Transition{}, ExcludeMalformed, false
	}
	switch r.Decision {
	case DecisionMiss:
	case DecisionHit:
		return Transition{}, ExcludeHit, false
	case DecisionLeaseWait:
		return Transition{}, ExcludeLeaseWait, false
	case DecisionPassthrough:
		return Transition{}, ExcludePassthrough, false
	case decisionVerifyRecord:
		return Transition{}, ExcludeVerify, false
	default:
		return Transition{}, unknownDecisionReason(r.Decision), false
	}

	// A record with no after-state is an action whose response was never
	// observed. Under the leakage rule that is not a transition, and to a
	// trainer it is a row with no s'.
	if r.TreeBefore == "" || r.TreeAfter == "" || r.EnvFPBefore == "" || r.EnvFPAfter == "" {
		return Transition{}, ExcludeNoAfter, false
	}

	treeMoved := r.TreeAfter != r.TreeBefore
	envMoved := r.EnvFPAfter != r.EnvFPBefore
	return Transition{
		Schema:      TransitionSchema,
		TS:          r.TS,
		Agent:       r.Agent,
		TreeBefore:  r.TreeBefore,
		EnvFPBefore: r.EnvFPBefore,
		CwdRel:      r.CwdRel,
		Cmd:         r.Cmd,
		CmdNorm:     r.CmdNorm,
		TreeAfter:   r.TreeAfter,
		EnvFPAfter:  r.EnvFPAfter,
		ExitCode:    r.ExitCode,
		DurationMS:  r.DurationMS,
		Mutated:     treeMoved || envMoved,
		TreeMutated: treeMoved,
		EnvMutated:  envMoved,
		Servable:    r.Servable,
		Policy:      r.Policy,
		Reason:      r.Reason,
		Key:         r.Key,
		StdoutBlob:  r.StdoutBlob,
		StderrBlob:  r.StderrBlob,
	}, "", true
}

// unknownDecisionReason keeps the offending value visible without letting a
// corrupt log grow the reason table without bound.
func unknownDecisionReason(d string) string {
	if d == "" {
		return ExcludeUnknown + " (empty)"
	}
	clean := make([]rune, 0, 24)
	for _, c := range d {
		if len(clean) == 24 {
			break
		}
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			clean = append(clean, c)
		}
	}
	if len(clean) == 0 {
		return ExcludeUnknown
	}
	return ExcludeUnknown + " (" + string(clean) + ")"
}

// TransitionFilter selects which population to export. The two are useful for
// different questions: non-mutating transitions are what the cache can serve,
// mutating ones are what actually moved the workspace.
type TransitionFilter int

const (
	FilterAll TransitionFilter = iota
	FilterMutatingOnly
	FilterNonMutatingOnly
)

func (f TransitionFilter) String() string {
	switch f {
	case FilterMutatingOnly:
		return "mutating-only"
	case FilterNonMutatingOnly:
		return "non-mutating-only"
	default:
		return "all"
	}
}

// admits reports whether a converted transition survives the filter.
func (f TransitionFilter) admits(t Transition) (string, bool) {
	switch f {
	case FilterMutatingOnly:
		if !t.Mutated {
			return ExcludeNonMutating, false
		}
	case FilterNonMutatingOnly:
		if t.Mutated {
			return ExcludeMutating, false
		}
	}
	return "", true
}

// ExportStats is the accounting. Scanned == Exported + sum(Excluded), always;
// the exporter asserts it rather than trusting it, because a corpus whose
// drops do not add up has had something removed silently, which is the exact
// failure the leakage rule exists to prevent.
type ExportStats struct {
	Scanned  int            `json:"records_scanned"`
	Exported int            `json:"transitions_exported"`
	Excluded map[string]int `json:"excluded_by_reason"`
}

// ExcludedTotal is the number of scanned records that did not become a row.
func (s ExportStats) ExcludedTotal() int {
	n := 0
	for _, c := range s.Excluded {
		n += c
	}
	return n
}

// Balanced is the invariant. Every scanned record either became a transition
// or is counted under a reason it did not.
func (s ExportStats) Balanced() bool {
	return s.Scanned == s.Exported+s.ExcludedTotal()
}

// Reasons returns the exclusion reasons in descending count order, ties broken
// alphabetically, so two runs of the same export print the same summary.
func (s ExportStats) Reasons() []string {
	out := make([]string, 0, len(s.Excluded))
	for r := range s.Excluded {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if s.Excluded[out[i]] != s.Excluded[out[j]] {
			return s.Excluded[out[i]] > s.Excluded[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

func (s *ExportStats) drop(reason string) {
	if s.Excluded == nil {
		s.Excluded = map[string]int{}
	}
	s.Excluded[reason]++
}

// ScanTransitions reads a log.jsonl stream and returns the transitions in it
// together with a full account of what was dropped.
//
// It parses the log directly rather than going through Store. Two reasons:
// OpenStore creates directories under the cache root, and an exporter pointed
// at a mistyped --home should read nothing rather than write something; and a
// torn line is invisible through the store's index but is a real gap in the
// corpus, so it is counted here instead of skipped.
func ScanTransitions(r io.Reader, filter TransitionFilter) ([]Transition, ExportStats, error) {
	stats := ExportStats{Excluded: map[string]int{}}
	out := []Transition{}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue // a blank line is not a record, so it is not scanned either
		}
		stats.Scanned++

		var rec Record
		if json.Unmarshal([]byte(line), &rec) != nil {
			stats.drop(ExcludeMalformed)
			continue
		}
		t, reason, ok := TransitionFrom(&rec)
		if !ok {
			stats.drop(reason)
			continue
		}
		if reason, ok := filter.admits(t); !ok {
			stats.drop(reason)
			continue
		}
		out = append(out, t)
		stats.Exported++
	}
	if err := sc.Err(); err != nil {
		return nil, stats, err
	}
	if !stats.Balanced() {
		return nil, stats, fmt.Errorf("export accounting does not balance: scanned %d, exported %d, excluded %d",
			stats.Scanned, stats.Exported, stats.ExcludedTotal())
	}
	return out, stats, nil
}

// ExportManifest is the header on a --format json export.
//
// A bare array of transitions with no statement of where it came from is worth
// much less than the same array with one, so the header carries the rule that
// was enforced, where it was enforced, what was dropped, and what the corpus
// cannot answer.
type ExportManifest struct {
	Schema     string `json:"schema"`
	Generator  string `json:"generator"`
	ExportedAt string `json:"exported_at"`
	Source     string `json:"source"`
	SourceLog  string `json:"source_log"`
	Filter     string `json:"filter"`

	Scanned  int            `json:"records_scanned"`
	Exported int            `json:"transitions_exported"`
	Excluded int            `json:"records_excluded"`
	Reasons  map[string]int `json:"excluded_by_reason"`

	LeakageRule            string `json:"leakage_rule"`
	LeakageRuleSource      string `json:"leakage_rule_source"`
	LeakageRuleEnforcement string `json:"leakage_rule_enforcement"`

	Limitations []string `json:"limitations"`
}

// ExportDocument is what --format json emits: one object, header first.
type ExportDocument struct {
	Meta        ExportManifest `json:"meta"`
	Transitions []Transition   `json:"transitions"`
}

// TransitionLimitations are the constraints established in design_doc.md,
// carried in the file itself so a consumer meets them before they meet a
// surprise.
var TransitionLimitations = []string{
	"A tree hash is an identity, not a delta. Line-bucket and path-movement labels still require " +
		"git diff --numstat and git status --porcelain against the two trees; the tree hash is an " +
		"additional finer-grained bit, not a replacement for them.",
	"stdout and stderr are referenced by content hash and not included. No trainer consumes command " +
		"output, so emitting it would serve the cache rather than a model.",
	"The corpus is only as large as the runs that produced it, and those runs are short. Check " +
		"records_scanned before assuming this is a dataset.",
	"Rows are observations from one repository at a time. The cache root is per-repo, so a corpus " +
		"spanning repositories has to be concatenated deliberately and labelled as such.",
}

// NewExportManifest assembles the header from a completed scan.
func NewExportManifest(source, logPath, exportedAt string, filter TransitionFilter, stats ExportStats) ExportManifest {
	return ExportManifest{
		Schema:                 TransitionSchema,
		Generator:              "hindsight export",
		ExportedAt:             exportedAt,
		Source:                 source,
		SourceLog:              logPath,
		Filter:                 filter.String(),
		Scanned:                stats.Scanned,
		Exported:               stats.Exported,
		Excluded:               stats.ExcludedTotal(),
		Reasons:                stats.Excluded,
		LeakageRule:            LeakageRule,
		LeakageRuleSource:      LeakageRuleSource,
		LeakageRuleEnforcement: LeakageRuleEnforcement,
		Limitations:            TransitionLimitations,
	}
}

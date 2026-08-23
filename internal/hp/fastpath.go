package hp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// Caching is not free. Every intercepted command pays a process spawn, two
// tree hashes, two environment fingerprints and a daemon round trip. Measured
// on this machine that is roughly 35 ms on a small repo and 350 ms on a
// 50,000-file one.
//
// So a command that takes 3 ms costs an order of magnitude more to cache than
// it could ever save, and the classifier happily marks plenty of those SERVE:
// echo, pwd, basename, a small cat. Serving them is worse than useless.
//
// The fastpath is a tiny memo of how long each normalized command has actually
// taken. The hook consults it *before* computing any hash, so a command known
// to be cheap costs one small file read rather than two tree hashes.
//
// It keys on the command alone, deliberately not on workspace state. Duration
// is a property of the command, and mixing state in would make the memo as
// expensive to consult as the thing it is avoiding.

// DefaultMinDurationMS is the floor below which caching cannot pay for itself.
//
// The obvious floor is the hook's own cost, and that is what this used to be.
// It answers the wrong question. Measured on this machine: a bare
// `grep -rn def src/` takes 4.7 ms, the full interception path takes 52.8 ms,
// and a command the memo already knows is cheap costs 11.2 ms — that residual
// being Go process startup, which any hook pays and none can avoid.
//
// So the marginal cost of *trying* to cache, rather than passing straight
// through, is about 42 ms. And you pay it on every miss, not every hit. The
// break-even is therefore
//
//	hit_rate * duration > 42 ms
//
// At the 5.4% cross-agent reuse that reads actually show in the measured
// corpus, that needs a command taking roughly 800 ms. Reads are 48.8% of all
// commands and almost none of them are anywhere near that, so caching them is
// net negative however cheap the cache is.
//
// 500 ms is deliberately conservative against the 800 ms the arithmetic
// suggests, because hit rates are far higher than 5.4% during the opening
// lockstep phase of a fan-out, which is where most real hits happen.
const DefaultMinDurationMS = 500

// fastpathSamples is how many observations we need before trusting a verdict.
// One fast run of a normally-slow command (everything warm, nothing to do)
// should not permanently disable caching for it.
//
// Two is enough. Three costs a third more full-price interceptions before the
// memo takes effect, and the downside of being wrong is only that we stop
// caching something cheap.
const fastpathSamples = 2

// alwaysCheap are commands that cannot be slow whatever their arguments, so
// there is no reason to pay even one full interception to discover it.
//
// Deliberately short. `grep` and `cat` are absent because they are cheap on a
// small target and expensive on a large one, and the duration memo tracks the
// exact command string, so it distinguishes `grep foo README.md` from
// `grep -rn TODO .` where a name-based list could not.
var alwaysCheap = map[string]bool{
	"echo": true, "pwd": true, "true": true, "false": true, ":": true,
	"basename": true, "dirname": true, "printf": true,
}

// MinDurationMS is the configured floor. HP_MIN_DURATION_MS overrides it, and
// 0 disables the fastpath entirely.
func MinDurationMS() int64 {
	if v := os.Getenv("HP_MIN_DURATION_MS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return DefaultMinDurationMS
}

type fastpathEntry struct {
	// MaxMS is the slowest observation. We take the maximum rather than the
	// mean so that a command which is *ever* expensive keeps being cached;
	// abstaining from the memo is always the safe direction, since the cost of
	// being wrong is only wasted overhead.
	MaxMS int64 `json:"max_ms"`
	N     int   `json:"n"`
}

type Fastpath struct {
	path string
	mu   sync.Mutex
	m    map[string]fastpathEntry
}

func LoadFastpath(home string) *Fastpath {
	f := &Fastpath{path: filepath.Join(home, "fastpath.json"), m: map[string]fastpathEntry{}}
	b, err := os.ReadFile(f.path)
	if err != nil {
		return f
	}
	_ = json.Unmarshal(b, &f.m) // a corrupt memo is an empty memo, never an error
	return f
}

// KnownFast reports whether this command has consistently run faster than the
// floor, and is therefore not worth intercepting.
func (f *Fastpath) KnownFast(cmdNorm string, floorMS int64) bool {
	if floorMS <= 0 {
		return false
	}
	if head := firstWord(cmdNorm); alwaysCheap[head] {
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[cmdNorm]
	return ok && e.N >= fastpathSamples && e.MaxMS < floorMS
}

func firstWord(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			return s[:i]
		}
	}
	return s
}

// Observe records how long a command took.
func (f *Fastpath) Observe(cmdNorm string, durMS int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.m[cmdNorm]
	e.N++
	if durMS > e.MaxMS {
		e.MaxMS = durMS
	}
	f.m[cmdNorm] = e
}

// Save writes the memo. Best effort: losing it costs a little overhead on the
// next run and nothing else, so a failure here must never surface to the agent.
func (f *Fastpath) Save() {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := json.Marshal(f.m)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return
	}
	tmp := f.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, f.path)
	}
}

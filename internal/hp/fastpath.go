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
// Roughly the measured hook overhead on a small repository.
const DefaultMinDurationMS = 50

// fastpathSamples is how many observations we need before trusting a verdict.
// One fast run of a normally-slow command (everything warm, nothing to do)
// should not permanently disable caching for it.
const fastpathSamples = 3

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
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[cmdNorm]
	return ok && e.N >= fastpathSamples && e.MaxMS < floorMS
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

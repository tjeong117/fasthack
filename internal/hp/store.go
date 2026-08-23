package hp

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Record is the frozen log line. One JSON object per line in log.jsonl,
// appended only by the daemon.
type Record struct {
	V           int     `json:"v"`
	TS          float64 `json:"ts"`
	Agent       string  `json:"agent"`
	Cmd         string  `json:"cmd"`
	CmdNorm     string  `json:"cmd_norm"`
	CwdRel      string  `json:"cwd_rel"`
	TreeBefore  string  `json:"tree_before"`
	EnvFPBefore string  `json:"env_fp_before"`
	TreeAfter   string  `json:"tree_after"`
	EnvFPAfter  string  `json:"env_fp_after"`
	Key         string  `json:"key"`
	Policy      string  `json:"policy"`
	Reason      string  `json:"reason"`
	Decision    string  `json:"decision"`
	Servable    bool    `json:"servable"`
	ExitCode    int     `json:"exit_code"`
	DurationMS  int64   `json:"duration_ms"`
	StdoutBlob  string  `json:"stdout_blob"`
	StderrBlob  string  `json:"stderr_blob"`
	SourceAgent string  `json:"source_agent"`
	Verified    *bool   `json:"verified"`
}

// Decisions.
const (
	DecisionHit         = "HIT"
	DecisionMiss        = "MISS"
	DecisionLeaseWait   = "LEASE_WAIT"
	DecisionPassthrough = "PASSTHROUGH"
)

// Home resolves the cache root.
//
// It must live outside the workspace. Blobs written inside the tree would
// change the tree hash that keys the cache, which is a feedback loop that
// poisons every subsequent key.
func Home(repoRoot string) string {
	if h := os.Getenv("HP_HOME"); h != "" {
		return h
	}
	base, err := os.UserHomeDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, ".hindsight", repoID(repoRoot))
}

// HomeForCwd resolves the cache root without shelling out to git.
//
// Home() needs a repo root, and getting one properly costs two git
// subprocesses. The fastpath memo has to be consulted before we are willing to
// spend that, so this walks up for a .git entry instead — pure filesystem, a
// few microseconds. Being wrong here costs a memo miss, nothing more.
func HomeForCwd(cwd string) string {
	if h := os.Getenv("HP_HOME"); h != "" {
		return h
	}
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return Home(cwd)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return Home(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Home(cwd)
		}
		dir = parent
	}
}

func repoID(repoRoot string) string {
	sum := sha256.Sum256([]byte(repoRoot))
	name := filepath.Base(repoRoot)
	if name == "" || name == string(filepath.Separator) {
		name = "repo"
	}
	return name + "-" + hex.EncodeToString(sum[:])[:12]
}

// Paths resolves cache locations without opening the store. The hook needs
// blob paths on every command and must not pay to replay the whole log.
type Paths struct{ dir string }

func StorePaths(dir string) Paths { return Paths{dir: dir} }

func (p Paths) BlobPath(id string) string {
	h := strings.TrimPrefix(id, "sha256:")
	if len(h) < 2 {
		return filepath.Join(p.dir, "blobs", h)
	}
	return filepath.Join(p.dir, "blobs", h[:2], h)
}

func (p Paths) LogPath() string { return filepath.Join(p.dir, "log.jsonl") }

// Store is an append-only JSONL log plus content-addressed blobs, with an
// in-memory index rebuilt by scanning the log at startup.
//
// Deliberately not SQLite: no cgo, trivially inspectable during a demo, and
// about forty lines instead of two hundred.
type Store struct {
	Paths

	mu       sync.RWMutex
	servable map[string]*Record // key -> best servable record
	all      []*Record
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{Paths: Paths{dir: dir}, servable: map[string]*Record{}}
	if err := s.replay(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) replay() error {
	f, err := os.Open(s.LogPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		if json.Unmarshal([]byte(line), &r) != nil {
			continue // a torn line is a miss, not a crash
		}
		s.ingest(&r)
	}
	return sc.Err()
}

// ingest updates the in-memory index. Caller need not hold the lock during
// replay; Append holds it.
func (s *Store) ingest(r *Record) {
	s.all = append(s.all, r)
	if r.Servable && r.Key != "" {
		if r.Verified != nil && !*r.Verified {
			delete(s.servable, r.Key) // an eviction from shadow verification
			return
		}
		s.servable[r.Key] = r
	}
}

// Append writes one record and updates the index. The daemon is the only
// writer, which is what keeps the log free of interleaved partial lines.
func (s *Store) Append(r *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.LogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	s.ingest(r)
	return nil
}

// Lookup returns a servable record for the key, if one exists.
func (s *Store) Lookup(key string) (*Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.servable[key]
	return r, ok
}

// Evict drops a key from the servable index. Used when shadow verification
// finds a divergence.
func (s *Store) Evict(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.servable, key)
}

// PutBlob content-addresses a byte slice and returns "sha256:<hex>".
func (s *Store) PutBlob(b []byte) (string, error) {
	sum := sha256.Sum256(b)
	id := hex.EncodeToString(sum[:])
	path := s.BlobPath("sha256:" + id)
	if _, err := os.Stat(path); err == nil {
		return "sha256:" + id, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// Write-then-rename so a reader never sees a partial blob.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return "sha256:" + id, nil
}

func (s *Store) GetBlob(id string) ([]byte, error) { return os.ReadFile(s.BlobPath(id)) }

// ServableRecords snapshots what the cache is currently willing to serve.
// Shadow verification works through this list.
func (s *Store) ServableRecords() []*Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Record, 0, len(s.servable))
	for _, r := range s.servable {
		out = append(out, r)
	}
	return out
}

// Records returns a snapshot for stats and verification passes.
func (s *Store) Records() []*Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Record, len(s.all))
	copy(out, s.all)
	return out
}

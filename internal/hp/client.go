package hp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"time"
)

// DefaultDaemon is the shared cache endpoint. Two developers on one machine
// should use different ports.
const DefaultDaemon = "http://127.0.0.1:7777"

func DaemonURL() string {
	if u := os.Getenv("HP_DAEMON"); u != "" {
		return u
	}
	return DefaultDaemon
}

// LookupReq asks whether this exact command at this exact state has already
// been run by somebody.
type LookupReq struct {
	Key     string `json:"key"`
	Agent   string `json:"agent"`
	Cmd     string `json:"cmd"`
	CmdNorm string `json:"cmd_norm"`
	CwdRel  string `json:"cwd_rel"`
	Tree    string `json:"tree"`
	EnvFP   string `json:"env_fp"`
	Policy  string `json:"policy"`
	Reason  string `json:"reason"`
	// Serve is false for the baseline arm: record everything, serve nothing.
	Serve bool `json:"serve"`
	// RepoRoot lets the daemon run git against the caller's worktree for
	// Tier-1 scoping. Worktrees share one object store, so any of them can
	// resolve any recorded tree.
	RepoRoot string `json:"repo_root"`
}

type LookupResp struct {
	Decision    string `json:"decision"`
	ExitCode    int    `json:"exit_code"`
	StdoutBlob  string `json:"stdout_blob"`
	StderrBlob  string `json:"stderr_blob"`
	SourceAgent string `json:"source_agent"`
	DurationMS  int64  `json:"duration_ms"`
	// Tier is 0 for an exact tree match, 1 for a diff-disjoint promotion.
	Tier int `json:"tier"`
	// ScopeReason explains a Tier-1 promotion in human terms.
	ScopeReason string `json:"scope_reason,omitempty"`
	// WaitedMS is time spent blocked on a peer's in-flight execution. Nonzero
	// means single-flight saved a duplicate run.
	WaitedMS int64 `json:"waited_ms"`
}

// Client talks to the daemon. Every method fails soft: an unreachable daemon
// must degrade to normal execution, never to an error the agent sees.
type Client struct {
	base string
	http *http.Client
}

func NewClient() *Client {
	return &Client{
		base: DaemonURL(),
		// Generous, because /lookup blocks server-side while a peer holds a
		// lease on the same key. That blocking is the point: it is what turns
		// five simultaneous installs into one.
		http: &http.Client{Timeout: 10 * time.Minute},
	}
}

func (c *Client) post(path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := c.http.Post(c.base+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Lookup(req LookupReq) (*LookupResp, error) {
	var resp LookupResp
	if err := c.post("/lookup", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Record hands a completed execution to the daemon, which is the only writer
// of the log, and releases any lease held on the key.
func (c *Client) Record(r *Record) error { return c.post("/record", r, nil) }

// Release drops a lease without recording a result, for the case where the
// wrapped command never ran.
func (c *Client) Release(key string) error {
	return c.post("/release", map[string]string{"key": key}, nil)
}

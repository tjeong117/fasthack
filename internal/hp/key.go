package hp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// KeyVersion is baked into every key. Bump it whenever anything that feeds a
// key changes meaning, including the set of paths excluded from the tree hash.
const KeyVersion = "hs-v1"

const gitTimeout = 10 * time.Second

// envAllow is an allowlist, never the whole environment. Hashing the whole
// environment drives cross-agent sharing to zero, because harnesses inject
// per-session variables (session ids, per-agent state directories) that differ
// for every agent by construction while having no effect on command output.
var envAllow = []string{"LANG", "LC_ALL", "TZ", "VIRTUAL_ENV", "CONDA_DEFAULT_ENV", "PYTHONPATH"}

func git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// Workspace resolves the git identity of a directory once, so the per-command
// path does not re-shell for things that never change.
type Workspace struct {
	Root      string // absolute worktree root
	GitDir    string // absolute git dir; for a worktree this is <main>/.git/worktrees/<name>
	IndexPath string // side index, inside GitDir
}

// NewWorkspace resolves the worktree root and its private git dir.
//
// The side index MUST live in the per-worktree git dir. Hardcoding
// ".git/hp-index" is wrong twice over: in a linked worktree .git is a file
// rather than a directory, and if five worktrees somehow shared one index file
// their concurrent "git add -A" calls would corrupt each other and produce
// wrong trees, which means wrong keys, which means wrong answers.
func NewWorkspace(cwd string) (*Workspace, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	env := os.Environ()
	root, err := git(ctx, cwd, env, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	gitDir, err := git(ctx, cwd, env, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	return &Workspace{Root: root, GitDir: gitDir, IndexPath: filepath.Join(gitDir, "hp-index")}, nil
}

// TreeHash is git's own Merkle hash of the live worktree.
//
// Computed against a side index so .git/index is never touched and "git
// status" is unaffected. Warm, this is 20-30 ms; the persistent index is what
// makes that true, and a throwaway mktemp index costs 220 ms to 1.7 s.
//
// Note this deliberately cannot see gitignored files. That hole is exactly
// what EnvFingerprint covers.
func (w *Workspace) TreeHash() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	env := append(os.Environ(), "GIT_INDEX_FILE="+w.IndexPath)

	// Each agent normally has its own worktree and therefore its own git dir
	// and its own side index. But two hooks can still fire concurrently in one
	// worktree, and "git add" takes an exclusive lock on the index. Losing that
	// race would silently downgrade to a passthrough on every concurrent
	// command, which quietly costs most of the cache. Retry briefly instead.
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if _, err = git(ctx, w.Root, env, "add", "-A"); err == nil {
			break
		}
		if ctx.Err() != nil {
			return "", err
		}
		time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
	}
	if err != nil {
		return "", err
	}
	return git(ctx, w.Root, env, "write-tree")
}

// EnvFingerprint covers what the tree hash structurally cannot: everything
// gitignored that still changes what a command prints. In practice that is
// dominated by the virtualenv.
//
// Two worktrees with byte-identical trees and different installed packages
// must produce different keys. That is the one hole that can make Hindsight
// serve a wrong answer, so this is the function to be paranoid about.
//
// Cost is a readdir, about 1 ms. Deliberately not "pip freeze", which would
// cost 300 ms on every single command.
func (w *Workspace) EnvFingerprint() string {
	h := sha256.New()
	h.Write([]byte(KeyVersion + "\x00" + runtime.GOOS + "\x00" + runtime.GOARCH + "\x00"))

	home, _ := os.UserHomeDir()
	for _, k := range envAllow {
		v, ok := os.LookupEnv(k)
		if !ok {
			continue
		}
		// Scrub machine- and worktree-specific prefixes out of the *value*
		// before hashing. VIRTUAL_ENV and PYTHONPATH contain the worktree
		// path, which differs for every agent; hashing them raw would give
		// every agent a distinct key and zero sharing, which is the same
		// failure mode as hashing the whole environment.
		v = strings.ReplaceAll(v, w.Root, "{{ROOT}}")
		if home != "" {
			v = strings.ReplaceAll(v, home, "{{HOME}}")
		}
		h.Write([]byte(k + "=" + v + "\x00"))
	}

	for _, d := range w.sitePackages() {
		h.Write([]byte("pkg\x00" + d + "\x00"))
	}
	if cfg, err := os.ReadFile(filepath.Join(w.venvPath(), "pyvenv.cfg")); err == nil {
		h.Write([]byte("pyvenv\x00"))
		h.Write(cfg)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func (w *Workspace) venvPath() string {
	if v := os.Getenv("VIRTUAL_ENV"); v != "" {
		return v
	}
	return filepath.Join(w.Root, ".venv")
}

// sitePackages returns the sorted set of installed distribution directory
// names. This is the package set, cheaply.
func (w *Workspace) sitePackages() []string {
	var out []string
	libRoot := filepath.Join(w.venvPath(), "lib")
	pythons, err := os.ReadDir(libRoot)
	if err != nil {
		return nil
	}
	for _, p := range pythons {
		if !p.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(libRoot, p.Name(), "site-packages"))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if n := e.Name(); strings.HasSuffix(n, ".dist-info") || strings.HasSuffix(n, ".egg-info") {
				out = append(out, p.Name()+"/"+n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// State is the workspace identity at one instant. The purity gate compares two
// of these, one from before a command and one from after.
type State struct {
	Tree  string `json:"tree"`
	EnvFP string `json:"env_fp"`
}

func (w *Workspace) State() (State, error) {
	tree, err := w.TreeHash()
	if err != nil {
		return State{}, err
	}
	return State{Tree: tree, EnvFP: w.EnvFingerprint()}, nil
}

func (s State) Equal(o State) bool { return s.Tree == o.Tree && s.EnvFP == o.EnvFP }

// CwdRel is the command's working directory relative to the worktree root.
// The same command means different things in different directories, so it is
// part of the key. Relative, because the absolute path differs per worktree
// and must not.
func (w *Workspace) CwdRel(cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "."
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	root := w.Root
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "."
	}
	return rel
}

// NormalizeCommand collapses whitespace so trivially different spellings of
// the same command share a key. It deliberately does not reorder or rewrite
// anything: two commands that normalize identically must be interchangeable.
func NormalizeCommand(cmd string) string {
	return strings.Join(strings.Fields(cmd), " ")
}

// Key binds everything that can change what a command prints.
func Key(st State, cwdRel, cmdNorm string) string {
	h := sha256.New()
	for _, part := range []string{KeyVersion, st.Tree, st.EnvFP, cwdRel, cmdNorm} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return KeyVersion + ":" + hex.EncodeToString(h.Sum(nil))
}

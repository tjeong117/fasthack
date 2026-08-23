package hp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// KeyVersion is baked into every key. Bump it whenever anything that feeds a
// key changes meaning, including the set of paths excluded from the tree hash.
const KeyVersion = "hs-v1"

// gitTimeout bounds an ordinary warm hash. A cold `git add -A` on a very large
// repository legitimately takes far longer — measured at 13.5s on 108k files —
// so the first attempt gets coldGitTimeout instead. Killing git mid-write is
// not a harmless timeout: it leaves a lock behind that nothing else clears.
const gitTimeout = 10 * time.Second
const coldGitTimeout = 120 * time.Second

// staleLockAge is how long an index lock must sit untouched before we treat it
// as debris rather than as a peer mid-write. Comfortably longer than any real
// `git add -A`, including the cold 108k-file case.
const staleLockAge = 5 * time.Minute

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
	env := append(os.Environ(), "GIT_INDEX_FILE="+w.IndexPath)

	// A lock left by a killed git is debris, not contention: no process will
	// ever release it, so every future hash in this worktree fails and the
	// cache is permanently dead here. Clear it before trying.
	w.clearStaleIndexLock()

	// The first hash in a worktree builds the index from nothing and can
	// legitimately take a minute on a large repo. Every later one is warm.
	timeout := gitTimeout
	if _, err := os.Stat(w.IndexPath); err != nil {
		timeout = coldGitTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

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
			// We are about to abandon a git that may be mid-write. Leaving its
			// lock would wedge this worktree for good.
			w.clearStaleIndexLock()
			return "", err
		}
		time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
	}
	if err != nil {
		return "", err
	}
	return git(ctx, w.Root, env, "write-tree")
}

// clearStaleIndexLock removes an index lock old enough that no live git could
// still own it.
//
// Age is the only signal available: the lock carries no pid, and a worktree
// can be shared by several hook processes. Erring long is safe — a lock
// younger than the threshold is left alone and the retry loop handles genuine
// contention.
func (w *Workspace) clearStaleIndexLock() {
	lock := w.IndexPath + ".lock"
	fi, err := os.Stat(lock)
	if err != nil {
		return
	}
	if time.Since(fi.ModTime()) < staleLockAge {
		return
	}
	if os.Remove(lock) == nil {
		Debugf("removed a stale index lock left by a killed git: %s", lock)
	}
}

// EnvFingerprint covers what the tree hash structurally cannot: everything
// gitignored that still changes what a command prints. In practice that means
// installed dependencies, which every ecosystem hides in a gitignored
// directory of its own.
//
// Two worktrees with byte-identical trees and different installed packages
// must produce different keys. That is the one hole that can make Hindsight
// serve a wrong answer, so this is the function to be paranoid about — and it
// has to be paranoid for every language, not just the one we tested first.
//
// complete is false when an ecosystem was detected but could not be read. The
// caller must refuse to serve in that case rather than risk matching a
// workspace whose dependencies we failed to establish.
func (w *Workspace) EnvFingerprint() (fp string, complete bool) {
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

	_, complete = fingerprintEcosystems(w.Root, h)
	return hex.EncodeToString(h.Sum(nil))[:32], complete
}

// Ecosystems lists which dependency ecosystems are in use in this workspace.
func (w *Workspace) Ecosystems() []string {
	detected, _ := fingerprintEcosystems(w.Root, sha256.New())
	return detected
}

// State is the workspace identity at one instant. The purity gate compares two
// of these, one from before a command and one from after.
type State struct {
	Tree  string `json:"tree"`
	EnvFP string `json:"env_fp"`
	// EnvComplete is false when some detected ecosystem could not be read.
	// Such a state may be recorded but must never be served from.
	EnvComplete bool `json:"env_complete"`
}

func (w *Workspace) State() (State, error) {
	tree, err := w.TreeHash()
	if err != nil {
		return State{}, err
	}
	fp, complete := w.EnvFingerprint()
	return State{Tree: tree, EnvFP: fp, EnvComplete: complete}, nil
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
	h.Write([]byte(KeyText(st, cwdRel, cmdNorm)))
	return KeyVersion + ":" + hex.EncodeToString(h.Sum(nil))
}

// KeyText is the canonical pre-image the key hashes.
//
// A hash tells you two keys differ; it cannot tell you why, and "why did this
// miss?" is the question that actually comes up. Keeping the readable
// pre-image alongside the digest makes a miss diffable between two agents.
//
// This is not hypothetical. The environment fingerprint was hashing each
// worktree's own name out of pyvenv.cfg, which drove the hit rate on Python
// fan-outs to zero, looked exactly like agents diverging, and took a
// five-agent run on a real repository to find. Diffing two pre-images would
// have shown it immediately.
//
// The idea is borrowed from Experiential Labs (Apache-2.0), whose
// render_rag_key stores key_text beside key_sha256 for the same reason.
func KeyText(st State, cwdRel, cmdNorm string) string {
	var b strings.Builder
	for _, kv := range [][2]string{
		{"v", KeyVersion},
		{"tree", st.Tree},
		{"env_fp", st.EnvFP},
		{"cwd", cwdRel},
		{"cmd", cmdNorm},
	} {
		b.WriteString(kv[0])
		b.WriteByte('=')
		b.WriteString(kv[1])
		b.WriteByte('\n')
	}
	return b.String()
}

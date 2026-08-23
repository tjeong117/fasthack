package hp

import (
	"errors"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// An Ecosystem contributes installed-dependency state to the environment
// fingerprint.
//
// This exists because git's tree hash cannot see gitignored directories, and
// installed dependencies are almost always gitignored. Two worktrees with
// byte-identical trees and different installed packages must not share a cache
// key. Every language ecosystem needs its own answer to "what is installed
// here", and getting one wrong does not cost a hit — it serves one project's
// output into another.
type Ecosystem interface {
	// Name identifies the ecosystem in fingerprints and diagnostics.
	Name() string

	// Detect reports whether this ecosystem is in use in the workspace,
	// normally by looking for a manifest such as package.json or Cargo.toml.
	// Detection must not depend on dependencies being installed yet.
	Detect(root string) bool

	// Fingerprint writes a stable summary of the installed dependency state
	// into h. It must be cheap: this runs twice per intercepted command, so
	// aim for directory listings and manifest reads rather than invoking the
	// package manager.
	//
	// Returning an error means the state could not be established. That makes
	// the whole workspace unservable rather than silently matching another,
	// because an unknown environment is exactly the case where serving is
	// unsafe.
	Fingerprint(root string, h hash.Hash) error
}

// ErrNotInstalled says the ecosystem is in use but its dependencies are not
// present yet. That is a normal state, not a failure: there is nothing
// installed to disambiguate, so it contributes nothing and does not poison
// the fingerprint.
var ErrNotInstalled = errors.New("dependencies not installed")

// ecosystems is the registry consulted for every fingerprint, in a fixed
// order so the hash is stable.
var ecosystems = []Ecosystem{
	pythonEcosystem{},
}

// RegisterEcosystem adds a detector. Order matters to the hash, so registration
// should happen at init time only.
func RegisterEcosystem(e Ecosystem) { ecosystems = append(ecosystems, e) }

// EcosystemNames lists what is registered, for `hindsight doctor`.
func EcosystemNames() []string {
	out := make([]string, 0, len(ecosystems))
	for _, e := range ecosystems {
		out = append(out, e.Name())
	}
	return out
}

// fingerprintEcosystems folds every detected ecosystem into h. It reports
// which ones were detected, and whether all of them could be established.
//
// A detected-but-unreadable ecosystem sets complete=false, which makes the
// workspace unservable. Abstention over guessing.
func fingerprintEcosystems(root string, h hash.Hash) (detected []string, complete bool) {
	complete = true
	for _, e := range ecosystems {
		if !e.Detect(root) {
			continue
		}
		detected = append(detected, e.Name())
		h.Write([]byte("eco:" + e.Name() + "\x00"))
		switch err := e.Fingerprint(root, h); {
		case err == nil:
		case errors.Is(err, ErrNotInstalled):
			h.Write([]byte("not-installed\x00"))
		default:
			complete = false
			Debugf("ecosystem %s could not be fingerprinted: %v", e.Name(), err)
		}
	}
	return detected, complete
}

// hashSortedDirNames writes the sorted set of entry names under dir that pass
// keep. This is the workhorse for "what is installed here": a readdir costs
// about a millisecond, where asking the package manager costs hundreds.
func hashSortedDirNames(dir string, h hash.Hash, keep func(name string, isDir bool) bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if keep == nil || keep(e.Name(), e.IsDir()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		h.Write([]byte(n))
		h.Write([]byte{0})
	}
	return nil
}

// hashFiles folds the contents of the named files into h, skipping absent
// ones. Use for small pinned manifests such as lockfiles.
func hashFiles(root string, h hash.Hash, names ...string) (found bool) {
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(root, n))
		if err != nil {
			continue
		}
		found = true
		h.Write([]byte("file:" + n + "\x00"))
		h.Write(b)
		h.Write([]byte{0})
	}
	return found
}

func exists(paths ...string) bool {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Python
// ---------------------------------------------------------------------------

type pythonEcosystem struct{}

func (pythonEcosystem) Name() string { return "python" }

func (pythonEcosystem) Detect(root string) bool {
	return exists(
		filepath.Join(root, "pyproject.toml"),
		filepath.Join(root, "requirements.txt"),
		filepath.Join(root, "setup.py"),
		filepath.Join(root, "setup.cfg"),
		filepath.Join(root, "Pipfile"),
		venvPath(root),
	)
}

func (pythonEcosystem) Fingerprint(root string, h hash.Hash) error {
	venv := venvPath(root)
	if !exists(venv) {
		return ErrNotInstalled
	}
	hashPyvenvCfg(venv, h)

	libRoot := filepath.Join(venv, "lib")
	pythons, err := os.ReadDir(libRoot)
	if err != nil {
		return ErrNotInstalled
	}
	var any bool
	for _, p := range pythons {
		if !p.IsDir() {
			continue
		}
		sp := filepath.Join(libRoot, p.Name(), "site-packages")
		err := hashSortedDirNames(sp, h, func(name string, isDir bool) bool {
			return strings.HasSuffix(name, ".dist-info") || strings.HasSuffix(name, ".egg-info")
		})
		if err == nil {
			any = true
		}
	}
	if !any {
		return ErrNotInstalled
	}
	return nil
}

// pyvenvVolatileKeys name their own worktree and nothing about what is
// installed in it.
//
// This is the difference between the cache working and not working on Python.
// `python -m venv` records `command = ... -m venv /abs/path/a1/.venv` and
// `uv venv` records `prompt = a1`, so five worktrees of one project produce
// five fingerprints for byte-identical environments and the hit rate is
// structurally zero. It looks exactly like agents diverging, which is why it
// went unnoticed until a real fan-out on a real repository.
//
// Everything else in the file is kept, because `home`, `version`, `executable`
// and `include-system-site-packages` all genuinely change what a command does.
var pyvenvVolatileKeys = map[string]bool{
	"command": true,
	"prompt":  true,
}

// hashPyvenvCfg folds a virtualenv's configuration into h, minus the parts
// that describe where the worktree happens to live.
func hashPyvenvCfg(venv string, h hash.Hash) {
	b, err := os.ReadFile(filepath.Join(venv, "pyvenv.cfg"))
	if err != nil {
		return
	}
	h.Write([]byte("pyvenv.cfg\x00"))
	lines := strings.Split(string(b), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		key, _, ok := strings.Cut(line, "=")
		if ok && pyvenvVolatileKeys[strings.ToLower(strings.TrimSpace(key))] {
			continue
		}
		if s := strings.TrimSpace(line); s != "" {
			kept = append(kept, s)
		}
	}
	// Sorted, because the writer's field order is not part of the environment.
	sort.Strings(kept)
	for _, line := range kept {
		h.Write([]byte(line))
		h.Write([]byte{0})
	}
}

// venvPath resolves the active virtualenv for a workspace.
func venvPath(root string) string {
	if v := os.Getenv("VIRTUAL_ENV"); v != "" {
		return v
	}
	return filepath.Join(root, ".venv")
}

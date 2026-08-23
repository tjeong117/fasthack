package hp

import (
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func init() { RegisterEcosystem(nodeEcosystem{}) }

// ---------------------------------------------------------------------------
// Shared by the eco_*.go files
// ---------------------------------------------------------------------------

// hashDirIfPresent folds a shallow listing of dir into h under label.
//
// The three outcomes are deliberately distinct. A missing directory is a
// state, not a failure: nothing is installed there and the caller decides what
// that means. An unreadable directory is a failure, because we then cannot say
// what is installed and must not let this workspace match another.
func hashDirIfPresent(label, dir string, h hash.Hash) (present bool, err error) {
	h.Write([]byte(label + ":\x00"))
	switch err := hashSortedDirNames(dir, h, nil); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// ---------------------------------------------------------------------------
// Node — npm, yarn, pnpm, bun
// ---------------------------------------------------------------------------

// nodeEcosystem covers the JS package managers.
//
// node_modules is gitignored in every JS project, so the tree hash is blind to
// the entire dependency set. It is also the worst directory in computing to
// walk: npm hoists transitively, so a hundred thousand files is ordinary. The
// signal here is therefore the lockfile plus a one-level listing, never a
// recursive walk.
type nodeEcosystem struct{}

func (nodeEcosystem) Name() string { return "node" }

// Detect keys on the manifest rather than on node_modules, because "declared
// but not installed" is a real state that must not collide with "installed".
func (nodeEcosystem) Detect(root string) bool {
	return exists(filepath.Join(root, "package.json"))
}

// nodeLockfiles are the four package managers' pinned resolutions. A lockfile
// is hashed alongside the installed set rather than instead of it: it says
// what should be installed, which stops being true the moment an install fails
// halfway or someone edits node_modules by hand.
var nodeLockfiles = []string{
	"package-lock.json",
	"npm-shrinkwrap.json",
	"yarn.lock",
	"pnpm-lock.yaml",
	"bun.lockb",
	"bun.lock",
}

// nodeInstalledManifests are the compact records each package manager writes
// inside node_modules describing what it actually put there, versions and all.
// These are the ideal signal: one small file, exact versions, no walk.
var nodeInstalledManifests = []string{
	".package-lock.json", // npm >= 7
	".modules.yaml",      // pnpm
	".yarn-state.yml",    // yarn berry, node-modules linker
	".yarn-integrity",    // yarn classic
}

func (nodeEcosystem) Fingerprint(root string, h hash.Hash) error {
	pinned := hashFiles(root, h, nodeLockfiles...)

	// Yarn PnP installs no node_modules at all: the resolution table on disk
	// is the install. Hashing it is both the lockfile and the installed set.
	pnp := hashFiles(root, h, ".pnp.cjs", ".pnp.data.json")

	nm := filepath.Join(root, "node_modules")
	entries, err := os.ReadDir(nm)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		if pnp {
			return nil
		}
		return ErrNotInstalled
	default:
		return err
	}
	if len(entries) == 0 && !pnp {
		return ErrNotInstalled
	}

	recorded := hashFiles(nm, h, nodeInstalledManifests...)

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		// Deliberately not filtered on IsDir: pnpm's top level is symlinks,
		// and dropping them would report an installed tree as empty.
		names = append(names, e.Name())
	}
	sort.Strings(names)
	h.Write([]byte("node_modules:\x00"))
	for _, n := range names {
		h.Write([]byte(n))
		h.Write([]byte{0})
	}

	// Scoped packages live one level down, so node_modules/@types is a
	// directory of packages rather than a package. Two installs that differ
	// only inside a scope have identical top-level listings, which is a
	// collision and therefore a wrong answer. This is the only descent;
	// anything deeper is a package's own dependencies and walking it is how a
	// 1 ms readdir becomes a 100k-file traversal.
	for _, n := range names {
		if !strings.HasPrefix(n, "@") {
			continue
		}
		h.Write([]byte("scope:" + n + "\x00"))
		if err := hashSortedDirNames(filepath.Join(nm, n), h, nil); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // raced with a concurrent install
			}
			return err
		}
	}

	// Abstention. A name-only listing cannot tell left-pad@1.3.0 from
	// left-pad@1.3.1, and a package.json range routinely resolves differently
	// on two machines, so names alone would let two worktrees running
	// different code share a key.
	//
	// Every real package manager leaves version-bearing evidence — a lockfile
	// at the root, a PnP resolution table, or its own record inside
	// node_modules — so this fires only on a hand-assembled node_modules.
	// Reading all ~1000 hoisted package.json files would answer the question
	// and would also blow the millisecond budget twice per command, so we
	// refuse to serve instead of guessing. Everything observed above is
	// already in the hash; only the serve decision is withheld.
	if !pinned && !recorded && !pnp {
		return fmt.Errorf("node_modules in %s has no lockfile and no package-manager record, "+
			"so installed versions cannot be established from entry names alone", root)
	}
	return nil
}

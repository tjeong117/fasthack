package hp

import (
	"hash"
	"path/filepath"
)

func init() { RegisterEcosystem(goEcosystem{}) }

// goEcosystem covers Go modules.
//
// Go is the one ecosystem where the pinned file is enough on its own. The
// module cache is global ($GOMODCACHE), so there is no per-workspace install
// directory to list — but every module read out of that cache is verified
// against the hashes in go.sum before it is used, so an identical go.sum is a
// cryptographic statement that the bytes are identical too. Everywhere else a
// lockfile is a wish; here it is a proof.
type goEcosystem struct{}

func (goEcosystem) Name() string { return "go" }

func (goEcosystem) Detect(root string) bool {
	return exists(filepath.Join(root, "go.mod"))
}

func (goEcosystem) Fingerprint(root string, h hash.Hash) error {
	hashFiles(root, h, "go.mod")

	// go.work redirects resolution to a multi-module set, replacing what
	// go.mod would have selected. It is very commonly gitignored, which puts
	// it squarely in the class of state the tree hash cannot see.
	hashFiles(root, h, "go.work", "go.work.sum")

	resolved := hashFiles(root, h, "go.sum")

	// A vendored build ignores the module cache entirely and compiles what is
	// in vendor/. modules.txt is that directory's manifest. vendor/ is usually
	// committed and therefore covered by the tree hash, but it is gitignored
	// often enough that relying on that would be a guess.
	var vendored bool
	if exists(filepath.Join(root, "vendor")) {
		vendored = hashFiles(root, h, "vendor/modules.txt")
	}

	// No go.sum and no vendor/ means nothing has been resolved yet. A module
	// that depends only on the standard library also lands here, which is
	// correct in the sense that matters: there is no external dependency state
	// to disambiguate, and the marker for that is stable.
	if !resolved && !vendored {
		return ErrNotInstalled
	}
	return nil
}

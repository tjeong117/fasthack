package hp

import (
	"hash"
	"path/filepath"
)

func init() { RegisterEcosystem(rustEcosystem{}) }

// rustEcosystem covers Cargo.
//
// The registry cache is global (~/.cargo/registry) and content-addressed, and
// Cargo.lock carries an exact version and a checksum per crate, so the lockfile
// is the honest per-workspace statement of what will be compiled. target/ is
// separate: it is build output, always gitignored, and its presence is what
// makes `cargo test` mutate state that the tree hash cannot see. Listing its
// top level is what lets the purity gate notice.
type rustEcosystem struct{}

func (rustEcosystem) Name() string { return "rust" }

func (rustEcosystem) Detect(root string) bool {
	return exists(filepath.Join(root, "Cargo.toml"))
}

func (rustEcosystem) Fingerprint(root string, h hash.Hash) error {
	pinned := hashFiles(root, h, "Cargo.lock")

	// .cargo/config.toml can repoint the registry, change the default target
	// triple, or inject rustflags, all of which change what a build produces.
	// It is small and frequently gitignored.
	hashFiles(root, h, ".cargo/config.toml", ".cargo/config")

	// Top level only: debug/, release/, the target triples, CACHEDIR.TAG.
	// target/ routinely holds hundreds of thousands of files, so descending
	// even one level further is not affordable on the hot path.
	built, err := hashDirIfPresent("target", filepath.Join(root, "target"), h)
	if err != nil {
		return err
	}

	if !pinned && !built {
		return ErrNotInstalled
	}
	return nil
}

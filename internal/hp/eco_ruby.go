package hp

import (
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
)

func init() { RegisterEcosystem(rubyEcosystem{}) }

// rubyEcosystem covers Bundler.
//
// Gemfile.lock pins an exact version per gem, and a published (name, version)
// on rubygems is immutable, so the lockfile is a strong statement about what
// will be loaded. The cross-check is vendor/bundle, which is where CI and
// containerised setups put gems and which is always gitignored.
type rubyEcosystem struct{}

func (rubyEcosystem) Name() string { return "ruby" }

func (rubyEcosystem) Detect(root string) bool {
	return exists(filepath.Join(root, "Gemfile"))
}

func (rubyEcosystem) Fingerprint(root string, h hash.Hash) error {
	pinned := hashFiles(root, h, "Gemfile.lock")

	// .bundle/config holds BUNDLE_PATH and BUNDLE_WITHOUT: where gems went and
	// which groups were skipped. Both change what `bundle exec` can load.
	hashFiles(root, h, ".bundle/config")

	installed, err := hashBundlePath(filepath.Join(root, "vendor", "bundle"), h)
	if err != nil {
		return err
	}

	if !pinned && !installed {
		return ErrNotInstalled
	}
	return nil
}

// maxBundleFanout bounds the descent below. Bundler puts one or two engines
// (ruby, jruby) under vendor/bundle and a handful of ABI versions under each,
// so anything wider than this is not a bundler layout.
const maxBundleFanout = 32

// hashBundlePath folds a bundler install directory into h.
//
// Bundler's layout is <path>/<engine>/<abi>/gems/<name>-<version>, so the gem
// set sits at a fixed depth of three and the directory names carry the
// versions. Descending exactly that far costs four readdirs; listing only the
// top level would see "ruby" and nothing else, which would let two completely
// different gem sets share a key.
//
// The fan-out cap is an abstention, not a truncation. Silently hashing the
// first N entries of a directory we do not understand would be a guess about
// what is installed, and a wrong guess here serves one project's output into
// another.
func hashBundlePath(dir string, h hash.Hash) (found bool, err error) {
	h.Write([]byte("bundle:\x00"))

	engines, err := readDirBounded(dir, maxBundleFanout)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
	// os.ReadDir sorts by name, so this listing is stable without re-sorting.
	for _, e := range engines {
		h.Write([]byte(e.Name()))
		h.Write([]byte{0})
	}

	for _, engine := range engines {
		if !engine.IsDir() {
			continue
		}
		engineDir := filepath.Join(dir, engine.Name())
		abis, err := readDirBounded(engineDir, maxBundleFanout)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // raced with a concurrent install
			}
			return false, err
		}
		for _, abi := range abis {
			if !abi.IsDir() {
				continue
			}
			label := "gems/" + engine.Name() + "/" + abi.Name()
			gems := filepath.Join(engineDir, abi.Name(), "gems")
			present, err := hashDirIfPresent(label, gems, h)
			if err != nil {
				return false, err
			}
			if present {
				found = true
			}
		}
	}
	return found, nil
}

// readDirBounded is os.ReadDir with a ceiling, so a directory that does not
// have the shape we expect is reported rather than walked.
func readDirBounded(dir string, max int) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	if len(entries) > max {
		return nil, fmt.Errorf("%s has %d entries, more than the %d expected of a bundler layout; "+
			"refusing to guess the installed gem set", dir, len(entries), max)
	}
	return entries, nil
}

package hp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// ecoWrite lays down a workspace from a path->contents map. Parent directories
// are created, so "node_modules/left-pad/package.json" also creates the
// package directory.
func ecoWrite(t *testing.T, root string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// ecoMkdirAll creates empty directories, for the cases where the presence of
// the directory is itself the signal.
func ecoMkdirAll(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// ecoWorkspace builds a fresh temp workspace with the given files.
func ecoWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	return ecoWrite(t, t.TempDir(), files)
}

// ecoHash fingerprints root into a fresh hash and returns the digest alongside
// whatever Fingerprint reported. The digest is returned even on error, because
// everything the ecosystem managed to observe before abstaining is still in
// the hash and still worth distinguishing.
func ecoHash(t *testing.T, e Ecosystem, root string) (string, error) {
	t.Helper()
	h := sha256.New()
	err := e.Fingerprint(root, h)
	return hex.EncodeToString(h.Sum(nil)), err
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// ecoFixture describes one ecosystem well enough to assert the four properties
// that make the environment fingerprint sound.
type ecoFixture struct {
	eco Ecosystem

	// manifest declares the ecosystem with nothing installed. Detect must be
	// true here, because "declared but not installed" is a real state that
	// must not collide with "installed", and Fingerprint must say
	// ErrNotInstalled rather than inventing one.
	manifest map[string]string

	// installA and installB are two workspaces that agree on everything the
	// git tree can see and disagree on what is actually installed. Where the
	// ecosystem has a real per-workspace install directory they also share a
	// lockfile, so the test is measuring what is on disk rather than what was
	// declared.
	installA map[string]string
	installB map[string]string

	// difference names, in one phrase, what A and B disagree about.
	difference string
}

const nodeManifest = `{"name":"app","dependencies":{"left-pad":"^1.3.0"}}`
const nodeLock = `{"name":"app","lockfileVersion":3,"packages":{}}`

func ecoFixtures() map[string]ecoFixture {
	return map[string]ecoFixture{
		"node": {
			eco:        nodeEcosystem{},
			manifest:   map[string]string{"package.json": nodeManifest},
			difference: "node_modules contents, with a byte-identical lockfile",
			installA: map[string]string{
				"package.json":                   nodeManifest,
				"package-lock.json":              nodeLock,
				"node_modules/left-pad/index.js": "x",
			},
			installB: map[string]string{
				"package.json":                   nodeManifest,
				"package-lock.json":              nodeLock,
				"node_modules/left-pad/index.js": "x",
				"node_modules/lodash/index.js":   "y",
			},
		},
		"go": {
			eco:        goEcosystem{},
			manifest:   map[string]string{"go.mod": "module example.com/app\n\ngo 1.24\n"},
			difference: "go.sum, which for Go is the installed set",
			installA: map[string]string{
				"go.mod": "module example.com/app\n\ngo 1.24\n",
				"go.sum": "example.com/a v1.0.0 h1:aaa=\n",
			},
			installB: map[string]string{
				"go.mod": "module example.com/app\n\ngo 1.24\n",
				"go.sum": "example.com/a v1.0.0 h1:aaa=\nexample.com/b v2.0.0 h1:bbb=\n",
			},
		},
		"rust": {
			eco:        rustEcosystem{},
			manifest:   map[string]string{"Cargo.toml": "[package]\nname = \"app\"\n"},
			difference: "Cargo.lock, which pins an exact version and checksum per crate",
			installA: map[string]string{
				"Cargo.toml": "[package]\nname = \"app\"\n",
				"Cargo.lock": "[[package]]\nname = \"serde\"\nversion = \"1.0.100\"\n",
			},
			installB: map[string]string{
				"Cargo.toml": "[package]\nname = \"app\"\n",
				"Cargo.lock": "[[package]]\nname = \"serde\"\nversion = \"1.0.200\"\n",
			},
		},
		"ruby": {
			eco:        rubyEcosystem{},
			manifest:   map[string]string{"Gemfile": "source 'https://rubygems.org'\ngem 'rake'\n"},
			difference: "the gem set under vendor/bundle, with a byte-identical Gemfile.lock",
			installA: map[string]string{
				"Gemfile":      "source 'https://rubygems.org'\ngem 'rake'\n",
				"Gemfile.lock": "GEM\n  specs:\n    rake (13.0.6)\n",
				"vendor/bundle/ruby/3.2.0/gems/rake-13.0.6/rake.gemspec": "x",
			},
			installB: map[string]string{
				"Gemfile":      "source 'https://rubygems.org'\ngem 'rake'\n",
				"Gemfile.lock": "GEM\n  specs:\n    rake (13.0.6)\n",
				"vendor/bundle/ruby/3.2.0/gems/rake-13.0.6/rake.gemspec": "x",
				"vendor/bundle/ruby/3.2.0/gems/rack-3.0.0/rack.gemspec":  "y",
			},
		},
		"jvm": {
			eco:        jvmEcosystem{},
			manifest:   map[string]string{"build.gradle": "plugins { id 'java' }\n"},
			difference: "gradle.lockfile, the only place Gradle pins versions",
			installA: map[string]string{
				"build.gradle":    "plugins { id 'java' }\n",
				"gradle.lockfile": "com.google.guava:guava:32.0.0\n",
			},
			installB: map[string]string{
				"build.gradle":    "plugins { id 'java' }\n",
				"gradle.lockfile": "com.google.guava:guava:33.0.0\n",
			},
		},
	}
}

// ---------------------------------------------------------------------------
// The four properties, for every ecosystem
// ---------------------------------------------------------------------------

// TestEcoDetectsFromManifestAlone: detection must not wait for an install.
// An uninstalled workspace is a distinct state, and an ecosystem that only
// notices itself once dependencies are present cannot contribute the marker
// that keeps the two apart.
func TestEcoDetectsFromManifestAlone(t *testing.T) {
	for name, f := range ecoFixtures() {
		t.Run(name, func(t *testing.T) {
			root := ecoWorkspace(t, f.manifest)
			if !f.eco.Detect(root) {
				t.Fatalf("%s not detected from its manifest alone; an uninstalled workspace "+
					"would then be indistinguishable from one with dependencies", f.eco.Name())
			}
		})
	}
}

func TestEcoDetectIsFalseOnEmptyWorkspace(t *testing.T) {
	for name, f := range ecoFixtures() {
		t.Run(name, func(t *testing.T) {
			if f.eco.Detect(t.TempDir()) {
				t.Fatalf("%s detected in an empty directory; every unrelated workspace would "+
					"pay for a fingerprint it has no use for", f.eco.Name())
			}
		})
	}
}

// TestEcoReportsNotInstalledBeforeInstall: a declared-but-empty workspace is
// normal, not a failure. It contributes a marker and stays servable.
func TestEcoReportsNotInstalledBeforeInstall(t *testing.T) {
	for name, f := range ecoFixtures() {
		t.Run(name, func(t *testing.T) {
			root := ecoWorkspace(t, f.manifest)
			_, err := ecoHash(t, f.eco, root)
			if !errors.Is(err, ErrNotInstalled) {
				t.Fatalf("%s with a manifest and nothing installed returned %v, want ErrNotInstalled",
					f.eco.Name(), err)
			}
		})
	}
}

// TestEcoDistinguishesInstalledSets is the property the whole file exists for.
//
// Installed dependencies are gitignored in every ecosystem, so git's tree hash
// structurally cannot see them. If the fingerprint cannot see them either,
// then two worktrees with identical trees and different dependencies produce
// the same cache key, and Hindsight hands one project's recorded output to the
// other. That is a wrong answer, not a missed hit, and it is the one failure
// the design exists to make impossible.
func TestEcoDistinguishesInstalledSets(t *testing.T) {
	for name, f := range ecoFixtures() {
		t.Run(name, func(t *testing.T) {
			hashA, errA := ecoHash(t, f.eco, ecoWorkspace(t, f.installA))
			hashB, errB := ecoHash(t, f.eco, ecoWorkspace(t, f.installB))
			if errA != nil || errB != nil {
				t.Fatalf("both fixtures should be established states: A=%v B=%v", errA, errB)
			}
			if hashA == hashB {
				t.Fatalf("%s: two workspaces differing in %s produced the same fingerprint (%s). "+
					"With identical trees these share a cache key, so the cache would serve one "+
					"workspace's recorded output as the other's answer.",
					f.eco.Name(), f.difference, hashA)
			}
		})
	}
}

// TestEcoFingerprintIsStable guards the opposite failure. A fingerprint that
// moves when nothing moved is not unsafe, but it drives cross-agent sharing to
// zero, which is the same as having no cache.
func TestEcoFingerprintIsStable(t *testing.T) {
	for name, f := range ecoFixtures() {
		t.Run(name, func(t *testing.T) {
			root := ecoWorkspace(t, f.installA)
			first, err := ecoHash(t, f.eco, root)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 5; i++ {
				got, err := ecoHash(t, f.eco, root)
				if err != nil {
					t.Fatal(err)
				}
				if got != first {
					t.Fatalf("%s fingerprint unstable on an unchanged workspace: %s != %s",
						f.eco.Name(), got, first)
				}
			}
		})
	}
}

func TestEcoRegistered(t *testing.T) {
	have := map[string]bool{}
	for _, n := range EcosystemNames() {
		have[n] = true
	}
	for _, want := range []string{"python", "node", "go", "rust", "ruby", "jvm"} {
		if !have[want] {
			t.Fatalf("%s is not registered; its init() did not run and the whole ecosystem is "+
				"invisible to the fingerprint. Registered: %v", want, EcosystemNames())
		}
	}
}

// ---------------------------------------------------------------------------
// Node: cost, and the shape of the listing
// ---------------------------------------------------------------------------

// TestEcoNodeFingerprintIsCheapOnLargeNodeModules puts a ceiling on the hot
// path. Fingerprint runs twice per intercepted command, so anything expensive
// here is paid on every shell call the agent makes.
//
// The timer catches gross cost. The structural guard against someone making
// this recursive is TestEcoNodeDoesNotDescendIntoNestedDependencies, because
// 200 shallow packages are cheap to walk either way and a threshold alone
// would not notice.
func TestEcoNodeFingerprintIsCheapOnLargeNodeModules(t *testing.T) {
	root := ecoWorkspace(t, map[string]string{
		"package.json":      nodeManifest,
		"package-lock.json": nodeLock,
	})
	pkgs := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		pkgs = append(pkgs, fmt.Sprintf("pkg-%03d", i))
	}
	writeNodeModules(t, root, pkgs...)

	e := nodeEcosystem{}
	if _, err := ecoHash(t, e, root); err != nil { // warm the directory cache
		t.Fatal(err)
	}

	const runs = 20
	start := time.Now()
	for i := 0; i < runs; i++ {
		if err := e.Fingerprint(root, sha256.New()); err != nil {
			t.Fatal(err)
		}
	}
	per := time.Since(start) / runs
	t.Logf("node fingerprint over %d top-level packages: %v per call", len(pkgs), per)
	if per > 50*time.Millisecond {
		t.Fatalf("node fingerprint took %v per call over %d packages; the budget is a readdir, "+
			"and anything this slow means node_modules is being walked rather than listed",
			per, len(pkgs))
	}
}

// TestEcoNodeDoesNotDescendIntoNestedDependencies pins the decision not to
// walk. node_modules/<pkg>/node_modules is a package's own private
// dependencies and there can be a hundred thousand files below it.
//
// The trade is deliberate and it is not free: two installs differing only in a
// nested transitive dependency are invisible here. What covers that is the
// package manager's own record — .package-lock.json and friends list the whole
// resolved tree with versions, and that file is hashed. If this test ever has
// to change, the reason must be a correctness argument, not a convenience one.
func TestEcoNodeDoesNotDescendIntoNestedDependencies(t *testing.T) {
	root := ecoWorkspace(t, map[string]string{
		"package.json":                   nodeManifest,
		"package-lock.json":              nodeLock,
		"node_modules/left-pad/index.js": "x",
	})
	before, err := ecoHash(t, nodeEcosystem{}, root)
	if err != nil {
		t.Fatal(err)
	}

	ecoWrite(t, root, map[string]string{
		"node_modules/left-pad/node_modules/nested-dep/index.js": "deep",
	})
	after, err := ecoHash(t, nodeEcosystem{}, root)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("nested node_modules changed the fingerprint, which means the listing recursed; " +
			"that turns a 1 ms readdir into a traversal of every file in the dependency tree")
	}
}

// TestEcoNodeSeesScopedPackages: node_modules/@types is a directory of
// packages, not a package. Listing only the top level sees "@types" for both
// of these and calls them identical, which is a collision between two
// genuinely different installs.
func TestEcoNodeSeesScopedPackages(t *testing.T) {
	base := map[string]string{"package.json": nodeManifest, "package-lock.json": nodeLock}
	a := ecoWorkspace(t, base)
	b := ecoWorkspace(t, base)
	writeNodeModules(t, a, "@types/node")
	writeNodeModules(t, b, "@types/react")

	hashA, err := ecoHash(t, nodeEcosystem{}, a)
	if err != nil {
		t.Fatal(err)
	}
	hashB, err := ecoHash(t, nodeEcosystem{}, b)
	if err != nil {
		t.Fatal(err)
	}
	if hashA == hashB {
		t.Fatal("@types/node and @types/react produced the same fingerprint; both workspaces " +
			"have a single top-level entry named @types, so a scope must be listed one level in " +
			"or two different installs share a cache key")
	}
}

// TestEcoNodeAbstainsWhenVersionsCannotBeEstablished documents the one place
// node chooses abstention over a guess.
//
// A listing of node_modules gives names, not versions, and "left-pad": "^1.3.0"
// resolves to different versions on different machines. With no lockfile and
// no package-manager record inside node_modules there is nothing that pins a
// version, so two worktrees running different code would look identical. The
// honest answer is to refuse to serve. Reading every hoisted package.json
// would answer the question and would also cost tens of milliseconds twice per
// command, which is the budget for the entire hook.
func TestEcoNodeAbstainsWhenVersionsCannotBeEstablished(t *testing.T) {
	root := ecoWorkspace(t, map[string]string{"package.json": nodeManifest})
	writeNodeModules(t, root, "left-pad")

	_, err := ecoHash(t, nodeEcosystem{}, root)
	if err == nil {
		t.Fatal("a hand-assembled node_modules with no lockfile and no package-manager record " +
			"was reported as an established state; entry names cannot distinguish left-pad@1.3.0 " +
			"from left-pad@1.3.1, so this workspace must not be servable")
	}
	if errors.Is(err, ErrNotInstalled) {
		t.Fatal("packages are installed, so this is not ErrNotInstalled; it is an environment " +
			"we cannot establish, which must make the workspace unservable rather than " +
			"contributing a shared not-installed marker")
	}
}

// TestEcoNodeStaysServableWithALockfile is the other half of the test above:
// the abstention must fire only on the pathological case. Every real install
// leaves version-bearing evidence, and those workspaces have to stay servable
// or the node ecosystem is a cache with no hits.
func TestEcoNodeStaysServableWithALockfile(t *testing.T) {
	root := newRepo(t)
	ecoWrite(t, root, map[string]string{
		"package.json":      nodeManifest,
		"package-lock.json": nodeLock,
	})
	writeNodeModules(t, root, "left-pad")

	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	fp, complete := ws.EnvFingerprint()
	if !complete {
		t.Fatal("an ordinary npm workspace was reported as unestablishable; nothing would ever " +
			"be servable in a JS repo")
	}

	writeNodeModules(t, root, "lodash")
	after, complete := ws.EnvFingerprint()
	if !complete {
		t.Fatal("still an ordinary npm workspace")
	}
	if fp == after {
		t.Fatal("installing a package did not move the fingerprint, so the install would be " +
			"recorded as a no-op and its cache entries would outlive the state that produced them")
	}
}

// TestEcoNodeHandlesYarnPnP: under Plug'n'Play there is no node_modules at
// all, and the resolution table on disk is the install. Treating a missing
// node_modules as "nothing installed" would collapse every PnP workspace onto
// one marker regardless of what it actually resolves.
func TestEcoNodeHandlesYarnPnP(t *testing.T) {
	a := ecoWorkspace(t, map[string]string{
		"package.json": nodeManifest,
		".pnp.cjs":     `resolution: {"left-pad": "1.3.0"}`,
	})
	b := ecoWorkspace(t, map[string]string{
		"package.json": nodeManifest,
		".pnp.cjs":     `resolution: {"left-pad": "1.3.1"}`,
	})

	hashA, errA := ecoHash(t, nodeEcosystem{}, a)
	hashB, errB := ecoHash(t, nodeEcosystem{}, b)
	if errA != nil || errB != nil {
		t.Fatalf("a PnP workspace is installed, not empty: A=%v B=%v", errA, errB)
	}
	if hashA == hashB {
		t.Fatal("two PnP workspaces resolving left-pad to different versions produced the same " +
			"fingerprint, so the cache would serve one's output as the other's")
	}
}

// ---------------------------------------------------------------------------
// Per-ecosystem detail the shared table cannot express
// ---------------------------------------------------------------------------

// TestEcoRustSeesTargetDirectory: target/ is gitignored build output, so the
// tree hash cannot see it. Its top level is what lets the purity gate notice
// that `cargo build` changed the workspace.
func TestEcoRustSeesTargetDirectory(t *testing.T) {
	base := map[string]string{
		"Cargo.toml": "[package]\nname = \"app\"\n",
		"Cargo.lock": "[[package]]\nname = \"serde\"\nversion = \"1.0.100\"\n",
	}
	a := ecoWorkspace(t, base)
	b := ecoWorkspace(t, base)
	ecoMkdirAll(t, a, "target/debug")
	ecoMkdirAll(t, b, "target/debug", "target/release")

	hashA, errA := ecoHash(t, rustEcosystem{}, a)
	hashB, errB := ecoHash(t, rustEcosystem{}, b)
	if errA != nil || errB != nil {
		t.Fatalf("A=%v B=%v", errA, errB)
	}
	if hashA == hashB {
		t.Fatal("a debug-only target/ and a debug+release target/ produced the same fingerprint; " +
			"a build that only produced one of them would be recorded as leaving the workspace " +
			"unchanged")
	}
}

// TestEcoGoSeesVendorDirectory: a vendored build compiles vendor/ and ignores
// the module cache entirely, so modules.txt is what determines the result.
func TestEcoGoSeesVendorDirectory(t *testing.T) {
	base := map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.24\n",
		"go.sum": "example.com/a v1.0.0 h1:aaa=\n",
	}
	a := ecoWorkspace(t, base)
	b := ecoWorkspace(t, base)
	ecoWrite(t, a, map[string]string{"vendor/modules.txt": "# example.com/a v1.0.0\n"})
	ecoWrite(t, b, map[string]string{"vendor/modules.txt": "# example.com/a v1.1.0\n"})

	hashA, errA := ecoHash(t, goEcosystem{}, a)
	hashB, errB := ecoHash(t, goEcosystem{}, b)
	if errA != nil || errB != nil {
		t.Fatalf("A=%v B=%v", errA, errB)
	}
	if hashA == hashB {
		t.Fatal("two different vendored module sets produced the same fingerprint")
	}
}

// TestEcoRubyAbstainsOnUnrecognisedBundlePath: the gem set lives three levels
// down in a bundler layout, so the descent is bounded by knowing that shape. A
// vendor/bundle that does not have it is a directory we do not understand, and
// hashing the first few entries of it would be a guess about what is installed.
func TestEcoRubyAbstainsOnUnrecognisedBundlePath(t *testing.T) {
	root := ecoWorkspace(t, map[string]string{
		"Gemfile":      "source 'https://rubygems.org'\n",
		"Gemfile.lock": "GEM\n  specs:\n",
	})
	dirs := make([]string, 0, maxBundleFanout+1)
	for i := 0; i <= maxBundleFanout; i++ {
		dirs = append(dirs, fmt.Sprintf("vendor/bundle/entry-%03d", i))
	}
	ecoMkdirAll(t, root, dirs...)

	_, err := ecoHash(t, rubyEcosystem{}, root)
	if err == nil || errors.Is(err, ErrNotInstalled) {
		t.Fatalf("a vendor/bundle with %d top-level entries is not a bundler layout and its "+
			"installed gem set cannot be established; got err=%v, want an abstention",
			len(dirs), err)
	}
}

// TestEcoJVMSeesLocalBuildState: Maven and Gradle resolve into caches global to
// the machine, so target/, build/ and .gradle/ are the only per-workspace trace
// that anything was resolved or built here.
func TestEcoJVMSeesLocalBuildState(t *testing.T) {
	base := map[string]string{"pom.xml": "<project><artifactId>app</artifactId></project>"}
	a := ecoWorkspace(t, base)
	b := ecoWorkspace(t, base)
	ecoMkdirAll(t, a, "target/classes")
	ecoMkdirAll(t, b, "target/classes", "target/test-classes")

	hashA, errA := ecoHash(t, jvmEcosystem{}, a)
	hashB, errB := ecoHash(t, jvmEcosystem{}, b)
	if errA != nil || errB != nil {
		t.Fatalf("A=%v B=%v", errA, errB)
	}
	if hashA == hashB {
		t.Fatal("a compiled-only target/ and a compiled-and-tested target/ produced the same " +
			"fingerprint, so a build would look like it left the workspace untouched")
	}
}

// TestEcoDoesNotWriteToTheWorkspace: fingerprinting is on the hot path of every
// command and must be a pure read. A cache that writes into the tree it hashes
// changes the key it is about to compute.
func TestEcoDoesNotWriteToTheWorkspace(t *testing.T) {
	for name, f := range ecoFixtures() {
		t.Run(name, func(t *testing.T) {
			root := ecoWorkspace(t, f.installA)
			before := ecoListTree(t, root)
			if _, err := ecoHash(t, f.eco, root); err != nil {
				t.Fatal(err)
			}
			if after := ecoListTree(t, root); after != before {
				t.Fatalf("%s fingerprinting changed the workspace:\nbefore: %s\nafter:  %s",
					f.eco.Name(), before, after)
			}
		})
	}
}

func ecoListTree(t *testing.T, root string) string {
	t.Helper()
	var out string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out += rel + "\n"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

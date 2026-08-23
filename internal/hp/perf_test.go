package hp

// What the hook costs.
//
// A PreToolUse hook fires before every shell command an agent runs, and the
// key path is not free: resolving the worktree shells out to git twice, the
// tree hash shells out twice more, the environment fingerprint walks the
// dependency directories, and then there is a daemon round trip. On a miss the
// whole state computation happens a second time inside `hindsight record`, to
// close the purity gate.
//
// Nobody had measured that. If the per-command cost is comparable to the
// commands being cached, the cache is a tax on everything it fails to serve,
// and the break-even threshold below is the number that decides whether the
// tool is worth running at all.
//
// The authoritative run pins the iteration count, because the large fixtures
// take longer than the default benchtime and would otherwise be sampled once.
// BENCHMARKS.md reports the median of it:
//
//	go test ./internal/hp/ -run XXX -bench . -benchtime 10x -count 5
//
// The microbenchmarks below — the classifier, the key, the daemon, the memo
// lookup — need the opposite treatment. Ten iterations of a one-microsecond
// function measures the timer, and BenchmarkClassifyMix only reaches the whole
// command mix after thirty-five of them, so run those at a real benchtime:
//
//	go test ./internal/hp/ -run XXX -count 5 \
//	  -bench 'Classify|KeyOnly|Normalize|HookEnvelope|Fastpath|Daemon|Store'

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// perfFixtureVersion is baked into the fixture directory name so a layout
// change cannot be silently measured against a stale cache from an older run.
const perfFixtureVersion = "v1"

// perfFixtureRoot is where generated repositories live.
//
// Generating them costs more than every measurement in this file put together,
// so they are cached between runs rather than rebuilt per benchmark. That also
// keeps the page cache warm, which is the realistic condition: an agent's
// worktree is not cold.
//
// The consequence is that a plain `go test -bench .` leaves about a hundred
// thousand files behind on purpose. `scripts/bench.sh --with-go` points
// HP_PERF_FIXTURES at its own temp directory and removes them; otherwise
// delete $TMPDIR/hindsight-perf-v1 by hand when you are done.
func perfFixtureRoot(tb testing.TB) string {
	dir := os.Getenv("HP_PERF_FIXTURES")
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "hindsight-perf-"+perfFixtureVersion)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		tb.Fatalf("fixture root %s: %v", dir, err)
	}
	return dir
}

// perfRepoSpec describes a synthetic workspace.
type perfRepoSpec struct {
	name   string
	files  int
	python bool // gitignored .venv with installed distributions
	node   bool // gitignored node_modules with installed packages

	// nodeEntries and nodeLockBytes size the node fixture; zero means the
	// perfNode* defaults.
	//
	// They exist because the default fixture understates the node path. A
	// real package-lock.json is megabytes and the fingerprint hashes it
	// whole, twice per intercepted command, so a fixture with a 50-byte
	// lockfile measures the readdir and none of the read.
	nodeEntries   int
	nodeLockBytes int
}

func (s perfRepoSpec) nodeEntryCount() int {
	if s.nodeEntries > 0 {
		return s.nodeEntries
	}
	return perfNodeEntries
}

type perfRepo struct {
	root  string
	ws    *Workspace
	files []string // repo-relative paths, in generation order
}

var (
	perfRepoMu sync.Mutex
	perfRepos  = map[string]*perfRepo{}
)

// Fixture dependency counts, chosen to match a mid-size real project.
const (
	perfDistInfos   = 300 // installed Python distributions
	perfNodeEntries = 500 // top-level node_modules entries
	perfNodeScopes  = 25  // of which are scopes, each holding perfNodeInScope
	perfNodeInScope = 8
)

// perfRemoveAll deletes a fixture tree, retrying briefly.
//
// RemoveAll over twenty thousand files intermittently returns ENOTEMPTY on
// macOS, because something outside the test — Spotlight, a virus scanner, a
// peer benchmark run — created an entry in a directory the walk had already
// passed. Observed once in this suite. Retrying is the whole fix.
func perfRemoveAll(path string) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = os.RemoveAll(path); err == nil {
			return nil
		}
		time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
	}
	return err
}

// perfFreshRepo discards any cached copy of a fixture so the next caller
// rebuilds it from nothing.
//
// Needed by the dirty-tree benchmark, which is self-contaminating: every
// iteration writes one loose object per changed file, so a fixture that has
// already been measured carries the debris of every previous run.
//
// Best effort by design. Removing the stamp is enough to force a rebuild, and
// perfGetRepo clears the directory itself; if either fails we measure the
// cached fixture and say so rather than failing the suite, because fixture
// hygiene is not worth a red build.
func perfFreshRepo(tb testing.TB, spec perfRepoSpec) *perfRepo {
	perfRepoMu.Lock()
	delete(perfRepos, spec.name)
	perfRepoMu.Unlock()
	if err := os.Remove(perfStampPath(filepath.Join(perfFixtureRoot(tb), spec.name))); err != nil &&
		!os.IsNotExist(err) {
		tb.Logf("could not invalidate fixture %s, measuring the cached one: %v", spec.name, err)
	}
	return perfGetRepo(tb, spec)
}

func perfGetRepo(tb testing.TB, spec perfRepoSpec) *perfRepo {
	perfRepoMu.Lock()
	defer perfRepoMu.Unlock()
	if r, ok := perfRepos[spec.name]; ok {
		return r
	}
	root := filepath.Join(perfFixtureRoot(tb), spec.name)
	files := perfLayout(spec.files)
	if !perfStampMatches(root, spec) {
		if err := perfRemoveAll(root); err != nil {
			tb.Fatalf("clear stale fixture %s: %v", root, err)
		}
		perfBuildRepo(tb, root, spec, files)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		tb.Fatalf("resolve fixture workspace %s: %v", root, err)
	}
	r := &perfRepo{root: root, ws: ws, files: files}
	perfRepos[spec.name] = r
	return r
}

// The stamp lives beside the repository rather than inside it, because a file
// inside would be tracked and would change the very tree hash being measured.
func perfStampPath(root string) string { return root + ".stamp" }

func perfStampBody(spec perfRepoSpec) string {
	return fmt.Sprintf("%s files=%d python=%v node=%v dists=%d node_entries=%d lock_bytes=%d",
		perfFixtureVersion, spec.files, spec.python, spec.node, perfDistInfos,
		spec.nodeEntryCount(), spec.nodeLockBytes)
}

func perfStampMatches(root string, spec perfRepoSpec) bool {
	b, err := os.ReadFile(perfStampPath(root))
	return err == nil && string(b) == perfStampBody(spec)
}

// perfLayout produces repo-relative paths with realistic nesting: about twenty
// files per leaf directory inside a two-level package tree, which is roughly
// the shape of a large Python or Go repository. Nesting matters because git
// hashes one tree object per directory.
func perfLayout(n int) []string {
	const perDir, dirsPerTop = 20, 25
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		leaf := i / perDir
		out = append(out, fmt.Sprintf("src/pkg%03d/mod%02d/file%05d.go",
			leaf/dirsPerTop, leaf%dirsPerTop, i))
	}
	return out
}

// perfFileBody is a plausible small source file, about 700 bytes. Size matters
// to the cold path, where git reads and hashes every byte.
func perfFileBody(rel string, gen int) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "// %s\n// fixture generation %d\n\npackage fixture\n\n", rel, gen)
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&b, "func Fixture%02dGen%d() int { return %d }\n", i, gen, i*7+gen)
	}
	return []byte(b.String())
}

func perfGit(tb testing.TB, dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		tb.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func perfWrite(tb testing.TB, path string, body []byte) {
	if err := os.WriteFile(path, body, 0o644); err != nil {
		tb.Fatalf("write %s: %v", path, err)
	}
}

func perfMkdir(tb testing.TB, path string) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		tb.Fatalf("mkdir %s: %v", path, err)
	}
}

func perfBuildRepo(tb testing.TB, root string, spec perfRepoSpec, files []string) {
	start := time.Now()
	perfMkdir(tb, root)
	perfGit(tb, root, "init", "-q")
	perfGit(tb, root, "config", "user.email", "perf@hindsight.invalid")
	perfGit(tb, root, "config", "user.name", "hindsight perf")
	perfGit(tb, root, "config", "commit.gpgsign", "false")
	perfGit(tb, root, "config", "gc.auto", "0")

	// Every real project gitignores its installed dependencies. Leaving them
	// tracked would push a hundred thousand dependency files through the tree
	// hash, which measures something nobody actually pays.
	perfWrite(tb, filepath.Join(root, ".gitignore"), []byte(".venv/\nnode_modules/\n"))

	made := map[string]bool{}
	for _, rel := range files {
		dir := filepath.Dir(rel)
		if !made[dir] {
			perfMkdir(tb, filepath.Join(root, dir))
			made[dir] = true
		}
		perfWrite(tb, filepath.Join(root, rel), perfFileBody(rel, 0))
	}
	if spec.python {
		perfBuildVenv(tb, root)
	}
	if spec.node {
		perfBuildNodeModules(tb, root, spec)
	}

	perfGit(tb, root, "add", "-A")
	perfGit(tb, root, "commit", "-q", "-m", "fixture")
	perfWrite(tb, perfStampPath(root), []byte(perfStampBody(spec)))
	tb.Logf("built fixture %s (%d files, python=%v node=%v) in %s",
		filepath.Base(root), spec.files, spec.python, spec.node, time.Since(start).Round(time.Millisecond))
}

// perfBuildVenv writes a virtualenv holding perfDistInfos installed
// distributions. Each distribution contributes both a .dist-info directory and
// the importable package beside it, so the fingerprint's readdir has to filter
// roughly twice as many entries as it keeps — which is what a real
// site-packages looks like.
func perfBuildVenv(tb testing.TB, root string) {
	venv := filepath.Join(root, ".venv")
	sitePackages := filepath.Join(venv, "lib", "python3.12", "site-packages")
	perfMkdir(tb, sitePackages)
	perfWrite(tb, filepath.Join(venv, "pyvenv.cfg"),
		[]byte("home = /usr/bin\nversion = 3.12.4\ninclude-system-site-packages = false\n"))
	for i := 0; i < perfDistInfos; i++ {
		dist := filepath.Join(sitePackages, fmt.Sprintf("pkg%03d-1.%d.%d.dist-info", i, i%20, i%7))
		perfMkdir(tb, dist)
		perfWrite(tb, filepath.Join(dist, "METADATA"),
			[]byte(fmt.Sprintf("Metadata-Version: 2.1\nName: pkg%03d\nVersion: 1.%d.%d\n", i, i%20, i%7)))
		perfMkdir(tb, filepath.Join(sitePackages, fmt.Sprintf("pkg%03d", i)))
	}
}

// perfLockfileBody produces a package-lock.json of about n bytes.
//
// The content is never parsed — hashFiles folds the raw bytes into the
// fingerprint — but it is shaped like a real lockfile so that the entry sizes,
// and therefore the read, are representative. n <= 0 gives the stub lockfile,
// which is enough to stop the node ecosystem abstaining but measures no read.
func perfLockfileBody(n int) []byte {
	if n <= 0 {
		return []byte(`{"name":"fixture","lockfileVersion":3,"packages":{}}`)
	}
	var b strings.Builder
	b.Grow(n + 256)
	b.WriteString(`{"name":"fixture","lockfileVersion":3,"packages":{`)
	for i := 0; b.Len() < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"node_modules/pkg-%05d":{"version":"1.%d.%d",`+
			`"resolved":"https://registry.npmjs.org/pkg-%05d/-/pkg-%05d-1.%d.%d.tgz",`+
			`"integrity":"sha512-%s","license":"MIT"}`,
			i, i%20, i%7, i, i, i%20, i%7, strings.Repeat("a", 64))
	}
	b.WriteString("}}")
	return []byte(b.String())
}

// perfBuildNodeModules writes a node_modules with spec.nodeEntryCount()
// top-level entries, perfNodeScopes of which are scopes the fingerprint has to
// descend into. The lockfile and the package manager's own record are both
// present, because without them the node ecosystem correctly abstains and the
// measurement would be of the abstention path rather than the real one.
func perfBuildNodeModules(tb testing.TB, root string, spec perfRepoSpec) {
	entries := spec.nodeEntryCount()
	lock := perfLockfileBody(spec.nodeLockBytes)

	perfWrite(tb, filepath.Join(root, "package.json"),
		[]byte(`{"name":"fixture","version":"1.0.0","dependencies":{}}`))
	perfWrite(tb, filepath.Join(root, "package-lock.json"), lock)
	nm := filepath.Join(root, "node_modules")
	perfMkdir(tb, nm)
	// npm writes its own copy inside node_modules, so a realistic install has
	// two megabyte-scale files in the fingerprint, not one.
	perfWrite(tb, filepath.Join(nm, ".package-lock.json"), lock)

	for i := 0; i < entries-perfNodeScopes; i++ {
		pkg := filepath.Join(nm, fmt.Sprintf("pkg-%04d", i))
		perfMkdir(tb, pkg)
		perfWrite(tb, filepath.Join(pkg, "package.json"),
			[]byte(fmt.Sprintf(`{"name":"pkg-%04d","version":"1.0.%d"}`, i, i%9)))
	}
	for s := 0; s < perfNodeScopes; s++ {
		for j := 0; j < perfNodeInScope; j++ {
			perfMkdir(tb, filepath.Join(nm, fmt.Sprintf("@scope%02d", s), fmt.Sprintf("pkg-%02d", j)))
		}
	}
}

// perfDirtyGen makes every rewrite produce content git has never seen, and is
// seeded from the clock so that a second run against the cached fixture does
// not replay the first run's content.
//
// This matters more than it looks. `git add -A` writes a new blob object for
// content it does not already have, and skips the write for content it does.
// Repeating the same few generations measured five times faster than writing
// fresh content, which is not what an agent editing files produces.
var perfDirtyGen = int(time.Now().UnixNano() % 1e9)

// perfDirty rewrites the first count files with content nothing has seen, so
// the next tree hash has exactly that many blobs to hash and store.
func perfDirty(tb testing.TB, r *perfRepo, count int) {
	perfDirtyGen++
	for i := 0; i < count && i < len(r.files); i++ {
		perfWrite(tb, filepath.Join(r.root, r.files[i]), perfFileBody(r.files[i], perfDirtyGen))
	}
}

// perfPrime populates the side index, which is the difference between the warm
// path the hook normally takes and the cold path it takes exactly once.
func perfPrime(tb testing.TB, r *perfRepo) {
	for i := 0; i < 2; i++ {
		if _, err := r.ws.TreeHash(); err != nil {
			tb.Fatalf("prime side index: %v", err)
		}
	}
}

func perfMillis(b *testing.B) {
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e6, "ms/op")
}

func perfEcosystemRegistered(name string) bool {
	for _, n := range EcosystemNames() {
		if n == name {
			return true
		}
	}
	return false
}

// perfSizes is the repository-size curve. 50k files is a large monorepo; 100
// is a toy. The interesting question is where between them the cost stops
// being affordable.
var perfSizes = []int{100, 1_000, 5_000, 20_000, 50_000}

func perfSizeName(n int) string {
	if n >= 1_000 {
		return fmt.Sprintf("%dk_files", n/1_000)
	}
	return fmt.Sprintf("%d_files", n)
}

func perfEachSize(b *testing.B, fn func(b *testing.B, r *perfRepo)) {
	for _, n := range perfSizes {
		b.Run(perfSizeName(n), func(b *testing.B) {
			if testing.Short() && n > 5_000 {
				b.Skip("large fixture skipped under -short")
			}
			fn(b, perfGetRepo(b, perfRepoSpec{name: fmt.Sprintf("repo-%d", n), files: n}))
		})
	}
}

// ---------------------------------------------------------------------------
// 1. Tree hash versus repository size
// ---------------------------------------------------------------------------

// BenchmarkTreeHashWarm is the cost the hook actually pays, twice per
// intercepted command, with the persistent side index already populated.
func BenchmarkTreeHashWarm(b *testing.B) {
	perfEachSize(b, func(b *testing.B, r *perfRepo) {
		perfPrime(b, r)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := r.ws.TreeHash(); err != nil {
				b.Fatal(err)
			}
		}
		perfMillis(b)
	})
}

// BenchmarkTreeHashCold deletes the side index before each iteration, which
// forces git to read and hash every tracked file. This is what a throwaway
// mktemp index would cost on every single command, and it is the number that
// justifies the persistent index existing at all.
func BenchmarkTreeHashCold(b *testing.B) {
	perfEachSize(b, func(b *testing.B, r *perfRepo) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			if err := os.Remove(r.ws.IndexPath); err != nil && !os.IsNotExist(err) {
				b.Fatal(err)
			}
			b.StartTimer()
			if _, err := r.ws.TreeHash(); err != nil {
				b.Fatal(err)
			}
		}
		perfMillis(b)
	})
}

// BenchmarkTreeHashDirty holds the repository size fixed and varies how many
// files changed since the previous hash.
//
// This is the property that decides whether a large repository is usable:
// `git add -A` has to stat every tracked path, but it only re-hashes what
// moved. If the cost tracked the change count rather than the repository size,
// a 50k-file monorepo would be as cheap as a small one between edits.
//
// Unlike every other benchmark here the fixture is rebuilt rather than reused,
// because this one dirties what it measures: each iteration leaves one loose
// object per changed file behind, so a reused fixture carries the debris of
// every previous run.
//
// That turned out not to matter — five runs against freshly built fixtures
// agreed with a run against a fixture carrying half a million leftover objects
// to within 5% — but the rebuild stays, because a number that depends on how
// many times the suite has been run is not reproducible even when it happens
// to be stable.
func BenchmarkTreeHashDirty(b *testing.B) {
	if testing.Short() {
		b.Skip("large fixture skipped under -short")
	}
	const total = 20_000
	r := perfFreshRepo(b, perfRepoSpec{name: "repo-dirty", files: total})
	for _, changed := range []int{0, 1, 10, 100, 1_000, 10_000} {
		b.Run(fmt.Sprintf("%d_changed", changed), func(b *testing.B) {
			perfPrime(b, r)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				perfDirty(b, r, changed)
				b.StartTimer()
				if _, err := r.ws.TreeHash(); err != nil {
					b.Fatal(err)
				}
			}
			perfMillis(b)
		})
	}
	// Leave the fixture committed so a later run does not start dirty.
	perfGit(b, r.root, "checkout", "--", ".")
}

// ---------------------------------------------------------------------------
// 2. Environment fingerprint
// ---------------------------------------------------------------------------

// BenchmarkEnvFingerprint measures whatever ecosystems are registered, against
// a dependency set the size of a real project: perfDistInfos installed Python
// distributions and perfNodeEntries node_modules entries.
//
// Sub-benchmarks skip rather than fail when an ecosystem is not registered,
// because this file has to keep working while the ecosystem plugins are still
// being added.
func BenchmarkEnvFingerprint(b *testing.B) {
	cases := []perfRepoSpec{
		{name: "deps-none", files: 100},
		{name: "deps-python", files: 100, python: true},
		{name: "deps-node", files: 100, node: true},
		{name: "deps-both", files: 5_000, python: true, node: true},
		// A mid-size real Node app: npm hoists ~1250 packages to the top
		// level and writes a megabyte-scale lockfile at the root and another
		// inside node_modules. The fingerprint hashes both whole, so this is
		// the only node case that measures the read as well as the readdir.
		{name: "deps-node-real", files: 100, node: true, nodeEntries: 1_250, nodeLockBytes: 1 << 20},
	}
	for _, spec := range cases {
		b.Run(strings.TrimPrefix(spec.name, "deps-"), func(b *testing.B) {
			if spec.node && !perfEcosystemRegistered("node") {
				b.Skip("node ecosystem is not registered")
			}
			if spec.python && !perfEcosystemRegistered("python") {
				b.Skip("python ecosystem is not registered")
			}
			r := perfGetRepo(b, spec)
			// Pin the virtualenv to the fixture. An inherited VIRTUAL_ENV from
			// the developer's shell would point the fingerprint at a
			// completely different directory.
			b.Setenv("VIRTUAL_ENV", filepath.Join(r.root, ".venv"))

			_, complete := r.ws.EnvFingerprint()
			b.Logf("detected=%v complete=%v", r.ws.Ecosystems(), complete)
			// complete=0 means some detected ecosystem could not be
			// established, which makes the workspace unservable. Reported
			// rather than fatal, so a fixture the ecosystem plugins reject is
			// visible instead of silently mismeasured.
			if !complete {
				b.ReportMetric(0, "complete")
			} else {
				b.ReportMetric(1, "complete")
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.ws.EnvFingerprint()
			}
			perfMillis(b)
		})
	}
}

// ---------------------------------------------------------------------------
// 3. Full key derivation
// ---------------------------------------------------------------------------

// BenchmarkFullKey is everything the hook does before it can even ask the
// daemon a question: resolve the worktree, hash the tree, fingerprint the
// environment, derive the key. On a miss this whole thing runs a second time
// inside `hindsight record` to close the purity gate.
func BenchmarkFullKey(b *testing.B) {
	perfEachSize(b, func(b *testing.B, r *perfRepo) {
		perfPrime(b, r)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			st, err := r.ws.State()
			if err != nil {
				b.Fatal(err)
			}
			Key(st, ".", "pytest -q")
		}
		perfMillis(b)
	})
}

// BenchmarkFullKeyWithDependencies is the same measurement on a workspace that
// has its dependencies installed, which is the only realistic configuration.
func BenchmarkFullKeyWithDependencies(b *testing.B) {
	spec := perfRepoSpec{name: "deps-both", files: 5_000, python: true, node: true}
	r := perfGetRepo(b, spec)
	b.Setenv("VIRTUAL_ENV", filepath.Join(r.root, ".venv"))
	perfPrime(b, r)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := r.ws.State()
		if err != nil {
			b.Fatal(err)
		}
		Key(st, ".", "pytest -q")
	}
	perfMillis(b)
}

// BenchmarkNewWorkspace is the two `git rev-parse` calls the hook makes before
// any hashing starts. They resolve things that never change during a session,
// so this is pure per-invocation overhead.
func BenchmarkNewWorkspace(b *testing.B) {
	r := perfGetRepo(b, perfRepoSpec{name: "repo-100", files: 100})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := NewWorkspace(r.root); err != nil {
			b.Fatal(err)
		}
	}
	perfMillis(b)
}

// BenchmarkSpawnFloor decomposes the floor under everything above. The hook
// starts four git processes per intercepted command and `hindsight record`
// starts four more, so if spawning dominates then the cost is process creation
// rather than hashing, and no amount of caching inside git will help.
//
//   - true: the operating system's own process-creation cost.
//   - git_version: that plus loading and starting git, with no repository.
//   - git_rev_parse: that plus discovering and reading the repository.
func BenchmarkSpawnFloor(b *testing.B) {
	r := perfGetRepo(b, perfRepoSpec{name: "repo-100", files: 100})
	trueBin, err := exec.LookPath("true")
	if err != nil {
		trueBin = "/usr/bin/true"
	}
	cases := []struct {
		name string
		args []string
	}{
		{"true", []string{trueBin}},
		{"git_version", []string{"git", "--version"}},
		{"git_rev_parse", []string{"git", "rev-parse", "--git-dir"}},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				cmd := exec.Command(c.args[0], c.args[1:]...)
				cmd.Dir = r.root
				if err := cmd.Run(); err != nil {
					b.Fatal(err)
				}
			}
			perfMillis(b)
		})
	}
}

// BenchmarkKeyOnly is the sha256 by itself, to confirm the key derivation
// proper contributes nothing measurable.
func BenchmarkKeyOnly(b *testing.B) {
	st := State{Tree: strings.Repeat("a", 40), EnvFP: strings.Repeat("b", 32), EnvComplete: true}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Key(st, "src/api", "uv run pytest -q tests/test_billing.py")
	}
}

func BenchmarkNormalizeCommand(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		NormalizeCommand("  uv   run pytest -q   tests/test_billing.py  ")
	}
}

// ---------------------------------------------------------------------------
// 4. Classifier
// ---------------------------------------------------------------------------

// perfCommands is a realistic mix of what a coding agent actually runs, spread
// across every branch the classifier has: reads, builds, mutations, installs,
// git subcommands, chains, the non-hermeticity list, and quoting.
var perfCommands = []string{
	"ls",
	"ls -la src/",
	"pwd",
	"echo hello",
	"cat README.md",
	"head -n 40 internal/hp/key.go",
	"grep -rn 'func Classify' internal/",
	"rg --json -n 'TODO' .",
	"find . -name '*.go' -not -path './.git/*'",
	"wc -l internal/hp/*.go",
	"basename /usr/local/bin/hindsight",
	"sed -n '1,80p' internal/hp/policy.go",
	"sed -i.bak 's/foo/bar/' notes.txt",
	"git status --porcelain",
	"git diff --stat HEAD~1",
	"git log --oneline -20",
	"git push origin main",
	"go build ./cmd/hindsight",
	"go test ./... -run TestClassify",
	"go vet ./internal/hp/",
	"pytest -q tests/test_billing.py",
	"uv run pytest -q",
	"uv sync --extra dev",
	"pip install -e .",
	"npm test",
	"npx tsc --noEmit",
	"make build && make test",
	"mkdir -p build && cp -r src build/",
	"curl -s https://example.com/health",
	"date +%s",
	"echo $RANDOM",
	"cat out.txt | bash",
	"FOO=1 BAR=2 pytest -q -k 'billing and not slow'",
	"python3 -c \"import sys; print(sys.version)\"",
	"go test ./... 2>&1 | tail -20",
}

// BenchmarkClassifyMix is the per-command classifier cost on that mix. This
// runs on literally every command, including the ones that pass through, so it
// is the only part of the hook a PASSTHROUGH command pays.
func BenchmarkClassifyMix(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Classify(perfCommands[i%len(perfCommands)])
	}
}

// perfLongArgList is the shape an agent produces when it pastes an explicit
// file list into a command: one segment, thousands of tokens.
func perfLongArgList(n int) string {
	var b strings.Builder
	b.WriteString("grep -n 'needle'")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, " src/pkg%03d/mod%02d/file%05d.go", i/500, (i/20)%25, i)
	}
	return b.String()
}

// perfLongChain is the other pathological shape: hundreds of segments joined
// by &&, which exercises the chain rule rather than the tokenizer.
func perfLongChain(n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		if i > 0 {
			b.WriteString(" && ")
		}
		fmt.Fprintf(&b, "echo step%04d", i)
	}
	return b.String()
}

// perfLongQuoted keeps the whole payload inside one quoted argument, which is
// the worst case for the tokenizer's rune-by-rune scan.
func perfLongQuoted(n int) string {
	return "grep -n '" + strings.Repeat("needle|haystack|", n/16) + "' src/"
}

// BenchmarkClassifyLength checks the classifier is linear in command length.
// The ns/byte metric is the whole point: if it climbs with size, something in
// there is quadratic and a pasted file list will stall the hook.
func BenchmarkClassifyLength(b *testing.B) {
	for _, n := range []int{1 << 10, 4 << 10, 16 << 10, 64 << 10, 256 << 10} {
		b.Run(fmt.Sprintf("%dKB", n>>10), func(b *testing.B) {
			cmd := perfLongArgList(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				Classify(cmd)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(cmd)), "ns/byte")
		})
	}
}

// BenchmarkClassifyPathological is the specific 10KB command line, in each of
// the three shapes that stress a different part of the classifier, plus an
// unterminated quote because that path bails early and should be cheaper.
func BenchmarkClassifyPathological(b *testing.B) {
	const size = 10 << 10
	cases := []struct {
		name string
		cmd  string
	}{
		{"10KB_arg_list", perfLongArgList(size)},
		{"10KB_chain", perfLongChain(size)},
		{"10KB_quoted", perfLongQuoted(size)},
		{"10KB_unterminated_quote", "grep '" + strings.Repeat("x", size)},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				Classify(c.cmd)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e3, "us/op")
		})
	}
}

// ---------------------------------------------------------------------------
// Daemon round trip
// ---------------------------------------------------------------------------

func perfDaemon(b *testing.B) (*Store, *httptest.Server) {
	store, err := OpenStore(b.TempDir())
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	ts := httptest.NewServer(NewServer(store).Handler())
	b.Cleanup(ts.Close)
	b.Setenv("HP_DAEMON", ts.URL)
	return store, ts
}

// BenchmarkDaemonLookupMiss isolates the HTTP round trip, which is the other
// half of the hook's cost and the half that is easy to blame unfairly.
//
// Every iteration uses a distinct key. Repeating one key would take a lease and
// block the next caller, which is single-flight working correctly rather than
// the round trip being measured.
func BenchmarkDaemonLookupMiss(b *testing.B) {
	perfDaemon(b)
	c := NewClient()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Lookup(LookupReq{
			Key: fmt.Sprintf("hs-v1:perf-miss-%d", i), Agent: "perf",
			Cmd: "pytest -q", CmdNorm: "pytest -q", CwdRel: ".",
			Policy: "SERVE", Serve: true,
		}); err != nil {
			b.Fatal(err)
		}
	}
	perfMillis(b)
}

// BenchmarkDaemonLookupHit is the served path, which is more expensive than a
// miss because the daemon appends a HIT record to the log and broadcasts it
// before replying.
func BenchmarkDaemonLookupHit(b *testing.B) {
	store, _ := perfDaemon(b)
	const key = "hs-v1:perf-hit"
	if err := store.Append(&Record{
		V: 1, Agent: "a1", Key: key, Cmd: "pytest -q", CmdNorm: "pytest -q",
		CwdRel: ".", Policy: "SERVE", Decision: DecisionMiss, Servable: true,
		StdoutBlob: "sha256:00", StderrBlob: "sha256:01", DurationMS: 4634,
	}); err != nil {
		b.Fatalf("seed store: %v", err)
	}
	c := NewClient()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := c.Lookup(LookupReq{
			Key: key, Agent: "perf", Cmd: "pytest -q", CmdNorm: "pytest -q",
			CwdRel: ".", Policy: "SERVE", Serve: true,
		})
		if err != nil {
			b.Fatal(err)
		}
		if resp.Decision != DecisionHit {
			b.Fatalf("expected HIT, got %s", resp.Decision)
		}
	}
	perfMillis(b)
}

// BenchmarkDaemonLookupConcurrent is the round trip under fan-out.
//
// A serial number flatters the daemon: the whole premise is N agents running
// at once, and the daemon serializes index access behind one mutex and appends
// to one log file. If the round trip degrades with concurrency then the cost
// model built from the serial number understates what a real fleet pays.
//
// Distinct keys per iteration, for the same reason as the serial miss
// benchmark: repeating a key measures the lease, not the round trip.
func BenchmarkDaemonLookupConcurrent(b *testing.B) {
	for _, n := range []int{1, 5, 20} {
		b.Run(fmt.Sprintf("%d_agents", n), func(b *testing.B) {
			perfDaemon(b)

			// One shared client with an idle pool sized for the fan-out. Note
			// that SetParallelism multiplies by GOMAXPROCS, so "20 agents" is
			// 20 x GOMAXPROCS goroutines.
			//
			// The default transport keeps two idle connections per host. At
			// benchmark rates every goroutine beyond those two opens and closes
			// a socket per request, and the ephemeral port range runs out
			// before the timer does — the benchmark fails with "can't assign
			// requested address" rather than reporting anything.
			//
			// Pooling is the right instrument here anyway. Real agents are
			// separate short-lived processes issuing one request each and never
			// reuse a connection at all, so what is in question is whether the
			// daemon's single mutex and single log file degrade under
			// concurrency, not how fast this process can open sockets.
			conns := n * runtime.GOMAXPROCS(0) * 2
			c := NewClient()
			c.http = &http.Client{
				Transport: &http.Transport{MaxIdleConns: conns, MaxIdleConnsPerHost: conns},
				Timeout:   time.Minute,
			}

			var seq atomic.Int64
			b.SetParallelism(n)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					key := fmt.Sprintf("hs-v1:perf-conc-%d", seq.Add(1))
					if _, err := c.Lookup(LookupReq{
						Key: key, Agent: "perf", Cmd: "pytest -q", CmdNorm: "pytest -q",
						CwdRel: ".", Policy: "SERVE", Serve: true,
					}); err != nil {
						b.Error(err)
						return
					}
				}
			})
			perfMillis(b)
		})
	}
}

// ---------------------------------------------------------------------------
// 5. Process startup — the floor under every hook on every command
// ---------------------------------------------------------------------------

// perfRepoRoot walks up from the test's working directory for the go.mod.
func perfRepoRoot(tb testing.TB) string {
	dir, err := os.Getwd()
	if err != nil {
		tb.Skipf("no working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			tb.Skip("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// perfGoBuild compiles source into its own throwaway module and returns the
// binary path. Stdlib only, so it works offline.
func perfGoBuild(tb testing.TB, name, source string) string {
	if _, err := exec.LookPath("go"); err != nil {
		tb.Skip("go toolchain not on PATH")
	}
	dir := filepath.Join(tb.TempDir(), name)
	perfMkdir(tb, dir)
	perfWrite(tb, filepath.Join(dir, "go.mod"), []byte("module "+name+"\n\ngo 1.21\n"))
	perfWrite(tb, filepath.Join(dir, "main.go"), []byte(source))
	out := filepath.Join(dir, name+".bin")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GO111MODULE=on")
	if b, err := cmd.CombinedOutput(); err != nil {
		tb.Skipf("cannot build %s fixture: %v\n%s", name, err, b)
	}
	return out
}

// perfGoMinimalSrc is the smallest possible Go program: runtime startup and
// nothing else.
const perfGoMinimalSrc = `package main

func main() {}
`

// perfGoHooklikeSrc links the packages the real hook binary links and does
// what the disabled hook does — read one environment variable and exit.
//
// The difference between this and the minimal binary is what the dependency
// set costs at startup: a bigger image for dyld to map and more package init
// to run, before a single line of hook logic executes.
const perfGoHooklikeSrc = `package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
)

var sink any

func main() {
	if os.Getenv("HP_ENABLE") != "1" {
		return
	}
	// Unreachable in the benchmark, but it keeps the linker from dropping
	// the imports whose init cost is the thing being measured.
	sink = []any{sha256.New(), base64.StdEncoding, json.Marshal,
		flag.CommandLine, http.DefaultClient, exec.Command, filepath.Join}
}
`

// BenchmarkProcessStartup decomposes the cost of being a hook at all.
//
// A PreToolUse hook is a process the harness spawns before every command, so
// its startup is a tax on every command in the session including the ones that
// pass straight through. Nothing in the design can remove it; the only
// question is how big it is and how much of it is Go's rather than the
// operating system's.
//
//   - os_true: fork, exec and exit of a tiny C binary. The kernel's price.
//   - go_minimal: the same, for an empty Go program. The delta is the Go
//     runtime's own startup.
//   - go_hooklike: an empty Go program that links the hook's dependency set.
//     The delta from go_minimal is image size and package init.
//   - hindsight_disabled: the real binary on its kill-switch path. This is the
//     genuine floor a user pays per command, and the number the marginal cost
//     of interception has to be measured against.
func BenchmarkProcessStartup(b *testing.B) {
	trueBin, err := exec.LookPath("true")
	if err != nil {
		trueBin = "/usr/bin/true"
	}

	var hindsightBin string
	if _, err := exec.LookPath("go"); err == nil {
		out := filepath.Join(b.TempDir(), "hindsight")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/hindsight")
		cmd.Dir = perfRepoRoot(b)
		cmd.Env = append(os.Environ(), "GOFLAGS=")
		if msg, err := cmd.CombinedOutput(); err != nil {
			// Somebody else's in-flight edit can break the build. That must
			// cost this sub-benchmark and nothing else.
			b.Logf("cannot build cmd/hindsight, skipping that case: %v\n%s", err, msg)
		} else {
			hindsightBin = out
		}
	}

	cases := []struct {
		name string
		bin  string
		args []string
	}{
		{"os_true", trueBin, nil},
		{"go_minimal", perfGoBuild(b, "gominimal", perfGoMinimalSrc), nil},
		{"go_hooklike", perfGoBuild(b, "gohooklike", perfGoHooklikeSrc), nil},
		{"hindsight_disabled", hindsightBin, []string{"hook"}},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			if c.bin == "" {
				b.Skip("binary unavailable")
			}
			devNull, err := os.Open(os.DevNull)
			if err != nil {
				b.Fatal(err)
			}
			defer devNull.Close()
			spawn := func() {
				cmd := exec.Command(c.bin, c.args...)
				// The kill switch is what makes this the floor rather than a
				// full interception, so make sure it is off.
				cmd.Env = append(os.Environ(), "HP_ENABLE=")
				cmd.Stdin = devNull
				if err := cmd.Run(); err != nil {
					b.Fatal(err)
				}
			}
			// The first execution of a freshly built binary on macOS pays for
			// dyld, the page cache and a one-time signature check: measured at
			// 227 ms against 6 ms warm. A real session pays that once per
			// binary, so charging it to every command would be wrong by a
			// factor of thirty.
			for i := 0; i < 3; i++ {
				spawn()
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				spawn()
			}
			perfMillis(b)
		})
	}
}

// ---------------------------------------------------------------------------
// 6. The fastpath — what a command the memo already knows costs
// ---------------------------------------------------------------------------

// perfWriteFastpath lays down a memo of n entries and returns its home.
func perfWriteFastpath(tb testing.TB, n int) string {
	home := tb.TempDir()
	f := LoadFastpath(home)
	for i := 0; i < n; i++ {
		f.Observe(fmt.Sprintf("pytest -q tests/test_module%05d.py", i), int64(i%900))
	}
	f.Save()
	return home
}

// BenchmarkFastpathLoad is the cost of consulting the duration memo.
//
// The hook is a fresh process per command, so it re-reads and re-parses the
// whole memo every time — there is no warm map to inherit. That makes the
// memo's size a per-command cost, and a long-running fleet only ever adds to
// it. If this grows faster than the two tree hashes it exists to avoid, the
// optimization inverts.
func BenchmarkFastpathLoad(b *testing.B) {
	for _, n := range []int{0, 100, 1_000, 10_000} {
		b.Run(fmt.Sprintf("%d_entries", n), func(b *testing.B) {
			home := perfWriteFastpath(b, n)
			if fi, err := os.Stat(filepath.Join(home, "fastpath.json")); err == nil {
				b.ReportMetric(float64(fi.Size())/1024, "KB_memo")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				LoadFastpath(home)
			}
			perfMillis(b)
		})
	}
}

// BenchmarkFastpathKnownFast is the lookup itself, once the memo is loaded.
//
//   - always_cheap: the name-based shortcut, which never touches the map.
//   - hit: a command the memo knows is below the floor, so the hook bails.
//   - miss: a command the memo has never seen, so the hook goes on to hash.
func BenchmarkFastpathKnownFast(b *testing.B) {
	home := perfWriteFastpath(b, 1_000)
	f := LoadFastpath(home)
	floor := int64(DefaultMinDurationMS)
	cases := []struct{ name, cmd string }{
		{"always_cheap", "echo hello"},
		{"hit", "pytest -q tests/test_module00042.py"},
		{"miss", "uv run pytest -q tests/test_billing.py"},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				f.KnownFast(c.cmd, floor)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 7. Hook envelope — payload in, decision out
// ---------------------------------------------------------------------------

const perfHookPayload = `{"session_id":"bench","cwd":"/tmp/work/agent3",` +
	`"tool_name":"Bash","tool_input":{"command":"uv run pytest -q tests/test_billing.py"}}`

// BenchmarkHookEnvelope is the JSON either side of the decision. It is here to
// be ruled out of the cost model rather than because it is expected to matter.
func BenchmarkHookEnvelope(b *testing.B) {
	b.Run("parse", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, ok := ParseHookInput(strings.NewReader(perfHookPayload)); !ok {
				b.Fatal("payload did not parse")
			}
		}
	})
	b.Run("rewrite", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := Rewrite(io.Discard, HarnessClaude,
				"cat /tmp/blobs/ab/abc; cat /tmp/blobs/cd/cde >&2; exit 0",
				"hindsight: served from a2 (4634ms deleted)"); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// 8. Store open — what the record path pays as the corpus grows
// ---------------------------------------------------------------------------

// perfWriteLog lays down a log.jsonl of n records shaped like real ones, and
// returns the cache home holding it.
func perfWriteLog(tb testing.TB, n int) string {
	home := tb.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "blobs"), 0o755); err != nil {
		tb.Fatal(err)
	}
	f, err := os.Create(filepath.Join(home, "log.jsonl"))
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	enc := json.NewEncoder(w)
	for i := 0; i < n; i++ {
		rec := &Record{
			V: 1, TS: float64(i), Agent: fmt.Sprintf("a%d", i%5),
			Cmd:        fmt.Sprintf("uv run pytest -q tests/test_module%05d.py", i),
			CmdNorm:    fmt.Sprintf("uv run pytest -q tests/test_module%05d.py", i),
			CwdRel:     ".",
			TreeBefore: fmt.Sprintf("%040x", i), EnvFPBefore: fmt.Sprintf("%032x", i),
			TreeAfter: fmt.Sprintf("%040x", i), EnvFPAfter: fmt.Sprintf("%032x", i),
			Key:    fmt.Sprintf("hs-v1:%064x", i),
			Policy: "SERVE", Decision: DecisionMiss, Servable: true,
			DurationMS: int64(i % 5000),
			StdoutBlob: fmt.Sprintf("sha256:%064x", i),
			StderrBlob: fmt.Sprintf("sha256:%064x", i+1),
		}
		if err := enc.Encode(rec); err != nil {
			tb.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		tb.Fatal(err)
	}
	return home
}

// BenchmarkStoreOpen is the cost of rebuilding the in-memory index by scanning
// the append-only log.
//
// The daemon pays this once at startup, which is the case the design is
// written for and is entirely reasonable. But `hindsight record` calls
// OpenStore on every miss, in a fresh process, solely to write two blobs — and
// PutBlob needs only the Paths, never the replayed index. So this scan is on
// the critical path of every uncached command, and it grows with the corpus
// the cache is accumulating.
//
// 100k records is not a stretch: five agents on one task produced hundreds,
// and a shared team cache is append-only forever.
func BenchmarkStoreOpen(b *testing.B) {
	for _, n := range []int{0, 1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("%d_records", n), func(b *testing.B) {
			home := perfWriteLog(b, n)
			if fi, err := os.Stat(filepath.Join(home, "log.jsonl")); err == nil {
				b.ReportMetric(float64(fi.Size())/(1<<20), "MB_log")
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := OpenStore(home); err != nil {
					b.Fatal(err)
				}
			}
			perfMillis(b)
		})
	}
}

// BenchmarkStorePutBlob is what the record path actually needs from the store,
// for comparison with the scan above.
func BenchmarkStorePutBlob(b *testing.B) {
	store, err := OpenStore(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	// A test suite's output, which is the class of command worth caching.
	payload := []byte(strings.Repeat("test_module.py::test_case PASSED\n", 2_000))
	b.ReportMetric(float64(len(payload))/1024, "KB_blob")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Distinct content each time; an identical blob short-circuits on the
		// stat and would measure the deduplication rather than the write.
		if _, err := store.PutBlob(append(payload, byte(i), byte(i>>8))); err != nil {
			b.Fatal(err)
		}
	}
	perfMillis(b)
}

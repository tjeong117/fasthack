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
// take longer than the default benchtime and would otherwise be sampled once:
//
//	go test ./internal/hp/ -run XXX -bench . -benchtime 10x -count 3

import (
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
// worktree is not cold. scripts/bench.sh removes this directory when it
// finishes; set HP_PERF_FIXTURES to relocate it.
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

func perfGetRepo(tb testing.TB, spec perfRepoSpec) *perfRepo {
	perfRepoMu.Lock()
	defer perfRepoMu.Unlock()
	if r, ok := perfRepos[spec.name]; ok {
		return r
	}
	root := filepath.Join(perfFixtureRoot(tb), spec.name)
	files := perfLayout(spec.files)
	if !perfStampMatches(root, spec) {
		if err := os.RemoveAll(root); err != nil {
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
	return fmt.Sprintf("%s files=%d python=%v node=%v dists=%d node_entries=%d",
		perfFixtureVersion, spec.files, spec.python, spec.node, perfDistInfos, perfNodeEntries)
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
		perfBuildNodeModules(tb, root)
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

// perfBuildNodeModules writes a node_modules with perfNodeEntries top-level
// entries, perfNodeScopes of which are scopes the fingerprint has to descend
// into. The lockfile and the package manager's own record are both present,
// because without them the node ecosystem correctly abstains and the
// measurement would be of the abstention path rather than the real one.
func perfBuildNodeModules(tb testing.TB, root string) {
	perfWrite(tb, filepath.Join(root, "package.json"),
		[]byte(`{"name":"fixture","version":"1.0.0","dependencies":{}}`))
	perfWrite(tb, filepath.Join(root, "package-lock.json"),
		[]byte(`{"name":"fixture","lockfileVersion":3,"packages":{}}`))
	nm := filepath.Join(root, "node_modules")
	perfMkdir(tb, nm)
	perfWrite(tb, filepath.Join(nm, ".package-lock.json"),
		[]byte(`{"name":"fixture","lockfileVersion":3,"packages":{}}`))
	for i := 0; i < perfNodeEntries-perfNodeScopes; i++ {
		pkg := filepath.Join(nm, fmt.Sprintf("pkg-%03d", i))
		perfMkdir(tb, pkg)
		perfWrite(tb, filepath.Join(pkg, "package.json"),
			[]byte(fmt.Sprintf(`{"name":"pkg-%03d","version":"1.0.%d"}`, i, i%9)))
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
func BenchmarkTreeHashDirty(b *testing.B) {
	if testing.Short() {
		b.Skip("large fixture skipped under -short")
	}
	const total = 20_000
	r := perfGetRepo(b, perfRepoSpec{name: "repo-dirty", files: total})
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

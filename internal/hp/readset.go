package hp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Tier-2 dependency scoping: promotion against an observed read set.
//
// Tier 1 proves a peer's edit is irrelevant by comparing the diff against the
// command's literal path arguments. That is only a proof for commands whose
// read set *is* their arguments, and scope.go names the whole list: cat, head,
// wc, md5sum, diff. Every one of them is cheaper than the interception, so the
// sound subset of Tier 1 and the valuable subset of Tier 1 barely intersect,
// and the expensive commands worth caching are exactly the ones that follow a
// dependency graph the command line does not name.
//
// The fix is not a better parser. It is to stop inferring the read set and
// start observing it. A wrapper reports what the command actually read, and
// disjointness is then checked against a measurement rather than a guess.
//
// Why this is the piece that matters: a Tier-0 or lease-wait hit converts
// execution time into blocked time at par, so a five-agent run can delete 77%
// of execution-seconds and move wall clock by nothing. A Tier-1 promotion is
// an index hit against a peer's *completed* result, so it returns immediately.
// It is the only path in this system that turns deleted CPU into deleted wall
// clock, and until now it could not fire on any command anyone cared about.
//
// # The unsoundness this file is built around
//
// An observed read set is only valid for the state it was observed at. If the
// diff *adds* a file the command would now read -- a new test matching a glob,
// a new conftest.py in a parent directory, a new module that shadows an
// installed one -- the recorded set is silent about it by construction and
// disjointness is a lie. This is the known failure of observed-dependency
// caching and it is why Bazel makes you declare instead.
//
// It is handled by enumerating how a file gets read *without* being imported,
// not by refusing on additions wholesale. The three examples above are all
// discovered by scanning a directory or by resolving a name against a search
// path, and firstUnsafeAddition refuses on exactly those. An addition nothing
// discovers is unreachable, because promotion already requires that no file in
// the read set changed: the import graph is the one we measured, and a graph
// that did not change cannot reach a file that did not exist when we measured
// it.
//
// The second gap is the capture method itself. sys.modules reports imports,
// not file reads, so a test that opens a fixture with open() has a dependency
// the set does not contain. That is why a ReadSet carries the method that
// produced it: promotion requires not just that a changed path is absent from
// the set, but that the method *would have seen it* had it been read. A
// changed .py that is not in a sys.modules set was provably not imported. A
// changed .json proves nothing, and refuses.
//
// Every rule below fails towards refusal. A missed promotion costs a hit; a
// wrong one costs the product.

const readSetGitTimeout = 5 * time.Second

// ReadSetPythonImports is the method identifier written by
// scripts/hindsight_pytest_plugin.py.
const ReadSetPythonImports = "python/sys.modules"

// readSetPluginModule is the module name the plugin is imported under. It is
// deliberately unlikely to collide with anything in a target repo, because the
// directory holding it goes on PYTHONPATH for every wrapped command.
const readSetPluginModule = "hindsight_pytest_plugin"

// Environment variables read by the wrapper. HP_READSET_OUT is ours and
// overrides where the capture lands; the HINDSIGHT_ pair is the contract with
// the plugin and is only ever set on the child.
const (
	readSetOutVar      = "HINDSIGHT_READSET_OUT"
	readSetRootVar     = "HINDSIGHT_READSET_ROOT"
	readSetOutOverride = "HP_READSET_OUT"
)

// ReadSet is what a command was observed to read, together with the method
// that observed it.
//
// The method is not decoration. A set from sys.modules and a set from strace
// have different completeness guarantees, and a consumer that cannot tell them
// apart will eventually treat "absent from the set" as "not read", which is
// the one inference that turns a cache into a source of wrong answers.
type ReadSet struct {
	// Method identifies the wrapper that produced this set. Promotion is
	// refused outright for a method this binary does not know, because an
	// unknown method has an unstated guarantee.
	Method string `json:"method"`

	// Paths are repo-relative, slash-separated, sorted and deduplicated.
	Paths []string `json:"paths"`

	// TestGlobs are the project's own test discovery patterns, read from its
	// configuration rather than assumed. A file the diff adds that matches one
	// of these changes what the command runs while being absent from the set
	// by construction, so it refuses.
	TestGlobs []string `json:"test_globs,omitempty"`

	// Policy is the classifier's verdict for the command this set was observed
	// under. It travels with the set so that ScopeMatchObserved can refuse a
	// RECORD_ONLY command on its own, rather than trusting every future caller
	// to remember the check.
	Policy string `json:"policy,omitempty"`

	// Tool and Processes are provenance. Processes is the number of wrapper
	// reports that were unioned, which is above one for pytest-xdist or a
	// chained command.
	Tool      string `json:"tool,omitempty"`
	Processes int    `json:"processes,omitempty"`
}

// readSetMethodInfo records what a capture method can and cannot see.
type readSetMethodInfo struct {
	// observes are the file extensions whose reads this method reports. If a
	// changed path has one of these extensions and is absent from the set,
	// the method proves it was not read. Any other extension proves nothing.
	observes map[string]bool
	// guarantee is the human sentence a log reader needs.
	guarantee string
}

var readSetMethods = map[string]readSetMethodInfo{
	ReadSetPythonImports: {
		// A Python module in sys.modules carries __file__ pointing at its .py
		// source, or at the shared object for a compiled extension. Anything
		// else the process touched arrived through open(), which sys.modules
		// does not see.
		observes: map[string]bool{".py": true, ".so": true, ".pyd": true},
		guarantee: "every in-repo Python module the run imported, read out of sys.modules once the " +
			"session finished; a file opened with open() is not an import and is not in the set",
	},
}

// Guarantee is the note on how this set was captured and what it therefore
// covers. Empty for a method this binary does not recognise.
func (rs *ReadSet) Guarantee() string {
	if rs == nil {
		return ""
	}
	return readSetMethods[rs.Method].guarantee
}

// Observes reports whether a read of p would have appeared in a set captured
// by this method. It is the difference between "absent, therefore not read"
// and "absent, therefore unknown".
func (rs *ReadSet) Observes(p string) bool {
	if rs == nil {
		return false
	}
	return readSetMethods[rs.Method].observes[strings.ToLower(path.Ext(p))]
}

// Dirs are the directories the set read from, deduplicated and sorted.
func (rs *ReadSet) Dirs() []string {
	if rs == nil {
		return nil
	}
	return readSetDirsOf(rs.Paths)
}

func readSetDirsOf(paths []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		d := path.Dir(p)
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// testDirs are the directories the run collected a test file from, judged by
// the project's own discovery patterns.
//
// These are the directories where a new file can change the run without
// anything importing it, because pytest walks them rather than following a
// reference into them, and it walks them recursively. They are also where
// fixture data lives, which is the one place a test that globs a directory at
// runtime is likely to be pointed at.
func (rs *ReadSet) testDirs() []string {
	if rs == nil {
		return nil
	}
	var tests []string
	for _, p := range rs.Paths {
		if _, ok := rs.matchedTestGlob(path.Base(p)); ok {
			tests = append(tests, p)
		}
	}
	return readSetDirsOf(tests)
}

// matchedTestGlob reports which of the project's discovery patterns collects a
// file with this base name, if any.
func (rs *ReadSet) matchedTestGlob(base string) (string, bool) {
	for _, g := range rs.discoveryGlobs() {
		if ok, err := path.Match(g, base); err == nil && ok {
			return g, true
		}
	}
	return "", false
}

// importRoots are the directories this run is known to resolve *top-level*
// module names out of, and they are the reason additions cannot be waved
// through wholesale.
//
// A new module dropped into one of these shadows an installed distribution or
// a stdlib module of the same name: `import json` resolved to the stdlib when
// we observed the run, and resolves to the new file afterwards. Nothing in the
// read set changed, nothing new was imported by name, and the answer is still
// different. Deeper in the tree the risk goes away, because a file added to a
// package directory only creates the new dotted name pkg.thing, which no
// unchanged import statement mentions.
//
// The repo root is always one: pytest puts the rootdir on sys.path under the
// default prepend import mode, and `python -m pytest` puts the invocation
// directory there regardless. A directory holding a conftest.py the run
// imported is the other, for the same prepend rule.
func (rs *ReadSet) importRoots() map[string]bool {
	roots := map[string]bool{".": true}
	if rs == nil {
		return roots
	}
	for _, p := range rs.Paths {
		if path.Base(p) == "conftest.py" {
			roots[path.Dir(p)] = true
		}
	}
	return roots
}

// defaultTestGlobs are pytest's own defaults, used only when the wrapper could
// not read the project's configuration.
var defaultTestGlobs = []string{"test_*.py", "*_test.py"}

func (rs *ReadSet) discoveryGlobs() []string {
	if rs == nil || len(rs.TestGlobs) == 0 {
		return defaultTestGlobs
	}
	return rs.TestGlobs
}

// ScopeMatchObserved is ScopeMatch with a measured read set in place of the
// command's path arguments.
//
// The claim it makes is narrow and worth stating exactly: every path that
// differs between the two trees is one that the recorded run provably did not
// read, and the diff contains nothing the run would newly read. Anything the
// capture method cannot speak to refuses.
func ScopeMatchObserved(repoRoot, recordedTree, currentTree string, rs *ReadSet) ScopeDecision {
	if strings.TrimSpace(repoRoot) == "" {
		return ScopeDecision{Reason: "no repo root: a read set cannot be compared with a diff without one"}
	}
	if rs == nil {
		return ScopeDecision{Reason: "no read set was captured for the recorded run, and an unobserved " +
			"dependency set is exactly what tier 1 must never guess at"}
	}
	info, known := readSetMethods[rs.Method]
	if !known {
		return ScopeDecision{Reason: "read set captured by unknown method " + strconv.Quote(rs.Method) +
			": its completeness guarantee is unstated, so it cannot gate a promotion"}
	}
	if rs.Policy != SERVE.String() {
		return ScopeDecision{Reason: "the recorded command's policy was " + readSetPolicyName(rs.Policy) +
			" rather than SERVE, and observing what a command read does not make it servable"}
	}
	if len(rs.Paths) == 0 {
		return ScopeDecision{Reason: "the read set is empty: a run that read nothing inside the repo is " +
			"far more likely to be a capture that failed than a command with no inputs"}
	}
	if recordedTree == currentTree {
		return ScopeDecision{Reason: "identical trees: tier 0 already decides this, tier 2 has nothing to prove"}
	}
	if !isTreeName(recordedTree) || !isTreeName(currentTree) {
		return ScopeDecision{Reason: "malformed tree hash: refusing to hand it to git"}
	}

	changed, err := scopeChangedPaths(repoRoot, recordedTree, currentTree)
	if err != nil {
		return ScopeDecision{Reason: "cannot diff " + recordedTree + " against " + currentTree + ": " + err.Error()}
	}
	// Two different tree hashes always describe different content, so an empty
	// diff means we misread git rather than that nothing moved.
	if len(changed) == 0 {
		return ScopeDecision{Reason: "trees differ but diff-tree named no paths; refusing to guess why"}
	}

	// Part 3, and the reason this file exists rather than a two-line change to
	// scope.go. Additions are checked before anything else because they are
	// the failure the read set cannot see: the recorded run could not have
	// read a file that did not exist, so its absence from the set carries no
	// information at all.
	added, err := readSetAddedPaths(repoRoot, recordedTree, currentTree)
	if err != nil {
		return ScopeDecision{Reason: "cannot list additions between " + recordedTree + " and " +
			currentTree + ": " + err.Error()}
	}
	if p, why := firstUnsafeAddition(added, rs); p != "" {
		return ScopeDecision{
			Reason:       "the diff adds " + p + ", " + why,
			ChangedPaths: changed,
			ScopePaths:   rs.Paths,
		}
	}
	isAdded := make(map[string]bool, len(added))
	for _, a := range added {
		isAdded[a] = true
	}

	// A toolchain file reconfigures the run regardless of what it imported.
	// A changed pyproject.toml re-points testpaths and addopts; a changed
	// .gitignore changes what a walker walks. None of them need to be in the
	// read set to matter.
	if p := firstGloballyRelevant(changed); p != "" {
		return ScopeDecision{
			Reason:       "changed path " + p + " configures the toolchain and cannot be proven irrelevant to anything",
			ChangedPaths: changed,
			ScopePaths:   rs.Paths,
		}
	}

	for _, c := range changed {
		for _, s := range rs.Paths {
			if scopePathsConflict(s, c) {
				return ScopeDecision{
					Reason:       "changed path " + c + " is in the observed read set (" + s + ")",
					ChangedPaths: changed,
					ScopePaths:   rs.Paths,
				}
			}
		}
	}

	// The gate that makes absence mean something. A changed .py missing from a
	// sys.modules set was provably not imported. A changed .json missing from
	// the same set was possibly read with open() and never reported, so it
	// refuses even though the disjointness check above was satisfied.
	//
	// Additions are exempt, and only additions. The gate asks whether the
	// recorded run could have read this file's *previous* contents without the
	// method noticing; a file that did not exist has no previous contents and
	// the question is empty. What its existence does to the *next* run is a
	// different question, and firstUnsafeAddition above is the whole of our
	// answer to it.
	for _, c := range changed {
		if isAdded[c] {
			continue
		}
		if !info.observes[strings.ToLower(path.Ext(c))] {
			return ScopeDecision{
				Reason: "changed path " + c + " is not a kind of file this capture method reports (" +
					rs.Method + " sees " + info.guarantee + "), so its absence from the read set proves nothing",
				ChangedPaths: changed,
				ScopePaths:   rs.Paths,
			}
		}
	}

	return ScopeDecision{
		Promoted: true,
		Reason: "all " + strconv.Itoa(len(changed)) + " changed paths are absent from the " +
			strconv.Itoa(len(rs.Paths)) + "-path read set observed for this command by " + rs.Method +
			", the diff adds nothing it would newly read, and every changed path is of a kind that method reports",
		ChangedPaths: changed,
		ScopePaths:   rs.Paths,
	}
}

// readSetAutoDiscovered are files a Python run picks up by name rather than by
// reference. None of them has to be imported by anything to take effect, so
// none of them can be excluded by a read set, and adding one is always a
// refusal.
var readSetAutoDiscovered = map[string]bool{
	"conftest.py":      true,
	"__init__.py":      true,
	"pytest.ini":       true,
	"tox.ini":          true,
	"setup.cfg":        true,
	"pyproject.toml":   true,
	"sitecustomize.py": true,
	"usercustomize.py": true,
}

// firstUnsafeAddition returns the first added path that could change what the
// command reads, together with the argument for why it could.
//
// The narrowness here is load-bearing, and it rests on a precondition this
// function does not check: ScopeMatchObserved refuses whenever any file *in*
// the read set changed, and it does so for every diff that reaches this point.
// So the import graph rooted at the command's entry points is byte-identical
// to the one we measured, and an import graph that did not change cannot
// suddenly reach a file that did not exist when we measured it. Every rule
// below is therefore about the ways a file gets picked up *without* being
// imported by something -- by a directory being scanned, or by a name being
// resolved against a search path.
//
// Everything else -- a new README, a new module in a package nothing imports,
// a new fixture in a directory no test was collected from -- is unreachable
// from an unchanged import graph, and refusing it only costs hits. The earlier
// rule refused on any added .py and on anything sharing a directory with
// anything the run read, which in a repo with a root-level conftest.py meant
// every addition anywhere refused, which in a fan-out where agents add files
// constantly meant almost nothing promoted.
//
// The residual risk is code in the read set that globs a directory at runtime
// and opens what it finds. sys.modules cannot see that, and neither can we.
// Rule 3 covers where it actually happens -- fixtures under a test directory.
func firstUnsafeAddition(added []string, rs *ReadSet) (string, string) {
	testDirs := rs.testDirs()
	roots := rs.importRoots()
	for _, a := range added {
		base := path.Base(a)
		if readSetAutoDiscovered[base] || strings.HasSuffix(base, ".pth") {
			return a, "which python finds by name rather than by reference: the recorded run could not " +
				"have read a file that did not exist, and the next run will read it without anything importing it"
		}
		if g, ok := rs.matchedTestGlob(base); ok {
			return a, "which matches this project's own test discovery pattern " + strconv.Quote(g) +
				", so the recorded run collected a strictly smaller set of tests than the next one will"
		}
		if d := firstContainingDir(testDirs, a); d != "" {
			return a, "under " + readSetDirName(d) + ", which the recorded run collected tests from; " +
				"collection walks that directory rather than importing into it, and the method cannot " +
				"see a directory being walked"
		}
		if rs.Observes(a) && roots[path.Dir(a)] {
			return a, "a new top-level module in " + readSetDirName(path.Dir(a)) + ", which is on this " +
				"run's import search path: an unchanged `import " + strings.TrimSuffix(base, path.Ext(base)) +
				"` that resolved to an installed package would resolve to this file instead"
		}
	}
	return "", ""
}

// firstContainingDir reports the first of dirs that contains p, recursively.
// The repo root contains everything, which is deliberate.
func firstContainingDir(dirs []string, p string) string {
	for _, d := range dirs {
		if d == "." || strings.HasPrefix(p, d+"/") {
			return d
		}
	}
	return ""
}

func readSetDirName(d string) string {
	if d == "." {
		return "the repo root"
	}
	return d
}

func readSetPolicyName(p string) string {
	if strings.TrimSpace(p) == "" {
		return "unrecorded"
	}
	return p
}

// readSetAddedPaths asks git which paths exist in currentTree and not in
// recordedTree.
//
// The flags carry the same weight they do in scopeChangedPaths, plus one more:
// --no-renames is what makes a renamed file show up here as an addition. With
// rename detection on, git reports a single R entry and the new name never
// appears in the added set, which is precisely the case this check exists for.
func readSetAddedPaths(repoRoot, recordedTree, currentTree string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), readSetGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot,
		"diff-tree", "-r", "-z", "--name-only", "--no-commit-id", "--no-renames",
		"--diff-filter=A", recordedTree, currentTree)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = path.Clean(filepath.ToSlash(p))
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// ---------------------------------------------------------------------------
// Capture
// ---------------------------------------------------------------------------

// readSetLine is one wrapper report. One line per Python process, so an xdist
// run or a chained command produces several and the union is taken.
type readSetLine struct {
	V          int      `json:"v"`
	Method     string   `json:"method"`
	Tool       string   `json:"tool"`
	Root       string   `json:"root"`
	ExitStatus int      `json:"exit_status"`
	Complete   bool     `json:"complete"`
	TestGlobs  []string `json:"test_globs"`
	Paths      []string `json:"paths"`
}

// CaptureReadSet collects whatever the wrapper wrote for this process and
// removes the file.
//
// It is strict on purpose. Every rejection below degrades to a cache miss,
// while accepting a partial capture degrades to a wrong answer: the whole
// promotion argument is "this path is absent from the set, therefore it was
// not read", and a set that lost a line is a set that lost paths.
func CaptureReadSet(root string) (*ReadSet, bool) {
	p := readSetOutPath(root)
	b, err := os.ReadFile(p)
	// Removed whether or not it parsed. A file left behind would be read as
	// the next command's read set.
	_ = os.Remove(p)
	if err != nil {
		return nil, false
	}
	return parseReadSet(b, root)
}

func parseReadSet(b []byte, root string) (*ReadSet, bool) {
	wantRoot := readSetRealPath(root)

	rs := &ReadSet{}
	paths := map[string]bool{}
	globs := map[string]bool{}
	lines := 0

	for _, raw := range strings.Split(string(b), "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var l readSetLine
		if json.Unmarshal([]byte(raw), &l) != nil {
			// A torn line means two processes interleaved an append. The
			// surviving lines are a read set with paths missing.
			return nil, false
		}
		if l.V != SchemaVersionReadSet || !l.Complete {
			return nil, false
		}
		if _, known := readSetMethods[l.Method]; !known {
			return nil, false
		}
		// Mixed methods would need the weakest guarantee of the two, and there
		// is only one method today, so refuse rather than invent the lattice.
		if rs.Method != "" && rs.Method != l.Method {
			return nil, false
		}
		// pytest exit codes: 0 passed, 1 tests failed, 2 interrupted,
		// 3 internal error, 4 usage error, 5 nothing collected. Only the first
		// two describe a session that collected and imported normally; the
		// rest can stop before the import graph is complete, and a truncated
		// import graph is a read set with paths missing.
		if l.ExitStatus != 0 && l.ExitStatus != 1 {
			return nil, false
		}
		if wantRoot != "" && readSetRealPath(l.Root) != wantRoot {
			// A capture from another repo, or a stale file. Either way it
			// describes a different question.
			return nil, false
		}
		for _, p := range l.Paths {
			clean, ok := readSetCleanPath(p)
			if !ok {
				return nil, false
			}
			paths[clean] = true
		}
		for _, g := range l.TestGlobs {
			if g = strings.TrimSpace(g); g != "" {
				globs[g] = true
			}
		}
		rs.Method = l.Method
		if rs.Tool == "" {
			rs.Tool = l.Tool
		}
		lines++
	}
	if lines == 0 {
		return nil, false
	}

	rs.Processes = lines
	rs.Paths = sortedKeys(paths)
	rs.TestGlobs = sortedKeys(globs)
	return rs, true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// readSetCleanPath normalizes a wrapper-reported path and rejects anything
// that is not plainly inside the repo. Rejecting refuses the whole capture,
// which is the point: dropping the odd path quietly is how a real dependency
// escapes the set.
func readSetCleanPath(p string) (string, bool) {
	p = strings.TrimSpace(filepath.ToSlash(p))
	if p == "" || strings.HasPrefix(p, "/") {
		return "", false
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	return c, true
}

func readSetRealPath(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(r)
	}
	return filepath.Clean(p)
}

// ---------------------------------------------------------------------------
// Activation
// ---------------------------------------------------------------------------

// ReadSetEnv returns the environment additions that arm the wrapper, as
// KEY=VALUE strings.
//
// It is inert for anything that is not Python, because nothing but Python
// reads PYTHONPATH and nothing but pytest reads PYTEST_ADDOPTS. Both existing
// values are preserved rather than replaced.
//
// It returns nil if the plugin could not be written. That is not a nicety:
// pytest treats "-p <unimportable module>" as a fatal usage error, so emitting
// the flag without the module on the path would break every Python command in
// the repo rather than merely failing to observe it.
func ReadSetEnv(root string) []string {
	dir, err := readSetPluginDir(root)
	if err != nil {
		return nil
	}
	out := readSetOutPath(root)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return nil
	}
	// A file left by a crashed run whose pid we have been given again would
	// otherwise be collected as this command's read set.
	_ = os.Remove(out)

	addopts := strings.TrimSpace(os.Getenv("PYTEST_ADDOPTS") + " -p " + readSetPluginModule)
	return []string{
		readSetOutVar + "=" + out,
		readSetRootVar + "=" + root,
		"PYTHONPATH=" + prependPathList(dir, os.Getenv("PYTHONPATH")),
		"PYTEST_ADDOPTS=" + addopts,
	}
}

// ArmReadSet applies the wrapper environment to this process, so the command
// hindsight record is about to run inherits it, and returns the function that
// takes it back off again.
//
// Calling the returned function before the after-state is measured is not
// optional, and this is why the API hands back a disarm rather than a bool:
// PYTHONPATH is in envAllow, the environment allowlist that feeds the cache
// key. A record process that armed and never disarmed would measure an
// env fingerprint that no longer matches the one the hook measured before the
// command, fail the purity gate on every single command, and quietly reduce
// the whole cache to nothing while looking perfectly healthy.
//
// Safe to call unconditionally; the returned function is never nil, and is a
// no-op when arming did not happen. hindsight record runs one command per
// process, so mutating the process environment for the duration of that
// command has no other reader.
func ArmReadSet(root string) (disarm func()) {
	if strings.TrimSpace(root) == "" {
		return func() {}
	}
	env := ReadSetEnv(root)
	if len(env) == 0 {
		return func() {}
	}
	type saved struct {
		value string
		had   bool
	}
	prev := map[string]saved{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, seen := prev[k]; !seen {
			old, had := os.LookupEnv(k)
			prev[k] = saved{value: old, had: had}
		}
		if err := os.Setenv(k, v); err != nil {
			Debugf("could not arm read-set capture: %v", err)
		}
	}
	return func() {
		for k, s := range prev {
			if s.had {
				_ = os.Setenv(k, s.value)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	}
}

// ArmReadSetIn is ArmReadSet for a workspace that may not have resolved.
//
// NewWorkspace returns a nil *Workspace when the directory is not a git
// worktree, and the recording path deliberately carries on regardless -- the
// command still has to run and the agent still has to get its output. This
// exists so that the caller gets one unconditional line instead of a nil check
// wrapped around a deferred restore, which is the shape mistakes hide in.
func ArmReadSetIn(ws *Workspace) (disarm func()) {
	if ws == nil {
		return func() {}
	}
	return ArmReadSet(ws.Root)
}

func prependPathList(dir, existing string) string {
	if existing == "" {
		return dir
	}
	return dir + string(os.PathListSeparator) + existing
}

// readSetOutPath is where the wrapper writes and CaptureReadSet reads. Keyed
// on the pid so two hindsight record processes in one worktree cannot collect
// each other's observations.
func readSetOutPath(root string) string {
	if p := os.Getenv(readSetOutOverride); p != "" {
		return p
	}
	return filepath.Join(Home(root), "readsets", "rs-"+strconv.Itoa(os.Getpid())+".jsonl")
}

// readSetPluginDir materializes the plugin under $HP_HOME and returns the
// directory to put on PYTHONPATH.
//
// Under HP_HOME rather than in the workspace, for the usual reason: a file
// written inside the tree changes the tree hash that keys the cache. Written
// with a rename so that five agents arming simultaneously cannot hand pytest a
// half-written module.
func readSetPluginDir(root string) (string, error) {
	dir := filepath.Join(Home(root), "pyplugin")
	file := filepath.Join(dir, readSetPluginModule+".py")
	if b, err := os.ReadFile(file); err == nil && string(b) == readSetPluginSource {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".plugin-*.py")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(readSetPluginSource); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return "", err
	}
	if err := os.Rename(name, file); err != nil {
		os.Remove(name)
		return "", err
	}
	return dir, nil
}

// SchemaVersionReadSet is the wrapper line format version. A wrapper from a
// newer hindsight is refused rather than interpreted.
const SchemaVersionReadSet = 1

// readSetPluginSource is scripts/hindsight_pytest_plugin.py, carried in the
// binary so that a hindsight installed anywhere can arm itself without the
// source tree. readset_test.go asserts the two are byte-identical.
const readSetPluginSource = `"""Observed read sets for Hindsight, captured from a real pytest run.

Hindsight keys a shell command on git's tree hash. Once two agents edit
anything, their trees diverge and every command misses -- including commands
whose inputs did not move. Tier-1 scoping recovers those hits by proving the
changed paths are disjoint from what the command reads, which only works if
we know what the command reads.

Inferring that from the command line is unsound, and there is a test in this
repo that says so: "pytest tests/test_billing.py" reads src/billing.py through
an import, and that path appears nowhere in argv. So Tier-1 was restricted to
commands whose read set provably is their arguments -- cat, wc, md5sum -- all
of which are too cheap to be worth caching. This plugin removes the guess: it
reports what the run actually imported.

WHAT THIS CAPTURES, AND WHAT IT DOES NOT
========================================
When the session finishes we walk sys.modules and keep every module whose
__file__ resolves inside the repo root. That is the import graph, measured
rather than inferred, and it is exactly the dependency edge Tier-1 needs.

It is NOT the set of files the run opened. A test that does

    open("tests/data/rates.json")

reads a file that never enters sys.modules, and this plugin will not see it.
That gap is real and it is the reason the Go side does not treat this set as
a complete read set: internal/hp/readset.go refuses to promote whenever a
changed path is of a kind this method cannot observe, so a changed .json
refuses while a changed .py that was provably never imported promotes. A
missed promotion costs a hit; a wrong one costs the product.

sys.modules rather than coverage.py, for three reasons: it needs no
dependency, so it cannot fail to install in a target repo; it costs one dict
walk at teardown rather than a tracer on every line; and imports are the edge
Tier-1 reasons about, whereas line coverage is both more and less than that.

THREE RULES THIS FILE MUST NEVER BREAK
======================================
1. Print nothing, ever. Anything reaching stdout or stderr becomes part of the
   recorded output, which means it is baked into every cache entry for the
   command and diffed forever by shadow verification.
2. Raise nothing, ever. An exception from a pytest hook is an INTERNALERROR
   and changes the exit code of the command we are only supposed to be
   watching.
3. Emit a whole line or no line. A read set with a path missing is the single
   failure mode that produces a wrong answer rather than a slow one, so every
   partial observation is reported as incomplete and the Go side drops it.

ACTIVATION, WITHOUT ASKING ANYONE TO INSTALL ANYTHING
=====================================================
hindsight record adds three variables to the environment of the command it
wraps:

    PYTHONPATH=<dir holding this file>:$PYTHONPATH
    PYTEST_ADDOPTS=$PYTEST_ADDOPTS -p hindsight_pytest_plugin
    HINDSIGHT_READSET_OUT=<jsonl path>   HINDSIGHT_READSET_ROOT=<repo root>

No entry point, no pip install, no conftest edit, and nothing written inside
the workspace. It is inert for anything that is not Python, because nothing
else reads those variables. If either HINDSIGHT_ variable is missing this
plugin does nothing at all, which is what makes it safe to leave loaded.

OUTPUT FORMAT
=============
One JSON object per line, appended. One line per Python process, so an xdist
run or a chained "pytest a && pytest b" produces several and the Go side takes
the union.

    {"v": 1, "method": "python/sys.modules", "tool": "pytest", "pid": 4711,
     "root": "/abs/repo", "rootdir": "/abs/repo", "exit_status": 0,
     "complete": true, "test_globs": ["test_*.py", "*_test.py"],
     "paths": ["conftest.py", "src/billing.py", "tests/test_billing.py"]}

The line is written with a single os.write to a file opened O_APPEND, which
is atomic for any payload the kernel can take in one go. A torn line from a
pathologically large set does not parse, and the Go side refuses the whole
capture rather than silently using the surviving lines -- because the
surviving lines are a read set with paths missing, which is rule 3.
"""

import json
import os
import sys

SCHEMA_VERSION = 1

# Names the capture method so a consumer can tell a sys.modules set from an
# strace set. They do not have the same completeness guarantee and nothing
# downstream should have to infer which one it is holding.
METHOD = "python/sys.modules"

# Directory names whose contents are environment rather than tree. They are
# dropped from the read set, and dropping is normally the unsafe direction --
# but these are covered by the environment fingerprint instead, which is a
# component of the cache key, so a candidate record with a different installed
# package set is never even considered for promotion. Keeping them would add
# thousands of paths that no git diff can ever name.
SKIP_DIR_NAMES = frozenset((
    ".git",
    ".venv",
    "venv",
    ".tox",
    ".nox",
    ".eggs",
    "__pycache__",
    "site-packages",
    "dist-packages",
    "node_modules",
))

# A set this large is a sign we are looking at a vendored tree rather than a
# dependency graph. Report it as incomplete instead of writing a megabyte into
# every cache record.
MAX_PATHS = 20000

_emitted = False


def _in_repo(real_path, real_root):
    """Repo-relative slash path, or None if the file is outside the repo."""
    if real_path == real_root:
        return None
    prefix = real_root + os.sep
    if not real_path.startswith(prefix):
        return None
    rel = real_path[len(prefix):]
    if not rel:
        return None
    return rel.replace(os.sep, "/")


def _skipped(rel):
    return any(part in SKIP_DIR_NAMES for part in rel.split("/")[:-1])


def _collect(real_root):
    """Walk sys.modules. Returns (sorted paths, complete).

    complete is False when any module carried a __file__ we could not resolve
    to an absolute path. Python 3.9 and later always give an absolute
    __file__, so this should not fire -- but resolving a relative one against
    the current working directory would invent a path, and inventing a path is
    how a real dependency escapes the set.
    """
    complete = True
    found = set()
    # list() because a module's __getattr__ can import, which would mutate
    # sys.modules while we walk it.
    for _name, module in list(sys.modules.items()):
        try:
            filename = getattr(module, "__file__", None)
        except Exception:
            complete = False
            continue
        if not filename or not isinstance(filename, str):
            # Builtins and namespace packages have no file. A namespace
            # package contributes no source of its own, so there is nothing to
            # miss; its submodules appear here in their own right.
            continue
        if not os.path.isabs(filename):
            complete = False
            continue
        try:
            resolved = os.path.realpath(filename)
        except Exception:
            complete = False
            continue
        rel = _in_repo(resolved, real_root)
        if rel is None or _skipped(rel):
            continue
        found.add(rel)
        if len(found) > MAX_PATHS:
            return sorted(found), False
    return sorted(found), complete


def _test_globs(config):
    """The project's own test discovery patterns, not our guess at them.

    The Go side refuses to promote when the diff adds a file these patterns
    would collect, because such a file changes what the command runs while
    being absent from the read set by construction.
    """
    try:
        globs = [g for g in config.getini("python_files") if isinstance(g, str)]
    except Exception:
        globs = []
    return globs or ["test_*.py", "*_test.py"]


def _emit(payload, out_path):
    data = (json.dumps(payload, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
    fd = os.open(out_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    try:
        os.write(fd, data)
    finally:
        os.close(fd)


def pytest_sessionfinish(session, exitstatus):
    """The only hook. Runs after collection and execution, before teardown of
    the process, which is the point at which sys.modules is at its fullest.

    Everything is inside one try/except that swallows absolutely everything.
    A read-set capture that breaks the run it is measuring is worse than no
    capture at all: the cache degrades to a miss without it, and to a broken
    tool with it.
    """
    global _emitted
    try:
        if _emitted:
            return
        out_path = os.environ.get("HINDSIGHT_READSET_OUT")
        root = os.environ.get("HINDSIGHT_READSET_ROOT")
        if not out_path or not root:
            return
        real_root = os.path.realpath(root)
        paths, complete = _collect(real_root)

        config = getattr(session, "config", None)
        try:
            rootdir = str(config.rootpath)
        except Exception:
            rootdir = ""

        _emitted = True
        _emit({
            "v": SCHEMA_VERSION,
            "method": METHOD,
            "tool": "pytest",
            "pid": os.getpid(),
            "root": real_root,
            "rootdir": rootdir,
            "exit_status": int(exitstatus) if exitstatus is not None else -1,
            "complete": bool(complete),
            "test_globs": _test_globs(config) if config is not None else [],
            "paths": paths,
        }, out_path)
    except Exception:
        # Rule 2. There is no acceptable way to report this: stderr is part of
        # the recorded output, and a raise is an INTERNALERROR. Emitting
        # nothing is the correct outcome, because the Go side treats a missing
        # read set as a refusal to promote.
        pass
`

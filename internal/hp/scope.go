package hp

import (
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Tier-1 dependency scoping: diff-disjoint promotion.
//
// Tier 0 is exact tree match, which is always sound and is what ships today.
// Its weakness is coarseness: if a peer edits src/auth.py, our
// `pytest tests/test_billing.py` misses even though nothing it reads changed.
// The corpus says this is where nearly all the value leaks — cross-agent reuse
// runs at 16.9% across the first three commands and 1.0% after step 50,
// because agents diverge and never share a tree again.
//
// Tier 1 takes a tree-key miss as a *candidate*, asks git which paths differ
// between the recorded tree and the current one, and promotes to a hit when
// those paths are provably irrelevant to the command.
//
// The distinction from similarity matching matters and is the whole reason
// this is safe: we are not claiming two states look alike, we are proving the
// difference cannot affect this command. A wrong scope is a wrong answer, so
// promotion is allowed only from literal path arguments actually present in
// the command — never from an inferred or guessed dependency set.
//
// Everything below is written so that the failure direction is refusal. Three
// rules keep it that way, and they are worth stating because almost every
// decision in this file is an application of one of them:
//
//  1. Adding a path to the scope set can only block a promotion, so when a
//     token might be a path we treat it as one.
//  2. Dropping a path the command actually reads is the one way to serve a
//     wrong answer, so a token we cannot classify refuses the whole command
//     rather than being quietly skipped.
//  3. Anything we would have to guess about — a glob, a variable, an absolute
//     path, a build manifest — refuses.

const scopeGitTimeout = 5 * time.Second

// ScopeDecision explains a Tier-1 outcome, for logging and for the viewer.
type ScopeDecision struct {
	Promoted     bool
	Reason       string
	ChangedPaths []string
	ScopePaths   []string
}

// ScopeMatch reports whether a candidate record recorded at recordedTree can
// be promoted to a hit for a command running at currentTree.
//
// Path arguments are interpreted relative to repoRoot. A command that ran in a
// subdirectory has a different meaning for the same argument, so callers that
// know the working directory should use ScopeMatchAt instead; passing a
// subdirectory command here scopes it against the wrong paths, and the
// names-nothing-in-either-tree gate in tokenScopePath is what stops that from
// becoming a wrong answer rather than merely a missed hit.
func ScopeMatch(repoRoot, recordedTree, currentTree, cmd string) ScopeDecision {
	return ScopeMatchAt(repoRoot, ".", recordedTree, currentTree, cmd)
}

// ScopeMatchAt is ScopeMatch with the command's working directory, given
// relative to repoRoot exactly as Workspace.CwdRel returns it. Path arguments
// are resolved against it before being compared with git's output, which is
// always relative to the repo root.
func ScopeMatchAt(repoRoot, cwdRel, recordedTree, currentTree, cmd string) ScopeDecision {
	if strings.TrimSpace(repoRoot) == "" {
		return ScopeDecision{Reason: "no repo root: path arguments cannot be resolved"}
	}
	cwd, ok := scopeCleanCwd(cwdRel)
	if !ok {
		return ScopeDecision{Reason: "working directory " + cwdRel + " is not inside the repo root"}
	}
	if recordedTree == currentTree {
		return ScopeDecision{Reason: "identical trees: tier 0 already decides this, tier 1 has nothing to prove"}
	}
	if !isTreeName(recordedTree) || !isTreeName(currentTree) {
		return ScopeDecision{Reason: "malformed tree hash: refusing to hand it to git"}
	}

	changed, err := scopeChangedPaths(repoRoot, recordedTree, currentTree)
	if err != nil {
		return ScopeDecision{Reason: "cannot diff " + recordedTree + " against " + currentTree + ": " + err.Error()}
	}
	// Two different tree hashes always describe different content, so an empty
	// diff means we misread git rather than that nothing moved. Refuse instead
	// of promoting on an empty changed set, which would promote everything.
	if len(changed) == 0 {
		return ScopeDecision{Reason: "trees differ but diff-tree named no paths; refusing to guess why"}
	}
	if p := firstGloballyRelevant(changed); p != "" {
		return ScopeDecision{
			Reason:       "changed path " + p + " configures the toolchain and cannot be proven irrelevant to anything",
			ChangedPaths: changed,
		}
	}

	scope, refuse := commandScopePaths(repoRoot, cwd, cmd, changed)
	if refuse != "" {
		return ScopeDecision{Reason: refuse, ChangedPaths: changed}
	}

	for _, s := range scope {
		for _, c := range changed {
			if scopePathsConflict(s, c) {
				return ScopeDecision{
					Reason:       "changed path " + c + " is not disjoint from scope " + s,
					ChangedPaths: changed,
					ScopePaths:   scope,
				}
			}
		}
	}
	return ScopeDecision{
		Promoted:     true,
		Reason:       "all " + strconv.Itoa(len(changed)) + " changed paths are disjoint from the literal path arguments [" + strings.Join(scope, " ") + "]",
		ChangedPaths: changed,
		ScopePaths:   scope,
	}
}

// scopeChangedPaths asks git which paths differ between two trees. Both are
// real objects in the shared object store, so this never touches a worktree.
//
// The flags are all load-bearing. -z stops git from C-quoting paths with
// unusual bytes, which would otherwise arrive mangled and silently fail to
// match a scope path. --no-renames stops rename detection from reporting only
// the new name, which would hide the old path from the disjointness check.
func scopeChangedPaths(repoRoot, recordedTree, currentTree string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), scopeGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot,
		"diff-tree", "-r", "-z", "--name-only", "--no-commit-id", "--no-renames",
		recordedTree, currentTree)
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

// commandScopePaths returns the literal path arguments of a command, or a
// human-readable refusal.
//
// Chain rule, matching Classify: split on &&, ||, ; and |, and let the
// strictest segment win. Here strictest means a refusal anywhere refuses the
// whole command, and the scope is the union of every segment's paths.
//
// Every segment must be scoped on its own. Taking the union alone would let
// `cat src/a.py; pytest` promote on the scope of its first segment while its
// second reads the entire tree.
func commandScopePaths(repoRoot, cwd, cmd string, changed []string) ([]string, string) {
	if strings.TrimSpace(cmd) == "" {
		return nil, "empty command"
	}
	// Checked before splitting, because a substitution can span a separator.
	// Its output is not in the string, so neither are its paths.
	if hasCommandSubstitution(cmd) {
		return nil, "command substitution: the real arguments are not in the command string"
	}

	seen := map[string]bool{}
	var scope []string
	segments := 0
	for _, seg := range splitSegments(cmd) {
		if strings.TrimSpace(seg.text) == "" {
			continue
		}
		segments++
		head, paths, refuse := segmentScopePaths(repoRoot, cwd, seg, changed)
		if refuse != "" {
			return nil, refuse
		}
		if len(paths) == 0 && !(seg.sep == "|" && scopeStdinFilters[head]) {
			return nil, "no literal path arguments in `" + strings.TrimSpace(seg.text) +
				"`: an unscoped command reads whatever it likes"
		}
		for _, p := range paths {
			if !seen[p] {
				seen[p] = true
				scope = append(scope, p)
			}
		}
	}
	if segments == 0 {
		return nil, "empty command"
	}
	if len(scope) == 0 {
		return nil, "no literal path arguments: an unscoped command reads whatever it likes"
	}
	sort.Strings(scope)
	return scope, ""
}

func segmentScopePaths(repoRoot, cwd string, seg shellSegment, changed []string) (string, []string, string) {
	toks, redirect, ok := tokenizeSegment(seg.text)
	if !ok {
		return "", nil, "unparseable: unterminated quote"
	}
	// A redirection target is a write rather than a read, and the tokenizer
	// hands it back looking exactly like an argument. Refusing costs almost
	// nothing: 2>&1 is deliberately not counted as a redirection, and a
	// command that writes into the tree fails the purity gate anyway.
	if redirect {
		return "", nil, "output redirection: the target is a write we cannot bound"
	}
	for len(toks) > 0 && isEnvAssignment(toks[0].text) {
		toks = toks[1:]
	}
	if len(toks) == 0 {
		return "", nil, "no command after inline env assignments"
	}
	head := cmdBase(toks[0].text)
	if head == "" {
		return "", nil, "unparseable: no head command"
	}
	// The refuse-list is checked against every bare word, not just the head,
	// because wrappers hide the real command one token in: `uv run make` and
	// `timeout 60 find .` both have an innocent head. Quoted words are data
	// rather than command names, the same distinction nonHermeticWord draws.
	for _, t := range toks {
		if t.quoted || strings.HasPrefix(t.text, "-") {
			continue
		}
		if b := cmdBase(t.text); scopeRefuseHeads[b] {
			return "", nil, "output depends on the whole tree regardless of arguments: " + b
		}
	}

	var scope []string
	for _, t := range toks[1:] {
		p, refuse := tokenScopePath(repoRoot, cwd, t.text, changed)
		if refuse != "" {
			return "", nil, refuse
		}
		if p != "" {
			scope = append(scope, p)
		}
	}
	return head, scope, ""
}

// tokenScopePath classifies one argument into exactly one of three outcomes:
// a repo-relative path to add to the scope, nothing at all because the token
// provably is not a path, or a refusal because we cannot tell which.
//
// The third outcome is the one that matters. Silently skipping a token we
// cannot classify is how a command's real dependency escapes the scope set,
// and that is the only mistake in this file that produces a wrong answer.
func tokenScopePath(repoRoot, cwd, raw string, changed []string) (string, string) {
	if raw == "" {
		return "", ""
	}
	// Flags are never scoped. A flag that carries a path is a semantics
	// question we would have to answer per tool (--rootdir bounds pytest,
	// --ignore excludes, -k is not a path at all), so a flag whose value looks
	// like a path refuses the command instead of being interpreted.
	if strings.HasPrefix(raw, "-") {
		if i := strings.IndexByte(raw, '='); i >= 0 {
			if v := raw[i+1:]; v != "" && looksLikeRepoPath(repoRoot, cwd, stripNodeID(v)) {
				return "", "flag " + raw + " carries a path whose meaning depends on the tool"
			}
		}
		return "", ""
	}
	if strings.ContainsAny(raw, "$~`") {
		return "", "argument " + raw + " needs shell expansion, so the literal string is not the path"
	}
	// A glob's match set is a function of the tree, and the tree is exactly
	// what changed. There is nothing to prove here.
	if strings.ContainsAny(raw, "*?[]{}") {
		return "", "argument " + raw + " is a glob, and a glob's match set depends on the tree that changed"
	}

	cand := stripNodeID(raw)
	if cand != "/" {
		cand = strings.TrimSuffix(cand, "/")
	}
	if cand == "" || cand == "." {
		// `pytest .` scopes the whole tree, which is the same as no scope.
		if raw == "." || raw == "./" {
			return "", "argument " + raw + " scopes the whole tree"
		}
		return "", ""
	}
	// A surviving colon is a path we do not understand: a URL, a remote spec,
	// a tool-specific selector. Node ids are the one colon syntax we decode.
	if strings.ContainsRune(cand, ':') {
		return "", "argument " + raw + " is not a plain path"
	}
	if filepath.IsAbs(cand) {
		return "", "absolute path " + raw + ": refusing rather than guessing whether it resolves inside the repo"
	}

	if !looksLikeRepoPath(repoRoot, cwd, cand) {
		// Not a path by any test we have. Before ignoring it, make sure it is
		// not the bare name of something that changed — `cat notes.txt` with
		// notes.txt deleted has no file on disk to find, and ignoring it would
		// promote a command whose output is now an error message.
		if c := collidesWithChanged(cand, changed); c != "" {
			return "", "argument " + raw + " may name changed path " + c + ", and we cannot tell whether it is a path or a pattern"
		}
		return "", ""
	}

	rel := path.Join(cwd, filepath.ToSlash(cand))
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", "argument " + raw + " escapes the repo root"
	}
	if rel == "." {
		return "", "argument " + raw + " scopes the whole tree"
	}
	// It looks like a path, so it must name something in one of the two trees:
	// on disk covers the current tree, and the changed set covers whatever the
	// peer deleted. Naming nothing in either means our reading of the command
	// is wrong — most often because it did not run at the repo root — and an
	// unverified reading must not be allowed to gate correctness.
	if !existsUnder(repoRoot, rel) && !scopeTouchesChanged(rel, changed) {
		return "", "argument " + raw + " names nothing in either tree; the command was probably not run at the repo root"
	}
	return rel, ""
}

// looksLikeRepoPath is the "is this a path at all" test. A bare word in
// `grep TODO src/` is a pattern, not a path, and scoping on it would be
// meaningless; a bare word in `cat Makefile` is a path, and ignoring it would
// be unsafe. Containing a slash, existing on disk, or carrying a file
// extension we recognize each settle it.
//
// The extension list is deliberately generous. Over-recognizing only adds
// paths to the scope, which can block a promotion but never cause one.
func looksLikeRepoPath(repoRoot, cwd, cand string) bool {
	if cand == "" {
		return false
	}
	if strings.Contains(cand, "/") {
		return true
	}
	if scopePathExts[strings.ToLower(path.Ext(cand))] {
		return true
	}
	return existsUnder(repoRoot, path.Join(cwd, cand))
}

// stripNodeID turns a pytest node id into the file it selects:
// tests/test_x.py::TestClass::test_method scopes to tests/test_x.py. The
// selector narrows what runs, so the file is the widest thing it can read and
// therefore the safe scope.
func stripNodeID(s string) string {
	if i := strings.Index(s, "::"); i >= 0 {
		return s[:i]
	}
	return s
}

// scopePathsConflict reports whether a changed path can affect a scope path.
// They conflict when either contains the other, so a change inside a scoped
// directory conflicts and a scoped file inside a changed directory conflicts.
//
// Comparison is on path segments. The classic bug here is strings.HasPrefix:
// it reports that src2/x.py starts with src and invents a conflict that does
// not exist, which costs every hit in a repo with a src2.
func scopePathsConflict(scope, changed string) bool {
	s := path.Clean(filepath.ToSlash(scope))
	c := path.Clean(filepath.ToSlash(changed))
	if s == "." || c == "." {
		return true
	}
	if s == c {
		return true
	}
	return strings.HasPrefix(c, s+"/") || strings.HasPrefix(s, c+"/")
}

func scopeTouchesChanged(rel string, changed []string) bool {
	for _, c := range changed {
		if scopePathsConflict(rel, c) {
			return true
		}
	}
	return false
}

// collidesWithChanged reports whether a token we were about to dismiss as
// "not a path" happens to name a changed path.
func collidesWithChanged(cand string, changed []string) string {
	c := path.Clean(filepath.ToSlash(cand))
	for _, ch := range changed {
		if ch == c || path.Base(ch) == c {
			return ch
		}
	}
	return ""
}

func existsUnder(repoRoot, rel string) bool {
	if rel == "" || rel == "." {
		return true
	}
	_, err := os.Lstat(filepath.Join(repoRoot, filepath.FromSlash(rel)))
	return err == nil
}

// scopeCleanCwd normalizes a repo-relative working directory. "" and "." both
// mean the repo root; anything that climbs out of it is rejected.
func scopeCleanCwd(cwdRel string) (string, bool) {
	c := strings.TrimSpace(cwdRel)
	if c == "" {
		return "", true
	}
	if filepath.IsAbs(c) {
		return "", false
	}
	c = path.Clean(filepath.ToSlash(c))
	if c == "." {
		return "", true
	}
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	return c, true
}

// isTreeName guards the two strings we hand to git as revisions. A caller that
// passed through a value beginning with "-" would be handing git an option.
func isTreeName(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// firstGloballyRelevant reports a changed path that configures the toolchain
// itself, which no command-line argument can be disjoint from.
//
// This is what keeps the literal-argument rule honest for the cases where it
// is most obviously too narrow. `rg TODO src/` reads .gitignore to decide what
// to walk; `pytest tests/test_billing.py` imports every conftest.py above it;
// a changed pyproject.toml re-points the whole run. None of those files are
// inside the scope, so segment comparison would call them disjoint. They are
// not, and the cheapest correct answer is to refuse whenever one of them moved.
func firstGloballyRelevant(changed []string) string {
	for _, p := range changed {
		b := path.Base(p)
		if scopeGlobalFiles[b] {
			return p
		}
		for _, pre := range scopeGlobalPrefixes {
			if strings.HasPrefix(b, pre) {
				return p
			}
		}
	}
	return ""
}

// scopeRefuseHeads are commands whose output depends on the whole tree no
// matter what path you point them at. A path argument bounds nothing here, so
// there is no scope to compute and the honest answer is to refuse.
//
// This is not the same limitation as an import graph. `pytest tests/x.py`
// reads more than tests/x.py too, but its arguments at least bound what it
// runs; that residual gap is what Tier 2 observed read sets are for. The
// commands below have no such bound at all.
var scopeRefuseHeads = map[string]bool{
	// Report on the whole tree or index whatever you ask them about.
	"git": true, "hg": true, "svn": true, "jj": true,

	// Walk a directory, so their output includes files that did not exist
	// when the record was made. `ls` is here for the same reason even though
	// `ls file.py` would be bounded: telling a file from a directory needs
	// disk state, and the disk state is what moved.
	"find": true, "fd": true, "tree": true, "ls": true, "du": true,
	"rsync": true, "tar": true, "zip": true, "unzip": true,

	// Read a build manifest that can name any path in the tree.
	"make": true, "gmake": true, "cmake": true, "ninja": true, "meson": true,
	"bazel": true, "buck": true, "gradle": true, "mvn": true, "ant": true,
	"rake": true, "just": true, "task": true, "scons": true,

	// Resolve a package or module graph from a manifest rather than from argv,
	// so `go test ./pkg` and `tsc src/a.ts` read far more than they name.
	"go": true, "cargo": true, "npm": true, "yarn": true, "pnpm": true,
	"npx": true, "bun": true, "deno": true, "tsc": true, "tsx": true,
	"mypy": true, "pyright": true,

	// Run a command we cannot see, so we never get to check its head.
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true,
	"eval": true, "exec": true, "source": true,
	"xargs": true, "watch": true, "parallel": true,
}

// scopeStdinFilters read the pipe rather than the tree when they are given no
// path argument, so `pytest tests/a.py | head -5` is still bounded by tests/a.py.
// Honoured only immediately downstream of a pipe: the same head after a `;`
// has no pipe to read and the exemption would be a guess.
var scopeStdinFilters = map[string]bool{
	"head": true, "tail": true, "wc": true, "sort": true, "uniq": true,
	"cut": true, "tr": true, "nl": true, "cat": true, "tee": true, "rev": true,
	"grep": true, "rg": true, "sed": true, "awk": true, "jq": true,
	"column": true, "fold": true, "paste": true, "less": true, "more": true,
}

// scopePathExts decide that a bare token is a path. Generous on purpose: a
// false positive here only adds a scope path, and adding scope paths can only
// block promotions.
var scopePathExts = map[string]bool{
	".py": true, ".pyi": true, ".ipynb": true, ".go": true, ".rs": true,
	".js": true, ".jsx": true, ".mjs": true, ".cjs": true, ".ts": true, ".tsx": true,
	".java": true, ".kt": true, ".rb": true, ".php": true, ".pl": true, ".lua": true,
	".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true, ".cs": true,
	".swift": true, ".scala": true, ".ex": true, ".exs": true, ".erl": true,
	".sh": true, ".bash": true, ".zsh": true, ".sql": true, ".proto": true,
	".json": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true,
	".cfg": true, ".conf": true, ".lock": true, ".env": true,
	".md": true, ".rst": true, ".txt": true, ".csv": true, ".tsv": true,
	".html": true, ".css": true, ".scss": true, ".xml": true,
}

// scopeGlobalFiles configure a toolchain rather than belonging to one part of
// the tree, so a change to any of them blocks every promotion. Matched on the
// base name, which is what makes a nested tests/conftest.py count.
var scopeGlobalFiles = map[string]bool{
	".gitignore": true, ".gitattributes": true, ".ignore": true,
	".rgignore": true, ".npmignore": true, ".dockerignore": true,

	"conftest.py": true, "pytest.ini": true, "tox.ini": true,
	"setup.cfg": true, "setup.py": true, "pyproject.toml": true,
	"requirements.txt": true, "constraints.txt": true,
	"uv.lock": true, "poetry.lock": true, "Pipfile": true, "Pipfile.lock": true,
	"mypy.ini": true, "ruff.toml": true, ".ruff.toml": true, ".flake8": true,
	"sitecustomize.py": true, "usercustomize.py": true,

	"package.json": true, "package-lock.json": true, "yarn.lock": true,
	"pnpm-lock.yaml": true, "bun.lockb": true,
	"tsconfig.json": true, "jsconfig.json": true,

	"Makefile": true, "makefile": true, "GNUmakefile": true, "CMakeLists.txt": true,
	"Cargo.toml": true, "Cargo.lock": true, "go.mod": true, "go.sum": true,
	"Gemfile": true, "Gemfile.lock": true, "build.gradle": true, "pom.xml": true,

	"Dockerfile": true, ".env": true, ".envrc": true,
}

// scopeGlobalPrefixes catch the config families that vary by extension.
var scopeGlobalPrefixes = []string{
	".eslintrc", ".prettierrc", ".babelrc", ".editorconfig",
	"babel.config.", "jest.config.", "vitest.config.", "vite.config.",
	"webpack.config.", "rollup.config.", "eslint.config.",
}

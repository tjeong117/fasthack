package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tjeong117/fasthack/internal/hp"
)

// slowTreeHash is where a tree hash stops being free. The hook pays for it
// twice per intercepted command, so past this point the cache can plausibly
// cost more than it deletes.
const slowTreeHash = 200 * time.Millisecond

const (
	healthTimeout  = 1500 * time.Millisecond
	daemonBootWait = 5 * time.Second
)

type docStatus string

const (
	statusOK   docStatus = "ok"
	statusWarn docStatus = "warn"
	statusFail docStatus = "fail"
)

// docCheck is one diagnostic. Every failing one must name the fix: a report
// that says something is wrong without saying what to do about it is noise.
type docCheck struct {
	Name    string         `json:"name"`
	Status  docStatus      `json:"status"`
	Summary string         `json:"summary"`
	Detail  []string       `json:"detail,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

type docReport struct {
	OK       bool       `json:"ok"`
	Root     string     `json:"root"`
	Failed   int        `json:"failed"`
	Warnings int        `json:"warnings"`
	Checks   []docCheck `json:"checks"`
}

func (r *docReport) add(c docCheck) { r.Checks = append(r.Checks, c) }

func (r *docReport) finish() {
	for _, c := range r.Checks {
		switch c.Status {
		case statusFail:
			r.Failed++
		case statusWarn:
			r.Warnings++
		}
	}
	r.OK = r.Failed == 0
}

func (r *docReport) renderText(w io.Writer) {
	fmt.Fprintf(w, "hindsight doctor  %s\n\n", r.Root)
	for _, c := range r.Checks {
		fmt.Fprintf(w, "%-4s  %-18s  %s\n", strings.ToUpper(string(c.Status)), c.Name, c.Summary)
		for _, d := range c.Detail {
			fmt.Fprintf(w, "%-4s  %-18s  %s\n", "", "", d)
		}
	}
	fmt.Fprintln(w)
	switch {
	case r.Failed > 0:
		fmt.Fprintf(w, "%s, %s. This workspace cannot be cached safely.\n",
			pluralize(r.Failed, "failure"), pluralize(r.Warnings, "warning"))
	case r.Warnings > 0:
		fmt.Fprintf(w, "%s. Cacheable, with the caveats above.\n", pluralize(r.Warnings, "warning"))
	default:
		fmt.Fprintln(w, "All checks passed. This workspace is cacheable.")
	}
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "machine-readable output")
	ensure := fs.Bool("ensure-daemon", false, "start the daemon if it is not already reachable")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	dir := fs.Arg(0)
	if dir == "" {
		dir, _ = os.Getwd()
	}

	rep := &docReport{}
	if ws := rep.checkWorktree(dir); ws != nil {
		rep.Root = ws.Root
		rep.checkTreeHash(ws)
		rep.checkEnvFingerprint(ws)
		rep.checkEcosystemGap(ws)
		rep.checkCacheHome(ws)
		rep.checkHookConfig(ws)
		rep.checkKillSwitch()
		rep.checkClassifier()
		rep.checkDaemon(ws, *ensure)
		rep.checkStore(ws)
		rep.checkGitignore(ws)
	}
	rep.finish()

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		rep.renderText(os.Stdout)
	}
	if rep.Failed > 0 {
		return fmt.Errorf("%s failed", pluralize(rep.Failed, "check"))
	}
	return nil
}

// ---------------------------------------------------------------------------
// 1. Git worktree
// ---------------------------------------------------------------------------

func (r *docReport) checkWorktree(dir string) *hp.Workspace {
	ws, err := hp.NewWorkspace(dir)
	if err != nil {
		abs, _ := filepath.Abs(dir)
		r.Root = abs
		r.add(docCheck{
			Name: "git worktree", Status: statusFail,
			Summary: "not a git worktree: " + abs,
			Detail: []string{
				"Every command is keyed on git's tree hash of the workspace, so",
				"outside a repository there is nothing to key on and nothing can",
				"be cached. The hook passes through here rather than failing.",
				"Fix: run inside a git repository, or `git init` in this directory.",
			},
			Data: map[string]any{"error": err.Error()},
		})
		return nil
	}

	detail := []string{
		"git dir      " + ws.GitDir,
		"side index   " + ws.IndexPath,
	}
	status, summary := statusOK, ws.Root

	if !pathInside(ws.IndexPath, ws.GitDir) {
		status = statusFail
		summary = "side index is outside this worktree's git dir"
		detail = append(detail,
			"The side index must be private to this worktree. Worktrees sharing",
			"one index interleave their `git add -A` calls and produce a tree",
			"that describes no real state, which is a wrong key, which is a",
			"wrong answer.")
	}

	switch {
	case strings.Contains(filepath.ToSlash(ws.GitDir), "/.git/worktrees/"):
		detail = append(detail, "linked worktree; the side index is private to it")
	case filepath.Base(ws.GitDir) == ".git":
		detail = append(detail, "main worktree")
	}

	if v, ok := os.LookupEnv("GIT_DIR"); ok {
		if status == statusOK {
			status = statusWarn
			summary = ws.Root + "  (GIT_DIR is set)"
		}
		detail = append(detail,
			"GIT_DIR="+v+" is exported, which pins every worktree in this shell",
			"to one git dir and therefore to one shared side index. Fan out five",
			"worktrees like this and they will corrupt each other's trees.",
			"Fix: unset GIT_DIR before running agents.")
	}

	r.add(docCheck{Name: "git worktree", Status: status, Summary: summary, Detail: detail,
		Data: map[string]any{"root": ws.Root, "git_dir": ws.GitDir, "index_path": ws.IndexPath}})
	return ws
}

// ---------------------------------------------------------------------------
// 2. Tree hash: does it work, and what does it cost
// ---------------------------------------------------------------------------

func (r *docReport) checkTreeHash(ws *hp.Workspace) {
	// Timed twice. The first call may have to build the side index from
	// nothing, which costs an order of magnitude more than every call after
	// it; quoting that number would warn about a cost the hook never pays.
	start := time.Now()
	cold, err := ws.TreeHash()
	coldMS := time.Since(start).Milliseconds()
	if err != nil {
		r.add(docCheck{
			Name: "tree hash", Status: statusFail,
			Summary: "failed after " + fmt.Sprint(coldMS) + "ms: " + err.Error(),
			Detail: append(treeHashFailureHelp(ws, err),
				"Without a tree hash there is no key, so nothing is cached or served.",
				"The hook passes through, so commands still run normally."),
			Data: map[string]any{"error": err.Error(), "cold_ms": coldMS},
		})
		return
	}
	start = time.Now()
	warm, err := ws.TreeHash()
	warmMS := time.Since(start).Milliseconds()
	if err != nil {
		r.add(docCheck{Name: "tree hash", Status: statusFail,
			Summary: "second git write-tree failed: " + err.Error()})
		return
	}

	files := indexFileCount(ws)
	counted := pluralize(files, "file")
	if files < 0 {
		counted = "file count unavailable"
	}
	status := statusOK
	summary := fmt.Sprintf("%s  %dms warm (%dms cold), %s", cold[:12], warmMS, coldMS, counted)
	var detail []string

	if warm != cold {
		status = statusFail
		summary = "two consecutive tree hashes disagree"
		detail = append(detail,
			fmt.Sprintf("%s then %s, with nothing run in between.", cold[:12], warm[:12]),
			"Something is writing into the worktree — a watcher, a build daemon,",
			"or a cache home inside the tree. Nothing will ever be servable while",
			"that is true, because the state after a command never matches before.")
	} else if warmMS > slowTreeHash.Milliseconds() {
		status = statusWarn
		detail = append(detail,
			fmt.Sprintf("Above the %dms line. The hook hashes the workspace before and",
				slowTreeHash.Milliseconds()),
			fmt.Sprintf("after every command, so this repo pays about %dms per command", 2*warmMS),
			"whether it hits or not. That is only worth paying if the commands",
			"being cached are slower than it.",
			"Fix: gitignore large generated directories so they leave the index.")
	}

	r.add(docCheck{Name: "tree hash", Status: status, Summary: summary, Detail: detail,
		Data: map[string]any{"tree": cold, "warm_ms": warmMS, "cold_ms": coldMS, "files": files}})
}

// treeHashFailureHelp turns a git failure into something a reader can act on.
//
// The case worth naming is a stale lock. git takes an exclusive lock on the
// index, and a `git add -A` killed partway through leaves the lock file
// behind. Nothing removes it, and the retry inside TreeHash gives up after a
// few hundred milliseconds, so one killed run wedges the worktree until
// somebody deletes the file by hand. On a large repo the first, cold index
// build is slow enough to be killed by git's timeout, which makes this
// reachable rather than theoretical.
func treeHashFailureHelp(ws *hp.Workspace, err error) []string {
	lock := ws.IndexPath + ".lock"
	if fi, statErr := os.Stat(lock); statErr == nil {
		return []string{
			fmt.Sprintf("A stale index lock is present, last written %s ago:",
				time.Since(fi.ModTime()).Round(time.Second)),
			"  " + lock,
			"Every tree hash will fail while it exists, and nothing clears it.",
			"Fix: rm " + lock,
		}
	}
	if strings.Contains(err.Error(), "killed") || strings.Contains(err.Error(), "deadline") {
		return []string{
			"git was killed rather than failing, which means it ran past the",
			"timeout. The first tree hash has to build the index from nothing and",
			"is far slower than every one after it, so very large working trees",
			"can fail here and succeed on a second run.",
			"Fix: rerun, and gitignore generated directories to shrink the index.",
		}
	}
	return []string{"Fix: check that `git add -A` succeeds here and that the git dir is writable."}
}

// indexFileCount counts what actually enters the tree hash, which is the side
// index after `git add -A` — tracked and untracked-but-not-ignored alike, not
// just what is committed.
func indexFileCount(ws *hp.Workspace) int {
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = ws.Root
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+ws.IndexPath)
	out, err := cmd.Output()
	if err != nil {
		return -1
	}
	n := 0
	for _, b := range out {
		if b == 0 {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// 3. Environment fingerprint
// ---------------------------------------------------------------------------

func (r *docReport) checkEnvFingerprint(ws *hp.Workspace) {
	// Timed on its own rather than through State(), because the number worth
	// knowing is what the fingerprint costs on top of the tree hash. It is
	// meant to be a readdir, not a call into the package manager.
	start := time.Now()
	fp, complete := ws.EnvFingerprint()
	elapsed := time.Since(start).Milliseconds()

	detected := ws.Ecosystems()
	shown := "none detected"
	if len(detected) > 0 {
		shown = strings.Join(detected, ", ")
	}
	detail := []string{
		"detected     " + shown,
		"registered   " + strings.Join(hp.EcosystemNames(), ", "),
	}

	if !complete {
		// Ask again with diagnostics on, purely to name the ecosystem that
		// failed. Costs a readdir, and only on the path that already failed.
		reasons := captureDebug(func() { ws.EnvFingerprint() })
		detail = append(detail,
			"An ecosystem is in use here but its installed dependencies could",
			"not be read, so the key does not cover the environment. Nothing",
			"will be served in this workspace — the hook refuses rather than",
			"matching a workspace whose dependencies it failed to establish.")
		detail = append(detail, reasons...)
		detail = append(detail, "Fix: install the dependencies, or add the lockfile that pins them.")

		r.add(docCheck{
			Name: "env fingerprint", Status: statusFail,
			Summary: fp + "  incomplete",
			Detail:  detail,
			Data: map[string]any{
				"env_fp": fp, "complete": false, "ecosystems": detected, "reasons": reasons,
			},
		})
		return
	}

	r.add(docCheck{Name: "env fingerprint", Status: statusOK,
		Summary: fmt.Sprintf("%s  complete, %dms", fp, elapsed),
		Detail:  detail,
		Data:    map[string]any{"env_fp": fp, "complete": true, "ecosystems": detected}})
}

// ---------------------------------------------------------------------------
// 4. Ecosystem coverage gap
// ---------------------------------------------------------------------------

// manifestEcosystems maps a root manifest to the ecosystem that must be
// fingerprinting it. A manifest with no matching registered ecosystem is the
// one failure mode that produces a wrong answer rather than a miss.
var manifestEcosystems = []struct{ file, eco string }{
	{"package.json", "node"},
	{"go.mod", "go"},
	{"Cargo.toml", "rust"},
	{"Gemfile", "ruby"},
	{"pom.xml", "jvm"},
	{"build.gradle", "jvm"},
	{"build.gradle.kts", "jvm"},
	{"composer.json", "php"},
	{"mix.exs", "elixir"},
	{"pubspec.yaml", "dart"},
	{"pyproject.toml", "python"},
	{"requirements.txt", "python"},
}

func (r *docReport) checkEcosystemGap(ws *hp.Workspace) {
	registered := map[string]bool{}
	for _, n := range hp.EcosystemNames() {
		registered[n] = true
	}
	detected := map[string]bool{}
	for _, n := range ws.Ecosystems() {
		detected[n] = true
	}

	var unregistered, undetected, covered []string
	seen := map[string]bool{}
	for _, m := range manifestEcosystems {
		if _, err := os.Stat(filepath.Join(ws.Root, m.file)); err != nil {
			continue
		}
		switch {
		case !registered[m.eco]:
			unregistered = append(unregistered, m.file+" -> "+m.eco)
		case !detected[m.eco]:
			undetected = append(undetected, m.file+" -> "+m.eco)
		case !seen[m.eco]:
			seen[m.eco] = true
			covered = append(covered, m.file+" -> "+m.eco)
		}
	}

	if len(unregistered) == 0 && len(undetected) == 0 {
		summary := "no unrecognized manifests"
		if len(covered) > 0 {
			summary = strings.Join(covered, ", ")
		}
		r.add(docCheck{Name: "ecosystem coverage", Status: statusOK, Summary: summary,
			Data: map[string]any{"covered": covered}})
		return
	}

	detail := []string{
		"The tree hash cannot see installed dependencies — every ecosystem",
		"hides them in a gitignored directory — so an ecosystem nobody",
		"fingerprints means two workspaces with different dependency sets",
		"share a cache key. That serves one environment's output into",
		"another. This is a wrong answer, not a missed hit.",
	}
	for _, u := range unregistered {
		detail = append(detail, "no registered ecosystem:    "+u)
	}
	for _, u := range undetected {
		detail = append(detail, "registered but not matched: "+u)
	}
	detail = append(detail,
		"Fix: do not run agents with the cache armed in this workspace until",
		"an ecosystem covers it, or move the manifest out of the repo root.")

	r.add(docCheck{
		Name: "ecosystem coverage", Status: statusFail,
		Summary: fmt.Sprintf("%s not covered by any ecosystem",
			pluralize(len(unregistered)+len(undetected), "manifest")),
		Detail: detail,
		Data:   map[string]any{"unregistered": unregistered, "undetected": undetected},
	})
}

// ---------------------------------------------------------------------------
// 5. Cache home outside the worktree
// ---------------------------------------------------------------------------

func (r *docReport) checkCacheHome(ws *hp.Workspace) {
	home := hp.Home(ws.Root)
	if pathInside(home, ws.Root) {
		r.add(docCheck{
			Name: "cache home", Status: statusFail,
			Summary: home + " is inside the worktree",
			Detail: []string{
				"Blobs and the log written inside the tree change the tree hash",
				"that keys the cache. Every write invalidates every key, including",
				"the ones it just created, so the cache poisons itself.",
				"Fix: unset HP_HOME, or point it somewhere outside " + ws.Root + ".",
			},
			Data: map[string]any{"home": home, "inside": true},
		})
		return
	}
	source := "default"
	if os.Getenv("HP_HOME") != "" {
		source = "HP_HOME"
	}
	r.add(docCheck{Name: "cache home", Status: statusOK,
		Summary: home + "  (" + source + ")",
		Data:    map[string]any{"home": home, "inside": false}})
}

// ---------------------------------------------------------------------------
// 6. Hook config
// ---------------------------------------------------------------------------

// hookFile is the subset of both harness configs that matters here. Claude and
// Codex disagree about a great deal, but not about this shape.
type hookFile struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Timeout *float64 `json:"timeout"`
		} `json:"hooks"`
	} `json:"hooks"`
}

func (r *docReport) checkHookConfig(ws *hp.Workspace) {
	for _, cfg := range []struct{ name, rel string }{
		{"codex hook", filepath.Join(".codex", "hooks.json")},
		{"claude hook", filepath.Join(".claude", "settings.json")},
	} {
		status, summary, detail := inspectHookConfig(filepath.Join(ws.Root, cfg.rel))
		r.add(docCheck{Name: cfg.name, Status: status, Summary: cfg.rel + "  " + summary,
			Detail: detail, Data: map[string]any{"path": cfg.rel}})
	}
}

func inspectHookConfig(path string) (docStatus, string, []string) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return statusWarn, "not installed", []string{
			"No hook is registered, so no command is intercepted and nothing is",
			"cached. This is inert, not broken.",
			"Fix: run `hindsight init`.",
		}
	}
	if err != nil {
		return statusFail, "unreadable: " + err.Error(), nil
	}
	var cfg hookFile
	if err := json.Unmarshal(b, &cfg); err != nil {
		if !json.Valid(b) {
			return statusFail, "is not valid JSON", []string{
				"The harness will reject the whole file, so every hook in it is dead.",
				"Fix: repair the file, or run `hindsight init --force` to rewrite it.",
			}
		}
		return statusWarn, "has an unexpected shape", []string{
			"Valid JSON, but not the PreToolUse layout this checks for.",
			"Fix: run `hindsight init` and diff the result.",
		}
	}

	var detail []string
	found := false
	missingTimeout := false
	var binErr error
	for _, entry := range cfg.Hooks["PreToolUse"] {
		for _, h := range entry.Hooks {
			if !isHindsightCommand(h.Command) {
				continue
			}
			found = true
			bin, err := resolveHookBinary(h.Command)
			if err != nil {
				binErr = err
			} else {
				detail = append(detail, "binary       "+bin)
			}
			if h.Timeout == nil {
				missingTimeout = true
			} else {
				detail = append(detail, fmt.Sprintf("timeout      %gs", *h.Timeout))
			}
			if entry.Matcher != "" {
				detail = append(detail, "matcher      "+entry.Matcher)
			}
		}
	}

	switch {
	case !found:
		return statusWarn, "has no hindsight PreToolUse entry", []string{
			"The file exists but nothing in it calls hindsight, so no command is",
			"intercepted.",
			"Fix: run `hindsight init`.",
		}
	case binErr != nil:
		return statusFail, "points at a binary that will not run", append(detail,
			binErr.Error(),
			"Hooks fail open, so this does not break the agent — it silently",
			"caches nothing, which is worse because it looks like it is working.",
			"Fix: rebuild with `go build ./cmd/hindsight` and rerun `hindsight init`.")
	case missingTimeout:
		return statusWarn, "installed, no explicit timeout", append(detail,
			"Codex's default hook timeout is 600 seconds. A stalled daemon would",
			"hang every tool call for ten minutes before failing open.",
			"Fix: rerun `hindsight init`, which sets it explicitly.")
	}
	return statusOK, "installed", detail
}

// isHindsightCommand recognises our own hook entry in a config file.
//
// Matching on the binary's base name alone is too strict: `hindsight init`
// writes whatever `os.Executable()` returns, so a binary built to
// ./hindsight-bin, or installed as hindsight-v2, writes an entry doctor would
// then report as missing — telling you to run the init you just ran. Match on
// the `hook` subcommand as well, which is the part that is actually ours.
func isHindsightCommand(cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	base := filepath.Base(fields[0])
	if base == "hindsight" {
		return true
	}
	return strings.HasPrefix(base, "hindsight") &&
		len(fields) > 1 && fields[1] == "hook"
}

func resolveHookBinary(cmd string) (string, error) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "", errors.New("the hook command is empty")
	}
	bin := fields[0]
	if !strings.ContainsRune(bin, os.PathSeparator) {
		found, err := exec.LookPath(bin)
		if err != nil {
			return bin, fmt.Errorf("%s is not on PATH", bin)
		}
		bin = found
	}
	fi, err := os.Stat(bin)
	if err != nil {
		return bin, fmt.Errorf("%s does not exist", bin)
	}
	if fi.IsDir() || fi.Mode()&0o111 == 0 {
		return bin, fmt.Errorf("%s is not executable", bin)
	}
	return bin, nil
}

// ---------------------------------------------------------------------------
// 7. Kill switch
// ---------------------------------------------------------------------------

func (r *docReport) checkKillSwitch() {
	enabled := hp.Enabled()
	detail := []string{"agent id     " + hp.AgentID()}
	summary := "HP_ENABLE unset: the hook is inert"

	if enabled {
		summary = "HP_ENABLE=1: the hook is armed"
		detail = append(detail,
			"Commands run in this shell will be intercepted, keyed, and served",
			"from the cache where a peer already ran them.")
		if !hp.ServeEnabled() {
			detail = append(detail, "HP_SERVE=0: baseline arm — everything is recorded, nothing is served")
		}
	} else {
		detail = append(detail,
			"Nothing is intercepted, keyed, or served. This is the correct default",
			"for a development shell: this repo installs a hook into its own config,",
			"so an armed hook would intercept the session used to work on the hook.",
			"scripts/fleet.sh sets HP_ENABLE; you should not export it by hand.")
	}

	r.add(docCheck{Name: "kill switch", Status: statusOK, Summary: summary, Detail: detail,
		Data: map[string]any{"enabled": enabled, "serve": hp.ServeEnabled(), "agent": hp.AgentID()}})
}

// ---------------------------------------------------------------------------
// Classifier wiring
// ---------------------------------------------------------------------------

// checkClassifier confirms the policy table is real. A classifier that returns
// PASSTHROUGH for everything is a valid, safe, completely useless cache, and
// it looks identical to a working one from the outside.
func (r *docReport) checkClassifier() {
	servePolicy, _ := hp.Classify("grep -rn TODO .")
	denyPolicy, denyReason := hp.Classify("curl https://example.com")

	if denyPolicy != hp.PASSTHROUGH {
		r.add(docCheck{Name: "classifier", Status: statusFail,
			Summary: "curl classified as " + denyPolicy.String(),
			Detail: []string{
				"Non-hermetic commands must never be servable: they print something",
				"different on two runs at an identical workspace state, and state",
				"hashing is blind to that.",
				"Fix: this is a bug in internal/hp/policy.go, not a configuration problem.",
			},
			Data: map[string]any{"curl_policy": denyPolicy.String()}})
		return
	}
	if servePolicy != hp.SERVE {
		r.add(docCheck{Name: "classifier", Status: statusWarn,
			Summary: "no command classifies as SERVE",
			Detail: []string{
				"`grep -rn TODO .` came back as " + servePolicy.String() + ". The classifier is",
				"refusing everything, which is safe and caches nothing.",
				"Fix: check internal/hp/policy.go is not still a stub.",
			},
			Data: map[string]any{"grep_policy": servePolicy.String()}})
		return
	}

	r.add(docCheck{Name: "classifier", Status: statusOK,
		Summary: "SERVE for reads, PASSTHROUGH for non-hermetic commands",
		Detail:  []string{"curl         PASSTHROUGH (" + denyReason + ")"},
		Data:    map[string]any{"grep_policy": servePolicy.String(), "curl_policy": denyPolicy.String()}})
}

// ---------------------------------------------------------------------------
// 8. Daemon
// ---------------------------------------------------------------------------

func (r *docReport) checkDaemon(ws *hp.Workspace, ensure bool) {
	base := hp.DaemonURL()
	err := daemonHealthy(base, healthTimeout)
	if err == nil {
		r.add(docCheck{Name: "daemon", Status: statusOK, Summary: base + "  reachable",
			Data: map[string]any{"url": base, "reachable": true}})
		return
	}

	if !ensure {
		r.add(docCheck{
			Name: "daemon", Status: statusWarn, Summary: base + "  unreachable",
			Detail: []string{
				"The hook fails open, so nothing breaks: every command simply",
				"executes normally and nothing is cached or served.",
				"Fix: `hindsight daemon` in another terminal, or rerun this with",
				"--ensure-daemon to start one now.",
			},
			Data: map[string]any{"url": base, "reachable": false, "error": err.Error()},
		})
		return
	}

	started, detail, spawnErr := ensureDaemon(hp.Home(ws.Root), base, daemonBootWait)
	switch {
	case spawnErr != nil:
		r.add(docCheck{Name: "daemon", Status: statusWarn,
			Summary: base + "  could not be started: " + spawnErr.Error(),
			Detail: append(detail,
				"The hook fails open, so commands still run; nothing is cached."),
			Data: map[string]any{"url": base, "reachable": false, "started": false}})
	case started:
		r.add(docCheck{Name: "daemon", Status: statusOK, Summary: base + "  started",
			Detail: detail, Data: map[string]any{"url": base, "reachable": true, "started": true}})
	default:
		r.add(docCheck{Name: "daemon", Status: statusOK, Summary: base + "  reachable",
			Detail: detail, Data: map[string]any{"url": base, "reachable": true, "started": false}})
	}
}

func daemonHealthy(base string, timeout time.Duration) error {
	c := &http.Client{Timeout: timeout}
	resp, err := c.Get(strings.TrimRight(base, "/") + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %s", resp.Status)
	}
	return nil
}

// ensureDaemon starts a detached daemon if one is not already answering.
//
// Idempotent by construction: it re-checks health before spawning and again
// after losing the race to bind, so two doctors running at once leave one
// daemon rather than two or an error.
func ensureDaemon(home, base string, budget time.Duration) (started bool, detail []string, err error) {
	if daemonHealthy(base, healthTimeout) == nil {
		return false, []string{"already reachable; nothing started"}, nil
	}

	self, err := os.Executable()
	if err != nil {
		return false, nil, err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return false, nil, err
	}
	logPath := filepath.Join(home, "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, nil, err
	}
	defer logFile.Close()

	cmd := exec.Command(self, "daemon", "--addr", daemonAddr(base), "--home", home)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Its own process group, so the daemon outlives the shell that started it
	// and does not take a Ctrl-C aimed at this command.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return false, []string{"log          " + logPath}, err
	}

	// Reap in the background. If the spawn loses a bind race the child exits
	// immediately, and without this it would sit as a zombie until we do.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.Now().Add(budget)
	for {
		if daemonHealthy(base, healthTimeout) == nil {
			return true, []string{
				fmt.Sprintf("pid %d, detached", cmd.Process.Pid),
				"log          " + logPath,
			}, nil
		}
		select {
		case werr := <-exited:
			// Almost always a peer that won the race for the port.
			if daemonHealthy(base, healthTimeout) == nil {
				return false, []string{"another daemon already holds " + base}, nil
			}
			return false, []string{"log          " + logPath},
				fmt.Errorf("the daemon exited immediately (%v)", werr)
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return false, []string{
				fmt.Sprintf("pid %d is running but did not answer /healthz in %s", cmd.Process.Pid, budget),
				"log          " + logPath,
			}, errors.New("timed out waiting for /healthz")
		}
	}
}

func daemonAddr(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "127.0.0.1:7777"
	}
	return u.Host
}

// ---------------------------------------------------------------------------
// 9. Cache stats
// ---------------------------------------------------------------------------

func (r *docReport) checkStore(ws *hp.Workspace) {
	home := hp.Home(ws.Root)
	logPath := hp.StorePaths(home).LogPath()
	if _, err := os.Stat(logPath); err != nil {
		r.add(docCheck{Name: "cache", Status: statusOK, Summary: "empty; nothing recorded yet",
			Detail: []string{"log          " + logPath},
			Data:   map[string]any{"records": 0, "servable": 0}})
		return
	}
	store, err := hp.OpenStore(home)
	if err != nil {
		r.add(docCheck{Name: "cache", Status: statusWarn,
			Summary: "could not be read: " + err.Error(),
			Detail:  []string{"log          " + logPath}})
		return
	}
	records, servable := len(store.Records()), len(store.ServableRecords())
	detail := []string{"log          " + logPath}
	if records > 0 && servable == 0 {
		detail = append(detail,
			"Everything recorded here failed the purity gate: the tree hash or the",
			"environment fingerprint moved while the command ran, so none of it",
			"can be replayed. That is the gate working, not a fault.")
	}
	r.add(docCheck{Name: "cache", Status: statusOK,
		Summary: fmt.Sprintf("%d recorded, %d servable", records, servable),
		Detail:  detail,
		Data:    map[string]any{"records": records, "servable": servable, "home": home}})
}

// ---------------------------------------------------------------------------
// 10. .gitignore sanity
// ---------------------------------------------------------------------------

// dependencyDirs are the gitignored-by-convention directories each ecosystem
// installs into.
var dependencyDirs = map[string][]string{
	"python": {".venv"},
	"node":   {"node_modules"},
	"rust":   {"target"},
	"ruby":   {"vendor/bundle"},
	"jvm":    {"target", "build"},
}

func (r *docReport) checkGitignore(ws *hp.Workspace) {
	// Only nag about directories this workspace could plausibly grow: a Go
	// repo does not need to be told about node_modules.
	candidates := []string{}
	seen := map[string]bool{}
	addCandidate := func(d string) {
		if !seen[d] {
			seen[d] = true
			candidates = append(candidates, d)
		}
	}
	for _, eco := range ws.Ecosystems() {
		for _, d := range dependencyDirs[eco] {
			addCandidate(d)
		}
	}
	for _, dirs := range dependencyDirs {
		for _, d := range dirs {
			if fi, err := os.Stat(filepath.Join(ws.Root, d)); err == nil && fi.IsDir() {
				addCandidate(d)
			}
		}
	}

	if len(candidates) == 0 {
		where := "no ecosystem detected"
		if eco := ws.Ecosystems(); len(eco) > 0 {
			where = strings.Join(eco, ", ") + " installs outside the worktree"
		}
		r.add(docCheck{Name: "gitignore", Status: statusOK,
			Summary: "nothing to ignore; " + where})
		return
	}

	var unignored, ignored []string
	tracked := map[string]int{}
	for _, d := range candidates {
		if gitIgnores(ws.Root, d) {
			ignored = append(ignored, d)
			continue
		}
		unignored = append(unignored, d)
		if n := gitTrackedCount(ws.Root, d); n > 0 {
			tracked[d] = n
		}
	}

	if len(unignored) == 0 {
		r.add(docCheck{Name: "gitignore", Status: statusOK,
			Summary: "ignores " + strings.Join(ignored, ", "),
			Data:    map[string]any{"ignored": ignored}})
		return
	}

	detail := []string{
		"Installed dependencies inside the tree churn the tree hash on every",
		"install, so no two agents ever reach the same state and nothing is",
		"shared. They are also covered by the environment fingerprint already,",
		"so tracking them buys nothing and costs the whole cache.",
	}
	for _, d := range unignored {
		if n := tracked[d]; n > 0 {
			detail = append(detail, fmt.Sprintf("%s is not ignored and has %s tracked", d, pluralize(n, "file")))
		}
	}
	detail = append(detail, "Fix: add "+strings.Join(unignored, ", ")+" to .gitignore.")

	r.add(docCheck{Name: "gitignore", Status: statusWarn,
		Summary: "does not ignore " + strings.Join(unignored, ", "),
		Detail:  detail,
		Data:    map[string]any{"unignored": unignored, "tracked": tracked}})
}

// gitIgnores asks git rather than parsing .gitignore, so global excludes,
// .git/info/exclude and nested ignore files all count. The path need not exist.
func gitIgnores(root, rel string) bool {
	cmd := exec.Command("git", "check-ignore", "-q", "--", rel)
	cmd.Dir = root
	return cmd.Run() == nil
}

func gitTrackedCount(root, rel string) int {
	cmd := exec.Command("git", "ls-files", "-z", "--", rel)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, b := range out {
		if b == 0 {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// resolveSymlinkPrefix resolves the longest existing prefix of p. A cache home
// that does not exist yet still has to be compared against a worktree root
// that does, and on macOS /tmp is a symlink to /private/tmp, so comparing the
// unresolved strings gets that comparison wrong in both directions.
func resolveSymlinkPrefix(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	cur, rest := abs, ""
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func pathInside(child, parent string) bool {
	c, p := resolveSymlinkPrefix(child), resolveSymlinkPrefix(parent)
	if c == p {
		return true
	}
	rel, err := filepath.Rel(p, c)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// captureDebug runs fn with hp.Debugf armed and pointed at a pipe, so the
// reason an ecosystem failed lands in this report instead of the reader having
// to rerun the whole command under a different environment. An unactionable
// failure is barely better than no failure at all.
func captureDebug(fn func()) []string {
	pr, pw, err := os.Pipe()
	if err != nil {
		fn()
		return nil
	}
	prevStderr := os.Stderr
	prevDebug, hadDebug := os.LookupEnv("HP_DEBUG")
	os.Stderr = pw
	os.Setenv("HP_DEBUG", "1")

	lines := make(chan []string, 1)
	go func() {
		var got []string
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			got = append(got, strings.TrimPrefix(sc.Text(), "hindsight: "))
		}
		lines <- got
	}()

	fn()

	os.Stderr = prevStderr
	if hadDebug {
		os.Setenv("HP_DEBUG", prevDebug)
	} else {
		os.Unsetenv("HP_DEBUG")
	}
	pw.Close()
	got := <-lines
	pr.Close()
	return got
}

func pluralize(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

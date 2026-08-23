package main

import (
	"bytes"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tjeong117/fasthack/internal/hp"
)

// `hindsight warm` is the executor half of speculative pre-execution.
//
// The prediction lives in the daemon, which knows where every agent is and
// what commands have followed which states. The daemon cannot act on any of
// it, because it has no worktree — it is a log and an index. So a warmer
// process sits beside a real repository, asks /suggest what is worth
// computing, materializes the suggested tree into a scratch worktree, and runs
// the command.
//
// The one thing this program must not do is invent a shortcut. It executes by
// spawning `hindsight record` with exactly the arguments the PreToolUse hook
// spawns it with on a miss, in a real worktree, at a real state. That
// subprocess applies the same purity gate to the result as it would to an
// agent's, and the record it writes is distinguishable from an agent's only by
// the agent id, which the serve path never reads. There is deliberately no
// code path in here that touches the servable index.
//
// The safety argument is therefore structural rather than statistical. If the
// daemon's ranking is nonsense, this program burns CPU on results nobody wants
// and the cache stays exactly as correct as it was.

// warmDefaultInterval is how long to wait between passes. Suggestions change
// only when agents move, which is on the order of seconds at best.
const warmDefaultInterval = 5 * time.Second

// warmDefaultNice keeps the warmer out of the way of real agents. Speculation
// is worth doing with spare cycles and not worth doing with contended ones, and
// the kernel is far better placed to tell the difference than a poller is.
const warmDefaultNice = 10

func cmdWarm(args []string) error {
	fs := flag.NewFlagSet("warm", flag.ContinueOnError)
	daemon := fs.String("daemon", hp.DaemonURL(), "daemon URL")
	interval := fs.Duration("interval", warmDefaultInterval, "delay between passes")
	maxConc := fs.Int("max-concurrent", 1, "warm executions at a time")
	budgetSpec := fs.String("budget", "", "cap as a count (\"20\") or wall clock (\"10m\"); empty is unlimited")
	once := fs.Bool("once", false, "make a single pass and exit")
	limit := fs.Int("limit", 10, "suggestions to request per pass")
	repo := fs.String("repo", ".", "a worktree of the repository to warm")
	home := fs.String("home", "", "cache root (default HP_HOME, else derived from the repo)")
	scratchDir := fs.String("scratch", "", "where scratch worktrees live (default <home>/warm)")
	envLink := fs.String("env-link", "", "comma-separated gitignored directories to symlink from --repo, e.g. \".venv\"")
	niceness := fs.Int("nice", warmDefaultNice, "scheduling priority for this process and everything it spawns")
	yield := fs.Int("yield", -1, "pause a pass while more than N leases are in flight; -1 never pauses")
	timeout := fs.Duration("timeout", 30*time.Minute, "hard timeout for a warmed command")
	dryRun := fs.Bool("dry-run", false, "print the ranked suggestions and exit without executing")
	verbose := fs.Bool("v", false, "explain every decision")
	if err := fs.Parse(args); err != nil {
		return err
	}

	runBudget, timeBudget, err := parseBudget(*budgetSpec)
	if err != nil {
		return err
	}

	ws, err := hp.NewWorkspace(*repo)
	if err != nil {
		return fmt.Errorf("--repo must be a git worktree: %w", err)
	}

	// The blob store the warmer writes into has to be the one the daemon reads
	// out of, or every speculative result is a file nobody can find. Home() is
	// derived from the worktree root, and a warmer beside worktree B would
	// otherwise pick a different root than a daemon started in worktree A.
	// Pin it once, explicitly, and hand it to every child.
	cacheHome := *home
	if cacheHome == "" {
		cacheHome = hp.Home(ws.Root)
	}
	if err := os.Setenv("HP_HOME", cacheHome); err != nil {
		return err
	}

	if err := syscall.Setpriority(syscall.PRIO_PROCESS, 0, *niceness); err != nil && *verbose {
		fmt.Fprintf(os.Stderr, "warm: could not renice to %d: %v\n", *niceness, err)
	}

	w := &warmer{
		daemon: *daemon, repoRoot: ws.Root, home: cacheHome,
		timeout: *timeout, verbose: *verbose,
		refused: map[string]string{}, tally: map[string]int{},
	}
	if w.self, err = os.Executable(); err != nil {
		return err
	}
	if *envLink != "" {
		for _, n := range strings.Split(*envLink, ",") {
			if n = strings.TrimSpace(n); n != "" {
				w.links = append(w.links, n)
			}
		}
	}

	if *dryRun {
		return w.printSuggestions(*limit)
	}

	base := *scratchDir
	if base == "" {
		base = filepath.Join(cacheHome, "warm")
	}
	if *maxConc < 1 {
		*maxConc = 1
	}
	scratches, err := w.openScratches(base, *maxConc)
	if err != nil {
		return err
	}
	defer func() {
		for _, sc := range scratches {
			_ = sc.Remove()
		}
	}()

	b := &budget{runs: runBudget}
	if timeBudget > 0 {
		b.until = time.Now().Add(timeBudget)
	}

	for {
		resp, err := hp.FetchSuggestions(*daemon, *limit)
		if err != nil {
			// An unreachable daemon is not fatal for a process meant to sit
			// running for hours. It is fatal for a single pass, where silence
			// would look like "nothing to warm".
			if *once {
				return err
			}
			fmt.Fprintf(os.Stderr, "warm: %v\n", err)
		} else if *yield >= 0 && resp.Inflight > *yield {
			w.log("yielding: %d leases in flight", resp.Inflight)
		} else {
			w.pass(scratches, resp.Suggestions, b)
		}
		if *once || b.exhausted() {
			break
		}
		time.Sleep(*interval)
	}

	w.report(*daemon)
	return nil
}

type warmer struct {
	self     string
	daemon   string
	repoRoot string
	home     string
	links    []string
	timeout  time.Duration
	verbose  bool

	mu sync.Mutex
	// refused remembers keys we declined and why, so a suggestion the daemon
	// keeps offering — an environment we cannot reproduce, most often — is
	// materialized once rather than every interval forever.
	refused map[string]string
	tally   map[string]int
	ran     int
	spentMS int64
	// worstMaterializeMS is what getting to a state has cost, at worst, in
	// this repository. It is measured rather than configured because it varies
	// by three orders of magnitude between a toy repo and a monorepo.
	worstMaterializeMS int64
}

func (w *warmer) materializeMS() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.worstMaterializeMS
}

func (w *warmer) observeMaterialize(ms int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ms > w.worstMaterializeMS {
		w.worstMaterializeMS = ms
	}
}

func (w *warmer) log(format string, args ...any) {
	if w.verbose {
		fmt.Fprintf(os.Stderr, "warm: "+format+"\n", args...)
	}
}

func (w *warmer) count(reason string) {
	w.mu.Lock()
	w.tally[reason]++
	w.mu.Unlock()
}

func (w *warmer) openScratches(base string, n int) ([]*hp.Scratch, error) {
	var out []*hp.Scratch
	for i := 0; i < n; i++ {
		dir := filepath.Join(base, fmt.Sprintf("w%d-%d", os.Getpid(), i))
		_ = os.RemoveAll(dir)
		sc, err := hp.NewScratch(w.repoRoot, dir)
		if err != nil {
			for _, prev := range out {
				_ = prev.Remove()
			}
			return nil, err
		}
		for _, name := range w.links {
			if err := sc.Link(w.repoRoot, name); err != nil {
				fmt.Fprintf(os.Stderr, "warm: could not link %s: %v\n", name, err)
			}
		}
		out = append(out, sc)
	}
	return out, nil
}

// pass fans the current suggestion list across the scratch worktrees, in rank
// order, until the budget runs out.
func (w *warmer) pass(scratches []*hp.Scratch, sugs []hp.Suggestion, b *budget) {
	queue := make(chan hp.Suggestion)
	var wg sync.WaitGroup
	for _, sc := range scratches {
		wg.Add(1)
		go func(sc *hp.Scratch) {
			defer wg.Done()
			for sug := range queue {
				w.warmOne(sc, sug)
			}
		}(sc)
	}
	for _, sug := range sugs {
		w.mu.Lock()
		why, seen := w.refused[sug.Key]
		w.mu.Unlock()
		if seen {
			w.log("still refusing %s: %s", short(sug.Key), why)
			continue
		}
		if !b.take() {
			break
		}
		queue <- sug
	}
	close(queue)
	wg.Wait()
}

// warmOne is the whole of speculative execution: get to the state, prove it is
// the state that was asked for, then hand the command to the ordinary record
// path and let it decide what the result is worth.
func (w *warmer) warmOne(sc *hp.Scratch, sug hp.Suggestion) {
	// The classifier is a pure function of the command string and it is cheap,
	// so there is no reason to take the daemon's word for it. If the two ever
	// disagree — different binaries, mid-deploy — the local answer is the one
	// that governs, and the strict direction is to do nothing.
	policy, reason := hp.Classify(sug.Cmd)
	if policy != hp.SERVE {
		w.refuse(sug, "policy is "+policy.String()+", not SERVE")
		return
	}

	// Reaching a state is not free, and on a large repository it is not close
	// to free: checkout-index rewrites every file, measured at 1.6s on 5,000
	// of them. That cost is paid with certainty, while the saving is only
	// expected, so a candidate whose expected saving cannot even cover the
	// setup is one this program should decline however slow the command is.
	//
	// The estimate is the worst materialization seen so far rather than the
	// average, because the direction to err in when spending CPU on a guess is
	// downwards.
	if worst := w.materializeMS(); worst > 0 && sug.ExpectedMS < float64(worst) {
		w.refuse(sug, fmt.Sprintf("expected saving %.0fms does not cover the ~%dms this repository costs to materialize",
			sug.ExpectedMS, worst))
		return
	}

	checkoutStart := time.Now()
	if err := sc.Checkout(sug.Tree); err != nil {
		w.refuse(sug, "could not materialize the tree: "+err.Error())
		return
	}
	w.observeMaterialize(time.Since(checkoutStart).Milliseconds())

	ws, err := hp.NewWorkspace(sc.Dir)
	if err != nil {
		w.refuse(sug, "scratch is not a worktree: "+err.Error())
		return
	}
	state, err := ws.State()
	if err != nil {
		w.refuse(sug, "could not hash the scratch: "+err.Error())
		return
	}

	// The preflight. A speculative result is only worth anything if its key is
	// the key an agent will later compute, and the key is dominated by these
	// two hashes. Getting either wrong does not produce a wrong answer — the
	// record simply lands under a key nobody asks for — but it does mean
	// paying for a test suite to fill a slot in the index that will never be
	// read, which is the exact failure mode this feature is accused of.
	if state.Tree != sug.Tree {
		w.refuse(sug, "materialized tree is "+short(state.Tree)+", asked for "+short(sug.Tree))
		return
	}
	if !state.EnvComplete {
		w.refuse(sug, "an ecosystem is in use here and could not be read")
		return
	}
	if state.EnvFP != sug.EnvFP {
		// Overwhelmingly the common refusal, and the honest one: a scratch
		// worktree has no virtualenv and no node_modules, because those are
		// gitignored and a tree object cannot carry them. --env-link is the
		// only way through, and it is opt-in because it shares a live agent's
		// dependency directory rather than copying it.
		w.refuse(sug, "environment fingerprint is "+short(state.EnvFP)+
			", agents are at "+short(sug.EnvFP)+"; nothing here would match their key")
		return
	}

	cwdRel := sug.CwdRel
	if cwdRel == "" {
		cwdRel = "."
	}
	key := hp.Key(state, cwdRel, hp.NormalizeCommand(sug.Cmd))
	if key != sug.Key {
		w.refuse(sug, "key disagrees with the daemon's; refusing rather than filling a slot nobody reads")
		return
	}

	dir := filepath.Join(sc.Dir, cwdRel)
	if _, err := os.Stat(dir); err != nil {
		w.refuse(sug, "no "+cwdRel+" in this tree")
		return
	}

	// The command is about to write, so the scratch no longer holds a state we
	// can name.
	sc.Dirty()
	started := time.Now()
	exit, stderr := w.record(dir, key, state, cwdRel, sug.Cmd, reason)
	took := time.Since(started).Milliseconds()

	w.mu.Lock()
	w.ran++
	w.spentMS += took
	w.mu.Unlock()
	w.count("executed")
	w.log("ran %s at %s in %dms (exit %d), expected saving %.0fms",
		sug.CmdNorm, short(sug.Tree), took, exit, sug.ExpectedMS)
	if exit == 127 && stderr != "" {
		fmt.Fprintf(os.Stderr, "warm: record failed to start: %s\n", stderr)
	}
}

// record spawns `hindsight record` with the argument list the PreToolUse hook
// builds on a miss.
//
// This is the load-bearing line of the whole feature. Nothing about a
// speculative execution is special, so nothing about how it is executed is
// special either: the same binary, the same flags, the same base64 envelope,
// the same purity gate on the far side. The only difference is HP_AGENT, and
// the only thing that reads HP_AGENT is provenance and accounting.
func (w *warmer) record(dir, key string, state hp.State, cwdRel, command, reason string) (int, string) {
	cmd := exec.Command(w.self, "record",
		"--key", key,
		"--tree", state.Tree,
		"--envfp", state.EnvFP,
		"--policy", hp.SERVE.String(),
		"--reason", reason,
		"--cwdrel", cwdRel,
		"--timeout", w.timeout.String(),
		"--b64", base64.StdEncoding.EncodeToString([]byte(command)))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HP_AGENT="+hp.SpeculatorAgent,
		"HP_HOME="+w.home,
		"HP_DAEMON="+w.daemon)
	// The command's own output is captured by the child for the blob store, so
	// there is nothing to gain from replaying it here; a warmer that printed
	// every suite it ran would be unusable.
	cmd.Stdout = nil
	var errb bytes.Buffer
	cmd.Stderr = &errb
	err := cmd.Run()
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		exit = 127
	}
	return exit, strings.TrimSpace(errb.String())
}

func (w *warmer) refuse(sug hp.Suggestion, why string) {
	w.mu.Lock()
	w.refused[sug.Key] = why
	w.mu.Unlock()
	w.count(why)
	w.log("refused %s (%s): %s", short(sug.Key), sug.CmdNorm, why)
}

func (w *warmer) printSuggestions(limit int) error {
	resp, err := hp.FetchSuggestions(w.daemon, limit)
	if err != nil {
		return err
	}
	if len(resp.Suggestions) == 0 {
		fmt.Println("nothing worth warming: no command has been seen often enough at a state anybody is near")
		return nil
	}
	for i, s := range resp.Suggestions {
		fmt.Printf("%2d. %-40s tree=%s p=%.2f dur=%dms expected=%.0fms\n",
			i+1, truncate(s.CmdNorm, 40), short(s.Tree), s.P, s.DurationMS, s.ExpectedMS)
		fmt.Printf("    %s\n", s.Reason)
	}
	return nil
}

// report prints what speculation cost and what it bought.
//
// The interesting number is the hit rate, and it is deliberately printed even
// when it is zero, because a feature that spends CPU on a guess has to be
// willing to say when the guess was wrong.
func (w *warmer) report(daemon string) {
	w.mu.Lock()
	ran, spent := w.ran, w.spentMS
	tally := make([]string, 0, len(w.tally))
	for k := range w.tally {
		tally = append(tally, k)
	}
	counts := w.tally
	w.mu.Unlock()
	sort.Slice(tally, func(i, j int) bool { return counts[tally[i]] > counts[tally[j]] })

	fmt.Printf("warmed      %d commands in %.1fs\n", ran, float64(spent)/1000)
	if ms := w.materializeMS(); ms > 0 {
		fmt.Printf("  reaching a state costs up to %dms here, paid per state\n", ms)
	}
	for _, reason := range tally {
		if reason == "executed" {
			continue
		}
		fmt.Printf("  refused   %3d  %s\n", counts[reason], reason)
	}
	resp, err := hp.FetchSuggestions(daemon, 1)
	if err != nil {
		return
	}
	s := resp.Spec
	fmt.Printf("speculative results produced   %d (%d servable)\n", s.Produced, s.Servable)
	fmt.Printf("later asked for by an agent    %d\n", s.Used)
	fmt.Printf("speculation hit rate           %.1f%%\n", s.HitRate*100)
	fmt.Printf("cpu spent / execution deleted  %.1fs / %.1fs  (net %+.1fs)\n",
		s.SecondsSpent, s.SecondsDeleted, s.NetSeconds)
	if s.Produced > 0 && s.Used == 0 {
		fmt.Println("nothing speculative has been used yet: on this evidence the warmer is a CPU heater")
	}
}

// budget caps a warmer so it can be left running without becoming a permanent
// background load.
type budget struct {
	mu    sync.Mutex
	runs  int
	used  int
	until time.Time
}

func (b *budget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runs > 0 && b.used >= b.runs {
		return false
	}
	if !b.until.IsZero() && time.Now().After(b.until) {
		return false
	}
	b.used++
	return true
}

func (b *budget) exhausted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runs > 0 && b.used >= b.runs {
		return true
	}
	return !b.until.IsZero() && time.Now().After(b.until)
}

// parseBudget accepts a bare count or a duration. A count has no unit and
// time.ParseDuration rejects it, so the two are unambiguous.
func parseBudget(s string) (runs int, dur time.Duration, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, nil
	}
	if n, e := strconv.Atoi(s); e == nil {
		if n <= 0 {
			return 0, 0, fmt.Errorf("--budget count must be positive")
		}
		return n, 0, nil
	}
	d, e := time.ParseDuration(s)
	if e != nil {
		return 0, 0, fmt.Errorf("--budget %q is neither a count nor a duration", s)
	}
	return 0, d, nil
}

func short(s string) string {
	s = strings.TrimPrefix(s, "hs-v1:")
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/tjeong117/fasthack/internal/hp"
)

// cmdExport turns the cache log into a transition corpus.
//
// Every command the hook intercepts already produces (tree_before, cmd,
// tree_after, exit_code, duration_ms) with the state measured on both sides.
// That is a real state transition, observed rather than predicted, and it
// falls out of caching for free. This command is the difference between that
// being a claim in a design document and being a file someone can read.
//
// It exports only records the environment actually answered. The rule is
// enforced in hp.TransitionFrom, not here, and everything it drops is counted
// and printed, so the exclusion is visible rather than silent.
//
// Exposed as `hindsight transitions`, not `hindsight export`: `hindsight cache
// export` already means something unrelated (moving a warm cache between
// machines), and two commands called export would be a trap.
func cmdExport(args []string) error {
	fs := flag.NewFlagSet("transitions", flag.ContinueOnError)
	home := fs.String("home", "", "cache root (default from HP_HOME or the repo)")
	out := fs.String("out", "", "output file (default stdout)")
	format := fs.String("format", "jsonl", "output format: jsonl|json")
	includeNonMutating := fs.Bool("include-nonmutating", true, "include transitions that changed nothing")
	mutatingOnly := fs.Bool("mutating-only", false, "only transitions that moved the tree or the environment")
	statsOnly := fs.Bool("stats", false, "print a summary to stderr and export nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}

	filter, err := exportFilter(fs, *includeNonMutating, *mutatingOnly)
	if err != nil {
		return err
	}
	if *format != "jsonl" && *format != "json" {
		return fmt.Errorf("export: --format must be jsonl or json, got %q", *format)
	}

	dir := *home
	if dir == "" {
		cwd, _ := os.Getwd()
		root := cwd
		if ws, err := hp.NewWorkspace(cwd); err == nil {
			root = ws.Root
		}
		dir = hp.Home(root)
	}
	// Absolute, because the header is the corpus's only statement of where it
	// came from and "./cache" says nothing once the file has been moved.
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	logPath := hp.StorePaths(dir).LogPath()

	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			// An empty cache is a real answer, not a failure: it means nothing
			// has been recorded here yet. Say which path was empty, because
			// the usual cause is the wrong --home.
			return fmt.Errorf("export: no cache log at %s (nothing has been recorded for this repo yet)", logPath)
		}
		return err
	}
	defer f.Close()

	transitions, stats, err := hp.ScanTransitions(bufio.NewReaderSize(f, 1<<20), filter)
	if err != nil {
		return err
	}

	if *statsOnly {
		printExportStats(os.Stderr, dir, logPath, filter, stats)
		return nil
	}

	w, done, err := exportWriter(*out)
	if err != nil {
		return err
	}
	if err := writeExport(w, *format, dir, logPath, filter, stats, transitions); err != nil {
		done(false)
		return err
	}
	if err := done(true); err != nil {
		return err
	}

	// The summary goes to stderr unconditionally so that piping the corpus
	// somewhere does not hide what was left out of it.
	printExportStats(os.Stderr, dir, logPath, filter, stats)
	return nil
}

// exportFilter resolves the two filter flags, which overlap by design:
// --mutating-only is the shorthand, --include-nonmutating=false is the long
// form. Asking for both populations and then only one of them is a mistake
// worth refusing rather than resolving by precedence.
func exportFilter(fs *flag.FlagSet, includeNonMutating, mutatingOnly bool) (hp.TransitionFilter, error) {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	if mutatingOnly && explicit["include-nonmutating"] && includeNonMutating {
		return 0, fmt.Errorf("export: --mutating-only and --include-nonmutating=true contradict each other")
	}
	if mutatingOnly || !includeNonMutating {
		return hp.FilterMutatingOnly, nil
	}
	return hp.FilterAll, nil
}

// exportWriter returns the sink and a finisher. A file is written to a
// temporary path and renamed on success, so a failed export leaves no
// half-written corpus behind for someone to train on.
func exportWriter(path string) (io.Writer, func(commit bool) error, error) {
	if path == "" {
		bw := bufio.NewWriter(os.Stdout)
		return bw, func(commit bool) error {
			if !commit {
				return nil
			}
			return bw.Flush()
		}, nil
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, err
		}
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return nil, nil, err
	}
	bw := bufio.NewWriter(f)
	return bw, func(commit bool) error {
		if !commit {
			f.Close()
			os.Remove(tmp)
			return nil
		}
		if err := bw.Flush(); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if err := f.Close(); err != nil {
			os.Remove(tmp)
			return err
		}
		return os.Rename(tmp, path)
	}, nil
}

func writeExport(w io.Writer, format, dir, logPath string, filter hp.TransitionFilter, stats hp.ExportStats, transitions []hp.Transition) error {
	if format == "json" {
		doc := hp.ExportDocument{
			Meta: hp.NewExportManifest(dir, logPath,
				time.Now().UTC().Format(time.RFC3339), filter, stats),
			Transitions: transitions,
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
	}
	// JSONL. One transition per line, in log order, and no header — a header
	// line would be a row that is not a transition, which is precisely the
	// kind of thing this corpus is supposed to not contain.
	enc := json.NewEncoder(w)
	for i := range transitions {
		if err := enc.Encode(transitions[i]); err != nil {
			return err
		}
	}
	return nil
}

func printExportStats(w io.Writer, dir, logPath string, filter hp.TransitionFilter, stats hp.ExportStats) {
	fmt.Fprintf(w, "hindsight transitions \u2014 observed state transitions\n\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  cache\t%s\n", dir)
	fmt.Fprintf(tw, "  log\t%s\n", logPath)
	fmt.Fprintf(tw, "  schema\t%s\n", hp.TransitionSchema)
	fmt.Fprintf(tw, "  filter\t%s\n", filter)
	fmt.Fprintf(tw, "  records scanned\t%d\n", stats.Scanned)
	fmt.Fprintf(tw, "  transitions exported\t%d\n", stats.Exported)
	fmt.Fprintf(tw, "  records excluded\t%d\n", stats.ExcludedTotal())
	tw.Flush()

	if len(stats.Excluded) > 0 {
		fmt.Fprintf(w, "\nexcluded, and why \u2014 nothing is dropped without a line here\n\n")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, reason := range stats.Reasons() {
			fmt.Fprintf(tw, "  %d\t%s\n", stats.Excluded[reason], reason)
		}
		tw.Flush()
	}

	if !stats.Balanced() {
		// Unreachable: ScanTransitions refuses to return an unbalanced scan.
		// Printed anyway, because a corpus whose drops do not add up has had
		// something removed silently.
		fmt.Fprintf(w, "\nACCOUNTING ERROR: %d scanned != %d exported + %d excluded\n",
			stats.Scanned, stats.Exported, stats.ExcludedTotal())
	}

	fmt.Fprintf(w, "\nleakage rule (%s)\n  %s\n", hp.LeakageRuleSource, hp.LeakageRule)
	fmt.Fprintf(w, "\nEnforced in code: only decision == MISS is admitted, which is the one path on\n"+
		"which the command really ran and the state was measured on both sides of it.\n")
}

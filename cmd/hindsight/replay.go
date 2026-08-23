package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/tjeong117/fasthack/internal/hp"
)

// cmdReplay answers "what would Hindsight have saved on this recorded trace",
// using the real classifier and the real keying rather than a re-implementation.
//
// That constraint is the whole value of the number. A replay tool that
// reimplements the policy measures the reimplementation, and the gap between
// the two is exactly the thing nobody would notice.
func cmdReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	corpus := fs.String("corpus", "", "corpus directory (required)")
	keying := fs.String("key", "state", "which keying to simulate: state|command")
	asJSON := fs.Bool("json", false, "machine-readable output")
	byStep := fs.Bool("by-step", false, "include the step-index decay table")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *corpus == "" && fs.NArg() == 1 {
		*corpus = fs.Arg(0)
	}
	if *corpus == "" {
		return errors.New("replay: --corpus <dir> is required")
	}
	k, err := hp.ParseKeying(*keying)
	if err != nil {
		return err
	}

	trajs, load, err := hp.LoadCorpusReport(*corpus)
	if err != nil {
		return err
	}
	rep := hp.Replay(trajs, k)
	rep.Load = load
	if !*byStep {
		rep.ByStep = nil
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printReplay(os.Stdout, *corpus, rep)
	return nil
}

func printReplay(w io.Writer, dir string, rep hp.ReplayReport) {
	total := rep.Overall.Commands
	fmt.Fprintf(w, "hindsight replay \u2014 what this recorded trace would have saved\n\n")

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  corpus\t%s\n", dir)
	fmt.Fprintf(tw, "  keyed on\t%s\n", keyingBlurb(rep.Keying))
	fmt.Fprintf(tw, "  records\t%d found, %d loaded, %d skipped%s\n",
		rep.Load.Files, rep.Load.Loaded, rep.Load.Skipped(), skipDetail(rep.Load))
	fmt.Fprintf(tw, "  population\t%d multi-agent tasks, %d attempts, %s commands\n",
		rep.Instances, rep.Attempts, comma(total))
	if len(rep.FanOut) > 0 {
		fmt.Fprintf(tw, "  fan-out\t%s attempts per task\n", fanOutBlurb(rep.FanOut))
	}
	tw.Flush()

	if rep.Instances == 0 {
		fmt.Fprintf(w, "\nNo task in this corpus was attempted by more than one agent, so there is\nnothing to say about sharing between agents.\n")
		return
	}

	fmt.Fprintf(w, "\n  every percentage below is a share of all %s commands\n\n", comma(total))
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  avoidable in principle\t%s\t%s\n", comma(rep.Overall.Avoidable), share(rep.Overall.Avoidable, total))
	fmt.Fprintf(tw, "    self-reuse    one agent repeating itself\t%s\t%s\n", comma(rep.Overall.SelfReuse), share(rep.Overall.SelfReuse, total))
	fmt.Fprintf(tw, "    cross-agent   a peer had already run it\t%s\t%s\n", comma(rep.Overall.CrossAgent), share(rep.Overall.CrossAgent, total))
	fmt.Fprintf(tw, "  avoidable under our policy (SERVE only)\t%s\t%s\n", comma(rep.UnderPolicy.Avoidable), share(rep.UnderPolicy.Avoidable, total))
	fmt.Fprintf(tw, "    of which cross-agent\t%s\t%s\n", comma(rep.UnderPolicy.CrossAgent), share(rep.UnderPolicy.CrossAgent, total))
	tw.Flush()

	fmt.Fprintf(w, "\nby policy \u2014 what the shipping classifier says about each command\n\n")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  policy\tcommands\tof all\tavoidable\treuse rate\tcross-agent\n")
	for _, p := range rep.ByPolicy {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", p.Policy, comma(p.Commands), share(p.Commands, total),
			comma(p.Avoidable), rate(p.ReuseRate()), comma(p.CrossAgent))
	}
	tw.Flush()

	fmt.Fprintf(w, "\nby command class \u2014 whether the avoidable work is cheap reads or expensive suites\n\n")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  class\tcommands\tof all\tavoidable\treuse rate\tcross-agent\tcross rate\n")
	for _, c := range rep.ByClass {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.Class, comma(c.Commands), share(c.Commands, total),
			comma(c.Avoidable), rate(c.ReuseRate()), comma(c.CrossAgent), rate(c.CrossRate()))
	}
	tw.Flush()

	if len(rep.ByStep) > 0 {
		fmt.Fprintf(w, "\ncross-agent reuse by step index \u2014 the opening is the redundant part\n\n")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "  steps\tcommands\tcross-agent\tcross rate\n")
		for _, s := range rep.ByStep {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", s.Steps, comma(s.Commands), comma(s.CrossAgent), rate(s.CrossRate()))
		}
		tw.Flush()
	}

	fmt.Fprintf(w, "\n%s\n", summaryParagraph(rep))
}

// summaryParagraph is the part a reader screenshots, so it restates the
// denominator on every figure and states what the corpus is not.
func summaryParagraph(rep hp.ReplayReport) string {
	total := rep.Overall.Commands
	n := comma(total)
	var b strings.Builder

	fmt.Fprintf(&b, "Summary. Across %d tasks that more than one agent attempted \u2014 %d recorded\n"+
		"attempts, %s commands in total \u2014 %s commands (%s of %s) repeated a command\n"+
		"that had already been run at an identical workspace state, so a %s-keyed cache\n"+
		"could have served them instead of executing them. %s of those (%s of %s) were\n"+
		"an agent repeating itself, and %s (%s of %s) were work a peer had already paid\n"+
		"for. Only that second number is an argument for fanning out.\n",
		rep.Instances, rep.Attempts, n,
		comma(rep.Overall.Avoidable), share(rep.Overall.Avoidable, total), n, rep.Keying,
		comma(rep.Overall.SelfReuse), share(rep.Overall.SelfReuse, total), n,
		comma(rep.Overall.CrossAgent), share(rep.Overall.CrossAgent, total), n)

	fmt.Fprintf(&b, "\nHindsight's shipping classifier is willing to serve %s of the %s commands (%s),\n"+
		"and %s of the avoidable commands fall inside it \u2014 %s of the overlap above.\n"+
		"The rest is real duplicated work that our policy deliberately declines to\n"+
		"serve, so %s of %s, not %s, is what this corpus supports claiming.\n",
		comma(rep.UnderPolicy.Commands), n, share(rep.UnderPolicy.Commands, total),
		comma(rep.UnderPolicy.Avoidable), share(rep.UnderPolicy.Avoidable, rep.Overall.Avoidable),
		share(rep.UnderPolicy.Avoidable, total), n, share(rep.Overall.Avoidable, total))

	fmt.Fprintf(&b, "\nThis corpus is cross-model and starts from a pre-built container. Every task\n"+
		"mixes different models rather than running N copies of one agent, and no\n"+
		"attempt pays for setup, so install commands are nearly absent. Both suppress\n"+
		"overlap, which makes these figures a floor for the homogeneous fan-out case\n"+
		"rather than an estimate of it.\n")

	fmt.Fprintf(&b, "\nNo seconds are reported here, and none can be. The corpus records one wall-clock\n"+
		"figure per whole trajectory and no per-command durations, so any seconds figure\n"+
		"would be a cost model wearing a measurement's clothes. Multiply the counts above\n"+
		"by your own suite time if you want one.\n")
	return b.String()
}

func keyingBlurb(k string) string {
	if k == "command" {
		return "the command string alone \u2014 an upper bound, not what Hindsight can serve"
	}
	return "(workspace state, command) \u2014 how Hindsight actually keys"
}

func skipDetail(l hp.CorpusLoad) string {
	var parts []string
	for _, p := range []struct {
		n   int
		why string
	}{
		{l.Incomplete, "incomplete"},
		{l.NoSteps, "no steps"},
		{l.NoInstance, "no instance id"},
		{l.Malformed, "malformed"},
	} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.why))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// fanOutBlurb lists the distinct widths. The spread matters more than the mean:
// a corpus of 26-wide tasks and a corpus of 3-wide tasks answer different
// questions about sharing.
func fanOutBlurb(widths []int) string {
	seen := map[int]bool{}
	var uniq []int
	for _, w := range widths {
		if !seen[w] {
			seen[w] = true
			uniq = append(uniq, w)
		}
	}
	sort.Ints(uniq)
	strs := make([]string, len(uniq))
	for i, w := range uniq {
		strs[i] = fmt.Sprint(w)
	}
	return strings.Join(strs, ", ")
}

func share(n, d int) string {
	if d == 0 {
		return "\u2014"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(d))
}

func rate(p float64) string { return fmt.Sprintf("%.1f%%", p) }

func comma(n int) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

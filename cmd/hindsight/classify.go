package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/tjeong117/fasthack/internal/hp"
)

// cmdClassify runs the real classifier over commands on stdin, one per line.
//
// This exists so a policy question can be answered against a corpus rather
// than against intuition. "What would we do with these ten thousand commands"
// is a checkable question, and the checking should go through the same code
// path that ships rather than a re-implementation of it.
func cmdClassify(args []string) error {
	fs := flag.NewFlagSet("classify", flag.ContinueOnError)
	withReason := fs.Bool("reason", false, "include the classifier's reason")
	if err := fs.Parse(args); err != nil {
		return err
	}

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<16), 1<<22)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for in.Scan() {
		line := in.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		policy, reason := hp.Classify(line)
		if *withReason {
			fmt.Fprintf(out, "%s\t%s\t%s\n", policy, reason, line)
		} else {
			fmt.Fprintf(out, "%s\t%s\n", policy, line)
		}
	}
	return in.Err()
}

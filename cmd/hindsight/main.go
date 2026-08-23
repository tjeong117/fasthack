package main

import (
	"fmt"
	"os"

	"github.com/tjeong117/fasthack/internal/hp"
)

const usage = `hindsight - a build cache for coding agents

  hindsight hook [--harness codex|claude]   PreToolUse hook; reads payload on stdin
  hindsight key [dir]                       print tree hash and env fingerprint
  hindsight record <key> -- <cmd>...        run a command and record the result
  hindsight daemon [--addr host:port]       shared cache daemon
  hindsight verify [--limit N]              re-execute served results and diff
  hindsight stats                           print counters from the daemon
  hindsight init                            install hook config into this repo
  hindsight doctor                          check this workspace is cacheable
  hindsight classify [--reason]             classify commands from stdin, one per line
  hindsight cache export|import             move a warm cache between machines
  hindsight transitions [--stats]           emit the observed transition corpus
  hindsight replay <corpus>                 what a recorded trace would have saved

Environment:
  HP_ENABLE=1     arm the hook (default off, so it cannot break your own session)
  HP_SERVE=0      record but never serve; the baseline arm of the experiment
  HP_DAEMON       daemon URL, default http://127.0.0.1:7777
  HP_HOME         cache root, default ~/.hindsight/<repo-id>
  HP_AGENT        agent id used for provenance, default "local"
  HP_DEBUG=1      diagnostics to stderr
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "hook":
		err = cmdHook(os.Args[2:])
	case "key":
		err = cmdKey(os.Args[2:])
	case "record":
		err = cmdRecord(os.Args[2:])
	case "daemon":
		err = cmdDaemon(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "stats":
		err = cmdStats(os.Args[2:])
	case "init":
		err = cmdInit(os.Args[2:])
	case "classify":
		err = cmdClassify(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "cache":
		err = cmdCache(os.Args[2:])
	case "transitions":
		err = cmdExport(os.Args[2:])
	case "replay":
		err = cmdReplay(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "hindsight: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		// The hook path never reaches here with an error; everything else may.
		fmt.Fprintf(os.Stderr, "hindsight: %v\n", err)
		os.Exit(1)
	}
}

var _ = hp.Enabled

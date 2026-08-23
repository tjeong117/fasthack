package main

import (
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tjeong117/fasthack/internal/hp"
)

// cmdRecord runs the real command and hands the result to the daemon.
//
// It is transparent: stdout, stderr and the exit code reach the agent exactly
// as if the command had run unwrapped. If anything in the recording path
// fails, the command still ran and the agent still gets its output.
func cmdRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	key := fs.String("key", "", "cache key")
	tree := fs.String("tree", "", "tree hash before execution")
	envfp := fs.String("envfp", "", "env fingerprint before execution")
	policy := fs.String("policy", "SERVE", "policy assigned by the classifier")
	reason := fs.String("reason", "", "classifier reason")
	cwdRel := fs.String("cwdrel", ".", "cwd relative to worktree root")
	b64 := fs.String("b64", "", "base64 of the command to run")
	timeout := fs.Duration("timeout", 30*time.Minute, "hard process-group timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	raw, err := base64.StdEncoding.DecodeString(*b64)
	if err != nil {
		return fmt.Errorf("bad --b64: %w", err)
	}
	command := string(raw)

	cwd, _ := os.Getwd()
	ws, wsErr := hp.NewWorkspace(cwd)

	before := hp.State{Tree: *tree, EnvFP: *envfp}
	res := hp.Run(command, cwd, *timeout)

	// The purity gate. Recompute state after execution and refuse to mark the
	// record servable if the command changed anything.
	//
	// This is measured rather than declared, which is what makes it catch the
	// cases a static command table gets wrong: tsc emits .js, cargo test
	// writes target/, and uv sync mutates a virtualenv that is invisible to
	// the tree hash but plainly visible to the env fingerprint.
	servable := *policy == hp.SERVE.String() && !res.Truncated && !res.TimedOut
	var after hp.State
	if wsErr == nil {
		if st, err := ws.State(); err == nil {
			after = st
			if !before.Equal(after) {
				servable = false
			}
		} else {
			servable = false // could not prove purity, so do not claim it
		}
	} else {
		servable = false
	}

	rec := &hp.Record{
		V: 1, TS: float64(time.Now().UnixMilli()) / 1000,
		Agent: hp.AgentID(), Cmd: command, CmdNorm: hp.NormalizeCommand(command),
		CwdRel: *cwdRel, TreeBefore: before.Tree, EnvFPBefore: before.EnvFP,
		TreeAfter: after.Tree, EnvFPAfter: after.EnvFP,
		Key: *key, Policy: *policy, Reason: *reason, Decision: hp.DecisionMiss,
		Servable: servable, ExitCode: res.ExitCode, DurationMS: res.DurationMS,
	}

	if wsErr == nil {
		store, err := hp.OpenStore(hp.Home(ws.Root))
		if err == nil {
			// Blobs are content-addressed and written with an atomic rename,
			// so concurrent agents writing identical output is safe.
			if id, err := store.PutBlob(res.Stdout); err == nil {
				rec.StdoutBlob = id
			}
			if id, err := store.PutBlob(res.Stderr); err == nil {
				rec.StderrBlob = id
			}
		}
	}
	if rec.StdoutBlob == "" || rec.StderrBlob == "" {
		rec.Servable = false
	}

	client := hp.NewClient()
	if err := client.Record(rec); err != nil {
		hp.Debugf("could not reach daemon to record: %v", err)
		// The lease would otherwise pin peers until it expires.
		_ = client.Release(*key)
	}

	os.Exit(res.ExitCode)
	return nil
}

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

	// Arm the read-set wrapper for the duration of the command only.
	//
	// The disarm must happen before the after-state is measured. PYTHONPATH is
	// in the environment allowlist, so leaving it set would move the
	// fingerprint on every command, fail the purity gate every time, and stop
	// the cache serving anything at all with no error reported anywhere.
	disarm := hp.ArmReadSetIn(ws)
	res := hp.Run(command, cwd, *timeout)
	disarm()

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
			// A detected ecosystem we could not read means we cannot prove the
			// key covers the environment, so this must never be served.
			if !after.EnvComplete {
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
		// What the command was observed to read, if a wrapper could report it.
		// Absent is the normal case and costs a tier-2 promotion, nothing else.
		if rs, ok := hp.CaptureReadSet(ws.Root); ok {
			rs.Policy = *policy
			rec.ReadSet = rs
		}
		// Writing blobs needs a directory, not an index. OpenStore replays the
		// whole log to build one we never touch here, which is the only cost
		// in the system that grows without bound with cache size.
		store := hp.StorePaths(hp.Home(ws.Root))
		{
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

	// Feed the duration memo so the hook can stop intercepting this command if
	// it is reliably cheaper than the interception itself.
	if wsErr == nil {
		home := hp.Home(ws.Root)
		fast := hp.LoadFastpath(home)
		fast.Observe(rec.CmdNorm, res.DurationMS)
		fast.Save()
		if floor := hp.MinDurationMS(); floor > 0 && res.DurationMS < floor {
			// Serving this could never pay for the two tree hashes a lookup
			// costs, so keep it out of the servable index entirely.
			rec.Servable = false
			rec.Reason = "below the duration floor: caching costs more than it saves"
		}
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

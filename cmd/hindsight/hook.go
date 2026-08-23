package main

import (
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/tjeong117/fasthack/internal/hp"
)

// shellQuote wraps a path for POSIX sh. Cache paths are ours, but they sit
// under $HOME and a username with a space would otherwise silently corrupt
// the served command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cmdHook is the PreToolUse entry point.
//
// Every failure path in here is a passthrough. The hook may only ever cost a
// cache hit; it may never cost correctness, and it may never break the agent.
func cmdHook(args []string) error {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	harnessFlag := fs.String("harness", "", "codex|claude (default codex, which claude also accepts)")
	if err := fs.Parse(args); err != nil {
		return nil // fail open
	}
	harness := hp.HarnessFromEnv(*harnessFlag)

	// Kill switch, first thing. This repo installs a hook into its own config,
	// so without this the hook intercepts the very agent sessions used to
	// develop it and a bug takes out the loop needed to fix the bug.
	if !hp.Enabled() {
		return nil
	}

	in, ok := hp.ParseHookInput(os.Stdin)
	if !ok {
		hp.Debugf("unparseable payload or non-Bash tool; passing through")
		return nil
	}

	// Step-0 harness contract check: prove we can intercept and substitute a
	// command before any of the cache exists.
	if forced := os.Getenv("HP_SELFTEST_REWRITE"); forced != "" {
		return hp.Rewrite(os.Stdout, harness, forced, "hindsight selftest")
	}

	policy, reason := hp.Classify(in.ToolInput.Command)
	if policy == hp.PASSTHROUGH {
		hp.Debugf("passthrough: %s", reason)
		return nil
	}

	cmdNorm := hp.NormalizeCommand(in.ToolInput.Command)

	// Consult the duration memo before doing anything expensive. Two tree
	// hashes cost more than a command that reliably runs in single-digit
	// milliseconds could ever save, so for those the cheapest correct thing is
	// to get out of the way. One small file read, no hashing.
	fast := hp.LoadFastpath(hp.HomeForCwd(in.Cwd))
	if fast.KnownFast(cmdNorm, hp.MinDurationMS()) {
		hp.Debugf("known-fast, not worth intercepting: %s", cmdNorm)
		return nil
	}

	ws, err := hp.NewWorkspace(in.Cwd)
	if err != nil {
		hp.Debugf("not a git worktree: %v", err)
		return nil
	}
	state, err := ws.State()
	if err != nil {
		hp.Debugf("state hash failed: %v", err)
		return nil
	}
	if !state.EnvComplete {
		// Some ecosystem is in use but we could not establish what is
		// installed. The key therefore does not cover the environment, and
		// serving would be a guess.
		hp.Debugf("environment not fully established; passing through")
		return nil
	}

	cwdRel := ws.CwdRel(in.Cwd)
	key := hp.Key(state, cwdRel, cmdNorm)

	resp, err := hp.NewClient().Lookup(hp.LookupReq{
		Key: key, Agent: hp.AgentID(), Cmd: in.ToolInput.Command, CmdNorm: cmdNorm,
		CwdRel: cwdRel, Tree: state.Tree, EnvFP: state.EnvFP,
		Policy: policy.String(), Reason: reason,
		Serve:    hp.ServeEnabled() && policy == hp.SERVE,
		RepoRoot: ws.Root,
	})
	if err != nil {
		hp.Debugf("daemon unreachable, passing through: %v", err)
		return nil
	}

	home := hp.Home(ws.Root)
	store := hp.StorePaths(home)

	if resp.Decision == hp.DecisionHit {
		// Serve the recorded execution. stdout and stderr are replayed to their
		// own file descriptors; collapsing them would make served output
		// subtly different from real output and light up shadow verification.
		out := shellQuote(store.BlobPath(resp.StdoutBlob))
		errp := shellQuote(store.BlobPath(resp.StderrBlob))
		served := fmt.Sprintf("cat %s; cat %s >&2; exit %d", out, errp, resp.ExitCode)
		hp.Debugf("HIT tier=%d key=%s from=%s saved=%dms %s",
			resp.Tier, key, resp.SourceAgent, resp.DurationMS, resp.ScopeReason)
		note := fmt.Sprintf("hindsight: served from %s (%dms deleted)", resp.SourceAgent, resp.DurationMS)
		if resp.Tier == 1 {
			note += " [tier-1: " + resp.ScopeReason + "]"
		}
		return hp.Rewrite(os.Stdout, harness, served, note)
	}

	// Miss. Wrap the real execution so its result is recorded for everyone
	// else. The command travels base64-encoded so no amount of quoting in the
	// original can break the wrapper.
	self, err := os.Executable()
	if err != nil {
		return nil
	}
	wrapped := fmt.Sprintf("%s record --key %s --tree %s --envfp %s --policy %s --cwdrel %s --b64 %s",
		shellQuote(self), shellQuote(key), shellQuote(state.Tree), shellQuote(state.EnvFP),
		shellQuote(policy.String()), shellQuote(cwdRel),
		base64.StdEncoding.EncodeToString([]byte(in.ToolInput.Command)))
	hp.Debugf("MISS key=%s waited=%dms", key, resp.WaitedMS)
	return hp.Rewrite(os.Stdout, harness, wrapped, "hindsight: recording")
}

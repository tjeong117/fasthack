package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/tjeong117/fasthack/internal/hp"
)

// cmdVerify re-executes commands the cache is willing to serve and diffs the
// result against what would have been served.
//
// This runs agent-side rather than in the daemon because the daemon has no
// worktree, and a replay is only meaningful in the state it was recorded at.
// A record is only checked when the current workspace state still matches the
// state it was recorded in; anything else would be comparing two different
// questions.
//
// The diff is on normalized output, not raw bytes. Test runners print
// durations, temp paths and per-worktree absolute paths that legitimately
// differ between two correct runs, so a raw byte-diff would report a
// divergence on essentially every hit and the counter would be useless.
func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	limit := fs.Int("limit", 20, "maximum records to check")
	timeout := fs.Duration("timeout", 10*time.Minute, "per-command timeout")
	quiet := fs.Bool("quiet", false, "only print the summary")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cwd, _ := os.Getwd()
	ws, err := hp.NewWorkspace(cwd)
	if err != nil {
		return err
	}
	current, err := ws.State()
	if err != nil {
		return err
	}

	resp, err := http.Get(hp.DaemonURL() + "/servable")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var records []*hp.Record
	if err := json.NewDecoder(resp.Body).Decode(&records); err != nil {
		return err
	}

	store, err := hp.OpenStore(hp.Home(ws.Root))
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()

	var checked, agreed, diverged, skipped int
	for _, rec := range records {
		if checked >= *limit {
			break
		}
		if rec.TreeBefore != current.Tree || rec.EnvFPBefore != current.EnvFP {
			skipped++
			continue
		}
		checked++

		wantOut, err1 := store.GetBlob(rec.StdoutBlob)
		wantErr, err2 := store.GetBlob(rec.StderrBlob)
		if err1 != nil || err2 != nil {
			report(rec.Key, false, "recorded blob missing")
			diverged++
			continue
		}

		res := hp.Run(rec.Cmd, ws.Root, *timeout)

		rawMatch := bytes.Equal(res.Stdout, wantOut) && bytes.Equal(res.Stderr, wantErr)
		gotOutN := hp.Normalize(res.Stdout, ws.Root, home)
		gotErrN := hp.Normalize(res.Stderr, ws.Root, home)
		wantOutN := hp.Normalize(wantOut, ws.Root, home)
		wantErrN := hp.Normalize(wantErr, ws.Root, home)

		ok := res.ExitCode == rec.ExitCode &&
			bytes.Equal(gotOutN, wantOutN) && bytes.Equal(gotErrN, wantErrN)

		detail := "normalized match"
		switch {
		case res.ExitCode != rec.ExitCode:
			detail = fmt.Sprintf("exit code %d != recorded %d", res.ExitCode, rec.ExitCode)
		case !bytes.Equal(gotOutN, wantOutN):
			detail = "stdout diverged after normalization"
		case !bytes.Equal(gotErrN, wantErrN):
			detail = "stderr diverged after normalization"
		case rawMatch:
			detail = "byte-identical"
		}

		report(rec.Key, ok, detail)
		if ok {
			agreed++
		} else {
			diverged++
		}
		if !*quiet {
			fmt.Fprintf(os.Stderr, "  %-8s %s  %s\n", verdictWord(ok), truncate(rec.Cmd, 48), detail)
		}
	}

	fmt.Printf("served %d / verified %d / %d divergent", len(records), agreed, diverged)
	if skipped > 0 {
		fmt.Printf("  (%d skipped: workspace no longer in the recorded state)", skipped)
	}
	fmt.Println()
	if diverged > 0 {
		return fmt.Errorf("CACHE_MISMATCH: %d divergent record(s) evicted", diverged)
	}
	return nil
}

func verdictWord(ok bool) string {
	if ok {
		return "OK"
	}
	return "DIVERGED"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s + string(make([]byte, 0))
	}
	return s[:n-1] + "\u2026"
}

func report(key string, ok bool, detail string) {
	body, _ := json.Marshal(hp.VerifyVerdict{Key: key, OK: ok, Detail: detail})
	resp, err := http.Post(hp.DaemonURL()+"/verify", "application/json", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
	}
}

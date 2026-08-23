package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tjeong117/fasthack/internal/hp"
)

func cmdKey(args []string) error {
	fs := flag.NewFlagSet("key", flag.ContinueOnError)
	command := fs.String("cmd", "", "command to key (optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := fs.Arg(0)
	if dir == "" {
		dir, _ = os.Getwd()
	}

	ws, err := hp.NewWorkspace(dir)
	if err != nil {
		return err
	}
	started := time.Now()
	state, err := ws.State()
	if err != nil {
		return err
	}
	elapsed := time.Since(started)

	fmt.Printf("root        %s\n", ws.Root)
	fmt.Printf("git-dir     %s\n", ws.GitDir)
	fmt.Printf("side-index  %s\n", ws.IndexPath)
	fmt.Printf("hp-home     %s\n", hp.Home(ws.Root))
	fmt.Printf("tree        %s\n", state.Tree)
	fmt.Printf("env-fp      %s\n", state.EnvFP)
	fmt.Printf("ecosystems  %v\n", ws.Ecosystems())
	if !state.EnvComplete {
		fmt.Printf("            WARNING: an ecosystem was detected but could not be read;\n")
		fmt.Printf("            nothing will be served in this workspace\n")
	}
	fmt.Printf("elapsed     %s\n", elapsed.Round(time.Millisecond))
	if *command != "" {
		norm := hp.NormalizeCommand(*command)
		policy, reason := hp.Classify(*command)
		fmt.Printf("cmd-norm    %s\n", norm)
		fmt.Printf("policy      %s (%s)\n", policy, reason)
		fmt.Printf("key         %s\n", hp.Key(state, ws.CwdRel(dir), norm))
	}
	return nil
}

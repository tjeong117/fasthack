package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/tjeong117/fasthack/internal/hp"
)

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:7777", "listen address")
	home := fs.String("home", "", "cache root (default from HP_HOME or the repo)")
	if err := fs.Parse(args); err != nil {
		return err
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

	store, err := hp.OpenStore(dir)
	if err != nil {
		return err
	}
	srv := hp.NewServer(store)
	fmt.Fprintf(os.Stderr, "hindsight daemon on http://%s  cache=%s\n", *addr, dir)
	return http.ListenAndServe(*addr, srv.Handler())
}

func cmdStats(args []string) error {
	resp, err := http.Get(hp.DaemonURL() + "/stats")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var st hp.Stats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return err
	}
	fmt.Printf("served      %d\n", st.Served)
	fmt.Printf("executed    %d\n", st.Executed)
	fmt.Printf("deleted     %.1fs\n", st.SecondsDeleted)
	fmt.Printf("spent       %.1fs\n", st.SecondsSpent)
	fmt.Printf("verified    %d\n", st.Verified)
	fmt.Printf("divergent   %d\n", st.Divergent)
	fmt.Printf("agents      %d\n", st.Agents)
	fmt.Printf("inflight    %d\n", st.Inflight)
	return nil
}

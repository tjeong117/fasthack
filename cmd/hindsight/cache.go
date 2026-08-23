package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/tjeong117/fasthack/internal/hp"
)

func goosOf() string   { return runtime.GOOS }
func goarchOf() string { return runtime.GOARCH }

const cacheUsage = `hindsight cache - move a warm cache between machines

  hindsight cache export [--out FILE] [--all] [--note TEXT]
  hindsight cache import <FILE> [--verify]

A cache is a set of claims about what commands produce. Importing one is
trusting whoever produced it: blob contents are checked against the hash they
were filed under, which catches corruption, but it cannot tell you the
producer was honest.
`

func cmdCache(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, cacheUsage)
		return fmt.Errorf("cache: expected export or import")
	}
	switch args[0] {
	case "export":
		return cmdCacheExport(args[1:])
	case "import":
		return cmdCacheImport(args[1:])
	default:
		fmt.Fprint(os.Stderr, cacheUsage)
		return fmt.Errorf("cache: unknown subcommand %q", args[0])
	}
}

// resolveCacheHome mirrors how the rest of the CLI finds the store.
func resolveCacheHome(override string) string {
	if override != "" {
		return override
	}
	cwd, _ := os.Getwd()
	if ws, err := hp.NewWorkspace(cwd); err == nil {
		return hp.Home(ws.Root)
	}
	return hp.HomeForCwd(cwd)
}

func cmdCacheExport(args []string) error {
	fs := flag.NewFlagSet("cache export", flag.ContinueOnError)
	out := fs.String("out", "", "output file (default stdout)")
	home := fs.String("home", "", "cache root (default: this repo's)")
	all := fs.Bool("all", false, "include records the cache would not serve")
	note := fs.String("note", "", "free-text provenance note stored in the manifest")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir := resolveCacheHome(*home)
	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}

	st, err := hp.PackCache(dir, w, !*all, *note)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "packed %d record(s), %d blob(s), %s from %s\n",
		st.Records, st.Blobs, humanBytes(st.BlobBytes), dir)
	if st.Skipped > 0 {
		fmt.Fprintf(os.Stderr, "skipped %d record(s) the cache would not serve\n", st.Skipped)
	}
	return nil
}

func cmdCacheImport(args []string) error {
	fs := flag.NewFlagSet("cache import", flag.ContinueOnError)
	home := fs.String("home", "", "cache root (default: this repo's)")
	verify := fs.Bool("verify", false, "check the bundle without installing it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("cache import: expected exactly one bundle file")
	}

	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()

	dir := resolveCacheHome(*home)
	st, manifest, err := hp.UnpackCache(dir, f, *verify)
	if err != nil {
		return err
	}

	fmt.Printf("bundle      v%d, key version %s\n", manifest.Version, manifest.KeyVersion)
	fmt.Printf("built       %s on %s/%s\n",
		manifest.CreatedAt.Format("2006-01-02 15:04 MST"), manifest.SourceGOOS, manifest.SourceArch)
	if manifest.Note != "" {
		fmt.Printf("note        %s\n", manifest.Note)
	}
	fmt.Printf("contents    %d record(s), %d blob(s), %s\n",
		st.Records, st.Blobs, humanBytes(st.BlobBytes))

	// Keys hash the OS and architecture, so a bundle from a different platform
	// is not wrong — it simply cannot match anything. Better to say so than to
	// let someone conclude the cache is broken.
	if manifest.SourceGOOS != "" && (manifest.SourceGOOS != goosOf() || manifest.SourceArch != goarchOf()) {
		fmt.Printf("\nnote: built on %s/%s, importing on %s/%s. The environment\n",
			manifest.SourceGOOS, manifest.SourceArch, goosOf(), goarchOf())
		fmt.Printf("fingerprint covers OS and architecture, so none of these keys will\n")
		fmt.Printf("match here. Nothing will be served, and nothing will be wrong.\n")
	}

	if *verify {
		fmt.Printf("\nverified; nothing installed\n")
		return nil
	}
	fmt.Printf("\ninstalled into %s\n", dir)
	fmt.Printf("restart the daemon to pick it up\n")
	return nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tjeong117/fasthack/internal/hp"
)

// hookTimeoutSec must be set explicitly. Codex's default is 600 seconds, so an
// unset timeout means a stalled hook hangs a tool call for ten minutes before
// failing open.
const hookTimeoutSec = 10

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite an existing hindsight hook entry")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cwd, _ := os.Getwd()
	ws, err := hp.NewWorkspace(cwd)
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}

	if err := writeCodexHooks(ws.Root, self+" hook", *force); err != nil {
		return err
	}
	if err := writeClaudeHooks(ws.Root, self+" hook --harness claude", *force); err != nil {
		return err
	}

	fmt.Printf("hindsight installed in %s\n", ws.Root)
	fmt.Printf("  .codex/hooks.json\n  .claude/settings.json\n")
	fmt.Printf("\nThe hook is inert until HP_ENABLE=1 is set. scripts/fleet.sh sets it;\n")
	fmt.Printf("your own development sessions deliberately do not.\n")
	return nil
}

// hookEntry takes the fully-formed command line, not just the binary path.
// The two harnesses need different flags.
func hookEntry(command string) map[string]any {
	return map[string]any{
		"matcher": "Bash",
		"hooks": []any{map[string]any{
			"type":          "command",
			"command":       command,
			"timeout":       hookTimeoutSec,
			"statusMessage": "hindsight: checking cache",
		}},
	}
}

// mergeInto loads an existing JSON config, adds our PreToolUse entry, and
// writes it back. It merges rather than clobbers: these files belong to the
// user and may already carry their own hooks.
func mergeInto(path string, command string, force bool, set func(cfg map[string]any, entry map[string]any)) error {
	cfg := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			if !force {
				return fmt.Errorf("%s exists and is not valid JSON; rerun with --force", path)
			}
			cfg = map[string]any{}
		}
	}
	set(cfg, hookEntry(command))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func writeCodexHooks(root, command string, force bool) error {
	return mergeInto(filepath.Join(root, ".codex", "hooks.json"), command, force,
		func(cfg map[string]any, entry map[string]any) {
			cfg["description"] = "Hindsight: shared execution cache for parallel agents"
			hooks, _ := cfg["hooks"].(map[string]any)
			if hooks == nil {
				hooks = map[string]any{}
			}
			hooks["PreToolUse"] = replaceOurs(hooks["PreToolUse"], entry)
			cfg["hooks"] = hooks
		})
}

func writeClaudeHooks(root, command string, force bool) error {
	return mergeInto(filepath.Join(root, ".claude", "settings.json"), command, force,
		func(cfg map[string]any, entry map[string]any) {
			hooks, _ := cfg["hooks"].(map[string]any)
			if hooks == nil {
				hooks = map[string]any{}
			}
			hooks["PreToolUse"] = replaceOurs(hooks["PreToolUse"], entry)
			cfg["hooks"] = hooks
		})
}

// replaceOurs keeps any hooks the user already had and swaps out only a
// previous hindsight entry.
func replaceOurs(existing any, entry map[string]any) []any {
	var out []any
	if list, ok := existing.([]any); ok {
		for _, item := range list {
			if !isHindsightEntry(item) {
				out = append(out, item)
			}
		}
	}
	return append(out, entry)
}

func isHindsightEntry(item any) bool {
	m, ok := item.(map[string]any)
	if !ok {
		return false
	}
	hooks, ok := m["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, ok := hm["command"].(string); ok && filepath.Base(firstField(cmd)) == "hindsight" {
			return true
		}
	}
	return false
}

func firstField(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			return s[:i]
		}
	}
	return s
}

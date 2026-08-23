package hp

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Harness selects the response envelope. Codex and Claude Code are not
// byte-compatible: codex-rs/hooks/src/schema.rs says "Codex intentionally
// diverges from Claude's public hook docs here", and Codex applies
// deny_unknown_fields to the wire structs.
//
// We always emit the strict Codex-shaped subset, which Claude also accepts,
// and only add the optional reason field for Claude.
type Harness string

const (
	HarnessCodex  Harness = "codex"
	HarnessClaude Harness = "claude"
)

// HookInput is the PreToolUse payload. Both harnesses send this shape.
type HookInput struct {
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
	Cwd       string `json:"cwd"`
	SessionID string `json:"session_id"`
}

type hookSpecificOutput struct {
	HookEventName            string          `json:"hookEventName"`
	PermissionDecision       string          `json:"permissionDecision"`
	PermissionDecisionReason string          `json:"permissionDecisionReason,omitempty"`
	UpdatedInput             json.RawMessage `json:"updatedInput"`
}

type hookOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

// Passthrough emits nothing at all.
//
// This is not the same as permissionDecision "allow". Codex has no plain
// approve verb: "allow" without updatedInput is rejected, and updatedInput
// without "allow" is rejected. Silence is the only way to let a command run
// untouched.
func Passthrough() {}

// Rewrite emits exactly one JSON object instructing the harness to run cmd
// instead of what the model asked for.
//
// Nothing else may ever reach stdout. Output before the JSON makes the hook a
// silent no-op; output after it makes the hook fail loudly. All diagnostics go
// to stderr.
func Rewrite(w io.Writer, h Harness, cmd, reason string) error {
	input, err := json.Marshal(map[string]string{"command": cmd})
	if err != nil {
		return err
	}
	out := hookOutput{HookSpecificOutput: hookSpecificOutput{
		HookEventName:      "PreToolUse",
		PermissionDecision: "allow",
		UpdatedInput:       input,
	}}
	if h == HarnessClaude {
		out.HookSpecificOutput.PermissionDecisionReason = reason
	}
	return json.NewEncoder(w).Encode(out)
}

// ParseHookInput reads the whole payload. A malformed payload is not an error
// worth surfacing: we fail open, which means passthrough.
func ParseHookInput(r io.Reader) (*HookInput, bool) {
	var in HookInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return nil, false
	}
	if in.ToolName != "Bash" {
		return nil, false
	}
	if in.ToolInput.Command == "" || in.Cwd == "" {
		return nil, false
	}
	return &in, true
}

// Enabled reports whether the hook should do anything at all.
//
// Default off. This repo installs a PreToolUse hook into its own .codex and
// .claude config, so without a kill switch the hook intercepts the commands of
// the agent sessions being used to develop the hook, and a bug takes out the
// loop you would use to fix the bug. Only scripts/fleet.sh sets HP_ENABLE.
func Enabled() bool { return os.Getenv("HP_ENABLE") == "1" }

// ServeEnabled distinguishes the two fleet arms. Both arms run the hook and
// record identically; only this differs. A control arm measured differently
// from the treatment arm is not a control arm.
func ServeEnabled() bool { return os.Getenv("HP_SERVE") != "0" }

// AgentID names the agent for provenance ("served from a1").
func AgentID() string {
	if a := os.Getenv("HP_AGENT"); a != "" {
		return a
	}
	return "local"
}

// HarnessFromEnv resolves which envelope to emit. Defaults to the strict
// Codex shape, which both harnesses accept.
func HarnessFromEnv(flag string) Harness {
	v := flag
	if v == "" {
		v = os.Getenv("HP_HARNESS")
	}
	if strings.EqualFold(v, string(HarnessClaude)) {
		return HarnessClaude
	}
	return HarnessCodex
}

// Debugf writes to stderr only. Never stdout.
func Debugf(format string, args ...any) {
	if os.Getenv("HP_DEBUG") == "1" {
		fmt.Fprintf(os.Stderr, "hindsight: "+format+"\n", args...)
	}
}

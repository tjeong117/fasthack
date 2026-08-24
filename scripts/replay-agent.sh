#!/usr/bin/env bash
# replay-agent.sh — a deterministic stand-in for a coding agent.
#
# Reads a command per line and drives each one through the Hindsight hook
# exactly as a real harness would: build a PreToolUse payload, hand it to
# `hindsight hook`, and run whatever comes back (or the original, if the hook
# stayed silent).
#
# This exists for the baseline-versus-cached measurement. Rerunning a real
# model produces a different command sequence every time, and that variance
# swamps the effect being measured, so the two arms would not be comparable.
# A fixed sequence makes the comparison mean something. It is a measurement
# instrument, not a simulation of agent behaviour.
set -uo pipefail

BIN="${HINDSIGHT_BIN:-hindsight}"
SCRIPT="${1:-}"

if [ -z "$SCRIPT" ] || [ ! -r "$SCRIPT" ]; then
	echo "usage: replay-agent.sh <file-of-commands>" >&2
	exit 2
fi

# A worktree is a checkout of TRACKED files only, so anything gitignored that
# the target needs at runtime is missing — node_modules being the case that
# forced this. The prep runs before the first command, so its effect is
# included in the tree hash and the fingerprint that key the very first
# lookup, and it is never itself measured.
if [ -n "${HS_WORKTREE_PREP:-}" ]; then
	eval "$HS_WORKTREE_PREP" || {
		echo "replay-agent: worktree prep failed: $HS_WORKTREE_PREP" >&2
		exit 2
	}
fi

payload() {
	python3 -c 'import json,sys; print(json.dumps({
        "tool_name":"Bash","tool_input":{"command":sys.argv[1]},
        "cwd":sys.argv[2],"session_id":"replay"}))' "$1" "$PWD"
}

# Prints the rewritten command, or exits 9 when the hook declined to speak.
decide() {
	payload "$1" | "$BIN" hook | python3 -c 'import json,sys
raw = sys.stdin.read().strip()
if not raw:
    sys.exit(9)
print(json.loads(raw)["hookSpecificOutput"]["updatedInput"]["command"])'
}

while IFS= read -r cmd || [ -n "$cmd" ]; do
	case "$cmd" in '' | '#'*) continue ;; esac

	rewritten=$(decide "$cmd")
	if [ $? -eq 9 ]; then
		printf '[%s] pass  %s\n' "${HP_AGENT:-local}" "$cmd"
		bash -c "$cmd" >/dev/null 2>&1
		continue
	fi

	case "$rewritten" in
	cat\ *) printf '[%s] serve %s\n' "${HP_AGENT:-local}" "$cmd" ;;
	*) printf '[%s] run   %s\n' "${HP_AGENT:-local}" "$cmd" ;;
	esac
	bash -c "$rewritten" >/dev/null 2>&1
done <"$SCRIPT"

#!/usr/bin/env bash
#
# _load.sh — resolve a named demo target into shell variables.
#
# Sourced by demo-setup.sh and demo-run.sh so the two cannot drift apart. A
# target is a plain shell file in this directory; adding an ecosystem means
# adding two files and touching no code.
#
#   load_target <name>      populates the TARGET_* variables below
#   list_targets            prints "name — description" for every profile
#
# A profile declares:
#
#   TARGET_DESC            one line, shown by --list
#   TARGET_UPSTREAM        clone URL, or "" to mean "this repository itself"
#   TARGET_DIR             where the clone lives
#   TARGET_CMDS            command list, path relative to the repo root
#   TARGET_SETUP           shell run once in $TARGET_DIR to install deps
#   TARGET_SHELL_ENV       shell sourced into the demo shell before anything
#                          else — venv activation and the like. It has to be
#                          sourced rather than run, because it exports the
#                          environment the agents inherit.
#   TARGET_WORKTREE_PREP   shell run inside each agent's worktree before its
#                          commands. `git worktree add` materialises tracked
#                          files only, so anything gitignored that the target
#                          needs at runtime — node_modules is the whole reason
#                          this exists — has to be put back here.
#   TARGET_CLEANUP         shell run in $TARGET_DIR after the purity probe. A
#                          target that deliberately includes an impure command
#                          leaves that command's output behind, and the probe
#                          runs in the real checkout rather than a throwaway
#                          worktree. Without this, running setup twice reports
#                          a dirty tree the second time and blames the target.
#   TARGET_TIMEOUT         per-agent wall-clock kill, seconds. fleet.sh
#                          defaults to 900, which for a deterministic arm that
#                          should finish in ten seconds means one wedged agent
#                          holds the demo for fifteen minutes. Observed once:
#                          an express agent hung and the run looked dead.
#   TARGET_LIVE            1 if --live is supported (needs a committed hook)
#
# Everything except TARGET_DIR and TARGET_CMDS may be empty.

# HS_ROOT is the fasthack checkout, not the target repo.
HS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HS_TARGET_DIR_D="$HS_ROOT/scripts/demo-targets"

# is_git_repo <dir>
#
# Never test for a `.git` DIRECTORY. In a linked worktree — which is what
# `git worktree add` produces, and therefore what every agent in a fleet runs
# in — `.git` is a FILE containing a gitdir pointer. A `[ -d .git ]` check
# reports a perfectly good worktree as "not a git repository", and the
# self-targeting Go profile runs from exactly such a worktree.
is_git_repo() {
	git -C "$1" rev-parse --git-dir >/dev/null 2>&1
}

list_targets() {
	local f name
	for f in "$HS_TARGET_DIR_D"/*.env; do
		[ -e "$f" ] || continue
		name="$(basename "$f" .env)"
		# Read the description without executing the profile: sourcing every
		# profile just to list them would run each one's TARGET_SHELL_ENV.
		printf '  %-14s %s\n' "$name" \
			"$(sed -n 's/^TARGET_DESC="\(.*\)"$/\1/p' "$f" | head -1)"
	done
}

load_target() {
	local name="$1" file
	file="$HS_TARGET_DIR_D/$name.env"
	if [ ! -r "$file" ]; then
		printf 'unknown target: %s\n\nAvailable:\n' "$name" >&2
		list_targets >&2
		return 2
	fi

	# Reset every field before sourcing, so a profile that omits one cannot
	# inherit the previous target's value. SC2034: these are consumed by
	# demo-setup.sh and demo-run.sh, which source this file.
	# shellcheck disable=SC2034
	TARGET_NAME="$name"
	# shellcheck disable=SC2034
	TARGET_DESC=""
	# shellcheck disable=SC2034
	TARGET_UPSTREAM=""
	TARGET_DIR=""
	TARGET_CMDS=""
	# shellcheck disable=SC2034
	TARGET_SETUP=""
	# shellcheck disable=SC2034
	TARGET_SHELL_ENV=""
	# shellcheck disable=SC2034
	TARGET_WORKTREE_PREP=""
	# shellcheck disable=SC2034
	TARGET_CLEANUP=""
	# shellcheck disable=SC2034
	TARGET_TIMEOUT=180
	# shellcheck disable=SC2034
	TARGET_LIVE=0

	# shellcheck disable=SC1090
	. "$file"

	[ -n "$TARGET_DIR" ] || { echo "$name: TARGET_DIR is required" >&2; return 2; }
	[ -n "$TARGET_CMDS" ] || { echo "$name: TARGET_CMDS is required" >&2; return 2; }

	case "$TARGET_CMDS" in
	/*) : ;;
	*) TARGET_CMDS="$HS_ROOT/$TARGET_CMDS" ;;
	esac
	return 0
}

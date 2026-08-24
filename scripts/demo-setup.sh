#!/usr/bin/env bash
#
# demo-setup.sh — prepare a demo target and verify it can actually produce
# cache hits. Idempotent: safe to re-run.
#
# A target is a named profile in scripts/demo-targets/. Adding an ecosystem is
# two files and no code change. See scripts/demo-targets/_load.sh for the
# contract a profile satisfies.
#
# The interesting part is the purity probe at the end. It measures, per
# command, the two things that decide whether the cache can serve it — the
# 500 ms duration floor and whether the tree or the dependency fingerprint
# moved — and then checks that verdict against the `#expect:` annotation in
# the command list. A target whose commands are deliberately refused is a
# valid target; a target whose commands are refused BY SURPRISE is a demo that
# shows a counter stuck at zero and nobody knowing why.
#
# Usage:
#   bash scripts/demo-setup.sh                          # sympy, the default
#   bash scripts/demo-setup.sh --target express
#   bash scripts/demo-setup.sh --target fasthack-go
#   bash scripts/demo-setup.sh --list
#   bash scripts/demo-setup.sh --target sympy --repo-dir /elsewhere/sympy
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/demo-targets/_load.sh
. "$HERE/scripts/demo-targets/_load.sh"

TARGET="sympy"
REPO_DIR_OVERRIDE=""

while [ $# -gt 0 ]; do
	case "$1" in
	--target) TARGET="$2"; shift 2 ;;
	--repo-dir) REPO_DIR_OVERRIDE="$2"; shift 2 ;;
	--list) printf 'Demo targets:\n'; list_targets; exit 0 ;;
	-h | --help) sed -n '2,26p' "$0"; exit 0 ;;
	*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf 'demo-setup: error: %s\n' "$*" >&2; exit 1; }

load_target "$TARGET" || exit 2
[ -n "$REPO_DIR_OVERRIDE" ] && TARGET_DIR="$REPO_DIR_OVERRIDE"

printf 'target  %s — %s\n' "$TARGET_NAME" "$TARGET_DESC"

say "build hindsight onto PATH"
command -v go >/dev/null 2>&1 || die "go is not on PATH (try: export PATH=/opt/homebrew/bin:\$PATH)"
mkdir -p "$HOME/.local/bin"
(cd "$HERE" && go build -o "$HOME/.local/bin/hindsight" ./cmd/hindsight) || die "go build failed"
case ":$PATH:" in
*":$HOME/.local/bin:"*) : ;;
*) echo "  NOTE: add \$HOME/.local/bin to PATH — fleet.sh requires 'hindsight' on PATH" ;;
esac
echo "  $(command -v hindsight || echo "$HOME/.local/bin/hindsight")"

say "target repo"
if [ -z "$TARGET_UPSTREAM" ]; then
	# A self-targeting profile (fasthack-go) points at a checkout that already
	# exists. Cloning it would be wrong, not merely unnecessary.
	is_git_repo "$TARGET_DIR" || die "$TARGET_DIR is not a git repository"
	echo "  in place: $TARGET_DIR"
else
	mkdir -p "$(dirname "$TARGET_DIR")"
	if is_git_repo "$TARGET_DIR"; then
		echo "  already present: $TARGET_DIR"
	else
		git clone --depth 1 "$TARGET_UPSTREAM" "$TARGET_DIR" || die "clone failed"
	fi
fi
echo "  tracked files: $(git -C "$TARGET_DIR" ls-files | wc -l | tr -d ' ')"

if [ -n "$TARGET_SETUP" ]; then
	say "install dependencies"
	setup_start=$(python3 -c 'import time; print(time.time())')
	(cd "$TARGET_DIR" && eval "$TARGET_SETUP") || die "dependency install failed"
	setup_end=$(python3 -c 'import time; print(time.time())')
	printf '  installed in %ss\n' "$(python3 -c "print(round($setup_end-$setup_start,1))")"
fi

if [ -n "$TARGET_SHELL_ENV" ]; then
	say "target environment"
	eval "$TARGET_SHELL_ENV"
	echo "  applied"
fi

cd "$TARGET_DIR" || die "cannot cd $TARGET_DIR"

say "the tree must be clean, or nothing is servable"
# The purity gate requires tree_after == tree_before. Anything left behind that
# git can see makes every record unservable.
if [ -n "$(git status --porcelain)" ]; then
	echo "  WARNING: worktree is dirty. Untracked output will break the purity gate:"
	git status --short | head
else
	echo "  clean"
fi

# state prints "<tree> <env-fp>" for the current working directory.
state() { hindsight key . 2>/dev/null | awk '/^tree /{t=$2} /^env-fp /{e=$2} END{print t" "e}'; }

say "purity probe — floor, tree and fingerprint, per command"
printf '  %9s  %-8s %-8s %-9s %s\n' "duration" "tree" "env-fp" "verdict" "command"
FAIL=0
EXPECT=""
SERVED_ANY=0
while IFS= read -r line || [ -n "$line" ]; do
	case "$line" in
	'#expect:'*) EXPECT="${line#\#expect:}"; continue ;;
	'' | '#'*) continue ;;
	esac
	cmd="$line"

	before="$(state)"
	start=$(python3 -c 'import time; print(time.time())')
	eval "$cmd" >/dev/null 2>&1
	end=$(python3 -c 'import time; print(time.time())')
	after="$(state)"
	ms=$(python3 -c "print(int(($end-$start)*1000))")

	tree_moved=no; env_moved=no
	[ "${before%% *}" = "${after%% *}" ] || tree_moved=yes
	[ "${before##* }" = "${after##* }" ] || env_moved=yes

	if [ "$ms" -lt 500 ]; then
		verdict="FLOOR"
	elif [ "$tree_moved" = yes ] || [ "$env_moved" = yes ]; then
		verdict="refused"
	else
		verdict="served"
		SERVED_ANY=1
	fi

	flag=""
	# A command under the floor is always a bug in the target: it can never be
	# served, whatever the profile expected.
	if [ "$verdict" = "FLOOR" ]; then
		flag="  <-- UNDER THE 500ms FLOOR, can never be served"
		FAIL=1
	elif [ -n "$EXPECT" ] && [ "$EXPECT" != "$verdict" ]; then
		flag="  <-- EXPECTED $EXPECT"
		FAIL=1
	fi

	printf '  %7sms  %-8s %-8s %-9s %s%s\n' "$ms" "$tree_moved" "$env_moved" "$verdict" "$cmd" "$flag"
	EXPECT=""
done <"$TARGET_CMDS"

if [ "$SERVED_ANY" = 0 ]; then
	echo "  NOTE: no command in this target is servable, so a fleet shows a 0% hit rate."
fi

# The probe runs in the real checkout, so an intentionally impure command has
# just left its output there. Fleet runs do not need this — those happen in
# worktrees that get removed — but setup does, or the second run reports a
# dirty tree and blames the target for it.
if [ -n "$TARGET_CLEANUP" ]; then
	eval "$TARGET_CLEANUP"
	printf '  cleanup ran; tree now %s\n' "$(state | cut -d' ' -f1)"
fi

say "workspace cacheability"
hindsight doctor 2>&1 | tail -n 20

if [ "$FAIL" = 1 ]; then
	cat <<EOF

SETUP INCOMPLETE: the probe above disagrees with the target's expectations.

A "FLOOR" row means the command is too fast to ever be served — pick a slower
one. An "EXPECTED served" row means something in the target now dirties the
tree; read the tree/env-fp columns to see which one moved.

EOF
	exit 1
fi

cat <<EOF

Setup complete.

  target        $TARGET_NAME
  repo          $TARGET_DIR
  commands      $TARGET_CMDS

Next:
  bash scripts/demo-run.sh --target $TARGET_NAME

EOF

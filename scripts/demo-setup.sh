#!/usr/bin/env bash
#
# demo-setup.sh — prepare the demo target repo and verify it can actually
# produce cache hits. Idempotent: safe to re-run.
#
# Why sympy: it is a real, recognisable, 2000-file repository that is pure
# Python with one runtime dependency, so it installs in seconds instead of
# minutes. That matters more than it sounds. The cache has a 500 ms duration
# floor (internal/hp/fastpath.go), below which nothing is servable, so the
# demo target must have commands that genuinely take seconds. sympy's per-file
# test targets run 2.5-3s and the whole core suite runs ~14s, all well clear of
# the floor, and the tree stays clean afterwards so the purity gate is
# satisfied. Repos like Apache Beam were rejected: Gradle plus a multi-minute
# multi-language install is a demo that fails on stage.
#
# Usage: bash scripts/demo-setup.sh [--repo-dir ~/src/sympy]
set -uo pipefail

REPO_DIR="${HOME}/src/sympy"
UPSTREAM="https://github.com/sympy/sympy.git"

while [ $# -gt 0 ]; do
	case "$1" in
	--repo-dir) REPO_DIR="$2"; shift 2 ;;
	-h | --help) sed -n '2,20p' "$0"; exit 0 ;;
	*) echo "unknown argument: $1" >&2; exit 2 ;;
	esac
done

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf 'demo-setup: error: %s\n' "$*" >&2; exit 1; }

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

say "build hindsight onto PATH"
command -v go >/dev/null 2>&1 || die "go is not on PATH (try: export PATH=/opt/homebrew/bin:\$PATH)"
mkdir -p "$HOME/.local/bin"
(cd "$HERE" && go build -o "$HOME/.local/bin/hindsight" ./cmd/hindsight) || die "go build failed"
case ":$PATH:" in
*":$HOME/.local/bin:"*) : ;;
*) echo "  NOTE: add \$HOME/.local/bin to PATH — fleet.sh requires 'hindsight' on PATH" ;;
esac
echo "  $(command -v hindsight || echo "$HOME/.local/bin/hindsight")"

say "clone target repo"
mkdir -p "$(dirname "$REPO_DIR")"
if [ -d "$REPO_DIR/.git" ]; then
	echo "  already present: $REPO_DIR"
else
	git clone --depth 1 "$UPSTREAM" "$REPO_DIR" || die "clone failed"
fi
echo "  tracked files: $(git -C "$REPO_DIR" ls-files | wc -l | tr -d ' ')"

say "python environment"
command -v uv >/dev/null 2>&1 || die "uv is not installed"
cd "$REPO_DIR" || die "cannot cd $REPO_DIR"
[ -d .venv ] || uv venv .venv || die "uv venv failed"
# hypothesis is required by sympy/conftest.py; without it every test errors out.
uv pip install -q -e . pytest hypothesis || die "dependency install failed"
# shellcheck disable=SC1091
source .venv/bin/activate
echo "  python  $(command -v python)"
echo "  sympy   $(python -c 'import sympy; print(sympy.__version__)' 2>/dev/null)"

say "the tree must be clean, or nothing is servable"
# The purity gate requires tree_after == tree_before. Anything the test run
# leaves behind that git can see makes every record unservable.
if [ -n "$(git status --porcelain)" ]; then
	echo "  WARNING: worktree is dirty. Untracked output will break the purity gate:"
	git status --short | head
else
	echo "  clean"
fi

say "verify every demo command clears the 500 ms floor"
# This is the check that decides whether the demo works at all. A command under
# the floor is recorded as unservable and the counter stays at zero.
FAIL=0
while IFS= read -r cmd; do
	case "$cmd" in '' | '#'*) continue ;; esac
	start=$(python3 -c 'import time; print(time.time())')
	eval "$cmd" >/dev/null 2>&1
	end=$(python3 -c 'import time; print(time.time())')
	ms=$(python3 -c "print(int(($end-$start)*1000))")
	if [ "$ms" -lt 500 ]; then
		printf '  %6sms  BELOW FLOOR — will never be served:  %s\n' "$ms" "$cmd"
		FAIL=1
	else
		printf '  %6sms  ok  %s\n' "$ms" "$cmd"
	fi
done <"$HERE/scripts/demo-cmds.txt"

say "workspace cacheability"
hindsight doctor 2>&1 | tail -n 20

if [ "$FAIL" = 1 ]; then
	printf '\nSETUP INCOMPLETE: at least one command is under the duration floor.\n'
	printf 'Pick slower targets in scripts/demo-cmds.txt, or the demo shows zero hits.\n\n'
	exit 1
fi

cat <<EOF

Setup complete.

  target repo   $REPO_DIR
  venv          $REPO_DIR/.venv   (activate before running the demo)
  commands      $HERE/scripts/demo-cmds.txt

Next:
  source $REPO_DIR/.venv/bin/activate
  bash scripts/demo-run.sh --repo $REPO_DIR

EOF

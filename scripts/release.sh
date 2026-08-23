#!/usr/bin/env bash
#
# Cross-compile release artifacts into dist/.
#
# Reproducible in the sense that matters here: two people building the same
# commit get byte-identical archives. -trimpath keeps build paths out of the
# binary, CGO_ENABLED=0 keeps the host toolchain out of it, mtimes are pinned
# to the commit date, ownership is normalised, members are written in a fixed
# order, and gzip -n keeps the timestamp out of the gzip header. The one thing
# not pinned is the gzip implementation itself, so a macOS-built and a
# Linux-built archive of the same commit hold the same tar stream but may not
# be the same compressed bytes.
#
# Usage: bash scripts/release.sh
#
#   VERSION=v0.1.0   override the version string (default: git describe)
#
set -euo pipefail

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

TARGETS=(darwin/arm64 darwin/amd64 linux/amd64 linux/arm64)
DIST="$ROOT/dist"
STAGE="$DIST/.stage"

# A stdlib-only Go binary that imports net/http lands around 7 MiB. Much
# larger than this and something has been linked in that should not be.
MAX_BINARY_MIB=20

# cmd/hindsight/main.go has no version variable, so there is nothing for
# -X to write into. When one is added, this becomes: "-s -w -X main.version=$version".
LDFLAGS="-s -w"

die() { printf 'release: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- host tools

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$@"; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$@"; }
else
  die "need sha256sum or shasum"
fi

# GNU tar and bsdtar spell identical intent differently. Both end up writing
# uid 0 / gid 0 / root / root in ustar format, so the streams agree.
if tar --version 2>/dev/null | grep -qi 'gnu tar'; then
  tar_flags=(--format=ustar --owner=root:0 --group=root:0)
else
  tar_flags=(--format=ustar --uid 0 --gid 0 --uname root --gname root)
fi

# ------------------------------------------------------------------ metadata

version="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
[ -n "$version" ] || die "could not determine a version"

# git formats dates portably; date(1) does not.
stamp="$(git log -1 --format=%cd --date=format:'%Y%m%d%H%M.%S' 2>/dev/null || echo '197001010000.00')"

for f in LICENSE README.md; do
  [ -f "$ROOT/$f" ] || die "missing $f, which every archive is supposed to carry"
done

printf 'hindsight %s\n' "$version"
printf 'go        %s\n\n' "$(go version | cut -d' ' -f3-)"

rm -rf "$DIST"
mkdir -p "$STAGE"

# ------------------------------------------------------------------- targets

archives=()
for target in "${TARGETS[@]}"; do
  os="${target%%/*}"
  arch="${target##*/}"
  base="hindsight_${version}_${os}_${arch}"
  work="$STAGE/$base"

  printf 'building %s/%s ... ' "$os" "$arch"
  mkdir -p "$work"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$work/hindsight" ./cmd/hindsight \
    || die "build failed for $os/$arch"

  cp "$ROOT/LICENSE" "$ROOT/README.md" "$work/"
  chmod 755 "$work/hindsight"
  chmod 644 "$work/LICENSE" "$work/README.md"
  touch -t "$stamp" "$work/hindsight" "$work/LICENSE" "$work/README.md"

  # Members listed explicitly, so the order does not depend on readdir.
  ( cd "$work" && tar "${tar_flags[@]}" -cf - LICENSE README.md hindsight ) \
    | gzip -n -9 > "$DIST/$base.tar.gz"

  archives+=("$base.tar.gz")
  printf '%s\n' "$base.tar.gz"
done

# ---------------------------------------------------------------- checksums
# Bare filenames in target order, so `shasum -c SHA256SUMS` works from dist/
# and install.sh can look a name up with a plain grep.

( cd "$DIST" && sha256 "${archives[@]}" > SHA256SUMS )

# ------------------------------------------------------------------- summary

mib() { awk -v b="$1" 'BEGIN { printf "%.1f", b / 1048576 }'; }

printf '\n%-46s %10s %10s\n' ARTIFACT BINARY ARCHIVE
printf '%-46s %10s %10s\n' "$(printf '%.0s-' {1..46})" ---------- ----------

oversized=0
for target in "${TARGETS[@]}"; do
  os="${target%%/*}"
  arch="${target##*/}"
  base="hindsight_${version}_${os}_${arch}"

  bin_bytes="$(wc -c < "$STAGE/$base/hindsight" | tr -d ' ')"
  tgz_bytes="$(wc -c < "$DIST/$base.tar.gz" | tr -d ' ')"
  bin_mib="$(mib "$bin_bytes")"

  printf '%-46s %9sM %9sM\n' "$base.tar.gz" "$bin_mib" "$(mib "$tgz_bytes")"

  if awk -v m="$bin_mib" -v cap="$MAX_BINARY_MIB" 'BEGIN { exit !(m > cap) }'; then
    printf '  WARNING: %s binary is %s MiB, over the %s MiB ceiling.\n' \
      "$target" "$bin_mib" "$MAX_BINARY_MIB" >&2
    printf '           A stdlib-only build should be a few MiB; check for a new dependency.\n' >&2
    oversized=1
  fi
done

rm -rf "$STAGE"

printf '\n%d archives + SHA256SUMS in dist/\n' "${#TARGETS[@]}"
[ "$oversized" -eq 0 ] || printf 'One or more binaries exceeded the size ceiling (see above).\n' >&2

#!/bin/sh
#
# Install Hindsight, a build cache for coding agents.
#
#   curl -fsSL https://raw.githubusercontent.com/tjeong117/fasthack/main/install.sh | sh
#
# HEADS UP: tjeong117/fasthack has no published release yet. Until the first
# tag is pushed this script will stop at the download step and tell you to
# build from source. Everything before that step -- platform detection,
# checksum verification, choosing an install directory -- works today.
#
# What it does: works out your OS and CPU, downloads the matching release
# tarball from GitHub, checks it against the published SHA256SUMS, and copies
# one static binary into a directory on your PATH. It writes nothing else,
# touches no shell profile, and needs no sudo if ~/.local/bin will do.
#
# Environment:
#   HINDSIGHT_VERSION       tag to install, e.g. v0.1.0. Default: latest release.
#   HINDSIGHT_INSTALL_DIR   where the binary goes. Default: the first of
#                           /usr/local/bin or ~/.local/bin that is writable.
#
set -eu

REPO="tjeong117/fasthack"
VERSION="${HINDSIGHT_VERSION:-latest}"

say()  { printf 'hindsight: %s\n' "$*"; }
die()  { printf 'hindsight: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

fetch() {
  if   have curl; then curl -fsSL "$1"
  elif have wget; then wget -qO- "$1"
  else die "need curl or wget to download anything"
  fi
}

sha256_of() {
  if   have sha256sum; then sha256sum "$1" | cut -d' ' -f1
  elif have shasum;    then shasum -a 256 "$1" | cut -d' ' -f1
  else die "need sha256sum or shasum to verify the download"
  fi
}

# Prints the "<os>_<arch>" half of a release artifact name for this machine.
# Refusing here is the point: installing the wrong binary is worse than not
# installing one.
detect_target() {
  os=$(uname -s)
  arch=$(uname -m)

  case "$os" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    *) die "unsupported OS: $os. Hindsight ships macOS and Linux builds only; build from source with 'go build ./cmd/hindsight'." ;;
  esac

  case "$arch" in
    x86_64 | amd64)  arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) die "unsupported CPU: $arch on $os. Hindsight ships amd64 and arm64 builds only; build from source with 'go build ./cmd/hindsight'." ;;
  esac

  printf '%s_%s\n' "$os" "$arch"
}

# The artifact names embed the version, and GitHub's /releases/latest redirect
# does not reveal the tag, so resolve it through the API.
resolve_version() {
  if [ "$VERSION" != latest ]; then
    printf '%s\n' "$VERSION"
    return
  fi
  tag=$(fetch "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null |
        sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1) || true
  [ -n "${tag:-}" ] ||
    die "$REPO has no published release yet. Build from source instead: git clone https://github.com/$REPO && cd fasthack && go build ./cmd/hindsight"
  printf '%s\n' "$tag"
}

# Prefer a directory we can already write, so the common case needs no sudo.
choose_dir() {
  if [ -n "${HINDSIGHT_INSTALL_DIR:-}" ]; then
    mkdir -p "$HINDSIGHT_INSTALL_DIR" 2>/dev/null ||
      die "cannot create HINDSIGHT_INSTALL_DIR=$HINDSIGHT_INSTALL_DIR"
    [ -w "$HINDSIGHT_INSTALL_DIR" ] ||
      die "HINDSIGHT_INSTALL_DIR=$HINDSIGHT_INSTALL_DIR is not writable"
    printf '%s\n' "$HINDSIGHT_INSTALL_DIR"
    return
  fi
  if [ -w /usr/local/bin ]; then
    printf '%s\n' /usr/local/bin
    return
  fi
  if mkdir -p "$HOME/.local/bin" 2>/dev/null && [ -w "$HOME/.local/bin" ]; then
    printf '%s\n' "$HOME/.local/bin"
    return
  fi
  die "neither /usr/local/bin nor $HOME/.local/bin is writable. Pick a directory with HINDSIGHT_INSTALL_DIR=<dir>, or re-run this script under sudo."
}

main() {
  target=$(detect_target)
  version=$(resolve_version)
  dir=$(choose_dir)

  archive="hindsight_${version}_${target}.tar.gz"
  base="https://github.com/$REPO/releases/download/$version"

  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT

  say "downloading $archive"
  fetch "$base/$archive"   > "$tmp/archive.tar.gz" || die "could not download $base/$archive"
  fetch "$base/SHA256SUMS" > "$tmp/SHA256SUMS"     || die "could not download $base/SHA256SUMS"

  want=$(awk -v name="$archive" '$2 == name { print $1 }' "$tmp/SHA256SUMS")
  got=$(sha256_of "$tmp/archive.tar.gz")
  [ -n "$want" ] || die "$archive is not listed in SHA256SUMS; refusing to install"
  [ "$want" = "$got" ] || die "checksum mismatch for $archive
  expected $want
  got      $got"
  say "checksum verified"

  tar -xzf "$tmp/archive.tar.gz" -C "$tmp"
  [ -f "$tmp/hindsight" ] || die "$archive did not contain a hindsight binary"

  # Copy then rename, so an upgrade cannot leave a half-written binary behind.
  cp "$tmp/hindsight" "$dir/.hindsight.new"
  chmod 755 "$dir/.hindsight.new"
  mv -f "$dir/.hindsight.new" "$dir/hindsight"

  say "installed $version to $dir/hindsight"

  case ":$PATH:" in
    *":$dir:"*) ;;
    *) say "note: $dir is not on your PATH -- add it to your shell profile" ;;
  esac

  cat <<EOF

Three commands to get started, from inside the repo you want to cache:

  hindsight doctor                   # can this workspace be cached safely?
  hindsight init                     # install the PreToolUse hook
  hindsight doctor --ensure-daemon   # start the shared daemon

The hook stays inert until you set HP_ENABLE=1, so nothing changes until
you ask for it.
EOF
}

# Sourced with HINDSIGHT_LIB=1 to test the functions above without installing.
[ "${HINDSIGHT_LIB:-0}" = 1 ] || main "$@"

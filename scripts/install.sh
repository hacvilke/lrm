#!/bin/sh
# LRM installer — gets a working `lrm` onto a machine with no Go, no git
# knowledge, and no root: it downloads a prebuilt binary for this OS/arch,
# verifies it against the release checksums, and drops it in a directory
# you own.
#
#   curl -fsSL https://raw.githubusercontent.com/hacvilke/lrm/main/scripts/install.sh | sh
#
# Options:
#   --dir DIR        install here (default: $LRM_INSTALL_DIR or ~/.local/bin,
#                    falling back to ~/bin, falling back to .)
#   --version TAG    install a specific release (e.g. v0.3.0; default: latest)
#   --from FILE      install a local binary instead of downloading (offline,
#                    air-gapped, or testing)
#   --source         build from source instead of downloading a release
#   --source-dir DIR build from a local checkout (implies --source)
#   --force          overwrite an existing lrm
#   --dry-run        print what would happen, change nothing
#
# Environment:
#   LRM_REPO         github repo (default hacvilke/lrm)
#   LRM_BASE_URL     release download base (mirror); default is GitHub
#   LRM_INSTALL_DIR  install directory
#   LRM_VERSION      release tag, same as --version
#
# POSIX sh, no bashisms, no dependencies beyond curl|wget, tar, and a
# sha256 tool. Exits non-zero on any failure so `set -e` users are safe.

set -eu

REPO="${LRM_REPO:-hacvilke/lrm}"
VERSION="${LRM_VERSION:-}"
BASE_URL="${LRM_BASE_URL:-}"
INSTALL_DIR="${LRM_INSTALL_DIR:-}"
FROM_FILE=""
USE_SOURCE=0
SOURCE_DIR=""
FORCE=0
DRY_RUN=0
BIN_NAME="lrm"

say()  { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dir)        INSTALL_DIR="${2:?--dir needs a value}"; shift 2 ;;
    --dir=*)      INSTALL_DIR="${1#*=}"; shift ;;
    --version)    VERSION="${2:?--version needs a value}"; shift 2 ;;
    --version=*)  VERSION="${1#*=}"; shift ;;
    --from)       FROM_FILE="${2:?--from needs a value}"; shift 2 ;;
    --from=*)     FROM_FILE="${1#*=}"; shift ;;
    --source)     USE_SOURCE=1; shift ;;
    --source-dir) SOURCE_DIR="${2:?--source-dir needs a value}"; USE_SOURCE=1; shift 2 ;;
    --source-dir=*) SOURCE_DIR="${1#*=}"; USE_SOURCE=1; shift ;;
    --force)      FORCE=1; shift ;;
    --dry-run)    DRY_RUN=1; shift ;;
    -h|--help)    sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)            die "unknown option $1 (try --help)" ;;
  esac
done

# --- what are we running on? ------------------------------------------------
detect_os() {
  case "$(uname -s)" in
    Linux)  echo linux ;;
    Darwin) echo darwin ;;
    CYGWIN*|MINGW*|MSYS*|Windows_NT) echo windows ;;
    *) die "unsupported OS: $(uname -s) — build from source with --source" ;;
  esac
}
detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)   echo amd64 ;;
    arm64|aarch64)  echo arm64 ;;
    armv7l|armv6l|arm) echo arm ;;
    i386|i686)      echo 386 ;;
    *) die "unsupported CPU: $(uname -m) — build from source with --source" ;;
  esac
}

OS="$(detect_os)"
ARCH="$(detect_arch)"
ASSET="lrm_${OS}_${ARCH}"
[ "$OS" = windows ] && ASSET="${ASSET}.exe"
[ "$OS" = windows ] && BIN_NAME="lrm.exe"

# --- where does it go? ------------------------------------------------------
pick_dir() {
  if [ -n "$INSTALL_DIR" ]; then printf '%s\n' "$INSTALL_DIR"; return; fi
  for d in "$HOME/.local/bin" "$HOME/bin"; do
    if [ -d "$d" ] && [ -w "$d" ]; then printf '%s\n' "$d"; return; fi
  done
  if mkdir -p "$HOME/.local/bin" 2>/dev/null; then printf '%s\n' "$HOME/.local/bin"; return; fi
  printf '%s\n' "."
}
DEST_DIR="$(pick_dir)"
DEST="$DEST_DIR/$BIN_NAME"

# --- helpers ----------------------------------------------------------------
fetch() { # fetch URL [OUTFILE]; to stdout when OUTFILE is "-" or missing
  url="$1"; out="${2:--}"
  if command -v curl >/dev/null 2>&1; then
    if [ "$out" = "-" ]; then curl -fsSL "$url"; else curl -fsSL -o "$out" "$url"; fi
  elif command -v wget >/dev/null 2>&1; then
    if [ "$out" = "-" ]; then wget -qO- "$url"; else wget -qO "$out" "$url"; fi
  else
    die "neither curl nor wget is available"
  fi
}

sha256_of() { # print hex sha256 of file
  f="$1"
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$f" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$f" | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then openssl dgst -sha256 "$f" | awk '{print $NF}'
  else die "no sha256 tool found (sha256sum/shasum/openssl)"
  fi
}

latest_version() {
  tag="$(fetch "https://api.github.com/repos/${REPO}/releases/latest" \
        | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
  [ -n "$tag" ] || die "could not find a release for ${REPO} (has anything been tagged yet?)
       build from source instead:  $0 --source"
  printf '%s\n' "$tag"
}

# --- plan -------------------------------------------------------------------
say "LRM installer"
say "  platform : ${OS}/${ARCH}"
say "  install  : ${DEST}"
[ "$DRY_RUN" = 1 ] && say "  mode     : dry run"
say ""

if [ "$DRY_RUN" = 1 ]; then
  say "would install ${BIN_NAME} to ${DEST}"
  [ -n "$FROM_FILE" ] && say "from local file ${FROM_FILE}"
  [ "$USE_SOURCE" = 1 ] && say "by building from source"
  [ -z "$FROM_FILE" ] && [ "$USE_SOURCE" = 0 ] && say "from release ${VERSION:-latest} of ${REPO}"
  exit 0
fi

[ -e "$DEST" ] && [ "$FORCE" = 0 ] && die "$DEST already exists (use --force to replace it, or --dir to install elsewhere)"
mkdir -p "$DEST_DIR" || die "cannot create ${DEST_DIR}"

TMP="$(mktemp -d 2>/dev/null || mktemp -d -t lrm-install)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT INT TERM

SRC=""

if [ -n "$FROM_FILE" ]; then
  # --- local file -----------------------------------------------------------
  [ -f "$FROM_FILE" ] || die "no such file: $FROM_FILE"
  SRC="$FROM_FILE"
  say "using local binary: ${FROM_FILE}"
elif [ "$USE_SOURCE" = 1 ]; then
  # --- build from source ----------------------------------------------------
  command -v go >/dev/null 2>&1 || die "Go is not installed — install it from https://go.dev/dl/ or grab a release binary instead (no --source)"
  if [ -n "$SOURCE_DIR" ]; then
    [ -d "$SOURCE_DIR/cmd/lrm" ] || die "${SOURCE_DIR} does not look like an LRM checkout"
    SRC_DIR="$SOURCE_DIR"
  else
    command -v git >/dev/null 2>&1 || die "git is not installed (needed to fetch the sources) — pass --source-dir with a local checkout"
    say "cloning https://github.com/${REPO} ..."
    git clone --depth 1 "https://github.com/${REPO}.git" "$TMP/src" >/dev/null 2>&1 || die "clone failed"
    SRC_DIR="$TMP/src"
  fi
  say "building lrm (this takes about a minute) ..."
  ( cd "$SRC_DIR" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$TMP/$BIN_NAME" ./cmd/lrm ) \
    || die "build failed"
  SRC="$TMP/$BIN_NAME"
else
  # --- download a release ---------------------------------------------------
  [ -n "$VERSION" ] || VERSION="$(latest_version)"
  [ "$VERSION" = "latest" ] && VERSION="$(latest_version)"
  if [ -n "$BASE_URL" ]; then
    URL="${BASE_URL%/}/${ASSET}"
    SUMS_URL="${BASE_URL%/}/SHA256SUMS.txt"
  else
    URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"
    SUMS_URL="https://github.com/${REPO}/releases/download/${VERSION}/SHA256SUMS.txt"
  fi
  say "downloading ${ASSET} (${VERSION}) ..."
  fetch "$URL" "$TMP/$ASSET" || die "download failed: ${URL}
       nothing has been released yet? build from source instead:  $0 --source"
  SRC="$TMP/$ASSET"

  say "verifying checksum ..."
  SUMS="$(fetch "$SUMS_URL" - 2>/dev/null || true)"
  if [ -n "$SUMS" ]; then
    want="$(printf '%s\n' "$SUMS" | awk -v a="$ASSET" '$2==a || $2=="*"a {print $1}' | head -1)"
    if [ -n "$want" ]; then
      got="$(sha256_of "$SRC")"
      [ "$want" = "$got" ] || die "checksum mismatch for ${ASSET}
       expected ${want}
       got      ${got}
       refusing to install; retry, or use --source if you trust the checkout"
      say "  ok ${got}"
    else
      warn "no checksum listed for ${ASSET} — installing without verification"
    fi
  else
    warn "SHA256SUMS.txt unavailable — installing without verification"
  fi
fi

# --- install ----------------------------------------------------------------
install -m 0755 "$SRC" "$DEST" 2>/dev/null || {
  cp "$SRC" "$DEST" || die "could not write ${DEST}"
  chmod 0755 "$DEST" || die "could not chmod ${DEST}"
}

say ""
version_out="$("$DEST" version 2>/dev/null | head -1 || true)"
say "installed: ${DEST}${version_out:+  ($version_out)}"

case ":${PATH}:" in
  *":${DEST_DIR}:"*) ;;
  *)
    say ""
    say "Add it to your PATH so \`lrm\` just works:"
    say "  echo 'export PATH=\"${DEST_DIR}:\$PATH\"' >> ~/.profile && . ~/.profile"
    ;;
esac

say ""
say "Next:"
say "  mkdir ~/my-project && cd ~/my-project"
say "  lrm init --user you"
say "  lrm daemon            # starts syncing with your other devices"
say "  lrm dashboard         # opens the local view in your browser"

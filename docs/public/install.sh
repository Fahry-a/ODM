#!/bin/sh
# ODM (Oryn Download Manager) install script
# Usage: curl -fsSL https://odm.orynix.id/install.sh | sh
#        curl -fsSL https://odm.orynix.id/install.sh | sh -s -- --version 1.7.0
set -e

# --- defaults ----------------------------------------------------------------
REPO="Fahry-a/odm"
PREFIX=""
VERSION=""
YES=0

# --- colors ------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
RESET='\033[0m'

info()  { printf "${CYAN}=> %s${RESET}\n" "$*"; }
ok()    { printf "${GREEN}=> %s${RESET}\n" "$*"; }
warn()  { printf "${YELLOW}=> %s${RESET}\n" "$*"; }
err()   { printf "${RED}=> %s${RESET}\n" "$*" >&2; exit 1; }

# --- arg parsing -------------------------------------------------------------
usage() {
    cat <<EOF
ODM (Oryn Download Manager) installer

Usage:
  curl -fsSL https://odm.orynix.id/install.sh | sh
  curl -fsSL https://odm.orynix.id/install.sh | sh -s -- [OPTIONS]

Options:
  --version VERSION   Install a specific version (default: latest)
  --prefix PATH       Install prefix (default: auto-detect)
  -y, --yes           Skip confirmation prompt
  -h, --help          Show this help

Install locations (auto-detected):
  writable /usr/local  → /usr/local/bin/odm, man, config
  no sudo available    → ~/.local/bin/odm, ~/.local/share/man, ~/.config/odm
EOF
    exit 0
}

while [ $# -gt 0 ]; do
    case "$1" in
        --version)  VERSION="$2"; shift 2 ;;
        --prefix)   PREFIX="$2"; shift 2 ;;
        -y|--yes)   YES=1; shift ;;
        -h|--help)  usage ;;
        *)          err "unknown option: $1 (try --help)" ;;
    esac
done

# --- dependency checks -------------------------------------------------------
need() {
    command -v "$1" >/dev/null 2>&1 || err "required: $1 (install it first)"
}

need curl
need uname
need mktemp
need install

# sha256sum is on Linux; macOS ships shasum instead
if command -v sha256sum >/dev/null 2>&1; then
    sha256_check() { sha256sum -c "$1"; }
elif command -v shasum >/dev/null 2>&1; then
    sha256_check() { shasum -a 256 -c "$1"; }
else
    sha256_check() { warn "no sha256 tool found — skipping checksum verification"; }
fi

# --- detect platform ---------------------------------------------------------
detect_os() {
    _os="$(uname -s)"
    case "$_os" in
        Linux*)  echo "linux" ;;
        Darwin*) echo "darwin" ;;
        *)       err "unsupported OS: $_os" ;;
    esac
}

detect_arch() {
    _arch="$(uname -m)"
    case "$_arch" in
        x86_64|amd64)   echo "amd64" ;;
        aarch64|arm64)   echo "arm64" ;;
        armv7*|armv7l|armv8*)  echo "arm" ;;
        i686|i386)       echo "386" ;;
        *)               err "unsupported architecture: $_arch" ;;
    esac
}

OS="$(detect_os)"
ARCH="$(detect_arch)"

info "detected platform: ${OS}/${ARCH}"

# --- resolve prefix (auto-detect writable) -----------------------------------
resolve_prefix() {
    # explicit --prefix wins
    if [ -n "$PREFIX" ]; then
        info "using prefix: ${PREFIX}"
        return
    fi

    # try /usr/local first (standard, system-wide)
    if [ -w "/usr/local/bin" ] 2>/dev/null; then
        PREFIX="/usr/local"
        info "installing system-wide to ${PREFIX}"
        return
    fi

    # check if sudo is available
    if command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
        PREFIX="/usr/local"
        info "installing system-wide to ${PREFIX} (via sudo)"
        return
    fi

    # fall back to ~/.local (no sudo needed)
    PREFIX="${HOME}/.local"
    mkdir -p "${PREFIX}/bin" 2>/dev/null || true
    info "installing to ${PREFIX} (no sudo)"
    warn "make sure ${PREFIX}/bin is in your PATH:"
    case ":${PATH}:" in
        *":${PREFIX}/bin:"*) ;;
        *) warn "  export PATH=\"${PREFIX}/bin:\$PATH\"" ;;
    esac
}

resolve_prefix

# --- resolve version ---------------------------------------------------------
if [ -z "$VERSION" ]; then
    info "fetching latest version from GitHub..."
    VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
        | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -1)"
    [ -z "$VERSION" ] && err "could not determine latest version (API rate-limited?) — use --version X.Y.Z"
fi

info "version: v${VERSION}"

# --- download ----------------------------------------------------------------
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/v${VERSION}/odm_${VERSION}_${OS}_${ARCH}.tar.gz"
CHECKSUM_URL="https://github.com/${REPO}/releases/download/v${VERSION}/checksums.txt"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

TARBALL="${TMPDIR}/odm.tar.gz"

info "downloading ${DOWNLOAD_URL}..."
curl -fSL -o "$TARBALL" "$DOWNLOAD_URL" || err "download failed — check if v${VERSION} exists for ${OS}/${ARCH}"

# verify checksum
info "verifying checksum..."
CHECKSUM_FILE="${TMPDIR}/checksums.txt"
curl -fsSL -o "$CHECKSUM_FILE" "$CHECKSUM_URL" 2>/dev/null || true

if [ -s "$CHECKSUM_FILE" ]; then
    TARBALL_NAME="odm_${VERSION}_${OS}_${ARCH}.tar.gz"
    grep "$TARBALL_NAME" "$CHECKSUM_FILE" > "${TMPDIR}/checksum.txt" 2>/dev/null || true
    if [ -s "${TMPDIR}/checksum.txt" ]; then
        (cd "$TMPDIR" && sha256_check "${TMPDIR}/checksum.txt") || err "checksum verification failed"
        ok "checksum verified"
    else
        warn "tarball not found in checksums.txt — skipping verification"
    fi
else
    warn "could not fetch checksums.txt — skipping verification"
fi

# --- extract -----------------------------------------------------------------
info "extracting..."
tar -xzf "$TARBALL" -C "$TMPDIR"

[ -f "$TMPDIR/odm" ] || err "binary 'odm' not found in tarball"

# --- confirm -----------------------------------------------------------------
INSTALL_BIN="${PREFIX}/bin/odm"

if [ "$YES" -eq 0 ] && [ -f "$INSTALL_BIN" ]; then
    EXISTING_VER="$("$INSTALL_BIN" --version 2>/dev/null || echo "unknown")"
    warn "existing installation found: ${EXISTING_VER}"
    printf "install v${VERSION} to ${INSTALL_BIN}? [y/N] "
    read -r REPLY
    case "$REPLY" in
        [yY][eE][sS]|[yY]) ;;
        *) info "aborted"; exit 0 ;;
    esac
fi

# --- install helper (auto sudo if needed) ------------------------------------
do_install() {
    _src="$1" _dst="$2" _mode="$3"
    _dir="$(dirname "$_dst")"
    if [ -w "$_dir" ] 2>/dev/null; then
        mkdir -p "$_dir"
        install -Dm"${_mode}" "$_src" "$_dst"
    elif [ -w "/usr/local" ] 2>/dev/null; then
        mkdir -p "$_dir"
        install -Dm"${_mode}" "$_src" "$_dst"
    else
        sudo mkdir -p "$_dir"
        sudo install -Dm"${_mode}" "$_src" "$_dst"
    fi
}

# --- install binary ----------------------------------------------------------
info "installing binary to ${INSTALL_BIN}..."
do_install "$TMPDIR/odm" "$INSTALL_BIN" 755

# --- install man page --------------------------------------------------------
if [ -f "$TMPDIR/odm.1" ]; then
    MAN_DIR="${PREFIX}/share/man/man1"
    info "installing man page to ${MAN_DIR}/odm.1..."
    do_install "$TMPDIR/odm.1" "${MAN_DIR}/odm.1" 644
fi

# --- install config example --------------------------------------------------
# Config goes to /etc/odm (system) or ~/.config/odm (user)
if [ "$PREFIX" = "/usr/local" ]; then
    CONFIG_DIR="/etc/odm"
else
    CONFIG_DIR="${HOME}/.config/odm"
fi

if [ ! -f "${CONFIG_DIR}/config.conf" ]; then
    info "installing config to ${CONFIG_DIR}/config.conf..."
    curl -fsSL -o "${TMPDIR}/odm.conf.example" \
        "https://raw.githubusercontent.com/${REPO}/main/configs/odm.conf.example" 2>/dev/null || true
    if [ -f "${TMPDIR}/odm.conf.example" ]; then
        do_install "${TMPDIR}/odm.conf.example" "${CONFIG_DIR}/config.conf" 644
    fi
fi

# --- done --------------------------------------------------------------------
echo ""
ok "ODM v${VERSION} installed to ${INSTALL_BIN}"
echo ""
"$INSTALL_BIN" --version 2>/dev/null || true
echo ""
info "run 'odm --help' to get started"

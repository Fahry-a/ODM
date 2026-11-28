#!/usr/bin/env bash
# Build the Go download core archive for one Rust target, if it is missing.
# Usage: scripts/build-go-core.sh <rust-target-triple>
# Single source of truth used by build.rs, CI and the release cross-build.
set -euo pipefail

TARGET="${1:-x86_64-unknown-linux-gnu}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

case "$TARGET" in
  x86_64-unknown-linux-gnu)          GOOS=linux;  GOARCH=amd64; CC_BIN="${CC:-cc}"; CXX_BIN="${CXX:-c++}"; LIB=libodmgo ;;
  x86_64-unknown-linux-musl)         GOOS=linux;  GOARCH=amd64; CC_BIN="zig cc -target x86_64-linux-musl"; CXX_BIN="zig c++ -target x86_64-linux-musl"; LIB=libodmgo_musl ;;
  aarch64-unknown-linux-musl)        GOOS=linux;  GOARCH=arm64; CC_BIN="zig cc -target aarch64-linux-musl"; CXX_BIN="zig c++ -target aarch64-linux-musl"; LIB=libodmgo_aarch64 ;;
  armv7-unknown-linux-musleabihf)    GOOS=linux;  GOARCH=arm;   CC_BIN="zig cc -target arm-linux-musleabihf"; CXX_BIN="zig c++ -target arm-linux-musleabihf"; LIB=libodmgo_armv7; export GOARM=7 ;;
  i686-unknown-linux-musl)           GOOS=linux;  GOARCH=386;   CC_BIN="zig cc -target x86-linux-musl"; CXX_BIN="zig c++ -target x86-linux-musl"; LIB=libodmgo_i686 ;;
  x86_64-apple-darwin)               GOOS=darwin; GOARCH=amd64; CC_BIN="zig cc -target x86_64-macos"; CXX_BIN="zig c++ -target x86_64-macos"; LIB=libodmgo_darwin_amd64 ;;
  aarch64-apple-darwin)              GOOS=darwin; GOARCH=arm64; CC_BIN="zig cc -target aarch64-macos"; CXX_BIN="zig c++ -target aarch64-macos"; LIB=libodmgo_darwin_arm64 ;;
  *) echo "unsupported target: $TARGET" >&2; exit 2 ;;
esac

if [ -f "$ROOT/gocore/$LIB.a" ] && [ -f "$ROOT/gocore/$LIB.h" ]; then
  exit 0
fi

mkdir -p "$ROOT/.build-tmp"
export GOTMPDIR="$ROOT/.build-tmp" TMPDIR="$ROOT/.build-tmp"
echo "[go-core] building $LIB for $GOOS/$GOARCH ($TARGET)" >&2
# VCS stamping fails when the repo is bind-mounted with a different owner (containers).
(cd "$ROOT/gocore" && CGO_ENABLED=1 CC="$CC_BIN" CXX="$CXX_BIN" GOOS="$GOOS" GOARCH="$GOARCH" \
  go build -buildvcs=false -buildmode=c-archive -o "$LIB.a" ./cmd/odmbridge)

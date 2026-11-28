#!/usr/bin/env bash
# Build odm with the embedded Go download core for a release target.
# Usage: scripts/build-static.sh <rust-target-triple>
# Linux targets produce fully static binaries (post-link constructor patch).
# macOS targets produce Mach-O binaries (Darwin init_array gets argc/argv natively).
set -euo pipefail

TARGET="${1:-x86_64-unknown-linux-musl}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

case "$TARGET" in
  x86_64-unknown-linux-musl)          GOOS=linux;  GOARCH=amd64; CC_BIN="musl-gcc"; CXX_BIN="musl-gcc"; LIB=libodmgo_musl.a; LINUX=1 ;;
  aarch64-unknown-linux-musl)         GOOS=linux;  GOARCH=arm64; CC_BIN="zig cc -target aarch64-linux-musl"; CXX_BIN="zig c++ -target aarch64-linux-musl"; LIB=libodmgo_aarch64.a; LINUX=1 ;;
  armv7-unknown-linux-musleabihf)     GOOS=linux;  GOARCH=arm;   CC_BIN="zig cc -target arm-linux-musleabihf"; CXX_BIN="zig c++ -target arm-linux-musleabihf"; LIB=libodmgo_armv7.a; LINUX=1; export GOARM=7 ;;
  i686-unknown-linux-musl)            GOOS=linux;  GOARCH=386;   CC_BIN="zig cc -target x86-linux-musl"; CXX_BIN="zig c++ -target x86-linux-musl"; LIB=libodmgo_i686.a; LINUX=1 ;;
  x86_64-apple-darwin)                GOOS=darwin; GOARCH=amd64; CC_BIN="zig cc -target x86_64-macos"; CXX_BIN="zig c++ -target x86_64-macos"; LIB=libodmgo_darwin_amd64.a; LINUX=0 ;;
  aarch64-apple-darwin)               GOOS=darwin; GOARCH=arm64; CC_BIN="zig cc -target aarch64-macos"; CXX_BIN="zig c++ -target aarch64-macos"; LIB=libodmgo_darwin_arm64.a; LINUX=0 ;;
  *) echo "unsupported target: $TARGET" >&2; exit 2 ;;
esac

mkdir -p "$ROOT/.build-tmp"
export GOTMPDIR="$ROOT/.build-tmp"
export TMPDIR="$ROOT/.build-tmp"

echo "[1/3] Go core archive ($TARGET -> $LIB)"
(cd gocore && CGO_ENABLED=1 CC="$CC_BIN" CXX="$CXX_BIN" GOOS="$GOOS" GOARCH="$GOARCH" \
  go build -buildmode=c-archive -o "$LIB" ./cmd/odmbridge)

echo "[2/3] Rust binary ($TARGET)"
if [ "$LINUX" = 1 ]; then
  export RUSTFLAGS="-C relocation-model=static -C link-arg=-no-pie -C strip=none"
fi
if [ "$TARGET" = "x86_64-unknown-linux-musl" ]; then
  CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_LINKER=musl-gcc \
    CC_x86_64_unknown_linux_musl=musl-gcc \
    cargo build --release --locked --target "$TARGET" -p odm
else
  cargo zigbuild --release --locked --target "$TARGET" -p odm
fi

BIN="target/$TARGET/release/odm"
if [ "$LINUX" = 1 ]; then
  echo "[3/3] Patch Go constructor for musl ($BIN)"
  python3 scripts/patch_go_ctor.py "$BIN"
else
  echo "[3/3] Darwin: no constructor patch needed"
fi

file "$BIN"
echo "OK: $BIN"

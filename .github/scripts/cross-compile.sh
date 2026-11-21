#!/usr/bin/env bash
# Cross-compile ODM (Rust) for the release targets into build/, packaged as
# odm_<version>_<os>_<arch>.tar.gz (binary "odm" + LICENSE inside).
#
# Uses cargo-zigbuild so one toolchain can target musl Linux and macOS; the
# macOS targets need the SDK, which the release workflow supplies via the
# ghcr.io/rust-cross/cargo-zigbuild image.
#
# Usage: cross-compile.sh <version>
#   <version>  the version being released, e.g. "1.5.2"
set -euo pipefail

VERSION="$1"

# rust target triple | artifact os | artifact arch
TARGETS=(
  "i686-unknown-linux-musl|linux|386"
  "x86_64-unknown-linux-musl|linux|amd64"
  "armv7-unknown-linux-musleabihf|linux|arm"
  "aarch64-unknown-linux-musl|linux|arm64"
  "x86_64-apple-darwin|darwin|amd64"
  "aarch64-apple-darwin|darwin|arm64"
)

mkdir -p build

for ENTRY in "${TARGETS[@]}"; do
  IFS='|' read -r TRIPLE GOOS GOARCH <<<"$ENTRY"
  TARBALL="build/odm_${VERSION}_${GOOS}_${GOARCH}.tar.gz"
  echo "Building $TARBALL ($TRIPLE) ..."
  cargo zigbuild --release --locked --target "$TRIPLE" -p odm
  STAGE="$(mktemp -d)"
  cp "target/${TRIPLE}/release/odm" "$STAGE/odm"
  cp LICENSE "$STAGE/LICENSE"
  tar -czf "$TARBALL" -C "$STAGE" odm LICENSE
  rm -rf "$STAGE"
done

echo "Build artifacts:"
ls -lh build/

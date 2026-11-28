#!/usr/bin/env bash
# Cross-compile ODM (Rust) for the release targets into build/, packaged as
# odm_<version>_<os>_<arch>.tar.gz (binary "odm" + LICENSE inside).
#
# Uses cargo-zigbuild so one toolchain can target musl Linux and macOS; the
# macOS targets need the SDK, which the release workflow supplies via the
# ghcr.io/rust-cross/cargo-zigbuild image.
#
# Usage: cross-compile.sh <version> [rust-target-triple]
#   <version>  the version being released, e.g. "1.5.2"
#   <triple>   build only this target (release matrix passes one per job)
set -euo pipefail

VERSION="$1"
FILTER="${2:-}"

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

# The Go core archives are gitignored; build them for every release target
# before Rust links against them (build.rs would do the same per target).
for ENTRY in "${TARGETS[@]}"; do
  IFS='|' read -r TRIPLE GOOS GOARCH <<<"$ENTRY"
  [ -z "$FILTER" ] || [ "$TRIPLE" = "$FILTER" ] || continue
  bash scripts/build-go-core.sh "$TRIPLE"
done

for ENTRY in "${TARGETS[@]}"; do
  IFS='|' read -r TRIPLE GOOS GOARCH <<<"$ENTRY"
  [ -z "$FILTER" ] || [ "$TRIPLE" = "$FILTER" ] || continue
  TARBALL="build/odm_${VERSION}_${GOOS}_${GOARCH}.tar.gz"
  echo "Building $TARBALL ($TRIPLE) ..."
  # musl invokes Go c-archive constructors without argc/argv; the post-link
  # patch is mandatory for every static Linux release binary.
  if [ "$GOOS" = "linux" ]; then
    # strip=none keeps the odm_go_ctor_* symbols the patcher rewrites.
    export RUSTFLAGS="-C relocation-model=static -C link-arg=-no-pie -C strip=none"
  else
    unset RUSTFLAGS
  fi
  cargo zigbuild --release --locked --target "$TRIPLE" -p odm
  if [ "$GOOS" = "linux" ]; then
    python3 scripts/patch_go_ctor.py "target/${TRIPLE}/release/odm"
  fi
  STAGE="$(mktemp -d)"
  cp "target/${TRIPLE}/release/odm" "$STAGE/odm"
  cp LICENSE "$STAGE/LICENSE"
  tar -czf "$TARBALL" -C "$STAGE" odm LICENSE
  rm -rf "$STAGE"
done

echo "Build artifacts:"
ls -lh build/

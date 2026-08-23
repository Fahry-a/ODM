#!/usr/bin/env bash
# Cross-compile ODM for the release targets into build/, packaged as
# odm_<version>_<os>_<arch>.tar.gz (binary "odm" + LICENSE inside).
#
# Usage: cross-compile.sh <version>
#   <version>  the version being released, e.g. "1.5.2"
set -euo pipefail

VERSION="$1"
LDFLAGS="-s -w -buildid="
export CGO_ENABLED=0
export GOFLAGS="-trimpath -mod=readonly"
export GOTOOLCHAIN=local

TARGETS="linux/386 linux/amd64 linux/arm linux/arm64 darwin/amd64 darwin/arm64"

mkdir -p build

for TARGET in $TARGETS; do
  GOOS="${TARGET%/*}"
  GOARCH="${TARGET#*/}"
  TARBALL="build/odm_${VERSION}_${GOOS}_${GOARCH}.tar.gz"
  echo "Building $TARBALL ..."
  STAGE="$(mktemp -d)"
  GOOS="$GOOS" GOARCH="$GOARCH" go build -ldflags="$LDFLAGS" -o "$STAGE/odm" ./cmd/odm
  cp LICENSE "$STAGE/LICENSE"
  # Binary keeps its exec bit; files are owned by the invoking user.
  tar -czf "$TARBALL" -C "$STAGE" odm LICENSE
  rm -rf "$STAGE"
done

echo "Build artifacts:"
ls -lh build/

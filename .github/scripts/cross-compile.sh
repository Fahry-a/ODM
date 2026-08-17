#!/usr/bin/env bash
# Cross-compile ODM for the release targets into build/.
#
# Usage: cross-compile.sh <version>
#   <version>  the version being released, e.g. "1.4.2"
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
  OUTPUT="build/odm_${VERSION}_${GOOS}_${GOARCH}"
  echo "Building $OUTPUT ..."
  GOOS="$GOOS" GOARCH="$GOARCH" go build -ldflags="$LDFLAGS" -o "$OUTPUT" ./cmd/odm
done

echo "Build artifacts:"
ls -lh build/

#!/usr/bin/env bash
# Verify that the given version matches the two version-bearing files.
# Used by release.yml, auto-tag.yml and ci.yml so the check lives in one place.
#
# Usage: verify-version.sh <version>
# Exit 0 if all match, 1 with ::error:: diagnostics otherwise.
set -euo pipefail

EXPECTED="$1"
VERSION_GO=$(grep -oP 'const Version = "odm/\K[^"]+' internal/version/version.go)
PKGBUILD_VER=$(grep -oP '^pkgver=\K.*' packaging/PKGBUILD)

FAIL=0
if [ "$EXPECTED" != "$VERSION_GO" ]; then
  echo "::error::Version mismatch: expected=$EXPECTED version.go=$VERSION_GO"
  FAIL=1
fi
if [ "$EXPECTED" != "$PKGBUILD_VER" ]; then
  echo "::error::Version mismatch: expected=$EXPECTED pkgbuild=$PKGBUILD_VER"
  FAIL=1
fi
if [ "$FAIL" -eq 1 ]; then
  echo "::error::Bump both internal/version/version.go and packaging/PKGBUILD together."
  exit 1
fi

echo "Versions in sync: $EXPECTED"

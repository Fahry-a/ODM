#!/usr/bin/env bash
# Check whether a tag for VERSION should be created: skip if it exists or if
# the version is not newer than the latest tag. Writes "skip" to $GITHUB_OUTPUT.
#
# Usage: tag-check.sh <version>
set -euo pipefail

VERSION="$1"
LATEST=$(git tag -l 'v*' | sort -V | tail -1 || true)
echo "version=$VERSION"
echo "latest=$LATEST"

echo "skip=false" >> "$GITHUB_OUTPUT"
if git rev-parse "v$VERSION" >/dev/null 2>&1; then
  echo "Tag v$VERSION already exists — nothing to do"
  echo "skip=true" >> "$GITHUB_OUTPUT"
elif [ -n "$LATEST" ] && [ "$(printf '%s\n%s' "$VERSION" "${LATEST#v}" | sort -V | tail -1)" = "${LATEST#v}" ]; then
  echo "Version $VERSION is not newer than the latest tag $LATEST — nothing to do"
  echo "skip=true" >> "$GITHUB_OUTPUT"
fi

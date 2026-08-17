#!/usr/bin/env bash
# Extract the release notes for a version from CHANGELOG.md, write them to
# /tmp/release-notes.md. Fails loudly if the version has no entry.
#
# Usage: extract-changelog.sh <version>
set -euo pipefail

VERSION="$1"

NOTES=$(awk -v ver="$VERSION" '
  /^## \[/ {
    if (found) exit
    gsub(/^## \[/, ""); gsub(/\].*/, "")
    if ($0 == ver) found=1
  }
  found { print }
' CHANGELOG.md)

if [ -z "$NOTES" ]; then
  echo "::error::No changelog entry found for $VERSION in CHANGELOG.md"
  echo "::error::Add a '## [$VERSION]' section before releasing (see AGENTS.md)."
  exit 1
fi

echo "$NOTES" > /tmp/release-notes.md
echo "Release notes extracted for $VERSION:"
cat /tmp/release-notes.md

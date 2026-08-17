#!/usr/bin/env bash
# Verify the CHANGELOG has an entry for the version, then create + push the
# annotated tag. Exit non-zero if the changelog entry is missing.
#
# Usage: tag-create.sh <version>
set -euo pipefail

VERSION="$1"

if ! grep -qE "^## \[$VERSION\]" CHANGELOG.md; then
  echo "::error::CHANGELOG.md has no '## [$VERSION]' entry"
  echo "::error::Add the release notes for v$VERSION before pushing the version bump."
  exit 1
fi
echo "CHANGELOG.md has an entry for $VERSION"

# git tag -a needs a committer identity, which bare runners don't have.
git config user.name "github-actions[bot]"
git config user.email "github-actions[bot]@users.noreply.github.com"
git tag -a "v$VERSION" -m "release: v$VERSION"
git push origin "v$VERSION"
echo "Created and pushed tag v$VERSION"

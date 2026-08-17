#!/usr/bin/env bash
# Dispatch release.yml for the tagged version. A tag pushed with GITHUB_TOKEN
# does NOT trigger other workflows (GitHub's recursion guard), so the release
# must be dispatched explicitly; workflow_dispatch is one of the few events
# GITHUB_TOKEN is allowed to raise. Dispatch on the tag ref itself so Release
# builds the exact tagged source, not main's latest.
#
# Usage: tag-trigger.sh <version>
set -euo pipefail

VERSION="$1"
gh workflow run release.yml -f "tag=$VERSION" --ref "v$VERSION"
echo "Dispatched release.yml for v$VERSION"

#!/usr/bin/env bash
# Extract the version being released from the event that triggered the run.
# Used by release.yml and auto-tag.yml (which pass the same inputs).
#
# Usage: extract-version.sh <event_name> [workflow_dispatch_tag]
#   <event_name>            "workflow_dispatch" or anything else (tag push)
#   [workflow_dispatch_tag] the inputs.tag value when event is workflow_dispatch
# Prints the version (no "v" prefix) to stdout.
set -euo pipefail

EVENT="$1"
DISPATCH_TAG="${2:-}"

if [ "$EVENT" = "workflow_dispatch" ]; then
  TAG="$DISPATCH_TAG"
  [ -n "$TAG" ] || { echo "::error::workflow_dispatch requires inputs.tag" >&2; exit 1; }
else
  TAG="${GITHUB_REF#refs/tags/v}"
fi

VERSION="${TAG#v}"
echo "$VERSION"

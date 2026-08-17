#!/usr/bin/env bash
# Resolve the AUR publish mode (release vs republish), version, tag, and
# upstream state. Writes the result to $GITHUB_OUTPUT.
#
# Usage: prepare.sh <event_name> [workflow_dispatch_tag]
#   <event_name>            "workflow_dispatch" (release) or anything else
#   [workflow_dispatch_tag] inputs.tag when event is workflow_dispatch
set -euo pipefail

EVENT="$1"
INPUT_TAG="${2:-}"

MODE="republish"
if [ "$EVENT" = "workflow_dispatch" ]; then
  MODE="release"
fi
echo "mode=$MODE" >> "$GITHUB_OUTPUT"

PKGNAME=$(grep -oP '^pkgname=\K.*' packaging/PKGBUILD)

if [ "$MODE" = "release" ]; then
  TAG="$INPUT_TAG"
  if ! [[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "::error::Invalid tag format: $TAG (expected vX.Y.Z)"
    exit 1
  fi
  VERSION="${TAG#v}"
else
  VERSION=$(grep -oP '^pkgver=\K.*' packaging/PKGBUILD)
  if [ -z "$VERSION" ]; then
    echo "::error::Could not read pkgver from packaging/PKGBUILD"
    exit 1
  fi
  TAG="v$VERSION"

  # Skip when this version was never released by the pipeline — a new version
  # goes through auto-tag → release.yml instead.
  if ! git rev-parse --verify "refs/tags/$TAG" >/dev/null 2>&1; then
    echo "No tag $TAG (new version) — release pipeline handles this; skipping"
    echo "skip=true" >> "$GITHUB_OUTPUT"
    exit 0
  fi

  # Compare against upstream AUR. Identical PKGBUILD = nothing to publish;
  # this is the defensive loop guard (on top of GitHub's recursion guard for
  # GITHUB_TOKEN commit-backs).
  AUR_DIR="$(mktemp -d)"
  if git clone -q --depth=1 "https://aur.archlinux.org/${PKGNAME}.git" "$AUR_DIR" 2>/dev/null; then
    if cmp -s packaging/PKGBUILD "$AUR_DIR/PKGBUILD"; then
      echo "Local PKGBUILD identical to upstream AUR — nothing to publish; skipping"
      echo "skip=true" >> "$GITHUB_OUTPUT"
      exit 0
    fi
    echo "up_pkgver=$(grep -oP '^pkgver=\K.*' "$AUR_DIR/PKGBUILD" || true)" >> "$GITHUB_OUTPUT"
    echo "up_pkgrel=$(grep -oP '^pkgrel=\K.*' "$AUR_DIR/PKGBUILD" || true)" >> "$GITHUB_OUTPUT"
  else
    echo "Upstream AUR clone failed (never published?) — publishing as-is"
    echo "up_pkgver=" >> "$GITHUB_OUTPUT"
    echo "up_pkgrel=" >> "$GITHUB_OUTPUT"
  fi
fi

echo "tag=$TAG" >> "$GITHUB_OUTPUT"
echo "version=$VERSION" >> "$GITHUB_OUTPUT"
echo "Mode=$MODE version=$VERSION tag=$TAG"

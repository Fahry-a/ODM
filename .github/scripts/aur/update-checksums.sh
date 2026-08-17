#!/usr/bin/env bash
# Fetch the release's checksums.txt and update the PKGBUILD's sha256sums.
# update-aur-checksums.py opens 'PKGBUILD' relative to the cwd, so run from
# packaging/ (the caller sets working-directory accordingly).
#
# Usage: update-checksums.sh <repo> <version>
set -euo pipefail

REPO="$1"
VERSION="$2"

curl -fsSL -o /tmp/checksums.txt \
  "https://github.com/${REPO}/releases/download/v${VERSION}/checksums.txt"

I686_SUM=$(awk -v file="odm_${VERSION}_linux_386" '$2 == file { print $1; exit }' /tmp/checksums.txt)
AMD64_SUM=$(awk -v file="odm_${VERSION}_linux_amd64" '$2 == file { print $1; exit }' /tmp/checksums.txt)
ARMV7H_SUM=$(awk -v file="odm_${VERSION}_linux_arm" '$2 == file { print $1; exit }' /tmp/checksums.txt)
ARM64_SUM=$(awk -v file="odm_${VERSION}_linux_arm64" '$2 == file { print $1; exit }' /tmp/checksums.txt)

if [ -z "$I686_SUM" ] || [ -z "$AMD64_SUM" ] || [ -z "$ARMV7H_SUM" ] || [ -z "$ARM64_SUM" ]; then
  echo "::error::Missing binary checksum in release artifacts"
  exit 1
fi

export I686_SUM AMD64_SUM ARMV7H_SUM ARM64_SUM
python3 ../.github/scripts/update-aur-checksums.py

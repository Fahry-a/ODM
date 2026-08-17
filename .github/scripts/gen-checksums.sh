#!/usr/bin/env bash
# Generate SHA-256 checksums for the release binaries into build/checksums.txt.
#
# Usage: gen-checksums.sh <version>
set -euo pipefail

VERSION="$1"
cd build
sha256sum odm_${VERSION}_* > checksums.txt
echo "Checksums:"
cat checksums.txt

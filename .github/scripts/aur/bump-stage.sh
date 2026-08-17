#!/usr/bin/env bash
# Bump pkgver in the PKGBUILD and stage the bundled assets for the AUR action.
#
# Usage: bump-stage.sh <version>
set -euo pipefail

VERSION="$1"

# Only pkgver is set here; pkgrel is handled by the AUR action (pkgrel_mode:
# auto), and `version` makes it bundle + version the assets from the stable
# paths below.
sed -i "s/^pkgver=.*/pkgver=${VERSION}/" packaging/PKGBUILD
mkdir -p assets
cp docs/odm.1 assets/odm.1
cp configs/odm.conf.example assets/odm.conf.example
cp packaging/odm.service assets/odm.service
cp LICENSE assets/LICENSE

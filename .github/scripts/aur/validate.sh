#!/usr/bin/env bash
# Sanity-check the PKGBUILD before publishing: pkgname + pkgver must be right.
#
# Usage: validate.sh <version>
set -euo pipefail

VERSION="$1"
errors=0
grep -q "^pkgname=odm-bin$" packaging/PKGBUILD || { echo "::error::Missing pkgname"; errors=1; }
grep -q "^pkgver=${VERSION}$" packaging/PKGBUILD || { echo "::error::Wrong pkgver"; errors=1; }
[ "$errors" -eq 0 ] || exit 1

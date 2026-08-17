#!/usr/bin/env bash
# Stage the bundled assets under the names PKGBUILD source= expects, verify
# them via makepkg in an archlinux container, and regenerate .SRCINFO.
#
# Usage: verify-srcinfo.sh <version>
set -euo pipefail

VERSION="$1"

# PKGBUILD's source= references LOCAL assets as odm-bin-<pkgver>.<ext>.
# makepkg --verifysource needs them present in the build dir (they are not
# URLs), so stage them into packaging/ with those exact names before
# verifying. They are transient — publish happens from assets/ via
# aur-sync-action, and the caller's cleanup removes them. PKGBUILD source=
# names these EXACTLY as aur-sync-action stages them: <pkgname><ext>-
# <version><ext>'s version is inserted before the LAST extension by
# version_suffix. `.1`→`odm-bin-1.4.0.1`, `.conf.example`→
# `odm-bin.conf-1.4.0.example`, `.service`→`odm-bin-1.4.0.service`,
# `.LICENSE`→`odm-bin-1.4.0.LICENSE`.
cp assets/odm.1 "packaging/odm-bin-${VERSION}.1"
cp assets/odm.conf.example "packaging/odm-bin.conf-${VERSION}.example"
cp assets/odm.service "packaging/odm-bin-${VERSION}.service"
cp assets/LICENSE "packaging/odm-bin-${VERSION}.LICENSE"

docker run --rm -v "$PWD/packaging:/pkg" -w /pkg archlinux:base-devel bash -c "
  useradd -m builder
  chown -R builder:builder /pkg
  su builder -c 'makepkg --verifysource --nodeps'
  su builder -c 'makepkg --printsrcinfo' > .SRCINFO
"

# The archlinux container chowns packaging/ to its 'builder' user, so the
# runner (non-root) can't remove the staged files — use sudo like the
# cleanup step does.
sudo rm -f "packaging/odm-bin-${VERSION}.1" \
      "packaging/odm-bin.conf-${VERSION}.example" \
      "packaging/odm-bin-${VERSION}.service" \
      "packaging/odm-bin-${VERSION}.LICENSE"

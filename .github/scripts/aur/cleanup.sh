#!/usr/bin/env bash
# Remove transient AUR build artifacts so the commit-to-main only carries the
# PKGBUILD edit.
set -euo pipefail

rm -rf assets
sudo rm -rf packaging/.SRCINFO packaging/odm-bin-*.* 2>/dev/null || true

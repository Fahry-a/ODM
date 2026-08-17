#!/usr/bin/env bash
# Commit the bumped PKGBUILD back to main (AUR workflow commit-back).
#
# Usage: commit-main.sh <mode> <version>
#   <mode>     "release" or "republish" (affects the commit message)
#   <version>  the version being published
set -euo pipefail

MODE="$1"
VERSION="$2"

# The archlinux container (Verify step) chowns packaging/ to its 'builder'
# user; restore runner ownership before rewriting files.
sudo chown -R "$(id -u):$(id -g)" packaging

git config user.name "github-actions[bot]"
git config user.email "github-actions[bot]@users.noreply.github.com"
git remote set-url origin "https://x-access-token:${GITHUB_TOKEN}@github.com/${GITHUB_REPOSITORY}.git"
git fetch origin main

# Rebase the PKGBUILD edits onto the latest main, then push explicitly. A bare
# `git push` only works on a branch checkout; the default checkout here is
# detached, so an explicit branch+refspec is required to land the commit on
# main. Snapshot the bumped PKGBUILD: if a queued prior republish already
# committed a pkgrel bump to main, `git checkout -B` would refuse to overwrite
# our working-tree edit. Restore it after the checkout.
cp packaging/PKGBUILD /tmp/PKGBUILD.bumped
git checkout -B aur-publish origin/main
cp /tmp/PKGBUILD.bumped packaging/PKGBUILD
git add packaging/PKGBUILD
if ! git diff --cached --quiet; then
  COMMIT_MSG="chore(aur): update PKGBUILD for v${VERSION}"
  if [ "$MODE" = "republish" ]; then
    COMMIT_MSG="chore(aur): bump pkgrel for v${VERSION}"
  fi
  git commit -m "$COMMIT_MSG"
  git push --force-with-lease origin aur-publish:main
else
  echo "No changes to commit"
fi

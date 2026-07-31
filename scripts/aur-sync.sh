#!/usr/bin/env bash
# aur-sync.sh — publish PKGBUILD + versioned assets to the AUR, pruning stale
# versioned files so the AUR repo never accumulates old-version artifacts.
#
# Run from the packaging/ directory:  bash ../scripts/aur-sync.sh <version>
#
# Env:
#   AUR_SSH_PRIVATE_KEY  (required) SSH private key with AUR push access
#   AUR_COMMIT_USERNAME  (required) commit author name
#   AUR_COMMIT_EMAIL     (required) commit author email
#
# Why this exists: the third-party publish action we replaced
# (ulises-jeremias/github-actions-aur-publish) only ever copied files into the
# AUR repo and never removed stale ones, so every release left the previous
# version's odm-bin-<ver>.1/.conf.example/.service behind. This script removes
# ALL versioned assets (plus any unversioned leak such as odm.service) first,
# then copies the current set in, and pushes a single atomic commit — no window
# where the AUR is missing its sources.
set -euo pipefail

VERSION="${1:?usage: aur-sync.sh <version>}"
PKGNAME="odm-bin"
SRC_DIR="$(pwd)"
ASSETS=(
    "${PKGNAME}-${VERSION}.1"
    "${PKGNAME}-${VERSION}.conf.example"
    "${PKGNAME}-${VERSION}.service"
)

# Every file to publish must exist — they are produced by earlier workflow
# steps (pkgver sed, copy bundled files, docker makepkg --printsrcinfo).
for f in PKGBUILD .SRCINFO "${ASSETS[@]}"; do
    if [[ ! -f "$f" ]]; then
        echo "::error::missing $f (run aur-sync.sh from packaging/ after all prep steps)" >&2
        exit 1
    fi
done

for v in AUR_SSH_PRIVATE_KEY AUR_COMMIT_USERNAME AUR_COMMIT_EMAIL; do
    if [[ -z "${!v:-}" ]]; then
        echo "::error::$v is not set" >&2
        exit 1
    fi
done

# SSH identity for the push.
mkdir -p "$HOME/.ssh"
chmod 700 "$HOME/.ssh"
ssh-keyscan -t rsa,ecdsa,ed25519 aur.archlinux.org >>"$HOME/.ssh/known_hosts" 2>/dev/null || true
umask 077
printf '%s\n' "$AUR_SSH_PRIVATE_KEY" >"$HOME/.ssh/aur_key"
chmod 600 "$HOME/.ssh/aur_key"
export GIT_SSH_COMMAND="ssh -i $HOME/.ssh/aur_key -o IdentitiesOnly=yes"

AUR_DIR="$(mktemp -d)"
trap 'rm -rf "$AUR_DIR"' EXIT

git clone -q "ssh://aur@aur.archlinux.org/${PKGNAME}.git" "$AUR_DIR"
cd "$AUR_DIR"

# Prune every stale versioned asset (and any unversioned leak), tolerating a
# repo where none exist yet. Globs expand against the cloned repo.
git rm -q --ignore-unmatch \
    "${PKGNAME}-"*.1 \
    "${PKGNAME}-"*.conf.example \
    "${PKGNAME}-"*.service \
    "${PKGNAME}.service" \
    || true

# Copy the verified artifacts in.
cp -v "$SRC_DIR/PKGBUILD" "$SRC_DIR/.SRCINFO" ./
for a in "${ASSETS[@]}"; do
    cp -v "$SRC_DIR/$a" ./
done

git add --all
if git diff --cached --quiet; then
    echo "No changes to publish"
    exit 0
fi

git config user.name "$AUR_COMMIT_USERNAME"
git config user.email "$AUR_COMMIT_EMAIL"
git commit -q -m "chore(aur): update to v${VERSION}"
git push -q origin master
echo "Published ${PKGNAME} v${VERSION} to AUR"

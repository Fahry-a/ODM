#!/usr/bin/env bash
# Build the per-release contributors list and append it to the release notes
# (/tmp/release-notes.md). Bots and the maintainer's own aliases are excluded.
#
# Usage: build-contributors.sh <version>
#   <version>  the version being released, e.g. "1.4.2"
# Writes to /tmp/release-notes.md (appends a "## Contributors" section).
set -euo pipefail

TAG="v$1"

# Per-release contributors: everyone who committed since the previous v* tag
# (or the whole history for the first release).
PREV=$(git tag -l 'v*' | sort -V | grep -v "^$TAG$" | tail -1 || true)
if [ -n "$PREV" ]; then
  AUTHORS=$(git log --format='%an <%ae>' "$PREV..$TAG" 2>/dev/null || true)
else
  AUTHORS=$(git log --format='%an <%ae>' 2>/dev/null || true)
fi
[ -n "$AUTHORS" ] || { echo "No commits for $TAG — skipping contributors"; exit 0; }

# Split "Name <email>" into name + email, dedupe by email, drop bots and the
# maintainer's own aliases (Fahry-a / farhanz share an email), keep first name.
NAMES=$(echo "$AUTHORS" | sed 's/ <\(.*\)>$/|\1/' | awk -F'|' '
  $0 ~ /bot/ { next }
  $2 ~ /farhannzarm@gmail.com/ { next }
  { email = $2; name = $1; if (!(email in n)) { n[email] = name } }
  END { for (e in n) print n[e] }
' | sort -u)
[ -n "$NAMES" ] || { echo "No human contributors — skipping"; exit 0; }

# GitHub usernames are @mention-able; fall back to the commit name.
# Only names shaped like a GitHub username go to the API — the raw git author
# Only names shaped like a GitHub username go to the API — the raw git author
while IFS= read -r name; do
  [ -z "$name" ] && continue
  if [[ ! "$name" =~ ^[a-zA-Z0-9]([a-zA-Z0-9-]{0,37}[a-zA-Z0-9])?$ ]]; then
    echo "- $name"
    continue
  fi
  user=$(gh api "users/${name}" --jq '.login' 2>/dev/null || true)
  if [ -n "$user" ]; then
    echo "- @$user"
  else
    echo "- $name"
  fi
done <<< "$NAMES" > /tmp/contributor-lines.md

{
  echo ""
  echo "---"
  echo ""
  echo "## Contributors"
  echo ""
  cat /tmp/contributor-lines.md
} >> /tmp/release-notes.md
rm -f /tmp/contributor-lines.md

echo "Appended contributors for $TAG:"
tail -10 /tmp/release-notes.md

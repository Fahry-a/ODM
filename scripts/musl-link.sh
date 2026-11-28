#!/usr/bin/env bash
# musl linker wrapper: link, then fix Go c-archive's constructor (see
# scripts/patch_go_ctor.py). musl calls .init_array with no args, so an
# unpatched embedded-Go binary segfaults as soon as the Go runtime is used.
set -euo pipefail

real_linker="${ODM_MUSL_LINKER:-musl-gcc}"
"$real_linker" "$@"

out=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then out="$arg"; break; fi
  prev="$arg"
done
if [ -n "$out" ] && [ -f "$out" ]; then
  # No `grep -q` in a pipefail pipeline: nm would get SIGPIPE and the `if`
  # would silently skip the patch.
  syms="$(nm "$out" 2>/dev/null || true)"
  case "$syms" in
    *odm_go_ctor_shim*)
      python3 "$(dirname "$0")/patch_go_ctor.py" "$out" >/dev/null 2>&1 \
        || { echo "musl-link: Go constructor patch failed for $out" >&2; exit 1; }
      ;;
  esac
fi

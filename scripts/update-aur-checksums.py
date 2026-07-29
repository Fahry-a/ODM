#!/usr/bin/env python3
"""Update PKGBUILD sha256sums.

When BUNDLED=1 (default), only the binary checksum is real; local files
(man, config, service) use SKIP since they are bundled as AUR assets.
When BUNDLED=0, all 4 checksums per arch are computed from env vars
(legacy mode for remote-source builds).
"""
import os, re, sys

PATTERN_X86 = r"sha256sums_x86_64=\([^)]+\)"
PATTERN_AARCH64 = r"sha256sums_aarch64=\([^)]+\)"

def main():
    bundled = os.environ.get('BUNDLED', '1') != '0'

    amd64 = os.environ.get('AMD64_SUM', '')
    arm64 = os.environ.get('ARM64_SUM', '')
    if not amd64 or not arm64:
        print("Missing AMD64_SUM or ARM64_SUM", file=sys.stderr)
        sys.exit(1)

    if bundled:
        man = 'SKIP'
        conf = 'SKIP'
        service = 'SKIP'
    else:
        for var in ['MAN_SUM', 'CONF_SUM', 'SERVICE_SUM']:
            if var not in os.environ:
                print(f"Missing env var: {var}", file=sys.stderr)
                sys.exit(1)
        man = os.environ['MAN_SUM']
        conf = os.environ['CONF_SUM']
        service = os.environ['SERVICE_SUM']

    path = 'PKGBUILD'
    text = open(path).read()

    if not re.search(PATTERN_X86, text):
        print("sha256sums_x86_64 pattern not found in PKGBUILD", file=sys.stderr)
        sys.exit(1)
    if not re.search(PATTERN_AARCH64, text):
        print("sha256sums_aarch64 pattern not found in PKGBUILD", file=sys.stderr)
        sys.exit(1)

    x86_new = f"""sha256sums_x86_64=('{amd64}'
                    '{man}'
                    '{conf}'
                    '{service}')"""
    aarch64_new = f"""sha256sums_aarch64=('{arm64}'
                    '{man}'
                    '{conf}'
                    '{service}')"""

    text = re.sub(PATTERN_X86, x86_new, text, count=1)
    text = re.sub(PATTERN_AARCH64, aarch64_new, text, count=1)

    open(path, 'w').write(text)
    print(f"Checksums updated (bundled={bundled})")

if __name__ == '__main__':
    main()

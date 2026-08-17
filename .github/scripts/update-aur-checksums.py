#!/usr/bin/env python3
"""Update PKGBUILD sha256sums.

When BUNDLED=1 (default), only the binary checksum is real; local files
(man, config, service, LICENSE) use SKIP since they are bundled as AUR
assets. When BUNDLED=0, all 5 checksums per arch are computed from env vars
(legacy mode for remote-source builds).
"""
import os
import re
import sys

# Arch name (as used in the PKGBUILD) -> regex matching its sha256sums block.
PATTERNS = {
    'i686': r"sha256sums_i686=\([^)]+\)",
    'x86_64': r"sha256sums_x86_64=\([^)]+\)",
    'armv7h': r"sha256sums_armv7h=\([^)]+\)",
    'aarch64': r"sha256sums_aarch64=\([^)]+\)",
}

# Arch name -> env var holding its binary checksum.
ENV_VARS = {
    'i686': 'I686_SUM',
    'x86_64': 'AMD64_SUM',
    'armv7h': 'ARMV7H_SUM',
    'aarch64': 'ARM64_SUM',
}

def main():
    bundled = os.environ.get('BUNDLED', '1') != '0'

    sums: dict[str, str] = {}
    missing: list[str] = []
    for arch, var in ENV_VARS.items():
        value = os.environ.get(var, '')
        if not value:
            missing.append(var)
        sums[arch] = value
    if missing:
        print(f"Missing env var(s): {', '.join(missing)}", file=sys.stderr)
        sys.exit(1)

    if bundled:
        man = 'SKIP'
        conf = 'SKIP'
        service = 'SKIP'
        license = 'SKIP'
    else:
        for var in ['MAN_SUM', 'CONF_SUM', 'SERVICE_SUM', 'LICENSE_SUM']:
            if var not in os.environ:
                print(f"Missing env var: {var}", file=sys.stderr)
                sys.exit(1)
        man = os.environ['MAN_SUM']
        conf = os.environ['CONF_SUM']
        service = os.environ['SERVICE_SUM']
        license = os.environ['LICENSE_SUM']

    path = 'PKGBUILD'
    with open(path, encoding='utf-8') as f:
        text = f.read()

    for arch, pattern in PATTERNS.items():
        if not re.search(pattern, text):
            print(f"sha256sums_{arch} pattern not found in PKGBUILD", file=sys.stderr)
            sys.exit(1)

    for arch, pattern in PATTERNS.items():
        block = f"""sha256sums_{arch}=('{sums[arch]}'
                    '{man}'
                    '{conf}'
                    '{service}'
                    '{license}')"""
        text = re.sub(pattern, block, text, count=1)

    with open(path, 'w', encoding='utf-8') as f:
        f.write(text)
    print(f"Checksums updated (bundled={bundled})")

if __name__ == '__main__':
    main()

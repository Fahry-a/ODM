#!/usr/bin/env python3
import os, re, sys

PATTERN_X86 = r"sha256sums_x86_64=\([^)]+\)"
PATTERN_AARCH64 = r"sha256sums_aarch64=\([^)]+\)"

def main():
    required = ['AMD64_SUM', 'ARM64_SUM', 'MAN_SUM', 'CONF_SUM', 'SERVICE_SUM']
    for var in required:
        if var not in os.environ:
            print(f"Missing env var: {var}", file=sys.stderr)
            sys.exit(1)

    amd64 = os.environ['AMD64_SUM']
    arm64 = os.environ['ARM64_SUM']
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

    # Extract version from PKGBUILD for versioned source filenames
    pkgver_match = re.search(r'^pkgver=(\S+)', text, re.MULTILINE)
    pkgver = pkgver_match.group(1) if pkgver_match else ''

    # Replace unversioned source references with versioned ones inside checksums
    source_x86_raw = f"""sha256sums_x86_64=('{amd64}'
                    '{man}'
                    '{conf}'
                    '{service}')"""
    source_aarch64_raw = f"""sha256sums_aarch64=('{arm64}'
                    '{man}'
                    '{conf}'
                    '{service}')"""

    if pkgver:
        # The checksums in source arrays reference files like odm-bin-1.1.0.1
        # We use the same ordering as the source arrays in PKGBUILD
        source_x86_raw = f"""sha256sums_x86_64=('{amd64}'
                    '{man}'
                    '{conf}'
                    '{service}')"""
        source_aarch64_raw = f"""sha256sums_aarch64=('{arm64}'
                    '{man}'
                    '{conf}'
                    '{service}')"""

    text = re.sub(PATTERN_X86, source_x86_raw, text, count=1)
    text = re.sub(PATTERN_AARCH64, source_aarch64_raw, text, count=1)

    open(path, 'w').write(text)

if __name__ == '__main__':
    main()

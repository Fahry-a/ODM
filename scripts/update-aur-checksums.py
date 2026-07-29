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

    x86_64_new = f"""sha256sums_x86_64=('{amd64}'
                    '{man}'
                    '{conf}'
                    '{service}')"""

    aarch64_new = f"""sha256sums_aarch64=('{arm64}'
                    '{man}'
                    '{conf}'
                    '{service}')"""

    text = re.sub(PATTERN_X86, x86_64_new, text, count=1)
    text = re.sub(PATTERN_AARCH64, aarch64_new, text, count=1)

    open(path, 'w').write(text)

if __name__ == '__main__':
    main()

#!/usr/bin/env python3
"""Post-link fix for Go c-archive + musl.

musl calls .init_array constructors with no arguments, but Go's c-archive
runtime constructor expects (argc, argv, envp) like glibc. Rewrite the Go
constructor slot to a C shim that computes argc/argv itself and stores the
original constructor address in a global for the shim to call.

Usage: patch_go_ctor.py <binary>
"""
import re
import subprocess
import sys

path = sys.argv[1]
syms = {}
for line in subprocess.check_output(["nm", "-n", path], text=True).splitlines():
    parts = line.split()
    if len(parts) >= 3:
        try:
            syms[parts[2]] = int(parts[0], 16)
        except ValueError:
            pass

shim = syms.get("odm_go_ctor_shim")
ptr = syms.get("odm_go_ctor_ptr")
if shim is None or ptr is None:
    raise SystemExit("missing odm_go_ctor_shim / odm_go_ctor_ptr symbols")

sections = {}
out = subprocess.check_output(["readelf", "-S", "-W", path], text=True)
for line in out.splitlines():
    m = re.match(r"\s*\[\s*\d+\]\s+(\S+)\s+\S+\s+([0-9a-fA-F]+)\s+([0-9a-fA-F]+)\s+([0-9a-fA-F]+)", line)
    if m:
        sections[m.group(1)] = (int(m.group(2), 16), int(m.group(3), 16), int(m.group(4), 16))


def file_off(addr):
    for name, (a, off, size) in sections.items():
        if a and a <= addr < a + size and name not in (".bss",):
            return off + (addr - a)
    raise SystemExit("no file offset for 0x%x" % addr)


hdr = subprocess.check_output(["readelf", "-h", path], text=True)
ptr_size = 4 if "ELF32" in hdr else 8
endian = "little"  # all CI targets are little-endian

ia_addr, ia_off, ia_size = sections[".init_array"]
ctor = None
with open(path, "r+b") as f:
    f.seek(ia_off)
    entries = f.read(ia_size)
    for i in range(0, len(entries), ptr_size):
        val = int.from_bytes(entries[i:i + ptr_size], endian)
        # The Go trampoline is named _rt0_<goos>_<arch>_lib (e.g. _rt0_amd64_linux_lib).
        for name, addr in syms.items():
            if re.fullmatch(r"_rt0_[a-z0-9_]*(linux_)?lib", name) and addr == val:
                ctor = (name, addr, i)
                break
        if ctor:
            break
    if ctor is None:
        raise SystemExit("Go constructor not found in .init_array")

    name, addr, idx = ctor
    f.seek(ia_off + idx)
    f.write(shim.to_bytes(ptr_size, endian))
    f.seek(file_off(ptr))
    f.write(addr.to_bytes(ptr_size, endian))

print("patched %s: %s@0x%x -> shim@0x%x, original stored in odm_go_ctor_ptr" % (path, name, addr, shim))

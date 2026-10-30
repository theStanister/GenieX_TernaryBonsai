#!/usr/bin/env python3
"""Generate a MinGW import library (.def) for a Windows DLL.

The CLI links against the GenieX SDK through cgo. Bazel supplies the import
library via `cdeps`, but a plain `go build` needs one on disk. This script
reads the DLL's PE export table and writes a linker definition file that
`dlltool -d <def> -l <lib>` can turn into an import library.

Usage:
    python make_import_lib.py <dll> <out.def>
"""

import os
import struct
import sys


def read_export_names(dll_path):
    with open(dll_path, "rb") as fh:
        data = fh.read()

    if data[:2] != b"MZ":
        raise SystemExit(f"{dll_path}: not a PE image (missing MZ)")

    pe_off = struct.unpack_from("<I", data, 0x3C)[0]
    if data[pe_off:pe_off + 4] != b"PE\0\0":
        raise SystemExit(f"{dll_path}: not a PE image (missing PE signature)")

    coff = pe_off + 4
    n_sections = struct.unpack_from("<H", data, coff + 2)[0]
    opt_size = struct.unpack_from("<H", data, coff + 16)[0]
    opt = coff + 20
    magic = struct.unpack_from("<H", data, opt)[0]
    pe32plus = magic == 0x20B

    export_rva, export_size = 0, 0
    if pe32plus:
        export_rva = struct.unpack_from("<I", data, opt + 112)[0]
        export_size = struct.unpack_from("<I", data, opt + 116)[0]
    else:
        export_rva = struct.unpack_from("<I", data, opt + 96)[0]
        export_size = struct.unpack_from("<I", data, opt + 100)[0]

    if not export_rva:
        raise SystemExit(f"{dll_path}: no export directory")

    sections = []
    sec_off = opt + opt_size
    for i in range(n_sections):
        base = sec_off + i * 40
        vaddr = struct.unpack_from("<I", data, base + 12)[0]
        vsize = struct.unpack_from("<I", data, base + 8)[0]
        raw = struct.unpack_from("<I", data, base + 20)[0]
        sections.append((vaddr, vsize, raw))

    def to_offset(rva):
        for vaddr, vsize, raw in sections:
            if vaddr <= rva < vaddr + max(vsize, 1):
                return raw + (rva - vaddr)
        raise SystemExit(f"{dll_path}: RVA 0x{rva:x} outside every section")

    exp_off = to_offset(export_rva)
    n_names = struct.unpack_from("<I", data, exp_off + 24)[0]
    names_rva = struct.unpack_from("<I", data, exp_off + 32)[0]
    names_off = to_offset(names_rva)

    out = []
    for i in range(n_names):
        name_rva = struct.unpack_from("<I", data, names_off + i * 4)[0]
        name_off = to_offset(name_rva)
        end = data.index(b"\0", name_off)
        out.append(data[name_off:end].decode("ascii"))
    return out


def main():
    if len(sys.argv) != 3:
        raise SystemExit(__doc__)
    dll_path, def_path = sys.argv[1], sys.argv[2]
    names = read_export_names(dll_path)
    dll_name = os.path.basename(dll_path)
    # The LIBRARY directive is what tells the loader (and dumpbin) which DLL
    # the import table binds to; without it the import table entry is "(null)"
    # and the process fails to start with STATUS_DLL_NOT_FOUND.
    # Data symbols and the Rust FFI re-exports come through too; keep them all
    # so the Go binding resolves every C symbol it references.
    with open(def_path, "w", encoding="ascii", newline="\n") as fh:
        fh.write(f"LIBRARY {dll_name}\n")
        fh.write("EXPORTS\n")
        for name in names:
            fh.write(f"{name}\n")
    print(f"{dll_path}: {len(names)} exports -> {def_path}")


if __name__ == "__main__":
    main()

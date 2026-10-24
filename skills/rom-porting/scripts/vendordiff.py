#!/usr/bin/env python3
"""Compare two extracted vendor/proprietary file trees.

A vendor swap is mostly a question of "what does the target expect that the
donor does not ship, and vice versa". This walks both trees and reports
missing files, size changes, and ELF ABI details, with no external tools.

    vendordiff.py <target_vendor_dir> <donor_vendor_dir> [--json]
"""
import json
import os
import struct
import sys

ELF_MAGIC = b"\x7fELF"


def elf_info(path):
    """Return (class, machine, needed_count) for an ELF, else None."""
    try:
        with open(path, "rb") as f:
            h = f.read(64)
            if len(h) < 64 or not h.startswith(ELF_MAGIC):
                return None
            cls = "64" if h[4] == 2 else "32"
            endian = "<" if h[5] == 1 else ">"
            etype = struct.unpack_from(endian + "H", h, 16)[0]
            machine = struct.unpack_from(endian + "H", h, 18)[0]
            kind = {1: "rel", 2: "exec", 3: "dyn", 4: "core"}.get(etype, str(etype))
            return {"class": cls, "machine": hex(machine), "type": kind}
    except OSError:
        return None


MACHINES = {0x28: "arm32", 0xB7: "aarch64", 0x3E: "x86_64", 0x03: "x86"}


def walk(root):
    out = {}
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in (".git", "node_modules")]
        for fn in filenames:
            p = os.path.join(dirpath, fn)
            rel = os.path.relpath(p, root)
            try:
                st = os.lstat(p)
            except OSError:
                continue
            entry = {"size": st.st_size}
            if os.path.islink(p):
                entry["symlink"] = os.readlink(p)
            else:
                e = elf_info(p)
                if e:
                    entry["elf"] = e
                    entry["arch"] = MACHINES.get(int(e["machine"], 16), e["machine"])
            out[rel] = entry
    return out


def main():
    if len(sys.argv) < 3:
        print(__doc__.strip())
        return 2
    a, b = sys.argv[1], sys.argv[2]
    as_json = "--json" in sys.argv[3:]
    for d in (a, b):
        if not os.path.isdir(d):
            print(f"not a directory: {d}", file=sys.stderr)
            return 1
    ta, tb = walk(a), walk(b)
    only_target = sorted(set(ta) - set(tb))
    only_donor = sorted(set(tb) - set(ta))
    changed = []
    arch_mismatch = []
    for name in sorted(set(ta) & set(tb)):
        if ta[name]["size"] != tb[name]["size"]:
            changed.append({"path": name,
                            "target": ta[name]["size"], "donor": tb[name]["size"]})
        ea, eb = ta[name].get("arch"), tb[name].get("arch")
        if ea and eb and ea != eb:
            arch_mismatch.append({"path": name, "target": ea, "donor": eb})

    report = {
        "target_root": os.path.abspath(a),
        "donor_root": os.path.abspath(b),
        "target_files": len(ta),
        "donor_files": len(tb),
        "only_in_target": only_target,
        "only_in_donor": only_donor,
        "size_changed": changed,
        "arch_mismatch": arch_mismatch,
    }
    if as_json:
        print(json.dumps(report, indent=2))
        return 0

    print(f"target: {a}  ({len(ta)} files)")
    print(f"donor : {b}  ({len(tb)} files)")
    print(f"\nonly in target (donor must supply these): {len(only_target)}")
    for n in only_target[:40]:
        print("  ", n)
    if len(only_target) > 40:
        print(f"   ... {len(only_target) - 40} more")
    print(f"\nonly in donor (target does not reference): {len(only_donor)}")
    for n in only_donor[:20]:
        print("  ", n)
    if len(only_donor) > 20:
        print(f"   ... {len(only_donor) - 20} more")
    print(f"\nsame path, different size: {len(changed)}")
    for c in changed[:20]:
        print(f"   {c['path']}: target {c['target']} vs donor {c['donor']}")
    print(f"\nELF architecture mismatch: {len(arch_mismatch)}")
    for m in arch_mismatch[:20]:
        print(f"   {m['path']}: target {m['target']} vs donor {m['donor']}")
    if arch_mismatch:
        print("   ^ a cross-architecture blob will not load; this is a hard stop")
    return 0


if __name__ == "__main__":
    sys.exit(main())

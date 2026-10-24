#!/usr/bin/env python3
"""Inspect Android ROM images without root or external tools.

Reads sparse images, ext4 superblocks, AVB footers and super.img LP
metadata using only the standard library. Read-only by design: nothing
here writes to or flashes a device.

    rominfo.py <image> [--json]
"""
import json
import struct
import sys
import os

SPARSE_MAGIC = 0xED26FF3A
EXT4_MAGIC = 0xEF53
AVB_MAGIC = b"AVBf"
LP_GEOMETRY_MAGIC = 0x616C4467
LP_HEADER_MAGIC = 0x414C5030

CHUNK_TYPES = {0xCAC1: "raw", 0xCAC2: "fill", 0xCAC3: "don't-care", 0xCAC4: "crc32"}


def read_sparse(f):
    """Return (info dict, expanded_size_bytes) or None if not sparse."""
    f.seek(0)
    head = f.read(28)
    if len(head) < 28:
        return None
    magic, major, minor, fhdr, chdr, blk, total_blks, total_chunks, _ = struct.unpack(
        "<IHHHHIIII", head)
    if magic != SPARSE_MAGIC:
        return None
    chunks = []
    f.seek(fhdr)
    for _ in range(total_chunks):
        raw = f.read(chdr)
        if len(raw) < 12:
            break
        ctype, _res, csz, tsz = struct.unpack("<HHII", raw[:12])
        chunks.append({"type": CHUNK_TYPES.get(ctype, hex(ctype)),
                       "blocks": csz, "bytes": tsz})
        f.seek(tsz - chdr, 1)
    return {
        "sparse": True,
        "major": major, "minor": minor,
        "block_size": blk,
        "total_blocks": total_blks,
        "total_chunks": total_chunks,
        "expanded_bytes": total_blks * blk,
        "chunk_summary": _summarise_chunks(chunks),
    }, blk


def _summarise_chunks(chunks):
    counts = {}
    for c in chunks:
        counts[c["type"]] = counts.get(c["type"], 0) + 1
    return counts


def read_ext4_superblock(f, base=0):
    """Read the ext4 superblock at base+1024. Returns dict or None."""
    f.seek(base + 1024)
    sb = f.read(1024)
    if len(sb) < 1024:
        return None
    magic = struct.unpack_from("<H", sb, 56)[0]
    if magic != EXT4_MAGIC:
        return None
    inodes = struct.unpack_from("<I", sb, 0)[0]
    blocks_lo = struct.unpack_from("<I", sb, 4)[0]
    blocks_hi = struct.unpack_from("<I", sb, 0x150)[0]
    log_bs = struct.unpack_from("<I", sb, 24)[0]
    block_size = 1024 << log_bs
    blocks = blocks_lo | (blocks_hi << 32)
    label = sb[120:136].split(b"\x00")[0].decode("utf-8", "replace")
    uuid = sb[104:120].hex()
    feat_compat = struct.unpack_from("<I", sb, 0x5C)[0]
    feat_incompat = struct.unpack_from("<I", sb, 0x60)[0]
    feat_ro = struct.unpack_from("<I", sb, 0x64)[0]
    return {
        "ext4": True,
        "volume_label": label,
        "uuid": uuid,
        "block_size": block_size,
        "block_count": blocks,
        "inode_count": inodes,
        "size_bytes": blocks * block_size,
        "features": {
            "compat": hex(feat_compat),
            "incompat": hex(feat_incompat),
            "ro_compat": hex(feat_ro),
        },
        "has_metadata_csum": bool(feat_ro & 0x400),
        "has_64bit": bool(feat_incompat & 0x80),
    }


def read_avb_footer(f, size):
    """AVB footer sits in the last 64 bytes of a vbmeta-bearing image."""
    if size < 64:
        return None
    f.seek(size - 64)
    tail = f.read(64)
    if len(tail) < 64 or not tail.startswith(AVB_MAGIC):
        return None
    vmaj, vmin = struct.unpack_from(">II", tail, 4)
    orig, vb_off, vb_size = struct.unpack_from(">QQQ", tail, 12)
    return {
        "avb": True,
        "footer_version": f"{vmaj}.{vmin}",
        "original_image_size": orig,
        "vbmeta_offset": vb_off,
        "vbmeta_size": vb_size,
    }


def read_lp_metadata(f):
    """Parse super.img LP geometry + partition table if present."""
    f.seek(4096)
    geo = f.read(52)
    if len(geo) < 52:
        return None
    magic = struct.unpack_from("<I", geo, 0)[0]
    if magic != LP_GEOMETRY_MAGIC:
        return None
    meta_max, slot_count, lb_size = struct.unpack_from("<III", geo, 12)
    # metadata header follows the geometry block
    f.seek(4096 + 52)
    hdr = f.read(80)
    if len(hdr) < 80:
        return None
    hm = struct.unpack_from("<I", hdr, 0)[0]
    if hm != LP_HEADER_MAGIC:
        return {"lp_geometry": {"metadata_max_size": meta_max,
                                "slot_count": slot_count,
                                "logical_block_size": lb_size},
                "lp_partitions": None}
    _struct_size, _cksum, tables_size, _tcksum = struct.unpack_from("<IIII", hdr, 4)
    parts = []
    off = 80
    # each partition entry is 52 bytes: name[36], attrs u32, first_extent u32,
    # num_extents u32, group_index u32
    f.seek(4096 + 52 + off)
    while len(parts) < 64:
        ent = f.read(52)
        if len(ent) < 52:
            break
        name = ent[0:36].split(b"\x00")[0].decode("utf-8", "replace")
        if not name:
            break
        attrs, fe, ne, gi = struct.unpack_from("<IIII", ent, 36)
        parts.append({"name": name, "attributes": hex(attrs),
                      "first_extent": fe, "num_extents": ne, "group": gi})
    return {"lp_geometry": {"metadata_max_size": meta_max,
                            "slot_count": slot_count,
                            "logical_block_size": lb_size},
            "lp_partitions": parts}


def inspect(path):
    out = {"path": os.path.abspath(path)}
    size = os.path.getsize(path)
    out["file_size"] = size
    with open(path, "rb") as f:
        sp = read_sparse(f)
        if sp:
            info, blk = sp
            out.update(info)
            # a sparse image's payload often starts with ext4
            f.seek(0)
            head = f.read(4)
        else:
            out["sparse"] = False
        # try ext4 at offset 0 (most partition images)
        sb = read_ext4_superblock(f, 0)
        if sb:
            out["filesystem"] = sb
        avb = read_avb_footer(f, size)
        if avb:
            out["avb_footer"] = avb
        lp = read_lp_metadata(f)
        if lp:
            out["super"] = lp
    out["format_guess"] = _guess(out)
    return out


def _guess(o):
    if o.get("super"):
        return "super.img (dynamic partitions)"
    fs = o.get("filesystem")
    if fs:
        return f"ext4 partition ({fs['volume_label'] or 'unlabeled'})"
    if o.get("sparse"):
        return "sparse image (payload format unknown)"
    return "unrecognised or raw"


def main():
    if len(sys.argv) < 2:
        print(__doc__.strip())
        return 2
    path = sys.argv[1]
    as_json = "--json" in sys.argv[2:]
    if not os.path.isfile(path):
        print(f"not a file: {path}", file=sys.stderr)
        return 1
    info = inspect(path)
    if as_json:
        print(json.dumps(info, indent=2))
        return 0
    print(f"{info['path']}  ({info['file_size']} bytes)")
    print(f"  format: {info['format_guess']}")
    if info.get("sparse"):
        print(f"  sparse: v{info['major']}.{info['minor']}, "
              f"{info['total_blocks']} blocks of {info['block_size']} "
              f"-> {info['expanded_bytes']} bytes expanded")
        print(f"  chunks: {info['chunk_summary']}")
    fs = info.get("filesystem")
    if fs:
        print(f"  ext4: label={fs['volume_label']!r} uuid={fs['uuid']}")
        print(f"        block={fs['block_size']} blocks={fs['block_count']} "
              f"size={fs['size_bytes']}")
        print(f"        64bit={fs['has_64bit']} metadata_csum={fs['has_metadata_csum']}")
    avb = info.get("avb_footer")
    if avb:
        print(f"  avb: vbmeta at {avb['vbmeta_offset']} "
              f"({avb['vbmeta_size']} bytes), original size {avb['original_image_size']}")
    sup = info.get("super")
    if sup:
        g = sup["lp_geometry"]
        print(f"  super: metadata_max={g['metadata_max_size']} slots={g['slot_count']} "
              f"logical_block={g['logical_block_size']}")
        for p in (sup.get("lp_partitions") or []):
            print(f"    {p['name']:<20} extents={p['num_extents']} group={p['group']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

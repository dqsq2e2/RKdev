"""Build and verify board recovery images. This module never opens a device for writing."""

import hashlib
import json
import os
import shutil
import struct
import subprocess
import tempfile
import uuid
import zlib
from pathlib import Path

from contract import GPT_TAIL, SECTOR, read_json, require, write_json


def run(args, **kwargs):
    return subprocess.run([str(x) for x in args], check=True, **kwargs)


def sha(path, *, offset=0, length=None):
    result = hashlib.sha256()
    with path.open("rb") as f:
        f.seek(offset)
        while length is None or length:
            buf = f.read(min(length, 1024 * 1024) if length is not None else 1024 * 1024)
            if not buf:
                require(length is None, f"short read: {path}")
                break
            result.update(buf)
            if length is not None:
                length -= len(buf)
    return result.hexdigest()


def environment_text(data, text):
    require(text.endswith(b"\n"), "expected newline-terminated U-Boot environment")
    rows = {}
    for row in text.splitlines():
        if row and not row.startswith(b"#"):
            key, sep, value = row.partition(b"=")
            require(sep and key and b"\0" not in row, "invalid U-Boot environment record")
            rows[key] = value
    rows.update({b"bootcmd": b"rkdevboot", b"rkdev_layout": data["layout_id"].encode("ascii"),
                 b"rkdev_targets": " ".join(data["boot_order"]).encode("ascii")})
    rows.pop(b"rkdev_installing", None)
    rows.pop(b"preboot", None)
    return b"\n".join(k + b"=" + v for k, v in rows.items()) + b"\n"


def env_image(path, data, text):
    env = data["environment"]
    path.write_bytes(environment_text(data, text))
    output = path.with_suffix(".bin")
    args = ["mkenvimage", "-s", str(env["size"]), "-o", output]
    if len(env["offsets"]) == 2:
        args += ["-r"]
    run([*args, path])
    return output


def check_environment(raw, data):
    start = 5 if len(data["environment"]["offsets"]) == 2 else 4
    require(len(raw) == data["environment"]["size"], "invalid environment size")
    require(struct.unpack_from("<I", raw)[0] == zlib.crc32(raw[start:]), "bad U-Boot environment CRC")
    pairs = raw[start:].split(b"\0\0", 1)[0].split(b"\0")
    require(b"rkdev_layout=" + data["layout_id"].encode("ascii") in pairs, "environment belongs to another layout")
    require(b"bootcmd=rkdevboot" in pairs, "environment does not select RKdev")


def gpt_options(data):
    r = data["recovery"]
    s = data["system"]
    head = r["offset"] // SECTOR
    end = (r["offset"] + r["length"]) // SECTOR - 1
    return [
        "--clear", "--set-alignment=1",
        f"--new=1:{head}:{end}", f"--change-name=1:{r['label']}", "--typecode=1:8300",
        f"--new=2:{s['offset'] // SECTOR}:0", f"--change-name=2:{s['label']}", "--typecode=2:8300",
    ]


def pack(data, loader, default_env, recovery, out, *, source_info=None, spi_trim=0):
    require(out.parent.is_dir(), "image output directory does not exist")
    for name, p in (("loader", loader), ("environment", default_env), ("recovery", recovery)):
        require(p.is_file() and p.stat().st_size > 0, f"missing {name} artifact")
    loader_size = loader.stat().st_size - spi_trim
    require(spi_trim >= 0 and 0 < loader_size <= data["loader"]["length"], "loader does not fit")
    if data["profile"] == "spi":
        require(spi_trim == data["loader"]["offset"], "SPI loader trimming must match its placement")
    else:
        require(spi_trim == 0, "cannot trim an eMMC loader")
    require(recovery.stat().st_size == data["recovery"]["length"], "incorrect recovery filesystem size")
    with tempfile.TemporaryDirectory(prefix=".rkdev-pack-", dir=out.parent) as temporary:
        temp = Path(temporary)
        image = temp / "image.img"
        with image.open("wb") as f:
            if data["profile"] == "emmc":
                f.truncate(data["capacity"])
            else:
                remaining = data["capacity"]
                erase = b"\xff" * (1024 * 1024)
                while remaining:
                    f.write(erase[:remaining])
                    remaining -= min(remaining, len(erase))
        if data["profile"] == "emmc":
            run(["sgdisk", *gpt_options(data), image])
        env = env_image(temp / "default-env.txt", data, default_env.read_bytes())
        check_environment(env.read_bytes(), data)
        extents = []
        with image.open("r+b") as f:
            def place(name, offset, src, skip=0):
                f.seek(offset)
                with src.open("rb") as s:
                    s.seek(skip)
                    shutil.copyfileobj(s, f)
                length = src.stat().st_size - skip
                extents.append({"kind": name, "offset": offset, "length": length,
                                "sha256": sha(src, offset=skip, length=length)})
            place("loader", data["loader"]["offset"], loader, spi_trim)
            for offset in data["environment"]["offsets"]:
                place("environment", offset, env)
            place("recovery", data["recovery"]["offset"], recovery)
            f.flush()
            os.fsync(f.fileno())
        manifest = {
            "schema": 1, "kind": "recovery-image", "layout": data,
            "image": {"name": out.name, "bytes": data["capacity"], "sha256": sha(image)},
            "extents": extents, "sources": source_info or {},
        }
        verify(data, image, manifest, check_filesystem=True)
        manifest_path = out.with_suffix(".json")
        require(not out.exists() and not manifest_path.exists(), f"output already exists: {out}")
        image.rename(out)
        write_json(manifest_path, manifest)
        return manifest


def verify_gpt(path, data):
    count = data["capacity"] // SECTOR
    with path.open("rb") as f:
        def header(lba, alternate):
            f.seek(lba * SECTOR)
            raw = f.read(SECTOR)
            require(raw[:8] == b"EFI PART", "missing GPT header")
            size, stored = struct.unpack_from("<II", raw, 12)
            require(92 <= size <= SECTOR, "bad GPT header size")
            require(zlib.crc32(raw[:16] + bytes(4) + raw[20:size]) == stored, "bad GPT header CRC")
            here, there, first, last = struct.unpack_from("<QQQQ", raw, 24)
            require((here, there, first, last) == (lba, alternate, 34, count - 34), "GPT geometry mismatch")
            entry_lba, entries, size, crc = struct.unpack_from("<QIII", raw, 72)
            require(entries == 128 and size == 128 and entry_lba == (2 if lba == 1 else count - 33),
                    "unexpected GPT entry layout")
            f.seek(entry_lba * SECTOR)
            table = f.read(entries * size)
            require(zlib.crc32(table) == crc, "bad GPT partition entry CRC")
            return raw[56:72], table
        guid, table = header(1, count - 1)
        require((guid, table) == header(count - 1, 1), "backup GPT differs from primary")
    for i, area in enumerate((data["recovery"], data["system"])):
        entry = table[i * 128:(i + 1) * 128]
        start, end = struct.unpack_from("<QQ", entry, 32)
        name = entry[56:128].decode("utf-16-le").split("\0", 1)[0]
        stop = area["offset"] + area["length"] if i == 0 else data["capacity"] - GPT_TAIL
        require(start * SECTOR == area["offset"] and (end + 1) * SECTOR == stop and name == area["label"],
                f"GPT partition {i + 1} differs from board layout")
    require(not any(table[256:]), "unexpected GPT partition")


def verify_payload(saved, work, data):
    def debug(command):
        return run(["debugfs", "-R", command, saved], capture_output=True).stdout

    def probe(key):
        return run(["blkid", "-p", "-s", key, "-o", "value", saved],
                   capture_output=True, text=True).stdout.strip()

    run(["e2fsck", "-fn", saved])
    require(probe("LABEL") == data["recovery"]["label"], "ext4 recovery label mismatch")
    require(probe("UUID") == str(uuid.UUID(hex=data["layout_id"][:32])), "ext4 recovery UUID mismatch")
    require(json.loads(debug("cat /layout.json")) == data, "recovery filesystem layout mismatch")
    kernel = "Image.uimg" if data["profile"] == "spi" else "Image"
    entries = debug("cat /extlinux/extlinux.conf").decode("utf-8").splitlines()
    require({f"kernel /{kernel}", "fdt /rkdev.dtb", "initrd /initramfs.cpio.gz"} <= {line.strip() for line in entries},
            "extlinux boot file paths do not match profile")
    for name in (kernel, "initramfs.cpio.gz"):
        require(b"Inode:" in debug(f"stat /{name}"), f"missing {name} in recovery filesystem")
    dtb = work / "recovery.dtb"
    debug(f"dump /rkdev.dtb {dtb}")
    def prop(node, key):
        return run(["fdtget", "-t", "s", dtb, node, key], capture_output=True, text=True).stdout.strip()
    require(data["compatible"] in prop("/", "compatible").split(), "DT compatible mismatch")
    for name in ("board_id", "revision", "profile", "layout_id", "schema"):
        require(prop("/chosen", "rkdev," + name.replace("_", "-")) == str(data[name]),
                f"DT /chosen {name} mismatch")


def verify(data, image, manifest, *, check_filesystem):
    require(image.is_file() and image.stat().st_size == data["capacity"], "image size mismatch")
    require(manifest["kind"] == "recovery-image" and manifest["schema"] == 1 and
            manifest["layout"] == data, "image board/profile/layout mismatch")
    require(manifest["image"]["bytes"] == data["capacity"] and sha(image) == manifest["image"]["sha256"],
            "whole image SHA-256 mismatch")
    expected = {
        ("loader", data["loader"]["offset"]): data["loader"]["length"],
        ("recovery", data["recovery"]["offset"]): data["recovery"]["length"],
        **{("environment", off): data["environment"]["size"] for off in data["environment"]["offsets"]},
    }
    for extent in manifest["extents"]:
        kind, offset, length = (extent[k] for k in ("kind", "offset", "length"))
        bound = expected.pop((kind, offset), 0)
        require((0 < length <= bound if kind == "loader" else 0 < length == bound),
                f"invalid {kind} extent")
        require(sha(image, offset=offset, length=length) == extent["sha256"], f"{kind} readback differs")
    require(not expected, "missing required image extents")
    if data["profile"] == "emmc":
        verify_gpt(image, data)
    with image.open("rb") as f:
        f.seek(data["loader"]["offset"])
        require(f.read(4) == b"RKNS", "RK3588 loader header is missing at declared offset")
        for offset in data["environment"]["offsets"]:
            f.seek(offset)
            check_environment(f.read(data["environment"]["size"]), data)
        if check_filesystem:
            recovery = data["recovery"]
            with tempfile.TemporaryDirectory(prefix="rkdev-verify-") as tmp:
                saved = Path(tmp) / "recovery.ext4"
                f.seek(recovery["offset"])
                with saved.open("wb") as dest:
                    pending = recovery["length"]
                    while pending:
                        chunk = f.read(min(pending, 1024 * 1024))
                        require(chunk, "short read extracting recovery volume")
                        dest.write(chunk)
                        pending -= len(chunk)
                verify_payload(saved, Path(tmp), data)

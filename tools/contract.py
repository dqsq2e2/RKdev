"""The board contract is shared by the image builder and runtime manifest."""

import hashlib
import json
import re
from pathlib import Path

SCHEMA = 1
SECTOR = 512
GPT_HEAD = 34 * SECTOR
GPT_TAIL = 33 * SECTOR
STORAGE = {"emmc", "sd", "usb", "nvme", "sata"}
IDENT = re.compile(r"[a-z0-9][a-z0-9.-]{0,47}\Z")
LABEL = re.compile(r"[A-Za-z0-9_-]{1,16}\Z")
SHA256 = re.compile(r"[a-f0-9]{64}\Z")
DEVICES = {
    "emmc": re.compile(r"/dev/mmcblk[0-9]+\Z"),
    "spi": re.compile(r"/dev/mtdblock[0-9]+\Z"),
}


class Invalid(ValueError):
    pass


def require(condition, message):
    if not condition:
        raise Invalid(message)


def read_json(path):
    def unique(pairs):
        data = {}
        for k, v in pairs:
            require(k not in data, f"duplicate JSON key: {k}")
            data[k] = v
        return data
    return json.loads(path.read_text(encoding="utf-8-sig"), object_pairs_hook=unique)


def canonical(data):
    return json.dumps(data, sort_keys=True, ensure_ascii=False, separators=(",", ":")).encode()


def write_json(path, data):
    path.write_text(json.dumps(data, sort_keys=True, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def matches(pattern, value):
    return isinstance(value, str) and pattern.fullmatch(value) is not None


def fields(value, required, optional=(), prefix="object"):
    require(isinstance(value, dict), f"{prefix} must be an object")
    require(set(required) <= value.keys(), f"{prefix} is missing: {sorted(set(required) - value.keys())}")
    require(value.keys() <= set(required) | set(optional), f"{prefix} has unknown keys: {sorted(value.keys() - set(required) - set(optional))}")


def number(value, low, high, name):
    require(type(value) is int and low <= value <= high, f"{name} must be an integer between {low} and {high}")
    return value


def path_in(root, relative, *, directory=False):
    require(isinstance(relative, str) and relative != "" and not Path(relative).is_absolute()
            and re.fullmatch(r"[A-Za-z0-9_./+-]+", relative) and ".." not in Path(relative).parts,
            f"invalid board path: {relative!r}")
    path = (root / relative).resolve()
    require(path.is_relative_to(root), f"path escapes board package: {relative}")
    require(path.is_dir() if directory else path.is_file(), f"missing board input: {relative}")
    return path


def source(root, value, name):
    fields(value, ("sha256",), ("url", "path", "file"), name)
    require(matches(SHA256, value["sha256"]), f"{name}.sha256 is invalid")
    require(len({"url", "path", "file"} & value.keys()) == 1, f"{name} needs one of path, file or url")
    if "path" in value:
        path_in(root, value["path"])
    elif "file" in value:
        require(matches(re.compile(r"[A-Za-z0-9_+.-]+\Z"), value["file"])
                and value["file"] not in {".", ".."}, f"invalid firmware file name: {name}")
    else:
        require(isinstance(value["url"], str) and value["url"].startswith("https://")
                and all(ord(c) > 32 for c in value["url"]), f"{name}.url must use HTTPS")


def validate_profile(root, name, cfg):
    fields(cfg, ("device", "capacity", "loader", "environment", "recovery", "boot_order",
                 "uboot", "linux"), ("system", "key", "boot_fallback", "erase_size", "mtd_name",
                 "recovery_args", "firmware", "release_url", "system_targets", "mmc_device",
                 "sd_device", "loader_index", "rkfw"), name)
    require(name in DEVICES and matches(DEVICES[name], cfg["device"]), f"{name}: invalid boot device")
    size = number(cfg["capacity"], 4 * 1024 * 1024, 2**40, "capacity")
    alignment = SECTOR
    if name == "spi":
        alignment = number(cfg.get("erase_size"), SECTOR, 2**20, "erase_size")
        require(alignment & (alignment - 1) == 0
                and matches(re.compile(r"[A-Za-z0-9_.-]{1,32}\Z"), cfg.get("mtd_name")),
                "invalid MTD erase size or name")
        require("system" not in cfg, "SPI has no system partition in the recovery flash")
    else:
        require("mtd_name" not in cfg and "erase_size" not in cfg and "system" in cfg, "eMMC needs a system area")
    require(size % alignment == 0, "capacity must be aligned")
    used = []

    def add_range(offset, length, kind):
        number(offset, 0, size, f"{kind} offset")
        number(length, 1, size, f"{kind} length")
        require(offset % alignment == length % alignment == 0 and offset + length <= size,
                f"{kind} is unaligned or outside media")
        used.append((offset, offset + length, kind))

    if name == "emmc":
        add_range(0, GPT_HEAD, "primary GPT")
        add_range(size - GPT_TAIL, GPT_TAIL, "backup GPT")
    loader = cfg["loader"]
    fields(loader, ("offset", "length", "format"), (), "loader")
    require(loader["format"] == ("rockchip-spi" if name == "spi" else "rockchip-sd"),
            "loader format is incompatible with this profile")
    require(loader["offset"] == 0x8000, "RK3588 loader must start at 0x8000")
    add_range(loader["offset"], loader["length"], "loader")
    env = cfg["environment"]
    fields(env, ("offsets", "size"), (), "environment")
    number(env["size"], 512, 2**20, "environment size")
    require(env["size"] % 512 == 0, "environment size must be sector aligned")
    offsets = env["offsets"]
    require(isinstance(offsets, list) and len(offsets) in ((1,) if name == "spi" else (1, 2)),
            "SPI requires one environment copy; eMMC supports one or two")
    if len(offsets) == 2:
        require(type(offsets[0]) is int and type(offsets[1]) is int and offsets[0] < offsets[1],
                "environment copies must be in offset order")
    for ix, offset in enumerate(offsets):
        add_range(offset, ((env["size"] + alignment - 1) // alignment) * alignment, f"environment copy {ix + 1}")
    rec = cfg["recovery"]
    fields(rec, ("offset", "length", "label"), ("partition", "load_address"), "recovery")
    add_range(rec["offset"], rec["length"], "recovery")
    require(matches(LABEL, rec["label"]), "recovery label must fit ext4 label field")
    if name == "emmc":
        require(rec.get("partition") == 1 and "load_address" not in rec, "eMMC v1 expects recovery partition 1")
        sys = cfg["system"]
        fields(sys, ("offset", "label"), (), "system")
        number(sys["offset"], rec["offset"] + rec["length"], size - GPT_TAIL - SECTOR, "system offset")
        require(matches(LABEL, sys["label"]) and sys["label"] != rec["label"], "invalid system label")
        add_range(sys["offset"], size - GPT_TAIL - sys["offset"], "system")
    else:
        require("partition" not in rec, "SPI recovery has no GPT partition number")
        addr = number(rec.get("load_address"), 0x18000000, 0xFFFFFFFF, "SPI recovery load address")
        require(addr % 4096 == 0 and addr + rec["length"] <= 0x100000000, "invalid SPI recovery buffer")
    ordered = sorted(used)
    for a, b in zip(ordered, ordered[1:]):
        require(a[1] <= b[0], f"{a[2]} overlaps {b[2]}")
    order = cfg["boot_order"]
    require(isinstance(order, list) and order and all(isinstance(x, str) and x in STORAGE for x in order)
            and len(set(order)) == len(order), "invalid boot_order")
    targets = cfg.get("system_targets", order)
    require(isinstance(targets, list) and all(isinstance(x, str) and x in STORAGE for x in targets)
            and len(set(targets)) == len(targets), "invalid system_targets")
    require(cfg.get("boot_fallback", "console") in ("console", "maskrom", "loader"), "invalid boot_fallback")
    require(name == "emmc" or cfg.get("boot_fallback") != "loader", "SPI Loader is not supported by RKdev v1")
    for controller in ("mmc_device", "sd_device", "loader_index"):
        if controller in cfg:
            number(cfg[controller], 0, 15, controller)
    args = cfg.get("recovery_args", "")
    require(isinstance(args, str) and "\n" not in args and "\r" not in args, "invalid recovery_args")
    if "rkfw" in cfg:
        rkfw = cfg["rkfw"]
        fields(rkfw, ("kernel", "initrd", "resource", "root", "args"), (), "rkfw")
        indices = [number(rkfw[k], 1, 128, f"rkfw.{k}") for k in ("kernel", "initrd", "resource", "root")]
        require(len(set(indices)) == 4 and matches(re.compile(r"[A-Za-z0-9,=.:_ /-]{0,512}\Z"), rkfw["args"]),
                "invalid Rockchip raw firmware parameters")
        require(name == "spi", "RKFW raw partition numbering is supported in SPI profiles only")
    if "release_url" in cfg:
        v = cfg["release_url"]
        require(isinstance(v, str) and v.startswith("https://") and all(ord(c) > 32 for c in v), "release_url must use HTTPS")
    if "firmware" in cfg:
        fields(cfg["firmware"], (), tuple(cfg["firmware"]), "firmware")
        for dest, entry in cfg["firmware"].items():
            parts = Path(dest).parts
            require(bool(parts) and re.fullmatch(r"[A-Za-z0-9_./+-]+", dest)
                    and not dest.startswith("/") and all(p not in {".", ".."} for p in parts), f"invalid firmware name: {dest}")
            source(root, entry, f"firmware {dest}")
    for comp in ("uboot", "linux"):
        entry = cfg[comp]
        fields(entry, ("config", "dts"), ("patches", "overlay"), comp)
        for field in ("config", "dts"):
            path_in(root, entry[field])
        if "overlay" in entry:
            path_in(root, entry["overlay"], directory=True)
        patches = entry.get("patches", [])
        require(isinstance(patches, list), f"{comp}.patches must be a list")
        for patch in patches:
            path_in(root, patch)
    if "key" in cfg:
        key = cfg["key"]
        fields(key, ("type", "action"), ("channel", "minimum", "maximum", "samples", "delay_ms",
              "usb_node", "wait_ms", "label"), "key")
        require(key["type"] in ("adc", "button"), "key.type must be adc or button")
        require(key["action"] in ("recovery", "maskrom", "host-or-recovery"), "invalid key action")
        if key["type"] == "adc":
            number(key.get("channel"), 0, 31, "ADC channel")
            low = number(key.get("minimum"), 0, 65535, "ADC minimum")
            number(key.get("maximum"), low, 65535, "ADC maximum")
        else:
            require(matches(re.compile(r"[A-Za-z0-9_ -]{1,48}\Z"), key.get("label")), "button label is invalid")
        number(key.get("samples", 1), 1, 20, "key sample count")
        number(key.get("delay_ms", 20), 0, 200, "key sample interval")
        if key["action"] == "host-or-recovery":
            require(matches(re.compile(r"/(?:[A-Za-z0-9,_@.-]+/)*[A-Za-z0-9,_@.-]+\Z"), key.get("usb_node")),
                    "missing USB gadget DT path")
            number(key.get("wait_ms", 2500), 50, 5000, "USB probe timeout")


def load_board(path, profile):
    manifest = path if path.is_file() else path / "board.json"
    root = manifest.resolve().parent
    board = read_json(manifest)
    fields(board, ("schema", "board_id", "revision", "name", "soc", "compatible",
                   "default_profile", "firmware", "profiles"), ("rootfs_overlay",), "board")
    require(type(board["schema"]) is int and board["schema"] == SCHEMA, "unsupported board schema")
    for key in ("board_id", "revision"):
        require(matches(IDENT, board[key]), f"invalid {key}")
    require(board["soc"] == "rk3588", "unsupported SoC")
    require(isinstance(board["name"], str) and 1 <= len(board["name"]) <= 96
            and all(ord(c) >= 32 for c in board["name"]), "invalid board name")
    require(matches(re.compile(r"[a-zA-Z0-9_.-]+,[a-zA-Z0-9_.-]+\Z"), board["compatible"]), "invalid DT compatible")
    require(isinstance(board["profiles"], dict) and board["profiles"], "missing profiles")
    require(board["default_profile"] in board["profiles"], "default profile unsupported")
    fields(board["firmware"], ("ddr", "bl31"), (), "boot firmware")
    for name, entry in board["firmware"].items():
        source(root, entry, name)
    if "rootfs_overlay" in board:
        path_in(root, board["rootfs_overlay"], directory=True)
    for name, cfg in board["profiles"].items():
        validate_profile(root, name, cfg)
    name = profile or board["default_profile"]
    require(name in board["profiles"], f"board does not support profile {name}")
    return root, board, name, board["profiles"][name]


def runtime(board, name, cfg):
    layout = {k: cfg[k] for k in ("device", "capacity", "loader", "environment", "recovery")
              + (("system",) if name == "emmc" else ("erase_size", "mtd_name"))}
    identity = {k: board[k] for k in ("board_id", "revision", "name", "compatible", "soc")}
    layout_id = hashlib.sha256(canonical({**layout, "profile": name, "board": board["board_id"],
                                          "revision": board["revision"], "compatible": board["compatible"]})).hexdigest()
    return {**identity, **layout, "schema": SCHEMA, "profile": name, "layout_id": layout_id,
            "boot_order": cfg["boot_order"], "system_targets": cfg.get("system_targets", cfg["boot_order"]),
            "release_url": cfg.get("release_url", "")}


def fw_env_config(data):
    env = data["environment"]
    geometry = "" if data["profile"] == "emmc" else f" 0x{data['erase_size']:x}"
    return "".join(f"{data['device']} 0x{off:x} 0x{env['size']:x}{geometry}\n" for off in env["offsets"])

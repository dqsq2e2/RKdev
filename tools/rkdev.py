#!/usr/bin/env python3
"""Build RKdev with a board directory supplied by any repository."""

import argparse
import gzip
import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import uuid
from pathlib import Path

from contract import Invalid, canonical, fw_env_config, load_board, path_in, read_json, require, runtime, write_json
from image import environment_text, pack, run, sha, verify
from uboot import inject, profile_header, put_settings, settings

CORE = Path(__file__).resolve().parents[1]
ROOTFS = {
    "url": "https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/aarch64/alpine-minirootfs-3.24.1-aarch64.tar.gz",
    "sha256": "f55a90f69052c5bd6f92cb09a8f47065970830b194c917a006fb94028e721259",
}
ROOT_PACKAGES = (
    "ca-certificates", "e2fsprogs", "e2fsprogs-extra", "gptfdisk", "util-linux",
    "smartmontools", "iptables", "parted", "sgdisk",
)


def download(cache, entry, board_root=None, overrides=()):
    target = cache / entry["sha256"]
    if target.is_file() and sha(target) == entry["sha256"]:
        return target
    cache.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".fetch-", dir=cache) as tmp:
        partial = Path(tmp) / "download"
        if "path" in entry:
            shutil.copyfile(path_in(board_root, entry["path"]), partial)
        else:
            name = entry["file"] if "file" in entry else entry["url"].rsplit("/", 1)[-1]
            names = [name, entry["sha256"]]
            overrides = [x / n for x in overrides for n in names]
            found = next((p for p in overrides if p.is_file() and sha(p) == entry["sha256"]), None)
            if found:
                shutil.copyfile(found, partial)
            else:
                require("url" in entry, f"provide {name} with --firmware-dir (expected SHA-256 {entry['sha256']})")
                run(["curl", "--fail", "--silent", "--show-error", "--location", "--proto", "=https",
                     "--retry", "2", "--output", partial, entry["url"]])
        require(sha(partial) == entry["sha256"], "source download checksum differs")
        partial.replace(target)
    return target


def archive_source(args, component, entry):
    path = args.cache / "archives" / (entry["commit"] + ".tar")
    path.parent.mkdir(parents=True, exist_ok=True)
    strip = 0
    if args.archive_dir:
        path = args.archive_dir / f"rkdev-{component}-base.tar"
        require(path.is_file(), f"missing source archive: {path}")
    elif "archive" in entry:
        path = download(args.cache / "source-downloads", entry["archive"])
        strip = 1
    elif not path.is_file():
        with tempfile.TemporaryDirectory(prefix=".source-", dir=path.parent) as td:
            repo = Path(td) / "git"
            candidate = Path(td) / "source.tar"
            run(["git", "init", "--bare", "-q", repo])
            run(["git", "-C", repo, "-c", "http.version=HTTP/1.1", "fetch", "--depth=1", "--no-tags",
                 entry["url"], entry.get("ref", entry["commit"])])
            revision = run(["git", "-C", repo, "rev-parse", "FETCH_HEAD^{commit}"],
                           capture_output=True, text=True).stdout.strip()
            require(revision == entry["commit"], f"upstream {component} ref no longer matches sources.lock")
            run(["git", "-C", repo, "-c", "core.autocrlf=false", "archive", "--format=tar", "-o", candidate, revision])
            candidate.replace(path)
    opener = gzip.open if strip else open
    with opener(path, "rb") as f:
        result = run(["git", "get-tar-commit-id"], input=f.read(1024), capture_output=True)
    require(result.stdout.decode("ascii").strip() == entry["commit"], f"source archive does not match {component} lock")
    return path, strip


def copy_overlay(root, destination):
    for item in root.rglob("*"):
        require(not item.is_symlink(), f"symlinks in board overlays are unsupported: {item}")
        if not item.is_file():
            continue
        target = destination / item.relative_to(root)
        require(target.resolve().is_relative_to(destination.resolve()), f"overlay writes outside build: {item}")
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(item, target)


def source_key(board_root, board_path, cfg, data, component, lock):
    origin = {k: v for k, v in lock[component].items() if k != "archive"}
    h = hashlib.sha256(canonical([2, data, cfg[component], origin]))
    files = [path_in(board_root, cfg[component]["config"]),
             path_in(board_root, cfg[component]["dts"])]
    if component == "uboot":
        files += [CORE / "boot/rkdev.c", CORE / "tools/uboot.py"]
    files += sorted((CORE / ("boot" if component == "uboot" else "recovery/kernel") / "patches").glob("*.patch"))
    files += [path_in(board_root, p) for p in cfg[component].get("patches", [])]
    if "overlay" in cfg[component]:
        overlay = path_in(board_root, cfg[component]["overlay"], directory=True)
        files += sorted(p for p in overlay.rglob("*") if p.is_file())
    for file in files:
        h.update(file.name.encode())
        h.update(file.read_bytes())
    return h.hexdigest()[:20]


def stage_source(args, board_root, cfg, data, component, lock):
    key = source_key(board_root, args.board, cfg, data, component, lock)
    work = args.cache / "work" / f"{data['board_id']}-{data['profile']}" / f"{component}-{key}"
    source = work / "src"
    output = work / "build"
    if not source.is_dir():
        archive, strip = archive_source(args, component, lock[component])
        work.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix=".unpack-", dir=work) as path:
            stage = Path(path) / "src"
            stage.mkdir()
            run(["tar", "-xf", archive, f"--strip-components={strip}", "-C", stage])
            entry = cfg[component]
            patch_dir = CORE / ("boot" if component == "uboot" else "recovery/kernel") / "patches"
            for patch in sorted(patch_dir.glob("*.patch")):
                run(["git", "apply", patch], cwd=stage)
            if "overlay" in entry:
                copy_overlay(path_in(board_root, entry["overlay"], directory=True), stage)
            for patch in entry.get("patches", []):
                run(["git", "apply", "--check", path_in(board_root, patch)], cwd=stage)
                run(["git", "apply", path_in(board_root, patch)], cwd=stage)
            dts = path_in(board_root, entry["dts"])
            require(dts.suffix == ".dts", "board device tree must end with .dts")
            config = path_in(board_root, entry["config"]).read_text()
            if component == "uboot":
                inject(stage, CORE)
                dts_target = stage / "dts/upstream/src/arm64/rockchip" / dts.name
                (stage / "board/rockchip/rkdev/rkdev_profile.h").write_text(profile_header(cfg, data))
                options = settings(cfg, data)
                options.update(CONFIG_DEFAULT_DEVICE_TREE=f'"rockchip/{dts.stem}"',
                               CONFIG_DEFAULT_FDT_FILE=f'"rockchip/{dts.stem}.dtb"')
                config = put_settings(config, options, strip_targets=True)
                dts_target.write_text(dts.read_text(), encoding="utf-8")
                (stage / "configs/rkdev_defconfig").write_text(config)
            else:
                dts_target = stage / "arch/arm64/boot/dts/rockchip" / dts.name
                chosen = "\n/ { chosen {\n" + "".join(
                    f"  rkdev,{k.replace('_','-')} = {json.dumps(str(data[k]))};\n"
                    for k in ("board_id", "revision", "profile", "layout_id", "schema")
                ) + "}; };\n"
                dts_target.write_text(dts.read_text() + chosen, encoding="utf-8")
                config = put_settings(config, {"CONFIG_INITRAMFS_SOURCE": '""', "CONFIG_BLK_DEV_INITRD": "y",
                                               "CONFIG_RD_GZIP": "y", "CONFIG_DEVTMPFS": "y",
                                               "CONFIG_LOCALVERSION": '"-rkdev"', "CONFIG_LOCALVERSION_AUTO": "n"})
                (stage / "arch/arm64/configs/rkdev_defconfig").write_text(config)
            stage.rename(source)
    output.mkdir(exist_ok=True, parents=True)
    return source, output


def build_uboot(args, root, board, cfg, data, lock):
    source, out = stage_source(args, root, cfg, data, "uboot", lock)
    environment = os.environ.copy()
    environment.update(CROSS_COMPILE=args.cross_compile, SOURCE_DATE_EPOCH=str(args.epoch),
                       BL31=str(download(args.cache / "downloads", board["firmware"]["bl31"], root, args.firmware_dir)),
                       ROCKCHIP_TPL=str(download(args.cache / "downloads", board["firmware"]["ddr"], root, args.firmware_dir)))
    make = ["make", "-C", source, f"O={out}"]
    run([*make, "rkdev_defconfig"], env=environment)
    selected = set((out / ".config").read_text().splitlines())
    mandatory = {"CONFIG_TARGET_RKDEV_RK3588": "y", "CONFIG_BOARD_LATE_INIT": "y",
                 "CONFIG_CMD_SYSBOOT": "y", "CONFIG_HUSH_PARSER": "y",
                 ("CONFIG_ENV_IS_IN_MMC" if data["profile"] == "emmc" else "CONFIG_ENV_IS_IN_SPI_FLASH"): "y"}
    for target, cmd in (("sata", "SCSI"), ("nvme", "NVME"), ("usb", "USB"), ("sd", "MMC"), ("emmc", "MMC")):
        if target in data["boot_order"]:
            mandatory[f"CONFIG_CMD_{cmd}"] = "y"
    mandatory["CONFIG_CMD_ITEST"] = "y"
    if data["profile"] == "spi":
        mandatory.update(CONFIG_CMD_BLKMAP="y", CONFIG_CMD_SF="y", CONFIG_LZMA="y",
                         CONFIG_LEGACY_IMAGE_FORMAT="y", CONFIG_CMD_BOOTM="y")
    if cfg.get("boot_fallback") == "loader":
        mandatory["CONFIG_CMD_ROCKUSB"] = "y"
    if cfg.get("key", {}).get("action") == "host-or-recovery":
        mandatory["CONFIG_USB_GADGET"] = "y"
    require(all(f"{k}={v}" in selected for k, v in mandatory.items()), f"U-Boot lost required config: {mandatory}")
    run([*make, f"-j{args.jobs}"], env=environment)
    artifact = out / ("u-boot-rockchip-spi.bin" if data["profile"] == "spi" else "u-boot-rockchip.bin")
    env = out / "u-boot-initial-env"
    if not env.is_file():
        run([*make, "u-boot-initial-env"], env=environment)
    require(artifact.is_file() and env.is_file(), "missing U-Boot outputs")
    return artifact, env, out


def build_linux(args, root, cfg, data, lock):
    source, out = stage_source(args, root, cfg, data, "linux", lock)
    env = os.environ.copy()
    env.update(ARCH="arm64", CROSS_COMPILE=args.cross_compile, SOURCE_DATE_EPOCH=str(args.epoch),
               KBUILD_BUILD_USER="rkdev", KBUILD_BUILD_HOST="builder")
    dts = path_in(root, cfg["linux"]["dts"]).stem
    make = ["make", "-C", source, f"O={out}"]
    run([*make, "rkdev_defconfig"], env=env)
    run([*make, f"-j{args.jobs}", "Image", "modules", f"rockchip/{dts}.dtb"], env=env)
    modules = out / "installed-modules"
    run([*make, "modules_install", f"INSTALL_MOD_PATH={modules}", "INSTALL_MOD_STRIP=1"], env=env)
    version = run([*make, "-s", "kernelrelease"], env=env, capture_output=True, text=True).stdout.strip()
    dtb = out / "arch/arm64/boot/dts/rockchip" / (dts + ".dtb")
    compatible = run(["fdtget", "-t", "s", dtb, "/", "compatible"], capture_output=True, text=True).stdout.split()
    require(data["compatible"] in compatible, "recovery DT does not advertise the board compatible")
    return out / "arch/arm64/boot/Image", dtb, modules, version


def build_rootfs(args, root, board, cfg, data, stage, modules, kernelrelease, env_text):
    require(os.geteuid() == 0, "run the Alpine rootfs build under sudo on Linux/WSL")
    alpine = download(args.cache / "downloads", ROOTFS)
    rootfs = stage / "rootfs"
    rootfs.mkdir()
    run(["tar", "-xf", alpine, "-C", rootfs])
    for name, major, minor in (("null", 1, 3), ("urandom", 1, 9), ("random", 1, 8)):
        node = rootfs / "dev" / name
        if not node.exists():
            os.mknod(node, stat.S_IFCHR | 0o666, os.makedev(major, minor))
    repositories = rootfs / "etc/apk/repositories"
    repositories.write_text(repositories.read_text().replace("http://", "https://"))
    resolver = rootfs / "etc/resolv.conf"
    resolver.write_bytes(Path("/etc/resolv.conf").read_bytes())
    packages = tuple(dict.fromkeys((*ROOT_PACKAGES, *board.get("rootfs_packages", []))))
    run(["chroot", rootfs, "apk", "add", "--no-cache", *packages])
    run(["chroot", rootfs, "/bin/sh", "-ec", '''
        for tool in sgdisk lsblk blockdev blkid partprobe e2fsck debugfs smartctl iptables; do
            command -v "$tool" >/dev/null || { echo "missing recovery tool: $tool" >&2; exit 1; }
        done
    '''])
    resolver.unlink()
    copy_overlay(CORE / "recovery/rootfs", rootfs)
    if "rootfs_overlay" in board:
        copy_overlay(path_in(root, board["rootfs_overlay"], directory=True), rootfs)
    for path in (rootfs / "init", rootfs / "usr/sbin/rkdev-start"):
        path.chmod(0o755)
    network = rootfs / "etc/rkdev-network"
    if network.is_file():
        network.chmod(0o755)
    scripts = rootfs / "etc/rkdev-init.d"
    if scripts.is_dir():
        for script in scripts.iterdir():
            if script.is_file():
                script.chmod(0o755)
    (rootfs / "etc/rkdev-build-epoch").write_text(f"{args.epoch}\n")
    write_json(rootfs / "etc/rkdev-layout.json", data)
    (rootfs / "etc/rkdev-default-env").write_bytes(environment_text(data, env_text))
    installed = modules / "lib/modules" / kernelrelease
    if installed.is_dir():
        target = rootfs / "lib/modules" / kernelrelease
        shutil.copytree(installed, target, symlinks=True, ignore=shutil.ignore_patterns("build", "source"))
        run(["depmod", "-b", rootfs, kernelrelease])
    for dest, entry in cfg.get("firmware", {}).items():
        path = rootfs / "lib/firmware" / dest
        path.parent.mkdir(exist_ok=True, parents=True)
        shutil.copyfile(download(args.cache / "downloads", entry, root, args.firmware_dir), path)
    env = os.environ.copy()
    env.update(GOOS="linux", GOARCH="arm64", CGO_ENABLED="0")
    app = rootfs / "usr/bin/rkdev"
    run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags", f"-s -w -X main.buildTime={args.version}",
         "-o", app, "."], cwd=CORE / "recovery/app", env=env)
    cp = run(["chroot", rootfs, "apk", "info", "-v"], capture_output=True, text=True)
    return rootfs, sorted(cp.stdout.splitlines())


def make_payload(args, cfg, data, stage, rootfs, linux, dtb):
    content = stage / "payload"
    (content / "extlinux").mkdir(parents=True)
    initrd = content / "initramfs.cpio.gz"
    with initrd.open("wb") as out:
        first = subprocess.Popen(["find", ".", "-xdev", "-print0"], cwd=rootfs, stdout=subprocess.PIPE)
        try:
            second = subprocess.Popen(["cpio", "--null", "-o", "--quiet", "-H", "newc", "--owner=0:0"],
                                      cwd=rootfs, stdin=first.stdout, stdout=subprocess.PIPE)
            first.stdout.close()
            try:
                run(["pigz", "-9", "-n", "-c"], stdin=second.stdout, stdout=out)
            finally:
                second.stdout.close()
                require(second.wait() == 0, "cpio generation failed")
        finally:
            require(first.wait() == 0, "rootfs enumeration failed")
    kernel = "Image"
    if data["profile"] == "spi":
        packed = stage / "Image.lzma"
        with packed.open("wb") as f:
            run(["xz", "--format=lzma", "-9", "-c", linux], stdout=f)
        kernel = "Image.uimg"
        run(["mkimage", "-A", "arm64", "-O", "linux", "-T", "kernel", "-C", "lzma",
             "-a", "0x02080000", "-e", "0x02080000", "-n", "RKdev recovery", "-d", packed, content / kernel])
    else:
        shutil.copyfile(linux, content / kernel)
    shutil.copyfile(dtb, content / "rkdev.dtb")
    write_json(content / "layout.json", data)
    (content / "extlinux/extlinux.conf").write_text(
        f"default rkdev\ntimeout 0\nlabel rkdev\n  menu label RKdev Recovery\n"
        f"  kernel /{kernel}\n  fdt /rkdev.dtb\n  initrd /initramfs.cpio.gz\n"
        f"  append rdinit=/init rw {cfg.get('recovery_args', '')}\n"
    )
    output = stage / "recovery.ext4"
    with output.open("wb") as f:
        f.truncate(data["recovery"]["length"])
    run(["mkfs.ext4", "-q", "-F", "-m", "0", "-L", data["recovery"]["label"],
         "-U", uuid.UUID(hex=data["layout_id"][:32]), "-d", content,
         "-O", "^has_journal,^metadata_csum,^64bit,^resize_inode", output])
    run(["e2fsck", "-fn", output])
    return output, kernel, content


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("lint", "prepare", "boot", "kernel", "pack", "verify", "build"))
    parser.add_argument("--board", type=Path, required=True)
    parser.add_argument("--profile", choices=("spi", "emmc"))
    parser.add_argument("--out", type=Path, default=Path("out"))
    parser.add_argument("--cache", type=Path, default=Path(os.environ.get("RKDEV_CACHE", ".build")))
    parser.add_argument("--archive-dir", type=Path, help="offline Git archives of the locked revisions")
    parser.add_argument("--firmware-dir", type=Path, action="append", default=[])
    parser.add_argument("--cross-compile", default="aarch64-linux-gnu-")
    parser.add_argument("--jobs", type=int, default=max(1, os.cpu_count() or 1))
    parser.add_argument("--version", default="0.1-dev")
    parser.add_argument("--epoch", type=int, default=int(os.environ.get("SOURCE_DATE_EPOCH", 1795305600)))
    parser.add_argument("--locked", action="store_true", help="the build always uses sources.lock")
    parser.add_argument("--image", type=Path)
    parser.add_argument("--loader", type=Path)
    parser.add_argument("--default-env", type=Path)
    parser.add_argument("--recovery", type=Path)
    args = parser.parse_args(argv)
    args.cache = args.cache.resolve()
    args.out = args.out.resolve()
    try:
        require(1 <= args.jobs <= 256, "--jobs must be between 1 and 256")
        require(re.fullmatch(r"[a-zA-Z0-9._-]{1,64}", args.version), "invalid version")
        root, board, profile, cfg = load_board(args.board, args.profile)
        data = runtime(board, profile, cfg)
        lock = read_json(CORE / "sources.lock")
        trim = data["loader"]["offset"] if profile == "spi" else 0
        if args.command == "lint":
            print(f"{data['board_id']}/{profile}: {data['layout_id']}")
            return 0
        args.out.mkdir(parents=True, exist_ok=True)
        if args.command == "prepare":
            write_json(args.out / "layout.json", data)
            (args.out / "fw_env.config").write_text(fw_env_config(data))
            (args.out / "rkdev_profile.h").write_text(profile_header(cfg, data))
        elif args.command == "verify":
            require(args.image is not None, "--image is required")
            verify(data, args.image, read_json(args.image.with_suffix(".json")), check_filesystem=True)
            print(f"verified {args.image}")
        elif args.command == "pack":
            require(all((args.image, args.loader, args.default_env, args.recovery)), "pack needs --image, --loader, --default-env and --recovery")
            pack(data, args.loader, args.default_env, args.recovery, args.image, spi_trim=trim)
        else:
            if args.command in ("boot", "build"):
                loader, env, boot_out = build_uboot(args, root, board, cfg, data, lock)
                shutil.copyfile(loader, args.out / loader.name)
                shutil.copyfile(env, args.out / env.name)
            if args.command in ("kernel", "build"):
                image, dtb, modules, version = build_linux(args, root, cfg, data, lock)
                shutil.copyfile(image, args.out / "Image")
                shutil.copyfile(dtb, args.out / "rkdev.dtb")
            if args.command == "build":
                with tempfile.TemporaryDirectory(prefix="build-", dir=args.cache) as td:
                    stage = Path(td)
                    rootfs, packages = build_rootfs(args, root, board, cfg, data, stage, modules, version, env.read_bytes())
                    payload, kernel, content = make_payload(args, cfg, data, stage, rootfs, image, dtb)
                    name = f"{board['board_id']}-{board['revision']}-{profile}-{args.version}.img"
                    info = {**lock, "version": args.version, "kernel_release": version,
                            "firmware": {k: v["sha256"] for k, v in board["firmware"].items()},
                            "rootfs": {**ROOTFS, "apk": packages},
                            "cc": run([args.cross_compile + "gcc", "--version"], capture_output=True, text=True).stdout.splitlines()[0]}
                    manifest = pack(data, loader, env, payload, args.out / name, source_info=info, spi_trim=trim)
                    (args.out / "SHA256SUMS").write_text(f"{manifest['image']['sha256']}  {name}\n")
                    print(f"verified recovery image: {args.out / name}")
    except (Invalid, OSError, KeyError, TypeError, subprocess.CalledProcessError, json.JSONDecodeError) as error:
        print(f"rkdev: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

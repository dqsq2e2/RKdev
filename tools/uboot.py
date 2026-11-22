import json
import re

from contract import require

SCAN = (
    "if part list ${devtype} ${devnum} rkdev_parts; then "
    "for p in ${rkdev_parts}; do setenv rkdev_label; part name ${devtype} ${devnum} 0x${p} rkdev_label; "
    'if test "${rkdev_label}" != "${rkdev_rec_label}"; then '
    "for dir in / /boot/; do "
    "if test -e ${devtype} ${devnum}:0x${p} ${dir}extlinux/extlinux.conf; then "
    "sysboot ${devtype} ${devnum}:0x${p} any ${scriptaddr} ${dir}extlinux/extlinux.conf; fi; "
    "if load ${devtype} ${devnum}:0x${p} ${scriptaddr} ${dir}boot.scr; then source ${scriptaddr}; fi; "
    "done; "
    "if test -e ${devtype} ${devnum}:0x${p} /extlinux.conf; then "
    "sysboot ${devtype} ${devnum}:0x${p} any ${scriptaddr} /extlinux.conf; fi; fi; done; fi"
)


def settings(config, data):
    env = data["environment"]
    settings = {
        "CONFIG_TARGET_RKDEV_RK3588": "y",
        "CONFIG_USE_BOOTCOMMAND": "y",
        "CONFIG_BOOTCOMMAND": '"rkdevboot"',
        "CONFIG_BOARD_LATE_INIT": "y",
        "CONFIG_BUTTON_CMD": "n",
        "CONFIG_HUSH_PARSER": "y",
        "CONFIG_CMD_FS_GENERIC": "y",
        "CONFIG_CMD_BOOTI": "y",
        "CONFIG_CMD_SYSBOOT": "y",
        "CONFIG_CMD_EXT4": "y",
        "CONFIG_CMD_PART": "y",
        "CONFIG_PARTITION_UUIDS": "y",
        "CONFIG_ENV_IS_IN_MMC": "y" if data["profile"] == "emmc" else "n",
        "CONFIG_ENV_IS_IN_SPI_FLASH": "y" if data["profile"] == "spi" else "n",
        "CONFIG_ENV_IS_NOWHERE": "n",
        "CONFIG_ENV_REDUNDANT": "y" if len(env["offsets"]) == 2 else "n",
        "CONFIG_ENV_SIZE": hex(env["size"]),
        "CONFIG_ENV_OFFSET": hex(env["offsets"][0]),
    }
    if len(env["offsets"]) == 2:
        settings["CONFIG_ENV_OFFSET_REDUND"] = hex(env["offsets"][1])
    if data["profile"] == "emmc":
        settings.update(CONFIG_SYS_MMC_ENV_DEV=str(config.get("mmc_device", 0)), CONFIG_SYS_MMC_ENV_PART="0")
    else:
        settings.update(CONFIG_ENV_SECT_SIZE=hex(data["erase_size"]), CONFIG_ROCKCHIP_SPI_IMAGE="y",
                        CONFIG_CMD_SF="y", CONFIG_BLKMAP="y", CONFIG_CMD_BLKMAP="y", CONFIG_LZMA="y",
                        CONFIG_LEGACY_IMAGE_FORMAT="y", CONFIG_CMD_BOOTM="y")
    if "sata" in data["boot_order"]:
        settings.update(CONFIG_AHCI="y", CONFIG_SCSI="y", CONFIG_CMD_SCSI="y")
    if "nvme" in data["boot_order"]:
        settings.update(CONFIG_NVME_PCI="y", CONFIG_CMD_NVME="y")
    if "rkfw" in config:
        settings.update(CONFIG_CMD_READ="y", CONFIG_CMD_ITEST="y", CONFIG_CMD_BOOTI="y")
    return settings


def put_settings(text, settings, strip_targets=False):
    lines = []
    for line in text.splitlines():
        m = re.match(r"(?:# )?(CONFIG_[A-Za-z0-9_]+)(?:=| is not set)", line)
        if m and (m[1] in settings or (strip_targets and m[1].startswith("CONFIG_TARGET_"))):
            continue
        lines.append(line)
    lines.extend(f"# {k} is not set" if v == "n" else f"{k}={v}" for k, v in sorted(settings.items()))
    return "\n".join(lines) + "\n"


def profile_header(cfg, data):
    def quoted(s):
        return json.dumps(s, ensure_ascii=True)

    def scan(prefix):
        script = prefix + f" setenv rkdev_rec_label {data['recovery']['label']}; " + SCAN
        if "rkfw" in cfg:
            k = cfg["rkfw"]
            script += (
                f"; if read ${{devtype}} ${{devnum}}:{k['kernel']:x} 0x01fffff8 0 0x14000 && "
                "itest.l *0x01fffff8 == 0x4c4e524b && "
                f"read ${{devtype}} ${{devnum}}:{k['initrd']:x} 0x04fffff8 0 0x10000 && "
                "itest.l *0x04fffff8 == 0x4c4e524b && "
                f"read ${{devtype}} ${{devnum}}:{k['resource']:x} 0x07000000 0 0x1000 && "
                "itest.l *0x07000000 == 0x45435352 && itest.l *0x07000800 == 0xedfe0dd0 && "
                f"part uuid ${{devtype}} ${{devnum}}:{k['root']:x} rkdev_uuid; then "
                f"setenv bootargs root=PARTUUID=${{rkdev_uuid}} {k['args']}; "
                "booti 0x02000000 0x05000000:0x2000000 0x07000800; fi"
            )
        return script

    emmc = cfg.get("mmc_device", 0)
    sd = cfg.get("sd_device", 1)
    targets = {
        "emmc": scan(f"setenv devtype mmc; setenv devnum {emmc}; if mmc dev {emmc} && mmc rescan; then") + "; fi",
        "sd": scan(f"setenv devtype mmc; setenv devnum {sd}; if mmc dev {sd} && mmc rescan; then") + "; fi",
        "usb": scan("usb start; setenv devtype usb; for devnum in 0 1; do if usb dev ${devnum}; then") + "; fi; done",
        "nvme": scan("pci enum; nvme scan; setenv devtype nvme; for devnum in 0 1; do if nvme dev ${devnum}; then") + "; fi; done",
        "sata": scan("scsi scan; setenv devtype scsi; for devnum in 0 1; do if scsi dev ${devnum}; then") + "; fi; done",
    }
    part = data["recovery"]
    if data["profile"] == "emmc":
        start = part["offset"] // 512
        sectors = part["length"] // 512
        recovery = (
            f"if mmc dev {emmc} && mmc rescan && part name mmc {emmc} 1 rkdev_label && "
            f"part start mmc {emmc} 1 rkdev_start && part size mmc {emmc} 1 rkdev_size && "
            f'itest ${{rkdev_start}} == 0x{start:x} && itest ${{rkdev_size}} == 0x{sectors:x} && '
            f'test "${{rkdev_label}}" = "{part["label"]}"; then '
            f"sysboot mmc {emmc}:1 any ${{scriptaddr}} /extlinux/extlinux.conf; fi"
        )
        loader = f"rockusb 0 mmc {cfg.get('loader_index', emmc)}"
    else:
        addr, offset, length = (part[k] for k in ("load_address", "offset", "length"))
        recovery = (
            f"if sf probe 0 && sf read 0x{addr:x} 0x{offset:x} 0x{length:x}; then "
            "if blkmap create rkdev; then "
            f"if blkmap map rkdev 0 0x{length // 512:x} mem 0x{addr:x} && "
            "blkmap get rkdev dev rkdev_map; then "
            "sysboot blkmap ${rkdev_map}:0 any ${scriptaddr} /extlinux/extlinux.conf; fi; blkmap destroy rkdev; fi; fi"
        )
        loader = ""
    key = cfg.get("key", {})
    values = {
        "RKDEV_PROFILE": quoted(data["profile"]),
        "RKDEV_LAYOUT": quoted(data["layout_id"]),
        "RKDEV_ORDER": quoted(" ".join(data["boot_order"])),
        "RKDEV_RECOVERY": quoted(recovery),
        "RKDEV_RECOVERY_KERNEL_ADDR": quoted("0x0a000000" if data["profile"] == "spi" else "0x02000000"),
        "RKDEV_LOADER": quoted(loader if cfg.get("boot_fallback") == "loader" else ""),
        "RKDEV_FALLBACK_MASKROM": int(cfg.get("boot_fallback") == "maskrom"),
        "RKDEV_KEY": {"adc": 1, "button": 2}.get(key.get("type"), 0),
        "RKDEV_ACTION": {"recovery": 1, "maskrom": 2, "host-or-recovery": 3}.get(key.get("action"), 0),
        "RKDEV_ADC": key.get("channel", 0),
        "RKDEV_ADC_LOW": key.get("minimum", 0),
        "RKDEV_ADC_HIGH": key.get("maximum", 0),
        "RKDEV_BUTTON": quoted(key.get("label", "")),
        "RKDEV_KEY_SAMPLES": key.get("samples", 1),
        "RKDEV_KEY_DELAY": key.get("delay_ms", 20),
        "RKDEV_USB_NODE": quoted(key.get("usb_node", "")),
        "RKDEV_USB_WAIT": key.get("wait_ms", 2500),
        "RKDEV_TARGETS": ", ".join("{%s,%s}" % (quoted(name), quoted(targets[name]))
                                    for name in data["boot_order"]),
    }
    return "/* Generated by RKdev. */\n" + "".join(f"#define {k} {v}\n" for k, v in values.items())


def inject(source, core):
    mach = source / "arch/arm/mach-rockchip"
    kconfig = mach / "rk3588/Kconfig"
    text = kconfig.read_text()
    marker = '\nconfig SYS_CONFIG_NAME\n\tdefault "rk3588_common"'
    require(text.count(marker) == 1, "U-Boot source no longer matches RKdev Kconfig integration")
    kconfig.write_text(text.replace(marker, """
config TARGET_RKDEV_RK3588
	bool "RKdev RK3588 external board package"
	select BOARD_LATE_INIT

config SYS_BOARD
	default "rkdev" if TARGET_RKDEV_RK3588

config SYS_VENDOR
	default "rockchip" if TARGET_RKDEV_RK3588
""" + marker))
    main = mach / "board.c"
    text = main.read_text()
    marker = "\tsetup_boot_mode();"
    require(text.count(marker) == 1, "U-Boot source no longer matches RKdev boot hook")
    main.write_text(text.replace("int board_late_init(void)", """
#if IS_ENABLED(CONFIG_TARGET_RKDEV_RK3588)
void rkdev_prepare_boot(void);
#endif

int board_late_init(void)""", 1).replace(marker, """#if IS_ENABLED(CONFIG_TARGET_RKDEV_RK3588)
	rkdev_prepare_boot();
#else
	setup_boot_mode();
#endif""", 1))
    project = source / "board/rockchip/rkdev"
    project.mkdir(parents=True, exist_ok=True)
    (project / "rkdev.c").write_bytes((core / "boot/rkdev.c").read_bytes())
    (project / "Makefile").write_text(
        "ifndef CONFIG_XPL_BUILD\nobj-y += rkdev.o\n"
        "ifneq ($(wildcard $(srctree)/board/rockchip/rkdev/board.c),)\nobj-y += board.o\nendif\nendif\n")

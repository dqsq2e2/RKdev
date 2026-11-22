import tempfile
import unittest
from pathlib import Path

from contract import Invalid, load_board, runtime, write_json
from uboot import profile_header


class BoardContract(unittest.TestCase):
    def setUp(self):
        td = tempfile.TemporaryDirectory()
        self.addCleanup(td.cleanup)
        self.board = Path(td.name)
        for file in ("kernel.config", "board.dts", "boot.config"):
            (self.board / file).touch()
        self.cfg = {
            "device": "/dev/mmcblk3",
            "capacity": 16 << 20,
            "loader": {"offset": 32768, "length": (1 << 20) - 32768, "format": "rockchip-sd"},
            "environment": {"offsets": [1 << 20, (1 << 20) + 8192], "size": 8192},
            "recovery": {"offset": 2 << 20, "length": 3 << 20, "label": "FIXTURE", "partition": 1},
            "system": {"offset": 6 << 20, "label": "SYSTEM"},
            "boot_order": ["usb", "emmc"],
            "uboot": {"config": "boot.config", "dts": "board.dts"},
            "linux": {"config": "kernel.config", "dts": "board.dts"},
            "boot_fallback": "loader",
            "mmc_device": 2,
        }
        self.data = {
            "schema": 1, "board_id": "fixture", "revision": "v1", "name": "Fixture", "soc": "rk3588",
            "compatible": "example,fixture", "default_profile": "emmc",
            "firmware": {k: {"file": k, "sha256": "ab" * 32} for k in ("bl31", "ddr")},
            "profiles": {"emmc": self.cfg},
        }

    def load(self, profile=None):
        write_json(self.board / "board.json", self.data)
        _, board, mode, cfg = load_board(self.board, profile)
        return runtime(board, mode, cfg)

    def test_custom_emmc_geometry_binds_boot_and_runtime(self):
        first = self.load()
        self.assertIn("part start mmc 2 1", profile_header(self.cfg, first))
        self.assertIn("itest ${rkdev_start} == 0x1000", profile_header(self.cfg, first))
        self.cfg["recovery"]["offset"] += 1 << 20
        moved = self.load()
        self.assertNotEqual(first["layout_id"], moved["layout_id"])
        self.assertIn("itest ${rkdev_start} == 0x1800", profile_header(self.cfg, moved))

    def test_rejects_unsupported_mode_and_overlapping_environment(self):
        with self.assertRaisesRegex(Invalid, "does not support profile"):
            self.load("spi")
        self.cfg["environment"]["offsets"][1] -= 512
        with self.assertRaisesRegex(Invalid, "overlaps"):
            self.load()

    def test_spi_buffer_and_erase_boundaries(self):
        self.data["profiles"] = {"spi": self.cfg}
        self.data["default_profile"] = "spi"
        self.cfg.pop("system")
        self.cfg.update(device="/dev/mtdblock2", erase_size=32768, mtd_name="flash.2", boot_fallback="maskrom")
        self.cfg["loader"]["format"] = "rockchip-spi"
        self.cfg["environment"]["offsets"] = [1 << 20]
        self.cfg["recovery"].pop("partition")
        self.cfg["recovery"]["load_address"] = 0x40000000
        valid = self.load()
        self.assertIn("blkmap get rkdev dev", profile_header(self.cfg, valid))
        self.cfg["recovery"]["load_address"] = 0x02000000
        with self.assertRaisesRegex(Invalid, "load address"):
            self.load()

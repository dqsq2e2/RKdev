import subprocess
import tempfile
import tarfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

from rkdev import apply_source_patch, source_key, stage_source


class SourcePatches(unittest.TestCase):
    def test_applies_patch_inside_another_git_repository(self):
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory).resolve()
            subprocess.run(["git", "init", "--quiet", repo], check=True)
            stage = repo / ".build" / "src"
            stage.mkdir(parents=True)
            content = stage / "fixture"
            content.write_bytes(b"old\n")
            patch = repo / "change.patch"
            patch.write_bytes(b"diff --git a/fixture b/fixture\n"
                              b"--- a/fixture\n+++ b/fixture\n@@ -1 +1 @@\n-old\n+new\n")

            apply_source_patch(stage, patch)

            self.assertEqual(content.read_bytes(), b"new\n")


class SharedLinuxLogo(unittest.TestCase):
    def test_public_logo_overrides_board_overlay_and_invalidates_cache(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            core = root / "core"
            logo = core / "recovery/kernel/logo_linux_clut224.ppm"
            logo.parent.mkdir(parents=True)
            logo.write_bytes(b"shared logo")
            board = root / "board"
            overlay_logo = board / "overlay/drivers/video/logo/logo_linux_clut224.ppm"
            overlay_logo.parent.mkdir(parents=True)
            overlay_logo.write_bytes(b"board logo")
            (board / "board.dts").write_text("/dts-v1/;\n/ {};\n")
            (board / "linux.config").write_text("# CONFIG_LOGO is not set\nCONFIG_LOGO_LINUX_MONO=y\n")
            upstream = root / "upstream"
            for path in ("drivers/video/logo", "arch/arm64/configs", "arch/arm64/boot/dts/rockchip"):
                (upstream / path).mkdir(parents=True)
            archive = root / "source.tar"
            with tarfile.open(archive, "w") as stream:
                stream.add(upstream, arcname=".")
            cfg = {"linux": {"config": "linux.config", "dts": "board.dts", "overlay": "overlay"}}
            data = dict(board_id="fixture", profile="emmc", revision="v1", layout_id="layout", schema=1)
            lock = {"linux": {"url": "https://example.com/linux.git", "commit": "a" * 40}}
            args = SimpleNamespace(board=board, cache=root / "cache")

            with patch("rkdev.CORE", core), patch("rkdev.archive_source", return_value=(archive, 0)):
                source, _ = stage_source(args, board, cfg, data, "linux", lock)
                self.assertEqual((source / "drivers/video/logo/logo_linux_clut224.ppm").read_bytes(), b"shared logo")
                config = (source / "arch/arm64/configs/rkdev_defconfig").read_text().splitlines()
                self.assertIn("CONFIG_LOGO=y", config)
                self.assertIn("CONFIG_LOGO_LINUX_CLUT224=y", config)
                self.assertIn("# CONFIG_LOGO_LINUX_MONO is not set", config)
                self.assertIn("# CONFIG_FRAMEBUFFER_CONSOLE_DEFERRED_TAKEOVER is not set", config)
                before = source_key(board, board, cfg, data, "linux", lock)
                logo.write_bytes(b"updated shared logo")
                self.assertNotEqual(source_key(board, board, cfg, data, "linux", lock), before)

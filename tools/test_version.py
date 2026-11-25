from concurrent.futures import ThreadPoolExecutor
from datetime import date
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from version import allocate_version, build_date


class DailyVersions(unittest.TestCase):
    def test_daily_sequence_reserves_failed_builds_and_resets_next_day(self):
        with tempfile.TemporaryDirectory() as directory:
            self.assertEqual(allocate_version(directory, date(2026, 11, 26)), "11.26-r1")
            self.assertEqual(allocate_version(directory, date(2026, 11, 26)), "11.26-r2")
            self.assertEqual(allocate_version(directory, date(2026, 11, 27)), "11.27-r1")

    def test_existing_image_advances_sequence(self):
        with tempfile.TemporaryDirectory() as directory:
            (Path(directory) / "board-v1-emmc-11.26-r4.img").touch()
            self.assertEqual(allocate_version(directory, date(2026, 11, 26)), "11.26-r5")

    def test_concurrent_builds_get_distinct_versions(self):
        with tempfile.TemporaryDirectory() as directory, ThreadPoolExecutor(max_workers=8) as workers:
            versions = list(workers.map(lambda _: allocate_version(directory, date(2026, 11, 26)), range(12)))
            self.assertEqual(set(versions), {f"11.26-r{revision}" for revision in range(1, 13)})

    def test_cloud_build_date_override(self):
        with patch.dict("os.environ", {"RKDEV_BUILD_DATE": "2026-11-26"}):
            self.assertEqual(build_date(), date(2026, 11, 26))

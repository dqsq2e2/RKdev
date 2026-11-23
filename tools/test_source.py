import subprocess
import tempfile
import unittest
from pathlib import Path

from rkdev import apply_source_patch


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

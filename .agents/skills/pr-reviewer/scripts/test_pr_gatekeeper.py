"""Regression tests using real Git index and working-tree states."""

import tempfile
import unittest
from pathlib import Path

from pr_gatekeeper import changed_files, check_forbidden_files, git


class ChangedFilesTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        git(self.repo, "init")
        git(self.repo, "config", "user.name", "Gatekeeper Test")
        git(self.repo, "config", "user.email", "test@example.invalid")
        git(self.repo, "commit", "--allow-empty", "-m", "Base")
        self.base = git(self.repo, "rev-parse", "HEAD")

    def test_reversed_changes_still_block_secrets(self):
        for committed in (False, True):
            with self.subTest(committed=committed):
                path = self.repo / "credentials.json"
                path.write_text("{}", encoding="utf-8")
                git(self.repo, "add", "credentials.json")
                if committed:
                    git(self.repo, "commit", "-m", "Add fixture")
                path.unlink()
                files = changed_files(self.repo, self.base)
                self.assertIn("credentials.json", files)
                self.assertFalse(check_forbidden_files(files).passed)

    def test_all_change_states_are_collected_once(self):
        for name in ("committed.txt", "staged.txt", "unstaged.txt"):
            (self.repo / name).write_text("base", encoding="utf-8")
        git(self.repo, "add", ".")
        git(self.repo, "commit", "-m", "Add files")
        self.base = git(self.repo, "rev-parse", "HEAD")
        (self.repo / "committed.txt").write_text("committed", encoding="utf-8")
        git(self.repo, "add", ".")
        git(self.repo, "commit", "-m", "Change file")
        (self.repo / "committed.txt").write_text("base", encoding="utf-8")
        (self.repo / "staged.txt").write_text("staged", encoding="utf-8")
        git(self.repo, "add", "staged.txt")
        (self.repo / "staged.txt").write_text("base", encoding="utf-8")
        (self.repo / "unstaged.txt").write_text("unstaged", encoding="utf-8")
        (self.repo / "untracked.txt").write_text("untracked", encoding="utf-8")
        self.assertEqual(
            changed_files(self.repo, self.base),
            ["committed.txt", "staged.txt", "unstaged.txt", "untracked.txt"],
        )


if __name__ == "__main__":
    unittest.main()

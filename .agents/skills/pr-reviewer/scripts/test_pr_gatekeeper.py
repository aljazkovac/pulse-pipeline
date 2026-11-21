"""Regression tests using real Git index and working-tree states."""

import tempfile
import subprocess
import sys
import unittest
from pathlib import Path

from pr_gatekeeper import changed_files, check_forbidden_files, git

SCRIPT = Path(__file__).with_name("pr_gatekeeper.py").resolve()


class ChangedFilesTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        git(self.repo, "init")
        git(self.repo, "config", "user.name", "Gatekeeper Test")
        git(self.repo, "config", "user.email", "test@example.invalid")
        git(self.repo, "config", "commit.gpgsign", "false")
        git(self.repo, "config", "core.hooksPath", "/dev/null")
        git(self.repo, "commit", "--allow-empty", "-m", "Base")
        self.base = git(self.repo, "rev-parse", "HEAD")

    def run_push_scan(self, before):
        return subprocess.run(
            [sys.executable, str(SCRIPT), "--skip-make", "--no-fetch", "--push-before", before],
            cwd=self.repo, capture_output=True, text=True, check=False,
        )

    def test_push_scans_commits_even_when_main_points_to_head(self):
        (self.repo / ".env").write_text("TOKEN=placeholder", encoding="utf-8")
        git(self.repo, "add", ".env")
        git(self.repo, "commit", "-m", "Add secret")
        git(self.repo, "rm", ".env")
        git(self.repo, "commit", "-m", "Remove secret")
        git(self.repo, "update-ref", "refs/remotes/origin/main", "HEAD")
        result = self.run_push_scan(self.base)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("    .env", result.stdout)

    def test_push_excludes_secrets_already_on_base(self):
        (self.repo / ".env").write_text("TOKEN=placeholder", encoding="utf-8")
        git(self.repo, "add", ".env")
        git(self.repo, "commit", "-m", "Existing secret")
        before = git(self.repo, "rev-parse", "HEAD")
        (self.repo / "safe.txt").write_text("safe", encoding="utf-8")
        git(self.repo, "add", "safe.txt")
        git(self.repo, "commit", "-m", "Safe push")
        result = self.run_push_scan(before)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("- safe.txt", result.stdout)

    def test_initial_push_includes_root_secret_even_if_removed(self):
        (self.repo / ".env").write_text("TOKEN=placeholder", encoding="utf-8")
        git(self.repo, "add", ".env")
        git(self.repo, "commit", "--amend", "--no-edit")
        git(self.repo, "rm", ".env")
        git(self.repo, "commit", "-m", "Remove root secret")
        result = self.run_push_scan("0" * 40)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("    .env", result.stdout)

    def test_initial_push_allows_safe_root_files(self):
        (self.repo / ".env.example").write_text("TOKEN=", encoding="utf-8")
        git(self.repo, "add", ".env.example")
        git(self.repo, "commit", "--amend", "--no-edit")
        result = self.run_push_scan("0" * 40)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("- .env.example", result.stdout)

    def test_push_with_missing_boundary_fails_closed(self):
        result = self.run_push_scan("1" * 40)
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertIn("gatekeeper error", result.stderr)

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

    def test_secret_removed_in_later_commit_still_blocks(self):
        (self.repo / "credentials.json").write_text("{}", encoding="utf-8")
        git(self.repo, "add", "credentials.json")
        git(self.repo, "commit", "-m", "Add secret")
        git(self.repo, "rm", "credentials.json")
        git(self.repo, "commit", "-m", "Remove secret")
        files = changed_files(self.repo, self.base)
        self.assertIn("credentials.json", files)
        self.assertFalse(check_forbidden_files(files).passed)

    def test_deleting_secret_from_base_does_not_block(self):
        (self.repo / ".env").write_text("TOKEN=placeholder", encoding="utf-8")
        git(self.repo, "add", ".env")
        git(self.repo, "commit", "-m", "Secret on base")
        self.base = git(self.repo, "rev-parse", "HEAD")
        git(self.repo, "rm", ".env")
        files = changed_files(self.repo, self.base)
        self.assertNotIn(".env", files)
        self.assertTrue(check_forbidden_files(files).passed)
        git(self.repo, "commit", "-m", "Remove secret")
        files = changed_files(self.repo, self.base)
        self.assertNotIn(".env", files)
        self.assertTrue(check_forbidden_files(files).passed)

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

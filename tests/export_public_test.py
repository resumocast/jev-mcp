import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("public_export", Path(__file__).resolve().parents[1] / "tools/export_public.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class PublicExportTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"; self.repo.mkdir()
        self.git("init", "-q")
        (self.repo / "README.md").write_text("Public product documentation\n")
        (self.repo / "public-files.json").write_text(json.dumps(["README.md"]))
        (self.repo / "private-notes.txt").write_text("Private evidence, never export\n")
        self.commit()

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.repo), *args], stderr=subprocess.DEVNULL)

    def commit(self):
        self.git("add", ".")
        self.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture")

    def test_export_uses_commit_allowlist_not_worktree_or_history(self):
        (self.repo / "README.md").write_text("Uncommitted private change\n")
        dest = self.root / "export"
        receipt = module.export(self.repo, "HEAD", dest)
        self.assertEqual((dest / "README.md").read_text(), "Public product documentation\n")
        self.assertEqual({p.name for p in dest.iterdir()}, {"README.md", "EXPORT.json"})
        self.assertFalse(receipt["history_included"])

    def test_forbidden_paths_are_refused(self):
        for name in ("../private-notes.txt", "research/case.json", ".git/config", ".env", "secret.key"):
            with self.subTest(name=name):
                (self.repo / "public-files.json").write_text(json.dumps([name]))
                self.commit()
                with self.assertRaises(ValueError): module.export(self.repo, "HEAD", self.root / "export")

    def test_symlink_refused(self):
        (self.repo / "README.md").unlink()
        (self.repo / "README.md").symlink_to("private-notes.txt")
        self.commit()
        with self.assertRaises(ValueError): module.export(self.repo, "HEAD", self.root / "export")

    def test_secret_pattern_refused_without_creating_output(self):
        (self.repo / "README.md").write_text("sk-proj-" + "A" * 24)
        self.commit()
        with self.assertRaises(ValueError): module.export(self.repo, "HEAD", self.root / "export")
        self.assertFalse((self.root / "export").exists())

    def test_additional_secret_formats_are_detected(self):
        for value in ("sk-" + "A" * 40, "github_pat_" + "A" * 30,
                      "AKIA" + "A" * 16, "xoxb-" + "1" * 20, "AIza" + "A" * 35):
            self.assertIsNotNone(module.SECRET.search(value.encode()))
        self.assertIsNone(module.SECRET.search(("opaque_base64_sk-" + "A" * 40).encode()))

    def test_existing_destination_and_duplicate_allowlist_refused(self):
        dest = self.root / "export"; dest.mkdir()
        with self.assertRaises(ValueError): module.export(self.repo, "HEAD", dest)
        (self.repo / "public-files.json").write_text('["README.md", "README.md"]')
        self.commit()
        with self.assertRaises(ValueError): module.export(self.repo, "HEAD", self.root / "other")


if __name__ == "__main__":
    unittest.main()

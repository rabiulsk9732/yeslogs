import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("console_release", Path(__file__).parents[1] / "console-release.py")
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "source"
        self.repo.mkdir()
        self.git("init", "-b", "main")
        self.git("config", "user.name", "Console Test")
        self.git("config", "user.email", "test@example.invalid")
        self.console = self.repo / release.CONSOLE
        (self.console / "assets").mkdir(parents=True)
        (self.console / "assets/app.js").write_text("const healthy = true;\n")
        (self.console / "index.html").write_text('<html><script src="/assets/app.js"></script><div id="login"></div><input id="li-email"><input id="li-pass"><button id="li-btn"></button><div id="app"></div></html>')
        (self.repo / "go.mod").write_text("module example.invalid/test\n")
        self.baseline = self.commit()
        self.deploy = self.root / "web"

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.repo), *args], stderr=subprocess.DEVNULL).decode().strip()

    def commit(self):
        self.git("add", ".")
        self.git("commit", "-m", "test")
        return self.git("rev-parse", "HEAD")

    def build(self, revision):
        output = self.deploy / "releases" / revision
        release.build(self.repo, revision, output)
        return output

    def test_committed_snapshot_pins_assets_and_ignores_dirty_files(self):
        (self.console / "index.html").write_text("uncommitted and broken")
        output = self.build(self.baseline)
        self.assertIn(f'/_ui/{self.baseline}/assets/app.js', (output / "index.html").read_text())
        self.assertNotIn("uncommitted", (output / "index.html").read_text())
        release.verify_release(output)

    def test_missing_assets_and_invalid_javascript_never_publish(self):
        (self.console / "assets/app.js").unlink()
        revision = self.commit()
        with self.assertRaisesRegex(ValueError, "Missing"):
            self.build(revision)
        self.assertFalse((self.deploy / "releases" / revision).exists())
        (self.console / "assets/app.js").write_text("function (")
        revision = self.commit()
        with self.assertRaises(subprocess.CalledProcessError):
            self.build(revision)
        self.assertFalse((self.deploy / "releases" / revision).exists())

    def test_archive_rejects_links_and_traversal(self):
        for name, kind in [("../escape", tarfile.REGTYPE), ("asset", tarfile.SYMTYPE)]:
            with self.subTest(name=name):
                data = io.BytesIO()
                with tarfile.open(fileobj=data, mode="w") as archive:
                    entry = tarfile.TarInfo(name)
                    entry.type = kind
                    entry.linkname = "/etc/passwd" if kind == tarfile.SYMTYPE else ""
                    archive.addfile(entry)
                with self.assertRaisesRegex(ValueError, "Unsafe"):
                    release.unpack(data.getvalue(), self.root / "unpack")

    def test_loaded_stylesheet_dependencies_are_validated(self):
        css = self.console / "assets/legacy.css"
        css.write_text('@font-face { src: url(missing.woff2); }')
        self.build(self.commit())  # Unused legacy stylesheet is not loaded.
        index = self.console / "index.html"
        index.write_text(index.read_text() + '<link rel="stylesheet" href="/assets/legacy.css">')
        with self.assertRaisesRegex(ValueError, "Missing"):
            self.build(self.commit())

    def test_failed_health_probe_restores_previous_release(self):
        first = self.build(self.baseline)
        release.activate(self.deploy, self.baseline, lambda: None)
        (self.console / "assets/app.js").write_text("const next = true;")
        revision = self.commit()
        self.build(revision)
        def fail():
            raise ValueError("failed probe")
        with self.assertRaisesRegex(ValueError, "failed probe"):
            release.activate(self.deploy, revision, fail)
        self.assertEqual((self.deploy / "current").resolve(), first)
        self.assertTrue(first.is_dir())

    def test_backend_changes_block_but_ui_changes_are_compatible(self):
        (self.console / "assets/app.js").write_text("const next = true;")
        revision = self.commit()
        release.check_backend(self.repo, self.baseline, revision)
        (self.repo / "go.mod").write_text("module example.invalid/changed\n")
        revision = self.commit()
        with self.assertRaisesRegex(ValueError, "separately deployed backend"):
            release.check_backend(self.repo, self.baseline, revision)

    def test_corrupted_release_cannot_replace_current(self):
        output = self.build(self.baseline)
        (output / "index.html").write_text("corrupt")
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            release.activate(self.deploy, self.baseline, lambda: None)
        self.assertFalse((self.deploy / "current").exists())

    def test_fetch_requires_promotion_then_applies_without_touching_checkout(self):
        self.git("branch", "console-live")
        config = {"remote": str(self.repo), "repository": str(self.root / "cache.git"),
                  "root": str(self.deploy), "backend_revision": self.baseline}
        with patch.object(release, "probe") as probe:
            release.fetch(config)
            self.assertEqual(probe.call_count, 1)
            (self.console / "assets/app.js").write_text("const next = true;")
            revision = self.commit()
            release.fetch(config)
            self.assertEqual(json.loads((self.deploy / "current/ui-version.json").read_text())["revision"], self.baseline)
            self.git("branch", "-f", "console-live", revision)
            (self.console / "assets/app.js").write_text("dirty work stays here")
            release.fetch(config)
            self.assertEqual(json.loads((self.deploy / "current/ui-version.json").read_text())["revision"], revision)
            self.assertEqual((self.console / "assets/app.js").read_text(), "dirty work stays here")
            self.assertTrue((self.deploy / "releases" / self.baseline).is_dir())


if __name__ == "__main__":
    unittest.main()

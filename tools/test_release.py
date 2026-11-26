#!/usr/bin/env python3
"""Unit tests for tools/release.py release preparation and tag publishing."""

from __future__ import annotations

import argparse
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

TOOLS_DIR = Path(__file__).resolve().parent
if str(TOOLS_DIR) not in sys.path:
    sys.path.insert(0, str(TOOLS_DIR))

import release


class ReleaseCliTest(unittest.TestCase):
    """ReleaseCliTest verifies version validation, bootstrapping, and local bare-remote tagging."""

    def test_parse_version_tag_accepts_canonical_and_rejects_suffixes(self) -> None:
        parsed = release.parse_version_tag("1.0.0")
        self.assertEqual(parsed.name, "v1.0.0")
        self.assertEqual(parsed.key, (1, 0, 0))

        self.assertEqual(release.parse_version_tag("v0.100.2").name, "v0.100.2")

        for bad in ("v1.0.0-custom.1", "v01.0.0", "1.0", "latest", "v1.0.0+meta"):
            with self.assertRaises(argparse.ArgumentTypeError):
                release.parse_version_tag(bad)

    def test_bootstrap_and_increment_with_bare_origin(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            bare = root / "origin.git"
            work = root / "work"
            subprocess.run(["git", "init", "--bare", str(bare)], check=True, stdout=subprocess.DEVNULL)
            subprocess.run(["git", "init", str(work)], check=True, stdout=subprocess.DEVNULL)
            subprocess.run(["git", "-C", str(work), "config", "user.name", "Tester"], check=True)
            subprocess.run(["git", "-C", str(work), "config", "user.email", "tester@example.test"], check=True)
            subprocess.run(["git", "-C", str(work), "remote", "add", "origin", str(bare)], check=True)

            (work / "README.md").write_text("hello\n", encoding="utf-8")
            subprocess.run(["git", "-C", str(work), "add", "README.md"], check=True)
            subprocess.run(["git", "-C", str(work), "commit", "-m", "feat: initial"], check=True, stdout=subprocess.DEVNULL)

            ctx = release.build_release_context(cwd=work)
            self.assertIsNone(ctx.previous)
            self.assertEqual(ctx.next_tag, "v1.0.0")

            notes_text = release.draft_release_notes(ctx, cwd=work)
            self.assertIn("## Changes", notes_text)
            self.assertIn("- feat: initial", notes_text)

            release.create_and_push_tag(ctx, notes_text.strip(), cwd=work)
            tags = subprocess.check_output(
                ["git", "-C", str(bare), "tag", "-l"],
                text=True,
            ).splitlines()
            self.assertEqual(tags, ["v1.0.0"])

            # Next commit increments patch automatically to v1.0.1.
            (work / "README.md").write_text("hello v2\n", encoding="utf-8")
            subprocess.run(["git", "-C", str(work), "add", "README.md"], check=True)
            subprocess.run(["git", "-C", str(work), "commit", "-m", "fix: next"], check=True, stdout=subprocess.DEVNULL)
            next_ctx = release.build_release_context(cwd=work)
            self.assertIsNotNone(next_ctx.previous)
            assert next_ctx.previous is not None
            self.assertEqual(next_ctx.previous.name, "v1.0.0")
            self.assertEqual(next_ctx.next_tag, "v1.0.1")

            with self.assertRaises(SystemExit):
                release.build_release_context(release.parse_version_tag("v1.0.0"), cwd=work)


if __name__ == "__main__":
    unittest.main()

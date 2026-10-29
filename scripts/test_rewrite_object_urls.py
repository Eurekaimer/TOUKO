#!/usr/bin/env python3
"""Deterministic safety tests for rewrite-object-urls.py."""

import contextlib
import importlib.util
import io
import json
import os
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
SPEC = importlib.util.spec_from_file_location(
    "rewrite_object_urls", os.path.join(HERE, "rewrite-object-urls.py")
)
REWRITE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(REWRITE)

OLD = "objects/default/old.webp"
NEW = "objects/default/new.webp"


def mapping_item(old_path=OLD, new_path=NEW, status="migrated"):
    return {
        "old_path": old_path,
        "new_path": new_path,
        "old_github_url": "https://raw.githubusercontent.com/Eurekaimer/TOUKO-WEBP-01/refs/heads/main/"
        + old_path,
        "old_r2_url": "https://img.eurekaimer.icu/" + old_path,
        "status": status,
    }


class RewriteObjectUrlsTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.mapping = os.path.join(self.directory.name, "url-mapping.json")
        self.note = os.path.join(self.directory.name, "note.md")

    def tearDown(self):
        self.directory.cleanup()

    def run_script(self, *arguments):
        stdout, stderr = io.StringIO(), io.StringIO()
        with mock.patch.object(sys, "argv", ["rewrite-object-urls.py", *arguments]):
            with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                return REWRITE.main()

    def write(self, entries, text):
        with open(self.mapping, "w", encoding="utf-8") as handle:
            json.dump(entries, handle)
        with open(self.note, "w", encoding="utf-8") as handle:
            handle.write(text)

    def test_repo_scope_and_query_suffix(self):
        self.write(
            [mapping_item()],
            "![ok](https://cdn.jsdelivr.net/gh/Eurekaimer/TOUKO-WEBP-01@main/"
            "objects/default/old.webp?raw=1#frag)\n"
            "![no](https://cdn.jsdelivr.net/gh/SomeoneElse/TOUKO-WEBP-01@main/"
            "objects/default/old.webp)\n",
        )
        self.assertEqual(self.run_script("--mapping", self.mapping, "--root", self.note, "--apply"), 0)
        with open(self.note, encoding="utf-8") as handle:
            text = handle.read()
        self.assertIn("Eurekaimer/TOUKO-WEBP-01@main/objects/default/new.webp?raw=1#frag", text)
        self.assertIn("SomeoneElse/TOUKO-WEBP-01@main/objects/default/old.webp", text)

    def test_apply_rejects_non_migrated_entries(self):
        self.write([mapping_item(status="would_migrate")], "x https://img.eurekaimer.icu/%s\n" % OLD)
        self.assertEqual(self.run_script("--mapping", self.mapping, "--root", self.note, "--apply"), 2)
        with open(self.note, encoding="utf-8") as handle:
            self.assertIn(OLD, handle.read())

    def test_ambiguous_mapping_is_refused(self):
        self.write(
            [mapping_item(new_path="objects/default/a.webp"), mapping_item(new_path="objects/default/b.webp")],
            "x https://img.eurekaimer.icu/%s\n" % OLD,
        )
        self.assertEqual(self.run_script("--mapping", self.mapping, "--root", self.note), 2)
        with open(self.note, encoding="utf-8") as handle:
            self.assertIn(OLD, handle.read())

    def test_verified_shared_object_uses_one_deterministic_target(self):
        first = mapping_item(new_path="objects/default/b.webp")
        second = mapping_item(new_path="objects/default/a.webp")
        for item in (first, second):
            item["sha256"] = "a" * 64
            item["size_bytes"] = 123
        self.write([first, second], "x https://img.eurekaimer.icu/%s\n" % OLD)
        self.assertEqual(self.run_script("--mapping", self.mapping, "--root", self.note, "--apply"), 0)
        with open(self.note, encoding="utf-8") as handle:
            self.assertIn("objects/default/a.webp", handle.read())

    def test_failed_mapping_blocks_all_rewrites(self):
        self.write(
            [mapping_item(), mapping_item(old_path="objects/default/other.webp", status="failed")],
            "x https://img.eurekaimer.icu/%s\n" % OLD,
        )
        self.assertEqual(self.run_script("--mapping", self.mapping, "--root", self.note, "--apply"), 2)
        with open(self.note, encoding="utf-8") as handle:
            self.assertIn(OLD, handle.read())

    def test_conflicting_shared_object_proofs_are_refused(self):
        first = mapping_item(new_path="objects/default/a.webp")
        second = mapping_item(new_path="objects/default/b.webp")
        first.update(sha256="a" * 64, size_bytes=123)
        second.update(sha256="b" * 64, size_bytes=123)
        self.write([first, second], "x https://img.eurekaimer.icu/%s\n" % OLD)
        self.assertEqual(self.run_script("--mapping", self.mapping, "--root", self.note, "--apply"), 2)
        with open(self.note, encoding="utf-8") as handle:
            self.assertIn(OLD, handle.read())

    def test_custom_path_with_spaces_is_rewritten(self):
        item = mapping_item(old_path=" photos/old name.png ", new_path="objects/default/new.png")
        item["old_r2_url"] = "https://img.eurekaimer.icu/%20photos/old%20name.png%20"
        self.write([item], "![photo](https://img.eurekaimer.icu/%20photos/old%20name.png%20)\n")
        self.assertEqual(self.run_script("--mapping", self.mapping, "--root", self.note, "--apply"), 0)
        with open(self.note, encoding="utf-8") as handle:
            self.assertEqual(handle.read(), "![photo](https://img.eurekaimer.icu/objects/default/new.png)\n")

    def test_missing_root_does_not_change_other_files(self):
        self.write([mapping_item()], "x https://img.eurekaimer.icu/%s\n" % OLD)
        missing = os.path.join(self.directory.name, "missing")
        result = self.run_script("--mapping", self.mapping, "--root", self.note, "--root", missing, "--apply")
        self.assertNotEqual(result, 0)
        with open(self.note, encoding="utf-8") as handle:
            self.assertIn(OLD, handle.read())

    def test_concurrent_file_edit_is_preserved(self):
        self.write([mapping_item()], "x https://img.eurekaimer.icu/%s\n" % OLD)
        original = REWRITE.write_atomic

        def edit_before_write(path, text, info):
            with open(path, "w", encoding="utf-8") as handle:
                handle.write("user revision\n")
            return original(path, text, info)

        with mock.patch.object(REWRITE, "write_atomic", side_effect=edit_before_write):
            self.assertNotEqual(self.run_script("--mapping", self.mapping, "--root", self.note, "--apply"), 0)
        with open(self.note, encoding="utf-8") as handle:
            self.assertEqual(handle.read(), "user revision\n")


if __name__ == "__main__":
    unittest.main()

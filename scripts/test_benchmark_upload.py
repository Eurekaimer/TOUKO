import hashlib
import http.server
import importlib.util
import json
from pathlib import Path
import tempfile
import threading
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location(
    "benchmark_upload", Path(__file__).with_name("benchmark-upload.py")
)
benchmark = importlib.util.module_from_spec(spec)
spec.loader.exec_module(benchmark)


def digest(data):
    return hashlib.sha256(data).hexdigest()


class BenchmarkResultsTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.image = Path(self.directory.name) / "fixture.png"
        self.image.write_bytes(b"x" * 1000)

    def record(self, total_ms, r2="off", ok=True):
        raw = {
            "add_ns": 20_000_000,
            "commit_ns": (total_ms - 20) * 1_000_000,
            "total_ns": total_ms * 1_000_000,
            "add_ok": True,
            "commit_ok": ok,
            "ok": ok,
            "phases_add": {
                "add.ingest": 10_000_000,
                "add.metadata": 2_000_000,
                "add.install": 6_000_000,
                "add.verify": 2_000_000,
                "add.save": 1_000_000,
                "ingest.hash": 1_000_000,
            },
            "phases_commit": {"commit.prepare": 5_000_000, "commit.r2": 20_000_000},
        }
        if r2 == "on":
            raw.update(
                r2_enabled=True,
                r2_status="synced",
                r2_failed=0,
                r2_pending=0,
            )
        return benchmark.normalise_record(raw, 0, str(self.image), r2, False)

    def cell(self, label, r2, record):
        summary = benchmark.summarise([record], 1, None if record["ok"] else 0)
        return {
            "metadata": {"label": label, "r2": r2, "rep": 1, "branch": "test", "namespace": "test"},
            "summary": summary,
            "records": [record],
        }

    def test_incomplete_group_cannot_produce_comparison(self):
        cells = [
            self.cell("old", "off", self.record(100)),
            self.cell("old", "on", self.record(130, "on")),
            self.cell("new", "off", self.record(90)),
            self.cell("new", "on", self.record(80, "on", ok=False)),
        ]
        groups = benchmark.aggregate_cells(cells, 1)
        comparisons = benchmark.compare_groups(groups)
        self.assertEqual(groups["old-r2off"]["total_ms"], 100)
        self.assertEqual(groups["old-r2off"]["throughput_mb_s"], 0.01)
        self.assertEqual(comparisons["old_r2_overhead_ms_per_image"], 30)
        self.assertEqual(comparisons["uuid_r2off_improvement_ms_per_image"], 10)
        self.assertEqual(comparisons["uuid_r2off_improvement_pct"], 10)
        self.assertIsNone(groups["new-r2on"]["total_ms"])
        self.assertIsNone(comparisons["new_r2_overhead_ms_per_image"])
        self.assertIsNone(comparisons["uuid_r2on_improvement_pct"])

    def test_disabled_r2_and_nested_phases_are_not_counted_twice(self):
        record = self.record(100)
        self.assertIsNone(record["r2_ms"])
        self.assertEqual(record["hash_ms"], 1)
        self.assertEqual(record["install_rest_ms"], 3)
        exclusive_ns = sum(record[name + "_ns"] or 0 for name in benchmark.EXCLUSIVE_PHASES)
        self.assertEqual(exclusive_ns + record["other_ns"], record["total_ns"])
        self.assertEqual(record["phase_unavailable"]["r2"], "R2 disabled for this cell")

    def test_hash_aggregates_sum_only_image_byte_writes(self):
        phases = benchmark.derive_phases(
            {
                "phases_add": {
                    "add.ingest": 9_000_000,
                    "ingest.hash": 1_000_000,
                    "verify.hash": 2_000_000,
                },
                "phases_commit": {
                    "commit.verify": 3_000_000,
                    "verify.hash": 3_000_000,
                    "upload.hash": 4_000_000,
                    "ingest.hash": 5_000_000,
                },
            }
        )
        self.assertEqual(phases["hash"], 1_000_000)
        self.assertEqual(phases["verify_hash"], 5_000_000)
        self.assertEqual(phases["upload_hash"], 4_000_000)
        self.assertEqual(phases["total_hash"], 15_000_000)
        self.assertIsNone(phases["r2_hash"])

    def test_r2_on_requires_synced_result_and_off_is_not_applicable(self):
        raw = {
            "add_ns": 1,
            "commit_ns": 1,
            "total_ns": 2,
            "add_ok": True,
            "commit_ok": True,
            "ok": True,
            "r2_enabled": True,
            "r2_status": "failed",
            "r2_failed": 1,
            "r2_pending": 0,
        }
        record = benchmark.normalise_record(raw, 0, str(self.image), "on", False)
        self.assertFalse(record["ok"])
        self.assertIn("R2 publication not synced", record["error"])

        raw.update(r2_status="synced", r2_failed=0)
        self.assertTrue(benchmark.normalise_record(raw, 0, str(self.image), "on", False)["ok"])

        raw.pop("r2_enabled")
        self.assertFalse(benchmark.normalise_record(raw, 0, str(self.image), "on", False)["ok"])

        off = benchmark.normalise_record(
            {**raw, "r2_items": [{"object_path": "objects/x/y"}]},
            0,
            str(self.image),
            "off",
            False,
        )
        self.assertFalse(off["r2_applicable"])
        self.assertEqual(off["r2_items"], [])
        self.assertEqual(off["r2_status"], "")

    def test_r2_config_is_a_dict_only_for_on_cells(self):
        r2 = {"endpoint": "https://example.test", "bucket": "private"}
        self.assertIs(benchmark.state_r2_config("on", r2), r2)
        self.assertIsNone(benchmark.state_r2_config("off", r2))


class PublishedBaseTest(unittest.TestCase):
    def test_exact_https_origin_and_base_path(self):
        base = benchmark.public_base_origin("https://cdn.example.test")
        self.assertTrue(
            benchmark.url_under_public_base("https://cdn.example.test/objects/default/a.webp", base)
        )
        for url in (
            "http://cdn.example.test/objects/default/a.webp",
            "https://evil.example/objects/default/a.webp",
            "https://cdn.example.test.evil.example/objects/default/a.webp",
            "https://cdn.example.test@evil.example/objects/default/a.webp",
            "https://cdn.example.test:8443/objects/default/a.webp",
            "https://cdn.example.test/objects/default/a.webp?token=secret",
            "https://cdn.example.test/objects//a.webp",
            "https://cdn.example.test/objects/../a.webp",
            "https://cdn.example.test/",
        ):
            self.assertFalse(benchmark.url_under_public_base(url, base), url)

        prefixed = benchmark.public_base_origin("https://cdn.example.test/gallery/")
        self.assertTrue(
            benchmark.url_under_public_base(
                "https://cdn.example.test/gallery/objects/default/a.webp", prefixed
            )
        )
        self.assertFalse(
            benchmark.url_under_public_base(
                "https://cdn.example.test/galleryevil/objects/default/a.webp", prefixed
            )
        )

    def test_public_base_requires_https_without_credentials(self):
        for value in (
            "",
            "http://cdn.example.test",
            "https://user:pass@cdn.example.test",
            "https://cdn.example.test/gallery?x=1",
            "https://cdn.example.test/gallery#fragment",
        ):
            with self.assertRaises(SystemExit, msg=value):
                benchmark.public_base_origin(value)


class PublishedCorpusTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.base = benchmark.public_base_origin("https://cdn.example.test")
        self.images = []
        for index, data in enumerate((b"alpha", b"alpha", b"beta")):
            path = Path(self.directory.name) / f"{index}.png"
            path.write_bytes(data)
            self.images.append(str(path))

    def entry(self, data, name, status="migrated", url=None):
        return {
            "asset_id": name,
            "sha256": digest(data),
            "size_bytes": len(data),
            "new_path": f"objects/default/{name}.png",
            "new_r2_url": url or f"https://cdn.example.test/objects/default/{name}.png",
            "status": status,
        }

    def write_mapping(self, entries):
        path = Path(self.directory.name) / "url-mapping.json"
        path.write_text(json.dumps(entries), encoding="utf-8")
        return str(path)

    def test_every_unique_content_is_fetched_once(self):
        entries = [self.entry(b"alpha", "a"), self.entry(b"alpha", "b"), self.entry(b"beta", "c")]
        calls = []
        with mock.patch.object(
            benchmark,
            "fetch_public_content",
            side_effect=lambda url, sha, size, timeout: calls.append(url) or size,
        ):
            report = benchmark.verify_published_corpus(self.images, entries, self.base, 1)
        self.assertEqual(report["input_files"], 3)
        self.assertEqual(report["unique_contents"], 2)
        self.assertEqual(report["input_bytes"], 14)
        self.assertEqual(report["unique_content_bytes"], 9)
        self.assertEqual(len(calls), 2)
        self.assertEqual(len(set(calls)), 2)

    def test_missing_mapping_entry_fails_before_any_fetch(self):
        entries = [self.entry(b"alpha", "a"), self.entry(b"alpha", "b")]
        with mock.patch.object(benchmark, "fetch_public_content") as fetch:
            with self.assertRaises(SystemExit):
                benchmark.verify_published_corpus(self.images, entries, self.base, 1)
        fetch.assert_not_called()

    def test_failed_or_foreign_mapping_rows_are_not_proof(self):
        failed = [self.entry(b"alpha", "a", status="failed"), self.entry(b"alpha", "b")]
        with self.assertRaises(SystemExit):
            benchmark.verify_published_corpus(self.images, failed, self.base, 1)
        foreign = [
            self.entry(b"alpha", "a", url="https://evil.example/objects/default/a.png"),
            self.entry(b"alpha", "b"),
            self.entry(b"beta", "c"),
        ]
        with self.assertRaises(SystemExit):
            benchmark.verify_published_corpus(self.images, foreign, self.base, 1)

    def test_mapping_loader_skips_non_migrated_rows(self):
        path = self.write_mapping(
            [self.entry(b"alpha", "a"), self.entry(b"beta", "c", status="failed")]
        )
        entries = benchmark.load_published_mapping(path)
        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0]["sha256"], digest(b"alpha"))


class PublicFetchTest(unittest.TestCase):
    def setUp(self):
        routes = {
            "/good": (200, b"good", {}),
            "/bad": (200, b"evil", {}),
            "/short": (200, b"go", {}),
            "/redirect": (302, b"", {"Location": "/good"}),
            "/missing": (404, b"missing", {}),
        }
        self.seen_headers = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                routes_headers = getattr(self, "routes_headers", None)
                if routes_headers is not None:
                    routes_headers.append(dict(self.headers))
                status, body, headers = routes.get(self.path, (500, b"error", {}))
                self.send_response(status)
                for key, value in headers.items():
                    self.send_header(key, value)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        Handler.routes_headers = self.seen_headers
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.root = f"http://127.0.0.1:{self.server.server_port}"

    def test_good_body_passes(self):
        size = benchmark.fetch_public_content(
            self.root + "/good", digest(b"good"), 4, 2, attempts=1
        )
        self.assertEqual(size, 4)
        self.assertTrue(self.seen_headers)
        self.assertNotIn("Authorization", self.seen_headers[0])
        self.assertNotIn("Cookie", self.seen_headers[0])

    def test_redirect_mismatch_short_and_missing_fail(self):
        for path, sha, size in (
            ("/redirect", digest(b"good"), 4),
            ("/bad", digest(b"good"), 4),
            ("/short", digest(b"go"), 4),
            ("/missing", digest(b"missing"), 7),
        ):
            with self.assertRaises(SystemExit, msg=path):
                benchmark.fetch_public_content(self.root + path, sha, size, 2, attempts=1)


class ResolveR2SettingsTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.workspace = {
            "cloudflare": {
                "enabled": True,
                "endpoint": "https://account.r2.cloudflarestorage.com",
                "bucket": "production-bucket",
                "public_base_url": "https://img.example.test",
            }
        }
        self.mapping = Path(self.directory.name) / "url-mapping.json"
        self.mapping.write_text(
            json.dumps(
                [
                    {
                        "asset_id": "a",
                        "sha256": "0" * 64,
                        "size_bytes": 1,
                        "new_path": "objects/default/a.png",
                        "new_r2_url": "https://img.example.test/objects/default/a.png",
                        "status": "migrated",
                    }
                ]
            ),
            encoding="utf-8",
        )

    def call(self, **overrides):
        values = {
            "bucket": "",
            "endpoint": "",
            "public_base": None,
            "private_confirmed": False,
            "prefix": "",
            "published_mapping": str(self.mapping),
            "credentials": "/tmp/credentials.json",
            "images": [],
            "timeout": 1,
            "stamp": "20260101T000000Z",
        }
        values.update(overrides)
        return benchmark.resolve_r2_settings(self.workspace, **values)

    def test_production_bucket_is_refused_without_published_mode(self):
        with self.assertRaises(SystemExit):
            benchmark.resolve_r2_settings(
                self.workspace,
                bucket="production-bucket",
                endpoint="https://account.r2.cloudflarestorage.com",
                public_base=None,
                private_confirmed=True,
                prefix="",
                published_mapping="",
                credentials="/tmp/credentials.json",
                images=[],
                timeout=1,
                stamp="20260101T000000Z",
            )

    def test_published_mode_uses_workspace_values_and_random_prefix(self):
        report = {"entries": 1, "input_files": 1, "unique_contents": 1}
        with mock.patch.object(benchmark, "verify_published_corpus", return_value=report):
            first = self.call(bucket="production-bucket")
            second = self.call()
        self.assertEqual(first["mode"], "published")
        self.assertFalse(first["private_bucket_confirmed"])
        self.assertEqual(first["bucket"], "production-bucket")
        self.assertEqual(first["endpoint"], self.workspace["cloudflare"]["endpoint"])
        self.assertEqual(first["public_base_url"], "https://img.example.test")
        self.assertRegex(first["prefix"], r"^b[0-9a-f]{32}$")
        self.assertNotEqual(first["prefix"], second["prefix"])
        self.assertIs(first["published"], report)

    def test_published_mode_rejects_unsafe_overrides(self):
        report = {"entries": 1}
        cases = (
            {"private_confirmed": True},
            {"prefix": "deterministic"},
            {"public_base": "https://img.example.test"},
            {"bucket": "another-bucket"},
            {"endpoint": "https://other.r2.cloudflarestorage.com"},
        )
        with mock.patch.object(benchmark, "verify_published_corpus", return_value=report):
            for overrides in cases:
                with self.assertRaises(SystemExit, msg=repr(overrides)):
                    self.call(**overrides)

    def test_private_mode_keeps_dedicated_bucket_requirements(self):
        with self.assertRaises(SystemExit):
            self.call(
                bucket="test-bucket",
                endpoint="https://test.r2.cloudflarestorage.com",
                private_confirmed=False,
                published_mapping="",
            )
        settings = self.call(
            bucket="test-bucket",
            endpoint="https://test.r2.cloudflarestorage.com",
            private_confirmed=True,
            published_mapping="",
            public_base="https://private.invalid",
        )
        self.assertEqual(settings["mode"], "private")
        self.assertTrue(settings["private_bucket_confirmed"])
        self.assertEqual(settings["prefix"], "benchmark-20260101t000000z")


if __name__ == "__main__":
    unittest.main()

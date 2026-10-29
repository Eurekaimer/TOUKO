import importlib.util
from pathlib import Path
import tempfile
import unittest


spec = importlib.util.spec_from_file_location(
    "benchmark_upload", Path(__file__).with_name("benchmark-upload.py")
)
benchmark = importlib.util.module_from_spec(spec)
spec.loader.exec_module(benchmark)


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


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3
# Copyright The Prometheus Authors
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Deterministic runner/continuation checks; no Go subprocesses or Linux host required."""

import copy
import importlib.util
import json
from pathlib import Path
import shutil
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

sys.dont_write_bytecode = True
module = importlib.util.spec_from_file_location("scale", Path(__file__).with_name("benchmark-metadata-pipeline-scale.py"))
scale = importlib.util.module_from_spec(module)
module.loader.exec_module(scale)


class ScaleStudyTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="metadata-scale-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.host = dict(kernel="test-kernel", boot="test-boot")
        patch = mock.patch.object(scale, "host_identity", return_value=self.host)
        patch.start()
        self.addCleanup(patch.stop)
        self.source = self.root / "source"
        (self.source / "scripts").mkdir(parents=True)
        (self.source / "scripts/benchmark-metadata-pipeline-scale.py").write_text("Legacy frozen runner.\n")

    def freeze(self, root, schema):
        root.mkdir()
        (root / "remote.test").write_bytes(b"Synthetic binary, never executed.")
        (root / "remote.test").chmod(0o755)
        with tarfile.open(root / "source.tar.gz", "w:gz") as archive:
            archive.add(self.source, arcname=".")
        shutil.copyfile(scale.__file__, root / "runner.py")
        planned = scale.manifest(schema)
        planned.update(source=str(self.source), host=self.host,
                       hashes={p: scale.sha(root / p) for p in ("remote.test", "source.tar.gz")},
                       source_hashes=scale.inventory(self.source))
        planned["runner_sha256"] = (planned["source_hashes"]["scripts/benchmark-metadata-pipeline-scale.py"]
                                    if schema == 1 else scale.sha(root / "runner.py"))
        self.save_manifest(root, planned)
        return planned

    def save_manifest(self, root, planned):
        scale.write(root / "expected-runs.json", planned)
        (root / "expected-runs.sha256").write_text(scale.sha(root / "expected-runs.json")+"  expected-runs.json\n")

    def trace(self, folder, member, planned, noisy=False, swap=False):
        folder.mkdir(parents=True)
        c = scale.config(member)
        n = c["Series"] * c["Sweeps"]
        backlog, heap = c["Group"] == "backlog", member["kind"] == "heap"
        result = dict(Config=c | {"Base": 1000}, Diagnostic=heap, Samples=n, ResidentSeries=c["Series"],
                      Transactions=n//c["CommitSize"], ScheduledTransactions=n//c["CommitSize"] if c["SamplesPerSecond"] else 0,
                      SweepsLateByInterval=0, TransactionsLateByInterval=0,
                      CPU=dict(Available=True, User=n, System=n), ReceiverCPU=dict(Available=True), LifecycleCPU=dict(Available=True),
                      Completion=10, Lifecycle=11, Ingestion=5, Drain=3, Shutdown=1,
                      OutstandingAtWriterEnd=n if backlog else 0, PeakSampledQueue=0,
                      BacklogWait=1 if backlog else 0, ReleaseToDrain=2 if backlog else 0,
                      SeededHeap=int(heap), BacklogHeap=int(heap and backlog), DrainedHeap=int(heap),
                      AllocatedBytes=n, Allocations=n, WALBytes=n, RequestBytes=n, ReceiverServiceNanos=1)
        metrics = f"10 ns/op 1 B/op 1 allocs/op {n} samples/op 1 samples/s 2 cpu-ns/sample 1 alloc-B/sample 1 allocs/sample 1 wal-B/sample 1 wire-B/sample 1 drain-ms/op 1 txn-p99-ns"
        (folder / "stdout.txt").write_text("goos: linux\ngoarch: amd64\n"+scale.name(member)+"-4 1 "+metrics+"\nmetadata-pipeline-result: "+json.dumps(result)+"\nPASS\n")
        (folder / "stderr.txt").write_text("")
        before = dict(cpu={"cpu": [0]*8, "cpu0": [0]*8}, swap=dict(pswpin=0, pswpout=0))
        after = dict(cpu={"cpu": [10, 0, 0, 41, 0, 0, 0, int(noisy)], "cpu0": [10, 0, 0, 41, 0, 0, 0, int(noisy)]},
                     swap=dict(pswpin=int(swap), pswpout=0))
        assessment = scale.host_activity(before, after, [], planned["schema"])
        record = dict(member=member, binary_sha256=planned["hashes"]["remote.test"], exit=0,
                      environment={k: v for k, v in scale.environment(member).items() if k in scale.ENV or k.startswith("PROMETHEUS_METADATA_PIPELINE_")},
                      before=before, after=after, activity=[], started=0, ended=.52,
                      stdout_sha256=scale.sha(folder / "stdout.txt"), stderr_sha256=scale.sha(folder / "stderr.txt"), noise=assessment["noise"])
        if planned["schema"] == 2:
            record.update(assessment)
        scale.write(folder / "record.json", record)
        if member["kind"] == "profile":
            for name in ("cpu.pprof", "alloc.pprof"):
                (folder / name).write_bytes(b"Synthetic profile.")
        return assessment

    def block(self, root, block, planned, attempt, noisy=False, accepted=False):
        folder = root / "results" / block["id"] / f"attempt-{attempt}"
        reasons, warnings = [], []
        for i, member in enumerate(block["members"]):
            assessment = self.trace(folder / str(i), member, planned, noisy)
            reasons += assessment["noise"]
            warnings += assessment["warnings"]
        status = dict(block=block, attempt=attempt, noise=reasons)
        if planned["schema"] == 2:
            status["warnings"] = warnings
        scale.write(folder / "block.json", status)
        if accepted:
            scale.write(folder.parent / "accepted.json", dict(attempt=attempt))
        return reasons

    def smoke(self, root, planned):
        for i, member in enumerate(scale.smoke_members()):
            self.trace(root / "smoke" / str(i), member, planned)
        (root / "SMOKE_COMPLETE").write_text("Synthetic smoke checks.\n")

    def parent(self, prefix=1):
        root = self.root / "old"
        planned = self.freeze(root, 1)
        self.smoke(root, planned)
        for index, block in enumerate(planned["blocks"][:prefix]):
            if index == 0:
                self.block(root, block, planned, 0, noisy=True)
            self.block(root, block, planned, int(index == 0), accepted=True)
        terminal = planned["blocks"][prefix]
        self.block(root, terminal, planned, 0, noisy=True)
        reasons = self.block(root, terminal, planned, 1, noisy=True)
        scale.write(root / "INCONCLUSIVE.json", dict(block=terminal, noise=reasons))
        return root, planned

    def test_steal_is_informational_only_in_new_policy(self):
        before = dict(cpu={"cpu": [0]*8, "cpu0": [0]*8}, swap=dict(pswpin=0, pswpout=0))
        for cpu in ("cpu", "cpu0"):
            with self.subTest(cpu=cpu):
                after = copy.deepcopy(before)
                after["cpu"][cpu] = [4, 0, 0, 47, 0, 0, 0, 1]
                self.assertTrue(scale.host_activity(before, after, [], 1)["noise"])
                assessment = scale.host_activity(before, after, [], 2)
                self.assertFalse(assessment["noise"])
                self.assertAlmostEqual(assessment["steal_fractions"][cpu], 1/52)
                self.assertTrue(assessment["warnings"])
                after["swap"]["pswpin"] = 1
                self.assertEqual(scale.host_activity(before, after, [], 2)["noise"], ["swap activity"])
        self.assertEqual(scale.host_activity(before, before, [dict(offenders=["compile"])], 2)["noise"],
                         ["overlapping compilation, tests, or package installation"])

    def test_full_continuation_and_relocated_analysis(self):
        old, previous = self.parent(prefix=59)
        original = scale.inventory(old)
        root = self.root / "new"
        scale.prepare_continuation(old, root)
        planned = scale.load(root)
        self.assertEqual(len(planned["parent"]["inherited"]), 59)
        self.assertEqual(len(planned["parent"]["remaining"]), 47)
        self.assertEqual(planned["parent"]["inherited"][0]["attempt"], 1)
        self.assertEqual(planned["hashes"], previous["hashes"])
        self.assertEqual((root / "remote.test").stat().st_mode, (old / "remote.test").stat().st_mode)
        self.smoke(root, planned)
        with self.assertRaises(FileNotFoundError):
            scale.complete(root, planned)
        self.assertFalse((root / "MEASUREMENT_COMPLETE").exists())
        with mock.patch.object(scale, "run_one", side_effect=lambda _, folder, member, p: self.trace(folder, member, p, noisy=True)) as execute:
            with mock.patch("builtins.print"):
                scale.run(root)
        self.assertEqual(execute.call_count, 254, "steal must not retry any trace")
        self.assertTrue((root / "MEASUREMENT_COMPLETE").is_file())
        self.assertFalse((root / "INCONCLUSIVE.json").exists())
        self.assertEqual(scale.inventory(old), original)
        root.rename(self.root / "relocated")
        root = self.root / "relocated"
        self.source.rename(self.root / "offline-source")
        with mock.patch.object(scale.subprocess, "run"):
            scale.analyze(root)
        coverage = json.loads((root / "analysis/coverage.json").read_text())
        self.assertEqual(coverage["segments"], dict(original=414, continuation=254))
        self.assertEqual(coverage["counts"], dict(scored=504, heap=108, capacity=48, profile=8))
        self.assertEqual(len(coverage["warnings"]), 254)
        summary = json.loads((root / "analysis/summary.json").read_text())
        self.assertEqual({s["n"] for s in summary if s["segment"] == "all"}, {6})
        self.assertEqual({s["n"] for s in summary if s["segment"] == "continuation"}, {2, 3})
        block = planned["blocks"][59]
        record = root / "results" / block["id"] / "attempt-0/0/record.json"
        data = json.loads(record.read_text())
        data["exit"] = 1
        scale.write(record, data)
        with self.assertRaises(AssertionError):
            scale.collect(root, planned)

    def test_prepare_rejects_conflicts_and_corruption(self):
        old, previous = self.parent()
        for destination in (old, old / "child", self.root, self.source / "child"):
            with self.subTest(destination=destination), self.assertRaises(AssertionError):
                scale.prepare_continuation(old, destination)
        root = self.root / "new"
        with mock.patch.object(scale, "host_identity", return_value=dict(kernel="changed", boot="changed")):
            with self.assertRaises(AssertionError):
                scale.prepare_continuation(old, root)
        self.assertFalse(root.exists())
        (old / "remote.test").write_bytes(b"Changed binary.")
        with self.assertRaises(AssertionError):
            scale.prepare_continuation(old, root)
        self.assertFalse(root.exists())

    def test_parent_mapping_and_evidence_are_frozen(self):
        old, _ = self.parent()
        root = self.root / "new"
        scale.prepare_continuation(old, root)
        planned = scale.load(root)
        altered = copy.deepcopy(planned)
        altered["parent"]["inherited"][0]["attempt"] = 0
        with self.assertRaises(AssertionError):
            scale.parent_results(root, altered)
        altered = copy.deepcopy(planned)
        altered["parent"]["remaining"].pop()
        with self.assertRaises(AssertionError):
            scale.parent_results(root, altered)
        (root / "parent/unexpected.txt").write_text("Extra evidence.\n")
        with self.assertRaises(AssertionError):
            scale.parent_results(root, planned)

    def test_legacy_parent_rejects_missing_and_extra_results(self):
        old, planned = self.parent()
        first = old / "results" / planned["blocks"][0]["id"]
        marker = first / "accepted.json"
        marker.rename(first / "missing.json")
        with self.assertRaises(FileNotFoundError):
            scale.legacy_parent(old)
        (first / "missing.json").rename(marker)
        extra = old / "results/unexpected"
        extra.mkdir()
        scale.write(extra / "accepted.json", dict(attempt=0))
        with self.assertRaises(AssertionError):
            scale.legacy_parent(old)

    def test_hard_interference_and_benchmark_failures_still_stop(self):
        for failure in ("swap", "benchmark"):
            with self.subTest(failure=failure):
                root = self.root / failure
                planned = self.freeze(root, 2)
                self.smoke(root, planned)
                def execute(_, folder, member, p):
                    if failure == "benchmark":
                        raise AssertionError("benchmark failure")
                    return self.trace(folder, member, p, noisy=True, swap=True)
                with mock.patch.object(scale, "run_one", side_effect=execute), mock.patch("builtins.print"):
                    with self.assertRaises((AssertionError, RuntimeError)):
                        scale.run(root)
                self.assertFalse((root / "MEASUREMENT_COMPLETE").exists())
                self.assertEqual((root / "INCONCLUSIVE.json").exists(), failure == "swap")

    def test_unknown_schema_and_runner_changes_are_rejected(self):
        root = self.root / "new"
        planned = self.freeze(root, 2)
        altered = copy.deepcopy(planned)
        altered["schema"] = 99
        self.save_manifest(root, altered)
        with self.assertRaises(AssertionError):
            scale.load(root)
        self.save_manifest(root, planned)
        (root / "runner.py").write_text("Changed runner.\n")
        with self.assertRaises(AssertionError):
            scale.load(root)


if __name__ == "__main__":
    unittest.main()

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

from collections import Counter
import copy
import importlib.util
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("study", Path(__file__).with_name("benchmark-metadata-pipeline-phases.py"))
study = importlib.util.module_from_spec(spec)
spec.loader.exec_module(study)


class PipelinePhasesTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="pipeline-phases-test-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def test_frozen_coverage_and_order(self):
        p = study.manifest()
        self.assertEqual(400, sum(p["counts"].values()))
        self.assertEqual(176, len(p["blocks"]))
        self.assertEqual(176, len({b["id"] for b in p["blocks"]}))
        self.assertEqual(44, len(study.smoke_members(p)))
        cells, order, seen_profile = {}, {}, False
        for b in p["blocks"]:
            if not b["cohort"]:
                seen_profile = True
            else:
                self.assertFalse(seen_profile)
                for m in b["members"]:
                    key = (b["cohort"], m["scenario"], m["kind"], m["variant"])
                    cells.setdefault(key, []).append(m["arm"])
                if b["id"].startswith("bridge/"):
                    key = (b["cohort"], b["id"].split("/")[-1])
                    order.setdefault(key, []).append(b["members"][0]["variant"])
            for m in b["members"]:
                self.assertEqual("native", m["mode"])
                self.assertEqual(4, m["procs"])
                self.assertEqual("1x", m["benchtime"])
                self.assertEqual("stringlabels", m["tags"])
        for arms in cells.values():
            self.assertEqual(Counter(forwarding=6, candidate=6), Counter(arms))
            self.assertEqual(Counter({study.ARMS: 3, study.ARMS[::-1]: 3}), Counter(tuple(arms[i:i+2]) for i in range(0, 12, 2)))
        for variants in order.values():
            self.assertEqual(Counter(archived=3, rebuilt=3), Counter(variants))
        self.assertEqual("record", p["policy"]["steal_action"])

    def test_test_only_overlay(self):
        original = {"go.mod": "module", "tsdb/head.go": "production", study.OVERLAY[0]: "old", study.OVERLAY[1]: "old"}
        patched = original | {p: "new" for p in study.OVERLAY}
        self.assertEqual(3, len(study.verify_overlay(original, patched)))
        for bad in (patched | {"tsdb/head.go": "changed"}, patched | {"new.go": "new"}, patched | {"go.mod": "changed"},
                    {p: v for p, v in patched.items() if p != "tsdb/head.go"}):
            with self.assertRaises(AssertionError):
                study.verify_overlay(original, bad)

    def output(self, m):
        c = study.scale.config(m)
        n = 400000
        cpu = dict(Available=True, User=1, System=1)
        r = dict(Config=c | dict(Base=1000), Diagnostic=False, Samples=n, ResidentSeries=c["Series"], Transactions=400,
                 ScheduledTransactions=0, SweepsLateByInterval=0, TransactionsLateByInterval=0, CPU=cpu, ReceiverCPU=cpu,
                 LifecycleCPU=cpu, Completion=10, Lifecycle=11, Initialization=1, Ingestion=5, Drain=3, Shutdown=1,
                 DBClose=1, OutstandingAtWriterEnd=n, PeakSampledQueue=0, BacklogWait=1, ReleaseToDrain=2,
                 SeededHeap=0, BacklogHeap=0, DrainedHeap=0, AllocatedBytes=n, Allocations=n, WALBytes=n,
                 RequestBytes=n, ReceiverServiceNanos=1)
        units = {u: 1 for u in ("ns/op", "B/op", "allocs/op", "samples/s", "cpu-ns/sample", "alloc-B/sample", "allocs/sample",
                  "wal-B/sample", "wire-B/sample", "drain-ms/op", "txn-p99-ns")}
        units["samples/op"] = n
        text = "goos: linux\ngoarch: amd64\n"+study.history.name(m)+"-4 1 "+" ".join(f"{v} {k}" for k, v in units.items())
        text += "\nmetadata-pipeline-result: "+json.dumps(r)+"\nPASS\n"
        d = dict(Accounting=m["kind"] == "accounting", BackoffCap=5000000 if m["scenario"] == "capped" else 0,
                 CPUPhase=m["phase"] if m["kind"] == "cpu" else "", Trace=m["kind"] == "trace", Traced=m["kind"] == "trace",
                 Profiled=m["kind"] == "cpu", Phases=[], Progress=None, Events=[])
        memory = dict(AllocatedBytes=10, Allocations=1, GCPause=0, GC=0)
        for i, name in enumerate(study.PHASES):
            start = dict(Started=10*i, Finished=10*i+1, CPU=cpu, Memory=memory, Receiver=dict(CPU=cpu, Memory=memory))
            end = start | dict(Started=10*i+5, Finished=10*i+6)
            d["Phases"].append(dict(Name=name, Start=start, End=end))
        d["Events"] = [dict(Name=name, At=i) for i, name in enumerate(("writer-complete", "release-command-start", "release-command-complete", "drain-complete"))]
        if d["Accounting"]:
            d["Progress"] = [dict(Started=1, Finished=2, Pending=0, Acknowledged=500000, HeldRequests=1, EnqueueRetries=2, RecordsRead=dict(samples=500))]
        return text, d

    def test_output_modes_and_completeness(self):
        for kind in ("plain", "accounting", "cpu", "trace", "alloc", "locks"):
            m = study.member("backlog", kind, phase="drain" if kind == "cpu" else "") | dict(arm="candidate")
            text, d = self.output(m)
            extra = "metadata-pipeline-diagnostics: "+json.dumps(d)+"\n"
            full = text+extra if kind != "plain" else text
            study.parse_output(full, m)
            for bad in (full.replace("metadata-pipeline-result:", "missing:"), full+text, full+extra):
                with self.assertRaises(AssertionError):
                    study.parse_output(bad, m)
            if kind != "plain":
                for key, value in (("Accounting", not d["Accounting"]), ("BackoffCap", 17), ("Phases", d["Phases"][:-1]), ("Profiled", not d["Profiled"])):
                    with self.assertRaises(AssertionError):
                        study.parse_output(text+"metadata-pipeline-diagnostics: "+json.dumps(d | {key: value}), m)

    def record(self, folder, m, p):
        folder.mkdir(parents=True)
        text, d = self.output(m)
        if m["kind"] != "plain":
            text += "metadata-pipeline-diagnostics: "+json.dumps(d)+"\n"
        (folder / "stdout.txt").write_text(text)
        (folder / "stderr.txt").write_text("")
        before = dict(cpu={"cpu": [0]*8}, swap=dict(pswpin=0, pswpout=0))
        after = dict(cpu={"cpu": [10, 0, 0, 70, 0, 0, 0, 20]}, swap=dict(pswpin=0, pswpout=0))
        record = dict(member=m, exit=0, host=p["host"], binary_sha256=p["hashes"][study.binary(m)],
            environment=study.recorded_env(study.environment(m, folder)), command=study.command(self.root, folder, m),
            before=before, after=after, activity=[], started=0, ended=1, profiles={}, **study.scale.host_activity(before, after, [], 2))
        for stream in ("stdout", "stderr"):
            record[stream+"_sha256"] = study.scale.sha(folder / (stream+".txt"))
        study.scale.write(folder / "record.json", record)
        return record

    def test_record_integrity_and_relocation(self):
        m = study.member("backlog") | dict(arm="candidate")
        p = dict(host=dict(kernel="test", boot="test"), hashes={study.binary(m): "binary-hash"})
        folder = self.root / "original"
        r = self.record(folder, m, p)
        parsed = study.validate(self.root, folder, m, p)
        self.assertTrue(parsed["host"]["warnings"])
        self.assertFalse(parsed["host"]["noise"], "Steal is not an exclusion")
        relocated = self.root / "relocated"
        shutil.copytree(folder, relocated)
        study.validate(self.root, relocated, m, p)
        for key, value in (("exit", 1), ("environment", {}), ("binary_sha256", "wrong"), ("profiles", {"cpu.pprof": "missing"})):
            study.scale.write(relocated / "record.json", r | {key: value})
            with self.assertRaises(AssertionError):
                study.validate(self.root, relocated, m, p)
        study.scale.write(relocated / "record.json", r)
        (relocated / "stdout.txt").write_text("corruption")
        with self.assertRaises(AssertionError):
            study.validate(self.root, relocated, m, p)

    def test_missing_corrupt_and_valid_empty_profiles(self):
        path = self.root / "cpu.pprof"
        with self.assertRaises(AssertionError):
            study.profile_info(path)
        path.write_bytes(b"test profile")
        for raw, count in (("Samples:\nsamples/count cpu/nanoseconds\nLocations\n", 0),
                           ("Samples:\nsamples/count cpu/nanoseconds\n 3 30000000: 1 2\n 2 20000000: 3\nLocations\n", 5)):
            with mock.patch.object(study.subprocess, "check_output", return_value=raw):
                self.assertEqual(dict(valid=True, samples=count), study.profile_info(path))
        with mock.patch.object(study.subprocess, "check_output", side_effect=subprocess.CalledProcessError(1, "pprof")):
            with self.assertRaises(subprocess.CalledProcessError):
                study.profile_info(path)

    def test_collection_requires_exact_coverage(self):
        block = dict(id="bridge/1/0/backlog", cohort=1, members=[study.member("backlog") | dict(arm=a) for a in study.ARMS])
        p = dict(blocks=[block], counts=dict(plain=2), host=dict(kernel="test", boot="test"),
                 hashes={study.binary(m): "hash" for m in block["members"]})
        folder = self.root / "results" / block["id"] / "attempt-0"
        warnings = []
        for i, m in enumerate(block["members"]):
            r = self.record(folder / str(i), m, p)
            warnings += r["warnings"]
        study.scale.write(folder / "block.json", dict(block=block, attempt=0, noise=[], warnings=warnings))
        study.scale.write(folder.parent / "accepted.json", dict(attempt=0))
        rows, exclusions = study.collect(self.root, p)
        self.assertEqual(2, len(rows))
        self.assertFalse(exclusions)
        shutil.copytree(folder / "0", folder / "duplicate")
        with self.assertRaises(AssertionError):
            study.collect(self.root, p)
        shutil.rmtree(folder / "duplicate")
        (folder / "0" / "record.json").unlink()
        with self.assertRaises(FileNotFoundError):
            study.collect(self.root, p)

    def test_bridge_and_cohorts_are_separate(self):
        rows = []
        for b in study.manifest()["blocks"]:
            if not b["cohort"]:
                continue
            for m in b["members"]:
                rows.append(dict(block=b["id"], cohort=b["cohort"], member=m,
                    metrics={"ns/op": (100 if m["arm"] == "forwarding" else 90)*(2 if m["variant"] == "rebuilt" else 1)}))
        summary, _ = study.comparisons(rows)
        self.assertEqual(26, len(summary))
        for row in summary:
            expected = 2 if row["comparison"].startswith("bridge-") else 1 if row["comparison"].startswith("control-") else .9
            self.assertAlmostEqual(expected, row["metrics"]["ns/op"]["ratio"]["median"])
        with self.assertRaises(AssertionError):
            study.comparisons(rows+rows[:1])
        with self.assertRaises(AssertionError):
            study.comparisons(rows[1:])


if __name__ == "__main__":
    unittest.main()

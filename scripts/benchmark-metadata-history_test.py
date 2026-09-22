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

import collections
import copy
import importlib.util
import itertools
import json
from pathlib import Path
import shutil
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

sys.dont_write_bytecode = True
module = importlib.util.spec_from_file_location("history", Path(__file__).with_name("benchmark-metadata-history.py"))
study = importlib.util.module_from_spec(module)
module.loader.exec_module(study)


class HistoryStudyTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="metadata-history-test-")
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.host = dict(kernel="test-kernel", boot="test-boot")

    def record(self, root, folder, member, planned, swap=False):
        folder.mkdir()
        speed = dict(baseline=100, index=105, series=90)[member["arm"]]
        text = ("goos: linux\ngoarch: amd64\n"+study.name(member)+f"-{member['procs']} 1 {speed} ns/op 0 B/op 0 allocs/op\nPASS\n")
        (folder / "stdout.txt").write_text(text)
        (folder / "stderr.txt").write_text("")
        before = dict(cpu={"cpu": [0]*8, "cpu0": [0]*8}, swap=dict(pswpin=0, pswpout=0))
        after = dict(cpu={"cpu": [10, 0, 0, 70, 0, 0, 0, 20], "cpu0": [10, 0, 0, 70, 0, 0, 0, 20]},
                     swap=dict(pswpin=int(swap), pswpout=0))
        env = study.environment(member)
        record = dict(member=member, binary_sha256=planned["hashes"][study.binary_path(member)],
                      command=study.command(root, folder, member), before=before, after=after, activity=[],
                      environment={k: v for k, v in env.items() if k in study.scale.ENV or k.startswith("PROMETHEUS_METADATA_PIPELINE_")},
                      started=0, ended=1, exit=0, host=self.host, profiles={},
                      **study.scale.host_activity(before, after, [], 2))
        for stream in ("stdout", "stderr"):
            record[stream+"_sha256"] = study.scale.sha(folder / (stream+".txt"))
        study.scale.write(folder / "record.json", record)
        return record

    def fixture(self, rejected=False):
        root = self.directory / "study"
        root.mkdir()
        member = dict(package="tsdb", bench="BenchmarkHeadMetricMetadataQuery/versions=1/every=1/limit=10",
                      procs=2, benchtime="1x", kind="head", tags="stringlabels")
        smoke = [member | dict(arm=arm) for arm in study.ARMS]
        manifest = dict(schema=1, counts={"head": 18}, blocks=[
            dict(id=f"head/1/{i}/stringlabels/0", cohort=1, members=smoke) for i in range(6)])
        planned = copy.deepcopy(manifest) | dict(host=self.host, sources={}, hashes={})
        for arm in study.ARMS:
            source = self.directory / ("source-"+arm)
            source.mkdir()
            (source / "fixture.go").write_text(arm+" source\n")
            destination = root / arm
            destination.mkdir()
            with tarfile.open(destination / "source.tar.gz", "w:gz") as archive:
                archive.add(source / "fixture.go", arcname="fixture.go")
            binary = destination / "tsdb-stringlabels.test"
            binary.write_text("Synthetic binary; never executed. "+arm)
            planned["sources"][arm] = dict(path=str(source), source_hashes=study.scale.inventory(source))
            for path in (destination / "source.tar.gz", binary):
                planned["hashes"][path.relative_to(root).as_posix()] = study.scale.sha(path)
        for filename in ("benchmark-metadata-history.py", "benchmark-metadata-pipeline-scale.py"):
            shutil.copyfile(Path(study.__file__).with_name(filename), root / filename)
            planned["hashes"][filename] = study.scale.sha(root / filename)
        study.scale.write(root / "expected-runs.json", planned)
        (root / "expected-runs.sha256").write_text(study.scale.sha(root / "expected-runs.json")+"\n")
        (root / "smoke").mkdir()
        for i, m in enumerate(smoke):
            self.record(root, root / "smoke" / str(i), m, planned)
        (root / "SMOKE_COMPLETE").touch()
        for block in planned["blocks"]:
            destination = root / "results" / block["id"]
            destination.mkdir(parents=True)
            accepted = int(rejected and block == planned["blocks"][0])
            for attempt in range(accepted+1):
                folder = destination / f"attempt-{attempt}"
                folder.mkdir()
                noise, warnings = [], []
                for i, m in enumerate(block["members"]):
                    r = self.record(root, folder / str(i), m, planned, swap=attempt != accepted and i == 0)
                    noise += r["noise"]
                    warnings += r["warnings"]
                study.scale.write(folder / "block.json", dict(block=block, attempt=attempt, noise=noise, warnings=warnings))
            study.scale.write(destination / "accepted.json", dict(attempt=accepted))
        (root / "MEASUREMENT_COMPLETE").touch()
        patch = mock.patch.object(study, "manifest", return_value=manifest)
        patch.start()
        self.addCleanup(patch.stop)
        patch = mock.patch.object(study, "smoke_members", return_value=smoke)
        patch.start()
        self.addCleanup(patch.stop)
        return root, planned

    def test_balanced_frozen_matrix(self):
        planned = study.manifest()
        self.assertEqual(1728, planned["counts"]["scored"])
        self.assertEqual(24408, planned["counts"]["head"]+planned["counts"]["head-heap"])
        self.assertEqual(2178, len(study.smoke_members()))
        for cohort in (1, 2):
            blocks = [b for b in planned["blocks"] if b["id"].startswith(f"pipeline/{cohort}/")]
            self.assertEqual(96, len(blocks))
            for case in range(16):
                selected = [b for b in blocks if b["id"].endswith("/"+str(case))]
                positions = collections.Counter()
                for block in selected:
                    self.assertEqual({(arm, mode) for arm in study.ARMS for mode in study.MODES},
                                     {(m["arm"], m["mode"]) for m in block["members"]})
                    for position, member in enumerate(block["members"]):
                        positions[member["arm"], position//3] += 1
                self.assertEqual({6}, set(positions.values()))
        self.assertFalse(any("/mode=dual" in study.name(m) for b in planned["blocks"] for m in b["members"]))

    def test_smoke_selection_and_order(self):
        heads = study.head_cases()
        sparse = [m["bench"] for m in heads if "AppendSparseChangesInMemory/" in m["bench"]]
        self.assertEqual(["BenchmarkHeadMetricMetadataAppendSparseChangesInMemory/mode="+mode
                          for mode in ("off", "native")], sparse)
        self.assertIn("BenchmarkHeadMetricMetadataSeriesChurn/mode=legacy", [m["bench"] for m in heads])
        smoke = study.smoke_members()
        self.assertEqual([m | dict(arm="baseline", tags="stringlabels", benchtime="1x") for m in heads],
                         smoke[:len(heads)])
        head_count = len(heads)*len(study.ARMS)*len(study.TAGS)
        self.assertTrue(all(m["package"] == "tsdb" for m in smoke[:head_count]))
        self.assertTrue(all(m["package"] == "remote" for m in smoke[head_count:]))
        expected = {json.dumps(m | dict(benchtime="1x"), sort_keys=True)
                    for block in study.manifest()["blocks"] if block["cohort"] for m in block["members"]}
        self.assertEqual(collections.Counter({key: 1 for key in expected}),
                         collections.Counter(json.dumps(m, sort_keys=True) for m in smoke))

    def test_failed_smoke_preserves_evidence_and_blocks_measurement(self):
        root = self.directory / "failed-smoke"
        (root / "baseline").mkdir(parents=True)
        member = study.head_cases()[0] | dict(arm="baseline", tags="stringlabels", benchtime="1x")
        binary = study.binary_path(member)
        (root / binary).write_text("Synthetic binary; never executed.")
        planned = dict(host=self.host, hashes={binary: study.scale.sha(root / binary)},
                       sources={"baseline": dict(path=str(root / "baseline"))})

        def no_measurement(args, **kwargs):
            kwargs["stdout"].write("PASS\n")
            return mock.Mock(poll=mock.Mock(return_value=0), returncode=0, pid=1234)

        with mock.patch.object(study, "load", return_value=planned), \
             mock.patch.object(study, "smoke_members", return_value=[member]), \
             mock.patch.object(study.scale, "host_identity", return_value=self.host), \
             mock.patch.object(study.scale, "snapshot", return_value={}), \
             mock.patch.object(study.scale, "processes", return_value={}), \
             mock.patch.object(study.scale, "host_activity", return_value=dict(noise=[], warnings=[], steal_fractions=[])), \
             mock.patch.object(study.subprocess, "Popen", side_effect=no_measurement):
            with self.assertRaisesRegex(RuntimeError, "missing benchmark") as failure:
                study.smoke(root)
            for context in (study.name(member), "baseline", "stringlabels", "-test.bench=", str(root / "smoke/0")):
                self.assertIn(context, str(failure.exception))
            self.assertEqual("PASS\n", (root / "smoke/0/stdout.txt").read_text())
            self.assertEqual("", (root / "smoke/0/stderr.txt").read_text())
            self.assertEqual(0, json.loads((root / "smoke/0/record.json").read_text())["exit"])
            self.assertFalse((root / "SMOKE_COMPLETE").exists())
            with mock.patch.object(study, "run_one") as execute:
                with self.assertRaises(AssertionError):
                    study.run(root)
                execute.assert_not_called()
            self.assertFalse((root / "results").exists())

    def test_complete_coverage_and_steal_are_retained(self):
        root, _ = self.fixture()
        planned = study.load(root)
        rows, exclusions = study.collect(root, planned)
        self.assertEqual(18, len(rows))
        self.assertFalse(exclusions)
        self.assertTrue(all(o["host"]["warnings"] and not o["host"]["noise"] for o in rows))
        summary = study.paired_summary(rows)
        self.assertEqual(1, len(summary))
        self.assertAlmostEqual(.9, summary[0]["metrics"]["ns/op"]["series/baseline"]["median"])
        with self.assertRaisesRegex(AssertionError, "duplicate"):
            study.paired_summary(rows+[rows[0]])

    def test_rejected_block_is_not_pooled(self):
        root, planned = self.fixture(rejected=True)
        rows, exclusions = study.collect(root, planned)
        self.assertEqual(18, len(rows))
        self.assertEqual(1, len(exclusions))
        self.assertEqual(["swap activity"], exclusions[0]["reasons"])

    def test_missing_and_foreign_observations_fail(self):
        root, planned = self.fixture()
        path = root / "results/head/1/0/stringlabels/0/attempt-0/0/record.json"
        record = json.loads(path.read_text())
        changed = copy.deepcopy(record)
        changed["member"]["arm"] = "series"
        study.scale.write(path, changed)
        with self.assertRaises(AssertionError):
            study.collect(root, planned)
        study.scale.write(path, record)
        extra = root / "results/foreign"
        extra.mkdir()
        shutil.copyfile(path, extra / "record.json")
        with self.assertRaises(AssertionError):
            study.collect(root, planned)
        (extra / "record.json").unlink()
        (root / "results/head/1/1/stringlabels/0/accepted.json").unlink()
        with self.assertRaises(FileNotFoundError):
            study.collect(root, planned)

    def test_binary_and_raw_output_tampering_fail(self):
        root, planned = self.fixture()
        binary = root / "series/tsdb-stringlabels.test"
        original = binary.read_bytes()
        binary.write_bytes(original+b" changed")
        with self.assertRaises(AssertionError):
            study.load(root)
        binary.write_bytes(original)
        output = root / "results/head/1/0/stringlabels/0/attempt-0/0/stdout.txt"
        output.write_text(output.read_text().replace("100 ns/op", "50 ns/op"))
        with self.assertRaises(AssertionError):
            study.collect(root, planned)

    def test_offline_analysis_after_relocation(self):
        root, planned = self.fixture()
        relocated = self.directory / "relocated"
        shutil.copytree(root, relocated)
        for source in planned["sources"].values():
            shutil.rmtree(source["path"])
        def benchstat(args, stdout, check):
            self.assertEqual("benchstat", args[0])
            stdout.write("Synthetic benchstat output.\n")
        with mock.patch.object(study.subprocess, "run", side_effect=benchstat), \
             mock.patch.object(study, "assess_gates", return_value={"synthetic_test": True}), \
             mock.patch.object(study.scale, "host_identity", side_effect=AssertionError("offline analysis inspected live host")):
            study.analyze(relocated)
        summary = json.loads((relocated / "analysis/summary.json").read_text())
        self.assertEqual(1, len(summary))
        self.assertEqual(1, summary[0]["cohort"])

    def test_malformed_or_missing_metrics_fail(self):
        member = study.head_cases()[0] | dict(arm="baseline", tags="stringlabels", benchtime="1x")
        content = "goos: linux\ngoarch: amd64\n"+study.name(member)+"-2 1 10 ns/op 0 B/op 0 allocs/op\nPASS\n"
        self.assertEqual(10, study.parse_output(content, member)["metrics"]["ns/op"])
        for bad in (content.replace("10 ns/op", "NaN ns/op"), content.replace(" 1 10 ", " 2 10 "),
                    content.replace("PASS", "FAIL"), content+content):
            with self.subTest(output=bad), self.assertRaises(AssertionError):
                study.parse_output(bad, member)
        with self.assertRaisesRegex(AssertionError, "heap"):
            study.parse_output(content, member | dict(kind="head-heap"))

        # All parser paths must reject successful exits without the exact measurement.
        members = [member, next(m for m in study.pipeline_cases() if m["group"] == "original"),
                   next(m for m in study.pipeline_cases() if m["group"] != "original")]
        for m in members:
            header = "goos: linux\ngoarch: amd64\n"
            line = study.name(m)+f"-{m['procs']} 1 10 ns/op 0 B/op 0 allocs/op\n"
            for reason, output in (("missing benchmark", "PASS\n"),
                                   ("missing benchmark", header+"--- SKIP: "+study.name(m)+"\nPASS\n"),
                                   ("unexpected benchmark", header+line.replace(study.name(m), study.name(m)+"Unknown")+"PASS\n"),
                                   ("duplicate benchmark", header+line+line+"PASS\n")):
                with self.subTest(benchmark=study.name(m), reason=reason), self.assertRaisesRegex(AssertionError, reason):
                    study.parse_output(output, m)

    def test_cohorts_do_not_pool(self):
        root, planned = self.fixture()
        rows, _ = study.collect(root, planned)
        other = [o | dict(cohort=2, block=o["block"].replace("/1/", "/2/", 1),
                          metrics=o["metrics"] | {"ns/op": 2*o["metrics"]["ns/op"]}) for o in rows]
        result = study.paired_summary(rows+other)
        self.assertEqual([1, 2], [s["cohort"] for s in result])
        self.assertEqual([100, 200], [s["metrics"]["ns/op"]["medians"]["baseline"] for s in result])



    def test_incremental_heap_and_pipeline_ratios(self):
        rows = []
        for cohort, repetition, arm, mode in itertools.product((1, 2), range(6), study.ARMS, ("off", "native")):
            overhead = dict(baseline=100, index=110, series=104)[arm]
            rows.append(dict(cohort=cohort, block=f"head/{cohort}/{repetition}/stringlabels/0",
                             member=dict(package="tsdb", tags="stringlabels", arm=arm,
                                         bench="BenchmarkHeadMetricMetadataRetainedHeap/scenario=stable/mode="+mode),
                             metrics={"heap-B/series": 500+(overhead if mode == "native" else 0)}))
        result = study.incremental_heap(rows)
        self.assertEqual([1, 2], [r["cohort"] for r in result])
        self.assertEqual(104, result[0]["bytes_per_series"]["series"])
        self.assertAlmostEqual(1.04, result[0]["series/baseline"]["median"])
        with self.assertRaisesRegex(AssertionError, "duplicate"):
            study.incremental_heap(rows+[rows[0]])
        for row in rows:
            if row["member"]["arm"] == "baseline":
                row["metrics"]["heap-B/series"] = 500
        self.assertTrue(all("inconclusive" in r for r in study.incremental_heap(rows)))
        rows = []
        for cohort, repetition, arm, mode in itertools.product((1, 2), range(6), study.ARMS, study.MODES):
            value = dict(native=2, wal=1, disabled=.5)[mode]
            rows.append(dict(cohort=cohort, block=f"pipeline/{cohort}/{repetition}/0",
                             member=dict(package="remote", group="original", case="cold", arm=arm, mode=mode),
                             metrics={metric: value for metric in ("cpu-ns/sample", "alloc-B/sample", "samples/s")}))
        ratios = study.pipeline_ratios(rows)
        self.assertEqual(6, len(ratios))
        self.assertTrue(all(r["native_wal"]["cpu-ns/sample"]["median"] == 2 for r in ratios))
        with self.assertRaises(AssertionError):
            study.pipeline_ratios(rows[:-1])

    def test_adoption_gates_fail_closed(self):
        def metric(ratio, maximum=None):
            return {"series/baseline": dict(median=ratio, minimum=ratio, maximum=ratio if maximum is None else maximum),
                    "medians": dict(baseline=100, index=100, series=100*ratio)}
        primary = [dict(cohort=cohort, tags="stringlabels", procs=4, package="remote",
                        bench="BenchmarkRemoteWriteMetadataPipelineScale/group=backlog/case=changes/values=distinct/series=100000/source=native",
                        metrics={"cpu-ns/sample": metric(.94)}) for cohort in (1, 2)]
        self.assertTrue(study.assess_gates(primary, [])["performance_gates_pass"])
        for ratio, maximum in ((.96, .98), (.94, 1)):
            changed = copy.deepcopy(primary)
            changed[0]["metrics"]["cpu-ns/sample"] = metric(ratio, maximum)
            self.assertFalse(study.assess_gates(changed, [])["performance_gates_pass"])
        with self.assertRaisesRegex(AssertionError, "primary replication"):
            study.assess_gates(primary[:1], [])
        protected = dict(cohort=1, tags="stringlabels", procs=2, package="tsdb",
                         bench="BenchmarkHeadMetricMetadataAppend/case=stable/mode=native",
                         metrics={"ns/op": metric(1), "B/op": metric(1), "allocs/op": metric(1)})
        for unit, ratio in (("ns/op", 1.06), ("B/op", 1.01), ("allocs/op", 1.01)):
            changed = copy.deepcopy(protected)
            changed["metrics"][unit] = metric(ratio)
            self.assertFalse(study.assess_gates(primary+[changed], [])["performance_gates_pass"])
        for heap in ({"series/baseline": dict(median=1.06)}, {"inconclusive": "zero baseline"}):
            self.assertFalse(study.assess_gates(primary, [heap])["performance_gates_pass"])
        total = protected | dict(bench="BenchmarkHeadMetricMetadataRetainedHeap/scenario=stable/mode=native",
                                 metrics={"heap-B/series": metric(1.06)})
        self.assertFalse(study.assess_gates(primary+[total], [])["performance_gates_pass"])

    def test_original_pipeline_parser(self):
        for case in ("cold", "newseries"):
            with self.subTest(case=case):
                member = next(m for m in study.pipeline_cases() if m["case"] == case)
                samples = 10000 if case == "cold" else 2000000
                result = dict(
                    Config=dict(Base=0, Group="", Case=case, Source="native", Series=10000, Values=100,
                                Sweeps=200, Writers=4, Shards=4, Batch=2000, Capacity=10000, CommitSize=1000,
                                ReceiverProcs=2, Mixed=False, SweepInterval=0, SamplesPerSecond=0),
                    Diagnostic=False, Samples=samples, ResidentSeries=10000 if case == "cold" else 30000,
                    Transactions=12 if case == "cold" else 2400, ScheduledTransactions=0,
                    TransactionsLateByInterval=0, SweepsLateByInterval=0,
                    CPU=dict(Available=True, User=10*samples, System=0),
                    ReceiverCPU=dict(Available=True), LifecycleCPU=dict(Available=True),
                    Completion=10, Lifecycle=11, Ingestion=5, Drain=3, Shutdown=2,
                    BacklogWait=0, ReleaseToDrain=0, SeededHeap=0, BacklogHeap=0, DrainedHeap=0,
                    OutstandingAtWriterEnd=0, PeakSampledQueue=0, AllocatedBytes=2*samples,
                    Allocations=samples, WALBytes=1, RequestBytes=1, ReceiverServiceNanos=1)
                metrics = {"ns/op": 10, "B/op": 0, "allocs/op": 0, "samples/op": samples, "samples/s": samples,
                           "cpu-ns/sample": 10, "alloc-B/sample": 2, "allocs/sample": 1,
                           "wal-B/sample": 1, "wire-B/sample": 1, "drain-ms/op": 1, "txn-p99-ns": 1}
                def output(r, values):
                    return ("goos: linux\ngoarch: amd64\n"+study.name(member)+"-4 1 "+
                            " ".join(f"{v} {k}" for k, v in values.items())+"\n"+
                            "metadata-pipeline-result: "+json.dumps(r)+"\nPASS\n")
                self.assertEqual(10, study.parse_output(output(result, metrics), member)["metrics"]["cpu-ns/sample"])
                for changed in (result | dict(Transactions=0), result | dict(Diagnostic=True),
                                result | dict(ResidentSeries=1), result | dict(AllocatedBytes=-1)):
                    with self.assertRaises(AssertionError):
                        study.parse_output(output(changed, metrics), member)
                del metrics["samples/s"]
                with self.assertRaisesRegex(AssertionError, "pipeline metric"):
                    study.parse_output(output(result, metrics), member)


if __name__ == "__main__":
    unittest.main()

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
spec = importlib.util.spec_from_file_location("forwarding", Path(__file__).with_name("benchmark-metadata-history-forwarding.py"))
study = importlib.util.module_from_spec(spec)
spec.loader.exec_module(study)


class ForwardingStudyTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="metadata-forwarding-test-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.host = dict(kernel="test", boot="test")

    def test_manifest_and_balanced_order(self):
        planned = study.manifest()
        self.assertEqual(406, len(planned["blocks"]))
        self.assertEqual(812, sum(planned["counts"].values()))
        self.assertEqual(78, len(study.current.smoke_members(planned)))
        self.assertEqual(406, len({b["id"] for b in planned["blocks"]}))
        groups, stages, profiles = {}, Counter(), Counter()
        for block in planned["blocks"]:
            for m in block["members"]:
                self.assertEqual("stringlabels", m["tags"])
                self.assertEqual(m["stage"], block["id"].split("/")[0])
                if block["cohort"]:
                    key = (m["stage"], block["cohort"], m["kind"], study.history.name(m), m["procs"], m["benchtime"])
                    groups.setdefault(key, []).append(m["arm"])
                    stages[m["stage"]] += 1
                else:
                    profiles[m["stage"]] += 1
                    if m["stage"] == "diagnosis":
                        self.assertEqual("32768x", m["benchtime"])
        self.assertEqual(dict(diagnosis=168, forwarding=624), stages)
        self.assertEqual(dict(diagnosis=12, forwarding=8), profiles)
        for key, arms in groups.items():
            pair = study.PAIRS[key[0]]
            self.assertEqual(Counter({a: 6 for a in pair}), Counter(arms))
            self.assertEqual(Counter({pair: 3, tuple(reversed(pair)): 3}), Counter(tuple(arms[i:i+2]) for i in range(0, 12, 2)))
        self.assertEqual("record", planned["policy"]["steal_action"])
        self.assertNotIn("improvement", planned["primary"])

    def test_work_distribution_metrics(self):
        for n, scheduling, kind in itertools.product((1, 7, 8, 17, 8192, 32768), ("balanced", "shared-budget"), ("head-work-distribution", "profile", "locks")):
            with self.subTest(n=n, scheduling=scheduling, kind=kind):
                member = dict(stage="diagnosis", arm="candidate", package="tsdb", bench=study.FIXED+scheduling,
                              procs=2, kind=kind, benchtime=f"{n}x")
                counts = [n//8+(i<n%8) for i in range(8)] if scheduling == "balanced" else [n]+[0]*7
                metrics = dict(zip(("ns/op", "B/op", "allocs/op", "transactions", "acknowledgments", "samples", "samples/op", "ns/sample", "GC-cycles", "GC-pause-ns"),
                                   (100, 2000, 2, n, n, n*1000, 1000, .1, 0, 0)))
                metrics.update({f"worker{i}-transactions": value for i, value in enumerate(counts)})
                self.assertEqual(2, study.validate_metrics(member, dict(metrics=metrics.copy()))["metrics"]["alloc-B/sample"])
                for key in ("samples", "transactions", "acknowledgments", "samples/op", "worker0-transactions"):
                    with self.assertRaises(AssertionError):
                        study.validate_metrics(member, dict(metrics=metrics | {key: metrics[key]+1}))
                with self.assertRaises(AssertionError):
                    study.validate_metrics(member | dict(stage="forwarding", arm="coallocated"), dict(metrics=metrics))

    def test_pipeline_phase_metrics(self):
        cases = [b["members"][0] for b in study.manifest()["blocks"]
                 if b["cohort"] == 1 and b["id"].split("/")[2] == "0" and b["members"][0]["package"] == "remote"]
        self.assertEqual(4, len(cases))
        for member in cases:
            with self.subTest(case=study.history.name(member)):
                phases = {field: 123000000 for field in ("Ingestion", "Initialization", "Drain", "Shutdown", "Completion", "BacklogWait", "ReleaseToDrain")}
                parsed = study.validate_metrics(member, dict(metrics={"ns/op": 1, "B/op": 0, "allocs/op": 0}, result=phases))
                self.assertEqual(123, parsed["metrics"]["ReleaseToDrain-ms"])
                self.assertEqual(123, parsed["metrics"]["BacklogWait-ms"])

    def record(self, root, folder, member, planned):
        folder.mkdir()
        speed = dict(coallocated=100, forwarding=110, candidate=95)[member["arm"]]
        (folder / "stdout.txt").write_text(
            f"goos: linux\ngoarch: amd64\n{study.history.name(member)}-{member['procs']} 1 {speed} ns/op 0 B/op 0 allocs/op 1 ns/sample\nPASS\n")
        (folder / "stderr.txt").write_text("")
        before = dict(cpu={"cpu": [0]*8}, swap=dict(pswpin=0, pswpout=0))
        after = dict(cpu={"cpu": [10, 0, 0, 70, 0, 0, 0, 20]}, swap=dict(pswpin=0, pswpout=0))
        record = dict(member=member, binary_sha256=planned["hashes"][study.history.binary_path(member)],
                      command=study.history.command(root, folder, member), before=before, after=after,
                      activity=[], started=0, ended=1, exit=0, host=self.host, profiles={},
                      environment={k: v for k, v in study.history.environment(member).items()
                                   if k in study.scale.ENV or k.startswith("PROMETHEUS_METADATA_PIPELINE_")},
                      **study.scale.host_activity(before, after, [], 2))
        for stream in ("stdout", "stderr"):
            record[stream+"_sha256"] = study.scale.sha(folder / (stream+".txt"))
        study.scale.write(folder / "record.json", record)
        return record

    def fixture(self):
        root = self.directory / "study"
        root.mkdir()
        member = dict(package="tsdb", bench=study.current.PRIMARY, procs=2, benchtime="1s", kind="head", tags="stringlabels")
        minimal = study.manifest() | dict(blocks=[
            dict(id=f"{stage}/{cohort}/{repeat}/0", cohort=cohort,
                 members=[member | dict(stage=stage, arm=arm) for arm in study.PAIRS[stage]])
            for stage, cohort, repeat in itertools.product(study.PAIRS, (1, 2), range(6))], counts=dict(head=48))
        planned = minimal | dict(host=self.host, sources={}, hashes={}, inputs={})
        for arm in study.REVISIONS:
            source = self.directory / (arm+"-source")
            source.mkdir()
            (source / "fixture_bench_test.go").write_text("package tsdb\n")
            destination = root / arm
            destination.mkdir()
            for fixture in study.FIXTURES:
                (source / fixture).parent.mkdir(exist_ok=True)
                (source / fixture).write_text("package tsdb\n")
            with tarfile.open(destination / "production.tar", "w", format=tarfile.PAX_FORMAT,
                              pax_headers={"comment": study.REVISIONS[arm]}) as archive:
                archive.add(source / "fixture_bench_test.go", arcname="fixture_bench_test.go")
                if arm == "candidate":
                    archive.add(source / study.FIXTURES[0], arcname=study.FIXTURES[0])
            planned["fixture_hashes"] = study.current.fixture_hashes(source)
            inventory = study.scale.inventory(source)
            planned["sources"][arm] = dict(path=str(source), source_hashes=inventory)
            planned["inputs"][arm] = dict(revision=study.REVISIONS[arm], source_hashes=inventory,
                overlay={p: inventory[p] for p in study.FIXTURES if arm != "candidate" or p != study.FIXTURES[0]},
                production_archive_sha256=study.scale.sha(destination / "production.tar"))
            with tarfile.open(destination / "source.tar.gz", "w:gz") as archive:
                for path in source.rglob("*"):
                    if path.is_file():
                        archive.add(path, arcname=path.relative_to(source).as_posix())
            for package in study.packages(arm):
                (destination / f"{package}-stringlabels.test").write_text("Synthetic binary, never executed.\n")
            for path in destination.iterdir():
                planned["hashes"][path.relative_to(root).as_posix()] = study.scale.sha(path)
        for filename in study.SCRIPTS:
            shutil.copy2(Path(study.__file__).with_name(filename), root / filename)
            planned["hashes"][filename] = study.scale.sha(root / filename)
        study.scale.write(root / "expected-runs.json", planned)
        (root / "expected-runs.sha256").write_text(study.scale.sha(root / "expected-runs.json")+"\n")
        (root / "smoke").mkdir()
        for i, m in enumerate(study.current.smoke_members(planned)):
            self.record(root, root / "smoke" / str(i), m, planned)
        (root / "SMOKE_COMPLETE").touch()
        for block in planned["blocks"]:
            destination = root / "results" / block["id"]
            destination.mkdir(parents=True)
            attempt = destination / "attempt-0"
            attempt.mkdir()
            warnings = []
            for i, m in enumerate(block["members"]):
                warnings += self.record(root, attempt / str(i), m, planned)["warnings"]
            study.scale.write(attempt / "block.json", dict(block=block, attempt=0, noise=[], warnings=warnings))
            study.scale.write(destination / "accepted.json", dict(attempt=0))
        (root / "MEASUREMENT_COMPLETE").touch()
        self.addCleanup(mock.patch.stopall)
        mock.patch.object(study, "manifest", return_value=minimal).start()
        return root, planned

    def test_add_only_overlays(self):
        root, planned = self.fixture()
        for arm in study.REVISIONS:
            entry = planned["inputs"][arm]
            study.verify_overlay(root / arm / "production.tar", entry)
            for bad in (dict(revision="wrong"), dict(overlay={}),
                        dict(source_hashes=entry["source_hashes"] | {"unexpected.go": "modified"}),
                        dict(source_hashes=entry["source_hashes"] | {"fixture_bench_test.go": "modified"})):
                with self.subTest(arm=arm, bad=bad), self.assertRaises(AssertionError):
                    study.verify_overlay(root / arm / "production.tar", entry | bad)

    def test_relocated_analysis_and_stage_separation(self):
        root, planned = self.fixture()
        for record in planned["sources"].values():
            shutil.rmtree(record["path"])
        moved = self.directory / "relocated"
        root.rename(moved)
        study.load(moved)
        study.validate_smoke(moved, planned)
        rows, exclusions = study.collect(moved, planned)
        self.assertFalse(exclusions)
        self.assertTrue(all(r["host"]["warnings"] for r in rows))
        summary = study.paired_summary(rows)
        self.assertEqual(4, len(summary))
        for row in summary:
            expected = .95 if row["stage"] == "diagnosis" else 95/110
            self.assertAlmostEqual(expected, row["metrics"]["ns/op"]["ratio"]["median"])
        self.assertFalse(study.assess(summary)["allocation_flags"])
        with mock.patch.object(study.subprocess, "run") as run:
            study.analyze(moved)
        self.assertEqual(4, run.call_count)
        for call in run.call_args_list:
            _, before, after = call.args[0]
            self.assertEqual(before.split("-cohort-")[0], after.split("-cohort-")[0])
            for name in (before, after):
                self.assertFalse(Path(name).is_absolute())
                counts = Counter(line.split()[0] for line in (Path(call.kwargs["cwd"]) / name).read_text().splitlines() if line.startswith("Benchmark"))
                self.assertEqual({6}, set(counts.values()))
        bad = copy.deepcopy(rows)
        bad[0]["member"]["stage"] = "forwarding"
        with self.assertRaises(AssertionError):
            study.paired_summary(bad)
        with self.assertRaises(AssertionError):
            study.paired_summary(rows + [rows[0]])
        with self.assertRaises(AssertionError):
            study.paired_summary(rows[1:])

    def test_tampering_and_incomplete_results(self):
        root, planned = self.fixture()
        binary = root / "candidate/tsdb-stringlabels.test"
        saved = binary.read_bytes()
        binary.write_bytes(b"tampered")
        with self.assertRaises(AssertionError):
            study.load(root)
        binary.write_bytes(saved)
        record = root / "results/diagnosis/1/0/0/attempt-0/0/stdout.txt"
        saved = record.read_bytes()
        record.write_bytes(saved.replace(b"100 ns/op", b"90 ns/op"))
        with self.assertRaises(AssertionError):
            study.collect(root, planned)
        record.write_bytes(saved)
        (root / "MEASUREMENT_COMPLETE").unlink()
        with self.assertRaises(AssertionError):
            study.analyze(root)
        (root / "expected-runs.sha256").write_text("wrong\n")
        with self.assertRaises(AssertionError):
            study.load(root)

    def test_zero_baseline_and_regression_flags(self):
        pair = study.PAIRS["forwarding"]
        for before, after in ((0, 0), (0, 1), (100, 110), (100, 90)):
            values = study.comparison(dict(zip(pair, ([before]*6, [after]*6))), pair)
            self.assertEqual(after-before, values["absolute_delta"]["median"])
            self.assertEqual(before > 0, "median" in values["ratio"])
            row = dict(stage="forwarding", kind="head", metrics={unit: values for unit in ("ns/op", "B/op", "allocs/op")})
            assessment = study.assess([row])
            self.assertEqual(after > before, bool(assessment["allocation_flags"]))
            self.assertEqual(after > before, bool(assessment["protected_flags"]))

    def test_run_retains_steal_and_bounds_local_interference(self):
        for noise in (False, True):
            with self.subTest(noise=noise):
                root = self.directory / str(noise)
                root.mkdir()
                block = dict(id="diagnosis/1/0/0", cohort=1, members=[dict(stage="diagnosis", arm=a) for a in study.PAIRS["diagnosis"]])
                planned = dict(blocks=[block])
                result = dict(noise=["swap activity"] if noise else [], warnings=["cpu steal 20%"])
                with mock.patch.object(study, "load", return_value=planned), mock.patch.object(study, "validate_smoke"), \
                     mock.patch.object(study.history, "run_one", return_value=result) as run, mock.patch.object(study, "validate"), \
                     mock.patch.object(study, "collect"):
                    if noise:
                        with self.assertRaises(RuntimeError):
                            study.run(root)
                        self.assertEqual(4, run.call_count)
                        self.assertTrue((root / "INCOMPLETE.json").is_file())
                    else:
                        study.run(root)
                        self.assertEqual(2, run.call_count)
                        self.assertTrue((root / "MEASUREMENT_COMPLETE").is_file())


if __name__ == "__main__":
    unittest.main()

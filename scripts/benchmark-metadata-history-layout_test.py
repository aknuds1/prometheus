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
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("layout", Path(__file__).with_name("benchmark-metadata-history-layout.py"))
study = importlib.util.module_from_spec(spec)
spec.loader.exec_module(study)


class LayoutStudyTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="metadata-layout-test-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.host = dict(kernel="test", boot="test")

    def test_manifest_and_order(self):
        planned = study.manifest()
        self.assertEqual(dict(blocks=328, observations=980, smoke=86), study.counts_text(planned))
        self.assertEqual(328, len({b["id"] for b in planned["blocks"]}))
        groups = {}
        for block in planned["blocks"]:
            for m in block["members"]:
                self.assertEqual("stringlabels", m["tags"])
                if block["cohort"]:
                    key = (block["cohort"], m["kind"], study.history.name(m), m["procs"], m["benchtime"])
                    groups.setdefault(key, []).append(m["arm"])
        for arms in groups.values():
            self.assertEqual(Counter({a: 6 for a in study.ARMS}), Counter(arms))
            orders = [tuple(arms[i:i+3]) for i in range(0, len(arms), 3)]
            self.assertEqual(set(itertools.permutations(study.ARMS)), set(orders))
            for a, b in itertools.combinations(study.ARMS, 2):
                self.assertEqual(3, sum(o.index(a) < o.index(b) for o in orders))
        self.assertFalse(planned["adoption_authorized"])
        self.assertEqual("record", planned["policy"]["steal_action"])

    def record(self, root, folder, member, planned):
        folder.mkdir()
        speed = dict(coallocated=100, inline=95, candidate=90)[member["arm"]]
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
        member = dict(package="tsdb", bench=study.current.PRIMARY, procs=8, benchtime="1s", kind="head", tags="stringlabels")
        minimal = study.manifest() | dict(blocks=[
            dict(id=f"scored/{cohort}/{repeat}/0", cohort=cohort,
                 members=[member | dict(arm=arm) for arm in study.ARMS])
            for cohort, repeat in itertools.product((1, 2), range(6))], counts=dict(head=36))
        planned = minimal | dict(host=self.host, sources={}, hashes={}, inputs={})
        for arm in study.ARMS:
            source = self.directory / (arm+"-source")
            source.mkdir()
            (source / "fixture_bench_test.go").write_text("package tsdb\n")
            destination = root / arm
            destination.mkdir()
            (source / study.FIXTURE).parent.mkdir()
            (source / study.FIXTURE).write_text("package tsdb\n")
            with tarfile.open(destination / "production.tar", "w", format=tarfile.PAX_FORMAT,
                              pax_headers={"comment": study.REVISIONS[arm]}) as archive:
                archive.add(source / "fixture_bench_test.go", arcname="fixture_bench_test.go")
                if arm == "candidate":
                    archive.add(source / study.FIXTURE, arcname=study.FIXTURE)
            planned["fixture_hashes"] = study.current.fixture_hashes(source)
            inventory = study.scale.inventory(source)
            planned["sources"][arm] = dict(path=str(source), source_hashes=inventory)
            planned["inputs"][arm] = dict(revision=study.REVISIONS[arm], source_hashes=inventory,
                overlay={} if arm == "candidate" else {study.FIXTURE: inventory[study.FIXTURE]},
                production_archive_sha256=study.scale.sha(destination / "production.tar"))
            with tarfile.open(destination / "source.tar.gz", "w:gz") as archive:
                for path in source.rglob("*"):
                    if path.is_file():
                        archive.add(path, arcname=path.relative_to(source).as_posix())
            for package in ("tsdb", "remote"):
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

    def test_overlay_forms_and_tampering(self):
        root, planned = self.fixture()
        for arm in study.ARMS:
            entry = planned["inputs"][arm]
            study.verify_overlay(root / arm / "production.tar", entry)
            for field, bad in (("revision", "wrong"), ("source_hashes", entry["source_hashes"] | {"unexpected.go": "modified"}),
                               ("overlay", {study.FIXTURE: "replaced"})):
                with self.subTest(arm=arm, field=field), self.assertRaises(AssertionError):
                    study.verify_overlay(root / arm / "production.tar", entry | {field: bad})

    def test_relocated_analysis_and_pairwise_reports(self):
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
        self.assertEqual(2, len(summary))
        metric = summary[0]["metrics"]["ns/op"]
        self.assertAlmostEqual(.90, metric["candidate/coallocated"]["median"])
        self.assertAlmostEqual(90/95, metric["candidate/inline"]["median"])
        self.assertAlmostEqual(.95, metric["inline/coallocated"]["median"])
        assessment = study.assess(summary, [])
        self.assertTrue(assessment["primary_target_met"])
        self.assertFalse(assessment["adoption_authorized"])
        with mock.patch.object(study.subprocess, "run") as run:
            study.analyze(moved)
        self.assertEqual(6, run.call_count)
        for call in run.call_args_list:
            _, before, after = call.args[0]
            self.assertFalse(Path(before).is_absolute())
            names = [Counter(line.split()[0] for line in (Path(call.kwargs["cwd"]) / p).read_text().splitlines()
                             if line.startswith("Benchmark")) for p in (before, after)]
            self.assertEqual(names[0], names[1])
            self.assertEqual({6}, set(names[0].values()))

    def test_tampered_binary_output_and_manifest(self):
        root, planned = self.fixture()
        binary = root / "candidate/tsdb-stringlabels.test"
        saved = binary.read_bytes()
        binary.write_bytes(b"tampered")
        with self.assertRaises(AssertionError):
            study.load(root)
        binary.write_bytes(saved)
        output = root / "results/scored/1/0/0/attempt-0/0/stdout.txt"
        output.write_text(output.read_text().replace("100 ns/op", "90 ns/op"))
        with self.assertRaises(AssertionError):
            study.collect(root, planned)
        (root / "expected-runs.sha256").write_text("wrong\n")
        with self.assertRaises(AssertionError):
            study.load(root)

    def test_missing_duplicate_and_incomplete_runs(self):
        root, planned = self.fixture()
        rows, _ = study.collect(root, planned)
        for broken in (rows[:-1], rows + [rows[0]]):
            with self.assertRaises(AssertionError):
                study.paired_summary(broken)
        (root / "MEASUREMENT_COMPLETE").unlink()
        with self.assertRaises(AssertionError):
            study.analyze(root)

    def test_fixed_work_counts_and_sizes(self):
        rows = []
        for rounds, cohort, repetition, arm in itertools.product((1024, 4096), (1, 2), range(6), study.ARMS):
            m = dict(package="tsdb", kind="head-fixed-work", bench=study.history.HEAD_PREFIX+"AppendFixedWork/case=fixed-concurrency",
                     procs=2, tags="stringlabels", arm=arm, benchtime=f"{rounds}x")
            output = f"goos: linux\ngoarch: amd64\n{m['bench']}-2 {rounds} 100 ns/op 8 B/op 2 allocs/op 1 ns/sample 8000 samples/op {8*rounds} transactions {8000*rounds} samples\nPASS\n"
            parsed = study.inline.validate_metrics(m, study.history.metrics_line(output, m))
            self.assertEqual(.001, parsed["metrics"]["alloc-B/sample"])
            for unit in ("samples", "samples/op", "transactions"):
                bad = copy.deepcopy(parsed)
                bad["metrics"][unit] += 1
                with self.assertRaises(AssertionError):
                    study.inline.validate_metrics(m, bad)
            rows.append(dict(cohort=cohort, block=f"scored/{cohort}/{repetition}/{rounds}", member=m, **parsed))
        summary = study.paired_summary(rows)
        self.assertEqual(Counter({"1024x": 2, "4096x": 2}), Counter(r["benchtime"] for r in summary))

    def test_heap_tradeoff_and_nonpositive_denominator(self):
        root, planned = self.fixture()
        rows, _ = study.collect(root, planned)
        heap_rows = []
        values = dict(coallocated=105, inline=89, candidate=105)
        for cohort, repetition, arm, mode in itertools.product((1, 2), range(6), study.ARMS, ("off", "native")):
            member = dict(package="tsdb", kind="head-heap", bench=study.history.HEAD_PREFIX+"RetainedHeap/scenario=stable/mode="+mode,
                          procs=2, tags="stringlabels", arm=arm, benchtime="1x")
            heap_rows.append(dict(cohort=cohort, block=f"scored/{cohort}/{repetition}/{mode}", member=member,
                                  metrics={"heap-B/series": 800+(values[arm] if mode == "native" else 0)}))
        heap = study.incremental_heap(heap_rows)
        self.assertEqual(2, len(heap))
        self.assertEqual(values, heap[0]["medians"])
        assessment = study.assess(study.paired_summary(rows + heap_rows), heap)
        self.assertEqual(2, len(assessment["incremental_heap_flags"]))
        self.assertTrue(all(r["expected_tradeoff"] for r in assessment["incremental_heap_flags"]))
        result = study.comparison(dict(coallocated=[0]*6, inline=[-1]*6, candidate=[1]*6))
        self.assertIn("unavailable", result["candidate/coallocated"])
        self.assertIn("unavailable", result["candidate/inline"])

    def test_recovery_requires_both_cohorts_and_all_pairs(self):
        root, planned = self.fixture()
        rows, _ = study.collect(root, planned)
        next(r for r in rows if r["cohort"] == 2 and r["member"]["arm"] == "candidate")["metrics"]["ns/op"] = 101
        result = study.assess(study.paired_summary(rows), [])
        self.assertFalse(result["primary_target_met"])
        self.assertTrue(result["primary"][0]["passes"])
        self.assertFalse(result["primary"][1]["passes"])

    def test_optimizer_cannot_disable_checks(self):
        result = subprocess.run([sys.executable, "-B", "-O", study.__file__], text=True, capture_output=True)
        self.assertNotEqual(0, result.returncode)
        self.assertIn("requires Python without", result.stderr)


if __name__ == "__main__":
    unittest.main()

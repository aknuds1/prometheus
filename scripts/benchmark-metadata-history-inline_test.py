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
spec = importlib.util.spec_from_file_location("inline", Path(__file__).with_name("benchmark-metadata-history-inline.py"))
study = importlib.util.module_from_spec(spec)
spec.loader.exec_module(study)


class InlineStudyTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="metadata-inline-test-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.host = dict(kernel="test", boot="test")

    def test_manifest_and_order(self):
        planned = study.manifest()
        self.assertEqual(2112, sum(planned["counts"].values()))
        self.assertEqual(660, len(planned["blocks"]))
        self.assertEqual(189, len(study.current.smoke_members(planned)))
        self.assertEqual(660, len({b["id"] for b in planned["blocks"]}))
        for cohort in study.ORDERS:
            for a, b in itertools.combinations("ABCDE", 2):
                self.assertEqual(3, sum(o.index(a) < o.index(b) for o in cohort))
        groups = {}
        for block in planned["blocks"]:
            if not block["cohort"]:
                continue
            for m in block["members"]:
                key = (block["cohort"], m["arm"], m["kind"], study.history.name(m), m["procs"], m["benchtime"])
                groups[key] = groups.get(key, 0)+1
        self.assertEqual({6}, set(groups.values()))
        self.assertFalse(planned["adoption_authorized"])
        self.assertEqual("record", planned["policy"]["steal_action"])

    def fixed_member(self, rounds=1024, case="legacy-stable"):
        return dict(package="tsdb", kind="head-fixed-work", bench=study.history.HEAD_PREFIX+"AppendFixedWork/case="+case,
                    procs=study.FIXED[case][1], tags="stringlabels", arm="baseline", benchtime=f"{rounds}x")

    def fixed_output(self, member):
        rounds = int(member["benchtime"][:-1])
        workers = study.FIXED[member["bench"].split("/case=")[1]][0]
        return (f"goos: linux\ngoarch: amd64\n{member['bench']}-{member['procs']} {rounds} "
                f"100 ns/op 8 B/op 2 allocs/op 1 ns/sample {workers*1000} samples/op "
                f"{workers*rounds} transactions {workers*rounds*1000} samples\nPASS\n")

    def test_fixed_work_validation(self):
        for case, rounds in itertools.product(study.FIXED, (1, 1024, 4096)):
            with self.subTest(case=case, rounds=rounds):
                member = self.fixed_member(rounds, case)
                parsed = study.history.metrics_line(self.fixed_output(member), member)
                study.validate_metrics(member, parsed)
                self.assertEqual(8/(study.FIXED[case][0]*1000), parsed["metrics"]["alloc-B/sample"])
                for unit in ("transactions", "samples", "samples/op"):
                    broken = copy.deepcopy(parsed)
                    broken["metrics"][unit] += 1
                    with self.assertRaises(AssertionError):
                        study.validate_metrics(member, broken)
                for old, new in (("1024 ", "1023 "), ("100 ns/op", "nan ns/op"), ("PASS", "FAIL")):
                    if old in self.fixed_output(member):
                        with self.assertRaises(AssertionError):
                            study.history.metrics_line(self.fixed_output(member).replace(old, new), member)

    def record(self, root, folder, m, planned):
        folder.mkdir()
        speed = dict(baseline=100, coallocated=110, inline=105, cancel=100, unlocked=99)[m["arm"]]
        (folder / "stdout.txt").write_text(
            f"goos: linux\ngoarch: amd64\n{study.history.name(m)}-{m['procs']} 1 {speed} ns/op 0 B/op 0 allocs/op 1 ns/sample\nPASS\n")
        (folder / "stderr.txt").write_text("")
        before = dict(cpu={"cpu": [0]*8}, swap=dict(pswpin=0, pswpout=0))
        after = dict(cpu={"cpu": [10, 0, 0, 70, 0, 0, 0, 20]}, swap=dict(pswpin=0, pswpout=0))
        record = dict(member=m, binary_sha256=planned["hashes"][study.history.binary_path(m)],
                      command=study.history.command(root, folder, m), before=before, after=after,
                      activity=[], started=0, ended=1, exit=0, host=self.host, profiles={},
                      environment={k: v for k, v in study.history.environment(m).items()
                                   if k in study.scale.ENV or k.startswith("PROMETHEUS_METADATA_PIPELINE_")},
                      **study.scale.host_activity(before, after, [], 2))
        for stream in ("stdout", "stderr"):
            record[stream+"_sha256"] = study.scale.sha(folder / (stream+".txt"))
        study.scale.write(folder / "record.json", record)
        return record

    def fixture(self, protected_case=False):
        root = self.directory / "study"
        root.mkdir()
        m = dict(package="tsdb", bench=study.current.PRIMARY, procs=8, benchtime="1s", kind="head", tags="stringlabels")
        minimal = study.manifest() | dict(blocks=[
            dict(id=f"head/{cohort}/{repeat}/0", cohort=cohort,
                 members=[m | dict(arm=arm) for arm in study.ARMS])
            for cohort, repeat in itertools.product((1, 2), range(6))], counts={"head": 60})
        if protected_case:
            minimal["blocks"] += [
                dict(id=f"head/{cohort}/{repeat}/1", cohort=cohort,
                     members=[m | dict(arm=arm, bench=study.history.HEAD_PREFIX+"AppendInMemory/case=stable/mode=off")
                              for arm in study.CONTROLS])
                for cohort, repeat in itertools.product((1, 2), range(6))]
            minimal["counts"] = {"head": 96}
        planned = copy.deepcopy(minimal) | dict(host=self.host, sources={}, hashes={}, inputs={}, fixture_hashes={})
        for arm in study.ARMS:
            source = self.directory / ("source-"+arm)
            source.mkdir()
            (source / "fixture_bench_test.go").write_text("package tsdb\n")
            destination = root / arm
            destination.mkdir()
            with tarfile.open(destination / "production.tar", "w", format=tarfile.PAX_FORMAT,
                              pax_headers={"comment": study.REVISIONS[arm]}) as archive:
                archive.add(source / "fixture_bench_test.go", arcname="fixture_bench_test.go")
            (source / study.FIXTURE).parent.mkdir()
            (source / study.FIXTURE).write_text("package tsdb\n")
            planned["fixture_hashes"] = study.current.fixture_hashes(source)
            inventory = study.scale.inventory(source)
            planned["sources"][arm] = dict(path=str(source), source_hashes=inventory)
            planned["inputs"][arm] = dict(revision=study.REVISIONS[arm], source_hashes=inventory,
                overlay={study.FIXTURE: study.scale.sha(source / study.FIXTURE)},
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
        for i, member in enumerate(study.current.smoke_members(planned)):
            self.record(root, root / "smoke" / str(i), member, planned)
        (root / "SMOKE_COMPLETE").touch()
        for block in planned["blocks"]:
            destination = root / "results" / block["id"]
            destination.mkdir(parents=True)
            attempt = destination / "attempt-0"
            attempt.mkdir()
            warnings = []
            for i, member in enumerate(block["members"]):
                warnings += self.record(root, attempt / str(i), member, planned)["warnings"]
            study.scale.write(attempt / "block.json", dict(block=block, attempt=0, noise=[], warnings=warnings))
            study.scale.write(destination / "accepted.json", dict(attempt=0))
        (root / "MEASUREMENT_COMPLETE").touch()
        self.addCleanup(mock.patch.stopall)
        mock.patch.object(study, "manifest", return_value=minimal).start()
        return root, planned

    def test_relocated_offline_analysis_retains_steal(self):
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
        result = study.assess(summary, [])
        self.assertTrue(result["primary_target_met"])
        self.assertFalse(result["adoption_authorized"])
        with mock.patch.object(study.subprocess, "run"):
            study.analyze(moved)
        self.assertEqual([], json.loads((moved / "analysis/coverage.json").read_text())["exclusions"])

    def test_reject_tampered_source_overlay(self):
        root, planned = self.fixture()
        entry = copy.deepcopy(planned["inputs"]["baseline"])
        entry["source_hashes"]["unexpected.go"] = "modified"
        with self.assertRaisesRegex(AssertionError, "Changes beyond"):
            study.verify_overlay(root / "baseline/production.tar", entry)
        entry["revision"] = "wrong"
        with self.assertRaisesRegex(AssertionError, "Wrong Git archive"):
            study.verify_overlay(root / "baseline/production.tar", entry)

    def test_pairwise_reports_have_identical_configurations(self):
        root, _ = self.fixture(protected_case=True)
        with mock.patch.object(study.subprocess, "run") as run:
            study.analyze(root)
        for call in run.call_args_list:
            _, before, after = call.args[0]
            names = []
            for path in (before, after):
                names.append(Counter(line.split()[0] for line in Path(path).read_text().splitlines()
                                     if line.startswith("Benchmark")))
            self.assertEqual(names[0], names[1])
            self.assertEqual({6}, set(names[0].values()))

    def test_reject_tampered_binary_and_output(self):
        root, planned = self.fixture()
        binary = root / "baseline/tsdb-stringlabels.test"
        saved = binary.read_bytes()
        binary.write_bytes(b"tampered")
        with self.assertRaises(AssertionError):
            study.load(root)
        binary.write_bytes(saved)
        output = root / "results/head/1/0/0/attempt-0/0/stdout.txt"
        output.write_text(output.read_text().replace("100 ns/op", "90 ns/op"))
        with self.assertRaises(AssertionError):
            study.collect(root, planned)

    def test_missing_and_duplicate_work_rejected(self):
        root, planned = self.fixture()
        rows, _ = study.collect(root, planned)
        with self.assertRaises(AssertionError):
            study.paired_summary(rows[:-1])
        with self.assertRaises(AssertionError):
            study.paired_summary(rows + [rows[0]])

    def test_fixed_sizes_not_pooled(self):
        rows = []
        for rounds, cohort, repetition, arm in itertools.product((1024, 4096), (1, 2), range(6), study.CONTROLS):
            member = self.fixed_member(rounds) | dict(arm=arm)
            parsed = study.validate_metrics(member, study.history.metrics_line(self.fixed_output(member), member))
            rows.append(dict(cohort=cohort, block=f"head/{cohort}/{repetition}/{rounds}", member=member, **parsed))
        summary = study.paired_summary(rows)
        self.assertEqual(Counter({"1024x": 2, "4096x": 2}), Counter(r["benchtime"] for r in summary))

    def test_optimizer_cannot_disable_checks(self):
        result = subprocess.run([sys.executable, "-B", "-O", study.__file__], text=True, capture_output=True)
        self.assertNotEqual(0, result.returncode)
        self.assertIn("requires Python without", result.stderr)


if __name__ == "__main__":
    unittest.main()

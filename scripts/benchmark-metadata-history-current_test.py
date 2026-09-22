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
module = importlib.util.spec_from_file_location("current", Path(__file__).with_name("benchmark-metadata-history-current.py"))
study = importlib.util.module_from_spec(module)
module.loader.exec_module(study)


class CurrentValueStudyTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="metadata-current-test-")
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.host = dict(kernel="test", boot="test")

    def record(self, root, folder, member, planned, swap=False):
        folder.mkdir()
        speed = dict(baseline=100, index=101, series=120, coallocated=108)[member["arm"]]
        text = f"goos: linux\ngoarch: amd64\n{study.history.name(member)}-{member['procs']} 1 {speed} ns/op 0 B/op 0 allocs/op 1 ns/sample\nPASS\n"
        (folder / "stdout.txt").write_text(text)
        (folder / "stderr.txt").write_text("")
        before = dict(cpu={"cpu": [0]*8}, swap=dict(pswpin=0, pswpout=0))
        after = dict(cpu={"cpu": [10, 0, 0, 70, 0, 0, 0, 20]}, swap=dict(pswpin=int(swap), pswpout=0))
        env = study.history.environment(member)
        record = dict(member=member, binary_sha256=planned["hashes"][study.history.binary_path(member)],
            command=study.history.command(root, folder, member), before=before, after=after, activity=[],
            environment={k: v for k, v in env.items() if k in study.scale.ENV or k.startswith("PROMETHEUS_METADATA_PIPELINE_")},
            started=0, ended=1, exit=0, host=self.host, profiles={}, **study.scale.host_activity(before, after, [], 2))
        for stream in ("stdout", "stderr"):
            record[stream+"_sha256"] = study.scale.sha(folder / (stream+".txt"))
        study.scale.write(folder / "record.json", record)
        return record

    def fixture(self, rejected=False):
        root = self.directory / "study"
        root.mkdir()
        member = dict(package="tsdb", bench=study.PRIMARY, procs=8, benchtime="1x", kind="head", tags="stringlabels")
        minimal = dict(stage="trial", counts={"head": 48}, blocks=[
            dict(id=f"head/{cohort}/{i}/stringlabels/0", cohort=cohort,
                 members=[member | dict(arm=arm) for arm in study.ARMS])
            for cohort, i in itertools.product((1, 2), range(6))])
        planned = copy.deepcopy(minimal) | dict(host=self.host, sources={}, hashes={})
        for arm in study.ARMS:
            source = self.directory / ("source-"+arm)
            source.mkdir()
            (source / "fixture.go").write_text("Synthetic source " + arm)
            destination = root / arm
            destination.mkdir()
            with tarfile.open(destination / "source.tar.gz", "w:gz") as archive:
                archive.add(source / "fixture.go", arcname="fixture.go")
            binary = destination / "tsdb-stringlabels.test"
            binary.write_text("Synthetic binary; never executed. " + arm)
            planned["sources"][arm] = dict(path=str(source), source_hashes=study.scale.inventory(source))
            for path in (destination / "source.tar.gz", binary):
                planned["hashes"][path.relative_to(root).as_posix()] = study.scale.sha(path)
        for name in study.SCRIPTS:
            shutil.copyfile(Path(study.__file__).with_name(name), root / name)
            planned["hashes"][name] = study.scale.sha(root / name)
        study.scale.write(root / "expected-runs.json", planned)
        (root / "expected-runs.sha256").write_text(study.scale.sha(root / "expected-runs.json")+"\n")
        (root / "smoke").mkdir()
        for i, m in enumerate(study.smoke_members(planned)):
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
        patch = mock.patch.object(study, "manifest", return_value=minimal)
        patch.start()
        self.addCleanup(patch.stop)
        return root, planned

    def test_exact_matrix_and_balanced_order(self):
        profiles, trial = study.manifest("profiles"), study.manifest("trial")
        self.assertEqual(36, sum(profiles["counts"].values()))
        self.assertEqual(6444, sum(trial["counts"].values()))
        self.assertEqual(548, len(study.smoke_members(trial)))
        self.assertEqual(36, len(study.smoke_members(profiles)))
        combined = Counter()
        for cohort in (1, 2):
            positions, before = Counter(), 0
            blocks = [b for b in trial["blocks"] if b["cohort"] == cohort and b["id"].endswith("/stringlabels/0")]
            self.assertEqual(6, len(blocks))
            for b in blocks:
                arms = [m["arm"] for m in b["members"]]
                self.assertEqual(set(study.ARMS), set(arms))
                before += arms.index("coallocated") < arms.index("series")
                positions.update((arm, i) for i, arm in enumerate(arms))
            self.assertEqual(3, before)
            self.assertEqual({1, 2}, set(positions.values()))
            combined.update(positions)
        self.assertEqual({3}, set(combined.values()))
        members = [m for b in trial["blocks"] for m in b["members"]]
        self.assertFalse(any("/mode=dual" in study.history.name(m) for m in members))
        self.assertFalse(any("distinct4096/state=current" in study.history.name(m) for m in members))
        allocations = [m for m in members if m["kind"] == "head-allocation"]
        self.assertEqual({"10000x"}, {m["benchtime"] for m in allocations})
        self.assertEqual({"native"}, {m["mode"] for m in members if m["package"] == "remote"})

    def test_screen_is_exact_stringlabels_projection(self):
        trial, screen = study.manifest("trial"), study.manifest("screen")
        self.assertEqual([b for b in trial["blocks"] if all(m["tags"] == "stringlabels" for m in b["members"])], screen["blocks"])
        self.assertEqual(564, len(screen["blocks"]))
        self.assertEqual(2220, sum(screen["counts"].values()))
        self.assertEqual(196, len(study.smoke_members(screen)))
        for key in trial.keys() - {"stage", "counts", "blocks"}:
            self.assertEqual(trial[key], screen[key])
        for cohort in (1, 2):
            cells = Counter((m["kind"], study.history.name(m), m["procs"], m["arm"])
                            for b in screen["blocks"] if b["cohort"] == cohort for m in b["members"])
            self.assertEqual(46*4, len(cells))
            self.assertEqual({6}, set(cells.values()))

    def test_screen_imports_incomplete_inputs_without_building_or_reusing_results(self):
        parent, prior = self.fixture()
        prior.update(fixture_hashes={}, tool_versions=["go version go1.27.1 linux/amd64\n",
                     "benchstat v0.0.0-20250305200902-02a15fd477ba\n"])
        for arm in study.ARMS:
            for name in ("remote-stringlabels.test", "tsdb-slicelabels.test", "tsdb-dedupelabels.test"):
                binary = parent / arm / name
                binary.write_text("Synthetic binary; never executed.")
                prior["hashes"][f"{arm}/{name}"] = study.scale.sha(binary)
        study.scale.write(parent / "expected-runs.json", prior)
        digest = study.scale.sha(parent / "expected-runs.json")
        (parent / "expected-runs.sha256").write_text(digest+"\n")
        (parent / "MEASUREMENT_COMPLETE").unlink()
        minimal = study.manifest("trial")
        root = self.directory / "screen"
        with mock.patch.object(study, "TRIAL_SHA256", digest), mock.patch.object(study.sys, "platform", "linux"), \
                mock.patch.object(study.scale, "host_identity", return_value=self.host), \
                mock.patch.object(study, "manifest", side_effect=lambda stage: minimal | dict(stage=stage)), \
                mock.patch.object(study.subprocess, "check_output", side_effect=prior["tool_versions"]), \
                mock.patch.object(study.subprocess, "run") as command:
            study.freeze(root, "screen", parent)
            command.assert_not_called()
            self.assertFalse((root / "results").exists())
            self.assertFalse((root / "smoke").exists())
            planned = study.load(root, execution=True)
            self.assertEqual(prior["sources"]["coallocated"]["source_hashes"], planned["sources"]["coallocated"]["source_hashes"])
            self.assertFalse(any(root.glob("*/tsdb-slicelabels.test")))
            self.assertFalse(any(root.glob("*/tsdb-dedupelabels.test")))
            with self.assertRaises(AssertionError):
                study.analyze(root)

            # Synthetic observations exercise offline reproduction, not input import.
            shutil.copytree(parent / "results", root / "results")
            shutil.copytree(parent / "smoke", root / "smoke")
            (root / "SMOKE_COMPLETE").touch()
            (root / "MEASUREMENT_COMPLETE").touch()
            relocated = self.directory / "relocated-screen"
            shutil.copytree(root, relocated)
            shutil.rmtree(parent)
            shutil.rmtree(root)
            for path in self.directory.glob("source-*"):
                shutil.rmtree(path)
            with mock.patch.object(study.scale, "host_identity", side_effect=AssertionError("Offline host access")):
                study.analyze(relocated)
            assessment = json.loads((relocated / "analysis/assessment.json").read_text())
            self.assertTrue(assessment["primary_target_met"])
            self.assertFalse(assessment["adoption_authorized"])
            (relocated / "coallocated/remote-stringlabels.test").unlink()
            with self.assertRaises(FileNotFoundError):
                study.load(relocated)
            del planned["hashes"]["coallocated/remote-stringlabels.test"]
            study.scale.write(relocated / "expected-runs.json", planned)
            (relocated / "expected-runs.sha256").write_text(study.scale.sha(relocated / "expected-runs.json")+"\n")
            with self.assertRaisesRegex(AssertionError, "Incomplete screen artifacts"):
                study.load(relocated)

    def test_complete_coverage_and_steal_are_retained(self):
        root, planned = self.fixture()
        study.load(root)
        study.validate_smoke(root, planned)
        rows, exclusions = study.collect(root, planned)
        self.assertEqual(48, len(rows))
        self.assertFalse(exclusions)
        self.assertTrue(all(r["host"]["warnings"] and not r["host"]["noise"] for r in rows))
        summary = study.paired_summary(rows)
        self.assertEqual([1, 2], [r["cohort"] for r in summary])
        self.assertEqual(.9, summary[0]["metrics"]["ns/op"]["coallocated/series"]["median"])
        self.assertEqual(1.08, summary[0]["metrics"]["ns/op"]["coallocated/baseline"]["median"])
        result = study.assess(summary, [])
        self.assertTrue(result["primary_target_met"])
        self.assertFalse(result["adoption_authorized"])
        with self.assertRaisesRegex(AssertionError, "Duplicate"):
            study.paired_summary(rows+[rows[0]])
        with self.assertRaises(AssertionError):
            study.paired_summary(rows[:-1])
        with self.assertRaisesRegex(AssertionError, "Metric sets differ"):
            study.paired_summary([rows[0] | dict(metrics={"ns/op": 1})] + rows[1:])

    def test_excluded_attempt_is_preserved_not_pooled(self):
        root, planned = self.fixture(rejected=True)
        rows, exclusions = study.collect(root, planned)
        self.assertEqual(48, len(rows))
        self.assertEqual(1, len(exclusions))
        self.assertEqual(["swap activity"], exclusions[0]["reasons"])

    def test_offline_relocation(self):
        root, _ = self.fixture()
        relocated = self.directory / "relocated"
        shutil.copytree(root, relocated)
        for path in self.directory.glob("source-*"):
            shutil.rmtree(path)
        def benchstat(args, stdout, check):
            self.assertEqual("benchstat", args[0])
            self.assertEqual(5, len(args))
            stdout.write("Synthetic comparison.\n")
        with mock.patch.object(study.subprocess, "run", side_effect=benchstat), mock.patch.object(
                study.scale, "host_identity", side_effect=AssertionError("Offline analysis inspected live host")):
            study.analyze(relocated)
        summary = json.loads((relocated / "analysis/summary.json").read_text())
        self.assertEqual(2, len(summary))

    def test_manifest_binary_and_raw_tampering(self):
        root, planned = self.fixture()
        for path in (root / "expected-runs.json", root / "coallocated/tsdb-stringlabels.test"):
            original = path.read_bytes()
            path.write_bytes(original+b"changed")
            with self.assertRaises(AssertionError):
                study.load(root)
            path.write_bytes(original)
        output = root / "results/head/1/0/stringlabels/0/attempt-0/0/stdout.txt"
        output.write_text(output.read_text().replace("100 ns/op", "10 ns/op"))
        with self.assertRaises(AssertionError):
            study.collect(root, planned)

    def test_missing_and_foreign_records(self):
        root, planned = self.fixture()
        record = root / "results/head/1/0/stringlabels/0/attempt-0/0/record.json"
        extra = root / "results/foreign"
        extra.mkdir()
        shutil.copyfile(record, extra / "record.json")
        with self.assertRaises(AssertionError):
            study.collect(root, planned)
        (extra / "record.json").unlink()
        record.unlink()
        with self.assertRaises(FileNotFoundError):
            study.collect(root, planned)

    def test_invalid_measurement_fails(self):
        member = study.profile_cases()[0] | dict(kind="head", arm="series", benchtime="1x")
        line = study.history.name(member)+f"-{member['procs']} 1 10 ns/op 0 B/op 0 allocs/op\n"
        for text in ("PASS\n", "--- SKIP: benchmark\nPASS\n", "goos: linux\ngoarch: amd64\n"+line+line+"PASS\n",
                     "goos: linux\ngoarch: amd64\n"+line.replace("10 ns/op", "NaN ns/op")+"PASS\n"):
            with self.assertRaises(AssertionError):
                study.history.parse_output(text, member)
        with mock.patch.object(study.history, "validate_record", return_value={"metrics": {"ns/op": 1}}):
            with self.assertRaisesRegex(AssertionError, "Missing required"):
                study.validate(None, None, member, None)

    def test_profiles_require_all_files(self):
        root, planned = self.fixture()
        member = planned["blocks"][0]["members"][0] | dict(kind="profile")
        folder = root / "profile-missing"
        self.record(root, folder, member, planned)
        with self.assertRaises(AssertionError):
            study.validate(root, folder, member, planned)

        record = json.loads((folder / "record.json").read_text())
        for name in ("cpu.pprof", "alloc.pprof"):
            (folder / name).write_bytes(b"Synthetic profile; checksum validation only.")
            record["profiles"][name] = study.scale.sha(folder / name)
        study.scale.write(folder / "record.json", record)
        study.validate(root, folder, member, planned)
        (folder / "cpu.pprof").write_bytes(b"Changed profile.")
        with self.assertRaises(AssertionError):
            study.validate(root, folder, member, planned)

    def test_measurements_require_complete_smoke(self):
        root, planned = self.fixture()
        (root / "SMOKE_COMPLETE").unlink()
        with mock.patch.object(study, "load", return_value=planned), mock.patch.object(study.history, "run_one") as run:
            with self.assertRaises(AssertionError):
                study.run(root)
            run.assert_not_called()

    def test_fixture_hashes_include_build_and_benchmark_inputs(self):
        for name in ("go.mod", "go.sum", "go.work", "go.work.sum", "example_bench_test.go",
                     "metadata_pipeline.go", "metadata_pipeline_test.go", "unit_test.go"):
            (self.directory / name).write_text("original")
        before = study.fixture_hashes(self.directory)
        self.assertEqual(7, len(before))
        for name in before:
            (self.directory / name).write_text("changed")
            self.assertNotEqual(before, study.fixture_hashes(self.directory))
            (self.directory / name).write_text("original")
        (self.directory / "unit_test.go").write_text("changed representation test")
        self.assertEqual(before, study.fixture_hashes(self.directory))

    def test_primary_and_allocation_guards(self):
        root, planned = self.fixture()
        rows, _ = study.collect(root, planned)
        summary = study.paired_summary(rows)
        self.assertTrue(study.assess(summary, [])["promising"])
        primary_ratio = summary[0]["metrics"]["ns/op"]["coallocated/series"]
        primary_ratio["maximum"] = 1.01
        self.assertFalse(study.assess(summary, [])["primary_target_met"])
        primary_ratio["maximum"] = .94
        primary_ratio["median"] = .96
        self.assertFalse(study.assess(summary, [])["primary_target_met"])
        primary_ratio["median"] = .9

        # A zero baseline is still a stable-path allocation regression.
        summary[0]["metrics"]["allocs/op"]["medians"]["coallocated"] = 1
        result = study.assess(summary, [])
        self.assertTrue(result["primary_target_met"])
        self.assertFalse(result["promising"])
        self.assertTrue(result["allocation_flags"][0]["controlled_work"])

        parallel = copy.deepcopy(summary[0])
        parallel["bench"] = study.PREFIX + "Lookup/values=shared/state=current/parallel=true"
        summary[0]["metrics"]["allocs/op"]["medians"]["coallocated"] = 0
        # Calibrated worker-buffer allocations are reported, but not scored.
        self.assertTrue(study.assess(summary+[parallel], [])["promising"])
        parallel["kind"] = "head-allocation"
        self.assertFalse(study.assess(summary+[parallel], [])["promising"])

    def test_incremental_heap_and_zero_denominators(self):
        rows = []
        for cohort, repetition, arm, mode in itertools.product((1, 2), range(6), study.ARMS, ("off", "native")):
            overhead = dict(baseline=100, index=120, series=110, coallocated=110)[arm]
            rows.append(dict(cohort=cohort, block=f"head/{cohort}/{repetition}/stringlabels/0",
                member=dict(kind="head-heap", package="tsdb", tags="stringlabels", arm=arm,
                            bench=study.PREFIX+"RetainedHeap/scenario=stable/mode="+mode),
                metrics={"heap-B/series": 500+(overhead if mode == "native" else 0)}))
        heap = study.incremental_heap(rows)
        self.assertEqual([1, 2], [r["cohort"] for r in heap])
        self.assertEqual(1, heap[0]["coallocated/series"]["median"])
        self.assertEqual(1.1, heap[0]["coallocated/baseline"]["median"])
        self.assertIn("unavailable", study.comparison({a: [0]*6 for a in study.ARMS})["coallocated/series"])
        with self.assertRaisesRegex(AssertionError, "Duplicate"):
            study.incremental_heap(rows+[rows[0]])
        with self.assertRaises(AssertionError):
            study.incremental_heap(rows[:-1])

    def test_python_optimization_cannot_disable_validation(self):
        result = subprocess.run([sys.executable, "-B", "-O", study.__file__], capture_output=True, text=True)
        self.assertNotEqual(0, result.returncode)
        self.assertIn("requires Python without", result.stderr)


if __name__ == "__main__":
    unittest.main()

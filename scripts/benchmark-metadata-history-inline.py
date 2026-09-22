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

"""Frozen inline-state experiment. All controls are rebuilt with common fixtures.

freeze ROOT INPUTS_JSON accepts five Git-free sources and their archive provenance.
smoke/run/analyze ROOT execute or independently reproduce the frozen study.
No inputs or observations are imported from an earlier measurement.
"""

from collections import Counter
import hashlib
import importlib.util
import itertools
import json
from pathlib import Path
import shutil
import statistics
import subprocess
import sys
import tarfile
import time

if not __debug__:
    raise RuntimeError("Study validation requires Python without -O/PYTHONOPTIMIZE")
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("current", Path(__file__).with_name("benchmark-metadata-history-current.py"))
current = importlib.util.module_from_spec(spec)
spec.loader.exec_module(current)
history, scale = current.history, current.scale
ARMS = ("baseline", "coallocated", "inline", "cancel", "unlocked")
CONTROLS = ("baseline", "coallocated", "unlocked")
REVISIONS = dict(zip(ARMS, (
    "948f6783afa635f7857b031417e312141520f1a7",
    "61acdce838c6efa116f5d1caaa062a4a2fe7898a",
    "43b5b155592a471f09857ed4d0f373ee2c140ad8",
    "f12fed6f2c40aa4d4a3d84f9724f75091a7f983e",
    "203bbe2edd1ce57d7e1eac650456688fdc7422c0",
)))
ORDERS = (("DEABC", "DCBAE", "ABCDE", "CBAED", "EABCD", "EDCBA"),
          ("BCDEA", "BAEDC", "DEABC", "AEDCB", "CDEAB", "CBAED"))
SCRIPTS = current.SCRIPTS + ("benchmark-metadata-history-inline.py", "benchmark-metadata-history-inline_test.py")
FIXTURE = "tsdb/head_native_metric_metadata_fixed_work_bench_test.go"
FIXED = {
    "fixed-concurrency": (8, 2),
    "lookup-append-zero-destinations": (8, 8),
    "legacy-stable": (1, 2),
}


def verify_overlay(archive_path, entry):
    with tarfile.open(archive_path) as archive:
        assert archive.pax_headers.get("comment") == entry["revision"], "Wrong Git archive revision"
        production = {}
        for member in archive:
            path = Path(member.name)
            assert not path.is_absolute() and ".." not in path.parts
            assert member.isdir() or member.isfile()
            if member.isfile():
                assert member.name not in production
                production[member.name] = hashlib.sha256(archive.extractfile(member).read()).hexdigest()
    assert FIXTURE not in production, "Diagnostic must be a new common fixture"
    assert production | entry["overlay"] == entry["source_hashes"], "Changes beyond the fixture overlay"


def manifest():
    timing, heap = current.selected_cases()
    ablations = {(m["bench"], m["procs"]) for m in current.profile_cases()}
    blocks = []
    for cohort, repetition in itertools.product((1, 2), range(6)):
        five = [ARMS[ord(c)-ord("A")] for c in ORDERS[cohort-1][repetition]]
        orders = list(itertools.permutations(CONTROLS))
        three = orders[repetition if cohort == 1 else 5-repetition]
        allocations = [m | dict(kind="head-allocation", benchtime="10000x") for m in timing
                       if m["bench"].endswith("Lookup/values=shared/state=current/parallel=true")]
        fixed = [dict(package="tsdb", bench=history.HEAD_PREFIX+"AppendFixedWork/case="+case,
                      procs=procs, kind="head-fixed-work", benchtime=f"{rounds}x")
                 for (case, (_, procs)), rounds in itertools.product(FIXED.items(), (1024, 4096))]
        for case, member in enumerate(timing + heap + allocations + fixed):
            order = five if member["kind"] == "head" and (member["bench"], member["procs"]) in ablations else three
            blocks.append(dict(id=f"head/{cohort}/{repetition}/{case}", cohort=cohort,
                               members=[member | dict(tags="stringlabels", arm=arm) for arm in order]))
        pipeline = [m | dict(mode="native") for m in history.pipeline_cases() if
                    (m["group"] == "original" and m["case"] == "cold") or
                    (m["group"] == "backlog" and m["sharing"] == "distinct" and m["series"] == 100000) or
                    m["group"] == "equal-work"]
        assert len(pipeline) == 4
        for case, member in enumerate(pipeline):
            blocks.append(dict(id=f"pipeline/{cohort}/{repetition}/{case}", cohort=cohort,
                               members=[member | dict(arm=arm) for arm in three]))
    for case, member in enumerate(current.profile_cases()):
        for kind in ("profile", "locks"):
            order = ("coallocated", "unlocked") if case % 2 == 0 else ("unlocked", "coallocated")
            blocks.append(dict(id=f"profiles/0/0/{case}-{kind}", cohort=0,
                               members=[member | dict(kind=kind, arm=arm) for arm in order]))
    counts = dict(Counter(m["kind"] for b in blocks for m in b["members"]))
    assert counts == dict(head=936, **{"head-heap": 720, "head-allocation": 72, "head-fixed-work": 216},
                          scored=144, profile=12, locks=12)
    assert len(blocks) == 660 and sum(counts.values()) == 2112
    return dict(schema=1, arms=list(ARMS), revisions=REVISIONS, environment=scale.ENV,
                policy=scale.POLICIES[2], blocks=blocks, counts=counts,
                primary=dict(bench=current.PRIMARY, procs=8, improvement=.05, all_pairs_favorable=True),
                protected_regression=.05, adoption_authorized=False,
                expected_tradeoffs=["legacy-only sidecar growth", "386 native and legacy sidecar growth"])


def freeze(root, inputs_path):
    assert sys.platform == "linux", "Freeze and measurement require Linux"
    inputs = json.loads(inputs_path.read_text())
    assert set(inputs) == set(ARMS)
    root.mkdir()
    planned = manifest() | dict(host=scale.host_identity(), sources={}, hashes={}, inputs=inputs)
    fixtures = None
    for arm in ARMS:
        entry = inputs[arm]
        assert entry["revision"] == REVISIONS[arm]
        source = Path(entry["path"]).resolve()
        assert source != root and source not in root.parents and root not in source.parents
        assert not (source / ".git").exists()
        assert scale.inventory(source) == entry["source_hashes"], "Source changed after preparation"
        assert entry["overlay"] == {FIXTURE: scale.sha(source / FIXTURE)}
        verify_overlay(Path(entry["production_archive"]), entry)
        source_fixtures = current.fixture_hashes(source)
        if fixtures is None:
            fixtures = source_fixtures
        assert source_fixtures == fixtures, "Workloads or dependencies differ"
        destination = root / arm
        destination.mkdir()
        shutil.copy2(entry["production_archive"], destination / "production.tar")
        assert scale.sha(destination / "production.tar") == entry["production_archive_sha256"]
        planned["hashes"][f"{arm}/production.tar"] = entry["production_archive_sha256"]
        subprocess.run(["tar", "-czf", str(destination / "source.tar.gz"), "-C", str(source), "."], check=True)
        planned["sources"][arm] = dict(path=str(source), source_hashes=entry["source_hashes"])
        scale.source_archive(destination, planned["sources"][arm])
        planned["hashes"][f"{arm}/source.tar.gz"] = scale.sha(destination / "source.tar.gz")
        for package in ("tsdb", "remote"):
            binary = destination / f"{package}-stringlabels.test"
            with (destination / f"{package}-build.txt").open("w") as log:
                subprocess.run(["go", "test", "-p=2", "-c", "-tags=stringlabels", "-o", str(binary),
                                "./tsdb" if package == "tsdb" else "./storage/remote"],
                               cwd=source, env=scale.environment(), stdout=log, stderr=subprocess.STDOUT, check=True)
            planned["hashes"][binary.relative_to(root).as_posix()] = scale.sha(binary)
    for filename in SCRIPTS:
        shutil.copy2(Path(__file__).with_name(filename), root / filename)
        planned["hashes"][filename] = scale.sha(root / filename)
    versions = [subprocess.check_output(args, text=True).strip() for args in
                (["go", "version"], ["go", "version", "-m", shutil.which("benchstat")])]
    assert "go1.27.1 linux/amd64" in versions[0]
    assert "v0.0.0-20250305200902-02a15fd477ba" in versions[1]
    planned.update(fixture_hashes=fixtures, tool_versions=versions)
    scale.write(root / "expected-runs.json", planned)
    (root / "expected-runs.sha256").write_text(scale.sha(root / "expected-runs.json")+"\n")
    print("Frozen", planned["counts"], "smoke", len(current.smoke_members(planned)), flush=True)


def load(root, execution=False):
    assert scale.sha(root / "expected-runs.json") == (root / "expected-runs.sha256").read_text().strip()
    planned = json.loads((root / "expected-runs.json").read_text())
    for key, value in manifest().items():
        assert planned[key] == value, key
    required = set(SCRIPTS) | {f"{arm}/{name}" for arm in ARMS for name in
                              ("production.tar", "source.tar.gz", "tsdb-stringlabels.test", "remote-stringlabels.test")}
    assert set(planned["hashes"]) == required and set(planned["sources"]) == set(ARMS)
    assert set(planned["inputs"]) == set(ARMS)
    for filename, digest in planned["hashes"].items():
        assert scale.sha(root / filename) == digest, filename
    for arm, record in planned["sources"].items():
        assert planned["inputs"][arm]["revision"] == REVISIONS[arm]
        assert planned["inputs"][arm]["source_hashes"] == record["source_hashes"]
        assert planned["inputs"][arm]["overlay"] == {FIXTURE: planned["fixture_hashes"][FIXTURE]}
        assert planned["inputs"][arm]["production_archive_sha256"] == planned["hashes"][f"{arm}/production.tar"]
        verify_overlay(root / arm / "production.tar", planned["inputs"][arm])
        scale.source_archive(root / arm, record)
        if execution:
            assert scale.inventory(Path(record["path"])) == record["source_hashes"]
    if execution:
        assert scale.host_identity() == planned["host"], "Host boot/kernel changed"
        for filename in SCRIPTS:
            assert scale.sha(Path(__file__).with_name(filename)) == planned["hashes"][filename]
    return planned


def validate_metrics(member, parsed):
    metrics = parsed["metrics"]
    assert current.required_metrics(member) <= metrics.keys()
    if member["kind"] == "head-fixed-work":
        case = member["bench"].split("/case=")[1]
        workers, procs = FIXED[case]
        assert member["procs"] == procs
        assert {"transactions", "samples", "samples/op", "ns/sample"} <= metrics.keys()
        rounds = int(member["benchtime"][:-1])
        assert metrics["transactions"] == workers*rounds
        assert metrics["samples"] == workers*rounds*1000
        assert metrics["samples/op"] == workers*1000
        metrics["alloc-B/sample"] = metrics["B/op"] / metrics["samples/op"]
        metrics["allocs/sample"] = metrics["allocs/op"] / metrics["samples/op"]
    return parsed


def validate(root, folder, member, planned):
    return validate_metrics(member, history.validate_record(root, folder, member, planned))


def validate_smoke(root, planned):
    assert (root / "SMOKE_COMPLETE").is_file()
    members = current.smoke_members(planned)
    assert {p.name for p in (root / "smoke").iterdir()} == {str(i) for i in range(len(members))}
    for i, member in enumerate(members):
        validate(root, root / "smoke" / str(i), member, planned)


def smoke(root):
    planned = load(root, execution=True)
    (root / "smoke").mkdir()
    members = current.smoke_members(planned)
    for i, member in enumerate(members):
        print("Smoke", i+1, len(members), member["arm"], history.name(member), flush=True)
        history.run_one(root, root / "smoke" / str(i), member, planned)
        validate(root, root / "smoke" / str(i), member, planned)
    (root / "SMOKE_COMPLETE").touch()


def collect(root, planned):
    rows, exclusions = history.collect(root, planned)
    for row in rows:
        validate_metrics(row["member"], row)
    return rows, exclusions


def run(root):
    planned = load(root, execution=True)
    validate_smoke(root, planned)
    (root / "results").mkdir()
    for ordinal, block in enumerate(planned["blocks"]):
        destination = root / "results" / block["id"]
        destination.mkdir(parents=True)
        for attempt in (0, 1):
            folder = destination / f"attempt-{attempt}"
            folder.mkdir()
            noise, warnings = [], []
            for i, member in enumerate(block["members"]):
                progress = dict(time=time.time(), block=ordinal+1, total=len(planned["blocks"]),
                                id=block["id"], attempt=attempt, arm=member["arm"])
                scale.write(root / "progress.json", progress)
                print(time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), progress, flush=True)
                result = history.run_one(root, folder / str(i), member, planned)
                validate(root, folder / str(i), member, planned)
                noise += result["noise"]
                warnings += result["warnings"]
            scale.write(folder / "block.json", dict(block=block, attempt=attempt, noise=noise, warnings=warnings))
            if not noise:
                scale.write(destination / "accepted.json", dict(attempt=attempt))
                break
            if attempt:
                scale.write(root / "INCOMPLETE.json", dict(block=block, reasons=noise))
                raise RuntimeError("Local interference recurred; all attempts preserved")
    collect(root, planned)
    load(root, execution=True)
    (root / "MEASUREMENT_COMPLETE").touch()


def comparison(values):
    result = dict(medians={arm: statistics.median(v) for arm, v in values.items()})
    for numerator, denominator in (("inline", "coallocated"), ("cancel", "inline"), ("unlocked", "cancel"),
                                   ("unlocked", "coallocated"), ("unlocked", "baseline"), ("coallocated", "baseline")):
        if numerator not in values or denominator not in values:
            continue
        if all(v > 0 for v in values[denominator]):
            ratios = [a/b for a, b in zip(values[numerator], values[denominator])]
            result[numerator+"/"+denominator] = dict(median=statistics.median(ratios), minimum=min(ratios), maximum=max(ratios))
        else:
            result[numerator+"/"+denominator] = dict(unavailable="nonpositive denominator")
    return result


def paired_summary(rows):
    cells = {}
    for row in rows:
        if not row["cohort"]:
            continue
        m = row["member"]
        # Fixed-work runs at 1024x and 4096x must never be pooled.
        key = (row["cohort"], m["kind"], m["package"], m["tags"], m["procs"], history.name(m), m["benchtime"])
        arm = cells.setdefault(key, {}).setdefault(m["arm"], {})
        assert row["block"] not in arm
        arm[row["block"]] = row["metrics"]
    result = []
    for key, arms in sorted(cells.items()):
        assert set(arms) in (set(ARMS), set(CONTROLS))
        blocks = sorted(arms["baseline"])
        assert len(blocks) == 6 and all(set(a) == set(blocks) for a in arms.values())
        units = set(arms["baseline"][blocks[0]])
        assert all(set(m) == units for arm in arms.values() for m in arm.values())
        metrics = {unit: comparison({arm: [measurements[b][unit] for b in blocks] for arm, measurements in arms.items()})
                   for unit in sorted(units)}
        result.append(dict(cohort=key[0], kind=key[1], package=key[2], tags=key[3],
                           procs=key[4], bench=key[5], benchtime=key[6], metrics=metrics))
    return result


def incremental_heap(rows):
    cells = {}
    for row in rows:
        m = row["member"]
        name = history.name(m)
        if m["kind"] != "head-heap" or "WithoutMetadata" in name or name.endswith("/mode=legacy"):
            continue
        bench, mode = name.rsplit("/mode=", 1)
        repetition = int(row["block"].split("/")[2])
        cell = cells.setdefault((row["cohort"], bench), {}).setdefault(m["arm"], {}).setdefault(mode, {})
        assert repetition not in cell
        cell[repetition] = row["metrics"]["heap-B/series"]
    result = []
    for (cohort, bench), arms in sorted(cells.items()):
        assert set(arms) == set(CONTROLS)
        values = {}
        for arm, modes in arms.items():
            assert set(modes) == {"off", "native"}
            assert set(modes["off"]) == set(modes["native"]) == set(range(6))
            values[arm] = [modes["native"][i]-modes["off"][i] for i in range(6)]
        result.append(dict(cohort=cohort, bench=bench, **comparison(values)))
    return result


def assess(summary, heap):
    primary, flags, allocations = [], [], []
    for row in summary:
        label = {k: v for k, v in row.items() if k != "metrics"}
        if row["kind"] == "head" and row["bench"] == current.PRIMARY and row["procs"] == 8:
            ratio = row["metrics"]["ns/op"]["unlocked/coallocated"]
            primary.append(label | ratio | dict(passes=ratio["median"] <= .95 and ratio["maximum"] < 1))
        units = ("ns/op",) if row["kind"] == "head" else ("cpu-ns/sample",) if row["kind"] == "scored" else (
            ("heap-B/series",) if row["kind"] == "head-heap" else ())
        for unit in units:
            ratio = row["metrics"][unit]["unlocked/coallocated"]
            if "median" not in ratio or ratio["median"] > 1.05:
                flags.append(label | dict(metric=unit, comparison=ratio,
                    expected_tradeoff=row["kind"] == "head-heap" and row["bench"].endswith("/mode=legacy")
                    and "WithoutMetadata" not in row["bench"]))
        stable = row["kind"] in ("head-fixed-work", "head-allocation") or any(
            part in row["bench"] for part in ("/case=stable/", "/state=current/", "/changing=false/", "/change=0/"))
        if stable:
            for unit in ("B/op", "allocs/op"):
                medians = row["metrics"][unit]["medians"]
                if medians["unlocked"] > medians["coallocated"]:
                    allocations.append(label | dict(metric=unit, values=medians,
                        equal_iterations=row["kind"] in ("head-fixed-work", "head-allocation")))
    assert {row["cohort"] for row in primary} == {1, 2}
    heap_flags = [r for r in heap if "median" not in r["unlocked/coallocated"] or r["unlocked/coallocated"]["median"] > 1.05]
    return dict(primary=primary, primary_target_met=all(r["passes"] for r in primary),
                protected_flags=flags, incremental_heap_flags=heap_flags, allocation_flags=allocations,
                adoption_authorized=False, original_gate_verdict="Unchanged; preserve every historical flag and verdict.")


def analyze(root):
    planned = load(root)
    assert (root / "MEASUREMENT_COMPLETE").is_file()
    validate_smoke(root, planned)
    rows, exclusions = collect(root, planned)
    output = root / "analysis"
    output.mkdir(exist_ok=True)
    scale.write(output / "coverage.json", dict(counts=planned["counts"], exclusions=exclusions,
        warnings=[dict(block=r["block"], member=r["member"], warnings=r["host"]["warnings"])
                  for r in rows if r["host"]["warnings"]]))
    scale.write(output / "observations.json", rows)
    summary, heap = paired_summary(rows), incremental_heap(rows)
    scale.write(output / "summary.json", summary)
    scale.write(output / "incremental-heap.json", heap)
    scale.write(output / "assessment.json", assess(summary, heap))
    # Separate files prevent benchstat from pooling distinct iteration counts.
    groups = {(r["cohort"], r["member"]["kind"], r["member"]["benchtime"]) for r in rows if r["cohort"]}
    for cohort, kind, duration in sorted(groups):
        selected = [r for r in rows if (r["cohort"], r["member"]["kind"], r["member"]["benchtime"]) == (cohort, kind, duration)]
        files = {}
        for arm in ARMS:
            lines = [r["line"] for r in selected if r["member"]["arm"] == arm]
            if not lines:
                continue
            path = output / f"cohort-{cohort}-{kind}-{duration}-{arm}.txt"
            path.write_text("goos: linux\ngoarch: amd64\n"+"\n".join(lines)+"\n")
            files[arm] = path
        for numerator, denominator in (("inline", "coallocated"), ("cancel", "inline"), ("unlocked", "cancel"),
                                       ("unlocked", "coallocated"), ("unlocked", "baseline"), ("coallocated", "baseline")):
            if numerator not in files or denominator not in files:
                continue
            # The intermediate arms cover only six configurations. Match rows
            # explicitly rather than emitting unpaired control-only benchmarks.
            names = {arm: {(history.name(r["member"]), r["member"]["procs"]) for r in selected
                           if r["member"]["arm"] == arm} for arm in (numerator, denominator)}
            shared = names[numerator] & names[denominator]
            assert shared
            prefix = f"cohort-{cohort}-{kind}-{duration}-{numerator}-vs-{denominator}"
            pair = []
            for arm in (denominator, numerator):
                path = output / f"{prefix}-{arm}.txt"
                lines = [r["line"] for r in selected if r["member"]["arm"] == arm and
                         (history.name(r["member"]), r["member"]["procs"]) in shared]
                path.write_text("goos: linux\ngoarch: amd64\n"+"\n".join(lines)+"\n")
                pair.append(str(path))
            with (output / f"{prefix}.benchstat.txt").open("w") as report:
                subprocess.run(["benchstat", *pair], stdout=report, check=True)
    print("Complete; independent cohorts, fixed-work sizes and diagnostic profiles remain separate.")


if __name__ == "__main__":
    action, root = sys.argv[1], Path(sys.argv[2]).resolve()
    if action == "freeze":
        freeze(root, Path(sys.argv[3]).resolve())
    else:
        {"smoke": smoke, "run": run, "analyze": analyze}[action](root)

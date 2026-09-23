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

"""Frozen B/E/F layout comparison, separate from every historical measurement.

freeze ROOT INPUTS_JSON builds exact Git archives with a common fixed-work fixture.
smoke/run/analyze ROOT validate, measure, or reproduce the frozen study.
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
spec = importlib.util.spec_from_file_location("inline", Path(__file__).with_name("benchmark-metadata-history-inline.py"))
inline = importlib.util.module_from_spec(spec)
spec.loader.exec_module(inline)
current, history, scale = inline.current, inline.history, inline.scale
ARMS = ("coallocated", "inline", "candidate")
REVISIONS = dict(zip(ARMS, (
    "61acdce838c6efa116f5d1caaa062a4a2fe7898a",
    "203bbe2edd1ce57d7e1eac650456688fdc7422c0",
    "f511970f08bcefbd7f33a9483db08c03d4ba7b14",
)))
PAIRS = (("candidate", "coallocated"), ("candidate", "inline"), ("inline", "coallocated"))
SCRIPTS = inline.SCRIPTS + ("benchmark-metadata-history-layout.py", "benchmark-metadata-history-layout_test.py",
                           "benchmark-metadata-history_test.py", "benchmark-metadata-pipeline-scale_test.py")
FIXTURE = inline.FIXTURE


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
    # The candidate already contains this fixture. Controls may only add it,
    # never replace existing files or silently normalize different fixtures.
    expected = {} if FIXTURE in production else {FIXTURE: entry["source_hashes"][FIXTURE]}
    assert entry["overlay"] == expected, "Unexpected overlay"
    assert production | expected == entry["source_hashes"], "Changes beyond the fixture overlay"


def manifest():
    prefix = history.HEAD_PREFIX
    wanted = {
        ("AppendFixedConcurrency/change=0/mode=native", 2), ("AppendFixedConcurrency/change=0/mode=native", 8),
        ("AppendFixedConcurrency/change=100/mode=native", 8), ("AppendSparseChangesInMemory/mode=native", 2),
        ("AppendInMemory/case=stable/mode=off", 2), ("AppendInMemory/case=stable/mode=legacy", 2),
        ("Lookup/values=shared/state=current/parallel=false", 2), ("Lookup/values=shared/state=current/parallel=true", 8),
        ("Lookup/values=shared/state=historical-full/parallel=false", 2), ("Lookup/values=shared/state=missing-full/parallel=false", 2),
        ("LookupAppendConcurrent/lookup=current/changing=false/destinations=1", 2),
        ("LookupAppendConcurrent/lookup=current/changing=false/destinations=1", 8),
        ("LookupAppendConcurrent/lookup=current/changing=false/destinations=2", 2),
        ("Query/versions=5/every=100/limit=10", 2),
    }
    timing, heap = current.selected_cases()
    timing = [m for m in timing if (m["bench"].removeprefix(prefix), m["procs"]) in wanted]
    assert {(m["bench"].removeprefix(prefix), m["procs"]) for m in timing} == wanted
    heap = [m for m in heap if m["bench"].startswith(("BenchmarkHeadSeriesWithoutMetadataRetainedHeap/",
            prefix+"RetainedHeap/scenario=stable/", prefix+"RetainedHeap/scenario=observed-versions=5/"))]
    assert len(heap) == 8
    fixed = [dict(package="tsdb", bench=prefix+"AppendFixedWork/case="+case, procs=inline.FIXED[case][1],
                  kind="head-fixed-work", benchtime=f"{n}x") for case, n in (
                      ("fixed-concurrency", 1024), ("fixed-concurrency", 4096),
                      ("lookup-append-zero-destinations", 4096), ("legacy-stable", 4096))]
    pipeline = [m | dict(mode="native") for m in history.pipeline_cases() if m["group"] == "original" and m["case"] == "cold"]
    assert len(pipeline) == 1
    blocks, orders = [], list(itertools.permutations(ARMS))
    for cohort, repetition in itertools.product((1, 2), range(6)):
        order = orders[repetition if cohort == 1 else 5-repetition]
        for case, member in enumerate(timing + heap + fixed + pipeline):
            blocks.append(dict(id=f"scored/{cohort}/{repetition}/{case}", cohort=cohort,
                               members=[member | dict(tags="stringlabels", arm=arm) for arm in order]))
    profiles = [m for m in current.profile_cases() if (m["bench"] == current.PRIMARY and m["procs"] == 8) or
                (m["bench"] == prefix+"Lookup/values=shared/state=current/parallel=false" and m["procs"] == 2)]
    assert len(profiles) == 2
    for case, member in enumerate(profiles):
        for kind in ("profile", "locks"):
            order = ("inline", "candidate") if case == 0 else ("candidate", "inline")
            blocks.append(dict(id=f"profiles/0/0/{case}-{kind}", cohort=0,
                               members=[member | dict(arm=arm, kind=kind) for arm in order]))
    counts = dict(Counter(m["kind"] for b in blocks for m in b["members"]))
    assert counts == dict(head=504, **{"head-heap": 288, "head-fixed-work": 144}, scored=36, profile=4, locks=4)
    assert len(blocks) == 328 and sum(counts.values()) == 980
    return dict(schema=1, arms=list(ARMS), revisions=REVISIONS, blocks=blocks, counts=counts,
                environment=scale.ENV, policy=scale.POLICIES[2], adoption_authorized=False,
                primary=dict(bench=current.PRIMARY, procs=8, improvement=.05, all_pairs_favorable=True),
                protected_regression=.05,
                expected_tradeoffs=["candidate native heap grows versus inline", "candidate legacy heap shrinks versus inline"])


def freeze(root, inputs_path):
    assert sys.platform == "linux", "Freeze and measurement require Linux"
    inputs = json.loads(inputs_path.read_text())
    assert set(inputs) == set(ARMS)
    root.mkdir()
    planned = manifest() | dict(host=scale.host_identity(), inputs=inputs, sources={}, hashes={})
    fixtures = None
    for arm in ARMS:
        entry = inputs[arm]
        assert entry["revision"] == REVISIONS[arm]
        source = Path(entry["path"]).resolve()
        assert source != root and source not in root.parents and root not in source.parents
        assert not (source / ".git").exists()
        assert scale.inventory(source) == entry["source_hashes"], "Source changed after preparation"
        verify_overlay(Path(entry["production_archive"]), entry)
        source_fixtures = current.fixture_hashes(source)
        if fixtures is None:
            fixtures = source_fixtures
        assert source_fixtures == fixtures, "Workloads or dependencies differ"
        destination = root / arm
        destination.mkdir()
        shutil.copy2(entry["production_archive"], destination / "production.tar")
        assert scale.sha(destination / "production.tar") == entry["production_archive_sha256"]
        subprocess.run(["tar", "-czf", str(destination / "source.tar.gz"), "-C", str(source), "."], check=True)
        planned["sources"][arm] = dict(path=str(source), source_hashes=entry["source_hashes"])
        scale.source_archive(destination, planned["sources"][arm])
        for package in ("tsdb", "remote"):
            binary = destination / f"{package}-stringlabels.test"
            with (destination / f"{package}-build.txt").open("w") as log:
                subprocess.run(["go", "test", "-p=2", "-c", "-tags=stringlabels", "-o", str(binary),
                                "./tsdb" if package == "tsdb" else "./storage/remote"], cwd=source,
                               env=scale.environment(), stdout=log, stderr=subprocess.STDOUT, check=True)
        for name in ("production.tar", "source.tar.gz", "tsdb-stringlabels.test", "remote-stringlabels.test"):
            planned["hashes"][f"{arm}/{name}"] = scale.sha(destination / name)
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
    load(root, execution=True)
    print("Frozen", counts_text(planned), flush=True)


def counts_text(planned):
    return dict(blocks=len(planned["blocks"]), observations=sum(planned["counts"].values()),
                smoke=len(current.smoke_members(planned)))


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
        entry = planned["inputs"][arm]
        assert entry["revision"] == REVISIONS[arm] and entry["source_hashes"] == record["source_hashes"]
        assert entry["production_archive_sha256"] == planned["hashes"][f"{arm}/production.tar"]
        verify_overlay(root / arm / "production.tar", entry)
        scale.source_archive(root / arm, record)
        assert record["source_hashes"][FIXTURE] == planned["fixture_hashes"][FIXTURE]
        assert {p: digest for p, digest in record["source_hashes"].items() if p.endswith("_bench_test.go") or
                Path(p).name.startswith("metadata_pipeline") and p.endswith(".go") or
                Path(p).name in ("go.mod", "go.sum", "go.work", "go.work.sum")} == planned["fixture_hashes"]
        if execution:
            assert scale.inventory(Path(record["path"])) == record["source_hashes"]
    if execution:
        assert scale.host_identity() == planned["host"], "Host boot/kernel changed"
        for filename in SCRIPTS:
            assert scale.sha(Path(__file__).with_name(filename)) == planned["hashes"][filename]
    return planned


def validate(root, folder, member, planned):
    return inline.validate_metrics(member, history.validate_record(root, folder, member, planned))


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
        inline.validate_metrics(row["member"], row)
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
    assert set(values) == set(ARMS) and all(len(v) == 6 for v in values.values())
    result = dict(medians={arm: statistics.median(v) for arm, v in values.items()})
    for numerator, denominator in PAIRS:
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
        key = (row["cohort"], m["kind"], m["package"], m["tags"], m["procs"], history.name(m), m["benchtime"])
        arm = cells.setdefault(key, {}).setdefault(m["arm"], {})
        assert row["block"] not in arm, "Duplicate observation"
        arm[row["block"]] = row["metrics"]
    result = []
    for key, arms in sorted(cells.items()):
        assert set(arms) == set(ARMS)
        blocks = sorted(arms["coallocated"])
        assert len(blocks) == 6 and all(set(a) == set(blocks) for a in arms.values())
        units = set(arms["coallocated"][blocks[0]])
        assert all(set(m) == units for arm in arms.values() for m in arm.values())
        metrics = {unit: comparison({arm: [measurements[b][unit] for b in blocks] for arm, measurements in arms.items()})
                   for unit in sorted(units)}
        result.append(dict(cohort=key[0], kind=key[1], package=key[2], tags=key[3], procs=key[4],
                           bench=key[5], benchtime=key[6], metrics=metrics))
    return result


def incremental_heap(rows):
    cells = {}
    for row in rows:
        m, name = row["member"], history.name(row["member"])
        if m["kind"] != "head-heap" or "WithoutMetadata" in name or name.endswith("/mode=legacy"):
            continue
        bench, mode = name.rsplit("/mode=", 1)
        repetition = int(row["block"].split("/")[2])
        cell = cells.setdefault((row["cohort"], bench), {}).setdefault(m["arm"], {}).setdefault(mode, {})
        assert repetition not in cell
        cell[repetition] = row["metrics"]["heap-B/series"]
    result = []
    for (cohort, bench), arms in sorted(cells.items()):
        assert set(arms) == set(ARMS)
        values = {}
        for arm, modes in arms.items():
            assert set(modes) == {"off", "native"} and all(set(v) == set(range(6)) for v in modes.values())
            values[arm] = [modes["native"][i]-modes["off"][i] for i in range(6)]
        result.append(dict(cohort=cohort, bench=bench, **comparison(values)))
    return result


def assess(summary, heap):
    primary, flags, allocations = [], [], []
    for row in summary:
        label = {k: v for k, v in row.items() if k != "metrics"}
        if row["kind"] == "head" and row["bench"] == current.PRIMARY and row["procs"] == 8:
            ratio = row["metrics"]["ns/op"]["candidate/coallocated"]
            primary.append(label | ratio | dict(passes=ratio["median"] <= .95 and ratio["maximum"] < 1))
        units = ("ns/op",) if row["kind"] == "head" else ("cpu-ns/sample",) if row["kind"] == "scored" else (
            ("heap-B/series",) if row["kind"] == "head-heap" else ())
        for base in ("coallocated", "inline"):
            for unit in units:
                ratio = row["metrics"][unit]["candidate/"+base]
                if "median" not in ratio or ratio["median"] > 1.05:
                    expected = base == "inline" and unit == "heap-B/series" and row["bench"].endswith("/mode=native") and "WithoutMetadata" not in row["bench"]
                    flags.append(label | dict(metric=unit, baseline=base, comparison=ratio, expected_tradeoff=expected))
            stable = row["kind"] == "head-fixed-work" or any(part in row["bench"] for part in
                        ("/case=stable/", "/state=current/", "/changing=false/", "/change=0/"))
            if stable:
                for unit in ("B/op", "allocs/op"):
                    medians = row["metrics"][unit]["medians"]
                    if medians["candidate"] > medians[base]:
                        allocations.append(label | dict(metric=unit, baseline=base, values=medians,
                                                        equal_iterations=row["kind"] == "head-fixed-work"))
    assert {r["cohort"] for r in primary} == {1, 2}
    heap_flags = [dict(cohort=r["cohort"], bench=r["bench"], baseline=base, comparison=r["candidate/"+base],
                       expected_tradeoff=base == "inline") for r in heap for base in ("coallocated", "inline")
                  if "median" not in r["candidate/"+base] or r["candidate/"+base]["median"] > 1.05]
    return dict(primary=primary, primary_target_met=all(r["passes"] for r in primary), protected_flags=flags,
                incremental_heap_flags=heap_flags, allocation_flags=allocations, adoption_authorized=False,
                original_gate_verdict="Unchanged; preserve every historical flag and verdict.")


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
    groups = {(r["cohort"], r["member"]["kind"], r["member"]["benchtime"]) for r in rows if r["cohort"]}
    for cohort, kind, duration in sorted(groups):
        selected = [r for r in rows if (r["cohort"], r["member"]["kind"], r["member"]["benchtime"]) == (cohort, kind, duration)]
        files = {}
        for arm in ARMS:
            lines = [r["line"] for r in selected if r["member"]["arm"] == arm]
            path = output / f"cohort-{cohort}-{kind}-{duration}-{arm}.txt"
            path.write_text("goos: linux\ngoarch: amd64\n"+"\n".join(lines)+"\n")
            files[arm] = path.name
        for numerator, denominator in PAIRS:
            path = output / f"cohort-{cohort}-{kind}-{duration}-{numerator}-vs-{denominator}.benchstat.txt"
            with path.open("w") as report:
                subprocess.run(["benchstat", files[denominator], files[numerator]], cwd=output, stdout=report, check=True)
    print("Complete; B/E/F cohorts, fixed-work sizes and diagnostic profiles remain separate.")


if __name__ == "__main__":
    action, root = sys.argv[1], Path(sys.argv[2]).resolve()
    if action == "freeze":
        freeze(root, Path(sys.argv[3]).resolve())
    else:
        {"smoke": smoke, "run": run, "analyze": analyze}[action](root)

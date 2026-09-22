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

"""Frozen current-value experiment; never modifies the parent study.

freeze-profiles ROOT COMPLETED_HISTORY_STUDY imports the three frozen controls.
freeze-trial ROOT COMPLETED_PROFILES CANDIDATE_SOURCE adds the coallocation arm.
freeze-screen ROOT FROZEN_TRIAL imports inputs for a fresh stringlabels screen.
smoke/run/analyze ROOT validate, measure, or reproduce that stage independently.
"""

from collections import Counter
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
module = importlib.util.spec_from_file_location("history", Path(__file__).with_name("benchmark-metadata-history.py"))
history = importlib.util.module_from_spec(module)
module.loader.exec_module(history)
scale = history.scale
ARMS = ("baseline", "index", "series", "coallocated")
PARENT_SHA256 = "15b1bc49421076d7c7c936461b7bca2fd81272fd6ea0d499a39f0822eb47f279"
TRIAL_SHA256 = "4bca24dda7c356ae8986798e718f4c43167cc074ba9a323ea71b42644b346717"
PREFIX = history.HEAD_PREFIX
PRIMARY = PREFIX + "LookupAppendConcurrent/lookup=current/changing=false/destinations=1"
SCRIPTS = ("benchmark-metadata-history-current.py", "benchmark-metadata-history-current_test.py",
           "benchmark-metadata-history.py", "benchmark-metadata-pipeline-scale.py")
# Each cohort splits series/coallocated order 3/3; all positions balance over both.
ORDERS = (("ABDC", "BCAD", "CDBA", "DACB", "ABCD", "DCBA"),
          ("ABDC", "BCAD", "CDBA", "DACB", "BADC", "CDAB"))


def selected_cases():
    timing, heap = [], []
    for member in history.head_cases():
        name = member["bench"].removeprefix(PREFIX)
        selected = (name.startswith("LookupAppendConcurrent/lookup=current/changing=false/") or
                    name.startswith("Lookup/values=shared/state=current/") or name in (
                        "AppendFixedConcurrency/change=0/mode=native", "AppendFixedConcurrency/change=100/mode=native",
                        "AppendInMemory/case=stable/mode=off", "AppendInMemory/case=stable/mode=legacy",
                        "AppendSparseChangesInMemory/mode=off", "AppendSparseChangesInMemory/mode=native",
                        "Lookup/values=shared/state=historical-full/parallel=false",
                        "Lookup/values=shared/state=missing-full/parallel=false",
                        "Query/versions=1/every=100/limit=10", "Query/versions=5/every=100/limit=10",
                        "SeriesChurn/mode=native"))
        if selected and member["kind"] == "head":
            timing.append(member)
        if member["kind"] == "head-heap" and (
            ((name.startswith("RetainedHeap/") or name.startswith("CollapsedHistoryRetainedHeap/")) and
             name.endswith(("/mode=off", "/mode=native"))) or name == "RetainedHeap/scenario=stable/mode=legacy" or
                member["bench"].startswith("BenchmarkHeadSeriesWithoutMetadataRetainedHeap/")):
            heap.append(member)
    assert len(timing) == 22 and len(heap) == 20
    return timing, heap


def profile_cases():
    timing, _ = selected_cases()
    names = {PRIMARY, PREFIX + "Lookup/values=shared/state=current/parallel=false",
             PREFIX + "AppendFixedConcurrency/change=0/mode=native", PREFIX + "AppendSparseChangesInMemory/mode=native"}
    result = [m | dict(tags="stringlabels", benchtime="30s") for m in timing if m["bench"] in names]
    assert len(result) == 6
    return result


def manifest(stage):
    assert stage in ("profiles", "trial", "screen")
    if stage == "screen":
        planned = manifest("trial")
        blocks = [b for b in planned["blocks"] if all(m["tags"] == "stringlabels" for m in b["members"])]
        counts = dict(Counter(m["kind"] for b in blocks for m in b["members"]))
        assert counts == dict(head=1056, **{"head-heap": 960, "head-allocation": 96}, scored=96, profile=6, locks=6)
        return planned | dict(stage=stage, blocks=blocks, counts=counts)
    blocks = []
    if stage == "trial":
        timing, heap = selected_cases()
        allocations = [m | dict(kind="head-allocation", benchtime="10000x") for m in timing
                       if m["bench"].endswith("Lookup/values=shared/state=current/parallel=true")]
        pipeline = [m | dict(mode="native") for m in history.pipeline_cases()
                    if m["group"] == "original" and m["case"] == "cold" or
                    m["group"] == "backlog" and m["sharing"] == "distinct" and m["series"] == 100000]
        assert len(allocations) == len(pipeline) == 2
        for cohort, repetition in itertools.product((1, 2), range(6)):
            order = [ARMS[ord(c)-ord("A")] for c in ORDERS[cohort-1][repetition]]
            for tag in history.TAGS:
                for case, member in enumerate(timing + heap + allocations):
                    blocks.append(dict(id=f"head/{cohort}/{repetition}/{tag}/{case}", cohort=cohort,
                                       members=[member | dict(tags=tag, arm=arm) for arm in order]))
            for case, member in enumerate(pipeline):
                blocks.append(dict(id=f"pipeline/{cohort}/{repetition}/{case}", cohort=cohort,
                                   members=[member | dict(arm=arm) for arm in order]))
    arms = ARMS[:3] if stage == "profiles" else ARMS[3:]
    for case, member in enumerate(profile_cases()):
        for kind in ("profile", "locks"):
            blocks.append(dict(id=f"profiles/0/0/{case}-{kind}", cohort=0,
                               members=[member | dict(kind=kind, arm=arm) for arm in arms]))
    counts = dict(Counter(m["kind"] for b in blocks for m in b["members"]))
    expected = dict(profile=18, locks=18) if stage == "profiles" else dict(
        head=3168, **{"head-heap": 2880, "head-allocation": 288}, scored=96, profile=6, locks=6)
    assert counts == expected, counts
    return dict(schema=1, stage=stage, arms=list(ARMS[:3] if stage == "profiles" else ARMS),
                environment=scale.ENV, policy=scale.POLICIES[2], blocks=blocks, counts=counts,
                primary=dict(bench=PRIMARY, tags="stringlabels", procs=8, improvement=.05, all_pairs_favorable=True),
                protected_regression=.05, adoption_authorized=False)


def smoke_members(planned):
    result = {}
    for block in planned["blocks"]:
        for m in block["members"]:
            member = m | dict(benchtime="1x")
            result.setdefault(json.dumps(member, sort_keys=True), member)
    return list(result.values())


def fixture_hashes(source):
    return {p.relative_to(source).as_posix(): scale.sha(p) for p in source.rglob("*") if p.is_file() and
            (p.name.endswith("_bench_test.go") or p.name.startswith("metadata_pipeline") and p.suffix == ".go" or
             p.name in ("go.mod", "go.sum", "go.work", "go.work.sum"))}


def restore_source(root, arm, expected):
    scale.source_archive(root / arm, expected)
    destination = root / "sources" / arm
    destination.mkdir(parents=True)
    with tarfile.open(root / arm / "source.tar.gz") as archive:
        for member in archive:
            path = Path(member.name)
            assert (member.isdir() or member.isfile()) and not path.is_absolute() and ".." not in path.parts
        archive.extractall(destination, filter="data")
    assert scale.inventory(destination) == expected["source_hashes"]
    return destination


def import_arm(root, arm, parent, prior, planned, stringlabels_only=False):
    destination = root / arm
    destination.mkdir()
    for name in ("source.tar.gz", "tsdb-stringlabels.test", "tsdb-slicelabels.test", "tsdb-dedupelabels.test", "remote-stringlabels.test"):
        if stringlabels_only and name in ("tsdb-slicelabels.test", "tsdb-dedupelabels.test"):
            continue
        relative = f"{arm}/{name}"
        assert scale.sha(parent / relative) == prior["hashes"][relative], relative
        shutil.copy2(parent / relative, destination / name)
        assert scale.sha(destination / name) == prior["hashes"][relative]
        planned["hashes"][relative] = prior["hashes"][relative]
    source = restore_source(root, arm, prior["sources"][arm])
    planned["sources"][arm] = dict(path=str(source), source_hashes=prior["sources"][arm]["source_hashes"])


def freeze(root, stage, parent, candidate=None):
    assert sys.platform == "linux", "Freeze and measurement require Linux"
    assert stage in ("profiles", "trial", "screen")
    assert (candidate is not None) == (stage == "trial"), "Only freeze-trial takes a candidate source"
    root, parent = root.resolve(), parent.resolve()
    assert not root.exists() and root != parent and parent not in root.parents
    if stage == "profiles":
        assert scale.sha(parent / "expected-runs.json") == PARENT_SHA256
        assert (parent / "MEASUREMENT_COMPLETE").is_file()
        prior = json.loads((parent / "expected-runs.json").read_text())
    elif stage == "screen":
        assert scale.sha(parent / "expected-runs.json") == TRIAL_SHA256, "Unexpected trial inputs"
        # Import frozen inputs, not observations: the superseded trial may be incomplete.
        prior = load(parent)
        assert prior["stage"] == "trial"
        assert scale.host_identity() == prior["host"], "Host boot/kernel changed"
    else:
        prior = load(parent)
        assert prior["stage"] == "profiles" and (parent / "MEASUREMENT_COMPLETE").is_file()
        collect(parent, prior)
        candidate = candidate.resolve()
        assert candidate != root and candidate not in root.parents and root not in candidate.parents
    root.mkdir()
    planned = manifest(stage) | dict(host=scale.host_identity(), sources={}, hashes={},
                                    parent=dict(path=str(parent), manifest_sha256=scale.sha(parent / "expected-runs.json")))
    imported = ARMS if stage == "screen" else ARMS[:3]
    for arm in imported:
        import_arm(root, arm, parent, prior, planned, stringlabels_only=stage == "screen")
    reference = fixture_hashes(Path(planned["sources"]["baseline"]["path"]))
    for arm in imported:
        assert fixture_hashes(Path(planned["sources"][arm]["path"])) == reference
    if stage == "screen":
        assert reference == prior["fixture_hashes"]
        name = "parent-expected-runs.json"
        shutil.copyfile(parent / "expected-runs.json", root / name)
        planned["hashes"][name] = TRIAL_SHA256
    if candidate is not None:
        arm = "coallocated"
        assert not (candidate / ".git").exists(), "Use a Git-free candidate snapshot"
        assert fixture_hashes(candidate) == reference, "Candidate benchmark fixtures/dependencies differ"
        destination = root / arm
        destination.mkdir()
        inventory = scale.inventory(candidate)
        subprocess.run(["tar", "-czf", str(destination / "source.tar.gz"), "-C", str(candidate), "."], check=True)
        expected = dict(source_hashes=inventory)
        source = restore_source(root, arm, expected)
        planned["sources"][arm] = dict(path=str(source), **expected)
        planned["hashes"][f"{arm}/source.tar.gz"] = scale.sha(destination / "source.tar.gz")
        for package, tag in [("remote", "stringlabels")] + [("tsdb", tag) for tag in history.TAGS]:
            target = destination / f"{package}-{tag}.test"
            with (destination / f"{package}-{tag}-build.txt").open("w") as output:
                subprocess.run(["go", "test", "-p=2", "-c", "-tags="+tag, "-o", str(target),
                                "./tsdb" if package == "tsdb" else "./storage/remote"],
                               cwd=source, env=scale.environment(), stdout=output, stderr=subprocess.STDOUT, check=True)
            planned["hashes"][f"{arm}/{target.name}"] = scale.sha(target)
    for name in SCRIPTS:
        shutil.copyfile(Path(__file__).with_name(name), root / name)
        planned["hashes"][name] = scale.sha(root / name)
    versions = [subprocess.check_output(command, text=True, env=scale.environment()) for command in (
        ["go", "version"], ["go", "version", "-m", shutil.which("benchstat")])]
    assert "go1.27.1 linux/amd64" in versions[0]
    assert "v0.0.0-20250305200902-02a15fd477ba" in versions[1]
    if stage == "screen":
        assert versions == prior["tool_versions"], "Toolchain changed"
    planned.update(fixture_hashes=reference, tool_versions=versions)
    scale.write(root / "expected-runs.json", planned)
    (root / "expected-runs.sha256").write_text(scale.sha(root / "expected-runs.json")+"\n")
    print("Frozen", stage, planned["counts"], "smoke", len(smoke_members(planned)), flush=True)


def load(root, execution=False):
    assert scale.sha(root / "expected-runs.json") == (root / "expected-runs.sha256").read_text().strip()
    planned = json.loads((root / "expected-runs.json").read_text())
    for key, value in manifest(planned["stage"]).items():
        assert planned[key] == value, key
    if planned["stage"] == "screen":
        required = set(SCRIPTS) | {"parent-expected-runs.json"} | {
            f"{arm}/{name}" for arm in ARMS for name in ("source.tar.gz", "tsdb-stringlabels.test", "remote-stringlabels.test")}
        assert set(planned["hashes"]) == required, "Incomplete screen artifacts"
        assert set(planned["sources"]) == set(ARMS), "Incomplete screen sources"
        assert planned["hashes"]["parent-expected-runs.json"] == TRIAL_SHA256
        assert planned["parent"]["manifest_sha256"] == TRIAL_SHA256
    for name, digest in planned["hashes"].items():
        assert scale.sha(root / name) == digest, name
    for arm, record in planned["sources"].items():
        scale.source_archive(root / arm, record)
        if execution:
            assert scale.inventory(Path(record["path"])) == record["source_hashes"]
    if execution:
        assert scale.host_identity() == planned["host"], "Host boot/kernel changed"
        for name in SCRIPTS:
            assert scale.sha(Path(__file__).with_name(name)) == planned["hashes"][name], name
    return planned


def required_metrics(m):
    result = {"ns/op", "B/op", "allocs/op"}
    if m["kind"] == "head-heap":
        result |= {"heap-B/series", "heap-objects/series"}
    if "LookupAppendConcurrent/" in history.name(m) or "AppendFixedConcurrency/" in history.name(m):
        result.add("ns/sample")
    return result


def validate(root, folder, member, planned):
    result = history.validate_record(root, folder, member, planned)
    assert required_metrics(member) <= result["metrics"].keys(), "Missing required metric"
    return result


def validate_smoke(root, planned):
    assert (root / "SMOKE_COMPLETE").is_file()
    members = smoke_members(planned)
    assert {p.name for p in (root / "smoke").iterdir()} == {str(i) for i in range(len(members))}
    for i, member in enumerate(members):
        validate(root, root / "smoke" / str(i), member, planned)


def smoke(root):
    planned = load(root, execution=True)
    (root / "smoke").mkdir()
    members = smoke_members(planned)
    for i, member in enumerate(members):
        print("Smoke", i+1, len(members), member["arm"], history.name(member), flush=True)
        history.run_one(root, root / "smoke" / str(i), member, planned)
        validate(root, root / "smoke" / str(i), member, planned)
    (root / "SMOKE_COMPLETE").touch()


def collect(root, planned):
    rows, exclusions = history.collect(root, planned)
    for row in rows:
        assert required_metrics(row["member"]) <= row["metrics"].keys(), "Missing required metric"
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
                print(time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), ordinal+1, len(planned["blocks"]),
                      block["id"], attempt, member["arm"], flush=True)
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
    for numerator, denominator in (("coallocated", "series"), ("coallocated", "baseline"),
                                   ("series", "baseline"), ("index", "baseline")):
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
        key = (row["cohort"], m["kind"], m["package"], m["tags"], m["procs"], history.name(m))
        arm = cells.setdefault(key, {}).setdefault(m["arm"], {})
        assert row["block"] not in arm, "Duplicate observation"
        arm[row["block"]] = row["metrics"]
    result = []
    for key, arms in sorted(cells.items()):
        assert set(arms) == set(ARMS)
        blocks = sorted(arms["baseline"])
        assert len(blocks) == 6 and all(set(arm) == set(blocks) for arm in arms.values())
        units = set(arms["baseline"][blocks[0]])
        assert all(set(metrics) == units for arm in arms.values() for metrics in arm.values()), "Metric sets differ"
        metrics = {unit: comparison({arm: [arms[arm][b][unit] for b in blocks] for arm in ARMS}) for unit in units}
        result.append(dict(cohort=key[0], kind=key[1], package=key[2], tags=key[3], procs=key[4], bench=key[5], metrics=metrics))
    return result


def incremental_heap(rows):
    cells = {}
    for row in rows:
        m = row["member"]
        name = history.name(m)
        if m["kind"] != "head-heap" or "WithoutMetadata" in name or name.endswith("/mode=legacy"):
            continue
        bench, mode = name.rsplit("/mode=", 1)
        cell = cells.setdefault((row["cohort"], m["tags"], bench), {}).setdefault(m["arm"], {}).setdefault(mode, {})
        repetition = int(row["block"].split("/")[2])
        assert repetition not in cell, "Duplicate heap observation"
        cell[repetition] = row["metrics"]["heap-B/series"]
    result = []
    for key, arms in sorted(cells.items()):
        assert set(arms) == set(ARMS)
        values = {}
        for arm, modes in arms.items():
            assert set(modes) == {"off", "native"}
            assert set(modes["off"]) == set(modes["native"]) == set(range(6))
            values[arm] = [modes["native"][i]-modes["off"][i] for i in range(6)]
        result.append(dict(cohort=key[0], tags=key[1], bench=key[2], **comparison(values)))
    return result


def assess(summary, heap):
    primary, flags, allocation_flags = [], [], []
    for row in summary:
        label = {k: v for k, v in row.items() if k != "metrics"}
        if row["kind"] == "head" and row["bench"] == PRIMARY and row["tags"] == "stringlabels" and row["procs"] == 8:
            ratio = row["metrics"]["ns/op"]["coallocated/series"]
            primary.append(label | ratio | dict(passes=ratio["median"] <= .95 and ratio["maximum"] < 1))
        units = ("ns/op",) if row["kind"] == "head" else ("cpu-ns/sample",) if row["kind"] == "scored" else (
            ("heap-B/series",) if row["kind"] == "head-heap" else ())
        for unit in units:
            ratio = row["metrics"][unit]["coallocated/series"]
            if "median" not in ratio or ratio["median"] > 1.05:
                flags.append(label | dict(metric=unit, comparison=ratio))
        stable = ("/case=stable/" in row["bench"] or "/state=current/" in row["bench"] or
                  "/changing=false/" in row["bench"] or "/change=0/" in row["bench"])
        if stable:
            for unit in ("B/op", "allocs/op"):
                values = row["metrics"][unit]["medians"]
                if values["coallocated"] > values["series"]:
                    allocation_flags.append(label | dict(metric=unit, values=values,
                        controlled_work=row["kind"] == "head-allocation" or "/parallel=true" not in row["bench"]))
    assert {row["cohort"] for row in primary} == {1, 2}, "Missing primary replication"
    heap_flags = [row for row in heap if "median" not in row["coallocated/series"] or row["coallocated/series"]["median"] > 1.05]
    return dict(primary=primary, protected_flags=flags, incremental_heap_flags=heap_flags,
                allocation_flags=allocation_flags, primary_target_met=all(row["passes"] for row in primary),
                promising=all(row["passes"] for row in primary) and not flags and not heap_flags and
                    not any(row["controlled_work"] for row in allocation_flags),
                adoption_authorized=False, original_gate_verdict="Unchanged; this is a focused comparison, not full adoption validation.")


def analyze(root):
    planned = load(root)
    assert (root / "MEASUREMENT_COMPLETE").is_file()
    validate_smoke(root, planned)
    rows, exclusions = collect(root, planned)
    output = root / "analysis"
    output.mkdir(exist_ok=True)
    scale.write(output / "coverage.json", dict(counts=planned["counts"], exclusions=exclusions,
        warnings=[dict(block=r["block"], member=r["member"], warnings=r["host"]["warnings"]) for r in rows if r["host"]["warnings"]]))
    scale.write(output / "observations.json", rows)
    if planned["stage"] == "profiles":
        print("Complete profile controls; not scored performance observations.")
        return
    summary, heap = paired_summary(rows), incremental_heap(rows)
    scale.write(output / "summary.json", summary)
    scale.write(output / "incremental-heap.json", heap)
    scale.write(output / "assessment.json", assess(summary, heap))
    for cohort, kind, tags in sorted({(r["cohort"], r["member"]["kind"], r["member"]["tags"]) for r in rows if r["cohort"]}):
        selected = [r for r in rows if (r["cohort"], r["member"]["kind"], r["member"]["tags"]) == (cohort, kind, tags)]
        paths = []
        for arm in ARMS:
            path = output / f"cohort-{cohort}-{kind}-{tags}-{arm}.txt"
            path.write_text("goos: linux\ngoarch: amd64\n"+"\n".join(r["line"] for r in selected if r["member"]["arm"] == arm)+"\n")
            paths.append(str(path))
        with (output / f"cohort-{cohort}-{kind}-{tags}.benchstat.txt").open("w") as report:
            subprocess.run(["benchstat", *paths], stdout=report, check=True)
    print("Complete; cohorts, fixed-work allocations, heap and profiles remain separate.")


if __name__ == "__main__":
    action, root = sys.argv[1], Path(sys.argv[2]).resolve()
    if action.startswith("freeze-"):
        freeze(root, action.removeprefix("freeze-"), Path(sys.argv[3]), Path(sys.argv[4]) if len(sys.argv) == 5 else None)
    else:
        {"smoke": smoke, "run": run, "analyze": analyze}[action](root)

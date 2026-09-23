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

"""Independent B/F allocation diagnosis and direct A/F forwarding comparison.

freeze ROOT INPUTS_JSON builds exact archives with add-only benchmark overlays.
smoke/run/analyze ROOT validate, measure, or reproduce the frozen study.
"""

from collections import Counter
import hashlib
import importlib.util
import itertools
import json
import math
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
spec = importlib.util.spec_from_file_location("layout", Path(__file__).with_name("benchmark-metadata-history-layout.py"))
layout = importlib.util.module_from_spec(spec)
spec.loader.exec_module(layout)
current, history, scale = layout.current, layout.history, layout.scale
REVISIONS = dict(forwarding="948f6783afa635f7857b031417e312141520f1a7",
                 coallocated="61acdce838c6efa116f5d1caaa062a4a2fe7898a",
                 candidate="f511970f08bcefbd7f33a9483db08c03d4ba7b14")
PAIRS = dict(diagnosis=("coallocated", "candidate"), forwarding=("forwarding", "candidate"))
FIXTURES = (layout.FIXTURE, "tsdb/head_native_metric_metadata_lookup_fixed_work_bench_test.go")
FIXED = history.HEAD_PREFIX + "LookupAppendFixedWork/scheduling="
SCRIPTS = tuple(dict.fromkeys(layout.SCRIPTS + ("benchmark-metadata-history-forwarding.py",
                                               "benchmark-metadata-history-forwarding_test.py")))


def manifest():
    selected = [b["members"][0] for b in layout.manifest()["blocks"]
                if b["cohort"] == 1 and b["id"].split("/")[2] == "0"]
    head = [{k: v for k, v in m.items() if k != "arm"} for m in selected if m["kind"] in ("head", "head-heap")]
    assert Counter(m["kind"] for m in head) == {"head": 14, "head-heap": 8}
    original = next(m for m in head if m["bench"] == current.PRIMARY and m["procs"] == 2)
    diagnosis = [original | dict(benchtime=duration) for duration in ("1s", "8192x", "32768x")]
    diagnosis += [dict(package="tsdb", bench=FIXED+scheduling, procs=2, kind="head-work-distribution", benchtime=f"{n}x")
                  for scheduling, n in itertools.product(("shared-budget", "balanced"), (8192, 32768))]
    pipeline = [m | dict(mode="native") for m in history.pipeline_cases() if
                (m["group"] == "original" and m["case"] == "cold") or
                (m["group"] == "backlog" and m["sharing"] == "distinct" and m["series"] == 100000) or
                m["group"] == "equal-work"]
    assert len(diagnosis) == 7 and len(pipeline) == 4
    stages = dict(diagnosis=diagnosis, forwarding=head+pipeline)
    blocks = []
    for stage, cases in stages.items():
        for cohort, repetition in itertools.product((1, 2), range(6)):
            order = PAIRS[stage] if (repetition+cohort) % 2 else tuple(reversed(PAIRS[stage]))
            for case, member in enumerate(cases):
                blocks.append(dict(id=f"{stage}/{cohort}/{repetition}/{case}", cohort=cohort,
                                   members=[member | dict(stage=stage, tags="stringlabels", arm=arm) for arm in order]))
    captures = dict(diagnosis=[m for m in diagnosis if m["benchtime"] == "32768x"],
                    forwarding=[m for m in current.profile_cases() if
                                (m["bench"] == current.PRIMARY and m["procs"] == 8) or
                                (m["bench"] == history.HEAD_PREFIX+"Lookup/values=shared/state=current/parallel=false" and m["procs"] == 2)])
    for stage, cases in captures.items():
        for case, member in enumerate(cases):
            for kind in ("profile", "locks"):
                order = PAIRS[stage] if case % 2 == 0 else tuple(reversed(PAIRS[stage]))
                blocks.append(dict(id=f"{stage}/0/profiles/{case}-{kind}", cohort=0,
                                   members=[member | dict(stage=stage, tags="stringlabels", arm=arm, kind=kind) for arm in order]))
    counts = dict(Counter(m["kind"] for b in blocks for m in b["members"]))
    assert counts == dict(head=408, **{"head-heap": 192, "head-work-distribution": 96}, scored=96, profile=10, locks=10)
    assert len(blocks) == 406 and sum(counts.values()) == 812
    return dict(schema=1, revisions=REVISIONS, pairs=PAIRS, blocks=blocks, counts=counts,
                environment=scale.ENV, policy=scale.POLICIES[2], protected_regression=.05,
                primary=dict(stage="forwarding", bench=current.PRIMARY, procs=8),
                interpretation="No inherited B-relative improvement gate; no automatic adoption or parity verdict.")


def packages(arm):
    return ("tsdb",) if arm == "coallocated" else ("tsdb", "remote")


def verify_overlay(archive_path, entry):
    production = {}
    with tarfile.open(archive_path) as archive:
        assert archive.pax_headers.get("comment") == entry["revision"], "Wrong Git archive revision"
        for member in archive:
            path = Path(member.name)
            assert not path.is_absolute() and ".." not in path.parts
            assert member.isdir() or member.isfile()
            if member.isfile():
                assert member.name not in production
                production[member.name] = hashlib.sha256(archive.extractfile(member).read()).hexdigest()
    expected = {p: entry["source_hashes"][p] for p in FIXTURES if p not in production}
    assert entry["overlay"] == expected, "Unexpected overlay"
    assert production | expected == entry["source_hashes"], "Changes beyond benchmark additions"


def freeze(root, inputs_path):
    assert sys.platform == "linux", "Freeze and measurement require Linux"
    inputs = json.loads(inputs_path.read_text())
    assert set(inputs) == set(REVISIONS)
    root.mkdir()
    planned = manifest() | dict(host=scale.host_identity(), inputs=inputs, sources={}, hashes={})
    fixtures = None
    for arm, revision in REVISIONS.items():
        entry = inputs[arm]
        assert entry["revision"] == revision
        source = Path(entry["path"]).resolve()
        assert source != root and source not in root.parents and root not in source.parents
        assert not (source / ".git").exists()
        assert scale.inventory(source) == entry["source_hashes"]
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
        for package in packages(arm):
            binary = destination / f"{package}-stringlabels.test"
            with (destination / f"{package}-build.txt").open("w") as log:
                subprocess.run(["go", "test", "-p=2", "-c", "-tags=stringlabels", "-o", str(binary),
                                "./tsdb" if package == "tsdb" else "./storage/remote"], cwd=source,
                               env=scale.environment(), stdout=log, stderr=subprocess.STDOUT, check=True)
        for name in ("production.tar", "source.tar.gz") + tuple(f"{p}-stringlabels.test" for p in packages(arm)):
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
    print("Frozen", planned["counts"], "smoke", len(current.smoke_members(planned)), flush=True)


def load(root, execution=False):
    assert scale.sha(root / "expected-runs.json") == (root / "expected-runs.sha256").read_text().strip()
    planned = json.loads((root / "expected-runs.json").read_text())
    for key, value in json.loads(json.dumps(manifest())).items():
        assert planned[key] == value, key
    required = set(SCRIPTS) | {f"{arm}/{name}" for arm in REVISIONS for name in
                              ("production.tar", "source.tar.gz") + tuple(f"{p}-stringlabels.test" for p in packages(arm))}
    assert set(planned["hashes"]) == required and set(planned["sources"]) == set(REVISIONS)
    assert set(planned["inputs"]) == set(REVISIONS)
    for filename, digest in planned["hashes"].items():
        assert scale.sha(root / filename) == digest, filename
    for arm, record in planned["sources"].items():
        entry = planned["inputs"][arm]
        assert entry["revision"] == REVISIONS[arm] and entry["source_hashes"] == record["source_hashes"]
        assert entry["production_archive_sha256"] == planned["hashes"][f"{arm}/production.tar"]
        verify_overlay(root / arm / "production.tar", entry)
        scale.source_archive(root / arm, record)
        assert {p: digest for p, digest in record["source_hashes"].items() if p.endswith("_bench_test.go") or
                Path(p).name.startswith("metadata_pipeline") and p.endswith(".go") or
                Path(p).name in ("go.mod", "go.sum", "go.work", "go.work.sum")} == planned["fixture_hashes"]
        assert all(p in planned["fixture_hashes"] for p in FIXTURES)
        if execution:
            assert scale.inventory(Path(record["path"])) == record["source_hashes"]
    if execution:
        assert scale.host_identity() == planned["host"], "Host boot/kernel changed"
        for filename in SCRIPTS:
            assert scale.sha(Path(__file__).with_name(filename)) == planned["hashes"][filename]
    return planned


def validate_metrics(member, parsed):
    assert member["stage"] in PAIRS and member["arm"] in PAIRS[member["stage"]]
    metrics = parsed["metrics"]
    assert current.required_metrics(member) <= metrics.keys()
    name = history.name(member)
    if name.startswith(FIXED):
        assert member["benchtime"].endswith("x")
        transactions = int(member["benchtime"][:-1])
        assert {"transactions", "acknowledgments", "samples", "samples/op", "ns/sample", "GC-cycles", "GC-pause-ns"} <= metrics.keys()
        assert metrics["transactions"] == metrics["acknowledgments"] == transactions
        assert metrics["samples"] == transactions*1000 and metrics["samples/op"] == 1000
        workers = [metrics[f"worker{i}-transactions"] for i in range(8)]
        assert all(n == int(n) and 0 <= n <= transactions for n in workers) and sum(workers) == transactions
        if name.endswith("=balanced"):
            assert workers == [transactions//8 + (i < transactions % 8) for i in range(8)]
        metrics["alloc-B/sample"] = metrics["B/op"] / 1000
        metrics["allocs/sample"] = metrics["allocs/op"] / 1000
    elif name == current.PRIMARY:
        metrics["alloc-B/sample"] = metrics["B/op"] / 1000
        metrics["allocs/sample"] = metrics["allocs/op"] / 1000
    if member["package"] == "remote":
        r = parsed["result"]
        for field in ("Ingestion", "Initialization", "Drain", "Shutdown", "Completion", "BacklogWait", "ReleaseToDrain"):
            metrics[field+"-ms"] = r[field] / 1e6
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
        print("Smoke", i+1, len(members), member["stage"], member["arm"], history.name(member), flush=True)
        history.run_one(root, root / "smoke" / str(i), member, planned)
        validate(root, root / "smoke" / str(i), member, planned)
    (root / "SMOKE_COMPLETE").touch()


def collect(root, planned):
    rows, exclusions = history.collect(root, planned)
    for row in rows:
        assert row["block"].split("/")[0] == row["member"]["stage"]
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


def comparison(values, pair):
    baseline, candidate = pair
    assert set(values) == set(pair) and all(len(v) == 6 for v in values.values())
    result = dict(medians={arm: statistics.median(v) for arm, v in values.items()})
    deltas = [b-a for a, b in zip(values[baseline], values[candidate])]
    result["absolute_delta"] = dict(median=statistics.median(deltas), minimum=min(deltas), maximum=max(deltas))
    if all(v > 0 for v in values[baseline]):
        ratios = [b/a for a, b in zip(values[baseline], values[candidate])]
        result["ratio"] = dict(median=statistics.median(ratios), minimum=min(ratios), maximum=max(ratios))
    else:
        result["ratio"] = dict(unavailable="nonpositive baseline; use absolute differences")
    return result


def paired_summary(rows):
    cells = {}
    for row in rows:
        if not row["cohort"]:
            continue
        m = row["member"]
        assert row["block"].split("/")[0] == m["stage"]
        assert m["arm"] in PAIRS[m["stage"]]
        key = (m["stage"], row["cohort"], m["kind"], m["package"], m["tags"], m["procs"], history.name(m), m["benchtime"])
        arm = cells.setdefault(key, {}).setdefault(m["arm"], {})
        assert row["block"] not in arm, "Duplicate observation"
        arm[row["block"]] = row["metrics"]
    result = []
    for key, arms in sorted(cells.items()):
        pair = PAIRS[key[0]]
        assert set(arms) == set(pair)
        blocks = sorted(arms[pair[0]])
        assert len(blocks) == 6 and all(set(a) == set(blocks) for a in arms.values())
        units = set(arms[pair[0]][blocks[0]])
        assert all(set(m) == units for arm in arms.values() for m in arm.values())
        metrics = {unit: comparison({arm: [measurements[b][unit] for b in blocks] for arm, measurements in arms.items()}, pair)
                   for unit in sorted(units)}
        result.append(dict(stage=key[0], cohort=key[1], kind=key[2], package=key[3], tags=key[4], procs=key[5],
                           bench=key[6], benchtime=key[7], metrics=metrics))
    return result


def incremental_heap(rows):
    cells = {}
    for row in rows:
        m, name = row["member"], history.name(row["member"])
        if m["kind"] != "head-heap" or "WithoutMetadata" in name or name.endswith("/mode=legacy"):
            continue
        assert m["stage"] == "forwarding"
        bench, mode = name.rsplit("/mode=", 1)
        repetition = int(row["block"].split("/")[2])
        cell = cells.setdefault((row["cohort"], bench), {}).setdefault(m["arm"], {}).setdefault(mode, {})
        assert repetition not in cell
        cell[repetition] = row["metrics"]["heap-B/series"]
    result = []
    for (cohort, bench), arms in sorted(cells.items()):
        assert set(arms) == set(PAIRS["forwarding"])
        values = {}
        for arm, modes in arms.items():
            assert set(modes) == {"off", "native"} and all(set(v) == set(range(6)) for v in modes.values())
            values[arm] = [modes["native"][i]-modes["off"][i] for i in range(6)]
        result.append(dict(stage="forwarding", cohort=cohort, bench=bench, **comparison(values, PAIRS["forwarding"])))
    return result


def assess(summary):
    flags, allocations = [], []
    for row in summary:
        label = {k: v for k, v in row.items() if k != "metrics"}
        units = ("ns/op",) if row["kind"] == "head" else ("cpu-ns/sample",) if row["kind"] == "scored" else (
            ("heap-B/series",) if row["kind"] == "head-heap" else ())
        for unit in units:
            value = row["metrics"][unit]
            ratio = value["ratio"]
            if ("median" in ratio and ratio["median"] > 1.05) or (
                    "median" not in ratio and value["absolute_delta"]["median"] > 0):
                flags.append(label | dict(metric=unit, comparison=value))
        for unit in ("B/op", "allocs/op"):
            value = row["metrics"][unit]
            baseline, candidate = PAIRS[row["stage"]]
            if value["medians"][candidate] > value["medians"][baseline]:
                allocations.append(label | dict(metric=unit, comparison=value))
    return dict(protected_flags=flags, allocation_flags=allocations,
                interpretation="Descriptive flags, not automatic acceptance/rejection; cohorts and historical warnings stay separate.")


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
    summary = paired_summary(rows)
    scale.write(output / "summary.json", summary)
    scale.write(output / "incremental-heap.json", incremental_heap(rows))
    scale.write(output / "assessment.json", assess(summary))
    groups = {(r["member"]["stage"], r["cohort"], r["member"]["kind"], r["member"]["benchtime"]) for r in rows if r["cohort"]}
    for stage, cohort, kind, duration in sorted(groups):
        selected = [r for r in rows if (r["member"]["stage"], r["cohort"], r["member"]["kind"], r["member"]["benchtime"]) == (stage, cohort, kind, duration)]
        files = []
        for arm in PAIRS[stage]:
            lines = []
            for row in selected:
                if row["member"]["arm"] != arm:
                    continue
                parts = row["line"].split()
                # Keep original benchmark output immutable; render validated derived metrics too.
                line = " ".join(parts[:2]) + " " + " ".join(f"{value:.12g} {unit}" for unit, value in sorted(row["metrics"].items()))
                assert all(math.isfinite(value) for value in row["metrics"].values())
                lines.append(line)
            path = output / f"{stage}-cohort-{cohort}-{kind}-{duration}-{arm}.txt"
            path.write_text("goos: linux\ngoarch: amd64\n"+"\n".join(lines)+"\n")
            files.append(path.name)
        with (output / f"{stage}-cohort-{cohort}-{kind}-{duration}.benchstat.txt").open("w") as report:
            subprocess.run(["benchstat", *files], cwd=output, stdout=report, check=True)
    print("Complete; B/F diagnosis and A/F comparison remain separate, with no automatic promotion.")


if __name__ == "__main__":
    action, root = sys.argv[1], Path(sys.argv[2]).resolve()
    if action == "freeze":
        freeze(root, Path(sys.argv[3]).resolve())
    else:
        {"smoke": smoke, "run": run, "analyze": analyze}[action](root)

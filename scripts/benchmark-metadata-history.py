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

"""Frozen three-arm Linux comparison; see metadata_history_experiment.md.

freeze RESULTS BASELINE INDEX_CONTROL CANDIDATE builds and freezes all inputs.
smoke RESULTS validates one fresh-process observation of every case/build/arm.
run RESULTS collects the complete matrix; analyze RESULTS works offline.
The existing scale runner remains unchanged, including its archived evidence.
"""

import importlib.util
import itertools
import json
import math
import os
from pathlib import Path
import re
import shlex
import shutil
import signal
import statistics
import subprocess
import sys
import tarfile
import time

sys.dont_write_bytecode = True
module = importlib.util.spec_from_file_location("scale", Path(__file__).with_name("benchmark-metadata-pipeline-scale.py"))
scale = importlib.util.module_from_spec(module)
module.loader.exec_module(scale)
ARMS = ("baseline", "index", "series")
TAGS = ("stringlabels", "slicelabels", "dedupelabels")
ORDERS = list(itertools.permutations(ARMS))
MODES = ("native", "wal", "disabled")
MODE_ORDERS = list(itertools.permutations(MODES))
BASELINE = "948f6783afa635f7857b031417e312141520f1a7"
GATES = dict(primary_improvement=.05, protected_regression=.05, retained_heap_regression=.05,
             primary_all_pairs_favor_candidate=True, stable_allocations_must_not_increase=True,
             disabled_series_layout_must_not_grow=True)
HEAD_PREFIX = "BenchmarkHeadMetricMetadata"


def head_cases():
    """Explicit existing fixtures; a missing/renamed subcase is a hard failure."""
    cases = []
    def add(suffix, tail="", heap=False):
        name = HEAD_PREFIX + suffix + tail
        procs = (2, 8) if re.search("Concurrent|Concurrency|parallel=true", name) else (2,)
        duration = "1x" if heap else ("20x" if suffix in ("LargeTransactions", "SeriesChurn") else "1s")
        for cpu in procs:
            cases.append(dict(package="tsdb", bench=name, procs=cpu, benchtime=duration, kind="head-heap" if heap else "head"))
    modes = ("off", "legacy", "native")
    for suffix, case, mode in itertools.product(("Append", "AppendInMemory"),
            ("stable", "stable-fresh-strings", "stable-at-cap", "stable-grouped", "stable-unique", "changing-at-cap"), modes):
        add(suffix, f"/case={case}/mode={mode}")
    for case, mode in itertools.product(("stable", "stable-unique"), ("off", "native")):
        add("AppendConcurrent", f"/case={case}/mode={mode}")
    for change, mode in itertools.product((0, 1, 100), ("off", "native")):
        add("AppendFixedConcurrency", f"/change={change}/mode={mode}")
    for mode in modes:
        # This fixture has no legacy-only case; dual-mode measurements are excluded.
        if mode != "legacy":
            add("AppendSparseChangesInMemory", f"/mode={mode}")
        add("SeriesChurn", f"/mode={mode}")
        add("CollapsedHistoryRetainedHeap", f"/mode={mode}", heap=True)
        cases.append(dict(package="tsdb", bench=f"BenchmarkHeadSeriesWithoutMetadataRetainedHeap/mode={mode}",
                          procs=2, benchtime="1x", kind="head-heap"))
    for count, mode in itertools.product((16000, 33000, 100000), ("off", "native")):
        add("LargeTransactions", f"/series={count}/mode={mode}")
    for scenario, mode in itertools.product(
            ("stable", "stable-unique", "observed-versions=2", "observed-versions=3",
             "observed-versions=4", "observed-versions=5", "observed-versions=6"), modes):
        add("RetainedHeap", f"/scenario={scenario}/mode={mode}", heap=True)
    for case in ("shared-grouped", "shared-round-robin", "single-series", "unique"):
        cases.append(dict(package="tsdb", bench=f"BenchmarkNativeMetricMetadataPendingHeap/{case}",
                          procs=2, benchtime="1x", kind="head-heap"))
    for values in ("shared", "distinct1000", "distinct4096", "oversized"):
        states = ["current", "historical", "missing", "disabled"]
        if values in ("shared", "distinct4096"):
            states.append("historical-full")
        if values == "shared":
            states.append("missing-full")
        for state, parallel in itertools.product(states, ("false", "true")):
            add("Lookup", f"/values={values}/state={state}/parallel={parallel}")
    for size in (10000, 1, 64, 256, 257):
        for versions, every, limit in itertools.product((1, 5), (0, 1, 100), (10, 0)):
            if (every == 0 and (versions != 1 or limit != 10)) or (size == 1 and every == 100):
                continue
            suffix = "Query" if size == 10000 else "QuerySmall"
            tail = "" if size == 10000 else f"/series={size}"
            add(suffix, tail + f"/versions={versions}/every={every}/limit={limit}")
    for lookup, changing, destinations in itertools.product(
            ("current", "historical", "oldest"), ("false", "true"), (0, 1, 2)):
        if (lookup != "current" and destinations == 0) or (lookup == "oldest" and (changing == "false" or destinations != 2)):
            continue
        add("LookupAppendConcurrent", f"/lookup={lookup}/changing={changing}/destinations={destinations}")
    for every, changing in itertools.product((1, 100), ("false", "true")):
        add("QueryAppendConcurrent", f"/every={every}/changing={changing}")
    assert len({(c["bench"], c["procs"]) for c in cases}) == len(cases)
    return cases


def pipeline_cases():
    result = {}
    for block in scale.manifest()["blocks"]:
        for m in block["members"]:
            if m["kind"] == "scored":
                key = (m["group"], m["case"], m["sharing"], m["series"])
                result[key] = m | dict(package="remote", procs=4, tags="stringlabels", benchtime="1x")
    for case in ("cold", "newseries"):
        result[("original", case, "shared", 10000)] = dict(
            group="original", case=case, sharing="shared", series=10000, mode="native", kind="scored",
            receiver=2, package="remote", procs=4, tags="stringlabels", benchtime="1x")
    assert len(result) == 16
    return list(result.values())


def name(m):
    if m["package"] == "tsdb":
        return m["bench"]
    if m["group"] == "original":
        return f"BenchmarkRemoteWriteMetadataPipeline/case={m['case']}/series=10000/source={m['mode']}"
    return scale.name(m)


def manifest():
    blocks = []
    for cohort in (1, 2):
        for repetition in range(6):
            for index, case in enumerate(pipeline_cases()):
                members = []
                for arm_index, arm in enumerate(ORDERS[(repetition+cohort+index) % 6]):
                    for mode in MODE_ORDERS[(repetition+cohort+index+arm_index) % 6]:
                        members.append(case | dict(arm=arm, mode=mode))
                blocks.append(dict(id=f"pipeline/{cohort}/{repetition}/{index}", cohort=cohort, members=members))
    for cohort in (1, 2):
        for repetition in range(6):
            for tags in TAGS:
                for index, case in enumerate(head_cases()):
                    members = [case | dict(arm=arm, tags=tags) for arm in ORDERS[(repetition+cohort+index) % 6]]
                    blocks.append(dict(id=f"head/{cohort}/{repetition}/{tags}/{index}", cohort=cohort, members=members))
    # Repeat the existing scale heap/capacity diagnostics for each arm.
    for original in scale.manifest()["blocks"]:
        if original["members"][0]["kind"] not in ("heap", "capacity"):
            continue
        members = []
        for m in original["members"]:
            members += [m | dict(arm=arm, package="remote", procs=4, tags="stringlabels", benchtime="1x")
                        for arm in ARMS]
        blocks.append(dict(id="diagnostic/"+original["id"], cohort=0, members=members))
    # Process-wide CPU/allocation profiles are separate from lock profiles.
    for group, sharing, profile in itertools.product(("history", "backlog"), ("shared", "distinct"), ("profile", "locks")):
        case = "unchanged" if group == "history" else "changes"
        members = [scale.spec(group, case, sharing, 100000, "native", profile) |
                   dict(arm=arm, package="remote", procs=4, tags="stringlabels", benchtime="1x") for arm in ARMS]
        blocks.append(dict(id=f"diagnostic/profiles/{group}/{sharing}/{profile}", cohort=0, members=members))
    counts = {}
    for block in blocks:
        for m in block["members"]:
            counts[m["kind"]] = counts.get(m["kind"], 0)+1
    assert counts["scored"] == 1728
    return dict(schema=1, baseline=BASELINE, gates=GATES, environment=scale.ENV,
                policy=scale.POLICIES[2], blocks=blocks, counts=counts)


def environment(m):
    env = scale.environment(m if m["package"] == "remote" else None)
    env["GOMAXPROCS"] = str(m["procs"])
    if m["package"] == "remote":
        env.update(PROMETHEUS_METADATA_PIPELINE_SERIES="10000", PROMETHEUS_METADATA_PIPELINE_SWEEPS="200")
    return env


def binary_path(m):
    return f"{m['arm']}/{m['package']}-{m['tags']}.test"


def metrics_line(content, m):
    output = content.splitlines()
    assert "PASS" in output and "FAIL" not in content, "benchmark did not pass"
    lines = [line for line in output if line.startswith("Benchmark")]
    assert lines, "missing benchmark measurement"
    assert len(lines) == 1, "duplicate benchmark measurements"
    assert "goos: linux" in content and "goarch: amd64" in content
    parts = lines[0].split()
    expected = name(m)+f"-{m['procs']}"
    assert parts[0] == expected, f"unexpected benchmark: got {parts[0]}, want {expected}"
    assert len(parts) >= 4 and int(parts[1]) > 0, parts[:2]
    if m["benchtime"].endswith("x"):
        assert int(parts[1]) == int(m["benchtime"][:-1])
    assert len(parts) % 2 == 0
    metrics = {parts[i+1]: float(parts[i]) for i in range(2, len(parts), 2)}
    assert len(metrics) == (len(parts)-2)//2
    assert {"ns/op", "B/op", "allocs/op"} <= metrics.keys()
    assert all(math.isfinite(v) and v >= 0 for v in metrics.values()) and metrics["ns/op"] > 0
    return dict(line=lines[0], metrics=metrics)


def parse_output(content, m):
    parsed = metrics_line(content, m)
    if m["package"] == "remote" and m["group"] != "original":
        return scale.parse_output(content, m)
    if m["package"] == "tsdb":
        if m["kind"] == "head-heap":
            unit = "pending-heap-B/observation" if "PendingHeap" in m["bench"] else "heap-B/series"
            assert unit in parsed["metrics"], "missing retained-heap measurement"
        return parsed
    assert {"samples/op", "samples/s", "cpu-ns/sample", "alloc-B/sample", "allocs/sample",
            "wal-B/sample", "wire-B/sample", "drain-ms/op", "txn-p99-ns"} <= parsed["metrics"].keys(), "missing pipeline metric"
    traces = re.findall(r"metadata-pipeline-result: (.+)", content)
    assert len(traces) == 1
    r = json.loads(traces[0])
    c = r["Config"].copy()
    assert isinstance(c.pop("Base"), int)
    expected = dict(Group="", Case=m["case"], Source=m["mode"], Series=10000, Values=100, Sweeps=200,
                    Writers=4, Shards=4, Batch=2000, Capacity=10000, CommitSize=1000, ReceiverProcs=2,
                    Mixed=False, SweepInterval=0, SamplesPerSecond=0)
    assert c == expected, (c, expected)
    cold = m["case"] == "cold"
    samples = 10000 if cold else 2000000
    assert not r["Diagnostic"]
    assert r["Samples"] == samples and parsed["metrics"]["samples/op"] == samples
    assert r["ResidentSeries"] == (10000 if cold else 30000)
    assert r["Transactions"] == (12 if cold else 2400)
    assert r["ScheduledTransactions"] == r["TransactionsLateByInterval"] == r["SweepsLateByInterval"] == 0
    assert r["CPU"]["Available"] and r["ReceiverCPU"]["Available"] and r["LifecycleCPU"]["Available"]
    assert 0 < r["Completion"] <= r["Lifecycle"]
    assert r["Completion"] >= r["Ingestion"] + r["Drain"] + r["Shutdown"]
    assert r["BacklogWait"] == r["ReleaseToDrain"] == r["SeededHeap"] == r["BacklogHeap"] == r["DrainedHeap"] == 0
    assert 0 <= r["OutstandingAtWriterEnd"] <= samples and r["PeakSampledQueue"] >= 0
    for field in ("AllocatedBytes", "Allocations", "WALBytes", "RequestBytes", "ReceiverServiceNanos"):
        assert math.isfinite(r[field]) and r[field] >= 0, field
    parsed["metrics"].update({"cpu-ns/sample": (r["CPU"]["User"]+r["CPU"]["System"])/samples,
                              "alloc-B/sample": r["AllocatedBytes"]/samples, "allocs/sample": r["Allocations"]/samples})
    parsed["result"] = r
    return parsed


def freeze(root, sources):
    root = root.resolve()
    sources = [p.resolve() for p in sources]
    assert len(set(sources)) == 3
    assert all(root != p and p not in root.parents for p in sources)
    root.mkdir()
    planned = manifest()
    planned.update(host=scale.host_identity(), sources={}, hashes={})
    # Timing fixtures and dependencies must match; representation-specific unit
    # helpers may differ, but no benchmark workload is changed in the candidate.
    reference = None
    for arm, source in zip(ARMS, sources):
        fixture = {p.relative_to(source).as_posix(): scale.sha(p) for p in source.rglob("*")
                   if p.is_file() and (p.name.endswith("_bench_test.go") or
                      p.name.startswith("metadata_pipeline") and p.suffix == ".go" or p.name in ("go.mod", "go.sum"))}
        if reference is None:
            reference = fixture
        assert fixture == reference, f"{arm} benchmark fixtures/dependencies differ"
        destination = root / arm
        destination.mkdir()
        source_hashes = scale.inventory(source)
        archive = destination / "source.tar.gz"
        subprocess.run(["tar", "-czf", str(archive), "-C", str(source), "."], check=True)
        scale.source_archive(destination, dict(source_hashes=source_hashes))
        planned["sources"][arm] = dict(path=str(source), source_hashes=source_hashes)
        planned["hashes"][f"{arm}/source.tar.gz"] = scale.sha(archive)
        for package, tags in [("remote", "stringlabels")] + [("tsdb", tag) for tag in TAGS]:
            target = destination / f"{package}-{tags}.test"
            with (destination / f"{package}-{tags}-build.txt").open("w") as log:
                subprocess.run(["go", "test", "-p=2", "-c", "-tags="+tags, "-o", str(target),
                                "./storage/remote" if package == "remote" else "./tsdb"],
                               cwd=source, env=scale.environment(), stdout=log, stderr=subprocess.STDOUT, check=True)
            planned["hashes"][target.relative_to(root).as_posix()] = scale.sha(target)
    for filename in ("benchmark-metadata-history.py", "benchmark-metadata-pipeline-scale.py", "benchmark-metadata-history_test.py"):
        shutil.copyfile(Path(__file__).with_name(filename), root / filename)
        planned["hashes"][filename] = scale.sha(root / filename)
    planned["fixture_hashes"] = reference
    with (root / "provenance.txt").open("w") as log:
        for command in (["date", "-u"], ["uname", "-a"], ["lscpu"], ["free", "-b"], ["go", "version"],
                        ["go", "version", "-m", shutil.which("benchstat")]):
            subprocess.run(command, env=scale.environment(), stdout=log, stderr=subprocess.STDOUT, check=True)
    scale.write(root / "expected-runs.json", planned)
    (root / "expected-runs.sha256").write_text(scale.sha(root / "expected-runs.json")+"\n")
    return planned


def load(root, execution=False):
    assert scale.sha(root / "expected-runs.json") == (root / "expected-runs.sha256").read_text().strip()
    planned = json.loads((root / "expected-runs.json").read_text())
    for key, value in manifest().items():
        assert planned[key] == value, key
    for path, digest in planned["hashes"].items():
        assert scale.sha(root / path) == digest, path
    for arm in ARMS:
        scale.source_archive(root / arm, planned["sources"][arm])
        if execution:
            assert scale.inventory(Path(planned["sources"][arm]["path"])) == planned["sources"][arm]["source_hashes"]
    if execution:
        assert scale.host_identity() == planned["host"], "host boot/kernel changed"
        assert scale.sha(Path(__file__)) == planned["hashes"]["benchmark-metadata-history.py"]
        assert scale.sha(Path(scale.__file__)) == planned["hashes"]["benchmark-metadata-pipeline-scale.py"]
    return planned


def command(root, folder, m):
    args = [str(root / binary_path(m)), "-test.run=^$",
            "-test.bench="+"/".join("^"+re.escape(p)+"$" for p in name(m).split("/")),
            "-test.benchtime="+m["benchtime"], "-test.benchmem", "-test.timeout=10m"]
    if m["kind"] == "profile":
        args += ["-test.cpuprofile="+str(folder / "cpu.pprof"), "-test.memprofile="+str(folder / "alloc.pprof")]
    if m["kind"] == "locks":
        args += ["-test.mutexprofile="+str(folder / "mutex.pprof"), "-test.mutexprofilefraction=1",
                 "-test.blockprofile="+str(folder / "block.pprof"), "-test.blockprofilerate=1"]
    return args


def run_one(root, folder, m, planned):
    assert scale.host_identity() == planned["host"]
    folder.mkdir()
    binary = binary_path(m)
    assert scale.sha(root / binary) == planned["hashes"][binary]
    env = environment(m)
    args = command(root, folder, m)
    before, activity, started = scale.snapshot(), [scale.processes(-1)], time.time()
    with (folder / "stdout.txt").open("w") as out, (folder / "stderr.txt").open("w") as err:
        process = subprocess.Popen(args, cwd=planned["sources"][m["arm"]]["path"], env=env,
                                   stdout=out, stderr=err, start_new_session=True)
        while process.poll() is None:
            activity.append(scale.processes(process.pid))
            if time.time()-started > 660:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
                break
            time.sleep(.5)
    after = scale.snapshot()
    assessment = scale.host_activity(before, after, activity, 2)
    record = dict(member=m, binary_sha256=planned["hashes"][binary], command=args,
                  environment={k: v for k, v in env.items() if k in scale.ENV or k.startswith("PROMETHEUS_METADATA_PIPELINE_")},
                  before=before, after=after, activity=activity, started=started, ended=time.time(),
                  host=scale.host_identity(), exit=process.returncode, **assessment)
    for stream in ("stdout", "stderr"):
        record[stream+"_sha256"] = scale.sha(folder / (stream+".txt"))
    profiles = ("cpu.pprof", "alloc.pprof") if m["kind"] == "profile" else (
                ("mutex.pprof", "block.pprof") if m["kind"] == "locks" else ())
    record["profiles"] = {p: scale.sha(folder / p) for p in profiles if (folder / p).is_file()}
    scale.write(folder / "record.json", record)
    try:
        assert process.returncode == 0, f"benchmark exited with status {process.returncode}"
        assert record["host"] == planned["host"], "host boot/kernel changed"
        assert set(record["profiles"]) == set(profiles), "missing profile"
        parse_output((folder / "stdout.txt").read_text(), m)
    except (AssertionError, ValueError, KeyError, IndexError) as error:
        raise RuntimeError(f"{name(m)} ({m['arm']}, {m['tags']}, GOMAXPROCS={m['procs']}): {error}\n"
                           f"Command: {shlex.join(args)}\nArtifacts: {folder}") from error
    if assessment["warnings"]:
        print("CPU steal retained:", assessment["warnings"], flush=True)
    return assessment


def validate_record(root, folder, m, planned):
    record = json.loads((folder / "record.json").read_text())
    assert record["member"] == m and record["exit"] == 0
    assert record["host"] == planned["host"]
    assert record["binary_sha256"] == planned["hashes"][binary_path(m)]
    # Absolute paths change when an archive is relocated; compare command shapes.
    expected = command(root, folder, m)
    normalize = lambda args: [Path(x).name if i == 0 else re.sub(r"(-test.(?:cpu|mem|mutex|block)profile=).*/", r"\1", x)
                              for i, x in enumerate(args)]
    assert normalize(record["command"]) == normalize(expected)
    env = environment(m)
    assert record["environment"] == {k: v for k, v in env.items() if k in scale.ENV or k.startswith("PROMETHEUS_METADATA_PIPELINE_")}
    assert math.isfinite(record["ended"]-record["started"]) and record["ended"] >= record["started"]
    assessment = scale.host_activity(record["before"], record["after"], record["activity"], 2)
    assert all(record[k] == assessment[k] for k in ("noise", "warnings", "steal_fractions"))
    for stream in ("stdout", "stderr"):
        assert record[stream+"_sha256"] == scale.sha(folder / (stream+".txt"))
    profiles = ("cpu.pprof", "alloc.pprof") if m["kind"] == "profile" else (
                ("mutex.pprof", "block.pprof") if m["kind"] == "locks" else ())
    assert set(record["profiles"]) == set(profiles)
    for path, digest in record["profiles"].items():
        assert scale.sha(folder / path) == digest
    return dict(member=m, host=assessment, **parse_output((folder / "stdout.txt").read_text(), m))


def smoke_members():
    # Audit every Head selection on baseline/stringlabels before the other builds
    # and the more expensive pipeline traces, without adding extra observations.
    cases = [m | dict(arm=arm, tags=tags, benchtime="1x")
             for arm, tags, m in itertools.product(ARMS, TAGS, head_cases())]
    for m in pipeline_cases():
        for arm, mode in itertools.product(ARMS, MODES):
            cases.append(m | dict(arm=arm, mode=mode))
    return cases


def smoke(root):
    planned = load(root, execution=True)
    destination = root / "smoke"
    destination.mkdir()
    members = smoke_members()
    for i, m in enumerate(members):
        print(time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "smoke", i+1, len(members),
              name(m), m["arm"], m["tags"], flush=True)
        run_one(root, destination / str(i), m, planned)
    (root / "SMOKE_COMPLETE").touch()


def validate_smoke(root, planned):
    assert (root / "SMOKE_COMPLETE").is_file()
    members = smoke_members()
    assert {p.name for p in (root / "smoke").iterdir()} == {str(i) for i in range(len(members))}
    for i, m in enumerate(members):
        validate_record(root, root / "smoke" / str(i), m, planned)


def run(root):
    planned = load(root, execution=True)
    validate_smoke(root, planned)
    (root / "results").mkdir()
    for index, block in enumerate(planned["blocks"]):
        destination = root / "results" / block["id"]
        destination.mkdir(parents=True)
        for attempt in (0, 1):
            folder = destination / f"attempt-{attempt}"
            folder.mkdir()
            reasons, warnings = [], []
            for i, m in enumerate(block["members"]):
                print(time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), index+1, len(planned["blocks"]),
                      block["id"], attempt, i, name(m), m["arm"], flush=True)
                result = run_one(root, folder / str(i), m, planned)
                reasons += result["noise"]
                warnings += result["warnings"]
            scale.write(folder / "block.json", dict(block=block, attempt=attempt, noise=reasons, warnings=warnings))
            if not reasons:
                scale.write(destination / "accepted.json", dict(attempt=attempt))
                break
            if attempt:
                scale.write(root / "INCOMPLETE.json", dict(block=block, reasons=reasons))
                raise RuntimeError("local interference recurred; all attempts preserved")
    collect(root, planned)
    (root / "MEASUREMENT_COMPLETE").touch()


def collect(root, planned):
    observations, exclusions = [], []
    expected_records = set()
    counts = {}
    for block in planned["blocks"]:
        destination = root / "results" / block["id"]
        accepted = json.loads((destination / "accepted.json").read_text())["attempt"]
        assert accepted in (0, 1)
        assert {p.name for p in destination.iterdir()} == {"accepted.json"} | {f"attempt-{a}" for a in range(accepted+1)}
        for attempt in range(accepted+1):
            folder = destination / f"attempt-{attempt}"
            assert {p.name for p in folder.iterdir()} == {"block.json"} | {str(i) for i in range(len(block["members"]))}
            reasons, warnings = [], []
            for i, m in enumerate(block["members"]):
                expected_records.add((folder / str(i) / "record.json").relative_to(root).as_posix())
                parsed = validate_record(root, folder / str(i), m, planned)
                reasons += parsed["host"]["noise"]
                warnings += parsed["host"]["warnings"]
                if attempt == accepted:
                    observations.append(dict(block=block["id"], cohort=block["cohort"], **parsed))
                    counts[m["kind"]] = counts.get(m["kind"], 0)+1
            assert json.loads((folder / "block.json").read_text()) == dict(block=block, attempt=attempt, noise=reasons, warnings=warnings)
            assert bool(reasons) == (attempt != accepted)
            if reasons:
                exclusions.append(dict(block=block["id"], attempt=attempt, reasons=reasons))
    assert counts == planned["counts"], counts
    assert {p.relative_to(root).as_posix() for p in (root / "results").rglob("record.json")} == expected_records
    return observations, exclusions


def paired_summary(observations):
    cells = {}
    for o in observations:
        if not o["cohort"]:
            continue
        m = o["member"]
        key = (o["cohort"], m["package"], m["tags"], m["procs"], name(m))
        values = cells.setdefault(key, {}).setdefault(m["arm"], {})
        assert o["block"] not in values, "duplicate paired observation"
        values[o["block"]] = o["metrics"]
    result = []
    for key, arms in sorted(cells.items()):
        assert set(arms) == set(ARMS)
        blocks = arms["baseline"].keys()
        assert len(blocks) == 6 and all(v.keys() == blocks for v in arms.values())
        metrics = set.intersection(*(set(v) for arm in arms.values() for v in arm.values()))
        summaries = {}
        for metric in sorted(metrics):
            values = {arm: [arms[arm][b][metric] for b in blocks] for arm in ARMS}
            row = dict(medians={arm: statistics.median(v) for arm, v in values.items()})
            for numerator, denominator in (("series", "baseline"), ("index", "baseline"), ("series", "index")):
                if all(v > 0 for v in values[denominator]):
                    ratios = [a/b for a, b in zip(values[numerator], values[denominator])]
                    row[numerator+"/"+denominator] = dict(median=statistics.median(ratios), minimum=min(ratios), maximum=max(ratios))
            summaries[metric] = row
        result.append(dict(cohort=key[0], package=key[1], tags=key[2], procs=key[3], bench=key[4], metrics=summaries))
    return result



def pipeline_ratios(observations):
    cells = {}
    for o in observations:
        m = o["member"]
        if not o["cohort"] or m["package"] != "remote":
            continue
        key = (o["cohort"], m["arm"], name(m).rsplit("/source=", 1)[0])
        values = cells.setdefault(key, {}).setdefault(m["mode"], {})
        assert o["block"] not in values
        values[o["block"]] = o["metrics"]
    result = []
    for key, modes in sorted(cells.items()):
        assert set(modes) == set(MODES)
        blocks = modes["wal"].keys()
        assert len(blocks) == 6 and all(v.keys() == blocks for v in modes.values())
        metrics = {}
        for metric in ("cpu-ns/sample", "alloc-B/sample", "samples/s"):
            ratios = [modes["native"][b][metric]/modes["wal"][b][metric] for b in blocks]
            metrics[metric] = dict(median=statistics.median(ratios), minimum=min(ratios), maximum=max(ratios))
        result.append(dict(cohort=key[0], arm=key[1], bench=key[2], native_wal=metrics))
    return result


def incremental_heap(observations):
    cells = {}
    for o in observations:
        m = o["member"]
        if m["package"] != "tsdb" or not re.match(
                r"BenchmarkHeadMetricMetadata(?:RetainedHeap|CollapsedHistoryRetainedHeap)/", name(m)):
            continue
        mode = name(m).rsplit("/mode=", 1)[1]
        if mode not in ("native", "off"):
            continue
        key = (o["cohort"], m["tags"], name(m).rsplit("/mode=", 1)[0])
        repetition = o["block"].split("/")[2]
        values = cells.setdefault(key, {}).setdefault(m["arm"], {}).setdefault(mode, {})
        assert repetition not in values, "duplicate retained-heap observation"
        values[repetition] = o["metrics"]["heap-B/series"]
    result = []
    for key, arms in sorted(cells.items()):
        assert set(arms) == set(ARMS)
        deltas = {}
        for arm in ARMS:
            modes = arms[arm]
            assert set(modes) == {"off", "native"}
            assert modes["native"].keys() == modes["off"].keys() and len(modes["off"]) == 6
            deltas[arm] = {r: value-modes["off"][r] for r, value in modes["native"].items()}
        row = dict(cohort=key[0], tags=key[1], bench=key[2],
                   bytes_per_series={arm: statistics.median(list(v.values())) for arm, v in deltas.items()})
        if all(v > 0 for v in deltas["baseline"].values()):
            ratios = [deltas["series"][r]/v for r, v in deltas["baseline"].items()]
            row["series/baseline"] = dict(median=statistics.median(ratios), minimum=min(ratios), maximum=max(ratios))
        else:
            row["inconclusive"] = "nonpositive baseline native-minus-disabled heap"
        result.append(row)
    return result


def assess_gates(summary, heap):
    blockers, primary = [], []
    for row in summary:
        metrics, bench = row["metrics"], row["bench"]
        label = {k: row[k] for k in ("cohort", "tags", "procs", "bench")}
        if bench == ("BenchmarkRemoteWriteMetadataPipelineScale/group=backlog/case=changes/"
                     "values=distinct/series=100000/source=native"):
            ratio = metrics["cpu-ns/sample"]["series/baseline"]
            primary.append(label | ratio)
            if ratio["median"] > 1-GATES["primary_improvement"] or ratio["maximum"] >= 1:
                blockers.append(label | dict(reason="primary CPU improvement not demonstrated"))
        if row["package"] == "remote":
            protected = "/case=unchanged/" in bench or bench.endswith(("/source=wal", "/source=disabled"))
            timing = ("cpu-ns/sample",) if protected else ()
        else:
            timing = () if "Heap" in bench else ("ns/op", "reader-ns/query", "writer-ns/txn")
        for metric in timing:
            if metric in metrics and metrics[metric]["series/baseline"]["median"] > 1+GATES["protected_regression"]:
                blockers.append(label | dict(reason="protected timing budget exceeded", metric=metric,
                                               ratio=metrics[metric]["series/baseline"]["median"]))
        stable = "/case=stable" in bench or "/state=current/" in bench
        if stable:
            for metric in ("B/op", "allocs/op"):
                values = metrics[metric]["medians"]
                if values["series"] > values["baseline"]:
                    blockers.append(label | dict(reason="stable allocations increased", metric=metric, values=values))
        if "heap-B/series" in metrics and metrics["heap-B/series"]["series/baseline"]["median"] > 1+GATES["retained_heap_regression"]:
            blockers.append(label | dict(reason="retained heap budget exceeded",
                                          ratio=metrics["heap-B/series"]["series/baseline"]["median"]))
    for row in heap:
        if "inconclusive" in row or row["series/baseline"]["median"] > 1+GATES["retained_heap_regression"]:
            blockers.append(row | dict(reason="incremental native heap guard not met"))
    assert {r["cohort"] for r in primary} == {1, 2}, "missing primary replication"
    return dict(primary=primary, blockers=blockers, performance_gates_pass=not blockers,
                requires="Correctness, lint, unchanged memSeries layout, and unchanged forwarding-branch baseline must also be verified. No Git changes are performed by this runner.")


def analyze(root):
    planned = load(root)
    assert (root / "MEASUREMENT_COMPLETE").is_file()
    validate_smoke(root, planned)
    observations, exclusions = collect(root, planned)
    output = root / "analysis"
    output.mkdir(exist_ok=True)
    scale.write(output / "observations.json", observations)
    scale.write(output / "coverage.json", dict(counts=planned["counts"], exclusions=exclusions,
                warnings=[dict(block=o["block"], member=o["member"], warnings=o["host"]["warnings"])
                          for o in observations if o["host"]["warnings"]]))
    summary = paired_summary(observations)
    scale.write(output / "summary.json", summary)
    scale.write(output / "native-wal.json", pipeline_ratios(observations))
    heap = incremental_heap(observations)
    scale.write(output / "incremental-heap.json", heap)
    scale.write(output / "gates.json", assess_gates(summary, heap))
    for cohort, package, tags in itertools.product((1, 2), ("remote", "tsdb"), TAGS):
        rows = [o for o in observations if o["cohort"] == cohort and o["member"]["package"] == package and o["member"]["tags"] == tags]
        if not rows:
            continue
        paths = []
        for arm in ARMS:
            path = output / f"cohort-{cohort}-{package}-{tags}-{arm}.txt"
            path.write_text("goos: linux\ngoarch: amd64\npkg: github.com/prometheus/prometheus/"+("storage/remote" if package == "remote" else package)+"\n"+
                            "\n".join(o["line"] for o in rows if o["member"]["arm"] == arm)+"\n")
            paths.append(path)
        with (output / f"cohort-{cohort}-{package}-{tags}.benchstat.txt").open("w") as report:
            subprocess.run(["benchstat", *map(str, paths)], stdout=report, check=True)
    print("Complete: cohorts remain separate; paired ranges are observations, not confidence intervals.")


def main():
    action = sys.argv[1]
    if action == "selftest":
        assert len(smoke_members()) == 2178
        print(manifest()["counts"])
        return
    root = Path(sys.argv[2]).resolve()
    if action == "freeze":
        assert len(sys.argv) == 6
        freeze(root, [Path(p) for p in sys.argv[3:]])
    else:
        {"smoke": smoke, "run": run, "analyze": analyze}[action](root)


if __name__ == "__main__":
    main()

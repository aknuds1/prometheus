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

"""Serial Linux study: selftest, freeze DIRECTORY, smoke DIRECTORY, run DIRECTORY, analyze DIRECTORY.

continue OLD_RESULTS NEW_RESULTS finishes an interrupted schema-1 study separately.

Run freeze from the repository root after validation, with go and benchstat on PATH.
The new results directory must be outside the source tree. Run never overwrites or
resumes partial measurements. Analyze also works on a downloaded results directory.
"""

import copy
import hashlib
import itertools
import json
import math
import os
from pathlib import Path
import re
import shutil
import signal
import statistics
import subprocess
import sys
import tarfile
import time


ENV = {"GOMAXPROCS": "4", "GOGC": "100", "GOMEMLIMIT": "off", "GODEBUG": "",
       "GOENV": "off", "GOTOOLCHAIN": "local", "GOWORK": "off", "GOFLAGS": "",
       "GOEXPERIMENT": "", "GOAMD64": "v1", "CGO_ENABLED": "1", "GOOS": "linux", "GOARCH": "amd64"}
MODES = ("wal", "native", "disabled")
POLICIES = {
    1: dict(steal_fraction_max=.01, retries_per_block=1, benchtime="1x"),
    2: dict(steal_action="record", steal_fraction_warning=.01, retries_per_block=1, benchtime="1x"),
}


def write(path, value):
    temporary = path.with_name(path.name + ".tmp")
    temporary.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")
    temporary.replace(path)


def sha(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def spec(group, case, sharing, series, mode, kind="scored", receiver=2):
    return dict(group=group, case=case, sharing=sharing, series=series, mode=mode,
                kind=kind, receiver=receiver)


def manifest(schema=2):
    blocks = []
    permutations = list(itertools.permutations(MODES))
    for cohort in (1, 2):
        for repetition in range(6):
            for case_index, (case, sharing) in enumerate(itertools.product(("unchanged", "changes", "backlog"), ("shared", "distinct"))):
                traces = [("backlog" if case == "backlog" else "history", series) for series in (10000, 100000)]
                if case == "unchanged":
                    traces.append(("equal-work", 100000))
                orders = list(itertools.permutations(traces))
                traces = orders[(repetition + cohort + case_index) % len(orders)]
                members = []
                for trace_index, (group, series) in enumerate(traces):
                    for mode in permutations[(repetition + cohort + case_index + trace_index) % 6]:
                        members.append(spec(group, "changes" if case == "backlog" else case, sharing, series, mode))
                blocks.append(dict(id=f"cohort-{cohort}/{case}-{sharing}/{repetition+1}", members=members))
    for repetition in range(3):
        for case, sharing in itertools.product(("unchanged", "changes", "backlog"), ("shared", "distinct")):
            group = "backlog" if case == "backlog" else "history"
            members = [spec(group, "changes" if case == "backlog" else case, sharing, series, mode, "heap")
                       for series in (10000, 100000) for mode in permutations[repetition]]
            blocks.append(dict(id=f"heap/{case}-{sharing}/{repetition+1}", members=members))
        for case, sharing in itertools.product(("unchanged", "changes"), ("shared", "distinct")):
            members = [spec("capacity", case, sharing, 100000, mode, "capacity", receiver)
                       for mode in (("wal", "native") if repetition % 2 == 0 else ("native", "wal"))
                       for receiver in ((2, 4) if repetition % 2 == 0 else (4, 2))]
            blocks.append(dict(id=f"capacity/{case}-{sharing}/{repetition+1}", members=members))
    for case, sharing in itertools.product(("unchanged", "changes"), ("shared", "distinct")):
        blocks.append(dict(id=f"profiles/{case}-{sharing}", members=[
            spec("history", case, sharing, 100000, mode, "profile") for mode in ("wal", "native")]))
    counts = {kind: sum(m["kind"] == kind for b in blocks for m in b["members"])
              for kind in ("scored", "heap", "capacity", "profile")}
    assert counts == dict(scored=504, heap=108, capacity=48, profile=8), counts
    return dict(schema=schema, environment=ENV, blocks=blocks, counts=counts, policy=POLICIES[schema])


def name(m):
    return (f"BenchmarkRemoteWriteMetadataPipelineScale/group={m['group']}/case={m['case']}"
            f"/values={m['sharing']}/series={m['series']}/source={m['mode']}")


def config(m):
    backlog = m["group"] == "backlog"
    return dict(Group=m["group"], Case="backlog" if backlog else m["case"], Source=m["mode"],
                Series=m["series"], Values=100 if m["sharing"] == "shared" else m["series"],
                Sweeps=4 if backlog else (20 if m["group"] == "equal-work" else 200),
                Writers=1 if backlog else 4, Shards=1 if backlog else 4, Batch=2000, Capacity=10000,
                CommitSize=1000 if backlog else 500, ReceiverProcs=m["receiver"], Mixed=False,
                SweepInterval=0, SamplesPerSecond=0 if backlog or m["group"] == "capacity" else 500000)


def environment(m=None):
    env = {k: v for k, v in os.environ.items() if not k.startswith("PROMETHEUS_METADATA_PIPELINE_")}
    env.update(ENV)
    if m:
        env.update(PROMETHEUS_METADATA_PIPELINE_RECEIVER_PROCS=str(m["receiver"]),
                   PROMETHEUS_METADATA_PIPELINE_HEAP="1" if m["kind"] == "heap" else "0")
    return env


def parse_output(content, m):
    assert "\nPASS\n" in content and "FAIL" not in content, "benchmark did not pass"
    assert "goos: linux" in content and "goarch: amd64" in content
    lines = [line for line in content.splitlines() if line.startswith("Benchmark")]
    assert len(lines) == 1, "missing or duplicate benchmark"
    parts = lines[0].split()
    assert parts[0] == name(m) + "-4" and parts[1] == "1", parts[:2]
    assert len(parts) % 2 == 0
    metrics = {parts[i+1]: float(parts[i]) for i in range(2, len(parts), 2)}
    assert len(metrics) == (len(parts)-2)//2
    assert {"ns/op", "B/op", "allocs/op", "samples/op", "samples/s", "cpu-ns/sample",
            "alloc-B/sample", "allocs/sample", "wal-B/sample", "wire-B/sample",
            "drain-ms/op", "txn-p99-ns"} <= metrics.keys(), "missing metric"
    assert all(math.isfinite(v) and v >= 0 for v in metrics.values())
    assert metrics["ns/op"] > 0
    raw = re.findall(r"metadata-pipeline-result: (.+)", content)
    assert len(raw) == 1, "missing or duplicate trace"
    r = json.loads(raw[0])
    c = r["Config"].copy()
    assert isinstance(c.pop("Base"), int)
    assert c == config(m), (c, config(m))
    assert r["Diagnostic"] == (m["kind"] == "heap"), "diagnostic/scored mismatch"
    n = c["Series"] * c["Sweeps"]
    assert r["Samples"] == n and r["ResidentSeries"] == c["Series"]
    assert metrics["samples/op"] == n
    assert r["Transactions"] == n // c["CommitSize"]
    assert r["ScheduledTransactions"] == (r["Transactions"] if c["SamplesPerSecond"] else 0)
    assert r["SweepsLateByInterval"] == 0
    assert 0 <= r["TransactionsLateByInterval"] <= r["ScheduledTransactions"]
    assert r["CPU"]["Available"] and r["ReceiverCPU"]["Available"] and r["LifecycleCPU"]["Available"]
    assert 0 < r["Completion"] <= r["Lifecycle"]
    assert r["Completion"] >= r["Ingestion"] + r["Drain"] + r["Shutdown"]
    assert 0 <= r["OutstandingAtWriterEnd"] <= n and r["PeakSampledQueue"] >= 0
    if c["Group"] == "backlog":
        assert r["BacklogWait"] > 0 and r["ReleaseToDrain"] > 0
        assert r["Drain"] == r["BacklogWait"] + r["ReleaseToDrain"]
        assert r["OutstandingAtWriterEnd"] == n
    else:
        assert r["BacklogWait"] == r["ReleaseToDrain"] == 0
    for field in ("SeededHeap", "DrainedHeap", "BacklogHeap"):
        expected = m["kind"] == "heap" and (field != "BacklogHeap" or c["Group"] == "backlog")
        assert (r[field] > 0) == expected, field
    for field in ("AllocatedBytes", "Allocations", "WALBytes", "RequestBytes", "ReceiverServiceNanos"):
        assert math.isfinite(r[field]) and r[field] >= 0, field
    metrics.update({"cpu-ns/sample": (r["CPU"]["User"] + r["CPU"]["System"])/n,
                    "alloc-B/sample": r["AllocatedBytes"]/n, "allocs/sample": r["Allocations"]/n})
    return dict(line=lines[0], metrics=metrics, result=r)


def snapshot():
    cpu = {}
    for line in Path("/proc/stat").read_text().splitlines():
        fields = line.split()
        if re.fullmatch(r"cpu[0-9]*", fields[0]):
            cpu[fields[0]] = list(map(int, fields[1:9]))
    vm = dict(line.split() for line in Path("/proc/vmstat").read_text().splitlines())
    return dict(cpu=cpu, swap={k: int(vm[k]) for k in ("pswpin", "pswpout")})


def processes(exclude_group):
    raw = subprocess.check_output(["ps", "-eo", "pid,pgid,comm,pcpu", "--no-headers"], text=True)
    offenders = []
    for line in raw.splitlines():
        pid, group, comm, _ = line.split()
        if int(group) == exclude_group:
            continue
        try:
            executable = Path(os.readlink(f"/proc/{pid}/exe")).name
        except OSError:
            executable = comm
        if executable in {"go", "compile", "link", "golangci-lint", "gcc", "cc1", "make", "apt", "apt-get", "dpkg"} or executable.endswith(".test"):
            offenders.append(line)
    return dict(time=time.time(), processes=raw, offenders=offenders)


def host_activity(before, after, activity, schema):
    """Keep legacy exclusions reproducible; CPU steal is informational in schema 2."""
    fractions, warnings = {}, []
    assert before["cpu"].keys() == after["cpu"].keys()
    for cpu in before["cpu"]:
        delta = [a-b for a, b in zip(after["cpu"][cpu], before["cpu"][cpu])]
        assert all(x >= 0 for x in delta)
        fractions[cpu] = delta[7]/sum(delta) if sum(delta) else 0
        if fractions[cpu] > .01:
            warnings.append(f"{cpu} steal {fractions[cpu]:.4%}")
    reasons = warnings.copy() if schema == 1 else []
    if before["swap"] != after["swap"]:
        reasons.append("swap activity")
    if any(a["offenders"] for a in activity):
        reasons.append("overlapping compilation, tests, or package installation")
    return dict(steal_fractions=fractions, warnings=warnings, noise=reasons)


def host_identity():
    return dict(kernel=os.uname().release, boot=Path("/proc/sys/kernel/random/boot_id").read_text().strip())


def freeze(root):
    source = Path.cwd().resolve()
    assert (source / "storage/remote/metadata_pipeline_bench_test.go").is_file()
    assert root != source and source not in root.parents, "results must be outside source"
    root.mkdir()
    planned = manifest()
    planned["source"] = str(source)
    planned["host"] = host_identity()
    shutil.copyfile(__file__, root / "runner.py")
    with (root / "build.txt").open("w") as log:
        subprocess.run(["go", "test", "-p=2", "-c", "-o", str(root / "remote.test"), "./storage/remote"],
                       env=environment(), stdout=log, stderr=subprocess.STDOUT, check=True)
    subprocess.run(["tar", "--exclude=.git", "-czf", str(root / "source.tar.gz"), "."], check=True)
    planned["hashes"] = {p: sha(root / p) for p in ("remote.test", "source.tar.gz")}
    planned["runner_sha256"] = sha(Path(__file__))
    planned["source_hashes"] = {str(p.relative_to(source)): sha(p) for p in sorted(source.rglob("*"))
                                if p.is_file() and ".git" not in p.relative_to(source).parts}
    with (root / "provenance.txt").open("w") as log:
        for command in (["date", "-u"], ["uname", "-a"], ["lscpu"], ["free", "-b"],
                        ["go", "version"], ["go", "version", "-m", str(root / "remote.test")],
                        ["go", "version", "-m", subprocess.check_output(["which", "benchstat"], text=True).strip()]):
            subprocess.run(command, env=environment(), stdout=log, stderr=subprocess.STDOUT, check=True)
    write(root / "expected-runs.json", planned)
    (root / "expected-runs.sha256").write_text(sha(root / "expected-runs.json") + "  expected-runs.json\n")


def source_archive(root, planned):
    """Verify archived source without depending on the original absolute checkout path."""
    hashes = {}
    with tarfile.open(root / "source.tar.gz") as archive:
        for member in archive:
            if member.isdir():
                continue
            path = Path(member.name)
            assert member.isfile() and not path.is_absolute() and ".." not in path.parts
            name = path.as_posix()
            assert name not in hashes, name
            hashes[name] = hashlib.file_digest(archive.extractfile(member), "sha256").hexdigest()
    assert hashes == planned["source_hashes"], "source archive differs from source inventory"


def inventory(root):
    files = {}
    for path in sorted(root.rglob("*")):
        assert not path.is_symlink(), path
        if path.is_file():
            files[path.relative_to(root).as_posix()] = sha(path)
    return files


def load(root, execution=False):
    assert sha(root / "expected-runs.json") == (root / "expected-runs.sha256").read_text().split()[0]
    planned = json.loads((root / "expected-runs.json").read_text())
    assert planned["schema"] in POLICIES, "unknown manifest schema"
    for key, value in manifest(planned["schema"]).items():
        assert planned[key] == value, key
    assert set(planned["hashes"]) == {"remote.test", "source.tar.gz"}
    for path, digest in planned["hashes"].items():
        assert sha(root / path) == digest, path
    source_archive(root, planned)
    if planned["schema"] == 1:
        assert "parent" not in planned
        assert planned["runner_sha256"] == planned["source_hashes"]["scripts/benchmark-metadata-pipeline-scale.py"]
    else:
        assert planned["runner_sha256"] == sha(root / "runner.py")
    if execution:
        assert planned["schema"] == 2, "legacy evidence is read-only"
        assert planned["runner_sha256"] == sha(Path(__file__)), "executing runner differs from frozen runner"
        assert host_identity() == planned["host"], "host boot/kernel changed"
        for path, digest in planned["source_hashes"].items():
            assert sha(Path(planned["source"]) / path) == digest, path
    return planned


def run_one(root, folder, m, planned):
    assert host_identity() == planned["host"], "host boot/kernel changed"
    folder.mkdir()
    command = [str(root / "remote.test"), "-test.run=^$", "-test.bench=" + "/".join("^"+re.escape(p)+"$" for p in name(m).split("/")),
               "-test.benchtime=1x", "-test.benchmem", "-test.timeout=6m"]
    if m["kind"] == "profile":
        command += ["-test.cpuprofile="+str(folder / "cpu.pprof"), "-test.memprofile="+str(folder / "alloc.pprof")]
    before, activity, started = snapshot(), [processes(-1)], time.time()
    with (folder / "stdout.txt").open("w") as out, (folder / "stderr.txt").open("w") as err:
        p = subprocess.Popen(command, env=environment(m), cwd=planned["source"], stdout=out, stderr=err, start_new_session=True)
        while p.poll() is None:
            activity.append(processes(p.pid))
            if time.time()-started > 420:
                os.killpg(p.pid, signal.SIGKILL)
                p.wait()
                break
            time.sleep(.5)
    after = snapshot()
    assessment = host_activity(before, after, activity, planned["schema"])
    record = dict(member=m, command=command, environment={k: v for k, v in environment(m).items() if k in ENV or k.startswith("PROMETHEUS_METADATA_PIPELINE_")},
                  binary_sha256=planned["hashes"]["remote.test"], started=started, ended=time.time(), exit=p.returncode,
                  before=before, after=after, activity=activity, **assessment,
                  stdout_sha256=sha(folder / "stdout.txt"), stderr_sha256=sha(folder / "stderr.txt"))
    write(folder / "record.json", record)
    assert p.returncode == 0, ("benchmark failed", folder)
    parse_output((folder / "stdout.txt").read_text(), m)
    if m["kind"] == "profile":
        for filename in ("cpu.pprof", "alloc.pprof"):
            assert (folder / filename).stat().st_size > 0
    assert host_identity() == planned["host"]
    if assessment["warnings"]:
        print("CPU-steal warning (informational):", ", ".join(assessment["warnings"]), flush=True)
    return assessment


def validate_record(folder, member, planned):
    record = json.loads((folder / "record.json").read_text())
    assert record["member"] == member and record["exit"] == 0
    assert record["binary_sha256"] == planned["hashes"]["remote.test"]
    expected_env = {k: v for k, v in environment(member).items() if k in ENV or k.startswith("PROMETHEUS_METADATA_PIPELINE_")}
    assert record["environment"] == expected_env
    for stream in ("stdout", "stderr"):
        assert record[stream+"_sha256"] == sha(folder / (stream+".txt"))
    assessment = host_activity(record["before"], record["after"], record["activity"], planned["schema"])
    assert record["noise"] == assessment["noise"]
    if planned["schema"] == 2:
        assert all(record[key] == assessment[key] for key in ("warnings", "steal_fractions"))
    assert math.isfinite(record["ended"] - record["started"]) and record["ended"] >= record["started"]
    parsed = parse_output((folder / "stdout.txt").read_text(), member)
    if member["kind"] == "profile":
        assert all((folder / filename).stat().st_size > 0 for filename in ("cpu.pprof", "alloc.pprof"))
    return dict(**parsed, host=dict(**assessment, seconds=record["ended"]-record["started"]))


def collect_block(root, block, planned, segment, stopped=False):
    destination = root / "results" / block["id"]
    if stopped:
        assert not (destination / "accepted.json").exists()
        accepted, attempts = None, range(2)
    else:
        accepted = json.loads((destination / "accepted.json").read_text())["attempt"]
        assert accepted in (0, 1)
        attempts = range(accepted+1)
    expected = {f"attempt-{i}" for i in attempts} | ({"accepted.json"} if not stopped else set())
    assert {p.name for p in destination.iterdir()} == expected
    observations, excluded = [], []
    for attempt in attempts:
        folder = destination / f"attempt-{attempt}"
        assert {p.name for p in folder.iterdir()} == {str(i) for i in range(len(block["members"]))} | {"block.json"}
        reasons, warnings = [], []
        for i, member in enumerate(block["members"]):
            parsed = validate_record(folder / str(i), member, planned)
            reasons += parsed["host"]["noise"]
            warnings += parsed["host"]["warnings"]
            if attempt == accepted:
                observations.append(dict(block=block["id"], member=member, segment=segment, schema=planned["schema"], **parsed))
        status = dict(block=block, attempt=attempt, noise=reasons)
        if planned["schema"] == 2:
            status["warnings"] = warnings
        assert json.loads((folder / "block.json").read_text()) == status
        assert bool(reasons) == (attempt != accepted)
        if reasons:
            excluded.append(dict(block=block["id"], attempt=attempt, segment=segment, reasons=reasons))
    return observations, excluded, accepted


def smoke_members():
    members = [spec("equal-work", "unchanged", "shared", 100000, mode) for mode in MODES]
    members += [spec("backlog", "changes", "distinct", 100000, mode, "heap") for mode in ("wal", "native")]
    return members + [spec("history", "changes", "shared", 10000, "native")]


def validate_smoke(root, planned):
    assert (root / "SMOKE_COMPLETE").is_file()
    members = smoke_members()
    assert {p.name for p in (root / "smoke").iterdir()} == {str(i) for i in range(len(members))}
    for i, member in enumerate(members):
        validate_record(root / "smoke" / str(i), member, planned)


def check_block_paths(root, blocks):
    expected = {b["id"] for b in blocks}
    results = root / "results"
    assert {p.parent.parent.relative_to(results).as_posix() for p in results.rglob("block.json")} == expected
    assert {p.parent.parent.parent.relative_to(results).as_posix() for p in results.rglob("record.json")} == expected
    assert {p.parent.relative_to(results).as_posix() for p in results.rglob("accepted.json")} <= expected


def legacy_parent(root):
    """Validate a completed prefix and one exhausted legacy block, without changing either."""
    planned = load(root)
    assert planned["schema"] == 1 and not (root / "MEASUREMENT_COMPLETE").exists()
    validate_smoke(root, planned)
    stopped = json.loads((root / "INCONCLUSIVE.json").read_text())
    terminal = next(i for i, block in enumerate(planned["blocks"]) if block == stopped["block"])
    inherited, observations, excluded = [], [], []
    for i, block in enumerate(planned["blocks"]):
        if i > terminal:
            assert not (root / "results" / block["id"]).exists()
            continue
        rows, exclusions, accepted = collect_block(root, block, planned, "original", stopped=i == terminal)
        observations += rows
        excluded += exclusions
        if i < terminal:
            inherited.append(dict(block=block["id"], attempt=accepted))
        else:
            assert exclusions[-1]["reasons"] == stopped["noise"]
    check_block_paths(root, planned["blocks"][:terminal+1])
    return planned, inherited, observations, excluded


def parent_results(root, planned):
    if "parent" not in planned:
        return [], [], []
    parent = planned["parent"]
    assert parent["directory"] == "parent"
    directory = root / "parent"
    assert inventory(directory) == parent["files"], "parent evidence changed"
    previous, inherited, observations, excluded = legacy_parent(directory)
    assert inherited == parent["inherited"]
    assert planned["hashes"] == previous["hashes"]
    assert planned["host"] == previous["host"] and planned["source_hashes"] == previous["source_hashes"]
    remaining = [b["id"] for b in planned["blocks"][len(inherited):]]
    assert remaining == parent["remaining"]
    return inherited, observations, excluded


def prepare_continuation(old, root):
    old, root = old.resolve(), root.resolve()
    assert root != old and root not in old.parents and old not in root.parents, "overlapping results directories"
    assert not root.exists(), "continuation destination already exists"
    previous, inherited, _, _ = legacy_parent(old)
    source = Path(previous["source"]).resolve()
    assert root != source and source not in root.parents and root not in source.parents
    assert host_identity() == previous["host"], "host boot/kernel changed"
    for path, digest in previous["source_hashes"].items():
        assert sha(source / path) == digest, path
    parent_files = inventory(old)
    root.mkdir()
    shutil.copytree(old, root / "parent")
    assert inventory(root / "parent") == parent_files
    for path in previous["hashes"]:
        shutil.copy2(old / path, root / path)
    shutil.copyfile(__file__, root / "runner.py")
    planned = manifest()
    planned.update(source=previous["source"], host=previous["host"], hashes=previous["hashes"],
                   source_hashes=previous["source_hashes"], runner_sha256=sha(root / "runner.py"),
                   parent=dict(directory="parent", files=parent_files, inherited=inherited,
                               remaining=[b["id"] for b in planned["blocks"][len(inherited):]]))
    write(root / "expected-runs.json", planned)
    (root / "expected-runs.sha256").write_text(sha(root / "expected-runs.json")+"  expected-runs.json\n")
    load(root, execution=True)
    parent_results(root, planned)


def collect(root, planned):
    inherited, observations, excluded = parent_results(root, planned)
    validate_smoke(root, planned)
    remaining = planned["blocks"][len(inherited):]
    for block in remaining:
        rows, exclusions, _ = collect_block(root, block, planned, "continuation" if inherited else "original")
        observations += rows
        excluded += exclusions
    check_block_paths(root, remaining)
    expected = {(b["id"], json.dumps(m, sort_keys=True)) for b in planned["blocks"] for m in b["members"]}
    actual = [(o["block"], json.dumps(o["member"], sort_keys=True)) for o in observations]
    assert len(actual) == len(set(actual)) and set(actual) == expected
    return observations, excluded


def complete(root, planned):
    collect(root, planned)
    assert not (root / "INCONCLUSIVE.json").exists()
    marker = root / "MEASUREMENT_COMPLETE"
    assert not marker.exists()
    temporary = root / "MEASUREMENT_COMPLETE.tmp"
    temporary.write_text(time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())+"\n")
    temporary.replace(marker)


def run(root):
    planned = load(root, execution=True)
    inherited, _, _ = parent_results(root, planned)
    validate_smoke(root, planned)
    (root / "results").mkdir()
    for index, block in enumerate(planned["blocks"][len(inherited):], start=len(inherited)):
        assert sha(root / "remote.test") == planned["hashes"]["remote.test"]
        destination = root / "results" / block["id"]
        destination.mkdir(parents=True)
        for attempt in (0, 1):
            folder = destination / f"attempt-{attempt}"
            folder.mkdir()
            reasons, warnings = [], []
            for i, m in enumerate(block["members"]):
                print(time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), f"block={index+1}/{len(planned['blocks'])}", block["id"], attempt, i, m, flush=True)
                assessment = run_one(root, folder / str(i), m, planned)
                reasons += assessment["noise"]
                warnings += assessment["warnings"]
            write(folder / "block.json", dict(block=block, attempt=attempt, noise=reasons, warnings=warnings))
            if not reasons:
                write(destination / "accepted.json", dict(attempt=attempt))
                break
            if attempt:
                write(root / "INCONCLUSIVE.json", dict(block=block, noise=reasons))
                raise RuntimeError("host interference recurred; stopped without a complete-study claim")
    complete(root, planned)


def smoke(root):
    planned = load(root, execution=True)
    destination = root / "smoke"
    destination.mkdir()
    for i, m in enumerate(smoke_members()):
        run_one(root, destination / str(i), m, planned)
    (root / "SMOKE_COMPLETE").write_text("Six real-trace parser and measurement smoke checks passed.\n")


def analyze(root):
    planned = load(root)
    assert (root / "MEASUREMENT_COMPLETE").is_file()
    observations, excluded = collect(root, planned)
    output = root / "analysis"
    output.mkdir(exist_ok=True)
    write(output / "observations.json", observations)
    warnings = [dict(block=o["block"], member=o["member"], segment=o["segment"], **o["host"])
                for o in observations if o["host"]["warnings"]]
    write(output / "coverage.json", dict(counts=planned["counts"], exclusions=excluded, warnings=warnings,
                                         segments={s: sum(o["segment"] == s for o in observations)
                                                   for s in sorted({o["segment"] for o in observations})}))
    groups = [(cohort, "all") for cohort in (1, 2)]
    if "parent" in planned:
        for cohort in (1, 2):
            segments = {o["segment"] for o in observations if o["block"].startswith(f"cohort-{cohort}/")}
            if len(segments) > 1:
                groups += [(cohort, segment) for segment in sorted(segments)]
    summary = []
    for cohort, segment in groups:
        rows = [o for o in observations if o["block"].startswith(f"cohort-{cohort}/")
                and (segment == "all" or o["segment"] == segment)]
        text = "goos: linux\ngoarch: amd64\npkg: github.com/prometheus/prometheus/storage/remote\n"+"\n".join(o["line"] for o in rows)+"\n"
        label = f"cohort-{cohort}" + (f"-{segment}" if segment != "all" else "")
        path = output / f"{label}.txt"
        path.write_text(text)
        with (output / f"{label}.benchstat.txt").open("w") as out:
            subprocess.run(["benchstat", "-col", "/source@(wal native disabled)", "-row", "/group,/case,/values,/series", str(path)], stdout=out, check=True)
        cells = sorted({(o["member"]["group"], o["member"]["case"], o["member"]["sharing"], o["member"]["series"]) for o in rows})
        for cell in cells:
            selected = [o for o in rows if tuple(o["member"][k] for k in ("group", "case", "sharing", "series")) == cell]
            for metric in ("cpu-ns/sample", "alloc-B/sample", "samples/s"):
                values = {mode: {o["block"]: o["metrics"][metric] for o in selected if o["member"]["mode"] == mode} for mode in MODES}
                assert values["wal"].keys() == values["native"].keys() == values["disabled"].keys()
                if segment == "all":
                    assert all(len(v) == 6 for v in values.values())
                ratios = [values["native"][b]/v for b, v in values["wal"].items()]
                summary.append(dict(cohort=cohort, segment=segment, n=len(ratios), cell=cell, metric=metric,
                                    medians={k: statistics.median(list(v.values())) for k, v in values.items()},
                                    native_wal_ratio=dict(median=statistics.median(ratios), minimum=min(ratios), maximum=max(ratios))))
    write(output / "summary.json", summary)


def continue_study(old, root):
    prepare_continuation(old, root)
    smoke(root)
    run(root)
    analyze(root)


def selftest():
    planned = manifest()
    assert len(planned["blocks"]) == 106
    for b in planned["blocks"]:
        assert len(b["members"]) == len({json.dumps(m, sort_keys=True) for m in b["members"]})
    m = spec("history", "unchanged", "shared", 10000, "native")
    c = config(m) | {"Base": 1000}
    r = dict(Config=c, Diagnostic=False, Samples=2000000, ResidentSeries=10000,
             Transactions=4000, ScheduledTransactions=4000, SweepsLateByInterval=0, TransactionsLateByInterval=0,
             CPU=dict(Available=True, User=1, System=1), ReceiverCPU=dict(Available=True), LifecycleCPU=dict(Available=True),
             Completion=10, Lifecycle=11, Ingestion=5, Drain=3, Shutdown=1, OutstandingAtWriterEnd=0, PeakSampledQueue=0,
             BacklogWait=0, ReleaseToDrain=0, SeededHeap=0, BacklogHeap=0, DrainedHeap=0,
             AllocatedBytes=1, Allocations=1, WALBytes=1, RequestBytes=1, ReceiverServiceNanos=1)
    def content(result):
        metrics = "10 ns/op 1 B/op 1 allocs/op 2000000 samples/op 1 samples/s 1 cpu-ns/sample 1 alloc-B/sample 1 allocs/sample 1 wal-B/sample 1 wire-B/sample 1 drain-ms/op 1 txn-p99-ns"
        return "goos: linux\ngoarch: amd64\n"+name(m)+"-4 1 "+metrics+"\nmetadata-pipeline-result: "+json.dumps(result)+"\nPASS\n"
    valid = content(r)
    parse_output(valid, m)
    invalid = [valid.replace(name(m), "BenchmarkWrong"), valid+valid, valid.replace("metadata-pipeline-result:", "missing:"), valid.replace("10 ns/op", "NaN ns/op"), valid.replace("1 cpu-ns/sample ", "")]
    for field, value in (("Diagnostic", True), ("Transactions", 3999), ("ScheduledTransactions", 0), ("ResidentSeries", 100000)):
        altered = copy.deepcopy(r)
        altered[field] = value
        invalid.append(content(altered))
    altered = copy.deepcopy(r)
    altered["Config"]["Sweeps"] = 20
    invalid.append(content(altered))
    for value in invalid:
        try:
            parse_output(value, m)
        except (AssertionError, KeyError, ValueError):
            continue
        raise AssertionError("invalid result accepted")
    print("Manifest and parser self-tests passed.")


if __name__ == "__main__":
    if sys.flags.optimize:
        raise RuntimeError("assertions must remain enabled")
    if len(sys.argv) == 2 and sys.argv[1] == "selftest":
        selftest()
    elif len(sys.argv) == 4 and sys.argv[1] == "continue":
        continue_study(*(Path(arg).resolve() for arg in sys.argv[2:]))
    elif len(sys.argv) == 3 and sys.argv[1] in ("freeze", "smoke", "run", "analyze"):
        globals()[sys.argv[1]](Path(sys.argv[2]).resolve())
    else:
        raise SystemExit(__doc__)

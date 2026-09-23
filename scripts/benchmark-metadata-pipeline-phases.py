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

"""Frozen A/F cold-start and backlog diagnosis; never a promotion gate.

freeze ROOT PARENT_STUDY OVERLAY builds identical test-only overlays on pinned
archives. smoke/run/analyze ROOT preserve and validate all fresh-process evidence.
Analyze supports relocation; archived bridge observations are not a third cohort.
"""

from collections import Counter
import importlib.util
import itertools
import json
import math
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import time

if not __debug__:
    raise RuntimeError("Study validation requires Python without -O")
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("forwarding", Path(__file__).with_name("benchmark-metadata-history-forwarding.py"))
forwarding = importlib.util.module_from_spec(spec)
spec.loader.exec_module(forwarding)
history, scale = forwarding.history, forwarding.scale
ARMS = ("forwarding", "candidate")
REVISIONS = {arm: forwarding.REVISIONS[arm] for arm in ARMS}
OVERLAY = tuple("storage/remote/"+name for name in (
    "metadata_pipeline_test.go", "metadata_pipeline_bench_test.go", "metadata_pipeline_diagnostics_test.go"))
SCRIPTS = forwarding.SCRIPTS + ("benchmark-metadata-pipeline-phases.py", "benchmark-metadata-pipeline-phases_test.py")
ENV = "PROMETHEUS_METADATA_PIPELINE_DIAGNOSTICS"
PHASES = ["initialization", "ingestion", "backlog-wait", "drain", "shutdown", "db-close"]


def member(scenario, kind="plain", variant="rebuilt", phase=""):
    cases = history.pipeline_cases()
    if scenario == "cold":
        base = next(m for m in cases if m["group"] == "original" and m["case"] == "cold")
    else:
        base = next(m for m in cases if m["group"] == "backlog" and m["sharing"] == "distinct" and m["series"] == 100000)
    return base | dict(scenario=scenario, kind=kind, variant=variant, phase=phase, mode="native")


def manifest():
    blocks = []
    for cohort, repetition in itertools.product((1, 2), range(6)):
        arms = ARMS if (cohort+repetition) % 2 else ARMS[::-1]
        variants = ("archived", "rebuilt") if repetition % 2 else ("rebuilt", "archived")
        for scenario in ("cold", "backlog"):
            blocks.append(dict(id=f"bridge/{cohort}/{repetition}/{scenario}", cohort=cohort,
                members=[member(scenario, variant=v) | dict(arm=a) for v, a in itertools.product(variants, arms)]))
        for scenario in ("cold", "backlog", "capped"):
            blocks.append(dict(id=f"accounting/{cohort}/{repetition}/{scenario}", cohort=cohort,
                members=[member(scenario, "accounting") | dict(arm=a) for a in arms]))
    for scenario in ("cold", "backlog", "capped"):
        phases = ("initialization", "ingestion", "drain") if scenario == "cold" else ("ingestion", "drain")
        for phase, repetition in itertools.product(phases, range(32 if scenario == "cold" else 3)):
            arms = ARMS if repetition % 2 else ARMS[::-1]
            blocks.append(dict(id=f"cpu/{scenario}/{phase}/{repetition}", cohort=0,
                members=[member(scenario, "cpu", phase=phase) | dict(arm=a) for a in arms]))
        for kind in ("alloc", "locks") + (() if scenario == "cold" else ("trace",)):
            blocks.append(dict(id=f"{kind}/{scenario}", cohort=0,
                members=[member(scenario, kind) | dict(arm=a) for a in ARMS]))
    counts = dict(Counter(m["kind"] for b in blocks for m in b["members"]))
    assert counts == dict(plain=96, accounting=72, cpu=216, alloc=6, locks=6, trace=4)
    # Keep every profiled process after every unprofiled process.
    assert sum(counts.values()) == 400
    return dict(schema=1, revisions=REVISIONS, blocks=blocks, counts=counts, environment=scale.ENV,
                policy=scale.POLICIES[2], cpu_sample_threshold=100,
                interpretation="Diagnostic only. No inherited performance gate; no adaptive repetitions or steal exclusions.")


def smoke_members(planned):
    unique = {}
    for block in planned["blocks"]:
        for m in block["members"]:
            unique.setdefault(json.dumps(m, sort_keys=True), m)
    return list(unique.values())


def verify_overlay(original, patched):
    assert set(patched)-set(original) == {OVERLAY[2]}, "Unexpected added source"
    assert set(original) <= set(patched), "Removed source"
    assert all(patched[p] == digest for p, digest in original.items() if p not in OVERLAY), "Non-allowlisted change"
    assert all(p.endswith("_test.go") for p in OVERLAY)
    return {p: dict(original=original.get(p), patched=patched[p]) for p in OVERLAY}


def freeze(root, parent, overlay):
    assert sys.platform == "linux"
    previous = forwarding.load(parent)
    root.mkdir()
    planned = manifest() | dict(host=scale.host_identity(), sources={}, hashes={}, overlays={},
                               parent_manifest_sha256=scale.sha(parent / "expected-runs.json"))
    reference = None
    for arm in ARMS:
        assert previous["revisions"][arm] == REVISIONS[arm]
        dest = root / arm
        dest.mkdir()
        for name, target in (("production.tar", "production.tar"), ("source.tar.gz", "original-source.tar.gz"),
                             ("remote-stringlabels.test", "archived.test")):
            shutil.copy2(parent / arm / name, dest / target)
            assert scale.sha(dest / target) == previous["hashes"][f"{arm}/{name}"]
        source = dest / "source"
        source.mkdir()
        with tarfile.open(dest / "original-source.tar.gz") as archive:
            archive.extractall(source, filter="data")
        original = scale.inventory(source)
        assert original == previous["sources"][arm]["source_hashes"]
        for path in OVERLAY:
            shutil.copy2(overlay / Path(path).name, source / path)
        patched = scale.inventory(source)
        planned["overlays"][arm] = verify_overlay(original, patched)
        fixture_hashes = {p: patched[p] for p in OVERLAY}
        if reference is None:
            reference = fixture_hashes
        assert reference == fixture_hashes
        planned["sources"][arm] = dict(path=str(source), source_hashes=patched, original_hashes=original,
                                        original_entry=previous["inputs"][arm])
        subprocess.run(["tar", "-czf", str(dest / "source.tar.gz"), "-C", str(source), "."], check=True)
        with (dest / "build.txt").open("w") as log:
            subprocess.run(["go", "test", "-p=2", "-c", "-tags=stringlabels", "-o", str(dest / "rebuilt.test"), "./storage/remote"],
                           cwd=source, env=scale.environment(), stdout=log, stderr=subprocess.STDOUT, check=True)
        for name in ("production.tar", "original-source.tar.gz", "source.tar.gz", "archived.test", "rebuilt.test"):
            planned["hashes"][f"{arm}/{name}"] = scale.sha(dest / name)
    for filename in SCRIPTS:
        shutil.copy2(Path(__file__).with_name(filename), root / filename)
        planned["hashes"][filename] = scale.sha(root / filename)
    planned["tool_versions"] = [subprocess.check_output(args, text=True).strip() for args in
        (["go", "version"], ["go", "version", "-m", shutil.which("benchstat")])]
    assert "go1.27.1 linux/amd64" in planned["tool_versions"][0]
    assert "v0.0.0-20250305200902-02a15fd477ba" in planned["tool_versions"][1]
    scale.write(root / "expected-runs.json", planned)
    (root / "expected-runs.sha256").write_text(scale.sha(root / "expected-runs.json")+"\n")
    load(root, execution=True)
    print("Frozen", planned["counts"], flush=True)


def load(root, execution=False):
    assert scale.sha(root / "expected-runs.json") == (root / "expected-runs.sha256").read_text().strip()
    p = json.loads((root / "expected-runs.json").read_text())
    for k, v in json.loads(json.dumps(manifest())).items():
        assert p[k] == v, k
    expected = set(SCRIPTS) | {f"{a}/{n}" for a in ARMS for n in
        ("production.tar", "original-source.tar.gz", "source.tar.gz", "archived.test", "rebuilt.test")}
    assert set(p["hashes"]) == expected
    for name, digest in p["hashes"].items():
        assert scale.sha(root / name) == digest, name
    for arm in ARMS:
        s = p["sources"][arm]
        forwarding.verify_overlay(root / arm / "production.tar", s["original_entry"])
        assert s["original_entry"]["source_hashes"] == s["original_hashes"]
        assert s["original_entry"]["revision"] == REVISIONS[arm]
        scale.source_archive(root / arm, s)
        assert p["overlays"][arm] == verify_overlay(s["original_hashes"], s["source_hashes"])
        if execution:
            assert scale.inventory(Path(s["path"])) == s["source_hashes"]
    assert all(p["overlays"]["forwarding"][f]["patched"] == p["overlays"]["candidate"][f]["patched"] for f in OVERLAY)
    if execution:
        assert scale.host_identity() == p["host"], "Host changed"
        assert all(scale.sha(Path(__file__).with_name(f)) == p["hashes"][f] for f in SCRIPTS)
    return p


def artifacts(m):
    return dict(cpu=("cpu.pprof",), trace=("execution.trace",), alloc=("alloc.pprof",),
                locks=("mutex.pprof", "block.pprof")).get(m["kind"], ())


def binary(m):
    return f"{m['arm']}/{m['variant']}.test"


def environment(m, folder):
    env = history.environment(m)
    mode = dict(accounting="accounting", cpu="cpu:"+m["phase"], trace="trace", alloc="control", locks="control").get(m["kind"], "")
    env[ENV] = mode
    env[ENV+"_BACKOFF"] = "5ms" if m["scenario"] == "capped" else ""
    env[ENV+"_OUTPUT"] = str(folder / artifacts(m)[0]) if m["kind"] in ("cpu", "trace") else ""
    return env


def command(root, folder, m):
    args = [str(root / binary(m)), "-test.run=^$", "-test.bench="+"/".join("^"+re.escape(s)+"$" for s in history.name(m).split("/")),
            "-test.benchtime=1x", "-test.benchmem", "-test.timeout=10m"]
    if m["kind"] == "alloc":
        args += ["-test.memprofile="+str(folder / "alloc.pprof")]
    if m["kind"] == "locks":
        args += ["-test.mutexprofile="+str(folder / "mutex.pprof"), "-test.mutexprofilefraction=1",
                 "-test.blockprofile="+str(folder / "block.pprof"), "-test.blockprofilerate=1"]
    return args


def normalize_args(args):
    return [Path(x).name if i == 0 else re.sub(r"(-test.(?:mem|mutex|block)profile=).*/", r"\1", x) for i, x in enumerate(args)]


def recorded_env(env):
    return {k: Path(v).name if k == ENV+"_OUTPUT" and v else v for k, v in env.items()
            if k in scale.ENV or k.startswith("PROMETHEUS_METADATA_PIPELINE_")}


def parse_output(content, m):
    result = history.parse_output(content, m)
    matches = re.findall(r"metadata-pipeline-diagnostics: (.+)", content)
    assert len(matches) == (m["kind"] != "plain"), "Missing, duplicate, or wrong diagnostic mode"
    metrics, r = result["metrics"], result["result"]
    for name in ("Initialization", "Ingestion", "BacklogWait", "ReleaseToDrain", "Drain", "Shutdown", "DBClose", "Completion"):
        metrics[name+"-ms"] = r[name]/1e6
    metrics["receiver-cpu-ns/sample"] = (r["ReceiverCPU"]["User"]+r["ReceiverCPU"]["System"])/r["Samples"]
    if not matches:
        return result
    d = json.loads(matches[0])
    assert d["Accounting"] == (m["kind"] == "accounting")
    assert d["BackoffCap"] == (5000000 if m["scenario"] == "capped" else 0)
    assert d["CPUPhase"] == (m["phase"] if m["kind"] == "cpu" else "")
    assert d["Profiled"] == (m["kind"] == "cpu") and d["Trace"] == d["Traced"] == (m["kind"] == "trace")
    assert [p["Name"] for p in d["Phases"]] == PHASES
    last = 0
    for p in d["Phases"]:
        start, end = p["Start"], p["End"]
        assert last <= start["Started"] <= start["Finished"] <= end["Started"] <= end["Finished"]
        last = end["Finished"]
        if not d["Accounting"]:
            continue
        n = p["Name"]
        metrics[n+"-observed-ms"] = (end["Started"]-start["Finished"])/1e6
        metrics[n+"-accounting-ms"] = (end["Finished"]-end["Started"]+start["Finished"]-start["Started"])/1e6
        for prefix, s, e in (("sender", start, end), ("receiver", start["Receiver"], end["Receiver"])):
            assert s["CPU"]["Available"] and e["CPU"]["Available"]
            metrics[f"{n}-{prefix}-cpu-ms"] = sum(e["CPU"][v]-s["CPU"][v] for v in ("User", "System"))/1e6
            for field in ("AllocatedBytes", "Allocations", "GCPause", "GC"):
                delta = e["Memory"][field]-s["Memory"][field]
                assert delta >= 0
                metrics[f"{n}-{prefix}-{field}"] = delta
    events = d["Events"]
    assert all(a["At"] <= b["At"] for a, b in zip(events, events[1:]))
    names = [e["Name"] for e in events]
    required = ["writer-complete", "drain-complete"]
    if m["scenario"] != "cold":
        required += ["release-command-start", "release-command-complete"]
    assert all(names.count(n) == 1 for n in required)
    progress = d["Progress"] or []
    assert bool(progress) == d["Accounting"] and len(progress) <= 30001
    if progress:
        expected = r["Samples"] + (0 if m["scenario"] == "cold" else r["Config"]["Series"])
        assert progress[-1]["Acknowledged"] == expected and progress[-1]["Pending"] == 0
        assert progress[-1]["RecordsRead"].get("samples", 0) > 0
        for i, p in enumerate(progress):
            assert 0 <= p["Started"] <= p["Finished"] and 0 <= p["Acknowledged"] <= expected and p["Pending"] >= 0
            if i:
                before = progress[i-1]
                assert before["Finished"] <= p["Started"]
                assert all(before[k] <= p[k] for k in ("Acknowledged", "HeldRequests", "EnqueueRetries"))
                assert all(v <= p["RecordsRead"].get(k, 0) for k, v in before["RecordsRead"].items())
    assert all(math.isfinite(v) and v >= 0 for v in metrics.values())
    result["diagnostics"] = d
    return result


def profile_info(path):
    assert path.is_file() and path.stat().st_size > 0, "Missing or empty profile"
    if path.suffix == ".trace":
        subprocess.run(["go", "tool", "trace", "-pprof=sched", str(path)], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, check=True)
        return dict(valid=True)
    raw = subprocess.check_output(["go", "tool", "pprof", "-raw", str(path)], text=True, stderr=subprocess.PIPE)
    assert "Samples:" in raw and "Locations" in raw, "Malformed profile"
    # The first CPU value is samples/count, not nanoseconds. A valid empty
    # profile is retained, distinct from corruption or a missing artifact.
    if path.name == "cpu.pprof":
        assert re.search(r"samples/count\s+cpu/nanoseconds", raw)
        section = raw.split("Samples:", 1)[1].split("Locations", 1)[0]
        samples = sum(int(v) for v in re.findall(r"^\s*(\d+)\s+\d+:.*$", section, re.M))
        return dict(valid=True, samples=samples)
    return dict(valid=True)


def run_one(root, folder, m, planned):
    assert scale.host_identity() == planned["host"]
    folder.mkdir()
    assert scale.sha(root / binary(m)) == planned["hashes"][binary(m)]
    env, args = environment(m, folder), command(root, folder, m)
    before, activity, started = scale.snapshot(), [scale.processes(-1)], time.time()
    with (folder / "stdout.txt").open("w") as out, (folder / "stderr.txt").open("w") as err:
        proc = subprocess.Popen(args, cwd=planned["sources"][m["arm"]]["path"], env=env, stdout=out, stderr=err, start_new_session=True)
        while proc.poll() is None:
            activity.append(scale.processes(proc.pid))
            if time.time()-started > 660:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait()
                break
            time.sleep(.5)
    after = scale.snapshot()
    assessment = scale.host_activity(before, after, activity, 2)
    record = dict(member=m, binary_sha256=planned["hashes"][binary(m)], command=args, environment=recorded_env(env),
        before=before, after=after, activity=activity, started=started, ended=time.time(), host=scale.host_identity(),
        exit=proc.returncode, profiles={p: scale.sha(folder / p) for p in artifacts(m) if (folder / p).is_file()}, **assessment)
    record.update({s+"_sha256": scale.sha(folder / (s+".txt")) for s in ("stdout", "stderr")})
    scale.write(folder / "record.json", record)
    validate(root, folder, m, planned)
    return assessment


def validate(root, folder, m, planned):
    r = json.loads((folder / "record.json").read_text())
    assert r["member"] == m and r["exit"] == 0 and r["host"] == planned["host"]
    assert r["binary_sha256"] == planned["hashes"][binary(m)]
    assert normalize_args(r["command"]) == normalize_args(command(root, folder, m))
    assert r["environment"] == recorded_env(environment(m, folder))
    assert math.isfinite(r["ended"]-r["started"]) and r["ended"] >= r["started"]
    assessment = scale.host_activity(r["before"], r["after"], r["activity"], 2)
    assert all(r[k] == assessment[k] for k in assessment)
    assert set(r["profiles"]) == set(artifacts(m)), "Missing or unexpected profiles"
    assert {p.name for p in folder.iterdir()} == {"record.json", "stdout.txt", "stderr.txt"} | set(artifacts(m))
    for stream in ("stdout", "stderr"):
        assert r[stream+"_sha256"] == scale.sha(folder / (stream+".txt"))
    profiles = {}
    for name, digest in r["profiles"].items():
        assert scale.sha(folder / name) == digest, "Corrupt profile"
        profiles[name] = profile_info(folder / name)
    return dict(member=m, host=assessment, profiles=profiles, folder=folder.relative_to(root).as_posix(),
                **parse_output((folder / "stdout.txt").read_text(), m))


def smoke(root):
    p = load(root, execution=True)
    (root / "smoke").mkdir()
    for i, m in enumerate(smoke_members(p)):
        print("Smoke", i+1, m, flush=True)
        run_one(root, root / "smoke" / str(i), m, p)
    (root / "SMOKE_COMPLETE").touch()


def validate_smoke(root, p):
    assert (root / "SMOKE_COMPLETE").is_file()
    members = smoke_members(p)
    assert {f.name for f in (root / "smoke").iterdir()} == {str(i) for i in range(len(members))}
    for i, m in enumerate(members):
        validate(root, root / "smoke" / str(i), m, p)


def run(root):
    p = load(root, execution=True)
    validate_smoke(root, p)
    (root / "results").mkdir()
    for ordinal, block in enumerate(p["blocks"]):
        dest = root / "results" / block["id"]
        dest.mkdir(parents=True)
        for attempt in (0, 1):
            folder = dest / f"attempt-{attempt}"
            folder.mkdir()
            noise, warnings = [], []
            for i, m in enumerate(block["members"]):
                status = dict(block=ordinal+1, total=len(p["blocks"]), id=block["id"], member=m, attempt=attempt, time=time.time())
                scale.write(root / "progress.json", status)
                print(status, flush=True)
                result = run_one(root, folder / str(i), m, p)
                noise += result["noise"]
                warnings += result["warnings"]
            scale.write(folder / "block.json", dict(block=block, attempt=attempt, noise=noise, warnings=warnings))
            if not noise:
                scale.write(dest / "accepted.json", dict(attempt=attempt))
                break
            if attempt:
                scale.write(root / "INCOMPLETE.json", dict(block=block, reasons=noise))
                raise RuntimeError("Local interference recurred; attempts preserved")
    collect(root, p)
    load(root, execution=True)
    (root / "MEASUREMENT_COMPLETE").touch()


def collect(root, p):
    rows, exclusions, expected = [], [], set()
    for block in p["blocks"]:
        dest = root / "results" / block["id"]
        accepted = json.loads((dest / "accepted.json").read_text())["attempt"]
        assert accepted in (0, 1)
        assert {d.name for d in dest.iterdir()} == {"accepted.json"} | {f"attempt-{i}" for i in range(accepted+1)}
        for attempt in range(accepted+1):
            folder = dest / f"attempt-{attempt}"
            assert {d.name for d in folder.iterdir()} == {"block.json"} | {str(i) for i in range(len(block["members"]))}
            noise, warnings = [], []
            for i, m in enumerate(block["members"]):
                expected.add(folder / str(i) / "record.json")
                parsed = validate(root, folder / str(i), m, p)
                noise += parsed["host"]["noise"]
                warnings += parsed["host"]["warnings"]
                if accepted == attempt:
                    rows.append(dict(block=block["id"], cohort=block["cohort"], **parsed))
            assert json.loads((folder / "block.json").read_text()) == dict(block=block, attempt=attempt, noise=noise, warnings=warnings)
            assert bool(noise) == (attempt != accepted)
            if noise:
                exclusions.append(dict(block=block["id"], attempt=attempt, reasons=noise))
    assert dict(Counter(r["member"]["kind"] for r in rows)) == p["counts"]
    assert set((root / "results").rglob("record.json")) == expected
    return rows, exclusions


def comparisons(rows):
    groups = {}
    for r in rows:
        if not r["cohort"]:
            continue
        m = r["member"]
        rep = int(r["block"].split("/")[2])
        comparisons = [(m["scenario"], f"arms-{m['variant']}-{m['kind']}", m["arm"], ARMS)]
        if m["kind"] == "plain":
            comparisons.append((m["scenario"], "bridge-"+m["arm"], m["variant"], ("archived", "rebuilt")))
        if m["kind"] == "accounting" and m["scenario"] in ("backlog", "capped"):
            comparisons.append(("backoff-control", "control-"+m["arm"], m["scenario"], ("backlog", "capped")))
        for scenario, label, arm, pair in comparisons:
            key = (r["cohort"], scenario, label, pair)
            cell = groups.setdefault(key, {}).setdefault(arm, {})
            assert rep not in cell
            cell[rep] = r["metrics"]
    summaries = []
    for (cohort, scenario, label, pair), arms in sorted(groups.items()):
        assert set(arms) == set(pair) and all(set(a) == set(range(6)) for a in arms.values())
        units = set(arms[pair[0]][0])
        assert all(set(v) == units for a in arms.values() for v in a.values())
        summaries.append(dict(cohort=cohort, scenario=scenario, comparison=label, pair=pair,
            metrics={unit: forwarding.comparison({a: [arms[a][i][unit] for i in range(6)] for a in pair}, pair) for unit in sorted(units)}))
    return summaries, groups


def analyze(root):
    p = load(root)
    assert (root / "MEASUREMENT_COMPLETE").is_file()
    validate_smoke(root, p)
    rows, exclusions = collect(root, p)
    out = root / "analysis"
    out.mkdir(exist_ok=True)
    summaries, groups = comparisons(rows)
    scale.write(out / "observations.json", rows)
    scale.write(out / "summary.json", summaries)
    scale.write(out / "coverage.json", dict(counts=p["counts"], exclusions=exclusions,
        warnings=[dict(block=r["block"], member=r["member"], warnings=r["host"]["warnings"]) for r in rows if r["host"]["warnings"]]))
    for (cohort, scenario, label, pair), arms in sorted(groups.items()):
        stem = f"cohort-{cohort}-{scenario}-{label}"
        files = []
        for arm in pair:
            path = out / f"{stem}-{arm}.txt"
            lines = ["BenchmarkPipeline-4 1 " + " ".join(f"{v:.12g} {k}" for k, v in sorted(arms[arm][i].items())) for i in range(6)]
            path.write_text("goos: linux\ngoarch: amd64\n"+"\n".join(lines)+"\n")
            files.append(path.name)
        with (out / (stem+".benchstat.txt")).open("w") as report:
            subprocess.run(["benchstat", *files], cwd=out, stdout=report, check=True)
    profile_groups = {}
    for r in rows:
        m = r["member"]
        if m["kind"] == "cpu":
            key = (m["arm"], m["scenario"], m["phase"])
            profile_groups.setdefault(key, []).append(r)
    profile_summary = []
    for (arm, scenario, phase), captures in sorted(profile_groups.items()):
        samples = sum(r["profiles"]["cpu.pprof"]["samples"] for r in captures)
        profile_summary.append(dict(arm=arm, scenario=scenario, phase=phase, captures=len(captures), samples=samples,
            ranking="sufficient" if samples >= p["cpu_sample_threshold"] else "insufficient",
            originals=[r["folder"]+"/cpu.pprof" for r in captures]))
    scale.write(out / "cpu-profile-coverage.json", profile_summary)
    print("Complete: cohorts, binary bridge, instrumentation, and short-profile limits remain separate.")


if __name__ == "__main__":
    action, root = sys.argv[1], Path(sys.argv[2]).resolve()
    if action == "freeze":
        freeze(root, Path(sys.argv[3]).resolve(), Path(sys.argv[4]).resolve())
    else:
        {"smoke": smoke, "run": run, "analyze": analyze}[action](root)

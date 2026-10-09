#!/usr/bin/env bash
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

set -euo pipefail

if [[ $# != 1 || -e "$1" ]]; then
  echo "Usage: bash scripts/benchmark-metadata-pipeline.sh NEW_RESULTS_DIRECTORY" >&2
  exit 1
fi
if [[ ! -f storage/remote/metadata_pipeline_bench_test.go ]]; then
  echo "Run from the repository root." >&2
  exit 1
fi
command -v benchstat >/dev/null
mkdir -p "$1"
results=$(cd "$1" && pwd)

# Pin the workload and runtime settings; a smoke-test override must not silently
# turn into a scored observation. All invocations below use this same binary.
export GOMAXPROCS=4 GOGC=100 GOMEMLIMIT=off GODEBUG="" GOWORK=off GOFLAGS=""
export PROMETHEUS_METADATA_PIPELINE_SERIES=10000
export PROMETHEUS_METADATA_PIPELINE_SWEEPS=200
export PROMETHEUS_METADATA_PIPELINE_RECEIVER_PROCS=2
export PROMETHEUS_METADATA_PIPELINE_HEAP=0
{
  date -u
  git rev-parse HEAD
  git status --short --branch
  go version
  go env GOOS GOARCH GOAMD64 GOARM64 GOEXPERIMENT CGO_ENABLED
  uname -a
  if [[ $(uname -s) == Darwin ]]; then
    sysctl hw.memsize hw.ncpu hw.model machdep.cpu.brand_string
  else
    lscpu
    free -b
  fi
  env | LC_ALL=C sort | sed -n '/^GODEBUG=/p; /^GOGC=/p; /^GOMEMLIMIT=/p; /^GOMAXPROCS=/p; /^PROMETHEUS_METADATA_PIPELINE_/p'
} >"$results/provenance.txt"
git diff --binary HEAD >"$results/worktree.patch"
tar -czf "$results/harness-source.tar.gz" \
  storage/remote/metadata_pipeline* \
  scripts/benchmark-metadata-pipeline.sh
go test -c -o "$results/remote.test" ./storage/remote
go version -m "$results/remote.test" >"$results/binary-buildinfo.txt"
shasum -a 256 "$results/remote.test" "$results/harness-source.tar.gz" >"$results/SHA256SUMS"

cases=(cold unchanged changes newseries backlog cardinality distinct)
modes=(wal native disabled)

run() {
  local destination=$1 workload=$2 mode=$3
  echo "$(date -u +%FT%TZ) $destination" | tee -a "$results/progress.txt"
  ps -Ao pid,pcpu,comm >"$destination.processes.txt"
  "$results/remote.test" -test.run='^$' \
    -test.bench="^BenchmarkRemoteWriteMetadataPipeline$/^case=$workload$/^series=[0-9]+$/^source=$mode$" \
    -test.benchtime=1x -test.benchmem -test.timeout=10m >"$destination.txt" 2>"$destination.stderr"
}

run_cohorts() {
  local prefix=$1
  shift
  local workloads=("$@")
  local cohort repetition case_index workload offset mode
  for cohort in 1 2; do
    mkdir "$results/$prefix-$cohort"
    for repetition in 1 2 3 4 5 6; do
      for case_index in "${!workloads[@]}"; do
        workload=${workloads[$case_index]}
        for offset in 0 1 2; do
          mode=${modes[$(((cohort + repetition + case_index + offset) % 3))]}
          run "$results/$prefix-$cohort/$workload-$mode-$repetition" "$workload" "$mode"
        done
      done
    done
    benchstat -col '/source@(wal native disabled)' -row /case,/series "$results/$prefix-$cohort/"*-[1-6].txt >"$results/$prefix-$cohort.benchstat.txt"
  done
}

run_cohorts cohort "${cases[@]}"
# Paced and high-diversity changing traces have their own reporting groups.
run_cohorts companion-cohort changes-distinct paced-unchanged paced-changes

# These are separate diagnostic cohorts, never pooled with the primary ones.
mkdir "$results/receiver-capacity" "$results/heap" "$results/profiles"
for repetition in 1 2 3 4 5 6; do
  for workload in unchanged distinct; do
    for mode in "${modes[@]}"; do
      for offset in 0 1; do
        receiver_procs=$((2 + 2 * ((repetition + offset) % 2)))
        export PROMETHEUS_METADATA_PIPELINE_RECEIVER_PROCS=$receiver_procs
        destination="$results/receiver-capacity/$workload-$mode-$receiver_procs-$repetition"
        run "$destination" "$workload" "$mode"
        # Benchstat treats this as a configuration key.
        printf '\nreceiver-procs: %d\n' "$receiver_procs" >"$destination.config.txt"
        sed '/^Benchmark/!d' "$destination.txt" >>"$destination.config.txt"
      done
    done
  done
done
benchstat -col receiver-procs -row /case,/source "$results/receiver-capacity/"*.config.txt >"$results/receiver-capacity.benchstat.txt"
export PROMETHEUS_METADATA_PIPELINE_RECEIVER_PROCS=2
export PROMETHEUS_METADATA_PIPELINE_HEAP=1
for workload in "${cases[@]}"; do
  for mode in "${modes[@]}"; do
    run "$results/heap/$workload-$mode" "$workload" "$mode"
  done
done
export PROMETHEUS_METADATA_PIPELINE_HEAP=0
for workload in unchanged changes; do
  for mode in wal native; do
    "$results/remote.test" -test.run='^$' \
      -test.bench="^BenchmarkRemoteWriteMetadataPipeline$/^case=$workload$/^series=10000$/^source=$mode$" \
      -test.benchtime=1x -test.timeout=10m \
      -test.cpuprofile="$results/profiles/$workload-$mode.cpu.pprof" \
      -test.memprofile="$results/profiles/$workload-$mode.alloc.pprof" \
      >"$results/profiles/$workload-$mode.txt" 2>"$results/profiles/$workload-$mode.stderr"
  done
done
date -u >"$results/COMPLETE"

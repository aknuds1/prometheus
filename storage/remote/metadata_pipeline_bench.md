# Remote-write metadata pipeline benchmark

`BenchmarkRemoteWriteMetadataPipeline` supplements the queue-manager microbenchmarks.
It compares native-only and WAL-metadata-only forwarding, plus a metadata-disabled
diagnostic baseline. All modes ingest identical observations through TSDB and send
samples through the actual WAL watcher, queue manager, Snappy encoder, and RW2 HTTP
client. Metadata is never injected into queue caches. The receiver validates every
delivery in a separate process; it does not retain request bodies.

## Running

Run correctness checks before measurement:

```sh
go test ./storage/remote -run '^TestRemoteWriteMetadataPipeline'
go test -race ./storage/remote -run '^Test(RemoteWriteMetadataPipeline|NativeMetadataWALDelivery)'
```

For reproducible measurements, close competing workloads and run from the repository
root with `benchstat` on `PATH` and permission to bind loopback ports:

```sh
bash scripts/benchmark-metadata-pipeline.sh /absolute/path/to/new-results-directory
```

The script builds one binary and records two independent cohorts of six fresh-process
observations per case/mode, rotating mode order. It also saves source provenance,
host/runtime settings, process snapshots, raw results, and separate `benchstat`
tables. Receiver-capacity checks, retained-heap passes, and profiles follow, and are
not pooled into either primary cohort. It stops on the first failed run and writes
`COMPLETE` only when all runs finish. Never compare measurements from different
binary builds as if their harness provenance were identical.

For an individual diagnostic run:

```sh
GOMAXPROCS=4 go test ./storage/remote -run '^$' \
  -bench '^BenchmarkRemoteWriteMetadataPipeline$/^case=unchanged$/^series=10000$/^source=(wal|native)$' \
  -benchtime=1x -benchmem
```

`PROMETHEUS_METADATA_PIPELINE_SERIES` changes the base cardinality (a multiple of
100); the cardinality case uses ten times that count. `PROMETHEUS_METADATA_PIPELINE_SWEEPS`
changes the default 200 measured sweeps (at most 400 to preserve native history).
Backlog always uses four sweeps. Counts below the queue's capacity are not suitable
for the backlog case. `PROMETHEUS_METADATA_PIPELINE_RECEIVER_PROCS` controls receiver
`GOMAXPROCS` (default 2). `PROMETHEUS_METADATA_PIPELINE_HEAP=1` enables diagnostic GCs;
its timings must not be used as throughput measurements. The cohort script pins
these settings, so diagnostic overrides cannot silently change its workload.

## Workloads

Defaults are 10,000 active series, six labels, 100 shared metadata values, 64-byte
help strings, and float samples. Each sweep appends one observation per active
series. Logical sample timestamps advance by 15 seconds. The original workloads
below are unpaced; the companion cases add independently paced ingestion.
There are four disjoint producers and four fixed shards, transactions of at most
1,000 samples, 2,000-sample request batches, and a 10,000-sample per-shard channel
capacity (excluding in-flight and partially filled batches). The 100 ms send deadline
is a fixture override, not the production default. Sender `GOMAXPROCS` is 4.

| Case | Measured trace |
| --- | --- |
| `cold` | Open an empty TSDB and sender, then create and deliver the first sweep. |
| `unchanged` | 200 sweeps with unchanged metadata supplied on every append. |
| `changes` | Change one of 100 groups each sweep: 1% of observations, twice per series. |
| `newseries` | Replace 1% of active identities each sweep; 10,000 active but 30,000 resident series at the end. |
| `backlog` | One writer/shard; hold receiver acknowledgements until four changing sweeps commit and the WAL reader hits queue backpressure, then release and drain. |
| `cardinality` | Unchanged observations over 100,000 active series and 100 metadata values. |
| `distinct` | Unchanged observations with a distinct metadata value per series. |

Warm cases first create one untimed sweep in ID order, establishing identical WAL
refs and shard assignments. Subsequent sweeps use concurrent producers. Cold
creation and later new-series creation are concurrent, so reference assignment and
request grouping can differ. Logical deliveries must match; compressed byte counts
and packet boundaries need not. Metadata-disabled output intentionally lacks metadata.

Each trace uses a fresh database. The sample WAL is always enabled and explicitly
uses Snappy, matching the Prometheus server default rather than the uncompressed
`tsdb.DefaultOptions()`. Compaction and sample-age filtering are disabled. Future
logical timestamps avoid startup filtering. No synthetic WAL notifications or queue
locks are used to schedule work. Native history must not evict any version.

## Measurement boundaries

One Go benchmark operation is an entire trace, not a sample. The principal interval
begins after warm-up (before opening the TSDB for `cold`) and ends only after all
producers finish, all expected data is acknowledged, and sender shutdown joins its
workers. An acknowledgement alone does not prove sender buffer cleanup is complete.
`samples/s` uses this interval, including drain and sender shutdown. Transaction
latencies cover append plus commit, not time waiting for remote delivery.

The JSON `metadata-pipeline-result` accompanying each observation records ingestion,
drain, shutdown, initialization, TSDB close, and lifecycle durations. TSDB close,
including its final WAL sync, is outside principal throughput but inside lifecycle
totals. Lifecycle totals also include warm-up and boundary instrumentation. Fixture
descriptor generation and receiver startup are outside both intervals. Ordinary WAL
writes and any segment-rotation work during ingestion remain measured.

CPU measurements use process user/system time on Darwin and Linux and exclude the
receiver. Other platforms omit CPU metrics. Sender allocations include its test
driver and control polling, not receiver decoding/validation. Receiver CPU and
decode/validation service time are reported separately; the latter excludes the
intentional acknowledgement hold. The receiver shares the host's hardware despite
being a separate process. Capacity diagnostics compare receiver `GOMAXPROCS=2` and 4.

WAL bytes are encoded record-part bytes, including framing/compression headers,
not filesystem allocation, padding, or physical disk traffic. Wire bytes are
compressed request-body bytes, excluding HTTP/TCP headers. Outstanding samples at
writer completion include unread WAL data; sampled queue occupancy does not.

Heap diagnostics force two GCs with the fixture kept alive, while the sender and
TSDB are still open. They report whole sender-process retained heap, including common
input descriptors, after draining and, for backlog, at queue backpressure with the
same committed outstanding sample count. They are not isolated metadata-ownership
measurements. Profiles are separate process-wide diagnostic runs, including setup
and teardown; they are not limited to the principal timed interval.

## Interpretation and follow-up

These are finite-trace pipeline comparisons, not sustained-capacity or
network/storage durability comparisons. Report final-drain delays rather than
hiding them with artificial notifications; the production watcher's polling timeout
can dominate short traces. A receiver-limited result or nonsignificant difference
does not establish sender parity. Interpret both independent cohorts and report
uncertainty instead of pooling them.

Warm WAL metadata is a legitimate steady state. The improvement over a preloaded
queue microbenchmark is that metadata checks, changes, initial population, WAL
encoding/reading, and cache maintenance are now observable in their appropriate
intervals. Native metadata remains Head-only; restart, eviction, checkpoints,
compaction, and long-term series cleanup are outside this comparison.

The companion cases described below do not change the original unpaced workloads.
The scoped Linux study below covers cold, unchanged, changing, and companion traces;
the remaining Linux cases are still outstanding. A subsequent study can examine
500,000 and one million active series, repeating smaller controls on
the same host and independently varying ingestion rate and metadata sharing. The
100,000-series case here also increases total sample work tenfold: it is not an
isolated cardinality experiment. These results neither supersede the narrower
microbenchmarks nor reopen adoption gates for parked forwarding prototypes.

## Paced and high-diversity companions

The runner records two additional, independent six-observation cohorts under
`companion-cohort-*`, using all three metadata modes. Keep these separate from
the original cohorts and from historical results produced with older harnesses.

| Case | Difference from the original trace |
| --- | --- |
| `changes-distinct` | The 1%-changing burst workload, with 10,000 distinct initial metadata values instead of 100 shared values. |
| `paced-unchanged` | Unchanged metadata, with each writer starting a sweep every 20 ms. |
| `paced-changes` | The 1%-changing workload with the same 20 ms pacing. |

Paced writers share an absolute schedule, staggered by 5 ms, after the usual
unpaced seed and drain. They do not wait for receiver acknowledgements between
sweeps. A late writer catches up without dropping samples or shifting subsequent
deadlines. The nominal offered rate is 500,000 samples/second with 10,000 active
series: compressed-time pacing, not a real 15-second scrape cadence or a sender
capacity measurement. Logical timestamps still advance by 15 seconds.

Paced results add actual ingestion rate, scheduling p50/p99/maximum lateness,
and the number of writer sweeps at least one full interval late. Existing delivered
rate, outstanding-at-writer-end, sampled queue occupancy, and drain metrics remain
available. Preserve late observations and report the deviation from the offered
schedule; a late trace must not silently be described as keeping pace. Scheduling
waits are cancellable and do not count toward append/commit transaction latency.

`BenchmarkHeadMetricMetadataLookup` supplies a complementary fixed-history lookup
control with 100 shared, 1,000 distinct, 4,096 distinct, or four oversized historical
metadata values. Every series has the same current value, so the current controls
do not measure distinct current metadata. It covers current/history/missing/disabled
states and independent parallel readers sharing one Head. Each operation visits
4,096 series; this isolates lookup and value materialization rather than WAL,
encoding, or HTTP delivery. The existing append/encode benchmarks retain their
original loop shapes.

The `historical-full` cases select the oldest of five retained versions, with
shared or 4,096-distinct historical values. `missing-full` scans the same full
history for a timestamp before its oldest version. Both include serial and
parallel readers; existing two-version cases are unchanged.
`BenchmarkHeadMetricMetadataLookupAppendConcurrent/lookup=oldest/changing=true/destinations=2`
adds the corresponding full-history lookup while eight ingestion workers replace
versions and wait for two lookup destinations before advancing their own series.

## Linux findings (2026-09-20)

Measured on Debian 13.7, an eight-vCPU Xeon Platinum 8358 guest with 16 GB RAM,
Go 1.27.1 linux/amd64, stringlabels, `GOAMD64=v1`, and CGO enabled. Sender/receiver
`GOMAXPROCS` were 4/2, with `GOGC=100`, no memory limit, and the default fixture
settings above. Production code was frozen at `9b7356d4703046d5feecbcf8ce41fd6dbf86cf60`;
both builds used identical additional benchmark fixtures. No production change
resulted from this experiment.

Two separate studies each used two independent cohorts of six fresh-process
observations per case/variant, with alternating revisions or rotating modes:

- 240 observations screened a frozen historical-copy-cache candidate against the
  original implementation: three native pipeline cases and seven lookup controls.
- 216 observations compared the original native, WAL, and disabled implementations
  across the six pipeline cases below. Cold measured 10,000 samples; warm traces
  measured two million, with the initial 10,000 also counted in lifecycle totals.

All 456 observations passed delivery and coverage validation. No blocks required
exclusion or retry for CPU steal, swapping, or competing compilation/tests.
Results were not pooled across studies, cohorts, or platforms. Each bracket below
is the minimum/maximum of six paired ratios, a pointwise order-statistic bound,
not simultaneous coverage. Separate `benchstat` tables accompany the raw evidence.
Paired-ratio medians need not equal `benchstat`'s ratios of medians.

### Native versus WAL metadata

Sender CPU/sample change, expressed as native relative to WAL:

| Case | Cohort 1: paired median [bounds] | Cohort 2: paired median [bounds] |
| --- | ---: | ---: |
| `cold` | -9.8% [-39.9%, +10.3%] | +0.7% [-8.0%, +31.1%] |
| `unchanged` | +0.4% [-1.2%, +7.6%] | -0.9% [-3.0%, +2.4%] |
| `changes` | +5.4% [+3.5%, +13.7%] | +7.1% [+4.7%, +11.4%] |
| `changes-distinct` | +26.1% [+21.7%, +28.7%] | +24.6% [+17.7%, +28.1%] |
| `paced-unchanged` | -3.2% [-4.6%, +4.0%] | -2.0% [-3.2%, -0.8%] |
| `paced-changes` | -5.5% [-10.0%, -3.9%] | -4.6% [-9.1%, -1.6%] |

Allocation medians in bytes/sample, shown as cohort 1 / cohort 2. Disabled mode
is diagnostic: it intentionally does not deliver metadata.

| Case | Disabled | WAL | Native |
| --- | ---: | ---: | ---: |
| `cold` | 3271.0 / 3252.4 | 3569.0 / 3667.7 | 3527.9 / 3513.1 |
| `unchanged` | 22.50 / 22.28 | 25.98 / 26.13 | 26.14 / 26.10 |
| `changes` | 22.31 / 22.50 | 30.48 / 30.65 | 42.94 / 42.83 |
| `changes-distinct` | 22.29 / 22.35 | 32.00 / 31.87 | 68.70 / 69.17 |
| `paced-unchanged` | 20.19 / 20.32 | 23.43 / 23.37 | 23.49 / 23.34 |
| `paced-changes` | 20.19 / 20.25 | 25.41 / 25.51 | 24.93 / 24.72 |

Unchanged bursts show no consistently established CPU advantage for either mode;
cold CPU results are variable. Changing bursts cost more with native metadata,
especially with distinct values. Paced changes instead use less native sender CPU
in both cohorts. This is workload-sensitive evidence, not general forwarding parity
or a sender-capacity result.

The native changing bursts finish ingestion in about 275-326 ms with roughly
1.83 million samples outstanding, then spend about 1.69-1.95 seconds draining.
Paced traces ingest in about 4.00 seconds with 8,000 samples outstanding and drain
in about 11 ms. Median pacing p99 lateness is approximately 1.06 ms; across all
72 paced observations, maximum lateness was 3.46 ms and no sweep was 20 ms late.
These different backlogs are important context; the burst result does not isolate
lookup CPU.
Sender shutdown medians are below 0.3 ms; warm DB close medians are 6-9 ms.

Warm sample-WAL bytes are about 4.045/sample in native and disabled modes. Changing
WAL metadata adds about 0.149/sample with shared values or 0.170/sample with distinct
values. Native avoids that WAL work, but it does not persist metadata. Native changing
bursts use about 4-8% more compressed wire bytes than WAL mode despite identical
logical deliveries; request grouping and compression need not match. Paced changing
wire bytes are about 12.27/sample in both modes. Lifecycle, receiver CPU, allocation
counts, phase timings, and backlog measurements are retained in the full report.

### Historical-copy cache: not adopted

The frozen candidate used a bounded Head-owned FIFO cache of immutable historical
metadata copies. It reduced shared-value changing-pipeline allocated bytes by
30.9% / 31.3%, but CPU reductions of 3.5% / 0.8% did not meet the required 5%
improvement bound. It increased distinct-changing pipeline CPU by 4.0% / 6.3% and
4,096-value historical serial lookup time by 90.8% / 90.5% (parallel: 24.8% / 24.5%).
Several 5% regression-protection bounds also failed. Linux therefore confirms the
decision from the earlier, separately measured Mac screen to leave this cache out.
The candidate was not retuned; broader adoption and retained-memory gates were not run.

The evidence archive `native-metadata-linux.D6L8QF/evidence.tar.gz` contains the
frozen source trees, binaries and hashes, expected-run manifest, runner/analyzer,
monitoring, validation logs, raw results, per-cohort `benchstat` tables, and `REPORT.md`.
Its SHA-256 is recorded alongside it. This was a scoped run, not a run of the entire
repository benchmark script. It did not cover Linux `newseries`, held backlog,
unchanged distinct metadata, 100,000-series sensitivity, receiver-capacity variants,
heap/profiles, the full transport-only matrix, or larger-cardinality studies.

### Historical point selection: not adopted (2026-09-20)

A subsequent experiment compared `d0f862d2a1a6291b18bcb4922b586c6d80ca68d1`
against direct historical point selection under the metadata stripe read lock,
copying only the selected handle instead of a complete history snapshot. The
publication barrier, current-value fast path, materialization, and query snapshots
were semantically unchanged. Both revisions used identical additional fixtures.
The host, kernel (`6.12.107+deb13-amd64`), Go toolchain, and runtime settings were
unchanged from the earlier Linux study.

Two separate six-pair fresh-process cohorts covered 18 lookup cases, three
concurrent lookup/append cases, and six pipeline cases: native unchanged, changing,
distinct-changing, and paced-changing; WAL changing; and disabled unchanged.
All 648 scored observations passed validation, with no host-noise exclusions or
retries. Lookup/concurrency runs used one-second measurement targets; each pipeline
run delivered a two-million-sample trace. Revision order alternated within pairs.

Selected candidate/baseline changes are paired-ratio medians with minimum/maximum
paired ratios in brackets, not simultaneous confidence bounds:

| Case / metric | Cohort 1 | Cohort 2 |
| --- | ---: | ---: |
| Shared historical serial lookup time | -2.62% [-4.07%, -2.08%] | -5.14% [-7.95%, -2.71%] |
| 4,096-distinct historical serial lookup time | -3.11% [-4.76%, -1.95%] | -2.81% [-3.40%, -1.62%] |
| Current-value serial lookup time | +4.26% [+3.02%, +7.63%] | +3.71% [+2.10%, +4.85%] |
| Native unchanged pipeline CPU/sample | +1.21% [-2.38%, +2.48%] | -1.67% [-4.48%, +3.52%] |
| Native changing pipeline CPU/sample | -1.65% [-5.59%, +3.40%] | -1.30% [-3.10%, +4.21%] |
| Native distinct-changing pipeline CPU/sample | +1.29% [-0.62%, +6.83%] | +1.60% [-2.64%, +4.93%] |
| Native paced-changing pipeline CPU/sample | +0.26% [-1.06%, +3.03%] | -0.78% [-2.83%, +1.21%] |

Both predeclared historical serial primaries passed the improvement gate: at least
2% paired-median improvement and every pair faster in both cohorts. Adoption also
required every timing and allocation control's worst paired ratio to remain within
5%; zero-allocation controls had to remain zero. Fourteen case/metric/cohort controls
failed that bound, including current-value lookup, parallel lookup, concurrent
current lookup, and changing-pipeline completion. Several failures had favorable
medians, so failure of the conservative gate does not itself establish a regression.
The current-value serial slowdown was consistent across both cohorts, but its cause
was not isolated. The candidate was rejected without retuning or additional scored
runs; **no production optimization from this experiment is retained**.

Full-five-version serial lookup gains were smaller: shared -0.62% / -1.11%,
4,096-distinct -1.86% / -2.00%. Concurrent oldest-version lookup changed by
-0.46% / -1.00%. Lookup allocation counts were unchanged, including zero-allocation
serial current/missing/disabled controls. These results do not establish a pipeline
win or forwarding parity with WAL metadata.

Before scoring, 50 separate diagnostic traces collected sender CPU/allocation
profiles for native and WAL unchanged, changing, distinct-changing, and
paced-changing workloads, plus block/mutex profiles for distinct-changing workloads.
Each diagnostic process ran five traces, including Go's calibration trace; profiles
include setup and teardown and are not scored phase measurements. Historical value
materialization accounted for about 19.5% of sampled allocation bytes with shared
changing values and 33.8% with distinct changing values. Selection accounted for
about 9.9% of sampled CPU cumulatively in the shared-changing native profile, which
includes its callees rather than isolating snapshot copying. In the distinct-changing
native CPU profile, RW2 time-series population accounted for 24.0%, selection 7.7%,
materialization 4.6%, background GC marking 3.6%, and GC assists 0.2%. These cumulative
figures overlap and do not attribute all GC work to metadata. Large allocation
savings therefore would not imply a proportional sender-CPU improvement.

In the separate distinct-changing native blocking profile, selection accumulated
about 0.73 seconds of waiting across five traces; semaphore acquisition across all
callers accumulated 2.34 seconds. Mutex contention was dominated by WAL logging
(about 158 ms of 166 ms total), rather than metadata publication. Blocking totals
include concurrent and background goroutine waits, not elapsed time or removable
CPU cost. These diagnostics do not establish publication waits as the principal
source of the native-versus-WAL CPU gap.

The findings leave historical value ownership/materialization as a candidate for
separate future work, not a demonstrated fix or a reason to reopen the rejected copy
cache. They do not complete the other pipeline, retained-heap, receiver-capacity,
transport-only, or larger-cardinality studies listed above.

The archive `native-metadata-point-lookup.yzvd7i/evidence.tar.gz` preserves the rejected
patch, both source trees and binaries, validation and profile outputs, the frozen
648-run manifest and gates, monitoring, raw observations, separate `benchstat`
tables, and `REPORT.md`. SHA-256:
`936f76e47336f8c779ef703a1e1b9f48c2f0becae559b8a4bca0d3ab42ee85cf`.

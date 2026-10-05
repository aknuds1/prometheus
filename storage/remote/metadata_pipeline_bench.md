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
its timings must not be used as throughput measurements.
`PROMETHEUS_METADATA_PIPELINE_TIMEOUT_MINUTES` bounds one run (default 5). The cohort script pins
these settings, so diagnostic overrides cannot silently change its workload.

## Workloads

### Bounded Linux scale study

`BenchmarkRemoteWriteMetadataPipelineScale` keeps the original benchmark unchanged
and separates history-preserving, equal-work, held-backlog, and receiver-capacity
traces. Its cardinalities and sweep counts are fixed; the original benchmark's
`SERIES` and `SWEEPS` environment overrides do not apply.

| Group | Metadata | Series | Measured sweeps |
| --- | --- | --- | --- |
| `history` | Unchanged/changing, each with 100 shared values or distinct values per series. | 10,000 and 100,000 | 200 |
| `equal-work` | Unchanged, shared/distinct. | 100,000 | 20 |
| `backlog` | Shared/distinct; every series changes each sweep. | 10,000 and 100,000 | 4 |
| `capacity` | Unchanged/changing, shared/distinct; unpaced diagnostics. | 100,000 | 200 |

History and equal-work traces offer 500,000 samples/second at absolute transaction
deadlines, with four staggered writers. Late transactions catch up without dropping
observations or shifting deadlines. Transactions contain 500 samples: 1,000-sample
transactions would produce 20% more commits/sample at 10,000 series because each
writer's 2,500-series partition ends in a half-full transaction. Capacity diagnostics
use the same 500-sample transactions without pacing. Backlog retains the original
one-writer/one-shard, 1,000-sample-transaction settings. Other queue settings match
the original benchmark.

History traces preserve 200 observations and, when changing, two changes per
series. Their 2M/20M samples take about 4/40 seconds before final drain. Equal-work
traces match the smaller trace's total samples and scheduled duration, but not its
per-series sample/chunk history. Neither comparison isolates cardinality perfectly.
Compare native/WAL within each cell before interpreting the scale difference.

The dedicated runner uses Python 3.11+ on Linux, with Go and `benchstat` on `PATH`.
After correctness/race/lint validation, run from the repository root, placing the
new results directory outside the source tree:

```sh
python3 scripts/benchmark-metadata-pipeline-scale.py selftest
python3 scripts/benchmark-metadata-pipeline-scale_test.py
python3 scripts/benchmark-metadata-pipeline-scale.py freeze /absolute/path/to/new-scale-results
python3 scripts/benchmark-metadata-pipeline-scale.py smoke /absolute/path/to/new-scale-results
python3 scripts/benchmark-metadata-pipeline-scale.py run /absolute/path/to/new-scale-results
python3 scripts/benchmark-metadata-pipeline-scale.py analyze /absolute/path/to/new-scale-results
```

The frozen manifest specifies 504 scored observations across two separate cohorts
of six fresh-process observations/cell/mode. Related cardinality and equal-work
traces form comparison blocks, with balanced mode/trace order. Each process runs
exactly one trace. Separate diagnostics comprise 108 heap traces, 48 receiver P2/P4
capacity traces, and eight process-wide CPU/allocation profiles. Six real-trace smoke
checks validate measurement/parser behavior before scoring. No results are pooled
with another cohort, diagnostics, or an earlier harness build.

Sender CPU/sample is the primary comparison; capped completion rates do not measure
maximum throughput. JSON results include transaction counts and scheduling lateness.
`BacklogWait` measures writer completion to receiver release; `ReleaseToDrain` starts
immediately before issuing release and ends at acknowledged delivery with no pending
samples. Their sum equals `Drain`; receiver-control overhead is included.

Scale heap diagnostics additionally record `SeededHeap` after warm-up. Timing
buffers are allocated before warm-up and retained across all heap checkpoints.
The seed/backlog/drained figures still include input descriptors, sample storage,
and live sender buffers, not isolated metadata ownership. Diagnostic GC timings are
never performance observations. Unchanged distinct-value controls distinguish
diversity from churn; receiver P2/P4 checks indicate capacity sensitivity, not proof
of unlimited receiver headroom.

The runner freezes source/binary hashes, configuration, host boot/kernel identity,
and raw results. In schema 2, CPU steal is informational: aggregate/per-vCPU values
and monitoring duration are retained, with warnings above 1%, but steal never causes
exclusion, retry, or termination. Lateness and saturation are also results, not
exclusion reasons. Correctness/provenance failures still stop execution. Overlapping
builds/tests/package installation or swap activity reject a whole comparison block,
with one replacement allowed before stopping. Legacy schema-1 evidence retains its
original CPU-steal rejection policy.

An explicit continuation can finish an interrupted schema-1 study without rebuilding
its benchmark binary or altering its evidence:

```sh
python3 scripts/benchmark-metadata-pipeline-scale.py continue /absolute/path/to/old-results /absolute/path/to/new-results
```

The destination must be new and separate from the old results and source. The runner
validates and copies the entire parent results directory, freezes its inherited
block/attempt mapping and remaining blocks, and repeats six smoke traces separately.
It then reruns the entire stopped block and completes the remaining original order
under schema 2. Old excluded attempts stay excluded. Host boot/kernel identity and
the original source and binary must still match; the new control runner is frozen
separately. This is not an arbitrary resume or chained-continuation mechanism.

Completion requires exact combined coverage, not just the end of the new segment.
Analysis checks legacy and new records under their respective policies, tags each
observation's segment, and supplies separate tables for mixed cohorts' segments as
well as the combined table. A mixed-policy cohort is not a fresh uniform-policy
replication. CPU-steal warnings remain in analysis without filtering observations.
Keep the entire child directory (including `parent`), runner, and validation evidence
together in a checksummed archive. Analysis works after relocation without the live
source checkout or access to the droplet.

### Original workloads

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

## Batched and restart companions

| Case | Difference from the original trace |
| --- | --- |
| `batched` | The 1%-changing workload, with 10 consecutive steps of 100 series per transaction, as in RW2 and OTLP requests. Transactions still hold 1,000 samples. |
| `restart` | Change every series' metadata in each of the first four sweeps, then leave it unchanged. After sweep `PROMETHEUS_METADATA_PIPELINE_RESTART_STEP` (default 30), checkpoint the WAL and restart TSDB and sender. Only the sweeps after the restart are measured. |

WAL-only forwarding writes each transaction's metadata records before its samples
and keeps only the latest value per series. The receiver therefore expects every
WAL-only sample in a batched transaction to carry the transaction's final version,
and every native sample to carry the version at its own timestamp.

Before the restart, a single writer appends in ID order, so segment cuts and
checkpoint contents are identical across runs. Segments are 32 KiB per 10,000
series, and truncation at the midpoint timestamp checkpoints about two thirds of
them. The checkpoint must include the whole metadata history. While draining
the pre-restart sweeps, the fixture notifies the watcher every 10 ms: a watcher
that moves to a new segment otherwise waits up to 15 s to read it. Reopened
storage writes default-size segments, so measured sweeps neither roll over nor
use synthetic notifications. Producers forget their series refs at the restart.

The result reports truncation and replay wall time, the WAL truncation summary,
the replay duration gauge, checkpoint file bytes, and decompressed checkpoint
record bytes by type. Heap passes add the peak sampled heap during truncation and
the retained heap after replay, before the sender opens. Every run also reports
decompressed WAL record bytes by type after the database closes, including the
pre-restart segments that remain after truncation.

## Per-run oracles and accounting

Every run checks the metadata it leaves in the WAL against a model of the
trace. The model covers every transaction's entries, their records and their
exact decompressed payload, using the series refs that the WAL's series
records actually assign, so concurrent series creation needs no assumed order.
Builds describe their own semantics: by default, WAL-only sources log a legacy
entry for each sample whose metadata differs from the series' committed
metadata, and other sources log none. A restart run checks only the segments
written after the restart. It checks the checkpoint separately: every series is
kept, and its metadata holds the whole history the build retains. Unknown-kind
entry counters must be zero.

Results report compressed WAL bytes for the seed (`SeedWALBytes`), the measured
phase (`WALBytes`) and the whole run (`LifecycleWALBytes`). In restart runs, the
whole run includes the bytes written before the restart
(`Restart.PreRestartWALBytes`). Checkpoints are never counted. Results also
report the decompressed record bytes of the segments that remain at the end
(`WALPayloadBytes`), never including the checkpoint. Truncation deletes the
segments a checkpoint covers, so after a restart these no longer hold the seed.
Restart runs also report the decompressed bytes of the segments written after
the restart, and of the checkpoint.

Heap passes need `-test.memprofilerate=1`. After the drain and two collections,
with the queues alive, they attribute in-use heap by allocation stack: to the
sender (remote-write code, its WAL watcher and readers, and HTTP clients),
to the fixture, or to everything else. Shared libraries, such as record
decoding, belong to their caller, so metadata strings the watcher decodes count
as the sender's. The process-wide interner is one allocation site, counted once
however many endpoints share it. Allocation stacks approximate ownership; they
do not establish which object retains an allocation. Results keep the whole
process's heap as a cross-check. `PROMETHEUS_METADATA_PIPELINE_ENDPOINTS=2` adds a
second queue and receiver, for any case without a restart. In a held backlog,
`PROMETHEUS_METADATA_PIPELINE_RELEASE_LAG` releases the second receiver only once
the first has acknowledged that many samples since the hold. Counts start from
each receiver's acknowledged items at the hold, which include the seed. The lag
is in delivered samples: queue buffers and in-flight requests separate it from
how far apart the WAL readers are. Results record each baseline, the first
receiver's delta at the second release, its overshoot over the lag, and the
second receiver's delta, which must be zero.

`BenchmarkHeadMetricMetadataBackfillWAL` reports the decompressed metadata and
compressed WAL bytes per sample of an out-of-order backfill, after an untimed
seed, for each metadata mode. It is encoding evidence only, not end-to-end
backfill CPU or latency.

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

### Scale and held-backlog study: incomplete (2026-09-20)

This records the original stop. The explicit continuation below supersedes its
completion status and recommendation to restart, without changing its evidence.

The bounded scale study above used the same host, boot, kernel, toolchain, and
runtime settings, with benchmark-only additions to
`b251931766dc2d0b6d16ba1640fa62ff42babbf1`. Production code was unchanged.
It stopped at block 60/106 under the predeclared host-noise policy: per-vCPU steal
exceeded 1% in a distinct-value backlog comparison and again in its sole replacement
(1.92%). The threshold was not relaxed and the study was not restarted.

There are 414 accepted scored observations: all 252 from cohort 1 and 162 from
the incomplete cohort 2. Thirty other observations belong to five rejected block
attempts, including both attempts at the terminal block. All 444 executed
observations and six separate smoke traces passed delivery/configuration
validation. The planned 108 heap, 48 receiver-capacity, and eight profile diagnostics
did not run; heap-enabled smoke traces do not replace them. **The overall study is
inconclusive**, not a completed confirmation or an adoption decision.

For orientation only, the complete first cohort has these native/WAL sender
CPU/sample changes. Entries are medians of six paired ratios, not confidence bounds
or maximum-throughput comparisons. The archive preserves every paired range and
separate `benchstat` tables, including the explicitly partial second cohort.

| Workload / metadata values | 10,000 series | 100,000 series |
| --- | ---: | ---: |
| Paced unchanged / shared | -2.16% | -8.17% |
| Paced unchanged / distinct | +3.72% | +0.41% |
| Paced 1%-changing / shared | -10.86% | -12.02% |
| Paced 1%-changing / distinct | +10.13% | +3.40% |
| Held backlog, every observation changes / shared | -8.94% | +6.14% |
| Held backlog, every observation changes / distinct | +138.81% | +138.28% |

The equal-work, unchanged 100,000-series controls have first-cohort CPU changes of
-9.61% shared and -2.17% distinct. They match the smaller trace's work/duration,
not its per-series history. Paced traces finished ingestion near their 4/40-second
schedule, but occasional late transactions remain in the results; they are not
zero-lateness or unlimited-capacity evidence.

Distinct full-churn backlog is the clearest provisional concern. At 100,000 series,
first-cohort median ingestion took 1.225 seconds native versus 0.149 seconds WAL;
release-to-drain took 0.846 versus 0.731 seconds, and allocation was 459 versus
194 bytes/sample. This measures ingestion plus forwarding, not an isolated lookup
penalty. It suggests profiling the changing append/commit path as well as historical
forwarding before selecting an ownership redesign. Without the remaining diagnostics,
these data cannot attribute the excess CPU or quantify retained-memory tradeoffs.

Keep production unchanged. A follow-up needs a new frozen run with both cohorts
and the missing diagnostics; do not fill gaps by pooling this partial run with it.
The checksummed archive `native-metadata-scale.N2xMtZ/evidence.tar.gz` contains the
original stopped study, frozen sources/binaries, all accepted and rejected attempts,
validation logs, monitoring, and an independently cross-checked partial analysis.
SHA-256:
`ff7a68a18bdc6b4576eec3e342043e98a052a6257ed13667cdf41d521e0b1192`.

### Scale and held-backlog study: completed continuation (2026-09-20)

At the user's request, CPU steal became informational: retain the measurement and
record the warning, without rejection, retry, or interruption. An explicit
continuation reused the exact frozen binary, source, host boot, kernel, workload
matrix, and remaining execution order. It inherited the 414 accepted observations
and executed the remaining 90 scored observations, 108 heap diagnostics, 48
receiver-capacity diagnostics, and eight CPU/allocation profiles. Production and
Go benchmark code were unchanged.

All 504 scored observations and 164 diagnostics are now covered. Four new accepted
observations had per-vCPU steal warnings of 1.85-1.92%; none caused a retry or stop.
One six-trace heap block was repeated because an `apt-get` process overlapped it,
under the unchanged local-process interference rule. Its replacement passed.
The five original rejected block attempts remain excluded and archived, including
both attempts at the original terminal block. Twelve smoke traces are separate.

Cohort 1 is entirely original evidence; cohort 2 mixes original and continuation
evidence. This completes the planned coverage, **not a fresh uniform-policy
replication**. The archive includes combined and segment-specific cohort-2 tables;
its continuation segment has only two observations per cell, or three for distinct
backlog. These small segments are descriptive, not an independent confirmation.

Native/WAL sender CPU/sample changes below are medians of six paired ratios per
cohort. Each entry shows cohort 1 / cohort 2; observed paired ranges and separate
`benchstat` tables are in the report, not confidence bounds or pooled estimates.

| Workload / metadata values | 10,000 series | 100,000 series |
| --- | ---: | ---: |
| Paced unchanged / shared | -2.16% / -4.83% | -8.17% / -9.03% |
| Paced unchanged / distinct | +3.72% / +5.99% | +0.41% / +0.79% |
| Paced 1%-changing / shared | -10.86% / -9.38% | -12.02% / -11.94% |
| Paced 1%-changing / distinct | +10.13% / +9.67% | +3.40% / +3.90% |
| Held backlog, every observation changes / shared | -8.94% / +1.42% | +6.14% / -0.13% |
| Held backlog, every observation changes / distinct | +138.81% / +134.30% | +138.28% / +144.61% |

Equal-work, unchanged 100,000-series CPU changes are -9.61% / -5.11% with shared
values and -2.17% / -0.37% with distinct values. Paced completion remains constrained
by the offered schedule, not a capacity measurement. Late transactions are retained.

Distinct full-churn backlog remains the clearest performance concern. At 100,000
series, native ingestion medians are about 1.225 seconds versus 0.149 seconds WAL
in both cohorts. Release-to-drain takes 0.80-0.85 seconds versus 0.73 seconds;
allocation is about 459 versus 194 bytes/sample. The roughly 2.4-fold CPU cost
includes ingestion and forwarding, not just historical metadata lookup.

Heap diagnostics have three observations per cell/mode. Selected whole-process
retained-heap medians at 100,000 series, in MiB:

| State / workload | WAL | Native | Disabled |
| --- | ---: | ---: | ---: |
| Drained paced unchanged / shared | 165.0 | 154.6 | 144.3 |
| Drained paced unchanged / distinct | 220.7 | 244.2 | 197.7 |
| Drained paced 1%-changing / distinct | 221.4 | 307.5 | 197.7 |
| Held full-churn backlog / shared | 127.9 | 123.3 | 107.2 |
| Held full-churn backlog / distinct | 181.6 | 328.7 | 160.6 |
| Drained full-churn backlog / distinct | 181.5 | 327.9 | 160.5 |

These include live fixture descriptors, sample storage, and sender buffers, not
isolated metadata ownership. Native retains five versions in the full-churn case;
the WAL queue retains current metadata, so their memory semantics differ. Native
metadata is still Head-only, whereas WAL metadata is persisted. Disabled mode
does not transmit metadata and is diagnostic only.

The receiver-capacity diagnostics used three unpaced 20-million-sample traces per
combination. Raising receiver `GOMAXPROCS` from 2 to 4 reduced completion medians
by 15-25% across workloads/modes. For distinct changing metadata, native completion
was 23.89 / 20.02 seconds at receiver P2/P4, versus 21.52 / 16.77 seconds WAL.
The receiver and shared host therefore influence observed capacity; these results
do not establish isolated sender capacity or general forwarding parity.

The profiles cover paced 100,000-series history workloads, **not full-churn
backlog**. In the distinct-changing native CPU profile, cumulative contributions
were 18.0% for RW2 time-series population, 16.6% for symbol-table reset, 3.6% for
batch metadata selection, and 3.3% for native metadata commit. These overlapping,
process-wide samples include setup/teardown and do not explain the full-churn gap.
CPU and allocation profiles, including flat/cumulative summaries, are archived.

Keep production unchanged. The next optimization investigation should profile
append/commit and retained-history ownership under distinct full-churn backlog,
with shared and unchanged controls, before choosing another ownership redesign.
The evidence does not justify assuming a lookup-only change would close the gap.
Restarts, history eviction, series replacement, sparse queries, resource attributes,
and cardinalities beyond 100,000 remain outside this study.

The archive `native-metadata-continuation.0E803L/evidence.tar.gz` preserves the
immutable original evidence, frozen binary and both runner versions, new accepted
and rejected attempts, validation/monitoring, profiles, segment-aware analysis,
and `REPORT.md`. It also records the initial executable-permission startup failure,
which occurred before any benchmark trace ran. The downloaded archive and its
internal checksums were verified; offline analysis reproduced all JSON, the report,
and `benchstat` tables (after normalizing relocated paths). An independent raw-trace
cross-check also matched all observations and paired summaries. Archive SHA-256:
`80c773b122fab46149daa518cb4b69a8be15db1f11db3148a9c98b86425b0753`.

# Cold-start and backlog phase diagnosis

This is a test-only follow-up to [the forwarding comparison](metadata_history_forwarding_comparison.md).
It investigates the candidate's longer backlog release-to-drain interval and
variable cold-start CPU cost. It is not a production optimization or promotion.

## Frozen implementations and workload

| Arm | Revision | Representation |
| --- | --- | --- |
| A | 948f6783afa635f7857b031417e312141520f1a7 | Forwarding implementation with striped histories. |
| F | f511970f08bcefbd7f33a9483db08c03d4ba7b14 | Series-owned, co-allocated state with batch cancellation and early index unlock. |

The real TSDB/sample-WAL/queue-manager/RW2/validating-receiver fixture is reused.
Cold start appends 10000 series once; it includes new TSDB/queue initialization,
not fixture preparation, receiver-process startup, or cold OS caches. The backlog
case seeds 100000 series with distinct metadata, holds HTTP acknowledgments, then
appends four metadata-changing sweeps before release. It must leave unread WAL
work, preserve all five versions, and deliver every sample with its expected
metadata. Failed, dropped, retried deliveries and metadata evictions must be zero.

Sender GOMAXPROCS is 4; receiver GOMAXPROCS is 2. Both use stringlabels. There is
no WAL-metadata arm: this study cannot establish native/WAL parity.

## Instrumentation and controls

The new runner, `scripts/benchmark-metadata-pipeline-phases.py`, copies the previous
study's pinned archives and binaries without changing them. It permits identical
overlays of only `metadata_pipeline_test.go`, `metadata_pipeline_bench_test.go`,
and the added `metadata_pipeline_diagnostics_test.go`. Original/patched source
hashes, original Git archives, complete sources, binaries, scripts, tool versions,
commands and environments are retained. Dependency and production changes fail
validation.

Instrumentation is off by default. An archived/rebuilt comparison measures
transferability of the rebuilt, instrumentation-disabled harness before using
its diagnostics. Each comparison keeps its own cohort and mode; archived bridge
observations are reused, not counted as an independent third replication.

Accounting records initialization, ingestion, backlog wait, drain, sender shutdown
and DB close. Sender/receiver CPU, allocations and GC counters remain separate.
Backlog initialization includes seeding, outside its original completion interval.
Observation start/end timestamps expose collection overhead. MemStats reads stop
the world; counter snapshots and control-pipe round trips can alter scheduling.
These are approximate accounting brackets, not overhead-subtracted phase costs.
Instrumented timings never substitute for uninstrumented observations.

Drain progress extends the existing 10 ms polling loop with a bounded,
preallocated buffer. The benchmark goroutine remains the sole receiver-command
owner. WAL record counters are gathered separately from other collectors.
Snapshots are observational, not atomic across components:

- Pending samples include in-flight requests.
- Acknowledgments are receiver-side completed-delivery counters.
- Held requests are cumulative, not the currently blocked request count.
- WAL records are counted before decoding and queue handoff, not after enqueue
  or delivery. Record types remain separate.
- Watcher counters can disappear on shutdown; monotonic progress checks apply
  during drain, not across teardown.

A separate control sets queue MinBackoff and MaxBackoff to 5 ms before startup.
It changes timer CPU/allocation work as well as wake-up latency. Convergence
alone cannot prove backoff explains the original tail difference. No queue-lock
orchestration, artificial WAL notifications or production instrumentation is used.

## Fixed coverage and interpretation

The schedule has 400 fresh benchmark processes, excluding 44 smokes:

| Pass | Processes |
| --- | ---: |
| Archived, cold and backlog; two cohorts, six observations per arm/case | 48 |
| Rebuilt, instrumentation off; same coverage | 48 |
| Accounting, cold/default-backlog/capped-backlog; same cohorts/repetitions | 72 |
| Phase CPU, cold: 32 captures per arm for initialization/ingestion/drain | 192 |
| Phase CPU, backlog: three captures per arm/backoff mode for ingestion/drain | 24 |
| Whole-process allocations and separate mutex/block captures | 12 |
| Execution traces, one per arm/backoff mode | 4 |

Archived/rebuilt and A/F order is deterministic and balanced within each cohort.
All scheduled unprofiled measurements finish before the profiling passes. CPU capture is one
phase per fresh process, with the output opened before the phase. StopCPUProfile
flushes synchronously, so later phases in that process are not undisturbed timing
observations. Profiles are aggregated only within one arm/configuration/phase.
Fewer than 100 aggregate CPU samples means insufficient evidence for reliable
function ranking; valid empty profiles are retained, and no adaptive repetitions
are added. Whole-process allocation and lock profiles include setup and cleanup,
not just measured ingestion/drain. Traces cover backlog ingestion through drain
and mark writer completion, release-command boundaries and drain completion.

CPU steal is recorded without interrupting, retrying or excluding observations.
Swap or competing local jobs permit one whole-block retry, preserving both
attempts. Correctness/provenance failures invalidate a run, not an outlier.
Reports retain independent cohorts, paired summaries and benchstat results;
nonsignificance is not parity. Backoff attribution requires agreement between the
uninstrumented results, progress records, traces and the capped control. Otherwise
the explanation remains mixed or unresolved.

## Results: 2026-09-23 Linux study

All 400 scheduled processes and 44 smokes completed, with no retries/exclusions.
Eight observations had CPU-steal warnings; all were retained. The host remained
Debian 13, Linux 6.12.107+deb13-amd64, eight Xeon Platinum 8358 vCPUs and 16 GiB
without swap, using Go 1.27.1. All 82 downloaded analysis files reproduced
byte-for-byte with the pinned benchstat version.

The table reports medians of six within-pair F/A ratios per cohort, not benchstat's
ratios of marginal medians. Both cohorts stay independent.

| Archived binaries, uninstrumented | Cohort 1 | Cohort 2 |
| --- | ---: | ---: |
| Backlog ingestion time | -17.3% | -19.7% |
| Backlog sender CPU/sample | -10.0% | -13.2% |
| Backlog allocation bytes/sample | -3.4% | -3.5% |
| Backlog release-to-drain | +18.3% | +27.0% |
| Backlog completion | -0.7% | +0.9% |
| Cold sender CPU/sample | -0.6% | -0.5% |

Backlog ingestion, CPU, allocation and tail differences resolve with benchstat in
both cohorts; completion and cold CPU do not. Rebuilt, instrumentation-disabled
binaries retain the backlog pattern. Within-arm archived/rebuilt backlog CPU,
ingestion, tail and completion comparisons do not resolve. Cold bridge CPU remains
variable (A +4.0%/+8.4%, F -1.8%/+2.1% paired medians, none resolved); this limits
transfer of precise cold estimates. The earlier +24.7% cold-CPU warning is retained,
not erased by this repeat or converted into a parity claim.

### Backlog: a demonstrated enqueue-backoff component

All accounting observations enter backlog drain with 13999 pending samples.
Acknowledgments advance promptly, but the next WAL sample-record count arrives
after median 151/136 ms for A and 356/341 ms for F. With the 5 ms cap, both reach
the next count in about 11 ms, near the polling resolution.

The default execution traces show the WAL reader inside a 640 ms sleep in
`nativeMetadataBatch.flush`. That sleep begins around 680 ms after ingestion starts
and ends around 1320 ms in both arms. A releases after about 1202 ms, F after
952 ms; their remaining waits are 117 and 372 ms. The faster writer therefore
starts its measured drain earlier in the same long backoff. The capped traces
reduce the remaining wait to about 1–5 ms. Runnable-to-running delays are only a
few microseconds in these events, not hundreds of milliseconds.

In repeated capped accounting runs, F/A tail deltas become -0.7%/+3.3% (neither
resolved), while completion improves -10.9%/-12.3% (both p=0.002). Within F, capping
reduces tail time about 39%/37% and completion about 21% in both cohorts. These
instrumented control results are not replacements for the uninstrumented table.
The control increases enqueue-backoff waits in the traces from 97/103 for A/F
to 296/273; it changes timer work as well as latency.

Together the uninstrumented reproduction, progress, traces and control support
enqueue-backoff timing as a major cause of this tail regression. They do not prove
perfect drain parity, establish that every difference is waiting, or justify
shipping a blanket 5 ms cap.

### Remaining CPU and cold-start uncertainty

Default F ingestion profiles put about 47.5% of sampled CPU under `unique.Make`
and 13.3% under metadata commit (A: 45.8% and 24.9%). Distinct-value interning is
a remaining churn cost. During default drain, RW2 shard processing is about half
of sender CPU; metadata lookup is about 16.0% in A and 7.5% in F. Cumulative
percentages overlap and profile totals are not independent speed measurements.

Cold ingestion has 204/194 aggregate CPU samples for A/F and shows ordinary series
creation, postings, commit/WAL work and GC in both. Initialization has only 18/19
samples and drain 30/29, insufficient for ranking. Accounting ingestion CPU is
roughly 67–70 ms with no resolved difference; initialization changes direction
between cohorts. No repeatable candidate-specific cold bottleneck is established.
Cold completion remains near 153–156 ms with about 121 ms drain and the existing
100 ms batching deadline; it must not be interpreted as CPU-only throughput.

Whole-process allocation/lock reports are retained separately. Allocation profiles
include substantial fixture/seed work, and lock profiles do not measure
uncontended atomic costs. Neither clears the earlier fixed-work allocation
question or current-lookup regression.

### Recommendation and retained evidence

Keep F experimental. The next backlog-latency experiment should examine
queue-space-driven wake-up or a separately justified enqueue-backoff policy,
including shutdown, resharding, slow/failing receivers and native/WAL controls.
Do not redesign cold initialization from these short profiles. The prior
retained-heap, current-lookup and tx-ring allocation questions remain separate.

Focused A/F tests, all-label diagnostic/benchmark smokes, A/F race checks, executed
386 coverage, lint and 68 driver tests passed. No production change or push is
included. Source inventories validated before the run match the frozen sources.

Detailed report, original captures, matching binaries, source archives, derived
profile/trace reports and reproduction receipts:
`/Users/arve/Projects/prometheus/benchmark-results/native-metadata/20260923-linux-pipeline-phases`.
The completed-evidence archive SHA256 is
`ca3aeab2be84c7df8bfb4b54d8554ccdce9ceb2dbf73262aec9186963c9e06fb`.
Regenerate analysis only in a separate extracted copy; the previous study and this
study's original evidence remain unchanged.

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
series. Logical sample timestamps advance by 15 seconds, but ingestion is unpaced.
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

These are finite-trace, unpaced pipeline comparisons, not sustained-capacity or
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

Linux confirmation remains outstanding after Mac measurements. A subsequent study
can examine 500,000 and one million active series, repeating smaller controls on
the same host and independently varying ingestion rate and metadata sharing. The
100,000-series case here also increases total sample work tenfold: it is not an
isolated cardinality experiment. These results neither supersede the narrower
microbenchmarks nor reopen adoption gates for parked forwarding prototypes.

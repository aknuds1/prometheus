# Series-owned history versus the forwarding implementation

This study separates a B/F allocation investigation from a direct A/F comparison.
Production code, the forwarding branch and historical evidence remain unchanged.

## Revisions and controls

| Arm | Revision | Implementation |
| --- | --- | --- |
| A | 948f6783afa635f7857b031417e312141520f1a7 | Forwarding baseline, with striped metadata history. |
| B | 61acdce838c6efa116f5d1caaa062a4a2fe7898a | Series-owned, co-allocated native state, original lookup. |
| F | f511970f08bcefbd7f33a9483db08c03d4ba7b14 | Co-allocated native state, batch cancellation and early index unlock. |

scripts/benchmark-metadata-history-forwarding.py builds each pinned Git archive
with add-only benchmark overlays. The existing fixed-work fixture is added where
absent, and the new lookup/append diagnostic is identical in every arm. No existing
file is overwritten. Sources, dependencies, fixtures, scripts and binaries are
hashed and verified. The previous study's B-relative improvement gate is not an
A/F acceptance criterion; the driver reports observations and warnings, not an
automatic adoption verdict.

## Allocation diagnosis

The original one-destination, unchanged-current-metadata benchmark is unchanged.
At GOMAXPROCS=2 it runs with a calibrated one-second duration and fixed budgets of
8192 and 32768 transactions. The new diagnostic runs those same fixed budgets with
two scheduling modes: a shared atomic transaction budget and balanced worker
quotas. Shared-budget is deliberately not Go's RunParallel scheduler. Both new
modes share preallocated scaffolding and timing boundaries, and b.N always means
total transactions, unlike the older append-only fixed-work fixture.

All three shapes have eight workers, 8000 series, 100 metadata families, five
seeded versions, unchanged metadata, constant sample values, and one sequential
lookup consumer acknowledging every committed batch. Each transaction appends
1000 samples. Worker and acknowledgment counts, final metadata and sample totals
are validated. GC snapshots bracket timing without forcing collection; validation
runs afterward. Compare implementations within matched configurations rather than
treating scaffolding differences as a speedup or attributing causes from one quiet
control. The historical +21.74% allocation warning remains part of the record.

## Direct forwarding comparison

A/F uses the preceding layout study's 14 timing and eight retained-heap cases,
plus four native-only RW2 pipelines: cold start with 10000 series, unchanged
forwarding with shared and distinct metadata at 100000 series, and distinct
metadata changes with a held backlog at 100000 series. Sample WAL, real queue
managers and validating receivers remain enabled. The unchanged traces are paced,
so their throughput is not a maximum-capacity measurement. Held-backlog wait and
active release-to-drain time are reported separately. There is no WAL-metadata arm
and no native/WAL parity claim.

## Evidence and results

The 2026-09-23 Linux run completed all 792 scored fresh-process observations,
78 smokes and 20 diagnostic profile runs (40 raw profiles). Both stages have two independent cohorts
of six observations per configuration and arm, with pair order balanced within
each cohort. Stages, cohorts, budgets and profiles are never pooled. CPU steal is
recorded without stopping, retrying or excluding measurements.

Host: Debian 13, Linux 6.12.107+deb13-amd64, eight Xeon Platinum 8358 vCPUs,
16 GiB RAM, no swap; Go 1.27.1 linux/amd64, stringlabels. The run had no retries or
exclusions. Four retained-heap observations recorded CPU-steal warnings; none of
the timing, pipeline or profile observations did. This does not establish a
noise-free host. All 53 analysis files reproduced byte-for-byte from the downloaded
archive using the pinned benchstat version.

The following percentages are medians of the six within-pair ratios, separately
for each cohort. Positive means F costs more. They are not ratios of marginal
medians; benchstat uses the latter and supplies separate significance evidence.
The cohorts are not pooled, and absence of significance is not proof of parity.

| Direct A/F measurement | Cohort 1 | Cohort 2 |
| --- | ---: | ---: |
| Constant metadata churn, append G8 | -28.8% | -28.9% |
| Historical-full lookup G2 | -32.4% | -33.1% |
| Missing-full lookup G2 | -47.1% | -47.0% |
| Sequential current lookup G2 | +4.3% | +5.0% |
| Sparse metadata changes, append G2 | +4.7% | +4.7% |
| Unchanged append G8 | +2.8% | +2.8% |
| One-destination unchanged lookup/append G8 | +0.5% | +1.4% |
| Two-destination unchanged lookup/append G2 | +2.5% | +9.0% |
| RW2 unchanged/shared sender CPU per sample | -0.1% | -0.1% |
| RW2 unchanged/distinct sender CPU per sample | +1.1% | +0.7% |
| RW2 held-backlog changes, sender CPU per sample | -20.5% | -12.3% |
| RW2 held-backlog changes, release-to-drain time | +15.0% | +23.2% |
| RW2 cold start, sender CPU per sample | -2.8% | +24.7% |

Churn, historical/missing lookup gains and the smaller current-lookup,
sparse-change and G8 unchanged-append slowdowns resolve in both cohorts with
benchstat. The primary G8 lookup/append and unchanged RW2 CPU differences do not.
Two-destination G2 and cold-start CPU regressions resolve only in cohort 2.
Sparse query time changes by less than 1% in both cohorts (resolves only in cohort
2). Disabled and legacy stable-append controls show no resolved timing change.

Backlog CPU and allocation improve (about 3.6% fewer allocated bytes/sample), but
release-to-drain is slower in every matched pair, resolving in both cohorts.
Ingestion is faster while total completion is little changed. The receiver is
released after ingestion, so the amount of sender/queue work already completed at
release can differ. This is a phase-specific regression, not proof of lower
steady-state drain capacity. No pipeline profiles were captured in this study;
the Head profiles cannot establish its cause.

Stable native retained heap is 907.7 -> 916.3 B/series; five-version heap is
1224 -> 1232 B/series. Native-minus-off overhead increases 96.4 -> 105 B/series
(+8.9%) and 168 -> 176 B/series (+4.8%), respectively. Metadata-free series are
unchanged; stable legacy metadata saves 8 B/series. These results agree across
cohorts. Small percentages of total Head heap must not hide the native increment.

### Allocation diagnosis verdict

The historical +21.74% B/F warning is not cleared. Original calibrated G2 B/op
has paired median changes of +2.1% and +1.8%, but the fixed-work results remain
variable: shared-budget 8192 transactions gives +6.9% and +26.0%; balanced 32768
gives +0.9% and -3.3%. None of the 14 B/op comparisons resolves with benchstat.
That is inconclusive, not evidence that allocation overhead is zero.

All six allocation profiles show no sampled allocations beneath metadata lookup.
Worker-focused profiles instead identify ordinary chunks, append batches and
transaction-ID rings. In the shared-budget profile, txRing.add accounts for about
4.00 -> 15.51 MiB of sampled worker allocation; balanced profiles show about
7 MiB in each arm. Both balanced runs execute 4096 transactions per worker and
both have four timed GC cycles, yet some B/op variation remains. Scheduler effects
on isolation-watermark retention, chunk boundaries and pool reuse are plausible;
the individual profiles do not prove which caused the historical warning.
Whole-process profiles include setup, calibration, validation and cleanup, not
just timed work; their totals must not be divided by the final iteration count.

### Recommendation and next work

Keep F as an experimental candidate; do not promote it as a general forwarding
performance improvement. It delivers substantial churn/history gains, but the
current-lookup, heap and backlog-tail costs are real tradeoffs. Next, capture
phase-specific pipeline profiles and queue/receiver progress at backlog release,
and repeat the cold-start case. For the allocation question, instrument tx-ring
growth and pool misses under the existing fixed-work controls before changing
production allocation behavior. Sequential current lookup still spends roughly
half of sampled CPU in RWMutex reader-counter atomics, making per-item index
locking a separate optimization target, not a demonstrated fix here.

Focused TSDB/remote checks, fixture smokes for all three label builds and G2/G8,
all-label race smokes, an executed 386 smoke, lint and 61 study-driver tests passed.
An initial preflight failed lint on the new fixture's atomic import; it was fixed
before freezing or measuring, and that failed preflight was preserved separately.

The detailed report, raw stdout/stderr, source archives, matching binaries, host
records, profiles and reproduction receipts are retained locally under
`/Users/arve/Projects/prometheus/benchmark-results/native-metadata/20260923-linux-forwarding-comparison`.
The completed-evidence archive SHA256 is
`bdb4db1f7836acebfd177fec07a1e3065a4705384700624b5c6cffaeed06c95e`.
Run the archived driver's `analyze` command on a separate extracted copy to
reproduce its analysis without altering the retained evidence. No production
change, promotion, amendment or push is part of this study.

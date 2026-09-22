# Inline native metadata state experiment

This is an experimental follow-up to the completed co-allocation screen, not an
adopted optimization. Keep the forwarding branch and all previous evidence and
verdicts unchanged. Preserve this implementation even if its performance target
fails.

## Representation and lookup changes

The lazy memSeriesMetadata sidecar embeds the native current point and history
descriptor before the legacy pointer. Older entries remain lazy. A nil
current-metadata pointer means no committed native history, including on a
legacy-only sidecar. Adding the other metadata mode never replaces a published
sidecar.

No field is added to memSeries. Relative to co-allocation, expected amd64
bookkeeping allocations shrink from 80 to 64 bytes for native-only series, but
legacy-only sidecars grow from 16 to 64 bytes. On 386, native-only bookkeeping
grows from 40 to 48 bytes and legacy-only sidecars from 8 to 48 bytes. These
figures exclude unchanged immutable values and older-history allocations.
Metadata-free series allocate no sidecar.

Two separate lookup changes check cancellation at 256-entry batch boundaries
and release the Head reference-index lock after capturing a live native-state
pointer. The publication barrier still excludes native updates throughout
selection. Deletion must leave retired native state unchanged. No series mutex,
legacy field, or packed series state is accessed through the unlocked read path.
Cancellation still interrupts publication waits; it does not make Head index
locks cancellable.

## Frozen comparison

The independent signed production commits are:

| Arm | Revision | Change |
| --- | --- | --- |
| A | 948f6783afa635f7857b031417e312141520f1a7 | Native forwarding baseline. |
| B | 61acdce838c6efa116f5d1caaa062a4a2fe7898a | Co-allocation checkpoint. |
| C | 43b5b155592a471f09857ed4d0f373ee2c140ad8 | Inline native state. |
| D | f12fed6f2c40aa4d4a3d84f9724f75091a7f983e | C plus batch-boundary cancellation. |
| E | 203bbe2edd1ce57d7e1eac650456688fdc7422c0 | D plus earlier index unlock. |

The driver is scripts/benchmark-metadata-history-inline.py. Every variant is
rebuilt from its pinned Git archive plus exactly the same new fixed-work
benchmark fixture; earlier binaries and observations are not reused.

Use Debian amd64, Go 1.27.1, stringlabels, and benchstat
v0.0.0-20250305200902-02a15fd477ba. Two independent cohorts contain six fresh
processes per configuration, with every five-arm pair ordered three times each
way per cohort. The three-arm blocks use all six permutations. Do not pool
cohorts, fixed-work sizes, diagnostic profiles, or historical studies.

The frozen matrix has 660 blocks and 2,112 observations, plus 189 fresh smoke
checks:

- All five arms: six current-value timing configurations.
- A/B/E: 16 other protected timings, 20 retained-heap cases, and two fixed-work
  parallel current-lookup allocation configurations.
- A/B/E: native cold initialization at 10k series, distinct-churn backlog at
  100k, and shared/distinct unchanged equal-work pipelines at 100k.
- A/B/E: three fixed-append allocation diagnostics at 1,024 and 4,096
  transactions per worker. Unchanged fixed-concurrency append uses eight workers
  at G2; zero-destination lookup/append uses eight at G8; stable legacy append
  preserves its single writer at G2. Seeding, values and timestamps match their
  respective original workloads. Every worker finishes before timing stops.
- B/E: six matched 30-second CPU/allocation captures and six separate
  mutex/block captures per arm.

CPU steal is recorded and retained, never an exclusion, retry, or stopping
condition. Swap and competing local compilation/tests retain the existing
one-retry, whole-block policy. A correctness or provenance failure stops the
runner, preserving its artifacts. Validation and compilation finish before
measurement; a completion marker and successful exit receipt are required.

## Interpretation

The primary target is at least 5% lower median paired time for E versus B on
unchanged current lookup/append with one destination at G8, with all six pairs
favorable in both cohorts. Benchstat compares distributions separately; its
ratio of medians is not the paired-ratio estimator.

Report every incremental change and the remaining gap versus A. Keep all
protected timing/heap regressions above 5% and allocation warnings visible,
including the expected legacy heap tradeoff. Report total and incremental native
heap separately. Fixed-work diagnostics investigate earlier calibrated
allocation warnings; they cannot retroactively erase them.

Profiles include process setup, benchmark calibration, and cleanup. Do not divide
their totals by the final benchmark iteration count or equate sampled load costs
with demonstrated cache misses. These comparisons do not establish native
versus WAL forwarding parity or authorize promotion.

## Completed Linux result

The primary target passed in both independent cohorts. The median paired E/B
time fell 14.48% and 13.75%, with every pair favorable. Ordinary benchmark
medians fell from 53.60 to 45.48 microseconds and from 53.09 to 46.01 microseconds
(benchstat: -15.15% and -13.34%, p=0.002, n=6 per arm in each cohort).

| Incremental primary comparison | Cohort 1 paired change | Cohort 2 paired change |
| --- | ---: | ---: |
| C/B: inline state | -3.22% | -1.20% |
| D/C: batch cancellation | -6.22% | -7.60% |
| E/D: earlier index unlock | -6.31% | -5.13% |
| E/B: combined | -14.48% | -13.75% |
| E/A: versus forwarding baseline | -3.42% | -1.60% |

The benefit is strongest in lookup, not unchanged append alone. Sequential
current lookup improves about 15% versus B in both cohorts; the selected sparse
API queries remain essentially unchanged. E retains the substantial historical
lookup and constant-churn append gains versus A. These results do not establish
WAL parity or a general throughput improvement.

Native retained heap drops about 16 bytes per series versus B. In the stable
shared-value case, total heap falls from 916.3 to 900.3 bytes per series, and
incremental native heap from 105 to 89 bytes. Legacy-only total heap rises from
887 to 935 bytes per series (+5.41%). Metadata-free heap is unchanged.

This is not a clean all-gates pass: cohort 2 flags cold-pipeline CPU at +6.43%
on the paired estimator, versus -7.68% in cohort 1; benchstat does not resolve a
distribution difference. Allocation warnings remain, including fixed-work
append diagnostics. At 1,024 rounds their fixed-concurrency allocation delta is
+6.98% in cohort 1 and -2.25% in cohort 2; at 4,096 rounds it is +0.90% and
-0.01%. Keep these warnings and all earlier study verdicts intact. Two-destination
G2 lookup/append also has a +8.40% paired timing warning versus A in cohort 2
(not versus B), although benchstat is inconclusive. No adoption is authorized.

Profiles suggest reference-index atomic locking is the next lookup bottleneck;
they do not prove cache misses or that another layout change would help. Most of
the primary gain comes from the two lookup changes, not inline state alone.

## Validation and preserved evidence

All five variants passed focused Linux TSDB/remote tests and fixture smoke
checks. E passed the full TSDB/remote suites and focused race tests under
stringlabels, slicelabels, and dedupelabels, executed 386 checks, and make lint.
The four Python driver suites passed 44 tests. No Go correctness test was forced
with -count=1.

The completed study contains 660 blocks, 2,112 observations, 189 smoke checks,
and all 24 matched diagnostic captures, with zero exclusions. Fifteen CPU-steal
warnings were retained. The supervisor returned zero and emitted its completion
receipt. Source, fixture, script, and binary provenance was verified after
download. Local reproduction matched 147 analysis files byte-for-byte and all
48 benchstat reports after normalizing absolute paths and whitespace; the frozen
extraction remained unchanged.

Local evidence is in
/Users/arve/Projects/prometheus/benchmark-results/native-metadata/20260922-linux-inline-native-state.
REPORT.md contains the analysis, limitations, and profile references;
preservation.json records verification. The completed-evidence.tar.gz SHA-256 is
12b3ab6864f30578370cd48cd3c0c9fe62562d562387617ab62a1186f74480ed.
No previous archive or evidence directory was modified. Keep the implementation
on the experimental branch; the forwarding branch remains at A.

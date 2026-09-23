# Co-allocation with optimized metadata lookups

The Linux comparison meets its targeted recovery criterion, but does not make
co-allocation equivalent to inline state. Keep both implementations experimental;
the forwarding branch and all historical study verdicts remain unchanged.

## Candidate and controls

| Arm | Revision | Representation and lookup |
| --- | --- | --- |
| B | 61acdce838c6efa116f5d1caaa062a4a2fe7898a | Co-allocated native state, original lookup. |
| E | 203bbe2edd1ce57d7e1eac650456688fdc7422c0 | Inline state, batch cancellation and early index unlock. |
| F | f511970f08bcefbd7f33a9483db08c03d4ba7b14 | Co-allocated state, both lookup optimizations. |

F restores B's pointer-based lazy sidecar and architecture-specific allocation
path. Its only non-test production Go difference from B is the lookup file.
Published sidecars are never replaced; publication and retired-history lifetime
guarantees are preserved. The full inline implementation remains in history at
7b989917b3af319277c19a9e0813d1852256ddc5.

The dedicated driver is scripts/benchmark-metadata-history-layout.py. Every arm
is freshly rebuilt from its pinned Git archive. B/E receive only the identical
fixed-work fixture addition; F already contains it and has no overlay. Workload,
dependency, source, script and binary hashes are verified. Historical runners
and evidence are not modified.

## Completed Linux comparison

Measurements ran on September 22, 2026, on Debian amd64, eight Xeon Platinum
8358 vCPUs, Go 1.27.1 and stringlabels, using benchstat
v0.0.0-20250305200902-02a15fd477ba. Two independent cohorts each contain six
fresh processes per configuration. Each uses all six B/E/F permutations, with
the sequence reversed in cohort two. Cohorts and fixed-work sizes are not pooled.

The primary target is F/B median paired time at most 0.95, with all six pairs
favorable in both cohorts, for unchanged current lookup/append with one
destination at G8. F passes: -10.56% and -11.95%, with all pairs favorable.
Ordinary medians are 53.09 -> 47.64 microseconds and 53.40 -> 47.36 microseconds
(benchstat -10.26% and -11.31%, p=0.002 in each cohort).

| Median paired timing change | Cohort 1 | Cohort 2 |
| --- | ---: | ---: |
| Primary F/B | -10.56% | -11.95% |
| Primary E/B | -13.95% | -14.24% |
| Primary F/E | +3.95% | +3.48% |
| Sequential current lookup F/B | -6.65% | -6.61% |
| Sequential current lookup F/E | +9.53% | +8.85% |
| Historical-full lookup F/E | +7.10% | +4.68% |
| Missing-full lookup F/E | +6.00% | +5.65% |
| Sparse metadata changes F/E | +5.16% | +4.02% |

F/E is an observed difference, not an equivalence gate. Six protected timing
comparisons exceed +5%, all against E; none exceed it against B. The selected
sparse API query, unchanged append, metadata-disabled and legacy append show
no protected timing regression. Cold 10k pipeline CPU remains variable:
F/B is -5.48% and +0.32%, F/E -7.12% and -5.93%, without a benchstat-resolved
difference. These results do not establish cold-pipeline improvement or parity.

Retained heap medians agree in both cohorts:

| Bytes per series | B | E | F |
| --- | ---: | ---: | ---: |
| Stable native, total | 916.3 | 900.3 | 916.3 |
| Stable native minus off | 105 | 89 | 105 |
| Five observed versions, native total | 1232 | 1216 | 1232 |
| Five observed versions, native minus off | 176 | 160 | 176 |
| Stable legacy, total | 887 | 935 | 887 |

F costs 16 B per native-only metadata series more than E, but saves 48 B per
legacy-only metadata series. Metadata-free heap is unchanged across arms. The native
increment is +17.98% for stable values and +10% for five versions, even though
the total-heap percentage is small. Keep these expected tradeoffs visible.

All 30 baseline-specific allocation warnings remain recorded. In particular,
one-destination G2 lookup/append has B/op medians B/E/F of
2677.5/3067/3259.5 in cohort one and 2948.5/2882.5/3040 in cohort two.
The first F/B comparison is +21.74% by benchstat (p=0.002); the second is not
resolved. The fixed-work diagnostics include smaller allocation increases and
do not reproduce that exact positive-destination workload. They cannot clear
its warning or any historical warning.

## Profiles and interpretation

All eight E/F captures are retained, covering sequential current lookup at G2
and the primary concurrent workload at G8, with CPU/allocation and separate
mutex/block profiles. The reference-index RWMutex atomic operations dominate
the sequential CPU captures (about 52-56% flat). F retains an extra native-pointer
load; its samples are consistent with an indirection cost, but do not prove
cache misses or explain the entire measured difference. Mutex waiting is small
in these captures; blocking is dominated by benchmark channels and joins.
Concurrent allocations are predominantly ordinary sample/chunk machinery.
Profiles include setup, calibration and cleanup, and are not normalized by the
final benchmark iteration count.

F is a useful smaller-legacy-footprint alternative, not the fastest native
layout. E remains faster and smaller for native metadata. Choosing between them
requires an explicit memory/performance tradeoff; neither is promoted here.
Further reference-index amortization and exact positive-destination fixed-work
allocation analysis remain separate work. This study contains no fresh
forwarding-baseline or WAL arm and makes no claim of native/WAL parity.

## Validation and evidence

All arms passed focused Linux checks and fixture smokes. F passed full
TSDB/remote suites and focused race checks with stringlabels, slicelabels and
dedupelabels, executed 386 checks, and make lint. All five Python driver suites
passed 53 tests. Go correctness tests were not forced with -count=1.

The completed study has 328 blocks, 980 observations, 86 smoke checks and all
eight diagnostic captures. There were no exclusions or retries. Six measured
CPU-steal warnings and two smoke warnings were retained, never used to stop or
retry a block. Validation and benchmark compilation preceded measurement.
The supervisor returned zero and emitted its completion receipt.

The downloaded archive's sources, fixtures, scripts, binaries and outputs were
verified. Independent local reproduction matched all 65 analysis files,
including all 30 benchstat reports, byte-for-byte. The frozen extraction remained
unchanged. Profiles were rendered using the matching Linux binaries and sources.

Evidence directory:
/Users/arve/Projects/prometheus/benchmark-results/native-metadata/20260922-linux-coallocation-lookups.
REPORT.md contains all protected timings, allocation warnings and interpretation;
preservation.json records verification. The completed-evidence.tar.gz SHA-256 is
d17465610652f8c88573be61cafe63bbeecae60338ff14e90398696dcb6b8fb5.
No previous evidence was modified or deleted. No promotion or push is authorized
by this comparison.

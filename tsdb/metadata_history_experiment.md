# Series-owned native metadata history experiment

Status: isolated prototype. The completed three-arm Linux study improved churn
CPU but failed its protected performance and memory gates. The forwarding branch
remains unchanged; a focused current-value allocation experiment follows below.

## Question and controls

Does moving native history into a lazy series-owned sidecar reduce full-churn
ingestion/forwarding cost without regressing unchanged metadata, sparse queries,
concurrent consumers, disabled/legacy paths, or retained memory?

The baseline is `948f6783afa635f7857b031417e312141520f1a7`. Compare three
independently built arms with identical Go benchmark fixtures and dependencies:

| Arm | History ownership | Presence filtering |
| --- | --- | --- |
| baseline | Existing external history map | Existing history map |
| index | Existing external history map | Separate ref-to-series map |
| series | Lazy series sidecar | Same separate ref-to-series map |

The index arm isolates the cost of the presence map. Its only production changes
add/update that map and use it for presence checks; it retains baseline history
ownership, merging, cache publication, and forwarding. Frozen source archives
preserve this diagnostic arm; it is not a second candidate implementation.

The original candidate leaves `memSeries` itself unchanged. Its two-pointer metadata
sidecar allocates native history independently from legacy WAL metadata. Native
state holds the newest point inline and allocates older points lazily, growing
through capacities one, two, and four. Histories still retain at most five
coalesced changes with sticky truncation and incoming observations winning ties.
The experiment changes only in-memory representation, not persistence or APIs.

## Synchronization and lifetime

- Series locks protect native mutations and query snapshots. Immutable values
  are prepared outside those locks; commit rechecks stability before merging.
- Presence locks protect membership only. Readers release them before taking
  series or Head-index locks. First publication updates presence under the
  series lock; deletion removes presence before examining retired series.
- Pending-sample reservations keep committing series live until batched
  accounting finishes.
- Forwarding retains the existing publication barrier and bounded lookup batches.
  It checks Head membership under the Head reference-index lock, without taking a
  series lock, then reads native fields. The sidecar pointer is loaded atomically
  because legacy commits can install it independently.
- GC does not clear retired native state: a lookup can retain that state after
  removal. Queries revalidate the GC flag under the series lock. Reset replaces
  the Head series index before clearing presence and current counters.

No metadata is put in `memChunk`; no new bitmap or columnar dependency is added.

## Frozen Linux study

Use Go 1.27.1 and the existing Debian droplet. Run correctness, race, alternate
label-build, 386, and lint checks before freezing. Source directories must be
Git-free copies outside the new results directory and remain unchanged until
measurement completes.

```sh
python3 scripts/benchmark-metadata-history_test.py
python3 scripts/benchmark-metadata-pipeline-scale_test.py
python3 scripts/benchmark-metadata-history.py freeze RESULTS BASELINE INDEX_CONTROL CANDIDATE
python3 RESULTS/benchmark-metadata-history.py smoke RESULTS
python3 RESULTS/benchmark-metadata-history.py run RESULTS
python3 RESULTS/benchmark-metadata-history.py analyze RESULTS
```

The manifest fixes the matrix before measurement:

- 1,728 scored pipeline observations: all 14 existing scale workloads plus cold
  initialization and new-series churn, three arms, native/WAL/disabled modes,
  and two independent cohorts of six fresh processes.
- 24,408 Head observations: existing append, query, lookup, concurrent-consumer,
  series-churn, transaction-size, and heap cases across stringlabels, slicelabels,
  and dedupelabels; two independent cohorts of six fresh processes per arm/case.
  Dual-mode performance is excluded.
- 324 forced-GC pipeline heap diagnostics, 144 capacity diagnostics, 12 CPU/alloc
  profiles, and 12 separate mutex/block profiles. These are not scored timings.
- 2,178 unscored smoke observations cover every pipeline and Head case/build/arm.

Smoke checks validate every Head selection on baseline/stringlabels first, then
the remaining Head builds/arms, and finally the pipeline traces. Successful
process termination without exactly the expected benchmark measurement is a
failure, with its command and raw output retained for diagnosis.

Timing runs use `-benchmem`, one second for ordinary Head cases, 20 iterations
for large transactions and series churn, and one iteration for finite pipeline
traces and heap diagnostics. Six separate processes replace an in-process
`-count=6` to avoid warming process-global interning between repetitions.
Sender/receiver and concurrent Head parallelism remain explicit in the manifest.

CPU steal is recorded, never a rejection, retry, or stopping condition. Swap and
overlapping local compilation/testing/package installation reject the entire
paired block, with one predetermined replacement; correctness or provenance
failures stop the run. All attempts are preserved. Poor performance does not
trigger selective reruns or early stopping.

The runner checks source, binary, manifest, raw-output, and profile checksums.
Each observation includes host/boot identity, command, environment, and sampled
host activity. Analysis requires complete coverage, keeps cohorts separate, and
can be reproduced after downloading and relocating the archive without contacting
the droplet. Old studies are not pooled with this comparison.

## Adoption gates

Compare the series arm with baseline, separately in both cohorts:

1. At least 5% median sender CPU improvement for the 100,000-series,
   distinct-value, full-churn held-backlog trace; every paired primary observation
   must favor the candidate.
2. No more than 5% median regression for protected timing cases: unchanged
   pipeline metadata, WAL/disabled pipeline controls, and non-heap Head
   ingestion/query/lookup/concurrent-consumer cases.
3. No increased stable-path allocations or disabled `memSeries` footprint.
4. No more than 5% retained-heap regression, including per-series totals and
   incremental native-minus-disabled memory.
5. All correctness, race, architecture/build, and lint checks pass.

The runner reports gate failures but never moves branches. A memory or protected
performance tradeoff requires explicit approval. If every gate passes, verify
the forwarding branch still points to the frozen baseline before fast-forwarding.
Otherwise retain the isolated experiment and report the complete findings.
Commit locally with DCO/signing; do not push or update a PR automatically.

## Findings

The first frozen attempt stopped during smoke checks on 2026-09-21 at 07:37 UTC,
after 684 observations. The harness selected a nonexistent legacy-only sparse
change case; the process returned `PASS` without a benchmark measurement. This
was a selection error, not a native-metadata failure or CPU-steal rejection.
The repair removes only that nonexistent case, strengthens selection regression
coverage and diagnostics, and starts a fresh frozen study. The original attempt
is preserved and its smoke measurements are not pooled into scored results.

The repaired comparison completed on 2026-09-22. Median paired series/baseline
changes, reported separately for its two cohorts, include:

| Measurement | Cohort 1 | Cohort 2 |
| --- | ---: | ---: |
| 100k distinct-value churn pipeline CPU/sample | -18.26% | -13.56% |
| Current unchanged lookup/append, one destination, GOMAXPROCS 8 | +21.30% | +20.22% |
| Shared current sequential lookup, GOMAXPROCS 2 | +12.37% | +13.75% |
| Full historical sequential lookup, GOMAXPROCS 2 | -28.09% | -29.33% |
| Native sparse-change append, GOMAXPROCS 2 | +6.88% | +6.81% |
| 100k shared-value churn pipeline completion time | +9.79% | +7.94% |

Head rows use stringlabels. Current lookup/append regresses across all label
builds, while the index-only control stays close to baseline. Stable incremental
native-minus-disabled retained heap increases from about 96.4 to 105 B/series;
four observed versions increase from about 150 to 175 B/series. Total retained
heap is within budget, but this does not erase the incremental metadata cost.
The primary CPU objective passes; the full frozen gate verdict remains failed.

The complete report, verified raw archive, matching sources/binaries, profiles,
and reproducible analysis are retained under
`../benchmark-results/native-metadata/20260922-linux-series-owned-history/`.
Checkpoint `f97b4151ecfd3c10114c1880419c0dca62c6d150` preserves the experimental
source used by that study. Its source hashes, not a moving branch name, identify
the measured implementation.

## Current-value co-allocation experiment

This follow-up tests one representation change: on 64-bit targets, the first
native publication may allocate the two-pointer sidecar and native state in one
object, with the sidecar first. A pre-existing legacy sidecar must be retained,
using a separate native allocation. Publication initializes the internal pointer
before storing the atomic sidecar pointer; synchronization and lifetime contracts
above remain unchanged. No fields are added to `memSeries` or its sidecar.

For the measured Go layouts, the combined object is 72 bytes, rounded to 80,
versus separate 16-byte and 56-byte objects, rounded to 16 and 64. It saves an
object, not allocator bytes, and retains the logical pointer indirection.
Improved locality is a hypothesis, not an established consequence. On 386,
combining 8 and 32 bytes would round 40 to 48, so 32-bit builds retain separate
allocation. Older-history capacities, interning, the presence index, and all
lookup/query algorithms are unchanged. The build-mode experiment stays deferred.

Run the focused study in two fresh directories; neither mutates the completed
study or pools measurements with it:

```sh
python3 scripts/benchmark-metadata-history-current_test.py
python3 scripts/benchmark-metadata-history-current.py freeze-profiles PROFILES COMPLETED_STUDY
python3 PROFILES/benchmark-metadata-history-current.py smoke PROFILES
python3 PROFILES/benchmark-metadata-history-current.py run PROFILES
python3 PROFILES/benchmark-metadata-history-current.py analyze PROFILES
# Inspect control profiles before changing the candidate representation.
python3 scripts/benchmark-metadata-history-current.py freeze-trial TRIAL PROFILES CANDIDATE_SOURCE
python3 TRIAL/benchmark-metadata-history-current.py smoke TRIAL
python3 TRIAL/benchmark-metadata-history-current.py run TRIAL
python3 TRIAL/benchmark-metadata-history-current.py analyze TRIAL
```

The first stage imports the three original source archives and matching binaries
and collects 36 diagnostic runs. Six stringlabels configurations cover current
sequential lookup, unchanged lookup/append and fixed-concurrency append at
GOMAXPROCS 2 and 8, and sparse-change append. Each configuration has a 30-second
CPU/allocation capture and a separate 30-second mutex/block capture per arm.

The second stage adds the coallocated arm, checking identical benchmark fixtures
and dependencies before building it. It freezes two cohorts of six fresh
processes across four arms, balanced candidate/control order, and all three label
builds. The 6,444 observations comprise:

- 3,168 Head timings protecting current, historical, missing, sparse-query,
  unchanged/changed append, series churn, and disabled/legacy controls.
- 2,880 retained-heap observations: stable/shared/unique values, two through six
  observed versions, collapsed history, and series without metadata.
- 288 fixed-work allocation diagnostics for parallel current lookup, using
  10,000 iterations so worker-buffer allocation is not confounded by calibration.
- 96 native-only cold-init and 100k distinct-churn pipeline observations.
- 12 additional CPU/allocation and mutex/block captures for the new arm.

Smoke sets contain 36 and 548 observations respectively, excluded from results.
Profiles, calibrated timings, and fixed-work allocation diagnostics remain
separate. Captures include fixture setup and calibration; do not treat total
profile samples as a scored steady-state measurement or infer cache misses from
source attribution alone. CPU steal continues to be recorded, not rejected.

The primary target is at least 5% paired median improvement versus the original
series arm in current unchanged lookup/append, one destination, stringlabels,
GOMAXPROCS 8, with all six pairs favorable in each cohort. Report protected
timing and total/incremental heap regressions above 5%, and stable allocations
separately from calibrated parallel-buffer effects. Always also report both
series arms versus baseline. Passing this focused comparison cannot reverse
the original full-study verdict or authorize adoption. Preserve the prototype
even if its performance target fails.

### Stringlabels optimization screen

The broad trial is retained as a superseded, incomplete run while optimization
work focuses on stringlabels. The screen imports its frozen sources and matching
stringlabels TSDB/remote binaries without recompilation, but never imports smoke
checks or measurements. The parent manifest is pinned by checksum and included
for provenance; offline analysis does not need the parent directory. This input
import deliberately permits an incomplete parent without relaxing measurement
completeness checks.

```sh
python3 scripts/benchmark-metadata-history-current.py freeze-screen SCREEN FROZEN_TRIAL
python3 SCREEN/benchmark-metadata-history-current.py smoke SCREEN
python3 SCREEN/benchmark-metadata-history-current.py run SCREEN
python3 SCREEN/benchmark-metadata-history-current.py analyze SCREEN
```

The exact stringlabels projection retains all four arms, the two separate
six-process cohorts, paired ordering, workloads, benchtimes, and assessment
thresholds. Its 564 blocks contain 2,220 observations: 1,056 Head timings, 960
retained-heap measurements, 96 fixed-work allocation diagnostics, 96 pipeline
measurements, and 12 profile captures. All 196 smoke observations are fresh.
CPU steal remains recorded and never triggers rejection or stopping.

The scope-change decision uses only progress, not performance: if at least 1,072
of the broad trial's 1,608 non-profile blocks are already accepted immediately
before cutover, let it finish instead. Otherwise stop the validated runner and
its separate benchmark/receiver process group, preserve partial output, and
archive/checksum the stopped run before measuring the screen. Record the scope
change separately rather than marking the old run complete or performance-failed.
No compilation or archival runs alongside replacement measurements.

A screen result cannot establish other label-build performance or authorize
adoption. The completed screen finished on 2026-09-22 at 11:27:21 UTC: all 196
smoke checks and 564 measurement blocks passed, with no exclusions or retries.
CPU-steal warnings in 16 measured observations remain included. The full archive,
verified sources/binaries, matched profiles, reproduced analysis and report are
retained under
`../benchmark-results/native-metadata/20260922-linux-current-value-coallocation/`.

The primary paired median improvement versus series-owned history is 4.45% and
4.54% in the separate cohorts, with all six pairs favorable in each. Benchstat
also finds a significant improvement (p=0.002/0.004), but neither cohort reaches
the frozen 5% target. The candidate remains 13.45%/14.11% slower than the native
forwarding baseline; this screen is not a native-versus-WAL comparison.

Co-allocation saves about one retained object per native-first series, not
bytes. Stable incremental heap remains about 105 B/series versus 96.4 in the
baseline. Cold pipeline allocations fall about 4.25%, but pipeline CPU results
are variable. No protected timing/heap or incremental-heap flags are raised
versus the original series arm. All 12 allocation flags remain recorded:
calibrated append iteration counts and chunk lifecycle differ, so nonsignificant
allocation comparisons do not establish parity. Fixed-iteration append
diagnostics remain follow-up work; frozen classifications are not changed.

Matched profiles are consistent with cheaper unchanged-append metadata checks,
but current sequential lookup still trails baseline by about 12%. The dependent
native-state read remains, and reference-index lock atomics are substantial in
both designs. Mutex profiles do not identify a large new blocking bottleneck.
Whole-process captures include setup, calibration and cleanup, with different
iteration counts; sample percentages are diagnostic, not measured speedups or
proof of cache misses. The report records bounded read-path/layout experiments
for future work. Preserve this candidate in a separate signed/DCO follow-up;
the original failed adoption gates remain unchanged.

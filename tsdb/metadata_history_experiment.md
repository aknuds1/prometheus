# Series-owned native metadata history experiment

Status: isolated prototype; Linux comparison pending. No adoption decision has
been made, and the forwarding branch remains unchanged.

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

The candidate leaves `memSeries` itself unchanged. Its two-pointer metadata
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

Pending the complete frozen comparison. Baseline profiles are diagnostic evidence
only; neither smoke runs nor partial cohorts establish an optimization win.

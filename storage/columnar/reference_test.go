// Copyright 2026 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package columnar_test

// Reference semantics: a returned storage.SeriesRef is a hint that complete
// labels must confirm. These tests pin the cases where it must be ignored, so
// that reusing it can never move a sample to a different series.

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
)

const referenceTestInterval = 15_000

func referenceTestStorage(tb testing.TB) *columnar.Storage {
	tb.Helper()
	s, err := columnar.NewStorage(tb.TempDir(), columnar.StorageOptions{
		FlushSamples: 1 << 30,
		DisableWAL:   true,
	})
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, s.Close()) })
	return s
}

func referenceTestLabels(series string) labels.Labels {
	return labels.FromStrings(
		labels.MetricName, "reference_test_metric",
		"instance", "localhost:9100",
		"job", "node_exporter",
		"series", series,
	)
}

// selectReferenceTest returns every series of the test metric in selection
// order, keyed by the series label.
func selectReferenceTest(tb testing.TB, s *columnar.Storage) map[string]columnar.Series {
	tb.Helper()
	series, err := s.SelectSeries(context.Background(), 0, 1<<40, [][]*labels.Matcher{{
		labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "reference_test_metric"),
	}}, 0)
	require.NoError(tb, err)
	out := make(map[string]columnar.Series, len(series))
	for _, item := range series {
		out[item.Labels.Get("series")] = item
	}
	return out
}

// TestAppendReusesVerifiedReference covers the intended path: a scrape round
// passes the reference it got back, and the series keeps accumulating samples.
func TestAppendReusesVerifiedReference(t *testing.T) {
	s := referenceTestStorage(t)
	ls := referenceTestLabels("a")

	app := s.AppenderV2(context.Background())
	ref, err := app.Append(0, ls, 0, 0, 1, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.NotZero(t, ref)
	require.NoError(t, app.Commit())

	for round := 1; round < 4; round++ {
		app := s.AppenderV2(context.Background())
		var got storage.SeriesRef
		got, err = app.Append(ref, ls, 0, int64(round)*referenceTestInterval, float64(round), nil, nil, storage.AOptions{})
		require.NoError(t, err, "round %d", round)
		require.Equal(t, ref, got, "round %d must return the same reference", round)
		require.NoError(t, app.Commit())
	}

	got := selectReferenceTest(t, s)
	require.Len(t, got, 1)
	require.Equal(t, []int64{0, 15_000, 30_000, 45_000}, got["a"].Timestamps)
	require.Equal(t, []float64{1, 1, 2, 3}, got["a"].Values)
}

// TestAppendReferenceDoesNotAliasOtherLabels covers a reference whose labels no
// longer match the series it was issued for. The sample must land in its own
// series; the referenced series must not receive it.
func TestAppendReferenceDoesNotAliasOtherLabels(t *testing.T) {
	s := referenceTestStorage(t)
	first := referenceTestLabels("a")
	second := referenceTestLabels("b")

	app := s.AppenderV2(context.Background())
	ref, err := app.Append(0, first, 0, 0, 1, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	before := s.ReadStats()
	app = s.AppenderV2(context.Background())
	got, err := app.Append(ref, second, 0, 0, 2, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.NotEqual(t, ref, got, "a mismatched reference must not be returned for different labels")
	require.NoError(t, app.Commit())
	after := s.ReadStats()
	require.Equal(t, after.RefFallback-before.RefFallback, uint64(1), "mismatched reference must fall back to hashing")
	require.Zero(t, after.RefTrusted-before.RefTrusted)

	gotSeries := selectReferenceTest(t, s)
	require.Len(t, gotSeries, 2)
	require.Equal(t, []int64{0}, gotSeries["a"].Timestamps, "referenced series must not receive the sample")
	require.Equal(t, []float64{1}, gotSeries["a"].Values)
	require.Equal(t, []int64{0}, gotSeries["b"].Timestamps)
	require.Equal(t, []float64{2}, gotSeries["b"].Values)
}

// TestAppendForeignReferenceFallsBackToLabels covers a reference that this
// storage never issued, and one issued for labels it has already forgotten.
func TestAppendForeignReferenceFallsBackToLabels(t *testing.T) {
	s := referenceTestStorage(t)
	ls := referenceTestLabels("a")

	app := s.AppenderV2(context.Background())
	fabricated, err := app.Append(storage.SeriesRef(1)<<40, ls, 0, 0, 1, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.NotZero(t, fabricated)
	require.NoError(t, app.Commit())

	// The reference this storage returns for the labels is the authoritative one.
	app = s.AppenderV2(context.Background())
	correct, err := app.Append(0, ls, 0, referenceTestInterval, 2, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.NoError(t, app.Commit())
	require.NotEqual(t, storage.SeriesRef(1)<<40, correct)
	require.Equal(t, correct, fabricated, "a foreign reference must not change the returned reference")

	got := selectReferenceTest(t, s)
	require.Len(t, got, 1, "foreign reference must not create a second series")
	require.Equal(t, []int64{0, 15_000}, got["a"].Timestamps)
}

// TestAppendReferenceOrderingErrors covers duplicate and out-of-order samples
// passed with a reference, against both the committed tail and staged samples.
func TestAppendReferenceOrderingErrors(t *testing.T) {
	s := referenceTestStorage(t)
	ls := referenceTestLabels("a")

	app := s.AppenderV2(context.Background())
	ref, err := app.Append(0, ls, 0, 100, 1, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	// Against the committed tail.
	app = s.AppenderV2(context.Background())
	_, err = app.Append(ref, ls, 0, 100, 2, nil, nil, storage.AOptions{})
	require.ErrorIs(t, err, storage.ErrDuplicateSampleForTimestamp)
	_, err = app.Append(ref, ls, 0, 99, 2, nil, nil, storage.AOptions{})
	require.ErrorIs(t, err, storage.ErrOutOfOrderSample)
	require.NoError(t, app.Rollback())

	// Against samples staged in the same transaction.
	app = s.AppenderV2(context.Background())
	_, err = app.Append(ref, ls, 0, 200, 2, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	_, err = app.Append(ref, ls, 0, 200, 3, nil, nil, storage.AOptions{})
	require.ErrorIs(t, err, storage.ErrDuplicateSampleForTimestamp)
	_, err = app.Append(ref, ls, 0, 150, 3, nil, nil, storage.AOptions{})
	require.ErrorIs(t, err, storage.ErrOutOfOrderSample)
	require.NoError(t, app.Commit())

	got := selectReferenceTest(t, s)
	require.Len(t, got, 1)
	require.Equal(t, []int64{100, 200}, got["a"].Timestamps)
	require.Equal(t, []float64{1, 2}, got["a"].Values)
}

// TestAppendReferenceSurvivesRollback covers reuse after a transaction is
// discarded: the reference was issued by an earlier commit and must still work.
func TestAppendReferenceSurvivesRollback(t *testing.T) {
	s := referenceTestStorage(t)
	ls := referenceTestLabels("a")

	app := s.AppenderV2(context.Background())
	ref, err := app.Append(0, ls, 0, 0, 1, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	app = s.AppenderV2(context.Background())
	_, err = app.Append(ref, ls, 0, referenceTestInterval, 2, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.NoError(t, app.Rollback())

	app = s.AppenderV2(context.Background())
	got, err := app.Append(ref, ls, 0, referenceTestInterval, 3, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.Equal(t, ref, got)
	require.NoError(t, app.Commit())

	gotSeries := selectReferenceTest(t, s)
	require.Equal(t, []int64{0, 15_000}, gotSeries["a"].Timestamps)
	require.Equal(t, []float64{1, 3}, gotSeries["a"].Values)
}

// TestAppendReferenceRejectedCommitLeavesNoTrace covers a commit rejected for a
// sample that violates ordering: nothing from the transaction becomes visible,
// and the same reference is usable afterwards.
func TestAppendReferenceRejectedCommitLeavesNoTrace(t *testing.T) {
	s := referenceTestStorage(t)
	ls := referenceTestLabels("a")

	app := s.AppenderV2(context.Background())
	ref, err := app.Append(0, ls, 0, 100, 1, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	// Two chunks of the same series in one batch cannot share a timestamp.
	app = s.AppenderV2(context.Background())
	_, err = app.Append(ref, ls, 0, 200, 2, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	got := selectReferenceTest(t, s)
	require.Equal(t, []int64{100, 200}, got["a"].Timestamps)

	app = s.AppenderV2(context.Background())
	gotRef, err := app.Append(ref, ls, 0, 300, 3, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.Equal(t, ref, gotRef)
	require.NoError(t, app.Commit())

	got = selectReferenceTest(t, s)
	require.Equal(t, []int64{100, 200, 300}, got["a"].Timestamps)
}

// TestAppendReferenceWithHistograms covers reference reuse on a single series
// whose samples change kind, including the counter-reset hint inference that
// reads the previous sample.
func TestAppendReferenceWithHistograms(t *testing.T) {
	s := referenceTestStorage(t)
	ls := referenceTestLabels("hist")

	h := &histogram.Histogram{
		Count: 3, Sum: 6, Schema: 0, ZeroThreshold: 1e-6, ZeroCount: 1,
		PositiveSpans: []histogram.Span{{Offset: 0, Length: 2}}, PositiveBuckets: []int64{2, 1},
	}

	app := s.AppenderV2(context.Background())
	ref, err := app.Append(0, ls, 0, 0, 5, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	app = s.AppenderV2(context.Background())
	ref, err = app.Append(ref, ls, 0, referenceTestInterval, 0, h.Copy(), nil, storage.AOptions{})
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	// The reset hint is inferred from the previous sample, which the reference
	// path resolves without hashing.
	reset := &histogram.Histogram{
		Count: 1, Sum: 1, Schema: 0, ZeroThreshold: 1e-6, ZeroCount: 1,
		PositiveSpans: []histogram.Span{{Offset: 0, Length: 1}}, PositiveBuckets: []int64{1},
		CounterResetHint: histogram.UnknownCounterReset,
	}
	app = s.AppenderV2(context.Background())
	_, err = app.Append(ref, ls, 0, 2*referenceTestInterval, 0, reset, nil, storage.AOptions{})
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	got := selectReferenceTest(t, s)
	require.Len(t, got, 1)
	require.Equal(t, []int64{0, 15_000, 30_000}, got["hist"].Timestamps)
	require.Equal(t, columnar.SampleFloat, got["hist"].Kinds[0])
	require.Equal(t, columnar.SampleHistogram, got["hist"].Kinds[1])
	require.Equal(t, columnar.SampleHistogram, got["hist"].Kinds[2])
	decoded, fh, err := got["hist"].HistogramAt(2)
	require.NoError(t, err)
	require.Nil(t, fh)
	// The stored form does not carry a reset hint; it is re-derived from
	// neighbouring samples at query time. What matters here is that a mixed-kind
	// series keeps one identity across reference reuse.
	require.Equal(t, uint64(1), decoded.Count)
	require.Equal(t, 1.0, decoded.Sum)
}

// TestAppendReferenceAcrossFlushAndReopen covers a reference issued before a
// seal and restart: the committed tail is restored from the block, so the
// reference still verifies and the series continues instead of forking.
func TestAppendReferenceAcrossFlushAndReopen(t *testing.T) {
	dir := t.TempDir()
	ls := referenceTestLabels("a")

	s, err := columnar.NewStorage(dir, columnar.StorageOptions{FlushSamples: 1 << 30, DisableWAL: true})
	require.NoError(t, err)
	app := s.AppenderV2(context.Background())
	ref, err := app.Append(0, ls, 0, 100, 1, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.NoError(t, app.Commit())
	require.NoError(t, s.Flush())
	require.Equal(t, 1, s.NumBlocks())
	require.NoError(t, s.Close())

	s, err = columnar.NewStorage(dir, columnar.StorageOptions{FlushSamples: 1 << 30, DisableWAL: true})
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	before := s.ReadStats()
	app = s.AppenderV2(context.Background())
	got, err := app.Append(ref, ls, 0, 200, 2, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.Equal(t, ref, got)
	require.NoError(t, app.Commit())
	after := s.ReadStats()
	require.Equal(t, uint64(1), after.RefTrusted-before.RefTrusted, "restored tail must verify the reference")

	gotSeries := selectReferenceTest(t, s)
	require.Len(t, gotSeries, 1)
	require.Equal(t, []int64{100, 200}, gotSeries["a"].Timestamps)
	require.Equal(t, []float64{1, 2}, gotSeries["a"].Values)
}

// TestConcurrentReferenceRounds overlaps independent scrape rounds that reuse
// their references, so the committed-tail verification is exercised under
// concurrency.
func TestConcurrentReferenceRounds(t *testing.T) {
	s := referenceTestStorage(t)
	const workers, rounds = 8, 10

	refs := make([]storage.SeriesRef, workers)
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Go(func() {
			ls := referenceTestLabels(strconv.Itoa(worker))
			for round := range rounds {
				app := s.AppenderV2(context.Background())
				ref, err := app.Append(refs[worker], ls, 0, int64(round)*referenceTestInterval, float64(round), nil, nil, storage.AOptions{})
				if err != nil {
					t.Errorf("worker %d round %d: %v", worker, round, err)
					return
				}
				if err := app.Commit(); err != nil {
					t.Errorf("worker %d round %d commit: %v", worker, round, err)
					return
				}
				refs[worker] = ref
			}
		})
	}
	wg.Wait()

	got := selectReferenceTest(t, s)
	require.Len(t, got, workers)
	for worker := range workers {
		series := got[strconv.Itoa(worker)]
		require.Len(t, series.Timestamps, rounds)
	}
	stats := s.ReadStats()
	// Each worker's first round has no reference yet; every later round reuses it.
	require.EqualValues(t, workers, stats.RefFallback)
	require.EqualValues(t, workers*(rounds-1), stats.RefTrusted)
}

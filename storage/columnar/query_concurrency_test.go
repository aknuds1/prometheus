// Copyright The Prometheus Authors
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

package columnar

// TestConcurrentHeadSelectAndCommit is the permanent regression test for the
// head read/commit race: a querier must be able to select head series and
// iterate every sample while other goroutines commit new batches.
//
// It deliberately does not stop ingestion before reading. Every returned sample
// is checked exactly, and a batch that is only partly applied is visible as a
// sample count that is not a whole number of committed batches (or as a bounds
// failure), so the test fails if the store ever publishes torn state.

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

const (
	// concurrentHeadBatches is the number of writer batches. Each batch appends
	// three samples to the seeded series, two to a series it creates on its first
	// batch, and one histogram sample every third batch.
	concurrentHeadBatches = 400
	// concurrentHeadSeedSampleCount is the number of exact samples seeded before
	// the writer starts.
	concurrentHeadSeedSampleCount = 4
	// Writer timestamps start above the seeded range, leaving a gap that a pruned
	// selection can prove empty.
	concurrentHeadWriterStart = 1000
	concurrentHeadNewStart    = 2000
	concurrentHeadHistStart   = 3000
)

// concurrentHeadValueBits is the deterministic float bit pattern the seed
// series carries at timestamp t. Every seventh timestamp is a stale marker, so
// staleness is exercised while commits are concurrent.
func concurrentHeadValueBits(t int64) uint64 {
	if t%7 == 0 {
		return value.StaleNaN
	}
	return 0x3ff0000000000000 + uint64(t)
}

// concurrentHeadHistograms returns the int or float histogram the histogram
// series carries at one of its timestamps.
func concurrentHeadHistograms(intHistogram bool) (*histogram.Histogram, *histogram.FloatHistogram) {
	spans := []histogram.Span{{Offset: 0, Length: 1}}
	if intHistogram {
		return &histogram.Histogram{
			Count: 1, Sum: 1, Schema: 0,
			PositiveSpans: spans, PositiveBuckets: []int64{1},
		}, nil
	}
	return nil, &histogram.FloatHistogram{
		Count: 1, Sum: 1, Schema: 0,
		PositiveSpans: spans, PositiveBuckets: []float64{1},
	}
}

// concurrentHeadHistExpectation describes one histogram sample the writer adds.
type concurrentHeadHistExpectation struct {
	intHistogram bool
}

func TestConcurrentHeadSelectAndCommit(t *testing.T) {
	dir := t.TempDir()
	opts := StorageOptions{FlushSamples: 1_000_000, DisableWAL: true}
	s, err := NewStorage(dir, opts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	seedLabels := labels.FromStrings(labels.MetricName, "concurrent_head_seed")
	newLabels := labels.FromStrings(labels.MetricName, "concurrent_head_new")
	histLabels := labels.FromStrings(labels.MetricName, "concurrent_head_hist")

	// Seed one exact special value per kind: positive zero, negative zero, a NaN
	// payload and a stale marker.
	app := s.AppenderV2(context.Background())
	for _, seed := range []struct {
		t    int64
		bits uint64
	}{
		{t: 0, bits: 0x0000000000000000},
		{t: 1, bits: 0x8000000000000000},
		{t: 2, bits: 0x7ff8000000000001},
		{t: 3, bits: value.StaleNaN},
	} {
		_, err := app.Append(0, seedLabels, 0, seed.t, math.Float64frombits(seed.bits), nil, nil, storage.AOptions{})
		require.NoError(t, err)
	}
	require.NoError(t, app.Commit())

	// The histogram payloads are fixed before the goroutines start and are only
	// read afterwards, so readers need no extra synchronization for them.
	histExpectations := make(map[int64]concurrentHeadHistExpectation, concurrentHeadBatches/3)
	for gen := int64(1); gen <= concurrentHeadBatches; gen++ {
		if gen%3 != 0 {
			continue
		}
		histExpectations[concurrentHeadHistStart+gen] = concurrentHeadHistExpectation{intHistogram: (gen/3)%2 == 1}
	}

	var (
		wg     sync.WaitGroup
		errMtx sync.Mutex
		errs   []error
	)
	addErr := func(err error) {
		errMtx.Lock()
		defer errMtx.Unlock()
		errs = append(errs, err)
	}
	started := make(chan struct{})
	done := make(chan struct{})
	var startOnce sync.Once

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for gen := int64(1); gen <= concurrentHeadBatches; gen++ {
			app := s.AppenderV2(context.Background())
			for j := int64(0); j < 3; j++ {
				ts := concurrentHeadWriterStart + gen*3 + j
				if _, err := app.Append(0, seedLabels, 0, ts, math.Float64frombits(concurrentHeadValueBits(ts)), nil, nil, storage.AOptions{}); err != nil {
					addErr(fmt.Errorf("append seed sample at %d: %w", ts, err))
					_ = app.Rollback()
					return
				}
			}
			for j := int64(0); j < 2; j++ {
				ts := concurrentHeadNewStart + gen*2 + j
				if _, err := app.Append(0, newLabels, 0, ts, math.Float64frombits(concurrentHeadValueBits(ts)), nil, nil, storage.AOptions{}); err != nil {
					addErr(fmt.Errorf("append new-series sample at %d: %w", ts, err))
					_ = app.Rollback()
					return
				}
			}
			if gen%3 == 0 {
				ts := concurrentHeadHistStart + gen
				expectation := histExpectations[ts]
				h, fh := concurrentHeadHistograms(expectation.intHistogram)
				if _, err := app.Append(0, histLabels, ts-5, ts, 0, h, fh, storage.AOptions{}); err != nil {
					addErr(fmt.Errorf("append histogram sample at %d: %w", ts, err))
					_ = app.Rollback()
					return
				}
			}
			if err := app.Commit(); err != nil {
				addErr(fmt.Errorf("commit batch %d: %w", gen, err))
				return
			}
			startOnce.Do(func() { close(started) })
		}
	}()

	// Two readers verify every sample of every buffered series; one reader only
	// exercises no-match and range-pruned selections, which must never invent a
	// match. All readers run until the writer has committed its last batch.
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-started
			for {
				if err := verifyConcurrentHeadSnapshot(s, seedLabels, newLabels, histLabels, histExpectations); err != nil {
					addErr(err)
					return
				}
				select {
				case <-done:
					return
				default:
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-started
		for {
			if err := verifyConcurrentHeadPruned(s, seedLabels); err != nil {
				addErr(err)
				return
			}
			if err := verifyConcurrentSelectSeries(s, seedLabels); err != nil {
				addErr(err)
				return
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	wg.Wait()

	errMtx.Lock()
	collected := errs
	errMtx.Unlock()
	for _, err := range collected {
		require.NoError(t, err)
	}

	// After the writer stops, the complete retained state must match exactly, and
	// a flush plus reopen must preserve it.
	require.NoError(t, verifyConcurrentHeadSnapshot(s, seedLabels, newLabels, histLabels, histExpectations))
	require.NoError(t, verifyConcurrentSelectSeries(s, seedLabels))
	require.NoError(t, s.Flush())
	require.NoError(t, verifyConcurrentHeadSnapshot(s, seedLabels, newLabels, histLabels, histExpectations))
	require.NoError(t, verifyConcurrentSelectSeries(s, seedLabels))

	require.NoError(t, s.Close())
	reopened, err := NewStorage(dir, opts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	require.NoError(t, verifyConcurrentHeadSnapshot(reopened, seedLabels, newLabels, histLabels, histExpectations))
	require.NoError(t, verifyConcurrentSelectSeries(reopened, seedLabels))
}

// verifyConcurrentHeadSnapshot selects every head series by name and checks
// every returned sample, including the batch-aligned sample counts that prove a
// batch is published whole.
func verifyConcurrentHeadSnapshot(q storage.Queryable, seedLabels, newLabels, histLabels labels.Labels, histExpectations map[int64]concurrentHeadHistExpectation) error {
	querier, err := q.Querier(math.MinInt64, math.MaxInt64)
	if err != nil {
		return fmt.Errorf("creating querier: %w", err)
	}
	defer querier.Close()

	set := querier.Select(context.Background(), true, nil,
		labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, "concurrent_head_.+"))
	counts := map[string]int{}
	for set.Next() {
		series := set.At()
		name := series.Labels().Get(labels.MetricName)
		if _, seen := counts[name]; seen {
			return fmt.Errorf("series %s returned twice", name)
		}
		counts[name] = 0
		switch name {
		case seedLabels.Get(labels.MetricName):
			count, err := verifySeedSeries(series)
			if err != nil {
				return err
			}
			counts[name] = count
		case newLabels.Get(labels.MetricName):
			count, err := verifyGrownSeries(series, concurrentHeadNewStart)
			if err != nil {
				return err
			}
			counts[name] = count
		case histLabels.Get(labels.MetricName):
			count, err := verifyHistogramSeries(series, histExpectations)
			if err != nil {
				return err
			}
			counts[name] = count
		default:
			return fmt.Errorf("unexpected series %s", series.Labels())
		}
	}
	if err := set.Err(); err != nil {
		return fmt.Errorf("selecting head series: %w", err)
	}

	// LabelValues reads the same head snapshot; it must report exactly the names
	// Select just returned, so the label path is exercised under the same
	// concurrent commits.
	names, _, err := querier.LabelValues(context.Background(), labels.MetricName, nil,
		labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, "concurrent_head_.+"))
	if err != nil {
		return fmt.Errorf("reading head label values: %w", err)
	}
	wantNames := make([]string, 0, len(counts))
	for name := range counts {
		wantNames = append(wantNames, name)
	}
	slices.Sort(wantNames)
	if !slices.Equal(names, wantNames) {
		return fmt.Errorf("LabelValues returned %v, want the selected names %v", names, wantNames)
	}

	seedCount, ok := counts[seedLabels.Get(labels.MetricName)]
	if !ok {
		return fmt.Errorf("the seeded series disappeared")
	}
	if seedCount < concurrentHeadSeedSampleCount || (seedCount-concurrentHeadSeedSampleCount)%3 != 0 {
		return fmt.Errorf("seed series has %d samples, which is not %d plus whole 3-sample batches",
			seedCount, concurrentHeadSeedSampleCount)
	}
	batches := (seedCount - concurrentHeadSeedSampleCount) / 3

	newCount, hasNew := counts[newLabels.Get(labels.MetricName)]
	switch {
	case batches == 0 && hasNew:
		return fmt.Errorf("new series exists before its first batch")
	case batches > 0 && !hasNew:
		return fmt.Errorf("new series is missing after %d batches (seed %d)", batches, seedCount)
	case batches > 0 && newCount != 2*batches:
		return fmt.Errorf("new series has %d samples after %d batches, want %d", newCount, batches, 2*batches)
	}

	histCount, hasHist := counts[histLabels.Get(labels.MetricName)]
	wantHist := 0
	for t := range histExpectations {
		if t-concurrentHeadHistStart <= int64(batches) {
			wantHist++
		}
	}
	switch {
	case wantHist == 0 && hasHist:
		return fmt.Errorf("histogram series exists before its first sample")
	case wantHist > 0 && !hasHist:
		return fmt.Errorf("histogram series is missing after %d batches (seed %d, want %d histogram samples)", batches, seedCount, wantHist)
	case wantHist > 0 && histCount != wantHist:
		return fmt.Errorf("histogram series has %d samples after %d batches, want %d", histCount, batches, wantHist)
	}
	return nil
}

// verifySeedSeries checks the exact seeded values and the deterministic pattern
// the writer appends after them.
func verifySeedSeries(series storage.Series) (int, error) {
	seeds := []uint64{0x0000000000000000, 0x8000000000000000, 0x7ff8000000000001, value.StaleNaN}
	iterator := series.Iterator(nil)
	count := 0
	for typ := iterator.Next(); typ != chunkenc.ValNone; typ = iterator.Next() {
		if typ != chunkenc.ValFloat {
			return 0, fmt.Errorf("unexpected value type %v in seed series", typ)
		}
		t := iterator.AtT()
		_, v := iterator.At()
		bits := math.Float64bits(v)
		if count < len(seeds) {
			if t != int64(count) || bits != seeds[count] {
				return 0, fmt.Errorf("seed sample %d is t=%d bits=%#x, want t=%d bits=%#x", count, t, bits, count, seeds[count])
			}
		} else {
			want := concurrentHeadValueBits(t)
			if bits != want {
				return 0, fmt.Errorf("seed sample at t=%d has bits %#x, want %#x", t, bits, want)
			}
			if want := concurrentHeadWriterStart + 3 + int64(count-len(seeds)); t != want {
				return 0, fmt.Errorf("seed sample %d has timestamp %d, want %d", count, t, want)
			}
		}
		count++
	}
	if err := iterator.Err(); err != nil {
		return 0, fmt.Errorf("iterating seed series: %w", err)
	}
	return count, nil
}

// verifyGrownSeries checks a series whose writer timestamps are contiguous from
// start+2.
func verifyGrownSeries(series storage.Series, start int64) (int, error) {
	iterator := series.Iterator(nil)
	count := 0
	var previous int64 = math.MinInt64
	for iterator.Next() != chunkenc.ValNone {
		t := iterator.AtT()
		if t <= previous {
			return 0, fmt.Errorf("series %s timestamps are not strictly increasing at %d", series.Labels(), t)
		}
		previous = t
		_, v := iterator.At()
		if got, want := math.Float64bits(v), concurrentHeadValueBits(t); got != want {
			return 0, fmt.Errorf("series %s sample at t=%d has bits %#x, want %#x", series.Labels(), t, got, want)
		}
		if want := start + 2 + int64(count); t != want {
			return 0, fmt.Errorf("series %s sample %d has timestamp %d, want %d", series.Labels(), count, t, want)
		}
		count++
	}
	if err := iterator.Err(); err != nil {
		return 0, fmt.Errorf("iterating series %s: %w", series.Labels(), err)
	}
	return count, nil
}

// verifyHistogramSeries checks every histogram payload and start timestamp.
func verifyHistogramSeries(series storage.Series, expectations map[int64]concurrentHeadHistExpectation) (int, error) {
	iterator := series.Iterator(nil)
	count := 0
	var previous int64 = math.MinInt64
	for typ := iterator.Next(); typ != chunkenc.ValNone; typ = iterator.Next() {
		t := iterator.AtT()
		if t <= previous {
			return 0, fmt.Errorf("histogram timestamps are not strictly increasing at %d", t)
		}
		previous = t
		expectation, ok := expectations[t]
		if !ok {
			return 0, fmt.Errorf("unexpected histogram timestamp %d", t)
		}
		wantInt, wantFloat := concurrentHeadHistograms(expectation.intHistogram)
		switch typ {
		case chunkenc.ValHistogram:
			_, h := iterator.AtHistogram(nil)
			if wantInt == nil || !h.Equals(wantInt) {
				return 0, fmt.Errorf("histogram at t=%d does not equal the committed payload", t)
			}
		case chunkenc.ValFloatHistogram:
			_, fh := iterator.AtFloatHistogram(nil)
			if wantFloat == nil || !fh.Equals(wantFloat) {
				return 0, fmt.Errorf("float histogram at t=%d does not equal the committed payload", t)
			}
		default:
			return 0, fmt.Errorf("histogram series returned value type %v at t=%d", typ, t)
		}
		if st := iterator.AtST(); st != t-5 {
			return 0, fmt.Errorf("histogram at t=%d has start timestamp %d, want %d", t, st, t-5)
		}
		count++
	}
	if err := iterator.Err(); err != nil {
		return 0, fmt.Errorf("iterating histogram series: %w", err)
	}
	return count, nil
}

// verifyConcurrentSelectSeries exercises the exported multi-matcher head path
// used by the Arrow endpoint, under the same concurrent commits.
func verifyConcurrentSelectSeries(s *Storage, seedLabels labels.Labels) error {
	series, err := s.SelectSeries(context.Background(), math.MinInt64, math.MaxInt64, [][]*labels.Matcher{{
		labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, seedLabels.Get(labels.MetricName)),
	}}, 0)
	if err != nil {
		return fmt.Errorf("selecting head series directly: %w", err)
	}
	if len(series) != 1 {
		return fmt.Errorf("direct head selection returned %d series, want exactly the seeded one", len(series))
	}
	if !labels.Equal(series[0].Labels, seedLabels) {
		return fmt.Errorf("direct head selection returned %s, want %s", series[0].Labels, seedLabels)
	}
	count, err := verifySeedSeries(&seriesView{s: series[0]})
	if err != nil {
		return fmt.Errorf("direct head selection: %w", err)
	}
	if count < concurrentHeadSeedSampleCount || (count-concurrentHeadSeedSampleCount)%3 != 0 {
		return fmt.Errorf("direct head selection has %d samples, which is not %d plus whole 3-sample batches",
			count, concurrentHeadSeedSampleCount)
	}
	return nil
}

// seriesView adapts a returned Series to storage.Series for the shared checks.
type seriesView struct {
	s Series
}

func (v *seriesView) Labels() labels.Labels { return v.s.Labels }

func (v *seriesView) Iterator(chunkenc.Iterator) chunkenc.Iterator {
	return &sampleIterator{series: v.s, pos: -1}
}

// verifyConcurrentHeadPruned checks that no-match and out-of-range selections do
// not invent a match while commits run.
func verifyConcurrentHeadPruned(q storage.Queryable, seedLabels labels.Labels) error {
	querier, err := q.Querier(math.MinInt64, math.MaxInt64)
	if err != nil {
		return fmt.Errorf("creating pruned querier: %w", err)
	}
	defer querier.Close()

	absent := querier.Select(context.Background(), true, nil,
		labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "concurrent_head_absent"))
	if absent.Next() {
		return fmt.Errorf("a no-match selection returned %s", absent.At().Labels())
	}
	if err := absent.Err(); err != nil {
		return fmt.Errorf("no-match selection: %w", err)
	}

	// The gap between the seeded samples and the writer's first timestamp is
	// empty for the whole test.
	gap := querier.Select(context.Background(), true,
		&storage.SelectHints{Start: 10, End: 99},
		labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, seedLabels.Get(labels.MetricName)))
	if gap.Next() {
		return fmt.Errorf("the empty timestamp gap returned %s", gap.At().Labels())
	}
	if err := gap.Err(); err != nil {
		return fmt.Errorf("gap selection: %w", err)
	}

	// Only the first writer batch lies in [1003, 1005], so the series is either
	// absent or carries exactly those three samples.
	first := querier.Select(context.Background(), true,
		&storage.SelectHints{Start: concurrentHeadWriterStart + 3, End: concurrentHeadWriterStart + 5},
		labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, seedLabels.Get(labels.MetricName)))
	count := 0
	for first.Next() {
		iterator := first.At().Iterator(nil)
		for iterator.Next() != chunkenc.ValNone {
			if t := iterator.AtT(); t < concurrentHeadWriterStart+3 || t > concurrentHeadWriterStart+5 {
				return fmt.Errorf("pruned selection returned out-of-range sample %d", t)
			}
			count++
		}
		if err := iterator.Err(); err != nil {
			return fmt.Errorf("iterating the first batch window: %w", err)
		}
	}
	if err := first.Err(); err != nil {
		return fmt.Errorf("first batch window selection: %w", err)
	}
	if count != 0 && count != 3 {
		return fmt.Errorf("first batch window returned %d samples, want 0 or 3", count)
	}
	return nil
}

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

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

func TestMergedColumnarIteratorBatchPreservesSampleOrder(t *testing.T) {
	s, err := NewStorage(t.TempDir(), StorageOptions{FlushSamples: 1, DisableWAL: true, DisableNativeMetadata: true})
	require.NoError(t, err)
	defer s.Close()
	ls := labels.FromStrings(labels.MetricName, "merged_batch")
	for _, timestamp := range []int64{100, 200, 300} {
		require.NoError(t, s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{batchFloatChunk(ls, timestamp)}}))
	}
	q, err := s.Querier(0, 300)
	require.NoError(t, err)
	defer q.Close()
	set := q.Select(t.Context(), false, nil, labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "merged_batch"))
	require.True(t, set.Next())
	batch, ok := set.At().Iterator(nil).(storage.BatchIterator)
	require.True(t, ok)
	require.True(t, batch.NextBatch(0))
	require.Equal(t, []int64{100, 200, 300}, batch.Batch().Timestamps)
	require.Equal(t, []float64{100, 200, 300}, batch.Batch().Values)
	require.False(t, batch.NextBatch(0))
	require.NoError(t, batch.Err())
	require.False(t, set.Next())
	require.NoError(t, set.Err())
}

func TestSampleIteratorBatchMatchesScalar(t *testing.T) {
	item := NewFloatSeries(labels.FromStrings(labels.MetricName, "batch_query"), []int64{0, 60_000, 120_000}, []float64{1, 3, 5})
	direct := &sampleIterator{series: item, pos: -1}
	batchIt, ok := any(direct).(storage.BatchIterator)
	require.True(t, ok)
	require.True(t, batchIt.NextBatch(0))
	require.Equal(t, 3, batchIt.Batch().Len())
	require.Equal(t, []int64{0, 60_000, 120_000}, batchIt.Batch().Timestamps)
	require.Equal(t, []float64{1, 3, 5}, batchIt.Batch().Values)
	timestamp, value := batchIt.At()
	require.Equal(t, int64(120_000), timestamp)
	require.Equal(t, 5.0, value)
	require.Equal(t, chunkenc.ValNone, batchIt.Next())

	reads := &readStats{}
	buffered := storage.NewBufferIterator(&sampleIterator{series: item, reads: reads, pos: -1}, 120_000)
	require.Equal(t, chunkenc.ValFloat, buffered.Seek(120_000))
	ring := buffered.Buffer()
	require.Equal(t, chunkenc.ValFloat, ring.Next())
	_, value = ring.At()
	require.Equal(t, 1.0, value)
	require.Equal(t, chunkenc.ValFloat, ring.Next())
	_, value = ring.At()
	require.Equal(t, 3.0, value)
	_, value = buffered.At()
	require.Equal(t, 5.0, value)
	require.Equal(t, chunkenc.ValNone, ring.Next())
	require.Equal(t, uint64(1), reads.sampleBatches.Load())

	paged := &sampleIterator{series: item, pos: -1}
	require.True(t, paged.NextBatch(1))
	require.Equal(t, int64(0), paged.AtT())
	require.Equal(t, chunkenc.ValFloat, paged.Next())
	require.Equal(t, int64(60_000), paged.AtT())
	require.Equal(t, chunkenc.ValFloat, paged.Next())
	require.Equal(t, int64(120_000), paged.AtT())

	ranged := &sampleIterator{series: item, pos: -1}
	require.True(t, ranged.NextBatchUntil(0, 60_000))
	require.Equal(t, []int64{0, 60_000}, ranged.Batch().Timestamps)
	require.False(t, ranged.NextBatchUntil(0, 60_000), "the already consumed boundary is not returned twice")
	require.True(t, ranged.NextBatchUntil(0, 120_000), "a future sample remains available")
	require.Equal(t, []int64{120_000}, ranged.Batch().Timestamps)
}

func TestRangeBatchHeadQueriesMatchScalar(t *testing.T) {
	s, err := NewStorage(t.TempDir(), StorageOptions{FlushSamples: 1_000_000, DisableWAL: true, DisableNativeMetadata: true})
	require.NoError(t, err)
	defer s.Close()
	floatLabels := labels.FromStrings(labels.MetricName, "range_batch_requests")
	histogramLabels := labels.FromStrings(labels.MetricName, "range_batch_histogram")
	histogramPayload := func(count uint64) []byte {
		return typedHistogramPayload(t, prompb.Histogram{
			Count: &prompb.Histogram_CountInt{CountInt: count}, Sum: float64(count),
			PositiveSpans: []prompb.BucketSpan{{Length: 1}}, PositiveDeltas: []int64{1},
		})
	}
	batch := Batch{Series: []BatchSeries{
		{Labels: floatLabels, Timestamps: []int64{0, 60_000}, Kinds: []SampleKind{0, 0}, Values: []float64{1, 3}},
		{
			Labels: histogramLabels, Timestamps: []int64{0, 60_000}, Kinds: []SampleKind{SampleHistogram, SampleHistogram},
			Values: []float64{0, 0}, Histograms: [][]byte{histogramPayload(1), histogramPayload(2)},
		},
	}}
	require.NoError(t, s.AppendBatch(t.Context(), batch))
	engine := promqltest.NewTestEngine(t, false, 5*time.Minute, promqltest.DefaultMaxSamplesPerQuery)
	for _, expr := range []string{
		"sum(range_batch_requests)", "rate(range_batch_requests[2m])", "rate(range_batch_histogram[2m])",
	} {
		t.Run(expr, func(t *testing.T) {
			evaluate := func(q storage.Queryable) string {
				query, err := engine.NewInstantQuery(t.Context(), q, nil, expr, time.UnixMilli(60_000))
				require.NoError(t, err)
				result := query.Exec(t.Context())
				defer query.Close()
				require.NoError(t, result.Err)
				return result.Value.String()
			}
			direct := evaluate(s)
			scalar := evaluate(scalarRangeBatchQueryable{Queryable: s})
			require.Equal(t, scalar, direct)
		})
	}
}

type scalarRangeBatchQueryable struct{ storage.Queryable }

func (q scalarRangeBatchQueryable) Querier(mint, maxt int64) (storage.Querier, error) {
	inner, err := q.Queryable.Querier(mint, maxt)
	return scalarRangeBatchQuerier{Querier: inner}, err
}

type scalarRangeBatchQuerier struct{ storage.Querier }

func (q scalarRangeBatchQuerier) Select(ctx context.Context, sortSeries bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	return scalarRangeBatchSeriesSet{SeriesSet: q.Querier.Select(ctx, sortSeries, hints, matchers...)}
}

type scalarRangeBatchSeriesSet struct{ storage.SeriesSet }

func (s scalarRangeBatchSeriesSet) At() storage.Series {
	return scalarRangeBatchSeries{Series: s.SeriesSet.At()}
}

type scalarRangeBatchSeries struct{ storage.Series }

func (s scalarRangeBatchSeries) Iterator(reuse chunkenc.Iterator) chunkenc.Iterator {
	return scalarOnlyIterator{Iterator: s.Series.Iterator(reuse)}
}

func TestBufferedBatchSeekMatchesScalar(t *testing.T) {
	item := NewFloatSeries(labels.FromStrings(labels.MetricName, "batch_seek"), []int64{0, 60_000, 120_000, 180_000}, []float64{1, 2, 3, 4})
	read := func(batch bool) ([]int64, []float64, chunkenc.ValueType) {
		var input chunkenc.Iterator = &sampleIterator{series: item, pos: -1}
		if !batch {
			input = scalarOnlyIterator{Iterator: input}
		}
		wrapped := storage.NewBufferIterator(input, 120_000)
		var times []int64
		var values []float64
		typeofSample := wrapped.Seek(180_000)
		ring := wrapped.Buffer()
		for ring.Next() != chunkenc.ValNone {
			t, f := ring.At()
			times = append(times, t)
			values = append(values, f)
		}
		if typeofSample != chunkenc.ValNone {
			t, f := wrapped.At()
			times = append(times, t)
			values = append(values, f)
		}
		return times, values, typeofSample
	}
	wantTimes, wantValues, wantType := read(false)
	gotTimes, gotValues, gotType := read(true)
	require.Equal(t, wantTimes, gotTimes)
	require.Equal(t, wantValues, gotValues)
	require.Equal(t, wantType, gotType)
}

type scalarOnlyIterator struct{ chunkenc.Iterator }

func TestSampleIteratorMixedBatchMatchesScalar(t *testing.T) {
	intPayload := typedHistogramPayload(t, prompb.Histogram{Count: &prompb.Histogram_CountInt{CountInt: 2}, Sum: 3})
	floatPayload := typedHistogramPayload(t, prompb.Histogram{Count: &prompb.Histogram_CountFloat{CountFloat: 2.5}, Sum: 3.5})
	item := Series{
		Labels: labels.FromStrings(labels.MetricName, "mixed_batch"), Timestamps: []int64{1, 2, 3, 4},
		StartTimestamps: []int64{0, 0, 1, 2}, Kinds: []SampleKind{SampleFloat, SampleStale, SampleHistogram, SampleFloatHistogram},
		Values: []float64{1, math.Float64frombits(value.StaleNaN), 0, 0}, Histograms: [][]byte{nil, nil, intPayload, floatPayload},
	}

	collectScalar := func() Series {
		it := &sampleIterator{series: item, pos: -1}
		out := Series{Labels: item.Labels}
		for it.Next() != chunkenc.ValNone {
			out.Timestamps = append(out.Timestamps, it.AtT())
			out.StartTimestamps = append(out.StartTimestamps, it.AtST())
			out.Kinds = append(out.Kinds, item.Kinds[it.pos])
			switch item.Kinds[it.pos] {
			case SampleFloat, SampleStale:
				_, f := it.At()
				out.Values = append(out.Values, f)
				out.Histograms = append(out.Histograms, nil)
			case SampleHistogram:
				h, _, err := item.HistogramAt(it.pos)
				require.NoError(t, err)
				payload := typedHistogramPayload(t, prompb.FromIntHistogram(0, h))
				out.Values = append(out.Values, 0)
				out.Histograms = append(out.Histograms, payload)
			case SampleFloatHistogram:
				_, fh, err := item.HistogramAt(it.pos)
				require.NoError(t, err)
				out.Values = append(out.Values, 0)
				out.Histograms = append(out.Histograms, typedHistogramPayload(t, prompb.FromFloatHistogram(0, fh)))
			}
		}
		return out
	}
	want := collectScalar()
	it := &sampleIterator{series: item, pos: -1}
	var got []storage.SampleBatch
	require.True(t, it.NextBatch(2))
	got = append(got, it.Batch())
	require.True(t, it.NextBatch(2))
	got = append(got, it.Batch())
	require.False(t, it.NextBatch(2))
	for batchIndex, batch := range got {
		for i := range batch.Len() {
			absolute := batchIndex*2 + i
			require.Equal(t, want.Timestamps[absolute], batch.Timestamps[i])
			require.Equal(t, want.StartTimestamps[absolute], batch.StartTimestamp(i))
			switch want.Kinds[absolute] {
			case SampleFloat, SampleStale:
				require.Equal(t, chunkenc.ValFloat, batch.ValueType(i))
				require.Equal(t, math.Float64bits(want.Values[absolute]), math.Float64bits(batch.Values[i]))
			case SampleHistogram:
				require.Equal(t, chunkenc.ValHistogram, batch.ValueType(i))
				require.Equal(t, want.Histograms[absolute], typedHistogramPayload(t, prompb.FromIntHistogram(0, batch.Histograms[i])))
			case SampleFloatHistogram:
				require.Equal(t, chunkenc.ValFloatHistogram, batch.ValueType(i))
				require.Equal(t, want.Histograms[absolute], typedHistogramPayload(t, prompb.FromFloatHistogram(0, batch.FloatHistograms[i])))
			}
		}
	}
}

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
	"path/filepath"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
)

func TestSampleSegmentPageRoundTrip(t *testing.T) {
	lset := labels.FromStrings(labels.MetricName, "segment_metric")
	h := &histogram.Histogram{
		Count: 3, Sum: 6, Schema: 0, ZeroThreshold: 1e-6, ZeroCount: 1,
		PositiveSpans: []histogram.Span{{Offset: 0, Length: 2}}, PositiveBuckets: []int64{1, 1},
	}
	payload, err := encodeHistogram(h, nil)
	require.NoError(t, err)
	segment, err := NewSampleSegment(
		lset,
		[]int64{0, 100, 200, 300, 400},
		[]int64{10, 10, 20, 20, 30},
		[]SampleKind{SampleFloat, SampleFloat, SampleHistogram, SampleStale, SampleFloat},
		[]float64{1.25, 1.5, 0, 0, 1.5},
		[][]byte{nil, nil, payload, nil, nil},
	)
	require.NoError(t, err)
	require.Equal(t, 5, segment.Count)
	require.Equal(t, int64(0), segment.MinTime)
	require.Equal(t, int64(400), segment.MaxTime)

	iterator := segment.NewPageIterator(lset)
	require.True(t, iterator.Next())
	page := iterator.Page()
	require.Equal(t, []int64{0, 100, 200, 300, 400}, page.Timestamps)
	require.Equal(t, []int64{10, 10, 20, 20, 30}, page.StartTimestamps)
	require.Equal(t, []SampleKind{SampleFloat, SampleFloat, SampleHistogram, SampleStale, SampleFloat}, page.Kinds)
	require.Equal(t, []float64{1.25, 1.5, 0, 0, 1.5}, page.Values)
	require.Nil(t, page.Histograms[0])
	require.NoError(t, iterator.Err())
	decoded, _, err := NewTypedPageSeries(page).HistogramAt(2)
	require.NoError(t, err)
	require.True(t, h.Equals(decoded))
	require.False(t, iterator.Next())
	require.NoError(t, iterator.Close())
}

func TestSampleSegmentRejectsNonIncreasingTime(t *testing.T) {
	_, err := NewSampleSegment(
		labels.FromStrings(labels.MetricName, "invalid_segment"),
		[]int64{100, 100}, []int64{0, 0}, []SampleKind{SampleFloat, SampleFloat},
		[]float64{1, 2}, [][]byte{nil, nil},
	)
	require.Error(t, err)
}

func TestSegmentBlockRoundTripAndBoundaries(t *testing.T) {
	const sampleCount = DefaultSamplesPerSegment*2 + 88
	timestamps := make([]int64, sampleCount)
	values := make([]float64, sampleCount)
	for i := range sampleCount {
		timestamps[i] = int64(i) * 15_000
		values[i] = float64(i) * 1.37
	}
	series := NewFloatSeries(labels.FromStrings(labels.MetricName, "segment_boundaries"), timestamps, values)
	dir := t.TempDir()
	meta, err := WriteBlock(dir, []Series{series}, WriteOptions{})
	require.NoError(t, err)
	block, err := OpenBlock(filepath.Join(dir, meta.ULID))
	require.NoError(t, err)
	reader, err := block.recordReader(t.Context(), nil, nil)
	require.NoError(t, err)
	defer reader.Release()
	require.True(t, reader.Next())
	record := reader.RecordBatch()
	require.Equal(t, int64(3), record.NumRows(), "one cohort/time-window page per segment")
	payloads := record.Column(pageSeriesPayloadsColumn).(*array.List)
	for row := range int(record.NumRows()) {
		start, end := payloads.ValueOffsets(row)
		require.Equal(t, int64(1), end-start, "one stable series payload offset per page")
	}
	decoded, err := block.Series(t.Context())
	require.NoError(t, err)
	require.Len(t, decoded, 1)
	require.Equal(t, timestamps, decoded[0].Timestamps)
	require.Equal(t, values, decoded[0].Values)
}

func TestSegmentBlockUsesPhysicalReader(t *testing.T) {
	dir := t.TempDir()
	timestamps := []int64{100, 200, 300}
	values := []float64{1.5, 2.5, 3.5}
	meta, err := WriteBlock(dir, []Series{NewFloatSeries(labels.FromStrings(labels.MetricName, "segments_in_block"), timestamps, values)}, WriteOptions{})
	require.NoError(t, err)
	block, err := OpenBlock(filepath.Join(dir, meta.ULID))
	require.NoError(t, err)
	series, err := block.Series(t.Context())
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, timestamps, series[0].Timestamps)
	require.Equal(t, values, series[0].Values)
	require.Equal(t, 3, series[0].Len())
}

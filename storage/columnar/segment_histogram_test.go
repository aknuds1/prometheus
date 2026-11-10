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
	"math"
	"path/filepath"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/stretchr/testify/require"

	modelhistogram "github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
)

func typedHistogramPayload(tb testing.TB, hp prompb.Histogram) []byte {
	tb.Helper()
	payload, err := hp.Marshal()
	require.NoError(tb, err)
	return payload
}

func TestTypedHistogramSegmentExactRoundTrips(t *testing.T) {
	integer := prompb.Histogram{
		Count: &prompb.Histogram_CountInt{CountInt: 42}, Sum: 12.5, Schema: 1,
		ZeroThreshold: 0.001, ZeroCount: &prompb.Histogram_ZeroCountInt{ZeroCountInt: 3},
		NegativeSpans: []prompb.BucketSpan{{Offset: -1, Length: 2}}, NegativeDeltas: []int64{2, -1},
		PositiveSpans: []prompb.BucketSpan{{Offset: 0, Length: 3}}, PositiveDeltas: []int64{4, -1, 2},
		ResetHint: prompb.Histogram_YES,
	}
	floating := prompb.Histogram{
		Count: &prompb.Histogram_CountFloat{CountFloat: 7.25}, Sum: math.Float64frombits(0x7ff8000000000001),
		Schema: -2, ZeroThreshold: math.Copysign(0, -1), ZeroCount: &prompb.Histogram_ZeroCountFloat{ZeroCountFloat: 0.5},
		NegativeSpans: []prompb.BucketSpan{{Offset: -1, Length: 2}}, NegativeCounts: []float64{1.5, 2.5},
		PositiveSpans: []prompb.BucketSpan{{Offset: 0, Length: 3}}, PositiveCounts: []float64{3.5, 4.5, 5.5},
		ResetHint: prompb.Histogram_GAUGE,
	}
	irregular := integer
	irregular.PositiveSpans = []prompb.BucketSpan{{Offset: 2, Length: 1}, {Offset: -5, Length: 2}}
	irregular.PositiveDeltas = []int64{-2, 1, 0}
	irregular.Count = &prompb.Histogram_CountInt{CountInt: math.MaxUint64}
	irregular.ZeroCount = &prompb.Histogram_ZeroCountInt{ZeroCountInt: 1<<53 + 1}
	custom := prompb.Histogram{
		Count: &prompb.Histogram_CountFloat{CountFloat: 4}, Schema: modelhistogram.CustomBucketsSchema,
		ZeroCount: &prompb.Histogram_ZeroCountFloat{}, PositiveSpans: []prompb.BucketSpan{{Length: 3}},
		PositiveCounts: []float64{1, 2, 3}, CustomValues: []float64{0, 1.5, 10}, ResetHint: prompb.Histogram_NO,
	}

	for _, tc := range []struct {
		name             string
		histograms       []prompb.Histogram
		stable           bool
		intCountsAsFloat bool
	}{
		{name: "stable integer", histograms: []prompb.Histogram{integer, integer, integer}, stable: true, intCountsAsFloat: true},
		{name: "stable mixed kinds", histograms: []prompb.Histogram{integer, floating, integer}, stable: true, intCountsAsFloat: true},
		{name: "mixed kinds and irregular buckets", histograms: []prompb.Histogram{integer, floating, irregular, custom}, intCountsAsFloat: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := len(tc.histograms) + 1
			timestamps := make([]int64, n)
			kinds := make([]SampleKind, n) // Leading float exercises histogram indexing.
			values := make([]float64, n)
			payloads := make([][]byte, n)
			expected := make([][]byte, n)
			for i, hp := range tc.histograms {
				index := i + 1
				timestamps[index] = int64(index * 1000)
				if hp.IsFloatHistogram() {
					kinds[index] = SampleFloatHistogram
				} else {
					kinds[index] = SampleHistogram
				}
				expected[index] = typedHistogramPayload(t, hp)
				payloads[index] = expected[index]
			}
			ls := labels.FromStrings(labels.MetricName, "typed_histogram")
			segment, err := NewSampleSegment(ls, timestamps, nil, kinds, values, payloads)
			require.NoError(t, err)
			require.Equal(t, tc.stable, segment.histograms.positiveLayoutStable)
			require.Equal(t, tc.intCountsAsFloat, segment.histograms.intCountsAsFloat)
			if tc.stable {
				require.NotEqual(t, integerVarint, segment.histograms.positiveIntBucketCodec)
			} else {
				require.Equal(t, integerVarint, segment.histograms.positiveIntBucketCodec)
				require.NotEmpty(t, segment.histograms.negativeBucketCounts)
			}

			page, err := segment.Decode(ls)
			require.NoError(t, err)
			require.Equal(t, expected, page.Histograms)

			item := Series{Labels: ls, Timestamps: timestamps, Kinds: kinds, Values: values, Histograms: expected}
			dir := t.TempDir()
			meta, err := WriteBlock(dir, []Series{item}, WriteOptions{})
			require.NoError(t, err)
			block, err := OpenBlock(filepath.Join(dir, meta.ULID))
			require.NoError(t, err)
			got, err := block.Series(t.Context())
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, expected, got[0].Histograms)

			record, err := buildRecord(memory.NewGoAllocator(), []Series{item})
			require.NoError(t, err)
			decoded, err := DecodeRecord(record)
			record.Release()
			require.NoError(t, err)
			require.Len(t, decoded, 1)
			require.Equal(t, expected, decoded[0].Histograms)
		})
	}
}

func TestHistogramGaugeToCounterTransitionHints(t *testing.T) {
	s, err := NewStorage(t.TempDir(), StorageOptions{FlushSamples: 100, DisableNativeMetadata: true, DisableWAL: true})
	require.NoError(t, err)
	defer s.Close()
	ls := labels.FromStrings(labels.MetricName, "gauge_transition")
	app := s.Appender(t.Context())
	_, err = app.AppendHistogram(0, ls, 100, &modelhistogram.Histogram{Count: 4, Sum: 4, CounterResetHint: modelhistogram.GaugeType}, nil)
	require.NoError(t, err)
	_, err = app.AppendHistogram(0, ls, 200, &modelhistogram.Histogram{Count: 5, Sum: 5}, nil)
	require.NoError(t, err)
	require.NoError(t, app.Commit())
	series, err := s.SelectSeries(t.Context(), 0, 200, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	first, _, err := series[0].HistogramAt(0)
	require.NoError(t, err)
	second, _, err := series[0].HistogramAt(1)
	require.NoError(t, err)
	require.Equal(t, modelhistogram.GaugeType, first.CounterResetHint)
	require.Equal(t, modelhistogram.UnknownCounterReset, second.CounterResetHint)
}

func TestTypedHistogramRejectsNegativeBucketCount(t *testing.T) {
	hp := prompb.Histogram{Count: &prompb.Histogram_CountInt{CountInt: 1}, PositiveSpans: []prompb.BucketSpan{{Length: 1}}, PositiveDeltas: []int64{1}}
	payload := typedHistogramPayload(t, hp)
	kinds := []SampleKind{SampleHistogram}
	segment, err := NewSampleSegment(labels.FromStrings(labels.MetricName, "invalid_histogram"), []int64{1}, nil, kinds, []float64{0}, [][]byte{payload})
	require.NoError(t, err)
	segment.histograms.positiveLayoutStable = false
	segment.histograms.positiveBucketCounts = encodeIntPlain([]int64{-1})
	segment.histograms.positiveBucketCountCodec = integerPlain
	_, err = segment.histograms.decode(kinds)
	require.Error(t, err)
}

func TestTypedHistogramRejectsMismatchedKind(t *testing.T) {
	payload := typedHistogramPayload(t, prompb.Histogram{Count: &prompb.Histogram_CountInt{CountInt: 1}})
	_, err := NewSampleSegment(labels.FromStrings(labels.MetricName, "bad_histogram"), []int64{1}, nil,
		[]SampleKind{SampleFloatHistogram}, []float64{0}, [][]byte{payload})
	require.ErrorContains(t, err, "disagrees with count representation")
}

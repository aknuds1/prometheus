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
	"testing"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
)

var histogramBenchmarkCount uint64

// BenchmarkHistogramSegmentCodecsStageG1 compares typed columns with the former
// opaque protobuf payloads while materializing every histogram. This measures
// histogram work separately from the float/XOR2 microbenchmark.
func BenchmarkHistogramSegmentCodecsStageG1(b *testing.B) {
	const sampleCount = 100
	timestamps := make([]int64, sampleCount)
	kinds := make([]SampleKind, sampleCount)
	values := make([]float64, sampleCount)
	payloads := make([][]byte, sampleCount)
	opaqueBytes := 0
	for i := range sampleCount {
		timestamps[i] = int64(i * 1000)
		kinds[i] = SampleHistogram
		hp := prompb.Histogram{
			Count: &prompb.Histogram_CountInt{CountInt: uint64(i + 100)}, Sum: float64(i),
			Schema: 1, ZeroThreshold: 0.001, ZeroCount: &prompb.Histogram_ZeroCountInt{ZeroCountInt: 1},
			PositiveSpans:  []prompb.BucketSpan{{Offset: 0, Length: 3}},
			PositiveDeltas: []int64{int64(i), -1, 2}, ResetHint: prompb.Histogram_NO,
		}
		payloads[i] = typedHistogramPayload(b, hp)
		opaqueBytes += len(payloads[i])
	}
	ls := labels.FromStrings(labels.MetricName, "histogram_codec")
	segment, err := NewSampleSegment(ls, timestamps, nil, kinds, values, payloads)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("typed encoded column bytes=%d, opaque protobuf bytes=%d", segment.EncodedSize(), opaqueBytes)

	for _, mode := range []string{"typed", "opaque"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(sampleCount, "samples/op")
			b.ResetTimer()
			for range b.N {
				var total uint64
				if mode == "typed" {
					page, err := segment.Decode(ls)
					if err != nil {
						b.Fatal(err)
					}
					series := NewTypedPageSeries(page)
					for i := range series.Len() {
						h, _, err := series.HistogramAt(i)
						if err != nil {
							b.Fatal(err)
						}
						total += h.Count
					}
				} else {
					for _, payload := range payloads {
						h, _, err := decodeSampleHistogram(payload)
						if err != nil {
							b.Fatal(err)
						}
						total += h.Count
					}
				}
				histogramBenchmarkCount = total
			}
		})
	}
}

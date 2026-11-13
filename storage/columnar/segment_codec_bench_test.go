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
	"testing"
)

// timestampBenchCases are the timestamp shapes the shared format-10 column codec
// has to choose between. Timestamps only, so an encoded size difference is the
// codec choice, not a payload effect.
var timestampBenchCases = []struct {
	name   string
	values []int64
}{
	{name: "constant_interval", values: func() []int64 {
		values := make([]int64, 60)
		for i := range values {
			values[i] = 1_700_000_000_000 + int64(i)*15_000
		}
		return values
	}()},
	{name: "missed_scrape", values: missedScrapeTimestamps()},
	{name: "jittered", values: jitteredTimestamps()},
	{name: "extreme", values: []int64{math.MinInt64, 0, math.MaxInt64, math.MaxInt64 - 1, 0, 1}},
}

// BenchmarkSegmentTimestampEncode isolates the codec choice: it reports the
// encoded bytes per sample alongside the time, because a column that silently
// falls back to the plain representation is cheap to encode and eight bytes wide.
func BenchmarkSegmentTimestampEncode(b *testing.B) {
	for _, tc := range timestampBenchCases {
		b.Run(tc.name, func(b *testing.B) {
			encoded, codec, err := encodeTimestamps(tc.values)
			if err != nil {
				b.Fatalf("encode: %v", err)
			}
			decoded, err := decodeIntegerColumn(encoded, len(tc.values), codec)
			if err != nil {
				b.Fatalf("decode: %v", err)
			}
			for i := range tc.values {
				if decoded[i] != tc.values[i] {
					b.Fatalf("sample %d round trip: got %d, want %d", i, decoded[i], tc.values[i])
				}
			}

			b.ReportMetric(float64(len(encoded))/float64(len(tc.values)), "encoded-B/sample")
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, _, err := encodeTimestamps(tc.values); err != nil {
					b.Fatalf("encode: %v", err)
				}
			}
			b.ReportMetric(float64(len(encoded)), "encoded-B/op")
		})
	}
}

// BenchmarkSegmentTimestampDecode consumes a payload produced by
// BenchmarkSegmentTimestampEncode's fixture, so the pair separates the cost of
// choosing a narrow column from the cost of reading it back.
func BenchmarkSegmentTimestampDecode(b *testing.B) {
	for _, tc := range timestampBenchCases {
		b.Run(tc.name, func(b *testing.B) {
			encoded, codec, err := encodeTimestamps(tc.values)
			if err != nil {
				b.Fatalf("encode: %v", err)
			}
			destination := make([]int64, len(tc.values))
			b.ReportMetric(float64(len(encoded))/float64(len(tc.values)), "encoded-B/sample")
			b.ReportAllocs()
			b.ResetTimer()
			var sum int64
			for range b.N {
				decoded, err := decodeIntegerColumnInto(encoded, len(tc.values), codec, destination)
				if err != nil {
					b.Fatalf("decode: %v", err)
				}
				sum += decoded[len(decoded)-1]
			}
			timestampDecodeSink = sum
			b.ReportMetric(float64(len(encoded)), "encoded-B/op")
		})
	}
}

// timestampDecodeSink keeps the decoded values live so the compiler cannot drop
// the decode loop.
var timestampDecodeSink int64

// TestSegmentTimestampBenchFixturesRoundTrip guards the benchmark fixtures: an
// encode/decode comparison is only meaningful if both arms carry the same samples.
func TestSegmentTimestampBenchFixturesRoundTrip(t *testing.T) {
	for _, tc := range timestampBenchCases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, codec, err := encodeTimestamps(tc.values)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			decoded, err := decodeIntegerColumn(encoded, len(tc.values), codec)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			for i := range tc.values {
				if decoded[i] != tc.values[i] {
					t.Fatalf("sample %d round trip: got %d, want %d", i, decoded[i], tc.values[i])
				}
			}
		})
	}
}

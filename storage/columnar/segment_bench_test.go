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

package columnar_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

// decodedValueSum prevents benchmark-only dead-result paths. The writer is set
// by each decoder arm after the comparison; tests and production code never share it.
var decodedValueSum float64

// BenchmarkSegmentCodecsStageG1 compares encoding and whole-page decoding with
// the equivalent XOR2 chunk, including timestamps and exact values. Both decoders
// consume every value in original series order.
func BenchmarkSegmentCodecsStageG1(b *testing.B) {
	timestamps := make([]int64, 100)
	values := make([]float64, 100)
	for i := range timestamps {
		timestamps[i] = int64(i) * 15_000
		values[i] = float64(i) * 1.37
	}
	ls := labels.FromStrings(labels.MetricName, "codec_benchmark")
	segment, err := columnar.NewSampleSegment(ls, timestamps, make([]int64, len(timestamps)), make([]columnar.SampleKind, len(timestamps)), values, make([][]byte, len(timestamps)))
	require.NoError(b, err)
	control := newXOR2Control(timestamps, values)

	b.Run("columnar-encode", func(b *testing.B) {
		b.ReportMetric(float64(len(values)), "samples/op")
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			result, err := columnar.NewSampleSegment(ls, timestamps, make([]int64, len(timestamps)), make([]columnar.SampleKind, len(timestamps)), values, make([][]byte, len(timestamps)))
			require.NoError(b, err)
			require.Equal(b, len(values), result.Count)
		}
	})
	b.Run("xor2-encode", func(b *testing.B) {
		b.ReportMetric(float64(len(values)), "samples/op")
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			_ = newXOR2Control(timestamps, values)
		}
	})
	b.Run("columnar-decode", func(b *testing.B) {
		b.ReportMetric(float64(len(values)), "samples/op")
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			iterator := segment.NewPageIterator(ls)
			if !iterator.Next() {
				b.Fatal(iterator.Err())
			}
			page := iterator.Page()
			if page.Len() != len(values) {
				b.Fatalf("decoded %d samples, want %d", page.Len(), len(values))
			}
			sum := 0.0
			for _, value := range page.Values {
				sum += value
			}
			decodedValueSum = sum
			if err := iterator.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("xor2-decode", func(b *testing.B) {
		b.ReportMetric(float64(len(values)), "samples/op")
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			iterator := control.Iterator(nil)
			count := 0
			sum := 0.0
			for iterator.Next() == chunkenc.ValFloat {
				_, value := iterator.At()
				sum += value
				count++
			}
			if err := iterator.Err(); err != nil {
				b.Fatal(err)
			}
			if count != len(values) {
				b.Fatalf("decoded %d samples, want %d", count, len(values))
			}
			decodedValueSum = sum
		}
	})
}

func TestSegmentCodecBytesAgainstXOR2(t *testing.T) {
	const seriesCount = 500
	allLabels, timestamps, values := syntheticSeries(seriesCount, 100)
	rows := make([]columnar.Series, 0, seriesCount)
	var payloadBytes, xor2Bytes int
	for i := range seriesCount {
		rows = append(rows, columnar.NewFloatSeries(allLabels[i], timestamps[i], values[i]))
		for start := 0; start < len(timestamps[i]); start += columnar.DefaultSamplesPerSegment {
			end := min(start+columnar.DefaultSamplesPerSegment, len(timestamps[i]))
			segment, err := columnar.NewSampleSegment(
				allLabels[i], timestamps[i][start:end], rows[i].StartTimestamps[start:end],
				rows[i].Kinds[start:end], values[i][start:end], rows[i].Histograms[start:end],
			)
			require.NoError(t, err)
			payloadBytes += segment.EncodedSize()
		}
		for controlSegStart := 0; controlSegStart < len(timestamps[i]); controlSegStart += columnar.DefaultSamplesPerSegment {
			end := min(controlSegStart+columnar.DefaultSamplesPerSegment, len(timestamps[i]))
			control := newXOR2Control(timestamps[i][controlSegStart:end], values[i][controlSegStart:end])
			xor2Bytes += len(control.Bytes())
		}
	}

	dir := t.TempDir()
	meta, err := columnar.WriteBlock(dir, rows, columnar.WriteOptions{})
	require.NoError(t, err)
	info, err := os.Stat(filepath.Join(dir, meta.ULID, "data.parquet"))
	require.NoError(t, err)
	sampleCount := seriesCount * len(timestamps[0])
	payloadRatio := float64(payloadBytes) / float64(xor2Bytes)
	blockRatio := float64(info.Size()) / float64(xor2Bytes)
	t.Logf("payload: %.3f B/sample, XOR2: %.3f B/sample, ratio=%.3f",
		float64(payloadBytes)/float64(sampleCount), float64(xor2Bytes)/float64(sampleCount), payloadRatio)
	t.Logf("Parquet data file: %.3f B/sample, XOR2 ratio=%.3f",
		float64(info.Size())/float64(sampleCount), blockRatio)
	// Payload ratios explain codec choice; the format gate is about the complete
	// Parquet block with its labels, segment headers and projections included.
	if blockRatio > 0.8 {
		t.Errorf("Parquet block ratio %.3f exceeds the 0.8x XOR2 gate", blockRatio)
	}
}

func newXOR2Control(timestamps []int64, values []float64) *chunkenc.XOR2Chunk {
	chunk := chunkenc.NewXOR2Chunk()
	appender, err := chunk.Appender()
	if err != nil {
		panic(err)
	}
	for i := range timestamps {
		appender.Append(0, int64(i), values[i])
	}
	return chunk
}

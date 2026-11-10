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
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/prometheus/prometheus/prompb"
)

// typedHistogramSegment stores only histogram samples, addressed by their kind
// positions in the enclosing segment. Counter reset hints, schemas and sizes are
// their own columns; no per-sample Prometheus protobuf payload remains here.
type typedHistogramSegment struct {
	count int

	intCounts        []byte
	intCountCodec    integerCodec
	floatCounts      []byte
	floatCountsCodec floatCodec
	intCountsAsFloat bool

	sums     []byte
	sumCodec floatCodec

	schemas     []byte
	schemaCodec integerCodec

	zeroThresholds       []byte
	zeroThresholdCodec   floatCodec
	intZeroCounts        []byte
	intZeroCountCodec    integerCodec
	floatZeroCounts      []byte
	floatZeroCountCodec  floatCodec
	intZeroCountsAsFloat bool

	resetHints     []byte
	resetHintCodec integerCodec

	positiveSpans            []byte
	positiveLayoutStable     bool
	negativeSpans            []byte
	negativeLayoutStable     bool
	positiveIntBuckets       []byte
	positiveIntBucketCodec   integerCodec
	negativeIntBuckets       []byte
	negativeIntBucketCodec   integerCodec
	positiveFloatBuckets     []byte
	positiveFloatBucketCodec floatCodec
	negativeFloatBuckets     []byte
	negativeFloatBucketCodec floatCodec
	positiveBucketCounts     []byte
	positiveBucketCountCodec integerCodec
	negativeBucketCounts     []byte
	negativeBucketCountCodec integerCodec

	customValues     []byte
	customValueCodec floatCodec
	customCounts     []byte
	customCountCodec integerCodec
}

type histogramEncodingState struct {
	count                int
	intCounts            []int64
	floatCounts          []float64
	intCountsAsFloat     bool
	sums                 []float64
	schemas              []int64
	zeroThresholds       []float64
	intZeroCounts        []int64
	floatZeroCounts      []float64
	intZeroCountsAsFloat bool
	resetHints           []int64
	positiveSpans        []prompb.BucketSpan
	negativeSpans        []prompb.BucketSpan
	positiveIntBuckets   []int64
	negativeIntBuckets   []int64
	positiveFloatBuckets []float64
	negativeFloatBuckets []float64
	positiveBucketCounts []int64
	negativeBucketCounts []int64
	customValues         []float64
	customCounts         []int64
	positiveSpanCounts   []int64
	negativeSpanCounts   []int64
	positiveLayoutStable bool
	negativeLayoutStable bool
}

func encodeTypedHistograms(kinds []SampleKind, payloads [][]byte) (typedHistogramSegment, error) {
	if countHistogramKinds(kinds) == 0 {
		return typedHistogramSegment{}, nil
	}
	state := histogramEncodingState{intCountsAsFloat: true, intZeroCountsAsFloat: true, positiveLayoutStable: true, negativeLayoutStable: true}
	first := true
	var previousPositiveSpans, previousNegativeSpans []prompb.BucketSpan
	var previousPositiveBuckets, previousNegativeBuckets int
	for i, kind := range kinds {
		if kind != SampleHistogram && kind != SampleFloatHistogram {
			continue
		}
		if len(payloads) <= i || len(payloads[i]) == 0 {
			return typedHistogramSegment{}, fmt.Errorf("histogram at sample %d has no payload", i)
		}
		var hp prompb.Histogram
		if err := hp.Unmarshal(payloads[i]); err != nil {
			return typedHistogramSegment{}, fmt.Errorf("decoding histogram at sample %d: %w", i, err)
		}
		if hp.IsFloatHistogram() != (kind == SampleFloatHistogram) {
			return typedHistogramSegment{}, fmt.Errorf("histogram kind %d disagrees with count representation at sample %d", kind, i)
		}
		state.resetHints = append(state.resetHints, int64(hp.ResetHint))
		state.schemas = append(state.schemas, int64(hp.Schema))
		state.zeroThresholds = append(state.zeroThresholds, hp.ZeroThreshold)
		state.sums = append(state.sums, hp.Sum)
		if kind == SampleHistogram {
			count := hp.GetCountInt()
			zeroCount := hp.GetZeroCountInt()
			state.intCounts = append(state.intCounts, int64(count))
			state.intZeroCounts = append(state.intZeroCounts, int64(zeroCount))
			countFloat, zeroFloat := float64(count), float64(zeroCount)
			state.floatCounts = append(state.floatCounts, countFloat)
			state.floatZeroCounts = append(state.floatZeroCounts, zeroFloat)
			state.intCountsAsFloat = state.intCountsAsFloat && uint64(countFloat) == count
			state.intZeroCountsAsFloat = state.intZeroCountsAsFloat && uint64(zeroFloat) == zeroCount
			state.positiveIntBuckets = append(state.positiveIntBuckets, hp.PositiveDeltas...)
			state.negativeIntBuckets = append(state.negativeIntBuckets, hp.NegativeDeltas...)
			state.positiveBucketCounts = append(state.positiveBucketCounts, int64(len(hp.PositiveDeltas)))
			state.negativeBucketCounts = append(state.negativeBucketCounts, int64(len(hp.NegativeDeltas)))
		} else {
			state.intCounts = append(state.intCounts, 0)
			state.intZeroCounts = append(state.intZeroCounts, 0)
			state.floatCounts = append(state.floatCounts, hp.GetCountFloat())
			state.floatZeroCounts = append(state.floatZeroCounts, hp.GetZeroCountFloat())
			state.positiveFloatBuckets = append(state.positiveFloatBuckets, hp.PositiveCounts...)
			state.negativeFloatBuckets = append(state.negativeFloatBuckets, hp.NegativeCounts...)
			state.positiveBucketCounts = append(state.positiveBucketCounts, int64(len(hp.PositiveCounts)))
			state.negativeBucketCounts = append(state.negativeBucketCounts, int64(len(hp.NegativeCounts)))
		}
		state.positiveSpanCounts = append(state.positiveSpanCounts, int64(len(hp.PositiveSpans)))
		state.negativeSpanCounts = append(state.negativeSpanCounts, int64(len(hp.NegativeSpans)))
		state.positiveSpans = append(state.positiveSpans, hp.PositiveSpans...)
		state.negativeSpans = append(state.negativeSpans, hp.NegativeSpans...)
		state.customValues = append(state.customValues, hp.CustomValues...)
		state.customCounts = append(state.customCounts, int64(len(hp.CustomValues)))
		state.count++
		if first {
			previousPositiveSpans, previousNegativeSpans = hp.PositiveSpans, hp.NegativeSpans
			previousPositiveBuckets = int(state.positiveBucketCounts[len(state.positiveBucketCounts)-1])
			previousNegativeBuckets = int(state.negativeBucketCounts[len(state.negativeBucketCounts)-1])
			first = false
			continue
		}
		state.positiveLayoutStable = state.positiveLayoutStable && sameBucketLayout(previousPositiveSpans, hp.PositiveSpans, previousPositiveBuckets, state.positiveBucketCounts)
		state.negativeLayoutStable = state.negativeLayoutStable && sameBucketLayout(previousNegativeSpans, hp.NegativeSpans, previousNegativeBuckets, state.negativeBucketCounts)
	}
	return state.finish()
}

func sameBucketLayout(previous, current []prompb.BucketSpan, previousBuckets int, currentCounts []int64) bool {
	return previousBuckets == int(currentCounts[len(currentCounts)-1]) && slices.EqualFunc(previous, current, func(a, b prompb.BucketSpan) bool {
		return a.Offset == b.Offset && a.Length == b.Length
	})
}

func (s histogramEncodingState) finish() (typedHistogramSegment, error) {
	out := typedHistogramSegment{
		count: s.count, intCountsAsFloat: s.intCountsAsFloat, intZeroCountsAsFloat: s.intZeroCountsAsFloat,
		positiveLayoutStable: s.positiveLayoutStable, negativeLayoutStable: s.negativeLayoutStable,
	}
	var err error
	encodeInt := func(destination *[]byte, codec *integerCodec, values []int64) bool {
		*destination, *codec, err = encodeIntegerColumn(values)
		return err == nil
	}
	encodeFloat := func(destination *[]byte, codec *floatCodec, values []float64) bool {
		*destination, *codec, err = encodeFloatColumn(values)
		return err == nil
	}
	if !s.intCountsAsFloat && !encodeInt(&out.intCounts, &out.intCountCodec, s.intCounts) {
		return typedHistogramSegment{}, err
	}
	if !s.intZeroCountsAsFloat && !encodeInt(&out.intZeroCounts, &out.intZeroCountCodec, s.intZeroCounts) {
		return typedHistogramSegment{}, err
	}
	if !encodeFloat(&out.floatCounts, &out.floatCountsCodec, s.floatCounts) ||
		!encodeFloat(&out.sums, &out.sumCodec, s.sums) ||
		!encodeInt(&out.schemas, &out.schemaCodec, s.schemas) {
		return typedHistogramSegment{}, err
	}
	out.zeroThresholds, out.zeroThresholdCodec, err = encodeFloatRLEColumn(s.zeroThresholds)
	if err != nil {
		return typedHistogramSegment{}, err
	}
	if !encodeFloat(&out.floatZeroCounts, &out.floatZeroCountCodec, s.floatZeroCounts) ||
		!encodeInt(&out.resetHints, &out.resetHintCodec, s.resetHints) {
		return typedHistogramSegment{}, err
	}

	out.positiveSpans = encodeSpans(s.positiveSpans, s.positiveSpanCounts, s.positiveLayoutStable)
	out.negativeSpans = encodeSpans(s.negativeSpans, s.negativeSpanCounts, s.negativeLayoutStable)
	out.positiveIntBuckets, out.positiveIntBucketCodec = encodeBuckets(s.positiveIntBuckets, s.positiveLayoutStable)
	out.negativeIntBuckets, out.negativeIntBucketCodec = encodeBuckets(s.negativeIntBuckets, s.negativeLayoutStable)
	out.positiveFloatBuckets, out.positiveFloatBucketCodec, err = encodeFloatColumn(s.positiveFloatBuckets)
	if err != nil {
		return typedHistogramSegment{}, err
	}
	out.negativeFloatBuckets, out.negativeFloatBucketCodec, err = encodeFloatColumn(s.negativeFloatBuckets)
	if err != nil {
		return typedHistogramSegment{}, err
	}
	if !s.positiveLayoutStable {
		if !encodeInt(&out.positiveBucketCounts, &out.positiveBucketCountCodec, s.positiveBucketCounts) {
			return typedHistogramSegment{}, err
		}
	}
	if !s.negativeLayoutStable {
		if !encodeInt(&out.negativeBucketCounts, &out.negativeBucketCountCodec, s.negativeBucketCounts) {
			return typedHistogramSegment{}, err
		}
	}
	out.customValues, out.customValueCodec, err = encodeFloatColumn(s.customValues)
	if err != nil {
		return typedHistogramSegment{}, err
	}
	if !encodeInt(&out.customCounts, &out.customCountCodec, s.customCounts) {
		return typedHistogramSegment{}, err
	}
	return out, nil
}

func encodeBuckets(values []int64, stable bool) ([]byte, integerCodec) {
	if stable {
		encoded, codec, _ := encodeIntegerColumn(values)
		return encoded, codec
	}
	var out []byte
	for _, value := range values {
		out = binary.AppendVarint(out, value)
	}
	return out, integerVarint
}

func encodeSpans(spans []prompb.BucketSpan, counts []int64, stable bool) []byte {
	var out []byte
	if stable {
		// A stable layout stores the shared span vector once, not once per
		// histogram. Counts are omitted because bucket lengths derive from spans.
		if len(counts) > 0 {
			spans = spans[:counts[0]]
		} else {
			spans = nil
		}
	} else {
		for _, count := range counts {
			out = binary.AppendVarint(out, count)
		}
	}
	for _, span := range spans {
		out = binary.AppendVarint(out, int64(span.Offset))
		out = binary.AppendVarint(out, int64(span.Length))
	}
	return out
}

func countHistogramKinds(kinds []SampleKind) int {
	count := 0
	for _, kind := range kinds {
		if kind == SampleHistogram || kind == SampleFloatHistogram {
			count++
		}
	}
	return count
}

func (h typedHistogramSegment) encodedSize() int {
	total := 0
	for _, column := range [][]byte{
		h.intCounts, h.floatCounts, h.sums, h.schemas, h.zeroThresholds, h.intZeroCounts,
		h.floatZeroCounts, h.resetHints, h.positiveSpans, h.negativeSpans, h.positiveIntBuckets,
		h.negativeIntBuckets, h.positiveFloatBuckets, h.negativeFloatBuckets, h.positiveBucketCounts,
		h.negativeBucketCounts, h.customValues, h.customCounts,
	} {
		total += len(column)
	}
	return total
}

func (h typedHistogramSegment) decode(kinds []SampleKind) ([][]byte, error) {
	if h.count == 0 {
		return nil, nil
	}
	var intCounts []int64
	if !h.intCountsAsFloat {
		var err error
		intCounts, err = decodeIntegerColumn(h.intCounts, h.count, h.intCountCodec)
		if err != nil {
			return nil, fmt.Errorf("decoding histogram counts: %w", err)
		}
	}
	floatCounts, err := decodeFloatColumn(h.floatCounts, h.count, h.floatCountsCodec)
	if err != nil {
		return nil, fmt.Errorf("decoding histogram float counts: %w", err)
	}
	sums, err := decodeFloatColumn(h.sums, h.count, h.sumCodec)
	if err != nil {
		return nil, fmt.Errorf("decoding histogram sums: %w", err)
	}
	schemas, err := decodeIntegerColumn(h.schemas, h.count, h.schemaCodec)
	if err != nil {
		return nil, fmt.Errorf("decoding histogram schemas: %w", err)
	}
	zeroThresholds, err := decodeFloatColumn(h.zeroThresholds, h.count, h.zeroThresholdCodec)
	if err != nil {
		return nil, fmt.Errorf("decoding histogram zero thresholds: %w", err)
	}
	var intZeroCounts []int64
	if !h.intZeroCountsAsFloat {
		var err error
		intZeroCounts, err = decodeIntegerColumn(h.intZeroCounts, h.count, h.intZeroCountCodec)
		if err != nil {
			return nil, fmt.Errorf("decoding histogram zero counts: %w", err)
		}
	}
	floatZeroCounts, err := decodeFloatColumn(h.floatZeroCounts, h.count, h.floatZeroCountCodec)
	if err != nil {
		return nil, fmt.Errorf("decoding histogram float zero counts: %w", err)
	}
	resetHints, err := decodeIntegerColumn(h.resetHints, h.count, h.resetHintCodec)
	if err != nil {
		return nil, fmt.Errorf("decoding histogram reset hints: %w", err)
	}
	positiveSpans, err := decodeSpans(h.positiveSpans, h.count, h.positiveLayoutStable)
	if err != nil {
		return nil, fmt.Errorf("decoding positive histogram spans: %w", err)
	}
	negativeSpans, err := decodeSpans(h.negativeSpans, h.count, h.negativeLayoutStable)
	if err != nil {
		return nil, fmt.Errorf("decoding negative histogram spans: %w", err)
	}
	positiveCounts, err := histogramBucketCounts(h.positiveBucketCounts, h.positiveBucketCountCodec, h.positiveLayoutStable, h.count, positiveSpans)
	if err != nil {
		return nil, err
	}
	negativeCounts, err := histogramBucketCounts(h.negativeBucketCounts, h.negativeBucketCountCodec, h.negativeLayoutStable, h.count, negativeSpans)
	if err != nil {
		return nil, err
	}
	positiveIntBucketCount := sumBucketsOfKind(positiveCounts, kinds, SampleHistogram)
	positiveIntBuckets, err := decodeIntegerColumn(h.positiveIntBuckets, positiveIntBucketCount, h.positiveIntBucketCodec)
	if err != nil {
		return nil, fmt.Errorf("decoding %d positive integer histogram buckets: %w", positiveIntBucketCount, err)
	}
	negativeIntBuckets, err := decodeIntegerColumn(h.negativeIntBuckets, sumBucketsOfKind(negativeCounts, kinds, SampleHistogram), h.negativeIntBucketCodec)
	if err != nil {
		return nil, fmt.Errorf("decoding negative integer histogram buckets: %w", err)
	}
	positiveFloatBuckets, err := decodeFloatColumn(h.positiveFloatBuckets, sumBucketsOfKind(positiveCounts, kinds, SampleFloatHistogram), h.positiveFloatBucketCodec)
	if err != nil {
		return nil, fmt.Errorf("decoding positive float histogram buckets: %w", err)
	}
	negativeFloatBuckets, err := decodeFloatColumn(h.negativeFloatBuckets, sumBucketsOfKind(negativeCounts, kinds, SampleFloatHistogram), h.negativeFloatBucketCodec)
	if err != nil {
		return nil, fmt.Errorf("decoding negative float histogram buckets: %w", err)
	}
	customCounts, err := decodeIntegerColumn(h.customCounts, h.count, h.customCountCodec)
	if err != nil {
		return nil, fmt.Errorf("decoding custom-value counts: %w", err)
	}
	customValues, err := decodeFloatColumn(h.customValues, sumInts(customCounts), h.customValueCodec)
	if err != nil {
		return nil, fmt.Errorf("decoding histogram custom values: %w", err)
	}

	out := make([][]byte, len(kinds))
	histogramIndex := 0
	positiveIntIndex, negativeIntIndex := 0, 0
	positiveFloatIndex, negativeFloatIndex := 0, 0
	customIndex := 0
	for sampleIndex, kind := range kinds {
		if kind != SampleHistogram && kind != SampleFloatHistogram {
			continue
		}
		if histogramIndex >= h.count {
			return nil, errors.New("more histogram kinds than typed histogram rows")
		}
		i := histogramIndex
		if resetHints[i] < 0 || resetHints[i] > math.MaxInt32 || schemas[i] < math.MinInt32 || schemas[i] > math.MaxInt32 {
			return nil, errors.New("histogram schema or reset hint overflows int32")
		}
		hp := prompb.Histogram{
			Sum: sums[i], Schema: int32(schemas[i]), ZeroThreshold: zeroThresholds[i],
			ResetHint:     prompb.Histogram_ResetHint(resetHints[i]),
			PositiveSpans: positiveSpans[i], NegativeSpans: negativeSpans[i],
		}
		positiveLength, negativeLength := int(positiveCounts[i]), int(negativeCounts[i])
		if positiveLength < 0 || negativeLength < 0 {
			return nil, errors.New("negative histogram bucket count")
		}
		switch kind {
		case SampleHistogram:
			var count, zeroCount uint64
			if h.intCountsAsFloat {
				var ok bool
				count, ok = exactHistogramUint(floatCounts[i])
				if !ok {
					return nil, errors.New("invalid ALP integer histogram count")
				}
			} else {
				count = uint64(intCounts[i])
			}
			if h.intZeroCountsAsFloat {
				var ok bool
				zeroCount, ok = exactHistogramUint(floatZeroCounts[i])
				if !ok {
					return nil, errors.New("invalid ALP integer histogram zero count")
				}
			} else {
				zeroCount = uint64(intZeroCounts[i])
			}
			hp.Count = &prompb.Histogram_CountInt{CountInt: count}
			hp.ZeroCount = &prompb.Histogram_ZeroCountInt{ZeroCountInt: zeroCount}
			hp.PositiveDeltas = slices.Clone(positiveIntBuckets[positiveIntIndex : positiveIntIndex+positiveLength])
			hp.NegativeDeltas = slices.Clone(negativeIntBuckets[negativeIntIndex : negativeIntIndex+negativeLength])
			positiveIntIndex += positiveLength
			negativeIntIndex += negativeLength
		case SampleFloatHistogram:
			hp.Count = &prompb.Histogram_CountFloat{CountFloat: floatCounts[i]}
			hp.ZeroCount = &prompb.Histogram_ZeroCountFloat{ZeroCountFloat: floatZeroCounts[i]}
			hp.PositiveCounts = slices.Clone(positiveFloatBuckets[positiveFloatIndex : positiveFloatIndex+positiveLength])
			hp.NegativeCounts = slices.Clone(negativeFloatBuckets[negativeFloatIndex : negativeFloatIndex+negativeLength])
			positiveFloatIndex += positiveLength
			negativeFloatIndex += negativeLength
		}
		customLength := customCounts[i]
		if customLength < 0 || customLength > int64(len(customValues)-customIndex) {
			return nil, errors.New("invalid histogram custom-value count")
		}
		hp.CustomValues = slices.Clone(customValues[customIndex : customIndex+int(customLength)])
		customIndex += int(customLength)
		encoded, err := hp.Marshal()
		if err != nil {
			return nil, err
		}
		out[sampleIndex] = encoded
		histogramIndex++
	}
	if histogramIndex != h.count {
		return nil, errors.New("typed histogram rows exceed histogram kinds")
	}
	return out, nil
}

func histogramBucketCounts(payload []byte, codec integerCodec, stable bool, histogramCount int, spans [][]prompb.BucketSpan) ([]int64, error) {
	if !stable {
		return decodeIntegerColumn(payload, histogramCount, codec)
	}
	out := make([]int64, histogramCount)
	for i := range histogramCount {
		for _, span := range spans[i] {
			out[i] += int64(span.Length)
		}
	}
	return out, nil
}

func decodeSpans(payload []byte, histogramCount int, stable bool) ([][]prompb.BucketSpan, error) {
	out := make([][]prompb.BucketSpan, histogramCount)
	position := 0
	readSpans := func() ([]prompb.BucketSpan, int, error) {
		var spans []prompb.BucketSpan
		for position < len(payload) {
			offset, n, err := readSigned(payload[position:])
			if err != nil {
				return nil, 0, err
			}
			position += n
			length, n, err := readSigned(payload[position:])
			if err != nil {
				return nil, 0, err
			}
			position += n
			if offset < math.MinInt32 || offset > math.MaxInt32 || length < 0 || length > math.MaxUint32 {
				return nil, 0, errors.New("histogram span overflows its model type")
			}
			spans = append(spans, prompb.BucketSpan{Offset: int32(offset), Length: uint32(length)})
		}
		return spans, position, nil
	}
	if stable {
		spans, _, err := readSpans()
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i] = slices.Clone(spans)
		}
		return out, nil
	}
	// Non-stable frames carry every span count up front, followed by pairs.
	counts := make([]int64, histogramCount)
	for i := range counts {
		count, n, err := readSigned(payload[position:])
		if err != nil {
			return nil, err
		}
		if count < 0 {
			return nil, errors.New("negative histogram span count")
		}
		position += n
		counts[i] = count
	}
	for i, count := range counts {
		out[i] = make([]prompb.BucketSpan, 0, count)
		for range count {
			offset, n, err := readSigned(payload[position:])
			if err != nil {
				return nil, err
			}
			position += n
			length, n, err := readSigned(payload[position:])
			if err != nil {
				return nil, err
			}
			position += n
			if offset < math.MinInt32 || offset > math.MaxInt32 || length < 0 || length > math.MaxUint32 {
				return nil, errors.New("histogram span overflows its model type")
			}
			out[i] = append(out[i], prompb.BucketSpan{Offset: int32(offset), Length: uint32(length)})
		}
	}
	if position != len(payload) {
		return nil, errors.New("trailing bytes in histogram spans")
	}
	return out, nil
}

func sumInts(values []int64) int {
	total := 0
	for _, value := range values {
		if value < 0 || value > int64(math.MaxInt-total) {
			return -1
		}
		total += int(value)
	}
	return total
}

func sumBucketsOfKind(counts []int64, kinds []SampleKind, wanted SampleKind) int {
	total, histogramIndex := 0, 0
	for _, kind := range kinds {
		if kind != SampleHistogram && kind != SampleFloatHistogram {
			continue
		}
		if histogramIndex >= len(counts) {
			return -1
		}
		if kind == wanted {
			value := counts[histogramIndex]
			if value < 0 || value > int64(math.MaxInt-total) {
				return -1
			}
			total += int(value)
		}
		histogramIndex++
	}
	if histogramIndex != len(counts) {
		return -1
	}
	return total
}

func exactHistogramUint(value float64) (uint64, bool) {
	if value < 0 || value >= math.Exp2(64) || math.Trunc(value) != value {
		return 0, false
	}
	return uint64(value), true
}

const (
	histIntCounts = iota
	histIntCountCodec
	histFloatCounts
	histFloatCountCodec
	histIntCountsAsFloat
	histSums
	histSumCodec
	histSchemas
	histSchemaCodec
	histZeroThresholds
	histZeroThresholdCodec
	histIntZeroCounts
	histIntZeroCountCodec
	histFloatZeroCounts
	histFloatZeroCountCodec
	histIntZeroCountsAsFloat
	histResetHints
	histResetHintCodec
	histPositiveSpans
	histPositiveLayoutStable
	histNegativeSpans
	histNegativeLayoutStable
	histPositiveIntBuckets
	histPositiveIntBucketCodec
	histNegativeIntBuckets
	histNegativeIntBucketCodec
	histPositiveFloatBuckets
	histPositiveFloatBucketCodec
	histNegativeFloatBuckets
	histNegativeFloatBucketCodec
	histPositiveBucketCounts
	histPositiveBucketCountCodec
	histNegativeBucketCounts
	histNegativeBucketCountCodec
	histCustomValues
	histCustomValueCodec
	histCustomCounts
	histCustomCountCodec
	histogramColumnCount
)

func typedHistogramArrowType() arrow.DataType {
	fields := make([]arrow.Field, histogramColumnCount)
	for i := range fields {
		fields[i] = arrow.Field{Type: arrow.BinaryTypes.Binary, Nullable: false}
	}
	names := [...]string{
		"count_int", "count_int_codec", "count_float", "count_float_codec", "count_int_as_float",
		"sum", "sum_codec", "schema", "schema_codec", "zero_threshold", "zero_threshold_codec",
		"zero_count_int", "zero_count_int_codec", "zero_count_float", "zero_count_float_codec", "zero_count_int_as_float",
		"reset_hint", "reset_hint_codec", "positive_spans", "positive_layout_stable",
		"negative_spans", "negative_layout_stable", "positive_buckets_int", "positive_buckets_int_codec",
		"negative_buckets_int", "negative_buckets_int_codec", "positive_buckets_float", "positive_buckets_float_codec",
		"negative_buckets_float", "negative_buckets_float_codec", "positive_bucket_counts", "positive_bucket_counts_codec",
		"negative_bucket_counts", "negative_bucket_counts_codec", "custom_values", "custom_values_codec",
		"custom_value_counts", "custom_value_counts_codec",
	}
	for _, i := range []int{histIntCountsAsFloat, histPositiveLayoutStable, histNegativeLayoutStable, histIntZeroCountsAsFloat} {
		fields[i].Type = arrow.FixedWidthTypes.Boolean
	}
	for _, i := range []int{
		histIntCountCodec, histFloatCountCodec, histSumCodec, histSchemaCodec, histZeroThresholdCodec,
		histIntZeroCountCodec, histFloatZeroCountCodec, histResetHintCodec, histPositiveIntBucketCodec,
		histNegativeIntBucketCodec, histPositiveFloatBucketCodec, histNegativeFloatBucketCodec,
		histPositiveBucketCountCodec, histNegativeBucketCountCodec, histCustomValueCodec, histCustomCountCodec,
	} {
		fields[i].Type = arrow.PrimitiveTypes.Uint8
	}
	for i, name := range &names {
		fields[i].Name = name
	}
	return arrow.StructOf(fields...)
}

func histogramBinaryFields(h *typedHistogramSegment) (fields [histogramColumnCount][]byte) {
	fields[histIntCounts], fields[histFloatCounts], fields[histSums] = h.intCounts, h.floatCounts, h.sums
	fields[histSchemas], fields[histZeroThresholds] = h.schemas, h.zeroThresholds
	fields[histIntZeroCounts], fields[histFloatZeroCounts], fields[histResetHints] = h.intZeroCounts, h.floatZeroCounts, h.resetHints
	fields[histPositiveSpans], fields[histNegativeSpans] = h.positiveSpans, h.negativeSpans
	fields[histPositiveIntBuckets], fields[histNegativeIntBuckets] = h.positiveIntBuckets, h.negativeIntBuckets
	fields[histPositiveFloatBuckets], fields[histNegativeFloatBuckets] = h.positiveFloatBuckets, h.negativeFloatBuckets
	fields[histPositiveBucketCounts], fields[histNegativeBucketCounts] = h.positiveBucketCounts, h.negativeBucketCounts
	fields[histCustomValues], fields[histCustomCounts] = h.customValues, h.customCounts
	return fields
}

func histogramIntegerCodecs(h *typedHistogramSegment) (codecs [histogramColumnCount]uint8) {
	codecs[histIntCountCodec] = uint8(h.intCountCodec)
	codecs[histSchemaCodec] = uint8(h.schemaCodec)
	codecs[histIntZeroCountCodec] = uint8(h.intZeroCountCodec)
	codecs[histResetHintCodec] = uint8(h.resetHintCodec)
	codecs[histPositiveIntBucketCodec] = uint8(h.positiveIntBucketCodec)
	codecs[histNegativeIntBucketCodec] = uint8(h.negativeIntBucketCodec)
	codecs[histPositiveBucketCountCodec] = uint8(h.positiveBucketCountCodec)
	codecs[histNegativeBucketCountCodec] = uint8(h.negativeBucketCountCodec)
	codecs[histCustomCountCodec] = uint8(h.customCountCodec)
	return codecs
}

func histogramFloatCodecs(h *typedHistogramSegment) (codecs [histogramColumnCount]uint8) {
	codecs[histFloatCountCodec] = uint8(h.floatCountsCodec)
	codecs[histSumCodec] = uint8(h.sumCodec)
	codecs[histZeroThresholdCodec] = uint8(h.zeroThresholdCodec)
	codecs[histFloatZeroCountCodec] = uint8(h.floatZeroCountCodec)
	codecs[histPositiveFloatBucketCodec] = uint8(h.positiveFloatBucketCodec)
	codecs[histNegativeFloatBucketCodec] = uint8(h.negativeFloatBucketCodec)
	codecs[histCustomValueCodec] = uint8(h.customValueCodec)
	return codecs
}

func appendTypedHistogram(builder *array.StructBuilder, h typedHistogramSegment) {
	builder.Append(h.count > 0)
	if h.count == 0 {
		return
	}
	binaries, integers, floats := histogramBinaryFields(&h), histogramIntegerCodecs(&h), histogramFloatCodecs(&h)
	for field := range histogramColumnCount {
		switch field {
		case histIntCountsAsFloat:
			builder.FieldBuilder(field).(*array.BooleanBuilder).Append(h.intCountsAsFloat)
		case histIntZeroCountsAsFloat:
			builder.FieldBuilder(field).(*array.BooleanBuilder).Append(h.intZeroCountsAsFloat)
		case histPositiveLayoutStable:
			builder.FieldBuilder(field).(*array.BooleanBuilder).Append(h.positiveLayoutStable)
		case histNegativeLayoutStable:
			builder.FieldBuilder(field).(*array.BooleanBuilder).Append(h.negativeLayoutStable)
		case histIntCountCodec, histSchemaCodec, histIntZeroCountCodec, histResetHintCodec,
			histPositiveIntBucketCodec, histNegativeIntBucketCodec, histPositiveBucketCountCodec,
			histNegativeBucketCountCodec, histCustomCountCodec:
			builder.FieldBuilder(field).(*array.Uint8Builder).Append(integers[field])
		case histFloatCountCodec, histSumCodec, histZeroThresholdCodec, histFloatZeroCountCodec,
			histPositiveFloatBucketCodec, histNegativeFloatBucketCodec, histCustomValueCodec:
			builder.FieldBuilder(field).(*array.Uint8Builder).Append(floats[field])
		default:
			binaryBuilder, ok := builder.FieldBuilder(field).(*array.BinaryBuilder)
			if !ok {
				panic(fmt.Sprintf("histogram binary field %d has builder %T", field, builder.FieldBuilder(field)))
			}
			binaryBuilder.Append(binaries[field])
		}
	}
}

func readTypedHistogram(fields *array.Struct, row int) typedHistogramSegment {
	binaries := func(index int) []byte { return slices.Clone(fields.Field(index).(*array.Binary).Value(row)) }
	unsigned := func(index int) uint8 { return fields.Field(index).(*array.Uint8).Value(row) }
	boolean := func(index int) bool { return fields.Field(index).(*array.Boolean).Value(row) }
	return typedHistogramSegment{
		intCounts: binaries(histIntCounts), intCountCodec: integerCodec(unsigned(histIntCountCodec)),
		floatCounts: binaries(histFloatCounts), floatCountsCodec: floatCodec(unsigned(histFloatCountCodec)),
		intCountsAsFloat: boolean(histIntCountsAsFloat), sums: binaries(histSums), sumCodec: floatCodec(unsigned(histSumCodec)),
		schemas: binaries(histSchemas), schemaCodec: integerCodec(unsigned(histSchemaCodec)),
		zeroThresholds: binaries(histZeroThresholds), zeroThresholdCodec: floatCodec(unsigned(histZeroThresholdCodec)),
		intZeroCounts: binaries(histIntZeroCounts), intZeroCountCodec: integerCodec(unsigned(histIntZeroCountCodec)),
		floatZeroCounts: binaries(histFloatZeroCounts), floatZeroCountCodec: floatCodec(unsigned(histFloatZeroCountCodec)),
		intZeroCountsAsFloat: boolean(histIntZeroCountsAsFloat), resetHints: binaries(histResetHints),
		resetHintCodec: integerCodec(unsigned(histResetHintCodec)), positiveSpans: binaries(histPositiveSpans),
		positiveLayoutStable: boolean(histPositiveLayoutStable), negativeSpans: binaries(histNegativeSpans),
		negativeLayoutStable: boolean(histNegativeLayoutStable), positiveIntBuckets: binaries(histPositiveIntBuckets),
		positiveIntBucketCodec: integerCodec(unsigned(histPositiveIntBucketCodec)), negativeIntBuckets: binaries(histNegativeIntBuckets),
		negativeIntBucketCodec: integerCodec(unsigned(histNegativeIntBucketCodec)), positiveFloatBuckets: binaries(histPositiveFloatBuckets),
		positiveFloatBucketCodec: floatCodec(unsigned(histPositiveFloatBucketCodec)), negativeFloatBuckets: binaries(histNegativeFloatBuckets),
		negativeFloatBucketCodec: floatCodec(unsigned(histNegativeFloatBucketCodec)), positiveBucketCounts: binaries(histPositiveBucketCounts),
		positiveBucketCountCodec: integerCodec(unsigned(histPositiveBucketCountCodec)), negativeBucketCounts: binaries(histNegativeBucketCounts),
		negativeBucketCountCodec: integerCodec(unsigned(histNegativeBucketCountCodec)), customValues: binaries(histCustomValues),
		customValueCodec: floatCodec(unsigned(histCustomValueCodec)), customCounts: binaries(histCustomCounts),
		customCountCodec: integerCodec(unsigned(histCustomCountCodec)),
	}
}

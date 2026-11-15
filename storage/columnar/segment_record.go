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
	"fmt"
	"math"
	"slices"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/prometheus/prometheus/model/labels"
)

const DefaultSamplesPerSegment = 256

// appendRecordSeries keeps the existing read/decode entry point while routing
// physical segment pages through the common typed decoder.
func appendRecordSeries(out *[]Series, record arrow.RecordBatch) error {
	return appendSegmentRecordSeries(out, record)
}

func buildSegmentRecord(mem memory.Allocator, series []Series) (arrow.RecordBatch, error) {
	builder := array.NewRecordBuilder(mem, BlockSchema())
	defer builder.Release()
	ids := builder.Field(0).(*array.Uint64Builder)
	labelSets := builder.Field(1).(*array.ListBuilder)
	segmentLists := builder.Field(2).(*array.ListBuilder)

	for _, item := range series {
		ids.Append(metadataSeriesKey(item.Labels))
		appendMetadataLabels(labelSets, item.Labels)
		segmentLists.Append(true)
		segments := segmentLists.ValueBuilder().(*array.StructBuilder)
		for start := 0; start < item.Len(); start += DefaultSamplesPerSegment {
			end := min(start+DefaultSamplesPerSegment, item.Len())
			var starts []int64
			if len(item.StartTimestamps) > 0 {
				starts = item.StartTimestamps[start:end]
			}
			var histograms [][]byte
			if len(item.Histograms) > 0 {
				histograms = item.Histograms[start:end]
			}
			segment, err := NewSampleSegment(
				item.Labels, item.Timestamps[start:end], starts,
				item.Kinds[start:end], item.Values[start:end], histograms,
			)
			if err != nil {
				return nil, err
			}
			appendSampleSegment(segments, segment)
		}
	}
	return builder.NewRecordBatch(), nil
}

func appendSampleSegment(builder *array.StructBuilder, segment SampleSegment) {
	builder.Append(true)
	builder.FieldBuilder(0).(*array.Uint64Builder).Append(uint64(segment.Count))
	builder.FieldBuilder(1).(*array.Int64Builder).Append(segment.MinTime)
	builder.FieldBuilder(2).(*array.Int64Builder).Append(segment.MaxTime)
	builder.FieldBuilder(3).(*array.Float64Builder).Append(segment.MinValue)
	builder.FieldBuilder(4).(*array.Float64Builder).Append(segment.MaxValue)
	builder.FieldBuilder(5).(*array.Uint8Builder).Append(uint8(segment.timestampCodec))
	builder.FieldBuilder(6).(*array.BinaryBuilder).Append(segment.timestamps)
	builder.FieldBuilder(7).(*array.Uint8Builder).Append(uint8(segment.valueCodec))
	builder.FieldBuilder(8).(*array.BinaryBuilder).Append(segment.values)
	builder.FieldBuilder(9).(*array.Uint8Builder).Append(uint8(segment.kindCodec))
	builder.FieldBuilder(10).(*array.BinaryBuilder).Append(segment.kinds)
	builder.FieldBuilder(11).(*array.Uint8Builder).Append(uint8(segment.startCodec))
	builder.FieldBuilder(12).(*array.BinaryBuilder).Append(segment.startTimes)
	appendTypedHistogram(builder.FieldBuilder(13).(*array.StructBuilder), segment.histograms)
}

func appendSegmentRecordSeries(out *[]Series, record arrow.RecordBatch) error {
	labelSets := record.Column(1).(*array.List)
	labelPairs := labelSets.ListValues().(*array.Struct)
	labelNames := labelPairs.Field(0).(*array.String)
	labelValues := labelPairs.Field(1).(*array.String)
	segmentsColumn := record.Column(2).(*array.List)
	segments := segmentsColumn.ListValues().(*array.Struct)
	counts := segments.Field(0).(*array.Uint64)
	minTimes := segments.Field(1).(*array.Int64)
	maxTimes := segments.Field(2).(*array.Int64)
	minValues := segments.Field(3).(*array.Float64)
	maxValues := segments.Field(4).(*array.Float64)
	timestampCodecs := segments.Field(5).(*array.Uint8)
	timestampPayloads := segments.Field(6).(*array.Binary)
	valueCodecs := segments.Field(7).(*array.Uint8)
	valuePayloads := segments.Field(8).(*array.Binary)
	kindCodecs := segments.Field(9).(*array.Uint8)
	kindPayloads := segments.Field(10).(*array.Binary)
	startCodecs := segments.Field(11).(*array.Uint8)
	startPayloads := segments.Field(12).(*array.Binary)
	histograms := segments.Field(13).(*array.Struct)

	for row := range int(record.NumRows()) {
		start, end := labelSets.ValueOffsets(row)
		pairs := make([]labels.Label, 0, end-start)
		for i := start; i < end; i++ {
			pairs = append(pairs, labels.Label{Name: labelNames.Value(int(i)), Value: labelValues.Value(int(i))})
		}
		ls := labels.New(pairs...)
		item := Series{Labels: ls}
		segmentStart, segmentEnd := segmentsColumn.ValueOffsets(row)
		for index := segmentStart; index < segmentEnd; index++ {
			i := int(index)
			count := counts.Value(i)
			if count > math.MaxInt {
				return fmt.Errorf("segment sample count %d overflows int", count)
			}
			segment := SampleSegment{
				SeriesHash: stableSeriesHash(ls), Count: int(count),
				MinTime: minTimes.Value(i), MaxTime: maxTimes.Value(i),
				MinValue: minValues.Value(i), MaxValue: maxValues.Value(i),
				timestampCodec: integerCodec(timestampCodecs.Value(i)), timestamps: slices.Clone(timestampPayloads.Value(i)),
				valueCodec: floatCodec(valueCodecs.Value(i)), values: slices.Clone(valuePayloads.Value(i)),
				kindCodec: integerCodec(kindCodecs.Value(i)), kinds: slices.Clone(kindPayloads.Value(i)),
				startCodec: integerCodec(startCodecs.Value(i)), startTimes: slices.Clone(startPayloads.Value(i)),
			}
			segment.histograms = readTypedHistogram(histograms, i)
			page, err := segment.Decode(ls)
			if err != nil {
				return fmt.Errorf("decoding segment %d: %w", i, err)
			}
			appendDecodedPage(&item, page)
		}
		if err := item.Validate(); err != nil {
			return err
		}
		*out = append(*out, item)
	}
	return nil
}

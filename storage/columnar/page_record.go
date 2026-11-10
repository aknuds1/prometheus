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
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sync"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
)

const (
	pageCohortColumn = iota
	pageSegmentColumn
	pageCountColumn
	pageMinTimeColumn
	pageMaxTimeColumn
	pageTimestampCodecColumn
	pageTimestampsColumn
	pageSeriesPayloadsColumn
)

// pageSchema is built once: constructing the Arrow schema for every cohort and
// every block was measurable overhead.
var (
	pageSchema           = buildPageSchema()
	pageWriterPropsCache sync.Map // compress.Compression -> *parquet.WriterProperties
	pageArrowWriterProps = pqarrow.NewArrowWriterProperties(pqarrow.WithStoreSchema())
)

// pageWriterProperties returns cached writer properties for codec. They are
// immutable configuration, so rebuilding them per block was pure overhead.
func pageWriterProperties(codec compress.Compression) *parquet.WriterProperties {
	if cached, ok := pageWriterPropsCache.Load(codec); ok {
		return cached.(*parquet.WriterProperties)
	}
	props := parquet.NewWriterProperties(
		parquet.WithCompression(codec),
		// Each Write starts a new row group. Do not split a long cohort's
		// segment rows merely because MaxSeriesPerRowGroup bounds membership.
		parquet.WithMaxRowGroupLength(math.MaxInt64),
		parquet.WithDictionaryPath(parquet.ColumnPath{"series_payloads", "list", "element", valuesColumn}, false),
	)
	pageWriterPropsCache.Store(codec, props)
	return props
}

// PageSchema is the physical sample schema. One row is one time window of one
// cohort slice. Its timestamp column is shared by every series payload, while
// each payload retains independent value, kind, start-time and histogram
// columns. Rows of one cohort form one Parquet row group.
func buildPageSchema() *arrow.Schema {
	seriesPayload := arrow.StructOf(
		arrow.Field{Name: seriesIDColumn, Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
		arrow.Field{Name: "min_value", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		arrow.Field{Name: "max_value", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		arrow.Field{Name: "value_codec", Type: arrow.PrimitiveTypes.Uint8, Nullable: false},
		arrow.Field{Name: valuesColumn, Type: arrow.BinaryTypes.Binary, Nullable: false},
		arrow.Field{Name: "kind_codec", Type: arrow.PrimitiveTypes.Uint8, Nullable: false},
		arrow.Field{Name: kindsColumn, Type: arrow.BinaryTypes.Binary, Nullable: false},
		arrow.Field{Name: "start_timestamp_codec", Type: arrow.PrimitiveTypes.Uint8, Nullable: false},
		arrow.Field{Name: startTimesCol, Type: arrow.BinaryTypes.Binary, Nullable: false},
		arrow.Field{Name: histogramsCol, Type: typedHistogramArrowType(), Nullable: true},
	)
	return arrow.NewSchema([]arrow.Field{
		{Name: "cohort_id", Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
		{Name: "segment_index", Type: arrow.PrimitiveTypes.Uint32, Nullable: false},
		{Name: "count", Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
		{Name: "min_time", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "max_time", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "timestamp_codec", Type: arrow.PrimitiveTypes.Uint8, Nullable: false},
		{Name: timestampsCol, Type: arrow.BinaryTypes.Binary, Nullable: false},
		{Name: "series_payloads", Type: arrow.ListOf(seriesPayload), Nullable: false},
	}, nil)
}

// writePageFile writes one record and therefore one Parquet row group per
// cohort. No labels are repeated in the sample payload; the block catalog owns
// the series dictionary and all row/cohort offsets.
// pageBuilderPool reuses Arrow RecordBuilders across cohorts and flushes.
// NewRecordBatch resets the builder, so a returned builder is clean for reuse.
var pageBuilderPool = sync.Pool{New: func() any { return array.NewRecordBuilder(memory.NewGoAllocator(), pageSchema) }}

func writePageFile(path string, series []Series, catalog blockCatalog, opts WriteOptions) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	writer, err := pqarrow.NewFileWriter(
		pageSchema, f,
		pageWriterProperties(opts.Compression),
		pageArrowWriterProps,
	)
	if err != nil {
		f.Close()
		return err
	}
	defer func() {
		if closeErr := writer.Close(); err == nil {
			err = closeErr
		}
	}()

	byID := make(map[uint64]int, len(series))
	for i, item := range series {
		byID[metadataSeriesKey(item.Labels)] = i
	}
	mem := memory.NewGoAllocator()
	for _, cohort := range catalog.Cohorts {
		members := make([]Series, cohort.NumSeries)
		for offset, id := range cohort.SeriesIDs {
			i, ok := byID[id]
			if !ok {
				return fmt.Errorf("cohort %d references an unknown series ID %d", cohort.ID, id)
			}
			members[offset] = series[i]
		}
		for i := range members {
			if members[i].Len() == 0 {
				return fmt.Errorf("cohort %d has no series at offset %d", cohort.ID, i)
			}
		}
		record, err := buildCohortRecord(mem, cohort, members)
		if err != nil {
			return err
		}
		err = writer.Write(record)
		record.Release()
		if err != nil {
			return err
		}
	}
	return nil
}

func buildCohortRecord(_ memory.Allocator, cohort blockCohortEntry, members []Series) (arrow.RecordBatch, error) {
	builder := pageBuilderPool.Get().(*array.RecordBuilder)
	defer func() {
		// NewRecordBatch already reset the builder; just return it to the pool.
		pageBuilderPool.Put(builder)
	}()
	cohorts := builder.Field(pageCohortColumn).(*array.Uint64Builder)
	segmentIndexes := builder.Field(pageSegmentColumn).(*array.Uint32Builder)
	counts := builder.Field(pageCountColumn).(*array.Uint64Builder)
	minTimes := builder.Field(pageMinTimeColumn).(*array.Int64Builder)
	maxTimes := builder.Field(pageMaxTimeColumn).(*array.Int64Builder)
	timestampCodecs := builder.Field(pageTimestampCodecColumn).(*array.Uint8Builder)
	timestampPayloads := builder.Field(pageTimestampsColumn).(*array.BinaryBuilder)
	payloadLists := builder.Field(pageSeriesPayloadsColumn).(*array.ListBuilder)

	for segmentIndex := range cohort.NumSegments {
		start := segmentIndex * DefaultSamplesPerSegment
		for i, item := range members {
			end := min(start+DefaultSamplesPerSegment, item.Len())
			if end <= start {
				return nil, fmt.Errorf("cohort member %s has no segment %d", item.Labels, segmentIndex)
			}
			var starts []int64
			if len(item.StartTimestamps) > 0 {
				starts = item.StartTimestamps[start:end]
			}
			var histograms [][]byte
			if len(item.Histograms) > 0 {
				histograms = item.Histograms[start:end]
			}
			segment, err := NewSampleSegment(item.Labels, item.Timestamps[start:end], starts, item.Kinds[start:end], item.Values[start:end], histograms)
			if err != nil {
				return nil, err
			}
			if i == 0 {
				cohorts.Append(cohort.ID)
				segmentIndexes.Append(uint32(segmentIndex))
				counts.Append(uint64(segment.Count))
				minTimes.Append(segment.MinTime)
				maxTimes.Append(segment.MaxTime)
				timestampCodecs.Append(uint8(segment.timestampCodec))
				timestampPayloads.Append(segment.timestamps)
				payloadLists.Append(true)
			}
			payloads := payloadLists.ValueBuilder().(*array.StructBuilder)
			payloads.Append(true)
			payloads.FieldBuilder(0).(*array.Uint64Builder).Append(metadataSeriesKey(item.Labels))
			payloads.FieldBuilder(1).(*array.Float64Builder).Append(segment.MinValue)
			payloads.FieldBuilder(2).(*array.Float64Builder).Append(segment.MaxValue)
			payloads.FieldBuilder(3).(*array.Uint8Builder).Append(uint8(segment.valueCodec))
			payloads.FieldBuilder(4).(*array.BinaryBuilder).Append(segment.values)
			payloads.FieldBuilder(5).(*array.Uint8Builder).Append(uint8(segment.kindCodec))
			payloads.FieldBuilder(6).(*array.BinaryBuilder).Append(segment.kinds)
			payloads.FieldBuilder(7).(*array.Uint8Builder).Append(uint8(segment.startCodec))
			payloads.FieldBuilder(8).(*array.BinaryBuilder).Append(segment.startTimes)
			appendTypedHistogram(payloads.FieldBuilder(9).(*array.StructBuilder), segment.histograms)
		}
	}
	return builder.NewRecordBatch(), nil
}

// selectedBlockSeries identifies only the time-overlapping segments required
// for one dictionary entry.
type selectedBlockSeries struct {
	entry    blockSeriesEntry
	segments []blockSegmentRef
	decoded  int
}

// readSelectedSeries opens exactly the row groups referenced by selections and
// decodes only their requested per-series payload offsets. Shared timestamps are
// decoded once per cohort/time-window row, not once per member series.
func (b *Block) readSelectedSeries(ctx context.Context, selections []selectedBlockSeries) ([]Series, error) {
	out := make([]Series, len(selections))
	groups := map[int]struct{}{}
	offsetsByGroup := map[int]map[int]map[int]int{}
	for i, selection := range selections {
		// The block entry is immutable and belongs to the storage lifetime.
		// Consumers may construct independent mutable copies if needed.
		out[i].Labels = selection.entry.Labels
		for _, ref := range selection.segments {
			groups[ref.RowGroup] = struct{}{}
			if offsetsByGroup[ref.RowGroup] == nil {
				offsetsByGroup[ref.RowGroup] = map[int]map[int]int{}
			}
			if offsetsByGroup[ref.RowGroup][ref.Row] == nil {
				offsetsByGroup[ref.RowGroup][ref.Row] = map[int]int{}
			}
			offsetsByGroup[ref.RowGroup][ref.Row][ref.Offset] = i
		}
	}
	if len(groups) == 0 {
		return out, nil
	}
	rowGroups := make([]int, 0, len(groups))
	for group := range groups {
		rowGroups = append(rowGroups, group)
	}
	sortInts(rowGroups)
	if b.reads != nil {
		b.reads.rowGroups.Add(uint64(len(rowGroups)))
	}

	type rowRequest struct {
		cohort  blockCohortEntry
		row     int
		offsets map[int]int
	}
	var requests []rowRequest
	for _, group := range rowGroups {
		offsetsByRow := offsetsByGroup[group]
		cohort := b.catalog.Cohorts[group]
		for row := range cohort.NumSegments {
			requests = append(requests, rowRequest{cohort: cohort, row: row, offsets: offsetsByRow[row]})
		}
	}

	rr, err := b.recordReader(ctx, rowGroups, nil)
	if err != nil {
		return nil, err
	}
	defer rr.Release()

	requestIndex := 0
	for rr.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record := rr.RecordBatch()
		for row := range int(record.NumRows()) {
			if requestIndex >= len(requests) {
				return nil, errors.New("sample payload returned more rows than the block catalog")
			}
			request := requests[requestIndex]
			requestIndex++
			if err := decodeCohortRow(record, row, request.cohort, request.row, request.offsets, selections, out); err != nil {
				return nil, err
			}
		}
	}
	if err := rr.Err(); err != nil {
		return nil, err
	}
	if requestIndex != len(requests) {
		return nil, fmt.Errorf("sample payload returned %d rows, catalog expected %d", requestIndex, len(requests))
	}
	for i := range selections {
		if selections[i].decoded != len(selections[i].segments) {
			return nil, fmt.Errorf("series %s decoded %d of %d selected segments", selections[i].entry.Labels, selections[i].decoded, len(selections[i].segments))
		}
		if err := out[i].Validate(); err != nil {
			return nil, err
		}
	}
	if b.reads != nil {
		for i := range out {
			if out[i].Len() > 0 {
				b.reads.sampleRows.Inc()
			}
		}
	}
	return out, nil
}

func decodeCohortRow(record arrow.RecordBatch, row int, cohort blockCohortEntry, segmentRow int, offsets map[int]int, selections []selectedBlockSeries, out []Series) error {
	if len(offsets) == 0 {
		return nil
	}
	cohortID := record.Column(pageCohortColumn).(*array.Uint64).Value(row)
	if cohortID != cohort.ID {
		return fmt.Errorf("sample payload cohort %d does not match catalog cohort %d", cohortID, cohort.ID)
	}
	segmentIndex := record.Column(pageSegmentColumn).(*array.Uint32).Value(row)
	if int(segmentIndex) != segmentRow {
		return fmt.Errorf("sample payload segment %d does not match catalog segment %d", segmentIndex, segmentRow)
	}
	count := record.Column(pageCountColumn).(*array.Uint64).Value(row)
	if count > math.MaxInt {
		return fmt.Errorf("segment sample count %d overflows int", count)
	}
	timestamps, err := decodeSharedTimestamps(
		record.Column(pageTimestampsColumn).(*array.Binary).Value(row),
		int(count),
		integerCodec(record.Column(pageTimestampCodecColumn).(*array.Uint8).Value(row)),
		record.Column(pageMinTimeColumn).(*array.Int64).Value(row),
		record.Column(pageMaxTimeColumn).(*array.Int64).Value(row),
	)
	if err != nil {
		return err
	}
	payloadList := record.Column(pageSeriesPayloadsColumn).(*array.List)
	payloads := payloadList.ListValues().(*array.Struct)
	seriesIDs := payloads.Field(0).(*array.Uint64)
	minValues := payloads.Field(1).(*array.Float64)
	maxValues := payloads.Field(2).(*array.Float64)
	valueCodecs := payloads.Field(3).(*array.Uint8)
	valuePayloads := payloads.Field(4).(*array.Binary)
	kindCodecs := payloads.Field(5).(*array.Uint8)
	kindPayloads := payloads.Field(6).(*array.Binary)
	startCodecs := payloads.Field(7).(*array.Uint8)
	startPayloads := payloads.Field(8).(*array.Binary)
	histograms := payloads.Field(9).(*array.Struct)
	start, end := payloadList.ValueOffsets(row)
	for offset, selectedIndex := range offsets {
		payloadIndex := int(start) + offset
		if payloadIndex >= int(end) {
			return fmt.Errorf("cohort %d segment %d has no payload offset %d", cohort.ID, segmentRow, offset)
		}
		selection := &selections[selectedIndex]
		if actual := seriesIDs.Value(payloadIndex); actual != selection.entry.ID {
			return fmt.Errorf("cohort payload series ID %d does not match dictionary ID %d", actual, selection.entry.ID)
		}
		segment := SampleSegment{
			SeriesHash: selection.entry.ID, Count: int(count), MinTime: timestamps[0], MaxTime: timestamps[len(timestamps)-1],
			MinValue: minValues.Value(payloadIndex), MaxValue: maxValues.Value(payloadIndex),
			valueCodec: floatCodec(valueCodecs.Value(payloadIndex)), values: valuePayloads.Value(payloadIndex),
			kindCodec: integerCodec(kindCodecs.Value(payloadIndex)), kinds: kindPayloads.Value(payloadIndex),
			startCodec: integerCodec(startCodecs.Value(payloadIndex)), startTimes: startPayloads.Value(payloadIndex),
		}
		segment.histograms = readTypedHistogram(histograms, payloadIndex)
		page, err := segment.decodeWithSharedTimestamps(selection.entry.Labels, timestamps)
		if err != nil {
			return fmt.Errorf("decoding cohort %d segment %d series %s: %w", cohort.ID, segmentRow, selection.entry.Labels, err)
		}
		appendDecodedPage(&out[selectedIndex], page)
		selection.decoded++
	}
	return nil
}

func appendDecodedPage(item *Series, page TypedPage) {
	previous := item.Len()
	item.Timestamps = append(item.Timestamps, page.Timestamps...)
	item.Kinds = append(item.Kinds, page.Kinds...)
	item.Values = append(item.Values, page.Values...)
	if len(item.StartTimestamps) > 0 || len(page.StartTimestamps) > 0 {
		item.StartTimestamps = append(item.StartTimestamps, make([]int64, previous-len(item.StartTimestamps))...)
		if len(page.StartTimestamps) > 0 {
			item.StartTimestamps = append(item.StartTimestamps, page.StartTimestamps...)
		} else {
			item.StartTimestamps = append(item.StartTimestamps, make([]int64, page.Len())...)
		}
	}
	if len(item.Histograms) > 0 || len(page.Histograms) > 0 {
		item.Histograms = append(item.Histograms, make([][]byte, previous-len(item.Histograms))...)
		if len(page.Histograms) > 0 {
			item.Histograms = append(item.Histograms, page.Histograms...)
		} else {
			item.Histograms = append(item.Histograms, make([][]byte, page.Len())...)
		}
	}
}

func decodeSharedTimestamps(payload []byte, count int, codec integerCodec, minTime, maxTime int64) ([]int64, error) {
	if codec == integerConstantInterval {
		timestamps := make([]int64, count)
		if count > 0 {
			timestamps[0] = minTime
		}
		if count > 1 {
			interval := (maxTime - minTime) / int64(count-1)
			for i := 1; i < count; i++ {
				timestamps[i] = timestamps[i-1] + interval
			}
		}
		return timestamps, nil
	}
	timestamps, err := decodeIntegerColumnInto(payload, count, codec, nil)
	if err != nil {
		return nil, fmt.Errorf("decoding shared timestamps: %w", err)
	}
	return timestamps, nil
}

func sortInts(values []int) { slices.Sort(values) }

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
	"os"
	"path/filepath"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"

	"github.com/prometheus/prometheus/model/labels"
)

const (
	metaFileName    = "meta.json"
	dataFileName    = "data.parquet"
	tmpDirSuffix    = ".tmp"
	seriesIDColumn  = "series_id"
	labelsColumn    = "labels"
	timestampsCol   = "timestamps"
	valuesColumn    = "values"
	labelNameColumn = "name"
	labelValueCol   = "value"
)

// BlockSchema is the Arrow schema of a block file.
//
// One row is one series: a block-local series ID, the full label set, and the
// series' timestamps and values as parallel lists. Timestamps are int64
// milliseconds, matching Prometheus. Values are float64 for now; native
// histogram samples are not representable yet, which is why ingestion rejects
// them rather than silently dropping them.
func BlockSchema() *arrow.Schema {
	labelsType := arrow.ListOf(arrow.StructOf(
		arrow.Field{Name: labelNameColumn, Type: arrow.BinaryTypes.String, Nullable: false},
		arrow.Field{Name: labelValueCol, Type: arrow.BinaryTypes.String, Nullable: false},
	))
	return arrow.NewSchema([]arrow.Field{
		{Name: seriesIDColumn, Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
		{Name: labelsColumn, Type: labelsType, Nullable: false},
		{Name: timestampsCol, Type: arrow.ListOf(arrow.PrimitiveTypes.Int64), Nullable: false},
		{Name: valuesColumn, Type: arrow.ListOf(arrow.PrimitiveTypes.Float64), Nullable: false},
	}, nil)
}

// Series is one series of a block: its identity and its samples.
//
// Timestamps must be sorted ascending and contain no duplicates; a block is a
// sorted, immutable unit. Values must have the same length as Timestamps.
type Series struct {
	Labels     labels.Labels
	Timestamps []int64
	Values     []float64
}

// Validate reports whether the series is well formed.
func (s Series) Validate() error {
	if s.Labels.IsEmpty() {
		return errors.New("series without labels")
	}
	if len(s.Timestamps) != len(s.Values) {
		return fmt.Errorf("series %s: %d timestamps but %d values", s.Labels, len(s.Timestamps), len(s.Values))
	}
	for i := 1; i < len(s.Timestamps); i++ {
		if s.Timestamps[i] <= s.Timestamps[i-1] {
			return fmt.Errorf("series %s: timestamps not strictly increasing at index %d (%d after %d)",
				s.Labels, i, s.Timestamps[i], s.Timestamps[i-1])
		}
	}
	return nil
}

// WriteOptions configures how a block file is written.
type WriteOptions struct {
	// MaxSeriesPerRowGroup bounds how many series share a row group. Since a row
	// group is the unit Parquet reads and decompresses, this trades scan
	// granularity against per-group overhead.
	// Zero selects DefaultMaxSeriesPerRowGroup.
	MaxSeriesPerRowGroup int64

	// Compression is the Parquet compression codec. The zero value selects
	// DefaultCompression.
	Compression compress.Compression
}

// DefaultMaxSeriesPerRowGroup is the default number of series per row group.
const DefaultMaxSeriesPerRowGroup = 1024

// DefaultCompression is the default Parquet compression codec.
//
// Zstd because the payload is overwhelmingly numeric time series, where
// dictionary and delta encoding have already done most of the work and the
// remaining redundancy is not worth the decode cost of a heavier codec.
var DefaultCompression = compress.Codecs.Zstd

func (o WriteOptions) withDefaults() WriteOptions {
	if o.MaxSeriesPerRowGroup <= 0 {
		o.MaxSeriesPerRowGroup = DefaultMaxSeriesPerRowGroup
	}
	if o.Compression == 0 {
		o.Compression = DefaultCompression
	}
	return o
}

// writeBlockFile writes series to path as a Parquet file with the block schema.
//
// Callers must have validated and sorted series already.
func writeBlockFile(path string, series []Series, opts WriteOptions) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}

	writer, err := pqarrow.NewFileWriter(
		BlockSchema(),
		f,
		parquet.NewWriterProperties(
			parquet.WithCompression(opts.Compression),
			parquet.WithMaxRowGroupLength(opts.MaxSeriesPerRowGroup),
		),
		pqarrow.NewArrowWriterProperties(
			// Keep the Arrow schema in the file metadata, so the reader does not
			// have to reconstruct it from Parquet types.
			pqarrow.WithStoreSchema(),
		),
	)
	if err != nil {
		// The writer owns the file from here on, so it is only closed on this path.
		f.Close()
		return err
	}
	defer func() {
		// Closing the Parquet writer also closes the underlying file.
		if closeErr := writer.Close(); err == nil {
			err = closeErr
		}
	}()

	mem := memory.NewGoAllocator()
	record, err := buildRecord(mem, series)
	if err != nil {
		return err
	}
	defer record.Release()

	return writer.Write(record)
}

// buildRecord converts series into one Arrow record batch.
func buildRecord(mem memory.Allocator, series []Series) (arrow.RecordBatch, error) {
	rb := array.NewRecordBuilder(mem, BlockSchema())
	defer rb.Release()

	idBuilder := rb.Field(0).(*array.Uint64Builder)
	listBuilder := rb.Field(1).(*array.ListBuilder)
	labelsBuilder := listBuilder.ValueBuilder().(*array.StructBuilder)
	nameBuilder := labelsBuilder.FieldBuilder(0).(*array.StringBuilder)
	valueBuilder := labelsBuilder.FieldBuilder(1).(*array.StringBuilder)
	tsBuilder := rb.Field(2).(*array.ListBuilder)
	tsValueBuilder := tsBuilder.ValueBuilder().(*array.Int64Builder)
	valBuilder := rb.Field(3).(*array.ListBuilder)
	valValueBuilder := valBuilder.ValueBuilder().(*array.Float64Builder)

	for i, s := range series {
		idBuilder.Append(uint64(i))

		// One list element per series, one struct element per label pair.
		listBuilder.Append(true)
		s.Labels.Range(func(l labels.Label) {
			labelsBuilder.Append(true)
			nameBuilder.Append(l.Name)
			valueBuilder.Append(l.Value)
		})

		tsBuilder.Append(true)
		tsValueBuilder.AppendValues(s.Timestamps, nil)

		valBuilder.Append(true)
		valValueBuilder.AppendValues(s.Values, nil)
	}

	return rb.NewRecordBatch(), nil
}

// ReadAll reads every series of the block file at path.
//
// It materialises the whole block, which is acceptable for tests and for small
// blocks but not for the query path: selectors must read only the series they
// need, and row groups are the only unit Parquet allows that to happen at.
func readAllSeries(ctx context.Context, path string) ([]Series, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	pf, err := file.NewParquetReader(f)
	if err != nil {
		return nil, err
	}
	defer pf.Close()

	reader, err := pqarrow.NewFileReader(pf, pqarrow.ArrowReadProperties{}, memory.NewGoAllocator())
	if err != nil {
		return nil, err
	}

	rr, err := reader.GetRecordReader(ctx, nil, nil)
	if err != nil {
		return nil, err
	}
	defer rr.Release()

	var out []Series
	for rr.Next() {
		record := rr.RecordBatch()
		if err := appendRecordSeries(&out, record); err != nil {
			return nil, err
		}
	}
	if err := rr.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func appendRecordSeries(out *[]Series, record arrow.RecordBatch) error {
	labelsCol := record.Column(1).(*array.List)
	labelsStruct := labelsCol.ListValues().(*array.Struct)
	names := labelsStruct.Field(0).(*array.String)
	values := labelsStruct.Field(1).(*array.String)
	tsCol := record.Column(2).(*array.List)
	tsValues := tsCol.ListValues().(*array.Int64)
	valCol := record.Column(3).(*array.List)
	valValues := valCol.ListValues().(*array.Float64)

	for row := range int(record.NumRows()) {
		start, end := labelsCol.ValueOffsets(row)
		ls := make([]labels.Label, 0, end-start)
		for i := start; i < end; i++ {
			ls = append(ls, labels.Label{Name: names.Value(int(i)), Value: values.Value(int(i))})
		}

		tsStart, tsEnd := tsCol.ValueOffsets(row)
		timestamps := make([]int64, tsEnd-tsStart)
		for i := range timestamps {
			timestamps[i] = tsValues.Value(int(tsStart) + i)
		}

		vStart, vEnd := valCol.ValueOffsets(row)
		vals := make([]float64, vEnd-vStart)
		for i := range vals {
			vals[i] = valValues.Value(int(vStart) + i)
		}
		if len(timestamps) != len(vals) {
			return fmt.Errorf("malformed block row %d: %d timestamps but %d values", row, len(timestamps), len(vals))
		}

		*out = append(*out, Series{Labels: labels.New(ls...), Timestamps: timestamps, Values: vals})
	}
	return nil
}

// blockDataPath returns the path of the Parquet file inside a block directory.
func blockDataPath(dir string) string { return filepath.Join(dir, dataFileName) }

// blockMetaPath returns the path of the metadata file inside a block directory.
func blockMetaPath(dir string) string { return filepath.Join(dir, metaFileName) }

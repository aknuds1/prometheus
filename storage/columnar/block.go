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

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
)

const (
	metaFileName    = "meta.json"
	dataFileName    = "data.parquet"
	tmpDirSuffix    = ".tmp"
	seriesIDColumn  = "series_id"
	labelsColumn    = "labels"
	timestampsCol   = "timestamps"
	kindsColumn     = "kinds"
	valuesColumn    = "values"
	histogramsCol   = "histograms"
	labelNameColumn = "name"
	labelValueCol   = "value"
)

// SampleKind describes what one sample of a series holds.
type SampleKind int8

const (
	// SampleFloat is a float64 sample.
	SampleFloat SampleKind = 0
	// SampleHistogram is an integer-count native histogram sample.
	SampleHistogram SampleKind = 1
	// SampleFloatHistogram is a float-count native histogram sample.
	SampleFloatHistogram SampleKind = 2
	// SampleStale is a staleness marker.
	//
	// Staleness gets its own kind rather than being a NaN payload, because Arrow
	// and Parquet canonicalise every NaN to a quiet NaN. Prometheus marks staleness
	// with a signalling NaN, so a NaN round trip through this format silently turns
	// a stale marker back into a normal NaN sample, which changes query results.
	// The stored value of a stale sample is not meaningful.
	SampleStale SampleKind = 3
)

// BlockSchema is the Arrow schema of a block file.
//
// One row is one series: a block-local series ID, the full label set, and the
// series' samples as parallel lists. Timestamps are int64 milliseconds, matching
// Prometheus.
//
// Samples of different types share the row rather than getting a row each,
// because a PromQL range over a series must see one interleaved sequence. The
// per-sample kind column says which of the value columns is populated, so a
// float-only series pays for the histogram column only as a run of nulls, which
// Parquet encodes almost for free.
func BlockSchema() *arrow.Schema {
	labelsType := arrow.ListOf(arrow.StructOf(
		arrow.Field{Name: labelNameColumn, Type: arrow.BinaryTypes.String, Nullable: false},
		arrow.Field{Name: labelValueCol, Type: arrow.BinaryTypes.String, Nullable: false},
	))
	return arrow.NewSchema([]arrow.Field{
		{Name: seriesIDColumn, Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
		{Name: labelsColumn, Type: labelsType, Nullable: false},
		{Name: timestampsCol, Type: arrow.ListOf(arrow.PrimitiveTypes.Int64), Nullable: false},
		{Name: kindsColumn, Type: arrow.ListOf(arrow.PrimitiveTypes.Int8), Nullable: false},
		{Name: valuesColumn, Type: arrow.ListOf(arrow.PrimitiveTypes.Float64), Nullable: false},
		{Name: histogramsCol, Type: arrow.ListOf(arrow.BinaryTypes.Binary), Nullable: false},
	}, nil)
}

// Series is one series of a block: its identity and its samples.
//
// Timestamps, Kinds, Values and Histograms are parallel and must have the same
// length. Timestamps must be strictly increasing; a block is a sorted, immutable
// unit. For a SampleFloat entry the value in Values is used and Histograms holds
// nil; for a histogram entry the reverse.
type Series struct {
	Labels     labels.Labels
	Timestamps []int64
	Kinds      []SampleKind
	// Values holds the float64 value of every sample, 0 for histogram samples.
	Values []float64
	// Histograms holds the encoded histogram of every sample, nil for float samples.
	Histograms [][]byte
}

// NewFloatSeries returns a Series holding only float samples, which is the
// common case. Callers with histogram samples must fill Kinds and Histograms
// themselves.
func NewFloatSeries(lbls labels.Labels, timestamps []int64, values []float64) Series {
	kinds := make([]SampleKind, len(timestamps))
	return Series{
		Labels:     lbls,
		Timestamps: timestamps,
		Kinds:      kinds,
		Values:     values,
		Histograms: make([][]byte, len(timestamps)),
	}
}

// Len returns the number of samples in the series.
func (s Series) Len() int { return len(s.Timestamps) }

// HistogramAt decodes the histogram at index i.
func (s Series) HistogramAt(i int) (*histogram.Histogram, *histogram.FloatHistogram, error) {
	var hp prompb.Histogram
	if err := hp.Unmarshal(s.Histograms[i]); err != nil {
		return nil, nil, fmt.Errorf("decoding histogram sample %d: %w", i, err)
	}
	if hp.IsFloatHistogram() {
		return nil, hp.ToFloatHistogram(), nil
	}
	return hp.ToIntHistogram(), nil, nil
}

// encodeHistogram serialises a histogram sample.
//
// The timestamp is not stored in the blob: it is a column of its own, and
// duplicating it would let the two disagree.
func encodeHistogram(h *histogram.Histogram, fh *histogram.FloatHistogram) ([]byte, error) {
	var hp prompb.Histogram
	switch {
	case fh != nil:
		hp = prompb.FromFloatHistogram(0, fh)
	case h != nil:
		hp = prompb.FromIntHistogram(0, h)
	default:
		return nil, errors.New("encodeHistogram called without a histogram")
	}
	return hp.Marshal()
}

// Validate reports whether the series is well formed.
func (s Series) Validate() error {
	if s.Labels.IsEmpty() {
		return errors.New("series without labels")
	}
	n := len(s.Timestamps)
	if len(s.Kinds) != n || len(s.Values) != n || len(s.Histograms) != n {
		return fmt.Errorf("series %s: %d timestamps, %d kinds, %d values, %d histograms",
			s.Labels, n, len(s.Kinds), len(s.Values), len(s.Histograms))
	}
	for i := range n {
		if i > 0 && s.Timestamps[i] <= s.Timestamps[i-1] {
			return fmt.Errorf("series %s: timestamps not strictly increasing at index %d (%d after %d)",
				s.Labels, i, s.Timestamps[i], s.Timestamps[i-1])
		}
		switch s.Kinds[i] {
		case SampleFloat, SampleStale:
			if s.Histograms[i] != nil {
				return fmt.Errorf("series %s: float sample at index %d carries a histogram", s.Labels, i)
			}
		case SampleHistogram, SampleFloatHistogram:
			if len(s.Histograms[i]) == 0 {
				return fmt.Errorf("series %s: histogram sample at index %d has no payload", s.Labels, i)
			}
		default:
			return fmt.Errorf("series %s: unknown sample kind %d at index %d", s.Labels, s.Kinds[i], i)
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
	kindBuilder := rb.Field(3).(*array.ListBuilder)
	kindValueBuilder := kindBuilder.ValueBuilder().(*array.Int8Builder)
	valBuilder := rb.Field(4).(*array.ListBuilder)
	valValueBuilder := valBuilder.ValueBuilder().(*array.Float64Builder)
	histBuilder := rb.Field(5).(*array.ListBuilder)
	histValueBuilder := histBuilder.ValueBuilder().(*array.BinaryBuilder)

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

		kindBuilder.Append(true)
		for _, kind := range s.Kinds {
			kindValueBuilder.Append(int8(kind))
		}

		valBuilder.Append(true)
		for j, kind := range s.Kinds {
			if kind == SampleFloat || kind == SampleStale {
				valValueBuilder.Append(s.Values[j])
				continue
			}
			valValueBuilder.AppendNull()
		}

		histBuilder.Append(true)
		for _, hb := range s.Histograms {
			if hb == nil {
				histValueBuilder.AppendNull()
				continue
			}
			histValueBuilder.Append(hb)
		}
	}

	return rb.NewRecordBatch(), nil
}

// readAllSeries reads every series of the block file at path.
//
// It materialises the whole block, which is acceptable for small blocks and for
// the conformance path, but not for querying: a selector should read only the
// row groups it needs, and Parquet offers no finer unit.
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
		if err := appendRecordSeries(&out, rr.RecordBatch()); err != nil {
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
	kindCol := record.Column(3).(*array.List)
	kindValues := kindCol.ListValues().(*array.Int8)
	valCol := record.Column(4).(*array.List)
	valValues := valCol.ListValues().(*array.Float64)
	histCol := record.Column(5).(*array.List)
	histValues := histCol.ListValues().(*array.Binary)

	for row := range int(record.NumRows()) {
		start, end := labelsCol.ValueOffsets(row)
		ls := make([]labels.Label, 0, end-start)
		for i := start; i < end; i++ {
			ls = append(ls, labels.Label{Name: names.Value(int(i)), Value: values.Value(int(i))})
		}

		tsStart, tsEnd := tsCol.ValueOffsets(row)
		n := int(tsEnd - tsStart)
		timestamps := make([]int64, n)
		for i := range timestamps {
			timestamps[i] = tsValues.Value(int(tsStart) + i)
		}

		kindStart, _ := kindCol.ValueOffsets(row)
		kinds := make([]SampleKind, n)
		for i := range kinds {
			kinds[i] = SampleKind(kindValues.Value(int(kindStart) + i))
		}

		valStart, _ := valCol.ValueOffsets(row)
		vals := make([]float64, n)
		for i := range vals {
			if valValues.IsNull(int(valStart) + i) {
				continue
			}
			vals[i] = valValues.Value(int(valStart) + i)
		}

		histStart, _ := histCol.ValueOffsets(row)
		hists := make([][]byte, n)
		for i := range hists {
			if histValues.IsNull(int(histStart) + i) {
				continue
			}
			hists[i] = append([]byte(nil), histValues.Value(int(histStart)+i)...)
		}

		*out = append(*out, Series{
			Labels:     labels.New(ls...),
			Timestamps: timestamps,
			Kinds:      kinds,
			Values:     vals,
			Histograms: hists,
		})
	}
	return nil
}

// blockDataPath returns the path of the Parquet file inside a block directory.
func blockDataPath(dir string) string { return filepath.Join(dir, dataFileName) }

// blockMetaPath returns the path of the metadata file inside a block directory.
func blockMetaPath(dir string) string { return filepath.Join(dir, metaFileName) }

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
	segmentsColumn  = "segments"
	tmpDirSuffix    = ".tmp"
	seriesIDColumn  = "series_id"
	labelsColumn    = "labels"
	timestampsCol   = "timestamps"
	kindsColumn     = "kinds"
	valuesColumn    = "values"
	histogramsCol   = "histograms"
	startTimesCol   = "start_timestamps"
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

// BlockSchema is the Arrow schema of a block file. Each row contains one
// series and its sealed segments. Each segment has independent typed columns
// for time, float value, kind, start time and histogram payloads, plus bounds and
// codec IDs. DecodeRecord and PageIterator hide those physical choices.
func BlockSchema() *arrow.Schema {
	segmentType := arrow.StructOf(
		arrow.Field{Name: "count", Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
		arrow.Field{Name: "min_time", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		arrow.Field{Name: "max_time", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		arrow.Field{Name: "min_value", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		arrow.Field{Name: "max_value", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		arrow.Field{Name: "timestamp_codec", Type: arrow.PrimitiveTypes.Uint8, Nullable: false},
		arrow.Field{Name: "timestamps", Type: arrow.BinaryTypes.Binary, Nullable: false},
		arrow.Field{Name: "value_codec", Type: arrow.PrimitiveTypes.Uint8, Nullable: false},
		arrow.Field{Name: "values", Type: arrow.BinaryTypes.Binary, Nullable: false},
		arrow.Field{Name: "kind_codec", Type: arrow.PrimitiveTypes.Uint8, Nullable: false},
		arrow.Field{Name: "kinds", Type: arrow.BinaryTypes.Binary, Nullable: false},
		arrow.Field{Name: "start_timestamp_codec", Type: arrow.PrimitiveTypes.Uint8, Nullable: false},
		arrow.Field{Name: "start_timestamps", Type: arrow.BinaryTypes.Binary, Nullable: false},
		arrow.Field{Name: "histograms", Type: arrow.ListOf(arrow.BinaryTypes.Binary), Nullable: false},
	)
	return arrow.NewSchema([]arrow.Field{
		{Name: seriesIDColumn, Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
		{Name: labelsColumn, Type: metadataStringPairsType(), Nullable: false},
		{Name: segmentsColumn, Type: arrow.ListOf(segmentType), Nullable: false},
	}, nil)
}

// Series is one series of a block: its identity and its samples.
//
// Timestamps, Kinds and Values are parallel and must have the same length.
// StartTimestamps and Histograms may be nil when the entire optional column is
// respectively unknown/empty. Timestamps are strictly increasing; blocks are immutable
// unit. For a SampleFloat entry the value in Values is used and Histograms holds
// nil; for a histogram entry the reverse.
type Series struct {
	Labels     labels.Labels
	Timestamps []int64
	// StartTimestamps is the start timestamp of each sample, or 0 when unknown.
	StartTimestamps []int64
	Kinds           []SampleKind
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
		Labels:          lbls,
		Timestamps:      timestamps,
		StartTimestamps: make([]int64, len(timestamps)),
		Kinds:           kinds,
		Values:          values,
		Histograms:      make([][]byte, len(timestamps)),
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
	if len(s.StartTimestamps) != 0 && len(s.StartTimestamps) != n {
		return fmt.Errorf("series %s: %d timestamps but %d start timestamps", s.Labels, n, len(s.StartTimestamps))
	}
	if len(s.Kinds) != n || len(s.Values) != n {
		return fmt.Errorf("series %s: %d timestamps, %d kinds, %d values",
			s.Labels, n, len(s.Kinds), len(s.Values))
	}
	if len(s.Histograms) != 0 && len(s.Histograms) != n {
		return fmt.Errorf("series %s: %d timestamps, %d histograms", s.Labels, n, len(s.Histograms))
	}
	for i := range n {
		if i > 0 && s.Timestamps[i] <= s.Timestamps[i-1] {
			return fmt.Errorf("series %s: timestamps not strictly increasing at index %d (%d after %d)",
				s.Labels, i, s.Timestamps[i], s.Timestamps[i-1])
		}
		switch s.Kinds[i] {
		case SampleFloat, SampleStale:
			if len(s.Histograms) > 0 && s.Histograms[i] != nil {
				return fmt.Errorf("series %s: float sample at index %d carries a histogram", s.Labels, i)
			}
		case SampleHistogram, SampleFloatHistogram:
			if len(s.Histograms) == 0 || len(s.Histograms[i]) == 0 {
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

	// IndexedResourceAttributes is the selected descriptive Resource attribute
	// set recorded with a metadata block. It affects only the in-memory/global query
	// index; no Parquet payload index is written for unselected attributes.
	IndexedResourceAttributes []string
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
			// Dictionary encoding must stay off for the sample values.
			//
			// A dictionary is keyed by value, and NaN never equals itself, so every NaN
			// lands in whichever slot the encoder happens to reuse. That silently
			// rewrites a quiet NaN into a signalling one, or the reverse, depending on
			// what else is in the column. Prometheus marks staleness with a signalling
			// NaN, so this is the difference between a stale marker and a value of NaN.
			// The kind column carries staleness explicitly, but the value column must
			// still round trip unchanged.
			parquet.WithDictionaryPath(parquet.ColumnPath{"values", "list", "element"}, false),
			parquet.WithDictionaryPath(parquet.ColumnPath{"histograms", "list", "element"}, false),
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
	return buildSegmentRecord(mem, series)
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

// DecodeRecord converts a record batch in the block schema back into series.
//
// It is exported because it is also the decoder a consumer of the Arrow endpoint
// needs: the stream carries exactly this schema.
func DecodeRecord(record arrow.RecordBatch) ([]Series, error) {
	var out []Series
	if err := appendRecordSeries(&out, record); err != nil {
		return nil, err
	}
	return out, nil
}

// blockDataPath returns the path of the Parquet file inside a block directory.
func blockDataPath(dir string) string { return filepath.Join(dir, dataFileName) }

// blockMetaPath returns the path of the metadata file inside a block directory.
func blockMetaPath(dir string) string { return filepath.Join(dir, metaFileName) }

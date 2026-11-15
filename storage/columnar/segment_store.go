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
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Format 10 stores samples as offset-addressed segments rather than Parquet rows.
//
// A Parquet row group is the only unit that reader can skip, and the previous
// layout kept every series of a cohort inside one row group, so a selector for a
// few series decoded the whole group. Segments are addressed directly instead:
// the catalog carries a byte range per series/time-window, so a query reads only
// the payloads it matched.
const (
	timestampsFileName = "timestamps.bin"
	segmentsFileName   = "segments.bin"
)

// blockSegmentRef locates one series/time-window payload. Timestamps are shared
// per cohort, so a payload carries only the non-time columns.
type blockSegmentRef struct {
	// Cohort indexes blockCatalog.Cohorts for the shared timestamp vector, and
	// TsStart is this segment's sample offset within that vector.
	Cohort           int
	TsStart          int
	Offset           int64
	Length           int
	Count            int
	MinTime, MaxTime int64
	ValueCodec       uint8
	KindCodec        uint8
	StartCodec       uint8
	// Compression is payloadCompressionRaw or payloadCompressionZstd.
	Compression uint8
}

const (
	payloadCompressionRaw uint8 = iota
	payloadCompressionZstd
)

var (
	payloadEncoderOnce sync.Once
	payloadEncoder     *zstd.Encoder
	payloadDecoderOnce sync.Once
	payloadDecoder     *zstd.Decoder
)

func payloadEncoderFor() *zstd.Encoder {
	payloadEncoderOnce.Do(func() {
		// Level 1: segments are 2-4 KB, so speed matters more than ratio.
		payloadEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	})
	return payloadEncoder
}

func payloadDecoderFor() *zstd.Decoder {
	payloadDecoderOnce.Do(func() {
		payloadDecoder, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	})
	return payloadDecoder
}

// compressPayload compresses a segment payload, keeping the raw bytes when
// compression does not pay.
func compressPayload(blob []byte) ([]byte, uint8) {
	if len(blob) < 64 {
		return blob, payloadCompressionRaw
	}
	enc := payloadEncoderFor()
	for _, dst := range [][]byte{make([]byte, 0, len(blob)/2+64)} {
		_ = dst
	}
	compressed := enc.EncodeAll(blob, make([]byte, 0, len(blob)/2+64))
	if len(compressed) >= len(blob) {
		return blob, payloadCompressionRaw
	}
	return compressed, payloadCompressionZstd
}

func decompressPayload(ref blockSegmentRef, blob []byte) ([]byte, error) {
	if ref.Compression == payloadCompressionRaw {
		return blob, nil
	}
	if ref.Compression != payloadCompressionZstd {
		return nil, fmt.Errorf("unknown payload compression %d", ref.Compression)
	}
	return payloadDecoderFor().DecodeAll(blob, nil)
}

// writeSegmentFiles appends timestamp vectors and per-series payloads for the
// block catalog, filling each segment reference with its byte range.
//
// Cohorts are written in catalog order and payloads in (cohort, row, member)
// order, which keeps each series' segments time-ordered.
func writeSegmentFiles(dir string, series []Series, catalog *blockCatalog) (err error) {
	byID := make(map[uint64]Series, len(series))
	for _, item := range series {
		byID[metadataSeriesKey(item.Labels)] = item
	}

	tsFile, err := os.Create(filepath.Join(dir, timestampsFileName))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := tsFile.Close(); err == nil {
			err = closeErr
		}
	}()
	segFile, err := os.Create(filepath.Join(dir, segmentsFileName))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := segFile.Close(); err == nil {
			err = closeErr
		}
	}()

	tsWriter := newOffsetWriter(tsFile)
	segWriter := newOffsetWriter(segFile)

	for c := range catalog.Cohorts {
		cohort := &catalog.Cohorts[c]
		if cohort.NumSeries == 0 {
			return errors.New("empty cohort")
		}
		first, ok := byID[cohort.SeriesIDs[0]]
		if !ok {
			return fmt.Errorf("cohort %d references unknown series %d", cohort.ID, cohort.SeriesIDs[0])
		}
		encoded, codec, err := encodeTimestamps(first.Timestamps)
		if err != nil {
			return err
		}
		cohort.TsOffset = tsWriter.offset
		cohort.TsLength = len(encoded)
		cohort.TsCodec = uint8(codec)
		if _, err := tsWriter.Write(encoded); err != nil {
			return err
		}

		for row := 0; row < cohort.NumSegments; row++ {
			start := row * DefaultSamplesPerSegment
			for _, id := range cohort.SeriesIDs {
				item, ok := byID[id]
				if !ok {
					return fmt.Errorf("cohort %d references unknown series %d", cohort.ID, id)
				}
				end := min(start+DefaultSamplesPerSegment, item.Len())
				if end <= start {
					return fmt.Errorf("series %s has no segment %d", item.Labels, row)
				}
				var starts []int64
				if len(item.StartTimestamps) > 0 {
					starts = item.StartTimestamps[start:end]
				}
				var histograms [][]byte
				if len(item.Histograms) > 0 {
					histograms = item.Histograms[start:end]
				}
				payload, err := NewSegmentPayload(
					item.Labels, item.Kinds[start:end], item.Values[start:end], starts, histograms,
				)
				if err != nil {
					return err
				}
				blob, compression := compressPayload(encodeSegmentPayload(payload))
				idx := sortSearchSeries(catalog.Series, id)
				if idx < 0 {
					return fmt.Errorf("series %d missing from the dictionary", id)
				}
				catalog.Series[idx].Segments = append(catalog.Series[idx].Segments, blockSegmentRef{
					Cohort: c, TsStart: start, Offset: segWriter.offset, Length: len(blob), Count: payload.Count,
					MinTime: item.Timestamps[start], MaxTime: item.Timestamps[end-1],
					ValueCodec: uint8(payload.valueCodec), KindCodec: uint8(payload.kindCodec),
					StartCodec: uint8(payload.startCodec), Compression: compression,
				})
				if _, err := segWriter.Write(blob); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// offsetWriter tracks the append offset of a file.
type offsetWriter struct {
	w      io.Writer
	offset int64
}

func newOffsetWriter(w io.Writer) *offsetWriter { return &offsetWriter{w: w} }

func (o *offsetWriter) Write(p []byte) (int, error) {
	n, err := o.w.Write(p)
	o.offset += int64(n)
	return n, err
}

// Payload framing: one blob holding the non-time columns and their codecs.
//
//	valueLen, values, kindLen, kinds, startLen, startTimes, histogramFrame
//
// Lengths are varints. The histogram frame is a single 0 byte when the segment
// holds no histogram samples.
func encodeSegmentPayload(segment SampleSegment) []byte {
	var out []byte
	out = appendVarBytes(out, segment.values)
	out = appendVarBytes(out, segment.kinds)
	out = appendVarBytes(out, segment.startTimes)
	return encodeHistogramFrame(out, segment.histograms)
}

func encodeHistogramFrame(out []byte, h typedHistogramSegment) []byte {
	if h.count == 0 {
		return append(out, 0)
	}
	out = append(out, 1)
	binaries := histogramBinaryFields(&h)
	integers := histogramIntegerCodecs(&h)
	floats := histogramFloatCodecs(&h)
	for field := range histogramColumnCount {
		switch field {
		case histIntCountsAsFloat:
			out = appendBoolByte(out, h.intCountsAsFloat)
		case histIntZeroCountsAsFloat:
			out = appendBoolByte(out, h.intZeroCountsAsFloat)
		case histPositiveLayoutStable:
			out = appendBoolByte(out, h.positiveLayoutStable)
		case histNegativeLayoutStable:
			out = appendBoolByte(out, h.negativeLayoutStable)
		case histIntCountCodec, histSchemaCodec, histIntZeroCountCodec, histResetHintCodec,
			histPositiveIntBucketCodec, histNegativeIntBucketCodec, histPositiveBucketCountCodec,
			histNegativeBucketCountCodec, histCustomCountCodec:
			out = append(out, integers[field])
		case histFloatCountCodec, histSumCodec, histZeroThresholdCodec, histFloatZeroCountCodec,
			histPositiveFloatBucketCodec, histNegativeFloatBucketCodec, histCustomValueCodec:
			out = append(out, floats[field])
		default:
			out = appendVarBytes(out, binaries[field])
		}
	}
	return out
}

func appendBoolByte(out []byte, v bool) []byte {
	if v {
		return append(out, 1)
	}
	return append(out, 0)
}

func appendVarBytes(out, payload []byte) []byte {
	out = binary.AppendUvarint(out, uint64(len(payload)))
	return append(out, payload...)
}

// decodeSegmentPayload reconstructs the non-time columns of a segment.
func decodeSegmentPayload(ref blockSegmentRef, blob []byte) (SampleSegment, error) {
	segment := SampleSegment{
		Count: ref.Count, MinTime: ref.MinTime, MaxTime: ref.MaxTime,
		valueCodec: floatCodec(ref.ValueCodec), kindCodec: integerCodec(ref.KindCodec),
		startCodec: integerCodec(ref.StartCodec),
	}
	var err error
	if segment.values, blob, err = takeVarBytes(blob); err != nil {
		return SampleSegment{}, fmt.Errorf("values: %w", err)
	}
	if segment.kinds, blob, err = takeVarBytes(blob); err != nil {
		return SampleSegment{}, fmt.Errorf("kinds: %w", err)
	}
	if segment.startTimes, blob, err = takeVarBytes(blob); err != nil {
		return SampleSegment{}, fmt.Errorf("start timestamps: %w", err)
	}
	if segment.histograms, blob, err = decodeHistogramFrame(blob); err != nil {
		return SampleSegment{}, err
	}
	if len(blob) != 0 {
		return SampleSegment{}, errors.New("trailing bytes in segment payload")
	}
	return segment, nil
}

func takeVarBytes(in []byte) (payload, rest []byte, err error) {
	length, n := binary.Uvarint(in)
	if n <= 0 {
		return nil, nil, errors.New("invalid length prefix")
	}
	in = in[n:]
	if uint64(len(in)) < length {
		return nil, nil, errors.New("truncated payload")
	}
	return in[:length], in[length:], nil
}

func decodeHistogramFrame(in []byte) (typedHistogramSegment, []byte, error) {
	if len(in) == 0 {
		return typedHistogramSegment{}, nil, errors.New("missing histogram frame")
	}
	hasHistograms := in[0]
	in = in[1:]
	var h typedHistogramSegment
	if hasHistograms == 0 {
		return h, in, nil
	}
	var binaries [histogramColumnCount][]byte
	var integers [histogramColumnCount]uint8
	var floats [histogramColumnCount]uint8
	takeByte := func() (byte, error) {
		if len(in) == 0 {
			return 0, errors.New("truncated histogram frame")
		}
		v := in[0]
		in = in[1:]
		return v, nil
	}
	for field := range histogramColumnCount {
		switch field {
		case histIntCountsAsFloat:
			b, err := takeByte()
			if err != nil {
				return h, nil, err
			}
			h.intCountsAsFloat = b == 1
		case histIntZeroCountsAsFloat:
			b, err := takeByte()
			if err != nil {
				return h, nil, err
			}
			h.intZeroCountsAsFloat = b == 1
		case histPositiveLayoutStable:
			b, err := takeByte()
			if err != nil {
				return h, nil, err
			}
			h.positiveLayoutStable = b == 1
		case histNegativeLayoutStable:
			b, err := takeByte()
			if err != nil {
				return h, nil, err
			}
			h.negativeLayoutStable = b == 1
		case histIntCountCodec, histSchemaCodec, histIntZeroCountCodec, histResetHintCodec,
			histPositiveIntBucketCodec, histNegativeIntBucketCodec, histPositiveBucketCountCodec,
			histNegativeBucketCountCodec, histCustomCountCodec:
			b, err := takeByte()
			if err != nil {
				return h, nil, err
			}
			integers[field] = b
		case histFloatCountCodec, histSumCodec, histZeroThresholdCodec, histFloatZeroCountCodec,
			histPositiveFloatBucketCodec, histNegativeFloatBucketCodec, histCustomValueCodec:
			b, err := takeByte()
			if err != nil {
				return h, nil, err
			}
			floats[field] = b
		default:
			payload, rest, err := takeVarBytes(in)
			if err != nil {
				return h, nil, err
			}
			in = rest
			binaries[field] = payload
		}
	}
	h.intCounts = binaries[histIntCounts]
	h.intCountCodec = integerCodec(integers[histIntCountCodec])
	h.floatCounts = binaries[histFloatCounts]
	h.floatCountsCodec = floatCodec(floats[histFloatCountCodec])
	h.sums = binaries[histSums]
	h.sumCodec = floatCodec(floats[histSumCodec])
	h.schemas = binaries[histSchemas]
	h.schemaCodec = integerCodec(integers[histSchemaCodec])
	h.zeroThresholds = binaries[histZeroThresholds]
	h.zeroThresholdCodec = floatCodec(floats[histZeroThresholdCodec])
	h.intZeroCounts = binaries[histIntZeroCounts]
	h.intZeroCountCodec = integerCodec(integers[histIntZeroCountCodec])
	h.floatZeroCounts = binaries[histFloatZeroCounts]
	h.floatZeroCountCodec = floatCodec(floats[histFloatZeroCountCodec])
	h.resetHints = binaries[histResetHints]
	h.resetHintCodec = integerCodec(integers[histResetHintCodec])
	h.positiveSpans = binaries[histPositiveSpans]
	h.negativeSpans = binaries[histNegativeSpans]
	h.positiveIntBuckets = binaries[histPositiveIntBuckets]
	h.positiveIntBucketCodec = integerCodec(integers[histPositiveIntBucketCodec])
	h.negativeIntBuckets = binaries[histNegativeIntBuckets]
	h.negativeIntBucketCodec = integerCodec(integers[histNegativeIntBucketCodec])
	h.positiveFloatBuckets = binaries[histPositiveFloatBuckets]
	h.positiveFloatBucketCodec = floatCodec(floats[histPositiveFloatBucketCodec])
	h.negativeFloatBuckets = binaries[histNegativeFloatBuckets]
	h.negativeFloatBucketCodec = floatCodec(floats[histNegativeFloatBucketCodec])
	h.positiveBucketCounts = binaries[histPositiveBucketCounts]
	h.positiveBucketCountCodec = integerCodec(integers[histPositiveBucketCountCodec])
	h.negativeBucketCounts = binaries[histNegativeBucketCounts]
	h.negativeBucketCountCodec = integerCodec(integers[histNegativeBucketCountCodec])
	h.customValues = binaries[histCustomValues]
	h.customValueCodec = floatCodec(floats[histCustomValueCodec])
	h.customCounts = binaries[histCustomCounts]
	h.customCountCodec = integerCodec(integers[histCustomCountCodec])
	return h, in, nil
}

// segmentReader holds the open payload files of a block.
type segmentReader struct {
	segments   *os.File
	timestamps *os.File
}

// readAt returns length bytes at offset.
func (b *Block) readAt(f *os.File, offset int64, length int) ([]byte, error) {
	if length < 0 || offset < 0 {
		return nil, errors.New("negative segment range")
	}
	buf := make([]byte, length)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return nil, err
	}
	return buf, nil
}

// timestampVector decodes the shared timestamp column of a cohort.
func (b *Block) timestampVector(cohort int) ([]int64, error) {
	if cohort < 0 || cohort >= len(b.catalog.Cohorts) {
		return nil, fmt.Errorf("cohort %d out of range", cohort)
	}
	c := b.catalog.Cohorts[cohort]
	payload, err := b.readAt(b.reader.timestamps, c.TsOffset, c.TsLength)
	if err != nil {
		return nil, err
	}
	return decodeIntegerColumn(payload, c.TsCount, integerCodec(c.TsCodec))
}

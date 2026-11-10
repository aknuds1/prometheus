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
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// selectedBlockSeries identifies only the time-overlapping segments required for
// one dictionary entry.
type selectedBlockSeries struct {
	entry    blockSeriesEntry
	segments []blockSegmentRef
	decoded  int
}

// readSelectedSeries reads exactly the requested segment payloads. Unlike a
// Parquet row group, a payload range is a single series' own bytes, so the read
// cost scales with the number of matched series rather than with every series in
// the block.
func (b *Block) readSelectedSeries(ctx context.Context, selections []selectedBlockSeries) ([]Series, error) {
	out := make([]Series, len(selections))
	if len(selections) == 0 {
		return out, nil
	}
	if b.reads != nil {
		b.reads.samples.Inc()
	}
	reader, err := b.openSegmentReader()
	if err != nil {
		return nil, err
	}

	timestampsByCohort := make(map[int][]int64, 8)
	for i, selection := range selections {
		out[i].Labels = selection.entry.Labels
		for _, ref := range selection.segments {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			timestamps, ok := timestampsByCohort[ref.Cohort]
			if !ok {
				timestamps, err = b.timestampVector(ref.Cohort)
				if err != nil {
					return nil, fmt.Errorf("reading cohort %d timestamps: %w", ref.Cohort, err)
				}
				timestampsByCohort[ref.Cohort] = timestamps
			}
			if ref.TsStart < 0 || ref.TsStart+ref.Count > len(timestamps) {
				return nil, fmt.Errorf("segment window [%d,%d) exceeds cohort vector of %d samples",
					ref.TsStart, ref.TsStart+ref.Count, len(timestamps))
			}
			blob, err := b.readAt(reader.segments, ref.Offset, ref.Length)
			if err != nil {
				return nil, fmt.Errorf("reading segment at %d: %w", ref.Offset, err)
			}
			if b.reads != nil {
				b.reads.payloadBytes.Add(uint64(len(blob)))
				b.reads.segmentsRead.Inc()
			}
			blob, err = decompressPayload(ref, blob)
			if err != nil {
				return nil, fmt.Errorf("decompressing segment at %d: %w", ref.Offset, err)
			}
			segment, err := decodeSegmentPayload(ref, blob)
			if err != nil {
				return nil, fmt.Errorf("decoding segment at %d: %w", ref.Offset, err)
			}
			page := TypedPage{Labels: selection.entry.Labels, Timestamps: timestamps[ref.TsStart : ref.TsStart+ref.Count]}
			if err := segment.decodeColumnsInto(selection.entry.Labels, &page); err != nil {
				return nil, fmt.Errorf("decoding segment at %d: %w", ref.Offset, err)
			}
			appendDecodedPage(&out[i], page)
			selection.decoded++
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

// appendDecodedPage appends one decoded page to series.
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

// openSegmentReader lazily opens the payload files of the block.
func (b *Block) openSegmentReader() (*segmentReader, error) {
	b.readerMtx.Lock()
	defer b.readerMtx.Unlock()
	if b.reader != nil {
		return b.reader, nil
	}
	segments, err := openForRead(blockSegmentsPath(b.dir))
	if err != nil {
		return nil, err
	}
	timestamps, err := openForRead(blockTimestampsPath(b.dir))
	if err != nil {
		segments.Close()
		return nil, err
	}
	b.reader = &segmentReader{segments: segments, timestamps: timestamps}
	return b.reader, nil
}

func openForRead(path string) (*os.File, error) { return os.Open(path) }

func joinBlockFile(dir, name string) string { return filepath.Join(dir, name) }

// blockSegmentsPath returns the payload file of a block directory.
func blockSegmentsPath(dir string) string { return joinBlockFile(dir, segmentsFileName) }

// blockTimestampsPath returns the shared timestamp file of a block directory.
func blockTimestampsPath(dir string) string { return joinBlockFile(dir, timestampsFileName) }

func sortInts(values []int) { slices.Sort(values) }

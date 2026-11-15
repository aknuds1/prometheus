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
	"slices"
	"strconv"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

const walDirName = "wal"

const (
	walSamples uint8 = iota
	walAssociation
	walOwnerVersion
)

var walSchema = arrow.NewSchema([]arrow.Field{
	{Name: "sequence", Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
	{Name: "entry_kind", Type: arrow.PrimitiveTypes.Uint8, Nullable: false},
	{Name: "labels", Type: metadataStringPairsType(), Nullable: false},
	{Name: "timestamps", Type: arrow.ListOf(arrow.PrimitiveTypes.Int64), Nullable: false},
	{Name: "start_timestamps", Type: arrow.ListOf(arrow.PrimitiveTypes.Int64), Nullable: false},
	{Name: "kinds", Type: arrow.ListOf(arrow.PrimitiveTypes.Int8), Nullable: false},
	{Name: "values", Type: arrow.ListOf(arrow.PrimitiveTypes.Float64), Nullable: false},
	{Name: "histograms", Type: arrow.ListOf(arrow.BinaryTypes.Binary), Nullable: false},
	{Name: "owner_kind", Type: arrow.PrimitiveTypes.Uint8, Nullable: false},
	{Name: "owner_key", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "attributes", Type: metadataStringPairsType(), Nullable: false},
	{Name: "effective_from", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
}, nil)

// appendWALBatch durably publishes a completed Arrow IPC segment before the
// batch enters the head. Calls hold Storage.mtx, so sequence and filenames are
// allocated in commit order. Replay and explicitly WAL-disabled mode create none.
//
// Records are appended to an open segment rather than one file per batch: a
// per-batch open/write/fsync/rename dominated WAL cost for small scrape batches.
// Each record is still fsynced before the batch is acknowledged, so durability is
// unchanged; only file creation overhead is removed.
func (s *Storage) appendWALBatch(batch Batch) error {
	if s.opts.DisableWAL || s.replaying || len(batch.Series)+len(batch.Updates)+len(batch.OwnerUpdates) == 0 {
		return nil
	}
	sequence := s.walSequence + 1
	record, err := buildWALRecord(batch, sequence)
	if err != nil {
		return err
	}
	defer record.Release()

	if err := s.ensureWALSegment(sequence); err != nil {
		return fmt.Errorf("opening columnar WAL segment: %w", err)
	}
	if err := s.walSeg.write(record); err != nil {
		return fmt.Errorf("writing columnar WAL record %d: %w", sequence, err)
	}
	s.walSeg.maxSequence = sequence
	s.walSequence = sequence
	if s.walSeg.size >= walSegmentRotateBytes {
		if err := s.rotateWALSegment(); err != nil {
			return fmt.Errorf("rotating columnar WAL segment: %w", err)
		}
	}
	return nil
}

// walClosedSegment is a rotated segment awaiting deletion once a block covers
// every sequence it holds.
type walClosedSegment struct {
	path        string
	maxSequence uint64
}

// walSegmentRotateBytes bounds a segment's size before rotation.
const walSegmentRotateBytes = 4 << 20

// walSegment is one open Arrow IPC stream holding consecutive batch records.
type walSegment struct {
	file        *os.File
	writer      *ipc.Writer
	path        string
	firstSeq    uint64
	maxSequence uint64
	size        int64
}

// countingWriter tracks how much has been written so rotation is size-based
// without knowing a record's encoded size up front.
type countingWriter struct {
	w *os.File
	n *int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	*c.n += int64(n)
	return n, err
}

func (seg *walSegment) write(record arrow.RecordBatch) error {
	if err := seg.writer.Write(record); err != nil {
		return err
	}
	// Durability contract: a batch is acknowledged only after its record is on
	// disk, exactly as with the previous per-batch segment files.
	return seg.file.Sync()
}

// ensureWALSegment opens a segment starting at sequence when none is open.
func (s *Storage) ensureWALSegment(sequence uint64) error {
	if s.walSeg != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(s.dir, walDirName), 0o750); err != nil {
		return err
	}
	path := walSegmentPath(s.dir, sequence)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	seg := &walSegment{file: file, path: path, firstSeq: sequence, maxSequence: sequence}
	seg.writer = ipc.NewWriter(countingWriter{w: file, n: &seg.size})
	s.walSeg = seg
	return nil
}

// rotateWALSegment closes the open segment and remembers its last sequence so
// discardWALThrough can retire it once a block covers it.
func (s *Storage) rotateWALSegment() error {
	if s.walSeg == nil {
		return nil
	}
	seg := s.walSeg
	s.walSeg = nil
	if err := seg.writer.Close(); err != nil {
		return err
	}
	if err := seg.file.Sync(); err != nil {
		return err
	}
	if err := seg.file.Close(); err != nil {
		return err
	}
	s.walClosedSegments = append(s.walClosedSegments, walClosedSegment{path: seg.path, maxSequence: seg.maxSequence})
	return nil
}

func walSegmentPath(dir string, sequence uint64) string {
	return filepath.Join(dir, walDirName, fmt.Sprintf("%020d.arrow", sequence))
}

func buildWALRecord(batch Batch, sequence uint64) (arrow.RecordBatch, error) {
	builder := array.NewRecordBuilder(memory.NewGoAllocator(), walSchema)
	defer builder.Release()
	sequences := builder.Field(0).(*array.Uint64Builder)
	entryKinds := builder.Field(1).(*array.Uint8Builder)
	labelSets := builder.Field(2).(*array.ListBuilder)
	timestamps := builder.Field(3).(*array.ListBuilder)
	startTimestamps := builder.Field(4).(*array.ListBuilder)
	kinds := builder.Field(5).(*array.ListBuilder)
	values := builder.Field(6).(*array.ListBuilder)
	histograms := builder.Field(7).(*array.ListBuilder)
	ownerKinds := builder.Field(8).(*array.Uint8Builder)
	ownerKeys := builder.Field(9).(*array.StringBuilder)
	attributes := builder.Field(10).(*array.ListBuilder)
	effectiveFrom := builder.Field(11).(*array.Int64Builder)

	appendPrefix := func(entryKind uint8, owner OwnerKey, content Content, effective int64) {
		sequences.Append(sequence)
		entryKinds.Append(entryKind)
		ownerKinds.Append(uint8(owner.Kind))
		ownerKeys.Append(owner.ID)
		appendMetadataAttributes(attributes, content.Attributes)
		effectiveFrom.Append(effective)
	}
	for _, series := range batch.Series {
		if len(series.Timestamps) == 0 {
			continue
		}
		appendPrefix(walSamples, OwnerKey{}, Content{}, 0)
		appendMetadataLabels(labelSets, series.Labels)
		timestamps.Append(true)
		timestamps.ValueBuilder().(*array.Int64Builder).AppendValues(series.Timestamps, nil)
		startTimestamps.Append(len(series.StartTimestamps) > 0)
		if len(series.StartTimestamps) > 0 {
			startTimestamps.ValueBuilder().(*array.Int64Builder).AppendValues(series.StartTimestamps, nil)
		}
		kinds.Append(true)
		kindValues := kinds.ValueBuilder().(*array.Int8Builder)
		for _, kind := range series.Kinds {
			kindValues.Append(int8(kind))
		}
		values.Append(true)
		values.ValueBuilder().(*array.Float64Builder).AppendValues(series.Values, nil)
		histograms.Append(len(series.Histograms) > 0)
		if len(series.Histograms) > 0 {
			histogramValues := histograms.ValueBuilder().(*array.BinaryBuilder)
			for _, payload := range series.Histograms {
				histogramValues.Append(payload)
			}
		}
	}
	for _, association := range batch.Updates {
		appendPrefix(walAssociation, association.Update.Owner, association.Update.Content, association.Update.EffectiveFrom)
		appendMetadataLabels(labelSets, association.Labels)
		for _, list := range []int{3, 4, 5, 6, 7} {
			builder.Field(list).(*array.ListBuilder).Append(true)
		}
	}
	for _, update := range batch.OwnerUpdates {
		appendPrefix(walOwnerVersion, update.Owner, update.Content, update.EffectiveFrom)
		labelSets.Append(true)
		for _, list := range []int{3, 4, 5, 6, 7} {
			builder.Field(list).(*array.ListBuilder).Append(true)
		}
	}
	return builder.NewRecordBatch(), nil
}

// replayWAL replays valid completed Arrow IPC segments in sequence order.
// Block.WALSequence skips a segment already covered by a published block, even
// if crash recovery left that segment behind before its deletion.
func (s *Storage) replayWAL(ctx context.Context) error {
	dir := filepath.Join(s.dir, walDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	type segment struct {
		sequence uint64
		path     string
	}
	var segments []segment
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".arrow") {
			continue
		}
		sequence, err := strconv.ParseUint(strings.TrimSuffix(entry.Name(), ".arrow"), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid columnar WAL segment name %q: %w", entry.Name(), err)
		}
		segments = append(segments, segment{sequence: sequence, path: filepath.Join(dir, entry.Name())})
	}
	slices.SortFunc(segments, func(a, b segment) int {
		switch {
		case a.sequence < b.sequence:
			return -1
		case a.sequence > b.sequence:
			return 1
		default:
			return 0
		}
	})
	publishedThrough := s.walSequence
	for _, segment := range segments {
		batches, sequences, err := readWALRecords(segment.path)
		if err != nil {
			return fmt.Errorf("replaying columnar WAL segment %d: %w", segment.sequence, err)
		}
		var maxSequence uint64
		for i := range batches {
			sequence := sequences[i]
			maxSequence = max(maxSequence, sequence)
			if sequence <= publishedThrough {
				// Already durable in a published block; replaying it would look
				// like a duplicate append.
				continue
			}
			// Auto-flush during replay uses this sequence for its manifest.
			// appendWALBatch itself remains disabled while replaying: the source
			// segment is the log.
			s.walSequence = sequence
			if err := s.AppendBatch(ctx, batches[i]); err != nil {
				return fmt.Errorf("replaying columnar WAL record %d: %w", sequence, err)
			}
		}
		if maxSequence > 0 {
			s.walClosedSegments = append(s.walClosedSegments, walClosedSegment{path: segment.path, maxSequence: maxSequence})
			s.walSequence = max(s.walSequence, maxSequence)
		}
	}
	return nil
}

// discardWALThrough removes only records a published block already covers. The
// open segment is rotated first so every remaining segment has a known maximum
// sequence; a segment that straddles the boundary is retained whole.
func (s *Storage) discardWALThrough(sequence uint64) error {
	if s.opts.DisableWAL {
		return nil
	}
	if err := s.rotateWALSegment(); err != nil {
		return err
	}
	kept := s.walClosedSegments[:0]
	for _, seg := range s.walClosedSegments {
		if seg.maxSequence > sequence {
			kept = append(kept, seg)
			continue
		}
		if err := os.Remove(seg.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	s.walClosedSegments = kept
	return nil
}

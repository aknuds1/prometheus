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

package wlog

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/fileutil"
	"github.com/prometheus/prometheus/tsdb/nativemetadata"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/tsdb/tombstones"
	"github.com/prometheus/prometheus/tsdb/tsdbutil"
)

// CheckpointStats returns stats about a created checkpoint.
type CheckpointStats struct {
	DroppedSeries     int
	DroppedSamples    int // Includes histograms.
	DroppedTombstones int
	DroppedExemplars  int
	DroppedMetadata   int
	TotalSeries       int // Processed series including dropped ones.
	TotalSamples      int // Processed float and histogram samples including dropped ones.
	TotalTombstones   int // Processed tombstones including dropped ones.
	TotalExemplars    int // Processed exemplars including dropped ones.
	TotalMetadata     int // Processed metadata including dropped ones.
	// UnknownMetadata counts kept native metadata entries of unknown kind,
	// which were applied as single-point overrides.
	UnknownMetadata int
}

// LastCheckpoint returns the directory name and index of the most recent checkpoint.
// If dir does not contain any checkpoints, ErrNotFound is returned.
func LastCheckpoint(dir string) (string, int, error) {
	checkpoints, err := listCheckpoints(dir)
	if err != nil {
		return "", 0, err
	}

	if len(checkpoints) == 0 {
		return "", 0, record.ErrNotFound
	}

	checkpoint := checkpoints[len(checkpoints)-1]
	return filepath.Join(dir, checkpoint.name), checkpoint.index, nil
}

// DeleteCheckpoints deletes all checkpoints in a directory below a given index.
func DeleteCheckpoints(dir string, maxIndex int) error {
	checkpoints, err := listCheckpoints(dir)
	if err != nil {
		return err
	}

	var errs []error
	for _, checkpoint := range checkpoints {
		if checkpoint.index >= maxIndex {
			break
		}
		errs = append(errs, os.RemoveAll(filepath.Join(dir, checkpoint.name)))
	}
	return errors.Join(errs...)
}

// CheckpointTempFileSuffix is the suffix used when creating temporary checkpoint files.
const CheckpointTempFileSuffix = ".tmp"

// DeleteTempCheckpoints deletes all temporary checkpoint directories in the given directory.
func DeleteTempCheckpoints(logger *slog.Logger, dir string) error {
	if err := tsdbutil.RemoveTmpDirs(logger, dir, isTempDir); err != nil {
		return fmt.Errorf("remove previous temporary checkpoint dirs: %w", err)
	}
	return nil
}

const (
	// checkpointMetadataBatch bounds the entries in one checkpoint metadata record.
	checkpointMetadataBatch = 4096
	// checkpointMetadataInternerLimit bounds the distinct values a checkpoint shares.
	checkpointMetadataInternerLimit = 1 << 16
)

// checkpointMetadata is a series' metadata reduced from WAL entries. Series
// that only had legacy entries keep a legacy entry with the latest value.
type checkpointMetadata struct {
	nativemetadata.State
	native bool
}

// Checkpoint creates a compacted checkpoint of segments in range [from, to] in the given WAL.
// It includes the most recent checkpoint if it exists.
// All series not satisfying keep, samples/exemplars below mint, and tombstones not
// satisfying keep or with all intervals below mint are dropped. Metadata entries
// are reduced per kept series: legacy entries to the latest one, and native
// entries to an override carrying the reduced history and truncation flag.
//
// keep is evaluated per record as segments are read, so its result for a given ref
// must not change while Checkpoint runs. Otherwise records for the same ref could be
// treated inconsistently, e.g. a series record kept but its tombstone dropped. The
// Head satisfies this by serializing checkpointing with every series-deleting path
// (GC and series truncation) via chunkSnapshotMtx.
//
// The checkpoint is stored in a directory named checkpoint.N in the same
// segmented format as the original WAL itself.
// This makes it easy to read it through the WAL package and concatenate
// it with the original WAL.
func Checkpoint(logger *slog.Logger, w *WL, from, to int, keep func(id chunks.HeadSeriesRef) bool, mint int64, enableSTStorage bool) (*CheckpointStats, error) {
	stats := &CheckpointStats{}
	var sgmReader io.ReadCloser

	logger.Info("Creating checkpoint", "from_segment", from, "to_segment", to, "mint", mint)

	{
		var sgmRange []SegmentRange
		dir, idx, err := LastCheckpoint(w.Dir())
		if err != nil && !errors.Is(err, record.ErrNotFound) {
			return nil, fmt.Errorf("find last checkpoint: %w", err)
		}
		last := idx + 1
		if err == nil {
			if from > last {
				return nil, fmt.Errorf("unexpected gap to last checkpoint. expected:%v, requested:%v", last, from)
			}
			// Ignore WAL files below the checkpoint. They shouldn't exist to begin with.
			from = last

			sgmRange = append(sgmRange, SegmentRange{Dir: dir, Last: math.MaxInt32})
		}

		sgmRange = append(sgmRange, SegmentRange{Dir: w.Dir(), First: from, Last: to})
		sgmReader, err = NewSegmentsRangeReader(sgmRange...)
		if err != nil {
			return nil, fmt.Errorf("create segment reader: %w", err)
		}
		defer sgmReader.Close()
	}

	if err := DeleteTempCheckpoints(logger, w.Dir()); err != nil {
		return nil, err
	}

	cpdir := CheckpointDir(w.Dir(), to)
	cpdirtmp := cpdir + CheckpointTempFileSuffix

	if err := os.MkdirAll(cpdirtmp, 0o777); err != nil {
		return nil, fmt.Errorf("create checkpoint dir: %w", err)
	}
	cp, err := New(nil, nil, cpdirtmp, w.CompressionType())
	if err != nil {
		return nil, fmt.Errorf("open checkpoint: %w", err)
	}

	// Ensures that an early return caused by an error doesn't leave any tmp files.
	defer func() {
		cp.Close()
		os.RemoveAll(cpdirtmp)
	}()

	r := NewReader(sgmReader)

	var (
		series                []record.RefSeries
		samples               []record.RefSample
		histogramSamples      []record.RefHistogramSample
		floatHistogramSamples []record.RefFloatHistogramSample
		tstones               []tombstones.Stone
		exemplars             []record.RefExemplar
		metadata              []record.RefNativeMetadata
		metadataPoints        []record.RefNativeMetadataPoint
		metadataValues        []nativemetadata.Point
		st                    = labels.NewSymbolTable() // Needed for decoding; labels do not outlive this function.
		dec                   = record.NewDecoder(st, logger)
		enc                   = record.Encoder{EnableSTStorage: enableSTStorage}
		buf                   []byte
		recs                  [][]byte

		reducedMetadata = make(map[chunks.HeadSeriesRef]checkpointMetadata)
		interner        = nativemetadata.NewInterner(checkpointMetadataInternerLimit)
		compact         record.CompactNativeMetadata
		dictionary      nativemetadata.Dictionary
	)
	for r.Next() {
		series, samples, histogramSamples, floatHistogramSamples, tstones, exemplars, metadata = series[:0], samples[:0], histogramSamples[:0], floatHistogramSamples[:0], tstones[:0], exemplars[:0], metadata[:0]

		// We don't reset the buffer since we batch up multiple records
		// before writing them to the checkpoint.
		// Remember where the record for this iteration starts.
		start := len(buf)
		rec := r.Record()

		switch dec.Type(rec) {
		case record.Series:
			series, err = dec.Series(rec, series)
			if err != nil {
				return nil, fmt.Errorf("decode series: %w", err)
			}
			// Drop irrelevant series in place.
			repl := series[:0]
			for _, s := range series {
				if keep(s.Ref) {
					repl = append(repl, s)
				}
			}
			if len(repl) > 0 {
				buf = enc.Series(repl, buf)
			}
			stats.TotalSeries += len(series)
			stats.DroppedSeries += len(series) - len(repl)

		case record.Samples, record.SamplesV2:
			samples, err = dec.Samples(rec, samples)
			if err != nil {
				return nil, fmt.Errorf("decode samples: %w", err)
			}
			// Drop irrelevant samples in place.
			repl := samples[:0]
			for _, s := range samples {
				if s.T >= mint {
					repl = append(repl, s)
				}
			}
			if len(repl) > 0 {
				buf = enc.Samples(repl, buf)
			}
			stats.TotalSamples += len(samples)
			stats.DroppedSamples += len(samples) - len(repl)

		case record.HistogramSamples, record.HistogramSamplesV2:
			histogramSamples, err = dec.HistogramSamples(rec, histogramSamples)
			if err != nil {
				return nil, fmt.Errorf("decode histogram samples: %w", err)
			}
			// Drop irrelevant histogramSamples in place.
			repl := histogramSamples[:0]
			for _, h := range histogramSamples {
				if h.T >= mint {
					repl = append(repl, h)
				}
			}
			if len(repl) > 0 {
				var leftover []record.RefHistogramSample
				buf, leftover = enc.HistogramSamples(repl, buf)
				if len(leftover) > 0 {
					// Flush the exponential-histogram record before
					// appending the custom-bucket record so they are
					// written as two separate WAL records.
					if expEnd := len(buf); expEnd > start {
						recs = append(recs, buf[start:expEnd])
						start = expEnd
					}
					buf = enc.CustomBucketsHistogramSamples(leftover, buf)
				}
			}
			stats.TotalSamples += len(histogramSamples)
			stats.DroppedSamples += len(histogramSamples) - len(repl)
		case record.CustomBucketsHistogramSamples:
			histogramSamples, err = dec.HistogramSamples(rec, histogramSamples)
			if err != nil {
				return nil, fmt.Errorf("decode histogram samples: %w", err)
			}
			// Drop irrelevant histogramSamples in place.
			repl := histogramSamples[:0]
			for _, h := range histogramSamples {
				if h.T >= mint {
					repl = append(repl, h)
				}
			}
			if len(repl) > 0 {
				buf = enc.CustomBucketsHistogramSamples(repl, buf)
			}
			stats.TotalSamples += len(histogramSamples)
			stats.DroppedSamples += len(histogramSamples) - len(repl)
		case record.FloatHistogramSamples, record.FloatHistogramSamplesV2:
			floatHistogramSamples, err = dec.FloatHistogramSamples(rec, floatHistogramSamples)
			if err != nil {
				return nil, fmt.Errorf("decode float histogram samples: %w", err)
			}
			// Drop irrelevant floatHistogramSamples in place.
			repl := floatHistogramSamples[:0]
			for _, fh := range floatHistogramSamples {
				if fh.T >= mint {
					repl = append(repl, fh)
				}
			}
			if len(repl) > 0 {
				var floatLeftover []record.RefFloatHistogramSample
				buf, floatLeftover = enc.FloatHistogramSamples(repl, buf)
				if len(floatLeftover) > 0 {
					// Flush the exponential-float-histogram record before
					// appending the custom-bucket record so they are
					// written as two separate WAL records.
					if expEnd := len(buf); expEnd > start {
						recs = append(recs, buf[start:expEnd])
						start = expEnd
					}
					buf = enc.CustomBucketsFloatHistogramSamples(floatLeftover, buf)
				}
			}
			stats.TotalSamples += len(floatHistogramSamples)
			stats.DroppedSamples += len(floatHistogramSamples) - len(repl)
		case record.CustomBucketsFloatHistogramSamples:
			floatHistogramSamples, err = dec.FloatHistogramSamples(rec, floatHistogramSamples)
			if err != nil {
				return nil, fmt.Errorf("decode float histogram samples: %w", err)
			}
			// Drop irrelevant floatHistogramSamples in place.
			repl := floatHistogramSamples[:0]
			for _, fh := range floatHistogramSamples {
				if fh.T >= mint {
					repl = append(repl, fh)
				}
			}
			if len(repl) > 0 {
				buf = enc.CustomBucketsFloatHistogramSamples(repl, buf)
			}
			stats.TotalSamples += len(floatHistogramSamples)
			stats.DroppedSamples += len(floatHistogramSamples) - len(repl)
		case record.Tombstones:
			tstones, err = dec.Tombstones(rec, tstones)
			if err != nil {
				return nil, fmt.Errorf("decode deletes: %w", err)
			}
			// Drop irrelevant tombstones in place. A tombstone is dropped together with
			// its series record, or once all its intervals age out of the WAL.
			repl := tstones[:0]
			for _, s := range tstones {
				if !keep(chunks.HeadSeriesRef(s.Ref)) {
					continue
				}
				for _, iv := range s.Intervals {
					if iv.Maxt >= mint {
						repl = append(repl, s)
						break
					}
				}
			}
			if len(repl) > 0 {
				buf = enc.Tombstones(repl, buf)
			}
			stats.TotalTombstones += len(tstones)
			stats.DroppedTombstones += len(tstones) - len(repl)

		case record.Exemplars:
			exemplars, err = dec.Exemplars(rec, exemplars)
			if err != nil {
				return nil, fmt.Errorf("decode exemplars: %w", err)
			}
			// Drop irrelevant exemplars in place.
			repl := exemplars[:0]
			for _, e := range exemplars {
				if e.T >= mint {
					repl = append(repl, e)
				}
			}
			if len(repl) > 0 {
				buf = enc.Exemplars(repl, buf)
			}
			stats.TotalExemplars += len(exemplars)
			stats.DroppedExemplars += len(exemplars) - len(repl)
		case record.Metadata:
			metadata, metadataPoints, err = dec.NativeMetadata(rec, metadata, metadataPoints[:0])
			if err != nil {
				return nil, fmt.Errorf("decode metadata: %w", err)
			}
			// Keep one reduced entry per series, in WAL order.
			repl := 0
			for _, m := range metadata {
				if !keep(m.Ref) {
					continue
				}
				reduced, ok := reducedMetadata[m.Ref]
				if !ok {
					repl++
				}
				metadataValues = nativemetadata.AppendRecordPoints(metadataValues[:0], m.Points, interner.Intern)
				if reduced.Apply(m.Kind, m.Truncated, metadataValues) {
					stats.UnknownMetadata++
				}
				reduced.native = reduced.native || m.Kind != record.NativeMetadataLegacy
				reducedMetadata[m.Ref] = reduced
			}
			clear(metadataValues)
			stats.TotalMetadata += len(metadata)
			stats.DroppedMetadata += len(metadata) - repl
		case record.NativeMetadataCompact:
			if err := dec.CompactNativeMetadata(rec, &compact); err != nil {
				return nil, fmt.Errorf("decode compact native metadata: %w", err)
			}
			// Reduce as above, resolving each used value once. Entries that
			// leave state unchanged count as unknown.
			dictionary.Reset(compact.Values, interner.Intern)
			repl := 0
			for _, m := range compact.Entries {
				if !keep(m.Ref) {
					continue
				}
				if m.Ignored() {
					stats.UnknownMetadata++
					continue
				}
				reduced, ok := reducedMetadata[m.Ref]
				if !ok {
					repl++
				}
				metadataValues = dictionary.AppendPoints(metadataValues[:0], m.Points)
				if reduced.Apply(m.Kind, m.Truncated, metadataValues) {
					stats.UnknownMetadata++
				}
				reduced.native = true
				reducedMetadata[m.Ref] = reduced
			}
			clear(metadataValues)
			stats.TotalMetadata += len(compact.Entries)
			stats.DroppedMetadata += len(compact.Entries) - repl
			dictionary.Reset(nil, nil)
			compact.Reset()
		default:
			// Unknown record type, probably from a future Prometheus version.
			continue
		}
		if len(buf[start:]) == 0 {
			continue // All contents discarded.
		}
		recs = append(recs, buf[start:])

		// Flush records in 1 MB increments.
		if len(buf) > 1*1024*1024 {
			if err := cp.Log(recs...); err != nil {
				return nil, fmt.Errorf("flush records: %w", err)
			}
			buf, recs = buf[:0], recs[:0]
		}
	}
	// If we hit any corruption during checkpointing, repairing is not an option.
	// The head won't know which series records are lost.
	if r.Err() != nil {
		return nil, fmt.Errorf("read segments: %w", r.Err())
	}

	// Flush remaining records.
	if err := cp.Log(recs...); err != nil {
		return nil, fmt.Errorf("flush records: %w", err)
	}

	// Flush the reduced metadata of each series, in bounded records: native
	// histories as overrides in compact records, whose dictionaries hold each
	// distinct value once, and legacy values as legacy entries.
	metadata, metadataPoints = metadata[:0], metadataPoints[:0]
	var (
		overrides      []record.RefCompactNativeMetadata
		overridePoints []record.CompactNativeMetadataPoint
		values         []record.NativeMetadataValue
		valueIndices   = map[record.NativeMetadataValue]uint32{}
	)
	flushOverrides := func() error {
		buf = enc.CompactNativeMetadata(values, overrides, buf[:0])
		if err := cp.Log(buf); err != nil {
			return fmt.Errorf("flush metadata records: %w", err)
		}
		clear(overrides)
		clear(values)
		clear(valueIndices)
		overrides, overridePoints, values = overrides[:0], overridePoints[:0], values[:0]
		return nil
	}
	for ref, reduced := range reducedMetadata {
		if reduced.native {
			entry := record.RefCompactNativeMetadata{Ref: ref, Kind: record.NativeMetadataOverride, Truncated: reduced.Truncated}
			start := len(overridePoints)
			for _, p := range reduced.AppendPoints(metadataValues[:0]) {
				v := record.NativeMetadataValue{Type: record.GetMetricType(p.Metadata.Type), Unit: p.Metadata.Unit, Help: p.Metadata.Help}
				index, ok := valueIndices[v]
				if !ok {
					index = uint32(len(values))
					valueIndices[v] = index
					values = append(values, v)
				}
				overridePoints = append(overridePoints, record.CompactNativeMetadataPoint{EffectiveFrom: p.EffectiveFrom, Value: index})
			}
			entry.Points = overridePoints[start:len(overridePoints):len(overridePoints)]
			overrides = append(overrides, entry)
			if len(overrides) == checkpointMetadataBatch {
				if err := flushOverrides(); err != nil {
					return nil, err
				}
			}
			continue
		}
		entry := record.RefNativeMetadata{Ref: ref, Kind: record.NativeMetadataLegacy}
		start := len(metadataPoints)
		if reduced.Metadata != nil {
			metadataPoints = nativemetadata.AppendRecordPoint(metadataPoints, nativemetadata.Point{Metadata: reduced.Metadata})
		}
		entry.Points = metadataPoints[start:len(metadataPoints):len(metadataPoints)]
		metadata = append(metadata, entry)
		if len(metadata) == checkpointMetadataBatch {
			if err := cp.Log(enc.NativeMetadata(metadata, buf[:0])); err != nil {
				return nil, fmt.Errorf("flush metadata records: %w", err)
			}
			clear(metadata)
			clear(metadataPoints)
			metadata, metadataPoints = metadata[:0], metadataPoints[:0]
		}
	}
	if len(metadata) > 0 {
		if err := cp.Log(enc.NativeMetadata(metadata, buf[:0])); err != nil {
			return nil, fmt.Errorf("flush metadata records: %w", err)
		}
	}
	if len(overrides) > 0 {
		if err := flushOverrides(); err != nil {
			return nil, err
		}
	}

	if err := cp.Close(); err != nil {
		return nil, fmt.Errorf("close checkpoint: %w", err)
	}

	// Sync temporary directory before rename.
	df, err := fileutil.OpenDir(cpdirtmp)
	if err != nil {
		return nil, fmt.Errorf("open temporary checkpoint directory: %w", err)
	}
	if err := df.Sync(); err != nil {
		df.Close()
		return nil, fmt.Errorf("sync temporary checkpoint directory: %w", err)
	}
	if err = df.Close(); err != nil {
		return nil, fmt.Errorf("close temporary checkpoint directory: %w", err)
	}

	if err := fileutil.Replace(cpdirtmp, cpdir); err != nil {
		return nil, fmt.Errorf("rename checkpoint directory: %w", err)
	}

	return stats, nil
}

// checkpointPrefix is the prefix used for checkpoint files.
const checkpointPrefix = "checkpoint."

func CheckpointDir(dir string, i int) string {
	return filepath.Join(dir, fmt.Sprintf(checkpointPrefix+"%08d", i))
}

type checkpointRef struct {
	name  string
	index int
}

func listCheckpoints(dir string) (refs []checkpointRef, err error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for i := range files {
		fi := files[i]
		if !strings.HasPrefix(fi.Name(), checkpointPrefix) {
			continue
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("checkpoint %s is not a directory", fi.Name())
		}
		idx, err := strconv.Atoi(fi.Name()[len(checkpointPrefix):])
		if err != nil {
			continue
		}

		refs = append(refs, checkpointRef{name: fi.Name(), index: idx})
	}

	slices.SortFunc(refs, func(a, b checkpointRef) int {
		return a.index - b.index
	})

	return refs, nil
}

func isTempDir(fi fs.DirEntry) bool {
	return strings.HasPrefix(fi.Name(), checkpointPrefix) && strings.HasSuffix(fi.Name(), CheckpointTempFileSuffix)
}

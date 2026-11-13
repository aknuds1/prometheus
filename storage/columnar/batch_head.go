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
	"errors"
	"fmt"
	"slices"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
)

// newHeadSeriesAt builds a prepared series from a hash that is already known:
// either supplied by the appender or computed while indexing the batch.
func newHeadSeriesAt(hash uint64, ls labels.Labels) *headSeries {
	return &headSeries{hash: hash, labels: ls}
}

type indexedBatchSeries struct {
	index int
	hash  uint64
}

// prepareBatchSeries validates all timestamp ranges, including boundaries
// between repeated chunks of one series, and produces stable-hash-sorted typed
// transaction arrays. Its stable sort preserves the caller's sample order for a
// series while allowing unrelated series to arrive in any order.
func prepareBatchSeries(batch Batch) ([]*headSeries, ingestCounts, error) {
	var counts ingestCounts
	indexed := make([]indexedBatchSeries, 0, len(batch.Series))
	for i, series := range batch.Series {
		if err := validateSeriesColumns(series); err != nil {
			return nil, counts, fmt.Errorf("series chunk %d: %w", i, err)
		}
		if len(series.Timestamps) > 0 {
			hash := series.hash
			if !series.hashValid {
				counts.prepareHash++
				hash = stableSeriesHash(series.Labels)
			}
			indexed = append(indexed, indexedBatchSeries{index: i, hash: hash})
		}
	}
	slices.SortStableFunc(indexed, func(a, b indexedBatchSeries) int {
		switch {
		case a.hash < b.hash:
			return -1
		case a.hash > b.hash:
			return 1
		default:
			return 0
		}
	})

	var prepared []*headSeries
	for start := 0; start < len(indexed); {
		end := start + 1
		for end < len(indexed) && indexed[end].hash == indexed[start].hash {
			end++
		}
		first := batch.Series[indexed[start].index]
		// The indexed hash is the series' hash, so the prepared series reuses it
		// instead of hashing the same labels a second time.
		merged := newHeadSeriesAt(indexed[start].hash, first.Labels)
		for i := start; i < end; i++ {
			series := batch.Series[indexed[i].index]
			if !labels.Equal(merged.labels, series.Labels) {
				return nil, counts, fmt.Errorf("%w: %q and %q", ErrSeriesHashCollision, merged.labels, series.Labels)
			}
			if merged.Len() > 0 {
				previous := merged.ts[len(merged.ts)-1]
				next := series.Timestamps[0]
				if next <= previous {
					return nil, counts, batchTimestampError(previous, next)
				}
			}
			appendTypedSeries(merged, series)
		}
		prepared = append(prepared, merged)
		start = end
	}
	return prepared, counts, nil
}

func batchTimestampError(previous, next int64) error {
	if next == previous {
		return fmt.Errorf("%w: timestamp %d", storage.ErrDuplicateSampleForTimestamp, next)
	}
	return fmt.Errorf("%w: timestamp %d follows %d", storage.ErrOutOfOrderSample, next, previous)
}

// headSeries.Len reports its number of buffered samples.
func (h *headSeries) Len() int { return len(h.ts) }

// batchValidateAgainstHead checks both persistent block tails and the sorted
// head without mutating either. The complete batch is validated before its WAL
// segment is published.
func (s *Storage) batchValidateAgainstHead(series *headSeries) error {
	if series.Len() == 0 {
		return nil
	}
	if series.ts[0] < s.minValidTime {
		return fmt.Errorf("%w: series %s first buffer timestamp %d precedes retention cutoff %d", storage.ErrOutOfBounds, series.labels, series.ts[0], s.minValidTime)
	}
	last, ok := s.last[series.hash]
	if ok {
		if !labels.Equal(last.labels, series.labels) {
			return ErrSeriesHashCollision
		}
		if series.ts[0] <= last.t {
			return fmt.Errorf("%w: series %s first buffer timestamp %d not after %d",
				headTimestampError(last.t, series.ts[0]), series.labels, series.ts[0], last.t)
		}
	}
	return nil
}

func headTimestampError(previous, next int64) error {
	if next == previous {
		return storage.ErrDuplicateSampleForTimestamp
	}
	return storage.ErrOutOfOrderSample
}

// findHead returns the entry at hash from the sorted array. Complete labels are
// checked by the caller wherever the ID might have come from outside storage.
func (s *Storage) findHead(hash uint64) (*headSeries, bool) {
	hs, ok := s.buffer[hash]
	return hs, ok
}

// commitHead appends fully prepared arrays and updates tail state once after
// metadata validation and durable WAL publication. The owner-related metadata
// transaction and these samples therefore become visible as one batch.
func (s *Storage) commitHead(prepared []*headSeries) error {
	for _, state := range prepared {
		hs, ok := s.findHead(state.hash)
		if !ok {
			// Prepared arrays already own their storage, so a new series can enter
			// the head directly without an intermediate duplicate copy.
			hs = state
			s.buffer[state.hash] = hs
			name := hs.labels.Get(labels.MetricName)
			s.headByName[name] = append(s.headByName[name], hs)
		} else if !labels.Equal(hs.labels, state.labels) {
			return ErrSeriesHashCollision
		}

		if hs != state {
			appendTypedSeries(hs, BatchSeries{
				Labels: state.labels, Timestamps: state.ts, StartTimestamps: state.st,
				Kinds: state.kind, Values: state.v, Histograms: state.hist,
			})
		}
		s.bufferN += state.Len()
		if state.Len() == 0 {
			continue
		}
		last := &seriesState{
			labels: state.labels, t: state.ts[len(state.ts)-1], count: state.Len(),
		}
		if len(state.st) > 0 {
			last.st = state.st[len(state.st)-1]
		}
		if old, ok := s.last[state.hash]; ok {
			last.count += old.count
		}
		switch state.kind[len(state.kind)-1] {
		case SampleHistogram, SampleFloatHistogram:
			h, fh, err := decodeSampleHistogram(state.hist[len(state.hist)-1])
			if err != nil {
				return err
			}
			last.h, last.fh = h, fh
		case SampleFloat, SampleStale:
		default:
			return errors.New("columnar: prepared head has an invalid sample kind")
		}
		s.last[state.hash] = last
		s.bufferMin = min(s.bufferMin, state.ts[0])
		s.bufferMax = max(s.bufferMax, state.ts[len(state.ts)-1])
	}
	if s.bufferN >= s.opts.FlushSamples {
		return s.flushLocked()
	}
	return nil
}

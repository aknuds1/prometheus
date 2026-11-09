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

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
)

func newHeadSeries(ls labels.Labels) *headSeries {
	return &headSeries{labels: ls}
}

// batchValidateAgainstHead commits no samples until all timestamps and compressed
// identities have been verified against the current state.
func (s *Storage) batchValidateAgainstHead(series BatchSeries) error {
	if last, ok := s.last[stableSeriesHash(series.Labels)]; ok && len(series.Timestamps) > 0 {
		if !labels.Equal(last.labels, series.Labels) {
			return ErrSeriesHashCollision
		}
		if series.Timestamps[0] <= last.t {
			return fmt.Errorf("%w: series %s first buffer timestamp %d not after %d",
				storage.ErrOutOfOrderSample, series.Labels, series.Timestamps[0], last.t)
		}
	}
	return nil
}

// commitHead appends typed columns and updates the series dictionary/state once
// after a batch has been fully validated and Associated updates have occurred.
//
// The caller must hold Storage.mtx.
func (s *Storage) commitHead(series map[uint64]*headSeries, order []uint64) error {
	for _, hash := range order {
		hs, ok := s.buffer[hash]
		if !ok {
			hs = newHeadSeries(series[hash].labels)
			s.buffer[hash] = hs
		} else if !labels.Equal(hs.labels, series[hash].labels) {
			return ErrSeriesHashCollision
		}
		state := series[hash]
		hs.ts = append(hs.ts, state.ts...)
		hs.st = append(hs.st, state.st...)
		hs.v = append(hs.v, state.v...)
		hs.kind = append(hs.kind, state.kind...)
		hs.hist = append(hs.hist, state.hist...)

		s.bufferN += len(state.ts)
		if len(state.ts) > 0 {
			last := &seriesState{
				labels: state.labels, t: state.ts[len(state.ts)-1],
				st: state.st[len(state.st)-1], count: len(state.ts),
			}
			if old, ok := s.last[hash]; ok {
				last.count += old.count
			}
			if kind := state.kind[len(state.kind)-1]; kind != SampleFloat && kind != SampleStale {
				h, fh, err := decodeSampleHistogram(state.hist[len(state.hist)-1])
				if err != nil {
					return err
				}
				last.h, last.fh = h, fh
			}
			s.last[hash] = last
			s.bufferMin = min(s.bufferMin, state.ts[0])
			s.bufferMax = max(s.bufferMax, state.ts[len(state.ts)-1])
		}
	}
	if s.bufferN >= s.opts.FlushSamples {
		return s.flushLocked()
	}
	return nil
}

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

	"github.com/prometheus/prometheus/model/labels"
)

// restoreSeriesTail reconstructs the state needed to order the next append and
// infer histogram reset hints. It does not move immutable block samples into the
// head or duplicate them into a second physical representation.
func (s *Storage) restoreSeriesTail(block *Block) error {
	if block.meta.NumSamples == 0 {
		return nil
	}
	series, err := block.Series(context.Background())
	if err != nil {
		return err
	}
	for _, item := range series {
		if item.Len() == 0 {
			continue
		}
		hash := stableSeriesHash(item.Labels)
		lastSample := item.Len() - 1
		state := &seriesState{
			labels: item.Labels.Copy(), t: item.Timestamps[lastSample],
			count: item.Len(),
		}
		if len(item.StartTimestamps) > 0 {
			state.st = item.StartTimestamps[lastSample]
		}
		if kind := item.Kinds[lastSample]; kind == SampleHistogram || kind == SampleFloatHistogram {
			state.h, state.fh, err = decodeSampleHistogram(item.Histograms[lastSample])
			if err != nil {
				return fmt.Errorf("decoding last histogram of %s: %w", item.Labels, err)
			}
		}
		if previous, exists := s.last[hash]; exists {
			if !labels.Equal(previous.labels, item.Labels) {
				return fmt.Errorf("%w: %q and %q", ErrSeriesHashCollision, previous.labels, item.Labels)
			}
			state.count += previous.count
			if previous.t > state.t {
				state.t, state.st = previous.t, previous.st
				state.h, state.fh = previous.h, previous.fh
			}
		}
		s.last[hash] = state
	}
	return nil
}

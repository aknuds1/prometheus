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
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
)

// A scrape round of a few thousand targets must not flush a block per round.
// With a flush threshold below the samples-per-scrape, every scrape would emit
// its own block (observed: 2,592 blocks totalling 1.4 GB for a 15-minute run),
// forcing every query to open all of them.
func TestDefaultFlushThresholdExceedsOneScrape(t *testing.T) {
	const targets = 2500
	require.Greater(t, DefaultFlushSamples, targets,
		"default flush threshold must exceed a scrape round's sample count")

	s, err := NewStorage(t.TempDir(), StorageOptions{DisableWAL: true, DisableNativeMetadata: true})
	require.NoError(t, err)
	defer s.Close()

	// One scrape round.
	batch := Batch{}
	for i := range targets {
		batch.Series = append(batch.Series, BatchSeries{
			Labels:     labels.FromStrings(labels.MetricName, "up", "instance", strconv.Itoa(i)),
			Timestamps: []int64{1000}, StartTimestamps: []int64{0},
			Kinds: []SampleKind{SampleFloat}, Values: []float64{1},
		})
	}
	require.NoError(t, s.AppendBatch(t.Context(), batch))
	require.Zero(t, s.NumBlocks(), "one scrape round must stay buffered")

	// Several more rounds still fit before the threshold is reached.
	for round := 2; round <= 5; round++ {
		for i := range batch.Series {
			batch.Series[i].Timestamps = []int64{int64(round) * 5000}
		}
		require.NoError(t, s.AppendBatch(t.Context(), batch))
	}
	require.Zero(t, s.NumBlocks(), "five scrape rounds must not flush yet")

	require.NoError(t, s.Flush())
	require.Equal(t, 1, s.NumBlocks())
	got, err := s.SelectSeries(t.Context(), 0, 30000, nil, 0)
	require.NoError(t, err)
	require.Len(t, got, targets)
	for _, ser := range got {
		require.Equal(t, 5, ser.Len(), "series %s", ser.Labels)
	}
}

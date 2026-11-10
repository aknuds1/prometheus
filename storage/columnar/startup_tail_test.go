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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
)

func TestRestartReadsOnlyTailPagePerSeries(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStorage(dir, StorageOptions{DisableWAL: true, DisableNativeMetadata: true, FlushSamples: 100_000})
	require.NoError(t, err)
	ls := labels.FromStrings(labels.MetricName, "tail_recovery")
	timestamps := make([]int64, DefaultSamplesPerSegment*2+1)
	sample := BatchSeries{Labels: ls, Timestamps: timestamps, Kinds: make([]SampleKind, len(timestamps)), Values: make([]float64, len(timestamps))}
	for i := range timestamps {
		timestamps[i] = int64(i * 1000)
		sample.Values[i] = float64(i)
	}
	require.NoError(t, s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{sample}}))
	require.NoError(t, s.Flush())
	require.NoError(t, s.Close())

	reopened, err := NewStorage(dir, StorageOptions{DisableWAL: true, DisableNativeMetadata: true, FlushSamples: 100_000})
	require.NoError(t, err)
	defer reopened.Close()
	stats := reopened.ReadStats()
	require.Equal(t, uint64(1), stats.SampleRowsDecoded, "one final segment is read for the one series")
	require.Equal(t, uint64(1), stats.RowGroupsRead)

	// The recovered tail still rejects the immediately preceding duplicate.
	err = reopened.AppendBatch(t.Context(), Batch{Series: []BatchSeries{batchFloatChunk(ls, timestamps[len(timestamps)-1])}})
	require.ErrorIs(t, err, storage.ErrDuplicateSampleForTimestamp)

	selected, err := reopened.SelectSeries(t.Context(), timestamps[0], timestamps[len(timestamps)-1], nil, 0)
	require.NoError(t, err)
	require.Len(t, selected, 1)
	require.Equal(t, timestamps, selected[0].Timestamps)
}

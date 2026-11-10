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
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
)

func appendResourceFloat(t *testing.T, s *Storage, ls labels.Labels, owner OwnerKey, timestamp int64, content Content, value float64) {
	t.Helper()
	require.NoError(t, s.AppendBatch(t.Context(), Batch{
		Series: []BatchSeries{{
			Labels: ls, Timestamps: []int64{timestamp}, StartTimestamps: []int64{0},
			Kinds: []SampleKind{SampleFloat}, Values: []float64{value}, Histograms: [][]byte{nil},
		}},
		Updates: []AssociationUpdate{{Labels: ls, Update: Update{Owner: owner, Content: content, EffectiveFrom: timestamp}}},
	}))
}

func TestRestartRestoresBlocksAndMetadata(t *testing.T) {
	dir := t.TempDir()
	opts := StorageOptions{FlushSamples: 1, MetadataVersionCap: 4}
	s, err := NewStorage(dir, opts)
	require.NoError(t, err)
	owner := ResourceOwner(map[string]string{"service.name": "restart", "service.instance.id": "one"})
	ls := labels.FromStrings(labels.MetricName, "restart_metric", OTELResourceLabel, owner.Ref())

	for i, host := range []string{"a", "b", "c"} {
		timestamp := int64(100 * (i + 1))
		appendResourceFloat(t, s, ls, owner, timestamp, Content{Attributes: map[string]string{"host": host}}, float64(i+1))
	}
	require.Equal(t, 3, s.NumBlocks())
	require.NoError(t, s.Close())

	s, err = NewStorage(dir, opts)
	require.NoError(t, err)
	history, ok := s.MetadataHistory(owner)
	require.True(t, ok)
	require.Len(t, history.Versions, 3)
	series, err := s.SelectSeries(t.Context(), 0, 1000, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []int64{100, 200, 300}, series[0].Timestamps)
	require.Equal(t, []float64{1, 2, 3}, series[0].Values)
	for _, tc := range []struct {
		t    int64
		host string
	}{
		{t: 199, host: "a"},
		{t: 200, host: "b"},
		{t: 250, host: "b"},
		{t: 300, host: "c"},
	} {
		content, ok, truncated := s.MetadataAtLabels(ls, MetadataResource, tc.t)
		require.True(t, ok)
		require.False(t, truncated)
		require.Equal(t, tc.host, content.Attributes["host"])
	}

	// Reused block state validates ordering instead of re-appending an old sample.
	err = s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{{
		Labels: ls, Timestamps: []int64{200}, StartTimestamps: []int64{0},
		Kinds: []SampleKind{SampleFloat}, Values: []float64{9}, Histograms: [][]byte{nil},
	}}})
	require.Error(t, err)
	require.Equal(t, 3, s.NumBlocks())
	require.NoError(t, s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{{
		Labels: ls, Timestamps: []int64{400}, StartTimestamps: []int64{0},
		Kinds: []SampleKind{SampleFloat}, Values: []float64{4}, Histograms: [][]byte{nil},
	}}}))
	require.NoError(t, s.Close())

	s, err = NewStorage(dir, opts)
	require.NoError(t, err)
	require.Len(t, s.blocks, 4)
	defer s.Close()
	series, err = s.SelectSeries(t.Context(), 0, 1000, nil, 0)
	require.NoError(t, err)
	require.Equal(t, []int64{100, 200, 300, 400}, series[0].Timestamps)

	compactedDir := t.TempDir()
	meta, err := s.CompactTo(t.Context(), compactedDir)
	require.NoError(t, err)
	require.Equal(t, 1, meta.NumSeries)
	require.Equal(t, 4, meta.NumSamples)
	compacted, err := NewStorage(compactedDir, StorageOptions{DisableWAL: true, MetadataVersionCap: 4})
	require.NoError(t, err)
	defer compacted.Close()
	compactedSeries, err := compacted.SelectSeries(t.Context(), 0, 1000, nil, 0)
	require.NoError(t, err)
	require.Len(t, compactedSeries, 1)
	require.Equal(t, []int64{100, 200, 300, 400}, compactedSeries[0].Timestamps)
	compactedHistory, ok := compacted.MetadataHistory(owner)
	require.True(t, ok)
	require.Len(t, compactedHistory.Versions, 3)
	for _, tc := range []struct {
		t    int64
		host string
	}{
		{t: 199, host: "a"},
		{t: 200, host: "b"},
		{t: 250, host: "b"},
		{t: 300, host: "c"},
	} {
		content, ok, truncated := compacted.MetadataAtLabels(ls, MetadataResource, tc.t)
		require.True(t, ok)
		require.False(t, truncated)
		require.Equal(t, tc.host, content.Attributes["host"])
	}
}

func TestWALReplaysSamplesAndOwnerOnlyMetadata(t *testing.T) {
	dir := t.TempDir()
	opts := StorageOptions{FlushSamples: 1000}
	s, err := NewStorage(dir, opts)
	require.NoError(t, err)
	owner := ResourceOwner(map[string]string{"service.name": "wal", "service.instance.id": "one"})
	ls := labels.FromStrings(labels.MetricName, "wal_metric", OTELResourceLabel, owner.Ref())
	h := &histogram.Histogram{
		Count: 3, Sum: 6, Schema: 0, ZeroThreshold: 1e-6, ZeroCount: 1,
		PositiveSpans: []histogram.Span{{Offset: 0, Length: 2}}, PositiveBuckets: []int64{1, 1},
	}
	payload, err := encodeHistogram(h, nil)
	require.NoError(t, err)

	require.NoError(t, s.AppendBatch(t.Context(), Batch{
		Series: []BatchSeries{{
			Labels: ls, Timestamps: []int64{100, 200}, StartTimestamps: []int64{50, 50},
			Kinds: []SampleKind{SampleFloat, SampleHistogram}, Values: []float64{1, 0},
			Histograms: [][]byte{nil, payload},
		}},
		Updates: []AssociationUpdate{{Labels: ls, Update: Update{
			Owner: owner, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"host": "wal-host"}},
		}}},
	}))
	scrapeOwner := ScrapeOwner("wal-job")
	require.NoError(t, s.MetadataUpdate(Update{
		Owner: scrapeOwner, EffectiveFrom: 200,
		Content: Content{Attributes: map[string]string{"interval": "15s"}},
	}))
	require.Zero(t, s.NumBlocks())
	entries, err := os.ReadDir(walDirForTest(dir))
	require.NoError(t, err)
	require.NotEmpty(t, entries, "batches must be logged")
	// Consecutive batches share a segment, so assert on records rather than files.
	var records int
	for _, entry := range entries {
		batches, _, err := readWALRecords(walDirForTest(dir) + "/" + entry.Name())
		require.NoError(t, err)
		records += len(batches)
	}
	require.Equal(t, 2, records, "one record per ingested batch")
	require.NoError(t, s.Close())

	s, err = NewStorage(dir, opts)
	require.NoError(t, err)
	require.Zero(t, s.NumBlocks())
	series, err := s.SelectSeries(t.Context(), 0, 1000, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []int64{100, 200}, series[0].Timestamps)
	require.Equal(t, []int64{50, 50}, series[0].StartTimestamps)
	require.Equal(t, []SampleKind{SampleFloat, SampleHistogram}, series[0].Kinds)
	decoded, _, err := series[0].HistogramAt(1)
	require.NoError(t, err)
	require.True(t, h.Equals(decoded))
	resource, ok, _ := s.MetadataAtLabels(ls, MetadataResource, 200)
	require.True(t, ok)
	require.Equal(t, "wal-host", resource.Attributes["host"])
	cadence, ok, _ := s.MetadataAtOwner(scrapeOwner, 200)
	require.True(t, ok)
	require.Equal(t, "15s", cadence.Attributes["interval"])

	// Replaying the same unflushed WAL again affects no duplicate timestamp counts.
	require.NoError(t, s.Close())
	s, err = NewStorage(dir, opts)
	require.NoError(t, err)
	series, err = s.SelectSeries(t.Context(), 0, 1000, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []int64{100, 200}, series[0].Timestamps)

	require.NoError(t, s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{{
		Labels: ls, Timestamps: []int64{300}, StartTimestamps: []int64{0},
		Kinds: []SampleKind{SampleFloat}, Values: []float64{3}, Histograms: [][]byte{nil},
	}}}))
	require.Equal(t, uint64(3), s.walSequence, "new batches advance beyond every recovered segment")
	require.NoError(t, s.Close())
	s, err = NewStorage(dir, opts)
	require.NoError(t, err)
	defer s.Close()
	series, err = s.SelectSeries(t.Context(), 0, 1000, nil, 0)
	require.NoError(t, err)
	require.Equal(t, []int64{100, 200, 300}, series[0].Timestamps)
}

func TestPublishedBlockSkipsStaleWALSegment(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStorage(dir, StorageOptions{DisableNativeMetadata: true})
	require.NoError(t, err)
	ls := labels.FromStrings(labels.MetricName, "published_wal_metric")
	batch := Batch{Series: []BatchSeries{{
		Labels: ls, Timestamps: []int64{100}, StartTimestamps: []int64{0},
		Kinds: []SampleKind{SampleFloat}, Values: []float64{1}, Histograms: [][]byte{nil},
	}}}
	require.NoError(t, s.AppendBatch(t.Context(), batch))

	// Simulate a crash that published the block but deleted no log segment.
	segmentPath := walSegmentPath(dir, 1)
	segment, err := os.ReadFile(segmentPath)
	require.NoError(t, err)
	require.NoError(t, s.Flush())
	_, err = os.Stat(segmentPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, os.WriteFile(segmentPath, segment, 0o600))
	require.NoError(t, s.Close())

	s, err = NewStorage(dir, StorageOptions{DisableNativeMetadata: true})
	require.NoError(t, err)
	require.Zero(t, s.bufferN, "published WAL data must not replay into the head")
	series, err := s.SelectSeries(t.Context(), 0, 1000, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []int64{100}, series[0].Timestamps)
	require.NoError(t, s.Close())
}

func walDirForTest(storageDir string) string { return storageDir + "/wal" }

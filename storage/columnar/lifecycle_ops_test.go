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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
)

func TestCompactPublishesAndPreservesMetadata(t *testing.T) {
	dir := t.TempDir()
	opts := StorageOptions{FlushSamples: 1, MetadataVersionCap: 4, IndexedResourceAttributes: []string{"host"}}
	s, err := NewStorage(dir, opts)
	require.NoError(t, err)
	owner := ResourceOwner(map[string]string{"service.name": "compaction", "service.instance.id": "one"})
	ls := labels.FromStrings(labels.MetricName, "compact_metric", OTELResourceLabel, owner.Ref())
	for i, timestamp := range []int64{100, 200, 300} {
		host := "old"
		if i > 0 {
			host = "new"
		}
		appendResourceFloat(t, s, ls, owner, timestamp, Content{Attributes: map[string]string{"host": host}}, float64(i+1))
	}
	before := s.blocks
	require.Len(t, before, 3)
	s.reads.reset()
	_, err = s.SelectSeries(t.Context(), 0, 300, nil, 0)
	require.NoError(t, err)
	require.Equal(t, uint64(3), s.ReadStats().SamplePayloadOpens)
	require.Equal(t, uint64(3), s.ReadStats().RowGroupsRead)
	contentBefore, ok, _ := s.MetadataAtLabels(ls, MetadataResource, 200)
	require.True(t, ok)
	require.Equal(t, "new", contentBefore.Attributes["host"])

	meta, err := s.Compact(t.Context())
	require.NoError(t, err)
	require.Equal(t, 3, meta.NumSamples)
	require.Equal(t, FormatVersion, meta.FormatVersion)
	require.Len(t, meta.Replaces, 3)
	require.Len(t, s.blocks, 1)
	s.reads.reset()
	_, err = s.SelectSeries(t.Context(), 0, 300, nil, 0)
	require.NoError(t, err)
	require.Equal(t, uint64(1), s.ReadStats().SamplePayloadOpens)
	require.Equal(t, uint64(1), s.ReadStats().RowGroupsRead)
	content, ok, truncated := s.MetadataAtLabels(ls, MetadataResource, 200)
	require.True(t, ok)
	require.False(t, truncated)
	require.Equal(t, "new", content.Attributes["host"])
	series, err := s.SelectSeries(t.Context(), 0, 300, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []float64{1, 2, 3}, series[0].Values)

	for _, parent := range before {
		_, err := os.Stat(filepath.Join(dir, parent.meta.ULID))
		require.ErrorIs(t, err, os.ErrNotExist, "published parents are retired")
	}
	require.NoError(t, s.Close())
	reopened, err := NewStorage(dir, opts)
	require.NoError(t, err)
	defer reopened.Close()
	require.Len(t, reopened.blocks, 1)
	content, ok, _ = reopened.MetadataAtLabels(ls, MetadataResource, 300)
	require.True(t, ok)
	require.Equal(t, "new", content.Attributes["host"])
	require.NoError(t, reopened.AppendBatch(t.Context(), Batch{Series: []BatchSeries{batchFloatChunk(ls, 400)}}))
	series, err = reopened.SelectSeries(t.Context(), 0, 400, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []int64{100, 200, 300, 400}, series[0].Timestamps)
}

func TestRetentionKeepsPartiallyExpiredBlock(t *testing.T) {
	s, err := NewStorage(t.TempDir(), StorageOptions{DisableWAL: true, DisableNativeMetadata: true})
	require.NoError(t, err)
	defer s.Close()
	ls := labels.FromStrings(labels.MetricName, "partial_retention")
	require.NoError(t, s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{batchFloatChunk(ls, 100, 200)}}))
	require.NoError(t, s.Flush())
	require.NoError(t, s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{batchFloatChunk(ls, 300)}}))
	require.NoError(t, s.Flush())
	removed, err := s.RetainBefore(t.Context(), 200)
	require.NoError(t, err)
	require.Zero(t, removed, "a block ending after the cutoff is not partially deleted")
	require.Len(t, s.blocks, 2)
	series, err := s.SelectSeries(t.Context(), 0, 300, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []int64{100, 200, 300}, series[0].Timestamps)
	removed, err = s.RetainBefore(t.Context(), 201)
	require.NoError(t, err)
	require.Equal(t, 1, removed, "the exclusive sample maximum permits whole-block expiry at 201")
	series, err = s.SelectSeries(t.Context(), 0, 300, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []int64{300}, series[0].Timestamps)
}

func TestCompactionAndRetentionRequireFlushedHead(t *testing.T) {
	s, err := NewStorage(t.TempDir(), StorageOptions{FlushSamples: 100, DisableWAL: true, DisableNativeMetadata: true})
	require.NoError(t, err)
	defer s.Close()
	ls := labels.FromStrings(labels.MetricName, "pending_lifecycle")
	require.NoError(t, s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{batchFloatChunk(ls, 100)}}))
	require.NoError(t, s.Flush())
	require.NoError(t, s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{batchFloatChunk(ls, 200)}}))
	_, err = s.Compact(t.Context())
	require.ErrorIs(t, err, ErrHeadNotFlushed)
	_, err = s.RetainBefore(t.Context(), 200)
	require.ErrorIs(t, err, ErrHeadNotFlushed)
	require.Equal(t, 1, s.bufferN, "200 is still pending while sealed 100 precedes it; total samples are 2")
	require.Len(t, s.blocks, 1)
	require.NoError(t, s.Flush())
	_, err = s.Compact(t.Context())
	require.NoError(t, err)
	series, err := s.SelectSeries(t.Context(), 0, 200, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []int64{100, 200}, series[0].Timestamps)
}

func TestPendingLifecycleWALReplaysAfterRejectedPublication(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStorage(dir, StorageOptions{FlushSamples: 100, DisableNativeMetadata: true})
	require.NoError(t, err)
	ls := labels.FromStrings(labels.MetricName, "pending_wal")
	require.NoError(t, s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{batchFloatChunk(ls, 100)}}))
	require.NoError(t, s.Flush()) // Publishes and removes the already-sealed WAL.
	require.NoError(t, s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{batchFloatChunk(ls, 200)}}))
	_, err = s.Compact(t.Context())
	require.ErrorIs(t, err, ErrHeadNotFlushed)
	_, err = s.RetainBefore(t.Context(), 200)
	require.ErrorIs(t, err, ErrHeadNotFlushed)
	require.NoError(t, s.Close())

	reopened, err := NewStorage(dir, StorageOptions{FlushSamples: 100, DisableNativeMetadata: true})
	require.NoError(t, err)
	defer reopened.Close()
	series, err := reopened.SelectSeries(t.Context(), 0, 200, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []int64{100, 200}, series[0].Timestamps)
	require.Equal(t, uint64(2), reopened.walSequence)
}

func TestCompactionReopenSuppressesCrashLeftParents(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStorage(dir, StorageOptions{FlushSamples: 1, DisableWAL: true})
	require.NoError(t, err)
	owner := ResourceOwner(map[string]string{"service.name": "stale-parents"})
	ls := labels.FromStrings(labels.MetricName, "stale_parent_metric", OTELResourceLabel, owner.Ref())
	for i, timestamp := range []int64{100, 200, 300} {
		appendResourceFloat(t, s, ls, owner, timestamp, Content{Attributes: map[string]string{"host": "same"}}, float64(i+1))
	}
	var parents []string
	for _, block := range s.blocks {
		parents = append(parents, block.meta.ULID)
	}
	staging := t.TempDir()
	meta, err := s.CompactTo(t.Context(), staging)
	require.NoError(t, err)
	meta.Replaces = parents
	raw, err := json.Marshal(meta)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(staging, meta.ULID, metaFileName), raw, 0o600))
	copyBlockForLifecycleTest(t, filepath.Join(staging, meta.ULID), filepath.Join(dir, meta.ULID))
	// Deliberately leave every input published, as an interrupted cleanup would.
	require.NoError(t, s.Close())

	reopened, err := NewStorage(dir, StorageOptions{DisableWAL: true})
	require.NoError(t, err)
	defer reopened.Close()
	require.Len(t, reopened.blocks, 1, "replacement manifest suppresses each crash-left parent")
	series, err := reopened.SelectSeries(t.Context(), 0, 300, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []float64{1, 2, 3}, series[0].Values)
	reopened.reads.reset()
	_, err = reopened.SelectSeries(t.Context(), 0, 300, nil, 0)
	require.NoError(t, err)
	require.Equal(t, uint64(1), reopened.ReadStats().SamplePayloadOpens, "crash-left parents are not opened for samples")
	content, ok, _ := reopened.MetadataAtLabels(ls, MetadataResource, 300)
	require.True(t, ok)
	require.Equal(t, "same", content.Attributes["host"])
}

func TestRetentionWholeBlocksOOOAndRestartSemantics(t *testing.T) {
	dir := t.TempDir()
	opts := StorageOptions{FlushSamples: 1, DisableWAL: true}
	s, err := NewStorage(dir, opts)
	require.NoError(t, err)
	owner := ResourceOwner(map[string]string{"service.name": "retention", "service.instance.id": "one"})
	ls := labels.FromStrings(labels.MetricName, "retained_metric", OTELResourceLabel, owner.Ref())
	for i, timestamp := range []int64{100, 200, 300} {
		appendResourceFloat(t, s, ls, owner, timestamp, Content{Attributes: map[string]string{"host": "bound"}}, float64(i+1))
	}
	removed, err := s.RetainBefore(t.Context(), 200)
	require.NoError(t, err)
	require.Equal(t, 1, removed)
	require.Len(t, s.blocks, 2)
	series, err := s.SelectSeries(t.Context(), 0, 300, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []int64{200, 300}, series[0].Timestamps)
	content, ok, _ := s.MetadataAtLabels(ls, MetadataResource, 300)
	require.True(t, ok, "active metadata survives when its originating delta block expired")
	require.Equal(t, "bound", content.Attributes["host"])

	err = s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{batchFloatChunk(ls, 100)}})
	require.ErrorIs(t, err, storage.ErrOutOfBounds)
	err = s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{batchFloatChunk(ls, 300)}})
	require.ErrorIs(t, err, storage.ErrDuplicateSampleForTimestamp)
	err = s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{batchFloatChunk(ls, 250)}})
	require.ErrorIs(t, err, storage.ErrOutOfOrderSample, "OOO remains rejected; no hidden buffered-OOO support is claimed")

	require.NoError(t, s.Close())
	reopened, err := NewStorage(dir, opts)
	require.NoError(t, err)
	defer reopened.Close()
	series, err = reopened.SelectSeries(t.Context(), 0, 400, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []int64{200, 300}, series[0].Timestamps)
	fresh := labels.FromStrings(labels.MetricName, "new_after_restart")
	require.NoError(t, reopened.AppendBatch(t.Context(), Batch{Series: []BatchSeries{batchFloatChunk(fresh, 50)}}),
		"the retention cutoff is deliberately not a persisted tombstone for unseen series")
}

func TestRetentionCollectsChurnOwnersWithinVersionCap(t *testing.T) {
	const versionCap = 2
	s, err := NewStorage(t.TempDir(), StorageOptions{FlushSamples: 1, DisableWAL: true, MetadataVersionCap: versionCap, IndexedResourceAttributes: []string{"host"}})
	require.NoError(t, err)
	defer s.Close()
	var latestOwner OwnerKey
	var latestLabels labels.Labels
	ghostOwner := ResourceOwner(map[string]string{"service.name": "ghost-owner"})
	require.NoError(t, s.MetadataUpdate(Update{Owner: ghostOwner, EffectiveFrom: 0, Content: Content{Attributes: map[string]string{"host": "ghost"}}}))
	for i := range 6 {
		latestOwner = ResourceOwner(map[string]string{"service.name": "churn", "service.instance.id": string(rune('a' + i))})
		latestLabels = labels.FromStrings(labels.MetricName, "churn_metric", OTELResourceLabel, latestOwner.Ref())
		first := int64(i * 10)
		appendResourceFloat(t, s, latestLabels, latestOwner, first, Content{Attributes: map[string]string{"host": "first"}}, 1)
		appendResourceFloat(t, s, latestLabels, latestOwner, first+2, Content{Attributes: map[string]string{"host": "second"}}, 2)
	}
	require.Len(t, s.metadata.byOwner, 7, "six sample owners plus one owner-only delivery")
	removed, err := s.RetainBefore(t.Context(), 51)
	require.NoError(t, err)
	require.Equal(t, 11, removed)
	require.Len(t, s.metadata.byOwner, 1, "expired unique owners do not accumulate across retention")
	require.Len(t, s.metadata.bySeries, 1)
	history, ok := s.MetadataHistory(latestOwner)
	require.True(t, ok)
	require.Len(t, history.Versions, versionCap)
	content, ok, _ := s.MetadataAtLabels(latestLabels, MetadataResource, 52)
	require.True(t, ok)
	require.Equal(t, "second", content.Attributes["host"])
	for _, owner := range []OwnerKey{ResourceOwner(map[string]string{"service.name": "churn", "service.instance.id": "a"}), ghostOwner} {
		_, ok := s.MetadataHistory(owner)
		require.False(t, ok)
	}
	candidates, err := s.metadata.ownersMatchingAttributes(MetadataResource, []MetadataAttributeCondition{{Name: "host", Value: "first"}}, 0, 100)
	require.NoError(t, err)
	require.Len(t, candidates, 1, "expired owner candidates leave the global inverted index")
	_, ok = candidates[latestOwner]
	require.True(t, ok)
}

func copyBlockForLifecycleTest(t *testing.T, src, dst string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dst, 0o750))
	entries, err := os.ReadDir(src)
	require.NoError(t, err)
	for _, entry := range entries {
		require.False(t, entry.IsDir())
		data, err := os.ReadFile(filepath.Join(src, entry.Name()))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dst, entry.Name()), data, 0o600))
	}
}

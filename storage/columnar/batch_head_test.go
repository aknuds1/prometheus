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
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/storage"
)

func batchFloatChunk(ls labels.Labels, timestamps ...int64) BatchSeries {
	values := make([]float64, len(timestamps))
	for i, timestamp := range timestamps {
		values[i] = float64(timestamp)
	}
	return BatchSeries{
		Labels: ls, Timestamps: timestamps, StartTimestamps: make([]int64, len(timestamps)),
		Kinds: make([]SampleKind, len(timestamps)), Values: values, Histograms: make([][]byte, len(timestamps)),
	}
}

func TestAppendBatchSortedHeadAndDuplicateValidation(t *testing.T) {
	s, err := NewStorage(t.TempDir(), StorageOptions{DisableWAL: true, DisableNativeMetadata: true})
	require.NoError(t, err)
	defer s.Close()

	first := labels.FromStrings(labels.MetricName, "batch_metric", "series", "one")
	second := labels.FromStrings(labels.MetricName, "batch_metric", "series", "two")
	batch := Batch{Series: []BatchSeries{
		batchFloatChunk(second, 100, 200),
		batchFloatChunk(first, 100, 200),
		batchFloatChunk(second, 300, 400),
		batchFloatChunk(first, 300, 400),
	}}
	require.NoError(t, s.AppendBatch(t.Context(), batch))
	require.Len(t, s.buffer, 2)
	for _, want := range []labels.Labels{first, second} {
		hs, ok := s.buffer[stableSeriesHash(want)]
		require.True(t, ok, "series %s must be buffered by stable hash", want)
		require.True(t, labels.Equal(want, hs.labels))
	}
	series, err := s.SelectSeries(t.Context(), 0, 500, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 2)
	for _, item := range series {
		require.Equal(t, []int64{100, 200, 300, 400}, item.Timestamps)
	}

	beforeWAL := s.walSequence
	beforeSamples := s.bufferN
	err = s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{
		batchFloatChunk(first, 500),
		batchFloatChunk(second, 300),
	}})
	require.ErrorIs(t, err, storage.ErrOutOfOrderSample)
	require.Equal(t, beforeSamples, s.bufferN)
	require.Equal(t, beforeWAL, s.walSequence)
	selected, err := s.SelectSeries(t.Context(), 500, 500, nil, 0)
	require.NoError(t, err)
	require.Empty(t, selected, "a late invalid chunk rejects the entire sample batch")
}

func TestAppendBatchRejectsDuplicateAcrossChunksBeforeSeal(t *testing.T) {
	s, err := NewStorage(t.TempDir(), StorageOptions{DisableWAL: true, DisableNativeMetadata: true})
	require.NoError(t, err)
	defer s.Close()
	ls := labels.FromStrings(labels.MetricName, "batch_duplicate")
	batch := Batch{Series: []BatchSeries{batchFloatChunk(ls, 100), batchFloatChunk(ls, 100, 200)}}
	err = s.AppendBatch(t.Context(), batch)
	require.ErrorIs(t, err, storage.ErrDuplicateSampleForTimestamp)
	require.Zero(t, s.bufferN)
	require.Empty(t, s.buffer)
}

func TestAppendBatchValidationPrecedesWALAndMetadata(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStorage(dir, StorageOptions{})
	require.NoError(t, err)
	defer s.Close()
	owner := ResourceOwner(map[string]string{"service.name": "atomic", "service.instance.id": "one"})
	valid := labels.FromStrings(labels.MetricName, "atomic_metric", OTELResourceLabel, owner.Ref())
	invalid := labels.FromStrings(labels.MetricName, "invalid_metric")
	bad := batchFloatChunk(invalid, 100, 100)
	batch := Batch{
		Series: []BatchSeries{batchFloatChunk(valid, 100), bad},
		Updates: []AssociationUpdate{{Labels: valid, Update: Update{
			Owner: owner, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"host.name": "one"}},
		}}},
	}
	err = s.AppendBatch(t.Context(), batch)
	require.ErrorIs(t, err, storage.ErrDuplicateSampleForTimestamp)
	require.Zero(t, s.bufferN)
	_, err = os.Stat(filepath.Join(dir, walDirName))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, ok, _ := s.MetadataAtOwner(owner, 100)
	require.False(t, ok)
}

func TestAppendBatchSharedOwnerContextsReplay(t *testing.T) {
	dir := t.TempDir()
	opts := StorageOptions{FlushSamples: 100}
	s, err := NewStorage(dir, opts)
	require.NoError(t, err)
	resource := ResourceOwner(map[string]string{"service.name": "batch-contexts", "service.instance.id": "one"})
	ls := labels.FromStrings(labels.MetricName, "batch_context_metric", OTELResourceLabel, resource.Ref())
	owners := []OwnerKey{
		resource, ResourceSchemaOwner(resource), ScopeOwner("sdk", "v1"),
		ScrapeOwner("job-a"), SourceOwner("otap"), MetricOwner(ls),
	}
	batch := Batch{Series: []BatchSeries{batchFloatChunk(ls, 100)}}
	for i, owner := range owners {
		batch.Updates = append(batch.Updates, AssociationUpdate{Labels: ls, Update: Update{
			Owner: owner, EffectiveFrom: 100,
			Content: Content{Attributes: map[string]string{"context": owner.Kind.String(), "index": string(rune('a' + i))}},
		}})
	}
	require.NoError(t, s.AppendBatch(t.Context(), batch))
	require.NoError(t, s.Close())

	reopened, err := NewStorage(dir, opts)
	require.NoError(t, err)
	defer reopened.Close()
	for _, owner := range owners {
		history, ok := reopened.MetadataHistory(owner)
		require.True(t, ok, "%s owner missing after WAL replay", owner.Kind)
		require.Len(t, history.Versions, 1)
		associated, ok := reopened.SeriesMetadataAssociation(labels.StableHash(ls), owner.Kind, 100)
		require.True(t, ok)
		require.Equal(t, owner, associated)
	}
	series, err := reopened.SelectSeries(t.Context(), 0, 100, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1, "native associations do not recreate per-series target_info samples")
	require.True(t, labels.Equal(ls, series[0].Labels))
}

func TestAppendBatchStaleAndUnknownColumnsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	opts := StorageOptions{FlushSamples: 100, DisableNativeMetadata: true}
	s, err := NewStorage(dir, opts)
	require.NoError(t, err)
	ls := labels.FromStrings(labels.MetricName, "batch_stale")
	batch := Batch{Series: []BatchSeries{{
		Labels: ls, Timestamps: []int64{100, 200}, Kinds: []SampleKind{SampleFloat, SampleStale},
		Values: []float64{1, math.Float64frombits(value.StaleNaN)},
	}}}
	require.NoError(t, s.AppendBatch(t.Context(), batch))
	hs, ok := s.buffer[stableSeriesHash(ls)]
	require.True(t, ok)
	require.Nil(t, hs.st)
	require.Nil(t, hs.hist)
	require.NoError(t, s.Close())

	reopened, err := NewStorage(dir, opts)
	require.NoError(t, err)
	defer reopened.Close()
	series, err := reopened.SelectSeries(t.Context(), 0, 200, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []SampleKind{SampleFloat, SampleStale}, series[0].Kinds)
	require.True(t, value.IsStaleNaN(series[0].Values[1]))
	require.Empty(t, series[0].StartTimestamps)
	require.Empty(t, series[0].Histograms)

	err = reopened.AppendBatch(t.Context(), Batch{Series: []BatchSeries{{
		Labels: ls, Timestamps: []int64{300}, Kinds: []SampleKind{SampleStale}, Values: []float64{0},
	}}})
	require.ErrorContains(t, err, "does not carry the stale NaN")
}

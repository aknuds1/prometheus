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

func lookupTestSeries(t *testing.T, s *Storage, condition MetadataAttributeCondition) []MetadataSeriesResult {
	t.Helper()
	page, err := s.QueryMetadataSeries(t.Context(), MetadataSeriesQuery{
		Kind: MetadataResource, Start: 0, End: 400,
		Attributes: []MetadataAttributeCondition{condition},
	})
	require.NoError(t, err)
	return page.Series
}

func TestResourceIndexConfigurationAndReconfiguration(t *testing.T) {
	f := newMetadataAPITestFixture(t)
	require.NoError(t, f.storage.SetIndexedResourceAttrs([]string{"host.name"}))
	f.storage.reads.reset()

	results := lookupTestSeries(t, f.storage, MetadataAttributeCondition{Name: "host.name", Value: "h2"})
	require.Len(t, results, 1)
	require.Equal(t, "metric_a", results[0].Labels.Get(labels.MetricName))
	stats := f.storage.ReadStats()
	require.Equal(t, uint64(1), stats.MetadataIndexCandidates)
	require.Zero(t, stats.MetadataIndexFallbacks)
	require.Zero(t, stats.MetadataPayloadOpens)

	results = lookupTestSeries(t, f.storage, MetadataAttributeCondition{Name: "service.instance.id", Value: "two"})
	require.Len(t, results, 1)
	require.Equal(t, "metric_b", results[0].Labels.Get(labels.MetricName))
	stats = f.storage.ReadStats()
	require.Equal(t, uint64(2), stats.MetadataIndexCandidates, "identifying attributes are indexed by default")

	results = lookupTestSeries(t, f.storage, MetadataAttributeCondition{Name: "region", Value: "east"})
	require.Len(t, results, 2, "unindexed filters use the semantic fallback without false negatives")
	require.Equal(t, uint64(1), f.storage.ReadStats().MetadataIndexFallbacks)

	require.NoError(t, f.storage.SetIndexedResourceAttrs([]string{"region"}))
	require.Equal(t, []string{"region"}, f.storage.IndexedResourceAttrs())
	results = lookupTestSeries(t, f.storage, MetadataAttributeCondition{Name: "host.name", Value: "h2"})
	require.Len(t, results, 1)
	require.Equal(t, uint64(2), f.storage.ReadStats().MetadataIndexFallbacks)
	f.storage.reads.reset()
	results = lookupTestSeries(t, f.storage, MetadataAttributeCondition{Name: "region", Value: "east"})
	require.Len(t, results, 2)
	require.Equal(t, uint64(2), f.storage.ReadStats().MetadataIndexCandidates)
	require.Zero(t, f.storage.ReadStats().MetadataIndexFallbacks)

	// Clears remove only descriptive indexes, never identifier lookup.
	require.NoError(t, f.storage.SetIndexedResourceAttrs(nil))
	require.Empty(t, f.storage.IndexedResourceAttrs())
	f.storage.reads.reset()
	results = lookupTestSeries(t, f.storage, MetadataAttributeCondition{Name: "service.instance.id", Value: "one"})
	require.Len(t, results, 1)
	require.Equal(t, uint64(1), f.storage.ReadStats().MetadataIndexCandidates)
	require.Zero(t, f.storage.ReadStats().MetadataIndexFallbacks)
}

func TestMetadataIndexConfigurationSurvivesRestart(t *testing.T) {
	f := newMetadataAPITestFixture(t)
	require.NoError(t, f.storage.SetIndexedResourceAttrs([]string{"region", "host.name", "region"}))
	require.NoError(t, f.storage.Flush())
	latest := f.storage.blocks[len(f.storage.blocks)-1]
	require.Equal(t, []string{"host.name", "region"}, latest.meta.Metadata.IndexedAttributes)
	require.NoError(t, f.storage.Close())

	reopened, err := NewStorage(f.storage.dir, StorageOptions{})
	require.NoError(t, err)
	defer reopened.Close()
	require.Equal(t, []string{"host.name", "region"}, reopened.IndexedResourceAttrs())
	reopened.reads.reset()
	results := lookupTestSeries(t, reopened, MetadataAttributeCondition{Name: "region", Value: "east"})
	require.Len(t, results, 2)
	stats := reopened.ReadStats()
	require.Equal(t, uint64(2), stats.MetadataIndexCandidates)
	require.Zero(t, stats.MetadataIndexFallbacks)
	require.Zero(t, stats.MetadataPayloadOpens, "metadata query uses the recovered global index, not every block payload")

	// A resource unknown to the index uses no data or metadata payload and returns
	// no candidate, not an unvalidated series.
	results = lookupTestSeries(t, reopened, MetadataAttributeCondition{Name: "host.name", Value: "absent"})
	require.Empty(t, results)
	stats = reopened.ReadStats()
	require.Zero(t, stats.MetadataPayloadOpens)
	require.Zero(t, stats.SamplePayloadOpens)
	require.Zero(t, stats.LabelPayloadOpens)
	require.Equal(t, uint64(2), stats.MetadataIndexCandidates)
}

func TestLabelQueriesStreamAndSkipSamples(t *testing.T) {
	s, err := NewStorage(t.TempDir(), StorageOptions{DisableNativeMetadata: true, DisableWAL: true})
	require.NoError(t, err)
	defer s.Close()
	appendValue := func(value string, timestamp int64) {
		ls := labels.FromStrings(labels.MetricName, "streaming_metric", "shard", value)
		require.NoError(t, s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{{
			Labels: ls, Timestamps: []int64{timestamp}, StartTimestamps: []int64{0},
			Kinds: []SampleKind{SampleFloat}, Values: []float64{1}, Histograms: [][]byte{nil},
		}}}))
	}
	appendValue("one", 1000)
	require.NoError(t, s.Flush())
	appendValue("two", 2000)
	q, err := s.Querier(0, 3000)
	require.NoError(t, err)
	defer q.Close()

	names, warnings, err := q.LabelNames(t.Context(), nil)
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Equal(t, []string{"__name__", "shard"}, names)
	names, _, err = q.LabelNames(t.Context(), &storage.LabelHints{Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []string{"__name__"}, names)
	values, _, err := q.LabelValues(t.Context(), "shard", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"one", "two"}, values)
	values, _, err = q.LabelValues(t.Context(), "shard", &storage.LabelHints{Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []string{"one"}, values)

	stats := s.ReadStats()
	require.Zero(t, stats.LabelPayloadOpens, "the block catalog serves labels without a Parquet open")
	require.Zero(t, stats.SamplePayloadOpens, "label-only methods never open sample projections")
	require.Equal(t, uint64(8), stats.LabelRowsVisited, "one head row and one block row per query visit")
	require.Zero(t, stats.SampleRowsDecoded, "label operations must not decode sample columns")
}

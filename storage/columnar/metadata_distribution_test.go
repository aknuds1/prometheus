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

package columnar_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/common/promslog"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/tsdb/seriesmetadata"
)

func TestDistributedResourceMetadataUsesVersionsNotTargetInfo(t *testing.T) {
	owner := columnar.ResourceOwner(map[string]string{
		"service.name": "api", "service.instance.id": "one",
	})
	ls := labels.FromStrings(
		labels.MetricName, "sharded_native_metric",
		"job", "api", "instance", "one", columnar.OTELResourceLabel, owner.Ref(),
	)
	primary, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{})
	require.NoError(t, err)
	defer primary.Close()
	secondary, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{})
	require.NoError(t, err)
	defer secondary.Close()
	appendNativeShardPoint(t, primary, ls, owner, 1000, 1, "native-old")
	appendNativeShardPoint(t, secondary, ls, owner, 2000, 2, "native-latest")

	// This row exists as a migration control. It must not be joined across shards
	// once native membership supplies content.
	legacy := labels.FromStrings(
		labels.MetricName, "target_info", "job", "api", "instance", "one", "host.name", "legacy-host",
	)
	require.NoError(t, primary.AppendBatch(t.Context(), columnar.Batch{Series: []columnar.BatchSeries{{
		Labels: legacy, Timestamps: []int64{1000}, StartTimestamps: []int64{0},
		Kinds: []columnar.SampleKind{columnar.SampleFloat}, Values: []float64{1}, Histograms: [][]byte{nil},
	}}}))

	fanout := storage.NewFanout(promslog.NewNopLogger(), primary, secondary)
	query, err := fanout.Querier(0, 3000)
	require.NoError(t, err)
	resourceQuerier, ok := query.(storage.ResourceQuerier)
	require.True(t, ok)
	for _, tc := range []struct {
		t       int64
		host    string
		version int64
	}{
		{t: 500},
		{t: 1000, host: "native-old", version: 1000},
		{t: 1500, host: "native-old", version: 1000},
		{t: 2000, host: "native-latest", version: 2000},
	} {
		resource, ok := resourceQuerier.GetResourceAt(labels.StableHash(ls), tc.t)
		if tc.host == "" {
			require.False(t, ok)
			continue
		}
		require.True(t, ok)
		require.Equal(t, owner.String(), resource.OwnerID)
		require.Equal(t, tc.host, resource.Descriptive["host.name"])
		require.Equal(t, tc.version, resource.EffectiveFrom)
	}
	var names []string
	require.NoError(t, resourceQuerier.IterUniqueAttributeNames(func(name string) { names = append(names, name) }))
	require.Equal(t, []string{"host.name", "region"}, names)
	membership := query.(storage.ResourceMembershipQuerier)
	require.False(t, membership.HasResourceMembershipAt(labels.StableHash(ls), 500))
	require.True(t, membership.HasResourceMembershipAt(labels.StableHash(ls), 1000))
	require.NoError(t, query.Close())

	engine := stageEInfoEngine(t, true)
	q, err := engine.NewRangeQuery(t.Context(), fanout, nil, `info(sharded_native_metric)`, time.UnixMilli(1000), time.UnixMilli(2000), time.Second)
	require.NoError(t, err)
	result := q.Exec(t.Context())
	require.NoError(t, result.Err)
	matrix := result.Value.(promql.Matrix)
	var hosts []string
	for _, series := range matrix {
		hosts = append(hosts, series.Metric.Get("host.name"))
	}
	require.ElementsMatch(t, []string{"native-old", "native-latest"}, hosts)
	q.Close()
}

func TestMergedResourceConflictDoesNotSelectTargetInfo(t *testing.T) {
	owner := columnar.ResourceOwner(map[string]string{"service.name": "api", "service.instance.id": "one"})
	ls := labels.FromStrings(
		labels.MetricName, "conflicting_resource_metric", "job", "api", "instance", "one",
		columnar.OTELResourceLabel, owner.Ref(),
	)
	first, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{})
	require.NoError(t, err)
	defer first.Close()
	second, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{})
	require.NoError(t, err)
	defer second.Close()
	appendNativeShardPoint(t, first, ls, owner, 1000, 1, "a")
	appendNativeShardPoint(t, second, ls, owner, 1000, 2, "b")
	legacy := labels.FromStrings(
		labels.MetricName, "target_info", "job", "api", "instance", "one", "host.name", "legacy-host",
	)
	for _, store := range []*columnar.Storage{first, second} {
		require.NoError(t, store.AppendBatch(t.Context(), columnar.Batch{Series: []columnar.BatchSeries{{
			Labels: legacy, Timestamps: []int64{1000}, StartTimestamps: []int64{0},
			Kinds: []columnar.SampleKind{columnar.SampleFloat}, Values: []float64{1}, Histograms: [][]byte{nil},
		}}}))
	}

	firstQuery := &countingResourceQuerier{Querier: mustColumnarQuerier(t, first)}
	secondQuery := &countingResourceQuerier{Querier: mustColumnarQuerier(t, second)}
	merged := storage.NewMergeQuerier(
		[]storage.Querier{firstQuery}, []storage.Querier{secondQuery}, storage.ChainedSeriesMerge,
	)
	defer merged.Close()
	resourceQuerier := merged.(storage.ResourceQuerier)
	resource, found := resourceQuerier.GetResourceAt(labels.StableHash(ls), 1000)
	require.False(t, found)
	require.Nil(t, resource)
	require.True(t, merged.(storage.ResourceMembershipQuerier).HasResourceMembershipAt(labels.StableHash(ls), 1000))

	engine := stageEInfoEngine(t, true)
	query, err := engine.NewInstantQuery(t.Context(), readyQueryable{querier: merged}, nil, `info(conflicting_resource_metric)`, time.UnixMilli(1000))
	require.NoError(t, err)
	result := query.Exec(t.Context())
	require.NoError(t, result.Err)
	vector := result.Value.(promql.Vector)
	require.Len(t, vector, 1)
	require.Empty(t, vector[0].Metric.Get("host.name"), "conflicting namespace content is not replaced by target_info")
	require.Zero(t, firstQuery.targetInfoReads.Load())
	require.Zero(t, secondQuery.targetInfoReads.Load())
	query.Close()
}

type countingResourceQuerier struct {
	storage.Querier
	targetInfoReads atomic.Int64
}

func (q *countingResourceQuerier) Select(ctx context.Context, sorted bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	for _, matcher := range matchers {
		if matcher.Name == labels.MetricName && matcher.Type == labels.MatchEqual && matcher.Value == "target_info" {
			q.targetInfoReads.Add(1)
		}
	}
	return q.Querier.Select(ctx, sorted, hints, matchers...)
}

func (q *countingResourceQuerier) GetResourceAt(hash uint64, timestamp int64) (*seriesmetadata.ResourceVersion, bool) {
	return q.Querier.(storage.ResourceQuerier).GetResourceAt(hash, timestamp)
}

func (q *countingResourceQuerier) IterUniqueAttributeNames(fn func(string)) error {
	return q.Querier.(storage.ResourceQuerier).IterUniqueAttributeNames(fn)
}

func (q *countingResourceQuerier) HasResourceMembershipAt(hash uint64, timestamp int64) bool {
	return q.Querier.(storage.ResourceMembershipQuerier).HasResourceMembershipAt(hash, timestamp)
}

func TestRecentSelectorDoesNotOpenOlderBlocks(t *testing.T) {
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{
		FlushSamples: 1, DisableNativeMetadata: true, DisableWAL: true,
	})
	require.NoError(t, err)
	defer s.Close()
	const first = int64(1_700_000_000_000)
	var latest int64
	for i := range 24 {
		latest = first + int64(i)*time.Hour.Milliseconds()
		ls := labels.FromStrings(labels.MetricName, fmt.Sprintf("recent_metric_%d", i))
		require.NoError(t, s.AppendBatch(t.Context(), columnar.Batch{Series: []columnar.BatchSeries{{
			Labels: ls, Timestamps: []int64{latest}, StartTimestamps: []int64{0},
			Kinds: []columnar.SampleKind{columnar.SampleFloat}, Values: []float64{1}, Histograms: [][]byte{nil},
		}}}))
	}
	require.Equal(t, 24, s.NumBlocks())
	q, err := s.Querier(latest-time.Minute.Milliseconds(), latest)
	require.NoError(t, err)
	series := q.Select(t.Context(), false, &storage.SelectHints{
		Start: latest - 5*time.Minute.Milliseconds(), End: latest,
	}, labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "recent_metric_23"))
	require.True(t, series.Next())
	require.False(t, series.Next())
	require.NoError(t, series.Err())
	stats := s.ReadStats()
	require.Equal(t, uint64(1), stats.LabelPayloadOpens)
	require.Equal(t, uint64(1), stats.SamplePayloadOpens)
	require.Equal(t, uint64(1), stats.RowGroupsRead)
	require.Equal(t, uint64(1), stats.SampleRowsDecoded)
	require.Zero(t, stats.MetadataPayloadOpens)
	require.NoError(t, q.Close())
}

func mustColumnarQuerier(t *testing.T, s *columnar.Storage) storage.Querier {
	t.Helper()
	q, err := s.Querier(0, 3000)
	require.NoError(t, err)
	return q
}

type readyQueryable struct {
	querier storage.Querier
}

func (q readyQueryable) Querier(int64, int64) (storage.Querier, error) { return q.querier, nil }

func appendNativeShardPoint(t *testing.T, s *columnar.Storage, ls labels.Labels, owner columnar.OwnerKey, timestamp int64, value float64, host string) {
	t.Helper()
	require.NoError(t, s.AppendBatch(t.Context(), columnar.Batch{
		Series: []columnar.BatchSeries{{
			Labels: ls, Timestamps: []int64{timestamp}, StartTimestamps: []int64{0},
			Kinds: []columnar.SampleKind{columnar.SampleFloat}, Values: []float64{value}, Histograms: [][]byte{nil},
		}},
		Updates: []columnar.AssociationUpdate{{Labels: ls, Update: columnar.Update{
			Owner: owner, EffectiveFrom: timestamp,
			Content: columnar.Content{Attributes: map[string]string{"host.name": host, "region": "east"}},
		}}},
	}))
}

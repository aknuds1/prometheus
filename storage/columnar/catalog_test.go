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
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
)

func TestBlockPostingsMatchers(t *testing.T) {
	series := []Series{
		NewFloatSeries(labels.FromStrings(labels.MetricName, "postings_metric", "series", "a", "shard", "a"), []int64{100}, []float64{1}),
		NewFloatSeries(labels.FromStrings(labels.MetricName, "postings_metric", "series", "b"), []int64{100}, []float64{2}),
		NewFloatSeries(labels.FromStrings(labels.MetricName, "postings_metric", "series", "c", "shard", ""), []int64{100}, []float64{3}),
		NewFloatSeries(labels.FromStrings(labels.MetricName, "postings_metric", "series", "d", "shard", "b"), []int64{100}, []float64{4}),
		NewFloatSeries(labels.FromStrings(labels.MetricName, "other_metric", "series", "e", "shard", "c"), []int64{100}, []float64{5}),
	}
	dir := t.TempDir()
	meta, err := WriteBlock(dir, series, WriteOptions{})
	require.NoError(t, err)
	block, err := OpenBlock(filepath.Join(dir, meta.ULID))
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		matches []*labels.Matcher
		want    []string
	}{
		{name: "all", want: []string{"a", "b", "c", "d", "e"}},
		{name: "equal", matches: []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "shard", "a")}, want: []string{"a"}},
		{name: "equal empty includes absent", matches: []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "shard", "")}, want: []string{"b", "c"}},
		{name: "not equal", matches: []*labels.Matcher{labels.MustNewMatcher(labels.MatchNotEqual, "shard", "a")}, want: []string{"b", "c", "d", "e"}},
		{name: "not equal empty excludes absent", matches: []*labels.Matcher{labels.MustNewMatcher(labels.MatchNotEqual, "shard", "")}, want: []string{"a", "d", "e"}},
		{name: "regexp", matches: []*labels.Matcher{labels.MustNewMatcher(labels.MatchRegexp, "shard", "a|b")}, want: []string{"a", "d"}},
		{name: "not regexp includes absent", matches: []*labels.Matcher{labels.MustNewMatcher(labels.MatchNotRegexp, "shard", "a|b")}, want: []string{"b", "c", "e"}},
		{name: "not regexp all", matches: []*labels.Matcher{labels.MustNewMatcher(labels.MatchNotRegexp, "shard", ".*")}, want: nil},
		{name: "intersection", matches: []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "postings_metric"),
			labels.MustNewMatcher(labels.MatchNotEqual, "shard", "a"),
		}, want: []string{"b", "c", "d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := block.Select(t.Context(), 0, 1000, [][]*labels.Matcher{tc.matches}, 0)
			require.NoError(t, err)
			var ids []string
			for _, item := range got {
				ids = append(ids, item.Labels.Get("series"))
			}
			slices.Sort(ids)
			require.Equal(t, tc.want, ids)
		})
	}
}

func TestStableSeriesHashSharesMetadataIdentity(t *testing.T) {
	owner := ResourceOwner(map[string]string{"service.name": "hashing"})
	for _, ls := range []labels.Labels{
		labels.FromStrings(labels.MetricName, "named_metric"),
		labels.FromStrings(labels.MetricName, "resource_metric", OTELResourceLabel, owner.Ref()),
	} {
		require.Equal(t, metadataSeriesKey(ls), stableSeriesHash(ls))
	}
}

func TestStableSeriesIDSharedWithResourceMembership(t *testing.T) {
	s, err := NewStorage(t.TempDir(), StorageOptions{DisableWAL: true})
	require.NoError(t, err)
	defer s.Close()
	ownerA := ResourceOwner(map[string]string{"service.name": "checkout", "service.instance.id": "one"})
	ownerB := ResourceOwner(map[string]string{"service.name": "checkout", "service.instance.id": "two"})
	// Resource identity is part of the labels and therefore part of StableHash;
	// changing descriptive Resource content never changes this series ID.
	resourceA, resourceB := resourceLabel(ownerA), resourceLabel(ownerB)
	seriesA := labels.FromStrings(labels.MetricName, "resource_metric", resourceA.Name, resourceA.Value)
	seriesB := labels.FromStrings(labels.MetricName, "resource_metric", resourceB.Name, resourceB.Value)
	require.NotEqual(t, labels.StableHash(seriesA), labels.StableHash(seriesB))

	appendPoint := func(ls labels.Labels, owner OwnerKey, at int64, host string) {
		require.NoError(t, s.AppendBatch(t.Context(), Batch{
			Series: []BatchSeries{{
				Labels: ls, Timestamps: []int64{at}, StartTimestamps: []int64{0},
				Kinds: []SampleKind{SampleFloat}, Values: []float64{1}, Histograms: [][]byte{nil},
			}},
			Updates: []AssociationUpdate{{Labels: ls, Update: Update{
				Owner: owner, EffectiveFrom: at, Content: Content{Attributes: map[string]string{"host.name": host}},
			}}},
		}))
	}
	appendPoint(seriesA, ownerA, 100, "a-1")
	appendPoint(seriesB, ownerB, 100, "b-1")
	appendPoint(seriesA, ownerA, 200, "a-2")
	require.NoError(t, s.Flush())
	require.Len(t, s.blocks, 1)
	block := s.blocks[0]
	for _, entry := range block.catalog.Series {
		require.Equal(t, labels.StableHash(entry.Labels), entry.ID)
		require.NotEmpty(t, entry.Labels.Get(OTELResourceLabel))
	}
	relations, err := block.readMetadataRelations(t.Context())
	require.NoError(t, err)
	for _, membership := range relations.memberships {
		i := sortSearchSeries(block.catalog.Series, membership.seriesHash)
		require.GreaterOrEqual(t, i, 0)
		require.True(t, labels.Equal(membership.labels, block.catalog.Series[i].Labels))
	}

	s.reads.reset()
	page, err := s.QueryMetadataSeries(t.Context(), MetadataSeriesQuery{
		Kind: MetadataResource, Start: 200, End: 200,
		Attributes: []MetadataAttributeCondition{{Name: "host.name", Value: "a-2"}},
	})
	require.NoError(t, err)
	require.Len(t, page.Series, 1)
	require.True(t, labels.Equal(seriesA, page.Series[0].Labels))
	require.Zero(t, s.ReadStats().SamplePayloadOpens, "series-by-resource lookup is resolved by the metadata and series identity indexes")
}

func TestSelectiveAndAbsentSeriesReadCounts(t *testing.T) {
	dir := t.TempDir()
	shared := []int64{100, 200}
	series := []Series{
		NewFloatSeries(labels.FromStrings(labels.MetricName, "read_metric", "series", "one"), shared, []float64{1, 2}),
		NewFloatSeries(labels.FromStrings(labels.MetricName, "read_metric", "series", "two"), shared, []float64{3, 4}),
		NewFloatSeries(labels.FromStrings(labels.MetricName, "read_metric", "series", "other"), []int64{300, 400}, []float64{5, 6}),
	}
	meta, err := WriteBlock(dir, series, WriteOptions{})
	require.NoError(t, err)
	block, err := OpenBlock(filepath.Join(dir, meta.ULID))
	require.NoError(t, err)
	block.reads = &readStats{}

	selected, err := block.Select(t.Context(), 100, 200, [][]*labels.Matcher{{
		labels.MustNewMatcher(labels.MatchEqual, "series", "one"),
	}}, 0)
	require.NoError(t, err)
	require.Len(t, selected, 1)
	require.Equal(t, shared, selected[0].Timestamps)
	statsAfterOne := block.reads
	require.Equal(t, uint64(1), statsAfterOne.samples.Load())
	require.Equal(t, uint64(1), statsAfterOne.rowGroups.Load())
	require.Equal(t, uint64(1), statsAfterOne.sampleRows.Load())
	require.Zero(t, statsAfterOne.labels.Load())

	block.reads.reset()
	selected, err = block.Select(t.Context(), 100, 200, [][]*labels.Matcher{{
		labels.MustNewMatcher(labels.MatchEqual, OTELResourceLabel, "not-associated-with-any-series"),
	}}, 0)
	require.NoError(t, err)
	require.Empty(t, selected)
	require.Zero(t, block.reads.samples.Load(), "mismatched metadata identity does not open arbitrary samples")
	require.Zero(t, block.reads.rowGroups.Load())
	require.Zero(t, block.reads.sampleRows.Load())

	selected, err = block.Select(t.Context(), 100, 200, [][]*labels.Matcher{{
		labels.MustNewMatcher(labels.MatchEqual, "series", "other"),
	}}, 0)
	require.NoError(t, err)
	require.Empty(t, selected)
	require.Zero(t, block.reads.samples.Load(), "segment bounds prune an out-of-range series inside an overlapping block")
	require.Zero(t, block.reads.rowGroups.Load())
}

func TestCohortOptionalColumnsAlignAcrossSegments(t *testing.T) {
	n := DefaultSamplesPerSegment + 1
	timestamps := make([]int64, n)
	kinds := make([]SampleKind, n)
	values := make([]float64, n)
	histograms := make([][]byte, n)
	starts := make([]int64, n)
	for i := range n {
		timestamps[i] = int64(i * 1000)
	}
	kinds[n-1] = SampleHistogram
	payload, err := encodeHistogram(&histogram.Histogram{Count: 1, Sum: 2}, nil)
	require.NoError(t, err)
	histograms[n-1] = payload
	starts[n-1] = 123
	item := Series{Labels: labels.FromStrings(labels.MetricName, "optional_columns"), Timestamps: timestamps, StartTimestamps: starts, Kinds: kinds, Values: values, Histograms: histograms}
	dir := t.TempDir()
	meta, err := WriteBlock(dir, []Series{item}, WriteOptions{})
	require.NoError(t, err)
	block, err := OpenBlock(filepath.Join(dir, meta.ULID))
	require.NoError(t, err)
	got, err := block.Series(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, starts, got[0].StartTimestamps)
	require.Len(t, got[0].Histograms, n)
	require.Equal(t, payload, got[0].Histograms[n-1])
}

func TestCohortSharingUsesExactTimestampEquality(t *testing.T) {
	dir := t.TempDir()
	shared := make([]int64, DefaultSamplesPerSegment+1)
	values := make([]float64, len(shared))
	for i := range shared {
		shared[i] = int64(i * 1000)
		values[i] = float64(i)
	}
	series := []Series{
		NewFloatSeries(labels.FromStrings(labels.MetricName, "cohort_exact", "series", "one"), shared, values),
		NewFloatSeries(labels.FromStrings(labels.MetricName, "cohort_exact", "series", "two"), shared, slices.Clone(values)),
	}
	meta, err := WriteBlock(dir, series, WriteOptions{})
	require.NoError(t, err)
	block, err := OpenBlock(filepath.Join(dir, meta.ULID))
	require.NoError(t, err)
	require.Len(t, block.catalog.Cohorts, 1)
	require.Equal(t, 2, block.catalog.Cohorts[0].NumSeries)
	require.Equal(t, 2, block.catalog.Cohorts[0].NumSegments)
	for _, entry := range block.catalog.Series {
		require.Len(t, entry.Segments, 2)
		require.Equal(t, 0, entry.Segments[0].Row)
		require.Equal(t, 1, entry.Segments[1].Row)
	}
	got, err := block.Series(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, item := range got {
		require.Equal(t, shared, item.Timestamps)
		require.Equal(t, values, item.Values)
	}
}

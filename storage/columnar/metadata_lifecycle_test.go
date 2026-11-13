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

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
)

func TestMetadataChangeOnlyPublishingAndBoundedRestart(t *testing.T) {
	dir := t.TempDir()
	opts := StorageOptions{MetadataVersionCap: 2, IndexedResourceAttributes: []string{"host"}}
	s, err := NewStorage(dir, opts)
	require.NoError(t, err)
	owner := ResourceOwner(map[string]string{"service.name": "changes", "service.instance.id": "one"})
	ls1 := labels.FromStrings(labels.MetricName, "change_one", OTELResourceLabel, owner.Ref())
	ls2 := labels.FromStrings(labels.MetricName, "change_two", OTELResourceLabel, owner.Ref())
	associate := func(ls labels.Labels, timestamp int64, host string) {
		_, err := s.MetadataAssociate(ls, Update{
			Owner: owner, EffectiveFrom: timestamp,
			Content: Content{Attributes: map[string]string{"host": host}},
		})
		require.NoError(t, err)
	}

	associate(ls1, 100, "a")
	require.NoError(t, s.Flush())
	require.Len(t, s.blocks, 1)

	// Re-observing the unchanged context extends the recorded observation bounds.
	// That is an append-only relation delta: it restates the existing change point
	// with the extended bound instead of inventing a version, a membership or a
	// content, and the owner history is not replaced.
	associate(ls1, 200, "a")
	require.NoError(t, s.Flush())
	require.Len(t, s.blocks, 2)
	extension, err := s.blocks[1].readMetadataRelations(t.Context())
	require.NoError(t, err)
	require.False(t, extension.owners[0].replace)
	require.Len(t, extension.owners, 1)
	require.Len(t, extension.contents, 1)
	require.Len(t, extension.versions, 1)
	require.Equal(t, int64(100), extension.versions[0].effectiveFrom)
	require.Equal(t, int64(200), extension.versions[0].observedUntil)
	require.Len(t, extension.memberships, 1)
	require.Equal(t, int64(100), extension.memberships[0].effectiveFrom)
	require.Equal(t, int64(200), extension.memberships[0].observedUntil)

	// A new actual membership carries its owner reference and the owner bound its
	// own observation extended, without re-exporting unchanged content twice.
	associate(ls2, 300, "a")
	require.NoError(t, s.Flush())
	require.Len(t, s.blocks, 3)
	membershipBlock, err := s.blocks[2].readMetadataRelations(t.Context())
	require.NoError(t, err)
	require.Len(t, membershipBlock.owners, 1)
	require.False(t, membershipBlock.owners[0].replace)
	require.Len(t, membershipBlock.versions, 1, "only the extended version row is restated")
	require.Equal(t, int64(300), membershipBlock.versions[0].observedUntil)
	require.Len(t, membershipBlock.contents, 1)
	require.Len(t, membershipBlock.memberships, 1)
	require.Equal(t, int64(300), membershipBlock.memberships[0].effectiveFrom)

	// One new version appends normally beneath the cap.
	associate(ls1, 400, "b")
	require.NoError(t, s.Flush())
	require.Len(t, s.blocks, 4)
	appendedVersion, err := s.blocks[3].readMetadataRelations(t.Context())
	require.NoError(t, err)
	require.Len(t, appendedVersion.versions, 1)
	require.Equal(t, int64(400), appendedVersion.versions[0].effectiveFrom)
	require.False(t, appendedVersion.owners[0].replace)

	// Crossing the cap evicts the old version and its content. This delta carries
	// an explicit full-history replacement and only the two retained values.
	require.NoError(t, s.MetadataUpdate(Update{
		Owner: owner, EffectiveFrom: 500,
		Content: Content{Attributes: map[string]string{"host": "c"}},
	}))
	require.NoError(t, s.Flush())
	require.Len(t, s.blocks, 5)
	replacement, err := s.blocks[4].readMetadataRelations(t.Context())
	require.NoError(t, err)
	require.True(t, replacement.owners[0].replace)
	require.True(t, replacement.owners[0].truncated)
	require.Len(t, replacement.versions, 2)
	require.Len(t, replacement.contents, 2)
	for _, content := range replacement.contents {
		require.NotEqual(t, (Content{Attributes: map[string]string{"host": "a"}}).Canonical(), content.content.Canonical())
	}
	history := s.metadata.byOwner[owner]
	require.Len(t, history.versions, 2)
	require.Equal(t, int64(400), history.versions[0].EffectiveFrom)
	require.Equal(t, int64(500), history.versions[1].EffectiveFrom)
	require.Len(t, history.index, 2, "evicted attribute values are not retained in the inverted-index candidate set")

	// One more observation-only extension after the replacement must be recovered
	// by monotone merge, and must not resurrect the evicted version.
	associate(ls1, 600, "c")
	require.NoError(t, s.Flush())
	require.Len(t, s.blocks, 6)
	lateExtension, err := s.blocks[5].readMetadataRelations(t.Context())
	require.NoError(t, err)
	require.False(t, lateExtension.owners[0].replace)
	require.Len(t, lateExtension.versions, 1)
	require.Equal(t, int64(500), lateExtension.versions[0].effectiveFrom)
	require.Equal(t, int64(600), lateExtension.versions[0].observedUntil)
	require.Equal(t, int64(600), lateExtension.memberships[0].observedUntil)
	require.NoError(t, s.Close())

	s, err = NewStorage(dir, opts)
	require.NoError(t, err)
	defer s.Close()
	recovered, ok := s.MetadataHistory(owner)
	require.True(t, ok)
	require.True(t, recovered.Truncated)
	require.Len(t, recovered.Versions, 2)
	_, ok, truncated := s.MetadataAtLabels(ls1, MetadataResource, 100)
	require.False(t, ok, "the truncated pre-history must not be resurrected by older blocks")
	require.True(t, truncated)
	at400, ok, _ := s.MetadataAtLabels(ls1, MetadataResource, 400)
	require.True(t, ok)
	require.Equal(t, "b", at400.Attributes["host"])
	at500, ok, _ := s.MetadataAtLabels(ls2, MetadataResource, 500)
	require.True(t, ok, "a later-added membership must share the same owner history")
	require.Equal(t, "c", at500.Attributes["host"])
	require.Equal(t, int64(500), recovered.Versions[1].ObservedFrom)
	require.Equal(t, int64(600), recovered.Versions[1].ObservedUntil, "the recovered observation carries the late extension")
	require.Equal(t, int64(400), recovered.Versions[0].ObservedUntil, "an older version keeps its own observation bound")
	memberships, err := s.QueryMetadataSeries(t.Context(), MetadataSeriesQuery{
		Kind: MetadataResource, Start: 0, End: 1000,
	})
	require.NoError(t, err)
	require.Len(t, memberships.Series, 2)
	observedTo := map[string]int64{}
	for _, membership := range memberships.Series {
		observedTo[membership.Labels.Get(labels.MetricName)] = membership.MembershipObservedTo
	}
	require.Equal(t, map[string]int64{"change_one": 600, "change_two": 300}, observedTo,
		"each membership keeps its own observation bound")
}

func TestMetadataConflictDoesNotPublishWALPrefix(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStorage(dir, StorageOptions{})
	require.NoError(t, err)
	defer s.Close()
	first := ResourceOwner(map[string]string{"service.name": "atomic", "service.instance.id": "one"})
	second := ResourceOwner(map[string]string{"service.name": "atomic", "service.instance.id": "two"})
	ls := labels.FromStrings(labels.MetricName, "atomic_metric", OTELResourceLabel, first.Ref())
	_, err = s.MetadataAssociate(ls, Update{
		Owner: first, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"host": "a"}},
	})
	require.NoError(t, err)
	require.NoError(t, s.Flush())

	err = s.AppendBatch(t.Context(), Batch{Updates: []AssociationUpdate{
		{Labels: ls, Update: Update{Owner: first, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"host": "b"}}}},
		{Labels: ls, Update: Update{Owner: second, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"host": "other"}}}},
	}})
	require.ErrorIs(t, err, ErrConflictingMembership)
	history, ok := s.MetadataHistory(first)
	require.True(t, ok)
	require.Len(t, history.Versions, 1, "the first valid association in a rejected batch must not be committed")
	_, ok = s.MetadataHistory(second)
	require.False(t, ok)
	require.Equal(t, uint64(1), s.walSequence, "a rejected metadata transaction writes no WAL segment")
	entries, err := os.ReadDir(walDirForTest(dir))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestDisabledStorageDoesNotInitializeMetadataOrWAL(t *testing.T) {
	dir := t.TempDir()
	opts := StorageOptions{DisableNativeMetadata: true, DisableWAL: true}
	s, err := NewStorage(dir, opts)
	require.NoError(t, err)
	require.Nil(t, s.metadata)
	require.Zero(t, s.walSequence)
	owner := SourceOwner("disabled")
	err = s.MetadataUpdate(Update{
		Owner: owner, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"source": "otap"}},
	})
	require.ErrorIs(t, err, ErrMetadataDisabled)

	ls := labels.FromStrings(labels.MetricName, "disabled_wal_metric")
	require.NoError(t, s.AppendBatch(t.Context(), Batch{Series: []BatchSeries{{
		Labels: ls, Timestamps: []int64{100}, StartTimestamps: []int64{0},
		Kinds: []SampleKind{SampleFloat}, Values: []float64{1}, Histograms: [][]byte{nil},
	}}}))
	require.NoError(t, s.Flush())
	require.Len(t, s.blocks, 1)
	_, err = os.Stat(walDirForTest(dir))
	require.ErrorIs(t, err, os.ErrNotExist, "WAL-disabled samples must not create the directory")
	q, err := s.Querier(0, 200)
	require.NoError(t, err)
	resourceQuerier := q.(storage.ResourceQuerier)
	_, ok := resourceQuerier.GetResourceAt(labels.StableHash(ls), 100)
	require.False(t, ok)
	called := false
	require.NoError(t, resourceQuerier.IterUniqueAttributeNames(func(string) { called = true }))
	require.False(t, called)
	membership := q.(storage.ResourceMembershipQuerier)
	require.False(t, membership.HasResourceMembershipAt(labels.StableHash(ls), 100))
	require.NoError(t, q.Close())
	require.NoError(t, s.Close())

	s, err = NewStorage(dir, opts)
	require.NoError(t, err)
	defer s.Close()
	require.Nil(t, s.metadata)
	series, err := s.SelectSeries(t.Context(), 0, 200, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []int64{100}, series[0].Timestamps)
}

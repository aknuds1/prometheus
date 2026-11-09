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
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
)

func TestMetadataOnlyOwnerVersionBlock(t *testing.T) {
	s, err := NewStorage(t.TempDir(), StorageOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	owner := ResourceOwner(map[string]string{"service.name": "metadata-only"})
	content := Content{Attributes: map[string]string{"host": "a", "region": "east"}}
	require.NoError(t, s.MetadataUpdate(Update{Owner: owner, Content: content, EffectiveFrom: 100}))

	got, ok, truncated := s.MetadataAtOwner(owner, 99)
	require.False(t, ok)
	require.False(t, truncated)
	require.Empty(t, got.Attributes)
	got, ok, truncated = s.MetadataAtOwner(owner, 100)
	require.True(t, ok)
	require.False(t, truncated)
	require.Equal(t, content.Canonical(), got.Canonical())
	_, ok = s.SeriesMetadataAssociation(labels.StableHash(labels.FromStrings("__name__", "unassociated")), MetadataResource, 100)
	require.False(t, ok)

	require.NoError(t, s.Flush())
	require.Equal(t, 1, s.NumBlocks())
	blocks, err := ListBlocks(s.dir)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	block := blocks[0]
	require.Zero(t, block.meta.NumSamples)
	require.Zero(t, block.meta.NumSeries)
	require.False(t, block.Overlaps(99, 200))
	samples, err := block.Series(t.Context())
	require.NoError(t, err)
	require.Empty(t, samples)

	relations, err := block.readMetadataRelations(t.Context())
	require.NoError(t, err)
	require.Equal(t, &MetadataSummary{
		NumOwners: 1, NumContents: 1, NumVersions: 1,
		MinEffectiveFrom: 100, MaxEffectiveFrom: 100,
		Kinds: []string{"resource"}, AttributeNames: []string{"host", "region"},
	}, block.meta.Metadata)
	require.Len(t, relations.owners, 1)
	require.Equal(t, owner, relations.owners[0].key)
	require.Len(t, relations.contents, 1)
	require.Equal(t, content.Canonical(), relations.contents[0].content.Canonical())
	require.Len(t, relations.versions, 1)
	require.Equal(t, relations.owners[0].id, relations.versions[0].ownerID)
	require.Equal(t, relations.contents[0].id, relations.versions[0].contentID)
	require.Empty(t, relations.memberships)
}

func TestMetadataTenThousandMembersShareOwnerVersions(t *testing.T) {
	const memberCount = 10_000
	s, err := NewStorage(t.TempDir(), StorageOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	owner := ResourceOwner(map[string]string{"service.name": "payments", "service.instance.id": "pay01"})
	labelSets := make([]labels.Labels, memberCount)
	for i := range memberCount {
		labelSets[i] = labels.FromStrings(
			labels.MetricName, fmt.Sprintf("payments_%d", i),
			OTELResourceLabel, owner.Ref(),
		)
		_, err := s.MetadataAssociate(labelSets[i], Update{
			Owner: owner, EffectiveFrom: 100,
			Content: Content{Attributes: map[string]string{"host": "a", "version": "1"}},
		})
		require.NoError(t, err)
	}

	// One actual Resource change creates one version, irrespective of membership
	// count. No content or version row is copied into each member relation.
	_, err = s.MetadataAssociate(labelSets[0], Update{
		Owner: owner, EffectiveFrom: 120,
		Content: Content{Attributes: map[string]string{"host": "b", "version": "1"}},
	})
	require.NoError(t, err)
	history, ok := s.MetadataHistory(owner)
	require.True(t, ok)
	require.Len(t, history.Versions, 2)
	require.Equal(t, int64(100), history.Versions[0].EffectiveFrom)
	require.Equal(t, int64(120), history.Versions[1].EffectiveFrom)

	for _, member := range []int{0, memberCount / 2, memberCount - 1} {
		before, ok, _ := s.MetadataAtLabels(labelSets[member], MetadataResource, 119)
		require.True(t, ok)
		require.Equal(t, "a", before.Attributes["host"])
		after, ok, _ := s.MetadataAtLabels(labelSets[member], MetadataResource, 120)
		require.True(t, ok)
		require.Equal(t, "b", after.Attributes["host"])
	}

	require.NoError(t, s.Flush())
	blocks, err := ListBlocks(s.dir)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	require.Equal(t, &MetadataSummary{
		NumOwners: 1, NumContents: 2, NumVersions: 2, NumMemberships: memberCount,
		MinEffectiveFrom: 100, MaxEffectiveFrom: 120,
		Kinds: []string{"resource"}, AttributeNames: []string{"host", "version"},
	}, blocks[0].meta.Metadata)

	relations, err := blocks[0].readMetadataRelations(t.Context())
	require.NoError(t, err)
	require.Len(t, relations.owners, 1)
	require.Len(t, relations.contents, 2)
	require.Len(t, relations.versions, 2)
	require.Len(t, relations.memberships, memberCount)
	for _, membership := range relations.memberships {
		require.Equal(t, relations.owners[0].id, membership.ownerID)
	}
}

func TestMetadataRelationsCoverAllOwnerKinds(t *testing.T) {
	s, err := NewStorage(t.TempDir(), StorageOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	resource := ResourceOwner(map[string]string{"service.name": "shared"})
	owners := []OwnerKey{
		resource,
		ResourceSchemaOwner(resource),
		MetricOwner(labels.FromStrings(labels.MetricName, "requests_total")),
		ScopeOwner("sdk", "v1"),
		ScrapeOwner("job-a"),
		SourceOwner("receiver-a"),
	}
	shared := Content{Attributes: map[string]string{"shared": "yes"}}
	for _, owner := range owners {
		require.NoError(t, s.MetadataUpdate(Update{Owner: owner, Content: shared, EffectiveFrom: 200}))
	}
	require.NoError(t, s.Flush())

	blocks, err := ListBlocks(s.dir)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	require.Equal(t, KindNames[:], blocks[0].meta.Metadata.Kinds)
	relations, err := blocks[0].readMetadataRelations(t.Context())
	require.NoError(t, err)
	require.Len(t, relations.owners, len(owners))
	require.Len(t, relations.contents, 1, "equal content may be shared by otherwise distinct owners")
	require.Len(t, relations.versions, len(owners))
	require.Empty(t, relations.memberships)

	decodedOwners := make([]OwnerKey, len(relations.owners))
	for i, owner := range relations.owners {
		decodedOwners[i] = owner.key
	}
	slices.SortFunc(decodedOwners, func(a, b OwnerKey) int {
		switch {
		case a.Kind < b.Kind:
			return -1
		case a.Kind > b.Kind:
			return 1
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		default:
			return 0
		}
	})
	for _, owner := range owners {
		require.Contains(t, decodedOwners, owner)
		content, ok, _ := s.MetadataAtOwner(owner, 200)
		require.True(t, ok)
		require.Equal(t, shared.Canonical(), content.Canonical())
	}
}

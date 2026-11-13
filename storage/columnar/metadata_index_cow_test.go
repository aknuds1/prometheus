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
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOwnerCardinalityDescriptiveIndexCOW proves that a configured descriptive
// Resource index obeys the same once-per-transaction copy barrier as the
// identity-derived conditions, and that the indexed attribute resolves the
// associated series.
func TestOwnerCardinalityDescriptiveIndexCOW(t *testing.T) {
	spec := ownerCardinalitySpec{
		mode: ownerCardinalityVersionAndAssociations, records: 4, points: 4, owners: 4, timed: 3,
	}
	fx := buildOwnerCardinalityFixture(t, spec)
	st, err := NewStorage(t.TempDir(), StorageOptions{
		FlushSamples: 1 << 40, DisableWAL: true, MetadataVersionCap: -1,
		IndexedResourceAttributes: []string{"deployment.environment"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	require.NoError(t, st.AppendBatch(context.Background(), fx.entries[0].batch))
	for e := 1; e <= spec.timed; e++ {
		before := st.reads.metadataWorkSnapshot()
		require.NoError(t, st.AppendBatch(context.Background(), fx.entries[e].batch))
		work := st.reads.metadataWorkSnapshot().since(before)
		// Two shared identity buckets, one singleton per owner, and the environment
		// values retained by the histories plus the newly inserted one are copied
		// once each. Every environment-value bucket holds every resident owner.
		require.Equal(t, spec.owners+e+4, work.indexBucketClones,
			"entry %d copies each touched descriptive and identity condition once", e)
		require.Equal(t, spec.owners*(e+4), work.indexBucketClonedEntries,
			"entry %d copies the shared descriptive buckets once", e)
	}
	verifyOwnerCardinalityState(t, st, fx, spec.timed)

	// env-2 was effective at mid(1) and superseded at mid(2); the configured
	// attribute resolves the associated sum series at that instant.
	st.reads.reset()
	page, err := st.QueryMetadataSeries(context.Background(), MetadataSeriesQuery{
		Kind: MetadataResource, Start: spec.mid(1), End: spec.mid(1),
		Attributes: []MetadataAttributeCondition{{Name: "deployment.environment", Value: "env-2"}},
	})
	require.NoError(t, err)
	require.Len(t, page.Series, spec.records, "every associated record carries env-2 at mid(1)")
	for _, result := range page.Series {
		require.Len(t, result.Versions, 1, "only the env-2 version overlaps the queried instant")
		require.Equal(t, "env-2", result.Versions[0].Descriptive["deployment.environment"])
	}
	stats := st.ReadStats()
	require.Equal(t, uint64(spec.owners), stats.MetadataIndexCandidates,
		"the configured attribute resolves candidates through the index")
	require.Zero(t, stats.MetadataIndexFallbacks, "the configured attribute is indexed")
}

// TestResourceIndexReconfigurationOnClonedState proves that reconfiguring the
// descriptive index on an already cloned published catalog rebuilds fresh buckets and
// that the next transaction still copies every bucket it touches exactly once.
func TestResourceIndexReconfigurationOnClonedState(t *testing.T) {
	st, err := NewStorage(t.TempDir(), StorageOptions{
		FlushSamples: 1 << 40, DisableWAL: true, MetadataVersionCap: -1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	alpha := ResourceOwner(map[string]string{"service.name": "checkout", "service.instance.id": "one"})
	beta := ResourceOwner(map[string]string{"service.name": "checkout", "service.instance.id": "two"})
	content := func(environment string) Content {
		return Content{Attributes: map[string]string{
			"service.name": "checkout", "deployment.environment": environment,
		}}
	}
	appendOwners := func(effectiveFrom int64, alphaValue, betaValue string) metadataWork {
		before := st.reads.metadataWorkSnapshot()
		require.NoError(t, st.AppendBatch(context.Background(), Batch{OwnerUpdates: []Update{
			{Owner: alpha, EffectiveFrom: effectiveFrom, Content: content(alphaValue)},
			{Owner: beta, EffectiveFrom: effectiveFrom, Content: content(betaValue)},
		}}))
		return st.reads.metadataWorkSnapshot().since(before)
	}
	require.NoError(t, st.AppendBatch(context.Background(), Batch{OwnerUpdates: []Update{
		{Owner: alpha, EffectiveFrom: 50, Content: content("h0")},
	}}))
	require.NoError(t, st.AppendBatch(context.Background(), Batch{OwnerUpdates: []Update{
		{Owner: beta, EffectiveFrom: 50, Content: content("h0")},
	}}))

	first := appendOwners(100, "h1", "h1")
	require.Equal(t, 4, first.indexBucketClones, "only identity conditions are indexed before reconfiguration")
	require.Equal(t, 6, first.indexBucketClonedEntries)

	require.NoError(t, st.SetIndexedResourceAttrs([]string{"deployment.environment"}))
	published := st.metadata
	h0 := attributeCondition("deployment.environment", "h0")
	h1 := attributeCondition("deployment.environment", "h1")
	require.Len(t, published.attributeIndex[h0], 2, "the rebuild indexes every retained descriptive value")
	require.Len(t, published.attributeIndex[h1], 2, "the rebuild indexes the configured attribute")

	second := appendOwners(200, "h3", "h4")
	require.Equal(t, 8, second.indexBucketClones,
		"identity and configured conditions are each first-touched once")
	require.Equal(t, 10, second.indexBucketClonedEntries)

	require.Len(t, published.byOwner[alpha].versions, 2, "the published history is not mutated")
	require.Len(t, published.byOwner[beta].versions, 2)
	require.Len(t, published.attributeIndex[h0], 2, "the published bucket is not mutated")
	require.Len(t, published.attributeIndex[h1], 2)
	_, publishedH3 := published.attributeIndex[attributeCondition("deployment.environment", "h3")]
	require.False(t, publishedH3, "the published index gains no new condition")

	history, ok := st.MetadataHistory(alpha)
	require.True(t, ok)
	require.Len(t, history.Versions, 3)
	require.Equal(t, "h3", history.Versions[2].Content.Attributes["deployment.environment"])
	candidates, err := st.metadata.ownersMatchingAttributes(MetadataResource,
		[]MetadataAttributeCondition{{Name: "deployment.environment", Value: "h3"}}, 0, 300)
	require.NoError(t, err)
	require.Equal(t, map[OwnerKey]struct{}{alpha: {}}, candidates)
}

// TestResourceIndexBackfillEvictionAndRecreation proves that coalescing and a version
// cap leave no stale or resurrected index candidate, and that re-observing an evicted
// value indexes it again as a new version.
func TestResourceIndexBackfillEvictionAndRecreation(t *testing.T) {
	st, err := NewStorage(t.TempDir(), StorageOptions{
		FlushSamples: 1 << 40, DisableWAL: true, MetadataVersionCap: 3,
		IndexedResourceAttributes: []string{"deployment.environment"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	owner := ResourceOwner(map[string]string{"service.name": "checkout", "service.instance.id": "one"})
	condition := func(value string) string { return attributeCondition("deployment.environment", value) }
	update := func(effectiveFrom int64, value string) metadataWork {
		before := st.reads.metadataWorkSnapshot()
		require.NoError(t, st.MetadataUpdate(Update{
			Owner: owner, EffectiveFrom: effectiveFrom,
			Content: Content{Attributes: map[string]string{"deployment.environment": value}},
		}))
		return st.reads.metadataWorkSnapshot().since(before)
	}
	effectiveTimes := func() []int64 {
		history, ok := st.MetadataHistory(owner)
		require.True(t, ok)
		times := make([]int64, 0, len(history.Versions))
		for _, version := range history.Versions {
			times = append(times, version.EffectiveFrom)
		}
		return times
	}
	candidates := func(value string, start, end int64) map[OwnerKey]struct{} {
		found, err := st.metadata.ownersMatchingAttributes(MetadataResource,
			[]MetadataAttributeCondition{{Name: "deployment.environment", Value: value}}, start, end)
		require.NoError(t, err)
		return found
	}

	update(100, "a")
	update(200, "b")
	update(300, "c")
	backfill := update(150, "b")
	require.Equal(t, 1, backfill.versionsCoalesced,
		"the backfilled change point supersedes its equal successor")
	require.Equal(t, []int64{100, 150, 300}, effectiveTimes())

	evicted := update(400, "d")
	require.Equal(t, 1, evicted.versionsEvicted, "the cap evicts the oldest version")
	require.Equal(t, []int64{150, 300, 400}, effectiveTimes())
	history, ok := st.MetadataHistory(owner)
	require.True(t, ok)
	require.True(t, history.Truncated, "eviction is reported rather than silent")
	_, stale := st.metadata.attributeIndex[condition("a")]
	require.False(t, stale, "the evicted value leaves the global index")
	_, retired := st.metadata.ownerIndex[owner][condition("a")]
	require.False(t, retired, "the evicted value leaves the owner index")
	require.Empty(t, candidates("a", 0, 500), "no stale candidate is resurrected")
	for _, value := range []string{"b", "c", "d"} {
		require.Equal(t, map[OwnerKey]struct{}{owner: {}}, candidates(value, 0, 500),
			"retained value %q stays indexed", value)
	}

	reobserved := update(500, "a")
	require.Equal(t, 1, reobserved.versionsEvicted, "the cap evicts the next oldest version")
	require.Equal(t, []int64{300, 400, 500}, effectiveTimes())
	require.Equal(t, map[OwnerKey]struct{}{owner: {}}, candidates("a", 0, 600),
		"re-observing the evicted value indexes it again")
	_, resurrected := st.metadata.attributeIndex[condition("b")]
	require.False(t, resurrected, "the superseded value does not linger")
	content, found, truncated := st.MetadataAtOwner(owner, 500)
	require.True(t, found)
	require.True(t, truncated)
	require.Equal(t, "a", content.Attributes["deployment.environment"])
	_, found, truncated = st.MetadataAtOwner(owner, 100)
	require.False(t, found, "the evicted version is not silently returned")
	require.True(t, truncated)
}

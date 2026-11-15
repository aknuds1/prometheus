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
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalVersionCacheUsesOwnerAndEviction(t *testing.T) {
	store := newMetadataStore(1)
	first := ResourceOwner(map[string]string{"service.name": "first"})
	second := ResourceOwner(map[string]string{"service.name": "second"})
	require.NoError(t, store.update(Update{Owner: first, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"host": "a"}}}))
	_ = store.snapshot()
	require.Len(t, store.canonicalVersions, 1)
	require.NoError(t, store.update(Update{Owner: first, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"host": "b"}}}))
	require.NoError(t, store.update(Update{Owner: second, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"host": "b"}}}))
	_ = store.snapshot()
	require.Len(t, store.canonicalVersions, 2)
	_, expired := store.canonicalVersions[metadataVersionKey{owner: first, effectiveFrom: 100}]
	require.False(t, expired)
	history, ok := store.byOwner[first]
	require.True(t, ok)
	require.True(t, history.truncated)
	store.deleteOwner(first)
	require.Len(t, store.canonicalVersions, 1)
	_, ok = store.byOwner[first]
	require.False(t, ok)
}

func TestMetadataCOWOwnerHistoryIsolation(t *testing.T) {
	base := newMetadataStore(5)
	owner := ResourceOwner(map[string]string{"service.name": "cow"})
	require.NoError(t, base.update(Update{Owner: owner, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"host": "a"}}}))
	candidate, err := base.clone(nil)
	require.NoError(t, err)
	history := candidate.ownerHistory(owner, nil)
	require.NotSame(t, base.byOwner[owner], history)
	require.NoError(t, candidate.update(Update{Owner: owner, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"host": "b"}}}))
	require.Len(t, base.byOwner[owner].versions, 1)
	require.Equal(t, "a", base.byOwner[owner].versions[0].Content.Attributes["host"])
}

func TestMetadataAttributeIndexCopiedOncePerTransaction(t *testing.T) {
	base := newMetadataStore(0)
	alpha := ResourceOwner(map[string]string{"service.name": "checkout", "service.instance.id": "one"})
	beta := ResourceOwner(map[string]string{"service.name": "checkout", "service.instance.id": "two"})
	gamma := ResourceOwner(map[string]string{"service.name": "other", "service.instance.id": "one"})
	for _, owner := range []OwnerKey{alpha, beta, gamma} {
		require.NoError(t, base.update(Update{
			Owner: owner, EffectiveFrom: 100,
			Content: Content{Attributes: map[string]string{"slot": owner.ID}},
		}))
	}
	shared := attributeCondition("service.name", "checkout")
	require.Len(t, base.attributeIndex[shared], 2, "the shared bucket holds both checkout owners")

	insert := func(store *metadataStore, work *metadataWork, owner OwnerKey, effectiveFrom int64, slot string) {
		t.Helper()
		require.NoError(t, store.updateHistory(store.ownerHistory(owner, work), Update{
			Owner: owner, EffectiveFrom: effectiveFrom,
			Content: Content{Attributes: map[string]string{"slot": slot}},
		}, associationDerivation{}, nil, work))
	}

	first, err := base.clone(nil)
	require.NoError(t, err)
	var firstWork metadataWork
	// Three versions of one owner and one version of another in a single transaction:
	// the four conditions they touch are copied once each, not once per mutation.
	insert(first, &firstWork, alpha, 110, "a1")
	insert(first, &firstWork, alpha, 120, "a2")
	insert(first, &firstWork, alpha, 130, "a3")
	insert(first, &firstWork, beta, 140, "b1")
	require.Equal(t, 4, firstWork.indexCalls)
	require.Equal(t, 4, firstWork.indexBucketClones)
	require.Equal(t, 8, firstWork.indexBucketClonedEntries)

	require.NotEqual(t, reflect.ValueOf(base.attributeIndex[shared]).Pointer(),
		reflect.ValueOf(first.attributeIndex[shared]).Pointer(), "the transaction owns its bucket")
	require.Len(t, base.byOwner[alpha].versions, 1, "the published history is untouched")
	require.Len(t, base.byOwner[beta].versions, 1)
	require.Len(t, base.attributeIndex[shared], 2, "the published bucket is untouched")
	require.Equal(t, int64(100), base.attributeIndex[shared][alpha].from)

	// A second catalog clone must not inherit the first transaction's ownership: it
	// copies each bucket it touches again and leaves both predecessors alone.
	second, err := base.clone(nil)
	require.NoError(t, err)
	var secondWork metadataWork
	insert(second, &secondWork, alpha, 210, "a4")
	require.Equal(t, 3, secondWork.indexBucketClones, "a fresh transaction copies its first-touch buckets")
	require.Equal(t, 7, secondWork.indexBucketClonedEntries)
	require.NotEqual(t, reflect.ValueOf(first.attributeIndex[shared]).Pointer(),
		reflect.ValueOf(second.attributeIndex[shared]).Pointer())
	require.Len(t, base.byOwner[alpha].versions, 1)
	require.Len(t, first.byOwner[alpha].versions, 4)
	require.Len(t, second.byOwner[alpha].versions, 2)
}

func TestMetadataAttributeIndexRecreatedBucketIsNotPublishedState(t *testing.T) {
	base := newMetadataStore(1)
	base.setIndexedResourceAttrs([]string{"env"})
	alpha := ResourceOwner(map[string]string{"service.name": "a", "service.instance.id": "one"})
	beta := ResourceOwner(map[string]string{"service.name": "b", "service.instance.id": "two"})
	require.NoError(t, base.update(Update{Owner: alpha, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"env": "x"}}}))
	require.NoError(t, base.update(Update{Owner: beta, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"env": "y"}}}))
	xBucket := attributeCondition("env", "x")
	require.Len(t, base.attributeIndex[xBucket], 1, "only alpha carries env=x")

	tx, err := base.clone(nil)
	require.NoError(t, err)
	var work metadataWork
	// The cap-one history evicts alpha's env=x when env=z arrives, so the emptied
	// bucket is deleted. Beta then takes env=x in the same transaction, which must
	// create a new map rather than reuse the published one.
	require.NoError(t, tx.updateHistory(tx.ownerHistory(alpha, &work), Update{
		Owner: alpha, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"env": "z"}},
	}, associationDerivation{}, nil, &work))
	_, present := tx.attributeIndex[xBucket]
	require.False(t, present, "the emptied bucket is deleted from the transaction")
	require.NoError(t, tx.updateHistory(tx.ownerHistory(beta, &work), Update{
		Owner: beta, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"env": "x"}},
	}, associationDerivation{}, nil, &work))
	require.Equal(t, 8, work.indexBucketClones)
	require.Equal(t, 8, work.indexBucketClonedEntries)

	require.Len(t, tx.attributeIndex[xBucket], 1)
	_, ownedByBeta := tx.attributeIndex[xBucket][beta]
	require.True(t, ownedByBeta, "the recreated bucket names its new owner")
	_, ownedByAlpha := tx.attributeIndex[xBucket][alpha]
	require.False(t, ownedByAlpha)
	require.NotEqual(t, reflect.ValueOf(base.attributeIndex[xBucket]).Pointer(),
		reflect.ValueOf(tx.attributeIndex[xBucket]).Pointer())
	require.Len(t, base.attributeIndex[xBucket], 1, "the published bucket is untouched")
	_, publishedByAlpha := base.attributeIndex[xBucket][alpha]
	require.True(t, publishedByAlpha)
	require.Len(t, base.byOwner[alpha].versions, 1)
	require.Len(t, base.byOwner[beta].versions, 1)
}

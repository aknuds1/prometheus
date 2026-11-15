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
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// capacityOwner returns the owner used by the version-capacity tests.
func capacityOwner() OwnerKey {
	return ResourceOwner(map[string]string{"service.name": "capacity", "service.instance.id": "one"})
}

// capacityContent returns distinct descriptive content for one version.
func capacityContent(version int) Content {
	return Content{Attributes: map[string]string{
		"deployment.environment": fmt.Sprintf("env-%d", version),
	}}
}

// capacityStore returns a store with the owner's versions at the given effective
// times, built through normal non-COW construction.
func capacityStore(t *testing.T, effectiveFrom ...int64) *metadataStore {
	t.Helper()
	store := newMetadataStore(0)
	for _, at := range effectiveFrom {
		require.NoError(t, store.update(Update{
			Owner: capacityOwner(), EffectiveFrom: at, Content: capacityContent(int(at)),
		}))
	}
	return store
}

// versionEffectiveTimes returns the ordered effective times of versions.
func versionEffectiveTimes(versions []Version) []int64 {
	times := make([]int64, len(versions))
	for i, version := range versions {
		times[i] = version.EffectiveFrom
	}
	return times
}

// versionEnvironments returns the descriptive content of versions in order.
func versionEnvironments(versions []Version) []string {
	environments := make([]string, len(versions))
	for i, version := range versions {
		environments[i] = version.Content.Attributes["deployment.environment"]
	}
	return environments
}

// TestMetadataCOWVersionCopyKeepsOneSpareSlot proves the copy-on-write clone
// preserves the slice's nil-ness, length, order and contents while gaining one
// spare capacity slot, and that it never aliases the published array.
func TestMetadataCOWVersionCopyKeepsOneSpareSlot(t *testing.T) {
	t.Run("existing history", func(t *testing.T) {
		base := capacityStore(t, 100, 200)
		owner := capacityOwner()
		published := base.byOwner[owner]
		original := slices.Clone(published.versions)

		tx, err := base.clone(nil)
		require.NoError(t, err)
		copied := tx.ownerHistory(owner, nil)

		require.NotSame(t, published, copied)
		require.Len(t, copied.versions, len(published.versions))
		require.GreaterOrEqual(t, cap(copied.versions), len(copied.versions)+1,
			"the copy keeps one spare slot for the transaction's first insert")
		require.Equal(t, original, copied.versions, "length, order and contents are preserved")
		require.Equal(t, []int64{100, 200}, versionEffectiveTimes(copied.versions))
		require.Equal(t, []string{"env-100", "env-200"}, versionEnvironments(copied.versions))
		require.NotEqual(t, reflect.ValueOf(published.versions).Pointer(),
			reflect.ValueOf(copied.versions).Pointer(), "the transaction owns its backing array")

		copied.versions[0].EffectiveFrom = 42
		require.Equal(t, int64(100), published.versions[0].EffectiveFrom,
			"the published element is untouched")
	})

	t.Run("nil history", func(t *testing.T) {
		base := newMetadataStore(0)
		tx, err := base.clone(nil)
		require.NoError(t, err)
		copied := tx.ownerHistory(capacityOwner(), nil)

		require.True(t, copied.copyTouched)
		require.Nil(t, copied.versions, "a nil history stays nil")
		require.Empty(t, copied.versions)
		require.Zero(t, cap(copied.versions))
	})

	t.Run("empty non-nil history", func(t *testing.T) {
		base := newMetadataStore(0)
		owner := capacityOwner()
		base.byOwner[owner] = &metadataOwnerHistory{versions: []Version{}, index: map[string]struct{}{}}
		tx, err := base.clone(nil)
		require.NoError(t, err)
		copied := tx.ownerHistory(owner, nil)

		require.NotNil(t, copied.versions, "a non-nil empty history stays non-nil")
		require.Empty(t, copied.versions)
		require.GreaterOrEqual(t, cap(copied.versions), 1)
	})
}

// TestMetadataCOWVersionInsertUsesOrExceedsSpareSlot proves the first insert
// uses the spare slot, a backfill insert shifts without growing, a second insert
// grows normally without loss, and an observation-only mutation adds no version.
func TestMetadataCOWVersionInsertUsesOrExceedsSpareSlot(t *testing.T) {
	insert := func(t *testing.T, store *metadataStore, work *metadataWork, history *metadataOwnerHistory, at int64) {
		t.Helper()
		require.NoError(t, store.updateHistory(history, Update{
			Owner: capacityOwner(), EffectiveFrom: at, Content: capacityContent(int(at)),
		}, associationDerivation{}, nil, work))
	}

	t.Run("first tail insert", func(t *testing.T) {
		base := capacityStore(t, 100, 200)
		owner := capacityOwner()
		tx, err := base.clone(nil)
		require.NoError(t, err)
		var work metadataWork
		history := tx.ownerHistory(owner, &work)

		insert(t, tx, &work, history, 300)

		require.Equal(t, 1, work.ownerCOPCopies)
		require.Equal(t, 2, work.ownerCOPVersionRows)
		require.Equal(t, 2, work.versionInsertPreRows)
		require.Equal(t, 1, work.versionsInserted)
		require.Equal(t, 1, work.versionInsertTailPositions)
		require.Zero(t, work.versionInsertShiftedRows)
		require.Zero(t, work.versionInsertGrowths, "the spare slot absorbs the first insert")
		require.Equal(t, len(history.versions), cap(history.versions), "the spare slot is consumed")
		require.Equal(t, []int64{100, 200, 300}, versionEffectiveTimes(history.versions))
		require.Equal(t, []string{"env-100", "env-200", "env-300"}, versionEnvironments(history.versions))
		require.Equal(t, []int64{100, 200}, versionEffectiveTimes(base.byOwner[owner].versions),
			"the published history is unchanged")
	})

	t.Run("backfill insert", func(t *testing.T) {
		base := capacityStore(t, 100, 300)
		owner := capacityOwner()
		tx, err := base.clone(nil)
		require.NoError(t, err)
		var work metadataWork
		history := tx.ownerHistory(owner, &work)

		insert(t, tx, &work, history, 200)

		require.Equal(t, 1, work.versionsInserted)
		require.Zero(t, work.versionInsertTailPositions)
		require.Equal(t, 1, work.versionInsertShiftedRows, "the successor row is shifted once")
		require.Zero(t, work.versionInsertGrowths)
		require.Equal(t, []int64{100, 200, 300}, versionEffectiveTimes(history.versions))
		require.Equal(t, []string{"env-100", "env-200", "env-300"}, versionEnvironments(history.versions),
			"shifting preserves every complete value")
		require.Equal(t, []int64{100, 300}, versionEffectiveTimes(base.byOwner[owner].versions))
	})

	t.Run("second insert", func(t *testing.T) {
		base := capacityStore(t, 100, 200)
		owner := capacityOwner()
		tx, err := base.clone(nil)
		require.NoError(t, err)
		var work metadataWork
		history := tx.ownerHistory(owner, &work)

		insert(t, tx, &work, history, 300)
		require.Zero(t, work.versionInsertGrowths)
		insert(t, tx, &work, history, 400)

		require.Equal(t, 2, work.versionsInserted)
		require.Equal(t, 2, work.versionInsertTailPositions)
		require.Equal(t, 1, work.versionInsertGrowths, "the second insert exceeds the spare slot and grows")
		require.Len(t, history.versions, 4)
		require.GreaterOrEqual(t, cap(history.versions), 4)
		require.Equal(t, []int64{100, 200, 300, 400}, versionEffectiveTimes(history.versions))
		require.Equal(t, []string{"env-100", "env-200", "env-300", "env-400"}, versionEnvironments(history.versions))
		require.Equal(t, []int64{100, 200}, versionEffectiveTimes(base.byOwner[owner].versions))
	})

	t.Run("observation only", func(t *testing.T) {
		base := capacityStore(t, 100)
		owner := capacityOwner()
		tx, err := base.clone(nil)
		require.NoError(t, err)
		var work metadataWork
		history := tx.ownerHistory(owner, &work)
		require.GreaterOrEqual(t, cap(history.versions), len(history.versions)+1,
			"the copy carries the spare slot even though nothing is inserted")

		require.NoError(t, tx.updateHistory(history, Update{
			Owner: owner, EffectiveFrom: 150, Content: capacityContent(100),
		}, associationDerivation{}, nil, &work))

		require.Zero(t, work.versionsInserted)
		require.Zero(t, work.versionInsertGrowths)
		require.Equal(t, 1, work.versionBoundsExtended)
		require.Len(t, history.versions, 1)
		require.Equal(t, int64(100), history.versions[0].ObservedFrom)
		require.Equal(t, int64(150), history.versions[0].ObservedUntil)
		require.Equal(t, []int64{100}, versionEffectiveTimes(base.byOwner[owner].versions))
		require.Equal(t, int64(100), base.byOwner[owner].versions[0].ObservedUntil,
			"the published observation bounds are unchanged")
	})
}

// TestMetadataCOWVersionOwnershipIsFreshPerTransaction proves two consecutive
// transactions each copy the published version slice, each obtain their own
// spare slot, and neither reuses the other's writable state.
func TestMetadataCOWVersionOwnershipIsFreshPerTransaction(t *testing.T) {
	base := capacityStore(t, 100)
	owner := capacityOwner()

	first, err := base.clone(nil)
	require.NoError(t, err)
	var firstWork metadataWork
	firstHistory := first.ownerHistory(owner, &firstWork)
	require.NoError(t, first.updateHistory(firstHistory, Update{
		Owner: owner, EffectiveFrom: 200, Content: capacityContent(200),
	}, associationDerivation{}, nil, &firstWork))
	require.Equal(t, 1, firstWork.ownerCOPCopies)
	require.Zero(t, firstWork.versionInsertGrowths)

	second, err := first.clone(nil)
	require.NoError(t, err)
	var secondWork metadataWork
	secondHistory := second.ownerHistory(owner, &secondWork)

	require.NotSame(t, firstHistory, secondHistory)
	require.NotEqual(t, reflect.ValueOf(firstHistory.versions).Pointer(),
		reflect.ValueOf(secondHistory.versions).Pointer(),
		"the next transaction copies the published slice again")
	require.GreaterOrEqual(t, cap(secondHistory.versions), len(secondHistory.versions)+1,
		"each transaction reestablishes its own spare slot")
	require.Equal(t, []int64{100, 200}, versionEffectiveTimes(secondHistory.versions))

	require.NoError(t, second.updateHistory(secondHistory, Update{
		Owner: owner, EffectiveFrom: 300, Content: capacityContent(300),
	}, associationDerivation{}, nil, &secondWork))
	require.Equal(t, 1, secondWork.ownerCOPCopies)
	require.Zero(t, secondWork.versionInsertGrowths)

	require.Equal(t, []int64{100, 200}, versionEffectiveTimes(firstHistory.versions),
		"the first transaction is not mutated by the second")
	require.Equal(t, []int64{100, 200, 300}, versionEffectiveTimes(secondHistory.versions))
	require.Equal(t, []int64{100}, versionEffectiveTimes(base.byOwner[owner].versions),
		"the published history is unchanged")
}

// TestMetadataCOWVersionFirstInsertDoesNotGrowThroughAppendBatch proves the
// mechanism on the real transaction path: after an owner exists, every commit's
// first version insert uses the COW copy's spare slot, while creation of a new
// owner still grows its nil history.
func TestMetadataCOWVersionFirstInsertDoesNotGrowThroughAppendBatch(t *testing.T) {
	st, err := NewStorage(t.TempDir(), StorageOptions{
		FlushSamples: 1 << 40, DisableWAL: true, MetadataVersionCap: -1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	owner := capacityOwner()

	appendVersion := func(at int64) metadataWork {
		before := st.reads.metadataWorkSnapshot()
		require.NoError(t, st.AppendBatch(context.Background(), Batch{OwnerUpdates: []Update{{
			Owner: owner, EffectiveFrom: at, Content: capacityContent(int(at)),
		}}}))
		return st.reads.metadataWorkSnapshot().since(before)
	}

	created := appendVersion(100)
	require.Equal(t, 1, created.versionsInserted)
	require.Equal(t, 1, created.versionInsertGrowths,
		"a new owner's nil history is preserved and still grows on creation")

	for _, at := range []int64{200, 300} {
		work := appendVersion(at)
		require.Equal(t, 1, work.versionsInserted)
		require.Equal(t, 1, work.ownerCOPCopies)
		require.Zero(t, work.versionInsertGrowths,
			"the first insert into an existing owner's COW copy uses the spare slot")
	}

	history, ok := st.MetadataHistory(owner)
	require.True(t, ok)
	require.Equal(t, []int64{100, 200, 300}, versionEffectiveTimes(history.Versions))
	require.Equal(t, []string{"env-100", "env-200", "env-300"}, versionEnvironments(history.Versions))
	require.False(t, history.Truncated)
}

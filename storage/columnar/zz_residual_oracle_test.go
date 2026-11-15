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

// Measurement-only harness for the post-COW residual owner-cost diagnosis.
//
// These tests are not part of the production test contract. They dump the committed
// physical work of each pre-registered arm, and they assert the fixture-derived
// arithmetic fixed by the measurement card. Expectations come from the fixture rules
// and from the arm's declared shape; they are never produced by the production update
// algorithm.
//
// The dump uses reflection over metadataWork's integer fields, so the same source
// compiles against a tree with and without the additional diagnostics and reports
// whatever field set that tree carries. Nothing here changes production behaviour.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// zzDumpDir returns the evidence directory the harness writes into.
func zzDumpDir(tb testing.TB) string {
	tb.Helper()
	dir := os.Getenv("POSTCOW_DUMP_DIR")
	if dir == "" {
		tb.Skip("POSTCOW_DUMP_DIR is not set: this harness only runs as part of the measurement campaign")
	}
	require.NoError(tb, os.MkdirAll(dir, 0o755))
	return dir
}

// zzDumpState returns the binary state label ("h" or "d") of this run.
func zzDumpState(tb testing.TB) string {
	tb.Helper()
	state := os.Getenv("POSTCOW_STATE")
	if state == "" {
		tb.Skip("POSTCOW_STATE is not set: this harness only runs as part of the measurement campaign")
	}
	return state
}

// zzWorkFields renders one metadataWork as sorted integer key=value pairs. The
// transaction-scoped owner set is a map, not a counter, and is skipped.
func zzWorkFields(w metadataWork) []string {
	value := reflect.ValueOf(w)
	typ := value.Type()
	out := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.Type.Kind() != reflect.Int {
			continue
		}
		out = append(out, fmt.Sprintf("%s=%d", field.Name, value.Field(i).Int()))
	}
	sort.Strings(out)
	return out
}

// zzDump writes one arm's per-entry accounting as a single line.
func zzDump(out *strings.Builder, state, arm string, entry int, w metadataWork) {
	fmt.Fprintf(out, "state=%s arm=%s entry=%d %s\n", state, arm, entry, strings.Join(zzWorkFields(w), " "))
}

// zzControlledArms is the pre-registered controlled matrix, in benchmark order.
func zzControlledArms() []struct {
	name string
	spec ownerCardinalitySpec
} {
	var arms []struct {
		name string
		spec ownerCardinalitySpec
	}
	for _, mode := range []ownerCardinalityMode{
		ownerCardinalityNoMetadata, ownerCardinalityUnchanged,
		ownerCardinalityVersionAndAssociations, ownerCardinalityOwnerOnlyVersion,
	} {
		for _, owners := range []int{1, 10, 100, 1_000} {
			arms = append(arms, struct {
				name string
				spec ownerCardinalitySpec
			}{
				name: fmt.Sprintf("mode=%s,owners=%d", mode, owners),
				spec: ownerCostFullSpec(mode, owners, owners),
			})
		}
	}
	for _, updated := range []int{1, 10, 100, 1_000} {
		arms = append(arms, struct {
			name string
			spec ownerCardinalitySpec
		}{
			name: fmt.Sprintf("mode=%s,owners=1000,updated=%d", ownerCardinalityVersionAndAssociations, updated),
			spec: ownerCostFullSpec(ownerCardinalityVersionAndAssociations, 1_000, updated),
		})
	}
	return arms
}

// zzExistingExpect is the fixture-derived expectation of the pre-existing counters
// for one controlled-arm timed entry.
type zzExistingExpect struct {
	metadataTransactions, clones                             int
	versionsInserted, indexCalls, indexVersionsVisited       int
	bucketClones, bucketEntries                              int
	oldConditions, newConditions, rangeUpdates, identifying  int
	contentAttributeVisits                                   int
	ownerCopies, ownerCopRows                                int
	versionBoundsExtended, versionDuplicateNoops             int
	membershipsInserted, membershipBoundsExtended            int
	changeDetectionCanonicalizations, applyCanonicalizations int
	contentClones, seriesHashes, completeLabelChecks         int
	membershipSearches, versionSearches, changedOwners       int
	descriptiveVisits, historyEntriesWritten                 int
}

// zzExpectExisting derives one controlled-arm entry's expected existing counters.
func zzExpectExisting(spec ownerCardinalitySpec, e int) zzExistingExpect {
	var want zzExistingExpect
	if spec.mode == ownerCardinalityNoMetadata {
		return want
	}
	k, u, associations := spec.owners, spec.ownerUpdates(), spec.associations()
	want.metadataTransactions = 1
	want.clones = 1
	// Detection stops at the first observation that changes state, so exactly one
	// canonicalized comparison pair runs before the transaction is selected.
	want.changeDetectionCanonicalizations = 2
	want.seriesHashes = min(associations, 1)
	// Applying one association checks the complete labels four times: validateLabels
	// and the explicit chain comparison in associateWithHistory, then the same pair
	// in attachMembership.
	want.completeLabelChecks = 4 * associations
	want.contentClones = u
	want.applyCanonicalizations = 2*associations + 2*u
	want.versionsInserted = u
	want.changedOwners = u
	want.indexCalls = u
	want.versionBoundsExtended = associations
	want.versionDuplicateNoops = max(associations-k, 0)
	// One association per record means every resident owner is cloned at its first
	// mutation regardless of how many owners change. The prewarm gives every owner a
	// version at the entry's first timestamp, and the changed owners a second at its
	// mid timestamp, so a changed owner's first mutation copies one more row.
	want.ownerCopies = k
	switch spec.mode {
	case ownerCardinalityUnchanged:
		want.ownerCopRows = k
	case ownerCardinalityOwnerOnlyVersion:
		want.ownerCopRows = k * e
	case ownerCardinalityVersionAndAssociations:
		// An owner that is never updated keeps its single prewarm version and only
		// has its observation bound extended; a changed owner gains one version per
		// entry, so it holds 1+e+... at its first mutation of this entry.
		want.ownerCopRows = (k - u) + u*(e+1)
	}
	// Each insert adds a version before the owner's index is rebuilt.
	switch spec.mode {
	case ownerCardinalityOwnerOnlyVersion:
		want.indexVersionsVisited = u * (e + 1)
	case ownerCardinalityVersionAndAssociations:
		want.indexVersionsVisited = u * (e + 2)
	}
	want.contentAttributeVisits = 4 * want.indexVersionsVisited
	want.descriptiveVisits = 0
	want.historyEntriesWritten = 0
	// The prewarm entry states the memberships; every timed observation of the same
	// series therefore extends the recorded bound instead of inserting a row.
	want.membershipBoundsExtended = associations
	if u > 0 {
		// The two shared identity buckets plus one singleton per changed owner.
		want.bucketClones = u + 2
		want.bucketEntries = 2*k + u
		want.oldConditions, want.newConditions = 3*u, 3*u
		want.rangeUpdates, want.identifying = 3*u, 3*u
	}
	// The aggregate counts one attribution per applied owner-version search and two
	// per membership search, because a membership search passes through both the
	// helper and the attribution call. The detection phase attributes none of its
	// searches, so it is not a membership-search-only counter.
	want.versionSearches = 1 + associations + u
	want.membershipSearches = associations + u + 2*associations
	return want
}

// zzRequireExisting asserts the pre-registered arithmetic of the existing counters.
func zzRequireExisting(tb testing.TB, spec ownerCardinalitySpec, e int, w metadataWork) {
	tb.Helper()
	want := zzExpectExisting(spec, e)
	label := fmt.Sprintf("entry %d", e)
	require.Equal(tb, want.metadataTransactions, w.metadataTransactions, label+" metadata transactions")
	require.Equal(tb, want.clones, w.clones, label+" clones")
	require.Equal(tb, spec.samples(), w.inputSamples, label+" input samples")
	require.Equal(tb, spec.series(), w.inputSeries, label+" input series")
	require.Equal(tb, spec.associations(), w.inputAssociations, label+" input associations")
	require.Equal(tb, spec.ownerUpdates(), w.inputOwnerUpdates, label+" input owner updates")
	require.Equal(tb, want.versionsInserted, w.versionsInserted, label+" versions")
	require.Equal(tb, want.changedOwners, w.changedOwners, label+" changed owners")
	require.Equal(tb, want.indexCalls, w.indexCalls, label+" index calls")
	require.Equal(tb, want.indexVersionsVisited, w.indexVersionsVisited, label+" index versions visited")
	require.Equal(tb, want.bucketClones, w.indexBucketClones, label+" bucket clones")
	require.Equal(tb, want.bucketEntries, w.indexBucketClonedEntries, label+" bucket entries")
	require.Equal(tb, want.oldConditions, w.indexOldConditions, label+" old conditions")
	require.Equal(tb, want.newConditions, w.indexNewConditions, label+" new conditions")
	require.Equal(tb, want.rangeUpdates, w.indexGlobalRangeUpdates, label+" range updates")
	require.Equal(tb, want.identifying, w.indexIdentifyingVisits, label+" identifying visits")
	require.Equal(tb, want.contentAttributeVisits, w.indexContentAttributeVisits, label+" content attribute visits")
	require.Equal(tb, want.descriptiveVisits, w.indexDescriptiveAttributeVisits, label+" descriptive visits")
	require.Equal(tb, want.historyEntriesWritten, w.indexHistoryEntriesWritten, label+" history entries")
	require.Equal(tb, want.ownerCopies, w.ownerCOPCopies, label+" owner copies")
	require.Equal(tb, want.ownerCopRows, w.ownerCOPVersionRows, label+" owner COW rows")
	require.Zero(tb, w.ownerCOPIndexEntries, label+" owner COW index entries")
	require.Equal(tb, want.versionBoundsExtended, w.versionBoundsExtended, label+" bound extensions")
	require.Equal(tb, want.versionDuplicateNoops, w.versionDuplicateNoops, label+" duplicate no-ops")
	require.Equal(tb, want.membershipsInserted, w.membershipsInserted, label+" memberships inserted")
	require.Equal(tb, want.membershipBoundsExtended, w.membershipBoundsExtended, label+" membership bound extensions")
	require.Equal(tb, want.changeDetectionCanonicalizations, w.changeDetectionCanonicalizations, label+" detection canonicalizations")
	require.Equal(tb, want.applyCanonicalizations, w.applyCanonicalizations, label+" apply canonicalizations")
	require.Equal(tb, want.contentClones, w.contentClones, label+" content clones")
	require.Equal(tb, want.seriesHashes, w.seriesHashes, label+" series hashes")
	require.Equal(tb, want.completeLabelChecks, w.completeLabelChecks, label+" complete label checks")
	require.Equal(tb, want.versionSearches, w.versionSearches, label+" version searches")
	require.Equal(tb, want.membershipSearches, w.membershipSearches, label+" membership searches")
	require.Zero(tb, w.versionsEvicted, label+" evictions, the controlled matrix is uncapped")
	require.Zero(tb, w.versionsCoalesced, label+" coalescing")
	if spec.mode != ownerCardinalityNoMetadata {
		require.Equal(tb, spec.owners, w.residentOwners, label+" resident owners")
	}
}

// zzResidentState renders the resident catalog and cache state of one store.
func zzResidentState(st *Storage) string {
	var parts []string
	totalVersions, totalMemberships, historyIndexEntries := 0, 0, 0
	for _, history := range st.metadata.byOwner {
		totalVersions += len(history.versions)
		historyIndexEntries += len(history.index)
	}
	byKindRows := map[MetadataKind]int{}
	for _, kinds := range st.metadata.bySeries {
		for kind, rows := range kinds {
			totalMemberships += len(rows)
			byKindRows[kind] += len(rows)
		}
	}
	ownerRanges := 0
	for _, bucket := range st.metadata.attributeIndex {
		ownerRanges += len(bucket)
	}
	parts = append(parts,
		fmt.Sprintf("commits=%d", st.ReadStats().CommitCount),
		fmt.Sprintf("head-samples=%d", st.bufferN),
		fmt.Sprintf("head-series=%d", len(st.buffer)),
		fmt.Sprintf("seals=%d", st.ReadStats().SealCount),
		fmt.Sprintf("blocks=%d", st.ReadStats().Blocks),
		fmt.Sprintf("metadata-dirty=%t", st.metadataDirty),
		fmt.Sprintf("resident-owners=%d", len(st.metadata.byOwner)),
		fmt.Sprintf("resident-versions=%d", totalVersions),
		fmt.Sprintf("resident-series=%d", len(st.metadata.bySeries)),
		fmt.Sprintf("resident-chains=%d", len(st.metadata.chains)),
		fmt.Sprintf("resident-memberships=%d", totalMemberships),
		fmt.Sprintf("memberships-by-kind=%d", len(byKindRows)),
		fmt.Sprintf("attribute-conditions=%d", len(st.metadata.attributeIndex)),
		fmt.Sprintf("attribute-owner-ranges=%d", ownerRanges),
		fmt.Sprintf("owner-index-owners=%d", len(st.metadata.ownerIndex)),
		fmt.Sprintf("history-index-entries=%d", historyIndexEntries),
		fmt.Sprintf("canonical-versions=%d", len(st.metadata.canonicalVersions)),
		fmt.Sprintf("owned-attribute-buckets=%d", len(st.metadata.ownedAttributeBuckets)),
		fmt.Sprintf("live-heap-MiB=%.2f", liveHeapMiB()),
	)
	return strings.Join(parts, " ")
}

// TestZZResidualControlledCounters dumps and checks the controlled matrix.
func TestZZResidualControlledCounters(t *testing.T) {
	dir, state := zzDumpDir(t), zzDumpState(t)
	var out strings.Builder
	path := filepath.Join(dir, "controlled-"+state+".txt")
	defer func() { require.NoError(t, os.WriteFile(path, []byte(out.String()), 0o644)) }()
	for _, arm := range zzControlledArms() {
		t.Run(arm.name, func(t *testing.T) {
			spec := arm.spec
			fx := buildOwnerCardinalityFixture(t, spec)
			st := newOwnerCardinalityStorage(t)
			// Prewarm entry zero is outside the measured delta, exactly as in the
			// benchmark, so the dump starts at entry one.
			require.NoError(t, st.AppendBatch(context.Background(), fx.entries[0].batch))
			require.Equal(t, spec.samples(), st.bufferN, "head samples after prewarm")
			require.Len(t, st.buffer, spec.series(), "head series after prewarm")
			for e := 1; e <= spec.timed; e++ {
				before := st.reads.metadataWorkSnapshot()
				require.NoError(t, st.AppendBatch(context.Background(), fx.entries[e].batch))
				work := st.reads.metadataWorkSnapshot().since(before)
				zzDump(&out, state, arm.name, e, work)
				zzRequireExisting(t, spec, e, work)
				requireOwnerCardinalityHistories(t, st, fx, fx.entries[e].expect)
			}
			require.Equal(t, spec.samples()*(spec.timed+1), st.bufferN, "head samples after the timed entries")
			require.Len(t, st.buffer, spec.series(), "head series after the timed entries")
			require.Zero(t, st.ReadStats().SealCount, "no seal")
			require.Zero(t, st.ReadStats().Blocks, "no block")
			requireOwnerCardinalityResident(t, st, fx, spec.timed)
		})
	}
}

// TestZZResidualLegacyCounters dumps and checks the legacy per-record version-change
// corpus at the store's default version cap.
func TestZZResidualLegacyCounters(t *testing.T) {
	dir, state := zzDumpDir(t), zzDumpState(t)
	const records, entries = 1_000, 8
	fx := newLegacyOwnerVersionFixture(t, records, 2, entries)
	st, err := NewStorage(t.TempDir(), StorageOptions{FlushSamples: 1 << 40, DisableWAL: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	var out strings.Builder
	path := filepath.Join(dir, "legacy-"+state+".txt")
	defer func() { require.NoError(t, os.WriteFile(path, []byte(out.String()), 0o644)) }()
	for e := 0; e <= entries; e++ {
		before := st.reads.metadataWorkSnapshot()
		require.NoError(t, st.AppendBatch(context.Background(), fx.entries[e].batch))
		work := st.reads.metadataWorkSnapshot().since(before)
		zzDump(&out, state, "legacy-per-record-version-change", e, work)

		// Depth grows by two versions per entry, so the default cap of five first
		// drops a version at entry two.
		wantInserted := 2 * records
		wantEvicted := 0
		var wantOwnerCopRows int
		switch e {
		case 0:
			wantOwnerCopRows = 0
		case 1:
			wantOwnerCopRows = 2 * records
		case 2:
			wantEvicted = records
			wantOwnerCopRows = 4 * records
		default:
			wantEvicted = 2 * records
			wantOwnerCopRows = 5 * records
		}
		require.Equal(t, wantInserted, work.versionsInserted, "entry %d versions", e)
		require.Equal(t, wantInserted, work.indexCalls, "entry %d index calls", e)
		require.Equal(t, records, work.changedOwners, "entry %d changed owners", e)
		require.Equal(t, wantEvicted, work.versionsEvicted, "entry %d evictions", e)
		require.Equal(t, records+2, work.indexBucketClones, "entry %d bucket clones", e)
		if e == 0 {
			require.Zero(t, work.indexBucketClonedEntries, "entry 0 publishes no buckets to copy")
		} else {
			require.Equal(t, 3*records, work.indexBucketClonedEntries, "entry %d bucket entries", e)
		}
		require.Equal(t, records, work.ownerCOPCopies, "entry %d owner copies", e)
		require.Equal(t, wantOwnerCopRows, work.ownerCOPVersionRows, "entry %d owner COW rows", e)
		require.Equal(t, 2*records, work.contentClones, "entry %d content clones", e)
		if e == 0 {
			// The first commit creates every owner, so change detection resolves no
			// history and compares no content before selecting the transaction.
			require.Zero(t, work.changeDetectionCanonicalizations, "entry 0 creates the owners")
		} else {
			require.Equal(t, 2, work.changeDetectionCanonicalizations, "entry %d detection canonicalizations", e)
		}
		require.Equal(t, 1, work.seriesHashes, "entry %d detection series hashes", e)
		if e == 0 {
			// The first commit creates every owner, every chain and every membership,
			// so it compares no stored content and extends no observation bound.
			require.Zero(t, work.completeLabelChecks, "entry 0 creates every chain")
			require.Zero(t, work.versionBoundsExtended, "entry 0 extends no version bound")
			require.Equal(t, 2*records, work.applyCanonicalizations, "entry 0 canonicalizes the owner update only")
		} else {
			require.Equal(t, 4*records, work.completeLabelChecks, "entry %d complete label checks", e)
			// The legacy corpus restores a version before every owner update, so no
			// association ever reaches the observation-only bound path.
			require.Zero(t, work.versionBoundsExtended, "entry %d inserts instead of extending", e)
			require.Equal(t, 4*records, work.applyCanonicalizations, "entry %d apply canonicalizations", e)
		}
		if e == 0 {
			require.Equal(t, records, work.membershipsInserted, "entry 0 membership insertion")
			require.Zero(t, work.membershipBoundsExtended, "entry 0 extends no membership bound")
		} else {
			require.Zero(t, work.membershipsInserted, "entry %d re-states no membership", e)
			require.Equal(t, records, work.membershipBoundsExtended, "entry %d membership bound extensions", e)
		}
		wantIndexVersions := 3 * records
		switch {
		case e == 1:
			wantIndexVersions = 7 * records
		case e >= 2:
			wantIndexVersions = 10 * records
		}
		require.Equal(t, wantIndexVersions, work.indexVersionsVisited, "entry %d index versions visited", e)
		requireOwnerCardinalityHistories(t, st, fx, fx.entries[e].expect)
	}
}

// TestZZResidualResidentState dumps the resident catalog, cache and ownership state
// after prewarm and after every controlled entry for the pre-registered arms.
func TestZZResidualResidentState(t *testing.T) {
	dir, state := zzDumpDir(t), zzDumpState(t)
	arms := []struct {
		name string
		spec ownerCardinalitySpec
	}{
		{"mode=no-metadata,owners=1000", ownerCostFullSpec(ownerCardinalityNoMetadata, 1_000, 1_000)},
		{"mode=unchanged,owners=1000", ownerCostFullSpec(ownerCardinalityUnchanged, 1_000, 1_000)},
		{"mode=version-plus-associations,owners=1000", ownerCostFullSpec(ownerCardinalityVersionAndAssociations, 1_000, 1_000)},
		{"mode=owner-only-version,owners=1000", ownerCostFullSpec(ownerCardinalityOwnerOnlyVersion, 1_000, 1_000)},
		{"mode=version-plus-associations,owners=1000,updated=1", ownerCostFullSpec(ownerCardinalityVersionAndAssociations, 1_000, 1)},
		{"mode=version-plus-associations,owners=1000,updated=100", ownerCostFullSpec(ownerCardinalityVersionAndAssociations, 1_000, 100)},
	}
	var out strings.Builder
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			spec := arm.spec
			fx := buildOwnerCardinalityFixture(t, spec)
			st := newOwnerCardinalityStorage(t)
			require.NoError(t, st.AppendBatch(context.Background(), fx.entries[0].batch))
			fmt.Fprintf(&out, "state=%s arm=%s entry=0 %s\n", state, arm.name, zzResidentState(st))
			for e := 1; e <= spec.timed; e++ {
				require.NoError(t, st.AppendBatch(context.Background(), fx.entries[e].batch))
				fmt.Fprintf(&out, "state=%s arm=%s entry=%d %s\n", state, arm.name, e, zzResidentState(st))
			}
			// The controlled association arms keep exactly the resident shared and
			// singleton conditions and three owner ranges per owner.
			if spec.associations() > 0 {
				require.Len(t, st.metadata.attributeIndex, spec.owners+2, "attribute conditions")
				ownerRanges := 0
				for _, bucket := range st.metadata.attributeIndex {
					ownerRanges += len(bucket)
				}
				require.Equal(t, 3*spec.owners, ownerRanges, "owner ranges")
			}
			require.Empty(t, st.metadata.canonicalVersions, "the unflushed direct loop keeps the canonical cache empty")
		})
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "resident-"+state+".txt"), []byte(out.String()), 0o644))
}

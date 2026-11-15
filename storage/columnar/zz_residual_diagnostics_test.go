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

// Measurement-only assertions for the additional diagnostics of the post-COW
// residual owner-cost diagnosis. The expectations are derived from the fixture
// rules and from the arm's declared shape; they are never produced by the
// production update algorithm.

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// zzRequireReconciled asserts the identities that let the pre-existing and the
// additional counters be read together without redefining either.
func zzRequireReconciled(tb testing.TB, label string, w metadataWork) {
	tb.Helper()
	// The phase split partitions the same call sites the aggregate counts.
	require.Equal(tb, w.versionSearches, w.detectVersionSearches+w.applyVersionSearches,
		label+" version searches reconcile")
	require.Equal(tb, w.versionSearchComparisons,
		w.detectVersionSearchComparisons+w.applyVersionSearchComparisons,
		label+" version search comparisons reconcile")
	// A membership search passes through both the helper that counts it and the
	// attribution helper, and every applied owner-version search is attributed too.
	require.Equal(tb, w.membershipSearches,
		w.applyVersionSearches+2*(w.detectMembershipSearches+w.applyMembershipSearches),
		label+" membership searches reconcile with their call sites")
	// seriesHashes keeps its meaning; the apply-phase hash is the site it misses.
	require.Equal(tb, w.seriesHashes, w.detectAssociationHashes+w.validateSeriesHashes,
		label+" series hashes reconcile with their call sites")
	require.Equal(tb, w.indexBucketClones, w.ownAttributeBucketCalls-w.ownAttributeBucketOwned,
		label+" bucket clones are the ownership calls that were not already owned")
	require.Equal(tb, w.indexCalls, w.newConditionMaps, label+" one condition map per rebuild")
	require.Equal(tb, w.versionInsertTailPositions+w.versionInsertShiftedRows, w.versionsInserted,
		label+" every insert is either a tail insert or a shifting one")
	require.LessOrEqual(tb, w.versionInsertGrowths, w.versionsInserted, label+" growth is a subset of inserts")
}

// zzDiagnosticsExpect is the fixture-derived expectation of the additional
// diagnostics for one controlled-arm timed entry.
type zzDiagnosticsExpect struct {
	detectVersionSearches, applyVersionSearches                int
	detectMembershipSearches, applyMembershipSearches          int
	detectAssociationChecks, detectOwnerUpdateChecks           int
	detectAssociationHashes, applyAssociationHashes            int
	detectCanonicalizationAttributes, applyCanonicalAttributes int
	ownerIdentityDecodes, ownerIdentityBytes                   int
	newConditionMaps, conditionAssignments                     int
	toCopySets, toCopyInsertions                               int
	ownAttributeBucketCalls, ownAttributeBucketOwned           int
	versionInsertTailPositions, versionInsertPreRows           int
	clonedContentBytes                                         int
}

// zzExpectDiagnostics derives one controlled-arm entry's expected diagnostics.
func zzExpectDiagnostics(spec ownerCardinalitySpec, e int) zzDiagnosticsExpect {
	var want zzDiagnosticsExpect
	if spec.mode == ownerCardinalityNoMetadata {
		return want
	}
	u, associations := spec.ownerUpdates(), spec.associations()
	// Detection stops at the first observation that changes state, which is also the
	// only observation whose series hash it must derive.
	want.detectVersionSearches = 1
	want.detectAssociationChecks = min(associations, 1)
	if associations == 0 {
		want.detectOwnerUpdateChecks = min(u, 1)
	}
	want.detectAssociationHashes = min(associations, 1)
	want.applyVersionSearches = associations + u
	want.applyMembershipSearches = associations
	want.applyAssociationHashes = associations
	// One content comparison pair runs before the transaction is selected.
	want.detectCanonicalizationAttributes = 2 * 4
	want.applyCanonicalAttributes = 4 * (2*associations + 2*u)
	for j := range u {
		want.clonedContentBytes += contentAttributeBytes(ownerCardinalityContent(j, spec.versionVersion(e)).Attributes)
		want.ownerIdentityBytes += len(ResourceOwner(ownerCardinalityIdentity(j)).ID)
	}
	want.ownerIdentityDecodes = u
	want.newConditionMaps = u
	// Each identifying attribute is assigned once when created and once when its
	// range is initialised from the first retained version.
	want.conditionAssignments = 6 * u
	want.toCopySets = u
	want.toCopyInsertions = 6 * u
	if u > 0 {
		want.ownAttributeBucketCalls = 3 * u
		want.ownAttributeBucketOwned = 3*u - (u + 2)
	}
	want.versionInsertTailPositions = u
	switch spec.mode {
	case ownerCardinalityOwnerOnlyVersion:
		want.versionInsertPreRows = u * e
	default:
		want.versionInsertPreRows = u * (e + 1)
	}
	return want
}

// TestZZResidualDiagnostics dumps and checks the additional diagnostics of the
// controlled matrix.
func TestZZResidualDiagnostics(t *testing.T) {
	for _, arm := range zzControlledArms() {
		t.Run(arm.name, func(t *testing.T) {
			spec := arm.spec
			fx := buildOwnerCardinalityFixture(t, spec)
			st := newOwnerCardinalityStorage(t)
			require.NoError(t, st.AppendBatch(context.Background(), fx.entries[0].batch))
			for e := 1; e <= spec.timed; e++ {
				before := st.reads.metadataWorkSnapshot()
				require.NoError(t, st.AppendBatch(context.Background(), fx.entries[e].batch))
				work := st.reads.metadataWorkSnapshot().since(before)
				label := fmt.Sprintf("entry %d", e)
				zzRequireReconciled(t, label, work)
				want := zzExpectDiagnostics(spec, e)
				require.Equal(t, want.detectVersionSearches, work.detectVersionSearches, label+" detect version searches")
				require.Equal(t, want.applyVersionSearches, work.applyVersionSearches, label+" apply version searches")
				require.Equal(t, want.detectMembershipSearches, work.detectMembershipSearches, label+" detect membership searches")
				require.Equal(t, want.applyMembershipSearches, work.applyMembershipSearches, label+" apply membership searches")
				require.Equal(t, want.detectAssociationChecks, work.detectAssociationChecks, label+" detect association checks")
				require.Equal(t, want.detectOwnerUpdateChecks, work.detectOwnerUpdateChecks, label+" detect owner update checks")
				require.Equal(t, want.detectAssociationHashes, work.detectAssociationHashes, label+" detect association hashes")
				require.Zero(t, work.validateSeriesHashes, label+" validateLabels derives no hash")
				require.Equal(t, want.applyAssociationHashes, work.applyAssociationHashes, label+" apply association hashes")
				require.Equal(t, want.detectCanonicalizationAttributes, work.detectCanonicalizationAttributes,
					label+" detection canonicalization attributes")
				require.Equal(t, want.applyCanonicalAttributes, work.applyCanonicalizationAttributes,
					label+" apply canonicalization attributes")
				if want.detectCanonicalizationAttributes > 0 {
					require.Positive(t, work.detectCanonicalizationBytes, label+" detection canonicalization bytes")
					require.Positive(t, work.applyCanonicalizationBytes, label+" apply canonicalization bytes")
				}
				require.Equal(t, want.clonedContentBytes, work.clonedContentBytes, label+" cloned content bytes")
				require.Equal(t, want.ownerIdentityDecodes, work.ownerIdentityDecodes, label+" owner identity decodes")
				require.Equal(t, want.ownerIdentityBytes, work.ownerIdentityBytes, label+" owner identity bytes")
				require.Equal(t, want.newConditionMaps, work.newConditionMaps, label+" condition maps")
				require.Equal(t, want.conditionAssignments, work.conditionAssignments, label+" condition assignments")
				if want.newConditionMaps > 0 {
					require.Positive(t, work.conditionBytesConstructed, label+" condition bytes")
				}
				require.Equal(t, want.toCopySets, work.toCopySets, label+" toCopy sets")
				require.Equal(t, want.toCopyInsertions, work.toCopyInsertions, label+" toCopy insertions")
				require.Equal(t, want.ownAttributeBucketCalls, work.ownAttributeBucketCalls, label+" ownership calls")
				require.Equal(t, want.ownAttributeBucketOwned, work.ownAttributeBucketOwned, label+" already-owned checks")
				require.Equal(t, want.versionInsertTailPositions, work.versionInsertTailPositions, label+" tail insertions")
				require.Equal(t, want.versionInsertPreRows, work.versionInsertPreRows, label+" pre-insert rows")
				require.Zero(t, work.versionInsertShiftedRows, label+" controlled inserts never shift rows")
				require.Zero(t, work.evictionRetainedRowsCopied, label+" the controlled matrix is uncapped")
			}
		})
	}
}

// TestZZResidualLegacyDiagnostics dumps and checks the additional diagnostics of the
// legacy per-record version-change corpus at the store's default version cap.
func TestZZResidualLegacyDiagnostics(t *testing.T) {
	const records, entries = 1_000, 8
	fx := newLegacyOwnerVersionFixture(t, records, 2, entries)
	st, err := NewStorage(t.TempDir(), StorageOptions{FlushSamples: 1 << 40, DisableWAL: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	for e := 0; e <= entries; e++ {
		before := st.reads.metadataWorkSnapshot()
		require.NoError(t, st.AppendBatch(context.Background(), fx.entries[e].batch))
		work := st.reads.metadataWorkSnapshot().since(before)
		label := fmt.Sprintf("entry %d", e)
		zzRequireReconciled(t, label, work)

		// A comparison canonicalizes the stored identity content (three attribute
		// entries) and the stated descriptive content (four), so each pair visits
		// seven entries rather than eight.
		wantDetectSearches, wantDetectCanonical := 0, 0
		wantApplyCanonical := 7 * records
		if e > 0 {
			wantDetectSearches, wantDetectCanonical = 1, 7
			wantApplyCanonical = 14 * records
		}
		require.Equal(t, wantDetectSearches, work.detectVersionSearches, label+" detect version searches")
		require.Equal(t, 2*records, work.applyVersionSearches, label+" apply version searches")
		require.Zero(t, work.detectMembershipSearches, label+" detect membership searches")
		require.Equal(t, records, work.applyMembershipSearches, label+" apply membership searches")
		require.Equal(t, 1, work.detectAssociationChecks, label+" detect association checks")
		require.Zero(t, work.detectOwnerUpdateChecks, label+" detection stops at the first association")
		require.Equal(t, 1, work.detectAssociationHashes, label+" detect association hashes")
		require.Zero(t, work.validateSeriesHashes, label+" validateLabels derives no hash")
		require.Equal(t, records, work.applyAssociationHashes, label+" apply association hashes")
		require.Equal(t, wantDetectCanonical, work.detectCanonicalizationAttributes, label+" detection canonicalization attributes")
		require.Equal(t, wantApplyCanonical, work.applyCanonicalizationAttributes, label+" apply canonicalization attributes")
		require.Equal(t, 2*records, work.ownerIdentityDecodes, label+" owner identity decodes")
		require.Equal(t, 2*records, work.newConditionMaps, label+" condition maps")
		require.Equal(t, 12*records, work.conditionAssignments, label+" condition assignments")
		require.Equal(t, 2*records, work.toCopySets, label+" toCopy sets")
		wantToCopyInsertions := 12 * records
		if e == 0 {
			// The first rebuild of a new owner has no previous condition set to copy.
			wantToCopyInsertions = 9 * records
		}
		require.Equal(t, wantToCopyInsertions, work.toCopyInsertions, label+" toCopy insertions")
		require.Equal(t, 6*records, work.ownAttributeBucketCalls, label+" ownership calls")
		require.Equal(t, 6*records-(records+2), work.ownAttributeBucketOwned, label+" already-owned checks")
		require.Equal(t, 2*records, work.versionInsertTailPositions, label+" tail insertions")
		require.Zero(t, work.versionInsertShiftedRows, label+" every insert is at the tail")

		var wantPreRows int
		switch e {
		case 0:
			wantPreRows = records
		case 1:
			wantPreRows = 5 * records
		case 2:
			wantPreRows = 9 * records
		default:
			// Each insert evicts back to the cap, so every later insert starts at five
			// retained versions.
			wantPreRows = 10 * records
		}
		require.Equal(t, wantPreRows, work.versionInsertPreRows, label+" pre-insert rows")

		if e < 2 {
			require.Zero(t, work.evictionRetainedRowsCopied, label+" no eviction before the cap is exceeded")
			require.Zero(t, work.evictionConditionWrites, label+" no eviction rebuild")
			continue
		}
		// Each eviction copies the cap it keeps: five retained versions. Entry two
		// evicts once per owner, every later entry twice, and the attribute total
		// follows the alternating identity/descriptive content of the retained window.
		wantEvictions, wantAttrs := records, 18
		if e > 2 {
			wantEvictions, wantAttrs = 2*records, 35
		}
		require.Equal(t, 5*wantEvictions, work.evictionRetainedRowsCopied, label+" retained rows copied")
		require.Equal(t, 5*wantEvictions, work.evictionVersionsScanned, label+" retained versions scanned")
		require.Equal(t, wantAttrs*records, work.evictionAttributesScanned, label+" retained attributes scanned")
		require.Equal(t, wantAttrs*records, work.evictionConditionWrites, label+" condition writes")
	}
}

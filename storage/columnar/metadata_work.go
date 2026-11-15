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

import "go.uber.org/atomic"

// metadataWork is the physical-work accounting of one metadata transaction.
//
// It exists to attribute the cost of a commit to a mechanism instead of to a
// profile share: every field counts a specific call site, so the number of owner
// versions, ordered searches, canonicalizations, copy-on-write copies and, above
// all, the entries in cloned inverted-index buckets can be read per commit and per
// entry. Fields are plain per-transaction arithmetic; the transaction publishes
// them once, only if it commits, with one atomic add per field.
//
// The counters describe committed work only. A rejected transaction publishes
// nothing, so its partial work can never be mistaken for a commit.
type metadataWork struct {
	// attempts is published at AppendBatch entry and commits at publication, so
	// attempts minus commits exposes rejection. metadataTransactions counts the
	// commits that cloned and applied the catalog.
	attempts             int
	commits              int
	metadataTransactions int

	// Input and resident shape at commit time.
	inputSamples      int
	inputSeries       int
	inputAssociations int
	inputOwnerUpdates int
	residentOwners    int
	residentSeries    int
	changedOwners     int

	// Change detection and catalog cloning.
	batchChangeChecks    int
	batchChangeUnchanged int
	batchChangeSelected  int
	clones               int
	cloneByOwnerEntries  int
	cloneOwnerStructs    int
	cloneBySeriesEntries int
	cloneChainsEntries   int
	cloneOwnerIndex      int
	cloneAttributeConds  int

	// Owner history resolution, ordered searches and identity checks.
	ownerHistoryResolutions  int
	versionSearches          int
	versionSearchComparisons int
	membershipSearches       int
	membershipSearchCompares int
	seriesHashes             int
	completeLabelChecks      int

	// Content identity work.
	changeDetectionCanonicalizations int
	applyCanonicalizations           int
	contentComparisons               int
	contentClones                    int
	clonedContentAttributes          int

	// Applied owner-version work.
	versionsInserted      int
	versionBoundsExtended int
	versionDuplicateNoops int
	versionsCoalesced     int
	versionsEvicted       int

	// Applied membership work.
	membershipsInserted      int
	membershipOwnershipMoves int
	membershipBoundsExtended int
	membershipDuplicateNoops int
	membershipRowsCollapsed  int

	// Copy-on-write of touched owner and membership state.
	ownerCOPCopies        int
	ownerCOPVersionRows   int
	ownerCOPIndexEntries  int
	membershipCOPGroups   int
	membershipCOPKindRows int
	membershipCOPRows     int

	// Resource index maintenance in indexResourceOwner.
	indexCalls                      int
	indexVersionsVisited            int
	indexIdentifyingVisits          int
	indexContentAttributeVisits     int
	indexDescriptiveAttributeVisits int
	indexOldConditions              int
	indexNewConditions              int
	indexRetainedConditions         int
	indexDeletedConditions          int
	indexAddedConditions            int
	indexBucketClones               int
	indexBucketClonedEntries        int
	indexGlobalRangeUpdates         int
	indexHistoryClears              int
	indexHistoryEntriesWritten      int

	// versionedOwners tracks which owners gained a version in this transaction, so
	// changedOwners is a distinct count rather than an apply-call count. It is
	// transaction-scoped and never published.
	versionedOwners map[OwnerKey]struct{}

	// Phase-split ordered searches. The aggregates above stay authoritative; these
	// separate the same call sites by phase so change detection and apply can be
	// compared, and so a membership search is no longer indistinguishable from the
	// owner-version search that shares its attribution helper.
	detectVersionSearches             int
	detectVersionSearchComparisons    int
	applyVersionSearches              int
	applyVersionSearchComparisons     int
	detectMembershipSearches          int
	detectMembershipSearchComparisons int
	applyMembershipSearches           int
	applyMembershipSearchComparisons  int

	// How far batchChanges had to walk before it selected or skipped the
	// transaction, so an early exit is visible rather than inferred.
	detectAssociationChecks int
	detectOwnerUpdateChecks int

	// Complete-label hashes by call site. seriesHashes keeps its meaning: it covers
	// change detection and the validateLabels branch that hashes an unresolved
	// association, and never covered the hash batchMetadataTransaction.associate
	// derives before validateLabels sees a resolved one.
	detectAssociationHashes int
	applyAssociationHashes  int
	validateSeriesHashes    int

	// Content canonicalization volume, incremented only where Content.Canonical
	// actually executes, so a caller-derivable or memoized identity adds nothing.
	detectCanonicalizationAttributes int
	detectCanonicalizationBytes      int
	applyCanonicalizationAttributes  int
	applyCanonicalizationBytes       int
	// clonedContentBytes is the summed byte length of the attribute names and values
	// the owner-version clone copies. It is source-level accounting, not a claim about
	// the bytes the allocator moves for a map of string headers.
	clonedContentBytes int

	// Attribute-index construction and copy-on-write ownership.
	ownerIdentityDecodes      int
	ownerIdentityBytes        int
	newConditionMaps          int
	conditionAssignments      int
	conditionBytesConstructed int
	toCopySets                int
	toCopyInsertions          int
	ownAttributeBucketCalls   int
	ownAttributeBucketOwned   int

	// Version insertion geometry and the bounded-history rebuild.
	versionInsertTailPositions int
	versionInsertPreRows       int
	versionInsertShiftedRows   int
	versionInsertGrowths       int
	evictionRetainedRowsCopied int
	evictionVersionsScanned    int
	evictionAttributesScanned  int
	evictionConditionWrites    int
}

// metadataPhase selects the transaction phase an operation belongs to: deciding
// whether the batch changes the published catalog, or applying it to the clone.
type metadataPhase int

const (
	changeDetectionPhase metadataPhase = iota
	applyPhase
)

// noteCanonicalVolume records the input and output size of one executed
// Content.Canonical call.
func noteCanonicalVolume(w *metadataWork, phase metadataPhase, attributes, bytes int) {
	if w == nil {
		return
	}
	if phase == changeDetectionPhase {
		w.detectCanonicalizationAttributes += attributes
		w.detectCanonicalizationBytes += bytes
		return
	}
	w.applyCanonicalizationAttributes += attributes
	w.applyCanonicalizationBytes += bytes
}

// contentAttributeBytes returns the summed byte length of an attribute map's names
// and values.
func contentAttributeBytes(attributes map[string]string) int {
	total := 0
	for name, value := range attributes {
		total += len(name) + len(value)
	}
	return total
}

// since returns the work this state added beyond an earlier snapshot.
func (w metadataWork) since(previous metadataWork) metadataWork {
	return metadataWork{
		attempts:                         w.attempts - previous.attempts,
		commits:                          w.commits - previous.commits,
		metadataTransactions:             w.metadataTransactions - previous.metadataTransactions,
		inputSamples:                     w.inputSamples - previous.inputSamples,
		inputSeries:                      w.inputSeries - previous.inputSeries,
		inputAssociations:                w.inputAssociations - previous.inputAssociations,
		inputOwnerUpdates:                w.inputOwnerUpdates - previous.inputOwnerUpdates,
		residentOwners:                   w.residentOwners - previous.residentOwners,
		residentSeries:                   w.residentSeries - previous.residentSeries,
		changedOwners:                    w.changedOwners - previous.changedOwners,
		batchChangeChecks:                w.batchChangeChecks - previous.batchChangeChecks,
		batchChangeUnchanged:             w.batchChangeUnchanged - previous.batchChangeUnchanged,
		batchChangeSelected:              w.batchChangeSelected - previous.batchChangeSelected,
		clones:                           w.clones - previous.clones,
		cloneByOwnerEntries:              w.cloneByOwnerEntries - previous.cloneByOwnerEntries,
		cloneOwnerStructs:                w.cloneOwnerStructs - previous.cloneOwnerStructs,
		cloneBySeriesEntries:             w.cloneBySeriesEntries - previous.cloneBySeriesEntries,
		cloneChainsEntries:               w.cloneChainsEntries - previous.cloneChainsEntries,
		cloneOwnerIndex:                  w.cloneOwnerIndex - previous.cloneOwnerIndex,
		cloneAttributeConds:              w.cloneAttributeConds - previous.cloneAttributeConds,
		ownerHistoryResolutions:          w.ownerHistoryResolutions - previous.ownerHistoryResolutions,
		versionSearches:                  w.versionSearches - previous.versionSearches,
		versionSearchComparisons:         w.versionSearchComparisons - previous.versionSearchComparisons,
		membershipSearches:               w.membershipSearches - previous.membershipSearches,
		membershipSearchCompares:         w.membershipSearchCompares - previous.membershipSearchCompares,
		seriesHashes:                     w.seriesHashes - previous.seriesHashes,
		completeLabelChecks:              w.completeLabelChecks - previous.completeLabelChecks,
		changeDetectionCanonicalizations: w.changeDetectionCanonicalizations - previous.changeDetectionCanonicalizations,
		applyCanonicalizations:           w.applyCanonicalizations - previous.applyCanonicalizations,
		contentComparisons:               w.contentComparisons - previous.contentComparisons,
		contentClones:                    w.contentClones - previous.contentClones,
		clonedContentAttributes:          w.clonedContentAttributes - previous.clonedContentAttributes,
		versionsInserted:                 w.versionsInserted - previous.versionsInserted,
		versionBoundsExtended:            w.versionBoundsExtended - previous.versionBoundsExtended,
		versionDuplicateNoops:            w.versionDuplicateNoops - previous.versionDuplicateNoops,
		versionsCoalesced:                w.versionsCoalesced - previous.versionsCoalesced,
		versionsEvicted:                  w.versionsEvicted - previous.versionsEvicted,
		membershipsInserted:              w.membershipsInserted - previous.membershipsInserted,
		membershipOwnershipMoves:         w.membershipOwnershipMoves - previous.membershipOwnershipMoves,
		membershipBoundsExtended:         w.membershipBoundsExtended - previous.membershipBoundsExtended,
		membershipDuplicateNoops:         w.membershipDuplicateNoops - previous.membershipDuplicateNoops,
		membershipRowsCollapsed:          w.membershipRowsCollapsed - previous.membershipRowsCollapsed,
		ownerCOPCopies:                   w.ownerCOPCopies - previous.ownerCOPCopies,
		ownerCOPVersionRows:              w.ownerCOPVersionRows - previous.ownerCOPVersionRows,
		ownerCOPIndexEntries:             w.ownerCOPIndexEntries - previous.ownerCOPIndexEntries,
		membershipCOPGroups:              w.membershipCOPGroups - previous.membershipCOPGroups,
		membershipCOPKindRows:            w.membershipCOPKindRows - previous.membershipCOPKindRows,
		membershipCOPRows:                w.membershipCOPRows - previous.membershipCOPRows,
		indexCalls:                       w.indexCalls - previous.indexCalls,
		indexVersionsVisited:             w.indexVersionsVisited - previous.indexVersionsVisited,
		indexIdentifyingVisits:           w.indexIdentifyingVisits - previous.indexIdentifyingVisits,
		indexContentAttributeVisits:      w.indexContentAttributeVisits - previous.indexContentAttributeVisits,
		indexDescriptiveAttributeVisits:  w.indexDescriptiveAttributeVisits - previous.indexDescriptiveAttributeVisits,
		indexOldConditions:               w.indexOldConditions - previous.indexOldConditions,
		indexNewConditions:               w.indexNewConditions - previous.indexNewConditions,
		indexRetainedConditions:          w.indexRetainedConditions - previous.indexRetainedConditions,
		indexDeletedConditions:           w.indexDeletedConditions - previous.indexDeletedConditions,
		indexAddedConditions:             w.indexAddedConditions - previous.indexAddedConditions,
		indexBucketClones:                w.indexBucketClones - previous.indexBucketClones,
		indexBucketClonedEntries:         w.indexBucketClonedEntries - previous.indexBucketClonedEntries,
		indexGlobalRangeUpdates:          w.indexGlobalRangeUpdates - previous.indexGlobalRangeUpdates,
		indexHistoryClears:               w.indexHistoryClears - previous.indexHistoryClears,
		indexHistoryEntriesWritten:       w.indexHistoryEntriesWritten - previous.indexHistoryEntriesWritten,

		detectVersionSearches:             w.detectVersionSearches - previous.detectVersionSearches,
		detectVersionSearchComparisons:    w.detectVersionSearchComparisons - previous.detectVersionSearchComparisons,
		applyVersionSearches:              w.applyVersionSearches - previous.applyVersionSearches,
		applyVersionSearchComparisons:     w.applyVersionSearchComparisons - previous.applyVersionSearchComparisons,
		detectMembershipSearches:          w.detectMembershipSearches - previous.detectMembershipSearches,
		detectMembershipSearchComparisons: w.detectMembershipSearchComparisons - previous.detectMembershipSearchComparisons,
		applyMembershipSearches:           w.applyMembershipSearches - previous.applyMembershipSearches,
		applyMembershipSearchComparisons:  w.applyMembershipSearchComparisons - previous.applyMembershipSearchComparisons,
		detectAssociationChecks:           w.detectAssociationChecks - previous.detectAssociationChecks,
		detectOwnerUpdateChecks:           w.detectOwnerUpdateChecks - previous.detectOwnerUpdateChecks,
		detectAssociationHashes:           w.detectAssociationHashes - previous.detectAssociationHashes,
		applyAssociationHashes:            w.applyAssociationHashes - previous.applyAssociationHashes,
		validateSeriesHashes:              w.validateSeriesHashes - previous.validateSeriesHashes,
		detectCanonicalizationAttributes:  w.detectCanonicalizationAttributes - previous.detectCanonicalizationAttributes,
		detectCanonicalizationBytes:       w.detectCanonicalizationBytes - previous.detectCanonicalizationBytes,
		applyCanonicalizationAttributes:   w.applyCanonicalizationAttributes - previous.applyCanonicalizationAttributes,
		applyCanonicalizationBytes:        w.applyCanonicalizationBytes - previous.applyCanonicalizationBytes,
		clonedContentBytes:                w.clonedContentBytes - previous.clonedContentBytes,
		ownerIdentityDecodes:              w.ownerIdentityDecodes - previous.ownerIdentityDecodes,
		ownerIdentityBytes:                w.ownerIdentityBytes - previous.ownerIdentityBytes,
		newConditionMaps:                  w.newConditionMaps - previous.newConditionMaps,
		conditionAssignments:              w.conditionAssignments - previous.conditionAssignments,
		conditionBytesConstructed:         w.conditionBytesConstructed - previous.conditionBytesConstructed,
		toCopySets:                        w.toCopySets - previous.toCopySets,
		toCopyInsertions:                  w.toCopyInsertions - previous.toCopyInsertions,
		ownAttributeBucketCalls:           w.ownAttributeBucketCalls - previous.ownAttributeBucketCalls,
		ownAttributeBucketOwned:           w.ownAttributeBucketOwned - previous.ownAttributeBucketOwned,
		versionInsertTailPositions:        w.versionInsertTailPositions - previous.versionInsertTailPositions,
		versionInsertPreRows:              w.versionInsertPreRows - previous.versionInsertPreRows,
		versionInsertShiftedRows:          w.versionInsertShiftedRows - previous.versionInsertShiftedRows,
		versionInsertGrowths:              w.versionInsertGrowths - previous.versionInsertGrowths,
		evictionRetainedRowsCopied:        w.evictionRetainedRowsCopied - previous.evictionRetainedRowsCopied,
		evictionVersionsScanned:           w.evictionVersionsScanned - previous.evictionVersionsScanned,
		evictionAttributesScanned:         w.evictionAttributesScanned - previous.evictionAttributesScanned,
		evictionConditionWrites:           w.evictionConditionWrites - previous.evictionConditionWrites,
	}
}

// noteVersionOwner records that owner gained a version in this transaction.
func (w *metadataWork) noteVersionOwner(owner OwnerKey) {
	if w.versionedOwners == nil {
		w.versionedOwners = map[OwnerKey]struct{}{}
	}
	w.versionedOwners[owner] = struct{}{}
}

// metadataWorkAccumulator publishes committed transactions' accounting. It is
// written once per commit under the storage lock and read by an in-package snapshot,
// so publication stays a fixed number of atomic adds and the metadata path adds no
// exported ReadStats surface.
type metadataWorkAccumulator struct {
	attempts, commits, transactions                               atomic.Uint64
	inputSamples, inputSeries                                     atomic.Uint64
	inputAssociations, inputOwnerUpdates                          atomic.Uint64
	residentOwners, residentSeries, changedOwners                 atomic.Uint64
	batchChangeChecks, batchChangeUnchanged, batchChangeSelected  atomic.Uint64
	clones, cloneByOwnerEntries, cloneOwnerStructs                atomic.Uint64
	cloneBySeriesEntries, cloneChainsEntries                      atomic.Uint64
	cloneOwnerIndex, cloneAttributeConds                          atomic.Uint64
	ownerHistoryResolutions                                       atomic.Uint64
	versionSearches, versionSearchComparisons                     atomic.Uint64
	membershipSearches, membershipSearchCompares                  atomic.Uint64
	seriesHashes, completeLabelChecks                             atomic.Uint64
	changeDetectionCanonicalizations, applyCanonicalizations      atomic.Uint64
	contentComparisons                                            atomic.Uint64
	contentClones, clonedContentAttributes                        atomic.Uint64
	versionsInserted, versionBoundsExtended                       atomic.Uint64
	versionDuplicateNoops, versionsCoalesced                      atomic.Uint64
	versionsEvicted                                               atomic.Uint64
	membershipsInserted, membershipOwnershipMoves                 atomic.Uint64
	membershipBoundsExtended, membershipDuplicateNoops            atomic.Uint64
	membershipRowsCollapsed                                       atomic.Uint64
	ownerCOPCopies, ownerCOPVersionRows                           atomic.Uint64
	ownerCOPIndexEntries                                          atomic.Uint64
	membershipCOPGroups, membershipCOPKindRows                    atomic.Uint64
	membershipCOPRows                                             atomic.Uint64
	indexCalls, indexVersionsVisited                              atomic.Uint64
	indexIdentifyingVisits, indexContentAttributeVisits           atomic.Uint64
	indexDescriptiveAttributeVisits                               atomic.Uint64
	indexOldConditions, indexNewConditions                        atomic.Uint64
	indexRetainedConditions, indexDeletedConditions               atomic.Uint64
	indexAddedConditions                                          atomic.Uint64
	indexBucketClones, indexBucketClonedEntries                   atomic.Uint64
	indexGlobalRangeUpdates                                       atomic.Uint64
	indexHistoryClears, indexHistoryEntriesWritten                atomic.Uint64
	detectVersionSearches, detectVersionSearchComparisons         atomic.Uint64
	applyVersionSearches, applyVersionSearchComparisons           atomic.Uint64
	detectMembershipSearches, detectMembershipSearchCompares      atomic.Uint64
	applyMembershipSearches, applyMembershipSearchCompares        atomic.Uint64
	detectAssociationChecks, detectOwnerUpdateChecks              atomic.Uint64
	detectAssociationHashes, applyAssociationHashes               atomic.Uint64
	validateSeriesHashes                                          atomic.Uint64
	detectCanonicalizationAttributes, detectCanonicalizationBytes atomic.Uint64
	applyCanonicalizationAttributes, applyCanonicalizationBytes   atomic.Uint64
	clonedContentBytes                                            atomic.Uint64
	ownerIdentityDecodes, ownerIdentityBytes                      atomic.Uint64
	newConditionMaps, conditionAssignments                        atomic.Uint64
	conditionBytesConstructed                                     atomic.Uint64
	toCopySets, toCopyInsertions                                  atomic.Uint64
	ownAttributeBucketCalls, ownAttributeBucketOwned              atomic.Uint64
	versionInsertTailPositions, versionInsertPreRows              atomic.Uint64
	versionInsertShiftedRows, versionInsertGrowths                atomic.Uint64
	evictionRetainedRowsCopied, evictionVersionsScanned           atomic.Uint64
	evictionAttributesScanned, evictionConditionWrites            atomic.Uint64
}

// add publishes one committed transaction's accounting.
func (a *metadataWorkAccumulator) add(w metadataWork) {
	addWork(&a.commits, w.commits)
	addWork(&a.transactions, w.metadataTransactions)
	addWork(&a.inputSamples, w.inputSamples)
	addWork(&a.inputSeries, w.inputSeries)
	addWork(&a.inputAssociations, w.inputAssociations)
	addWork(&a.inputOwnerUpdates, w.inputOwnerUpdates)
	addWork(&a.residentOwners, w.residentOwners)
	addWork(&a.residentSeries, w.residentSeries)
	addWork(&a.changedOwners, w.changedOwners)
	addWork(&a.batchChangeChecks, w.batchChangeChecks)
	addWork(&a.batchChangeUnchanged, w.batchChangeUnchanged)
	addWork(&a.batchChangeSelected, w.batchChangeSelected)
	addWork(&a.clones, w.clones)
	addWork(&a.cloneByOwnerEntries, w.cloneByOwnerEntries)
	addWork(&a.cloneOwnerStructs, w.cloneOwnerStructs)
	addWork(&a.cloneBySeriesEntries, w.cloneBySeriesEntries)
	addWork(&a.cloneChainsEntries, w.cloneChainsEntries)
	addWork(&a.cloneOwnerIndex, w.cloneOwnerIndex)
	addWork(&a.cloneAttributeConds, w.cloneAttributeConds)
	addWork(&a.ownerHistoryResolutions, w.ownerHistoryResolutions)
	addWork(&a.versionSearches, w.versionSearches)
	addWork(&a.versionSearchComparisons, w.versionSearchComparisons)
	addWork(&a.membershipSearches, w.membershipSearches)
	addWork(&a.membershipSearchCompares, w.membershipSearchCompares)
	addWork(&a.seriesHashes, w.seriesHashes)
	addWork(&a.completeLabelChecks, w.completeLabelChecks)
	addWork(&a.changeDetectionCanonicalizations, w.changeDetectionCanonicalizations)
	addWork(&a.applyCanonicalizations, w.applyCanonicalizations)
	addWork(&a.contentComparisons, w.contentComparisons)
	addWork(&a.contentClones, w.contentClones)
	addWork(&a.clonedContentAttributes, w.clonedContentAttributes)
	addWork(&a.versionsInserted, w.versionsInserted)
	addWork(&a.versionBoundsExtended, w.versionBoundsExtended)
	addWork(&a.versionDuplicateNoops, w.versionDuplicateNoops)
	addWork(&a.versionsCoalesced, w.versionsCoalesced)
	addWork(&a.versionsEvicted, w.versionsEvicted)
	addWork(&a.membershipsInserted, w.membershipsInserted)
	addWork(&a.membershipOwnershipMoves, w.membershipOwnershipMoves)
	addWork(&a.membershipBoundsExtended, w.membershipBoundsExtended)
	addWork(&a.membershipDuplicateNoops, w.membershipDuplicateNoops)
	addWork(&a.membershipRowsCollapsed, w.membershipRowsCollapsed)
	addWork(&a.ownerCOPCopies, w.ownerCOPCopies)
	addWork(&a.ownerCOPVersionRows, w.ownerCOPVersionRows)
	addWork(&a.ownerCOPIndexEntries, w.ownerCOPIndexEntries)
	addWork(&a.membershipCOPGroups, w.membershipCOPGroups)
	addWork(&a.membershipCOPKindRows, w.membershipCOPKindRows)
	addWork(&a.membershipCOPRows, w.membershipCOPRows)
	addWork(&a.indexCalls, w.indexCalls)
	addWork(&a.indexVersionsVisited, w.indexVersionsVisited)
	addWork(&a.indexIdentifyingVisits, w.indexIdentifyingVisits)
	addWork(&a.indexContentAttributeVisits, w.indexContentAttributeVisits)
	addWork(&a.indexDescriptiveAttributeVisits, w.indexDescriptiveAttributeVisits)
	addWork(&a.indexOldConditions, w.indexOldConditions)
	addWork(&a.indexNewConditions, w.indexNewConditions)
	addWork(&a.indexRetainedConditions, w.indexRetainedConditions)
	addWork(&a.indexDeletedConditions, w.indexDeletedConditions)
	addWork(&a.indexAddedConditions, w.indexAddedConditions)
	addWork(&a.indexBucketClones, w.indexBucketClones)
	addWork(&a.indexBucketClonedEntries, w.indexBucketClonedEntries)
	addWork(&a.indexGlobalRangeUpdates, w.indexGlobalRangeUpdates)
	addWork(&a.indexHistoryClears, w.indexHistoryClears)
	addWork(&a.indexHistoryEntriesWritten, w.indexHistoryEntriesWritten)
	addWork(&a.detectVersionSearches, w.detectVersionSearches)
	addWork(&a.detectVersionSearchComparisons, w.detectVersionSearchComparisons)
	addWork(&a.applyVersionSearches, w.applyVersionSearches)
	addWork(&a.applyVersionSearchComparisons, w.applyVersionSearchComparisons)
	addWork(&a.detectMembershipSearches, w.detectMembershipSearches)
	addWork(&a.detectMembershipSearchCompares, w.detectMembershipSearchComparisons)
	addWork(&a.applyMembershipSearches, w.applyMembershipSearches)
	addWork(&a.applyMembershipSearchCompares, w.applyMembershipSearchComparisons)
	addWork(&a.detectAssociationChecks, w.detectAssociationChecks)
	addWork(&a.detectOwnerUpdateChecks, w.detectOwnerUpdateChecks)
	addWork(&a.detectAssociationHashes, w.detectAssociationHashes)
	addWork(&a.applyAssociationHashes, w.applyAssociationHashes)
	addWork(&a.validateSeriesHashes, w.validateSeriesHashes)
	addWork(&a.detectCanonicalizationAttributes, w.detectCanonicalizationAttributes)
	addWork(&a.detectCanonicalizationBytes, w.detectCanonicalizationBytes)
	addWork(&a.applyCanonicalizationAttributes, w.applyCanonicalizationAttributes)
	addWork(&a.applyCanonicalizationBytes, w.applyCanonicalizationBytes)
	addWork(&a.clonedContentBytes, w.clonedContentBytes)
	addWork(&a.ownerIdentityDecodes, w.ownerIdentityDecodes)
	addWork(&a.ownerIdentityBytes, w.ownerIdentityBytes)
	addWork(&a.newConditionMaps, w.newConditionMaps)
	addWork(&a.conditionAssignments, w.conditionAssignments)
	addWork(&a.conditionBytesConstructed, w.conditionBytesConstructed)
	addWork(&a.toCopySets, w.toCopySets)
	addWork(&a.toCopyInsertions, w.toCopyInsertions)
	addWork(&a.ownAttributeBucketCalls, w.ownAttributeBucketCalls)
	addWork(&a.ownAttributeBucketOwned, w.ownAttributeBucketOwned)
	addWork(&a.versionInsertTailPositions, w.versionInsertTailPositions)
	addWork(&a.versionInsertPreRows, w.versionInsertPreRows)
	addWork(&a.versionInsertShiftedRows, w.versionInsertShiftedRows)
	addWork(&a.versionInsertGrowths, w.versionInsertGrowths)
	addWork(&a.evictionRetainedRowsCopied, w.evictionRetainedRowsCopied)
	addWork(&a.evictionVersionsScanned, w.evictionVersionsScanned)
	addWork(&a.evictionAttributesScanned, w.evictionAttributesScanned)
	addWork(&a.evictionConditionWrites, w.evictionConditionWrites)
}

// snapshot returns the accumulated accounting, including the attempt count that is
// published before a transaction commits.
func (a *metadataWorkAccumulator) snapshot() metadataWork {
	return metadataWork{
		attempts:                          int(a.attempts.Load()),
		commits:                           int(a.commits.Load()),
		metadataTransactions:              int(a.transactions.Load()),
		inputSamples:                      int(a.inputSamples.Load()),
		inputSeries:                       int(a.inputSeries.Load()),
		inputAssociations:                 int(a.inputAssociations.Load()),
		inputOwnerUpdates:                 int(a.inputOwnerUpdates.Load()),
		residentOwners:                    int(a.residentOwners.Load()),
		residentSeries:                    int(a.residentSeries.Load()),
		changedOwners:                     int(a.changedOwners.Load()),
		batchChangeChecks:                 int(a.batchChangeChecks.Load()),
		batchChangeUnchanged:              int(a.batchChangeUnchanged.Load()),
		batchChangeSelected:               int(a.batchChangeSelected.Load()),
		clones:                            int(a.clones.Load()),
		cloneByOwnerEntries:               int(a.cloneByOwnerEntries.Load()),
		cloneOwnerStructs:                 int(a.cloneOwnerStructs.Load()),
		cloneBySeriesEntries:              int(a.cloneBySeriesEntries.Load()),
		cloneChainsEntries:                int(a.cloneChainsEntries.Load()),
		cloneOwnerIndex:                   int(a.cloneOwnerIndex.Load()),
		cloneAttributeConds:               int(a.cloneAttributeConds.Load()),
		ownerHistoryResolutions:           int(a.ownerHistoryResolutions.Load()),
		versionSearches:                   int(a.versionSearches.Load()),
		versionSearchComparisons:          int(a.versionSearchComparisons.Load()),
		membershipSearches:                int(a.membershipSearches.Load()),
		membershipSearchCompares:          int(a.membershipSearchCompares.Load()),
		seriesHashes:                      int(a.seriesHashes.Load()),
		completeLabelChecks:               int(a.completeLabelChecks.Load()),
		changeDetectionCanonicalizations:  int(a.changeDetectionCanonicalizations.Load()),
		applyCanonicalizations:            int(a.applyCanonicalizations.Load()),
		contentComparisons:                int(a.contentComparisons.Load()),
		contentClones:                     int(a.contentClones.Load()),
		clonedContentAttributes:           int(a.clonedContentAttributes.Load()),
		versionsInserted:                  int(a.versionsInserted.Load()),
		versionBoundsExtended:             int(a.versionBoundsExtended.Load()),
		versionDuplicateNoops:             int(a.versionDuplicateNoops.Load()),
		versionsCoalesced:                 int(a.versionsCoalesced.Load()),
		versionsEvicted:                   int(a.versionsEvicted.Load()),
		membershipsInserted:               int(a.membershipsInserted.Load()),
		membershipOwnershipMoves:          int(a.membershipOwnershipMoves.Load()),
		membershipBoundsExtended:          int(a.membershipBoundsExtended.Load()),
		membershipDuplicateNoops:          int(a.membershipDuplicateNoops.Load()),
		membershipRowsCollapsed:           int(a.membershipRowsCollapsed.Load()),
		ownerCOPCopies:                    int(a.ownerCOPCopies.Load()),
		ownerCOPVersionRows:               int(a.ownerCOPVersionRows.Load()),
		ownerCOPIndexEntries:              int(a.ownerCOPIndexEntries.Load()),
		membershipCOPGroups:               int(a.membershipCOPGroups.Load()),
		membershipCOPKindRows:             int(a.membershipCOPKindRows.Load()),
		membershipCOPRows:                 int(a.membershipCOPRows.Load()),
		indexCalls:                        int(a.indexCalls.Load()),
		indexVersionsVisited:              int(a.indexVersionsVisited.Load()),
		indexIdentifyingVisits:            int(a.indexIdentifyingVisits.Load()),
		indexContentAttributeVisits:       int(a.indexContentAttributeVisits.Load()),
		indexDescriptiveAttributeVisits:   int(a.indexDescriptiveAttributeVisits.Load()),
		indexOldConditions:                int(a.indexOldConditions.Load()),
		indexNewConditions:                int(a.indexNewConditions.Load()),
		indexRetainedConditions:           int(a.indexRetainedConditions.Load()),
		indexDeletedConditions:            int(a.indexDeletedConditions.Load()),
		indexAddedConditions:              int(a.indexAddedConditions.Load()),
		indexBucketClones:                 int(a.indexBucketClones.Load()),
		indexBucketClonedEntries:          int(a.indexBucketClonedEntries.Load()),
		indexGlobalRangeUpdates:           int(a.indexGlobalRangeUpdates.Load()),
		indexHistoryClears:                int(a.indexHistoryClears.Load()),
		indexHistoryEntriesWritten:        int(a.indexHistoryEntriesWritten.Load()),
		detectVersionSearches:             int(a.detectVersionSearches.Load()),
		detectVersionSearchComparisons:    int(a.detectVersionSearchComparisons.Load()),
		applyVersionSearches:              int(a.applyVersionSearches.Load()),
		applyVersionSearchComparisons:     int(a.applyVersionSearchComparisons.Load()),
		detectMembershipSearches:          int(a.detectMembershipSearches.Load()),
		detectMembershipSearchComparisons: int(a.detectMembershipSearchCompares.Load()),
		applyMembershipSearches:           int(a.applyMembershipSearches.Load()),
		applyMembershipSearchComparisons:  int(a.applyMembershipSearchCompares.Load()),
		detectAssociationChecks:           int(a.detectAssociationChecks.Load()),
		detectOwnerUpdateChecks:           int(a.detectOwnerUpdateChecks.Load()),
		detectAssociationHashes:           int(a.detectAssociationHashes.Load()),
		applyAssociationHashes:            int(a.applyAssociationHashes.Load()),
		validateSeriesHashes:              int(a.validateSeriesHashes.Load()),
		detectCanonicalizationAttributes:  int(a.detectCanonicalizationAttributes.Load()),
		detectCanonicalizationBytes:       int(a.detectCanonicalizationBytes.Load()),
		applyCanonicalizationAttributes:   int(a.applyCanonicalizationAttributes.Load()),
		applyCanonicalizationBytes:        int(a.applyCanonicalizationBytes.Load()),
		clonedContentBytes:                int(a.clonedContentBytes.Load()),
		ownerIdentityDecodes:              int(a.ownerIdentityDecodes.Load()),
		ownerIdentityBytes:                int(a.ownerIdentityBytes.Load()),
		newConditionMaps:                  int(a.newConditionMaps.Load()),
		conditionAssignments:              int(a.conditionAssignments.Load()),
		conditionBytesConstructed:         int(a.conditionBytesConstructed.Load()),
		toCopySets:                        int(a.toCopySets.Load()),
		toCopyInsertions:                  int(a.toCopyInsertions.Load()),
		ownAttributeBucketCalls:           int(a.ownAttributeBucketCalls.Load()),
		ownAttributeBucketOwned:           int(a.ownAttributeBucketOwned.Load()),
		versionInsertTailPositions:        int(a.versionInsertTailPositions.Load()),
		versionInsertPreRows:              int(a.versionInsertPreRows.Load()),
		versionInsertShiftedRows:          int(a.versionInsertShiftedRows.Load()),
		versionInsertGrowths:              int(a.versionInsertGrowths.Load()),
		evictionRetainedRowsCopied:        int(a.evictionRetainedRowsCopied.Load()),
		evictionVersionsScanned:           int(a.evictionVersionsScanned.Load()),
		evictionAttributesScanned:         int(a.evictionAttributesScanned.Load()),
		evictionConditionWrites:           int(a.evictionConditionWrites.Load()),
	}
}

// observeAttempt records one AppendBatch entry, so attempt and commit counts can be
// compared even though a rejected transaction publishes no work.
func (a *metadataWorkAccumulator) observeAttempt() { a.attempts.Add(1) }

// addWork adds n to the counter when n is non-zero, so a field that did not occur
// costs no atomic operation.
func addWork(dst *atomic.Uint64, n int) {
	if n != 0 {
		dst.Add(uint64(n))
	}
}

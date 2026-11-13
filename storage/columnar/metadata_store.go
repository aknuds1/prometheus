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
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/prometheus/prometheus/model/labels"
)

var (
	// ErrMetadataDisabled is returned when a batch asks an explicitly
	// metadata-disabled store to initialize or update the native catalog.
	ErrMetadataDisabled = errors.New("columnar: native metadata is disabled")
	// ErrConflictingMetadata describes two different contents at the same owner and
	// effective timestamp. The store must not silently choose one by map order.
	ErrConflictingMetadata = errors.New("columnar: conflicting metadata at the same owner and effective time")
	// ErrConflictingMembership describes different owners asserted for one
	// series, metadata kind and effective timestamp. It is an association error,
	// not data to join arbitrarily on read.
	ErrConflictingMembership = errors.New("columnar: conflicting metadata ownership for series")
	// ErrSeriesHashCollision prevents metadata for unrelated labels merging through
	// an owner index keyed by compressed series references.
	ErrSeriesHashCollision = errors.New("columnar: series hash collision")
)

// metadataOwnerHistory is the mutable head history for one owner.
type metadataOwnerHistory struct {
	versions    []Version
	truncated   bool
	index       map[string]struct{} // Inverted attribute index: name/value -> membership lookup.
	copyTouched bool
}

// seriesMembershipRow is one owner and its series membership. No attributes are
// copied per member; the owner carries its own versions.
type seriesMembershipRow struct {
	owner OwnerKey
	ls    labels.Labels
	// effectiveFrom is when this owner became associated with the series. A later
	// Resource content change must not mutate this association.
	effectiveFrom int64
	observedUntil int64
}

// metadataStore is the logical metadata catalog. It has no dependency on a
// sample's physical codec. Storage serializes access with its transaction lock;
// the catalog itself does not add another lock ordering requirement.
type metadataStore struct {
	byOwner  map[OwnerKey]*metadataOwnerHistory
	bySeries map[uint64]map[MetadataKind][]seriesMembershipRow
	chains   map[uint64]labels.Labels

	indexedResourceAttrs map[string]struct{}
	attributeIndex       map[string]map[OwnerKey]metadataIndexedRange
	ownerIndex           map[OwnerKey]map[string]metadataIndexedRange
	versionCap           int
	reads                *readStats

	// copyOnWrite is disabled for normal construction and enabled only on a
	// cloned transaction before the Storage lock publishes it.
	copyOnWrite       bool
	copiedMemberships map[metadataMembershipGroup]struct{}
	canonicalVersions map[metadataVersionKey]string

	// ownedAttributeBuckets holds the attribute-index conditions whose bucket this
	// cloned transaction has already cloned or created. A bucket is copied at most
	// once per transaction, on its first mutation, so repeated owner-version
	// mutations do not clone the same shared bucket again. The set belongs to one
	// cloned transaction: clone allocates a fresh set and it is never reused across
	// batches or after publication.
	ownedAttributeBuckets map[string]struct{}
}

type metadataVersionKey struct {
	owner         OwnerKey
	effectiveFrom int64
}

// newMetadataStore returns an operational metadata catalog with an optional
// per-owner version cap. Zero means no cap; the caller then records pathological
// version cardinality before assuming bounded memory.
func newMetadataStore(versionCap int) *metadataStore {
	return &metadataStore{
		byOwner:              make(map[OwnerKey]*metadataOwnerHistory),
		bySeries:             make(map[uint64]map[MetadataKind][]seriesMembershipRow),
		chains:               make(map[uint64]labels.Labels),
		indexedResourceAttrs: map[string]struct{}{},
		attributeIndex:       map[string]map[OwnerKey]metadataIndexedRange{},
		ownerIndex:           map[OwnerKey]map[string]metadataIndexedRange{},
		versionCap:           versionCap,
		canonicalVersions:    map[metadataVersionKey]string{},
	}
}

func (s *metadataStore) update(update Update) error {
	if update.Owner.Kind > MetadataSource {
		return fmt.Errorf("columnar: invalid metadata kind %d", update.Owner.Kind)
	}
	return s.updateHistory(s.ownerHistory(update.Owner, nil), update, associationDerivation{}, nil, nil)
}

func (s *metadataStore) canonicalVersion(owner OwnerKey, version Version) string {
	key := metadataVersionKey{owner: owner, effectiveFrom: version.EffectiveFrom}
	if canonical, ok := s.canonicalVersions[key]; ok {
		return canonical
	}
	canonical := version.Content.Canonical()
	s.canonicalVersions[key] = canonical
	return canonical
}

func (s *metadataStore) deleteCanonicalVersion(owner OwnerKey, effectiveFrom int64) {
	delete(s.canonicalVersions, metadataVersionKey{owner: owner, effectiveFrom: effectiveFrom})
}

func (s *metadataStore) deleteOwner(owner OwnerKey) {
	for key := range s.canonicalVersions {
		if key.owner == owner {
			delete(s.canonicalVersions, key)
		}
	}
	for condition := range s.ownerIndex[owner] {
		// Retention deletes owners from a cloned catalog, so a bucket still shared
		// with the published catalog must be copied before this transaction removes
		// an entry from it.
		s.ownAttributeBucket(condition, nil)
		delete(s.attributeIndex[condition], owner)
		if len(s.attributeIndex[condition]) == 0 {
			delete(s.attributeIndex, condition)
		}
	}
	delete(s.ownerIndex, owner)
	delete(s.byOwner, owner)
}

func (s *metadataStore) copyOwnerForWrite(owner OwnerKey, history *metadataOwnerHistory, w *metadataWork) *metadataOwnerHistory {
	if !s.copyOnWrite || history.copyTouched {
		return history
	}
	if w != nil {
		w.ownerCOPCopies++
		w.ownerCOPVersionRows += len(history.versions)
		w.ownerCOPIndexEntries += len(history.index)
	}
	copied := *history
	copied.versions = slices.Clone(history.versions)
	copied.index = maps.Clone(history.index)
	copied.copyTouched = true
	s.byOwner[owner] = &copied
	return &copied
}

func (s *metadataStore) copyMembershipForWrite(seriesHash uint64, kind MetadataKind, w *metadataWork) {
	if !s.copyOnWrite {
		return
	}
	group := metadataMembershipGroup{seriesHash: seriesHash, kind: kind}
	if _, done := s.copiedMemberships[group]; done {
		return
	}
	kinds := s.bySeries[seriesHash]
	if kinds != nil {
		if w != nil {
			w.membershipCOPGroups++
			w.membershipCOPKindRows += len(kinds)
			w.membershipCOPRows += len(kinds[kind])
		}
		kinds = maps.Clone(kinds)
		kinds[kind] = slices.Clone(kinds[kind])
		s.bySeries[seriesHash] = kinds
	}
	s.copiedMemberships[group] = struct{}{}
}

func (s *metadataStore) ownerHistory(owner OwnerKey, w *metadataWork) *metadataOwnerHistory {
	history, ok := s.byOwner[owner]
	if !ok {
		history = &metadataOwnerHistory{index: map[string]struct{}{}}
		s.byOwner[owner] = history
	}
	return s.copyOwnerForWrite(owner, history, w)
}

// updateHistory applies a version after the batch metadata transaction has
// resolved its owner once. Redundant observations update bounds only.
//
// m, when set, attributes the canonicalizations and searches this update performs
// to the ordinary path's accounting.
func (s *metadataStore) updateHistory(history *metadataOwnerHistory, update Update, derived associationDerivation, m *ordinaryDerivationStats, w *metadataWork) error {
	// An equal-time assertion must agree exactly. Do this before changing any
	// observation bounds so a rejected update leaves the owner untouched.
	i := searchVersionsFrom(history.versions, update.EffectiveFrom, w)
	countMembershipSearch(m, w)
	if i < len(history.versions) && history.versions[i].EffectiveFrom == update.EffectiveFrom {
		existing := &history.versions[i]
		if !contentsEqual(history, i, update, derived, m, w, applyCanonicalizations) {
			return ErrConflictingMetadata
		}
		previousFrom, previousUntil := existing.ObservedFrom, existing.ObservedUntil
		existing.ObservedFrom = min(existing.ObservedFrom, update.EffectiveFrom)
		existing.ObservedUntil = max(existing.ObservedUntil, update.EffectiveFrom)
		noteVersionBoundUpdate(w, previousFrom == existing.ObservedFrom && previousUntil == existing.ObservedUntil)
		return nil
	}

	// Re-observing the content already active at this instant is not a change
	// point. This also handles a backfilled observation between two versions.
	if i > 0 && contentsEqual(history, i-1, update, derived, m, w, applyCanonicalizations) {
		existing := &history.versions[i-1]
		previousFrom, previousUntil := existing.ObservedFrom, existing.ObservedUntil
		existing.ObservedFrom = min(existing.ObservedFrom, update.EffectiveFrom)
		existing.ObservedUntil = max(existing.ObservedUntil, update.EffectiveFrom)
		noteVersionBoundUpdate(w, previousFrom == existing.ObservedFrom && previousUntil == existing.ObservedUntil)
		return nil
	}

	if w != nil {
		w.contentClones++
		w.clonedContentAttributes += len(update.Content.Attributes)
	}
	version := Version{
		Content:       update.Content.Clone(),
		EffectiveFrom: update.EffectiveFrom,
		ObservedFrom:  update.EffectiveFrom,
		ObservedUntil: update.EffectiveFrom,
	}
	history.versions = slices.Insert(history.versions, i, version)
	if w != nil {
		w.versionsInserted++
		w.noteVersionOwner(update.Owner)
	}

	// A backfilled change can make its successor redundant. Preserve both
	// observation ranges while retaining only the actual change point.
	if i+1 < len(history.versions) && contentsEqual(history, i+1, update, derived, m, w, applyCanonicalizations) {
		next := history.versions[i+1]
		// The removed change point's identity must not outlive it: a later observation in
		// this transaction may state a new version at the same effective time.
		delete(derived.versions, metadataVersionKey{owner: update.Owner, effectiveFrom: next.EffectiveFrom})
		history.versions[i].ObservedFrom = min(history.versions[i].ObservedFrom, next.ObservedFrom)
		history.versions[i].ObservedUntil = max(history.versions[i].ObservedUntil, next.ObservedUntil)
		history.versions = slices.Delete(history.versions, i+1, i+2)
		if w != nil {
			w.versionsCoalesced++
		}
		s.deleteCanonicalVersion(update.Owner, next.EffectiveFrom)
	}
	if s.versionCap > 0 && len(history.versions) > s.versionCap {
		drop := len(history.versions) - s.versionCap
		if w != nil {
			w.versionsEvicted += drop
		}
		for _, evicted := range history.versions[:drop] {
			s.deleteCanonicalVersion(update.Owner, evicted.EffectiveFrom)
		}
		// Drop oldest only after marking the history as intentionally truncated.
		// This remains observable rather than becoming a silent current-value lookup.
		// Clone drops references to evicted content in the old backing array.
		history.versions = slices.Clone(history.versions[drop:])
		history.truncated = true
		// The provisional inverted index must not retain evicted values either.
		clear(history.index)
		for _, version := range history.versions {
			for key, value := range version.Content.Attributes {
				history.index[attributeCondition(key, value)] = struct{}{}
			}
		}
	}

	if update.Owner.Kind == MetadataResource {
		s.indexResourceOwner(update.Owner, history, w)
	}
	return nil
}

// noteVersionBoundUpdate records one observation-only bound update and whether it
// repeated the bounds the version already held.
func noteVersionBoundUpdate(w *metadataWork, unchanged bool) {
	if w == nil {
		return
	}
	w.versionBoundsExtended++
	if unchanged {
		w.versionDuplicateNoops++
	}
}

// searchVersionsFrom returns the first index whose effective time is at or after t,
// which is the owner-version ordered search, and counts its comparisons.
func searchVersionsFrom(versions []Version, t int64, w *metadataWork) int {
	lo, hi := 0, len(versions)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if w != nil {
			w.versionSearchComparisons++
		}
		if versions[mid].EffectiveFrom < t {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if w != nil {
		w.versionSearches++
	}
	return lo
}

// searchMembershipsFrom returns the first index whose effective time is at or after t,
// which is the membership ordered search, and counts its comparisons.
func searchMembershipsFrom(rows []seriesMembershipRow, t int64, w *metadataWork) int {
	lo, hi := 0, len(rows)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if w != nil {
			w.membershipSearchCompares++
		}
		if rows[mid].effectiveFrom < t {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if w != nil {
		w.membershipSearches++
	}
	return lo
}

// countMembershipSearch attributes one ordered search to each path that counts it.
func countMembershipSearch(m *ordinaryDerivationStats, w *metadataWork) {
	if m != nil {
		m.MembershipSearches++
	}
	if w != nil {
		w.membershipSearches++
	}
}

// countCompleteLabelCheck attributes one complete-label comparison to the
// ordinary path.
func countCompleteLabelCheck(m *ordinaryDerivationStats, w *metadataWork) {
	if m != nil {
		m.CompleteLabelChecks++
	}
	if w != nil {
		w.completeLabelChecks++
	}
}

// canonicalizationPhase selects the phase a content comparison is attributed to.
type canonicalizationPhase int

const (
	changeDetectionCanonicalizations canonicalizationPhase = iota
	applyCanonicalizations
)

// countCanonicalization attributes one Content.Canonical call to each path that
// counts it, split by the phase that needed the identity.
func countCanonicalization(m *ordinaryDerivationStats, w *metadataWork, phase canonicalizationPhase) {
	if m != nil {
		if phase == changeDetectionCanonicalizations {
			m.ChangeDetectionContentCanonicalizations++
		} else {
			m.ApplyContentCanonicalizations++
		}
	}
	if w == nil {
		return
	}
	if phase == changeDetectionCanonicalizations {
		w.changeDetectionCanonicalizations++
	} else {
		w.applyCanonicalizations++
	}
}

// storedVersionCanonical returns the canonical content of stored version i, reusing the
// transaction's memo when the observation brought a derived identity with it.
func storedVersionCanonical(history *metadataOwnerHistory, i int, update Update, derived associationDerivation, m *ordinaryDerivationStats, w *metadataWork, phase canonicalizationPhase) string {
	key := metadataVersionKey{owner: update.Owner, effectiveFrom: history.versions[i].EffectiveFrom}
	if derived.versions != nil {
		if canonical, ok := derived.versions[key]; ok {
			return canonical
		}
	}
	countCanonicalization(m, w, phase)
	canonical := history.versions[i].Content.Canonical()
	if derived.versions != nil {
		derived.versions[key] = canonical
	}
	return canonical
}

// observedCanonical returns the observation's canonical content, reusing the identity
// the caller derived when it stated one.
func observedCanonical(update Update, derived associationDerivation, m *ordinaryDerivationStats, w *metadataWork, phase canonicalizationPhase) string {
	if derived.canonical != "" {
		return derived.canonical
	}
	countCanonicalization(m, w, phase)
	return update.Content.Canonical()
}

// contentsEqual reports whether stored version i and the observation state equal
// content. The comparison is always between two complete canonical forms: a derived
// identity removes deriving one of them again, it never replaces the comparison.
func contentsEqual(history *metadataOwnerHistory, i int, update Update, derived associationDerivation, m *ordinaryDerivationStats, w *metadataWork, phase canonicalizationPhase) bool {
	if w != nil {
		w.contentComparisons++
	}
	return storedVersionCanonical(history, i, update, derived, m, w, phase) ==
		observedCanonical(update, derived, m, w, phase)
}

// associate links one series into the owner, with an effective time.
// Owner history and membership are deliberately separate. A new member never
// duplicates the Resource owner's descriptive attributes.
func (s *metadataStore) associateWithHistory(seriesHash uint64, ls labels.Labels, update Update, history *metadataOwnerHistory, derived associationDerivation, m *ordinaryDerivationStats, w *metadataWork) error {
	// Reject all identity conflicts before mutating the owner's history.
	if err := s.validateLabels(update.Owner, ls, seriesHash, m, w); err != nil {
		return err
	}
	if existing, ok := s.chains[seriesHash]; ok {
		countCompleteLabelCheck(m, w)
		if !labels.Equal(existing, ls) {
			return fmt.Errorf("%w: %q vs %q", ErrSeriesHashCollision, existing, ls)
		}
	}
	rows := s.bySeries[seriesHash][update.Owner.Kind]
	for _, row := range rows {
		if row.effectiveFrom == update.EffectiveFrom && row.owner != update.Owner {
			return fmt.Errorf("%w: %s and %s at effective time %d",
				ErrConflictingMembership, row.owner.ID, update.Owner.ID, update.EffectiveFrom)
		}
	}
	if err := s.updateHistory(history, update, derived, m, w); err != nil {
		return err
	}
	return s.attachMembership(seriesHash, ls, update.Owner, update.EffectiveFrom, update.EffectiveFrom, m, w)
}

// attachMembership records only an owner relationship. Unlike associate, it
// never creates an owner version. Restoring rows retains their observation bound.
func (s *metadataStore) attachMembership(seriesHash uint64, ls labels.Labels, owner OwnerKey, effectiveFrom, observedUntil int64, m *ordinaryDerivationStats, w *metadataWork) error {
	s.copyMembershipForWrite(seriesHash, owner.Kind, w)
	if err := s.validateLabels(owner, ls, seriesHash, m, w); err != nil {
		return err
	}
	if existing, ok := s.chains[seriesHash]; ok {
		countCompleteLabelCheck(m, w)
		if !labels.Equal(existing, ls) {
			return fmt.Errorf("%w: %q vs %q", ErrSeriesHashCollision, existing, ls)
		}
	}
	kinds := s.bySeries[seriesHash]
	if kinds == nil {
		kinds = map[MetadataKind][]seriesMembershipRow{}
		s.bySeries[seriesHash] = kinds
	}
	rows := kinds[owner.Kind]

	i := searchMembershipsFrom(rows, effectiveFrom, w)
	countMembershipSearch(m, w)
	if i < len(rows) && rows[i].effectiveFrom == effectiveFrom {
		if rows[i].owner != owner {
			return fmt.Errorf("%w: %s and %s at effective time %d", ErrConflictingMembership,
				rows[i].owner.ID, owner.ID, effectiveFrom)
		}
		previous := rows[i].observedUntil
		rows[i].observedUntil = max(rows[i].observedUntil, observedUntil)
		if previous == rows[i].observedUntil {
			if w != nil {
				w.membershipDuplicateNoops++
			}
		} else if w != nil {
			w.membershipBoundsExtended++
		}
		kinds[owner.Kind] = rows
		return nil
	}
	if i > 0 && rows[i-1].owner == owner {
		// Content updates and repeated observations of the active owner do not
		// create memberships. Its successor still ends this membership.
		previous := rows[i-1].observedUntil
		rows[i-1].observedUntil = max(rows[i-1].observedUntil, observedUntil)
		if previous == rows[i-1].observedUntil {
			if w != nil {
				w.membershipDuplicateNoops++
			}
		} else if w != nil {
			w.membershipBoundsExtended++
		}
		kinds[owner.Kind] = rows
		return nil
	}

	if _, ok := s.chains[seriesHash]; !ok {
		s.chains[seriesHash] = ls
	}
	rows = slices.Insert(rows, i, seriesMembershipRow{
		owner: owner, ls: ls, effectiveFrom: effectiveFrom, observedUntil: observedUntil,
	})
	if w != nil {
		w.membershipsInserted++
		if i > 0 && rows[i-1].owner != owner {
			w.membershipOwnershipMoves++
		}
	}
	if i+1 < len(rows) && rows[i].owner == rows[i+1].owner {
		// Backfilled ownership subsumes an equal successor membership.
		rows[i].observedUntil = max(rows[i].observedUntil, rows[i+1].observedUntil)
		rows = slices.Delete(rows, i+1, i+2)
		if w != nil {
			w.membershipRowsCollapsed++
		}
	}
	kinds[owner.Kind] = rows
	return nil
}

// at resolves the owner's current value at timestamp t using
// latest-effectiveFrom <= t. There is no lookahead into a future version.
func (s *metadataStore) at(owner OwnerKey, t int64) (Content, bool, bool) {
	version, ok, truncated := s.atVersion(owner, t)
	return version.Content, ok, truncated
}

func (s *metadataStore) atVersion(owner OwnerKey, t int64) (Version, bool, bool) {
	history, ok := s.byOwner[owner]
	if !ok {
		return Version{}, false, false
	}
	i := sort.Search(len(history.versions), func(i int) bool {
		return history.versions[i].EffectiveFrom > t
	})
	if i == 0 {
		return Version{}, false, history.truncated
	}
	version := history.versions[i-1]
	version.Content = version.Content.Clone()
	return version, true, history.truncated
}

// ownersAt returns the associated owner for one kind at t. If no owner
// association is effective yet, it returns false.
func (s *metadataStore) ownersAt(seriesHash uint64, kind MetadataKind, t int64) (OwnerKey, bool) {
	rows := s.bySeries[seriesHash][kind]
	i := sort.Search(len(rows), func(i int) bool {
		return rows[i].effectiveFrom > t
	})
	if i == 0 {
		return OwnerKey{}, false
	}
	return rows[i-1].owner, true
}

// getAt resolves series→owner→version, with no label-name mutation.
func (s *metadataStore) getAt(seriesHash uint64, kind MetadataKind, t int64) (Content, bool, bool) {
	owner, ok := s.ownersAt(seriesHash, kind, t)
	if !ok {
		return Content{}, false, false
	}
	return s.at(owner, t)
}

// history returns a copy-safe snapshot of the owner's version history.
func (s *metadataStore) history(owner OwnerKey) (OwnerHistory, bool) {
	byOwner, ok := s.byOwner[owner]
	if !ok {
		return OwnerHistory{}, false
	}
	return OwnerHistory{Owner: owner, Versions: cloneVersions(byOwner.versions), Truncated: byOwner.truncated}, ok
}

func cloneVersions(in []Version) []Version {
	out := make([]Version, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Content = in[i].Content.Clone()
	}
	return out
}

// resourceAttributeNames returns names from retained Resource versions only.
// Other owner kinds are deliberately absent from Resource discovery.
func (s *metadataStore) resourceAttributeNames() []string {
	unique := map[string]struct{}{}
	for owner, history := range s.byOwner {
		if owner.Kind != MetadataResource {
			continue
		}
		for _, version := range history.versions {
			for name := range version.Content.Attributes {
				unique[name] = struct{}{}
			}
		}
	}
	return slices.Sorted(maps.Keys(unique))
}

func attributeCondition(name, value string) string {
	return name + "\x00" + value
}

// updateAndAssociate checks both side of a metadata update belongs to the same
// real identity before recording any owner history.
func (s *metadataStore) validateLabels(owner OwnerKey, ls labels.Labels, seriesHash uint64, m *ordinaryDerivationStats, w *metadataWork) error {
	if seriesHash == 0 {
		if m != nil {
			m.ApplySeriesHashes++
		}
		if w != nil {
			w.seriesHashes++
		}
		seriesHash = metadataSeriesKey(ls)
	}
	if existing, ok := s.chains[seriesHash]; ok {
		countCompleteLabelCheck(m, w)
		if !labels.Equal(existing, ls) {
			return fmt.Errorf("%w: %q vs %q", ErrSeriesHashCollision, existing, ls)
		}
	}
	if owner.Kind == MetadataResource {
		fromLabels := ls.Get(OTELResourceLabel)
		if got, want := fromLabels, owner.Ref(); got != want {
			return fmt.Errorf("%w: Resource identity label %q has value %q but owner key %q", ErrConflictingMembership, OTELResourceLabel, fromLabels, owner.ID)
		}
	}
	return nil
}

// ownerUpdateChanges reports whether update would alter the published owner
// history or extend a recorded observation bound. A duplicate observation inside
// the bounds a version already records is not a change: it would write the same
// bounds again, so the transaction that would record it is skipped.
func (s *metadataStore) ownerUpdateChanges(update Update, derived associationDerivation, m *ordinaryDerivationStats, w *metadataWork) bool {
	history, ok := s.byOwner[update.Owner]
	if !ok {
		return true
	}
	i := searchVersionsFrom(history.versions, update.EffectiveFrom, w)
	if i < len(history.versions) && history.versions[i].EffectiveFrom == update.EffectiveFrom {
		return !contentsEqual(history, i, update, derived, m, w, changeDetectionCanonicalizations)
	}
	if i > 0 && contentsEqual(history, i-1, update, derived, m, w, changeDetectionCanonicalizations) {
		version := history.versions[i-1]
		return update.EffectiveFrom < version.ObservedFrom || update.EffectiveFrom > version.ObservedUntil
	}
	return true
}

// associationChanges reports whether update would change owner history, extend an
// observation bound, or change the series membership for its kind.
func (s *metadataStore) associationChanges(seriesHash uint64, ls labels.Labels, update Update, derived associationDerivation, m *ordinaryDerivationStats, w *metadataWork) bool {
	if s.ownerUpdateChanges(update, derived, m, w) {
		return true
	}
	if existing, ok := s.chains[seriesHash]; ok {
		countCompleteLabelCheck(m, w)
		if !labels.Equal(existing, ls) {
			return true
		}
	}
	rows := s.bySeries[seriesHash][update.Owner.Kind]
	i := searchMembershipsFrom(rows, update.EffectiveFrom, w)
	countMembershipSearch(m, w)
	if i < len(rows) && rows[i].effectiveFrom == update.EffectiveFrom {
		return rows[i].owner != update.Owner
	}
	if i > 0 && rows[i-1].owner == update.Owner {
		// A repeated observation of the active owner is a change only when it
		// extends the membership's own observation bound.
		return update.EffectiveFrom > rows[i-1].observedUntil
	}
	return true
}

// batchChanges reports whether any update in the batch would change the published
// metadata state or extend an observation bound.
func (s *metadataStore) batchChanges(batch Batch, m *ordinaryDerivationStats, w *metadataWork) bool {
	for i, association := range batch.Updates {
		identity := associationIdentityAt(batch.associationIdentities, i)
		seriesHash := identity.seriesHash
		if seriesHash == 0 {
			if m != nil {
				m.ChangeDetectionSeriesHashes++
			}
			if w != nil {
				w.seriesHashes++
			}
			seriesHash = metadataSeriesKey(association.Labels)
		}
		derived := associationDerivation{canonical: identity.canonical}
		if s.associationChanges(seriesHash, association.Labels, association.Update, derived, m, w) {
			return true
		}
	}
	return slices.ContainsFunc(batch.OwnerUpdates, func(update Update) bool {
		return s.ownerUpdateChanges(update, associationDerivation{}, m, w)
	})
}

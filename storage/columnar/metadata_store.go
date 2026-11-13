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
	return s.updateHistory(s.ownerHistory(update.Owner), update, nil)
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
		delete(s.attributeIndex[condition], owner)
		if len(s.attributeIndex[condition]) == 0 {
			delete(s.attributeIndex, condition)
		}
	}
	delete(s.ownerIndex, owner)
	delete(s.byOwner, owner)
}

func (s *metadataStore) copyOwnerForWrite(owner OwnerKey, history *metadataOwnerHistory) *metadataOwnerHistory {
	if !s.copyOnWrite || history.copyTouched {
		return history
	}
	copied := *history
	copied.versions = slices.Clone(history.versions)
	copied.index = maps.Clone(history.index)
	copied.copyTouched = true
	s.byOwner[owner] = &copied
	return &copied
}

func (s *metadataStore) copyMembershipForWrite(seriesHash uint64, kind MetadataKind) {
	if !s.copyOnWrite {
		return
	}
	group := metadataMembershipGroup{seriesHash: seriesHash, kind: kind}
	if _, done := s.copiedMemberships[group]; done {
		return
	}
	kinds := s.bySeries[seriesHash]
	if kinds != nil {
		kinds = maps.Clone(kinds)
		kinds[kind] = slices.Clone(kinds[kind])
		s.bySeries[seriesHash] = kinds
	}
	s.copiedMemberships[group] = struct{}{}
}

func (s *metadataStore) ownerHistory(owner OwnerKey) *metadataOwnerHistory {
	history, ok := s.byOwner[owner]
	if !ok {
		history = &metadataOwnerHistory{index: map[string]struct{}{}}
		s.byOwner[owner] = history
	}
	return s.copyOwnerForWrite(owner, history)
}

// updateHistory applies a version after the batch metadata transaction has
// resolved its owner once. Redundant observations update bounds only.
//
// m, when set, attributes the canonicalizations and searches this update performs
// to the ordinary path's accounting.
func (s *metadataStore) updateHistory(history *metadataOwnerHistory, update Update, m *ordinaryDerivationStats) error {
	// An equal-time assertion must agree exactly. Do this before changing any
	// observation bounds so a rejected update leaves the owner untouched.
	i := sort.Search(len(history.versions), func(i int) bool {
		return history.versions[i].EffectiveFrom >= update.EffectiveFrom
	})
	countMembershipSearch(m)
	if i < len(history.versions) && history.versions[i].EffectiveFrom == update.EffectiveFrom {
		existing := &history.versions[i]
		if !contentsEqualApplying(*existing, update, m) {
			return ErrConflictingMetadata
		}
		existing.ObservedFrom = min(existing.ObservedFrom, update.EffectiveFrom)
		existing.ObservedUntil = max(existing.ObservedUntil, update.EffectiveFrom)
		return nil
	}

	// Re-observing the content already active at this instant is not a change
	// point. This also handles a backfilled observation between two versions.
	if i > 0 && contentsEqualApplying(history.versions[i-1], update, m) {
		existing := &history.versions[i-1]
		existing.ObservedFrom = min(existing.ObservedFrom, update.EffectiveFrom)
		existing.ObservedUntil = max(existing.ObservedUntil, update.EffectiveFrom)
		return nil
	}

	version := Version{
		Content:       update.Content.Clone(),
		EffectiveFrom: update.EffectiveFrom,
		ObservedFrom:  update.EffectiveFrom,
		ObservedUntil: update.EffectiveFrom,
	}
	history.versions = slices.Insert(history.versions, i, version)

	// A backfilled change can make its successor redundant. Preserve both
	// observation ranges while retaining only the actual change point.
	if i+1 < len(history.versions) && contentsEqualBoth(history.versions[i], history.versions[i+1], m) {
		next := history.versions[i+1]
		history.versions[i].ObservedFrom = min(history.versions[i].ObservedFrom, next.ObservedFrom)
		history.versions[i].ObservedUntil = max(history.versions[i].ObservedUntil, next.ObservedUntil)
		history.versions = slices.Delete(history.versions, i+1, i+2)
		s.deleteCanonicalVersion(update.Owner, next.EffectiveFrom)
	}
	if s.versionCap > 0 && len(history.versions) > s.versionCap {
		drop := len(history.versions) - s.versionCap
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
		s.indexResourceOwner(update.Owner, history)
	}
	return nil
}

func contentsEqual(a, b Content) bool {
	return a.Canonical() == b.Canonical()
}

// countMembershipSearch attributes one ordered search to the ordinary path.
func countMembershipSearch(m *ordinaryDerivationStats) {
	if m != nil {
		m.MembershipSearches++
	}
}

// countCompleteLabelCheck attributes one complete-label comparison to the
// ordinary path.
func countCompleteLabelCheck(m *ordinaryDerivationStats) {
	if m != nil {
		m.CompleteLabelChecks++
	}
}

// contentsEqualApplying reports whether stored and update state equal content,
// attributed to the ordinary apply phase. It performs the same two
// canonicalizations contentsEqual performs.
func contentsEqualApplying(stored Version, update Update, m *ordinaryDerivationStats) bool {
	if m != nil {
		m.ApplyContentCanonicalizations += 2
	}
	return contentsEqual(stored.Content, update.Content)
}

// contentsEqualBoth is contentsEqualApplying for two stored versions.
func contentsEqualBoth(a, b Version, m *ordinaryDerivationStats) bool {
	if m != nil {
		m.ApplyContentCanonicalizations += 2
	}
	return contentsEqual(a.Content, b.Content)
}

// contentsEqualDetecting is the change-detection counterpart.
func contentsEqualDetecting(a, b Content, m *ordinaryDerivationStats) bool {
	if m != nil {
		m.ChangeDetectionContentCanonicalizations += 2
	}
	return contentsEqual(a, b)
}

// associate links one series into the owner, with an effective time.
// Owner history and membership are deliberately separate. A new member never
// duplicates the Resource owner's descriptive attributes.
func (s *metadataStore) associateWithHistory(seriesHash uint64, ls labels.Labels, update Update, history *metadataOwnerHistory, m *ordinaryDerivationStats) error {
	// Reject all identity conflicts before mutating the owner's history.
	if err := s.validateLabels(update.Owner, ls, m); err != nil {
		return err
	}
	if existing, ok := s.chains[seriesHash]; ok {
		countCompleteLabelCheck(m)
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
	if err := s.updateHistory(history, update, m); err != nil {
		return err
	}
	return s.attachMembership(seriesHash, ls, update.Owner, update.EffectiveFrom, update.EffectiveFrom, m)
}

// attachMembership records only an owner relationship. Unlike associate, it
// never creates an owner version. Restoring rows retains their observation bound.
func (s *metadataStore) attachMembership(seriesHash uint64, ls labels.Labels, owner OwnerKey, effectiveFrom, observedUntil int64, m *ordinaryDerivationStats) error {
	s.copyMembershipForWrite(seriesHash, owner.Kind)
	if err := s.validateLabels(owner, ls, m); err != nil {
		return err
	}
	if existing, ok := s.chains[seriesHash]; ok {
		countCompleteLabelCheck(m)
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

	i := sort.Search(len(rows), func(i int) bool {
		return rows[i].effectiveFrom >= effectiveFrom
	})
	countMembershipSearch(m)
	if i < len(rows) && rows[i].effectiveFrom == effectiveFrom {
		if rows[i].owner != owner {
			return fmt.Errorf("%w: %s and %s at effective time %d", ErrConflictingMembership,
				rows[i].owner.ID, owner.ID, effectiveFrom)
		}
		rows[i].observedUntil = max(rows[i].observedUntil, observedUntil)
		kinds[owner.Kind] = rows
		return nil
	}
	if i > 0 && rows[i-1].owner == owner {
		// Content updates and repeated observations of the active owner do not
		// create memberships. Its successor still ends this membership.
		rows[i-1].observedUntil = max(rows[i-1].observedUntil, observedUntil)
		kinds[owner.Kind] = rows
		return nil
	}

	if _, ok := s.chains[seriesHash]; !ok {
		s.chains[seriesHash] = ls
	}
	rows = slices.Insert(rows, i, seriesMembershipRow{
		owner: owner, ls: ls, effectiveFrom: effectiveFrom, observedUntil: observedUntil,
	})
	if i+1 < len(rows) && rows[i].owner == rows[i+1].owner {
		// Backfilled ownership subsumes an equal successor membership.
		rows[i].observedUntil = max(rows[i].observedUntil, rows[i+1].observedUntil)
		rows = slices.Delete(rows, i+1, i+2)
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
func (s *metadataStore) validateLabels(owner OwnerKey, ls labels.Labels, m *ordinaryDerivationStats) error {
	if m != nil {
		m.ApplySeriesHashes++
	}
	if existing, ok := s.chains[metadataSeriesKey(ls)]; ok {
		countCompleteLabelCheck(m)
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
func (s *metadataStore) ownerUpdateChanges(update Update, m *ordinaryDerivationStats) bool {
	history, ok := s.byOwner[update.Owner]
	if !ok {
		return true
	}
	i := sort.Search(len(history.versions), func(i int) bool {
		return history.versions[i].EffectiveFrom >= update.EffectiveFrom
	})
	if i < len(history.versions) && history.versions[i].EffectiveFrom == update.EffectiveFrom {
		return !contentsEqualDetecting(history.versions[i].Content, update.Content, m)
	}
	if i > 0 && contentsEqualDetecting(history.versions[i-1].Content, update.Content, m) {
		version := history.versions[i-1]
		return update.EffectiveFrom < version.ObservedFrom || update.EffectiveFrom > version.ObservedUntil
	}
	return true
}

// associationChanges reports whether update would change owner history, extend an
// observation bound, or change the series membership for its kind.
func (s *metadataStore) associationChanges(seriesHash uint64, ls labels.Labels, update Update, m *ordinaryDerivationStats) bool {
	if s.ownerUpdateChanges(update, m) {
		return true
	}
	if existing, ok := s.chains[seriesHash]; ok {
		countCompleteLabelCheck(m)
		if !labels.Equal(existing, ls) {
			return true
		}
	}
	rows := s.bySeries[seriesHash][update.Owner.Kind]
	i := sort.Search(len(rows), func(i int) bool {
		return rows[i].effectiveFrom >= update.EffectiveFrom
	})
	countMembershipSearch(m)
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
func (s *metadataStore) batchChanges(batch Batch, m *ordinaryDerivationStats) bool {
	for _, association := range batch.Updates {
		if m != nil {
			m.ChangeDetectionSeriesHashes++
		}
		if s.associationChanges(metadataSeriesKey(association.Labels), association.Labels, association.Update, m) {
			return true
		}
	}
	return slices.ContainsFunc(batch.OwnerUpdates, func(update Update) bool { return s.ownerUpdateChanges(update, m) })
}

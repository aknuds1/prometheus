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
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/prometheus/prometheus/model/labels"
)

var (
	// ErrConflictingMetadata describes two different contents at the same owner and
	// effective timestamp. The store must not silently choose one by map order.
	ErrConflictingMetadata = errors.New("columnar: conflicting metadata at the same owner and effective time")
	// ErrConflictingMembership describes overlapping incomplete owner identities for
	// one series and metadata kind. It's an association error, not data to join
	// arbitrarily on read.
	ErrConflictingMembership = errors.New("columnar: conflicting metadata ownership for series")
	// ErrSeriesHashCollision prevents metadata for unrelated labels merging through
	// an owner index keyed by compressed series references.
	ErrSeriesHashCollision = errors.New("columnar: series hash collision")
)

// metadataOwnerHistory is the mutable head history for one owner.
type metadataOwnerHistory struct {
	versions  []Version
	truncated bool
	index     map[string]struct{} // inverted attribute index: name/value -> membership lookup
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
// sample's physical codec.
//
// It owns its own lock. Unlike a side-file reader, the head must be able to
// accept a new owner update while both Samples and Metadata are being ingested.
type metadataStore struct {
	byOwner  map[OwnerKey]*metadataOwnerHistory
	bySeries map[uint64]map[MetadataKind][]seriesMembershipRow
	chains   map[uint64]labels.Labels

	versionCap int
}

// newMetadataStore returns an operational metadata catalog with an optional
// per-owner version cap. Zero means no cap; the caller then records pathological
// version cardinality before assuming bounded memory.
func newMetadataStore(versionCap int) *metadataStore {
	return &metadataStore{
		byOwner:    make(map[OwnerKey]*metadataOwnerHistory),
		bySeries:   make(map[uint64]map[MetadataKind][]seriesMembershipRow),
		chains:     make(map[uint64]labels.Labels),
		versionCap: versionCap,
	}
}

func (s *metadataStore) update(update Update) error {
	history, ok := s.byOwner[update.Owner]
	if !ok {
		history = &metadataOwnerHistory{index: map[string]struct{}{}}
		s.byOwner[update.Owner] = history
	}

	newContent := update.Content.Clone()
	// Latest version matching the content can simply extend observation. No actual
	// change means no version write.
	for i := len(history.versions) - 1; i >= 0; i-- {
		existing := history.versions[i]
		if existing.EffectiveFrom > update.EffectiveFrom {
			continue
		}
		if existing.EffectiveFrom == update.EffectiveFrom {
			if !contentsEqual(existing.Content, newContent) {
				return ErrConflictingMetadata
			}
		}
		if contentsEqual(existing.Content, newContent) {
			history.versions[i].ObservedUntil = max(history.versions[i].ObservedUntil, update.EffectiveFrom)
			history.versions[i].ObservedFrom = min(history.versions[i].ObservedFrom, update.EffectiveFrom)
			return nil
		}
		break
	}

	version := Version{
		Content:       newContent,
		EffectiveFrom: update.EffectiveFrom,
		ObservedFrom:  newContentEffective(update.EffectiveFrom),
		ObservedUntil: update.EffectiveFrom,
	}
	history.versions = append(history.versions, version)
	slices.SortStableFunc(history.versions, func(a, b Version) int { return cmp.Compare(a.EffectiveFrom, b.EffectiveFrom) })
	if s.versionCap > 0 && len(history.versions) > s.versionCap {
		drop := len(history.versions) - s.versionCap
		// Drop oldest only after marking the history as intentionally truncated.
		// This remains observable rather than becoming a silent current-value lookup.
		history.versions = history.versions[drop:]
		history.truncated = true
	}

	for key := range newContent.Attributes {
		history.index[attributeCondition(key, newContent.Attributes[key])] = struct{}{}
	}
	return nil
}

// newContentEffective keeps the observe-from value readable inline. Adding
// metadata does not infer an authoritative timestamp other than its caller.
func newContentEffective(t int64) int64 { return t }

func contentsEqual(a, b Content) bool {
	return a.Canonical() == b.Canonical()
}

// associate links one series into the owner, with an effective time.
// Owner history and membership are deliberately separate. A new member never
// duplicates the Resource owner's descriptive attributes.
func (s *metadataStore) associate(seriesHash uint64, ls labels.Labels, update Update) error {
	// Reject an impoverished or inconsistent identity before contaminating owner
	// history. For Resource-associated series this is exactly what keeps the sample
	// series and Resource owner from silently being two different entities.
	if err := s.validateLabels(update.Owner, ls); err != nil {
		return err
	}
	if err := s.update(update); err != nil {
		return err
	}

	if existing, ok := s.chains[seriesHash]; ok {
		if !labels.Equal(existing, ls) {
			return fmt.Errorf("%w: %q vs %q", ErrSeriesHashCollision, existing, ls)
		}
	} else {
		s.chains[seriesHash] = ls
	}

	kinds := s.bySeries[seriesHash]
	if kinds == nil {
		kinds = map[MetadataKind][]seriesMembershipRow{}
		s.bySeries[seriesHash] = kinds
	}

	rows := kinds[update.Owner.Kind]
	for _, row := range rows {
		if row.owner != update.Owner {
			// Any time-based lookahead can't use both candidate owners as if they
			// were alternatives to reconcile inside an ordinary query.
			return fmt.Errorf("%w: %s and %s at same effective epoch %d",
				ErrConflictingMembership, row.owner.ID, update.Owner.ID, update.EffectiveFrom)
		}
	}
	if len(rows) != 0 {
		// Only the observation and earliest association bounds change. A Resource
		// content change does not create another member/ownership declaration.
		rows[0].effectiveFrom = min(rows[0].effectiveFrom, update.EffectiveFrom)
		rows[0].observedUntil = max(rows[0].observedUntil, update.EffectiveFrom)
		kinds[update.Owner.Kind] = rows
		return nil
	}
	kinds[update.Owner.Kind] = append(rows, seriesMembershipRow{
		owner: update.Owner, ls: ls,
		effectiveFrom: update.EffectiveFrom, observedUntil: update.EffectiveFrom,
	})
	return nil
}

// at resolves the owner's current value at timestamp t using
// latest-effectiveFrom <= t. There is no lookahead into a future version.
func (s *metadataStore) at(owner OwnerKey, t int64) (Content, bool, bool) {
	history, ok := s.byOwner[owner]
	if !ok {
		return Content{}, false, false
	}
	i := sort.Search(len(history.versions), func(i int) bool {
		return history.versions[i].EffectiveFrom > t
	})
	if i == 0 {
		return Content{}, false, history.truncated
	}
	return history.versions[i-1].Content.Clone(), true, history.truncated
}

// ownersAt returns the currently associated owner for one kind at t.
// If no owner association is effective yet, it returns false.
func (s *metadataStore) ownersAt(seriesHash uint64, kind MetadataKind, t int64) (OwnerKey, bool) {
	return s.ownersAtWithLabels(seriesHash, kind, t)
}

func (s *metadataStore) ownersAtWithLabels(seriesHash uint64, kind MetadataKind, t int64) (OwnerKey, bool) {
	rows := s.bySeries[seriesHash][kind]
	if len(rows) != 1 || t < rows[0].effectiveFrom {
		return OwnerKey{}, false
	}
	return rows[0].owner, true
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

func attributeCondition(name, value string) string {
	return name + "\x00" + value
}

// updateAndAssociate checks both side of a metadata update belongs to the same
// real identity before recording any owner history.
func (s *metadataStore) validateLabels(owner OwnerKey, ls labels.Labels) error {
	if existing, ok := s.chains[stableSeriesHash(ls)]; ok && !labels.Equal(existing, ls) {
		return fmt.Errorf("%w: %q vs %q", ErrSeriesHashCollision, existing, ls)
	}
	if owner.Kind == MetadataResource {
		fromLabels := ls.Get(OTELResourceLabel)
		if got, want := fromLabels, owner.Ref(); got != want {
			return fmt.Errorf("%w: Resource identity label %q has value %q but owner key %q", ErrConflictingMembership, OTELResourceLabel, fromLabels, owner.ID)
		}
	}
	return nil
}

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
	"maps"
	"slices"
)

type metadataIndexedRange struct {
	from  int64
	until *int64
}

func normalizeIndexedResourceAttributes(attributes []string) []string {
	if attributes == nil {
		return nil
	}
	normalized := slices.Clone(attributes)
	slices.Sort(normalized)
	return slices.Compact(normalized)
}

// SetIndexedResourceAttrs replaces the descriptive Resource-index configuration
// and rebuilds all in-memory owner candidates. Identifying attributes are always
// included. Nil clears descriptive indexes without initializing a disabled store.
func (s *Storage) SetIndexedResourceAttrs(attributes []string) error {
	if s.metadata == nil {
		return errors.New("columnar: cannot configure native metadata when disabled")
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()
	normalized := normalizeIndexedResourceAttributes(attributes)
	if normalized == nil {
		normalized = []string{}
	}
	s.setIndexedResourceAttrsLocked(normalized)
	return nil
}

func (s *Storage) setIndexedResourceAttrsLocked(attributes []string) {
	s.opts.IndexedResourceAttributes = slices.Clone(attributes)
	s.opts.WriteOptions.IndexedResourceAttributes = slices.Clone(attributes)
	s.metadata.setIndexedResourceAttrs(attributes)
}

// IndexedResourceAttrs returns the configured descriptive set, never a caller's
// mutable backing array.
func (s *Storage) IndexedResourceAttrs() []string {
	if s.metadata == nil {
		return nil
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return slices.Sorted(maps.Keys(s.metadata.indexedResourceAttrs))
}

func (s *metadataStore) setIndexedResourceAttrs(attributes []string) {
	s.indexedResourceAttrs = map[string]struct{}{}
	for _, name := range attributes {
		if name != "" {
			s.indexedResourceAttrs[name] = struct{}{}
		}
	}
	clear(s.attributeIndex)
	clear(s.ownerIndex)
	for owner, history := range s.byOwner {
		history.index = map[string]struct{}{}
		s.indexResourceOwner(owner, history)
	}
}

// indexResourceOwner rebuilds only one owner's contribution. The per-owner index
// set permits old values to be removed at truncation/backfill through the shared
// value→owner index without retaining an unbounded candidate history.
func (s *metadataStore) indexResourceOwner(owner OwnerKey, history *metadataOwnerHistory) {
	if owner.Kind != MetadataResource {
		return
	}
	if history.index == nil {
		history.index = map[string]struct{}{}
	}
	clear(history.index)
	newConditions := map[string]metadataIndexedRange{}
	for name, value := range metadataOwnerIdentity(owner) {
		condition := attributeCondition(name, value)
		newConditions[condition] = metadataIndexedRange{}
		if len(history.versions) > 0 {
			newConditions[condition] = metadataIndexedRange{from: history.versions[0].EffectiveFrom}
		}
	}
	for i, version := range history.versions {
		for name, value := range version.Content.Attributes {
			if _, configured := s.indexedResourceAttrs[name]; !configured {
				continue
			}
			condition := attributeCondition(name, value)
			history.index[condition] = struct{}{}
			candidate, exists := newConditions[condition]
			if !exists {
				candidate.from = version.EffectiveFrom
			}
			// Versions are effective-ordered. The last occurrence therefore
			// supplies the exclusive successor or open-ended lifetime. Disjoint
			// occurrences may over-approximate a range and are revalidated below.
			candidate.until = nextEffectiveFrom(history.versions, i)
			newConditions[condition] = candidate
		}
	}

	previous := s.ownerIndex[owner]
	if s.copyOnWrite {
		// Shared attribute/value maps are cloned before an owner mutation, for
		// both conditions being removed and newly introduced conditions. This
		// happens before old conditions are deleted from the shared maps.
		toCopy := map[string]struct{}{}
		for condition := range previous {
			toCopy[condition] = struct{}{}
		}
		for condition := range newConditions {
			toCopy[condition] = struct{}{}
		}
		for condition := range toCopy {
			s.attributeIndex[condition] = maps.Clone(s.attributeIndex[condition])
		}
	}
	for condition := range previous {
		if _, retained := newConditions[condition]; retained {
			continue
		}
		delete(s.attributeIndex[condition], owner)
		if len(s.attributeIndex[condition]) == 0 {
			delete(s.attributeIndex, condition)
		}
	}
	for condition, candidate := range newConditions {
		if s.attributeIndex[condition] == nil {
			s.attributeIndex[condition] = map[OwnerKey]metadataIndexedRange{}
		}
		s.attributeIndex[condition][owner] = candidate
	}
	s.ownerIndex[owner] = newConditions
}

// indexedOwners returns exact name/value candidates with overlapping current
// version ranges. The caller still verifies all original conditions and time
// validity, so over-approximation cannot create false positives either.
func (s *metadataStore) indexedOwners(conditions []MetadataAttributeCondition, start, end int64) (map[OwnerKey]struct{}, bool) {
	var candidates map[OwnerKey]struct{}
	for _, condition := range conditions {
		if s.attributeIndex == nil {
			return nil, false
		}
		byOwner, indexed := s.attributeIndex[attributeCondition(condition.Name, condition.Value)]
		if !indexed {
			return nil, false
		}
		matching := map[OwnerKey]struct{}{}
		for owner, validity := range byOwner {
			if metadataPeriodOverlaps(validity.from, validity.until, start, end) {
				matching[owner] = struct{}{}
			}
		}
		if candidates == nil {
			candidates = matching
		} else {
			for owner := range candidates {
				if _, ok := matching[owner]; !ok {
					delete(candidates, owner)
				}
			}
		}
		if len(candidates) == 0 {
			break
		}
	}
	if candidates == nil {
		return map[OwnerKey]struct{}{}, true
	}
	if s.reads != nil {
		s.reads.metadataCandidates.Add(uint64(len(candidates)))
	}
	return candidates, true
}

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

import "maps"

// restoreRelations merges one published block or full snapshot into s. Call in
// block order. Replacement histories/lists apply at whole-owner or
// series/kind granularity; append-only rows extend only their own observations.
func (s *metadataStore) restoreRelations(relations metadataRelations) error {
	_, contents := relations.lookupTables()
	owners := relations.ownersByID()

	for _, owner := range relations.owners {
		history, ok := s.byOwner[owner.key]
		if !ok {
			history = &metadataOwnerHistory{index: map[string]struct{}{}}
			s.byOwner[owner.key] = history
		}
		if owner.replace {
			history.versions = nil
			history.truncated = owner.truncated
			clear(history.index)
		} else {
			history.truncated = history.truncated || owner.truncated
		}
	}
	for _, version := range relations.versions {
		owner := owners[version.ownerID].key
		if err := s.update(Update{
			Owner: owner, Content: contents[version.contentID], EffectiveFrom: version.effectiveFrom,
		}); err != nil {
			return err
		}
		history := s.byOwner[owner]
		for i := range history.versions {
			if history.versions[i].EffectiveFrom != version.effectiveFrom {
				continue
			}
			history.versions[i].ObservedFrom = min(history.versions[i].ObservedFrom, version.observedFrom)
			history.versions[i].ObservedUntil = max(history.versions[i].ObservedUntil, version.observedUntil)
			break
		}
	}

	replaced := map[metadataMembershipGroup]struct{}{}
	for _, membership := range relations.memberships {
		owner := owners[membership.ownerID]
		group := metadataMembershipGroup{seriesHash: membership.seriesHash, kind: owner.key.Kind}
		if membership.replace {
			if _, done := replaced[group]; !done {
				delete(s.bySeries[membership.seriesHash], owner.key.Kind)
				replaced[group] = struct{}{}
			}
		}
		if err := s.attachMembership(
			membership.seriesHash, membership.labels, owner.key,
			membership.effectiveFrom, membership.observedUntil,
		); err != nil {
			return err
		}
	}
	return nil
}

// clone returns a transaction-local view using copy-on-write barriers rather
// than cloning every owner/content row. Untouched arrays and immutable content
// may be shared. Mutable owner, membership and index entries are copied before
// they change. Storage must validate the candidate before publication and hold
// Storage.mtx for preparation/publication.
func (s *metadataStore) clone() (*metadataStore, error) {
	clone := newMetadataStore(s.versionCap)
	clone.indexedResourceAttrs = maps.Clone(s.indexedResourceAttrs)
	clone.reads = s.reads
	clone.canonicalVersions = maps.Clone(s.canonicalVersions)
	clone.byOwner = maps.Clone(s.byOwner)
	for owner, history := range s.byOwner {
		// A copy-on-write history may already have been published. Its marker
		// belongs to that transaction's publication lifetime, not to this clone.
		// Give each new clone its own owner struct so freshness checks never
		// mutate the prior published struct.
		local := *history
		local.copyTouched = false
		clone.byOwner[owner] = &local
	}
	clone.bySeries = maps.Clone(s.bySeries)
	clone.chains = maps.Clone(s.chains)
	clone.ownerIndex = maps.Clone(s.ownerIndex)
	maps.Copy(clone.attributeIndex, s.attributeIndex)
	clone.copyOnWrite = true
	clone.copiedMemberships = map[metadataMembershipGroup]struct{}{}
	return clone, nil
}

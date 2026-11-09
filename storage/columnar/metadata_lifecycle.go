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
	"maps"
	"slices"

	"github.com/prometheus/prometheus/model/labels"
)

type metadataMembershipGroup struct {
	seriesHash uint64
	kind       MetadataKind
}

func (r metadataRelations) hasChanges() bool {
	return len(r.owners)+len(r.contents)+len(r.versions)+len(r.memberships) > 0
}

// pendingSince returns changed metadata only. Observation-only extensions are
// deliberately omitted: validity is defined by effective change points, not by
// copying a new row on every observed sample. Replacement sets handle eviction,
// coalescing and backfill without allowing old block state to reappear.
func (r metadataRelations) pendingSince(previous *metadataRelations) metadataRelations {
	var previousRelations metadataRelations
	if previous != nil {
		previousRelations = *previous
	}
	oldOwnersByKey, oldContents := previousRelations.lookupTables()
	_, newContents := r.lookupTables()
	oldOwnerIDs := previousRelations.ownersByID()
	newOwnerIDs := r.ownersByID()
	oldVersions := previousRelations.versionsByOwner()
	newVersions := r.versionsByOwner()

	selectedOwners := map[uint64]metadataOwnerRow{}
	var selectedVersions []metadataVersionRow
	var selectedMemberships []metadataMembershipRow
	for _, newOwner := range r.owners {
		oldOwner, existed := oldOwnersByKey[newOwner.key]
		var replacement bool
		var versions []metadataVersionRow
		if !existed {
			versions = newVersions[newOwner.id]
		} else {
			old := oldVersions[oldOwner.id]
			latest := newVersions[newOwner.id]
			prefix := metadataVersionPrefix(old, latest, oldContents, newContents)
			switch {
			case prefix && len(old) == len(latest) && oldOwner.truncated == newOwner.truncated:
				continue
			case prefix && oldOwner.truncated == newOwner.truncated:
				versions = latest[len(old):]
			default:
				replacement = true
				versions = latest
			}
		}
		if len(versions) == 0 && !replacement {
			continue
		}
		newOwner.replace = replacement
		selectedOwners[newOwner.id] = newOwner
		selectedVersions = append(selectedVersions, versions...)
	}

	oldMemberships := previousRelations.membershipsBySeriesKind(oldOwnerIDs)
	newMemberships := r.membershipsBySeriesKind(newOwnerIDs)
	for group, latest := range newMemberships {
		old := oldMemberships[group]
		prefix := metadataMembershipPrefix(old, latest, oldOwnerIDs, newOwnerIDs)
		var memberships []metadataMembershipRow
		var replacement bool
		switch {
		case prefix && len(old) == len(latest):
			continue
		case prefix:
			memberships = latest[len(old):]
		default:
			replacement = true
			memberships = latest
		}
		for _, membership := range memberships {
			membership.replace = replacement
			selectedMemberships = append(selectedMemberships, membership)
			selectedOwners[membership.ownerID] = newOwnerIDs[membership.ownerID]
		}
	}

	var out metadataRelations
	ownerSourceIDs := slices.Sorted(maps.Keys(selectedOwners))
	slices.SortFunc(ownerSourceIDs, func(a, b uint64) int {
		aKey, bKey := selectedOwners[a].key, selectedOwners[b].key
		switch {
		case aKey.Kind < bKey.Kind:
			return -1
		case aKey.Kind > bKey.Kind:
			return 1
		case aKey.ID < bKey.ID:
			return -1
		case aKey.ID > bKey.ID:
			return 1
		default:
			return 0
		}
	})
	ownerIDs := map[uint64]uint64{}
	for _, sourceID := range ownerSourceIDs {
		owner := selectedOwners[sourceID]
		owner.id = uint64(len(out.owners) + 1)
		ownerIDs[sourceID] = owner.id
		out.owners = append(out.owners, owner)
	}

	// Content IDs follow first use in canonical owner/version order. Unreferenced
	// and evicted content never enters this export.
	contentIDs := map[uint64]uint64{}
	for _, version := range selectedVersions {
		if _, exists := contentIDs[version.contentID]; exists {
			continue
		}
		content := newContents[version.contentID].Clone()
		out.contents = append(out.contents, metadataContentRow{
			id: uint64(len(out.contents) + 1), digest: metadataContentHash(content), content: content,
		})
		contentIDs[version.contentID] = uint64(len(out.contents))
	}
	for _, version := range selectedVersions {
		version.ownerID = ownerIDs[version.ownerID]
		version.contentID = contentIDs[version.contentID]
		out.versions = append(out.versions, version)
	}
	for _, membership := range selectedMemberships {
		membership.ownerID = ownerIDs[membership.ownerID]
		membership.labels = membership.labels.Copy()
		out.memberships = append(out.memberships, membership)
	}
	return out
}

func (r metadataRelations) lookupTables() (map[OwnerKey]metadataOwnerRow, map[uint64]Content) {
	owners := make(map[OwnerKey]metadataOwnerRow, len(r.owners))
	contents := make(map[uint64]Content, len(r.contents))
	for _, owner := range r.owners {
		owners[owner.key] = owner
	}
	for _, content := range r.contents {
		contents[content.id] = content.content
	}
	return owners, contents
}

func (r metadataRelations) ownersByID() map[uint64]metadataOwnerRow {
	out := make(map[uint64]metadataOwnerRow, len(r.owners))
	for _, owner := range r.owners {
		out[owner.id] = owner
	}
	return out
}

func (r metadataRelations) versionsByOwner() map[uint64][]metadataVersionRow {
	out := map[uint64][]metadataVersionRow{}
	for _, version := range r.versions {
		out[version.ownerID] = append(out[version.ownerID], version)
	}
	for _, versions := range out {
		slices.SortFunc(versions, func(a, b metadataVersionRow) int {
			switch {
			case a.effectiveFrom < b.effectiveFrom:
				return -1
			case a.effectiveFrom > b.effectiveFrom:
				return 1
			default:
				return 0
			}
		})
	}
	return out
}

func metadataVersionPrefix(old, latest []metadataVersionRow, oldContents, newContents map[uint64]Content) bool {
	if len(old) > len(latest) {
		return false
	}
	for i := range old {
		if old[i].effectiveFrom != latest[i].effectiveFrom || oldContents[old[i].contentID].Canonical() != newContents[latest[i].contentID].Canonical() {
			return false
		}
	}
	return true
}

func (r metadataRelations) membershipsBySeriesKind(owners map[uint64]metadataOwnerRow) map[metadataMembershipGroup][]metadataMembershipRow {
	out := map[metadataMembershipGroup][]metadataMembershipRow{}
	for _, membership := range r.memberships {
		owner := owners[membership.ownerID]
		group := metadataMembershipGroup{seriesHash: membership.seriesHash, kind: owner.key.Kind}
		out[group] = append(out[group], membership)
	}
	for _, memberships := range out {
		slices.SortFunc(memberships, func(a, b metadataMembershipRow) int {
			switch {
			case a.effectiveFrom < b.effectiveFrom:
				return -1
			case a.effectiveFrom > b.effectiveFrom:
				return 1
			case a.ownerID < b.ownerID:
				return -1
			case a.ownerID > b.ownerID:
				return 1
			default:
				return 0
			}
		})
	}
	return out
}

func metadataMembershipPrefix(old, latest []metadataMembershipRow, oldOwners, newOwners map[uint64]metadataOwnerRow) bool {
	if len(old) > len(latest) {
		return false
	}
	for i := range old {
		if old[i].effectiveFrom != latest[i].effectiveFrom ||
			oldOwners[old[i].ownerID].key != newOwners[latest[i].ownerID].key ||
			!labels.Equal(old[i].labels, latest[i].labels) {
			return false
		}
	}
	return true
}

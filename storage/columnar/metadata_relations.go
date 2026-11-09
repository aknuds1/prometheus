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
	"crypto/sha256"
	"maps"
	"slices"

	"github.com/apache/arrow-go/v18/arrow"

	"github.com/prometheus/prometheus/model/labels"
)

const (
	metadataOwnersFile      = "metadata_owners.parquet"
	metadataContentsFile    = "metadata_contents.parquet"
	metadataVersionsFile    = "metadata_versions.parquet"
	metadataMembershipsFile = "metadata_memberships.parquet"
)

// metadataOwnerRow identifies one owner. Truncated remains attached to the
// owner so a physical export cannot accidentally claim a complete history.
type metadataOwnerRow struct {
	id        uint64
	key       OwnerKey
	truncated bool
}

// metadataContentRow holds full content by block-local ID. Digest is generated
// from canonical content but is never the identity used to merge unequal rows.
type metadataContentRow struct {
	id      uint64
	digest  string
	content Content
}

type metadataVersionRow struct {
	ownerID       uint64
	contentID     uint64
	effectiveFrom int64
	observedFrom  int64
	observedUntil int64
}

type metadataMembershipRow struct {
	seriesHash    uint64
	ownerID       uint64
	effectiveFrom int64
	observedUntil int64
	labels        labels.Labels
}

// metadataRelations is an immutable snapshot for block publication. A snapshot
// shares no mutable content maps or label slices with its source store.
type metadataRelations struct {
	owners      []metadataOwnerRow
	contents    []metadataContentRow
	versions    []metadataVersionRow
	memberships []metadataMembershipRow
}

// snapshot creates deterministic, normalized Arrow-bound relations. Canonical
// string comparison deduplicates content before any hashes are exposed.
func (s *metadataStore) snapshot() metadataRelations {
	var out metadataRelations
	for key, history := range s.byOwner {
		out.owners = append(out.owners, metadataOwnerRow{key: key, truncated: history.truncated})
	}
	slices.SortFunc(out.owners, func(a, b metadataOwnerRow) int {
		if a.key.Kind < b.key.Kind {
			return -1
		}
		if a.key.Kind > b.key.Kind {
			return 1
		}
		switch {
		case a.key.ID < b.key.ID:
			return -1
		case a.key.ID > b.key.ID:
			return 1
		default:
			return 0
		}
	})

	ownerIDs := make(map[OwnerKey]uint64, len(out.owners))
	uniqueContent := map[string]Content{}
	for i, owner := range out.owners {
		owner.id = uint64(i + 1)
		out.owners[i].id = owner.id
		ownerIDs[owner.key] = owner.id
		for _, version := range s.byOwner[owner.key].versions {
			canonical := version.Content.Canonical()
			if _, exists := uniqueContent[canonical]; !exists {
				uniqueContent[canonical] = version.Content.Clone()
			}
		}
	}

	canonicalContents := slices.Sorted(maps.Keys(uniqueContent))
	contentIDs := make(map[string]uint64, len(canonicalContents))
	for i, canonical := range canonicalContents {
		id := uint64(i + 1)
		contentIDs[canonical] = id
		content := uniqueContent[canonical].Clone()
		out.contents = append(out.contents, metadataContentRow{
			id: id, digest: metadataContentHash(content), content: content,
		})
	}

	for _, owner := range out.owners {
		for _, version := range s.byOwner[owner.key].versions {
			out.versions = append(out.versions, metadataVersionRow{
				ownerID:       owner.id,
				contentID:     contentIDs[version.Content.Canonical()],
				effectiveFrom: version.EffectiveFrom,
				observedFrom:  version.ObservedFrom,
				observedUntil: version.ObservedUntil,
			})
		}
	}

	for seriesHash, kinds := range s.bySeries {
		for _, memberships := range kinds {
			for _, membership := range memberships {
				out.memberships = append(out.memberships, metadataMembershipRow{
					seriesHash:    seriesHash,
					ownerID:       ownerIDs[membership.owner],
					effectiveFrom: membership.effectiveFrom,
					observedUntil: membership.observedUntil,
					labels:        membership.ls.Copy(),
				})
			}
		}
	}
	slices.SortFunc(out.memberships, func(a, b metadataMembershipRow) int {
		switch {
		case a.seriesHash < b.seriesHash:
			return -1
		case a.seriesHash > b.seriesHash:
			return 1
		case a.ownerID < b.ownerID:
			return -1
		case a.ownerID > b.ownerID:
			return 1
		case a.effectiveFrom < b.effectiveFrom:
			return -1
		case a.effectiveFrom > b.effectiveFrom:
			return 1
		default:
			return 0
		}
	})
	return out
}

func metadataContentHash(content Content) string {
	digest := sha256.Sum256([]byte(content.Canonical()))
	return string(digest[:])
}

// summary creates the catalog before the block is published. Nil means the
// block carries no metadata relations at all.
func (r *metadataRelations) summary() *MetadataSummary {
	if r == nil {
		return nil
	}

	summary := &MetadataSummary{
		NumOwners:      len(r.owners),
		NumContents:    len(r.contents),
		NumVersions:    len(r.versions),
		NumMemberships: len(r.memberships),
	}
	first := true
	addTime := func(t int64) {
		if first || t < summary.MinEffectiveFrom {
			summary.MinEffectiveFrom = t
		}
		if first || t > summary.MaxEffectiveFrom {
			summary.MaxEffectiveFrom = t
		}
		first = false
	}
	for _, row := range r.versions {
		addTime(row.effectiveFrom)
	}
	for _, row := range r.memberships {
		addTime(row.effectiveFrom)
	}

	kinds := map[MetadataKind]struct{}{}
	contents := make(map[uint64]Content, len(r.contents))
	for _, content := range r.contents {
		contents[content.id] = content.content
	}
	resourceOwners := map[uint64]struct{}{}
	for _, owner := range r.owners {
		kinds[owner.key.Kind] = struct{}{}
		if owner.truncated {
			summary.HistoryTruncated = true
		}
		if owner.key.Kind == MetadataResource {
			resourceOwners[owner.id] = struct{}{}
		}
	}
	for _, kind := range slices.Sorted(maps.Keys(kinds)) {
		summary.Kinds = append(summary.Kinds, kind.String())
	}

	attributeNames := map[string]struct{}{}
	for _, version := range r.versions {
		if _, ok := resourceOwners[version.ownerID]; !ok {
			continue
		}
		for name := range contents[version.contentID].Attributes {
			attributeNames[name] = struct{}{}
		}
	}
	summary.AttributeNames = slices.Sorted(maps.Keys(attributeNames))
	return summary
}

func metadataOwnersSchema() *arrow.Schema {
	return arrow.NewSchema([]arrow.Field{
		{Name: "owner_id", Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
		{Name: "kind", Type: arrow.PrimitiveTypes.Uint8, Nullable: false},
		{Name: "owner_key", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "truncated", Type: arrow.FixedWidthTypes.Boolean, Nullable: false},
	}, nil)
}

func metadataContentsSchema() *arrow.Schema {
	return arrow.NewSchema([]arrow.Field{
		{Name: "content_id", Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
		{Name: "content_sha256", Type: arrow.BinaryTypes.Binary, Nullable: false},
		{Name: "attributes", Type: metadataStringPairsType(), Nullable: false},
	}, nil)
}

func metadataVersionsSchema() *arrow.Schema {
	return arrow.NewSchema([]arrow.Field{
		{Name: "owner_id", Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
		{Name: "content_id", Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
		{Name: "effective_from", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "observed_from", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "observed_until", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}, nil)
}

func metadataMembershipsSchema() *arrow.Schema {
	return arrow.NewSchema([]arrow.Field{
		{Name: "series_hash", Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
		{Name: "owner_id", Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
		{Name: "effective_from", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "observed_until", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "labels", Type: metadataStringPairsType(), Nullable: false},
	}, nil)
}

func metadataStringPairsType() arrow.DataType {
	return arrow.ListOf(arrow.StructOf(
		arrow.Field{Name: labelNameColumn, Type: arrow.BinaryTypes.String, Nullable: false},
		arrow.Field{Name: labelValueCol, Type: arrow.BinaryTypes.String, Nullable: false},
	))
}

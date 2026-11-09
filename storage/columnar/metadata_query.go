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
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/prometheus/prometheus/model/labels"
)

// MetadataValueView contains descriptive content and explicit successor
// validity. A nil ValidUntil means open-ended validity.
type MetadataValueView struct {
	Descriptive   map[string]string `json:"descriptive"`
	EffectiveFrom int64             `json:"effective_from"`
	ValidUntil    *int64            `json:"valid_until,omitempty"`
	ObservedFrom  int64             `json:"observed_from"`
	ObservedUntil int64             `json:"observed_until"`
}

// MetadataOwnerView is one independently versioned owner, never one result per
// member series solely because its shared content changed.
type MetadataOwnerView struct {
	ID               string              `json:"id"`
	Kind             string              `json:"kind"`
	Identifying      map[string]string   `json:"identifying"`
	HistoryTruncated bool                `json:"history_truncated"`
	Versions         []MetadataValueView `json:"versions"`
}

// MetadataOwnerPage contains owner-key pagination.
type MetadataOwnerPage struct {
	Owners    []MetadataOwnerView `json:"owners"`
	NextToken string              `json:"next_token,omitempty"`
}

// MetadataQuery controls owner selection. Start and End are inclusive query
// times. Projection selects descriptive attributes only; After is the decoded
// opaque owner cursor returned by a previous page.
type MetadataQuery struct {
	Kind       MetadataKind
	Start, End int64
	Projection []string
	After      string
	Matchers   [][]*labels.Matcher
	Limit      int
	Latest     bool
}

// QueryMetadata returns owner histories within the requested validity period.
// Metadata-only owners are included unless a series matcher narrows the owners.
func (s *Storage) QueryMetadata(ctx context.Context, query MetadataQuery) (MetadataOwnerPage, error) {
	if s.metadata == nil {
		return MetadataOwnerPage{}, ErrMetadataDisabled
	}
	if query.Start > query.End {
		return MetadataOwnerPage{}, fmt.Errorf("metadata query start %d is after end %d", query.Start, query.End)
	}
	if err := ctx.Err(); err != nil {
		return MetadataOwnerPage{}, err
	}
	matchingOwners, err := s.matchMetadataOwners(ctx, query.Kind, query.Matchers, query.Start, query.End)
	if err != nil {
		return MetadataOwnerPage{}, err
	}

	s.mtx.Lock()
	defer s.mtx.Unlock()
	owners := slices.Collect(maps.Keys(matchingOwners))
	sortOwnerKeys(owners)
	explicit := projectionSet(query.Projection)
	var out MetadataOwnerPage
	for _, owner := range owners {
		if err := ctx.Err(); err != nil {
			return MetadataOwnerPage{}, err
		}
		if owner.Kind != query.Kind || owner.String() <= query.After {
			continue
		}
		history := s.metadata.byOwner[owner]
		view := MetadataOwnerView{
			ID: owner.Ref(), Kind: owner.Kind.String(), Identifying: metadataOwnerIdentity(owner),
			HistoryTruncated: history.truncated,
		}
		for i := range history.versions {
			if !metadataPeriodOverlaps(history.versions[i].EffectiveFrom, nextEffectiveFrom(history.versions, i), query.Start, query.End) {
				continue
			}
			view.Versions = append(view.Versions, metadataValueView(history.versions, i, explicit))
		}
		if query.Latest && len(view.Versions) > 1 {
			view.Versions = view.Versions[len(view.Versions)-1:]
		}
		if len(view.Versions) == 0 {
			continue
		}
		out.Owners = append(out.Owners, view)
		if query.Limit > 0 && len(out.Owners) > query.Limit {
			out.NextToken = ownerCursor(owners[query.Limit-1])
			out.Owners = out.Owners[:query.Limit]
			return out, nil
		}
	}
	if out.Owners == nil {
		out.Owners = []MetadataOwnerView{}
	}
	return out, nil
}

func ownerCursor(owner OwnerKey) string { return owner.String() }

func sortOwnerKeys(owners []OwnerKey) {
	slices.SortFunc(owners, func(a, b OwnerKey) int {
		if a.Kind < b.Kind {
			return -1
		}
		if a.Kind > b.Kind {
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
}

func metadataOwnerIdentity(owner OwnerKey) map[string]string {
	switch owner.Kind {
	case MetadataResource:
		identity := map[string]string{}
		var pairs []string
		if json.Unmarshal([]byte(owner.ID), &pairs) == nil {
			for i := 0; i+1 < len(pairs); i += 2 {
				identity[pairs[i]] = pairs[i+1]
			}
		}
		return identity
	case MetadataScope:
		identity := map[string]string{}
		_ = json.Unmarshal([]byte(owner.ID), &identity)
		return identity
	case MetadataResourceSchema:
		return map[string]string{"resource": owner.ID}
	case MetadataMetric:
		return map[string]string{"series": owner.ID}
	case MetadataScrape:
		return map[string]string{"scrape": owner.ID}
	case MetadataSource:
		return map[string]string{"source": owner.ID}
	default:
		return map[string]string{"id": owner.ID}
	}
}

func projectionSet(projection []string) map[string]struct{} {
	if len(projection) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(projection))
	for _, name := range projection {
		if name != "" && name != "*" {
			set[name] = struct{}{}
		}
	}
	return set
}

func metadataValueView(versions []Version, i int, projection map[string]struct{}) MetadataValueView {
	version := versions[i]
	attributes := version.Content.Clone().Attributes
	if projection != nil {
		for name := range attributes {
			if _, selected := projection[name]; !selected {
				delete(attributes, name)
			}
		}
	}
	return MetadataValueView{
		Descriptive: attributes, EffectiveFrom: version.EffectiveFrom,
		ValidUntil:   nextEffectiveFrom(versions, i),
		ObservedFrom: version.ObservedFrom, ObservedUntil: version.ObservedUntil,
	}
}

func nextEffectiveFrom(versions []Version, i int) *int64 {
	if i+1 >= len(versions) {
		return nil
	}
	next := versions[i+1].EffectiveFrom
	return &next
}

func metadataPeriodOverlaps(effectiveFrom int64, successor *int64, start, end int64) bool {
	return effectiveFrom <= end && (successor == nil || *successor > start)
}

// matchMetadataOwners applies series matchers to existing samples. Resource
// labels retain the original Resource reference; other owner kinds resolve their
// stored association rather than reinterpreting help/schema/source fields.
func (s *Storage) matchMetadataOwners(ctx context.Context, kind MetadataKind, matcherSets [][]*labels.Matcher, start, end int64) (map[OwnerKey]struct{}, error) {
	return s.matchMetadataOwnersAt(ctx, kind, matcherSets, start, end, nil)
}

// matchMetadataOwnersAt optionally resolves membership at an exact timestamp.
// Otherwise any membership overlapping [start, end] contributes its owner.
func (s *Storage) matchMetadataOwnersAt(ctx context.Context, kind MetadataKind, matcherSets [][]*labels.Matcher, start, end int64, membershipAt *int64) (map[OwnerKey]struct{}, error) {
	result := map[OwnerKey]struct{}{}
	if len(matcherSets) == 0 {
		for owner := range s.metadata.byOwner {
			if owner.Kind == kind {
				result[owner] = struct{}{}
			}
		}
		return result, nil
	}

	series, err := s.SelectSeries(ctx, start, end, matcherSets, 0)
	if err != nil {
		return nil, err
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()
	for _, item := range series {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var owner OwnerKey
		switch kind {
		case MetadataResource:
			ref := item.Labels.Get(OTELResourceLabel)
			for candidate := range s.metadata.byOwner {
				if candidate.Kind == MetadataResource && candidate.Ref() == ref {
					if owner != (OwnerKey{}) && owner != candidate {
						return nil, fmt.Errorf("%w: Resource reference %q is ambiguous", ErrSeriesHashCollision, ref)
					}
					owner = candidate
				}
			}
			if owner.Kind == kind {
				result[owner] = struct{}{}
			}
		case MetadataMetric:
			owner = MetricOwner(item.Labels)
			result[owner] = struct{}{}
		default:
			rows := s.metadata.bySeries[labels.StableHash(item.Labels)][kind]
			for i, row := range rows {
				matches := metadataPeriodOverlaps(row.effectiveFrom, nextMembershipFrom(rows, i), start, end)
				if membershipAt != nil {
					next := nextMembershipFrom(rows, i)
					matches = row.effectiveFrom <= *membershipAt && (next == nil || *next > *membershipAt)
				}
				if matches {
					result[row.owner] = struct{}{}
				}
			}
		}
	}
	return result, nil
}

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
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"

	"github.com/prometheus/prometheus/model/labels"
)

type MetadataAttributeCondition struct {
	Name  string
	Value string
}

type MetadataSeriesQuery struct {
	Kind       MetadataKind
	Start, End int64
	Attributes []MetadataAttributeCondition
	Matchers   [][]*labels.Matcher
	Projection []string
	After      string
	Limit      int
}

type MetadataSeriesResult struct {
	Labels               labels.Labels       `json:"labels"`
	Owner                string              `json:"owner"`
	Kind                 string              `json:"kind"`
	OwnerIdentifying     map[string]string   `json:"owner_identifying"`
	MembershipStart      int64               `json:"membership_start"`
	MembershipUntil      *int64              `json:"membership_until,omitempty"`
	MembershipObservedTo int64               `json:"membership_observed_until"`
	Versions             []MetadataValueView `json:"versions"`
}

type MetadataSeriesPage struct {
	Series    []MetadataSeriesResult `json:"series"`
	NextToken string                 `json:"next_token,omitempty"`
}

func (s *Storage) QueryMetadataSeries(ctx context.Context, query MetadataSeriesQuery) (MetadataSeriesPage, error) {
	if s.metadata == nil {
		return MetadataSeriesPage{}, ErrMetadataDisabled
	}
	if query.Start > query.End {
		return MetadataSeriesPage{}, fmt.Errorf("metadata query start %d is after end %d", query.Start, query.End)
	}
	owners, err := s.metadata.ownersMatchingAttributes(query.Kind, query.Attributes, query.Start, query.End)
	if err != nil {
		return MetadataSeriesPage{}, err
	}
	projection := projectionSet(query.Projection)

	s.mtx.Lock()
	defer s.mtx.Unlock()
	var out MetadataSeriesPage
	for _, kinds := range s.metadata.bySeries {
		if err := ctx.Err(); err != nil {
			return MetadataSeriesPage{}, err
		}
		rows := kinds[query.Kind]
		for i, row := range rows {
			if _, matchesOwner := owners[row.owner]; !matchesOwner {
				continue
			}
			if len(query.Matchers) > 0 && !matchesAny(row.ls, query.Matchers) {
				continue
			}
			nextMembership := nextMembershipFrom(rows, i)
			versions := s.metadata.byOwner[row.owner].versions
			var matching []MetadataValueView
			for j, version := range versions {
				nextVersion := nextEffectiveFrom(versions, j)
				if !metadataMembershipAndVersionOverlap(row, nextMembership, version, nextVersion, query.Start, query.End) {
					continue
				}
				matching = append(matching, metadataValueView(versions, j, projection))
			}
			if len(matching) == 0 {
				continue
			}
			result := MetadataSeriesResult{
				Labels: row.ls.Copy(), Owner: row.owner.Ref(), Kind: row.owner.Kind.String(),
				OwnerIdentifying: metadataOwnerIdentity(row.owner),
				MembershipStart:  row.effectiveFrom, MembershipUntil: nextMembership,
				MembershipObservedTo: row.observedUntil, Versions: matching,
			}
			if query.After != "" && metadataSeriesCursor(result) <= query.After {
				continue
			}
			out.Series = append(out.Series, result)
		}
	}
	slices.SortFunc(out.Series, func(a, b MetadataSeriesResult) int {
		return labels.Compare(a.Labels, b.Labels)
	})
	if query.Limit > 0 && len(out.Series) > query.Limit {
		out.NextToken = metadataSeriesCursor(out.Series[query.Limit-1])
		out.Series = out.Series[:query.Limit]
	}
	if out.Series == nil {
		out.Series = []MetadataSeriesResult{}
	}
	return out, nil
}

func metadataSeriesCursor(result MetadataSeriesResult) string {
	return result.Labels.String() + "\x00" + result.Owner + "\x00" + strconv.FormatInt(result.MembershipStart, 10)
}

func nextMembershipFrom(rows []seriesMembershipRow, i int) *int64 {
	if i+1 >= len(rows) {
		return nil
	}
	next := rows[i+1].effectiveFrom
	return &next
}

func metadataMembershipAndVersionOverlap(row seriesMembershipRow, nextMembership *int64, version Version, nextVersion *int64, start, end int64) bool {
	return metadataIntervalsOverlap(row.effectiveFrom, nextMembership, version.EffectiveFrom, nextVersion, start, end)
}

type metadataUpperBound struct {
	value     int64
	exclusive bool
}

func metadataIntervalsOverlap(startA int64, endAExclusive *int64, startB int64, endBExclusive *int64, queryStart, queryEnd int64) bool {
	lower := max(max(startA, startB), queryStart)
	upper := metadataUpperBound{value: queryEnd}
	for _, bound := range []*int64{endAExclusive, endBExclusive} {
		if bound == nil {
			continue
		}
		if *bound < upper.value || (*bound == upper.value && !upper.exclusive) {
			upper = metadataUpperBound{value: *bound, exclusive: true}
		}
	}
	if upper.exclusive {
		return lower < upper.value
	}
	return lower <= upper.value
}

func (s *metadataStore) ownersMatchingAttributes(kind MetadataKind, conditions []MetadataAttributeCondition, start, end int64) (map[OwnerKey]struct{}, error) {
	out := map[OwnerKey]struct{}{}
	for owner, history := range s.byOwner {
		if owner.Kind != kind {
			continue
		}
		identity := metadataOwnerIdentity(owner)
		for i := range history.versions {
			version := history.versions[i]
			next := nextEffectiveFrom(history.versions, i)
			if !metadataPeriodOverlaps(version.EffectiveFrom, next, start, end) {
				continue
			}
			if metadataContentMatches(identity, version.Content.Attributes, conditions) {
				out[owner] = struct{}{}
				break
			}
		}
	}
	return out, nil
}

func metadataContentMatches(identity, attributes map[string]string, conditions []MetadataAttributeCondition) bool {
	for _, condition := range conditions {
		value := attributes[condition.Name]
		if _, exists := attributes[condition.Name]; !exists {
			value = identity[condition.Name]
		}
		if value != condition.Value {
			return false
		}
	}
	return true
}

type MetadataDiffQuery struct {
	Kind       MetadataKind
	Time       int64
	Projection []string
	Matchers   [][]*labels.Matcher
	After      string
	Limit      int
}

type MetadataChange struct {
	Owner   MetadataOwnerView  `json:"owner"`
	Before  *MetadataValueView `json:"before,omitempty"`
	After   MetadataValueView  `json:"after"`
	Changed map[string]string  `json:"changed"`
	owner   OwnerKey
}

type MetadataDiffPage struct {
	Changes   []MetadataChange `json:"changes"`
	NextToken string           `json:"next_token,omitempty"`
}

func (s *Storage) DiffMetadata(ctx context.Context, query MetadataDiffQuery) (MetadataDiffPage, error) {
	if s.metadata == nil {
		return MetadataDiffPage{}, ErrMetadataDisabled
	}
	owners, err := s.matchMetadataOwnersAt(ctx, query.Kind, query.Matchers, math.MinInt64, math.MaxInt64, &query.Time)
	if err != nil {
		return MetadataDiffPage{}, err
	}
	projection := projectionSet(query.Projection)

	if err := ctx.Err(); err != nil {
		return MetadataDiffPage{}, err
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()
	ordered := slices.Collect(maps.Keys(owners))
	sortOwnerKeys(ordered)
	var out MetadataDiffPage
	for _, owner := range ordered {
		if owner.String() <= query.After {
			continue
		}
		history := s.metadata.byOwner[owner]
		afterIndex := -1
		for i, version := range history.versions {
			if version.EffectiveFrom <= query.Time {
				afterIndex = i
			}
		}
		if afterIndex < 0 {
			continue
		}
		after := metadataValueView(history.versions, afterIndex, projection)
		change := MetadataChange{
			owner: owner,
			Owner: MetadataOwnerView{
				ID: owner.Ref(), Kind: owner.Kind.String(), Identifying: metadataOwnerIdentity(owner),
				HistoryTruncated: history.truncated,
			},
			After: after,
		}
		if afterIndex > 0 {
			before := metadataValueView(history.versions, afterIndex-1, projection)
			change.Before = &before
			change.Changed = diffMetadataContent(before.Descriptive, after.Descriptive)
		} else {
			change.Changed = maps.Clone(after.Descriptive)
		}
		if len(change.Changed) == 0 && change.Before != nil {
			continue
		}
		out.Changes = append(out.Changes, change)
		if query.Limit > 0 && len(out.Changes) > query.Limit {
			out.NextToken = out.Changes[query.Limit-1].owner.String()
			out.Changes = out.Changes[:query.Limit]
			return out, nil
		}
	}
	if out.Changes == nil {
		out.Changes = []MetadataChange{}
	}
	return out, nil
}

func diffMetadataContent(before, after map[string]string) map[string]string {
	out := map[string]string{}
	for name, afterValue := range after {
		beforeValue, existed := before[name]
		switch {
		case !existed:
			out[name] = afterValue
		case beforeValue != afterValue:
			out[name] = beforeValue + "->" + afterValue
		}
	}
	for name, beforeValue := range before {
		if _, exists := after[name]; !exists {
			out[name] = beforeValue + "->"
		}
	}
	return out
}

type MetadataDiscoveryQuery struct {
	Kind       MetadataKind
	Start, End int64
	Projection []string
	After      string
	Limit      int
}

type MetadataNameValues struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

type MetadataDiscoveryPage struct {
	Attributes []MetadataNameValues `json:"attributes"`
	NextToken  string               `json:"next_token,omitempty"`
}

func (s *Storage) DiscoverMetadata(ctx context.Context, query MetadataDiscoveryQuery) (MetadataDiscoveryPage, error) {
	if s.metadata == nil {
		return MetadataDiscoveryPage{}, ErrMetadataDisabled
	}
	if query.Start > query.End {
		return MetadataDiscoveryPage{}, fmt.Errorf("metadata query start %d is after end %d", query.Start, query.End)
	}
	if err := ctx.Err(); err != nil {
		return MetadataDiscoveryPage{}, err
	}
	s.mtx.Lock()
	defer s.mtx.Unlock()
	projection := projectionSet(query.Projection)
	values := map[string]map[string]struct{}{}
	for owner, history := range s.metadata.byOwner {
		if owner.Kind != query.Kind {
			continue
		}
		for i, version := range history.versions {
			if !metadataPeriodOverlaps(version.EffectiveFrom, nextEffectiveFrom(history.versions, i), query.Start, query.End) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return MetadataDiscoveryPage{}, err
			}
			for name, value := range version.Content.Attributes {
				if _, selected := projection[name]; projection != nil && !selected {
					continue
				}
				if values[name] == nil {
					values[name] = map[string]struct{}{}
				}
				values[name][value] = struct{}{}
			}
		}
	}
	var out MetadataDiscoveryPage
	for _, name := range slices.Sorted(maps.Keys(values)) {
		if name <= query.After {
			continue
		}
		out.Attributes = append(out.Attributes, MetadataNameValues{
			Name: name, Values: slices.Sorted(maps.Keys(values[name])),
		})
		if query.Limit > 0 && len(out.Attributes) > query.Limit {
			out.NextToken = out.Attributes[query.Limit-1].Name
			out.Attributes = out.Attributes[:query.Limit]
			if err := ctx.Err(); err != nil {
				return MetadataDiscoveryPage{}, err
			}
			return out, nil
		}
	}
	if out.Attributes == nil {
		out.Attributes = []MetadataNameValues{}
	}
	return out, nil
}

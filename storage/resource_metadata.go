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

package storage

import (
	"maps"
	"slices"

	"github.com/prometheus/prometheus/tsdb/seriesmetadata"
)

// resourceQuerierFromGeneric preserves optional Resource reads through secondary
// and chunk/sample generic adapters used by the merge tree.
func resourceQuerierFromGeneric(q genericQuerier) ResourceQuerier {
	switch q := q.(type) {
	case *secondaryQuerier:
		return resourceQuerierFromGeneric(q.genericQuerier)
	case ResourceQuerier:
		return q
	default:
		return nil
	}
}

func resourceMembershipQuerierFromGeneric(q genericQuerier) ResourceMembershipQuerier {
	switch q := q.(type) {
	case *secondaryQuerier:
		return resourceMembershipQuerierFromGeneric(q.genericQuerier)
	case ResourceMembershipQuerier:
		return q
	default:
		return nil
	}
}

// GetResourceAt forwards optional Resource reads without creating one for a
// querier that does not implement the interface.
func (q *genericQuerierAdapter) GetResourceAt(labelsHash uint64, timestamp int64) (*seriesmetadata.ResourceVersion, bool) {
	if q.q != nil {
		if querier, ok := q.q.(ResourceQuerier); ok {
			return querier.GetResourceAt(labelsHash, timestamp)
		}
	}
	if querier, ok := q.cq.(ResourceQuerier); ok {
		return querier.GetResourceAt(labelsHash, timestamp)
	}
	return nil, false
}

func (q *genericQuerierAdapter) IterUniqueAttributeNames(fn func(string)) error {
	if q.q != nil {
		if querier, ok := q.q.(ResourceQuerier); ok {
			return querier.IterUniqueAttributeNames(fn)
		}
	}
	if querier, ok := q.cq.(ResourceQuerier); ok {
		return querier.IterUniqueAttributeNames(fn)
	}
	return nil
}

func (q *genericQuerierAdapter) HasResourceMembershipAt(labelsHash uint64, timestamp int64) bool {
	if q.q != nil {
		if querier, ok := q.q.(ResourceMembershipQuerier); ok {
			return querier.HasResourceMembershipAt(labelsHash, timestamp)
		}
	}
	if querier, ok := q.cq.(ResourceMembershipQuerier); ok {
		return querier.HasResourceMembershipAt(labelsHash, timestamp)
	}
	return false
}

// GetResourceAt merges versions only when their full owner identity agrees.
// The newest effective version wins. Equal-time unequal content yields no value.
func (q *mergeGenericQuerier) GetResourceAt(labelsHash uint64, timestamp int64) (*seriesmetadata.ResourceVersion, bool) {
	var selected *seriesmetadata.ResourceVersion
	for _, generic := range q.queriers {
		querier := resourceQuerierFromGeneric(generic)
		if querier == nil {
			continue
		}
		candidate, found := querier.GetResourceAt(labelsHash, timestamp)
		if !found || candidate == nil {
			continue
		}
		if selected == nil {
			selected = candidate
			continue
		}
		if candidate.OwnerID != selected.OwnerID {
			return nil, false
		}
		if candidate.EffectiveFrom > selected.EffectiveFrom {
			selected = candidate
			continue
		}
		if candidate.EffectiveFrom < selected.EffectiveFrom {
			continue
		}
		if !maps.Equal(candidate.Identifying, selected.Identifying) || !maps.Equal(candidate.Descriptive, selected.Descriptive) {
			return nil, false
		}
	}
	return selected, selected != nil
}

// HasResourceMembershipAt reports native membership present on any shard. Native
// membership without a usable version deliberately remains visible to enrich()
// instead of causing an unrelated target_info join.
func (q *mergeGenericQuerier) HasResourceMembershipAt(labelsHash uint64, timestamp int64) bool {
	for _, generic := range q.queriers {
		if querier := resourceMembershipQuerierFromGeneric(generic); querier != nil && querier.HasResourceMembershipAt(labelsHash, timestamp) {
			return true
		}
	}
	return false
}

// IterUniqueAttributeNames merges/deduplicates attribute-name discovery in sorted
// order. Iteration occurs only after all sources succeeded, avoiding partial output.
func (q *mergeGenericQuerier) IterUniqueAttributeNames(fn func(string)) error {
	unique := map[string]struct{}{}
	for _, generic := range q.queriers {
		querier := resourceQuerierFromGeneric(generic)
		if querier == nil {
			continue
		}
		if err := querier.IterUniqueAttributeNames(func(name string) {
			unique[name] = struct{}{}
		}); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(unique)) {
		fn(name)
	}
	return nil
}

// These concrete adapters retain optional Resource interfaces after the generic
// merge path becomes an ordinary Querier again.
func (q *querierAdapter) GetResourceAt(labelsHash uint64, timestamp int64) (*seriesmetadata.ResourceVersion, bool) {
	if querier := resourceQuerierFromGeneric(q.genericQuerier); querier != nil {
		return querier.GetResourceAt(labelsHash, timestamp)
	}
	return nil, false
}

func (q *querierAdapter) IterUniqueAttributeNames(fn func(string)) error {
	if querier := resourceQuerierFromGeneric(q.genericQuerier); querier != nil {
		return querier.IterUniqueAttributeNames(fn)
	}
	return nil
}

func (q *querierAdapter) HasResourceMembershipAt(labelsHash uint64, timestamp int64) bool {
	if querier := resourceMembershipQuerierFromGeneric(q.genericQuerier); querier != nil {
		return querier.HasResourceMembershipAt(labelsHash, timestamp)
	}
	return false
}

func (q *chunkQuerierAdapter) GetResourceAt(labelsHash uint64, timestamp int64) (*seriesmetadata.ResourceVersion, bool) {
	if querier := resourceQuerierFromGeneric(q.genericQuerier); querier != nil {
		return querier.GetResourceAt(labelsHash, timestamp)
	}
	return nil, false
}

func (q *chunkQuerierAdapter) IterUniqueAttributeNames(fn func(string)) error {
	if querier := resourceQuerierFromGeneric(q.genericQuerier); querier != nil {
		return querier.IterUniqueAttributeNames(fn)
	}
	return nil
}

func (q *chunkQuerierAdapter) HasResourceMembershipAt(labelsHash uint64, timestamp int64) bool {
	if querier := resourceMembershipQuerierFromGeneric(q.genericQuerier); querier != nil {
		return querier.HasResourceMembershipAt(labelsHash, timestamp)
	}
	return false
}

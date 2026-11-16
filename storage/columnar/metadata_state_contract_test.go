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

// Decision 3 requires a positive value filter to match only an applicable
// present value. Absent, cleared and unknown states must not silently equal an
// empty-string value; selecting one of those states needs an explicit state
// predicate whose syntax is deferred to decision 6. The test below records the
// incumbent divergence rather than repairing it: a missing content key falls
// back to the owner identity and then to the empty string, so an empty-string
// condition selects an owner that does not contain the key at all. It is
// evidence for the manifest's defect-absent-empty-filter obligation, not a
// passing contract golden.

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
)

// TestMetadataEmptyAttributeMatcherSelectsAbsentValue reproduces the current
// empty-string matcher divergence: the owner whose content and identity do not
// contain host.name is returned for a host.name="" condition, and the owner
// whose content states a non-empty host.name is not.
func TestMetadataEmptyAttributeMatcherSelectsAbsentValue(t *testing.T) {
	store, err := NewStorage(t.TempDir(), StorageOptions{FlushSamples: 1000, MetadataVersionCap: -1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	presentOwner := ResourceOwner(map[string]string{
		"service.name": "state", "service.namespace": "shop", "service.instance.id": "pod-a",
	})
	absentOwner := ResourceOwner(map[string]string{
		"service.name": "state", "service.namespace": "shop", "service.instance.id": "pod-b",
	})
	presentLabels := labels.FromStrings(
		"__name__", "state_metric", "instance", "a", OTELResourceLabel, presentOwner.Ref(),
	)
	absentLabels := labels.FromStrings(
		"__name__", "state_metric", "instance", "b", OTELResourceLabel, absentOwner.Ref(),
	)

	require.NoError(t, store.AppendBatch(context.Background(), Batch{
		Series: []BatchSeries{
			{Labels: presentLabels, Timestamps: []int64{1000}, Values: []float64{1}, Kinds: []SampleKind{SampleFloat}},
			{Labels: absentLabels, Timestamps: []int64{1000}, Values: []float64{2}, Kinds: []SampleKind{SampleFloat}},
		},
		Updates: []AssociationUpdate{
			{
				Labels: presentLabels,
				Update: Update{
					Owner: presentOwner, Content: Content{Attributes: map[string]string{"host.name": "a-host"}},
					EffectiveFrom: 1000,
				},
			},
			{
				Labels: absentLabels,
				Update: Update{
					Owner: absentOwner, Content: Content{Attributes: map[string]string{"region": "east"}},
					EffectiveFrom: 1000,
				},
			},
		},
	}))

	page, err := store.QueryMetadataSeries(context.Background(), MetadataSeriesQuery{
		Kind: MetadataResource, Start: math.MinInt64, End: math.MaxInt64,
		Attributes: []MetadataAttributeCondition{{Name: "host.name", Value: ""}},
	})
	require.NoError(t, err)
	require.Len(t, page.Series, 1,
		"the incumbent selects the owner with no host.name for host.name=\"\" instead of selecting no owner")
	require.Equal(t, "pod-b", page.Series[0].OwnerIdentifying["service.instance.id"],
		"the selected series must be the owner whose content and identity omit host.name")
	require.Equal(t, map[string]string{"region": "east"}, page.Series[0].Versions[0].Descriptive,
		"the defect is that an absent key matches the empty string, not that the content changed")
}

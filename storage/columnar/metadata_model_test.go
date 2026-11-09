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

package columnar_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
)

func TestMetadataSharingWithManySeries(t *testing.T) {
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{})
	require.NoError(t, err)
	defer s.Close()

	owner := columnar.ResourceOwner(map[string]string{"service.name": "payments", "service.instance.id": "pay01"})
	content := columnar.Content{Attributes: map[string]string{"host.name": "h1", "version": "1"}}

	for i := range 100 {
		ls := resourceLabels(i)
		_, err := s.MetadataAssociate(ls, columnar.Update{
			Owner: owner, Content: content, EffectiveFrom: 100,
		})
		require.NoError(t, err)
	}

	history, ok := s.MetadataHistory(owner)
	require.True(t, ok)
	require.False(t, history.Truncated)
	require.Len(t, history.Versions, 1, "100 member series must not produce 100 versions")

	// Sharing remains intentional: one actual Resource change makes one new version
	// shared by all of them, not 100 metadata rows on sample/data-point identity.
	_, err = s.MetadataAssociate(resourceLabels(0), columnar.Update{
		Owner:         owner,
		Content:       columnar.Content{Attributes: map[string]string{"host.name": "h2", "version": "1"}},
		EffectiveFrom: 120,
	})
	require.NoError(t, err)

	history, ok = s.MetadataHistory(owner)
	require.True(t, ok)
	require.Len(t, history.Versions, 2)

	for _, member := range []int{0, 50, 99} {
		ls := resourceLabels(member)
		content, ok, truncated := s.MetadataAtLabels(ls, columnar.MetadataResource, 119)
		require.True(t, ok)
		require.False(t, truncated)
		require.Equal(t, "h1", content.Attributes["host.name"])
	}
}

func TestMetadataTemporalBoundaries(t *testing.T) {
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{})
	require.NoError(t, err)
	defer s.Close()

	owner := columnar.ResourceOwner(map[string]string{"service.name": "payments", "service.instance.id": "pay01"})
	for _, tc := range []struct {
		t    int64
		host string
	}{{300, "a"}, {500, "b"}, {800, "c"}} {
		_, err := s.MetadataAssociate(resourceLabels(0), columnar.Update{
			Owner:         owner,
			Content:       columnar.Content{Attributes: map[string]string{"host.name": tc.host, "version": "1"}},
			EffectiveFrom: tc.t,
		})
		require.NoError(t, err)
	}

	for _, tc := range []struct {
		t    int64
		host string
		ok   bool
	}{
		{200, "", false},
		{400, "a", true},
		{499, "a", true},
		{500, "b", true},
		{799, "b", true},
		{800, "c", true},
		{1100, "c", true},
	} {
		content, ok, _ := s.MetadataAtLabels(resourceLabels(0), columnar.MetadataResource, tc.t)
		require.Equal(t, tc.ok, ok)
		require.Equal(t, tc.host, content.Attributes["host.name"])
	}
}

func TestMetadataConflictingContentRejected(t *testing.T) {
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{})
	require.NoError(t, err)
	defer s.Close()

	owner := columnar.ResourceOwner(map[string]string{"service.name": "payments", "service.instance.id": "pay01"})
	for _, host := range []string{"a", "b"} {
		_, err := s.MetadataAssociate(resourceLabels(0), columnar.Update{
			Owner:         owner,
			Content:       columnar.Content{Attributes: map[string]string{"host.name": host}},
			EffectiveFrom: 100,
		})
		if host == "b" {
			require.ErrorIs(t, err, columnar.ErrConflictingMetadata)
			continue
		}
		require.NoError(t, err)
	}

	content, ok, _ := s.MetadataAtLabels(resourceLabels(0), columnar.MetadataResource, 200)
	require.True(t, ok)
	require.Equal(t, "a", content.Attributes["host.name"], "a failed second update must not overwrite the owner")
}

func TestMetadataIdentityLabelMatchesOwner(t *testing.T) {
	attributes := map[string]string{"service.name": "payments", "service.instance.id": "id-0"}
	owner1 := columnar.ResourceOwner(attributes)
	attributes2 := map[string]string{"service.name": "payments", "service.instance.id": "id-1"}
	owner2 := columnar.ResourceOwner(attributes2)
	require.NotEqual(t, owner1.Ref(), owner2.Ref())

	metricLabels := func(owner columnar.OwnerKey) labels.Labels {
		return labels.FromStrings("__otel_resource__", owner.Ref(), "__name__", "http_requests_total")
	}

	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{})
	require.NoError(t, err)
	defer s.Close()

	for _, owner := range []columnar.OwnerKey{owner1, owner2} {
		_, err := s.MetadataAssociate(metricLabels(owner), columnar.Update{
			Owner: owner, Content: columnar.Content{Attributes: map[string]string{"region": "eu"}}, EffectiveFrom: 1,
		})
		require.NoError(t, err)
	}

	// An impoverished or misleading identity label is explicitly rejected before
	// any metadata owner history is created.
	_, err = s.MetadataAssociate(metricLabels(owner1), columnar.Update{
		Owner: owner2, Content: columnar.Content{Attributes: map[string]string{"region": "us"}}, EffectiveFrom: 2,
	})
	require.ErrorIs(t, err, columnar.ErrConflictingMembership)
}

func TestMetadataKindsAreNotResourceLabels(t *testing.T) {
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{})
	require.NoError(t, err)
	defer s.Close()

	ls := labels.FromStrings("__name__", "http_requests_rate", "service", "payments")
	family := ls.Get(labels.MetricName)
	u := columnar.Update{
		Owner:         columnar.ScopeOwner("test-sdk", "1.0"),
		Content:       columnar.Content{Attributes: map[string]string{"schema_url": "https://schemas/acme/v1", "version": "1"}},
		EffectiveFrom: 12,
	}

	_, err = s.MetadataAssociate(ls, u)
	require.NoError(t, err)

	content, ok, _ := s.MetadataAtLabels(ls, columnar.MetadataScope, 12)
	require.True(t, ok)
	require.Equal(t, "https://schemas/acme/v1", content.Attributes["schema_url"])

	_, ok, _ = s.MetadataAtLabels(ls, columnar.MetadataResource, 12)
	require.False(t, ok, "Scope schema URL is not presented as a Resource attribute")

	_, err = s.MetadataAssociate(ls, columnar.Update{
		Owner:         columnar.ScrapeOwner(family),
		Content:       columnar.Content{Attributes: map[string]string{"interval_ms": "15000"}},
		EffectiveFrom: 12,
	})
	require.NoError(t, err)
	cadence, ok, _ := s.MetadataAtLabels(ls, columnar.MetadataScrape, 12)
	require.True(t, ok)
	require.Equal(t, "15000", cadence.Attributes["interval_ms"])
}

func resourceLabels(i int) labels.Labels {
	return labels.FromStrings("__name__", fmt.Sprintf("payments_%d", i), "__otel_resource__", resourceLabelForTest().Ref())
}

func resourceLabelForTest() columnar.OwnerKey {
	return columnar.ResourceOwner(map[string]string{"service.name": "payments", "service.instance.id": "pay01"})
}

func TestMetadataBatchSemantics(t *testing.T) {
	owner := columnar.ResourceOwner(map[string]string{"service.name": "payments", "service.instance.id": "id0"})
	ls := labels.FromStrings("__name__", "batch_metric", "__otel_resource__", owner.Ref())

	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{})
	require.NoError(t, err)
	defer s.Close()

	metadataUpdates := []columnar.AssociationUpdate{{Labels: ls, Update: columnar.Update{
		Owner:         owner,
		Content:       columnar.Content{Attributes: map[string]string{"host.name": "host-1"}},
		EffectiveFrom: 5000,
	}}}

	err = s.AppendBatch(t.Context(), columnar.Batch{
		Series: []columnar.BatchSeries{
			{Labels: ls, Timestamps: []int64{1000, 2000}, Kinds: []columnar.SampleKind{columnar.SampleFloat, columnar.SampleFloat}, Values: []float64{1, 2}, Histograms: make([][]byte, 2)},
			{Labels: ls, Timestamps: []int64{3000}, Kinds: []columnar.SampleKind{columnar.SampleFloat}, Values: []float64{3}, Histograms: make([][]byte, 1)},
		},
		Updates: metadataUpdates,
	})
	require.NoError(t, err)

	state, ok, _ := s.MetadataAtLabels(ls, columnar.MetadataResource, 6000)
	require.True(t, ok)
	require.Equal(t, "host-1", state.Attributes["host.name"])

	q, err := s.Querier(0, 10000)
	require.NoError(t, err)
	ss := q.Select(t.Context(), false, nil, labels.MustNewMatcher(labels.MatchEqual, "__name__", "batch_metric"))
	require.True(t, ss.Next())
	it := ss.At().Iterator(nil)
	require.Equal(t, 1, int(it.Next()))
	at, in := it.At()
	require.Equal(t, int64(1000), at)
	require.Equal(t, 1.0, in)

	// An individual timestamps input is not a free-form association update. Both
	// values and schemas must be typed on ingestion; string metadata doesn't drift
	// into labels.
	require.False(t, ss.Next())
	require.NoError(t, ss.Err())
}

func TestMetricKindIsSharedThroughRealSeries(t *testing.T) {
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{})
	require.NoError(t, err)
	defer s.Close()

	app := s.AppenderV2(t.Context())
	ls := labels.FromStrings("__name__", "request_total")
	_, err = app.Append(0, ls, 0, 1, 2, nil, nil, storage.AOptions{
		MetricFamilyName: "request",
		Metadata:         metadata.Metadata{Type: model.MetricTypeCounter, Unit: "1", Help: "the counter"},
	})
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	content, ok, truncated := s.MetadataAtLabels(ls, columnar.MetadataMetric, 1000)
	require.True(t, ok)
	require.False(t, truncated)
	require.Equal(t, "the counter", content.Attributes["help"])
	require.Equal(t, "1", content.Attributes["unit"])
	require.Equal(t, "counter", content.Attributes["type"])

	// No semantic projection into Resource metadata just because this is a metric-field
	// kind. The association is explicitly scoped to the Metric owner.
	_, ok, _ = s.MetadataAtLabels(ls, columnar.MetadataResource, 1000)
	require.False(t, ok)
}

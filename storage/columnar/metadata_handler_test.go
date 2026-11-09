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
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
)

type metadataAPITestFixture struct {
	storage *Storage
	handler *MetadataHandler
	lsA     labels.Labels
	lsB     labels.Labels
	ownerA  OwnerKey
	ownerB  OwnerKey
	scope   OwnerKey
}

func TestMetadataDisabledHTTP(t *testing.T) {
	s, err := NewStorage(t.TempDir(), StorageOptions{DisableNativeMetadata: true, DisableWAL: true})
	require.NoError(t, err)
	defer s.Close()
	handler := NewMetadataHandler(s, slog.New(slog.DiscardHandler))
	for _, path := range []string{"/resources", "/resources/series", "/resources/diff"} {
		query := url.Values{}
		if path == "/resources/series" {
			query.Set("resource.attr", "region:east")
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), http.NoBody))
		require.Equal(t, http.StatusNotImplemented, recorder.Code, recorder.Body.String())
	}
}

func newMetadataAPITestFixture(t *testing.T) metadataAPITestFixture {
	t.Helper()
	s, err := NewStorage(t.TempDir(), StorageOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ownerA := ResourceOwner(map[string]string{"service.name": "payments", "service.instance.id": "one"})
	ownerB := ResourceOwner(map[string]string{"service.name": "payments", "service.instance.id": "two"})
	scope := ScopeOwner("sdk", "v1")
	fixture := metadataAPITestFixture{
		storage: s, handler: NewMetadataHandler(s, slog.New(slog.DiscardHandler)),
		lsA:    labels.FromStrings(labels.MetricName, "metric_a", "instance", "one", "job", "payments", OTELResourceLabel, ownerA.Ref()),
		lsB:    labels.FromStrings(labels.MetricName, "metric_b", "instance", "two", "job", "payments", OTELResourceLabel, ownerB.Ref()),
		ownerA: ownerA, ownerB: ownerB, scope: scope,
	}

	appendPoint := func(ls labels.Labels, owner OwnerKey, timestamp int64, host string) {
		t.Helper()
		resourceContent := Content{Attributes: map[string]string{"host.name": host, "region": "east"}}
		updates := []AssociationUpdate{
			{Labels: ls, Update: Update{Owner: owner, EffectiveFrom: timestamp, Content: resourceContent}},
			{Labels: ls, Update: Update{Owner: ResourceSchemaOwner(owner), EffectiveFrom: timestamp, Content: schemaContent("resource-v1")}},
			{Labels: ls, Update: Update{Owner: scope, EffectiveFrom: timestamp, Content: schemaContent("scope-v1")}},
			{Labels: ls, Update: Update{Owner: MetricOwner(ls), EffectiveFrom: timestamp, Content: Content{Attributes: map[string]string{"help": "metric help", "type": "gauge"}}}},
		}
		require.NoError(t, s.AppendBatch(t.Context(), Batch{
			Series: []BatchSeries{{
				Labels: ls, Timestamps: []int64{timestamp}, StartTimestamps: []int64{0},
				Kinds: []SampleKind{SampleFloat}, Values: []float64{1}, Histograms: [][]byte{nil},
			}},
			Updates: updates,
		}))
	}
	appendPoint(fixture.lsA, ownerA, 100, "h1")
	appendPoint(fixture.lsB, ownerB, 200, "h9")
	appendPoint(fixture.lsA, ownerA, 300, "h2")

	// Scope ownership is independent of descriptive Resource changes and series.
	require.NoError(t, s.MetadataUpdate(Update{
		Owner: scope, EffectiveFrom: 400,
		Content: Content{Attributes: map[string]string{"schema_url": "scope-v2"}},
	}))
	_, err = s.MetadataAssociate(fixture.lsB, Update{
		Owner: scope, EffectiveFrom: 400,
		Content: Content{Attributes: map[string]string{"schema_url": "scope-v2"}},
	})
	require.NoError(t, err)

	for timestamp, endpoint := range map[int64]string{100: "a", 200: "b"} {
		_, err := s.MetadataAssociate(fixture.lsA, Update{
			Owner: SourceOwner("receiver-" + endpoint), EffectiveFrom: timestamp,
			Content: Content{Attributes: map[string]string{"endpoint": endpoint}},
		})
		require.NoError(t, err)
	}
	// An owner-only value never fabricates a series when no association exists.
	require.NoError(t, s.MetadataUpdate(Update{
		Owner: SourceOwner("otap"), EffectiveFrom: 50,
		Content: Content{Attributes: map[string]string{"transport": "otap"}},
	}))
	return fixture
}

func (f metadataAPITestFixture) request(t *testing.T, path string, query url.Values) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), http.NoBody))
	return recorder
}

func (f metadataAPITestFixture) query(t *testing.T, path string, query url.Values, target any) {
	t.Helper()
	recorder := f.request(t, path, query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var envelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Equal(t, "success", envelope.Status)
	// Test callers may reuse the same output variable across pages. Reset it, as
	// the JSON package otherwise retains keys in an existing non-nil map.
	reflect.ValueOf(target).Elem().SetZero()
	require.NoError(t, json.Unmarshal(envelope.Data, target))
}

func (f metadataAPITestFixture) queryError(t *testing.T, path string, query url.Values) string {
	t.Helper()
	recorder := f.request(t, path, query)
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	return recorder.Body.String()
}

func TestMetadataOwnersHTTP(t *testing.T) {
	f := newMetadataAPITestFixture(t)
	var page MetadataOwnerPage
	f.query(t, "/resources", url.Values{"start": {"0"}, "end": {"0.25"}}, &page)
	require.Len(t, page.Owners, 2)
	byID := map[string]MetadataOwnerView{}
	for _, owner := range page.Owners {
		byID[owner.ID] = owner
	}
	ownerA := byID[f.ownerA.Ref()]
	require.Equal(t, "one", ownerA.Identifying["service.instance.id"])
	require.Len(t, ownerA.Versions, 1)
	require.Equal(t, "h1", ownerA.Versions[0].Descriptive["host.name"])
	ownerB := byID[f.ownerB.Ref()]
	require.Equal(t, "h9", ownerB.Versions[0].Descriptive["host.name"])

	f.query(t, "/resources", url.Values{
		"end": {"0.4"}, "latest": {"true"}, "attributes": {"host.name"},
		"match[]": {`{__name__="metric_a"}`},
	}, &page)
	require.Len(t, page.Owners, 1)
	require.Equal(t, f.ownerA.Ref(), page.Owners[0].ID)
	require.Len(t, page.Owners[0].Versions, 1)
	require.Equal(t, "h2", page.Owners[0].Versions[0].Descriptive["host.name"])
	require.NotContains(t, page.Owners[0].Versions[0].Descriptive, "region")

	f.query(t, "/resources", url.Values{
		"kind": {"scope"}, "start": {"0"}, "end": {"0.4"},
	}, &page)
	require.Len(t, page.Owners, 1)
	require.Equal(t, f.scope.Ref(), page.Owners[0].ID)
	require.Equal(t, "sdk", page.Owners[0].Identifying["name"])
	require.Len(t, page.Owners[0].Versions, 2)
	require.NotContains(t, page.Owners[0].Versions[0].Descriptive, "host.name")

	var discovery MetadataDiscoveryPage
	f.query(t, "/resources", url.Values{
		"format": {"attributes"}, "start": {"0"}, "end": {"0.25"}, "attributes": {"host.name,region"}, "limit": {"1"},
	}, &discovery)
	require.Len(t, discovery.Attributes, 1)
	require.Equal(t, "host.name", discovery.Attributes[0].Name)
	require.Equal(t, []string{"h1", "h9"}, discovery.Attributes[0].Values)
	require.NotEmpty(t, discovery.NextToken)
	f.query(t, "/resources", url.Values{
		"format": {"attributes"}, "start": {"0"}, "end": {"0.25"}, "attributes": {"host.name,region"},
		"limit": {"1"}, "next_token": {discovery.NextToken},
	}, &discovery)
	require.Len(t, discovery.Attributes, 1)
	require.Equal(t, "region", discovery.Attributes[0].Name)
	require.Equal(t, []string{"east"}, discovery.Attributes[0].Values)
	require.Empty(t, discovery.NextToken)

	f.query(t, "/resources", url.Values{"limit": {"1"}}, &page)
	require.Len(t, page.Owners, 1)
	require.NotEmpty(t, page.NextToken)
	f.query(t, "/resources", url.Values{"limit": {"1"}, "next_token": {page.NextToken}}, &page)
	require.Len(t, page.Owners, 1)
	require.Empty(t, page.NextToken, "the second Resource has its own unchanged owner history")
	body := f.queryError(t, "/resources", url.Values{"projection": {"*"}})
	require.Contains(t, body, "unbounded metadata projection")
}

func TestMetadataSeriesHTTP(t *testing.T) {
	f := newMetadataAPITestFixture(t)
	var page MetadataSeriesPage
	f.query(t, "/resources/series", url.Values{
		"resource.attr": {"region:east"}, "start": {"0"}, "end": {"0.4"}, "limit": {"1"},
	}, &page)
	require.Len(t, page.Series, 1)
	require.Equal(t, "metric_a", page.Series[0].Labels.Get(labels.MetricName))
	require.Len(t, page.Series[0].Versions, 2)
	require.NotEmpty(t, page.NextToken)
	f.query(t, "/resources/series", url.Values{
		"resource.attr": {"region:east"}, "start": {"0"}, "end": {"0.4"}, "limit": {"1"}, "next_token": {page.NextToken},
	}, &page)
	require.Len(t, page.Series, 1)
	require.Equal(t, "metric_b", page.Series[0].Labels.Get(labels.MetricName))
	require.Empty(t, page.NextToken)

	f.query(t, "/resources/series", url.Values{
		"resource.attr": {"host.name:h1"}, "start": {"0"}, "end": {"0.05"},
	}, &page)
	require.Empty(t, page.Series, "future Resource versions do not match before their effective timestamp")
	f.query(t, "/resources/series", url.Values{
		"resource.attr": {"host.name:h1"}, "start": {"0.3"}, "end": {"0.3"},
	}, &page)
	require.Empty(t, page.Series, "successor version is exclusive at the change timestamp")

	f.query(t, "/resources/series", url.Values{
		"kind": {"scope"}, "resource.attr": {"schema_url:scope-v1"}, "start": {"0"}, "end": {"0.25"},
	}, &page)
	require.Len(t, page.Series, 2)
	for _, series := range page.Series {
		require.Equal(t, f.scope.Ref(), series.Owner)
		require.Len(t, series.Versions, 1)
		require.Equal(t, map[string]string{"schema_url": "scope-v1"}, series.Versions[0].Descriptive)
	}
	f.query(t, "/resources/series", url.Values{
		"kind": {"source"}, "resource.attr": {"endpoint:b"}, "start": {"0"}, "end": {"0.25"},
	}, &page)
	require.Len(t, page.Series, 1)
	require.Equal(t, int64(200), page.Series[0].MembershipStart)
	require.Equal(t, "metric_a", page.Series[0].Labels.Get(labels.MetricName))
	f.query(t, "/resources/series", url.Values{
		"kind": {"resource_schema"}, "resource.attr": {"schema_url:resource-v1"}, "start": {"0"}, "end": {"0.25"},
	}, &page)
	require.Len(t, page.Series, 2, "schema and Resource memberships remain distinct owner kinds")

	var names struct {
		Names []string `json:"names"`
	}
	f.query(t, "/resources/series", url.Values{
		"resource.attr": {"region:east"}, "start": {"0"}, "end": {"0.4"}, "view": {"names"},
	}, &names)
	require.Equal(t, []string{"metric_a", "metric_b"}, names.Names)
	var summary struct {
		MatchedSeries  int `json:"matched_series"`
		MetricFamilies int `json:"metric_families"`
	}
	f.query(t, "/resources/series", url.Values{
		"resource.attr": {"region:east"}, "start": {"0"}, "end": {"0.4"}, "view": {"summary"},
	}, &summary)
	require.Equal(t, 2, summary.MatchedSeries)
	require.Equal(t, 2, summary.MetricFamilies)
}

func TestMetadataDiffHTTP(t *testing.T) {
	f := newMetadataAPITestFixture(t)
	var page MetadataDiffPage
	f.query(t, "/resources/diff", url.Values{
		"time": {"0.4"}, "attributes": {"host.name"}, "match[]": {`{__name__="metric_a"}`},
	}, &page)
	require.Len(t, page.Changes, 1)
	change := page.Changes[0]
	require.Equal(t, f.ownerA.Ref(), change.Owner.ID)
	require.NotNil(t, change.Before)
	require.Equal(t, "h1", change.Before.Descriptive["host.name"])
	require.Equal(t, "h2", change.After.Descriptive["host.name"])
	require.Equal(t, "h1->h2", change.Changed["host.name"])
	require.NotContains(t, change.Changed, "region", "unprojected fields do not appear as spurious removals")

	f.query(t, "/resources/diff", url.Values{
		"time": {"0.4"}, "kind": {"scope"}, "match[]": {`{__name__="metric_b"}`},
	}, &page)
	require.Len(t, page.Changes, 1)
	require.Equal(t, f.scope.Ref(), page.Changes[0].Owner.ID)
	require.Equal(t, "scope-v1->scope-v2", page.Changes[0].Changed["schema_url"])

	f.query(t, "/resources/diff", url.Values{"time": {"0.4"}, "limit": {"1"}}, &page)
	require.Len(t, page.Changes, 1)
	require.NotEmpty(t, page.NextToken)
	f.query(t, "/resources/diff", url.Values{"time": {"0.4"}, "limit": {"1"}, "next_token": {page.NextToken}}, &page)
	require.Len(t, page.Changes, 1)
	require.Empty(t, page.NextToken)
	body := f.queryError(t, "/resources/series", url.Values{"resource.attr": {"bad"}})
	require.Contains(t, body, "name:value")
}

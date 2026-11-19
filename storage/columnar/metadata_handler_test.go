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

func newMetadataAPITestFixtureWithOptions(t *testing.T, opts StorageOptions) metadataAPITestFixture {
	t.Helper()
	s, err := NewStorage(t.TempDir(), opts)
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

func newMetadataAPITestFixture(t *testing.T) metadataAPITestFixture {
	t.Helper()
	return newMetadataAPITestFixtureWithOptions(t, StorageOptions{})
}

func (f metadataAPITestFixture) request(t *testing.T, method, path string, query url.Values) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, httptest.NewRequest(method, path+"?"+query.Encode(), http.NoBody))
	return recorder
}

func (f metadataAPITestFixture) query(t *testing.T, path string, query url.Values, target any) {
	t.Helper()
	recorder := f.request(t, http.MethodGet, path, query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var envelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Equal(t, "success", envelope.Status)
	require.NoError(t, json.Unmarshal(envelope.Data, target))
}

func (f metadataAPITestFixture) queryError(t *testing.T, path string, query url.Values, code string) string {
	t.Helper()
	recorder := f.request(t, http.MethodGet, path, query)
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	body := recorder.Body.String()
	require.Contains(t, body, `"code":"`+code+`"`, body)
	return body
}

// metadataTestEntry returns one exact content entry.
func metadataTestEntry(t *testing.T, entries []metadataHTTPEntry, key string) metadataTypedValue {
	t.Helper()
	for _, entry := range entries {
		if entry.Key == key {
			return entry.Value
		}
	}
	require.FailNowf(t, "missing content entry", "key %q", key)
	return metadataTypedValue{}
}

// metadataTestIdentity returns one identity attribute.
func metadataTestIdentity(t *testing.T, identity metadataHTTPIdentity, key string) metadataTypedValue {
	t.Helper()
	return metadataTestEntry(t, identity.Attributes, key)
}

func TestMetadataDisabledHTTP(t *testing.T) {
	s, err := NewStorage(t.TempDir(), StorageOptions{DisableNativeMetadata: true, DisableWAL: true})
	require.NoError(t, err)
	defer s.Close()
	handler := NewMetadataHandler(s, slog.New(slog.DiscardHandler))
	for _, path := range []string{"/resources", "/resources/series", "/resources/diff?time=1"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, http.NoBody))
		require.Equal(t, http.StatusNotImplemented, recorder.Code, recorder.Body.String())
		require.Contains(t, recorder.Body.String(), `"code":"metadata_disabled"`)
	}
}

func TestMetadataOwnersHTTP(t *testing.T) {
	f := newMetadataAPITestFixture(t)
	var page metadataHTTPOwnerPage
	f.query(t, "/resources", url.Values{"start": {"0"}, "end": {"0.25"}}, &page)
	require.Len(t, page.Owners, 2)
	require.NotEmpty(t, page.SnapshotID)
	require.Nil(t, page.NextToken, "a complete page has a null next_token")
	require.NotNil(t, page.Annotations)
	byRef := map[string]metadataHTTPOwnerRow{}
	for _, owner := range page.Owners {
		byRef[owner.Owner.Ref] = owner
	}
	ownerA := byRef[f.ownerA.Ref()]
	require.Equal(t, "resource", ownerA.Owner.Kind)
	instance := metadataTestIdentity(t, ownerA.Owner.Identity, "service.instance.id")
	require.Equal(t, "present", instance.state)
	require.Equal(t, "one", *instance.str)
	require.Len(t, ownerA.Versions, 1)
	assertion := ownerA.Versions[0]
	require.Equal(t, "complete_authoritative", assertion.Content.Scope)
	host := metadataTestEntry(t, assertion.Content.Entries, "host.name")
	require.Equal(t, "present", host.state)
	require.Equal(t, "string", host.typ)
	require.Equal(t, "h1", *host.str)
	require.Equal(t, "100", assertion.Validity.FromMS)
	require.Equal(t, "300", *assertion.Validity.UntilMS, "the successor ends this version exclusively")
	require.Equal(t, "100", *assertion.Observation.FirstMS)
	require.Equal(t, "covered", assertion.Coverage.State)
	require.Equal(t, "unknown", assertion.Source["state"])

	f.query(t, "/resources", url.Values{
		"end": {"0.4"}, "latest": {"true"}, "projection": {"host.name"},
		"match[]": {`{__name__="metric_a"}`},
	}, &page)
	require.Len(t, page.Owners, 1)
	require.Equal(t, f.ownerA.Ref(), page.Owners[0].Owner.Ref)
	require.Len(t, page.Owners[0].Versions, 1)
	require.Equal(t, "h2", *metadataTestEntry(t, page.Owners[0].Versions[0].Content.Entries, "host.name").str)
	require.Len(t, page.Owners[0].Versions[0].Content.Entries, 1, "projection filters unrequested content")
	require.Equal(t, "300", page.Owners[0].Versions[0].Validity.FromMS)
	require.Nil(t, page.Owners[0].Versions[0].Validity.UntilMS)

	f.query(t, "/resources", url.Values{
		"kind": {"scope"}, "start": {"0"}, "end": {"0.4"},
	}, &page)
	require.Len(t, page.Owners, 1)
	require.Equal(t, f.scope.Ref(), page.Owners[0].Owner.Ref)
	require.Equal(t, "sdk", *metadataTestIdentity(t, page.Owners[0].Owner.Identity, "name").str)
	require.Len(t, page.Owners[0].Versions, 2)

	var discovery metadataHTTPDiscoveryPage
	f.query(t, "/resources", url.Values{
		"format": {"attributes"}, "start": {"0"}, "end": {"0.25"},
		"projection": {"host.name,region"}, "limit": {"1"},
	}, &discovery)
	require.Len(t, discovery.Attributes, 1)
	require.Equal(t, "host.name", discovery.Attributes[0].Key)
	require.Len(t, discovery.Attributes[0].Values, 2)
	require.Equal(t, "h1", *discovery.Attributes[0].Values[0].str)
	require.Equal(t, "h9", *discovery.Attributes[0].Values[1].str)
	require.NotNil(t, discovery.NextToken)
	firstToken := *discovery.NextToken
	f.query(t, "/resources", url.Values{
		"format": {"attributes"}, "start": {"0"}, "end": {"0.25"},
		"projection": {"host.name,region"}, "limit": {"2"}, "next_token": {firstToken},
	}, &discovery)
	require.Len(t, discovery.Attributes, 1)
	require.Equal(t, "region", discovery.Attributes[0].Key)
	require.Nil(t, discovery.NextToken)

	f.query(t, "/resources", url.Values{"limit": {"1"}}, &page)
	require.Len(t, page.Owners, 1)
	require.NotNil(t, page.NextToken)
	f.query(t, "/resources", url.Values{"limit": {"1"}, "next_token": {*page.NextToken}}, &page)
	require.Len(t, page.Owners, 1)
	require.Nil(t, page.NextToken, "the second Resource has its own unchanged owner history")
	f.queryError(t, "/resources", url.Values{"projection": {"*"}}, "metadata_projection_invalid")
}

func TestMetadataSeriesHTTP(t *testing.T) {
	f := newMetadataAPITestFixture(t)
	var page metadataHTTPSeriesPage
	f.query(t, "/resources/series", url.Values{
		"resource.attr": {"region:east"}, "start": {"0"}, "end": {"0.4"}, "limit": {"1"},
	}, &page)
	require.Len(t, page.Series, 1)
	require.Equal(t, "applicable", page.Series[0].Association.State)
	require.Equal(t, "metric_a", page.Series[0].Series[0].Value)
	require.NotNil(t, page.Series[0].Owner)
	require.Len(t, page.Series[0].Versions, 2)
	require.NotNil(t, page.NextToken)
	f.query(t, "/resources/series", url.Values{
		"resource.attr": {"region:east"}, "start": {"0"}, "end": {"0.4"}, "limit": {"1"},
		"next_token": {*page.NextToken},
	}, &page)
	require.Len(t, page.Series, 1)
	require.Equal(t, "metric_b", page.Series[0].Series[0].Value)
	require.Nil(t, page.NextToken)

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
		require.Equal(t, f.scope.Ref(), series.Owner.Ref)
		require.Len(t, series.Versions, 1)
	}
	f.query(t, "/resources/series", url.Values{
		"kind": {"source"}, "resource.attr": {"endpoint:b"}, "start": {"0"}, "end": {"0.25"},
	}, &page)
	require.Len(t, page.Series, 1)
	require.Equal(t, "200", page.Series[0].Association.Validity.FromMS)
	require.Equal(t, "metric_a", page.Series[0].Series[0].Value)
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
	f.queryError(t, "/resources/series", url.Values{"resource.attr": {"bad"}}, "metadata_matcher_invalid")
}

func TestMetadataDiffHTTP(t *testing.T) {
	f := newMetadataAPITestFixture(t)
	var page metadataHTTPDiffPage
	f.query(t, "/resources/diff", url.Values{
		"time": {"0.4"}, "projection": {"host.name"}, "match[]": {`{__name__="metric_a"}`},
	}, &page)
	require.Len(t, page.Changes, 1)
	change := page.Changes[0]
	require.Equal(t, f.ownerA.Ref(), change.Owner.Ref)
	require.Equal(t, "300", change.EffectiveFromMS)
	require.Equal(t, "present", change.Before.State)
	require.Equal(t, "present", change.After.State)
	before := metadataTestEntry(t, change.Before.Assertion.Content.Entries, "host.name")
	after := metadataTestEntry(t, change.After.Assertion.Content.Entries, "host.name")
	require.Equal(t, "h1", *before.str)
	require.Equal(t, "h2", *after.str)
	require.Len(t, change.Fields, 1)
	require.Equal(t, "present", change.Fields[0].Before.state)
	require.Equal(t, "present", change.Fields[0].After.state)
	require.Equal(t, "h1", *change.Fields[0].Before.str)
	require.Equal(t, "h2", *change.Fields[0].After.str)

	f.query(t, "/resources/diff", url.Values{
		"time": {"0.4"}, "kind": {"scope"}, "match[]": {`{__name__="metric_b"}`},
	}, &page)
	require.Len(t, page.Changes, 1)
	require.Equal(t, f.scope.Ref(), page.Changes[0].Owner.Ref)
	field := changeField(t, page.Changes[0], "schema_url")
	require.Equal(t, "scope-v1", *field.Before.str)
	require.Equal(t, "scope-v2", *field.After.str)

	f.query(t, "/resources/diff", url.Values{"time": {"0.4"}, "limit": {"1"}}, &page)
	require.Len(t, page.Changes, 1)
	require.NotNil(t, page.NextToken)
	f.query(t, "/resources/diff", url.Values{
		"time": {"0.4"}, "limit": {"1"}, "next_token": {*page.NextToken},
	}, &page)
	require.Len(t, page.Changes, 1)
	require.Nil(t, page.NextToken)
	f.queryError(t, "/resources/diff", url.Values{}, "metadata_time_invalid")
}

func changeField(t *testing.T, change metadataHTTPDiffRow, key string) metadataHTTPDiffField {
	t.Helper()
	for _, field := range change.Fields {
		if field.Key == key {
			return field
		}
	}
	require.FailNowf(t, "missing diff field", "key %q", key)
	return metadataHTTPDiffField{}
}

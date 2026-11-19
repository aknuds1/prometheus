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

// Independent decision-7 HTTP oracle. Logical inputs and expected page objects
// are authored here from the accepted contract. The reference renderer and the
// cursor encoder are test-only and do not call the production response builders
// or the production cursor encoder. A deterministic internal randomness seam
// fixes the incarnation, key and snapshot IDs so expected token bytes can be
// computed independently.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

// oracleRandom is a deterministic byte stream installed through the internal
// metadata snapshot randomness seam.
type oracleRandom struct {
	data   []byte
	offset int
}

func installOracleRandom(t *testing.T, seed byte) *oracleRandom {
	t.Helper()
	data := make([]byte, 4096)
	for i := range data {
		data[i] = seed + byte(i)
	}
	reader := &oracleRandom{data: data}
	previous := metadataSnapshotRandomRead
	metadataSnapshotRandomRead = reader.read
	t.Cleanup(func() { metadataSnapshotRandomRead = previous })
	return reader
}

func (r *oracleRandom) read(p []byte) (int, error) {
	copy(p, r.data[r.offset:])
	r.offset += len(p)
	return len(p), nil
}

func (r *oracleRandom) bytes(from, length int) []byte {
	return r.data[from : from+length]
}

// oracleSnapshotID returns the nth snapshot ID created by the fixture (zero
// based), after the 16-byte incarnation and 32-byte cursor key.
func (r *oracleRandom) oracleSnapshotID(n int) string {
	return base64.RawURLEncoding.EncodeToString(r.bytes(48+16*n, 16))
}

// oracleFixture is one independently authored logical dataset.
type oracleFixture struct {
	store   *Storage
	handler *MetadataHandler
	random  *oracleRandom
	r1, r2  OwnerKey
	r1Ref   string
	r2Ref   string
	s1, s2  labels.Labels
	source  OwnerKey
	schema  OwnerKey
}

func oracleOwnerRef(kind, id string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + id))
	return hex.EncodeToString(sum[:])
}

func oracleResourceKey(name, namespace, instance string) (OwnerKey, string) {
	parts := []string{
		"service.instance.id", instance,
		"service.name", name,
		"service.namespace", namespace,
	}
	encoded, err := json.Marshal(parts)
	if err != nil {
		panic(err)
	}
	id := string(encoded)
	return OwnerKey{Kind: MetadataResource, ID: id}, oracleOwnerRef("resource", id)
}

func newOracleFixture(t *testing.T) *oracleFixture {
	t.Helper()
	random := installOracleRandom(t, 0x40)
	store, err := NewStorage(t.TempDir(), StorageOptions{DisableWAL: true, MetadataVersionCap: 2})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	handler := NewMetadataHandler(store, nil)

	r1, r1Ref := oracleResourceKey("oracle", "shop", "one")
	r2, r2Ref := oracleResourceKey("oracle", "shop", "two")
	s1 := labels.FromStrings(labels.MetricName, "oracle_total", "instance", "one", OTELResourceLabel, r1Ref)
	s2 := labels.FromStrings(labels.MetricName, "oracle_second", "instance", "two", OTELResourceLabel, r2Ref)
	source := SourceOwner("oracle-source")
	schema := ResourceSchemaOwner(r1)

	for _, update := range []Update{
		{Owner: r1, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"a": "1"}}},
		{Owner: r1, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"a": "2", "b": ""}}},
		{Owner: r1, EffectiveFrom: 300, Content: Content{Attributes: map[string]string{"a": "3"}}},
		{Owner: r2, EffectiveFrom: 250, Content: Content{Attributes: map[string]string{"x": "y"}}},
	} {
		require.NoError(t, store.MetadataUpdate(update))
	}
	metadataHTTPTestAssociate(t, store, s1, r1, map[string]string{"a": "2", "b": ""}, 200)
	metadataHTTPTestAssociate(t, store, s1, schema, map[string]string{"schema_url": "schema-v1"}, 200)
	metadataHTTPTestAssociate(t, store, s1, source, map[string]string{"endpoint": "a"}, 200)
	metadataHTTPTestAssociate(t, store, s2, r2, map[string]string{"x": "y"}, 250)

	return &oracleFixture{
		store: store, handler: handler, random: random,
		r1: r1, r2: r2, r1Ref: r1Ref, r2Ref: r2Ref, s1: s1, s2: s2,
		source: source, schema: schema,
	}
}

// ---------------------------------------------------------------------------
// Reference renderer.

func oracleString(value string) map[string]any {
	return map[string]any{"state": "present", "type": "string", "value": value}
}

func oracleAbsent() map[string]any { return map[string]any{"state": "absent"} }

func oracleUnknown(reason string) map[string]any {
	return map[string]any{"state": "unknown", "reason": reason}
}

func oracleMS(value int64) string { return strconv.FormatInt(value, 10) }

func oracleMSOrNil(value *int64) any {
	if value == nil {
		return nil
	}
	return oracleMS(*value)
}

func oracleResourceIdentity(name, namespace, instance string) []any {
	entries := []any{
		map[string]any{"key": "service.instance.id", "value": oracleString(instance)},
		map[string]any{"key": "service.name", "value": oracleString(name)},
	}
	if namespace == "" {
		entries = append(entries, map[string]any{
			"key": "service.namespace", "value": oracleUnknown("compatibility_identity_absent_empty_unrecoverable"),
		})
	} else {
		entries = append(entries, map[string]any{"key": "service.namespace", "value": oracleString(namespace)})
	}
	return entries
}

func oracleResourceOwner(name, namespace, instance, ref string) map[string]any {
	return map[string]any{
		"kind": "resource",
		"identity": map[string]any{
			"kind": "resource", "attributes": oracleResourceIdentity(name, namespace, instance),
		},
		"ref": ref,
	}
}

func oracleSourceOwner(name, ref string) map[string]any {
	return map[string]any{
		"kind": "source",
		"identity": map[string]any{
			"kind":       "source",
			"attributes": []any{map[string]any{"key": "source", "value": oracleString(name)}},
		},
		"ref": ref,
	}
}

func oracleLabels(series labels.Labels) []any {
	out := make([]any, 0, series.Len())
	series.Range(func(label labels.Label) {
		out = append(out, map[string]any{"name": label.Name, "value": label.Value})
	})
	return out
}

func oracleContent(attributes map[string]string, projection []string) map[string]any {
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	slicesSortStrings(keys)
	entries := []any{}
	if projection != nil {
		keys = append([]string(nil), projection...)
	}
	for _, key := range keys {
		value, present := attributes[key]
		if present {
			entries = append(entries, map[string]any{"key": key, "value": oracleString(value)})
		} else {
			entries = append(entries, map[string]any{"key": key, "value": oracleAbsent()})
		}
	}
	return map[string]any{"scope": "complete_authoritative", "entries": entries}
}

// slicesSortStrings avoids importing sort just for the reference renderer.
func slicesSortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func oracleCovered(scope string) map[string]any {
	return map[string]any{
		"state": "covered", "scope": scope, "earliest_retained_ms": nil,
		"ranges": []any{}, "reasons": []any{},
	}
}

func oracleTruncated(scope string, earliest int64, reasons ...string) map[string]any {
	renderedReasons := make([]any, 0, len(reasons))
	for _, reason := range reasons {
		renderedReasons = append(renderedReasons, reason)
	}
	return map[string]any{
		"state": "truncated", "scope": scope, "earliest_retained_ms": oracleMS(earliest),
		"ranges":  []any{map[string]any{"from_ms": oracleMS(earliest), "until_ms": nil}},
		"reasons": renderedReasons,
	}
}

func oracleUnknownCoverage(scope string, reasons ...string) map[string]any {
	renderedReasons := make([]any, 0, len(reasons))
	for _, reason := range reasons {
		renderedReasons = append(renderedReasons, reason)
	}
	return map[string]any{
		"state": "unknown", "scope": scope, "earliest_retained_ms": nil,
		"ranges": []any{}, "reasons": renderedReasons,
	}
}

func oracleAssertion(
	attributes map[string]string, from int64, until *int64, observedFrom, observedUntil int64,
	projection []string, historyTruncated bool, earliest, queryStart int64,
) map[string]any {
	coverage := oracleCovered("owner_history")
	if historyTruncated && queryStart < earliest {
		coverage = oracleTruncated("owner_history", earliest, "history_truncated")
	}
	return map[string]any{
		"content":  oracleContent(attributes, projection),
		"validity": map[string]any{"from_ms": oracleMS(from), "until_ms": oracleMSOrNil(until)},
		"observation": map[string]any{
			"first_ms": oracleMS(observedFrom), "last_ms": oracleMS(observedUntil),
		},
		"coverage": coverage,
		"source":   map[string]any{"state": "unknown", "reason": "provenance_not_recorded"},
	}
}

func oracleRequested(start, end int64) map[string]any {
	return map[string]any{"from_ms": oracleMS(start), "until_ms": oracleMS(end)}
}

func oracleRetained(earliest *int64, reasons ...string) map[string]any {
	renderedReasons := make([]any, 0, len(reasons))
	for _, reason := range reasons {
		renderedReasons = append(renderedReasons, reason)
	}
	retained := map[string]any{"earliest_retained_ms": nil}
	if earliest != nil {
		retained["earliest_retained_ms"] = oracleMS(*earliest)
		retained["ranges"] = []any{map[string]any{"from_ms": oracleMS(*earliest), "until_ms": nil}}
	} else {
		retained["ranges"] = []any{}
	}
	retained["reasons"] = renderedReasons
	return retained
}

func oracleOwnerHistoryAnnotation(owner map[string]any, series []any, scope string, start, end, earliest int64) map[string]any {
	annotation := map[string]any{
		"category": "coverage", "axis": "coverage", "scope": scope, "state": "truncated",
		"reason": "history_truncated", "owner": owner, "requested": oracleRequested(start, end),
		"retained": oracleRetained(&earliest, "history_truncated"),
	}
	if series != nil {
		annotation["series"] = series
	}
	return annotation
}

func oracleAssociationObservationAnnotation(owner map[string]any, series []any, from int64, until *int64, observedUntil, start, end int64) map[string]any {
	return map[string]any{
		"category": "coverage", "axis": "coverage", "scope": "series_association", "state": "unknown",
		"reason": "first_observation_unrecorded", "owner": owner, "series": series,
		"association": map[string]any{
			"validity": map[string]any{"from_ms": oracleMS(from), "until_ms": oracleMSOrNil(until)},
		},
		"observation": map[string]any{"first_ms": nil, "last_ms": oracleMS(observedUntil)},
		"requested":   oracleRequested(start, end),
		"retained":    oracleRetained(nil, "first_observation_unrecorded"),
	}
}

func oracleDiscoveryAnnotation(kind, key string, start, end, earliest int64) map[string]any {
	return map[string]any{
		"category": kind, "axis": "coverage", "scope": "discovery", "state": "truncated",
		"reason": "history_truncated", "key": key,
		"attribute": map[string]any{"category": kind, "key": key},
		"requested": oracleRequested(start, end),
		"retained":  oracleRetained(&earliest, "history_truncated"),
	}
}

func oracleDiffAnnotation(owner map[string]any, time, earliest int64) map[string]any {
	return map[string]any{
		"category": "coverage", "axis": "coverage", "scope": "diff", "state": "truncated",
		"reason": "lost_predecessor", "owner": owner,
		"requested": map[string]any{"time_ms": oracleMS(time)},
		"retained":  oracleRetained(&earliest, "lost_predecessor", "history_truncated"),
	}
}

func oracleOwnerRow(owner map[string]any, versions ...any) map[string]any {
	return map[string]any{"owner": owner, "versions": versions}
}

func oraclePage(snapshotID string, next any, annotations []any, key string, rows []any) map[string]any {
	if annotations == nil {
		annotations = []any{}
	}
	if rows == nil {
		rows = []any{}
	}
	return map[string]any{
		"snapshot_id": snapshotID, "next_token": next, "annotations": annotations, key: rows,
	}
}

func oracleSuccessEnvelope(data any) map[string]any {
	return map[string]any{"status": "success", "data": data}
}

func oracleRequireJSON(t *testing.T, got []byte, want any) {
	t.Helper()
	encoded, err := json.Marshal(want)
	require.NoError(t, err)
	require.JSONEq(t, string(encoded), string(got))
}

func oracleGet(t *testing.T, handler *MetadataHandler, path string, query url.Values) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), http.NoBody)
	handler.ServeHTTP(recorder, request)
	return recorder
}

// ---------------------------------------------------------------------------
// Independent cursor encoder.

type oracleOwnerPosition struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type oracleCursorSigned struct {
	Version     int             `json:"version"`
	Incarnation string          `json:"incarnation"`
	Snapshot    string          `json:"snapshot"`
	Binding     string          `json:"binding"`
	Position    json.RawMessage `json:"position"`
}

type oracleCursorEnvelope struct {
	oracleCursorSigned
	Signature string `json:"signature"`
}

type oracleBinding struct {
	Operation  string   `json:"operation"`
	Kind       string   `json:"kind"`
	StartMS    int64    `json:"start_ms"`
	EndMS      int64    `json:"end_ms"`
	TimeMS     *int64   `json:"time_ms,omitempty"`
	Matchers   []string `json:"matchers"`
	Attributes []string `json:"attributes"`
	Projection []string `json:"projection"`
	Latest     bool     `json:"latest"`
}

func oracleBindingJSON(operation, kind string, start, end int64, matchers, attributes, projection []string, latest bool) string {
	if matchers == nil {
		matchers = []string{}
	}
	if attributes == nil {
		attributes = []string{}
	}
	if projection == nil {
		projection = []string{}
	}
	encoded, err := json.Marshal(oracleBinding{
		Operation: operation, Kind: kind, StartMS: start, EndMS: end,
		Matchers: matchers, Attributes: attributes, Projection: projection, Latest: latest,
	})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func oracleCursorToken(t *testing.T, random *oracleRandom, binding string, position any, snapshotID string) string {
	t.Helper()
	rawPosition, err := json.Marshal(position)
	require.NoError(t, err)
	signed := oracleCursorSigned{
		Version: metadataCursorVersion, Incarnation: base64.RawURLEncoding.EncodeToString(random.bytes(0, 16)),
		Snapshot: snapshotID, Binding: binding, Position: rawPosition,
	}
	rawSigned, err := json.Marshal(signed)
	require.NoError(t, err)
	mac := hmac.New(sha256.New, random.bytes(16, 32))
	_, _ = mac.Write(rawSigned)
	envelope := oracleCursorEnvelope{oracleCursorSigned: signed, Signature: hex.EncodeToString(mac.Sum(nil))}
	rawEnvelope, err := json.Marshal(envelope)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(rawEnvelope)
}

func (f *oracleFixture) oracleCursor(t *testing.T, binding string, position any, snapshotID string) string {
	t.Helper()
	return oracleCursorToken(t, f.random, binding, position, snapshotID)
}

func (f *matchFixture) oracleCursor(t *testing.T, binding string, position any, snapshotID string) string {
	t.Helper()
	return oracleCursorToken(t, f.random, binding, position, snapshotID)
}

// ---------------------------------------------------------------------------
// Owner-history oracle.

func TestMetadataHTTPOracleOwnerHistory(t *testing.T) {
	f := newOracleFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	binding := oracleBindingJSON("history", "resource", 150, 350, nil, nil, nil, false)
	page1Token := f.oracleCursor(t, binding, oracleOwnerPosition{Kind: "resource", ID: f.r1.ID}, snapshotID)
	owner1 := oracleResourceOwner("oracle", "shop", "one", f.r1Ref)
	owner2 := oracleResourceOwner("oracle", "shop", "two", f.r2Ref)
	page1 := oracleSuccessEnvelope(oraclePage(snapshotID, page1Token, []any{
		oracleOwnerHistoryAnnotation(owner1, nil, "owner_history", 150, 350, 200),
	}, "owners", []any{
		oracleOwnerRow(owner1,
			oracleAssertion(map[string]string{"a": "2", "b": ""}, 200, int64Pointer(300), 200, 200, nil, true, 200, 150),
			oracleAssertion(map[string]string{"a": "3"}, 300, nil, 300, 300, nil, true, 200, 150),
		),
	}))
	recorder := oracleGet(t, f.handler, "/resources", url.Values{
		"limit": {"1"}, "start": {"0.15"}, "end": {"0.35"},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page1)

	page2 := oracleSuccessEnvelope(oraclePage(snapshotID, nil, nil, "owners", []any{
		oracleOwnerRow(owner2,
			oracleAssertion(map[string]string{"x": "y"}, 250, nil, 250, 250, nil, false, 0, 150),
		),
	}))
	recorder = oracleGet(t, f.handler, "/resources", url.Values{
		"limit": {"1"}, "start": {"0.15"}, "end": {"0.35"}, "next_token": {page1Token},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page2)
}

func int64Pointer(value int64) *int64 { return &value }

func TestMetadataHTTPOracleLatestAndProjection(t *testing.T) {
	f := newOracleFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	owner1 := oracleResourceOwner("oracle", "shop", "one", f.r1Ref)
	owner2 := oracleResourceOwner("oracle", "shop", "two", f.r2Ref)
	latest := oracleSuccessEnvelope(oraclePage(snapshotID, nil, []any{
		oracleOwnerHistoryAnnotation(owner1, nil, "owner_history", 150, 350, 200),
	}, "owners", []any{
		oracleOwnerRow(owner1, oracleAssertion(map[string]string{"a": "3"}, 300, nil, 300, 300, nil, true, 200, 150)),
		oracleOwnerRow(owner2, oracleAssertion(map[string]string{"x": "y"}, 250, nil, 250, 250, nil, false, 0, 150)),
	}))
	recorder := oracleGet(t, f.handler, "/resources", url.Values{
		"start": {"0.15"}, "end": {"0.35"}, "latest": {"true"},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), latest)

	projected := oracleSuccessEnvelope(oraclePage(f.random.oracleSnapshotID(1), nil, []any{
		oracleOwnerHistoryAnnotation(owner1, nil, "owner_history", 150, 350, 200),
	}, "owners", []any{
		oracleOwnerRow(owner1,
			oracleAssertion(map[string]string{"a": "2", "b": ""}, 200, int64Pointer(300), 200, 200, []string{"a"}, true, 200, 150),
			oracleAssertion(map[string]string{"a": "3"}, 300, nil, 300, 300, []string{"a"}, true, 200, 150),
		),
		oracleOwnerRow(owner2, oracleAssertion(map[string]string{"x": "y"}, 250, nil, 250, 250, []string{"a"}, false, 0, 150)),
	}))
	recorder = oracleGet(t, f.handler, "/resources", url.Values{
		"start": {"0.15"}, "end": {"0.35"}, "projection": {"a"},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), projected)
}

func TestMetadataHTTPOracleExactFullPage(t *testing.T) {
	f := newOracleFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	owner1 := oracleResourceOwner("oracle", "shop", "one", f.r1Ref)
	owner2 := oracleResourceOwner("oracle", "shop", "two", f.r2Ref)
	expected := oracleSuccessEnvelope(oraclePage(snapshotID, nil, []any{
		oracleOwnerHistoryAnnotation(owner1, nil, "owner_history", 150, 350, 200),
	}, "owners", []any{
		oracleOwnerRow(owner1,
			oracleAssertion(map[string]string{"a": "2", "b": ""}, 200, int64Pointer(300), 200, 200, nil, true, 200, 150),
			oracleAssertion(map[string]string{"a": "3"}, 300, nil, 300, 300, nil, true, 200, 150),
		),
		oracleOwnerRow(owner2, oracleAssertion(map[string]string{"x": "y"}, 250, nil, 250, 250, nil, false, 0, 150)),
	}))
	recorder := oracleGet(t, f.handler, "/resources", url.Values{"start": {"0.15"}, "end": {"0.35"}})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), expected)
}

func TestMetadataHTTPOracleEmptyAndAfterLast(t *testing.T) {
	emptyStore, err := NewStorage(t.TempDir(), StorageOptions{DisableWAL: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, emptyStore.Close()) })
	emptyHandler := NewMetadataHandler(emptyStore, nil)
	recorder := oracleGet(t, emptyHandler, "/resources", url.Values{"start": {"0"}, "end": {"1"}})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var envelope struct {
		Data struct {
			SnapshotID  string `json:"snapshot_id"`
			NextToken   any    `json:"next_token"`
			Annotations []any  `json:"annotations"`
			Owners      []any  `json:"owners"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.NotEmpty(t, envelope.Data.SnapshotID)
	require.Nil(t, envelope.Data.NextToken)
	require.NotNil(t, envelope.Data.Annotations)
	require.NotNil(t, envelope.Data.Owners)
	require.Empty(t, envelope.Data.Owners)

	f := newOracleFixture(t)
	first := metadataHTTPReadOwnerPage(t, oracleGet(t, f.handler, "/resources", url.Values{
		"limit": {"1"}, "start": {"0.15"}, "end": {"0.35"},
	}))
	require.NotNil(t, first.NextToken)
	snapshotID := first.SnapshotID
	binding := oracleBindingJSON("history", "resource", 150, 350, nil, nil, nil, false)
	pastToken := f.oracleCursor(t, binding, oracleOwnerPosition{Kind: "resource", ID: "zzzz"}, snapshotID)
	recorder = oracleGet(t, f.handler, "/resources", url.Values{
		"limit": {"1"}, "start": {"0.15"}, "end": {"0.35"}, "next_token": {pastToken},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), oracleSuccessEnvelope(oraclePage(snapshotID, nil, nil, "owners", nil)))
}

// ---------------------------------------------------------------------------
// Full series, discovery, diff and combined pages.

func TestMetadataHTTPOracleSeriesFull(t *testing.T) {
	f := newOracleFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	owner := oracleResourceOwner("oracle", "shop", "one", f.r1Ref)
	associationValidity := map[string]any{"from_ms": "200", "until_ms": nil}
	expected := oracleSuccessEnvelope(oraclePage(snapshotID, nil, []any{
		oracleAssociationObservationAnnotation(owner, oracleLabels(f.s1), 200, nil, 200, 150, 350),
		oracleOwnerHistoryAnnotation(owner, oracleLabels(f.s1), "owner_history", 150, 350, 200),
	}, "series", []any{
		map[string]any{
			"series": oracleLabels(f.s1),
			"association": map[string]any{
				"state": "applicable", "validity": associationValidity,
				"observation": map[string]any{"first_ms": nil, "last_ms": "200"},
				"coverage":    oracleUnknownCoverage("series_association", "first_observation_unrecorded"),
			},
			"owner": owner,
			"versions": []any{
				oracleAssertion(map[string]string{"a": "2", "b": ""}, 200, int64Pointer(300), 200, 200, nil, true, 200, 150),
			},
		},
	}))
	recorder := oracleGet(t, f.handler, "/resources/series", url.Values{
		"kind": {"resource"}, "resource.attr": {"a:2"}, "start": {"0.15"}, "end": {"0.35"},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), expected)
}

func TestMetadataHTTPOracleDiscovery(t *testing.T) {
	f := newOracleFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	expected := oracleSuccessEnvelope(oraclePage(snapshotID, nil, []any{
		oracleDiscoveryAnnotation("resource", "a", 150, 350, 200),
		oracleDiscoveryAnnotation("resource", "b", 150, 350, 200),
	}, "attributes", []any{
		map[string]any{
			"key": "a", "values": []any{oracleString("2"), oracleString("3")},
			"coverage": oracleTruncated("discovery", 200, "history_truncated"),
		},
		map[string]any{
			"key": "b", "values": []any{oracleString("")},
			"coverage": oracleTruncated("discovery", 200, "history_truncated"),
		},
		map[string]any{
			"key": "x", "values": []any{oracleString("y")},
			"coverage": oracleCovered("discovery"),
		},
	}))
	recorder := oracleGet(t, f.handler, "/resources", url.Values{
		"format": {"attributes"}, "start": {"0.15"}, "end": {"0.35"}, "projection": {"a,b,x"},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), expected)
}

func TestMetadataHTTPOracleDiff(t *testing.T) {
	f := newOracleFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	owner := oracleResourceOwner("oracle", "shop", "one", f.r1Ref)
	afterAssertion := oracleAssertion(map[string]string{"a": "2", "b": ""}, 200, int64Pointer(300), 200, 200, nil, true, 200, 200)
	expected := oracleSuccessEnvelope(oraclePage(snapshotID, nil, []any{
		oracleDiffAnnotation(owner, 200, 200),
	}, "changes", []any{
		map[string]any{
			"owner": owner, "effective_from_ms": "200",
			"before": map[string]any{
				"state": "unknown", "reason": "lost_predecessor",
				"coverage": oracleTruncated("diff", 200, "lost_predecessor", "history_truncated"),
			},
			"after": map[string]any{
				"state": "present", "assertion": afterAssertion, "coverage": oracleCovered("owner_history"),
			},
			"fields": []any{
				map[string]any{"key": "a", "before": oracleUnknown("lost_predecessor"), "after": oracleString("2")},
				map[string]any{"key": "b", "before": oracleUnknown("lost_predecessor"), "after": oracleString("")},
			},
			"coverage": oracleTruncated("diff", 200, "lost_predecessor", "history_truncated"),
		},
	}))
	recorder := oracleGet(t, f.handler, "/resources/diff", url.Values{
		"time": {"0.2"}, "match[]": {`{__name__="oracle_total"}`},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), expected)
}

func TestMetadataHTTPOracleCombined(t *testing.T) {
	f := newOracleFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	owner := oracleResourceOwner("oracle", "shop", "one", f.r1Ref)
	sourceOwner := oracleSourceOwner("oracle-source", oracleOwnerRef("source", "oracle-source"))
	component := map[string]any{
		"owner": owner,
		"schema": []any{
			oracleAssertion(map[string]string{"schema_url": "schema-v1"}, 200, nil, 200, 200, nil, false, 0, 150),
		},
		"versions": []any{
			oracleAssertion(map[string]string{"a": "2", "b": ""}, 200, int64Pointer(300), 200, 200, nil, true, 200, 150),
			oracleAssertion(map[string]string{"a": "3"}, 300, nil, 300, 300, nil, true, 200, 150),
		},
		"source_assertions": []any{
			map[string]any{
				"owner": sourceOwner,
				"versions": []any{
					oracleAssertion(map[string]string{"endpoint": "a"}, 200, nil, 200, 200, nil, false, 0, 150),
				},
			},
		},
	}
	expected := oracleSuccessEnvelope(oraclePage(snapshotID, nil, []any{
		oracleOwnerHistoryAnnotation(owner, oracleLabels(f.s1), "combined", 150, 350, 200),
	}, "groups", []any{
		map[string]any{
			"series": oracleLabels(f.s1), "relations": []any{}, "components": []any{component},
			"coverage": oracleTruncated("owner_history", 200, "history_truncated"),
		},
	}))
	recorder := oracleGet(t, f.handler, "/resources/series", url.Values{
		"view": {"combined"}, "kind": {"resource"}, "match[]": {`{__name__="oracle_total"}`},
		"start": {"0.15"}, "end": {"0.35"},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), expected)
}

func TestMetadataHTTPOracleErrorEnvelopeKeys(t *testing.T) {
	f := newOracleFixture(t)
	recorder := oracleGet(t, f.handler, "/resources", url.Values{"limit": {"0"}})
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Len(t, body, 5, "error payloads must carry status, errorType, error, data and details")
	for _, key := range []string{"status", "errorType", "error", "data", "details"} {
		require.Contains(t, body, key)
	}
	require.Equal(t, "error", body["status"])
	require.Equal(t, "bad_data", body["errorType"])
	require.Nil(t, body["data"], "a missing data key must not pass as nil")
	require.NotEmpty(t, body["error"])
	details := body["details"].(map[string]any)
	require.Equal(t, "metadata_limit_invalid", details["code"])

	// Reclaimed restart errors carry the reason in details.
	dir := t.TempDir()
	oldStore, err := NewStorage(dir, StorageOptions{DisableWAL: true})
	require.NoError(t, err)
	oldHandler := NewMetadataHandler(oldStore, nil)
	require.NoError(t, oldStore.MetadataUpdate(Update{
		Owner: f.r1, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"a": "1"}},
	}))
	require.NoError(t, oldStore.MetadataUpdate(Update{
		Owner: f.r2, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"x": "y"}},
	}))
	oldPage := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t, oldHandler, "/resources?limit=1&start=0&end=1"))
	require.NotNil(t, oldPage.NextToken)
	require.NoError(t, oldStore.Close())
	newStore, err := NewStorage(dir, StorageOptions{DisableWAL: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, newStore.Close()) })
	newHandler := NewMetadataHandler(newStore, nil)
	recorder = oracleGet(t, newHandler, "/resources", url.Values{
		"limit": {"1"}, "start": {"0"}, "end": {"1"}, "next_token": {*oldPage.NextToken},
	})
	require.Equal(t, http.StatusGone, recorder.Code)
	body = nil
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Len(t, body, 5)
	require.Nil(t, body["data"])
	details = body["details"].(map[string]any)
	require.Equal(t, "metadata_snapshot_reclaimed", details["code"])
	require.Equal(t, "store_restarted", details["reason"])
}

type oracleDiscoveryPosition struct {
	Key string `json:"key"`
}

func TestMetadataHTTPOracleDiscoveryPagination(t *testing.T) {
	f := newOracleFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	binding := oracleBindingJSON("discovery", "resource", 150, 350, nil, nil, []string{"a", "b", "x"}, false)
	page1Token := f.oracleCursor(t, binding, oracleDiscoveryPosition{Key: "a"}, snapshotID)
	page2Token := f.oracleCursor(t, binding, oracleDiscoveryPosition{Key: "b"}, snapshotID)
	query := url.Values{
		"format": {"attributes"}, "start": {"0.15"}, "end": {"0.35"}, "projection": {"a,b,x"}, "limit": {"1"},
	}
	page1 := oracleSuccessEnvelope(oraclePage(snapshotID, page1Token, []any{
		oracleDiscoveryAnnotation("resource", "a", 150, 350, 200),
	}, "attributes", []any{
		map[string]any{
			"key": "a", "values": []any{oracleString("2"), oracleString("3")},
			"coverage": oracleTruncated("discovery", 200, "history_truncated"),
		},
	}))
	recorder := oracleGet(t, f.handler, "/resources", query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page1)

	page2 := oracleSuccessEnvelope(oraclePage(snapshotID, page2Token, []any{
		oracleDiscoveryAnnotation("resource", "b", 150, 350, 200),
	}, "attributes", []any{
		map[string]any{
			"key": "b", "values": []any{oracleString("")},
			"coverage": oracleTruncated("discovery", 200, "history_truncated"),
		},
	}))
	query.Set("next_token", page1Token)
	recorder = oracleGet(t, f.handler, "/resources", query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page2)

	page3 := oracleSuccessEnvelope(oraclePage(snapshotID, nil, nil, "attributes", []any{
		map[string]any{
			"key": "x", "values": []any{oracleString("y")},
			"coverage": oracleCovered("discovery"),
		},
	}))
	query.Set("next_token", page2Token)
	recorder = oracleGet(t, f.handler, "/resources", query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page3)
}

type oracleSeriesPosition struct {
	Labels    string `json:"labels"`
	State     string `json:"state"`
	OwnerKind string `json:"owner_kind,omitempty"`
	OwnerID   string `json:"owner_id,omitempty"`
	FromMS    int64  `json:"from_ms"`
}

func oracleSeriesRow(series labels.Labels, owner map[string]any, from int64, versions ...any) map[string]any {
	return map[string]any{
		"series": oracleLabels(series),
		"association": map[string]any{
			"state":       "applicable",
			"validity":    map[string]any{"from_ms": oracleMS(from), "until_ms": nil},
			"observation": map[string]any{"first_ms": nil, "last_ms": oracleMS(from)},
			"coverage":    oracleUnknownCoverage("series_association", "first_observation_unrecorded"),
		},
		"owner":    owner,
		"versions": versions,
	}
}

func TestMetadataHTTPOracleSeriesPagination(t *testing.T) {
	f := newOracleFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	binding := oracleBindingJSON("series_full", "resource", 150, 350,
		[]string{"=~\x00__name__\x00oracle_.*"}, nil, nil, false)
	s2Position := oracleSeriesPosition{
		Labels: f.s2.String(), State: "applicable", OwnerKind: "resource", OwnerID: f.r2.ID, FromMS: 250,
	}
	page1Token := f.oracleCursor(t, binding, s2Position, snapshotID)
	owner1 := oracleResourceOwner("oracle", "shop", "one", f.r1Ref)
	owner2 := oracleResourceOwner("oracle", "shop", "two", f.r2Ref)
	query := url.Values{
		"kind": {"resource"}, "match[]": {`{__name__=~"oracle_.*"}`},
		"start": {"0.15"}, "end": {"0.35"}, "limit": {"1"},
	}
	page1 := oracleSuccessEnvelope(oraclePage(snapshotID, page1Token, []any{
		oracleAssociationObservationAnnotation(owner2, oracleLabels(f.s2), 250, nil, 250, 150, 350),
	}, "series", []any{
		oracleSeriesRow(f.s2, owner2, 250,
			oracleAssertion(map[string]string{"x": "y"}, 250, nil, 250, 250, nil, false, 0, 150)),
	}))
	recorder := oracleGet(t, f.handler, "/resources/series", query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page1)

	page2 := oracleSuccessEnvelope(oraclePage(snapshotID, nil, []any{
		oracleAssociationObservationAnnotation(owner1, oracleLabels(f.s1), 200, nil, 200, 150, 350),
		oracleOwnerHistoryAnnotation(owner1, oracleLabels(f.s1), "owner_history", 150, 350, 200),
	}, "series", []any{
		oracleSeriesRow(f.s1, owner1, 200,
			oracleAssertion(map[string]string{"a": "2", "b": ""}, 200, int64Pointer(300), 200, 200, nil, true, 200, 150),
			oracleAssertion(map[string]string{"a": "3"}, 300, nil, 300, 300, nil, true, 200, 150)),
	}))
	query.Set("next_token", page1Token)
	recorder = oracleGet(t, f.handler, "/resources/series", query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page2)
}

type oracleDiffPosition struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	From int64  `json:"from_ms"`
}

func oracleDiffBindingJSON(kind string, time int64, matchers, projection []string) string {
	at := time
	if matchers == nil {
		matchers = []string{}
	}
	if projection == nil {
		projection = []string{}
	}
	encoded, err := json.Marshal(oracleBinding{
		Operation: "diff", Kind: kind, TimeMS: &at,
		Matchers: matchers, Attributes: []string{}, Projection: projection,
	})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func TestMetadataHTTPOracleDiffPagination(t *testing.T) {
	f := newOracleFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	binding := oracleDiffBindingJSON("resource", 250, nil, nil)
	page1Token := f.oracleCursor(t, binding, oracleDiffPosition{Kind: "resource", ID: f.r1.ID, From: 200}, snapshotID)
	owner1 := oracleResourceOwner("oracle", "shop", "one", f.r1Ref)
	owner2 := oracleResourceOwner("oracle", "shop", "two", f.r2Ref)
	after1 := oracleAssertion(map[string]string{"a": "2", "b": ""}, 200, int64Pointer(300), 200, 200, nil, true, 200, 250)
	page1 := oracleSuccessEnvelope(oraclePage(snapshotID, page1Token, []any{
		oracleDiffAnnotation(owner1, 250, 200),
	}, "changes", []any{
		map[string]any{
			"owner": owner1, "effective_from_ms": "200",
			"before": map[string]any{
				"state": "unknown", "reason": "lost_predecessor",
				"coverage": oracleTruncated("diff", 200, "lost_predecessor", "history_truncated"),
			},
			"after": map[string]any{
				"state": "present", "assertion": after1, "coverage": oracleCovered("owner_history"),
			},
			"fields": []any{
				map[string]any{"key": "a", "before": oracleUnknown("lost_predecessor"), "after": oracleString("2")},
				map[string]any{"key": "b", "before": oracleUnknown("lost_predecessor"), "after": oracleString("")},
			},
			"coverage": oracleTruncated("diff", 200, "lost_predecessor", "history_truncated"),
		},
	}))
	query := url.Values{"time": {"0.25"}, "limit": {"1"}}
	recorder := oracleGet(t, f.handler, "/resources/diff", query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page1)

	after2 := oracleAssertion(map[string]string{"x": "y"}, 250, nil, 250, 250, nil, false, 0, 250)
	page2 := oracleSuccessEnvelope(oraclePage(snapshotID, nil, nil, "changes", []any{
		map[string]any{
			"owner": owner2, "effective_from_ms": "250",
			"before": map[string]any{"state": "absent", "coverage": oracleCovered("diff")},
			"after": map[string]any{
				"state": "present", "assertion": after2, "coverage": oracleCovered("owner_history"),
			},
			"fields": []any{
				map[string]any{"key": "x", "before": oracleAbsent(), "after": oracleString("y")},
			},
			"coverage": oracleCovered("diff"),
		},
	}))
	query.Set("next_token", page1Token)
	recorder = oracleGet(t, f.handler, "/resources/diff", query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page2)
}

type oracleGroupPosition struct {
	Labels    string `json:"labels"`
	Relation  string `json:"relation"`
	OwnerKind string `json:"owner_kind,omitempty"`
	OwnerID   string `json:"owner_id,omitempty"`
	FromMS    int64  `json:"from_ms"`
}

func TestMetadataHTTPOracleCombinedPagination(t *testing.T) {
	f := newOracleFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	binding := oracleBindingJSON("series_combined", "resource", 150, 350,
		[]string{"=~\x00__name__\x00oracle_.*"}, nil, nil, false)
	page1Token := f.oracleCursor(t, binding, oracleGroupPosition{
		Labels: f.s2.String(), OwnerKind: "resource", OwnerID: f.r2.ID, FromMS: 250,
	}, snapshotID)
	owner2 := oracleResourceOwner("oracle", "shop", "two", f.r2Ref)
	page1 := oracleSuccessEnvelope(oraclePage(snapshotID, page1Token, nil, "groups", []any{
		map[string]any{
			"series": oracleLabels(f.s2), "relations": []any{},
			"components": []any{
				map[string]any{
					"owner": owner2, "schema": []any{},
					"versions": []any{
						oracleAssertion(map[string]string{"x": "y"}, 250, nil, 250, 250, nil, false, 0, 150),
					},
					"source_assertions": []any{},
				},
			},
			"coverage": oracleCovered("combined"),
		},
	}))
	query := url.Values{
		"view": {"combined"}, "kind": {"resource"}, "match[]": {`{__name__=~"oracle_.*"}`},
		"start": {"0.15"}, "end": {"0.35"}, "limit": {"1"},
	}
	recorder := oracleGet(t, f.handler, "/resources/series", query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page1)

	owner1 := oracleResourceOwner("oracle", "shop", "one", f.r1Ref)
	sourceOwner := oracleSourceOwner("oracle-source", oracleOwnerRef("source", "oracle-source"))
	page2 := oracleSuccessEnvelope(oraclePage(snapshotID, nil, []any{
		oracleOwnerHistoryAnnotation(owner1, oracleLabels(f.s1), "combined", 150, 350, 200),
	}, "groups", []any{
		map[string]any{
			"series": oracleLabels(f.s1), "relations": []any{},
			"components": []any{
				map[string]any{
					"owner": owner1,
					"schema": []any{
						oracleAssertion(map[string]string{"schema_url": "schema-v1"}, 200, nil, 200, 200, nil, false, 0, 150),
					},
					"versions": []any{
						oracleAssertion(map[string]string{"a": "2", "b": ""}, 200, int64Pointer(300), 200, 200, nil, true, 200, 150),
						oracleAssertion(map[string]string{"a": "3"}, 300, nil, 300, 300, nil, true, 200, 150),
					},
					"source_assertions": []any{
						map[string]any{
							"owner": sourceOwner,
							"versions": []any{
								oracleAssertion(map[string]string{"endpoint": "a"}, 200, nil, 200, 200, nil, false, 0, 150),
							},
						},
					},
				},
			},
			"coverage": oracleTruncated("owner_history", 200, "history_truncated"),
		},
	}))
	query.Set("next_token", page1Token)
	recorder = oracleGet(t, f.handler, "/resources/series", query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page2)
}

func TestMetadataHTTPOracleBetweenPages(t *testing.T) {
	f := newOracleFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	page1 := metadataHTTPReadOwnerPage(t, oracleGet(t, f.handler, "/resources", url.Values{
		"limit": {"1"}, "start": {"0.15"}, "end": {"0.35"},
	}))
	require.NotNil(t, page1.NextToken)

	// Live changes after the first page: a new owner, a new version and a new
	// membership. The pinned continuation must not see any of them.
	r3, _ := oracleResourceKey("oracle", "shop", "three")
	require.NoError(t, f.store.MetadataUpdate(Update{
		Owner: r3, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"a": "live"}},
	}))
	require.NoError(t, f.store.MetadataUpdate(Update{
		Owner: f.r1, EffectiveFrom: 250, Content: Content{Attributes: map[string]string{"a": "live"}},
	}))
	metadataHTTPTestAssociate(t, f.store, f.s2, SourceOwner("late-source"), map[string]string{"endpoint": "late"}, 200)

	owner2 := oracleResourceOwner("oracle", "shop", "two", f.r2Ref)
	expected := oracleSuccessEnvelope(oraclePage(snapshotID, nil, nil, "owners", []any{
		oracleOwnerRow(owner2,
			oracleAssertion(map[string]string{"x": "y"}, 250, nil, 250, 250, nil, false, 0, 150)),
	}))
	recorder := oracleGet(t, f.handler, "/resources", url.Values{
		"limit": {"1"}, "start": {"0.15"}, "end": {"0.35"}, "next_token": {*page1.NextToken},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), expected)
}

func TestMetadataHTTPOracleCanceledEnvelopeKeys(t *testing.T) {
	f := newOracleFixture(t)
	first := metadataHTTPReadOwnerPage(t, oracleGet(t, f.handler, "/resources", url.Values{
		"limit": {"1"}, "start": {"0.15"}, "end": {"0.35"},
	}))
	require.NotNil(t, first.NextToken)
	recorder := metadataHTTPDelete(t, f.handler, "/resources/snapshots/"+first.SnapshotID)
	require.Equal(t, http.StatusNoContent, recorder.Code)
	recorder = oracleGet(t, f.handler, "/resources", url.Values{
		"limit": {"1"}, "start": {"0.15"}, "end": {"0.35"}, "next_token": {*first.NextToken},
	})
	require.Equal(t, http.StatusGone, recorder.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Len(t, body, 5)
	require.Nil(t, body["data"])
	require.Equal(t, "unavailable", body["errorType"])
	details := body["details"].(map[string]any)
	require.Equal(t, "metadata_snapshot_canceled", details["code"])
}

// matchFixture is the independent logical dataset for the accepted option-B
// match[] semantics. Samples are irrelevant to metadata association matching:
// the fixture includes sample-backed, metadata-only, owner-only, label-only,
// wrong-kind, out-of-range and exact-boundary subjects.
type matchFixture struct {
	store       *Storage
	handler     *MetadataHandler
	random      *oracleRandom
	owners      map[string]OwnerKey
	refs        map[string]string
	series      map[string]labels.Labels
	scopeOwners map[string]OwnerKey
	scopeRefs   map[string]string
	scopeSeries labels.Labels
}

func newMatchFixture(t *testing.T) *matchFixture {
	t.Helper()
	random := installOracleRandom(t, 0x80)
	store, err := NewStorage(t.TempDir(), StorageOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	fixture := &matchFixture{
		store: store, handler: NewMetadataHandler(store, nil), random: random,
		owners: map[string]OwnerKey{}, refs: map[string]string{}, series: map[string]labels.Labels{},
		scopeOwners: map[string]OwnerKey{}, scopeRefs: map[string]string{},
	}
	addResource := func(instance, name, zone string, effective, sampleAt int64, metadataOnly bool) {
		t.Helper()
		key, ref := oracleResourceKey("match", "shop", instance)
		fixture.owners[instance], fixture.refs[instance] = key, ref
		require.NoError(t, store.MetadataUpdate(Update{
			Owner: key, EffectiveFrom: effective, Content: Content{Attributes: map[string]string{"case": instance}},
		}))
		ls := labels.FromStrings(labels.MetricName, name, "instance", instance, "zone", zone, OTELResourceLabel, ref)
		fixture.series[instance] = ls
		update := AssociationUpdate{Labels: ls, Update: Update{
			Owner: key, EffectiveFrom: effective, Content: Content{Attributes: map[string]string{"case": instance}},
		}}
		if metadataOnly {
			metadataHTTPTestAssociate(t, store, ls, key, map[string]string{"case": instance}, effective)
			return
		}
		batch := Batch{
			Series: []BatchSeries{{
				Labels: ls, Timestamps: []int64{sampleAt}, StartTimestamps: []int64{0},
				Kinds: []SampleKind{SampleFloat}, Values: []float64{1}, Histograms: [][]byte{nil},
			}},
			Updates: []AssociationUpdate{update},
		}
		require.NoError(t, store.AppendBatch(t.Context(), batch))
	}
	addResource("a-in-range", "match_in_total", "east", 100, 150, false)
	addResource("b-out-sample", "match_out_sample_total", "east", 100, 1000, false)
	addResource("c-metadata", "match_metadata_total", "west", 100, 0, true)

	ownerOnly, _ := oracleResourceKey("match", "shop", "d-owner-only")
	fixture.owners["d-owner-only"] = ownerOnly
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: ownerOnly, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"case": "d-owner-only"}},
	}))

	labelOnly, labelOnlyRef := oracleResourceKey("match", "shop", "e-label-only")
	fixture.owners["e-label-only"], fixture.refs["e-label-only"] = labelOnly, labelOnlyRef
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: labelOnly, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"case": "e-label-only"}},
	}))
	labelSeries := labels.FromStrings(labels.MetricName, "match_label_only_total", "instance", "e-label-only", OTELResourceLabel, labelOnlyRef)
	fixture.series["e-label-only"] = labelSeries
	require.NoError(t, store.AppendBatch(t.Context(), Batch{Series: []BatchSeries{{
		Labels: labelSeries, Timestamps: []int64{150}, StartTimestamps: []int64{0},
		Kinds: []SampleKind{SampleFloat}, Values: []float64{1}, Histograms: [][]byte{nil},
	}}}))

	wrongKind, wrongKindRef := oracleResourceKey("match", "shop", "f-wrong-kind")
	fixture.owners["f-wrong-kind"], fixture.refs["f-wrong-kind"] = wrongKind, wrongKindRef
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: wrongKind, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"case": "f-wrong-kind"}},
	}))
	wrongSeries := labels.FromStrings(labels.MetricName, "match_wrong_kind_total", "instance", "f-wrong-kind", OTELResourceLabel, wrongKindRef)
	fixture.series["f-wrong-kind"] = wrongSeries
	require.NoError(t, store.AppendBatch(t.Context(), Batch{Series: []BatchSeries{{
		Labels: wrongSeries, Timestamps: []int64{150}, StartTimestamps: []int64{0},
		Kinds: []SampleKind{SampleFloat}, Values: []float64{1}, Histograms: [][]byte{nil},
	}}}))
	scopeWrong := ScopeOwner("wrong-kind-scope", "v1")
	metadataHTTPTestAssociate(t, store, wrongSeries, scopeWrong, map[string]string{"case": "wrong-kind-scope"}, 100)

	outRange, outRangeRef := oracleResourceKey("match", "shop", "g-out-of-range")
	fixture.owners["g-out-of-range"], fixture.refs["g-out-of-range"] = outRange, outRangeRef
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: outRange, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"case": "g-out-of-range"}},
	}))
	outRangeSeries := labels.FromStrings(labels.MetricName, "match_out_of_range_total", "instance", "g-out-of-range", OTELResourceLabel, outRangeRef)
	fixture.series["g-out-of-range"] = outRangeSeries
	metadataHTTPTestAssociate(t, store, outRangeSeries, outRange, map[string]string{"case": "g-out-of-range"}, 500)

	other, otherRef := oracleResourceKey("match", "shop", "h-other-label")
	fixture.owners["h-other-label"], fixture.refs["h-other-label"] = other, otherRef
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: other, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"case": "h-other-label"}},
	}))
	otherSeries := labels.FromStrings(labels.MetricName, "other_total", "instance", "h-other-label", OTELResourceLabel, otherRef)
	fixture.series["h-other-label"] = otherSeries
	metadataHTTPTestAssociate(t, store, otherSeries, other, map[string]string{"case": "h-other-label"}, 100)

	// A Scope membership successor at exactly 200 tests the exclusive successor
	// boundary without violating the Resource reference-label identity rule.
	scopeA := ScopeOwner("boundary-a", "v1")
	scopeB := ScopeOwner("boundary-b", "v1")
	fixture.scopeOwners["a"], fixture.scopeOwners["b"] = scopeA, scopeB
	fixture.scopeSeries = labels.FromStrings(labels.MetricName, "match_scope_boundary", "instance", "boundary")
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: scopeA, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"case": "scope-a"}},
	}))
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: scopeB, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"case": "scope-b"}},
	}))
	metadataHTTPTestAssociate(t, store, fixture.scopeSeries, scopeA, map[string]string{"case": "scope-a"}, 100)
	metadataHTTPTestAssociate(t, store, fixture.scopeSeries, scopeB, map[string]string{"case": "scope-b"}, 200)
	return fixture
}

func (f *matchFixture) resourceOwner(instance string) map[string]any {
	return oracleResourceOwner("match", "shop", instance, f.refs[instance])
}

func oracleScopeIdentity(name, version string) (string, string) {
	encoded, err := json.Marshal(map[string]string{"name": name, "version": version})
	if err != nil {
		panic(err)
	}
	id := string(encoded)
	return id, oracleOwnerRef("scope", id)
}

func oracleScopeOwner(name, version, ref string) map[string]any {
	return map[string]any{
		"kind": "scope",
		"identity": map[string]any{
			"kind": "scope",
			"attributes": []any{
				map[string]any{"key": "name", "value": oracleString(name)},
				map[string]any{"key": "version", "value": oracleString(version)},
			},
		},
		"ref": ref,
	}
}

func TestMetadataHTTPMatchOwnerHistoryB(t *testing.T) {
	f := newMatchFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	binding := oracleBindingJSON("history", "resource", 150, 350,
		[]string{"=~\x00__name__\x00match_.*"}, nil, nil, false)
	outSample := f.owners["b-out-sample"]
	page1Token := f.oracleCursor(t, binding, oracleOwnerPosition{Kind: "resource", ID: outSample.ID}, snapshotID)
	inRange := f.resourceOwner("a-in-range")
	outSampleOwner := f.resourceOwner("b-out-sample")
	metadataOwner := f.resourceOwner("c-metadata")
	page1 := oracleSuccessEnvelope(oraclePage(snapshotID, page1Token, nil, "owners", []any{
		oracleOwnerRow(inRange,
			oracleAssertion(map[string]string{"case": "a-in-range"}, 100, nil, 100, 100, nil, false, 0, 150)),
		oracleOwnerRow(outSampleOwner,
			oracleAssertion(map[string]string{"case": "b-out-sample"}, 100, nil, 100, 100, nil, false, 0, 150)),
	}))
	query := url.Values{
		"kind": {"resource"}, "match[]": {`{__name__=~"match_.*"}`},
		"start": {"0.15"}, "end": {"0.35"}, "limit": {"2"},
	}
	recorder := oracleGet(t, f.handler, "/resources", query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page1)

	// Between pages: a new owner, a changed content version and a new membership.
	require.NoError(t, f.store.MetadataUpdate(Update{
		Owner: f.owners["a-in-range"], EffectiveFrom: 200,
		Content: Content{Attributes: map[string]string{"case": "live-change"}},
	}))
	newOwner, _ := oracleResourceKey("match", "shop", "z-new")
	require.NoError(t, f.store.MetadataUpdate(Update{
		Owner: newOwner, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"case": "z-new"}},
	}))
	metadataHTTPTestAssociate(t, f.store, f.series["c-metadata"], SourceOwner("late"), map[string]string{"case": "late"}, 200)

	page2 := oracleSuccessEnvelope(oraclePage(snapshotID, nil, nil, "owners", []any{
		oracleOwnerRow(metadataOwner,
			oracleAssertion(map[string]string{"case": "c-metadata"}, 100, nil, 100, 100, nil, false, 0, 150)),
	}))
	query.Set("next_token", page1Token)
	recorder = oracleGet(t, f.handler, "/resources", query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page2)

	// A specific nonmatching-name selector includes the otherwise excluded owner.
	recorder = oracleGet(t, f.handler, "/resources", url.Values{
		"kind": {"resource"}, "match[]": {`{__name__="other_total"}`}, "start": {"0.15"}, "end": {"0.35"},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	other := f.resourceOwner("h-other-label")
	expectedOther := oracleSuccessEnvelope(oraclePage(
		f.random.oracleSnapshotID(1), nil, nil, "owners", []any{
			oracleOwnerRow(other,
				oracleAssertion(map[string]string{"case": "h-other-label"}, 100, nil, 100, 100, nil, false, 0, 150)),
		},
	))
	oracleRequireJSON(t, recorder.Body.Bytes(), expectedOther)

	// Wrong metadata kind: a Scope association does not satisfy a Resource query.
	recorder = oracleGet(t, f.handler, "/resources", url.Values{
		"kind": {"resource"}, "match[]": {`{__name__="match_wrong_kind_total"}`}, "start": {"0.15"}, "end": {"0.35"},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	emptyPage := oracleSuccessEnvelope(oraclePage(f.random.oracleSnapshotID(2), nil, nil, "owners", nil))
	oracleRequireJSON(t, recorder.Body.Bytes(), emptyPage)

	// The same series does satisfy a Scope query.
	_, scopeRef := oracleScopeIdentity("wrong-kind-scope", "v1")
	recorder = oracleGet(t, f.handler, "/resources", url.Values{
		"kind": {"scope"}, "match[]": {`{__name__="match_wrong_kind_total"}`}, "start": {"0.15"}, "end": {"0.35"},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	expectedScope := oracleSuccessEnvelope(oraclePage(
		f.random.oracleSnapshotID(3), nil, nil, "owners", []any{
			oracleOwnerRow(oracleScopeOwner("wrong-kind-scope", "v1", scopeRef),
				oracleAssertion(map[string]string{"case": "wrong-kind-scope"}, 100, nil, 100, 100, nil, false, 0, 150)),
		},
	))
	oracleRequireJSON(t, recorder.Body.Bytes(), expectedScope)
}

func TestMetadataHTTPMatchDiffB(t *testing.T) {
	f := newMatchFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	binding := oracleDiffBindingJSON("resource", 200, []string{"=~\x00__name__\x00match_.*"}, nil)
	// The accepted order is owner identity; in-range < out-sample. The cursor
	// names the last returned object, not the look-ahead.
	outSample := f.owners["b-out-sample"]
	page1Token := f.oracleCursor(t, binding, oracleDiffPosition{Kind: "resource", ID: outSample.ID, From: 100}, snapshotID)
	page1 := oracleSuccessEnvelope(oraclePage(snapshotID, page1Token, nil, "changes", []any{
		oracleInitialDiffRow(f.resourceOwner("a-in-range"), "100", map[string]string{"case": "a-in-range"}),
		oracleInitialDiffRow(f.resourceOwner("b-out-sample"), "100", map[string]string{"case": "b-out-sample"}),
	}))
	query := url.Values{
		"time": {"0.2"}, "match[]": {`{__name__=~"match_.*"}`}, "limit": {"2"},
	}
	recorder := oracleGet(t, f.handler, "/resources/diff", query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page1)

	require.NoError(t, f.store.MetadataUpdate(Update{
		Owner: f.owners["a-in-range"], EffectiveFrom: 250,
		Content: Content{Attributes: map[string]string{"case": "live-change"}},
	}))
	metadataHTTPTestAssociate(t, f.store, f.series["c-metadata"], SourceOwner("late"), map[string]string{"case": "late"}, 200)

	page2 := oracleSuccessEnvelope(oraclePage(snapshotID, nil, nil, "changes", []any{
		oracleInitialDiffRow(f.resourceOwner("c-metadata"), "100", map[string]string{"case": "c-metadata"}),
	}))
	query.Set("next_token", page1Token)
	recorder = oracleGet(t, f.handler, "/resources/diff", query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page2)
}

func oracleInitialDiffRow(owner map[string]any, effective string, content map[string]string) map[string]any {
	keys := make([]string, 0, len(content))
	for key := range content {
		keys = append(keys, key)
	}
	slicesSortStrings(keys)
	fields := make([]any, 0, len(content))
	for _, key := range keys {
		fields = append(fields, map[string]any{"key": key, "before": oracleAbsent(), "after": oracleString(content[key])})
	}
	from, _ := strconv.ParseInt(effective, 10, 64)
	return map[string]any{
		"owner": owner, "effective_from_ms": effective,
		"before":   map[string]any{"state": "absent", "coverage": oracleCovered("diff")},
		"after":    map[string]any{"state": "present", "assertion": oracleAssertion(content, from, nil, from, from, nil, false, 0, from), "coverage": oracleCovered("owner_history")},
		"fields":   fields,
		"coverage": oracleCovered("diff"),
	}
}

func TestMetadataHTTPMatchScopeBoundary(t *testing.T) {
	f := newMatchFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	scopeAID, scopeARef := oracleScopeIdentity("boundary-a", "v1")
	scopeBID, scopeBRef := oracleScopeIdentity("boundary-b", "v1")
	_ = scopeAID
	_ = scopeBID
	scopeA := oracleScopeOwner("boundary-a", "v1", scopeARef)
	scopeB := oracleScopeOwner("boundary-b", "v1", scopeBRef)
	history := oracleSuccessEnvelope(oraclePage(snapshotID, nil, nil, "owners", []any{
		oracleOwnerRow(scopeA, oracleAssertion(map[string]string{"case": "scope-a"}, 100, nil, 100, 100, nil, false, 0, 150)),
		oracleOwnerRow(scopeB, oracleAssertion(map[string]string{"case": "scope-b"}, 200, nil, 200, 200, nil, false, 0, 150)),
	}))
	recorder := oracleGet(t, f.handler, "/resources", url.Values{
		"kind": {"scope"}, "match[]": {`{__name__="match_scope_boundary"}`}, "start": {"0.15"}, "end": {"0.35"},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), history)

	// At exactly 200 the A membership successor is exclusive and only B applies.
	at200 := oracleSuccessEnvelope(oraclePage(f.random.oracleSnapshotID(1), nil, nil, "changes", []any{
		oracleInitialDiffRow(scopeB, "200", map[string]string{"case": "scope-b"}),
	}))
	recorder = oracleGet(t, f.handler, "/resources/diff", url.Values{
		"kind": {"scope"}, "time": {"0.2"}, "match[]": {`{__name__="match_scope_boundary"}`},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), at200)

	// Immediately before 200 only A applies.
	at199 := oracleSuccessEnvelope(oraclePage(f.random.oracleSnapshotID(2), nil, nil, "changes", []any{
		oracleInitialDiffRow(scopeA, "100", map[string]string{"case": "scope-a"}),
	}))
	recorder = oracleGet(t, f.handler, "/resources/diff", url.Values{
		"kind": {"scope"}, "time": {"0.199"}, "match[]": {`{__name__="match_scope_boundary"}`},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), at199)
}

func TestMetadataHTTPMatchOwnerOnlyPublication(t *testing.T) {
	random := installOracleRandom(t, 0x90)
	store, err := NewStorage(t.TempDir(), StorageOptions{DisableWAL: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	handler := NewMetadataHandler(store, nil)

	associated, associatedRef := oracleResourceKey("match", "shop", "a-associated")
	ownerOnly, ownerOnlyRef := oracleResourceKey("match", "shop", "z-owner-only")
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: ownerOnly, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"case": "owner-only"}},
	}))
	series := labels.FromStrings(labels.MetricName, "match_associated_total", "instance", "a-associated", OTELResourceLabel, associatedRef)
	require.NoError(t, store.AppendBatch(t.Context(), Batch{
		Series: []BatchSeries{{
			Labels: series, Timestamps: []int64{150}, StartTimestamps: []int64{0},
			Kinds: []SampleKind{SampleFloat}, Values: []float64{1}, Histograms: [][]byte{nil},
		}},
		Updates: []AssociationUpdate{{
			Labels: series, Update: Update{
				Owner: associated, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"case": "associated"}},
			},
		}},
	}))

	associatedOwner := oracleResourceOwner("match", "shop", "a-associated", associatedRef)
	ownerOnlyOwner := oracleResourceOwner("match", "shop", "z-owner-only", ownerOnlyRef)
	unfilteredHistory := oracleSuccessEnvelope(oraclePage(random.oracleSnapshotID(0), nil, nil, "owners", []any{
		oracleOwnerRow(associatedOwner, oracleAssertion(map[string]string{"case": "associated"}, 100, nil, 100, 100, nil, false, 0, 100)),
		oracleOwnerRow(ownerOnlyOwner, oracleAssertion(map[string]string{"case": "owner-only"}, 100, nil, 100, 100, nil, false, 0, 100)),
	}))
	recorder := oracleGet(t, handler, "/resources", url.Values{"start": {"0.1"}, "end": {"0.1"}})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), unfilteredHistory)

	filteredHistory := oracleSuccessEnvelope(oraclePage(random.oracleSnapshotID(1), nil, nil, "owners", []any{
		oracleOwnerRow(associatedOwner, oracleAssertion(map[string]string{"case": "associated"}, 100, nil, 100, 100, nil, false, 0, 100)),
	}))
	recorder = oracleGet(t, handler, "/resources", url.Values{
		"start": {"0.1"}, "end": {"0.1"}, "match[]": {`{__name__="match_associated_total"}`},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), filteredHistory)

	unfilteredDiff := oracleSuccessEnvelope(oraclePage(random.oracleSnapshotID(2), nil, nil, "changes", []any{
		oracleInitialDiffRow(associatedOwner, "100", map[string]string{"case": "associated"}),
		oracleInitialDiffRow(ownerOnlyOwner, "100", map[string]string{"case": "owner-only"}),
	}))
	recorder = oracleGet(t, handler, "/resources/diff", url.Values{"time": {"0.1"}})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), unfilteredDiff)

	filteredDiff := oracleSuccessEnvelope(oraclePage(random.oracleSnapshotID(3), nil, nil, "changes", []any{
		oracleInitialDiffRow(associatedOwner, "100", map[string]string{"case": "associated"}),
	}))
	recorder = oracleGet(t, handler, "/resources/diff", url.Values{
		"time": {"0.1"}, "match[]": {`{__name__="match_associated_total"}`},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), filteredDiff)
}

type oracleSampleState struct {
	Labels  string
	Samples []string
}

func oracleSelectSamples(t *testing.T, store *Storage, name string) []oracleSampleState {
	t.Helper()
	q, err := store.Querier(0, 2000)
	require.NoError(t, err)
	defer q.Close()
	ss := q.Select(t.Context(), true, nil, labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, name))
	var out []oracleSampleState
	for ss.Next() {
		series := oracleSampleState{Labels: ss.At().Labels().String()}
		it := ss.At().Iterator(nil)
		for it.Next() == chunkenc.ValFloat {
			timestamp, value := it.At()
			series.Samples = append(series.Samples,
				strconv.FormatInt(timestamp, 10)+"="+strconv.FormatFloat(value, 'g', -1, 64))
		}
		out = append(out, series)
	}
	require.NoError(t, ss.Err())
	return out
}

func TestMetadataHTTPMatchPhysicalAndSamples(t *testing.T) {
	f := newMatchFixture(t)
	beforeIn := oracleSelectSamples(t, f.store, "match_in_total")
	beforeOut := oracleSelectSamples(t, f.store, "match_out_sample_total")
	beforeLabel := oracleSelectSamples(t, f.store, "match_label_only_total")
	beforeMetadata := oracleSelectSamples(t, f.store, "match_metadata_total")
	beforeOther := oracleSelectSamples(t, f.store, "other_total")

	f.store.reads.reset()
	workBefore := f.store.reads.metadataWorkSnapshot()
	readsBefore := f.store.ReadStats()
	walBefore := contractWALSegments(t, f.store.dir)

	recorder := oracleGet(t, f.handler, "/resources", url.Values{
		"kind": {"resource"}, "match[]": {`{__name__=~"match_.*"}`}, "start": {"0.15"}, "end": {"0.35"},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	recorder = oracleGet(t, f.handler, "/resources/diff", url.Values{
		"time": {"0.2"}, "match[]": {`{__name__=~"match_.*"}`},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	readsAfter := f.store.ReadStats()
	require.Zero(t, readsAfter.SamplePayloadOpens, "match[] metadata reads open no sample payload")
	require.Zero(t, readsAfter.SegmentsRead, "match[] metadata reads read no sample segments")
	require.Zero(t, readsAfter.SampleRowsDecoded)
	require.Zero(t, readsAfter.PayloadBytesRead)
	require.Equal(t, readsBefore.CommitCount, readsAfter.CommitCount)
	workAfter := f.store.reads.metadataWorkSnapshot()
	require.Equal(t, workBefore.metadataTransactions, workAfter.metadataTransactions)
	require.Equal(t, workBefore.versionsInserted, workAfter.versionsInserted)
	require.Equal(t, workBefore.membershipsInserted, workAfter.membershipsInserted)
	require.Equal(t, walBefore, contractWALSegments(t, f.store.dir))
	responseWork := f.store.reads.metadataHTTPWorkSnapshot()
	require.Positive(t, responseWork.Rows)
	require.Positive(t, responseWork.Values)
	require.Positive(t, responseWork.Bytes)
	require.Positive(t, responseWork.SnapshotRows)

	require.Equal(t, beforeIn, oracleSelectSamples(t, f.store, "match_in_total"))
	require.Equal(t, beforeOut, oracleSelectSamples(t, f.store, "match_out_sample_total"))
	require.Equal(t, beforeLabel, oracleSelectSamples(t, f.store, "match_label_only_total"))
	require.Equal(t, beforeMetadata, oracleSelectSamples(t, f.store, "match_metadata_total"),
		"a metadata-only association fabricates no sample")
	require.Equal(t, beforeOther, oracleSelectSamples(t, f.store, "other_total"))
	require.Empty(t, beforeMetadata)
	require.NotEmpty(t, beforeLabel, "the label-only series retains its ordinary sample")

	for _, instance := range []string{"a-in-range", "b-out-sample", "c-metadata"} {
		state, ok, _ := f.store.MetadataAtLabels(f.series[instance], MetadataResource, 100)
		require.True(t, ok)
		require.Equal(t, instance, state.Attributes["case"])
	}
}

func oracleMatcherBinding(sets [][]string) []string {
	encoded := make([]string, 0, len(sets))
	for _, set := range sets {
		values := append([]string(nil), set...)
		slicesSortStrings(values)
		encoded = append(encoded, joinOracleStrings(values, "\x01"))
	}
	slicesSortStrings(encoded)
	return encoded
}

func joinOracleStrings(values []string, separator string) string {
	if len(values) == 0 {
		return ""
	}
	out := values[0]
	for _, value := range values[1:] {
		out += separator + value
	}
	return out
}

func TestMetadataHTTPMatchCommutativeBinding(t *testing.T) {
	f := newMatchFixture(t)
	snapshotID := f.random.oracleSnapshotID(0)
	setA := []string{"=\x00__name__\x00match_in_total", "=\x00zone\x00east"}
	setB := []string{"=\x00__name__\x00match_metadata_total", "=\x00instance\x00c-metadata"}
	matchers := oracleMatcherBinding([][]string{setA, setB})
	binding := oracleBindingJSON("history", "resource", 150, 350, matchers, nil, []string{"case", "missing"}, false)
	inRange := f.owners["a-in-range"]
	page1Token := f.oracleCursor(t, binding, oracleOwnerPosition{Kind: "resource", ID: inRange.ID}, snapshotID)
	inRangeOwner := f.resourceOwner("a-in-range")
	metadataOwner := f.resourceOwner("c-metadata")
	page1 := oracleSuccessEnvelope(oraclePage(snapshotID, page1Token, nil, "owners", []any{
		oracleOwnerRow(inRangeOwner,
			oracleAssertion(map[string]string{"case": "a-in-range"}, 100, nil, 100, 100, []string{"case", "missing"}, false, 0, 150)),
	}))
	query := url.Values{
		"kind": {"resource"}, "start": {"0.15"}, "end": {"0.35"}, "limit": {"1"},
		"match[]":    {`{__name__="match_in_total",zone="east"}`, `{__name__="match_metadata_total",instance="c-metadata"}`},
		"projection": {"case,missing"},
	}
	recorder := oracleGet(t, f.handler, "/resources", query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page1)

	// Reverse OR-set order, matcher order and projection order. The cursor binds
	// the commutative canonical query and must continue at the same place.
	page2 := oracleSuccessEnvelope(oraclePage(snapshotID, nil, nil, "owners", []any{
		oracleOwnerRow(metadataOwner,
			oracleAssertion(map[string]string{"case": "c-metadata"}, 100, nil, 100, 100, []string{"case", "missing"}, false, 0, 150)),
	}))
	query.Set("match[]", "")
	query["match[]"] = []string{
		`{instance="c-metadata",__name__="match_metadata_total"}`,
		`{zone="east",__name__="match_in_total"}`,
	}
	query.Set("projection", "missing,case")
	query.Set("next_token", page1Token)
	recorder = oracleGet(t, f.handler, "/resources", query)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), page2)

	// A changed matcher is a query mismatch with the full error envelope.
	query.Set("match[]", "")
	query["match[]"] = []string{`{__name__="match_metadata_total",instance="c-metadata"}`, `{__name__="match_in_total",zone="west"}`}
	recorder = oracleGet(t, f.handler, "/resources", query)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Len(t, body, 5)
	require.Nil(t, body["data"])
	require.Equal(t, "metadata_cursor_query_mismatch", body["details"].(map[string]any)["code"])
}

func TestMetadataHTTPMatchSubjectSemanticsAccepted(t *testing.T) {
	random := installOracleRandom(t, 0xA0)
	store, err := NewStorage(t.TempDir(), StorageOptions{DisableWAL: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	handler := NewMetadataHandler(store, nil)

	keys := map[string]OwnerKey{}
	refs := map[string]string{}
	series := map[string]labels.Labels{}
	for _, instance := range []string{"sample", "metadata", "owner-only", "label-only"} {
		key, ref := oracleResourceKey("contract", "shop", instance)
		keys[instance], refs[instance] = key, ref
		require.NoError(t, store.MetadataUpdate(Update{
			Owner: key, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"kind": instance}},
		}))
	}
	series["sample"] = labels.FromStrings(labels.MetricName, "match_sample_total", "instance", "sample", OTELResourceLabel, refs["sample"])
	require.NoError(t, store.AppendBatch(t.Context(), Batch{
		Series: []BatchSeries{{
			Labels: series["sample"], Timestamps: []int64{100}, StartTimestamps: []int64{0},
			Kinds: []SampleKind{SampleFloat}, Values: []float64{1}, Histograms: [][]byte{nil},
		}},
		Updates: []AssociationUpdate{{
			Labels: series["sample"], Update: Update{
				Owner: keys["sample"], EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"kind": "sample"}},
			},
		}},
	}))
	series["metadata"] = labels.FromStrings(labels.MetricName, "match_metadata_total", "instance", "metadata", OTELResourceLabel, refs["metadata"])
	metadataHTTPTestAssociate(t, store, series["metadata"], keys["metadata"], map[string]string{"kind": "metadata"}, 100)
	series["label-only"] = labels.FromStrings(labels.MetricName, "match_label_only_total", "instance", "label-only", OTELResourceLabel, refs["label-only"])
	require.NoError(t, store.AppendBatch(t.Context(), Batch{
		Series: []BatchSeries{{
			Labels: series["label-only"], Timestamps: []int64{100}, StartTimestamps: []int64{0},
			Kinds: []SampleKind{SampleFloat}, Values: []float64{1}, Histograms: [][]byte{nil},
		}},
	}))

	owner := func(instance string) map[string]any {
		return oracleResourceOwner("contract", "shop", instance, refs[instance])
	}
	assertion := func(instance string) map[string]any {
		return oracleAssertion(map[string]string{"kind": instance}, 100, nil, 100, 100, nil, false, 0, 0)
	}
	row := func(instance string) map[string]any {
		return oracleOwnerRow(owner(instance), assertion(instance))
	}
	initial := func(instance string) map[string]any {
		return oracleInitialDiffRow(owner(instance), "100", map[string]string{"kind": instance})
	}

	// Sample-backed and metadata-only associations match; owner-only and
	// label-only subjects do not match a series selector.
	filtered := oracleSuccessEnvelope(oraclePage(random.oracleSnapshotID(0), nil, nil, "owners", []any{
		row("metadata"), row("sample"),
	}))
	recorder := oracleGet(t, handler, "/resources", url.Values{
		"kind": {"resource"}, "match[]": {`{__name__=~"match_.+"}`}, "start": {"0"}, "end": {"1"},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), filtered)

	// Without match[], the owner-only publication is included.
	unfiltered := oracleSuccessEnvelope(oraclePage(random.oracleSnapshotID(1), nil, nil, "owners", []any{
		row("label-only"), row("metadata"), row("owner-only"), row("sample"),
	}))
	recorder = oracleGet(t, handler, "/resources", url.Values{"kind": {"resource"}, "start": {"0"}, "end": {"1"}})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), unfiltered)

	filteredDiff := oracleSuccessEnvelope(oraclePage(random.oracleSnapshotID(2), nil, nil, "changes", []any{
		initial("metadata"), initial("sample"),
	}))
	recorder = oracleGet(t, handler, "/resources/diff", url.Values{
		"time": {"0.1"}, "match[]": {`{__name__=~"match_.+"}`},
	})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), filteredDiff)

	unfilteredDiff := oracleSuccessEnvelope(oraclePage(random.oracleSnapshotID(3), nil, nil, "changes", []any{
		initial("label-only"), initial("metadata"), initial("owner-only"), initial("sample"),
	}))
	recorder = oracleGet(t, handler, "/resources/diff", url.Values{"time": {"0.1"}})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	oracleRequireJSON(t, recorder.Body.Bytes(), unfilteredDiff)
}

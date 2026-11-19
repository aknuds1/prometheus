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

// Decision 7 contract tests. Expected results are authored from the accepted
// logical contract, not captured from the incumbent. Typed non-string storage,
// clears, tombstones and termination remain target-only obligations because no
// durable typed/tombstone format is authorized.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
)

func metadataHTTPTestStore(t *testing.T, opts StorageOptions) (*Storage, *MetadataHandler) {
	t.Helper()
	store, err := NewStorage(t.TempDir(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store, NewMetadataHandler(store, slog.New(slog.DiscardHandler))
}

func metadataHTTPTestResourceOwner(instance string) OwnerKey {
	return ResourceOwner(map[string]string{
		"service.name": "contract", "service.namespace": "shop", "service.instance.id": instance,
	})
}

func metadataHTTPTestAssociate(t *testing.T, store *Storage, series labels.Labels, owner OwnerKey, content map[string]string, from int64) {
	t.Helper()
	_, err := store.MetadataAssociate(series, Update{
		Owner: owner, EffectiveFrom: from, Content: Content{Attributes: content},
	})
	require.NoError(t, err)
}

func metadataHTTPTestSeries(name, instance string, owner OwnerKey) labels.Labels {
	return labels.FromStrings(
		labels.MetricName, name, "instance", instance, OTELResourceLabel, owner.Ref(),
	)
}

func metadataHTTPGet(t *testing.T, handler *MetadataHandler, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	return recorder
}

func metadataHTTPDelete(t *testing.T, handler *MetadataHandler, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, path, http.NoBody))
	return recorder
}

func metadataHTTPRequireError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	require.Equal(t, status, recorder.Code, recorder.Body.String())
	var body struct {
		Status    string         `json:"status"`
		ErrorType string         `json:"errorType"`
		Data      any            `json:"data"`
		Details   map[string]any `json:"details"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "error", body.Status)
	require.Nil(t, body.Data, "errors expose no page prefix")
	require.Equal(t, code, body.Details["code"])
	wantType := "bad_data"
	switch status {
	case 410, 501:
		wantType = "unavailable"
	case 500:
		wantType = "internal"
	}
	require.Equal(t, wantType, body.ErrorType)
}

func metadataHTTPReadOwnerPage(t *testing.T, recorder *httptest.ResponseRecorder) metadataHTTPOwnerPage {
	t.Helper()
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var envelope struct {
		Status string                `json:"status"`
		Data   metadataHTTPOwnerPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Equal(t, "success", envelope.Status)
	return envelope.Data
}

func TestMetadataTypedValueRoundTrip(t *testing.T) {
	falseValue := metadataPresentBoolean(false)
	empty := metadataPresentString("")
	maxInt := metadataPresentInteger(math.MaxInt64)
	minInt := metadataPresentInteger(math.MinInt64)
	negativeZero := metadataPresentDouble(0x8000000000000000)
	nanPayload := metadataPresentDouble(0x7ff8000000000001)
	infinity := metadataPresentDouble(0x7ff0000000000000)
	denormal := metadataPresentDouble(0x0000000000000001)
	nestedMap, err := metadataPresentMap([]metadataTypedMapEntry{
		{Key: "z", Value: metadataPresentArray([]metadataTypedValue{empty, maxInt})},
		{Key: "a", Value: metadataNullValue()},
	})
	require.NoError(t, err)
	ambiguous := metadataAmbiguousValue([]metadataTypedValue{
		metadataPresentString("b"), metadataPresentString("a"), metadataPresentString("a"),
	})
	cases := []struct {
		name  string
		value metadataTypedValue
		want  string
	}{
		{"present-empty-string", empty, `{"state":"present","type":"string","value":""}`},
		{"present-boolean-false", falseValue, `{"state":"present","type":"boolean","value":false}`},
		{"present-max-integer", maxInt, `{"state":"present","type":"integer","decimal":"9223372036854775807"}`},
		{"present-min-integer", minInt, `{"state":"present","type":"integer","decimal":"-9223372036854775808"}`},
		{"present-signed-zero", negativeZero, `{"state":"present","type":"double","bits":"0x8000000000000000"}`},
		{"present-nan-payload", nanPayload, `{"state":"present","type":"double","bits":"0x7ff8000000000001"}`},
		{"present-infinity", infinity, `{"state":"present","type":"double","bits":"0x7ff0000000000000"}`},
		{"present-denormal", denormal, `{"state":"present","type":"double","bits":"0x0000000000000001"}`},
		{"present-empty-array", metadataPresentArray(nil), `{"state":"present","type":"array","items":[]}`},
		{"present-nested-map", nestedMap, `{"state":"present","type":"map","entries":[{"key":"a","value":{"state":"null"}},{"key":"z","value":{"state":"present","type":"array","items":[{"state":"present","type":"string","value":""},{"state":"present","type":"integer","decimal":"9223372036854775807"}]}}]}`},
		{"null", metadataNullValue(), `{"state":"null"}`},
		{"absent", metadataAbsentValue(), `{"state":"absent"}`},
		{"cleared", metadataClearedValue(), `{"state":"cleared"}`},
		{"unknown", metadataUnknownValue("partial_source"), `{"state":"unknown","reason":"partial_source"}`},
		{"ambiguous-dedup", ambiguous, `{"state":"ambiguous","assertions":[{"state":"present","type":"string","value":"a"},{"state":"present","type":"string","value":"b"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.value)
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(encoded))
			var decoded metadataTypedValue
			require.NoError(t, json.Unmarshal(encoded, &decoded))
			reencoded, err := json.Marshal(decoded)
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(reencoded))
			require.Equal(t, metadataTypedValueCanonical(tc.value), metadataTypedValueCanonical(decoded))
		})
	}
}

func TestMetadataTypedValueValidation(t *testing.T) {
	invalidJSON := []struct {
		name string
		raw  string
	}{
		{"missing-state", `{}`},
		{"unknown-state", `{"state":"maybe"}`},
		{"unknown-type", `{"state":"present","type":"bytes","value":"x"}`},
		{"string-missing-value", `{"state":"present","type":"string"}`},
		{"string-null-value", `{"state":"present","type":"string","value":null}`},
		{"string-extra-decimal", `{"state":"present","type":"string","value":"x","decimal":"1"}`},
		{"boolean-nonboolean", `{"state":"present","type":"boolean","value":1}`},
		{"integer-leading-zero", `{"state":"present","type":"integer","decimal":"01"}`},
		{"integer-negative-zero", `{"state":"present","type":"integer","decimal":"-0"}`},
		{"integer-plus", `{"state":"present","type":"integer","decimal":"+1"}`},
		{"integer-overflow", `{"state":"present","type":"integer","decimal":"9223372036854775808"}`},
		{"integer-nonstring", `{"state":"present","type":"integer","decimal":1}`},
		{"double-uppercase", `{"state":"present","type":"double","bits":"0X8000000000000000"}`},
		{"double-short", `{"state":"present","type":"double","bits":"0x800000000000000"}`},
		{"double-long", `{"state":"present","type":"double","bits":"0x80000000000000000"}`},
		{"double-missing", `{"state":"present","type":"double"}`},
		{"array-missing-items", `{"state":"present","type":"array"}`},
		{"array-null-items", `{"state":"present","type":"array","items":null}`},
		{"array-bad-nested", `{"state":"present","type":"array","items":[{"state":"present","type":"integer","decimal":"01"}]}`},
		{"map-missing-entries", `{"state":"present","type":"map"}`},
		{"map-duplicate-key", `{"state":"present","type":"map","entries":[{"key":"a","value":{"state":"absent"}},{"key":"a","value":{"state":"null"}}]}`},
		{"map-entry-missing-value", `{"state":"present","type":"map","entries":[{"key":"a"}]}`},
		{"null-with-type", `{"state":"null","type":"string"}`},
		{"absent-with-value", `{"state":"absent","value":"x"}`},
		{"unknown-missing-reason", `{"state":"unknown"}`},
		{"unknown-empty-reason", `{"state":"unknown","reason":""}`},
		{"unknown-with-type", `{"state":"unknown","reason":"x","type":"string"}`},
		{"ambiguous-empty", `{"state":"ambiguous","assertions":[]}`},
		{"ambiguous-equal", `{"state":"ambiguous","assertions":[{"state":"absent"},{"state":"absent"}]}`},
		{"ambiguous-with-value", `{"state":"ambiguous","assertions":[{"state":"absent"},{"state":"null"}],"value":"x"}`},
		{"unknown-field", `{"state":"absent","extra":1}`},
		{"trailing-data", `{"state":"absent"}{"state":"null"}`},
	}
	for _, tc := range invalidJSON {
		t.Run(tc.name, func(t *testing.T) {
			var value metadataTypedValue
			require.Error(t, json.Unmarshal([]byte(tc.raw), &value), tc.raw)
		})
	}

	present := metadataPresentString("x")
	invalidValues := []struct {
		name  string
		value metadataTypedValue
	}{
		{"ambiguous-empty", metadataAmbiguousValue(nil)},
		{"ambiguous-equal", metadataAmbiguousValue([]metadataTypedValue{metadataAbsentValue(), metadataAbsentValue()})},
		{"string-nil", metadataTypedValue{state: "present", typ: "string"}},
		{"string-extra", metadataTypedValue{state: "present", typ: "string", str: present.str, decimal: present.str}},
		{"integer-bad", metadataTypedValue{state: "present", typ: "integer", decimal: metadataString("01")}},
		{"double-bad", metadataTypedValue{state: "present", typ: "double", bits: metadataString("0xABC")}},
		{"null-payload", metadataTypedValue{state: "null", str: present.str}},
		{"unknown-no-reason", metadataTypedValue{state: "unknown"}},
		{"map-duplicate", metadataTypedValue{state: "present", typ: "map", entries: []metadataTypedMapEntry{
			{Key: "a", Value: metadataAbsentValue()}, {Key: "a", Value: metadataNullValue()},
		}}},
	}
	for _, tc := range invalidValues {
		t.Run("marshal-"+tc.name, func(t *testing.T) {
			_, err := json.Marshal(tc.value)
			require.Error(t, err)
			require.NotPanics(t, func() { _ = metadataTypedValueCanonical(tc.value) })
			require.NotPanics(t, func() { _ = metadataTypedValueDisplayKey(tc.value) })
		})
	}
}

func metadataString(value string) *string { return &value }

func TestMetadataTypedValueCanonicalization(t *testing.T) {
	left, err := metadataPresentMap([]metadataTypedMapEntry{
		{Key: "b", Value: metadataPresentInteger(2)},
		{Key: "a", Value: metadataPresentString("x")},
	})
	require.NoError(t, err)
	right, err := metadataPresentMap([]metadataTypedMapEntry{
		{Key: "a", Value: metadataPresentString("x")},
		{Key: "b", Value: metadataPresentInteger(2)},
	})
	require.NoError(t, err)
	require.Equal(t, metadataTypedValueCanonical(left), metadataTypedValueCanonical(right),
		"map equality ignores construction order")

	// JSON decoding order must not affect canonicalization either.
	rawLeft := `{"state":"present","type":"map","entries":[{"key":"a","value":{"state":"present","type":"string","value":"x"}},{"key":"b","value":{"state":"present","type":"integer","decimal":"2"}}]}`
	rawRight := `{"state":"present","type":"map","entries":[{"key":"b","value":{"state":"present","type":"integer","decimal":"2"}},{"key":"a","value":{"state":"present","type":"string","value":"x"}}]}`
	var decodedLeft, decodedRight metadataTypedValue
	require.NoError(t, json.Unmarshal([]byte(rawLeft), &decodedLeft))
	require.NoError(t, json.Unmarshal([]byte(rawRight), &decodedRight))
	require.Equal(t, metadataTypedValueCanonical(decodedLeft), metadataTypedValueCanonical(decodedRight))
	encodedLeft, err := json.Marshal(decodedLeft)
	require.NoError(t, err)
	encodedRight, err := json.Marshal(decodedRight)
	require.NoError(t, err)
	require.JSONEq(t, string(encodedLeft), string(encodedRight))

	first := metadataPresentArray([]metadataTypedValue{metadataPresentString("a"), metadataPresentString("b")})
	second := metadataPresentArray([]metadataTypedValue{metadataPresentString("b"), metadataPresentString("a")})
	require.NotEqual(t, metadataTypedValueCanonical(first), metadataTypedValueCanonical(second),
		"arrays preserve order")

	require.NotEqual(t,
		metadataTypedValueCanonical(metadataPresentInteger(1)),
		metadataTypedValueCanonical(metadataPresentDouble(0x3ff0000000000000)),
		"integer and double remain distinct even for the same numeric value")
	require.NotEqual(t,
		metadataTypedValueCanonical(metadataPresentString("1")),
		metadataTypedValueCanonical(metadataPresentInteger(1)),
		"string and integer remain distinct")
	firstNaN := metadataTypedValueCanonical(metadataPresentDouble(0x7ff8000000000001))
	secondNaN := metadataTypedValueCanonical(metadataPresentDouble(0x7ff8000000000001))
	require.Equal(t, firstNaN, secondNaN, "exact NaN payloads compare by bits")
	require.NotEqual(t,
		metadataTypedValueCanonical(metadataPresentDouble(0x7ff8000000000001)),
		metadataTypedValueCanonical(metadataPresentDouble(0x7ff8000000000002)),
		"different NaN payloads remain distinct")
	require.NotEqual(t,
		metadataTypedValueCanonical(metadataPresentDouble(0x0000000000000000)),
		metadataTypedValueCanonical(metadataPresentDouble(0x8000000000000000)),
		"signed zero remains distinct")
}

func TestMetadataTypedHTTPValues(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	owner := metadataHTTPTestResourceOwner("typed")
	series := metadataHTTPTestSeries("typed_total", "typed", owner)
	metadataHTTPTestAssociate(t, store, series, owner, map[string]string{"empty": "", "other": "x"}, 1000)
	recorder := metadataHTTPGet(t, handler, "/resources/series?kind=resource&resource.attr=other:x&start=0&end=2&projection=empty,missing,other")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var envelope struct {
		Data metadataHTTPSeriesPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Series, 1)
	entries := envelope.Data.Series[0].Versions[0].Content.Entries
	require.Len(t, entries, 3)
	require.Equal(t, "present", metadataTestEntry(t, entries, "empty").state)
	require.Empty(t, *metadataTestEntry(t, entries, "empty").str)
	require.Equal(t, "absent", metadataTestEntry(t, entries, "missing").state)
	require.Equal(t, "present", metadataTestEntry(t, entries, "other").state)
	for _, entry := range entries {
		require.NotEqual(t, "cleared", entry.Value.state, "no clear evidence exists")
	}
}

func TestMetadataHTTPTimestamps(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	owner := metadataHTTPTestResourceOwner("time")
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: owner, EffectiveFrom: 1500, Content: Content{Attributes: map[string]string{"k": "v"}},
	}))
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: metadataHTTPTestResourceOwner("negative"), EffectiveFrom: -500,
		Content: Content{Attributes: map[string]string{"k": "v"}},
	}))
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: metadataHTTPTestResourceOwner("negative"), EffectiveFrom: 0,
		Content: Content{Attributes: map[string]string{"k": "v2"}},
	}))
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: metadataHTTPTestResourceOwner("negative"), EffectiveFrom: 100,
		Content: Content{Attributes: map[string]string{"k": "v3"}},
	}))

	recorder := metadataHTTPGet(t, handler, "/resources?start=1.5&end=1.5&projection=k")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	body := recorder.Body.String()
	require.Contains(t, body, `"from_ms":"1500"`, "response timestamps are decimal strings")
	require.NotContains(t, body, `"from_ms":1500`)
	page := metadataHTTPReadOwnerPage(t, recorder)
	var timed metadataHTTPOwnerRow
	for _, ownerRow := range page.Owners {
		if *metadataTestIdentity(t, ownerRow.Owner.Identity, "service.instance.id").str == "time" {
			timed = ownerRow
		}
	}
	require.NotEmpty(t, timed.Owner.Ref, "start and end remain inclusive")
	require.Equal(t, "1500", timed.Versions[0].Validity.FromMS)
	require.Equal(t, "1500", *timed.Versions[0].Observation.FirstMS)

	recorder = metadataHTTPGet(t, handler, "/resources?start=-1&end=0")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), `"from_ms":"-500"`)

	for _, raw := range []string{"NaN", "Inf", "-Inf", "abc", "1/2", " 1", "0x10", "1_000"} {
		recorder = metadataHTTPGet(t, handler, "/resources?start="+url.QueryEscape(raw))
		metadataHTTPRequireError(t, recorder, 400, "metadata_time_invalid")
	}
	for _, raw := range []string{"1e999", "1e17", "0.0001", "1.0001"} {
		recorder = metadataHTTPGet(t, handler, "/resources?start="+url.QueryEscape(raw))
		metadataHTTPRequireError(t, recorder, 400, "metadata_timestamp_out_of_range")
	}
}

func TestMetadataHTTPErrorClasses(t *testing.T) {
	_, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	cases := []struct {
		name   string
		path   string
		status int
		code   string
	}{
		{"kind-empty", "/resources?kind=", 400, "metadata_kind_unknown"},
		{"kind-unknown", "/resources?kind=bogus", 400, "metadata_kind_unknown"},
		{"range-invalid", "/resources?start=2&end=1", 400, "metadata_range_invalid"},
		{"matcher-invalid", "/resources?match%5B%5D=%7Bbad", 400, "metadata_matcher_invalid"},
		{"projection-wildcard", "/resources?projection=host.*", 400, "metadata_projection_invalid"},
		{"limit-zero", "/resources?limit=0", 400, "metadata_limit_invalid"},
		{"limit-negative", "/resources?limit=-1", 400, "metadata_limit_invalid"},
		{"limit-excessive", "/resources?limit=10001", 400, "metadata_limit_invalid"},
		{"limit-noninteger", "/resources?limit=abc", 400, "metadata_limit_invalid"},
		{"discovery-match", "/resources?format=attributes&match%5B%5D=%7B__name__%3D%22x%22%7D", 400, "metadata_parameter_invalid"},
		{"discovery-latest", "/resources?format=attributes&latest=true", 400, "metadata_parameter_invalid"},
		{"history-attribute", "/resources?resource.attr=k:v", 400, "metadata_parameter_invalid"},
		{"history-resource-attr", "/resources?start=0&resource.attr=k:v", 400, "metadata_parameter_invalid"},
		{"diff-start", "/resources/diff?time=1&start=0", 400, "metadata_parameter_invalid"},
		{"diff-missing-time", "/resources/diff", 400, "metadata_time_invalid"},
		{"series-format", "/resources/series?format=attributes&resource.attr=k:v", 400, "metadata_parameter_invalid"},
		{"unknown-parameter", "/resources?timeout=1", 400, "metadata_parameter_invalid"},
		{"cursor-malformed", "/resources?next_token=%21%21%21", 400, "metadata_cursor_malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metadataHTTPRequireError(t, metadataHTTPGet(t, handler, tc.path), tc.status, tc.code)
		})
	}
}

func metadataHTTPTestSignCursor(t *testing.T, store *Storage, signed metadataCursorSigned) string {
	t.Helper()
	rawSigned, err := json.Marshal(signed)
	require.NoError(t, err)
	mac := hmac.New(sha256.New, store.metadataSnapshots.key)
	_, _ = mac.Write(rawSigned)
	envelope := metadataCursorEnvelope{
		metadataCursorSigned: signed, Signature: hex.EncodeToString(mac.Sum(nil)),
	}
	rawEnvelope, err := json.Marshal(envelope)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(rawEnvelope)
}

func metadataHTTPTestDecodeCursor(t *testing.T, token string) metadataCursorSigned {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(token)
	require.NoError(t, err)
	var envelope metadataCursorEnvelope
	require.NoError(t, json.Unmarshal(raw, &envelope))
	return envelope.metadataCursorSigned
}

func metadataHTTPTestPinOwners(t *testing.T, store *Storage, count int) {
	t.Helper()
	for i := 1; i <= count; i++ {
		require.NoError(t, store.MetadataUpdate(Update{
			Owner: metadataHTTPTestResourceOwner("snapshot-" + itoa(i)), EffectiveFrom: 100,
			Content: Content{Attributes: map[string]string{"seq": "old-" + itoa(i)}},
		}))
	}
}

func TestMetadataSnapshotPagination(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	metadataHTTPTestPinOwners(t, store, 5)

	first := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t, handler, "/resources?limit=2&start=0&end=1"))
	require.Len(t, first.Owners, 2)
	require.NotNil(t, first.NextToken)
	require.NotEmpty(t, first.SnapshotID)
	require.NotNil(t, first.Annotations)
	oldSeq := func(row metadataHTTPOwnerRow) string {
		return *metadataTestEntry(t, row.Versions[0].Content.Entries, "seq").str
	}
	require.Equal(t, "old-1", oldSeq(first.Owners[0]))

	// Mutate the live store after the first page: a new owner, a changed owner and
	// an appended version. None may leak into the pinned continuation.
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: metadataHTTPTestResourceOwner("snapshot-6"), EffectiveFrom: 100,
		Content: Content{Attributes: map[string]string{"seq": "new-6"}},
	}))
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: metadataHTTPTestResourceOwner("snapshot-1"), EffectiveFrom: 200,
		Content: Content{Attributes: map[string]string{"seq": "changed-1"}},
	}))
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: metadataHTTPTestResourceOwner("snapshot-3"), EffectiveFrom: 200,
		Content: Content{Attributes: map[string]string{"seq": "changed-3"}},
	}))

	second := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t,
		handler, "/resources?limit=2&start=0&end=1&next_token="+url.QueryEscape(*first.NextToken)))
	require.Len(t, second.Owners, 2)
	require.Equal(t, "old-3", oldSeq(second.Owners[0]), "the pinned snapshot keeps pre-mutation content")
	require.Equal(t, "old-4", oldSeq(second.Owners[1]))
	require.Equal(t, first.SnapshotID, second.SnapshotID)
	require.NotNil(t, second.NextToken)

	third := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t,
		handler, "/resources?limit=2&start=0&end=1&next_token="+url.QueryEscape(*second.NextToken)))
	require.Len(t, third.Owners, 1)
	require.Equal(t, "old-5", oldSeq(third.Owners[0]))
	require.Nil(t, third.NextToken, "the final page is terminal")

	// A changed limit is allowed; the binding excludes it.
	widened := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t,
		handler, "/resources?limit=10&start=0&end=1&next_token="+url.QueryEscape(*first.NextToken)))
	require.Len(t, widened.Owners, 3)
	require.Nil(t, widened.NextToken)
	require.NotContains(t, metadataHTTPOwnerIDs(widened), metadataHTTPTestResourceOwner("snapshot-6").Ref())

	// A changed semantic parameter is a query-mismatch error.
	metadataHTTPRequireError(t, metadataHTTPGet(t,
		handler, "/resources?limit=10&start=0&end=2&next_token="+url.QueryEscape(*first.NextToken)),
		400, "metadata_cursor_query_mismatch")

	// Corrupt integrity: re-encode a modified payload without re-signing.
	rawToken, err := base64.RawURLEncoding.DecodeString(*first.NextToken)
	require.NoError(t, err)
	var envelope metadataCursorEnvelope
	require.NoError(t, json.Unmarshal(rawToken, &envelope))
	envelope.Position = json.RawMessage(`{"kind":"resource","id":"tampered"}`)
	tamperedRaw, err := json.Marshal(envelope)
	require.NoError(t, err)
	tampered := base64.RawURLEncoding.EncodeToString(tamperedRaw)
	metadataHTTPRequireError(t, metadataHTTPGet(t,
		handler, "/resources?limit=2&start=0&end=1&next_token="+url.QueryEscape(tampered)),
		400, "metadata_cursor_corrupt")

	// A correctly signed cursor without a complete after-position is corrupt.
	signed := metadataHTTPTestDecodeCursor(t, *first.NextToken)
	signed.Position = json.RawMessage(`{}`)
	emptyPosition := metadataHTTPTestSignCursor(t, store, signed)
	metadataHTTPRequireError(t, metadataHTTPGet(t,
		handler, "/resources?limit=2&start=0&end=1&next_token="+url.QueryEscape(emptyPosition)),
		400, "metadata_cursor_corrupt")

	// Unsupported cursor version.
	signed = metadataHTTPTestDecodeCursor(t, *first.NextToken)
	signed.Version = 2
	versionToken := metadataHTTPTestSignCursor(t, store, signed)
	metadataHTTPRequireError(t, metadataHTTPGet(t,
		handler, "/resources?limit=2&start=0&end=1&next_token="+url.QueryEscape(versionToken)),
		400, "metadata_cursor_version_unsupported")

	// Unknown snapshot id with a valid signature and binding.
	signed = metadataHTTPTestDecodeCursor(t, *first.NextToken)
	signed.Snapshot = "unknown-snapshot"
	unknownToken := metadataHTTPTestSignCursor(t, store, signed)
	metadataHTTPRequireError(t, metadataHTTPGet(t,
		handler, "/resources?limit=2&start=0&end=1&next_token="+url.QueryEscape(unknownToken)),
		404, "metadata_snapshot_unknown")
}

func metadataHTTPOwnerIDs(page metadataHTTPOwnerPage) []string {
	ids := make([]string, 0, len(page.Owners))
	for _, owner := range page.Owners {
		ids = append(ids, owner.Owner.Ref)
	}
	return ids
}

func TestMetadataSnapshotTerminalPages(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	metadataHTTPTestPinOwners(t, store, 4)

	first := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t, handler, "/resources?limit=2&start=0&end=1"))
	require.NotNil(t, first.NextToken)
	final := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t,
		handler, "/resources?limit=2&start=0&end=1&next_token="+url.QueryEscape(*first.NextToken)))
	require.Len(t, final.Owners, 2, "an exact-full terminal page reports its full page")
	require.Nil(t, final.NextToken, "an exact-full page with no further object carries no cursor")

	// A cursor positioned past every object yields a terminal empty page.
	signed := metadataHTTPTestDecodeCursor(t, *first.NextToken)
	signed.Position = json.RawMessage(`{"kind":"resource","id":"zzzz"}`)
	pastToken := metadataHTTPTestSignCursor(t, store, signed)
	empty := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t,
		handler, "/resources?limit=2&start=0&end=1&next_token="+url.QueryEscape(pastToken)))
	require.Empty(t, empty.Owners)
	require.NotNil(t, empty.Owners, "an empty page reports a list")
	require.Nil(t, empty.NextToken)
}

func TestMetadataSnapshotLifecycle(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	metadataHTTPTestPinOwners(t, store, 3)
	first := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t, handler, "/resources?limit=1&start=0&end=1"))
	require.NotNil(t, first.NextToken)
	token := *first.NextToken
	continuation := "/resources?limit=1&start=0&end=1&next_token=" + url.QueryEscape(token)

	// Explicit cancellation is idempotent and returns 204 with no body.
	recorder := metadataHTTPDelete(t, handler, "/resources/snapshots/"+first.SnapshotID)
	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Empty(t, recorder.Body.String())
	recorder = metadataHTTPDelete(t, handler, "/resources/snapshots/"+first.SnapshotID)
	require.Equal(t, http.StatusNoContent, recorder.Code)
	metadataHTTPRequireError(t, metadataHTTPGet(t, handler, continuation), 410, "metadata_snapshot_canceled")
	metadataHTTPRequireError(t, metadataHTTPDelete(t, handler, "/resources/snapshots/unknown"), 404, "metadata_snapshot_unknown")

	// Expiry and forced reclamation retain their own classes.
	for _, tc := range []struct {
		name string
		set  func(string)
		code string
	}{
		{"expired", func(id string) { store.expireMetadataSnapshotForTesting(id) }, "metadata_snapshot_expired"},
		{"reclaimed", func(id string) { store.reclaimMetadataSnapshot(id, "forced") }, "metadata_snapshot_reclaimed"},
		{"state-unavailable", func(id string) { store.releaseMetadataSnapshotForTesting(id) }, "metadata_snapshot_state_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t, handler, "/resources?limit=1&start=0&end=1"))
			require.NotNil(t, page.NextToken)
			tc.set(page.SnapshotID)
			metadataHTTPRequireError(t, metadataHTTPGet(t,
				handler, "/resources?limit=1&start=0&end=1&next_token="+url.QueryEscape(*page.NextToken)),
				410, tc.code)
		})
	}

	// A token from a previous store incarnation is reclaimed as store_restarted.
	dir := t.TempDir()
	oldStore, err := NewStorage(dir, StorageOptions{DisableWAL: true})
	require.NoError(t, err)
	oldHandler := NewMetadataHandler(oldStore, slog.New(slog.DiscardHandler))
	require.NoError(t, oldStore.MetadataUpdate(Update{
		Owner: metadataHTTPTestResourceOwner("restart"), EffectiveFrom: 100,
		Content: Content{Attributes: map[string]string{"k": "v"}},
	}))
	// Add a second owner so the first page carries a cursor.
	require.NoError(t, oldStore.MetadataUpdate(Update{
		Owner: metadataHTTPTestResourceOwner("restart-2"), EffectiveFrom: 100,
		Content: Content{Attributes: map[string]string{"k": "v"}},
	}))
	oldPage := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t, oldHandler, "/resources?limit=1&start=0&end=1"))
	require.NotNil(t, oldPage.NextToken)
	oldToken := *oldPage.NextToken
	require.NoError(t, oldStore.Close())

	newStore, err := NewStorage(dir, StorageOptions{DisableWAL: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, newStore.Close()) })
	newHandler := NewMetadataHandler(newStore, slog.New(slog.DiscardHandler))
	metadataHTTPRequireError(t, metadataHTTPGet(t, newHandler,
		"/resources?limit=1&start=0&end=1&next_token="+url.QueryEscape(oldToken)),
		410, "metadata_snapshot_reclaimed")
}

func TestMetadataHTTPDiffStates(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	owner := metadataHTTPTestResourceOwner("diff")
	require.NoError(t, store.MetadataUpdate(Update{Owner: owner, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"a": "1"}}}))
	require.NoError(t, store.MetadataUpdate(Update{Owner: owner, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"a": "2", "b": ""}}}))
	require.NoError(t, store.MetadataUpdate(Update{Owner: owner, EffectiveFrom: 300, Content: Content{Attributes: map[string]string{"a": "2"}}}))

	var page metadataHTTPDiffPage
	recorder := metadataHTTPGet(t, handler, "/resources/diff?time=0.2")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var envelope struct {
		Data metadataHTTPDiffPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	page = envelope.Data
	require.Len(t, page.Changes, 1)
	change := page.Changes[0]
	require.Equal(t, "present", change.Before.State)
	require.Equal(t, "present", change.After.State)
	require.Equal(t, "present", changeField(t, change, "a").Before.state)
	require.Equal(t, "1", *changeField(t, change, "a").Before.str)
	require.Equal(t, "2", *changeField(t, change, "a").After.str)
	require.Equal(t, "absent", changeField(t, change, "b").Before.state, "initial appearance has an explicit absent before state")
	require.Equal(t, "present", changeField(t, change, "b").After.state)
	require.Empty(t, *changeField(t, change, "b").After.str, "present empty is not absent")
	require.NotContains(t, recorder.Body.String(), "->", "diff fields never use old-> strings")

	recorder = metadataHTTPGet(t, handler, "/resources/diff?time=0.3&projection=b")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	change = envelope.Data.Changes[0]
	require.Equal(t, "present", changeField(t, change, "b").Before.state)
	require.Equal(t, "absent", changeField(t, change, "b").After.state, "complete omission removes the key")

	// Projection does not suppress a logical diff event even when it hides every
	// changed key.
	recorder = metadataHTTPGet(t, handler, "/resources/diff?time=0.2&projection=missing")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Changes, 1)
	require.Equal(t, "absent", changeField(t, envelope.Data.Changes[0], "missing").Before.state)
	require.Equal(t, "absent", changeField(t, envelope.Data.Changes[0], "missing").After.state)

	// An initial appearance without truncation uses absent, not unknown.
	initialStore, initialHandler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	initialOwner := metadataHTTPTestResourceOwner("initial")
	require.NoError(t, initialStore.MetadataUpdate(Update{Owner: initialOwner, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"a": "1"}}}))
	recorder = metadataHTTPGet(t, initialHandler, "/resources/diff?time=0.1")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Equal(t, "absent", envelope.Data.Changes[0].Before.State)
	require.Equal(t, "covered", envelope.Data.Changes[0].Coverage.State)

	// A discarded predecessor is unknown/lost_predecessor, never absent.
	truncatedStore, truncatedHandler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true, MetadataVersionCap: 2})
	truncatedOwner := metadataHTTPTestResourceOwner("lost")
	require.NoError(t, truncatedStore.MetadataUpdate(Update{Owner: truncatedOwner, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"a": "1"}}}))
	require.NoError(t, truncatedStore.MetadataUpdate(Update{Owner: truncatedOwner, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"a": "2"}}}))
	require.NoError(t, truncatedStore.MetadataUpdate(Update{Owner: truncatedOwner, EffectiveFrom: 300, Content: Content{Attributes: map[string]string{"a": "3"}}}))
	recorder = metadataHTTPGet(t, truncatedHandler, "/resources/diff?time=0.2")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Changes, 1)
	change = envelope.Data.Changes[0]
	require.Equal(t, "unknown", change.Before.State)
	require.Equal(t, "lost_predecessor", change.Before.Reason)
	require.Equal(t, "truncated", change.Coverage.State)
	require.Equal(t, "unknown", changeField(t, change, "a").Before.state)
	require.Equal(t, "lost_predecessor", *changeField(t, change, "a").Before.reason)
	require.NotNil(t, envelope.Data.Annotations)
	require.Len(t, envelope.Data.Annotations, 1)
	require.Equal(t, "truncated", envelope.Data.Annotations[0]["state"])
}

func TestMetadataHTTPAssociationStates(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	owner := metadataHTTPTestResourceOwner("assoc")
	series := metadataHTTPTestSeries("assoc_total", "assoc", owner)
	metadataHTTPTestAssociate(t, store, series, owner, map[string]string{"host.name": ""}, 100)

	var page metadataHTTPSeriesPage
	recorder := metadataHTTPGet(t, handler, "/resources/series?kind=source&match%5B%5D=%7B__name__%3D%22assoc_total%22%7D&start=0&end=1")
	var envelope struct {
		Data metadataHTTPSeriesPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	page = envelope.Data
	require.Len(t, page.Series, 1)
	require.Equal(t, "unassociated", page.Series[0].Association.State)
	require.Nil(t, page.Series[0].Owner)
	require.Empty(t, page.Series[0].Versions)
	require.Empty(t, page.Annotations, "ordinary unassociated is not an uncertainty")

	// A positive empty-string matcher selects only a present empty value. An
	// absent key never equals the empty string.
	absentOwner := metadataHTTPTestResourceOwner("absent")
	absentSeries := metadataHTTPTestSeries("absent_total", "absent", absentOwner)
	metadataHTTPTestAssociate(t, store, absentSeries, absentOwner, map[string]string{"region": "east"}, 100)
	recorder = metadataHTTPGet(t, handler, "/resources/series?resource.attr=host.name:&start=0&end=1")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Series, 1)
	require.Equal(t, "assoc_total", envelope.Data.Series[0].Series[0].Value,
		"only the owner with a present empty host.name matches")
	recorder = metadataHTTPGet(t, handler, "/resources/series?resource.attr=missing:&start=0&end=1")
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Empty(t, envelope.Data.Series, "a key absent from content and identity never matches empty")
	// Identifying attributes are present applicable strings.
	recorder = metadataHTTPGet(t, handler, "/resources/series?resource.attr=service.name:contract&start=0&end=1")
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Series, 2)
}

func TestMetadataHTTPDiscoveryStates(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true, MetadataVersionCap: 2})
	owner := metadataHTTPTestResourceOwner("discovery")
	require.NoError(t, store.MetadataUpdate(Update{Owner: owner, EffectiveFrom: 100, Content: Content{Attributes: map[string]string{"k": "one"}}}))
	require.NoError(t, store.MetadataUpdate(Update{Owner: owner, EffectiveFrom: 200, Content: Content{Attributes: map[string]string{"k": "two"}}}))
	require.NoError(t, store.MetadataUpdate(Update{Owner: owner, EffectiveFrom: 300, Content: Content{Attributes: map[string]string{"k": "three"}}}))
	recorder := metadataHTTPGet(t, handler, "/resources?format=attributes&start=0&end=1")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var envelope struct {
		Data metadataHTTPDiscoveryPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Attributes, 1)
	row := envelope.Data.Attributes[0]
	require.Equal(t, "k", row.Key)
	require.Len(t, row.Values, 2, "the discarded oldest value is not claimed")
	require.Equal(t, "three", *row.Values[0].str)
	require.Equal(t, "two", *row.Values[1].str)
	require.Equal(t, "truncated", row.Coverage.State)
	require.Equal(t, "200", *row.Coverage.EarliestRetainedMS)
	require.Len(t, envelope.Data.Annotations, 1)
	require.Equal(t, "k", envelope.Data.Annotations[0]["key"])
}

func TestMetadataHTTPCombined(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	owner := metadataHTTPTestResourceOwner("combined")
	series := metadataHTTPTestSeries("combined_total", "combined", owner)
	metadataHTTPTestAssociate(t, store, series, owner, map[string]string{"host.name": "h"}, 100)
	metadataHTTPTestAssociate(t, store, series, ResourceSchemaOwner(owner), map[string]string{"schema_url": "resource-v1"}, 100)
	_, err := store.MetadataAssociate(series, Update{
		Owner: SourceOwner("receiver-a"), EffectiveFrom: 100,
		Content: Content{Attributes: map[string]string{"endpoint": "a"}},
	})
	require.NoError(t, err)
	secondOwner := metadataHTTPTestResourceOwner("combined-2")
	secondSeries := metadataHTTPTestSeries("combined_zz", "combined-2", secondOwner)
	metadataHTTPTestAssociate(t, store, secondSeries, secondOwner, map[string]string{"host.name": "h2"}, 100)

	recorder := metadataHTTPGet(t, handler, "/resources/series?view=combined&kind=resource&start=0&end=1&limit=1")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var envelope struct {
		Data metadataHTTPGroupPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Groups, 1)
	group := envelope.Data.Groups[0]
	require.Len(t, group.Components, 1)
	require.Empty(t, group.Relations, "no Entity relation evidence is invented")
	require.Equal(t, owner.Ref(), group.Components[0].Owner.Ref)
	require.Len(t, group.Components[0].Schema, 1)
	require.Equal(t, "resource-v1", *metadataTestEntry(t, group.Components[0].Schema[0].Content.Entries, "schema_url").str)
	require.Len(t, group.Components[0].SourceAssertions, 1)
	require.Equal(t, "receiver-a", *metadataTestEntry(t, group.Components[0].SourceAssertions[0].Owner.Identity.Attributes, "source").str)
	require.Equal(t, "a", *metadataTestEntry(t, group.Components[0].SourceAssertions[0].Versions[0].Content.Entries, "endpoint").str)
	require.NotNil(t, envelope.Data.NextToken)

	recorder = metadataHTTPGet(t, handler, "/resources/series?view=combined&kind=resource&start=0&end=1&limit=1&next_token="+url.QueryEscape(*envelope.Data.NextToken))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Groups, 1)
	require.Nil(t, envelope.Data.NextToken)
}

func TestMetadataHTTPAnnotations(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true, MetadataVersionCap: 2})
	owner := metadataHTTPTestResourceOwner("annotated")
	for _, effective := range []int64{100, 200, 300} {
		require.NoError(t, store.MetadataUpdate(Update{
			Owner: owner, EffectiveFrom: effective,
			Content: Content{Attributes: map[string]string{"k": itoa(int(effective))}},
		}))
	}

	// A query that needs discarded history reports truncated coverage.
	page := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t, handler, "/resources?start=0&end=1"))
	require.Len(t, page.Annotations, 1)
	annotation := page.Annotations[0]
	require.Equal(t, "coverage", annotation["category"])
	require.Equal(t, "coverage", annotation["axis"])
	require.Equal(t, "owner_history", annotation["scope"])
	require.Equal(t, "truncated", annotation["state"])
	ownerAnnotation, ok := annotation["owner"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, owner.Ref(), ownerAnnotation["ref"])
	requested := annotation["requested"].(map[string]any)
	require.Equal(t, "0", requested["from_ms"])
	require.Equal(t, "1000", requested["until_ms"])
	retained := annotation["retained"].(map[string]any)
	require.Equal(t, "200", retained["earliest_retained_ms"])
	require.Equal(t, []any{"history_truncated"}, retained["reasons"])
	require.NotNil(t, page.Owners[0].Versions[0].Coverage.EarliestRetainedMS)
	require.Equal(t, "truncated", page.Owners[0].Versions[0].Coverage.State)

	// The same retained history is sufficient for a query that begins at the
	// earliest retained version: no warning, and content coverage is covered.
	sufficient := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t, handler, "/resources?start=0.2&end=1"))
	require.Empty(t, sufficient.Annotations)
	require.Equal(t, "covered", sufficient.Owners[0].Versions[0].Coverage.State)

	// Discovery annotations identify the exact category and flattened key.
	discovery := metadataHTTPGet(t, handler, "/resources?format=attributes&start=0&end=1")
	var discoveryEnvelope struct {
		Data metadataHTTPDiscoveryPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(discovery.Body.Bytes(), &discoveryEnvelope))
	require.Len(t, discoveryEnvelope.Data.Annotations, 1)
	discoveryAnnotation := discoveryEnvelope.Data.Annotations[0]
	require.Equal(t, "resource", discoveryAnnotation["category"])
	require.Equal(t, "k", discoveryAnnotation["key"])
	require.Equal(t, "discovery", discoveryAnnotation["scope"])
	attribute := discoveryAnnotation["attribute"].(map[string]any)
	require.Equal(t, "resource", attribute["category"])
	require.Equal(t, "k", attribute["key"])
	require.Equal(t, "1000", discoveryAnnotation["requested"].(map[string]any)["until_ms"])
	discoverySufficient := metadataHTTPGet(t, handler, "/resources?format=attributes&start=0.2&end=1")
	require.NoError(t, json.Unmarshal(discoverySufficient.Body.Bytes(), &discoveryEnvelope))
	require.Empty(t, discoveryEnvelope.Data.Annotations)
}

func TestMetadataHTTPAssociationObservation(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	owner := metadataHTTPTestResourceOwner("observation")
	series := metadataHTTPTestSeries("observation_total", "observation", owner)
	// The membership starts at 100 and is observed again at 500. The incumbent
	// records observedUntil but not an independent first-observation bound, so
	// first_ms must stay null rather than being copied from effectiveFrom.
	metadataHTTPTestAssociate(t, store, series, owner, map[string]string{"host.name": "h"}, 100)
	metadataHTTPTestAssociate(t, store, series, owner, map[string]string{"host.name": "h"}, 500)

	recorder := metadataHTTPGet(t, handler, "/resources/series?kind=resource&resource.attr=host.name:h&start=0&end=1")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var envelope struct {
		Data metadataHTTPSeriesPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Series, 1)
	row := envelope.Data.Series[0]
	require.Equal(t, "applicable", row.Association.State)
	require.Equal(t, "100", row.Association.Validity.FromMS)
	require.Nil(t, row.Association.Validity.UntilMS, "membership validity remains exactly known")
	require.Nil(t, row.Association.Observation.FirstMS, "first observation is not synthesized from validity")
	require.Equal(t, "500", *row.Association.Observation.LastMS)
	require.Equal(t, "unknown", row.Association.Coverage.State)
	require.Equal(t, "series_association", row.Association.Coverage.Scope)
	require.Equal(t, []string{"first_observation_unrecorded"}, row.Association.Coverage.Reasons)

	require.NotEmpty(t, envelope.Data.Annotations)
	var observationAnnotation map[string]any
	for _, annotation := range envelope.Data.Annotations {
		if annotation["reason"] == "first_observation_unrecorded" {
			observationAnnotation = annotation
		}
	}
	require.NotNil(t, observationAnnotation)
	require.Equal(t, "series_association", observationAnnotation["scope"])
	require.Equal(t, "unknown", observationAnnotation["state"])
	require.Equal(t, "0", observationAnnotation["requested"].(map[string]any)["from_ms"])
	require.Equal(t, "1000", observationAnnotation["requested"].(map[string]any)["until_ms"])
	require.Equal(t, "500", observationAnnotation["observation"].(map[string]any)["last_ms"])
	require.Nil(t, observationAnnotation["observation"].(map[string]any)["first_ms"])
	association := observationAnnotation["association"].(map[string]any)
	require.Equal(t, "100", association["validity"].(map[string]any)["from_ms"])

	// Membership validity is still covered: the association is not labelled
	// truncated merely because its observation evidence is incomplete.
	require.NotEqual(t, "truncated", row.Association.Coverage.State)
}

func TestMetadataHTTPNoUnwarrantedWarnings(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	owner := metadataHTTPTestResourceOwner("quiet")
	series := metadataHTTPTestSeries("quiet_total", "quiet", owner)
	metadataHTTPTestAssociate(t, store, series, owner, map[string]string{"host.name": "h"}, 100)
	page := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t, handler, "/resources?start=0&end=1&projection=missing"))
	require.Empty(t, page.Annotations, "ordinary absent content needs no uncertainty warning")
	require.Equal(t, "absent", metadataTestEntry(t, page.Owners[0].Versions[0].Content.Entries, "missing").state)

	recorder := metadataHTTPGet(t, handler, "/resources/series?kind=source&match%5B%5D=%7B__name__%3D%22quiet_total%22%7D&start=0&end=1")
	var seriesEnvelope struct {
		Data metadataHTTPSeriesPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &seriesEnvelope))
	require.Equal(t, "unassociated", seriesEnvelope.Data.Series[0].Association.State)
	require.Empty(t, seriesEnvelope.Data.Annotations, "ordinary unassociated needs no uncertainty warning")
}

func TestMetadataHTTPPhysicalWork(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{FlushSamples: 1})
	owner := metadataHTTPTestResourceOwner("work")
	series := metadataHTTPTestSeries("work_total", "work", owner)
	require.NoError(t, store.AppendBatch(t.Context(), Batch{
		Series: []BatchSeries{{
			Labels: series, Timestamps: []int64{1000}, StartTimestamps: []int64{0},
			Kinds: []SampleKind{SampleFloat}, Values: []float64{1}, Histograms: [][]byte{nil},
		}},
		Updates: []AssociationUpdate{{
			Labels: series, Update: Update{
				Owner: owner, EffectiveFrom: 1000, Content: Content{Attributes: map[string]string{"host.name": "h"}},
			},
		}},
	}))
	require.NoError(t, store.Flush())
	store.reads.reset()
	workBefore := store.reads.metadataWorkSnapshot()
	commitsBefore := store.ReadStats().CommitCount

	for _, path := range []string{
		"/resources?start=0&end=2",
		"/resources?format=attributes&start=0&end=2",
		"/resources/series?resource.attr=host.name:h&start=0&end=2",
		"/resources/series?view=combined&resource.attr=host.name:h&start=0&end=2",
		"/resources/diff?time=1",
	} {
		recorder := metadataHTTPGet(t, handler, path)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	}
	read := store.ReadStats()
	require.Zero(t, read.SamplePayloadOpens, "metadata-only paths open no sample payload")
	require.Zero(t, read.SegmentsRead, "metadata-only paths read no sample segments")
	require.Zero(t, read.SampleRowsDecoded)
	require.Zero(t, read.PayloadBytesRead)
	workAfter := store.reads.metadataWorkSnapshot()
	require.Equal(t, workBefore.metadataTransactions, workAfter.metadataTransactions,
		"snapshot creation and metadata reads publish no metadata transaction")
	require.Equal(t, workBefore.versionsInserted, workAfter.versionsInserted)
	require.Equal(t, workBefore.membershipsInserted, workAfter.membershipsInserted)
	require.Equal(t, commitsBefore, store.ReadStats().CommitCount, "snapshot creation commits nothing")
	responseWork := store.reads.metadataHTTPWorkSnapshot()
	require.Positive(t, responseWork.Rows, "response rows are counted")
	require.Positive(t, responseWork.Values, "response value materialization is counted")
	require.Positive(t, responseWork.Bytes, "encoded response bytes are counted")
	require.Positive(t, responseWork.SnapshotRows,
		"snapshot metadata work is counted separately from sample work")
}

func TestMetadataSnapshotMembershipTransition(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	ownerA := metadataHTTPTestResourceOwner("member-a")
	ownerB := metadataHTTPTestResourceOwner("member-b")
	seriesA := metadataHTTPTestSeries("member_a_total", "a", ownerA)
	seriesB := metadataHTTPTestSeries("member_b_total", "b", ownerB)
	metadataHTTPTestAssociate(t, store, seriesA, ownerA, map[string]string{"host.name": "a"}, 100)
	metadataHTTPTestAssociate(t, store, seriesB, ownerB, map[string]string{"host.name": "b"}, 100)
	metadataHTTPTestAssociate(t, store, seriesA, SourceOwner("source-a"), map[string]string{"endpoint": "a"}, 100)
	metadataHTTPTestAssociate(t, store, seriesB, SourceOwner("source-b"), map[string]string{"endpoint": "b"}, 100)

	recorder := metadataHTTPGet(t, handler, "/resources/series?kind=source&start=0&end=10&limit=1")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var envelope struct {
		Data metadataHTTPSeriesPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Series, 1)
	require.NotNil(t, envelope.Data.NextToken)
	snapshotID := envelope.Data.SnapshotID

	// Between pages: a new Source membership at a later effective time and a new
	// series with a Source association. The pinned second page must not see them.
	metadataHTTPTestAssociate(t, store, seriesA, SourceOwner("source-a2"), map[string]string{"endpoint": "a2"}, 500)
	ownerC := metadataHTTPTestResourceOwner("member-c")
	seriesC := metadataHTTPTestSeries("member_c_total", "c", ownerC)
	metadataHTTPTestAssociate(t, store, seriesC, ownerC, map[string]string{"host.name": "c"}, 100)
	metadataHTTPTestAssociate(t, store, seriesC, SourceOwner("source-c"), map[string]string{"endpoint": "c"}, 100)

	recorder = metadataHTTPGet(t, handler, "/resources/series?kind=source&start=0&end=10&limit=10&next_token="+url.QueryEscape(*envelope.Data.NextToken))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Equal(t, snapshotID, envelope.Data.SnapshotID)
	require.Len(t, envelope.Data.Series, 1, "only the pre-existing second Source association remains")
	require.Equal(t, "member_b_total", envelope.Data.Series[0].Series[0].Value)
	require.Nil(t, envelope.Data.NextToken)
}

func TestMetadataSnapshotSurvivesFlushCompactionAndRetention(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{FlushSamples: 1})
	owner := metadataHTTPTestResourceOwner("zlifecycle")
	series := metadataHTTPTestSeries("lifecycle_total", "life", owner)
	peer := metadataHTTPTestResourceOwner("alifecycle")
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: peer, EffectiveFrom: 1000, Content: Content{Attributes: map[string]string{"host.name": "peer"}},
	}))
	require.NoError(t, store.AppendBatch(t.Context(), Batch{
		Series: []BatchSeries{{
			Labels: series, Timestamps: []int64{1000, 2000}, StartTimestamps: []int64{0, 0},
			Kinds: []SampleKind{SampleFloat, SampleFloat}, Values: []float64{1, 2}, Histograms: [][]byte{nil, nil},
		}},
		Updates: []AssociationUpdate{{
			Labels: series, Update: Update{
				Owner: owner, EffectiveFrom: 1000, Content: Content{Attributes: map[string]string{"host.name": "before"}},
			},
		}},
	}))
	first := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t, handler, "/resources?kind=resource&limit=1&start=0&end=3"))
	require.NotNil(t, first.NextToken)
	require.Equal(t, "peer", *metadataTestEntry(t, first.Owners[0].Versions[0].Content.Entries, "host.name").str)

	// A live content change, flush, compaction and retention between pages must not
	// alter the pinned continuation.
	require.NoError(t, store.MetadataUpdate(Update{
		Owner: owner, EffectiveFrom: 2000,
		Content: Content{Attributes: map[string]string{"host.name": "after"}},
	}))
	require.NoError(t, store.Flush())
	_, err := store.Compact(t.Context())
	require.NoError(t, err)
	_, err = store.RetainBefore(t.Context(), 500)
	require.NoError(t, err)

	second := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t,
		handler, "/resources?kind=resource&limit=1&start=0&end=3&next_token="+url.QueryEscape(*first.NextToken)))
	require.Len(t, second.Owners, 1)
	require.Equal(t, first.SnapshotID, second.SnapshotID)
	require.Nil(t, second.NextToken)
	require.Equal(t, owner.Ref(), second.Owners[0].Owner.Ref)
	require.Equal(t, "before", *metadataTestEntry(t, second.Owners[0].Versions[0].Content.Entries, "host.name").str,
		"the pinned snapshot keeps its committed state across flush, compaction and retention")
}

func TestMetadataSnapshotCursorCommutativeBinding(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	for i, instance := range []string{"one", "two"} {
		owner := metadataHTTPTestResourceOwner("commutative-" + instance)
		series := metadataHTTPTestSeries("commutative_"+instance, instance, owner)
		metadataHTTPTestAssociate(t, store, series, owner, map[string]string{
			"host.name": "h" + itoa(i), "region": "east", "zone": "north",
		}, 100)
	}
	firstPath := "/resources/series?resource.attr=region:east&resource.attr=zone:north" +
		"&match%5B%5D=%7B__name__%3D%22commutative_one%22%7D&match%5B%5D=%7B__name__%3D%22commutative_two%22%7D" +
		"&projection=host.name,region&limit=1&start=0&end=1"
	first := metadataHTTPGet(t, handler, firstPath)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	var envelope struct {
		Data metadataHTTPSeriesPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &envelope))
	require.NotNil(t, envelope.Data.NextToken)

	// The same commutative filters and projection stated in another order must
	// bind to the same cursor.
	secondPath := "/resources/series?resource.attr=zone:north&resource.attr=region:east" +
		"&match%5B%5D=%7B__name__%3D%22commutative_two%22%7D&match%5B%5D=%7B__name__%3D%22commutative_one%22%7D" +
		"&projection=region,host.name&limit=2&start=0&end=1&next_token=" + url.QueryEscape(*envelope.Data.NextToken)
	second := metadataHTTPGet(t, handler, secondPath)
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Series, 1)
	require.Nil(t, envelope.Data.NextToken)
}

func TestMetadataHTTPResourceIdentityLimit(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	// Both owners have empty service.namespace; the compatibility key cannot
	// distinguish absent from present-empty.
	ownerA := ResourceOwner(map[string]string{"service.name": "contract", "service.namespace": "", "service.instance.id": "a"})
	ownerB := ResourceOwner(map[string]string{"service.name": "contract", "service.namespace": "", "service.instance.id": "b"})
	seriesA := metadataHTTPTestSeries("identity_a_total", "a", ownerA)
	seriesB := metadataHTTPTestSeries("identity_b_total", "b", ownerB)
	metadataHTTPTestAssociate(t, store, seriesA, ownerA, map[string]string{"host.name": "a"}, 100)
	metadataHTTPTestAssociate(t, store, seriesB, ownerB, map[string]string{"service.namespace": "", "host.name": "b"}, 100)

	recorder := metadataHTTPGet(t, handler, "/resources?kind=resource&start=0&end=1&projection=host.name")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var envelope struct {
		Data metadataHTTPOwnerPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	var row metadataHTTPOwnerRow
	for _, candidate := range envelope.Data.Owners {
		if candidate.Owner.Ref == ownerA.Ref() {
			row = candidate
		}
	}
	require.NotEmpty(t, row.Owner.Ref)
	namespace := metadataTestIdentity(t, row.Owner.Identity, "service.namespace")
	require.Equal(t, "unknown", namespace.state, "an empty compatibility fallback is not proven present-empty")
	require.Equal(t, "compatibility_identity_absent_empty_unrecoverable", *namespace.reason)
	name := metadataTestIdentity(t, row.Owner.Identity, "service.name")
	require.Equal(t, "present", name.state)

	// Empty identity fallback does not satisfy resource.attr=service.namespace:.
	recorder = metadataHTTPGet(t, handler, "/resources/series?resource.attr=service.namespace:&start=0&end=1")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var seriesEnvelope struct {
		Data metadataHTTPSeriesPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &seriesEnvelope))
	require.Len(t, seriesEnvelope.Data.Series, 1)
	require.Equal(t, "identity_b_total", seriesEnvelope.Data.Series[0].Series[0].Value,
		"only the owner with explicit present-empty content matches")

	// A non-empty identifying value remains a present applicable string.
	recorder = metadataHTTPGet(t, handler, "/resources/series?resource.attr=service.name:contract&start=0&end=1")
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &seriesEnvelope))
	require.Len(t, seriesEnvelope.Data.Series, 2)
}

func TestMetadataSnapshotCancelDuringPage(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	metadataHTTPTestPinOwners(t, store, 3)
	first := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t, handler, "/resources?limit=1&start=0&end=1"))
	require.NotNil(t, first.NextToken)
	continuation := "/resources?limit=1&start=0&end=1&next_token=" + url.QueryEscape(*first.NextToken)

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	metadataSnapshotPageHook = func(*metadataSnapshot) {
		once.Do(func() {
			close(started)
			<-release
		})
	}
	t.Cleanup(func() { metadataSnapshotPageHook = nil })

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- metadataHTTPGet(t, handler, continuation)
	}()
	<-started

	// Cancellation succeeds while the admitted page is still reading.
	recorder := metadataHTTPDelete(t, handler, "/resources/snapshots/"+first.SnapshotID)
	require.Equal(t, http.StatusNoContent, recorder.Code)

	// No new page may begin after cancellation.
	metadataHTTPRequireError(t, metadataHTTPGet(t, handler, continuation), 410, "metadata_snapshot_canceled")

	close(release)
	completed := <-done
	require.Equal(t, http.StatusOK, completed.Code, completed.Body.String())
	var envelope struct {
		Data metadataHTTPOwnerPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(completed.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Owners, 1, "the admitted page completed from its pinned catalog")
	require.Equal(t, first.SnapshotID, envelope.Data.SnapshotID)

	snapshot := store.metadataSnapshots.snapshots[first.SnapshotID]
	snapshot.mu.Lock()
	require.Nil(t, snapshot.catalog, "the cloned catalog is released after the last admitted reader")
	snapshot.mu.Unlock()
}

func metadataHTTPTestEncodeEnvelope(t *testing.T, envelope metadataCursorEnvelope) string {
	t.Helper()
	raw, err := json.Marshal(envelope)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestMetadataCursorValidationPrecedence(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	metadataHTTPTestPinOwners(t, store, 3)
	first := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t, handler, "/resources?limit=1&start=0&end=1"))
	require.NotNil(t, first.NextToken)
	validPath := "/resources?limit=1&start=0&end=1&next_token="
	valid := metadataHTTPTestDecodeCursor(t, *first.NextToken)

	badPosition := valid
	badPosition.Position = json.RawMessage(`{}`)
	metadataHTTPRequireError(t, metadataHTTPGet(t, handler,
		validPath+url.QueryEscape(metadataHTTPTestEncodeEnvelope(t, metadataCursorEnvelope{metadataCursorSigned: badPosition, Signature: "00"}))),
		400, "metadata_cursor_corrupt")

	unsupported := valid
	unsupported.Version = 2
	unsupported.Position = json.RawMessage(`{}`)
	metadataHTTPRequireError(t, metadataHTTPGet(t, handler,
		validPath+url.QueryEscape(metadataHTTPTestEncodeEnvelope(t, metadataCursorEnvelope{metadataCursorSigned: unsupported, Signature: "00"}))),
		400, "metadata_cursor_version_unsupported")

	badHMAC := valid
	metadataHTTPRequireError(t, metadataHTTPGet(t, handler,
		validPath+url.QueryEscape(metadataHTTPTestEncodeEnvelope(t, metadataCursorEnvelope{metadataCursorSigned: badHMAC, Signature: "00"}))),
		400, "metadata_cursor_corrupt")

	unknown := valid
	unknown.Snapshot = "unknown-snapshot"
	unknownToken := metadataHTTPTestSignCursor(t, store, unknown)
	metadataHTTPRequireError(t, metadataHTTPGet(t, handler, validPath+url.QueryEscape(unknownToken)),
		404, "metadata_snapshot_unknown")

	// A structurally valid token from a foreign incarnation is classified as
	// store_restarted before the HMAC is checked, because the old key is not
	// retained. This is a lifecycle classification, not old-token authentication.
	foreign := valid
	foreign.Incarnation = "foreign-incarnation"
	metadataHTTPRequireError(t, metadataHTTPGet(t, handler,
		validPath+url.QueryEscape(metadataHTTPTestEncodeEnvelope(t, metadataCursorEnvelope{metadataCursorSigned: foreign, Signature: "00"}))),
		410, "metadata_snapshot_reclaimed")

	// Malformed envelope/base64 are rejected before any semantic classification.
	metadataHTTPRequireError(t, metadataHTTPGet(t, handler, validPath+url.QueryEscape("!!!")), 400, "metadata_cursor_malformed")
	metadataHTTPRequireError(t, metadataHTTPGet(t, handler,
		validPath+url.QueryEscape(base64.RawURLEncoding.EncodeToString([]byte(`not-json`)))),
		400, "metadata_cursor_malformed")
}

func TestMetadataHTTPCombinedMissingResourceComponent(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	owner := metadataHTTPTestResourceOwner("combined-scope")
	series := metadataHTTPTestSeries("combined_scope_total", "scope", owner)
	// Only a Scope association exists: the series has no original Resource
	// component, so the combined group may not claim covered context.
	metadataHTTPTestAssociate(t, store, series, ScopeOwner("sdk", "v1"), map[string]string{"name": "scope"}, 100)

	recorder := metadataHTTPGet(t, handler,
		"/resources/series?view=combined&kind=scope&match%5B%5D=%7B__name__%3D%22combined_scope_total%22%7D&start=0&end=1")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var envelope struct {
		Data metadataHTTPGroupPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Groups, 1)
	group := envelope.Data.Groups[0]
	require.Empty(t, group.Components)
	require.Equal(t, "unknown", group.Coverage.State)
	require.Equal(t, "combined", group.Coverage.Scope)
	require.Equal(t, []string{"no_resource_component"}, group.Coverage.Reasons)
	require.Len(t, envelope.Data.Annotations, 1)
	annotation := envelope.Data.Annotations[0]
	require.Equal(t, "combined", annotation["scope"])
	require.Equal(t, "unknown", annotation["state"])
	require.Equal(t, "no_resource_component", annotation["reason"])
	require.Equal(t, "0", annotation["requested"].(map[string]any)["from_ms"])
}

func TestMetadataHTTPCombinedSufficientRetainedRange(t *testing.T) {
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true, MetadataVersionCap: 2})
	owner := metadataHTTPTestResourceOwner("combined-retained")
	series := metadataHTTPTestSeries("combined_retained_total", "retained", owner)
	for _, effective := range []int64{100, 200, 300} {
		require.NoError(t, store.MetadataUpdate(Update{
			Owner: owner, EffectiveFrom: effective,
			Content: Content{Attributes: map[string]string{"k": itoa(int(effective))}},
		}))
	}
	metadataHTTPTestAssociate(t, store, series, owner, map[string]string{"k": "300"}, 300)

	recorder := metadataHTTPGet(t, handler,
		"/resources/series?view=combined&kind=resource&match%5B%5D=%7B__name__%3D%22combined_retained_total%22%7D&start=0.2&end=1")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var envelope struct {
		Data metadataHTTPGroupPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.Len(t, envelope.Data.Groups, 1)
	group := envelope.Data.Groups[0]
	require.Len(t, group.Components, 1)
	require.Equal(t, "covered", group.Coverage.State, "retained evidence suffices for a query starting at 200")
	require.Empty(t, envelope.Data.Annotations)
}

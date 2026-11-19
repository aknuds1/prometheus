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
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestMetadataTypedHTTPValues(t *testing.T) {
	empty := metadataPresentString("")
	boolean := metadataPresentBoolean(false)
	integer := metadataPresentInteger(9007199254740993)
	double := metadataPresentDouble(0x8000000000000000)
	array := metadataPresentArray(nil)
	typedMap, err := metadataPresentMap([]metadataTypedMapEntry{
		{Key: "z", Value: metadataNullValue()},
		{Key: "a", Value: empty},
	})
	require.NoError(t, err)

	cases := []struct {
		name  string
		value metadataTypedValue
		want  string
	}{
		{"present-empty-string", empty, `{"state":"present","type":"string","value":""}`},
		{"present-boolean-false", boolean, `{"state":"present","type":"boolean","value":false}`},
		{"present-large-integer", integer, `{"state":"present","type":"integer","decimal":"9007199254740993"}`},
		{"present-double-bits", double, `{"state":"present","type":"double","bits":"0x8000000000000000"}`},
		{"present-empty-array", array, `{"state":"present","type":"array","items":[]}`},
		{"present-map-sorted", typedMap, `{"state":"present","type":"map","entries":[{"key":"a","value":{"state":"present","type":"string","value":""}},{"key":"z","value":{"state":"null"}}]}`},
		{"null", metadataNullValue(), `{"state":"null"}`},
		{"absent", metadataAbsentValue(), `{"state":"absent"}`},
		{"cleared", metadataClearedValue(), `{"state":"cleared"}`},
		{"unknown", metadataUnknownValue("partial_source"), `{"state":"unknown","reason":"partial_source"}`},
		{"ambiguous", metadataAmbiguousValue(nil), `{"state":"ambiguous","assertions":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.value)
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(encoded))
		})
	}

	reversed, err := metadataPresentMap([]metadataTypedMapEntry{
		{Key: "a", Value: empty},
		{Key: "z", Value: metadataNullValue()},
	})
	require.NoError(t, err)
	require.Equal(t, metadataTypedValueCanonical(typedMap), metadataTypedValueCanonical(reversed),
		"map equality ignores entry iteration order")
	require.NotEqual(t,
		metadataTypedValueCanonical(metadataPresentArray([]metadataTypedValue{metadataPresentString("a"), metadataPresentString("b")})),
		metadataTypedValueCanonical(metadataPresentArray([]metadataTypedValue{metadataPresentString("b"), metadataPresentString("a")})),
		"arrays preserve order")
	_, err = metadataPresentMap([]metadataTypedMapEntry{
		{Key: "a", Value: empty}, {Key: "a", Value: empty},
	})
	require.Error(t, err, "duplicate typed map keys are rejected")

	// HTTP-observable distinction: a present empty string is not absent, and a
	// projected missing key is explicit absent rather than an omitted state.
	store, handler := metadataHTTPTestStore(t, StorageOptions{DisableWAL: true})
	owner := metadataHTTPTestResourceOwner("typed")
	series := metadataHTTPTestSeries("typed_total", "typed", owner)
	metadataHTTPTestAssociate(t, store, series, owner, map[string]string{"empty": "", "other": "x"}, 1000)
	recorder := metadataHTTPGet(t, handler, "/resources/series?kind=resource&resource.attr=other:x&start=0&end=2&projection=empty,missing,other")
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var page metadataHTTPSeriesPage
	var envelope struct {
		Data metadataHTTPSeriesPage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	page = envelope.Data
	require.Len(t, page.Series, 1)
	entries := page.Series[0].Versions[0].Content.Entries
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
		{"state-unavailable", func(id string) { store.metadataSnapshots.snapshots[id].catalog = nil }, "metadata_snapshot_state_unavailable"},
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
	page := metadataHTTPReadOwnerPage(t, metadataHTTPGet(t, handler, "/resources?start=0&end=1"))
	require.Len(t, page.Annotations, 1)
	annotation := page.Annotations[0]
	require.Equal(t, "coverage", annotation["category"])
	require.Equal(t, "coverage", annotation["axis"])
	require.Equal(t, "truncated", annotation["state"])
	ownerAnnotation, ok := annotation["owner"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, owner.Ref(), ownerAnnotation["ref"])
	require.Equal(t, "200", annotation["earliest_retained_ms"])
	require.NotNil(t, page.Owners[0].Versions[0].Coverage.EarliestRetainedMS)
	require.Equal(t, "truncated", page.Owners[0].Versions[0].Coverage.State)
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

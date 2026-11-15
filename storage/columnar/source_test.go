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
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
)

func newArrowTestStorage(t *testing.T, flushSamples int) *columnar.Storage {
	t.Helper()

	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{FlushSamples: flushSamples})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func appendSample(t *testing.T, s *columnar.Storage, ls labels.Labels, ts int64, v float64, h *histogram.Histogram, fh *histogram.FloatHistogram) {
	t.Helper()

	app := s.AppenderV2(t.Context())
	_, err := app.Append(0, ls, 0, ts, v, h, fh, storage.AOptions{})
	require.NoError(t, err)
	require.NoError(t, app.Commit())
}

// readArrowStream decodes every record batch of an Arrow IPC stream.
func readArrowStream(t *testing.T, r io.Reader) []columnar.Series {
	t.Helper()

	reader, err := ipc.NewReader(r, ipc.WithAllocator(memory.NewGoAllocator()))
	require.NoError(t, err)
	defer reader.Release()

	var out []columnar.Series
	for reader.Next() {
		batch := reader.RecordBatch()
		require.Equal(t, columnar.BlockSchema().String(), batch.Schema().String(), "the stream must use the documented schema")

		// Decode with the same function a consumer of this endpoint would use.
		series, err := columnar.DecodeRecord(batch)
		require.NoError(t, err)
		out = append(out, series...)
	}
	require.NoError(t, reader.Err())
	return out
}

func doRequest(t *testing.T, h http.Handler, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/arrow?"+rawQuery, http.NoBody)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	return recorder
}

func TestArrowEndpointEncodesSeries(t *testing.T) {
	s := newArrowTestStorage(t, 1000)

	appendSample(t, s, labels.FromStrings("__name__", "http_requests_total", "path", "/foo"), 1000, 1, nil, nil)
	appendSample(t, s, labels.FromStrings("__name__", "http_requests_total", "path", "/foo"), 2000, 2, nil, nil)
	appendSample(t, s, labels.FromStrings("__name__", "http_requests_total", "path", "/bar"), 2000, 5, nil, nil)
	appendSample(t, s, labels.FromStrings("__name__", "other_metric"), 2000, 9, nil, nil)

	h := columnar.NewArrowHandler(s, slog.New(slog.DiscardHandler), columnar.ArrowOptions{})

	recorder := doRequest(t, h, url.Values{"match[]": {`{__name__="http_requests_total"}`}}.Encode())
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, "application/vnd.apache.arrow.stream", recorder.Header().Get("Content-Type"))

	series := readArrowStream(t, recorder.Body)
	require.Len(t, series, 2, "only the matching series must be returned")

	require.Equal(t, "/bar", series[0].Labels.Get("path"))
	require.Equal(t, []int64{2000}, series[0].Timestamps)
	require.Equal(t, []float64{5}, series[0].Values)

	require.Equal(t, "/foo", series[1].Labels.Get("path"))
	require.Equal(t, []int64{1000, 2000}, series[1].Timestamps)
	require.Equal(t, []float64{1, 2}, series[1].Values)
}

func TestArrowEndpointTimeRange(t *testing.T) {
	s := newArrowTestStorage(t, 1000)

	ls := labels.FromStrings("__name__", "range_metric")
	for i := range 10 {
		appendSample(t, s, ls, int64(i)*1000, float64(i), nil, nil)
	}

	h := columnar.NewArrowHandler(s, slog.New(slog.DiscardHandler), columnar.ArrowOptions{})

	// 3s to 5s inclusive, in unix seconds.
	recorder := doRequest(t, h, url.Values{
		"match[]": {`range_metric`},
		"start":   {"3"},
		"end":     {"5"},
	}.Encode())
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	series := readArrowStream(t, recorder.Body)
	require.Len(t, series, 1)
	require.Equal(t, []int64{3000, 4000, 5000}, series[0].Timestamps)
	require.Equal(t, []float64{3, 4, 5}, series[0].Values)
}

func TestArrowEndpointMergesAcrossBlocks(t *testing.T) {
	// A small flush threshold spreads one series over several blocks.
	s := newArrowTestStorage(t, 2)

	ls := labels.FromStrings("__name__", "merge_metric")
	for i := range 7 {
		appendSample(t, s, ls, int64(i)*1000, float64(i), nil, nil)
	}
	require.Greater(t, s.NumBlocks(), 1, "the test needs the series to span blocks")

	h := columnar.NewArrowHandler(s, slog.New(slog.DiscardHandler), columnar.ArrowOptions{})
	recorder := doRequest(t, h, url.Values{"match[]": {`merge_metric`}}.Encode())
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	series := readArrowStream(t, recorder.Body)
	require.Len(t, series, 1, "a series split across blocks must come back as one row")
	require.Equal(t, []int64{0, 1000, 2000, 3000, 4000, 5000, 6000}, series[0].Timestamps, "samples must be merged in time order")
	require.Equal(t, []float64{0, 1, 2, 3, 4, 5, 6}, series[0].Values)
}

func TestArrowEndpointCarriesStalenessAndHistograms(t *testing.T) {
	s := newArrowTestStorage(t, 1000)

	ls := labels.FromStrings("__name__", "typed_metric")
	appendSample(t, s, ls, 1000, 1, nil, nil)
	appendSample(t, s, ls, 2000, math.Float64frombits(value.StaleNaN), nil, nil)

	h := labels.FromStrings("__name__", "hist_metric")
	hist := &histogram.Histogram{
		Schema:          0,
		Count:           4,
		Sum:             8,
		PositiveSpans:   []histogram.Span{{Offset: 0, Length: 3}},
		PositiveBuckets: []int64{1, 2, 1},
	}
	appendSample(t, s, h, 1000, 0, hist, nil)

	handler := columnar.NewArrowHandler(s, slog.New(slog.DiscardHandler), columnar.ArrowOptions{})
	recorder := doRequest(t, handler, url.Values{"match[]": {`{__name__=~".+"}`}}.Encode())
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	series := readArrowStream(t, recorder.Body)
	require.Len(t, series, 2)

	stale := series[0]
	require.Equal(t, "hist_metric", stale.Labels.Get("__name__"))
	require.Equal(t, []columnar.SampleKind{columnar.SampleHistogram}, stale.Kinds)

	typed := series[1]
	require.Equal(t, []columnar.SampleKind{columnar.SampleFloat, columnar.SampleStale}, typed.Kinds,
		"staleness must survive as its own kind, not as a NaN value")
}

func TestArrowEndpointRejectsBadRequests(t *testing.T) {
	s := newArrowTestStorage(t, 1000)
	h := columnar.NewArrowHandler(s, slog.New(slog.DiscardHandler), columnar.ArrowOptions{})

	for _, tc := range []struct {
		name  string
		query url.Values
	}{
		{name: "no matcher", query: url.Values{}},
		{name: "invalid matcher", query: url.Values{"match[]": {`{__name__=`}}},
		{name: "invalid start", query: url.Values{"match[]": {`x`}, "start": {"not-a-time"}}},
		{name: "end before start", query: url.Values{"match[]": {`x`}, "start": {"10"}, "end": {"5"}}},
		{name: "negative limit", query: url.Values{"match[]": {`x`}, "limit": {"-1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRequest(t, h, tc.query.Encode())
			require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
		})
	}
}

func TestArrowEndpointEmptyResultIsAValidStream(t *testing.T) {
	s := newArrowTestStorage(t, 1000)
	h := columnar.NewArrowHandler(s, slog.New(slog.DiscardHandler), columnar.ArrowOptions{})

	recorder := doRequest(t, h, url.Values{"match[]": {`does_not_exist`}}.Encode())
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	// An empty result still yields a readable stream with the documented schema,
	// so a consumer does not have to special-case "no series".
	series := readArrowStream(t, recorder.Body)
	require.Empty(t, series)
}

func TestArrowEndpointBatchesAndLimits(t *testing.T) {
	s := newArrowTestStorage(t, 1000)

	for i := range 7 {
		ls := labels.FromStrings("__name__", "batch_metric", "idx", string(rune('a'+i)))
		appendSample(t, s, ls, 1000, float64(i), nil, nil)
	}

	h := columnar.NewArrowHandler(s, slog.New(slog.DiscardHandler), columnar.ArrowOptions{BatchSeries: 2})

	recorder := doRequest(t, h, url.Values{"match[]": {`batch_metric`}}.Encode())
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	reader, err := ipc.NewReader(recorder.Body, ipc.WithAllocator(memory.NewGoAllocator()))
	require.NoError(t, err)
	defer reader.Release()

	batches := 0
	rows := 0
	for reader.Next() {
		batches++
		rows += int(reader.RecordBatch().NumRows())
	}
	require.NoError(t, reader.Err())
	require.Equal(t, 4, batches, "7 series at 2 per batch must produce 4 batches")
	require.Equal(t, 7, rows)

	limited := doRequest(t, h, url.Values{"match[]": {`batch_metric`}, "limit": {"3"}}.Encode())
	require.Equal(t, http.StatusOK, limited.Code)
	require.Len(t, readArrowStream(t, limited.Body), 3)
}

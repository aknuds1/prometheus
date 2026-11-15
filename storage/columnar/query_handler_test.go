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
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage/columnar"
)

func TestQueryHandlerRangeQuery(t *testing.T) {
	s := newArrowTestStorage(t, 1000)

	// 1 sample per minute for 10 minutes, constant 2/s slope => rate 0.0333/s.
	ls := labels.FromStrings("__name__", "q_rate_total")
	for i := range 10 {
		appendSample(t, s, ls, int64(i)*60000, float64(i)*120, nil, nil)
	}

	engine := promqltest.NewTestEngine(t, false, 5*time.Minute, promqltest.DefaultMaxSamplesPerQuery)
	h := columnar.NewQueryHandler(engine, s, slog.New(slog.DiscardHandler))

	params := url.Values{
		"query": {`rate(q_rate_total[5m])`},
		"start": {"0"},
		"end":   {"540"},
		"step":  {"60"},
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/query_range?"+params.Encode(), http.NoBody))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric labels.Labels `json:"metric"`
				Values [][2]any      `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Equal(t, "success", response.Status)
	require.Equal(t, "matrix", response.Data.ResultType)
	require.Len(t, response.Data.Result, 1)
	require.NotEmpty(t, response.Data.Result[0].Values)

	// The last rate must be 2/s: 120 units per 60s.
	last := response.Data.Result[0].Values[len(response.Data.Result[0].Values)-1]
	require.InDelta(t, 2.0, last[1], 1e-9)
}

func TestQueryHandlerErrors(t *testing.T) {
	s := newArrowTestStorage(t, 1000)
	engine := promqltest.NewTestEngine(t, false, 5*time.Minute, promqltest.DefaultMaxSamplesPerQuery)
	h := columnar.NewQueryHandler(engine, s, slog.New(slog.DiscardHandler))

	for _, tc := range []struct {
		name   string
		params url.Values
		status int
	}{
		{name: "no query", params: url.Values{"start": {"0"}, "end": {"1"}}, status: http.StatusBadRequest},
		{name: "no time range", params: url.Values{"query": {"up"}}, status: http.StatusBadRequest},
		{name: "bad step", params: url.Values{"query": {"up"}, "start": {"0"}, "end": {"1"}, "step": {"x"}}, status: http.StatusBadRequest},
		{name: "parse error", params: url.Values{"query": {"sum("}, "start": {"0"}, "end": {"1"}}, status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/query_range?"+tc.params.Encode(), http.NoBody))
			require.Equal(t, tc.status, recorder.Code, recorder.Body.String())
		})
	}
}

func TestQueryHandlerInstantQuery(t *testing.T) {
	s := newArrowTestStorage(t, 1000)
	appendSample(t, s, labels.FromStrings("__name__", "q_instant"), 120000, 42, nil, nil)

	engine := promqltest.NewTestEngine(t, false, 5*time.Minute, promqltest.DefaultMaxSamplesPerQuery)
	h := columnar.NewQueryHandler(engine, s, slog.New(slog.DiscardHandler))

	params := url.Values{"query": {"q_instant"}, "start": {"120"}, "end": {"120"}}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/query_range?"+params.Encode(), http.NoBody))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), `"resultType":"vector"`)
	require.Contains(t, recorder.Body.String(), `42`)
}

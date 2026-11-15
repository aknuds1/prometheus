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

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/route"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/promql/promqltest"
)

func TestQueryStorageIsIndependentOfIngestStorage(t *testing.T) {
	registry := prometheus.NewRegistry()
	primary := promqltest.LoadedStorage(t, "load 0s\n  query_storage_value 1")
	t.Cleanup(func() { require.NoError(t, primary.Close()) })
	selected := promqltest.LoadedStorage(t, "load 0s\n  query_storage_value 2")
	t.Cleanup(func() { require.NoError(t, selected.Close()) })
	engine := promqltest.NewTestEngine(t, false, 5*time.Minute, promqltest.DefaultMaxSamplesPerQuery)
	h := New(nil, &Options{
		ExternalURL: &url.URL{Scheme: "http", Host: "localhost"}, ListenAddresses: []string{"localhost:0"},
		Context: t.Context(), QueryEngine: engine, QueryStorage: selected, Storage: primary,
		LocalStorage: &dbAdapter{DB: primary.DB}, Gatherer: registry, Registerer: registry,
		RoutePrefix: "/", Version: &PrometheusVersion{},
	})
	h.SetReady(Ready)

	av1 := route.New()
	h.apiV1.Register(av1)
	recorder := httptest.NewRecorder()
	av1.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/query?query=query_storage_value&time=0", http.NoBody))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var response struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Equal(t, "success", response.Status)
	require.Len(t, response.Data.Result, 1)
	require.Equal(t, "2", response.Data.Result[0].Value[1])
}

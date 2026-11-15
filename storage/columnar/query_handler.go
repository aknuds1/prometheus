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
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
)

// QueryHandler runs PromQL against the columnar storage using the Prometheus
// query engine.
//
// The point is comparison, not capability: the engine, the parser and the
// functions are the ones Prometheus already uses, so the only difference against
// a normal query is which storage answers it. That makes latency and correctness
// comparable instead of a property of two different implementations.
//
// The response shape follows the Prometheus HTTP API closely enough for the
// measurements this prototype exists for, but it is not the API: label values,
// warnings and annotations are dropped, and histogram samples are rendered as a
// count and sum rather than in the API's format.
type QueryHandler struct {
	engine  promql.QueryEngine
	storage *Storage
	logger  *slog.Logger
}

// NewQueryHandler returns a handler serving PromQL over the storage.
func NewQueryHandler(engine promql.QueryEngine, storage *Storage, logger *slog.Logger) *QueryHandler {
	return &QueryHandler{engine: engine, storage: storage, logger: logger}
}

type queryResponse struct {
	Status    string    `json:"status"`
	Data      queryData `json:"data"`
	Error     string    `json:"error,omitempty"`
	ErrorType string    `json:"errorType,omitempty"`
}

type queryData struct {
	ResultType string        `json:"resultType"`
	Result     []querySeries `json:"result"`
}

type querySeries struct {
	Metric labels.Labels `json:"metric"`
	Floats [][2]any      `json:"values,omitempty"`
}

func (h *QueryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	expr := query.Get("query")
	if expr == "" {
		h.fail(w, http.StatusBadRequest, "bad_data", "query parameter is required")
		return
	}

	start, err := parseSeconds(query.Get("start"), math.NaN())
	if err != nil {
		h.fail(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	end, err := parseSeconds(query.Get("end"), math.NaN())
	if err != nil {
		h.fail(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	stepSeconds, err := parseSeconds(query.Get("step"), 0)
	if err != nil {
		h.fail(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	if math.IsNaN(start) || math.IsNaN(end) {
		h.fail(w, http.StatusBadRequest, "bad_data", "start and end are required")
		return
	}

	// Parsed values are seconds, as in the Prometheus API; evaluation works in
	// milliseconds.
	startTime := time.UnixMilli(int64(start * 1000)).UTC()
	endTime := time.UnixMilli(int64(end * 1000)).UTC()

	var q promql.Query
	if stepSeconds > 0 {
		q, err = h.engine.NewRangeQuery(r.Context(), h.storage, nil, expr, startTime, endTime, time.Duration(int64(stepSeconds*1000))*time.Millisecond)
	} else {
		q, err = h.engine.NewInstantQuery(r.Context(), h.storage, nil, expr, endTime)
	}
	if err != nil {
		h.fail(w, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	defer q.Close()

	result := q.Exec(r.Context())
	if result.Err != nil {
		h.fail(w, http.StatusUnprocessableEntity, "execution", result.Err.Error())
		return
	}

	response := queryResponse{Status: "success"}
	switch value := result.Value.(type) {
	case promql.Matrix:
		response.Data.ResultType = "matrix"
		for _, series := range value {
			response.Data.Result = append(response.Data.Result, encodeSeries(series.Metric, series.Floats, series.Histograms))
		}
	case promql.Vector:
		response.Data.ResultType = "vector"
		for _, sample := range value {
			series := querySeries{Metric: sample.Metric}
			series.Floats = append(series.Floats, [2]any{float64(sample.T) / 1000, sample.F})
			response.Data.Result = append(response.Data.Result, series)
		}
	case promql.Scalar:
		response.Data.ResultType = "scalar"
		response.Data.Result = []querySeries{{
			Metric: labels.EmptyLabels(),
			Floats: [][2]any{{float64(value.T) / 1000, value.V}},
		}}
	default:
		h.fail(w, http.StatusInternalServerError, "internal", fmt.Sprintf("unsupported result type %T", result.Value))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		h.logger.Error("Failed to encode the columnar query response", "err", err)
	}
}

// encodeSeries renders float and histogram samples of one series.
//
// Histogram samples are rendered as their count and sum. Anything richer belongs
// in the API's own format, which this prototype deliberately does not duplicate.
func encodeSeries(metric labels.Labels, floats []promql.FPoint, histograms []promql.HPoint) querySeries {
	series := querySeries{Metric: metric}
	for _, point := range floats {
		series.Floats = append(series.Floats, [2]any{float64(point.T) / 1000, point.F})
	}
	for _, point := range histograms {
		series.Floats = append(series.Floats, [2]any{float64(point.T) / 1000, map[string]float64{
			"count": point.H.Count,
			"sum":   point.H.Sum,
		}})
	}
	return series
}

// parseSeconds parses a timestamp or duration in seconds, returning fallback when
// the parameter is empty.
func parseSeconds(value string, fallback float64) (float64, error) {
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid time value %q", value)
	}
	return parsed, nil
}

func (*QueryHandler) fail(w http.ResponseWriter, status int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(queryResponse{Status: "error", ErrorType: errorType, Error: message})
}

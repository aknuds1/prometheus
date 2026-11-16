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

package remote

// The ordinary OTLP receiver translates exponential histograms into native
// histograms without checking the OTLP count against the bucket populations.
// A request whose count and buckets disagree therefore reaches the storage
// appender. These tests drive the real OTLP HTTP handler against a stock TSDB
// and the experimental columnar store and require both to reject the request
// with the same stock-compatible behavior.

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/common/promslog"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/util/teststorage"
)

const (
	otlpHistogramName = "otlp_histogram_validation"
	otlpGaugeName     = "otlp_histogram_validation_gauge"
	otlpHistogramTime = int64(1000)
)

// otlpExponentialHistogramRequest builds a real OTLP export request with one
// exponential histogram data point. The two positive buckets state one and four
// observations; count states 4 for the inconsistent case and 5 for the
// consistent control. With this offset and scale the translator produces the
// exact N0 shape: spans [{Offset:1, Length:2}] and deltas [1,3].
func otlpExponentialHistogramRequest(count uint64, sum float64, bucketCounts []uint64, withGauge bool) pmetricotlp.ExportRequest {
	metrics := pmetric.NewMetrics()
	scope := metrics.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()

	if withGauge {
		gaugeMetric := scope.Metrics().AppendEmpty()
		gaugeMetric.SetName(otlpGaugeName)
		gauge := gaugeMetric.SetEmptyGauge()
		gaugePoint := gauge.DataPoints().AppendEmpty()
		gaugePoint.SetTimestamp(pcommon.NewTimestampFromTime(time.UnixMilli(otlpHistogramTime)))
		gaugePoint.SetDoubleValue(1)
	}

	metric := scope.Metrics().AppendEmpty()
	metric.SetName(otlpHistogramName)
	exp := metric.SetEmptyExponentialHistogram()
	exp.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	point := exp.DataPoints().AppendEmpty()
	point.SetTimestamp(pcommon.NewTimestampFromTime(time.UnixMilli(otlpHistogramTime)))
	point.SetStartTimestamp(pcommon.NewTimestampFromTime(time.UnixMilli(otlpHistogramTime - 500)))
	point.SetCount(count)
	point.SetSum(sum)
	point.SetScale(0)
	point.Positive().SetOffset(0)
	point.Positive().BucketCounts().FromRaw(bucketCounts)
	return pmetricotlp.NewExportRequestFromMetrics(metrics)
}

// otlpPost sends one marshalled OTLP export request through the real handler.
func otlpPost(t *testing.T, handler http.Handler, request pmetricotlp.ExportRequest) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := request.MarshalProto()
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "", bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-protobuf")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

// otlpSeriesRequire renders every retained sample of the named series so a
// failure names the exact state, not only a status code.
func otlpSeriesRequire(t *testing.T, q storage.Queryable, name string) []string {
	t.Helper()
	querier, err := q.Querier(math.MinInt64, math.MaxInt64)
	require.NoError(t, err)
	defer func() { require.NoError(t, querier.Close()) }()
	set := querier.Select(context.Background(), true, nil,
		labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, name))
	out := []string{}
	for set.Next() {
		iterator := set.At().Iterator(nil)
		for typ := iterator.Next(); typ != chunkenc.ValNone; typ = iterator.Next() {
			switch typ {
			case chunkenc.ValFloat:
				_, v := iterator.At()
				out = append(out, fmt.Sprintf("%s|%d|float|%v", set.At().Labels(), iterator.AtT(), v))
			case chunkenc.ValHistogram:
				_, h := iterator.AtHistogram(nil)
				out = append(out, fmt.Sprintf("%s|%d|histogram|count=%d;sum=%v;spans=%v;buckets=%v",
					set.At().Labels(), iterator.AtT(), h.Count, h.Sum, h.PositiveSpans, h.PositiveBuckets))
			case chunkenc.ValFloatHistogram:
				_, fh := iterator.AtFloatHistogram(nil)
				out = append(out, fmt.Sprintf("%s|%d|float_histogram|count=%v;sum=%v;spans=%v;buckets=%v",
					set.At().Labels(), iterator.AtT(), fh.Count, fh.Sum, fh.PositiveSpans, fh.PositiveBuckets))
			default:
				t.Fatalf("unexpected value type %v", typ)
			}
		}
		require.NoError(t, iterator.Err())
	}
	require.NoError(t, set.Err())
	return out
}

// otlpAllSeriesRequire renders every retained sample of every series.
func otlpAllSeriesRequire(t *testing.T, q storage.Queryable) []string {
	t.Helper()
	querier, err := q.Querier(math.MinInt64, math.MaxInt64)
	require.NoError(t, err)
	defer func() { require.NoError(t, querier.Close()) }()
	set := querier.Select(context.Background(), true, nil,
		labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+"))
	out := []string{}
	for set.Next() {
		out = append(out, set.At().Labels().String())
	}
	require.NoError(t, set.Err())
	return out
}

func TestOTLPHistogramValidationMatchesStock(t *testing.T) {
	stores := []struct {
		name     string
		newStore func(t *testing.T) (storage.AppendableV2, storage.Queryable)
	}{
		{
			name: "stock_tsdb",
			newStore: func(t *testing.T) (storage.AppendableV2, storage.Queryable) {
				db := teststorage.New(t)
				return db, db
			},
		},
		{
			name: "columnar",
			newStore: func(t *testing.T) (storage.AppendableV2, storage.Queryable) {
				store, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{FlushSamples: 1_000})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				return store, store
			},
		},
	}

	cases := []struct {
		name            string
		request         pmetricotlp.ExportRequest
		wantStatus      int
		wantErrContains string
		// wantHistogram, when set, is asserted exactly after a successful request.
		wantHistogram *histogram.Histogram
	}{
		{
			name:            "inconsistent integer histogram",
			request:         otlpExponentialHistogramRequest(4, 10.5, []uint64{1, 4}, false),
			wantStatus:      http.StatusInternalServerError,
			wantErrContains: "observations found in buckets",
		},
		{
			name:       "consistent integer histogram",
			request:    otlpExponentialHistogramRequest(5, 10.5, []uint64{1, 4}, false),
			wantStatus: http.StatusOK,
			wantHistogram: &histogram.Histogram{
				Count: 5, Sum: 10.5, Schema: 0, ZeroThreshold: 1e-128,
				PositiveSpans:   []histogram.Span{{Offset: 1, Length: 2}},
				PositiveBuckets: []int64{1, 3},
			},
		},
		{
			name:            "mixed valid gauge and inconsistent histogram",
			request:         otlpExponentialHistogramRequest(4, 10.5, []uint64{1, 4}, true),
			wantStatus:      http.StatusInternalServerError,
			wantErrContains: "observations found in buckets",
		},
		{
			name:       "zero count and empty buckets",
			request:    otlpExponentialHistogramRequest(0, 0, nil, false),
			wantStatus: http.StatusOK,
			wantHistogram: &histogram.Histogram{
				Count: 0, Sum: 0, Schema: 0, ZeroThreshold: 1e-128,
			},
		},
		{
			// The histogram model accepts a zero count with a non-zero sum; the
			// OTLP translator only warns. Stock TSDB accepts and stores it, so the
			// columnar route must not invent a stricter rule.
			name:       "zero count with non-zero sum",
			request:    otlpExponentialHistogramRequest(0, 10.5, nil, false),
			wantStatus: http.StatusOK,
			wantHistogram: &histogram.Histogram{
				Count: 0, Sum: 10.5, Schema: 0, ZeroThreshold: 1e-128,
			},
		},
	}

	for _, store := range stores {
		t.Run(store.name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					appendable, queryable := store.newStore(t)
					handler := NewOTLPWriteHandler(promslog.NewNopLogger(), nil, appendable, func() config.Config {
						return config.Config{}
					}, OTLPOptions{})

					recorder := otlpPost(t, handler, tc.request)
					stored := otlpAllSeriesRequire(t, queryable)
					t.Logf("status=%d stored=%v histogram=%v body=%q", recorder.Code, stored,
						otlpSeriesRequire(t, queryable, otlpHistogramName), recorder.Body.String())

					require.Equal(t, tc.wantStatus, recorder.Code)
					if tc.wantErrContains != "" {
						require.Contains(t, recorder.Body.String(), tc.wantErrContains)
						require.Empty(t, stored, "a rejected OTLP request must publish no series")
						return
					}
					require.Len(t, stored, 1)
					rendered := otlpSeriesRequire(t, queryable, otlpHistogramName)
					require.Len(t, rendered, 1)
					require.Equal(t,
						fmt.Sprintf("%s|%d|histogram|count=%d;sum=%v;spans=%v;buckets=%v",
							labels.FromStrings(labels.MetricName, otlpHistogramName), otlpHistogramTime,
							tc.wantHistogram.Count, tc.wantHistogram.Sum,
							tc.wantHistogram.PositiveSpans, tc.wantHistogram.PositiveBuckets),
						rendered[0])
				})
			}
		})
	}
}

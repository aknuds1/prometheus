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

package otap_test

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	colarspb "github.com/open-telemetry/otel-arrow/go/api/experimental/arrow/v1"
	"github.com/open-telemetry/otel-arrow/go/pkg/otel/arrow_record"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/storage/remote/otap"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/util/teststorage"
)

// TestIngestionEquivalence asserts the property the OTAP receiver depends on: the
// same metrics ingested over OTAP and over OTLP/HTTP produce the same stored
// samples and the same stored native metadata.
//
// This is the guard against the two transports drifting apart, which would be
// invisible to transport-level tests.
func TestIngestionEquivalence(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	cfgFunc := func() config.Config { return config.Config{OTLPConfig: config.DefaultOTLPConfig} }
	otlpOpts := remote.OTLPOptions{LookbackDelta: 5 * time.Minute}

	otlpDB := teststorage.New(t, func(opts *tsdb.Options) { opts.EnableNativeMetadata = true })
	otapDB := teststorage.New(t, func(opts *tsdb.Options) { opts.EnableNativeMetadata = true })

	md := generateEquivalenceMetrics(t)

	// Path 1: OTLP over HTTP.
	handler := remote.NewOTLPWriteHandler(logger, prometheus.NewRegistry(), otlpDB, cfgFunc, otlpOpts)
	payload, err := pmetricotlp.NewExportRequestFromMetrics(md).MarshalProto()
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/otlp/v1/metrics", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/x-protobuf")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	// Path 2: the same payload over OTAP, through the real gRPC service.
	client, _ := newTestReceiver(t, remote.NewMetricsConsumer(logger, prometheus.NewRegistry(), otapDB, cfgFunc, otlpOpts), nil, otap.ReceiverOptions{})
	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })
	batch, err := producer.BatchArrowRecordsFromMetrics(md)
	require.NoError(t, err)

	stream, err := client.ArrowMetrics(t.Context())
	require.NoError(t, err)
	require.NoError(t, stream.Send(batch))
	ack, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, colarspb.StatusCode_OK, ack.GetStatusCode(), ack.GetStatusMessage())
	require.NoError(t, stream.CloseSend())
	_, err = stream.Recv()
	require.ErrorIs(t, err, io.EOF)

	// Both transports must have produced the same series with the same samples.
	wantSamples := collectSamples(t, otlpDB)
	require.NotEmpty(t, wantSamples, "the test payload must produce samples, otherwise the comparison proves nothing")
	require.Equal(t, wantSamples, collectSamples(t, otapDB))

	// And the same metric metadata, which is the part that would silently diverge
	// if the OTAP path bypassed the shared translation.
	wantMeta := collectMetadata(t, otlpDB)
	require.NotEmpty(t, wantMeta, "the test payload must produce metadata, otherwise the comparison proves nothing")
	require.Equal(t, wantMeta, collectMetadata(t, otapDB))
}

func generateEquivalenceMetrics(t *testing.T) pmetric.Metrics {
	t.Helper()

	ts := pcommon.NewTimestampFromTime(time.Unix(1700000000, 0))

	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "equivalence-service")

	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("equivalence-scope")

	counter := sm.Metrics().AppendEmpty()
	counter.SetName("equivalence_requests_total")
	counter.SetDescription("Total requests.")
	counter.SetUnit("1")
	sum := counter.SetEmptySum()
	sum.SetIsMonotonic(true)
	sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	sdp := sum.DataPoints().AppendEmpty()
	sdp.SetTimestamp(ts)
	sdp.SetIntValue(17)
	sdp.Attributes().PutStr("path", "/api")

	gauge := sm.Metrics().AppendEmpty()
	gauge.SetName("equivalence_temperature")
	gauge.SetDescription("Current temperature.")
	gauge.SetUnit("Cel")
	gdp := gauge.SetEmptyGauge().DataPoints().AppendEmpty()
	gdp.SetTimestamp(ts)
	gdp.SetDoubleValue(21.5)

	hist := sm.Metrics().AppendEmpty()
	hist.SetName("equivalence_latency")
	hist.SetDescription("Request latency.")
	hist.SetUnit("s")
	h := hist.SetEmptyHistogram()
	h.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	hdp := h.DataPoints().AppendEmpty()
	hdp.SetTimestamp(ts)
	hdp.SetCount(2)
	hdp.SetSum(3.5)
	hdp.BucketCounts().FromRaw([]uint64{1, 1})

	return md
}

// collectSamples returns a stable, sorted representation of all stored samples.
func collectSamples(t *testing.T, db *teststorage.TestStorage) []string {
	t.Helper()

	q, err := db.Querier(math.MinInt64, math.MaxInt64)
	require.NoError(t, err)
	defer q.Close()

	var out []string
	ss := q.Select(t.Context(), false, nil, labels.MustNewMatcher(labels.MatchRegexp, "", ".+"))
	for ss.Next() {
		s := ss.At()
		it := s.Iterator(nil)
		for {
			switch it.Next() {
			case chunkenc.ValNone:
				require.NoError(t, it.Err())
				goto nextSeries
			case chunkenc.ValFloat:
				ts, v := it.At()
				out = append(out, fmt.Sprintf("%s @%d = %v", s.Labels(), ts, v))
			case chunkenc.ValHistogram:
				ts, h := it.AtHistogram(nil)
				out = append(out, fmt.Sprintf("%s @%d = %v", s.Labels(), ts, h))
			case chunkenc.ValFloatHistogram:
				ts, h := it.AtFloatHistogram(nil)
				out = append(out, fmt.Sprintf("%s @%d = %v", s.Labels(), ts, h))
			}
		}
	nextSeries:
	}
	require.NoError(t, ss.Err())

	sort.Strings(out)
	return out
}

// collectMetadata returns the stored native metadata per series, keyed by the
// series labels so that both transports can be compared without knowing the
// expected series beforehand.
func collectMetadata(t *testing.T, db *teststorage.TestStorage) map[string][]tsdb.NativeMetricMetadataVersion {
	t.Helper()

	series, truncated, err := db.NativeMetricMetadata(t.Context(), [][]*labels.Matcher{{
		labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+"),
	}}, 0)
	require.NoError(t, err)
	require.False(t, truncated)

	out := map[string][]tsdb.NativeMetricMetadataVersion{}
	for _, s := range series {
		if len(s.Versions) == 0 {
			continue
		}
		out[s.Labels.String()] = s.Versions
	}
	return out
}

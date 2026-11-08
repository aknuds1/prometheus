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
	"log/slog"
	"testing"
	"time"

	"github.com/open-telemetry/otel-arrow/go/pkg/otel/arrow_record"
	"github.com/open-telemetry/otel-arrow/go/pkg/record_message"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage/columnar"
)

// recordsFor produces the Arrow records of a batch, which is what the sink
// consumes. The producer is the client side of the same protocol.
func recordsFor(t *testing.T, md pmetric.Metrics) []*record_message.RecordMessage {
	t.Helper()

	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })

	batch, err := producer.BatchArrowRecordsFromMetrics(md)
	require.NoError(t, err)

	consumer := arrow_record.NewConsumer()
	t.Cleanup(func() { _ = consumer.Close() })

	records, err := consumer.Consume(batch)
	require.NoError(t, err)
	return records
}

func TestOTAPSinkAppendsGaugeAndSum(t *testing.T) {
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{FlushSamples: 1000})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "svc")
	rm.Resource().Attributes().PutStr("host.name", "host-1")

	sm := rm.ScopeMetrics().AppendEmpty()
	gauge := sm.Metrics().AppendEmpty()
	gauge.SetName("columnar_gauge")
	gauge.SetUnit("By")
	dp := gauge.SetEmptyGauge().DataPoints().AppendEmpty()
	dp.SetTimestamp(pcommon.Timestamp(1700000000_000_000_000))
	dp.SetDoubleValue(21.5)
	dp.Attributes().PutStr("path", "/x")

	sumMetric := sm.Metrics().AppendEmpty()
	sumMetric.SetName("columnar_requests_total")
	sum := sumMetric.SetEmptySum()
	sum.SetIsMonotonic(true)
	sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	sdp := sum.DataPoints().AppendEmpty()
	sdp.SetTimestamp(pcommon.Timestamp(1700000000_000_000_000))
	sdp.SetIntValue(7)

	sink := columnar.NewOTAPSink(s, slog.New(slog.DiscardHandler))
	require.NoError(t, sink.ConsumeRecords(t.Context(), recordsFor(t, md)))

	series, err := s.SelectSeries(t.Context(), 0, 2_000_000_000_000, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 2)

	byName := map[string]columnar.Series{}
	for _, ser := range series {
		byName[ser.Labels.Get(labels.MetricName)] = ser
	}

	gaugeSeries, ok := byName["columnar_gauge"]
	require.True(t, ok)
	require.Equal(t, int64(1700000000000), gaugeSeries.Timestamps[0], "timestamps must be milliseconds")
	require.Equal(t, []float64{21.5}, gaugeSeries.Values)
	// Resource and data point attributes both become labels.
	require.Equal(t, "svc", gaugeSeries.Labels.Get("service.name"))
	require.Equal(t, "host-1", gaugeSeries.Labels.Get("host.name"))
	require.Equal(t, "/x", gaugeSeries.Labels.Get("path"))

	sumSeries, ok := byName["columnar_requests_total"]
	require.True(t, ok)
	require.Equal(t, []float64{7}, sumSeries.Values, "integer values are widened")
}

func TestOTAPSinkIgnoresUnsupportedSignals(t *testing.T) {
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	// A histogram-only batch has no NUMBER_DATA_POINTS record the sink can use, and
	// must be skipped rather than approximated.
	md := pmetric.NewMetrics()
	sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	hist := sm.Metrics().AppendEmpty()
	hist.SetName("columnar_histogram")
	h := hist.SetEmptyHistogram()
	h.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	hdp := h.DataPoints().AppendEmpty()
	hdp.SetTimestamp(pcommon.Timestamp(1700000000_000_000_000))
	hdp.SetCount(3)
	hdp.SetSum(6)
	hdp.BucketCounts().FromRaw([]uint64{1, 2})

	sink := columnar.NewOTAPSink(s, slog.New(slog.DiscardHandler))
	require.NoError(t, sink.ConsumeRecords(t.Context(), recordsFor(t, md)))

	series, err := s.SelectSeries(t.Context(), 0, 2_000_000_000_000, nil, 0)
	require.NoError(t, err)
	require.Empty(t, series, "unsupported signals must be dropped, not approximated")
}

func TestOTAPSinkHandlesSuccessiveBatches(t *testing.T) {
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{FlushSamples: 2})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	sink := columnar.NewOTAPSink(s, slog.New(slog.DiscardHandler))

	build := func(base int64) pmetric.Metrics {
		md := pmetric.NewMetrics()
		sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
		m := sm.Metrics().AppendEmpty()
		m.SetName("successive_metric")
		g := m.SetEmptyGauge()
		for i := range 3 {
			dp := g.DataPoints().AppendEmpty()
			dp.SetTimestamp(pcommon.Timestamp(uint64(time.Unix(base+int64(i), 0).UnixNano())))
			dp.SetDoubleValue(float64(base + int64(i)))
		}
		return md
	}

	// Reuse one producer, as a real client does: later batches carry only records,
	// not the schema.
	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })
	consumer := arrow_record.NewConsumer()
	t.Cleanup(func() { _ = consumer.Close() })

	for _, base := range []int64{1000, 2000} {
		batch, err := producer.BatchArrowRecordsFromMetrics(build(base))
		require.NoError(t, err)
		records, err := consumer.Consume(batch)
		require.NoError(t, err)
		require.NoError(t, sink.ConsumeRecords(t.Context(), records))
	}

	series, err := s.SelectSeries(t.Context(), 0, 4_000_000, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1, "both batches must land in the same series")
	require.Equal(t, []int64{1000000, 1001000, 1002000, 2000000, 2001000, 2002000}, series[0].Timestamps)
	require.Equal(t, []float64{1000, 1001, 1002, 2000, 2001, 2002}, series[0].Values)
}

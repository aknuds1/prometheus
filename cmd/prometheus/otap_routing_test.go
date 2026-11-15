// Copyright 2026 The Prometheus Authors
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

package main

// OTAP receiver routing integration tests.
//
// These run the real OTAP receiver (gRPC service, stream decoder, record sink)
// against the real columnar-only local storage wiring, and prove how many times a
// logical input sample reaches the store. A performance measurement of OTAP
// ingestion is meaningless if the same sample is appended twice, and a duplicate
// that is merely rejected by the store looks identical to a second append from the
// outside, so the tests count route calls and stored samples separately.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	colarspb "github.com/open-telemetry/otel-arrow/go/api/experimental/arrow/v1"
	"github.com/open-telemetry/otel-arrow/go/pkg/otel/arrow_record"
	"github.com/open-telemetry/otel-arrow/go/pkg/record_message"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	uatomic "go.uber.org/atomic"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/storage/remote/otap"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

// newLiveConfig returns the reloadable configuration the OTLP translation settings
// are read from, as the server holds it for the OTAP receiver.
func newLiveConfig(t *testing.T) *uatomic.Pointer[config.Config] {
	t.Helper()
	return uatomic.NewPointer(&config.Config{OTLPConfig: config.DefaultOTLPConfig})
}

// otapRoutingFixture is the logical input of every routing test: four series over
// three timestamps, split over a gauge and a cumulative sum, one shared Resource
// and one Scope.
type otapRoutingFixture struct {
	metrics pmetric.Metrics
	// samples is the number of logical samples in the batch.
	samples int
	series  int
	mint    int64
	maxt    int64
}

// otapRoutingNamePrefix marks the fixture's own metric names, so stored series the
// OTLP translator adds (target_info) can be reported separately.
const otapRoutingNamePrefix = "otap_routing_"

func newOTAPRoutingFixture() otapRoutingFixture {
	const base = int64(1_700_000_000_000)

	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "checkout")
	rm.Resource().Attributes().PutStr("host.name", "host-1")
	rm.Resource().Attributes().PutStr("region", "east")

	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("prometheus.otap.routing")
	sm.Scope().SetVersion("1.0.0")

	gauge := sm.Metrics().AppendEmpty()
	gauge.SetName("otap_routing_gauge")
	gauge.SetUnit("1")
	gaugePoints := gauge.SetEmptyGauge().DataPoints()
	for _, path := range []string{"/a", "/b"} {
		for step := range 3 {
			dp := gaugePoints.AppendEmpty()
			dp.SetTimestamp(pcommon.Timestamp((base + int64(step)*15_000) * int64(time.Millisecond)))
			dp.SetDoubleValue(float64(step) + 0.5)
			dp.Attributes().PutStr("path", path)
		}
	}

	sum := sm.Metrics().AppendEmpty()
	sum.SetName("otap_routing_requests_total")
	sum.SetUnit("1")
	summer := sum.SetEmptySum()
	summer.SetIsMonotonic(true)
	summer.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	sumPoints := summer.DataPoints()
	for _, method := range []string{"GET", "POST"} {
		for step := range 3 {
			dp := sumPoints.AppendEmpty()
			dp.SetTimestamp(pcommon.Timestamp((base + int64(step)*15_000) * int64(time.Millisecond)))
			dp.SetIntValue(int64(100 + step))
			dp.Attributes().PutStr("method", method)
		}
	}

	return otapRoutingFixture{
		metrics: md,
		samples: 12,
		series:  4,
		mint:    base,
		maxt:    base + 2*15_000,
	}
}

// otapRouteCounters counts how often each ingestion route was invoked. The
// decorators below still call the real sink and the real consumer, so counting a
// route must not change what it does.
type otapRouteCounters struct {
	sink     uatomic.Int64
	consumer uatomic.Int64
}

func (c *otapRouteCounters) String() string {
	return fmt.Sprintf("sinkCalls=%d consumerCalls=%d", c.sink.Load(), c.consumer.Load())
}

type countedSink struct {
	inner otap.RecordSink
	n     *uatomic.Int64
}

func (s countedSink) ConsumeRecords(ctx context.Context, records []*record_message.RecordMessage) error {
	s.n.Add(1)
	return s.inner.ConsumeRecords(ctx, records)
}

type countedConsumer struct {
	inner consumer.Metrics
	n     *uatomic.Int64
}

func (c countedConsumer) ConsumeMetrics(ctx context.Context, md pmetric.Metrics) error {
	c.n.Add(1)
	return c.inner.ConsumeMetrics(ctx, md)
}

func (c countedConsumer) Capabilities() consumer.Capabilities { return c.inner.Capabilities() }

// otapRoutingStorage is the columnar-only local storage the server uses in
// prototype mode: one store, no TSDB, WAL disabled so the retention state of the
// test does not depend on a WAL directory.
type otapRoutingStorage struct {
	storage *columnar.Storage
	dir     string
}

func newOTAPRoutingStorage(t *testing.T) otapRoutingStorage {
	t.Helper()
	dir := t.TempDir()
	st, err := columnar.NewStorage(dir, columnar.StorageOptions{
		FlushSamples: 1 << 40, // Keep the fixture in the head; the WAL is off.
		DisableWAL:   true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	return otapRoutingStorage{storage: st, dir: dir}
}

// consumerFor returns the ordinary OTLP ingestion consumer exactly as the server
// constructs it for the OTAP receiver and for OTLP/HTTP.
func (s otapRoutingStorage) consumerFor(t *testing.T, reg *prometheus.Registry, counts *otapRouteCounters) consumer.Metrics {
	t.Helper()
	liveConfig := newLiveConfig(t)
	return countedConsumer{
		inner: remote.NewMetricsConsumer(
			slog.New(slog.DiscardHandler),
			reg,
			s.storage,
			func() config.Config { return *liveConfig.Load() },
			remote.OTLPOptions{LookbackDelta: 5 * time.Minute},
		),
		n: &counts.consumer,
	}
}

// nativeSinkFor returns the native OTAP record sink exactly as the server
// constructs it, when the columnar prototype is enabled.
func (s otapRoutingStorage) nativeSinkFor(t *testing.T, counts *otapRouteCounters) otap.RecordSink {
	t.Helper()
	sink := columnarRecordSink(s.storage, slog.New(slog.DiscardHandler), newLiveConfig(t), 5*time.Minute, false)
	require.NotNil(t, sink, "the columnar prototype must provide a record sink")
	return countedSink{inner: sink, n: &counts.sink}
}

// appendBatch sends the fixture as one OTAP batch through a real gRPC receiver and
// returns the batch's acknowledgement.
func appendBatch(t *testing.T, client colarspb.ArrowMetricsServiceClient, md pmetric.Metrics) *colarspb.BatchStatus {
	t.Helper()

	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })

	stream, err := client.ArrowMetrics(t.Context())
	require.NoError(t, err)
	batch, err := producer.BatchArrowRecordsFromMetrics(md)
	require.NoError(t, err)
	require.NoError(t, stream.Send(batch))
	require.NoError(t, stream.CloseSend())

	ack, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, batch.GetBatchId(), ack.GetBatchId())
	_, err = stream.Recv()
	require.ErrorIs(t, err, io.EOF)
	return ack
}

// storedSeriesPoints is one stored series with its complete timestamp/value pairs.
type storedSeriesPoints struct {
	labels labels.Labels
	points []string
}

// storedSeries drains the whole store and returns every selected series with its
// complete samples. It keeps series the fixture does not own, such as the
// target_info stream the OTLP translator adds, because filtering them out could hide
// exactly the duplication these tests exist to find.
func storedSeries(t *testing.T, st storage.Queryable, mint, maxt int64) []storedSeriesPoints {
	t.Helper()

	q, err := st.Querier(mint, maxt)
	require.NoError(t, err)
	defer func() { require.NoError(t, q.Close()) }()

	set := q.Select(context.Background(), true, nil, labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+"))
	var out []storedSeriesPoints
	for set.Next() {
		s := set.At()
		it := s.Iterator(nil)
		var points []string
		for vt := it.Next(); vt != chunkenc.ValNone; vt = it.Next() {
			require.Equal(t, chunkenc.ValFloat, vt, "the fixture carries floats only")
			ts, v := it.At()
			points = append(points, strconv.FormatInt(ts, 10)+"="+strconv.FormatFloat(v, 'g', -1, 64))
		}
		require.NoError(t, it.Err())
		require.NotEmpty(t, points, "series %s has no samples", s.Labels())
		out = append(out, storedSeriesPoints{labels: s.Labels(), points: points})
	}
	require.NoError(t, set.Err())
	return out
}

// fixtureSeries keeps the stored series that belong to the fixture metric family,
// so the target_info stream the OTLP translator adds is reported separately.
func fixtureSeries(stored []storedSeriesPoints) []storedSeriesPoints {
	var out []storedSeriesPoints
	for _, s := range stored {
		if strings.HasPrefix(s.labels.Get(labels.MetricName), otapRoutingNamePrefix) {
			out = append(out, s)
		}
	}
	return out
}

// identityVariants counts the stored series per identity, where an identity is a
// label set with the columnar Resource identity label removed. A series stored once
// by the native route and once by the OTLP route therefore appears as one identity
// with two variants.
func identityVariants(series []storedSeriesPoints) map[string]int {
	out := map[string]int{}
	for _, s := range series {
		key := labels.NewBuilder(s.labels).Del(columnar.OTELResourceLabel).Labels().String()
		out[key]++
	}
	return out
}

// countPoints counts stored samples over the given series.
func countPoints(series []storedSeriesPoints) int {
	n := 0
	for _, s := range series {
		n += len(s.points)
	}
	return n
}

// TestOTAPReceiverDuplicateRouteStoresEverySampleTwice documents the defect that
// otapIngestionRoutes exists to prevent. With both the native record sink and the
// ordinary OTLP consumer pointed at the same columnar store, every batch is
// appended by the sink and then translated and appended again by the consumer. The
// two appends do not collide, because the native sink carries the OTLP Resource
// identity label and the OTLP translator does not, so the batch is acknowledged as
// successful while the store holds two series per logical series and twice as many
// samples.
//
// Nothing here relies on duplicate-timestamp rejection: the test compares complete
// stored series and samples, and the route counters prove that both routes ran.
func TestOTAPReceiverDuplicateRouteStoresEverySampleTwice(t *testing.T) {
	f := newOTAPRoutingFixture()
	local := newOTAPRoutingStorage(t)
	var counts otapRouteCounters
	reg := prometheus.NewRegistry()

	client := startOTAPReceiver(t, reg, local.consumerFor(t, reg, &counts), local.nativeSinkFor(t, &counts))
	ack := appendBatch(t, client, f.metrics)

	require.Equal(t, colarspb.StatusCode_OK, ack.GetStatusCode(),
		"both appends succeed, so the client sees no error at all: %s", ack.GetStatusMessage())
	require.Equal(t, int64(1), counts.sink.Load(), "the native sink must run once")
	require.Equal(t, int64(1), counts.consumer.Load(), "the OTLP consumer must run once, not be skipped")

	stored := storedSeries(t, local.storage, f.mint, f.maxt)
	fixture := fixtureSeries(stored)
	variants := identityVariants(fixture)

	require.Len(t, variants, f.series, "the fixture has four logical series")
	for identity, count := range variants {
		require.Equal(t, 2, count, "identity %s is stored twice: with and without its Resource identity", identity)
	}
	require.Equal(t, 2*f.samples, countPoints(fixture), "every logical sample is stored twice")

	// The native route's copy is the one that carries the Resource identity, which is
	// also why the two appends do not collide.
	withIdentity := 0
	for _, s := range fixture {
		if s.labels.Has(columnar.OTELResourceLabel) {
			withIdentity++
		}
	}
	require.Equal(t, f.series, withIdentity, "the native route stores every fixture series with its Resource identity")
	require.Equal(t, uint64(2), local.storage.ReadStats().CommitCount, "two transactions were committed")

	t.Logf("duplicate route reproduction: ack=%s %s storedSeries=%d storedSamples=%d "+
		"fixtureSeries=%d withIdentity=%d commits=%d",
		ack.GetStatusCode(), counts.String(), len(stored), countPoints(stored), len(fixture), withIdentity,
		local.storage.ReadStats().CommitCount)
}

// TestOTAPReceiverOrdinaryOTLPRouteAppendsAll is the single-route control: the same
// batch through the same receiver with only the ordinary OTLP consumer attached, as
// when the columnar prototype is off. Every logical sample is stored once and the
// batch is acknowledged. The series carry no Resource identity label, because the
// OTLP translation path does not produce one.
func TestOTAPReceiverOrdinaryOTLPRouteAppendsAll(t *testing.T) {
	f := newOTAPRoutingFixture()
	local := newOTAPRoutingStorage(t)
	var counts otapRouteCounters
	reg := prometheus.NewRegistry()

	client := startOTAPReceiver(t, reg, local.consumerFor(t, reg, &counts), nil)
	ack := appendBatch(t, client, f.metrics)

	require.Equal(t, colarspb.StatusCode_OK, ack.GetStatusCode(), ack.GetStatusMessage())
	require.Equal(t, int64(0), counts.sink.Load())
	require.Equal(t, int64(1), counts.consumer.Load())

	stored := storedSeries(t, local.storage, f.mint, f.maxt)
	fixture := fixtureSeries(stored)
	require.Equal(t, map[string]int{}, mapDiff(identityVariants(fixture), 1), "one stored series per logical series")
	require.Equal(t, f.samples, countPoints(fixture))
	require.Equal(t, uint64(1), local.storage.ReadStats().CommitCount)
	for _, s := range fixture {
		require.False(t, s.labels.Has(columnar.OTELResourceLabel),
			"the OTLP translation path carries no columnar Resource identity: %s", s.labels)
	}
	t.Logf("ordinary OTLP route: ack=%s %s storedSamples=%d", ack.GetStatusCode(), counts.String(), countPoints(fixture))
}

// mapDiff asserts that every variant count equals want and returns the offending
// entries, so a failure names the identity instead of only a count.
func mapDiff(variants map[string]int, want int) map[string]int {
	bad := map[string]int{}
	for identity, count := range variants {
		if count != want {
			bad[identity] = count
		}
	}
	return bad
}

// TestOTAPReceiverColumnarRouteExclusive proves the production route selection: with
// the columnar prototype on, the receiver has the native record sink and no OTLP
// consumer, so every logical input sample is appended exactly once.
func TestOTAPReceiverColumnarRouteExclusive(t *testing.T) {
	f := newOTAPRoutingFixture()
	local := newOTAPRoutingStorage(t)
	var counts otapRouteCounters
	reg := prometheus.NewRegistry()

	c, sink := otapIngestionRoutes(local.storage, slog.New(slog.DiscardHandler), newLiveConfig(t), 5*time.Minute, false,
		func() consumer.Metrics { return local.consumerFor(t, reg, &counts) })
	require.Nil(t, c, "the OTLP consumer must not be attached when the native route is")
	require.NotNil(t, sink)
	sink = countedSink{inner: sink, n: &counts.sink}

	client := startOTAPReceiver(t, reg, c, sink)
	ack := appendBatch(t, client, f.metrics)

	require.Equal(t, colarspb.StatusCode_OK, ack.GetStatusCode(), ack.GetStatusMessage())
	require.Equal(t, int64(1), counts.sink.Load(), "the native sink is the single route")
	require.Equal(t, int64(0), counts.consumer.Load(), "the OTLP consumer must never run")

	stored := storedSeries(t, local.storage, f.mint, f.maxt)
	fixture := fixtureSeries(stored)
	require.Equal(t, map[string]int{}, mapDiff(identityVariants(fixture), 1), "one stored series per logical series")
	require.Equal(t, f.samples, countPoints(fixture), "every logical sample is stored exactly once")
	for _, s := range fixture {
		require.True(t, s.labels.Has(columnar.OTELResourceLabel),
			"the native route keeps the Resource identity on every series: %s", s.labels)
	}
	require.Equal(t, uint64(1), local.storage.ReadStats().CommitCount, "one transaction")

	// The store must return the input samples, not a subset: compare the complete
	// set of stored timestamps and values against the fixture's own data points.
	want := map[string][]string{}
	for _, rm := range f.metrics.ResourceMetrics().All() {
		for _, sm := range rm.ScopeMetrics().All() {
			for _, m := range sm.Metrics().All() {
				switch m.Type() {
				case pmetric.MetricTypeGauge:
					for _, dp := range m.Gauge().DataPoints().All() {
						want[dp.Attributes().AsRaw()["path"].(string)] = append(want[dp.Attributes().AsRaw()["path"].(string)],
							strconv.FormatInt(dp.Timestamp().AsTime().UnixMilli(), 10)+"="+
								strconv.FormatFloat(dp.DoubleValue(), 'g', -1, 64))
					}
				case pmetric.MetricTypeSum:
					for _, dp := range m.Sum().DataPoints().All() {
						method := dp.Attributes().AsRaw()["method"].(string)
						want[method] = append(want[method], strconv.FormatInt(dp.Timestamp().AsTime().UnixMilli(), 10)+"="+
							strconv.FormatFloat(float64(dp.IntValue()), 'g', -1, 64))
					}
				}
			}
		}
	}
	require.Len(t, want, f.series)
	got := map[string][]string{}
	for _, s := range fixture {
		got[seriesKey(s.labels)] = s.points
	}
	require.Equal(t, want, got, "complete stored result matches the input data points")
	t.Logf("columnar route: ack=%s %s storedSeries=%d storedSamples=%d commits=%d",
		ack.GetStatusCode(), counts.String(), len(fixture), countPoints(fixture), local.storage.ReadStats().CommitCount)
}

// seriesKey identifies a fixture series by its distinguishing label, matching the
// per-series attribute in the input data points.
func seriesKey(ls labels.Labels) string {
	for _, name := range []string{"path", "method"} {
		if v := ls.Get(name); v != "" {
			return v
		}
	}
	return ""
}

// otapRejectionTimestamp is the instant every unsupported-payload fixture uses.
const otapRejectionTimestamp = pcommon.Timestamp(1_700_000_000_000_000_000)

// newOTAPRejectionFixture returns an empty batch whose Resource and Scope match the
// routing fixture. The Resource carries identifying and descriptive attributes, so a
// sink that committed a supported prefix would also commit a target_info sample and
// a metadata version, and the rejection tests would see them.
func newOTAPRejectionFixture() (pmetric.Metrics, pmetric.ScopeMetrics) {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "checkout")
	rm.Resource().Attributes().PutStr("host.name", "host-1")
	rm.Resource().Attributes().PutStr("region", "east")
	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("prometheus.otap.rejection")
	return md, sm
}

// appendOTAPGauge adds a gauge with one double-valued point and returns it.
func appendOTAPGauge(sm pmetric.ScopeMetrics, name string, value float64) pmetric.NumberDataPoint {
	metric := sm.Metrics().AppendEmpty()
	metric.SetName(name)
	dp := metric.SetEmptyGauge().DataPoints().AppendEmpty()
	dp.SetTimestamp(otapRejectionTimestamp)
	dp.SetDoubleValue(value)
	return dp
}

// appendOTAPClassicHistogram adds one cumulative classic histogram with one point.
func appendOTAPClassicHistogram(sm pmetric.ScopeMetrics, name string) {
	metric := sm.Metrics().AppendEmpty()
	metric.SetName(name)
	histogram := metric.SetEmptyHistogram()
	histogram.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	dp := histogram.DataPoints().AppendEmpty()
	dp.SetTimestamp(otapRejectionTimestamp)
	dp.SetCount(4)
	dp.SetSum(10)
	dp.BucketCounts().FromRaw([]uint64{1, 2, 1})
	dp.ExplicitBounds().FromRaw([]float64{1, 2})
}

// appendOTAPSummary adds one summary with one point.
func appendOTAPSummary(sm pmetric.ScopeMetrics, name string) {
	metric := sm.Metrics().AppendEmpty()
	metric.SetName(name)
	dp := metric.SetEmptySummary().DataPoints().AppendEmpty()
	dp.SetTimestamp(otapRejectionTimestamp)
	dp.SetCount(2)
	dp.SetSum(3)
	quantile := dp.QuantileValues().AppendEmpty()
	quantile.SetQuantile(0.5)
	quantile.SetValue(1.5)
}

// appendOTAPExponentialHistogram adds one cumulative exponential histogram with one
// point.
func appendOTAPExponentialHistogram(sm pmetric.ScopeMetrics, name string) {
	metric := sm.Metrics().AppendEmpty()
	metric.SetName(name)
	histogram := metric.SetEmptyExponentialHistogram()
	histogram.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	dp := histogram.DataPoints().AppendEmpty()
	dp.SetTimestamp(otapRejectionTimestamp)
	dp.SetCount(2)
	dp.SetSum(3)
	dp.SetScale(1)
	dp.Positive().SetOffset(0)
	dp.Positive().BucketCounts().FromRaw([]uint64{1, 1})
}

// TestOTAPReceiverRejectsUnsupportedBatch proves the native route's all-or-nothing
// contract end to end, through the real gRPC receiver, stream decoder and columnar
// sink. Before this contract the sink silently dropped every case below and the
// client still saw OK, so a producer could believe data was stored that never was.
//
// Both routes are attached so the test also proves a rejected batch cannot fall back
// to the ordinary OTLP consumer: that would be a second route hidden inside a
// rejection.
func TestOTAPReceiverRejectsUnsupportedBatch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		metrics func() pmetric.Metrics
		want    string
	}{
		{
			name: "classic histogram",
			metrics: func() pmetric.Metrics {
				md, sm := newOTAPRejectionFixture()
				appendOTAPClassicHistogram(sm, "otap_routing_histogram")
				return md
			},
			want: "classic histogram data points (1 row)",
		},
		{
			name: "summary",
			metrics: func() pmetric.Metrics {
				md, sm := newOTAPRejectionFixture()
				appendOTAPSummary(sm, "otap_routing_summary")
				return md
			},
			want: "summary data points (1 row)",
		},
		{
			name: "exponential histogram",
			metrics: func() pmetric.Metrics {
				md, sm := newOTAPRejectionFixture()
				appendOTAPExponentialHistogram(sm, "otap_routing_exponential_histogram")
				return md
			},
			want: "exponential histogram data points (1 row)",
		},
		{
			name: "supported numbers mixed with a classic histogram",
			metrics: func() pmetric.Metrics {
				md, sm := newOTAPRejectionFixture()
				appendOTAPGauge(sm, "otap_routing_gauge", 1)
				appendOTAPClassicHistogram(sm, "otap_routing_histogram")
				return md
			},
			want: "classic histogram data points (1 row)",
		},
		{
			name: "supported number carrying an exemplar",
			metrics: func() pmetric.Metrics {
				md, sm := newOTAPRejectionFixture()
				points := appendOTAPGauge(sm, "otap_routing_gauge", 1).Exemplars()
				exemplar := points.AppendEmpty()
				exemplar.SetTimestamp(otapRejectionTimestamp - 1_000_000)
				exemplar.SetDoubleValue(0.5)
				return md
			},
			want: "number data point exemplars (1 row)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := newOTAPRoutingStorage(t)
			var counts otapRouteCounters
			reg := prometheus.NewRegistry()

			client := startOTAPReceiver(t, reg, local.consumerFor(t, reg, &counts), local.nativeSinkFor(t, &counts))
			ack := appendBatch(t, client, tc.metrics())

			require.NotEqual(t, colarspb.StatusCode_OK, ack.GetStatusCode(),
				"an unsupported batch must not be acknowledged as stored")
			require.Contains(t, ack.GetStatusMessage(), tc.want,
				"the acknowledgement must name the payload the sink cannot convert")
			require.Equal(t, int64(1), counts.sink.Load(), "the native sink is called once")
			require.Equal(t, int64(0), counts.consumer.Load(), "a rejected batch must not fall back to the OTLP consumer")

			stored := storedSeries(t, local.storage, 0, 2_000_000_000_000)
			require.Empty(t, stored, "a rejected batch must not store a supported prefix or a target_info sample")
			require.Zero(t, local.storage.ReadStats().CommitCount, "a rejected batch must not reach a store transaction")
			t.Logf("rejected %s: status=%s message=%q commits=%d",
				tc.name, ack.GetStatusCode(), ack.GetStatusMessage(), local.storage.ReadStats().CommitCount)
		})
	}
}

// TestOTAPReceiverAcceptsEmptyBatch pins the other side of the contract: an input
// that carries no data point is still a successful no-op on the native route, so the
// rejection above cannot be explained by treating every batch as unreadable.
func TestOTAPReceiverAcceptsEmptyBatch(t *testing.T) {
	local := newOTAPRoutingStorage(t)
	var counts otapRouteCounters
	reg := prometheus.NewRegistry()

	c, sink := otapIngestionRoutes(local.storage, slog.New(slog.DiscardHandler), newLiveConfig(t), 5*time.Minute, false,
		func() consumer.Metrics { return local.consumerFor(t, reg, &counts) })
	require.Nil(t, c)
	require.NotNil(t, sink)
	sink = countedSink{inner: sink, n: &counts.sink}

	client := startOTAPReceiver(t, reg, c, sink)
	ack := appendBatch(t, client, pmetric.NewMetrics())

	require.Equal(t, colarspb.StatusCode_OK, ack.GetStatusCode(), ack.GetStatusMessage())
	require.Equal(t, int64(1), counts.sink.Load(), "the native sink still sees the empty batch")
	require.Equal(t, int64(0), counts.consumer.Load())
	require.Empty(t, storedSeries(t, local.storage, 0, 2_000_000_000_000))
	require.Zero(t, local.storage.ReadStats().CommitCount)
}

// TestOTAPReceiverOrdinaryOTLPRouteStoresHistogram is the control for the rejection
// above: classic histograms stay fully supported through the ordinary OTLP consumer,
// and their stored output is unchanged. The native route's refusal therefore removes
// no capability the receiver had, it only stops pretending the native route had it.
func TestOTAPReceiverOrdinaryOTLPRouteStoresHistogram(t *testing.T) {
	md, sm := newOTAPRejectionFixture()
	appendOTAPClassicHistogram(sm, "otap_routing_histogram")

	local := newOTAPRoutingStorage(t)
	var counts otapRouteCounters
	reg := prometheus.NewRegistry()

	client := startOTAPReceiver(t, reg, local.consumerFor(t, reg, &counts), nil)
	ack := appendBatch(t, client, md)

	require.Equal(t, colarspb.StatusCode_OK, ack.GetStatusCode(), ack.GetStatusMessage())
	require.Equal(t, int64(1), counts.consumer.Load())

	got := map[string][]string{}
	for _, s := range storedSeries(t, local.storage, 0, 2_000_000_000_000) {
		got[s.labels.String()] = s.points
	}
	require.Equal(t, map[string][]string{
		`{__name__="otap_routing_histogram_bucket", job="checkout", le="+Inf"}`:       {"1700000000000=4"},
		`{__name__="otap_routing_histogram_bucket", job="checkout", le="1"}`:          {"1700000000000=1"},
		`{__name__="otap_routing_histogram_bucket", job="checkout", le="2"}`:          {"1700000000000=3"},
		`{__name__="otap_routing_histogram_count", job="checkout"}`:                   {"1700000000000=4"},
		`{__name__="otap_routing_histogram_sum", job="checkout"}`:                     {"1700000000000=10"},
		`{__name__="target_info", host_name="host-1", job="checkout", region="east"}`: {"1700000000000=1"},
	}, got, "the ordinary OTLP consumer must keep storing classic histograms exactly")
}

// startOTAPReceiver serves a real OTAP receiver over bufconn and returns a client
// for it. A nil sink means the receiver only has the ordinary OTLP consumer.
func startOTAPReceiver(t *testing.T, reg *prometheus.Registry, c consumer.Metrics, sink otap.RecordSink) colarspb.ArrowMetricsServiceClient {
	t.Helper()

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	receiver := otap.NewReceiver(slog.New(slog.DiscardHandler), reg, c,
		otap.ReceiverOptions{RecordSink: sink})
	receiver.RegisterServer(srv)

	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.Stop()
		<-served
	})

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	return colarspb.NewArrowMetricsServiceClient(conn)
}

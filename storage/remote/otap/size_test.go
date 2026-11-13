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
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	colarspb "github.com/open-telemetry/otel-arrow/go/api/experimental/arrow/v1"
	"github.com/open-telemetry/otel-arrow/go/pkg/otel/arrow_record"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"google.golang.org/protobuf/proto"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/storage/remote/otap"
	"github.com/prometheus/prometheus/util/teststorage"
)

// TestWireSizeComparison reports the encoded size of the same metrics as OTLP,
// OTLP with zstd, and OTAP.
//
// OTAP is measured both cold and warm: the first batch on a stream carries the
// full Arrow schema and dictionaries, while later batches only carry delta
// dictionaries. Comparing a single cold batch against OTLP would be wrong in
// both directions - it inflates OTAP for small payloads and it is not how a
// stream behaves in practice - so the warm batch is the number that matters.
//
// OTAP is not compressed here: compression is a transport concern that the
// exporter applies to each payload, and this repository carries the producer's
// default output. That makes the comparison conservative, pitting uncompressed
// OTAP against compressed OTLP.
func TestWireSizeComparison(t *testing.T) {
	zstdEncoder, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	t.Cleanup(func() { zstdEncoder.Close() })

	for _, tc := range []struct {
		name  string
		build func(offset int) pmetric.Metrics
	}{
		{name: "single-series-100-points", build: singleSeriesMetrics},
		{name: "50-series-shared-resource-10-points", build: sharedResourceMetrics},
		{name: "500-series-unique-attrs-1-point", build: highCardinalityMetrics},
	} {
		t.Run(tc.name, func(t *testing.T) {
			producer := arrow_record.NewProducer()
			t.Cleanup(func() { require.NoError(t, producer.Close()) })

			coldBatch, err := producer.BatchArrowRecordsFromMetrics(tc.build(0))
			require.NoError(t, err)
			warmBatch, err := producer.BatchArrowRecordsFromMetrics(tc.build(1))
			require.NoError(t, err)

			otlpBytes, err := pmetricotlp.NewExportRequestFromMetrics(tc.build(1)).MarshalProto()
			require.NoError(t, err)
			otlpZstdBytes := zstdEncoder.EncodeAll(otlpBytes, nil)

			coldOTAP, warmOTAP := proto.Size(coldBatch), proto.Size(warmBatch)
			warmOTAPBytes, err := proto.Marshal(warmBatch)
			require.NoError(t, err)
			warmOTAPZstd := len(zstdEncoder.EncodeAll(warmOTAPBytes, nil))

			t.Logf("OTLP=%d B, OTLP+zstd=%d B, OTAP cold=%d B, OTAP warm=%d B, OTAP warm+zstd=%d B, "+
				"OTAP+zstd/OTLP+zstd=%.2fx",
				len(otlpBytes), len(otlpZstdBytes), coldOTAP, warmOTAP, warmOTAPZstd,
				float64(warmOTAPZstd)/float64(len(otlpZstdBytes)),
			)

			// A warm batch must be smaller than the uncompressed OTLP payload for the
			// same data. Anything else means a steady-state stream adds bytes for no
			// gain, whatever the cold-start cost.
			require.Less(t, warmOTAP, len(otlpBytes),
				"a warm OTAP batch must not be larger than the uncompressed OTLP payload it replaces")
		})
	}
}

// BenchmarkDecodeBatch measures decoding a fixed batch into OTLP metrics,
// without appending. This isolates the cost OTAP adds over an OTLP request.
func BenchmarkDecodeBatch(b *testing.B) {
	sink := &discardConsumer{}
	stream := otap.NewStreamConsumer(sink)
	b.Cleanup(func() { require.NoError(b, stream.Close()) })
	batches := generateBatchRing(b, stream)

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if err := stream.ConsumeBatch(b.Context(), batches[i%len(batches)]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecodeAndAppend measures the full OTAP path: decode, translate, and
// append into an in-memory appendable. Appends of already-seen samples are
// rejected by the appendable, which keeps this comparable to the decode cost
// without hiding the translation work.
func BenchmarkDecodeAndAppend(b *testing.B) {
	sink := remote.NewMetricsConsumer(
		slog.New(slog.DiscardHandler),
		prometheus.NewRegistry(),
		teststorage.NewAppendable(),
		func() config.Config { return config.Config{OTLPConfig: config.DefaultOTLPConfig} },
		remote.OTLPOptions{},
	)
	stream := otap.NewStreamConsumer(sink)
	b.Cleanup(func() { require.NoError(b, stream.Close()) })
	batches := generateBatchRing(b, stream)

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		// Errors are deliberately ignored: this measures the cost of the path, and
		// the appendable rejects duplicate samples after the first iteration.
		_ = stream.ConsumeBatch(b.Context(), batches[i%len(batches)])
	}
}

// BenchmarkInspectOTLP measures the equivalent OTLP path for comparison:
// unmarshalling the protobuf request and appending it through the same
// ingestion consumer. Together with BenchmarkDecodeAndAppend this is the
// end-to-end ingestion comparison between the two transports.
func BenchmarkIngestOTLP(b *testing.B) {
	sink := remote.NewMetricsConsumer(
		slog.New(slog.DiscardHandler),
		prometheus.NewRegistry(),
		teststorage.NewAppendable(),
		func() config.Config { return config.Config{OTLPConfig: config.DefaultOTLPConfig} },
		remote.OTLPOptions{},
	)

	payloads := make([][]byte, 0, 16)
	for i := range 16 {
		payload, err := pmetricotlp.NewExportRequestFromMetrics(sharedResourceMetrics(i)).MarshalProto()
		require.NoError(b, err)
		payloads = append(payloads, payload)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		req := pmetricotlp.NewExportRequest()
		if err := req.UnmarshalProto(payloads[i%len(payloads)]); err != nil {
			b.Fatal(err)
		}
		// Errors are deliberately ignored, as in BenchmarkDecodeAndAppend.
		_ = sink.ConsumeMetrics(b.Context(), req.Metrics())
	}
}

// generateBatchRing produces a ring of batches and feeds the first one to the
// stream so that the schema is installed, mirroring a stream whose connection has
// been established. A batch that carries a schema message can only be decoded once
// per stream, which is why the benchmarks do not simply replay a single batch.
func generateBatchRing(tb testing.TB, stream *otap.StreamConsumer) []*colarspb.BatchArrowRecords {
	tb.Helper()

	const ringSize = 16

	producer := arrow_record.NewProducer()
	tb.Cleanup(func() { require.NoError(tb, producer.Close()) })

	batches := make([]*colarspb.BatchArrowRecords, 0, ringSize)
	for i := range ringSize {
		batch, err := producer.BatchArrowRecordsFromMetrics(sharedResourceMetrics(i))
		require.NoError(tb, err)
		batches = append(batches, batch)
	}

	// The first batch carries the schema, so consume it once up front.
	require.NoError(tb, stream.ConsumeBatch(context.Background(), batches[0]))
	return batches[1:]
}

type discardConsumer struct{}

func (*discardConsumer) ConsumeMetrics(_ context.Context, _ pmetric.Metrics) error { return nil }

func (*discardConsumer) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

func singleSeriesMetrics(offset int) pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "size-test")
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("size_test_gauge")
	g := m.SetEmptyGauge()
	for i := range 100 {
		dp := g.DataPoints().AppendEmpty()
		dp.SetTimestamp(pcommon.NewTimestampFromTime(time.Unix(int64(1700000000+offset*1000+i), 0)))
		dp.SetDoubleValue(float64(i))
	}
	return md
}

func sharedResourceMetrics(offset int) pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	for k, v := range map[string]string{
		"service.name":           "size-test",
		"service.namespace":      "sizes",
		"deployment.environment": "prod",
		"k8s.cluster.name":       "cluster-a",
		"k8s.namespace.name":     "sizes",
	} {
		rm.Resource().Attributes().PutStr(k, v)
	}
	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("size-test-scope")
	sm.Scope().SetVersion("1.0.0")

	for i := range 50 {
		m := sm.Metrics().AppendEmpty()
		m.SetName(fmt.Sprintf("size_test_metric_%d", i))
		m.SetUnit("1")
		g := m.SetEmptyGauge()
		for j := range 10 {
			dp := g.DataPoints().AppendEmpty()
			dp.SetTimestamp(pcommon.NewTimestampFromTime(time.Unix(int64(1700000000+offset*1000+j), 0)))
			dp.SetDoubleValue(float64(j))
			dp.Attributes().PutStr("instance", fmt.Sprintf("instance-%d", i%5))
		}
	}
	return md
}

func highCardinalityMetrics(offset int) pmetric.Metrics {
	md := pmetric.NewMetrics()
	sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	for i := range 500 {
		m := sm.Metrics().AppendEmpty()
		m.SetName(fmt.Sprintf("size_test_unique_%d", i))
		dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
		dp.SetTimestamp(pcommon.NewTimestampFromTime(time.Unix(int64(1700000000+offset*1000), 0)))
		dp.SetDoubleValue(float64(i))
		dp.Attributes().PutStr("unique.id", fmt.Sprintf("id-%d", i))
		dp.Attributes().PutStr("request.path", fmt.Sprintf("/path/%d", i))
	}
	return md
}

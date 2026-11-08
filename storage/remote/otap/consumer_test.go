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
	"errors"
	"testing"
	"time"

	colarspb "github.com/open-telemetry/otel-arrow/go/api/experimental/arrow/v1"
	"github.com/open-telemetry/otel-arrow/go/pkg/otel/arrow_record"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"

	"github.com/prometheus/prometheus/storage/remote/otap"
)

// recordingConsumer captures the metrics handed to the ingestion consumer.
type recordingConsumer struct {
	got  []pmetric.Metrics
	err  error
	call int
}

func (c *recordingConsumer) ConsumeMetrics(_ context.Context, md pmetric.Metrics) error {
	c.call++
	if c.err != nil {
		return c.err
	}
	c.got = append(c.got, md)
	return nil
}

func (*recordingConsumer) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

func TestConsumeBatch_RoundTripsMetrics(t *testing.T) {
	md := generateMetrics(t)

	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })

	batch, err := producer.BatchArrowRecordsFromMetrics(md)
	require.NoError(t, err)

	sink := &recordingConsumer{}
	stream := otap.NewStreamConsumer(sink)
	t.Cleanup(func() { require.NoError(t, stream.Close()) })

	require.NoError(t, stream.ConsumeBatch(t.Context(), batch))
	require.Len(t, sink.got, 1)

	// OTAP promises a lossless conversion back to OTLP, so the decoded payload
	// must be identical to what was encoded. This pins the assumption that OTAP
	// ingestion inherits OTLP semantics exactly.
	require.Equal(t, marshalMetrics(t, md), marshalMetrics(t, sink.got[0]))
}

func TestConsumeBatch_SuccessiveBatchesReuseStreamState(t *testing.T) {
	sink := &recordingConsumer{}
	stream := otap.NewStreamConsumer(sink)
	t.Cleanup(func() { require.NoError(t, stream.Close()) })

	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })

	// Successively-growing attribute value sets force new dictionary entries and
	// exercise delta dictionaries on the server side.
	for i := range 5 {
		md := generateMetrics(t, string(rune('a'+i)))
		batch, err := producer.BatchArrowRecordsFromMetrics(md)
		require.NoError(t, err)
		require.NoError(t, stream.ConsumeBatch(t.Context(), batch))
	}

	require.Len(t, sink.got, 5)
	for i, md := range sink.got {
		rm := md.ResourceMetrics().At(0)
		require.Equal(t, "resource-value-"+string(rune('a'+i)), resourceAttrValue(t, rm))
	}
}

func TestConsumeBatch_EmptyBatch(t *testing.T) {
	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })

	batch, err := producer.BatchArrowRecordsFromMetrics(pmetric.NewMetrics())
	require.NoError(t, err)

	sink := &recordingConsumer{}
	stream := otap.NewStreamConsumer(sink)
	t.Cleanup(func() { require.NoError(t, stream.Close()) })

	require.NoError(t, stream.ConsumeBatch(t.Context(), batch))
	// Encoding empty metrics still yields a batch, which decodes back into one
	// empty metrics object. That is handed to the consumer like any other, and
	// appending it is a no-op.
	require.Len(t, sink.got, 1)
	require.Zero(t, sink.got[0].ResourceMetrics().Len())
}

func TestConsumeBatch_AppendErrorPropagates(t *testing.T) {
	appendErr := errors.New("storage is sad")
	sink := &recordingConsumer{err: appendErr}
	stream := otap.NewStreamConsumer(sink)
	t.Cleanup(func() { require.NoError(t, stream.Close()) })

	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })
	batch, err := producer.BatchArrowRecordsFromMetrics(generateMetrics(t))
	require.NoError(t, err)

	err = stream.ConsumeBatch(t.Context(), batch)
	require.ErrorIs(t, err, appendErr)
	require.NotErrorIs(t, err, otap.ErrInvalidBatch)
	require.NotErrorIs(t, err, otap.ErrMemoryLimit)
}

func TestConsumeBatch_InvalidBatch(t *testing.T) {
	sink := &recordingConsumer{}
	stream := otap.NewStreamConsumer(sink)
	t.Cleanup(func() { require.NoError(t, stream.Close()) })

	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })
	batch, err := producer.BatchArrowRecordsFromMetrics(generateMetrics(t))
	require.NoError(t, err)

	// Corrupt the Arrow payload: the stream state is unusable afterwards, so the
	// error must be reported as ErrInvalidBatch and the stream must be dropped.
	batch.ArrowPayloads[0].Record = []byte("not arrow ipc")

	err = stream.ConsumeBatch(t.Context(), batch)
	require.ErrorIs(t, err, otap.ErrInvalidBatch)
	require.Empty(t, sink.got)
}

func TestConsumeBatch_MemoryLimit(t *testing.T) {
	sink := &recordingConsumer{}
	stream := otap.NewStreamConsumer(sink, otap.WithMemoryLimit(1))
	t.Cleanup(func() { _ = stream.Close() })

	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })
	batch, err := producer.BatchArrowRecordsFromMetrics(generateMetrics(t))
	require.NoError(t, err)

	// A one-byte Arrow allocator cannot decode anything, and the caller must be
	// able to tell this apart from a malformed payload so it can ask the client
	// to retry with smaller batches.
	err = stream.ConsumeBatch(t.Context(), batch)
	require.ErrorIs(t, err, otap.ErrMemoryLimit)
	require.Empty(t, sink.got)
}

func TestConsumeBatch_NilBatch(t *testing.T) {
	sink := &recordingConsumer{}
	stream := otap.NewStreamConsumer(sink)
	t.Cleanup(func() { require.NoError(t, stream.Close()) })

	require.ErrorIs(t, stream.ConsumeBatch(t.Context(), nil), otap.ErrInvalidBatch)
}

func TestConsumeBatch_AfterClose(t *testing.T) {
	sink := &recordingConsumer{}
	stream := otap.NewStreamConsumer(sink)
	require.NoError(t, stream.Close())
	// Close is idempotent so that stream teardown paths can be unconditional.
	require.NoError(t, stream.Close())

	err := stream.ConsumeBatch(t.Context(), &colarspb.BatchArrowRecords{})
	require.Error(t, err)
	require.Empty(t, sink.got)
}

func generateMetrics(t *testing.T, attrValue ...string) pmetric.Metrics {
	t.Helper()

	value := ""
	if len(attrValue) > 0 {
		value = attrValue[0]
	}

	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "otap-test")
	if value != "" {
		rm.Resource().Attributes().PutStr("resource.value", "resource-value-"+value)
	}

	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("test-scope")
	sm.Scope().SetVersion("1.2.3")

	gauge := sm.Metrics().AppendEmpty()
	gauge.SetName("otap_gauge")
	gauge.SetDescription("a gauge")
	gauge.SetUnit("bytes")
	dp := gauge.SetEmptyGauge().DataPoints().AppendEmpty()
	dp.SetTimestamp(pcommon.NewTimestampFromTime(time.Unix(1700000000, 0)))
	dp.SetDoubleValue(42)
	dp.Attributes().PutStr("attr", "value")

	counter := sm.Metrics().AppendEmpty()
	counter.SetName("otap_counter_total")
	counter.SetUnit("1")
	sum := counter.SetEmptySum()
	sum.SetIsMonotonic(true)
	sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	cdp := sum.DataPoints().AppendEmpty()
	cdp.SetTimestamp(pcommon.NewTimestampFromTime(time.Unix(1700000000, 0)))
	cdp.SetIntValue(7)

	return md
}

func marshalMetrics(t *testing.T, md pmetric.Metrics) []byte {
	t.Helper()
	raw, err := pmetricotlp.NewExportRequestFromMetrics(md).MarshalProto()
	require.NoError(t, err)
	return raw
}

func resourceAttrValue(t *testing.T, rm pmetric.ResourceMetrics) string {
	t.Helper()
	v, ok := rm.Resource().Attributes().Get("resource.value")
	require.True(t, ok)
	return v.Str()
}

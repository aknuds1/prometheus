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
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	colarspb "github.com/open-telemetry/otel-arrow/go/api/experimental/arrow/v1"
	"github.com/open-telemetry/otel-arrow/go/pkg/otel/arrow_record"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/prometheus/prometheus/storage/remote/otap"
)

func newTestReceiver(t *testing.T, sink *recordingConsumer, reg *prometheus.Registry, opts otap.ReceiverOptions) (colarspb.ArrowMetricsServiceClient, colarspb.ArrowLogsServiceClient) {
	t.Helper()

	if reg == nil {
		reg = prometheus.NewRegistry()
	}

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()

	receiver := otap.NewReceiver(slog.New(slog.DiscardHandler), reg, sink, opts)
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

	return colarspb.NewArrowMetricsServiceClient(conn), colarspb.NewArrowLogsServiceClient(conn)
}

func TestArrowMetrics_StreamSucceeds(t *testing.T) {
	sink := &recordingConsumer{}
	client, _ := newTestReceiver(t, sink, nil, otap.ReceiverOptions{})

	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })

	stream, err := client.ArrowMetrics(t.Context())
	require.NoError(t, err)

	var sent []int64
	for range 3 {
		batch, err := producer.BatchArrowRecordsFromMetrics(generateMetrics(t))
		require.NoError(t, err)
		require.NoError(t, stream.Send(batch))
		sent = append(sent, batch.GetBatchId())
	}
	require.NoError(t, stream.CloseSend())

	// Acknowledgements carry the batch IDs they refer to, so the client can
	// correlate them with the batches it sent.
	for _, batchID := range sent {
		ack, err := stream.Recv()
		require.NoError(t, err)
		require.Equal(t, batchID, ack.GetBatchId())
		require.Equal(t, colarspb.StatusCode_OK, ack.GetStatusCode(), ack.GetStatusMessage())
	}

	_, err = stream.Recv()
	require.ErrorIs(t, err, io.EOF, "the server must close the stream after the client did")
	require.Len(t, sink.got, 3)
}

func TestArrowMetrics_InvalidBatchTerminatesStream(t *testing.T) {
	sink := &recordingConsumer{}
	client, _ := newTestReceiver(t, sink, nil, otap.ReceiverOptions{})

	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })

	stream, err := client.ArrowMetrics(t.Context())
	require.NoError(t, err)

	batch, err := producer.BatchArrowRecordsFromMetrics(generateMetrics(t))
	require.NoError(t, err)
	batch.ArrowPayloads[0].Record = []byte("not arrow ipc")
	require.NoError(t, stream.Send(batch))

	// The batch is acknowledged as unappendable, and then the stream is dropped
	// because the decoder state is no longer trustworthy.
	ack, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, colarspb.StatusCode_INVALID_ARGUMENT, ack.GetStatusCode())
	require.NotEmpty(t, ack.GetStatusMessage())

	_, err = stream.Recv()
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Empty(t, sink.got)
}

func TestArrowMetrics_MemoryLimitIsRetryable(t *testing.T) {
	sink := &recordingConsumer{}
	client, _ := newTestReceiver(t, sink, nil, otap.ReceiverOptions{MemoryLimit: 1})

	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })

	stream, err := client.ArrowMetrics(t.Context())
	require.NoError(t, err)

	// A one-byte limit cannot decode anything, but that is a retryable condition,
	// so the stream must stay open and keep acknowledging batches.
	for range 2 {
		batch, err := producer.BatchArrowRecordsFromMetrics(generateMetrics(t))
		require.NoError(t, err)
		require.NoError(t, stream.Send(batch))

		ack, err := stream.Recv()
		require.NoError(t, err)
		require.Equal(t, colarspb.StatusCode_RESOURCE_EXHAUSTED, ack.GetStatusCode())
	}
	require.Empty(t, sink.got)
}

func TestArrowMetrics_AppendErrorKeepsStream(t *testing.T) {
	appendErr := errors.New("storage is sad")
	sink := &recordingConsumer{failFirst: true, err: appendErr}
	client, _ := newTestReceiver(t, sink, nil, otap.ReceiverOptions{})

	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })

	stream, err := client.ArrowMetrics(t.Context())
	require.NoError(t, err)

	first, err := producer.BatchArrowRecordsFromMetrics(generateMetrics(t))
	require.NoError(t, err)
	require.NoError(t, stream.Send(first))

	ack, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, colarspb.StatusCode_INTERNAL, ack.GetStatusCode())
	require.ErrorContains(t, errors.New(ack.GetStatusMessage()), appendErr.Error())

	// A failed append does not poison the stream: the next batch is fine.
	second, err := producer.BatchArrowRecordsFromMetrics(generateMetrics(t))
	require.NoError(t, err)
	require.NoError(t, stream.Send(second))

	ack, err = stream.Recv()
	require.NoError(t, err)
	require.Equal(t, colarspb.StatusCode_OK, ack.GetStatusCode())
}

func TestArrowMetrics_StreamsActiveGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	sink := &recordingConsumer{}
	client, _ := newTestReceiver(t, sink, reg, otap.ReceiverOptions{})

	stream, err := client.ArrowMetrics(t.Context())
	require.NoError(t, err)

	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })
	batch, err := producer.BatchArrowRecordsFromMetrics(generateMetrics(t))
	require.NoError(t, err)
	require.NoError(t, stream.Send(batch))
	_, err = stream.Recv()
	require.NoError(t, err)

	require.Eventually(t, func() bool { return gaugeValue(t, reg, "prometheus_otap_receiver_streams_active") == 1 }, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, stream.CloseSend())
	_, err = stream.Recv()
	require.ErrorIs(t, err, io.EOF)

	require.Eventually(t, func() bool { return gaugeValue(t, reg, "prometheus_otap_receiver_streams_active") == 0 }, 5*time.Second, 10*time.Millisecond)
}

func TestArrowMetrics_LogsServiceIsUnimplemented(t *testing.T) {
	sink := &recordingConsumer{}
	_, logsClient := newTestReceiver(t, sink, nil, otap.ReceiverOptions{})

	stream, err := logsClient.ArrowLogs(t.Context())
	require.NoError(t, err)
	_, err = stream.Recv()
	require.Equal(t, codes.Unimplemented, status.Code(err), "OTAP logs are out of scope, so the service must not be served")
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()

	families, err := reg.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		var sum float64
		for _, m := range family.GetMetric() {
			sum += metricValue(m)
		}
		return sum
	}
	return 0
}

func metricValue(m *dto.Metric) float64 {
	if g := m.GetGauge(); g != nil {
		return g.GetValue()
	}
	if c := m.GetCounter(); c != nil {
		return c.GetValue()
	}
	return 0
}

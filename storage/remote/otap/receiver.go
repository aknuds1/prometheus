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

package otap

import (
	"context"
	"errors"
	"io"
	"log/slog"

	colarspb "github.com/open-telemetry/otel-arrow/go/api/experimental/arrow/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/collector/consumer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ReceiverOptions configures a [Receiver].
type ReceiverOptions struct {
	// MemoryLimit is the Arrow memory limit per stream, in bytes. Zero selects
	// [DefaultMemoryLimit].
	MemoryLimit int64
}

// Receiver serves the OTAP metrics gRPC service.
//
// Only the metrics service is served. OTAP logs and traces services are not
// registered, so clients asking for them get UNIMPLEMENTED.
//
// Acknowledgements are transport-level only: they tell the client that a batch
// was decoded and appended, and they may arrive out of order. They are not a
// delivery guarantee, and the receiver does not deduplicate batches, so a client
// that retries a batch it did not see acknowledged can append duplicate samples.
type Receiver struct {
	colarspb.UnimplementedArrowMetricsServiceServer

	logger   *slog.Logger
	consumer consumer.Metrics
	opts     ReceiverOptions

	streamsActive *prometheus.GaugeVec
	batches       *prometheus.CounterVec
	ackErrors     prometheus.Counter
}

// NewReceiver returns a Receiver appending decoded metrics through c.
func NewReceiver(logger *slog.Logger, reg prometheus.Registerer, c consumer.Metrics, opts ReceiverOptions) *Receiver {
	return &Receiver{
		logger:   logger,
		consumer: c,
		opts:     opts,
		streamsActive: promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "prometheus",
			Subsystem: "otap_receiver",
			Name:      "streams_active",
			Help:      "The number of currently open OTAP streams.",
		}, []string{"service"}),
		batches: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "prometheus",
			Subsystem: "otap_receiver",
			Name:      "batches_total",
			Help:      "The total number of received OTAP batches by outcome.",
		}, []string{"outcome"}),
		ackErrors: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Namespace: "prometheus",
			Subsystem: "otap_receiver",
			Name:      "ack_errors_total",
			Help:      "The total number of OTAP batch acknowledgements that could not be sent.",
		}),
	}
}

// RegisterServer registers the OTAP metrics service on s.
func (r *Receiver) RegisterServer(s grpc.ServiceRegistrar) {
	colarspb.RegisterArrowMetricsServiceServer(s, r)
}

// ArrowMetrics consumes the batches of a single OTAP metrics stream.
func (r *Receiver) ArrowMetrics(stream colarspb.ArrowMetricsService_ArrowMetricsServer) error {
	ctx := stream.Context()

	streams := r.streamsActive.WithLabelValues("metrics")
	streams.Inc()
	defer streams.Dec()

	dec := NewStreamConsumer(r.consumer, WithMemoryLimit(r.opts.MemoryLimit))
	defer func() {
		if err := dec.Close(); err != nil {
			r.logger.Warn("Failed to close OTAP stream decoder", "err", err)
		}
	}()

	for {
		batch, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			// Client closed the stream after sending everything it wanted to.
			return nil
		}
		if err != nil {
			// Transport-level failure: the client is gone or the connection broke.
			return err
		}

		code, terminate, err := r.handleBatch(ctx, dec, batch)
		if code != colarspb.StatusCode_OK {
			r.batches.WithLabelValues(outcomeFor(code)).Inc()
		} else {
			r.batches.WithLabelValues("ok").Inc()
		}

		if ackErr := stream.Send(&colarspb.BatchStatus{
			BatchId:       batch.GetBatchId(),
			StatusCode:    code,
			StatusMessage: statusMessage(err),
		}); ackErr != nil {
			r.ackErrors.Inc()
			r.logger.Debug("Failed to send OTAP batch acknowledgement", "err", ackErr)
			return ackErr
		}

		if terminate {
			// The decoder state cannot be trusted anymore, so the stream has to
			// be dropped rather than fed further batches.
			r.logger.Warn("Terminating OTAP stream after undecodable batch", "err", err)
			return status.Error(codes.InvalidArgument, statusMessage(err))
		}
	}
}

// handleBatch decodes and appends one batch, reporting how to acknowledge it and
// whether the stream has to be terminated afterwards.
func (r *Receiver) handleBatch(ctx context.Context, dec *StreamConsumer, batch *colarspb.BatchArrowRecords) (colarspb.StatusCode, bool, error) {
	if err := dec.ConsumeBatch(ctx, batch); err != nil {
		switch {
		case errors.Is(err, ErrMemoryLimit):
			// Retryable: ask the client to back off or send smaller batches.
			r.logger.Debug("OTAP batch exceeded the decoding memory limit", "err", err)
			return colarspb.StatusCode_RESOURCE_EXHAUSTED, false, err
		case errors.Is(err, ErrInvalidBatch):
			return colarspb.StatusCode_INVALID_ARGUMENT, true, err
		default:
			// Storage rejected the data. The stream is still usable, and the
			// client decides what to do with an unacknowledged batch.
			r.logger.Error("Failed to append OTAP metrics", "err", err)
			return colarspb.StatusCode_INTERNAL, false, err
		}
	}
	return colarspb.StatusCode_OK, false, nil
}

func outcomeFor(code colarspb.StatusCode) string {
	switch code {
	case colarspb.StatusCode_RESOURCE_EXHAUSTED:
		return "memory_limit"
	case colarspb.StatusCode_INVALID_ARGUMENT:
		return "invalid"
	default:
		return "error"
	}
}

func statusMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

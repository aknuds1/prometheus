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

// Package otap ingests metrics encoded with OTAP, the OpenTelemetry Protocol
// with Apache Arrow.
//
// OTAP batches are decoded into row-oriented OTLP metrics and handed to the
// shared OTLP-to-Prometheus ingestion consumer. This package adds a transport
// only: no Arrow data is retained, and no columnar-style processing happens
// here. Consequently OTAP and OTLP ingestion produce identical series and
// metadata, and any improvement is limited to bytes and conversions on the wire.
package otap

import (
	"context"
	"errors"
	"fmt"
	"sync"

	colarspb "github.com/open-telemetry/otel-arrow/go/api/experimental/arrow/v1"
	"github.com/open-telemetry/otel-arrow/go/pkg/otel/arrow_record"
	"go.opentelemetry.io/collector/consumer"
)

// ErrMemoryLimit is returned by [StreamConsumer.ConsumeBatch] when decoding a batch
// would exceed the batch decoder's Arrow memory limit. Callers are expected to
// signal the client to retry with a smaller batch, or to try again later.
var ErrMemoryLimit = errors.New("otap: arrow memory limit exceeded")

// ErrInvalidBatch is returned by [StreamConsumer.ConsumeBatch] when a batch cannot be
// decoded at all. The stream state is unreliable after this error, so callers
// must terminate the stream.
var ErrInvalidBatch = errors.New("otap: invalid batch")

// DefaultMemoryLimit is the default Arrow memory limit for decoding a single
// stream, in bytes. The limit is per stream, so the total memory used for
// decoding is bounded by the limit multiplied by the number of concurrent streams.
const DefaultMemoryLimit = 70 << 20

// Option configures a [StreamConsumer].
type Option func(*options)

type options struct {
	memoryLimit uint64
}

// WithMemoryLimit sets the Arrow memory limit for decoding a stream, in bytes.
// Values of zero or less select [DefaultMemoryLimit].
func WithMemoryLimit(bytes int64) Option {
	return func(o *options) {
		if bytes > 0 {
			o.memoryLimit = uint64(bytes)
		}
	}
}

// StreamConsumer decodes the OTAP batches of a single stream and appends the
// resulting metrics through the ingestion consumer.
//
// OTAP is stateful: dictionaries and schemas are built up over the lifetime of a
// stream and reused by later batches, and a schema change resets them. A
// StreamConsumer therefore owns that state and must not be shared between
// streams. It is not safe for concurrent use; a single stream must be consumed
// sequentially, which is what the OTAP bidirectional gRPC streams require anyway.
type StreamConsumer struct {
	consumer consumer.Metrics

	mtx     sync.Mutex
	decoder *arrow_record.Consumer
	closed  bool
}

// NewStreamConsumer returns a StreamConsumer appending data through c.
func NewStreamConsumer(c consumer.Metrics, opts ...Option) *StreamConsumer {
	o := options{memoryLimit: DefaultMemoryLimit}
	for _, opt := range opts {
		opt(&o)
	}

	return &StreamConsumer{
		consumer: c,
		decoder:  arrow_record.NewConsumer(arrow_record.WithMemoryLimit(o.memoryLimit)),
	}
}

// ConsumeBatch decodes the given batch and appends all metrics it carries.
//
// A batch either appends completely or not at all: decoding produces at most one
// [pmetric.Metrics], which the ingestion consumer appends in a single transaction.
// An unusable payload is reported as [ErrInvalidBatch]; see the error's
// documentation for why that must end the stream.
func (s *StreamConsumer) ConsumeBatch(ctx context.Context, batch *colarspb.BatchArrowRecords) error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	if s.closed {
		return errors.New("otap: stream consumer is closed")
	}
	if batch == nil {
		return fmt.Errorf("%w: nil batch", ErrInvalidBatch)
	}

	metrics, err := s.decoder.MetricsFrom(batch)
	if err != nil {
		if errors.Is(err, arrow_record.ErrConsumerMemoryLimit) {
			return fmt.Errorf("%w: %w", ErrMemoryLimit, err)
		}
		return fmt.Errorf("%w: %w", ErrInvalidBatch, err)
	}

	// MetricsFrom returns at most one entry. An empty batch decodes into one empty
	// metrics object, which appends nothing.
	for _, md := range metrics {
		if err := s.consumer.ConsumeMetrics(ctx, md); err != nil {
			return fmt.Errorf("appending decoded OTAP metrics: %w", err)
		}
	}

	return nil
}

// Close releases the resources held by the decoder. The StreamConsumer must not
// be used afterwards.
func (s *StreamConsumer) Close() error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true
	return s.decoder.Close()
}

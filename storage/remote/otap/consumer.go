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
	metricsotlp "github.com/open-telemetry/otel-arrow/go/pkg/otel/metrics/otlp"
	"github.com/open-telemetry/otel-arrow/go/pkg/record_message"
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
	recordSink  RecordSink
}

// RecordSink receives the Arrow records of a decoded batch, before they are
// converted to OTLP. It exists so that a columnar consumer can ingest the
// records directly, without a second decode and without the row-oriented
// round trip.
type RecordSink interface {
	// ConsumeRecords is called with the records of one batch. The records are
	// owned by the caller and released when the call returns, so implementations
	// must not retain them or their child arrays.
	ConsumeRecords(ctx context.Context, records []*record_message.RecordMessage) error
}

// WithRecordSink adds a sink receiving the Arrow records of every batch.
func WithRecordSink(sink RecordSink) Option {
	return func(o *options) { o.recordSink = sink }
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
	records  RecordSink

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
		records:  o.recordSink,
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

	records, err := s.decoder.Consume(batch)
	if err != nil {
		if errors.Is(err, arrow_record.ErrConsumerMemoryLimit) {
			return fmt.Errorf("%w: %w", ErrMemoryLimit, err)
		}
		return fmt.Errorf("%w: %w", ErrInvalidBatch, err)
	}

	// The record sink runs first, because converting the records to OTLP releases
	// them. A columnar sink therefore has to be cheap or it is on the critical path
	// of both consumers, which is exactly what the ingestion comparison measures.
	if s.records != nil {
		if err := s.records.ConsumeRecords(ctx, records); err != nil {
			releaseRecords(records)
			return fmt.Errorf("consuming OTAP records: %w", err)
		}
	}

	if s.consumer == nil {
		releaseRecords(records)
		return nil
	}

	// RelatedDataFrom takes ownership of the records and releases them.
	relatedData, metricsRecord, err := metricsotlp.RelatedDataFrom(records)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBatch, err)
	}
	if metricsRecord == nil {
		return nil
	}
	md, err := metricsotlp.MetricsFrom(metricsRecord.Record(), relatedData)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidBatch, err)
	}
	if err := s.consumer.ConsumeMetrics(ctx, md); err != nil {
		return fmt.Errorf("appending decoded OTAP metrics: %w", err)
	}
	return nil
}

// releaseRecords releases the Arrow records of a batch.
func releaseRecords(records []*record_message.RecordMessage) {
	for _, record := range records {
		record.Record().Release()
	}
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

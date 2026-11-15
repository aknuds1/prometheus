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

package columnar

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/open-telemetry/otel-arrow/go/pkg/record_message"

	otlpprw "github.com/prometheus/prometheus/storage/remote/otlptranslator/prometheusremotewrite"
)

// OTAPSink appends OTAP metric batches to columnar storage.
//
// Gauges and sums are decoded from Arrow records into typed per-series sample
// columns. Resource identity is captured before label translation and carried by
// __otel_resource__. Resource/schema, instrumentation Scope schema, source and
// exact per-series metric metadata have separate native owners. Descriptive
// Resource attributes are not labels merely because they arrived in the batch;
// existing promotion and target_info settings still provide compatibility.
//
// Classic histograms, exponential histograms, summaries and data point exemplars
// are not converted yet, and the sink accepts a batch as a whole or not at all. A
// batch carrying any of them, or a data point row it cannot attribute or value,
// fails with [ErrUnsupportedPayload] before anything is published, so its samples,
// associations and metadata versions never become visible. Delta samples are
// retained without conversion. Native values are stringified with the same
// pcommon.Value.AsString conversion used by the existing OTLP path.
type OTAPSink struct {
	storage  *Storage
	logger   *slog.Logger
	settings func() otlpprw.Settings
}

// ErrUnsupportedPayload is returned by [OTAPSink.ConsumeRecords] when a batch
// carries OTAP data the native sink cannot convert: summary, classic histogram or
// exponential histogram data points, data point exemplars, a metric row without a
// convertible name, or a data point row without a value.
//
// Such a batch is rejected as a whole. Dropping only the unconvertible part of it
// would acknowledge samples the store never received, so the transport reports a
// negative batch acknowledgement instead and leaves the decision to the client.
var ErrUnsupportedPayload = errors.New("columnar: unsupported OTAP payload")

// NewOTAPSink returns a sink appending to storage. An optional settings provider
// is evaluated once per batch, so live OTLP translation changes don't require
// rebuilding the receiver. Without a provider, the sink selects UTF-8 names, no
// suffixes and no target_info for terse standalone experiments.
func NewOTAPSink(storage *Storage, logger *slog.Logger, providers ...func() otlpprw.Settings) *OTAPSink {
	settings := func() otlpprw.Settings {
		return otlpprw.Settings{AllowUTF8: true, DisableTargetInfo: true}
	}
	if len(providers) > 0 && providers[0] != nil {
		settings = providers[0]
	}
	return &OTAPSink{storage: storage, logger: logger, settings: settings}
}

// ConsumeRecords appends the metrics carried by one OTAP batch.
//
// The batch is all-or-nothing. A readable batch is appended in one
// [Storage.AppendBatch] transaction; a batch the sink cannot convert is rejected
// before that call, so none of its samples, associations or metadata versions are
// published. An empty batch, or a batch whose metrics carry no data points, is a
// successful no-op, because it holds nothing that could be lost.
func (s *OTAPSink) ConsumeRecords(ctx context.Context, records []*record_message.RecordMessage) error {
	if len(records) == 0 {
		return nil
	}
	batch, appended, err := s.buildBatch(records)
	if err != nil {
		// The transport reports the rejection to the client; the sink's own log keeps
		// the payload-level reason next to the component that refused the batch.
		if s.logger != nil {
			s.logger.Debug("Rejecting OTAP batch the columnar sink cannot convert", "err", err)
		}
		return err
	}
	if appended == 0 {
		return nil
	}
	if err := s.storage.AppendBatch(ctx, batch); err != nil {
		return fmt.Errorf("appending OTAP batch: %w", err)
	}
	return nil
}

// column returns the first column with the given name, or nil.
func column(rec arrow.RecordBatch, name string) arrow.Array {
	for i, field := range rec.Schema().Fields() {
		if field.Name == name {
			return rec.Column(i)
		}
	}
	return nil
}

// uintValue reads an unsigned integer column of any width, including a
// dictionary-encoded one.
func uintValue(col arrow.Array, i int) uint64 {
	if col == nil || col.IsNull(i) {
		return 0
	}
	if dict, ok := col.(*array.Dictionary); ok {
		return uintValue(dict.Dictionary(), dict.GetValueIndex(i))
	}
	switch c := col.(type) {
	case *array.Uint16:
		return uint64(c.Value(i))
	case *array.Uint32:
		return uint64(c.Value(i))
	case *array.Uint8:
		return uint64(c.Value(i))
	case *array.Uint64:
		return c.Value(i)
	default:
		return 0
	}
}

// signedValue reads a signed integer column of any width, including a
// dictionary-encoded one.
func signedValue(col arrow.Array, i int) int64 {
	if col == nil || col.IsNull(i) {
		return 0
	}
	if dict, ok := col.(*array.Dictionary); ok {
		return signedValue(dict.Dictionary(), dict.GetValueIndex(i))
	}
	switch c := col.(type) {
	case *array.Int64:
		return c.Value(i)
	case *array.Int32:
		return int64(c.Value(i))
	case *array.Int16:
		return int64(c.Value(i))
	case *array.Int8:
		return int64(c.Value(i))
	default:
		return 0
	}
}

// stringValue reads a string or nullable string dictionary value.
func stringValue(col arrow.Array, i int) string {
	if col == nil || col.IsNull(i) {
		return ""
	}
	if dict, ok := col.(*array.Dictionary); ok {
		return stringValue(dict.Dictionary(), dict.GetValueIndex(i))
	}
	switch c := col.(type) {
	case *array.String:
		return c.Value(i)
	case *array.Binary:
		return string(c.Value(i))
	default:
		return ""
	}
}

// timestampMilliseconds reads a nanosecond timestamp column as milliseconds.
func timestampMilliseconds(col arrow.Array, i int) int64 {
	if col == nil || col.IsNull(i) {
		return 0
	}
	ts, ok := col.(*array.Timestamp)
	if !ok {
		return 0
	}
	return time.Unix(0, int64(ts.Value(i))).UnixMilli()
}

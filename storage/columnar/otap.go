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
	colarspb "github.com/open-telemetry/otel-arrow/go/api/experimental/arrow/v1"
	"github.com/open-telemetry/otel-arrow/go/pkg/record_message"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
)

// OTAPSink appends OTAP metric batches to columnar storage.
//
// It reads the Arrow records directly, without converting them to OTLP first,
// and builds series from them: resource attributes, metric name and data point
// attributes become the label set, and the time and value columns become the
// samples. That is the point of the exercise - the row-oriented round trip that
// the existing receiver pays for is skipped here - and the cost of the join is
// what the benchmarks measure.
//
// The supported subset is deliberately narrow, and everything outside it is
// dropped rather than approximated, because a wrong series is worse than a
// missing one:
//
//   - UNIVARIATE_METRICS with NUMBER_DATA_POINTS only: gauges and sums,
//     including delta temporality, but not histograms or summaries,
//   - string attribute values only; numeric, boolean and array attributes are
//     dropped,
//   - scope attributes, metric description and unit are not stored,
//   - exemplars are not stored.
//
// Prometheus semantics are not applied here at all: no delta-to-cumulative
// conversion, no unit or type suffixes, no resource attribute promotion policy,
// no staleness handling. This is a columnar transport for the metric model, and
// the differences from OTLP ingestion are expected to show up when the two are
// compared, which is one of the things worth measuring.
type OTAPSink struct {
	storage *Storage
	logger  *slog.Logger
}

// NewOTAPSink returns a sink appending to storage.
func NewOTAPSink(storage *Storage, logger *slog.Logger) *OTAPSink {
	return &OTAPSink{storage: storage, logger: logger}
}

// ConsumeRecords appends the metrics carried by one OTAP batch.
func (s *OTAPSink) ConsumeRecords(ctx context.Context, records []*record_message.RecordMessage) error {
	if len(records) == 0 {
		return nil
	}

	var metricsRecord, resourceRecord, numberDPRecord, dpAttrsRecord *record_message.RecordMessage
	for _, record := range records {
		switch record.PayloadType() {
		case colarspb.ArrowPayloadType_UNIVARIATE_METRICS:
			metricsRecord = record
		case colarspb.ArrowPayloadType_RESOURCE_ATTRS:
			resourceRecord = record
		case colarspb.ArrowPayloadType_NUMBER_DATA_POINTS:
			numberDPRecord = record
		case colarspb.ArrowPayloadType_NUMBER_DP_ATTRS:
			dpAttrsRecord = record
		}
	}
	if metricsRecord == nil || numberDPRecord == nil {
		// Nothing this sink understands: histograms, summaries, logs, traces.
		return nil
	}

	resourceAttrs := stringAttributes(resourceRecord)
	dpAttrs := stringAttributes(dpAttrsRecord)
	metricNames, metricResources := metricIndex(metricsRecord.Record())

	app := s.storage.AppenderV2(ctx)
	defer func() {
		if app != nil {
			_ = app.Rollback()
		}
	}()

	rec := numberDPRecord.Record()
	idCol := column(rec, "id")
	parentCol := column(rec, "parent_id")
	timeCol := column(rec, "time_unix_nano")
	intCol := column(rec, "int_value")
	doubleCol := column(rec, "double_value")
	if parentCol == nil || timeCol == nil {
		return errors.New("columnar: OTAP data points record lacks parent_id or time_unix_nano")
	}

	var (
		appended int
		skipped  int
	)
	for i := range int(rec.NumRows()) {
		metricID := uint16(uintValue(parentCol, i))
		name, ok := metricNames[metricID]
		if !ok || name == "" {
			skipped++
			continue
		}

		builder := labels.NewScratchBuilder(4)
		builder.Add(labels.MetricName, name)
		for key, value := range resourceAttrs[uint64(metricResources[metricID])] {
			builder.Add(key, value)
		}
		if idCol != nil {
			for key, value := range dpAttrs[uintValue(idCol, i)] {
				builder.Add(key, value)
			}
		}
		builder.Sort()
		ls := builder.Labels()

		t := timestampMilliseconds(timeCol, i)
		var value float64
		switch {
		case doubleCol != nil && !doubleCol.IsNull(i):
			value = doubleCol.(*array.Float64).Value(i)
		case intCol != nil && !intCol.IsNull(i):
			value = float64(intCol.(*array.Int64).Value(i))
		default:
			skipped++
			continue
		}

		if _, err := app.Append(0, ls, 0, t, value, nil, nil, storage.AOptions{}); err != nil {
			return fmt.Errorf("appending sample for %s: %w", ls, err)
		}
		appended++
	}

	if appended == 0 {
		return nil
	}
	if err := app.Commit(); err != nil {
		return err
	}
	app = nil

	if s.logger != nil && skipped > 0 {
		s.logger.Debug("Skipped OTAP samples the columnar sink does not support",
			"appended", appended, "skipped", skipped)
	}
	return nil
}

// metricIndex maps metric IDs to names and to their resource ID.
func metricIndex(rec arrow.RecordBatch) (map[uint16]string, map[uint16]uint16) {
	names := map[uint16]string{}
	resources := map[uint16]uint16{}

	idCol := column(rec, "id")
	nameCol := column(rec, "name")
	if idCol == nil || nameCol == nil {
		return names, resources
	}

	var resourceCol arrow.Array
	for _, field := range rec.Schema().Fields() {
		if field.Name == "resource" {
			resourceCol = column(rec, "resource")
			break
		}
	}
	// The resource is a struct with an "id" child.
	var resourceIDs *array.Uint16
	if resourceCol != nil {
		if st, ok := resourceCol.(*array.Struct); ok {
			if f := st.Field(0); f != nil {
				resourceIDs, _ = f.(*array.Uint16)
			}
		}
	}

	for i := range int(rec.NumRows()) {
		id := uint16(uintValue(idCol, i))
		names[id] = stringValue(nameCol, i)
		if resourceIDs != nil && !resourceIDs.IsNull(i) {
			resources[id] = resourceIDs.Value(i)
		}
	}
	return names, resources
}

// stringAttributes groups the string attributes of a resource or scope or data
// point attributes record by parent ID.
//
// The records are sorted by parent ID, so a single pass suffices; a map of maps
// is used anyway because the grouping is clearer than index arithmetic and this
// is not yet the hot path to optimise.
func stringAttributes(record *record_message.RecordMessage) map[uint64]map[string]string {
	out := map[uint64]map[string]string{}
	if record == nil {
		return out
	}

	rec := record.Record()
	parentCol := column(rec, "parent_id")
	keyCol := column(rec, "key")
	strCol := column(rec, "str")
	if parentCol == nil || keyCol == nil || strCol == nil {
		return out
	}

	for i := range int(rec.NumRows()) {
		if strCol.IsNull(i) {
			continue
		}
		parent := uintValue(parentCol, i)
		if out[parent] == nil {
			out[parent] = map[string]string{}
		}
		out[parent][stringValue(keyCol, i)] = stringValue(strCol, i)
	}
	return out
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

// uintValue reads an unsigned integer column of any width.
func uintValue(col arrow.Array, i int) uint64 {
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

// stringValue reads a string column, which OTAP dictionary-encodes.
func stringValue(col arrow.Array, i int) string {
	if dict, ok := col.(*array.Dictionary); ok {
		value := dict.Dictionary()
		switch v := value.(type) {
		case *array.String:
			return v.Value(dict.GetValueIndex(i))
		case *array.Binary:
			return string(v.Value(dict.GetValueIndex(i)))
		default:
			return ""
		}
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
	ts, ok := col.(*array.Timestamp)
	if !ok {
		return 0
	}
	ns := int64(ts.Value(i))
	return time.Unix(0, ns).UnixMilli()
}

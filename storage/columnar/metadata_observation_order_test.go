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
	"log/slog"
	"testing"
	"time"

	colarspb "github.com/open-telemetry/otel-arrow/go/api/experimental/arrow/v1"
	"github.com/open-telemetry/otel-arrow/go/pkg/otel/arrow_record"
	"github.com/open-telemetry/otel-arrow/go/pkg/record_message"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/prometheus/prometheus/model/labels"
	otlpprw "github.com/prometheus/prometheus/storage/remote/otlptranslator/prometheusremotewrite"
)

// The native sink folds repeated observations of one unchanged association
// within a batch. These tests cover the boundary that fold must respect: the
// order in which associations reach the builder, and the update order it emits.
// The end-to-end public results of the same fixture are asserted by
// TestNativeMetadataObservationOrderedEpochs in the external test package.

const (
	nativeOrderResourceSchemaA = "native-order-resource-a"
	nativeOrderResourceSchemaB = "native-order-resource-b"
	nativeOrderScopeSchemaA    = "native-order-scope-a"
	nativeOrderScopeSchemaB    = "native-order-scope-b"
	nativeOrderMetric          = "native_order_metric"
	nativeOrderRoute           = "/ordered"
)

// nativeOrderOccurrence is one Resource/Scope occurrence of the ordering fixture:
// one schema URL pair and the timestamps it carries.
type nativeOrderOccurrence struct {
	resourceSchema string
	scopeSchema    string
	host           string
	route          string
	timestamps     []int64
}

// nativeOrderMetrics builds the fixture through the real OTAP encoder input. Two
// occurrences state the same descriptive Resource content, and the one in between
// states different content, so an owner-ordered fold must not merge the two equal
// epochs.
func nativeOrderMetrics(occurrences ...nativeOrderOccurrence) pmetric.Metrics {
	md := pmetric.NewMetrics()
	for _, occurrence := range occurrences {
		rm := md.ResourceMetrics().AppendEmpty()
		rm.SetSchemaUrl(occurrence.resourceSchema)
		rm.Resource().Attributes().PutStr("service.name", "checkout")
		rm.Resource().Attributes().PutStr("service.namespace", "shop")
		rm.Resource().Attributes().PutStr("service.instance.id", "pod-a")
		rm.Resource().Attributes().PutStr("deployment.environment.name", "prod")
		rm.Resource().Attributes().PutStr("host.name", occurrence.host)
		rm.Resource().Attributes().PutStr("process.pid", "4242")
		rm.Resource().Attributes().PutStr("sandboxed", "true")
		sm := rm.ScopeMetrics().AppendEmpty()
		sm.SetSchemaUrl(occurrence.scopeSchema)
		sm.Scope().SetName("native-order-sdk")
		sm.Scope().SetVersion("v1")
		metric := sm.Metrics().AppendEmpty()
		metric.SetName(nativeOrderMetric)
		metric.SetDescription("Native ordering")
		metric.SetUnit("1")
		gauge := metric.SetEmptyGauge()
		route := occurrence.route
		if route == "" {
			route = nativeOrderRoute
		}
		for _, timestamp := range occurrence.timestamps {
			dp := gauge.DataPoints().AppendEmpty()
			dp.SetTimestamp(pcommon.Timestamp(timestamp * 1_000_000))
			dp.SetDoubleValue(float64(timestamp))
			dp.Attributes().PutStr("route", route)
		}
	}
	return md
}

// nativeOrderSettings keeps the Scope schema URL out of the series labels, so every
// occurrence states one series, and disables synthetic target_info.
func nativeOrderSettings() otlpprw.Settings {
	return otlpprw.Settings{
		AddMetricSuffixes:                    true,
		AllowUTF8:                            false,
		DisableTargetInfo:                    true,
		KeepIdentifyingResourceAttributes:    true,
		PromoteScopeMetadata:                 false,
		AllowDeltaTemporality:                true,
		LookbackDelta:                        5 * time.Minute,
		EnableTypeAndUnitLabels:              true,
		LabelNameUnderscoreSanitization:      true,
		LabelNamePreserveMultipleUnderscores: true,
	}
}

// nativeOrderRecords encodes the fixture with the real OTAP producer and decodes it
// with the real consumer, which is the record stream the sink receives.
func nativeOrderRecords(t *testing.T, md pmetric.Metrics) []*record_message.RecordMessage {
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

// nativeOrderDecodedRow is one number data point row as the sink decodes it.
type nativeOrderDecodedRow struct {
	resourceContent    string
	resourceSchemaURL  string
	scopeSchemaContent string
	owner              OwnerKey
	seriesRoute        string
	timestamp          int64
}

// nativeOrderDecodedRows walks the number data point rows exactly as
// appendNumberDataPoints does, so the association sequence the native fold receives
// is measured instead of inferred from the result it produces.
func nativeOrderDecodedRows(t *testing.T, records []*record_message.RecordMessage) []nativeOrderDecodedRow {
	t.Helper()
	var metricsRecord, resourceRecord, numberDPRecord, pointAttrsRecord *record_message.RecordMessage
	for _, record := range records {
		switch record.PayloadType() {
		case colarspb.ArrowPayloadType_UNIVARIATE_METRICS:
			metricsRecord = record
		case colarspb.ArrowPayloadType_RESOURCE_ATTRS:
			resourceRecord = record
		case colarspb.ArrowPayloadType_NUMBER_DATA_POINTS:
			numberDPRecord = record
		case colarspb.ArrowPayloadType_NUMBER_DP_ATTRS:
			pointAttrsRecord = record
		}
	}
	require.NotNil(t, metricsRecord, "fixture needs a metrics payload")
	require.NotNil(t, numberDPRecord, "fixture needs a number data point payload")
	resourceAttrs, err := decodeOTAPAttributes[uint16](resourceRecord)
	require.NoError(t, err)
	pointAttrs, err := decodeOTAPAttributes[uint32](pointAttrsRecord)
	require.NoError(t, err)
	metrics, err := readOTAPMetricContexts(metricsRecord.Record(), resourceAttrs)
	require.NoError(t, err)

	rec := numberDPRecord.Record()
	idCol := column(rec, "id")
	parentCol := column(rec, "parent_id")
	timeCol := column(rec, "time_unix_nano")
	require.NotNil(t, parentCol)
	require.NotNil(t, timeCol)

	var metricID uint16
	var pointID uint32
	rows := make([]nativeOrderDecodedRow, 0, rec.NumRows())
	for row := range int(rec.NumRows()) {
		metricID += uint16(uintValue(parentCol, row))
		ctx, ok := metrics[metricID]
		require.True(t, ok, "row %d refers to a metric the sink does not decode", row)
		var attributes map[string]string
		if idCol != nil && !idCol.IsNull(row) {
			pointID += uint32(uintValue(idCol, row))
			attributes = otapAttributeStrings(pointAttrs.AttributesByID(pointID))
		}
		rows = append(rows, nativeOrderDecodedRow{
			resourceContent:    ctx.resource.descriptive["host.name"],
			resourceSchemaURL:  ctx.resource.schemaURL,
			scopeSchemaContent: schemaContent(ctx.scopeSchemaURL).Attributes["schema_url"],
			owner:              ctx.resource.owner,
			seriesRoute:        attributes["route"],
			timestamp:          timestampMilliseconds(timeCol, row),
		})
	}
	return rows
}

// TestNativeMetadataObservationDecodeOrder proves the association sequence the
// native fold receives for the ordering fixture is A,A,B,A and not A,A,A,B: one
// Resource owner, one series, and two Scope-schema epochs of the same content
// separated by a different one.
func TestNativeMetadataObservationDecodeOrder(t *testing.T) {
	records := nativeOrderRecords(t, nativeOrderMetrics(
		nativeOrderOccurrence{
			resourceSchema: nativeOrderResourceSchemaA, scopeSchema: nativeOrderScopeSchemaA,
			host: "host-1", timestamps: []int64{1000, 2000},
		},
		nativeOrderOccurrence{
			resourceSchema: nativeOrderResourceSchemaA, scopeSchema: nativeOrderScopeSchemaB,
			host: "host-1", timestamps: []int64{3000},
		},
		nativeOrderOccurrence{
			resourceSchema: nativeOrderResourceSchemaB, scopeSchema: nativeOrderScopeSchemaA,
			host: "host-1", timestamps: []int64{4000},
		},
	))
	rows := nativeOrderDecodedRows(t, records)
	require.Len(t, rows, 4)

	owner := rows[0].owner
	// The fixture states two equal-content epochs around a different one. The
	// decoded association order must therefore be A(1000), A(2000), B(3000),
	// A(4000) and not A(1000), A(2000), A(4000), B(3000).
	require.Equal(t, []string{
		nativeOrderScopeSchemaA, nativeOrderScopeSchemaA,
		nativeOrderScopeSchemaB, nativeOrderScopeSchemaA,
	}, []string{
		rows[0].scopeSchemaContent, rows[1].scopeSchemaContent,
		rows[2].scopeSchemaContent, rows[3].scopeSchemaContent,
	})
	require.Equal(t, []int64{1000, 2000, 3000, 4000}, []int64{
		rows[0].timestamp, rows[1].timestamp, rows[2].timestamp, rows[3].timestamp,
	})

	for i, row := range rows {
		require.Equal(t, owner, row.owner, "row %d must belong to one Resource owner", i)
		require.Equal(t, "host-1", row.resourceContent, "row %d must state one Resource content", i)
		require.Equal(t, nativeOrderRoute, row.seriesRoute, "row %d must belong to one series", i)
	}
	// The middle occurrence is a different Resource schema epoch, which is a
	// separate owner and must not be confused with the Scope epoch that changes.
	require.Equal(t, nativeOrderResourceSchemaA, rows[0].resourceSchemaURL)
	require.Equal(t, nativeOrderResourceSchemaB, rows[3].resourceSchemaURL, "the last row re-states content in a later Resource schema epoch")
}

// TestNativeMetadataObservationUpdateOrder asserts the update order the native fold
// emits for one owner: the earliest observation of an unchanged context stays the
// change point, the latest extends its observation bound, and an epoch that is
// separated from its earlier observations by a different content emits a new
// ordered assertion instead of moving an already emitted boundary.
func TestNativeMetadataObservationUpdateOrder(t *testing.T) {
	owner := ScopeOwner("native-order-sdk", "v1")

	t.Run("unchanged_run_keeps_first_and_last", func(t *testing.T) {
		batch, appended, err := nativeOrderBatch(t, nativeOrderMetrics(nativeOrderOccurrence{
			resourceSchema: nativeOrderResourceSchemaA, scopeSchema: nativeOrderScopeSchemaA,
			host: "host-1", timestamps: []int64{1000, 2000, 3000, 4000, 5000},
		}))
		require.NoError(t, err)
		require.Equal(t, 5, appended)
		require.Equal(t, []int64{1000, 5000}, nativeOrderUpdateTimes(batch, owner),
			"an unchanged run must cost one change point and one observation bound")
		require.Len(t, batch.Series, 1, "the fixture is one series")
	})

	t.Run("interleaved_series_keep_one_pair_each", func(t *testing.T) {
		// Series sharing one Resource interleave in the row stream. That is not an
		// intervention in the owner's temporal domain: their content is one
		// unchanged epoch, so each series must keep one change point and one bound.
		batch, appended, err := nativeOrderBatch(t, nativeOrderMetrics(
			nativeOrderOccurrence{
				resourceSchema: nativeOrderResourceSchemaA, scopeSchema: nativeOrderScopeSchemaA,
				host: "host-1", route: "/first", timestamps: []int64{1000, 2000, 3000, 4000, 5000},
			},
			nativeOrderOccurrence{
				resourceSchema: nativeOrderResourceSchemaA, scopeSchema: nativeOrderScopeSchemaA,
				host: "host-1", route: "/second", timestamps: []int64{1000, 2000, 3000, 4000, 5000},
			},
			nativeOrderOccurrence{
				resourceSchema: nativeOrderResourceSchemaA, scopeSchema: nativeOrderScopeSchemaA,
				host: "host-1", route: "/third", timestamps: []int64{1000, 2000, 3000, 4000, 5000},
			},
		))
		require.NoError(t, err)
		require.Equal(t, 15, appended)
		require.Len(t, batch.Series, 3)
		resource := batch.Updates[0].Update.Owner
		require.Len(t, nativeOrderUpdateTimes(batch, resource), 6,
			"three series of one unchanged Resource cost two assertions each")
		for _, series := range batch.Series {
			require.Equal(t, []int64{1000, 5000}, nativeOrderUpdateTimesFor(batch, resource, series.Labels),
				"series %s must keep one change point and one bound", series.Labels)
		}
	})

	t.Run("intervening_content_opens_a_new_epoch", func(t *testing.T) {
		batch, appended, err := nativeOrderBatch(t, nativeOrderMetrics(
			nativeOrderOccurrence{
				resourceSchema: nativeOrderResourceSchemaA, scopeSchema: nativeOrderScopeSchemaA,
				host: "host-1", timestamps: []int64{1000, 2000},
			},
			nativeOrderOccurrence{
				resourceSchema: nativeOrderResourceSchemaA, scopeSchema: nativeOrderScopeSchemaB,
				host: "host-1", timestamps: []int64{3000},
			},
			nativeOrderOccurrence{
				resourceSchema: nativeOrderResourceSchemaB, scopeSchema: nativeOrderScopeSchemaA,
				host: "host-1", timestamps: []int64{4000},
			},
		))
		require.NoError(t, err)
		require.Equal(t, 4, appended)
		require.Equal(t, []int64{1000, 2000, 3000, 4000}, nativeOrderUpdateTimes(batch, owner),
			"the re-stated content after a different one is a new assertion, in input order")
	})
}

// nativeOrderBatch converts the fixture into the transaction-local batch the sink
// would append, without a store.
func nativeOrderBatch(t *testing.T, md pmetric.Metrics) (Batch, int, error) {
	t.Helper()
	sink := NewOTAPSink(nil, slog.New(slog.DiscardHandler), nativeOrderSettings)
	return sink.buildBatch(nativeOrderRecords(t, md))
}

// nativeOrderUpdateTimes returns the effective times of the batch's updates for one
// owner, in the order the store would apply them.
func nativeOrderUpdateTimes(batch Batch, owner OwnerKey) []int64 {
	var times []int64
	for _, association := range batch.Updates {
		if association.Update.Owner == owner {
			times = append(times, association.Update.EffectiveFrom)
		}
	}
	return times
}

// nativeOrderUpdateTimesFor returns the same times for one owner and one series.
func nativeOrderUpdateTimesFor(batch Batch, owner OwnerKey, ls labels.Labels) []int64 {
	var times []int64
	for _, association := range batch.Updates {
		if association.Update.Owner == owner && labels.Equal(association.Labels, ls) {
			times = append(times, association.Update.EffectiveFrom)
		}
	}
	return times
}

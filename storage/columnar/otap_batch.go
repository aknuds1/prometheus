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
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	colarspb "github.com/open-telemetry/otel-arrow/go/api/experimental/arrow/v1"
	commonotlp "github.com/open-telemetry/otel-arrow/go/pkg/otel/common/otlp"
	"github.com/open-telemetry/otel-arrow/go/pkg/record_message"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"golang.org/x/exp/constraints"

	"github.com/prometheus/prometheus/model/labels"
	promvalue "github.com/prometheus/prometheus/model/value"
	otlpprw "github.com/prometheus/prometheus/storage/remote/otlptranslator/prometheusremotewrite"
)

type otapResourceBounds struct {
	resource otapResourceContext
	min, max int64
}

// metadataBatchKey identifies one unchanged association within a batch: the same
// series, owner and complete content. Observations with the same key state one
// context at several times, so they describe one version and one membership.
type metadataBatchKey struct {
	seriesHash uint64
	owner      OwnerKey
	content    string
}

// metadataSeriesKind identifies the membership domain of one series and owner kind.
type metadataSeriesKind struct {
	seriesHash uint64
	kind       MetadataKind
}

// metadataBatchObservation is the observation range of one unchanged association
// within the batch being built. The entry at first carries the earliest observation,
// which stays the change point, and the entry at last carries the latest one, which
// extends the observation bound.
//
// An observation may only be folded while its epoch is open in every temporal domain
// it writes: its owner's version history and its series' membership for that kind.
// revision records the owner's revision when the epoch opened, so an assertion that
// intervenes in either domain closes the epoch instead of moving an already emitted
// boundary across it.
type metadataBatchObservation struct {
	first, last      int
	earliest, latest int64
	revision         int
}

// metadataBatchOwner is one owner's content in the batch and the number of
// assertions that ended an earlier observation epoch of it.
type metadataBatchOwner struct {
	content  string
	revision int
}

type targetInfoBatchKey struct {
	seriesHash uint64
	timestamp  int64
}

type otapBatchBuilder struct {
	settings otlpprw.Settings
	batch    Batch

	resourceAttrs *commonotlp.AttributesStore[uint16]
	scopeAttrs    *commonotlp.AttributesStore[uint16]
	pointAttrs    *commonotlp.AttributesStore[uint32]

	series         map[uint64]int
	observations   map[metadataBatchKey]*metadataBatchObservation
	owners         map[OwnerKey]metadataBatchOwner
	seriesOwners   map[metadataSeriesKind]OwnerKey
	translated     map[uint16]otapMetricTranslation
	labelNames     map[string]otapLabelNameResult
	resourceBounds map[uint16]otapResourceBounds
	targetInfoSeen map[targetInfoBatchKey]struct{}
	appended       int
	// counts is plain batch-local mechanism accounting. It is copied onto the
	// batch when the build succeeds and published by the commit, so nothing here
	// costs a per-sample atomic.
	counts NativeDerivationStats
}

// buildBatch converts one batch worth of records into a single transaction-local
// [Batch] and reports how many samples it holds.
//
// It never returns a partially converted batch: any payload or data point row the
// native sink cannot convert fails the whole call with [ErrUnsupportedPayload].
// Publishing part of a batch would acknowledge samples the store does not hold,
// which is worse than rejecting the batch the client can still choose to re-send
// elsewhere. An empty batch, or a batch whose metrics carry no data points, is a
// no-op: it holds nothing that could be lost.
func (s *OTAPSink) buildBatch(records []*record_message.RecordMessage) (Batch, int, error) {
	var metricsRecord, resourceRecord, scopeRecord, numberDPRecord, pointAttrsRecord *record_message.RecordMessage
	for _, record := range records {
		switch record.PayloadType() {
		case colarspb.ArrowPayloadType_UNIVARIATE_METRICS:
			metricsRecord = record
		case colarspb.ArrowPayloadType_RESOURCE_ATTRS:
			resourceRecord = record
		case colarspb.ArrowPayloadType_SCOPE_ATTRS:
			scopeRecord = record
		case colarspb.ArrowPayloadType_NUMBER_DATA_POINTS:
			numberDPRecord = record
		case colarspb.ArrowPayloadType_NUMBER_DP_ATTRS:
			pointAttrsRecord = record
		}
	}

	// Naming the unsupported payloads before decoding anything keeps the error about
	// the input rather than about a symptom of it.
	if unsupported := otapUnsupportedPayloads(records); len(unsupported) > 0 {
		return Batch{}, 0, fmt.Errorf("%w: %s; nothing was appended",
			ErrUnsupportedPayload, strings.Join(unsupported, ", "))
	}

	numberDPRows := 0
	if numberDPRecord != nil {
		numberDPRows = int(numberDPRecord.Record().NumRows())
	}
	switch {
	case numberDPRows == 0:
		// An empty batch, or metrics without data points. Nothing exists that the
		// sink could convert and therefore nothing exists that it could drop.
		return Batch{}, 0, nil
	case metricsRecord == nil:
		return Batch{}, 0, fmt.Errorf("%w: %s without a metrics payload; nothing was appended",
			ErrUnsupportedPayload, otapRowCount(numberDPRows, "number data point row", "number data point rows"))
	}

	resourceAttrs, err := decodeOTAPAttributes[uint16](resourceRecord)
	if err != nil {
		return Batch{}, 0, fmt.Errorf("decoding Resource attributes: %w", err)
	}
	scopeAttrs, err := decodeOTAPAttributes[uint16](scopeRecord)
	if err != nil {
		return Batch{}, 0, fmt.Errorf("decoding Scope attributes: %w", err)
	}
	pointAttrs, err := decodeOTAPAttributes[uint32](pointAttrsRecord)
	if err != nil {
		return Batch{}, 0, fmt.Errorf("decoding DataPoint attributes: %w", err)
	}
	metrics, err := readOTAPMetricContexts(metricsRecord.Record(), resourceAttrs)
	if err != nil {
		return Batch{}, 0, err
	}

	builder := &otapBatchBuilder{
		settings:       s.settings(),
		resourceAttrs:  resourceAttrs,
		scopeAttrs:     scopeAttrs,
		pointAttrs:     pointAttrs,
		series:         map[uint64]int{},
		observations:   map[metadataBatchKey]*metadataBatchObservation{},
		owners:         map[OwnerKey]metadataBatchOwner{},
		seriesOwners:   map[metadataSeriesKind]OwnerKey{},
		translated:     map[uint16]otapMetricTranslation{},
		labelNames:     map[string]otapLabelNameResult{},
		resourceBounds: map[uint16]otapResourceBounds{},
		targetInfoSeen: map[targetInfoBatchKey]struct{}{},
	}
	if err := builder.appendNumberDataPoints(numberDPRecord.Record(), metrics); err != nil {
		return Batch{}, 0, err
	}
	if err := builder.appendTargetInfo(); err != nil {
		return Batch{}, 0, err
	}
	builder.sortSamples()
	builder.recordBatchShape()
	builder.batch.nativeDerivation = builder.counts
	return builder.batch, builder.appended, nil
}

// recordBatchShape records the corpus facts of the finished batch: what it holds,
// not how much derivation it needed.
func (b *otapBatchBuilder) recordBatchShape() {
	b.counts.Batches = 1
	b.counts.Samples = uint64(b.appended)
	b.counts.Series = uint64(len(b.batch.Series))
	b.counts.Resources = uint64(len(b.resourceBounds))
	for owner := range b.owners {
		b.counts.OwnersByKind[owner.Kind]++
	}
}

// otapUnsupportedPayloads describes every payload of a batch that carries rows the
// native sink cannot convert, ordered by payload type so the same input always
// produces the same error.
func otapUnsupportedPayloads(records []*record_message.RecordMessage) []string {
	rows := map[colarspb.ArrowPayloadType]int{}
	for _, record := range records {
		kind := record.PayloadType()
		if _, unsupported := otapUnsupportedPayloadName(kind); !unsupported {
			continue
		}
		// A payload without rows drops nothing, so it is not a reason to reject.
		if n := int(record.Record().NumRows()); n > 0 {
			rows[kind] += n
		}
	}
	kinds := slices.Sorted(maps.Keys(rows))
	unsupported := make([]string, len(kinds))
	for i, kind := range kinds {
		name, _ := otapUnsupportedPayloadName(kind)
		unsupported[i] = fmt.Sprintf("%s (%s)", name, otapRowCount(rows[kind], "row", "rows"))
	}
	return unsupported
}

// otapUnsupportedPayloadName names a payload the native sink does not convert and
// reports whether records of that payload type are rejected. Attribute payloads
// carry no row of their own and travel with the payload that owns them, so only
// the owning payload is named. Payload types the sink does not know are rejected
// by name: failing closed is the only safe default for data of unknown shape.
func otapUnsupportedPayloadName(kind colarspb.ArrowPayloadType) (string, bool) {
	switch kind {
	case colarspb.ArrowPayloadType_RESOURCE_ATTRS,
		colarspb.ArrowPayloadType_SCOPE_ATTRS,
		colarspb.ArrowPayloadType_UNIVARIATE_METRICS,
		colarspb.ArrowPayloadType_NUMBER_DATA_POINTS,
		colarspb.ArrowPayloadType_NUMBER_DP_ATTRS,
		colarspb.ArrowPayloadType_SUMMARY_DP_ATTRS,
		colarspb.ArrowPayloadType_HISTOGRAM_DP_ATTRS,
		colarspb.ArrowPayloadType_EXP_HISTOGRAM_DP_ATTRS,
		colarspb.ArrowPayloadType_NUMBER_DP_EXEMPLAR_ATTRS,
		colarspb.ArrowPayloadType_HISTOGRAM_DP_EXEMPLAR_ATTRS,
		colarspb.ArrowPayloadType_EXP_HISTOGRAM_DP_EXEMPLAR_ATTRS:
		return "", false
	case colarspb.ArrowPayloadType_SUMMARY_DATA_POINTS:
		return "summary data points", true
	case colarspb.ArrowPayloadType_HISTOGRAM_DATA_POINTS:
		return "classic histogram data points", true
	case colarspb.ArrowPayloadType_EXP_HISTOGRAM_DATA_POINTS:
		return "exponential histogram data points", true
	case colarspb.ArrowPayloadType_NUMBER_DP_EXEMPLARS:
		return "number data point exemplars", true
	case colarspb.ArrowPayloadType_HISTOGRAM_DP_EXEMPLARS:
		return "classic histogram data point exemplars", true
	case colarspb.ArrowPayloadType_EXP_HISTOGRAM_DP_EXEMPLARS:
		return "exponential histogram data point exemplars", true
	case colarspb.ArrowPayloadType_MULTIVARIATE_METRICS:
		return "multivariate metrics", true
	default:
		return fmt.Sprintf("payload type %s", kind), true
	}
}

// otapRowCount renders a row count with the noun that agrees with it.
func otapRowCount(rows int, singular, plural string) string {
	if rows == 1 {
		return fmt.Sprintf("%d %s", rows, singular)
	}
	return fmt.Sprintf("%d %s", rows, plural)
}

func decodeOTAPAttributes[unsigned constraints.Unsigned](record *record_message.RecordMessage) (*commonotlp.AttributesStore[unsigned], error) {
	store := commonotlp.NewAttributesStore[unsigned]()
	if record == nil {
		return store, nil
	}
	if err := commonotlp.AttributesStoreFrom(record.Record(), store); err != nil {
		return nil, err
	}
	return store, nil
}

func (b *otapBatchBuilder) appendNumberDataPoints(rec arrow.RecordBatch, metrics map[uint16]otapMetricContext) error {
	idCol := column(rec, "id")
	parentCol := column(rec, "parent_id")
	timeCol := column(rec, "time_unix_nano")
	startCol := column(rec, "start_time_unix_nano")
	flagsCol := column(rec, "flags")
	intCol := column(rec, "int_value")
	doubleCol := column(rec, "double_value")
	if parentCol == nil || timeCol == nil {
		return errors.New("columnar: OTAP data points record lacks parent_id or time_unix_nano")
	}
	if _, ok := timeCol.(*array.Timestamp); !ok {
		return errors.New("columnar: OTAP time_unix_nano is not a timestamp")
	}

	var metricID uint16
	var pointID uint32
	for row := range int(rec.NumRows()) {
		// Both IDs are delta encoded within their respective parent sequences.
		metricID += uint16(uintValue(parentCol, row))
		ctx, ok := metrics[metricID]
		if !ok {
			return fmt.Errorf("%w: number data point row %d refers to metric %d, which the sink does not convert (unnamed or unsupported metric row); nothing was appended",
				ErrUnsupportedPayload, row, metricID)
		}

		var attributes map[string]string
		if idCol != nil && !idCol.IsNull(row) {
			pointID += uint32(uintValue(idCol, row))
			attributes = otapAttributeStrings(b.pointAttrs.AttributesByID(pointID))
		}
		start := int64(0)
		if startCol != nil && !startCol.IsNull(row) {
			start = timestampMilliseconds(startCol, row)
		}
		t := timestampMilliseconds(timeCol, row)
		kind := SampleFloat
		if pmetric.DataPointFlags(uintValue(flagsCol, row)).NoRecordedValue() {
			kind = SampleStale
		}

		var value float64
		if kind == SampleStale {
			value = math.Float64frombits(promvalue.StaleNaN)
		}
		hasValue := kind == SampleStale
		switch {
		case hasValue:
		case doubleCol != nil && !doubleCol.IsNull(row):
			value = otapFloatValue(doubleCol, row)
			hasValue = true
		case intCol != nil && !intCol.IsNull(row):
			value = float64(signedValue(intCol, row))
			hasValue = true
		}
		if !hasValue {
			return fmt.Errorf("%w: number data point row %d of metric %q carries neither an integer nor a double value; nothing was appended",
				ErrUnsupportedPayload, row, ctx.metric.Name())
		}

		translation, err := b.translateMetric(metricID, ctx)
		if err != nil {
			return err
		}
		ls, err := b.buildLabels(ctx, translation, attributes)
		if err != nil {
			return err
		}
		if err := b.appendSample(ls, ctx, translation.content, t, start, value, kind); err != nil {
			return err
		}

		bounds, exists := b.resourceBounds[ctx.resource.id]
		if !exists {
			bounds = otapResourceBounds{resource: ctx.resource, min: t, max: t}
		} else {
			bounds.min = min(bounds.min, t)
			bounds.max = max(bounds.max, t)
		}
		b.resourceBounds[ctx.resource.id] = bounds
	}
	return nil
}

func otapFloatValue(column arrow.Array, row int) float64 {
	if dictionary, ok := column.(*array.Dictionary); ok {
		return otapFloatValue(dictionary.Dictionary(), dictionary.GetValueIndex(row))
	}
	return column.(*array.Float64).Value(row)
}

func (b *otapBatchBuilder) appendSample(
	ls labels.Labels,
	ctx otapMetricContext,
	metricContent Content,
	t, start int64,
	value float64,
	kind SampleKind,
) error {
	hash := stableSeriesHash(ls)
	b.counts.SeriesLookupHashes++
	index, exists := b.series[hash]
	if !exists {
		index = len(b.batch.Series)
		b.series[hash] = index
		b.batch.Series = append(b.batch.Series, BatchSeries{Labels: ls.Copy()})
		b.counts.SeriesLookupNew++
	} else if !labels.Equal(b.batch.Series[index].Labels, ls) {
		return fmt.Errorf("%w: %q and %q", ErrSeriesHashCollision, b.batch.Series[index].Labels, ls)
	} else {
		b.counts.SeriesLookupHits++
	}
	series := &b.batch.Series[index]
	series.Timestamps = append(series.Timestamps, t)
	series.StartTimestamps = append(series.StartTimestamps, start)
	series.Kinds = append(series.Kinds, kind)
	series.Values = append(series.Values, value)
	series.Histograms = append(series.Histograms, nil)
	b.appended++

	b.counts.OwnerConstructors[MetadataResource]++
	b.counts.AssocKeyHashes[NativeDerivationResource]++
	b.associate(ls, ctx.resource.owner, Content{Attributes: ctx.resource.descriptive}, t)
	b.counts.OwnerConstructors[MetadataResourceSchema]++
	b.counts.AssocKeyHashes[NativeDerivationResourceSchema]++
	b.counts.SchemaContentDerivations++
	b.associate(ls, ResourceSchemaOwner(ctx.resource.owner), schemaContent(ctx.resource.schemaURL), t)
	if ctx.hasScope {
		b.counts.OwnerConstructors[MetadataScope]++
		b.counts.AssocKeyHashes[NativeDerivationScope]++
		b.counts.SchemaContentDerivations++
		b.associate(ls, ScopeOwner(ctx.scopeName, ctx.scopeVersion), schemaContent(ctx.scopeSchemaURL), t)
	}
	// OTAP has no authoritative scrape cadence, so no Scrape owner is invented.
	b.counts.OwnerConstructors[MetadataSource]++
	b.counts.AssocKeyHashes[NativeDerivationSource]++
	b.associate(ls, SourceOwner("otap"), Content{Attributes: map[string]string{"transport": "otap"}}, t)
	b.counts.OwnerConstructors[MetadataMetric]++
	b.counts.AssocKeyHashes[NativeDerivationMetric]++
	b.associate(ls, MetricOwner(ls), metricContent, t)
	return nil
}

// associate records one observation of a series' metadata context. Repeated
// observations of unchanged content are not extra change points, but they are not
// dropped either: the batch keeps the earliest observation as the change point and
// the latest one as the observation bound. Coalescing them to the earliest timestamp
// alone would lose the bound, and emitting one update per sample would make every
// unchanged sample a metadata transaction.
//
// Folding is confined to one open epoch. An assertion that changes the owner's content
// or moves a series' membership of that kind to another owner intervenes in the same
// temporal domain, so a later re-statement of the earlier content is emitted as its own
// ordered change point instead of rewriting an earlier observation bound.
func (b *otapBatchBuilder) associate(ls labels.Labels, owner OwnerKey, content Content, effectiveFrom int64) {
	b.counts.ContentDerivations++
	key := metadataBatchKey{seriesHash: labels.StableHash(ls), owner: owner, content: content.Canonical()}
	b.counts.ContentCanonicalizations++
	revision := b.observeOwnerContent(owner, key.content, key.seriesHash)
	observation, exists := b.observations[key]
	if exists && observation.revision == revision {
		b.counts.FoldedObservations++
		observation.earliest = min(observation.earliest, effectiveFrom)
		observation.latest = max(observation.latest, effectiveFrom)
		b.batch.Updates[observation.first].Update.EffectiveFrom = observation.earliest
		switch {
		case observation.last != observation.first:
			b.batch.Updates[observation.last].Update.EffectiveFrom = observation.latest
		case observation.latest != observation.earliest:
			// The context now spans two instants, so it needs a second assertion. The
			// first entry keeps the change point.
			b.batch.Updates = append(b.batch.Updates, AssociationUpdate{Labels: ls, Update: Update{
				Owner: owner, Content: content, EffectiveFrom: observation.latest,
			}})
			observation.last = len(b.batch.Updates) - 1
			b.counts.EmittedObservationBounds++
		}
		return
	}
	if exists {
		b.counts.EpochRestatements++
	}
	b.batch.Updates = append(b.batch.Updates, AssociationUpdate{Labels: ls, Update: Update{
		Owner: owner, Content: content, EffectiveFrom: effectiveFrom,
	}})
	b.counts.EmittedChangePoints++
	b.observations[key] = &metadataBatchObservation{
		first: len(b.batch.Updates) - 1, last: len(b.batch.Updates) - 1,
		earliest: effectiveFrom, latest: effectiveFrom, revision: revision,
	}
}

// observeOwnerContent records one assertion against the owner and series domains and
// returns the owner's revision an epoch opened now must carry. A changed content for
// the owner, or a different owner for the same series and kind, ends the observation
// epochs that preceded it. A repeated assertion writes nothing, so the steady-state
// path stays one map read per owner and series domain.
func (b *otapBatchBuilder) observeOwnerContent(owner OwnerKey, content string, seriesHash uint64) int {
	b.counts.EpochOwnerChecks++
	b.counts.EpochMembershipChecks++
	state := b.owners[owner]
	if content != state.content {
		if state.content != "" {
			state.revision++
			b.counts.OwnerRevisionChanges++
		}
		state.content = content
		b.owners[owner] = state
	}

	seriesKind := metadataSeriesKind{seriesHash: seriesHash, kind: owner.Kind}
	if previous, seen := b.seriesOwners[seriesKind]; !seen {
		b.seriesOwners[seriesKind] = owner
	} else if previous != owner {
		previousState := b.owners[previous]
		previousState.revision++
		b.owners[previous] = previousState
		b.seriesOwners[seriesKind] = owner
		b.counts.OwnerRevisionChanges++
	}
	return state.revision
}

func (b *otapBatchBuilder) sortSamples() {
	for i := range b.batch.Series {
		series := &b.batch.Series[i]
		order := make([]int, len(series.Timestamps))
		for j := range order {
			order[j] = j
		}
		slices.SortStableFunc(order, func(a, c int) int {
			switch {
			case series.Timestamps[a] < series.Timestamps[c]:
				return -1
			case series.Timestamps[a] > series.Timestamps[c]:
				return 1
			default:
				return 0
			}
		})
		if slices.IsSortedFunc(series.Timestamps, cmp.Compare[int64]) {
			continue
		}
		sortedTimestamps := make([]int64, len(order))
		sortedStart := make([]int64, len(order))
		sortedKinds := make([]SampleKind, len(order))
		sortedValues := make([]float64, len(order))
		sortedHistograms := make([][]byte, len(order))
		for j, old := range order {
			sortedTimestamps[j] = series.Timestamps[old]
			if len(series.StartTimestamps) > 0 {
				sortedStart[j] = series.StartTimestamps[old]
			}
			sortedKinds[j] = series.Kinds[old]
			sortedValues[j] = series.Values[old]
			if len(series.Histograms) > 0 {
				sortedHistograms[j] = series.Histograms[old]
			}
		}
		series.Timestamps = sortedTimestamps
		series.StartTimestamps = sortedStart
		series.Kinds = sortedKinds
		series.Values = sortedValues
		series.Histograms = sortedHistograms
	}
}

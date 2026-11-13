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
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	commonotlp "github.com/open-telemetry/otel-arrow/go/pkg/otel/common/otlp"
	"github.com/prometheus/common/model"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/prometheus/prometheus/model/labels"
	otlpprw "github.com/prometheus/prometheus/storage/remote/otlptranslator/prometheusremotewrite"
)

var otelIdentifyingResourceAttributes = [...]string{"service.name", "service.namespace", "service.instance.id"}

type otapResourceContext struct {
	id          uint16
	attributes  map[string]string
	descriptive map[string]string
	owner       OwnerKey
	schemaURL   string
}

type otapMetricContext struct {
	metric         pmetric.Metric
	resource       otapResourceContext
	hasScope       bool
	scopeID        uint16
	scopeName      string
	scopeVersion   string
	scopeSchemaURL string
	targetInfo     bool
}

type otapMetricTranslation struct {
	name    string
	content Content
}

type otapLabelNameResult struct {
	name string
	err  error
}

func readOTAPMetricContexts(rec arrow.RecordBatch, resourceAttrs *commonotlp.AttributesStore[uint16]) (map[uint16]otapMetricContext, error) {
	idCol := column(rec, "id")
	nameCol := column(rec, "name")
	if idCol == nil || nameCol == nil {
		return nil, errors.New("columnar: OTAP metrics record lacks id or name")
	}
	resourceIDs := otapStructColumn(rec, "resource", "id")
	resourceSchemas := otapStructColumn(rec, "resource", "schema_url")
	scopeIDs := otapStructColumn(rec, "scope", "id")
	scopeNames := otapStructColumn(rec, "scope", "name")
	scopeVersions := otapStructColumn(rec, "scope", "version")
	scopeCol := column(rec, "scope")
	scopeSchemas := column(rec, "schema_url")
	typeCol := column(rec, "metric_type")
	descriptionCol := column(rec, "description")
	unitCol := column(rec, "unit")
	temporalityCol := column(rec, "aggregation_temporality")
	monotonicCol := column(rec, "is_monotonic")

	metrics := map[uint16]otapMetricContext{}
	resources := map[uint16]otapResourceContext{}
	// Every ID of this record is delta encoded: the first value of a record is the
	// initial ID and each later value is that row's delta from the previous one.
	// All three cursors therefore advance for every row, including rows this
	// function filters out or that carry no data points, because a skipped row
	// still occupies its position in the sequence.
	var metricID, resourceID, scopeID uint16
	for row := range int(rec.NumRows()) {
		metricID += uint16(uintValue(idCol, row))
		resourceID += uint16(uintValue(resourceIDs, row))
		scopeID += uint16(uintValue(scopeIDs, row))
		metric := pmetric.NewMetric()
		metric.SetName(stringValue(nameCol, row))
		metric.SetDescription(stringValue(descriptionCol, row))
		metric.SetUnit(stringValue(unitCol, row))

		metricType := pmetric.MetricType(uintValue(typeCol, row))
		switch metricType {
		case pmetric.MetricTypeGauge:
			metric.SetEmptyGauge()
		case pmetric.MetricTypeSum:
			sum := metric.SetEmptySum()
			sum.SetAggregationTemporality(pmetric.AggregationTemporality(signedValue(temporalityCol, row)))
			sum.SetIsMonotonic(otapBoolValue(monotonicCol, row))
		default:
			// Other signals do not use the accepted NumberDataPoints path.
			continue
		}
		if metric.Name() == "" {
			continue
		}

		resource, exists := resources[resourceID]
		if !exists {
			resource = newOTAPResourceContext(resourceID, otapAttributeStrings(resourceAttrs.AttributesByID(resourceID)))
			resources[resourceID] = resource
		}
		resource.schemaURL = stringValue(resourceSchemas, row)

		metrics[metricID] = otapMetricContext{
			metric: metric, resource: resource,
			hasScope:  scopeCol != nil && !scopeCol.IsNull(row),
			scopeID:   scopeID,
			scopeName: stringValue(scopeNames, row), scopeVersion: stringValue(scopeVersions, row),
			scopeSchemaURL: stringValue(scopeSchemas, row),
		}
	}
	return metrics, nil
}

func newOTAPResourceContext(id uint16, attributes map[string]string) otapResourceContext {
	// An empty identity is a deliberate compatibility owner shared by inputs that
	// carry none of the known identifying attributes; it is not inferred from time.
	descriptive := map[string]string{}
	for key, value := range attributes {
		if slices.Contains(otelIdentifyingResourceAttributes[:], key) {
			continue
		}
		descriptive[key] = value
	}
	return otapResourceContext{
		id: id, attributes: attributes, descriptive: descriptive,
		owner: ResourceOwner(attributes),
	}
}

func otapStructColumn(rec arrow.RecordBatch, parent, child string) arrow.Array {
	column := column(rec, parent)
	parentStruct, ok := column.(*array.Struct)
	if !ok {
		return nil
	}
	typeStruct, ok := parentStruct.DataType().(*arrow.StructType)
	if !ok {
		return nil
	}
	index, ok := typeStruct.FieldIdx(child)
	if !ok {
		return nil
	}
	return parentStruct.Field(index)
}

func otapBoolValue(column arrow.Array, row int) bool {
	if column == nil || column.IsNull(row) {
		return false
	}
	if dictionary, ok := column.(*array.Dictionary); ok {
		return otapBoolValue(dictionary.Dictionary(), dictionary.GetValueIndex(row))
	}
	return column.(*array.Boolean).Value(row)
}

func otapAttributeStrings(attributes *pcommon.Map) map[string]string {
	if attributes == nil {
		return nil
	}
	out := make(map[string]string, attributes.Len())
	attributes.Range(func(key string, value pcommon.Value) bool {
		out[key] = value.AsString()
		return true
	})
	return out
}

func (b *otapBatchBuilder) translateMetric(id uint16, ctx otapMetricContext) (otapMetricTranslation, error) {
	if translated, ok := b.translated[id]; ok {
		b.counts.TranslationHits++
		return translated, nil
	}
	translated, err := b.translateMetricContext(ctx)
	if err != nil {
		return otapMetricTranslation{}, err
	}
	b.counts.TranslationMisses++
	b.translated[id] = translated
	return translated, nil
}

func (b *otapBatchBuilder) translateMetricContext(ctx otapMetricContext) (otapMetricTranslation, error) {
	name, err := (&otlptranslator.MetricNamer{
		Namespace: b.settings.Namespace, WithMetricSuffixes: b.settings.AddMetricSuffixes,
		UTF8Allowed: b.settings.AllowUTF8,
	}).Build(otlpprw.TranslatorMetricFromOtelMetric(ctx.metric))
	if err != nil {
		return otapMetricTranslation{}, fmt.Errorf("translating metric %q: %w", ctx.metric.Name(), err)
	}
	translated := otapMetricTranslation{
		name: name,
		content: Content{Attributes: map[string]string{
			"help": ctx.metric.Description(),
			"type": string(nativeOTLPMetricType(ctx.metric)),
			"unit": (&otlptranslator.UnitNamer{UTF8Allowed: b.settings.AllowUTF8}).Build(ctx.metric.Unit()),
		}},
	}
	return translated, nil
}

func nativeOTLPMetricType(metric pmetric.Metric) model.MetricType {
	switch metric.Type() {
	case pmetric.MetricTypeGauge:
		return model.MetricTypeGauge
	case pmetric.MetricTypeSum:
		if metric.Sum().AggregationTemporality() == pmetric.AggregationTemporalityDelta {
			return model.MetricTypeUnknown
		}
		if metric.Sum().IsMonotonic() {
			return model.MetricTypeCounter
		}
		return model.MetricTypeGauge
	default:
		return model.MetricTypeUnknown
	}
}

func (b *otapBatchBuilder) labelName(name string) (string, error) {
	if cached, ok := b.labelNames[name]; ok {
		b.counts.LabelNameHits++
		return cached.name, cached.err
	}
	b.counts.LabelNameMisses++
	result := otapLabelNameResult{}
	result.name, result.err = (&otlptranslator.LabelNamer{
		UTF8Allowed:                 b.settings.AllowUTF8,
		UnderscoreLabelSanitization: b.settings.LabelNameUnderscoreSanitization, //nolint:staticcheck
		PreserveMultipleUnderscores: b.settings.LabelNamePreserveMultipleUnderscores,
	}).Build(name)
	b.labelNames[name] = result
	return result.name, result.err
}

func (b *otapBatchBuilder) buildLabels(ctx otapMetricContext, translation otapMetricTranslation, attributes map[string]string) (labels.Labels, error) {
	b.counts.LabelBuilds++
	builder := labels.NewBuilder(labels.EmptyLabels())
	present := map[string]struct{}{}

	// Point attributes win over promoted Resource labels and concatenate the
	// legacy label-name collisions in deterministic original-key order.
	for _, name := range slices.Sorted(maps.Keys(attributes)) {
		if name == labels.MetricName {
			continue
		}
		output, err := b.labelName(name)
		if err != nil {
			return labels.EmptyLabels(), err
		}
		value := attributes[name]
		if _, exists := present[output]; exists {
			builder.Set(output, builder.Get(output)+";"+value)
			continue
		}
		builder.Set(output, value)
		present[output] = struct{}{}
	}

	for _, name := range slices.Sorted(maps.Keys(ctx.resource.attributes)) {
		if !b.settings.PromoteResourceAttributes.Promotes(name) {
			continue
		}
		output, err := b.labelName(name)
		if err != nil {
			return labels.EmptyLabels(), err
		}
		if _, exists := present[output]; exists {
			continue
		}
		builder.Set(output, ctx.resource.attributes[name])
		present[output] = struct{}{}
	}

	job := ctx.resource.attributes["service.name"]
	if namespace := ctx.resource.attributes["service.namespace"]; namespace != "" && job != "" {
		job = namespace + "/" + job
	}
	if job != "" {
		builder.Set(model.JobLabel, job)
		present[model.JobLabel] = struct{}{}
	}
	if instance := ctx.resource.attributes["service.instance.id"]; instance != "" {
		builder.Set(model.InstanceLabel, instance)
		present[model.InstanceLabel] = struct{}{}
	}

	for _, name := range slices.Sorted(maps.Keys(b.settings.ExternalLabels)) {
		if _, exists := present[name]; exists {
			continue
		}
		builder.Set(name, b.settings.ExternalLabels[name])
		present[name] = struct{}{}
	}

	if b.settings.PromoteScopeMetadata && ctx.hasScope && ctx.scopeName != "" {
		scope := map[string]string{
			"otel_scope_name": ctx.scopeName, "otel_scope_version": ctx.scopeVersion,
			"otel_scope_schema_url": ctx.scopeSchemaURL,
		}
		if attributes := b.scopeAttrs.AttributesByID(ctx.scopeID); attributes != nil {
			attributes.Range(func(name string, value pcommon.Value) bool {
				scope["otel_scope_"+name] = value.AsString()
				return true
			})
		}
		for _, name := range slices.Sorted(maps.Keys(scope)) {
			output, err := b.labelName(name)
			if err != nil {
				return labels.EmptyLabels(), err
			}
			builder.Set(output, scope[name])
			present[output] = struct{}{}
		}
	}

	if b.settings.EnableTypeAndUnitLabels && !ctx.targetInfo {
		metricType := translation.content.Attributes["type"]
		if metricType != "" && metricType != string(model.MetricTypeUnknown) {
			builder.Set(model.MetricTypeLabel, metricType)
		}
		if unit := translation.content.Attributes["unit"]; unit != "" {
			builder.Set(model.MetricUnitLabel, unit)
		}
	}

	builder.Set(labels.MetricName, translation.name)
	b.counts.OwnerReferences++
	builder.Set(OTELResourceLabel, ctx.resource.owner.Ref())
	return builder.Labels(), nil
}

func schemaContent(schemaURL string) Content {
	return Content{Attributes: map[string]string{"schema_url": schemaURL}}
}

func (b *otapBatchBuilder) appendTargetInfo() error {
	if b.settings.DisableTargetInfo {
		return nil
	}
	for _, resourceID := range slices.Sorted(maps.Keys(b.resourceBounds)) {
		bounds := b.resourceBounds[resourceID]
		resource := bounds.resource
		identifyingCount := 0
		for _, name := range otelIdentifyingResourceAttributes {
			if _, ok := resource.attributes[name]; ok {
				identifyingCount++
			}
		}
		if len(resource.attributes) == identifyingCount {
			continue
		}

		metric := pmetric.NewMetric()
		metric.SetName("target_info")
		metric.SetDescription("Target metadata")
		metric.SetEmptyGauge()
		ctx := otapMetricContext{metric: metric, resource: resource, targetInfo: true}
		translation, err := b.translateMetricContext(ctx)
		if err != nil {
			return err
		}
		attributes := resource.descriptive
		if b.settings.KeepIdentifyingResourceAttributes {
			attributes = resource.attributes
		}
		// One template per Resource: every synthetic target_info sample of this
		// Resource states the same context.
		template, err := b.buildSeriesTemplate(ctx, translation, attributes)
		if err != nil {
			return err
		}
		if template.labels.Get(model.JobLabel) == "" && template.labels.Get(model.InstanceLabel) == "" {
			continue
		}

		lookback := b.settings.LookbackDelta
		if lookback <= 0 {
			lookback = 5 * time.Minute
		}
		stepMilliseconds := max(int64(1), (lookback / 2).Milliseconds())
		for timestamp := bounds.min; timestamp < bounds.max; {
			if err := b.appendTargetInfoSample(template, timestamp); err != nil {
				return err
			}
			next := timestamp + stepMilliseconds
			if next <= timestamp || next >= bounds.max {
				break
			}
			timestamp = next
		}
		if err := b.appendTargetInfoSample(template, bounds.max); err != nil {
			return err
		}
	}
	return nil
}

func (b *otapBatchBuilder) appendTargetInfoSample(template *otapSeriesTemplate, timestamp int64) error {
	key := targetInfoBatchKey{seriesHash: template.hash, timestamp: timestamp}
	if _, exists := b.targetInfoSeen[key]; exists {
		return nil
	}
	b.targetInfoSeen[key] = struct{}{}
	return b.appendSample(template, timestamp, 0, 1, SampleFloat)
}

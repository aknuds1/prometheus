// Copyright 2025 The Prometheus Authors
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

package semconv

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/otlptranslator"

	"github.com/prometheus/prometheus/model/labels"
)

const cacheTTL = 1 * time.Hour

type schemaEngine struct {
	otelSchemaCache *cache[otelSchema]
	semconvCache    *cache[semconv]
}

func newSchemaEngine() *schemaEngine {
	return &schemaEngine{
		otelSchemaCache: newCache[otelSchema](),
		semconvCache:    newCache[semconv](),
	}
}

type matcherBuilder struct {
	metadata struct {
		Name string
		Type model.MetricType
		Unit string
	}
	other []*labels.Matcher
}

func newMatcherBuilder(matchers []*labels.Matcher) (matcherBuilder, error) {
	var b matcherBuilder
	for _, m := range matchers {
		switch m.Name {
		case model.MetricNameLabel:
			if m.Type != labels.MatchEqual {
				return b, errors.New("__name__ matcher must be equal")
			}
			b.metadata.Name = m.Value
		case model.MetricTypeLabel:
			if m.Type != labels.MatchEqual {
				return b, errors.New("__type__ matcher must be equal")
			}
			b.metadata.Type = model.MetricType(m.Value)
		case model.MetricUnitLabel:
			if m.Type != labels.MatchEqual {
				return b, errors.New("__unit__ matcher must be equal")
			}
			b.metadata.Unit = m.Value
		case schemaURLLabel, semconvURLLabel:
			// Skip schema/semconv labels as we will be querying to different versions.
		default:
			b.other = append(b.other, m)
		}
	}
	return b, nil
}

func (b matcherBuilder) Clone() matcherBuilder {
	return matcherBuilder{
		metadata: b.metadata,
		other:    slices.Clone(b.other),
	}
}

type queryContext struct {
	changes []change
	// otelLabelMapping is a mapping for reverse translation (nil if no schema).
	otelLabelMapping *otelLabelMapping
}

// FindMatcherVariants returns all variants to match for a single schematized metric selection.
// semconvURL points to a semantic conventions file (groups with metric metadata) and is required.
// schemaURL points to an OTel schema file (versions with attribute renames) and is optional.
// Returns variants for all OTLP translation strategies and a label mapping
// for transforming results back to original OTLP names.
// It returns an error if semconvURL is not provided or if the metric is not found.
func (e *schemaEngine) FindMatcherVariants(semconvURL, schemaURL string, originalMatchers []*labels.Matcher) ([][]*labels.Matcher, queryContext, error) {
	if semconvURL == "" {
		return nil, queryContext{}, errors.New("semconvURL is required")
	}

	// Fetch semantic conventions for metric metadata.
	sc, ok := e.semconvCache.get(semconvURL)
	if !ok {
		var err error
		sc, err = fetchSemconv(semconvURL)
		if err != nil {
			return nil, queryContext{}, err
		}
		e.semconvCache.set(semconvURL, sc)
	}

	matchers, err := newMatcherBuilder(originalMatchers)
	if err != nil {
		return nil, queryContext{}, err
	}

	metricName := matchers.metadata.Name
	meta, ok := sc.getMetricMetadata(metricName)
	if !ok {
		return nil, queryContext{}, fmt.Errorf("metric %q not found in groups of semconv %q", metricName, semconvURL)
	}

	variants, err := generateOTLPVariants(originalMatchers, otlptranslator.Metric{
		Name: metricName,
		Unit: meta.Unit,
		Type: promTypeToOTelType(meta.Type),
	})
	if err != nil {
		return nil, queryContext{}, err
	}

	attributes := sc.attributesPerMetric[metricName]

	// If schemaURL is provided, merge in additional attributes from schema (for renames).
	if schemaURL != "" {
		schema, ok := e.otelSchemaCache.get(schemaURL)
		if !ok {
			schema, err = fetchOTelSchema(schemaURL)
			if err != nil {
				return nil, queryContext{}, err
			}
			e.otelSchemaCache.set(schemaURL, schema)
		}
		// Merge schema attributes (which include renamed versions).
		seen := make(map[string]struct{}, len(attributes))
		for _, attr := range attributes {
			seen[attr] = struct{}{}
		}
		for _, attr := range schema.getAttributesForMetric(metricName) {
			if _, ok := seen[attr]; !ok {
				attributes = append(attributes, attr)
			}
		}
	}

	return variants, queryContext{
		// TODO: Explain why necessary.
		changes:          []change{{}},
		otelLabelMapping: buildLabelMapping(metricName, attributes),
	}, nil
}

// TransformSeries returns transformed series for OTLP data.
// It transforms labels back to original OTLP names using the label mapping.
func (*schemaEngine) TransformSeries(q queryContext, originalLabels labels.Labels) (labels.Labels, error) {
	// If we have a label mapping from a schema, transform back to original names.
	if q.otelLabelMapping != nil {
		return transformOTelSchemaLabels(originalLabels, q.otelLabelMapping), nil
	}

	schemaURL := originalLabels.Get(schemaURLLabel)
	if schemaURL == "" {
		return originalLabels, nil
	}

	// Remove __schema_url__.
	builder := labels.NewBuilder(originalLabels)
	builder.Del(schemaURLLabel)
	return builder.Labels(), nil
}

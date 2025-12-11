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
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/otlptranslator"

	"github.com/prometheus/prometheus/model/labels"
)

// semconv represents a semantic conventions file containing metric definitions.
// See: https://github.com/open-telemetry/semantic-conventions
//
// Example semconv YAML:
//
//	groups:
//	  - id: metric.http.server.duration
//	    type: metric
//	    metric_name: http.server.duration
//	    instrument: histogram
//	    unit: s
type semconv struct {
	Groups []semconvGroup `yaml:"groups"`

	// Pre-computed lookups (populated by init()).
	metricMetadata      map[string]metricMeta
	attributesPerMetric map[string][]string
}

// otelSchema represents an OpenTelemetry schema file.
// See: https://opentelemetry.io/docs/specs/otel/schemas/file_format_v1.1.0/
//
// Example schema YAML:
//
//	file_format: 1.1.0
//	schema_url: https://example.com/schemas/1.0.0
//
//	versions:
//	  1.0.0:
//	    metrics:
//	      changes:
//	        - rename_attributes:
//	            attribute_map:
//	              http.method: http.request.method
//	            apply_to_metrics:
//	              - http.server.duration
type otelSchema struct {
	FileFormat string                       `yaml:"file_format"`
	SchemaURL  string                       `yaml:"schema_url"`
	Versions   map[string]otelSchemaVersion `yaml:"versions"`

	// Pre-computed lookups (populated by init()).
	attributesPerMetric map[string]map[string]struct{}
}

// semconvGroup represents a semantic conventions group definition.
type semconvGroup struct {
	ID         string             `yaml:"id"`
	Type       string             `yaml:"type"`        // "metric", "attribute", "span", etc.
	MetricName string             `yaml:"metric_name"` // Only for type="metric"
	Instrument string             `yaml:"instrument"`  // counter, histogram, gauge, updowncounter
	Unit       string             `yaml:"unit"`
	Attributes []semconvAttribute `yaml:"attributes,omitempty"`
}

type semconvAttribute struct {
	// Ref to attribute ID.
	Ref string `yaml:"ref"`
}

// metricMeta contains unit and type information for a metric.
type metricMeta struct {
	Unit string
	Type model.MetricType
}

type otelSchemaVersion struct {
	All     *otelSchemaSection `yaml:"all,omitempty"`
	Metrics *otelSchemaSection `yaml:"metrics,omitempty"`
}

type otelSchemaSection struct {
	Changes []otelSchemaChange `yaml:"changes,omitempty"`
}

type otelSchemaChange struct {
	RenameAttributes *otelRenameAttributes `yaml:"rename_attributes,omitempty"`
}

type otelRenameAttributes struct {
	AttributeMap   map[string]string `yaml:"attribute_map,omitempty"`
	ApplyToMetrics []string          `yaml:"apply_to_metrics,omitempty"`
}

// init builds pre-computed lookups from the parsed semconv.
func (s *semconv) init() {
	s.metricMetadata = make(map[string]metricMeta)
	s.attributesPerMetric = make(map[string][]string)
	for _, group := range s.Groups {
		if group.Type != "metric" || group.MetricName == "" {
			continue
		}

		s.metricMetadata[group.MetricName] = metricMeta{
			Unit: group.Unit,
			Type: instrumentToMetricType(group.Instrument),
		}

		if len(group.Attributes) == 0 {
			continue
		}

		attrs := make([]string, 0, len(group.Attributes))
		for _, attr := range group.Attributes {
			attrs = append(attrs, attr.Ref)
		}
		s.attributesPerMetric[group.MetricName] = attrs
	}
}

// getMetricMetadata returns unit and type for a metric from the groups section.
func (s *semconv) getMetricMetadata(name string) (metricMeta, bool) {
	meta, ok := s.metricMetadata[name]
	return meta, ok
}

// init builds pre-computed lookups from the parsed schema.
func (s *otelSchema) init() {
	s.attributesPerMetric = map[string]map[string]struct{}{
		"": {},
	}
	for _, version := range s.Versions {
		s.collectAttributesFromVersion(version)
	}
}

// instrumentToMetricType converts OTel instrument to Prometheus metric type.
func instrumentToMetricType(instrument string) model.MetricType {
	switch instrument {
	case "counter":
		return model.MetricTypeCounter
	case "histogram":
		return model.MetricTypeHistogram
	case "gauge", "updowncounter":
		return model.MetricTypeGauge
	default:
		return model.MetricTypeUnknown
	}
}

func (s *otelSchema) collectAttributesFromVersion(version otelSchemaVersion) {
	// Collect from "all" section (global attributes).
	if version.All != nil {
		for _, change := range version.All.Changes {
			if change.RenameAttributes == nil {
				continue
			}

			for oldName, newName := range change.RenameAttributes.AttributeMap {
				s.attributesPerMetric[""][oldName] = struct{}{}
				s.attributesPerMetric[""][newName] = struct{}{}
			}
		}
	}

	// Collect from metrics section.
	if version.Metrics != nil {
		for _, change := range version.Metrics.Changes {
			if change.RenameAttributes == nil {
				continue
			}

			attrs := change.RenameAttributes
			if len(attrs.ApplyToMetrics) == 0 {
				// Applies to all metrics - add to global.
				for oldName, newName := range attrs.AttributeMap {
					s.attributesPerMetric[""][oldName] = struct{}{}
					s.attributesPerMetric[""][newName] = struct{}{}
				}
			} else {
				// Applies to specific metrics.
				for _, metricName := range attrs.ApplyToMetrics {
					if s.attributesPerMetric[metricName] == nil {
						s.attributesPerMetric[metricName] = make(map[string]struct{})
					}
					for oldName, newName := range attrs.AttributeMap {
						s.attributesPerMetric[metricName][oldName] = struct{}{}
						s.attributesPerMetric[metricName][newName] = struct{}{}
					}
				}
			}
		}
	}
}

// getAttributesForMetric returns all attribute names that apply to a given metric,
// including global attributes from the "all" section.
func (s *otelSchema) getAttributesForMetric(metricName string) []string {
	seen := make(map[string]struct{})

	// Add global attributes.
	for attr := range s.attributesPerMetric[""] {
		seen[attr] = struct{}{}
	}

	// Add metric-specific attributes.
	for attr := range s.attributesPerMetric[metricName] {
		seen[attr] = struct{}{}
	}

	result := make([]string, 0, len(seen))
	for attr := range seen {
		result = append(result, attr)
	}
	return result
}

// cache is a generic TTL cache for fetched resources.
type cache[T any] struct {
	mu      sync.RWMutex
	entries map[string]cacheEntry[T]
}

type cacheEntry[T any] struct {
	value     T
	fetchTime time.Time
}

func newCache[T any]() *cache[T] {
	return &cache[T]{entries: make(map[string]cacheEntry[T])}
}

func (c *cache[T]) get(url string) (T, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[url]
	if !ok || time.Since(entry.fetchTime) > cacheTTL {
		var zero T
		return zero, false
	}
	return entry.value, true
}

func (c *cache[T]) set(url string, value T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[url] = cacheEntry[T]{value: value, fetchTime: time.Now()}
}

func fetchOTelSchema(url string) (otelSchema, error) {
	var s otelSchema
	if err := fetchAndUnmarshal(url, &s); err != nil {
		return otelSchema{}, fmt.Errorf("fetch OTel schema %q: %w", url, err)
	}
	if s.FileFormat != "1.1.0" && s.FileFormat != "1.0.0" {
		return otelSchema{}, fmt.Errorf("unsupported OTel schema file format %q (expected 1.0.0 or 1.1.0)", s.FileFormat)
	}
	s.init()
	return s, nil
}

func fetchSemconv(url string) (semconv, error) {
	var s semconv
	if err := fetchAndUnmarshal(url, &s); err != nil {
		return semconv{}, fmt.Errorf("fetch semconv %q: %w", url, err)
	}
	s.init()
	return s, nil
}

// otelLabelMapping maps translated Prometheus names back to original OTLP names.
type otelLabelMapping struct {
	translatedToOriginal map[string]string // e.g., "http_method" -> "http.method"
	originalMetricName   string
}

// buildLabelMapping creates a mapping from translated label names back to original OTLP names.
func buildLabelMapping(metricName string, attributes []string) *otelLabelMapping {
	mapping := &otelLabelMapping{
		translatedToOriginal: make(map[string]string, len(attributes)*2),
		originalMetricName:   metricName,
	}
	for _, attr := range attributes {
		for _, strategy := range otelStrategies {
			labelNamer := otlptranslator.LabelNamer{
				UTF8Allowed:                 !strategy.ShouldEscape(),
				UnderscoreLabelSanitization: strategy.ShouldEscape(),
			}
			translatedName, err := labelNamer.Build(attr)
			if err != nil {
				continue
			}
			mapping.translatedToOriginal[translatedName] = attr
		}
	}

	return mapping
}

// transformOTelSchemaLabels transforms series labels back to original OTLP names
// using the label mapping from the schema.
func transformOTelSchemaLabels(originalLabels labels.Labels, mapping *otelLabelMapping) labels.Labels {
	builder := labels.NewScratchBuilder(originalLabels.Len())
	originalLabels.Range(func(l labels.Label) {
		switch l.Name {
		case semconvURLLabel, schemaURLLabel:
			// Skip.
		case model.MetricNameLabel:
			builder.Add(l.Name, mapping.originalMetricName)
		default:
			if originalName, ok := mapping.translatedToOriginal[l.Name]; ok {
				builder.Add(originalName, l.Value)
			} else {
				builder.Add(l.Name, l.Value)
			}
		}
	})
	return builder.Labels()
}

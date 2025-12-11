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
	"testing"

	"github.com/prometheus/common/model"
	"github.com/prometheus/otlptranslator"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
)

func TestOTelSchemaParser(t *testing.T) {
	t.Run("collects attributes from versions", func(t *testing.T) {
		schema := &otelSchema{
			FileFormat: "1.1.0",
			SchemaURL:  "https://example.com/schemas/1.0.0",
			Versions: map[string]otelSchemaVersion{
				"1.0.0": {
					Metrics: &otelSchemaSection{
						Changes: []otelSchemaChange{
							{
								RenameAttributes: &otelRenameAttributes{
									AttributeMap: map[string]string{
										"http.method":      "http.request.method",
										"http.status_code": "http.response.status_code",
									},
									ApplyToMetrics: []string{"http.server.duration"},
								},
							},
						},
					},
				},
			},
		}
		schema.init()

		attrs := schema.getAttributesForMetric("http.server.duration")
		require.Contains(t, attrs, "http.method")
		require.Contains(t, attrs, "http.request.method")
		require.Contains(t, attrs, "http.status_code")
		require.Contains(t, attrs, "http.response.status_code")
	})

	t.Run("parses groups section", func(t *testing.T) {
		sc := &semconv{
			Groups: []semconvGroup{
				{ID: "metric.http.server.duration", Type: "metric", MetricName: "http.server.duration", Instrument: "histogram", Unit: "s"},
				{ID: "metric.http.server.requests", Type: "metric", MetricName: "http.server.requests", Instrument: "counter", Unit: "1"},
				{ID: "attribute.http.method", Type: "attribute"}, // Should be ignored
			},
		}
		sc.init()

		meta, ok := sc.getMetricMetadata("http.server.duration")
		require.True(t, ok)
		require.Equal(t, "s", meta.Unit)
		require.Equal(t, model.MetricTypeHistogram, meta.Type)

		meta, ok = sc.getMetricMetadata("http.server.requests")
		require.True(t, ok)
		require.Equal(t, "1", meta.Unit)
		require.Equal(t, model.MetricTypeCounter, meta.Type)

		// Unknown metric returns not found.
		_, ok = sc.getMetricMetadata("unknown.metric")
		require.False(t, ok)
	})

	t.Run("collects global attributes from all section", func(t *testing.T) {
		schema := &otelSchema{
			FileFormat: "1.1.0",
			SchemaURL:  "https://example.com/schemas/1.0.0",
			Versions: map[string]otelSchemaVersion{
				"1.0.0": {
					All: &otelSchemaSection{
						Changes: []otelSchemaChange{
							{
								RenameAttributes: &otelRenameAttributes{
									AttributeMap: map[string]string{
										"global.old": "global.new",
									},
								},
							},
						},
					},
					Metrics: &otelSchemaSection{
						Changes: []otelSchemaChange{
							{
								RenameAttributes: &otelRenameAttributes{
									AttributeMap: map[string]string{
										"metric.old": "metric.new",
									},
									ApplyToMetrics: []string{"my.metric"},
								},
							},
						},
					},
				},
			},
		}
		schema.init()

		// Global attributes apply to all metrics.
		attrs := schema.getAttributesForMetric("my.metric")
		require.Contains(t, attrs, "global.old")
		require.Contains(t, attrs, "global.new")
		require.Contains(t, attrs, "metric.old")
		require.Contains(t, attrs, "metric.new")

		// Global attributes also apply to metrics not in versions.
		attrs = schema.getAttributesForMetric("other.metric")
		require.Contains(t, attrs, "global.old")
		require.Contains(t, attrs, "global.new")
	})
}

func TestTransformOTelSchemaLabels(t *testing.T) {
	t.Run("transforms metric and label names", func(t *testing.T) {
		lbls := labels.FromStrings(
			model.MetricNameLabel, "http_server_duration_seconds",
			"http_method", "GET",
			"http_status_code", "200",
			"instance", "localhost:8080",
		)

		mapping := &otelLabelMapping{
			originalMetricName: "http.server.duration",
			translatedToOriginal: map[string]string{
				"http_method":      "http.method",
				"http_status_code": "http.status_code",
			},
		}

		result := transformOTelSchemaLabels(lbls, mapping)

		require.Equal(t, "http.server.duration", result.Get(model.MetricNameLabel))
		require.Equal(t, "GET", result.Get("http.method"))
		require.Equal(t, "200", result.Get("http.status_code"))
		require.Equal(t, "localhost:8080", result.Get("instance"))
		require.Empty(t, result.Get("http_method"))
		require.Empty(t, result.Get("http_status_code"))
	})

	t.Run("removes __schema_url__", func(t *testing.T) {
		lbls := labels.FromStrings(
			model.MetricNameLabel, "http_server_duration_seconds",
			schemaURLLabel, "https://example.com/otel.yaml",
			"http_method", "GET",
		)

		mapping := &otelLabelMapping{
			originalMetricName:   "http.server.duration",
			translatedToOriginal: map[string]string{},
		}

		result := transformOTelSchemaLabels(lbls, mapping)

		require.Empty(t, result.Get(schemaURLLabel))
		require.Equal(t, "http.server.duration", result.Get(model.MetricNameLabel))
	})
}

func TestInstrumentToMetricType(t *testing.T) {
	tests := []struct {
		input    string
		expected model.MetricType
	}{
		{"counter", model.MetricTypeCounter},
		{"gauge", model.MetricTypeGauge},
		{"updowncounter", model.MetricTypeGauge},
		{"histogram", model.MetricTypeHistogram},
		{"unknown", model.MetricTypeUnknown},
		{"", model.MetricTypeUnknown},
		{"invalid", model.MetricTypeUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := instrumentToMetricType(tc.input)
			require.Equal(t, tc.expected, result)
		})
	}
}

func TestBuildLabelMapping(t *testing.T) {
	attributes := []string{
		"http.method",
		"http.status_code",
		"url.path",
	}

	mapping := buildLabelMapping("http.server.duration", attributes)

	require.Equal(t, "http.server.duration", mapping.originalMetricName)
	require.Equal(t, "http.method", mapping.translatedToOriginal["http_method"])
	require.Equal(t, "http.method", mapping.translatedToOriginal["http.method"])
	require.Equal(t, "http.status_code", mapping.translatedToOriginal["http_status_code"])
	require.Equal(t, "url.path", mapping.translatedToOriginal["url_path"])
	require.Equal(t, "url.path", mapping.translatedToOriginal["url.path"])
}

func TestOTelSchemaLabelMapping(t *testing.T) {
	attributes := []string{
		"http.method",
		"http.status_code",
		"url.path",
	}

	mapping := &otelLabelMapping{
		translatedToOriginal: make(map[string]string),
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

	require.Equal(t, "http.method", mapping.translatedToOriginal["http_method"])
	require.Equal(t, "http.method", mapping.translatedToOriginal["http.method"])
	require.Equal(t, "http.status_code", mapping.translatedToOriginal["http_status_code"])
	require.Equal(t, "url.path", mapping.translatedToOriginal["url_path"])
	require.Equal(t, "url.path", mapping.translatedToOriginal["url.path"])
}

func TestFindOTelSchemaMatcherVariants(t *testing.T) {
	e := newSchemaEngine()

	t.Run("generates variants with semconv and schema", func(t *testing.T) {
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, "http.server.duration"),
			labels.MustNewMatcher(labels.MatchEqual, semconvURLLabel, "./testdata/otel.yaml"),
			labels.MustNewMatcher(labels.MatchEqual, schemaURLLabel, "./testdata/otel.yaml"),
			labels.MustNewMatcher(labels.MatchEqual, "http.method", "GET"),
		}

		variants, qCtx, err := e.FindMatcherVariants("./testdata/otel.yaml", "./testdata/otel.yaml", matchers)
		require.NoError(t, err)
		require.NotNil(t, qCtx.otelLabelMapping)
		require.Equal(t, "http.server.duration", qCtx.otelLabelMapping.originalMetricName)
		require.NotEmpty(t, variants)

		names := make([]string, 0, len(variants))
		for _, v := range variants {
			for _, m := range v {
				if m.Name == model.MetricNameLabel {
					names = append(names, m.Value)
				}
			}
		}

		require.Contains(t, names, "http_server_duration")
	})

	t.Run("transforms results back to original names", func(t *testing.T) {
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, "http.server.duration"),
			labels.MustNewMatcher(labels.MatchEqual, semconvURLLabel, "./testdata/otel.yaml"),
			labels.MustNewMatcher(labels.MatchEqual, schemaURLLabel, "./testdata/otel.yaml"),
		}

		_, qCtx, err := e.FindMatcherVariants("./testdata/otel.yaml", "./testdata/otel.yaml", matchers)
		require.NoError(t, err)

		storedLabels := labels.FromStrings(
			model.MetricNameLabel, "http_server_duration_seconds",
			"http_method", "GET",
			"http_status_code", "200",
			"url_path", "/api/users",
			"instance", "localhost:8080",
		)

		result, err := e.TransformSeries(qCtx, storedLabels)
		require.NoError(t, err)

		require.Equal(t, "http.server.duration", result.Get(model.MetricNameLabel))
		require.Equal(t, "GET", result.Get("http.method"))
		require.Equal(t, "200", result.Get("http.status_code"))
		require.Equal(t, "/api/users", result.Get("url.path"))
		require.Equal(t, "localhost:8080", result.Get("instance"))
	})

	t.Run("errors on metric not in groups", func(t *testing.T) {
		engine := newSchemaEngine()

		// unknown.metric doesn't exist in groups.
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, "unknown.metric"),
			labels.MustNewMatcher(labels.MatchEqual, semconvURLLabel, "./testdata/otel.yaml"),
		}

		_, _, err := engine.FindMatcherVariants("./testdata/otel.yaml", "", matchers)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not found in groups")
	})

	t.Run("generates histogram variants", func(t *testing.T) {
		engine := newSchemaEngine()

		// http.server.duration has unit=s and type=histogram in the semconv.
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, "http.server.duration"),
			labels.MustNewMatcher(labels.MatchEqual, semconvURLLabel, "./testdata/otel.yaml"),
		}

		variants, _, err := engine.FindMatcherVariants("./testdata/otel.yaml", "", matchers)
		require.NoError(t, err)
		require.Len(t, variants, 4) // Original + 3 strategies

		names := make([]string, 0, len(variants))
		for _, v := range variants {
			for _, m := range v {
				if m.Name == model.MetricNameLabel {
					names = append(names, m.Value)
				}
			}
		}

		require.Contains(t, names, "http.server.duration")         // Original
		require.Contains(t, names, "http_server_duration_seconds") // UnderscoreEscapingWithSuffixes
		require.Contains(t, names, "http_server_duration")         // UnderscoreEscapingWithoutSuffixes
		require.Contains(t, names, "http.server.duration_seconds") // NoUTF8EscapingWithSuffixes
	})

	t.Run("generates counter variants", func(t *testing.T) {
		engine := newSchemaEngine()

		// http.server.request.count has unit=1 and type=counter in the semconv.
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, "http.server.request.count"),
			labels.MustNewMatcher(labels.MatchEqual, semconvURLLabel, "./testdata/otel.yaml"),
		}

		variants, _, err := engine.FindMatcherVariants("./testdata/otel.yaml", "", matchers)
		require.NoError(t, err)
		require.Len(t, variants, 4) // Original + 3 strategies

		names := make([]string, 0, len(variants))
		for _, v := range variants {
			for _, m := range v {
				if m.Name == model.MetricNameLabel {
					names = append(names, m.Value)
				}
			}
		}

		require.Contains(t, names, "http.server.request.count")       // Original
		require.Contains(t, names, "http_server_request_count_total") // UnderscoreEscapingWithSuffixes
		require.Contains(t, names, "http_server_request_count")       // UnderscoreEscapingWithoutSuffixes
		require.Contains(t, names, "http.server.request.count_total") // NoUTF8EscapingWithSuffixes
	})
}

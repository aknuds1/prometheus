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

func TestPromTypeToOTelType(t *testing.T) {
	// OTel metric type constants:
	// Unknown=0, NonMonotonicCounter=1, MonotonicCounter=2, Gauge=3,
	// Histogram=4, ExponentialHistogram=5, Summary=6
	tests := []struct {
		promType model.MetricType
		expected int // Using int to avoid import dependency
	}{
		{model.MetricTypeCounter, 2},   // MetricTypeMonotonicCounter
		{model.MetricTypeGauge, 3},     // MetricTypeGauge
		{model.MetricTypeHistogram, 4}, // MetricTypeHistogram
		{model.MetricTypeSummary, 6},   // MetricTypeSummary
		{model.MetricTypeUnknown, 0},   // MetricTypeUnknown
	}

	for _, tc := range tests {
		t.Run(string(tc.promType), func(t *testing.T) {
			result := promTypeToOTelType(tc.promType)
			require.Equal(t, tc.expected, int(result))
		})
	}
}

func TestGenerateOTLPVariants(t *testing.T) {
	t.Run("known type generates 4 variants (original + 3 translated)", func(t *testing.T) {
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, "http.server.duration"),
		}
		metric := otlptranslator.Metric{
			Name: "http.server.duration",
			Unit: "s",
			Type: otlptranslator.MetricTypeHistogram,
		}

		variants, err := generateOTLPVariants(matchers, metric)
		require.NoError(t, err)
		require.Len(t, variants, 4) // Original + 3 strategies

		names := collectMetricNames(variants)
		require.Contains(t, names, "http.server.duration")         // Original
		require.Contains(t, names, "http_server_duration_seconds") // UnderscoreEscapingWithSuffixes
		require.Contains(t, names, "http_server_duration")         // UnderscoreEscapingWithoutSuffixes
		require.Contains(t, names, "http.server.duration_seconds") // NoUTF8EscapingWithSuffixes
	})

	t.Run("gauge type generates 4 variants", func(t *testing.T) {
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, "process.cpu.utilization"),
		}
		metric := otlptranslator.Metric{
			Name: "process.cpu.utilization",
			Type: otlptranslator.MetricTypeGauge,
			Unit: "1",
		}

		variants, err := generateOTLPVariants(matchers, metric)
		require.NoError(t, err)
		require.Len(t, variants, 4)

		names := collectMetricNames(variants)
		require.Contains(t, names, "process.cpu.utilization")       // Original
		require.Contains(t, names, "process_cpu_utilization_ratio") // UnderscoreEscapingWithSuffixes
		require.Contains(t, names, "process_cpu_utilization")       // UnderscoreEscapingWithoutSuffixes
		require.Contains(t, names, "process.cpu.utilization_ratio") // NoUTF8EscapingWithSuffixes
	})

	t.Run("counter type with known unit generates _total suffix", func(t *testing.T) {
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, "http.server.requests"),
		}
		metric := otlptranslator.Metric{
			Name: "http.server.requests",
			Type: otlptranslator.MetricTypeMonotonicCounter,
			Unit: "1", // Known unit
		}

		variants, err := generateOTLPVariants(matchers, metric)
		require.NoError(t, err)
		require.Len(t, variants, 4)

		names := collectMetricNames(variants)
		require.Contains(t, names, "http.server.requests")       // Original
		require.Contains(t, names, "http_server_requests_total") // UnderscoreEscapingWithSuffixes
		require.Contains(t, names, "http_server_requests")       // UnderscoreEscapingWithoutSuffixes
		require.Contains(t, names, "http.server.requests_total") // NoUTF8EscapingWithSuffixes
	})

	t.Run("counter type with bytes unit generates _bytes_total suffix", func(t *testing.T) {
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, "http.server.request.size"),
		}
		metric := otlptranslator.Metric{
			Name: "http.server.request.size",
			Type: otlptranslator.MetricTypeMonotonicCounter,
			Unit: "By",
		}

		variants, err := generateOTLPVariants(matchers, metric)
		require.NoError(t, err)
		require.Len(t, variants, 4)

		names := collectMetricNames(variants)
		require.Contains(t, names, "http.server.request.size")             // Original
		require.Contains(t, names, "http_server_request_size_bytes_total") // UnderscoreEscapingWithSuffixes
		require.Contains(t, names, "http_server_request_size")             // UnderscoreEscapingWithoutSuffixes
		require.Contains(t, names, "http.server.request.size_bytes_total") // NoUTF8EscapingWithSuffixes
	})
}

func collectMetricNames(variants [][]*labels.Matcher) []string {
	names := make([]string, 0, len(variants))
	for _, v := range variants {
		for _, m := range v {
			if m.Name == model.MetricNameLabel {
				names = append(names, m.Value)
			}
		}
	}
	return names
}

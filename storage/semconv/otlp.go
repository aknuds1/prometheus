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

	"github.com/prometheus/common/model"
	"github.com/prometheus/otlptranslator"

	"github.com/prometheus/prometheus/model/labels"
)

// otelStrategies defines the translation strategies to generate variants for.
// These are the strategies that OTLP data may have been written with.
var otelStrategies = []otlptranslator.TranslationStrategyOption{
	otlptranslator.UnderscoreEscapingWithSuffixes,
	otlptranslator.UnderscoreEscapingWithoutSuffixes,
	otlptranslator.NoUTF8EscapingWithSuffixes,
}

// generateOTLPVariants generates matcher variants for all OTLP translation strategies.
func generateOTLPVariants(matchers []*labels.Matcher, metric otlptranslator.Metric) ([][]*labels.Matcher, error) {
	variants := [][]*labels.Matcher{matchers}
	for _, strategy := range otelStrategies {
		namer := otlptranslator.NewMetricNamer("", strategy)
		translatedName, err := namer.Build(otlptranslator.Metric{
			Name: metric.Name,
			Unit: metric.Unit,
			Type: metric.Type,
		})
		if err != nil {
			return nil, fmt.Errorf("translate name %q, unit %q, type %q: %w", metric.Name, metric.Unit, metric.Type, err)
		}

		labelNamer := otlptranslator.LabelNamer{
			UTF8Allowed:                 !strategy.ShouldEscape(),
			UnderscoreLabelSanitization: strategy.ShouldEscape(),
		}
		variant := make([]*labels.Matcher, 0, len(matchers))
		for _, m := range matchers {
			switch m.Name {
			case model.MetricNameLabel:
				variant = append(variant, labels.MustNewMatcher(m.Type, m.Name, translatedName))
			case semconvURLLabel, schemaURLLabel:
				// Skip.
			default:
				newName, err := labelNamer.Build(m.Name)
				if err != nil {
					return nil, fmt.Errorf("transform label name %q: %w", m.Name, err)
				}

				variant = append(variant, labels.MustNewMatcher(m.Type, newName, m.Value))
			}
		}
		variants = append(variants, variant)
	}

	return variants, nil
}

// promTypeToOTelType converts a Prometheus metric type to an OTel metric type.
func promTypeToOTelType(t model.MetricType) otlptranslator.MetricType {
	switch t {
	case model.MetricTypeCounter:
		return otlptranslator.MetricTypeMonotonicCounter
	case model.MetricTypeGauge:
		return otlptranslator.MetricTypeGauge
	case model.MetricTypeHistogram:
		return otlptranslator.MetricTypeHistogram
	case model.MetricTypeSummary:
		return otlptranslator.MetricTypeSummary
	default:
		return otlptranslator.MetricTypeUnknown
	}
}

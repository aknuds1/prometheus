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

package main

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

func TestMetricSelectors(t *testing.T) {
	p := parser.NewParser(parser.Options{})
	for _, name := range []string{
		"http_requests_total",
		"job:requests_total",
		"test.counter",
		"9requests",
		"métrique",
		"metric with spaces",
		"metric\"quote\\slash",
		"on",
		"bool",
		"NaN",
		"Inf",
	} {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				query string
				want  map[string]string
			}{
				{
					name:  "selector",
					query: sel(name),
					want:  map[string]string{labels.MetricName: name},
				},
				{
					name:  "with matcher",
					query: selWith(name, "job=\"demo\""),
					want:  map[string]string{labels.MetricName: name, "job": "demo"},
				},
				{
					name:  "without matchers",
					query: selWith(name),
					want:  map[string]string{labels.MetricName: name},
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					expr, err := p.ParseExpr(tc.query)
					require.NoError(t, err)
					require.IsType(t, &parser.VectorSelector{}, expr)
					requireMatchers(t, expr.(*parser.VectorSelector).LabelMatchers, tc.want)
				})
			}
		})
	}
}

func TestLabelMatchers(t *testing.T) {
	p := parser.NewParser(parser.Options{})
	for _, name := range []string{
		"tenant",
		"service.name",
		"foo:bar",
		"service name",
		"ténant",
		"label\"quote\\slash",
		"on",
		"bool",
		"without",
		"NaN",
		"Inf",
	} {
		t.Run(name, func(t *testing.T) {
			expr, err := p.ParseExpr(eraSeries("test.counter", name, strconv.Quote("200")))
			require.NoError(t, err)
			require.IsType(t, &parser.VectorSelector{}, expr)
			requireMatchers(t, expr.(*parser.VectorSelector).LabelMatchers, map[string]string{
				labels.MetricName:           "test.counter",
				name:                        "acme",
				"http.response.status_code": "200",
			})
		})
	}
}

func TestSchemaQuery(t *testing.T) {
	metric, version, schemaURL := *newMetric, *newVer, *schema
	t.Cleanup(func() {
		*newMetric, *newVer, *schema = metric, version, schemaURL
	})

	p := parser.NewParser(parser.Options{})
	for _, tc := range []struct {
		name    string
		metric  string
		version string
		schema  string
	}{
		{name: "defaults", metric: "test", version: "1.1.0", schema: "registry/registry.yaml"},
		{name: "escaped version", metric: "test.counter", version: "1.1.0\"\\\n", schema: "registry/registry.yaml"},
		{name: "escaped schema", metric: "test.counter", version: "1.1.0", schema: "registry/schema\"\\\n.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*newMetric, *newVer, *schema = tc.metric, tc.version, tc.schema
			expr, err := p.ParseExpr(schemaQuery())
			require.NoError(t, err)
			require.IsType(t, &parser.VectorSelector{}, expr)
			requireMatchers(t, expr.(*parser.VectorSelector).LabelMatchers, map[string]string{
				labels.MetricName: tc.metric,
				"__semconv_url__": "registry/" + tc.version,
				"__schema_url__":  tc.schema,
			})
		})
	}
}

func TestAttributeQuery(t *testing.T) {
	metric, attribute, version, schemaURL := *newMetric, *newAttr, *newVer, *schema
	t.Cleanup(func() {
		*newMetric, *newAttr, *newVer, *schema = metric, attribute, version, schemaURL
	})
	*newMetric, *newVer, *schema = "test.counter", "1.1.0", "registry/registry.yaml"

	p := parser.NewParser(parser.Options{})
	for _, name := range []string{
		"tenant",
		"service.name",
		"foo:bar",
		"service name",
		"ténant",
		"label\"quote\\slash",
		"on",
		"bool",
		"without",
		"NaN",
		"Inf",
	} {
		t.Run(name, func(t *testing.T) {
			*newAttr = name
			expr, err := p.ParseExpr(attributeQuery())
			require.NoError(t, err)
			require.IsType(t, &parser.AggregateExpr{}, expr)
			aggregate := expr.(*parser.AggregateExpr)
			require.Equal(t, parser.ItemType(parser.SUM), aggregate.Op)
			require.Equal(t, []string{name}, aggregate.Grouping)
			require.False(t, aggregate.Without)
			selectors := parser.ExtractSelectors(expr)
			require.Len(t, selectors, 1)
			requireMatchers(t, selectors[0], map[string]string{
				labels.MetricName: "test.counter",
				"__semconv_url__": "registry/1.1.0",
				"__schema_url__":  "registry/registry.yaml",
			})
		})
	}
}

func requireMatchers(t *testing.T, matchers []*labels.Matcher, want map[string]string) {
	t.Helper()
	require.Len(t, matchers, len(want))
	got := make(map[string]string, len(matchers))
	for _, matcher := range matchers {
		require.Equal(t, labels.MatchEqual, matcher.Type)
		got[matcher.Name] = matcher.Value
	}
	require.Equal(t, want, got)
}

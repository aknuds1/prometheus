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

package semconv

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
)

func TestFindMatcherVariants_RequiresSemconvURL(t *testing.T) {
	e := newSchemaEngine(embeddedRegistry)

	matchers := []*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "http.server.duration"),
	}

	_, _, err := e.findMatcherVariants("", "", matchers)
	require.Error(t, err)
	require.Contains(t, err.Error(), "semconvURL is required")
}

func TestNormalizeMetricMatchers(t *testing.T) {
	t.Run("normalizes compatible constraints around an equality", func(t *testing.T) {
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, `metric\.(current|old)`),
			labels.MustNewMatcher(labels.MatchEqual, "attribute", "value"),
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "metric.current"),
			labels.MustNewMatcher(labels.MatchNotEqual, labels.MetricName, "metric.old"),
		}

		name, got, satisfiable, err := normalizeMetricMatchers(matchers)
		require.NoError(t, err)
		require.True(t, satisfiable)
		require.Equal(t, "metric.current", name)
		require.Len(t, got, 2)
		require.Equal(t, labels.MatchEqual, got[0].Type)
		require.Equal(t, labels.MetricName, got[0].Name)
		require.Equal(t, "metric.current", got[0].Value)
		require.Same(t, matchers[1], got[1])
	})

	t.Run("keeps contradictory constraints for direct evaluation", func(t *testing.T) {
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "metric.current"),
			labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, "metric.old"),
		}

		name, got, satisfiable, err := normalizeMetricMatchers(matchers)
		require.NoError(t, err)
		require.False(t, satisfiable)
		require.Equal(t, "metric.current", name)
		require.Same(t, matchers[0], got[0])
		require.Same(t, matchers[1], got[1])
	})

	t.Run("finds a non-empty equality independent of matcher order", func(t *testing.T) {
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, ""),
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "metric.current"),
		}

		name, got, satisfiable, err := normalizeMetricMatchers(matchers)
		require.NoError(t, err)
		require.False(t, satisfiable)
		require.Equal(t, "metric.current", name)
		require.Same(t, matchers[0], got[0])
		require.Same(t, matchers[1], got[1])
	})

	t.Run("requires a non-empty exact anchor", func(t *testing.T) {
		for _, matcher := range []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, "metric.*"),
			labels.MustNewMatcher(labels.MatchNotEqual, labels.MetricName, "metric.old"),
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, ""),
		} {
			_, _, _, err := normalizeMetricMatchers([]*labels.Matcher{matcher})
			require.ErrorIs(t, err, errMetricNameAnchor)
			require.ErrorContains(t, err, "non-empty equality matcher")
		}
	})
}

func TestGenerateMatcherVariants(t *testing.T) {
	t.Run("applies per-version renames", func(t *testing.T) {
		schema := &otelSchema{
			versionRenames: []versionRenames{
				{
					version:    "1.0.0",
					metrics:    map[string]string{"metric.v2": "metric.v1", "metric.v1": "metric.v2"},
					attributes: map[string]string{},
				},
			},
		}
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "metric.v2"),
		}

		result, err := generateMatcherVariants("1.0.0", schema, matchers)
		require.NoError(t, err)

		// Should have original + 1 version variant.
		require.Len(t, result, 2)
		metricNames := extractMetricNames(result)
		require.Contains(t, metricNames, "metric.v2")
		require.Contains(t, metricNames, "metric.v1")
	})

	t.Run("applies metric and attribute renames together", func(t *testing.T) {
		schema := &otelSchema{
			versionRenames: []versionRenames{
				{
					version:    "1.0.0",
					metrics:    map[string]string{"metric.new": "metric.old", "metric.old": "metric.new"},
					attributes: map[string]string{"attr.new": "attr.old", "attr.old": "attr.new"},
				},
			},
		}
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "metric.new"),
			labels.MustNewMatcher(labels.MatchEqual, "attr.new", "value"),
		}

		result, err := generateMatcherVariants("1.0.0", schema, matchers)
		require.NoError(t, err)

		// Should have original + 1 version variant (both metric AND attr renamed together).
		require.Len(t, result, 2)

		// Verify the combinations.
		found := make(map[string]bool)
		for _, m := range result {
			metricName := ""
			attrName := ""
			for _, matcher := range m {
				if matcher.Name == labels.MetricName {
					metricName = matcher.Value
				} else {
					attrName = matcher.Name
				}
			}
			found[metricName+"/"+attrName] = true
		}
		// Original: metric.new/attr.new
		// Version rename: metric.old/attr.old (BOTH renamed together)
		require.True(t, found["metric.new/attr.new"])
		require.True(t, found["metric.old/attr.old"])
		// Should NOT have mix-and-match combinations.
		require.False(t, found["metric.new/attr.old"])
		require.False(t, found["metric.old/attr.new"])
	})

	t.Run("resolves transitive rename chains", func(t *testing.T) {
		// Versions are sorted: 1.0.0 (v1↔v2), then 1.1.0 (v2↔v3).
		schema := &otelSchema{
			versionRenames: []versionRenames{
				{
					version:    "1.0.0",
					metrics:    map[string]string{"metric.v2": "metric.v1", "metric.v1": "metric.v2"},
					attributes: map[string]string{},
				},
				{
					version:    "1.1.0",
					metrics:    map[string]string{"metric.v3": "metric.v2", "metric.v2": "metric.v3"},
					attributes: map[string]string{},
				},
			},
		}
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "metric.v3"),
		}

		result, err := generateMatcherVariants("1.1.0", schema, matchers)
		require.NoError(t, err)

		// Anchored at 1.1.0, backward walk resolves: v3 → v2 (via 1.1.0) → v1 (via 1.0.0).
		require.Len(t, result, 3)
		metricNames := extractMetricNames(result)
		require.Contains(t, metricNames, "metric.v3")
		require.Contains(t, metricNames, "metric.v2")
		require.Contains(t, metricNames, "metric.v1")
	})

	t.Run("resolves transitive chains with paired attributes", func(t *testing.T) {
		schema := &otelSchema{
			versionRenames: []versionRenames{
				{
					version:    "1.0.0",
					metrics:    map[string]string{"metric.v2": "metric.v1", "metric.v1": "metric.v2"},
					attributes: map[string]string{"attr.v2": "attr.v1", "attr.v1": "attr.v2"},
				},
				{
					version:    "1.1.0",
					metrics:    map[string]string{"metric.v3": "metric.v2", "metric.v2": "metric.v3"},
					attributes: map[string]string{"attr.v3": "attr.v2", "attr.v2": "attr.v3"},
				},
			},
		}
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "metric.v3"),
			labels.MustNewMatcher(labels.MatchEqual, "attr.v3", "value"),
		}

		result, err := generateMatcherVariants("1.1.0", schema, matchers)
		require.NoError(t, err)

		// Anchored at 1.1.0, should have 3 variants with metric+attr paired correctly.
		require.Len(t, result, 3)

		found := make(map[string]bool)
		for _, m := range result {
			metricName := ""
			attrName := ""
			for _, matcher := range m {
				if matcher.Name == labels.MetricName {
					metricName = matcher.Value
				} else {
					attrName = matcher.Name
				}
			}
			found[metricName+"/"+attrName] = true
		}

		// Each version's renames are applied together via backward walk.
		require.True(t, found["metric.v3/attr.v3"]) // Original
		require.True(t, found["metric.v2/attr.v2"]) // v1.1.0 renames applied
		require.True(t, found["metric.v1/attr.v1"]) // v1.0.0 renames applied to v2

		// Should NOT have cross-version combinations.
		require.False(t, found["metric.v3/attr.v2"])
		require.False(t, found["metric.v3/attr.v1"])
		require.False(t, found["metric.v2/attr.v3"])
		require.False(t, found["metric.v2/attr.v1"])
		require.False(t, found["metric.v1/attr.v3"])
		require.False(t, found["metric.v1/attr.v2"])
	})
	t.Run("no renames returns original only", func(t *testing.T) {
		schema := &otelSchema{}
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "my.metric"),
		}

		result, err := generateMatcherVariants("1.0.0", schema, matchers)
		require.NoError(t, err)

		require.Len(t, result, 1)
		require.Equal(t, matchers, result[0])
	})
}

func extractMetricNames(matcherSets [][]*labels.Matcher) []string {
	names := make([]string, 0, len(matcherSets))
	for _, set := range matcherSets {
		for _, m := range set {
			if m.Name == labels.MetricName {
				names = append(names, m.Value)
			}
		}
	}
	return names
}

func TestFindVersionAnchorIndex(t *testing.T) {
	versions := []versionRenames{
		{version: "1.0.0"},
		{version: "1.1.0"},
		{version: "1.2.0"},
	}

	tests := []struct {
		name          string
		targetVersion string
		expectedIndex int
	}{
		{"exact match first", "1.0.0", 0},
		{"exact match middle", "1.1.0", 1},
		{"exact match last", "1.2.0", 2},
		{"between versions", "1.0.5", 0},
		{"before all versions", "0.9.0", 0},
		{"after all versions", "2.0.0", 2},
		{"with v prefix", "v1.1.0", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx := findVersionAnchorIndex(versions, tc.targetVersion)
			require.Equal(t, tc.expectedIndex, idx)
		})
	}
	t.Run("empty versions", func(t *testing.T) {
		idx := findVersionAnchorIndex([]versionRenames{}, "1.0.0")
		require.Equal(t, 0, idx)
	})
}

func TestGenerateMatcherVariants_AnchoredTraversal(t *testing.T) {
	t.Run("anchored at middle version walks both directions", func(t *testing.T) {
		// Schema: [1.0.0: v1<->v2], [1.1.0: v2<->v3], [1.2.0: v3<->v4]
		schema := &otelSchema{
			versionRenames: []versionRenames{
				{version: "1.0.0", metrics: map[string]string{"metric.v2": "metric.v1", "metric.v1": "metric.v2"}},
				{version: "1.1.0", metrics: map[string]string{"metric.v3": "metric.v2", "metric.v2": "metric.v3"}},
				{version: "1.2.0", metrics: map[string]string{"metric.v4": "metric.v3", "metric.v3": "metric.v4"}},
			},
		}
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "metric.v3"),
		}

		// Anchored at 1.1.0: backward walks [1.0.0, 1.1.0], forward walks [1.1.0, 1.2.0].
		result, err := generateMatcherVariants("1.1.0", schema, matchers)
		require.NoError(t, err)

		require.Len(t, result, 4) // v1, v2, v3, v4
		names := extractMetricNames(result)
		require.Contains(t, names, "metric.v1")
		require.Contains(t, names, "metric.v2")
		require.Contains(t, names, "metric.v3")
		require.Contains(t, names, "metric.v4")
	})

	t.Run("anchored at first version only walks forward for newer", func(t *testing.T) {
		schema := &otelSchema{
			versionRenames: []versionRenames{
				{version: "1.0.0", metrics: map[string]string{"metric.v2": "metric.v1", "metric.v1": "metric.v2"}},
				{version: "1.1.0", metrics: map[string]string{"metric.v3": "metric.v2", "metric.v2": "metric.v3"}},
			},
		}
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "metric.v2"),
		}

		// Anchored at 1.0.0: backward walks [1.0.0], forward walks [1.0.0, 1.1.0].
		result, err := generateMatcherVariants("1.0.0", schema, matchers)
		require.NoError(t, err)

		names := extractMetricNames(result)
		require.Contains(t, names, "metric.v1") // backward from anchor
		require.Contains(t, names, "metric.v2") // original
		require.Contains(t, names, "metric.v3") // forward from anchor
	})

	t.Run("handles version with v prefix", func(t *testing.T) {
		schema := &otelSchema{
			versionRenames: []versionRenames{
				{version: "1.0.0", metrics: map[string]string{"metric.old": "metric.new", "metric.new": "metric.old"}},
			},
		}
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "metric.new"),
		}

		result, err := generateMatcherVariants("v1.0.0", schema, matchers)
		require.NoError(t, err)

		require.Len(t, result, 2)
		names := extractMetricNames(result)
		require.Contains(t, names, "metric.new")
		require.Contains(t, names, "metric.old")
	})
}

func TestBuildAttributeRenameMap(t *testing.T) {
	t.Run("single rename maps the historical alias to the anchor name", func(t *testing.T) {
		schema := &otelSchema{
			versionRenames: []versionRenames{
				{version: "1.1.0", attributes: map[string]string{"user": "tenant", "tenant": "user"}},
			},
		}
		got, err := buildAttributeRenameMap("1.1.0", schema, []string{"tenant"})
		require.NoError(t, err)
		require.Equal(t, map[string]string{"user": "tenant"}, got)
	})

	t.Run("transitive chain collapses every alias to the anchor name", func(t *testing.T) {
		// Anchored at 1.1.0, attr.v3 resolves backward: v3 → v2 (via 1.1.0) → v1
		// (via 1.0.0), so every historical alias maps to the anchor name.
		schema := &otelSchema{
			versionRenames: []versionRenames{
				{version: "1.0.0", attributes: map[string]string{"attr.v1": "attr.v2", "attr.v2": "attr.v1"}},
				{version: "1.1.0", attributes: map[string]string{"attr.v2": "attr.v3", "attr.v3": "attr.v2"}},
			},
		}
		got, err := buildAttributeRenameMap("1.1.0", schema, []string{"attr.v3"})
		require.NoError(t, err)
		require.Equal(t, map[string]string{"attr.v2": "attr.v3", "attr.v1": "attr.v3"}, got)
	})

	t.Run("no schema renames returns nil", func(t *testing.T) {
		got, err := buildAttributeRenameMap("1.1.0", &otelSchema{}, []string{"tenant"})
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("no canonical attributes returns nil", func(t *testing.T) {
		schema := &otelSchema{
			versionRenames: []versionRenames{
				{version: "1.1.0", attributes: map[string]string{"user": "tenant", "tenant": "user"}},
			},
		}
		got, err := buildAttributeRenameMap("1.1.0", schema, nil)
		require.NoError(t, err)
		require.Nil(t, got)
	})
}

func TestSchemaExpansionBudget(t *testing.T) {
	matchers := []*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "metric.current"),
	}

	t.Run("find variants returns no partial result", func(t *testing.T) {
		for name, limits := range map[string]schemaExpansionLimits{
			"work":      {work: 1, keyBytes: 1_000},
			"key bytes": {work: 1_000, keyBytes: 1},
		} {
			t.Run(name, func(t *testing.T) {
				e := newSchemaEngine(embeddedRegistry)
				e.limits = limits
				variants, _, err := e.findMatcherVariants(
					"registry/1.1.0",
					"registry/registry.yaml",
					[]*labels.Matcher{
						labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "test"),
					},
				)
				require.ErrorIs(t, err, errSchemaExpansion)
				require.ErrorContains(t, err, name)
				require.Nil(t, variants)
			})
		}
	})

	t.Run("preflights keys before allocation", func(t *testing.T) {
		keyBytes := uint64(len(matcherKey(matchers)))
		require.Positive(t, keyBytes)

		budget := newSchemaExpansionBudget(schemaExpansionLimits{work: 10, keyBytes: keyBytes - 1})
		key, err := matcherKeyWithBudget(matchers, budget)
		require.ErrorIs(t, err, errSchemaExpansion)
		require.ErrorContains(t, err, "deduplication key bytes")
		require.Empty(t, key)
		require.Zero(t, budget.keyBytes)
	})

	t.Run("charges duplicate candidate attempts", func(t *testing.T) {
		schema := &otelSchema{versionRenames: []versionRenames{{
			version: "1.0.0",
			metrics: map[string]string{
				"metric.current": "metric.old",
				"metric.old":     "metric.current",
			},
		}}}
		probe := newSchemaExpansionBudget(schemaExpansionLimits{work: 100, keyBytes: 10_000})
		variants, err := generateMatcherVariantsWithBudget("1.0.0", schema, matchers, probe)
		require.NoError(t, err)
		require.Len(t, variants, 2)
		require.Positive(t, probe.work)

		budget := newSchemaExpansionBudget(schemaExpansionLimits{work: probe.work - 1, keyBytes: 10_000})
		variants, err = generateMatcherVariantsWithBudget("1.0.0", schema, matchers, budget)
		require.ErrorIs(t, err, errSchemaExpansion)
		require.ErrorContains(t, err, "resolver work")
		require.Nil(t, variants)
	})

	t.Run("shares work across traversal directions", func(t *testing.T) {
		backward := &otelSchema{versionRenames: []versionRenames{
			{version: "1.0.0", metrics: map[string]string{"metric.middle": "metric.old", "metric.old": "metric.middle"}},
			{version: "1.1.0", metrics: map[string]string{"metric.current": "metric.middle", "metric.middle": "metric.current"}},
		}}
		forward := &otelSchema{versionRenames: []versionRenames{
			{version: "1.1.0"},
			{version: "1.2.0", metrics: map[string]string{"metric.current": "metric.new", "metric.new": "metric.current"}},
		}}
		combined := &otelSchema{versionRenames: append(slices.Clone(backward.versionRenames), forward.versionRenames[1:]...)}

		measure := func(t *testing.T, schema *otelSchema) uint64 {
			t.Helper()
			budget := newSchemaExpansionBudget(schemaExpansionLimits{work: 1_000, keyBytes: 100_000})
			_, err := generateMatcherVariantsWithBudget("1.1.0", schema, matchers, budget)
			require.NoError(t, err)
			return budget.work
		}
		limit := max(measure(t, backward), measure(t, forward))
		require.Greater(t, measure(t, combined), limit)

		for name, schema := range map[string]*otelSchema{"backward": backward, "forward": forward} {
			t.Run(name+" fits independently", func(t *testing.T) {
				budget := newSchemaExpansionBudget(schemaExpansionLimits{work: limit, keyBytes: 100_000})
				_, err := generateMatcherVariantsWithBudget("1.1.0", schema, matchers, budget)
				require.NoError(t, err)
			})
		}

		budget := newSchemaExpansionBudget(schemaExpansionLimits{work: limit, keyBytes: 100_000})
		variants, err := generateMatcherVariantsWithBudget("1.1.0", combined, matchers, budget)
		require.ErrorIs(t, err, errSchemaExpansion)
		require.Nil(t, variants)
	})

	t.Run("shares work with attribute mapping", func(t *testing.T) {
		schema := &otelSchema{versionRenames: []versionRenames{{
			version:    "1.0.0",
			metrics:    map[string]string{"metric.current": "metric.old", "metric.old": "metric.current"},
			attributes: map[string]string{"tenant": "user", "user": "tenant"},
		}}}
		measureVariants := newSchemaExpansionBudget(schemaExpansionLimits{work: 100, keyBytes: 10_000})
		_, err := generateMatcherVariantsWithBudget("1.0.0", schema, matchers, measureVariants)
		require.NoError(t, err)
		measureAttributes := newSchemaExpansionBudget(schemaExpansionLimits{work: 100, keyBytes: 10_000})
		_, err = buildAttributeRenameMapWithBudget("1.0.0", schema, []string{"tenant"}, measureAttributes)
		require.NoError(t, err)

		budget := newSchemaExpansionBudget(schemaExpansionLimits{
			work:     measureVariants.work + measureAttributes.work - 1,
			keyBytes: 10_000,
		})
		_, err = generateMatcherVariantsWithBudget("1.0.0", schema, matchers, budget)
		require.NoError(t, err)
		mapping, err := buildAttributeRenameMapWithBudget("1.0.0", schema, []string{"tenant"}, budget)
		require.ErrorIs(t, err, errSchemaExpansion)
		require.Nil(t, mapping)
	})

	t.Run("bounds resolver collections", func(t *testing.T) {
		schema := &otelSchema{versionRenames: []versionRenames{{
			version:    "1.0.0",
			metrics:    map[string]string{"metric.current": "metric.old"},
			attributes: map[string]string{"tenant": "user"},
		}}}
		budget := newSchemaExpansionBudget(schemaExpansionLimits{work: 100, keyBytes: 10_000})
		key, err := matcherKeyWithBudget(matchers, budget)
		require.NoError(t, err)
		result := make([][]*labels.Matcher, maxSchemaExpansion)
		variants, err := walkVersionsWithBudget(schema.versionRenames, matchers, map[string]struct{}{key: {}}, result, false, budget)
		require.ErrorIs(t, err, errSchemaExpansion)
		require.Nil(t, variants)

		canonicalAttrs := make([]string, maxSchemaExpansion+1)
		mapping, err := buildAttributeRenameMapWithBudget("1.0.0", schema, canonicalAttrs, budget)
		require.ErrorIs(t, err, errSchemaExpansion)
		require.Nil(t, mapping)
	})
}

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
	"context"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/annotations"
)

func TestStorageFanOutContracts(t *testing.T) {
	t.Run("canonical labels are valid", func(t *testing.T) {
		mapping := buildLabelMapping("metric.new", map[string]string{"old": "new"})
		got, err := transformOTelSchemaLabels(labels.FromStrings(
			model.MetricNameLabel, "metric.old",
			semconvURLLabel, "registry/1.0.0",
			"old", "same",
			"new", "same",
		), mapping)
		require.NoError(t, err)
		require.Equal(t, labels.FromStrings(model.MetricNameLabel, "metric.new", "new", "same"), got)
	})

	t.Run("conflicting canonical labels fail", func(t *testing.T) {
		mapping := buildLabelMapping("metric.new", map[string]string{"old": "new"})
		_, err := transformOTelSchemaLabels(labels.FromStrings("old", "first", "new", "second"), mapping)
		require.ErrorContains(t, err, "conflicting values")
	})

	t.Run("only attribute mappings require resorting", func(t *testing.T) {
		require.False(t, mappingNeedsResort(buildLabelMapping("metric.new", nil)))
		require.True(t, mappingNeedsResort(buildLabelMapping("metric.new", map[string]string{"old": "new"})))
	})

	t.Run("select hints are cloned", func(t *testing.T) {
		original := &storage.SelectHints{
			Start:            1,
			End:              2,
			Limit:            3,
			Grouping:         []string{"group"},
			ProjectionLabels: []string{"project"},
		}
		cloned := cloneSelectHints(original)
		cloned.Limit = 0
		cloned.Grouping[0] = "mutated group"
		cloned.ProjectionLabels[0] = "mutated projection"
		require.Equal(t, 3, original.Limit)
		require.Equal(t, []string{"group"}, original.Grouping)
		require.Equal(t, []string{"project"}, original.ProjectionLabels)
	})

	t.Run("canonical materialization is bounded", func(t *testing.T) {
		budget := canonicalSeriesBudget{kind: "series", limit: 2, remaining: 2}
		require.NoError(t, budget.take())
		require.NoError(t, budget.take())
		require.ErrorIs(t, budget.take(), errCanonicalSeriesMaterialization)
	})

	t.Run("label value fan-out is bounded", func(t *testing.T) {
		variants := []matcherVariant{
			{mapping: buildLabelMapping("metric", map[string]string{"old": "new"})},
			{mapping: buildLabelMapping("metric", nil)},
		}
		jobs, err := buildLabelValueJobs(variants, "new")
		require.NoError(t, err)
		require.Len(t, jobs, 3)

		_, err = buildLabelValueJobsUpTo(variants, "new", 2)
		require.ErrorIs(t, err, errSchemaExpansion)
	})
}

type resolverBudgetCallCounts struct {
	series      int
	chunk       int
	labelNames  int
	labelValues int
}

type resolverBudgetQuerier struct {
	storage.Querier
	calls *resolverBudgetCallCounts
}

func (q *resolverBudgetQuerier) Select(context.Context, bool, *storage.SelectHints, ...*labels.Matcher) storage.SeriesSet {
	q.calls.series++
	return storage.NoopSeriesSet()
}

func (q *resolverBudgetQuerier) LabelNames(context.Context, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	q.calls.labelNames++
	return nil, nil, nil
}

func (q *resolverBudgetQuerier) LabelValues(context.Context, string, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	q.calls.labelValues++
	return nil, nil, nil
}

type resolverBudgetChunkQuerier struct {
	storage.ChunkQuerier
	calls *resolverBudgetCallCounts
}

func (q *resolverBudgetChunkQuerier) Select(context.Context, bool, *storage.SelectHints, ...*labels.Matcher) storage.ChunkSeriesSet {
	q.calls.chunk++
	return storage.NoopChunkedSeriesSet()
}

func (q *resolverBudgetChunkQuerier) LabelNames(context.Context, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	q.calls.labelNames++
	return nil, nil, nil
}

func (q *resolverBudgetChunkQuerier) LabelValues(context.Context, string, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	q.calls.labelValues++
	return nil, nil, nil
}

func TestSchemaExpansionFailsBeforeStorage(t *testing.T) {
	engine := newSchemaEngine(embeddedRegistry)
	engine.limits = schemaExpansionLimits{work: 1, keyBytes: 1_000}
	calls := &resolverBudgetCallCounts{}
	matchers := []*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, "test"),
		labels.MustNewMatcher(labels.MatchEqual, semconvURLLabel, "registry/1.1.0"),
		labels.MustNewMatcher(labels.MatchEqual, schemaURLLabel, "registry/registry.yaml"),
	}

	querier := &awareQuerier{
		Querier:              &resolverBudgetQuerier{Querier: storage.NoopQuerier(), calls: calls},
		engine:               engine,
		canonicalSeriesLimit: maxCanonicalSeriesMaterialization,
	}
	series := querier.Select(t.Context(), true, nil, matchers...)
	require.False(t, series.Next())
	require.ErrorIs(t, series.Err(), errSchemaExpansion)

	chunkQuerier := &awareChunkQuerier{
		ChunkQuerier:         &resolverBudgetChunkQuerier{ChunkQuerier: storage.NoopChunkedQuerier(), calls: calls},
		engine:               engine,
		canonicalSeriesLimit: maxCanonicalSeriesMaterialization,
	}
	chunks := chunkQuerier.Select(t.Context(), true, nil, matchers...)
	require.False(t, chunks.Next())
	require.ErrorIs(t, chunks.Err(), errSchemaExpansion)

	_, _, err := querier.LabelNames(t.Context(), nil, matchers...)
	require.ErrorIs(t, err, errSchemaExpansion)
	_, _, err = querier.LabelValues(t.Context(), "tenant", nil, matchers...)
	require.ErrorIs(t, err, errSchemaExpansion)

	require.Equal(t, &resolverBudgetCallCounts{}, calls)
}

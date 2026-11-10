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
	"context"
	"maps"
	"slices"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/annotations"
)

func (q *memQuerier) LabelNames(ctx context.Context, hints *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return q.collectLabels(ctx, "", hints, matchers...)
}

func (q *memQuerier) LabelValues(ctx context.Context, name string, hints *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return q.collectLabels(ctx, name, hints, matchers...)
}

func (q *memQuerier) collectLabels(ctx context.Context, valueName string, hints *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	values := map[string]struct{}{}
	for _, series := range q.series {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if trimSeries(series, q.mint, q.maxt).Len() == 0 || !matchesAll(series.Labels, matchers) {
			continue
		}
		if q.reads != nil {
			q.reads.labelRows.Inc()
		}
		collectLabelValue(values, series.Labels, valueName)
	}
	return sortedLabelValues(values, hints), nil, nil
}

func (q *blockQuerier) LabelNames(ctx context.Context, hints *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	values := map[string]struct{}{}
	err := q.visitLabels(ctx, matchers, func(ls labels.Labels) error {
		collectLabelValue(values, ls, "")
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return sortedLabelValues(values, hints), nil, nil
}

func (q *blockQuerier) LabelValues(ctx context.Context, name string, hints *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	values := map[string]struct{}{}
	err := q.visitLabels(ctx, matchers, func(ls labels.Labels) error {
		collectLabelValue(values, ls, name)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return sortedLabelValues(values, hints), nil, nil
}

func (q *blockQuerier) visitLabels(ctx context.Context, matchers []*labels.Matcher, visit func(labels.Labels) error) error {
	if !q.block.Overlaps(q.mint, q.maxt) {
		return nil
	}
	return q.block.visitLabels(ctx, [][]*labels.Matcher{matchers}, visit)
}

// visitLabels uses the catalog's series dictionary and postings. Label APIs do
// not open or decode any sample payload, including for selective matchers.
func (b *Block) visitLabels(ctx context.Context, matcherSets [][]*labels.Matcher, visit func(labels.Labels) error) error {
	ids, err := b.resolveMatcherSets(matcherSets)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		i := sortSearchSeries(b.catalog.Series, id)
		if i < 0 || !matchesAny(b.catalog.Series[i].Labels, matcherSets) {
			continue
		}
		if b.reads != nil {
			b.reads.labelRows.Inc()
		}
		if err := visit(b.catalog.Series[i].Labels); err != nil {
			return err
		}
	}
	return nil
}

func collectLabelValue(values map[string]struct{}, ls labels.Labels, valueName string) {
	if valueName != "" {
		values[ls.Get(valueName)] = struct{}{}
		return
	}
	ls.Range(func(label labels.Label) {
		values[label.Name] = struct{}{}
	})
}

func sortedLabelValues(values map[string]struct{}, hints *storage.LabelHints) []string {
	result := slices.Sorted(maps.Keys(values))
	if hints != nil && hints.Limit > 0 {
		result = result[:min(len(result), hints.Limit)]
	}
	return result
}

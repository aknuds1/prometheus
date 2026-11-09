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

	"github.com/apache/arrow-go/v18/arrow/array"

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
	err := q.visitLabels(ctx, func(ls labels.Labels) error {
		if matchesAll(ls, matchers) {
			collectLabelValue(values, ls, "")
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return sortedLabelValues(values, hints), nil, nil
}

func (q *blockQuerier) LabelValues(ctx context.Context, name string, hints *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	values := map[string]struct{}{}
	err := q.visitLabels(ctx, func(ls labels.Labels) error {
		if matchesAll(ls, matchers) {
			collectLabelValue(values, ls, name)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return sortedLabelValues(values, hints), nil, nil
}

func (q *blockQuerier) visitLabels(ctx context.Context, visit func(labels.Labels) error) error {
	if !q.block.Overlaps(q.mint, q.maxt) {
		return nil
	}
	return q.block.visitLabels(ctx, visit)
}

func (b *Block) visitLabels(ctx context.Context, visit func(labels.Labels) error) error {
	reader, parquetFile, file, err := b.newRecordReader(ctx, nil, labelLeaves)
	if err != nil {
		return err
	}
	defer func() {
		reader.Release()
		parquetFile.Close()
		file.Close()
	}()

	for reader.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		record := reader.RecordBatch()
		labelList := record.Column(0).(*array.List)
		structs := labelList.ListValues().(*array.Struct)
		names := structs.Field(0).(*array.String)
		values := structs.Field(1).(*array.String)
		for row := range int(record.NumRows()) {
			start, end := labelList.ValueOffsets(row)
			pairs := make([]labels.Label, 0, end-start)
			for i := start; i < end; i++ {
				pairs = append(pairs, labels.Label{Name: names.Value(int(i)), Value: values.Value(int(i))})
			}
			if b.reads != nil {
				b.reads.labelRows.Inc()
			}
			if err := visit(labels.New(pairs...)); err != nil {
				return err
			}
		}
	}
	return reader.Err()
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

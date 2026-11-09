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

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/prometheus/prometheus/model/labels"
)

// Parquet leaf columns of the block schema.
//
// These are leaf indexes, not Arrow field indexes: pqarrow selects columns per
// leaf, so the labels list<struct<name,value>> occupies two of them. Asking for
// a single leaf of a struct yields a struct with one child, which decodes into
// label sets with no values.
const (
	leafSeriesID = iota
	leafLabelName
	leafLabelValue
	leafTimestamp
	leafKind
	leafValue
	leafHistogram
)

// labelLeaves are the leaves holding the label sets.
var labelLeaves = []int{leafLabelName, leafLabelValue}

// Select returns the series of the block that match matcherSets within
// [mint, maxt].
//
// It reads two passes rather than one. The first reads only the labels column and
// decides which rows match; the second reads every column of the row groups that
// contain those rows. Without that split a select costs a full scan of the block,
// which is what made queries two orders of magnitude slower than the TSDB: row
// groups are the only unit Parquet can skip, and skipping needs to know where the
// matches are before reading the payload.
func (b *Block) Select(ctx context.Context, mint, maxt int64, matcherSets [][]*labels.Matcher, limit int) ([]Series, error) {
	if !b.Overlaps(mint, maxt) {
		return nil, nil
	}

	// Pass one: labels only.
	rowLabels, err := b.readLabels(ctx)
	if err != nil {
		return nil, err
	}

	matched := make([]int, 0, 64)
	for i, ls := range rowLabels {
		if !matchesAny(ls, matcherSets) {
			continue
		}
		matched = append(matched, i)
	}
	if len(matched) == 0 {
		return nil, nil
	}

	// Pass two: only the row groups holding matches.
	rowGroups := b.rowGroupsFor(matched)
	rows, err := b.readRows(ctx, rowGroups)
	if err != nil {
		return nil, err
	}

	// The rows read are the rows of the selected row groups, in order, so walk the
	// groups in the same order to recover each row's global index.
	wanted := make(map[int]struct{}, len(matched))
	for _, row := range matched {
		wanted[row] = struct{}{}
	}

	perGroup := int(b.meta.SeriesPerRowGroup)
	out := make([]Series, 0, len(matched))
	next := 0
	for _, group := range rowGroups {
		start := group * perGroup
		end := min(start+perGroup, b.meta.NumSeries)
		for row := start; row < end && next < len(rows); row++ {
			series := rows[next]
			next++
			if _, ok := wanted[row]; !ok {
				continue
			}
			trimmed := trimSeries(series, mint, maxt)
			if trimmed.Len() == 0 {
				continue
			}
			out = append(out, trimmed)
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// readLabels reads the label set of every row, without reading any sample data.
func (b *Block) readLabels(ctx context.Context) ([]labels.Labels, error) {
	rr, pf, f, err := b.newRecordReader(ctx, nil, labelLeaves)
	if err != nil {
		return nil, err
	}
	defer func() {
		rr.Release()
		pf.Close()
		f.Close()
	}()

	out := make([]labels.Labels, 0, b.meta.NumSeries)
	for rr.Next() {
		out = append(out, decodeLabels(rr.RecordBatch())...)
	}
	return out, rr.Err()
}

// readRows reads the given row groups and returns their series in row order.
func (b *Block) readRows(ctx context.Context, rowGroups []int) ([]Series, error) {
	if len(rowGroups) == 0 {
		return nil, nil
	}
	if b.reads != nil {
		b.reads.rowGroups.Add(uint64(len(rowGroups)))
	}
	rr, pf, f, err := b.newRecordReader(ctx, rowGroups, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		rr.Release()
		pf.Close()
		f.Close()
	}()

	out := make([]Series, 0, int(b.meta.SeriesPerRowGroup)*len(rowGroups))
	for rr.Next() {
		series, err := DecodeRecord(rr.RecordBatch())
		if err != nil {
			return nil, err
		}
		if b.reads != nil {
			b.reads.sampleRows.Add(uint64(len(series)))
		}
		out = append(out, series...)
	}
	return out, rr.Err()
}

// rowGroupsFor maps matching row indexes to the row groups that contain them.
//
// The indexes returned by readRows are relative to the first row group read, so
// the caller needs the same mapping; it is derived again in Select by walking the
// matching rows in order.
func (b *Block) rowGroupsFor(matched []int) []int {
	perGroup := int(b.meta.SeriesPerRowGroup)
	if perGroup <= 0 {
		return nil // Unknown geometry: read everything.
	}

	seen := map[int]struct{}{}
	var groups []int
	for _, row := range matched {
		group := row / perGroup
		if _, ok := seen[group]; ok {
			continue
		}
		seen[group] = struct{}{}
		groups = append(groups, group)
	}
	return groups
}

// decodeLabels extracts the label sets of a record batch that holds only the
// labels column.
func decodeLabels(record arrow.RecordBatch) []labels.Labels {
	list := record.Column(0).(*array.List)
	structs := list.ListValues().(*array.Struct)
	names := structs.Field(0).(*array.String)
	values := structs.Field(1).(*array.String)

	out := make([]labels.Labels, 0, record.NumRows())
	for row := range int(record.NumRows()) {
		start, end := list.ValueOffsets(row)
		ls := make([]labels.Label, 0, end-start)
		for i := start; i < end; i++ {
			ls = append(ls, labels.Label{Name: names.Value(int(i)), Value: values.Value(int(i))})
		}
		out = append(out, labels.New(ls...))
	}
	return out
}

// Copyright 2026 The Prometheus Authors
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
	"math"
	"sort"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

// sourceTimeRange is the closed time range a querier can return samples for.
type sourceTimeRange struct {
	minTime, maxTime int64
}

// disjointSources reports whether the ranges are pairwise non-overlapping once
// sorted by time. Concatenation is only equivalent to a vertical merge then.
func disjointSources(ranges []sourceTimeRange) bool {
	if len(ranges) < 2 {
		return false
	}
	sorted := make([]sourceTimeRange, len(ranges))
	copy(sorted, ranges)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].minTime < sorted[j].minTime })
	for i := 1; i < len(sorted); i++ {
		// Ranges are closed, so a shared boundary timestamp still overlaps.
		if sorted[i].minTime <= sorted[i-1].maxTime {
			return false
		}
	}
	return true
}

// concatSeriesFunc merges series whose sources are time-disjoint. Unlike
// storage.ChainedSeriesMerge it keeps the per-source batch interface instead of
// driving a sample-at-a-time merge, which is what lets a matrix selector stay
// on the batched path.
//
// The variadic series arrive in source order, which the caller has already
// sorted by time, so no further ordering is needed here.
func concatSeriesFunc(series ...storage.Series) storage.Series {
	if len(series) == 1 {
		return series[0]
	}
	// The merge series set builds this slice for the call only, so reordering it
	// in place avoids a copy per merged series.
	if !orderPartsByTime(series) {
		// A part cannot report its range, so fall back rather than guess an order.
		return storage.ChainedSeriesMerge(series...)
	}
	return &concatSeries{series: series, lset: series[0].Labels()}
}

// rangedSeries reports the closed time range its samples span.
type rangedSeries interface {
	timeRange() (mint, maxt int64)
}

// orderPartsByTime sorts parts oldest first and reports whether every part could
// report a range. The merge series set is heap-ordered, so its output order says
// nothing about source order.
func orderPartsByTime(parts []storage.Series) bool {
	if len(parts) == 2 {
		a, aok := parts[0].(rangedSeries)
		b, bok := parts[1].(rangedSeries)
		if !aok || !bok {
			return false
		}
		amin, _ := a.timeRange()
		bmin, _ := b.timeRange()
		if bmin < amin {
			parts[0], parts[1] = parts[1], parts[0]
		}
		return true
	}
	mins := make([]int64, len(parts))
	for i, part := range parts {
		r, ok := part.(rangedSeries)
		if !ok {
			return false
		}
		mins[i], _ = r.timeRange()
	}
	// Insertion sort: parts per series are few, and this avoids an index slice.
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && mins[j] < mins[j-1]; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
			mins[j], mins[j-1] = mins[j-1], mins[j]
		}
	}
	return true
}

// concatSeries exposes one series as the concatenation of its per-source parts.
type concatSeries struct {
	series []storage.Series
	lset   labels.Labels
}

func (s *concatSeries) Labels() labels.Labels { return s.lset }

// Iterator returns an iterator over all parts in time order. The result
// implements storage.RangeBatchIterator so callers keep batching.
func (s *concatSeries) Iterator(reuse chunkenc.Iterator) chunkenc.Iterator {
	its := make([]chunkenc.Iterator, len(s.series))
	for i, part := range s.series {
		if i == 0 {
			its[i] = part.Iterator(reuse)
			continue
		}
		its[i] = part.Iterator(nil)
	}
	return &concatIterator{its: its, cur: -1}
}

// batchRange is a range batch iterator that can report exhaustion. Concatenation
// needs that to tell "no samples below maxt yet" from "no samples at all", which
// decides whether to move on to the next part.
type batchRange interface {
	storage.RangeBatchIterator
	exhausted() bool
}

// concatIterator walks time-ordered parts of one series without merging.
type concatIterator struct {
	its   []chunkenc.Iterator
	cur   int
	batch storage.SampleBatch
	err   error
}

// advance moves to the next part, reporting whether one exists.
func (it *concatIterator) advance() bool {
	if it.cur+1 >= len(it.its) {
		return false
	}
	it.cur++
	return true
}

// NextBatchUntil fills one batch from as many parts as fit. A single part is
// handed back zero-copy; only when consecutive parts both contribute does the
// batch have to be copied and extended.
func (it *concatIterator) NextBatchUntil(maxSamples int, maxt int64) bool {
	if it.err != nil {
		return false
	}
	if maxSamples <= 0 {
		maxSamples = math.MaxInt
	}
	if len(it.its) == 0 {
		it.batch = storage.SampleBatch{}
		return false
	}
	if it.cur < 0 && !it.advance() {
		it.batch = storage.SampleBatch{}
		return false
	}

	var batch storage.SampleBatch
	got := false
	for it.cur < len(it.its) {
		remaining := maxSamples - batch.Len()
		if remaining <= 0 {
			break
		}
		child := it.its[it.cur]
		br, ok := child.(batchRange)
		if !ok {
			// Non-columnar part: copy its samples into the batch.
			if it.accumulateInto(&batch, child, remaining, maxt) {
				got = true
			}
			if err := child.Err(); err != nil {
				it.err = err
				it.batch = storage.SampleBatch{}
				return false
			}
			if !it.advance() {
				break
			}
			continue
		}
		if !br.NextBatchUntil(remaining, maxt) {
			if err := br.Err(); err != nil {
				it.err = err
				it.batch = storage.SampleBatch{}
				return false
			}
			// Not exhausted means this part was truncated by maxt. Parts are
			// time-ordered, so no later part can hold a sample at or below maxt.
			if !br.exhausted() {
				break
			}
			if !it.advance() {
				break
			}
			continue
		}
		part := br.Batch()
		if !got {
			batch = part
			got = true
		} else {
			appendBatch(&batch, part)
		}
		if !br.exhausted() {
			// This part still has samples above maxt; resume there next call.
			break
		}
		if !it.advance() {
			break
		}
	}
	it.batch = batch
	return got
}

// appendBatch extends dst with the samples of src.
func appendBatch(dst *storage.SampleBatch, src storage.SampleBatch) {
	prev := len(dst.Timestamps)
	if len(src.Values) > 0 {
		dst.Values = append(dst.Values, src.Values...)
	}
	if src.Histograms != nil {
		dst.Histograms = growHistograms(dst.Histograms, prev)
		dst.Histograms = append(dst.Histograms, src.Histograms...)
	}
	if src.FloatHistograms != nil {
		dst.FloatHistograms = growFloatHistograms(dst.FloatHistograms, prev)
		dst.FloatHistograms = append(dst.FloatHistograms, src.FloatHistograms...)
	}
	if src.StartTimestamps != nil {
		dst.StartTimestamps = growInt64s(dst.StartTimestamps, prev)
		dst.StartTimestamps = append(dst.StartTimestamps, src.StartTimestamps...)
	}
	dst.Timestamps = append(dst.Timestamps, src.Timestamps...)
}

func growInt64s(in []int64, n int) []int64 {
	for len(in) < n {
		in = append(in, 0)
	}
	return in
}

func growHistograms(in []*histogram.Histogram, n int) []*histogram.Histogram {
	for len(in) < n {
		in = append(in, nil)
	}
	return in
}

func growFloatHistograms(in []*histogram.FloatHistogram, n int) []*histogram.FloatHistogram {
	for len(in) < n {
		in = append(in, nil)
	}
	return in
}

// accumulateInto copies samples from a part that cannot batch.
func (it *concatIterator) accumulateInto(batch *storage.SampleBatch, child chunkenc.Iterator, maxSamples int, maxt int64) bool {
	added := false
	for batch.Len() < maxSamples {
		vt := child.Next()
		if vt == chunkenc.ValNone {
			break
		}
		if child.AtT() > maxt {
			break
		}
		appendSampleAt(batch, child, vt)
		added = true
	}
	return added
}

func (it *concatIterator) NextBatch(maxSamples int) bool {
	return it.NextBatchUntil(maxSamples, math.MaxInt64)
}

func (it *concatIterator) Batch() storage.SampleBatch { return it.batch }

func (it *concatIterator) Next() chunkenc.ValueType {
	if !it.NextBatchUntil(1, math.MaxInt64) {
		return chunkenc.ValNone
	}
	return it.batch.ValueType(0)
}

// Seek positions the iterator at the first sample at or after t. The part is
// left positioned on that sample, so a following Next starts after it.
func (it *concatIterator) Seek(t int64) chunkenc.ValueType {
	if it.cur < 0 {
		it.cur = 0
	}
	for it.cur < len(it.its) {
		child := it.its[it.cur]
		if vt := child.Seek(t); vt != chunkenc.ValNone {
			it.batch = storage.SampleBatch{}
			appendSampleAt(&it.batch, child, vt)
			return it.batch.ValueType(0)
		}
		if !it.advance() {
			break
		}
	}
	it.batch = storage.SampleBatch{}
	return chunkenc.ValNone
}

func (it *concatIterator) At() (int64, float64) {
	if it.batch.Len() == 0 {
		return 0, 0
	}
	if it.batch.Values == nil {
		return it.batch.Timestamps[0], 0
	}
	return it.batch.Timestamps[0], it.batch.Values[0]
}

func (it *concatIterator) AtHistogram(*histogram.Histogram) (int64, *histogram.Histogram) {
	if it.batch.Len() == 0 || it.batch.Histograms == nil || it.batch.Histograms[0] == nil {
		if it.batch.Len() == 0 {
			return 0, nil
		}
		return it.batch.Timestamps[0], nil
	}
	return it.batch.Timestamps[0], it.batch.Histograms[0]
}

func (it *concatIterator) AtFloatHistogram(*histogram.FloatHistogram) (int64, *histogram.FloatHistogram) {
	if it.batch.Len() == 0 {
		return 0, nil
	}
	if it.batch.FloatHistograms == nil || it.batch.FloatHistograms[0] == nil {
		return it.batch.Timestamps[0], nil
	}
	return it.batch.Timestamps[0], it.batch.FloatHistograms[0]
}

func (it *concatIterator) AtT() int64 {
	if it.batch.Len() == 0 {
		return 0
	}
	return it.batch.Timestamps[0]
}

func (it *concatIterator) AtST() int64 {
	if it.batch.Len() == 0 {
		return 0
	}
	return it.batch.StartTimestamp(0)
}

func (it *concatIterator) Err() error { return it.err }

// appendSampleAt appends the sample an iterator is already positioned on. It
// must not advance the iterator, so the value type is passed in.
func appendSampleAt(batch *storage.SampleBatch, it chunkenc.Iterator, vt chunkenc.ValueType) {
	switch vt {
	case chunkenc.ValFloat:
		t, v := it.At()
		batch.Timestamps = append(batch.Timestamps, t)
		batch.Values = append(batch.Values, v)
	case chunkenc.ValHistogram:
		t, h := it.AtHistogram(nil)
		batch.Timestamps = append(batch.Timestamps, t)
		batch.Histograms = append(batch.Histograms, h)
	case chunkenc.ValFloatHistogram:
		t, fh := it.AtFloatHistogram(nil)
		batch.Timestamps = append(batch.Timestamps, t)
		batch.FloatHistograms = append(batch.FloatHistograms, fh)
	}
}

var _ storage.RangeBatchIterator = (*concatIterator)(nil)

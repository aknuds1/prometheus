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
	"math"
	"slices"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/util/annotations"
)

// Querier returns a querier over [mint, maxt].
//
// The buffer and every overlapping block are queried separately and merged, so
// that flushed and unflushed data look the same to the caller.
func (s *Storage) Querier(mint, maxt int64) (storage.Querier, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	var queriers []storage.Querier
	if len(s.buffer) > 0 {
		series := make([]Series, 0, len(s.buffer))
		for _, hs := range s.buffer {
			series = append(series, headToSeries(hs))
		}
		queriers = append(queriers, &memQuerier{series: series, mint: mint, maxt: maxt})
	}
	for _, block := range s.blocks {
		if !block.Overlaps(mint, maxt) {
			continue
		}
		queriers = append(queriers, &blockQuerier{block: block, mint: mint, maxt: maxt})
	}

	if len(queriers) == 0 {
		return storage.NoopQuerier(), nil
	}
	if len(queriers) == 1 {
		return queriers[0], nil
	}
	return storage.NewMergeQuerier(queriers, nil, storage.ChainedSeriesMerge), nil
}

// ChunkQuerier returns a chunk querier over [mint, maxt].
//
// Chunks are encoded on demand from the sample data. The block format stores
// samples, not chunks, so there is nothing to reuse here; this exists because
// storage.Storage requires it and because remote read expects it.
func (s *Storage) ChunkQuerier(mint, maxt int64) (storage.ChunkQuerier, error) {
	q, err := s.Querier(mint, maxt)
	if err != nil {
		return nil, err
	}
	return chunkQuerier{q: q}, nil
}

// memQuerier serves in-memory series.
type memQuerier struct {
	series []Series
	mint   int64
	maxt   int64
}

func (*memQuerier) LabelValues(context.Context, string, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (*memQuerier) LabelNames(context.Context, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (*memQuerier) Close() error { return nil }

func (q *memQuerier) Select(_ context.Context, sortSeries bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	return selectFromSeries(q.series, q.mint, q.maxt, sortSeries, hints, matchers)
}

// blockQuerier serves the series of one block.
type blockQuerier struct {
	block *Block
	mint  int64
	maxt  int64
}

func (*blockQuerier) LabelValues(context.Context, string, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (*blockQuerier) LabelNames(context.Context, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (*blockQuerier) Close() error { return nil }

func (q *blockQuerier) Select(ctx context.Context, _ bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	mint, maxt := relevantRange(q.mint, q.maxt, hints)
	series, err := q.block.Select(ctx, mint, maxt, [][]*labels.Matcher{matchers}, 0)
	if err != nil {
		return storage.ErrSeriesSet(err)
	}
	return &seriesSet{series: series}
}

// relevantRange applies the hint bounds to a querier range.
func relevantRange(mint, maxt int64, hints *storage.SelectHints) (int64, int64) {
	if hints == nil {
		return mint, maxt
	}
	if hints.Start > mint {
		mint = hints.Start
	}
	if hints.End < maxt {
		maxt = hints.End
	}
	return mint, maxt
}

// selectFromSeries applies matchers and the time range to a set of series.
func selectFromSeries(series []Series, mint, maxt int64, sortSeries bool, hints *storage.SelectHints, matchers []*labels.Matcher) storage.SeriesSet {
	if hints != nil {
		if hints.Start > mint {
			mint = hints.Start
		}
		if hints.End < maxt {
			maxt = hints.End
		}
	}

	selected := make([]Series, 0, len(series))
	for _, s := range series {
		if !matchesAll(s.Labels, matchers) {
			continue
		}
		trimmed := trimSeries(s, mint, maxt)
		if trimmed.Len() == 0 {
			continue
		}
		selected = append(selected, trimmed)
	}

	// Always sort inside a single source. storage.NewMergeQuerier and
	// ChainedSeriesMerge assume each input series set is ordered by label set, so
	// honouring sortSeries=false here would silently drop series whenever more than
	// one block or the buffer is involved.
	_ = sortSeries
	slices.SortFunc(selected, func(a, b Series) int { return labels.Compare(a.Labels, b.Labels) })
	return &seriesSet{series: selected}
}

func matchesAll(ls labels.Labels, matchers []*labels.Matcher) bool {
	for _, m := range matchers {
		if !m.Matches(ls.Get(m.Name)) {
			return false
		}
	}
	return true
}

// trimSeries returns the samples of s within [mint, maxt].
func trimSeries(s Series, mint, maxt int64) Series {
	from, to := 0, s.Len()
	for from < to && s.Timestamps[from] < mint {
		from++
	}
	for to > from && s.Timestamps[to-1] > maxt {
		to--
	}
	if from == 0 && to == s.Len() {
		return s
	}
	return Series{
		Labels:          s.Labels,
		Timestamps:      s.Timestamps[from:to],
		StartTimestamps: s.StartTimestamps[from:to],
		Kinds:           s.Kinds[from:to],
		Values:          s.Values[from:to],
		Histograms:      s.Histograms[from:to],
	}
}

// headToSeries converts a buffered series to the block representation.
func headToSeries(hs *headSeries) Series {
	s := Series{
		Labels:          hs.labels,
		Timestamps:      make([]int64, len(hs.samples)),
		StartTimestamps: make([]int64, len(hs.samples)),
		Kinds:           make([]SampleKind, len(hs.samples)),
		Values:          make([]float64, len(hs.samples)),
		Histograms:      make([][]byte, len(hs.samples)),
	}
	for i, smpl := range hs.samples {
		s.Timestamps[i] = smpl.t
		s.StartTimestamps[i] = smpl.st
		s.Kinds[i] = smpl.kind
		s.Values[i] = smpl.v
		s.Histograms[i] = smpl.hist
	}
	return s
}

// seriesSet is a series set over already-selected series.
type seriesSet struct {
	series []Series
	i      int
	err    error
}

func (s *seriesSet) Next() bool {
	if s.err != nil {
		return false
	}
	if s.i >= len(s.series) {
		return false
	}
	s.i++
	return true
}

func (s *seriesSet) At() storage.Series { return &series{s.series[s.i-1]} }

func (s *seriesSet) Err() error { return s.err }

func (*seriesSet) Warnings() annotations.Annotations { return nil }

// series is a storage.Series backed by one block row.
type series struct {
	s Series
}

func (s *series) Labels() labels.Labels { return s.s.Labels }

func (s *series) Iterator(_ chunkenc.Iterator) chunkenc.Iterator {
	// pos must start before the first sample, otherwise Next would skip it and a
	// single-sample series would yield nothing at all.
	return &sampleIterator{series: s.s, pos: -1}
}

// sampleIterator iterates the samples of a block row.
type sampleIterator struct {
	series Series
	// pos is the index of the current sample, -1 before the first Next.
	pos int
}

func (it *sampleIterator) Next() chunkenc.ValueType {
	if it.pos+1 >= it.series.Len() {
		it.pos = it.series.Len()
		return chunkenc.ValNone
	}
	it.pos++
	return it.valueType()
}

func (it *sampleIterator) Seek(t int64) chunkenc.ValueType {
	if it.pos < 0 {
		it.pos = 0
	}
	for it.pos < it.series.Len() && it.series.Timestamps[it.pos] < t {
		it.pos++
	}
	if it.pos >= it.series.Len() {
		return chunkenc.ValNone
	}
	return it.valueType()
}

func (it *sampleIterator) valueType() chunkenc.ValueType {
	switch it.series.Kinds[it.pos] {
	case SampleHistogram:
		return chunkenc.ValHistogram
	case SampleFloatHistogram:
		return chunkenc.ValFloatHistogram
	default:
		return chunkenc.ValFloat
	}
}

func (it *sampleIterator) At() (int64, float64) {
	if it.series.Kinds[it.pos] == SampleStale {
		// Reconstruct the exact stale marker bits: the format cannot store them.
		return it.series.Timestamps[it.pos], math.Float64frombits(value.StaleNaN)
	}
	return it.series.Timestamps[it.pos], it.series.Values[it.pos]
}

func (it *sampleIterator) AtHistogram(reuse *histogram.Histogram) (int64, *histogram.Histogram) {
	h, _, err := it.series.HistogramAt(it.pos)
	if err != nil || h == nil {
		return it.series.Timestamps[it.pos], nil
	}
	if reuse != nil {
		*reuse = *h
		return it.series.Timestamps[it.pos], reuse
	}
	return it.series.Timestamps[it.pos], h
}

func (it *sampleIterator) AtFloatHistogram(reuse *histogram.FloatHistogram) (int64, *histogram.FloatHistogram) {
	h, fh, err := it.series.HistogramAt(it.pos)
	if err != nil {
		return it.series.Timestamps[it.pos], nil
	}
	if fh == nil && h != nil {
		fh = h.ToFloat(nil)
	}
	if reuse != nil && fh != nil {
		*reuse = *fh
		return it.series.Timestamps[it.pos], reuse
	}
	return it.series.Timestamps[it.pos], fh
}

func (it *sampleIterator) AtT() int64 { return it.series.Timestamps[it.pos] }

// AtST returns the start timestamp of the current sample, or 0 when unknown.
func (it *sampleIterator) AtST() int64 {
	if it.pos < 0 || it.pos >= len(it.series.StartTimestamps) {
		return 0
	}
	return it.series.StartTimestamps[it.pos]
}

func (*sampleIterator) Err() error { return nil }

// chunkQuerier adapts a sample querier to a chunk querier.
type chunkQuerier struct {
	q storage.Querier
}

func (c chunkQuerier) Select(ctx context.Context, sortSeries bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.ChunkSeriesSet {
	return chunkSeriesSet{ss: c.q.Select(ctx, sortSeries, hints, matchers...)}
}

func (c chunkQuerier) LabelValues(ctx context.Context, name string, hints *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return c.q.LabelValues(ctx, name, hints, matchers...)
}

func (c chunkQuerier) LabelNames(ctx context.Context, hints *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return c.q.LabelNames(ctx, hints, matchers...)
}

func (c chunkQuerier) Close() error { return c.q.Close() }

type chunkSeriesSet struct {
	ss storage.SeriesSet
}

func (c chunkSeriesSet) Next() bool                        { return c.ss.Next() }
func (c chunkSeriesSet) At() storage.ChunkSeries           { return chunkSeries{c.ss.At().(*series)} }
func (c chunkSeriesSet) Err() error                        { return c.ss.Err() }
func (c chunkSeriesSet) Warnings() annotations.Annotations { return c.ss.Warnings() }

// chunkSeries encodes its samples into chunks on demand.
type chunkSeries struct {
	s *series
}

func (c chunkSeries) Labels() labels.Labels { return c.s.s.Labels }

func (c chunkSeries) Iterator(_ chunks.Iterator) chunks.Iterator {
	metas, err := buildChunks(c.s.s)
	return &chunkIterator{metas: metas, err: err}
}

// chunkSize is the number of samples per chunk. XOR chunks hold 120 float samples;
// staying below the limit for all sample types keeps one chunk type for the whole
// series, which is a simplification of the prototype, not a format decision.
const chunkSize = 120

// buildChunks encodes a series into chunks.
func buildChunks(s Series) ([]chunks.Meta, error) {
	var (
		metas []chunks.Meta
		cur   chunkenc.Chunk
		app   chunkenc.Appender
		count int
		minT  int64
		maxT  int64
	)

	finish := func() {
		if cur == nil || count == 0 {
			return
		}
		metas = append(metas, chunks.Meta{MinTime: minT, MaxTime: maxT, Chunk: cur})
		cur, app, count = nil, nil, 0
	}

	for i := range s.Len() {
		t := s.Timestamps[i]
		if cur == nil {
			cur = chunkenc.NewXORChunk()
			newApp, err := cur.Appender()
			if err != nil {
				return nil, err
			}
			app = newApp
			minT = t
		}

		switch s.Kinds[i] {
		case SampleFloat:
			app.Append(0, t, s.Values[i])
		default:
			var err error
			if s.Kinds[i] == SampleFloatHistogram {
				_, fh, decErr := s.HistogramAt(i)
				if decErr != nil {
					return nil, decErr
				}
				var replacement chunkenc.Chunk
				replacement, _, app, err = app.AppendFloatHistogram(nil, 0, t, fh, false)
				if replacement != nil {
					cur = replacement
				}
			} else {
				h, _, decErr := s.HistogramAt(i)
				if decErr != nil {
					return nil, decErr
				}
				var replacement chunkenc.Chunk
				replacement, _, app, err = app.AppendHistogram(nil, 0, t, h, false)
				if replacement != nil {
					cur = replacement
				}
			}
			if err != nil {
				return nil, err
			}
		}
		maxT = t
		count++
		if count >= chunkSize {
			finish()
		}
	}
	finish()
	return metas, nil
}

// chunkIterator iterates pre-built chunks.
type chunkIterator struct {
	metas []chunks.Meta
	err   error
	i     int
}

func (c *chunkIterator) Next() bool {
	if c.err != nil || c.i >= len(c.metas) {
		return false
	}
	c.i++
	return true
}

func (c *chunkIterator) At() chunks.Meta { return c.metas[c.i-1] }

func (c *chunkIterator) Seek(t int64) bool {
	for c.i < len(c.metas) {
		if c.metas[c.i].MaxTime >= t {
			c.i++
			return true
		}
		c.i++
	}
	return false
}

func (c *chunkIterator) Err() error { return c.err }

var (
	_ storage.Querier        = (*memQuerier)(nil)
	_ storage.Querier        = (*blockQuerier)(nil)
	_ storage.ChunkQuerier   = chunkQuerier{}
	_ storage.SeriesSet      = (*seriesSet)(nil)
	_ storage.Series         = (*series)(nil)
	_ chunkenc.Iterator      = (*sampleIterator)(nil)
	_ storage.ChunkSeriesSet = chunkSeriesSet{}
	_ storage.ChunkSeries    = chunkSeries{}
	_ chunks.Iterator        = (*chunkIterator)(nil)
)

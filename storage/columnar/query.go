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
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/seriesmetadata"
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
		queriers = append(queriers, &memQuerier{series: series, mint: mint, maxt: maxt, reads: s.reads})
	}
	for _, block := range s.blocks {
		if !block.Overlaps(mint, maxt) {
			continue
		}
		queriers = append(queriers, &blockQuerier{block: block, mint: mint, maxt: maxt})
	}

	var q storage.Querier
	switch len(queriers) {
	case 0:
		q = storage.NoopQuerier()
	case 1:
		q = queriers[0]
	default:
		q = storage.NewMergeQuerier(queriers, nil, storage.ChainedSeriesMerge)
	}
	return &resourceQuerier{Querier: q, storage: s}, nil
}

// resourceQuerier adds the metadata catalog to ordinary sample selection. The
// catalog belongs to Storage rather than an individual block or head buffer, so
// wrapping after the merge keeps all sample layouts on the same identity path.
type resourceQuerier struct {
	storage.Querier
	storage *Storage
}

// GetResourceAt implements storage.ResourceQuerier. The returned ResourceVersion
// owns its attribute map, which the caller may modify or retain.
func (q *resourceQuerier) GetResourceAt(seriesHash uint64, timestamp int64) (*seriesmetadata.ResourceVersion, bool) {
	q.storage.mtx.Lock()
	defer q.storage.mtx.Unlock()
	if q.storage.metadata == nil {
		return nil, false
	}
	owner, ok := q.storage.metadata.ownersAt(seriesHash, MetadataResource, timestamp)
	if !ok {
		return nil, false
	}
	version, ok, _ := q.storage.metadata.atVersion(owner, timestamp)
	if !ok {
		return nil, false
	}
	return &seriesmetadata.ResourceVersion{
		Descriptive: version.Content.Attributes, OwnerID: owner.String(), EffectiveFrom: version.EffectiveFrom,
	}, true
}

// HasResourceMembershipAt implements storage.ResourceMembershipQuerier.
func (q *resourceQuerier) HasResourceMembershipAt(seriesHash uint64, timestamp int64) bool {
	q.storage.mtx.Lock()
	defer q.storage.mtx.Unlock()
	if q.storage.metadata == nil {
		return false
	}
	_, ok := q.storage.metadata.ownersAt(seriesHash, MetadataResource, timestamp)
	return ok
}

// IterUniqueAttributeNames implements storage.ResourceQuerier.
func (q *resourceQuerier) IterUniqueAttributeNames(fn func(string)) error {
	q.storage.mtx.Lock()
	var names []string
	if q.storage.metadata != nil {
		names = q.storage.metadata.resourceAttributeNames()
	}
	q.storage.mtx.Unlock()
	for _, name := range names {
		fn(name)
	}
	return nil
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
	reads  *readStats
}

func (*memQuerier) Close() error { return nil }

func (q *memQuerier) Select(_ context.Context, sortSeries bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	return selectFromSeries(q.series, q.mint, q.maxt, sortSeries, hints, q.reads, matchers)
}

// blockQuerier serves the series of one block.
type blockQuerier struct {
	block *Block
	mint  int64
	maxt  int64
}

func (*blockQuerier) Close() error { return nil }

func (q *blockQuerier) Select(ctx context.Context, _ bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	mint, maxt := relevantRange(q.mint, q.maxt, hints)
	series, err := q.block.Select(ctx, mint, maxt, [][]*labels.Matcher{matchers}, 0)
	if err != nil {
		return storage.ErrSeriesSet(err)
	}
	return &seriesSet{series: series, reads: q.block.reads}
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
func selectFromSeries(series []Series, mint, maxt int64, sortSeries bool, hints *storage.SelectHints, reads *readStats, matchers []*labels.Matcher) storage.SeriesSet {
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
	return &seriesSet{series: selected, reads: reads}
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
	var starts []int64
	if len(s.StartTimestamps) > 0 {
		starts = s.StartTimestamps[from:to]
	}
	var histograms [][]byte
	if len(s.Histograms) > 0 {
		histograms = s.Histograms[from:to]
	}
	return Series{
		Labels:          s.Labels,
		Timestamps:      s.Timestamps[from:to],
		StartTimestamps: starts,
		Kinds:           s.Kinds[from:to],
		Values:          s.Values[from:to],
		Histograms:      histograms,
	}
}

// headToSeries converts a buffered series to the block representation.
func headToSeries(hs *headSeries) Series {
	return Series{
		Labels:          hs.labels,
		Timestamps:      hs.ts,
		StartTimestamps: hs.st,
		Kinds:           hs.kind,
		Values:          hs.v,
		Histograms:      hs.hist,
	}
}

// seriesSet is a series set over already-selected series.
type seriesSet struct {
	series []Series
	reads  *readStats
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

func (s *seriesSet) At() storage.Series { return &series{s: s.series[s.i-1], reads: s.reads} }

func (s *seriesSet) Err() error { return s.err }

func (*seriesSet) Warnings() annotations.Annotations { return nil }

// series is a storage.Series backed by one block row.
type series struct {
	s     Series
	reads *readStats
}

func (s *series) Labels() labels.Labels { return s.s.Labels }

func (s *series) Iterator(_ chunkenc.Iterator) chunkenc.Iterator {
	// pos must start before the first sample, otherwise Next would skip it and a
	// single-sample series would yield nothing at all.
	return &sampleIterator{series: s.s, reads: s.reads, pos: -1}
}

// sampleIterator iterates the samples of a block row.
type sampleIterator struct {
	series Series
	reads  *readStats
	batch  storage.SampleBatch
	err    error
	// pos is the index of the current sample, -1 before the first Next.
	pos int
}

// NextBatch exposes borrowed typed arrays without changing scalar iterator
// progress. Its last sample is also the ordinary scalar current sample.
func (it *sampleIterator) NextBatch(maxSamples int) bool {
	return it.NextBatchUntil(maxSamples, math.MaxInt64)
}

// NextBatchUntil leaves future samples available for subsequent range steps.
func (it *sampleIterator) NextBatchUntil(maxSamples int, maxt int64) bool {
	start := max(it.pos+1, 0)
	if it.err != nil || start >= it.series.Len() {
		it.batch = storage.SampleBatch{}
		return false
	}
	end := it.series.Len()
	if maxt != math.MaxInt64 {
		end = start + sort.Search(it.series.Len()-start, func(i int) bool {
			return it.series.Timestamps[start+i] > maxt
		})
	}
	if end == start {
		it.batch = storage.SampleBatch{}
		return false
	}
	if maxSamples > 0 {
		end = min(end, start+maxSamples)
	}
	batch := storage.SampleBatch{
		Timestamps: it.series.Timestamps[start:end], Values: it.series.Values[start:end],
	}
	if len(it.series.StartTimestamps) > 0 {
		batch.StartTimestamps = it.series.StartTimestamps[start:end]
	}
	count := end - start
	staleNormalized := false
	for i := range it.series.Kinds[start:end] {
		switch kind := it.series.Kinds[start+i]; kind {
		case SampleStale:
			// Direct block construction predates the stricter batch stale check.
			// Preserve scalar At's canonical stale marker in that case.
			if !staleNormalized {
				staleNormalized = true
				batch.Values = slices.Clone(batch.Values)
				for j, kind := range it.series.Kinds[start:end] {
					if kind == SampleStale {
						batch.Values[j] = math.Float64frombits(value.StaleNaN)
					}
				}
			}
		case SampleHistogram, SampleFloatHistogram:
			h, fh, err := it.series.HistogramAt(start + i)
			if err != nil {
				it.err = err
				it.batch = storage.SampleBatch{}
				it.pos = it.series.Len()
				return false
			}
			if h != nil {
				if batch.Histograms == nil {
					batch.Histograms = make([]*histogram.Histogram, count)
				}
				batch.Histograms[i] = h
			} else if fh != nil {
				if batch.FloatHistograms == nil {
					batch.FloatHistograms = make([]*histogram.FloatHistogram, count)
				}
				batch.FloatHistograms[i] = fh
			}
		case SampleFloat:
		default:
			it.err = fmt.Errorf("columnar: invalid sample kind %d", kind)
			it.batch = storage.SampleBatch{}
			it.pos = it.series.Len()
			return false
		}
	}
	it.batch = batch
	it.pos = end - 1
	if it.reads != nil {
		it.reads.sampleBatches.Inc()
	}
	return true
}

func (it *sampleIterator) Batch() storage.SampleBatch { return it.batch }

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

func (it *sampleIterator) Err() error { return it.err }

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
	_ storage.Querier                   = (*resourceQuerier)(nil)
	_ storage.ResourceQuerier           = (*resourceQuerier)(nil)
	_ storage.ResourceMembershipQuerier = (*resourceQuerier)(nil)
	_ storage.Querier                   = (*memQuerier)(nil)
	_ storage.Querier                   = (*blockQuerier)(nil)
	_ storage.ChunkQuerier              = chunkQuerier{}
	_ storage.SeriesSet                 = (*seriesSet)(nil)
	_ storage.Series                    = (*series)(nil)
	_ chunkenc.Iterator                 = (*sampleIterator)(nil)
	_ storage.BatchIterator             = (*sampleIterator)(nil)
	_ storage.RangeBatchIterator        = (*sampleIterator)(nil)
	_ storage.ChunkSeriesSet            = chunkSeriesSet{}
	_ storage.ChunkSeries               = chunkSeries{}
	_ chunks.Iterator                   = (*chunkIterator)(nil)
)

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

package columnar_test

// Stage 3 vertical slice: the smallest in-memory adapter that lets the real
// PromQL engine read tile representations, plus controls.
//
// The adapter is deliberately tiny and lives in this test file: it implements
// storage.Queryable by running the declared tile selection, decoding the selected
// lanes on demand inside the iterator, and merging the two sources with the
// ordinary storage.NewMergeQuerier chain. There is no second query engine, no
// persisted catalog and no production wiring.
//
// Declared state, and its limits:
//   - The tile payloads are immutable in-memory blobs. The control arms read
//     blocks from disk with a warm OS page cache (format 10 issues one read per
//     series segment; TSDB reads its chunks similarly), so the tile arms pay no
//     syscall for their payload. That flatters the tile arms and is stated rather
//     than corrected: the structural claim is the read-call reduction (157 tile
//     reads against 10,000 segment reads at 10k series), not the absolute number.
//   - Nothing is preselected, predecoded or precomputed before the timer. The
//     timed operation performs the tile selection, the decoding and the source
//     merge, and the counters below prove that path ran.
//   - No decoded-page cache is kept across queries: each query gets a fresh
//     selection, so a repeated iteration redoes the work.

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/util/annotations"
)

// tileQueryCounters is the physical-path evidence for the vertical slice.
type tileQueryCounters struct {
	queries      atomic.Int64
	selections   atomic.Int64
	tilesPruned  atomic.Int64
	tilesTouched atomic.Int64
	samples      atomic.Int64
	series       atomic.Int64
	readBytes    atomic.Int64
}

func (c *tileQueryCounters) addBytes(delta int64) { c.readBytes.Add(delta) }

// tileSource is one time-disjoint tile source (a stand-in for a sealed part).
type tileSource struct {
	set  *columnar.TileSet
	mint int64
	maxt int64
}

// tileQueryable adapts a set of tile sources to storage.Queryable.
type tileQueryable struct {
	sources  []*tileSource
	counters *tileQueryCounters
}

func (q *tileQueryable) Querier(mint, maxt int64) (storage.Querier, error) {
	q.counters.queries.Add(1)
	queriers := make([]storage.Querier, 0, len(q.sources))
	for _, source := range q.sources {
		if source.maxt < mint || source.mint > maxt {
			continue
		}
		queriers = append(queriers, &tileQuerier{source: source, mint: mint, maxt: maxt, counters: q.counters})
	}
	switch len(queriers) {
	case 0:
		return storage.NoopQuerier(), nil
	case 1:
		return queriers[0], nil
	default:
		return storage.NewMergeQuerier(queriers, nil, storage.ChainedSeriesMerge), nil
	}
}

// tileQuerier selects tile lanes for one source.
type tileQuerier struct {
	source   *tileSource
	mint     int64
	maxt     int64
	counters *tileQueryCounters
}

func (*tileQuerier) Close() error { return nil }

func (q *tileQuerier) LabelValues(context.Context, string, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (q *tileQuerier) LabelNames(context.Context, *storage.LabelHints, ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	return nil, nil, nil
}

func (q *tileQuerier) Select(_ context.Context, _ bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	mint, maxt := q.mint, q.maxt
	if hints != nil {
		if hints.Start > mint {
			mint = hints.Start
		}
		if hints.End < maxt {
			maxt = hints.End
		}
	}
	sel, err := q.source.set.Select(mint, maxt, matchers...)
	if err != nil {
		return storage.ErrSeriesSet(err)
	}
	return &tileSeriesSet{sel: sel, counters: q.counters}
}

// tileSeriesSet implements storage.SeriesSet over a tile selection. Each Series
// decodes its samples when the engine asks for an iterator, so selection alone
// cannot be mistaken for decoding.
type tileSeriesSet struct {
	sel      *columnar.TileSelection
	counters *tileQueryCounters
	i        int
	decoded  int
	samples  int

	publishedSelection bool
	prevTiles          int64
	prevBytes          int64
	prevSamples        int64
	prevSeries         int64
}

// publish reports the physical work done so far. Selection-level counters are
// published once because they are final when Select returns; byte and sample
// counters are published as deltas because decoding continues per lane.
func (s *tileSeriesSet) publish() {
	stats := s.sel.Stats()
	if !s.publishedSelection {
		s.publishedSelection = true
		s.counters.selections.Add(1)
		s.counters.tilesPruned.Add(int64(stats.TilesPrunedByTime + stats.TilesPrunedByLanes))
	}
	// Tiles touched grows while lanes decode, so it is published as a delta too.
	s.counters.tilesTouched.Add(int64(stats.TilesTouched) - s.prevTiles)
	s.prevTiles = int64(stats.TilesTouched)
	bytes := int64(stats.PayloadBytesTouched + stats.DirBytesTouched)
	s.counters.addBytes(bytes - s.prevBytes)
	s.prevBytes = bytes

	s.counters.samples.Add(int64(s.samples) - s.prevSamples)
	s.prevSamples = int64(s.samples)
	s.counters.series.Add(int64(s.decoded) - s.prevSeries)
	s.prevSeries = int64(s.decoded)
}

func (s *tileSeriesSet) Next() bool {
	if s.i >= s.sel.Len() {
		return false
	}
	s.i++
	return true
}

func (s *tileSeriesSet) At() storage.Series {
	return &tileSeries{sel: s.sel, index: s.i - 1, counters: s.counters, set: s}
}

func (s *tileSeriesSet) Err() error { return nil }

func (*tileSeriesSet) Warnings() annotations.Annotations { return nil }

// tileSeries is one selected lane. Its samples are decoded on first use.
type tileSeries struct {
	sel      *columnar.TileSelection
	index    int
	counters *tileQueryCounters
	set      *tileSeriesSet
	decoded  bool
	samples  columnar.Series
}

func (s *tileSeries) Labels() labels.Labels { return s.sel.Labels(s.index) }

func (s *tileSeries) Iterator(_ chunkenc.Iterator) chunkenc.Iterator {
	var dst columnar.Series
	if err := s.sel.Decode(s.index, &dst); err != nil {
		return &tileIterator{err: err}
	}
	if !s.decoded {
		s.decoded = true
		s.set.decoded++
		s.set.samples += dst.Len()
		s.set.publish()
	}
	s.samples = dst
	return &tileIterator{samples: dst, pos: -1}
}

// tileIterator is the minimal chunkenc.Iterator over decoded tile samples.
type tileIterator struct {
	samples columnar.Series
	pos     int
	err     error
}

func (it *tileIterator) Next() chunkenc.ValueType {
	if it.err != nil {
		return chunkenc.ValNone
	}
	if it.pos+1 >= it.samples.Len() {
		it.pos = it.samples.Len()
		return chunkenc.ValNone
	}
	it.pos++
	return it.valueType()
}

func (it *tileIterator) Seek(t int64) chunkenc.ValueType {
	if it.err != nil {
		return chunkenc.ValNone
	}
	if it.pos < 0 {
		it.pos = 0
	}
	for it.pos < it.samples.Len() && it.samples.Timestamps[it.pos] < t {
		it.pos++
	}
	if it.pos >= it.samples.Len() {
		return chunkenc.ValNone
	}
	return it.valueType()
}

func (it *tileIterator) valueType() chunkenc.ValueType {
	switch it.samples.Kinds[it.pos] {
	case columnar.SampleHistogram:
		return chunkenc.ValHistogram
	case columnar.SampleFloatHistogram:
		return chunkenc.ValFloatHistogram
	default:
		return chunkenc.ValFloat
	}
}

func (it *tileIterator) At() (int64, float64) {
	return it.samples.Timestamps[it.pos], it.samples.Values[it.pos]
}

func (it *tileIterator) AtHistogram(reuse *histogram.Histogram) (int64, *histogram.Histogram) {
	h, _, err := it.samples.HistogramAt(it.pos)
	if err != nil || h == nil {
		return it.samples.Timestamps[it.pos], nil
	}
	if reuse != nil {
		*reuse = *h
		return it.samples.Timestamps[it.pos], reuse
	}
	return it.samples.Timestamps[it.pos], h
}

func (it *tileIterator) AtFloatHistogram(reuse *histogram.FloatHistogram) (int64, *histogram.FloatHistogram) {
	h, fh, err := it.samples.HistogramAt(it.pos)
	if err != nil {
		return it.samples.Timestamps[it.pos], nil
	}
	if fh == nil && h != nil {
		fh = h.ToFloat(nil)
	}
	if reuse != nil && fh != nil {
		*reuse = *fh
		return it.samples.Timestamps[it.pos], reuse
	}
	return it.samples.Timestamps[it.pos], fh
}

func (it *tileIterator) AtT() int64 {
	if it.pos < 0 || it.pos >= it.samples.Len() {
		return 0
	}
	return it.samples.Timestamps[it.pos]
}

func (it *tileIterator) AtST() int64 {
	if it.pos < 0 || it.pos >= len(it.samples.StartTimestamps) {
		return 0
	}
	return it.samples.StartTimestamps[it.pos]
}

func (it *tileIterator) Err() error { return it.err }

// tileSources builds two time-disjoint sources from the fixture, mirroring the
// head+sealed overlap shape: the same series, split at the same timestamp.
func tileSources(tb testing.TB, f *fixture, cfg columnar.TileConfig) []*tileSource {
	tb.Helper()
	split := f.nSamples / 2
	build := func(from, to int) *tileSource {
		series := make([]columnar.Series, len(f.labels))
		var mint, maxt int64
		for i := range f.labels {
			n := to - from
			item := columnar.Series{
				Labels: f.labels[i], Timestamps: make([]int64, n), Kinds: make([]columnar.SampleKind, n),
				Values: make([]float64, n),
			}
			for j := range n {
				sample := f.samples[i][from+j]
				item.Timestamps[j] = sample.T()
				item.Values[j] = sample.F()
				item.Kinds[j] = columnar.SampleFloat
			}
			series[i] = item
			mint = item.Timestamps[0]
			maxt = item.Timestamps[n-1]
		}
		set, err := columnar.BuildTileSet(cfg, series)
		require.NoError(tb, err)
		return &tileSource{set: set, mint: mint, maxt: maxt}
	}
	return []*tileSource{build(0, split), build(split, f.nSamples)}
}

// tileEngineExpressions are the representative targets: a range function where
// lookback and resets matter, and a grouped aggregation over the same input.
var tileEngineExpressions = []struct {
	name string
	expr string
}{
	{"wide_rate", `rate(node_cpu_seconds_total[1m])`},
	{"wide_sum_rate", `sum by (mode) (rate(node_cpu_seconds_total[1m]))`},
	{"sel64_raw", `node_cpu_seconds_total{instance="host-1:9100"}`},
}

// TestTilePromQLEquivalence compares complete PromQL results across the tile
// layouts and both controls, outside any timed loop, and asserts the tile path
// was actually exercised.
func TestTilePromQLEquivalence(t *testing.T) {
	series := envInt(t, "STAGE0_SERIES", stage0TestSeries)
	perSeries := envInt(t, "STAGE0_SAMPLES", stage0DefaultSamples)
	f := newFixture(series, perSeries)

	columnarEngine := buildColumnarCorpus(t, f, corpusPlan{"head+sealed", 1, 1})
	tsdbEngine := buildTSDBCorpus(t, f, corpusPlan{"head+sealed", 1, 1})
	defer columnarEngine.close()
	defer tsdbEngine.close()

	for _, target := range tileEngineExpressions {
		expr := target.expr
		wantTSDB := canonicalPromQL(t, tsdbEngine.queryable, f, expr)
		wantColumnar := canonicalPromQL(t, columnarEngine.queryable, f, expr)
		require.NotEmpty(t, wantTSDB, expr)
		require.Equal(t, wantTSDB, wantColumnar, "%s: columnar control disagrees with TSDB", expr)

		for layout := columnar.TileLayout(0); layout < 3; layout++ {
			cfg := columnar.TileConfig{Layout: layout, MaxSeries: 64, MaxTimestamps: 64, IntervalMs: scrapeIntervalMs}
			counters := &tileQueryCounters{}
			queryable := &tileQueryable{sources: tileSources(t, f, cfg), counters: counters}

			got := canonicalPromQL(t, queryable, f, expr)
			require.Equal(t, wantTSDB, got, "layout=%d %s: complete PromQL result", layout, expr)

			require.NotZero(t, counters.selections.Load(), "layout=%d %s: no tile selection ran", layout, expr)
			require.NotZero(t, counters.samples.Load(), "layout=%d %s: no samples decoded", layout, expr)
			require.NotZero(t, counters.readBytes.Load(), "layout=%d %s: no payload bytes read", layout, expr)
			t.Logf("layout=%d %s selections=%d tilesTouched=%d samples=%d series=%d readB=%d",
				layout, expr, counters.selections.Load(), counters.tilesTouched.Load(),
				counters.samples.Load(), counters.series.Load(), counters.readBytes.Load())
		}
	}
}

// BenchmarkTilePromQL measures one representative target end to end through the
// real engine, for every tile layout and both controls, on the same corpus and
// source layout.
func BenchmarkTilePromQL(b *testing.B) {
	series := envInt(b, "STAGE0_SERIES", stage0DefaultSeries)
	perSeries := envInt(b, "STAGE0_SAMPLES", stage0DefaultSamples)
	f := newFixture(series, perSeries)

	columnarEngine := buildColumnarCorpus(b, f, corpusPlan{"head+sealed", 1, 1})
	tsdbEngine := buildTSDBCorpus(b, f, corpusPlan{"head+sealed", 1, 1})
	defer columnarEngine.close()
	defer tsdbEngine.close()

	ran := 0
	run := func(name string, queryable storage.Queryable, expr string) {
		ran++
		b.Run(name, func(b *testing.B) {
			engine := promqltest.NewTestEngineWithOpts(b, stage0EngineOpts())
			start := time.UnixMilli(f.mint)
			end := time.UnixMilli(f.maxt)
			// One untimed execution establishes the result shape and proves the arm works.
			query, err := engine.NewRangeQuery(context.Background(), queryable, nil, expr, start, end, 15*time.Second)
			require.NoError(b, err)
			result := query.Exec(context.Background())
			require.NoError(b, result.Err)
			matrix, ok := result.Value.(promql.Matrix)
			require.True(b, ok)
			require.NotEmpty(b, matrix)
			seriesCount, sampleCount := 0, 0
			for _, item := range matrix {
				seriesCount++
				sampleCount += len(item.Floats) + len(item.Histograms)
			}
			query.Close()

			runtime.GC()
			b.ReportAllocs()
			b.ResetTimer()
			var sink int
			for range b.N {
				query, err := engine.NewRangeQuery(context.Background(), queryable, nil, expr, start, end, 15*time.Second)
				require.NoError(b, err)
				result := query.Exec(context.Background())
				require.NoError(b, result.Err)
				sink += len(result.Value.(promql.Matrix))
				query.Close()
			}
			b.StopTimer()
			require.NotZero(b, sink)
			b.ReportMetric(float64(seriesCount), "result-series")
			b.ReportMetric(float64(sampleCount), "result-points")
		})
	}

	// TILE_EXPRS selects target names for a focused session; it defaults to all.
	wanted := strings.Split(os.Getenv("TILE_EXPRS"), ",")
	selected := func(name string) bool {
		if os.Getenv("TILE_EXPRS") == "" {
			return true
		}
		for _, want := range wanted {
			if strings.TrimSpace(want) == name {
				return true
			}
		}
		return false
	}
	for _, target := range tileEngineExpressions {
		expr, short := target.expr, target.name
		if !selected(short) {
			continue
		}
		run(fmt.Sprintf("engine=tsdb/expr=%s", short), tsdbEngine.queryable, expr)
		run(fmt.Sprintf("engine=current-columnar/expr=%s", short), columnarEngine.queryable, expr)
		for layout := columnar.TileLayout(0); layout < 3; layout++ {
			cfg := columnar.TileConfig{Layout: layout, MaxSeries: 64, MaxTimestamps: 64, IntervalMs: scrapeIntervalMs}
			counters := &tileQueryCounters{}
			queryable := &tileQueryable{sources: tileSources(b, f, cfg), counters: counters}
			run(fmt.Sprintf("layout=%s/expr=%s", tileLayoutNames[layout], short), queryable, expr)
			b.Logf("layout=%s expr=%s counters: selections=%d tilesTouched=%d samples=%d readB=%d",
				tileLayoutNames[layout], short, counters.selections.Load(), counters.tilesTouched.Load(),
				counters.samples.Load(), counters.readBytes.Load())
		}
	}
	require.NotZero(b, ran, "TILE_EXPRS=%q selected no target", os.Getenv("TILE_EXPRS"))
}

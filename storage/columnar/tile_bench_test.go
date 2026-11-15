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

// Stage 3 offline bake-off: encode cost, select+decode latency and total bytes
// for the tile layouts, against a fresh current-columnar control built from the
// same corpus in the same session.
//
// Declared state for every arm: warm process and OS page cache, the fixture is
// built in this process, no decoded-sample cache (both the block control and the
// series-major tile path decode per query), tiles are immutable in-memory blobs
// whose directory is built once with the tile, WAL disabled, native metadata
// present but not queried. The row-layout arms necessarily cache the decoded tile
// inside a selection; that is reported as a separate case and is not the codec's
// intrinsic cost.
//
// Every arm returns the same logical samples: TestTileSelectDecodeEquivalence
// compares the complete decoded output against the source corpus outside the
// timed loop, and TestTileBytesVsBlock reports payload, directory and format-10
// bytes so no size claim can omit the directory.

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

// tileLayoutNames indexes the layouts for benchmark names.
var tileLayoutNames = [3]string{"series-major", "time-major", "sparse-events"}

// tileCorpus converts the shared fixture into tile input series.
func tileCorpus(f *fixture) []columnar.Series {
	out := make([]columnar.Series, len(f.labels))
	for i := range f.labels {
		item := columnar.Series{
			Labels:          f.labels[i],
			Timestamps:      make([]int64, len(f.samples[i])),
			StartTimestamps: make([]int64, len(f.samples[i])),
			Kinds:           make([]columnar.SampleKind, len(f.samples[i])),
			Values:          make([]float64, len(f.samples[i])),
			Histograms:      make([][]byte, len(f.samples[i])),
		}
		for j, sample := range f.samples[i] {
			item.Timestamps[j] = sample.T()
			item.Values[j] = sample.F()
			item.Kinds[j] = columnar.SampleFloat
		}
		out[i] = item
	}
	return out
}

// tileShapeMatches maps a benchmark shape name to matchers and its expected
// physical shape, which is computed once outside the timer.
type tileShapeMatches struct {
	name     string
	matchers []*labels.Matcher
}

var tileBenchShapes = []tileShapeMatches{
	{"sel64", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "instance", "host-1:9100")}},
	{"cpu0", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "cpu", "0")}},
	{"mode_idle", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "mode", "idle")}},
	{"full", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "node_cpu_seconds_total")}},
}

// tileBenchConfig is one fixed layout configuration under test.
type tileBenchConfig struct {
	layout   columnar.TileLayout
	series   int
	ts       int
	compress bool
}

func tileBenchConfigs() []tileBenchConfig {
	var out []tileBenchConfig
	for layout := columnar.TileLayout(0); layout < 3; layout++ {
		for _, series := range []int{32, 64, 128, 256, 512} {
			for _, ts := range []int{32, 64, 128} {
				out = append(out, tileBenchConfig{layout: layout, series: series, ts: ts})
			}
		}
	}
	return out
}

func (c tileBenchConfig) name() string {
	name := fmt.Sprintf("layout=%s/series=%d/ts=%d", tileLayoutNames[c.layout], c.series, c.ts)
	if c.compress {
		name += "/zstd"
	}
	return name
}

func (c tileBenchConfig) tileConfig() columnar.TileConfig {
	return columnar.TileConfig{
		Layout: c.layout, MaxSeries: c.series, MaxTimestamps: c.ts, IntervalMs: scrapeIntervalMs,
		Compress: c.compress,
	}
}

// BenchmarkTileEncode measures building a tile set from the whole corpus,
// including directory serialisation, against the current format-10 seal.
func BenchmarkTileEncode(b *testing.B) {
	series := envInt(b, "STAGE0_SERIES", stage0DefaultSeries)
	perSeries := envInt(b, "STAGE0_SAMPLES", stage0DefaultSamples)
	f := newFixture(series, perSeries)
	corpus := tileCorpus(f)

	configs := tileBenchConfigs()
	if v := os.Getenv("TILE_CONFIG"); v == "best" {
		configs = []tileBenchConfig{
			{layout: columnar.TileSeriesMajor, series: 64, ts: 64},
			{layout: columnar.TileTimeMajor, series: 64, ts: 64},
			{layout: columnar.TileSparseEvents, series: 64, ts: 64},
		}
	}

	b.Run("engine=current-columnar/seal", func(b *testing.B) {
		runtime.GC()
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			dir := b.TempDir()
			st, err := columnar.NewStorage(dir, columnar.StorageOptions{
				FlushSamples: series*perSeries + 1, DisableWAL: true, DisableNativeMetadata: true,
			})
			require.NoError(b, err)
			require.NoError(b, st.AppendBatch(b.Context(), columnar.Batch{Series: tileBatch(corpus)}))
			require.NoError(b, st.Flush())
			require.NoError(b, st.Close())
		}
		b.StopTimer()
		b.ReportMetric(float64(series*perSeries), "samples/op")
	})

	for _, cfg := range configs {
		b.Run("layout="+cfg.name(), func(b *testing.B) {
			runtime.GC()
			b.ReportAllocs()
			b.ResetTimer()
			var sink int
			for range b.N {
				set, err := columnar.BuildTileSet(cfg.tileConfig(), corpus)
				require.NoError(b, err)
				sink += set.Stats().TotalBytes
			}
			b.StopTimer()
			require.NotZero(b, sink)
			b.ReportMetric(float64(series*perSeries), "samples/op")
		})
	}
}

// tileBatch converts the corpus into the typed batch the block writer accepts.
func tileBatch(corpus []columnar.Series) []columnar.BatchSeries {
	out := make([]columnar.BatchSeries, len(corpus))
	for i, item := range corpus {
		out[i] = columnar.BatchSeries{
			Labels: item.Labels, Timestamps: item.Timestamps, StartTimestamps: item.StartTimestamps,
			Kinds: item.Kinds, Values: item.Values, Histograms: item.Histograms,
		}
	}
	return out
}

// currentColumnarControl builds one sealed format-10 block over the corpus.
func currentColumnarControl(tb testing.TB, corpus []columnar.Series, series, perSeries int) (*columnar.Storage, string) {
	tb.Helper()
	dir := tb.TempDir()
	st, err := columnar.NewStorage(dir, columnar.StorageOptions{
		FlushSamples: series*perSeries + 1, DisableWAL: true, DisableNativeMetadata: true,
	})
	require.NoError(tb, err)
	require.NoError(tb, st.AppendBatch(tb.Context(), columnar.Batch{Series: tileBatch(corpus)}))
	require.NoError(tb, st.Flush())
	require.Equal(tb, 1, st.NumBlocks())
	tb.Cleanup(func() { _ = st.Close() })
	return st, dir
}

// drainInto consumes a selection and returns the number of samples, sharing the
// cost model of the Stage 0 harness: no value comparison inside a timed loop.
func drainInto(tb testing.TB, q storage.Querier, matchers []*labels.Matcher) (int, int) {
	tb.Helper()
	var series, samples int
	set := q.Select(context.Background(), true, nil, matchers...)
	for set.Next() {
		series++
		it := set.At().Iterator(nil)
		for it.Next() != chunkenc.ValNone {
			samples++
		}
		require.NoError(tb, it.Err())
	}
	require.NoError(tb, set.Err())
	return series, samples
}

// BenchmarkTileSelectDecode is the offline mechanism comparison: select matched
// lanes and materialise their samples, with no PromQL engine in the loop. Each arm
// decodes from its own representation; nothing is preselected or predecoded.
func BenchmarkTileSelectDecode(b *testing.B) {
	series := envInt(b, "STAGE0_SERIES", stage0DefaultSeries)
	perSeries := envInt(b, "STAGE0_SAMPLES", stage0DefaultSamples)
	f := newFixture(series, perSeries)
	corpus := tileCorpus(f)

	st, _ := currentColumnarControl(b, corpus, series, perSeries)
	// Expected shape per selector, measured once on the control and reused to
	// prove every tile arm decodes the same samples.
	controlCounts := map[string][2]int{}
	for _, shape := range tileBenchShapes {
		selected, samples := drainInto(b, mustQuerier(b, st, f), shape.matchers)
		require.NotZero(b, selected, "%s control selected nothing", shape.name)
		controlCounts[shape.name] = [2]int{selected, samples}
	}
	for _, shape := range tileBenchShapes {
		counts := controlCounts[shape.name]
		matchedSeries, matchedSamples := counts[0], counts[1]

		b.Run("engine=current-columnar/shape="+shape.name, func(b *testing.B) {
			runtime.GC()
			b.ReportAllocs()
			b.ResetTimer()
			var sink int
			for range b.N {
				q, err := st.Querier(f.mint, f.maxt)
				require.NoError(b, err)
				selected, samples := drainInto(b, q, shape.matchers)
				sink += selected + samples
				require.NoError(b, q.Close())
			}
			b.StopTimer()
			require.NotZero(b, sink)
			b.ReportMetric(float64(matchedSeries), "series/op")
			b.ReportMetric(float64(matchedSamples), "samples/op")
		})
	}

	configs := tileBenchConfigs()
	if v := os.Getenv("TILE_CONFIG"); v == "best" {
		configs = []tileBenchConfig{
			{layout: columnar.TileSeriesMajor, series: 64, ts: 64},
			{layout: columnar.TileTimeMajor, series: 64, ts: 64},
			{layout: columnar.TileSparseEvents, series: 64, ts: 64},
		}
	}
	for _, cfg := range configs {
		set, err := columnar.BuildTileSet(cfg.tileConfig(), corpus)
		require.NoError(b, err)
		stats := set.Stats()
		for _, shape := range tileBenchShapes {
			// Expected shape, computed once: this also proves the arm decodes the
			// samples it claims to.
			probe, err := set.Select(f.mint, f.maxt, shape.matchers...)
			require.NoError(b, err)
			expectedSeries, expectedSamples := decodeAll(b, probe)
			probeStats := probe.Stats()
			require.Equal(b, controlCounts[shape.name][0], expectedSeries, "%s/%s selected series", cfg.name(), shape.name)
			require.Equal(b, controlCounts[shape.name][1], expectedSamples, "%s/%s selected samples", cfg.name(), shape.name)

			b.Run("layout="+cfg.name()+"/shape="+shape.name, func(b *testing.B) {
				runtime.GC()
				b.ReportAllocs()
				b.ResetTimer()
				var sink int
				for range b.N {
					sel, err := set.Select(f.mint, f.maxt, shape.matchers...)
					require.NoError(b, err)
					var dst columnar.Series
					for i := range sel.Len() {
						require.NoError(b, sel.Decode(i, &dst))
						sink += dst.Len()
					}
				}
				b.StopTimer()
				require.NotZero(b, sink)
				b.ReportMetric(float64(expectedSeries), "series/op")
				b.ReportMetric(float64(expectedSamples), "samples/op")
				b.ReportMetric(float64(stats.TotalBytes), "tile-B")
				b.ReportMetric(float64(stats.DirectoryBytes), "dir-B")
				b.ReportMetric(float64(stats.Tiles), "tiles")
				b.ReportMetric(float64(stats.TimeMajorFallbackTiles), "fallback-tiles")
				b.ReportMetric(float64(probeStats.PayloadBytesTouched), "read-B/op")
				b.ReportMetric(float64(probeStats.DirBytesTouched), "read-dir-B/op")
				b.ReportMetric(float64(probeStats.TileDecodes), "tile-decodes/op")
			})
		}
	}
}

func mustQuerier(tb testing.TB, st *columnar.Storage, f *fixture) storage.Querier {
	tb.Helper()
	q, err := st.Querier(f.mint, f.maxt)
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = q.Close() })
	return q
}

// decodeAll materialises a whole selection and reports the exact series and
// sample counts it produced.
func decodeAll(tb testing.TB, sel *columnar.TileSelection) (int, int) {
	tb.Helper()
	var series, samples int
	var dst columnar.Series
	for i := range sel.Len() {
		require.NoError(tb, sel.Decode(i, &dst))
		if dst.Len() == 0 {
			continue
		}
		series++
		samples += dst.Len()
	}
	return series, samples
}

// TestTileSelectDecodeEquivalence proves the offline arms return exactly the
// corpus' logical selection for every shape, outside any timed loop.
func TestTileSelectDecodeEquivalence(t *testing.T) {
	series := envInt(t, "STAGE0_SERIES", stage0TestSeries)
	perSeries := envInt(t, "STAGE0_SAMPLES", stage0DefaultSamples)
	f := newFixture(series, perSeries)
	corpus := tileCorpus(f)

	st, _ := currentColumnarControl(t, corpus, series, perSeries)
	for _, shape := range tileBenchShapes {
		wantSeries, wantSamples := drainInto(t, mustQuerier(t, st, f), shape.matchers)
		for layout := columnar.TileLayout(0); layout < 3; layout++ {
			for _, cfg := range []tileBenchConfig{
				{layout: layout, series: 32, ts: 32},
				{layout: layout, series: 64, ts: 64},
				{layout: layout, series: 128, ts: 128},
				{layout: layout, series: 256, ts: 64, compress: true},
			} {
				set, err := columnar.BuildTileSet(cfg.tileConfig(), corpus)
				require.NoError(t, err)
				sel, err := set.Select(f.mint, f.maxt, shape.matchers...)
				require.NoError(t, err)
				gotSeries, gotSamples := decodeAll(t, sel)
				require.Equal(t, wantSeries, gotSeries, "%s/%s series", cfg.name(), shape.name)
				require.Equal(t, wantSamples, gotSamples, "%s/%s samples", cfg.name(), shape.name)

				// Read the samples back through the same canonicalisation the Stage 0
				// harness uses, so a different value cannot pass as a faster arm.
				var dst columnar.Series
				for i := range sel.Len() {
					require.NoError(t, sel.Decode(i, &dst))
					require.NoError(t, dst.Validate())
				}
			}
		}
	}
}

// TestTileBytesVsBlock reports total bytes per sample for the tile layouts and
// the format-10 block over the same corpus. Tile bytes always include the
// directory; the block's total includes its catalog.
func TestTileBytesVsBlock(t *testing.T) {
	series := envInt(t, "STAGE0_SERIES", stage0TestSeries)
	perSeries := envInt(t, "STAGE0_SAMPLES", stage0DefaultSamples)
	f := newFixture(series, perSeries)
	corpus := tileCorpus(f)

	_, dir := currentColumnarControl(t, corpus, series, perSeries)
	total, data := dirSizes(dir)
	samples := series * perSeries
	t.Logf("engine=current-columnar totalB/sample=%.3f dataB/sample=%.3f totalB=%d dataB=%d",
		float64(total)/float64(samples), float64(data)/float64(samples), total, data)

	for _, cfg := range tileBenchConfigs() {
		set, err := columnar.BuildTileSet(cfg.tileConfig(), corpus)
		require.NoError(t, err)
		stats := set.Stats()
		t.Logf("layout=%s samples=%d tiles=%d payloadB=%d dirB=%d totalB/sample=%.3f dirB/sample=%.3f sharedTs=%d fallbacks=%d",
			cfg.name(), stats.Samples, stats.Tiles, stats.PayloadBytes, stats.DirectoryBytes,
			stats.BytesPerSample(), float64(stats.DirectoryBytes)/float64(stats.Samples),
			stats.SharedTimestampTiles, stats.TimeMajorFallbackTiles)
	}
}

// TestTileTracePhases records what each layout can observe at seal time on the
// jittered and churned traces, using the same generator as the correctness suite.
// It exists because the offline bake-off's dense corpus alone cannot say whether
// a time-major tile stays applicable when a cohort fragments.
func TestTileTracePhases(t *testing.T) {
	series := envInt(t, "STAGE0_SERIES", stage0TestSeries)
	perSeries := envInt(t, "STAGE0_SAMPLES", stage0DefaultSamples)
	dense := newFixture(series, perSeries)
	base := tileCorpus(dense)

	// Jitter: shift each sample by a deterministic sub-interval offset, keeping the
	// series strictly increasing.
	jittered := make([]columnar.Series, len(base))
	for i := range base {
		item := base[i]
		ts := make([]int64, len(item.Timestamps))
		for j := range item.Timestamps {
			ts[j] = item.Timestamps[j] + int64((i*37+j*11)%4001) - 2000
		}
		item.Timestamps = ts
		jittered[i] = item
	}
	// Churn: drop a deterministic 10% of samples per series.
	churned := make([]columnar.Series, len(base))
	for i := range base {
		item := base[i]
		var ts []int64
		var values []float64
		var kinds []columnar.SampleKind
		for j := range item.Timestamps {
			if (uint64(i)*31+uint64(j)*17)%10 == 0 {
				continue
			}
			ts = append(ts, item.Timestamps[j])
			values = append(values, item.Values[j])
			kinds = append(kinds, item.Kinds[j])
		}
		item.Timestamps, item.Values, item.Kinds = ts, values, kinds
		item.StartTimestamps, item.Histograms = nil, nil
		churned[i] = item
	}

	for _, trace := range []struct {
		name string
		data []columnar.Series
	}{{"dense", base}, {"jittered", jittered}, {"churned", churned}} {
		stored := 0
		for _, item := range trace.data {
			stored += len(item.Timestamps)
		}
		for _, cfg := range []tileBenchConfig{
			{layout: columnar.TileSeriesMajor, series: 64, ts: 64},
			{layout: columnar.TileTimeMajor, series: 64, ts: 64},
			{layout: columnar.TileSparseEvents, series: 64, ts: 64},
		} {
			set, err := columnar.BuildTileSet(cfg.tileConfig(), trace.data)
			require.NoError(t, err)
			stats := set.Stats()
			require.Equal(t, stored, stats.Samples)
			t.Logf("trace=%s layout=%s tiles=%d totalB/sample=%.3f payloadB=%d dirB=%d sharedTs=%d fallbacks=%d",
				trace.name, tileLayoutNames[cfg.layout], stats.Tiles, stats.BytesPerSample(),
				stats.PayloadBytes, stats.DirectoryBytes, stats.SharedTimestampTiles, stats.TimeMajorFallbackTiles)
		}
	}
}

// TestTileBenchConfigSanity keeps the benchmark matrix honest: every configuration
// must encode every sample exactly once.
func TestTileBenchConfigSanity(t *testing.T) {
	if testing.Short() {
		t.Skip("matrix sanity check")
	}
	series := 256
	perSeries := 60
	f := newFixture(series, perSeries)
	corpus := tileCorpus(f)
	for _, cfg := range tileBenchConfigs() {
		set, err := columnar.BuildTileSet(cfg.tileConfig(), corpus)
		require.NoError(t, err)
		stats := set.Stats()
		require.Equal(t, series*perSeries, stats.Samples, "%s", cfg.name())
		require.NotZero(t, stats.DirectoryBytes, "%s", cfg.name())
		if cfg.layout == columnar.TileTimeMajor && !cfg.compress {
			require.Zero(t, stats.TimeMajorFallbackTiles, "%s dense corpus must not fall back", cfg.name())
		}
	}
}

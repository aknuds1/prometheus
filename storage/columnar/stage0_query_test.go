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

package columnar_test

// Stage 0 query baselines: selector selectivity and source overlap.
//
// Two matrices live here, and they answer different questions.
//
// BenchmarkScalingQuery restores the five-selectivity matrix that 0402d09e4
// removed from BenchmarkScaling, as a separate benchmark so the footprint study
// stays intact. Both engines are driven through storage.Querier from one sealed
// block each with an empty head, so the numbers isolate the sample layout and
// exclude block/source overlap. That is a control, not the deployed shape.
//
// BenchmarkSourceOverlap holds the logical corpus fixed - same series, same
// samples, same selectors, therefore the same result - and varies only how many
// sources hold it. A ratio difference between arms is per-source merge work, not
// more selected data. Head-only, one sealed block, head plus one sealed block and
// multi-source layouts are all built from that one corpus.
//
// Declared state for every timing in this file: process and OS page cache warm
// (the fixture is ingested and queried in the same process), no decoded-sample
// cache (format 10 decodes per query), WAL disabled, TSDB auto-compaction
// disabled so the built source layout is the measured one, and native metadata
// present but not queried. The encoded-read/decode case is the only
// representation case that exists at this HEAD; there is no decoded-page cache
// to declare.
//
// Timed loops drain the iterator and count series/samples only. Result
// comparison, physical counter checks and source accounting run outside the
// timer, in TestStage0QueryEquivalence, TestStage0PhysicalWork and
// TestStage0SourceReport.
//
// Sample-level interleaving across sources is deliberately absent: stock TSDB
// refuses to open a directory with overlapping blocks, so a layout where two
// sources hold alternating timestamps of the same range cannot be built on the
// control arm. Disjoint source ranges with the full series set shared are what
// both engines can represent; duplicate-timestamp merge semantics remain an
// untested gap for the persistence stage.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

const (
	// stage0DefaultSeries sizes the recorded benchmark shape; stage0TestSeries
	// keeps the always-run correctness suite cheap. Both are env-overridable.
	stage0DefaultSeries  = 7_500
	stage0TestSeries     = 500
	stage0DefaultSamples = 60
	// tsdbBlockChunkRangeMs is far above any window span the harness builds. The
	// chunk range also bounds the head's valid-time window during CreateBlock, so
	// a merely sufficient range fails with "out of bounds" once a commit has
	// advanced maxTime.
	tsdbBlockChunkRangeMs = int64(2 * time.Hour / time.Millisecond)
)

// envInt reads a positive integer environment override.
func envInt(tb testing.TB, name string, fallback int) int {
	tb.Helper()
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		tb.Fatalf("%s=%q is not a positive integer", name, v)
	}
	return n
}

// envIntList reads a comma-separated positive integer environment override.
func envIntList(tb testing.TB, name string, fallback []int) []int {
	tb.Helper()
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}
	var out []int
	for _, part := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n <= 0 {
			tb.Fatalf("%s=%q is not a list of positive integers", name, v)
		}
		out = append(out, n)
	}
	return out
}

// corpusPlan splits one logical corpus into query sources. Sample phases are
// divided into sealed+head contiguous windows; each sealed window becomes one
// block and the head windows stay buffered. Sealed windows always precede the
// head because both engines require per-series time order on the append tail.
type corpusPlan struct {
	name   string
	sealed int
	head   int
}

func (p corpusPlan) windows() int { return p.sealed + p.head }

// sources is the number of queriers a full-window query has to merge.
func (p corpusPlan) sources() int {
	if p.head > 0 {
		return p.sealed + 1
	}
	return p.sealed
}

// windowSamples returns the samples per series in one window, or an error when
// the requested layout does not divide the corpus evenly.
func (p corpusPlan) windowSamples(total int) (int, error) {
	if p.windows() <= 0 {
		return 0, fmt.Errorf("plan %q has no windows", p.name)
	}
	if total%p.windows() != 0 {
		return 0, fmt.Errorf("%d samples does not divide into %d %q windows", total, p.windows(), p.name)
	}
	return total / p.windows(), nil
}

var stage0Plans = []corpusPlan{
	{"head-only", 0, 1},
	{"one-sealed", 1, 0},
	{"head+sealed", 1, 1},
	{"4-sealed", 4, 0},
	{"head+3sealed", 3, 1},
}

// stage0PlanSelection returns the plans to build. STAGE0_PLANS selects a subset
// by name so a smoke run does not pay for every layout.
func stage0PlanSelection(tb testing.TB) []corpusPlan {
	tb.Helper()
	sel := os.Getenv("STAGE0_PLANS")
	if sel == "" {
		return stage0Plans
	}
	var out []corpusPlan
	for _, name := range strings.Split(sel, ",") {
		name = strings.TrimSpace(name)
		i := slices.IndexFunc(stage0Plans, func(p corpusPlan) bool { return p.name == name })
		if i < 0 {
			tb.Fatalf("STAGE0_PLANS names unknown plan %q", name)
		}
		out = append(out, stage0Plans[i])
	}
	return out
}

// stage0SourceShapes are the selector families used for the source matrix: one
// highly selective instance matcher, one quarter-selective mode matcher and one
// fully selective metric-name matcher.
var stage0SourceShapes = []struct {
	name     string
	matchers []*labels.Matcher
}{
	{"selective", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "instance", "host-1:9100")}},
	{"mode_idle", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "mode", "idle")}},
	{"wide", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "node_cpu_seconds_total")}},
}

// corpusEngine is one built source layout on one engine.
type corpusEngine struct {
	name      string
	root      string
	queryable storage.Queryable
	// sources is how many queriers a full-window query must merge.
	sources int
	// check re-asserts the built source layout, because TSDB background work or a
	// misplaced flush would otherwise silently change what is being measured.
	check func(tb testing.TB)
	close func()
}

// slogDiscard returns a logger that drops everything: fixture building and
// engine teardown are not what is being measured.
func slogDiscard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// stage0TSDBOptions configures the control arm. WAL is disabled because the
// columnar fixtures run with DisableWAL for the same reason: this measures the
// head and block read paths, not durable write-ahead logging. Retention is
// disabled because the fixture timestamps are fixed and would otherwise be
// deleted as expired during a long session.
func stage0TSDBOptions() *tsdb.Options {
	opts := tsdb.DefaultOptions()
	opts.WALSegmentSize = -1
	opts.RetentionDuration = 0
	opts.NoLockfile = true
	opts.EnableExemplarStorage = false
	return opts
}

// buildColumnarCorpus ingests the corpus in time order and seals exactly the
// plan's leading windows, leaving the trailing windows in the head.
func buildColumnarCorpus(tb testing.TB, f *fixture, plan corpusPlan) *corpusEngine {
	tb.Helper()
	per, err := plan.windowSamples(f.nSamples)
	require.NoError(tb, err)

	dir := tb.TempDir()
	// The commit threshold sits above one window, so only the explicit Flush
	// seals: source boundaries are the plan's, not a sample-count accident.
	st, err := columnar.NewStorage(dir, columnar.StorageOptions{
		FlushSamples: len(f.labels) * per * 2,
		DisableWAL:   true,
	})
	require.NoError(tb, err)

	for w := range plan.windows() {
		app := st.AppenderV2(context.Background())
		for j := w * per; j < (w+1)*per; j++ {
			for i := range f.labels {
				s := f.samples[i][j]
				if _, err := app.Append(0, f.labels[i], 0, s.T(), s.F(), nil, nil, storage.AOptions{}); err != nil {
					tb.Fatalf("columnar append %s at %d: %v", f.labels[i], s.T(), err)
				}
			}
		}
		require.NoError(tb, app.Commit())
		if w < plan.sealed {
			require.NoError(tb, st.Flush())
		}
	}
	require.Equal(tb, plan.sealed, st.NumBlocks(), "columnar blocks for plan %s", plan.name)
	require.Equal(tb, plan.sealed, st.ReadStats().Blocks)

	return &corpusEngine{
		name:      "columnar",
		root:      dir,
		queryable: st,
		sources:   plan.sources(),
		check: func(tb testing.TB) {
			tb.Helper()
			require.Equal(tb, plan.sealed, st.NumBlocks(), "columnar block count changed for plan %s", plan.name)
		},
		close: func() { _ = st.Close() },
	}
}

// buildTSDBCorpus writes one block per sealed window and appends the remaining
// windows through a WAL-disabled head, then disables auto-compaction so the
// block count stays the one under measurement.
func buildTSDBCorpus(tb testing.TB, f *fixture, plan corpusPlan) *corpusEngine {
	tb.Helper()
	per, err := plan.windowSamples(f.nSamples)
	require.NoError(tb, err)

	dir := tb.TempDir()
	logger := slogDiscard()

	for w := range plan.sealed {
		series := make([]storage.Series, len(f.labels))
		for i := range f.labels {
			series[i] = storage.NewListSeries(f.labels[i], f.samples[i][w*per:(w+1)*per])
		}
		blockDir := tb.TempDir()
		if _, err := tsdb.CreateBlock(series, blockDir, tsdbBlockChunkRangeMs, logger); err != nil {
			tb.Fatalf("creating TSDB block for plan %s window %d: %v", plan.name, w, err)
		}
		entries, err := os.ReadDir(blockDir)
		require.NoError(tb, err)
		for _, entry := range entries {
			require.NoError(tb, os.Rename(filepath.Join(blockDir, entry.Name()), filepath.Join(dir, entry.Name())))
		}
	}

	db, err := tsdb.Open(dir, logger, nil, stage0TSDBOptions(), nil)
	require.NoError(tb, err)
	db.DisableCompactions()

	if plan.head > 0 {
		app := db.AppenderV2(context.Background())
		refs := make([]storage.SeriesRef, len(f.labels))
		for j := plan.sealed * per; j < f.nSamples; j++ {
			for i := range f.labels {
				s := f.samples[i][j]
				ref, err := app.Append(refs[i], f.labels[i], 0, s.T(), s.F(), nil, nil, storage.AOptions{})
				if err != nil {
					tb.Fatalf("tsdb append %s at %d: %v", f.labels[i], s.T(), err)
				}
				refs[i] = ref
			}
		}
		require.NoError(tb, app.Commit())
	}

	wantHead := uint64(0)
	if plan.head > 0 {
		wantHead = uint64(len(f.labels))
	}
	require.Equal(tb, plan.sealed, len(db.Blocks()), "tsdb blocks for plan %s", plan.name)
	require.Equal(tb, wantHead, db.Head().NumSeries(), "tsdb head series for plan %s", plan.name)

	return &corpusEngine{
		name:      "tsdb",
		root:      dir,
		queryable: db,
		sources:   plan.sources(),
		check: func(tb testing.TB) {
			tb.Helper()
			require.Equal(tb, plan.sealed, len(db.Blocks()), "tsdb block count changed for plan %s", plan.name)
			require.Equal(tb, wantHead, db.Head().NumSeries(), "tsdb head series changed for plan %s", plan.name)
		},
		close: func() { _ = db.Close() },
	}
}

var stage0Builders = []struct {
	name  string
	build func(tb testing.TB, f *fixture, plan corpusPlan) *corpusEngine
}{
	{"columnar", buildColumnarCorpus},
	{"tsdb", buildTSDBCorpus},
}

// drainSelection fully consumes one selection and counts what came back. It
// performs no value comparison: that belongs to the preflight, which runs with
// the timer stopped.
func drainSelection(tb testing.TB, q storage.Querier, matchers []*labels.Matcher) (series, samples int) {
	tb.Helper()
	set := q.Select(context.Background(), true, nil, matchers...)
	for set.Next() {
		series++
		it := set.At().Iterator(nil)
		for vt := it.Next(); vt != chunkenc.ValNone; vt = it.Next() {
			samples++
		}
		if err := it.Err(); err != nil {
			tb.Fatalf("draining selection: %v", err)
		}
	}
	require.NoError(tb, set.Err())
	return series, samples
}

// countSelection runs one complete selection against a built engine.
func countSelection(tb testing.TB, e *corpusEngine, f *fixture, matchers []*labels.Matcher) (series, samples int) {
	tb.Helper()
	q, err := e.queryable.Querier(f.mint, f.maxt)
	require.NoError(tb, err)
	defer func() { require.NoError(tb, q.Close()) }()
	return drainSelection(tb, q, matchers)
}

// canonicalSelection renders the complete selected samples so two engines can be
// compared exactly: every series in label order, every timestamp and value.
func canonicalSelection(tb testing.TB, e *corpusEngine, f *fixture, matchers []*labels.Matcher) []string {
	tb.Helper()
	q, err := e.queryable.Querier(f.mint, f.maxt)
	require.NoError(tb, err)
	defer func() { require.NoError(tb, q.Close()) }()

	set := q.Select(context.Background(), true, nil, matchers...)
	var out []string
	for set.Next() {
		s := set.At()
		var b strings.Builder
		b.WriteString(s.Labels().String())
		it := s.Iterator(nil)
		for vt := it.Next(); vt != chunkenc.ValNone; vt = it.Next() {
			switch vt {
			case chunkenc.ValFloat:
				ts, v := it.At()
				fmt.Fprintf(&b, " %d=%s", ts, strconv.FormatFloat(v, 'g', -1, 64))
			case chunkenc.ValHistogram:
				ts, h := it.AtHistogram(nil)
				fmt.Fprintf(&b, " %d=%s", ts, h.String())
			case chunkenc.ValFloatHistogram:
				ts, fh := it.AtFloatHistogram(nil)
				fmt.Fprintf(&b, " %d=%s", ts, fh.String())
			default:
				tb.Fatalf("unexpected iterator value type %v", vt)
			}
		}
		require.NoError(tb, it.Err())
		out = append(out, b.String())
	}
	require.NoError(tb, set.Err())
	return out
}

// stage0EngineOpts is the PromQL configuration both arms use. It matches the
// existing query benchmarks so results stay comparable across the harness.
func stage0EngineOpts() promql.EngineOpts {
	return promql.EngineOpts{
		MaxSamples:               50_000_000,
		Timeout:                  5 * time.Minute,
		LookbackDelta:            5 * time.Minute,
		EnableAtModifier:         true,
		EnableNegativeOffset:     true,
		EnableDelayedNameRemoval: true,
		Parser:                   parser.NewParser(promqltest.TestParserOpts),
	}
}

// canonicalPromQL runs one range query through the real engine and renders the
// complete result in a stable order, so the consumer boundary is compared too.
func canonicalPromQL(tb testing.TB, q storage.Queryable, f *fixture, expr string) []string {
	tb.Helper()
	engine := promqltest.NewTestEngineWithOpts(tb, stage0EngineOpts())
	start := time.UnixMilli(f.mint)
	end := time.UnixMilli(f.maxt)
	query, err := engine.NewRangeQuery(context.Background(), q, nil, expr, start, end, 15*time.Second)
	require.NoError(tb, err)
	defer query.Close()
	result := query.Exec(context.Background())
	require.NoError(tb, result.Err)
	matrix, ok := result.Value.(promql.Matrix)
	require.True(tb, ok)

	out := make([]string, 0, len(matrix))
	for _, series := range matrix {
		var b strings.Builder
		b.WriteString(series.Metric.String())
		points := make([]string, 0, len(series.Floats)+len(series.Histograms))
		for _, p := range series.Floats {
			points = append(points, fmt.Sprintf("%d=%s", p.T, strconv.FormatFloat(p.F, 'g', -1, 64)))
		}
		for _, p := range series.Histograms {
			points = append(points, fmt.Sprintf("%d=%s", p.T, p.H.String()))
		}
		sort.Strings(points)
		b.WriteString(" ")
		b.WriteString(strings.Join(points, " "))
		out = append(out, b.String())
	}
	sort.Strings(out)
	return out
}

// BenchmarkScalingQuery measures the five selectivities at fixed cardinality and
// density. Series count and samples per series are constant across the shapes,
// so label cardinality, catalog entries, index postings and sample volume are
// constant; only matcher selectivity changes. Both engines expose exactly one
// sealed block and an empty head.
func BenchmarkScalingQuery(b *testing.B) {
	seriesList := envIntList(b, "SCALING_SERIES", []int{10_000})
	perSeries := envInt(b, "SCALING_PER_SERIES", stage0DefaultSamples)
	plan := corpusPlan{"one-sealed", 1, 0}

	for _, series := range seriesList {
		f := newFixture(series, perSeries)
		for _, builder := range stage0Builders {
			eng := builder.build(b, f, plan)
			eng.check(b)
			for _, shape := range scalingShapes {
				matchedSeries, matchedSamples := countSelection(b, eng, f, shape.matchers)
				require.NotZero(b, matchedSeries, "%s/%s selected nothing", eng.name, shape.name)

				b.Run(fmt.Sprintf("series=%d/engine=%s/%s", series, eng.name, shape.name), func(b *testing.B) {
					// Settle garbage from the fixture build and from earlier arms: a GC
					// cycle inside a one-second window moves a sample by more than the
					// effect being measured.
					runtime.GC()
					b.ReportAllocs()
					b.ResetTimer()
					var sink int
					for range b.N {
						q, err := eng.queryable.Querier(f.mint, f.maxt)
						require.NoError(b, err)
						selected, samples := drainSelection(b, q, shape.matchers)
						sink += selected + samples
						require.NoError(b, q.Close())
					}
					b.StopTimer()
					eng.check(b)
					require.NotZero(b, sink)
					// ReportMetric deletes metrics at ResetTimer on this Go version, so
					// every non-time metric is reported after the timed loop.
					b.ReportMetric(float64(matchedSeries), "series/op")
					b.ReportMetric(float64(matchedSamples), "samples/op")
				})
			}
			eng.close()
		}
	}
}

// BenchmarkSourceOverlap measures the same logical result over one, two and four
// query sources. The corpus is identical across arms, so a difference is source
// count and merge work rather than more selected data.
func BenchmarkSourceOverlap(b *testing.B) {
	series := envInt(b, "STAGE0_SERIES", stage0DefaultSeries)
	perSeries := envInt(b, "STAGE0_SAMPLES", stage0DefaultSamples)
	f := newFixture(series, perSeries)

	for _, plan := range stage0PlanSelection(b) {
		for _, builder := range stage0Builders {
			eng := builder.build(b, f, plan)
			eng.check(b)

			type counts struct{ series, samples int }
			measured := make([]counts, len(stage0SourceShapes))
			for i, shape := range stage0SourceShapes {
				s, n := countSelection(b, eng, f, shape.matchers)
				require.NotZero(b, s, "%s/%s/%s selected nothing", plan.name, eng.name, shape.name)
				measured[i] = counts{s, n}
			}
			total, data := dirSizes(eng.root)

			for i, shape := range stage0SourceShapes {
				b.Run(fmt.Sprintf("plan=%s/engine=%s/shape=%s", plan.name, eng.name, shape.name), func(b *testing.B) {
					runtime.GC()
					b.ReportAllocs()
					b.ResetTimer()
					var sink int
					for range b.N {
						q, err := eng.queryable.Querier(f.mint, f.maxt)
						require.NoError(b, err)
						selected, samples := drainSelection(b, q, shape.matchers)
						sink += selected + samples
						require.NoError(b, q.Close())
					}
					b.StopTimer()
					eng.check(b)
					require.NotZero(b, sink)
					b.ReportMetric(float64(measured[i].series), "series/op")
					b.ReportMetric(float64(measured[i].samples), "samples/op")
					b.ReportMetric(float64(eng.sources), "sources/query")
					b.ReportMetric(float64(total), "store-B")
					b.ReportMetric(float64(data), "data-B")
				})
			}
			eng.close()
		}
	}
}

// TestStage0QueryEquivalence proves both engines return the same complete
// result for every source layout and selector, at the storage boundary and
// through the PromQL engine. Counts alone are not evidence: a faster but
// different computation must not become the baseline.
func TestStage0QueryEquivalence(t *testing.T) {
	series := envInt(t, "STAGE0_SERIES", stage0TestSeries)
	perSeries := envInt(t, "STAGE0_SAMPLES", stage0DefaultSamples)
	f := newFixture(series, perSeries)

	for _, plan := range stage0PlanSelection(t) {
		col := buildColumnarCorpus(t, f, plan)
		control := buildTSDBCorpus(t, f, plan)

		shapes := stage0SourceShapes
		if plan.name == "one-sealed" {
			// The layout-isolation control also covers the five selectivities.
			shapes = append(slices.Clone(stage0SourceShapes), scalingShapes...)
		}
		for _, shape := range shapes {
			want := canonicalSelection(t, control, f, shape.matchers)
			got := canonicalSelection(t, col, f, shape.matchers)
			require.Equal(t, len(want), len(got), "%s/%s: selected series", plan.name, shape.name)
			require.Equal(t, want, got, "%s/%s: complete samples", plan.name, shape.name)
		}

		for _, expr := range []string{
			`rate(node_cpu_seconds_total[1m])`,
			`sum by (mode) (rate(node_cpu_seconds_total[1m]))`,
		} {
			want := canonicalPromQL(t, control.queryable, f, expr)
			got := canonicalPromQL(t, col.queryable, f, expr)
			require.NotEmpty(t, want, "%s/%s produced no result", plan.name, expr)
			require.Equal(t, want, got, "%s/%s: complete PromQL result", plan.name, expr)
		}

		col.close()
		control.close()
	}
}

// TestStage0PhysicalWork is the instrumented preflight: it proves each source
// arm actually reads what its name claims, that the merge path is entered, and
// that an absent value costs no payload read. These counters are the evidence a
// timing result cannot provide; they stay out of the timed loops.
func TestStage0PhysicalWork(t *testing.T) {
	series := envInt(t, "STAGE0_SERIES", stage0TestSeries)
	perSeries := envInt(t, "STAGE0_SAMPLES", stage0DefaultSamples)
	f := newFixture(series, perSeries)

	for _, plan := range stage0PlanSelection(t) {
		eng := buildColumnarCorpus(t, f, plan)
		st := eng.queryable.(*columnar.Storage)

		wide := stage0SourceShapes[2].matchers
		before := st.ReadStats()
		gotSeries, gotSamples := countSelection(t, eng, f, wide)
		after := st.ReadStats()

		require.EqualValues(t, 1, after.Queries-before.Queries, "%s: one query", plan.name)
		require.EqualValues(t, plan.sources(), after.BlocksOverlapping-before.BlocksOverlapping,
			"%s: overlapping sources", plan.name)
		require.Equal(t, series, gotSeries, "%s: selected series", plan.name)
		require.Equal(t, series*perSeries, gotSamples, "%s: selected samples", plan.name)
		// Every source contributes its own matched-series work, so the merge cannot
		// be a silent pass-through of one source.
		require.EqualValues(t, plan.sources()*series, after.SeriesMatched-before.SeriesMatched,
			"%s: per-source matched series", plan.name)

		if plan.sealed > 0 {
			require.EqualValues(t, plan.sealed, after.SelectCalls-before.SelectCalls, "%s: block selects", plan.name)
			require.NotZero(t, after.SegmentsRead-before.SegmentsRead, "%s: segments read", plan.name)
			require.NotZero(t, after.SampleRowsDecoded-before.SampleRowsDecoded, "%s: sample rows", plan.name)
			require.NotZero(t, after.PayloadBytesRead-before.PayloadBytesRead, "%s: payload bytes", plan.name)
		} else {
			require.Zero(t, after.PayloadBytesRead-before.PayloadBytesRead, "%s: head-only reads no payload", plan.name)
		}
		if plan.head > 0 {
			require.NotZero(t, after.HeadSeriesScanned-before.HeadSeriesScanned, "%s: head series scanned", plan.name)
		}

		// An absent metric must cost no payload read on any retained source.
		beforeMiss := st.ReadStats()
		missSeries, _ := countSelection(t, eng, f, []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "definitely_absent_metric"),
		})
		afterMiss := st.ReadStats()
		require.Zero(t, missSeries, "%s: absent metric selected series", plan.name)
		require.Zero(t, afterMiss.PayloadBytesRead-beforeMiss.PayloadBytesRead, "%s: absent metric payload bytes", plan.name)
		require.Zero(t, afterMiss.SeriesMatched-beforeMiss.SeriesMatched, "%s: absent metric matched series", plan.name)

		t.Logf("plan=%s counterOps=%d perWideQuery", plan.name, readCounterOps(after)-readCounterOps(before))
		// The selective shape is where a fixed per-query diagnostic cost is the
		// largest share, so account for it separately.
		beforeSelective := st.ReadStats()
		countSelection(t, eng, f, stage0SourceShapes[0].matchers)
		afterSelective := st.ReadStats()
		t.Logf("plan=%s counterOps=%d perSelectiveQuery", plan.name, readCounterOps(afterSelective)-readCounterOps(beforeSelective))
		t.Logf("plan=%s sources=%d blocks=%d matched=%d samples=%d segments=%d rows=%d payloadB=%d headScanned=%d merge=%t",
			plan.name, plan.sources(), st.NumBlocks(),
			after.SeriesMatched-before.SeriesMatched,
			gotSamples,
			after.SegmentsRead-before.SegmentsRead,
			after.SampleRowsDecoded-before.SampleRowsDecoded,
			after.PayloadBytesRead-before.PayloadBytesRead,
			after.HeadSeriesScanned-before.HeadSeriesScanned,
			plan.sources() > 1,
		)
		eng.close()
	}
}

// readCounterOps sums the unconditional read counters a query increments. Each
// observed increment costs at least one uncontended atomic read-modify-write, so
// this bounds the diagnostic overhead the ReadStats path adds to a query.
func readCounterOps(s columnar.ReadStats) uint64 {
	// PayloadBytesRead is a byte count, not a call count, so it is excluded;
	// the row counters are per decoded row rather than per row count.
	return s.LabelPayloadOpens + s.SamplePayloadOpens + s.MetadataPayloadOpens +
		s.RowGroupsRead + s.SampleRowsDecoded + s.SampleBatchesRead +
		s.SegmentsRead +
		s.SeriesMatched + s.SegmentsAvailable + s.SegmentsPlanned + s.SegmentsOutsideWindow +
		s.BlocksOverlapping + s.HeadIndexHits + s.HeadSeriesScanned +
		s.PostingsScanned + s.PostingsNameMatched + s.MatcherCalls +
		s.PostingsValueMatched + s.SelectCalls + s.Queries + s.LabelRowsVisited +
		s.MetadataIndexCandidates + s.MetadataIndexFallbacks
}

// TestStage0SourceReport records what each arm makes available and what it
// selects, including on-disk bytes, so the timing matrix is interpretable
// without re-deriving the fixture from code. It is evidence for a recorded
// session rather than a correctness assertion, so it only runs on request.
func TestStage0SourceReport(t *testing.T) {
	if os.Getenv("STAGE0_REPORT") == "" {
		t.Skip("set STAGE0_REPORT=1 to record the source matrix accounting")
	}
	series := envInt(t, "STAGE0_SERIES", stage0TestSeries)
	perSeries := envInt(t, "STAGE0_SAMPLES", stage0DefaultSamples)
	f := newFixture(series, perSeries)
	t.Logf("corpus series=%d samplesPerSeries=%d samples=%d intervalMs=%d spanMs=%d",
		series, perSeries, series*perSeries, scrapeIntervalMs, f.maxt-f.mint)

	for _, plan := range stage0PlanSelection(t) {
		for _, builder := range stage0Builders {
			eng := builder.build(t, f, plan)
			total, data := dirSizes(eng.root)
			var parts []string
			for _, shape := range stage0SourceShapes {
				s, n := countSelection(t, eng, f, shape.matchers)
				parts = append(parts, fmt.Sprintf("%s=%d series/%d samples", shape.name, s, n))
			}
			t.Logf("plan=%s engine=%s sources=%d storeB=%d dataB=%d %s",
				plan.name, eng.name, eng.sources, total, data, strings.Join(parts, " "))
			eng.close()
		}
	}
}

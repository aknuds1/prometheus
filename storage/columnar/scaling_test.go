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

// Scaling study: does per-series query cost grow more slowly in the columnar
// layout than in TSDB as cardinality rises?
//
// Both engines are driven through storage.Querier from one sealed block each, so
// the comparison excludes HTTP, the PromQL engine, and block overlap. Overlap is
// the dominant term at low cardinality (1.6-1.8 ms per extra block) and would
// otherwise be mistaken for a scaling effect.
//
// Run with: go test ./storage/columnar -run '^$' -bench Scaling -benchtime 5x
// Set SCALING_SERIES to a comma list to change the cardinalities (default 10k,100k).

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
)

// fxSample implements chunks.Sample so the fixture can feed both engines.
var _ chunks.Sample = fxSample{}

const scrapeIntervalMs = 15_000

type fxSample struct {
	t int64
	v float64
}

func (s fxSample) T() int64                      { return s.t }
func (s fxSample) ST() int64                     { return 0 }
func (s fxSample) F() float64                    { return s.v }
func (s fxSample) H() *histogram.Histogram       { return nil }
func (s fxSample) FH() *histogram.FloatHistogram { return nil }
func (s fxSample) Type() chunkenc.ValueType      { return chunkenc.ValFloat }
func (s fxSample) Copy() chunks.Sample           { return s }

type fixture struct {
	nSamples int
	labels   []labels.Labels
	samples  [][]chunks.Sample
	mint     int64
	maxt     int64
}

// newFixture builds node_exporter-shaped series so matcher selectivity behaves
// like the real corpus rather than like synthetic noise.
func newFixture(series, samplesPerSeries int) *fixture {
	const start = int64(1_700_000_000_000)
	f := &fixture{
		labels:  make([]labels.Labels, series),
		samples: make([][]chunks.Sample, series),
		mint:    start,
		maxt:    start + int64(samplesPerSeries-1)*scrapeIntervalMs,
	}
	for i := 0; i < series; i++ {
		f.labels[i] = labels.FromStrings(
			labels.MetricName, "node_cpu_seconds_total",
			"instance", fmt.Sprintf("host-%d:9100", i/64),
			"cpu", strconv.Itoa(i%16),
			"mode", []string{"idle", "user", "system", "iowait"}[i%4],
			"series", strconv.Itoa(i),
		)
		s := make([]chunks.Sample, samplesPerSeries)
		for j := 0; j < samplesPerSeries; j++ {
			s[j] = fxSample{t: start + int64(j)*scrapeIntervalMs, v: float64(i%97) + float64(j)*0.37}
		}
		f.samples[i] = s
	}
	f.nSamples = samplesPerSeries
	return f
}

type engine struct {
	name  string
	root  string
	query func(tb testing.TB, mint, maxt int64) storage.Querier
	close func()
}

func columnarEngine(tb testing.TB, f *fixture) *engine {
	tb.Helper()
	dir := tb.TempDir()
	// One block: the threshold must exceed the whole fixture or the study measures
	// block overlap instead of scaling.
	st, err := columnar.NewStorage(dir, columnar.StorageOptions{
		FlushSamples: len(f.labels) * f.nSamples * 2,
		DisableWAL:   true,
	})
	if err != nil {
		tb.Fatal(err)
	}
	app := st.AppenderV2(context.Background())
	for i := range f.labels {
		for _, s := range f.samples[i] {
			if _, err := app.Append(0, f.labels[i], 0, s.T(), s.F(), nil, nil, storage.AOptions{}); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := app.Commit(); err != nil {
		tb.Fatal(err)
	}
	if err := st.Flush(); err != nil {
		tb.Fatal(err)
	}
	if got := st.ReadStats().Blocks; got != 1 {
		tb.Fatalf("columnar produced %d blocks, want exactly 1", got)
	}
	return &engine{
		name: "columnar",
		root: dir,
		query: func(tb testing.TB, mint, maxt int64) storage.Querier {
			q, err := st.Querier(mint, maxt)
			if err != nil {
				tb.Fatal(err)
			}
			return q
		},
		close: func() { _ = st.Close() },
	}
}

func tsdbEngine(tb testing.TB, f *fixture) *engine {
	tb.Helper()
	dir := tb.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	series := make([]storage.Series, len(f.labels))
	for i := range f.labels {
		series[i] = storage.NewListSeries(f.labels[i], f.samples[i])
	}
	blockDir := tb.TempDir()
	// The chunk range must cover the fixture span or CreateBlock rejects samples
	// as out of bounds, which silently limits how long a series can be.
	// CreateBlock appends through the head, which rejects samples older than its
	// valid-time window once a 10,000-sample commit has advanced maxTime. The
	// window is derived from chunkRange, so it must exceed the fixture span by a
	// wide margin rather than merely cover it - otherwise longer fixtures fail with
	// "out of bounds" for a reason that has nothing to do with the data.
	chunkRange := 24 * time.Hour.Milliseconds()
	if _, err := tsdb.CreateBlock(series, blockDir, chunkRange, logger); err != nil {
		tb.Fatal(err)
	}
	db, err := tsdb.Open(dir, logger, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		tb.Fatal(err)
	}
	// Move the created block into the opened DB's directory so the head stays empty
	// and every query is a pure block read.
	entries, err := os.ReadDir(blockDir)
	if err != nil {
		tb.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Rename(blockDir+"/"+e.Name(), dir+"/"+e.Name()); err != nil {
			tb.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		tb.Fatal(err)
	}
	db, err = tsdb.Open(dir, logger, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		tb.Fatal(err)
	}
	return &engine{
		name: "tsdb",
		root: dir,
		query: func(tb testing.TB, mint, maxt int64) storage.Querier {
			q, err := db.Querier(mint, maxt)
			if err != nil {
				tb.Fatal(err)
			}
			return q
		},
		close: func() { _ = db.Close() },
	}
}

// dirSizes splits sealed block bytes into totals and data-only, excluding the
// index/catalog so that segment framing can be compared without the per-series
// metadata that scales with series count rather than with segment size.
func dirSizes(root string) (total, data int64) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		n := info.Size()
		total += n
		switch filepath.Base(p) {
		case "catalog.bin", "index", "lock", "queries.active":
		default:
			data += n
		}
		return nil
	})
	return total, data
}

var scalingShapes = []struct {
	name     string
	matchers []*labels.Matcher
}{
	{"selective", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "instance", "host-1:9100")}},
	{"cpu0", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "cpu", "0")}},
	{"cpu01regex", []*labels.Matcher{labels.MustNewMatcher(labels.MatchRegexp, "cpu", "[01]")}},
	{"mode_idle", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "mode", "idle")}},
	{"wide", []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "node_cpu_seconds_total")}},
}

func peakRSSMiB() int {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				kb, _ := strconv.Atoi(f[1])
				return kb / 1024
			}
		}
	}
	return 0
}

func BenchmarkScaling(b *testing.B) {
	// Series count is fixed so that label cardinality, catalog entries and index
	// postings are constant; only samples per series changes, which is the only
	// thing that changes segment and chunk size.
	series := 10_000
	if v := os.Getenv("SCALING_SERIES"); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			b.Fatal(err)
		}
		series = n
	}
	densities := []int{60, 600}
	if v := os.Getenv("SCALING_DENSITIES"); v != "" {
		densities = nil
		for _, p := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				b.Fatal(err)
			}
			densities = append(densities, n)
		}
	}
	for _, perSeries := range densities {
		f := newFixture(series, perSeries)
		want := int64(series * perSeries)
		for _, mk := range []func(testing.TB, *fixture) *engine{columnarEngine, tsdbEngine} {
			eng := mk(b, f)
			total, data := dirSizes(eng.root)
			b.Run(fmt.Sprintf("perSeries=%d/engine=%s/size", perSeries, eng.name), func(b *testing.B) {
				b.ReportMetric(float64(total)/float64(want), "total-B/sample")
				b.ReportMetric(float64(data)/float64(want), "data-B/sample")
				b.ReportMetric(float64(total)/float64(series), "total-B/series")
			})
			eng.close()
		}
	}
}

func selectAndDrain(tb testing.TB, eng *engine, f *fixture, matchers []*labels.Matcher) (storage.Querier, error) {
	tb.Helper()
	q := eng.query(tb, f.mint, f.maxt)
	set := q.Select(context.Background(), true, nil, matchers...)
	for set.Next() {
		it := set.At().Iterator(nil)
		for it.Next() != 0 { //nolint:revive // drain
		}
		if err := it.Err(); err != nil {
			return nil, err
		}
	}
	return q, set.Err()
}

func countMatched(tb testing.TB, eng *engine, f *fixture, matchers []*labels.Matcher) int {
	tb.Helper()
	q := eng.query(tb, f.mint, f.maxt)
	defer q.Close()
	set := q.Select(context.Background(), true, nil, matchers...)
	n := 0
	for set.Next() {
		n++
	}
	return n
}

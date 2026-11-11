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
	"log/slog"
	"os"
	"runtime"
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

const (
	scrapeIntervalMs = 15_000
	samplesPerSeries = 60
)

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
	labels  []labels.Labels
	samples [][]chunks.Sample
	mint    int64
	maxt    int64
}

// newFixture builds node_exporter-shaped series so matcher selectivity behaves
// like the real corpus rather than like synthetic noise.
func newFixture(series int) *fixture {
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
	return f
}

type engine struct {
	name  string
	query func(tb testing.TB, mint, maxt int64) storage.Querier
	close func()
}

func columnarEngine(tb testing.TB, f *fixture) *engine {
	tb.Helper()
	dir := tb.TempDir()
	// One block: the threshold must exceed the whole fixture or the study measures
	// block overlap instead of scaling.
	st, err := columnar.NewStorage(dir, columnar.StorageOptions{
		FlushSamples: len(f.labels) * samplesPerSeries * 2,
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
	if _, err := tsdb.CreateBlock(series, blockDir, 2*time.Hour.Milliseconds(), logger); err != nil {
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
	seriesList := []int{10_000, 100_000}
	if v := os.Getenv("SCALING_SERIES"); v != "" {
		seriesList = nil
		for _, p := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				b.Fatal(err)
			}
			seriesList = append(seriesList, n)
		}
	}
	for _, n := range seriesList {
		f := newFixture(n)
		for _, mk := range []func(testing.TB, *fixture) *engine{columnarEngine, tsdbEngine} {
			eng := mk(b, f)
			b.Run(fmt.Sprintf("series=%d/engine=%s/build", n, eng.name), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					e2 := mk(b, f)
					e2.close()
				}
				runtime.GC()
				b.ReportMetric(float64(peakRSSMiB()), "peakRSS-MiB")
			})
			for _, shape := range scalingShapes {
				matched := countMatched(b, eng, f, shape.matchers)
				q := eng.query(b, f.mint, f.maxt)
				q.Close()
				b.Run(fmt.Sprintf("series=%d/engine=%s/%s", n, eng.name, shape.name), func(b *testing.B) {
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						q, err := selectAndDrain(b, eng, f, shape.matchers)
						if err != nil {
							b.Fatal(err)
						}
						q.Close()
					}
					b.StopTimer()
					b.ReportMetric(float64(matched), "matched")
				})
			}
			eng.close()
		}
		b.Run(fmt.Sprintf("series=%d/rss", n), func(b *testing.B) { b.ReportMetric(float64(peakRSSMiB()), "peakRSS-MiB") })
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

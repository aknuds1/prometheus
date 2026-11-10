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

import (
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/util/teststorage"
)

// syntheticSeries builds n series of m samples each, shaped like node_exporter
// output: one metric family, a handful of low-cardinality labels, samples at a
// fixed interval.
func syntheticSeries(n, m int) ([]labels.Labels, [][]int64, [][]float64) {
	starts := int64(1_700_000_000_000)
	modes := []string{"idle", "user", "system", "iowait"}

	allLabels := make([]labels.Labels, 0, n)
	timestamps := make([][]int64, 0, n)
	values := make([][]float64, 0, n)

	for i := range n {
		allLabels = append(allLabels, labels.FromStrings(
			labels.MetricName, "node_cpu_seconds_total",
			"instance", "localhost:9100",
			"job", "node_exporter",
			"cpu", strconv.Itoa(i%16),
			"mode", modes[i%len(modes)],
			"series", strconv.Itoa(i),
		))

		ts := make([]int64, m)
		vs := make([]float64, m)
		for j := range m {
			ts[j] = starts + int64(j)*15000
			vs[j] = float64(j) * 1.37
		}
		timestamps = append(timestamps, ts)
		values = append(values, vs)
	}
	return allLabels, timestamps, values
}

// BenchmarkAppendColumnar measures the row-to-column ingestion path: one
// appender transaction, flushed into a block.
func BenchmarkAppendColumnar(b *testing.B) {
	const series, samples = 1000, 100
	allLabels, timestamps, values := syntheticSeries(series, samples)

	b.ReportAllocs()
	b.ReportMetric(float64(series*samples), "samples/op")
	b.ResetTimer()
	for range b.N {
		st, err := columnar.NewStorage(b.TempDir(), columnar.StorageOptions{FlushSamples: series * samples})
		require.NoError(b, err)

		app := st.AppenderV2(b.Context())
		for i := range series {
			for j := range samples {
				if _, err := app.Append(0, allLabels[i], 0, timestamps[i][j], values[i][j], nil, nil, storage.AOptions{}); err != nil {
					b.Fatal(err)
				}
			}
		}
		if err := app.Commit(); err != nil {
			b.Fatal(err)
		}
		require.NoError(b, st.Close())
	}
}

// BenchmarkAppendTSDB measures the same ingestion into the TSDB head, for
// comparison with BenchmarkAppendColumnar.
func BenchmarkAppendTSDB(b *testing.B) {
	const series, samples = 1000, 100
	allLabels, timestamps, values := syntheticSeries(series, samples)

	b.ReportAllocs()
	b.ReportMetric(float64(series*samples), "samples/op")
	b.ResetTimer()
	for range b.N {
		st := teststorage.New(b)
		app := st.AppenderV2(b.Context())
		for i := range series {
			for j := range samples {
				if _, err := app.Append(0, allLabels[i], 0, timestamps[i][j], values[i][j], nil, nil, storage.AOptions{}); err != nil {
					b.Fatal(err)
				}
			}
		}
		if err := app.Commit(); err != nil {
			b.Fatal(err)
		}
		require.NoError(b, st.Close())
	}
}

// BenchmarkAppendHeadG3 compares array-batch ingestion with the current scalar
// columnar appender over the same 100,000 samples. Both arms disable WAL and
// flushing deliberately: this is a head-path gate, not a durable-ingest claim.
func BenchmarkAppendHeadG3(b *testing.B) {
	const series, samples = 1000, 100
	allLabels, timestamps, values := syntheticSeries(series, samples)
	batch := columnar.Batch{Series: make([]columnar.BatchSeries, series)}
	for i := range series {
		batch.Series[i] = columnar.BatchSeries{
			Labels: allLabels[i], Timestamps: timestamps[i], Kinds: make([]columnar.SampleKind, samples), Values: values[i],
		}
	}

	for _, mode := range []string{"batch", "scalar"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(series*samples, "samples/op")
			b.ResetTimer()
			for range b.N {
				st, err := columnar.NewStorage(b.TempDir(), columnar.StorageOptions{
					FlushSamples: math.MaxInt, DisableWAL: true, DisableNativeMetadata: true,
				})
				require.NoError(b, err)
				if mode == "batch" {
					require.NoError(b, st.AppendBatch(b.Context(), batch))
				} else {
					app := st.AppenderV2(b.Context())
					for i := range series {
						for j := range samples {
							_, err := app.Append(0, allLabels[i], 0, timestamps[i][j], values[i][j], nil, nil, storage.AOptions{})
							require.NoError(b, err)
						}
					}
					require.NoError(b, app.Commit())
				}
				require.NoError(b, st.Close())
			}
		})
	}
}

// BenchmarkQueryRateColumnar measures a range query over the columnar store.
func BenchmarkQueryRateColumnar(b *testing.B) {
	columnarBench, cleanup := ingestedColumnar(b)
	defer cleanup()
	benchmarkQuery(b, columnarBench)
}

// BenchmarkQueryRateTSDB measures the same range query over the TSDB head.
func BenchmarkQueryRateTSDB(b *testing.B) {
	tsdbBench := ingestedTSDB(b)
	benchmarkQuery(b, tsdbBench)
}

func benchmarkQuery(b *testing.B, q storage.Queryable) {
	b.Helper()

	engine := promqltest.NewTestEngineWithOpts(b, promql.EngineOpts{
		MaxSamples:               50_000_000,
		Timeout:                  5 * time.Minute,
		LookbackDelta:            5 * time.Minute,
		EnableAtModifier:         true,
		EnableNegativeOffset:     true,
		EnableDelayedNameRemoval: true,
		Parser:                   parser.NewParser(promqltest.TestParserOpts),
	})
	start := time.UnixMilli(1_700_000_000_000)
	end := start.Add(100 * 15 * time.Second)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		query, err := engine.NewRangeQuery(b.Context(), q, nil, `rate(node_cpu_seconds_total[1m])`, start, end, 15*time.Second)
		require.NoError(b, err)
		result := query.Exec(b.Context())
		require.NoError(b, result.Err)
		query.Close()
	}
	b.StopTimer()
}

// ingestedStorages returns a columnar storage holding the benchmark data.
func ingestedColumnar(b *testing.B) (*columnar.Storage, func()) {
	b.Helper()

	const series, samples = 500, 100
	allLabels, timestamps, values := syntheticSeries(series, samples)

	st, err := columnar.NewStorage(b.TempDir(), columnar.StorageOptions{FlushSamples: 5000})
	require.NoError(b, err)

	app := st.AppenderV2(b.Context())
	for i := range series {
		for j := range samples {
			require.NoError(b, func() error {
				_, err := app.Append(0, allLabels[i], 0, timestamps[i][j], values[i][j], nil, nil, storage.AOptions{})
				return err
			}())
		}
	}
	require.NoError(b, app.Commit())
	return st, func() { _ = st.Close() }
}

// ingestedTSDB returns a TSDB storage holding the same data.
func ingestedTSDB(b *testing.B) storage.Storage {
	b.Helper()

	const series, samples = 500, 100
	allLabels, timestamps, values := syntheticSeries(series, samples)

	st := teststorage.New(b)
	app := st.AppenderV2(b.Context())
	for i := range series {
		for j := range samples {
			require.NoError(b, func() error {
				_, err := app.Append(0, allLabels[i], 0, timestamps[i][j], values[i][j], nil, nil, storage.AOptions{})
				return err
			}())
		}
	}
	require.NoError(b, app.Commit())
	return st
}

// TestBytesPerSample reports the on-disk cost of a written block.
func TestBytesPerSample(t *testing.T) {
	const series, samples = 1000, 100
	allLabels, timestamps, values := syntheticSeries(series, samples)

	dir := t.TempDir()
	rows := make([]columnar.Series, 0, series)
	for i := range series {
		rows = append(rows, columnar.NewFloatSeries(allLabels[i], timestamps[i], values[i]))
	}
	meta, err := columnar.WriteBlock(dir, rows, columnar.WriteOptions{})
	require.NoError(t, err)

	var bytes int64
	entries, err := os.ReadDir(filepath.Join(dir, meta.ULID))
	require.NoError(t, err)
	for _, entry := range entries {
		info, err := entry.Info()
		require.NoError(t, err)
		bytes += info.Size()
	}

	t.Logf("series=%d samples=%d block=%d bytes (%.2f bytes/sample, %.1f bytes/series)",
		meta.NumSeries, meta.NumSamples, bytes,
		float64(bytes)/float64(meta.NumSamples), float64(bytes)/float64(meta.NumSeries))
}

// TestReplayRealCorpus loads a real TSDB directory into a columnar store and
// reports ingestion cost, on-disk size and query latency against the same data.
//
// It is skipped unless COLUMNAR_REPLAY_DIR points at a TSDB data directory,
// because the corpus is not part of the repository. Point it at a copy: opening a
// directory can create a lock and a WAL.
func TestReplayRealCorpus(t *testing.T) {
	dir := os.Getenv("COLUMNAR_REPLAY_DIR")
	if dir == "" {
		t.Skip("set COLUMNAR_REPLAY_DIR to a TSDB data directory to replay real series")
	}

	logger := slog.New(slog.DiscardHandler)
	opts := tsdb.DefaultOptions()
	db, err := tsdb.Open(dir, logger, prometheus.NewRegistry(), opts, tsdb.NewDBStats())
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	start, err := db.StartTime()
	require.NoError(t, err)
	maxTime := db.Head().MaxTime()
	if maxTime == 0 {
		maxTime = time.Now().UnixMilli()
	}

	q, err := db.Querier(math.MinInt64, math.MaxInt64)
	require.NoError(t, err)

	type seriesData struct {
		labels     labels.Labels
		timestamps []int64
		values     []float64
	}
	var all []seriesData

	ss := q.Select(t.Context(), false, nil, labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+"))
	for ss.Next() {
		s := ss.At()
		it := s.Iterator(nil)
		var timestamps []int64
		var values []float64
		for {
			vt := it.Next()
			if vt == 0 {
				break
			}
			if vt != 1 { // ValFloat only; histograms are not replayed.
				continue
			}
			ts, v := it.At()
			timestamps = append(timestamps, ts)
			values = append(values, v)
		}
		// TSDB iterators surface io.EOF at the end of some chunk types rather than
		// reporting a clean end, which is not an error for this purpose.
		if err := it.Err(); err != nil && !errors.Is(err, io.EOF) {
			require.NoError(t, err)
		}
		if len(timestamps) == 0 {
			continue
		}
		// Replay in time order: the corpus has overlapping blocks, so a merged read
		// can repeat a timestamp, and the store rejects out of order appends exactly
		// as the TSDB does.
		type point struct {
			t int64
			v float64
		}
		points := make([]point, len(timestamps))
		for i := range timestamps {
			points[i] = point{t: timestamps[i], v: values[i]}
		}
		sort.SliceStable(points, func(a, b int) bool { return points[a].t < points[b].t })
		timestamps = timestamps[:0]
		values = values[:0]
		for i, p := range points {
			if i > 0 && p.t == points[i-1].t {
				continue
			}
			timestamps = append(timestamps, p.t)
			values = append(values, p.v)
		}

		all = append(all, seriesData{labels: s.Labels(), timestamps: timestamps, values: values})
	}
	require.NoError(t, ss.Err())
	q.Close()

	var totalSamples int
	for _, s := range all {
		totalSamples += len(s.timestamps)
	}

	// Take the query window from the block metadata: individual series in the corpus
	// carry outlier timestamps, and a window derived from them lands nowhere useful.
	for _, block := range db.Blocks() {
		maxTime = max(maxTime, block.Meta().MaxTime)
	}
	require.NotZero(t, totalSamples, "the corpus must contain samples")

	colDir := t.TempDir()
	st, err := columnar.NewStorage(colDir, columnar.StorageOptions{
		// One block per ingestion window, flushed explicitly below.
		FlushSamples: 1 << 40,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	ingestStart := time.Now()
	var appended int

	// Ingest in time order, in one-hour windows, flushing a block per window.
	//
	// This matters more than it looks. Appending series by series would write blocks
	// that each hold complete series across months, so every block would overlap
	// every query window and a three-hour query would decode months of data - a
	// property of the harness, not of the format. A real ingestion stream advances
	// in time, so blocks become time slices and a time-bounded query reads only the
	// slices it needs.
	var dataMin int64 = math.MaxInt64
	for _, s := range all {
		if len(s.timestamps) > 0 {
			dataMin = min(dataMin, s.timestamps[0])
		}
	}

	const window = int64(6 * 60 * 60 * 1000)
	cursor := make([]int, len(all))
	for from := dataMin; from <= maxTime; from += window {
		to := from + window
		app := st.AppenderV2(t.Context())
		var inWindow int
		for si := range all {
			s := &all[si]
			for cursor[si] < len(s.timestamps) && s.timestamps[cursor[si]] < to {
				i := cursor[si]
				if s.timestamps[i] >= from {
					if _, err := app.Append(0, s.labels, 0, s.timestamps[i], s.values[i], nil, nil, storage.AOptions{}); err != nil {
						t.Fatalf("appending %s at %d: %v", s.labels, s.timestamps[i], err)
					}
					appended++
					inWindow++
				}
				cursor[si]++
			}
		}
		if inWindow == 0 {
			require.NoError(t, app.Rollback())
			continue
		}
		require.NoError(t, app.Commit())
		require.NoError(t, st.Flush())
	}
	ingestElapsed := time.Since(ingestStart)

	var bytes int64
	entries, err := os.ReadDir(colDir)
	require.NoError(t, err)
	for _, entry := range entries {
		subEntries, err := os.ReadDir(filepath.Join(colDir, entry.Name()))
		if err != nil {
			continue
		}
		for _, sub := range subEntries {
			info, err := sub.Info()
			if err != nil {
				continue
			}
			if !sub.IsDir() {
				bytes += info.Size()
			}
		}
	}

	engine := promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
		MaxSamples:               50_000_000,
		Timeout:                  5 * time.Minute,
		LookbackDelta:            5 * time.Minute,
		EnableAtModifier:         true,
		EnableNegativeOffset:     true,
		EnableDelayedNameRemoval: true,
		Parser:                   parser.NewParser(promqltest.TestParserOpts),
	})
	// Query the last three hours of the corpus: a step over its whole multi-month
	// span would be millions of steps and measure the sample limit, not the storage.
	const queryWindow = int64(3 * 60 * 60 * 1000)
	rangeStart := time.UnixMilli(maxTime - queryWindow)
	rangeEnd := time.UnixMilli(maxTime)
	t.Logf("querying %s to %s (%s step)", rangeStart.Format(time.RFC3339), rangeEnd.Format(time.RFC3339), "1m")
	_ = start

	measure := func(name string, q storage.Queryable, expr string) {
		query, err := engine.NewRangeQuery(t.Context(), q, nil, expr, rangeStart, rangeEnd, time.Minute)
		require.NoError(t, err)
		begin := time.Now()
		result := query.Exec(t.Context())
		elapsed := time.Since(begin)
		require.NoError(t, result.Err)
		query.Close()
		t.Logf("%-24s %-40s %s", name, expr, elapsed.Round(time.Millisecond))
	}

	t.Logf("replayed series=%d samples=%d blocks=%d", len(all), appended, st.NumBlocks())
	t.Logf("ingestion: %s total, %s per sample, %d samples/s",
		ingestElapsed.Round(time.Millisecond), time.Duration(int64(ingestElapsed)/int64(appended)),
		int(int64(appended)/int64(ingestElapsed.Seconds()+1)))
	t.Logf("on disk: %d bytes, %.2f bytes/sample (%.1f bytes/series)",
		bytes, float64(bytes)/float64(appended), float64(bytes)/float64(len(all)))

	for _, expr := range []string{
		`count(node_cpu_seconds_total)`,
		`rate(node_cpu_seconds_total[5m])`,
		`sum by (mode) (rate(node_cpu_seconds_total[5m]))`,
	} {
		measure("columnar", st, expr)
		measure("tsdb", db, expr)
	}
}

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

// Stage 0 ingestion, residency and diagnostic baselines.
//
// BenchmarkScrapeRound measures the production scrape shape instead of the
// 100-samples-per-series fixture: one transaction per round, one new sample per
// established series, and the returned SeriesRef passed back on every later
// round. WAL and flushing are disabled on both arms and named in the
// sub-benchmark path, so the number is a head-path number and must not be read
// as durable ingestion. BenchmarkScrapeRoundFirstSeen is the same shape with
// identities that have never been seen; BenchmarkScrapeRoundBatch is the direct
// typed dense batch control over the same series.
//
// BenchmarkChurnResident keeps live cardinality fixed while identities arrive
// and depart, and reports query cost together with retained historical
// identities, block count and a heap proxy at successive checkpoints. A
// fresh-fixture number cannot say anything about sustained work.
//
// BenchmarkInfoMetadataMatrix widens the existing 500-series shared-owner
// fixture to 10k series with shared and per-series Resources, a short and a long
// range, and one Resource version change.
//
// BenchmarkStage0Instrumentation quantifies the only diagnostic switch that
// exists at this HEAD. ReadStats atomics are unconditional, so their cost is
// bounded separately by BenchmarkStage0CounterCost and the operation counts
// TestStage0PhysicalWork reports.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/util/teststorage"
)

// f0Mint is the fixture start used where only timestamps matter.
const f0Mint = int64(1_700_000_000_000)

// scrapeStorage is the subset of storage.Storage the ingestion arms need.
type scrapeStorage interface {
	AppenderV2(context.Context) storage.AppenderV2
}

// scrapeAppendRound appends one sample per series and commits. refs holds the
// references returned by the previous round, which is what scrape passes back.
func scrapeAppendRound(tb testing.TB, app storage.AppenderV2, ls []labels.Labels, refs []storage.SeriesRef, t int64, v float64) {
	tb.Helper()
	for i := range ls {
		ref, err := app.Append(refs[i], ls[i], 0, t, v, nil, nil, storage.AOptions{})
		if err != nil {
			tb.Fatalf("append %s at %d: %v", ls[i], t, err)
		}
		refs[i] = ref
	}
	require.NoError(tb, app.Commit())
}

// stage0IngestArms are the head-path ingestion arms. Both disable the WAL so the
// comparison is append-path only; the columnar arm also disables flushing.
func stage0IngestArms() []struct {
	name string
	open func(tb testing.TB) (scrapeStorage, func())
} {
	return []struct {
		name string
		open func(tb testing.TB) (scrapeStorage, func())
	}{
		{"columnar/wal=off/flush=off/scalar", func(tb testing.TB) (scrapeStorage, func()) {
			st, err := columnar.NewStorage(tb.TempDir(), columnar.StorageOptions{
				FlushSamples: 1 << 40,
				DisableWAL:   true,
			})
			require.NoError(tb, err)
			return st, func() { _ = st.Close() }
		}},
		{"tsdb/wal=off/scalar", func(tb testing.TB) (scrapeStorage, func()) {
			st := teststorage.New(tb, func(o *tsdb.Options) { o.WALSegmentSize = -1 })
			return st, func() { _ = st.Close() }
		}},
	}
}

// BenchmarkScrapeRound is the established-series scrape shape: one sample per
// series per round, SeriesRef reused across rounds, one commit per round.
func BenchmarkScrapeRound(b *testing.B) {
	series := envInt(b, "STAGE0_SCRAPE_SERIES", 1_000)
	warmRounds := envInt(b, "STAGE0_SCRAPE_WARM", 5)
	f := newFixture(series, 1)

	for _, arm := range stage0IngestArms() {
		b.Run(arm.name, func(b *testing.B) {
			st, closeFn := arm.open(b)
			defer closeFn()
			refs := make([]storage.SeriesRef, series)
			round := 0
			for ; round < warmRounds; round++ {
				scrapeAppendRound(b, st.AppenderV2(context.Background()), f.labels, refs,
					f.mint+int64(round)*scrapeIntervalMs, float64(round))
			}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				scrapeAppendRound(b, st.AppenderV2(context.Background()), f.labels, refs,
					f.mint+int64(round)*scrapeIntervalMs, float64(round))
				round++
			}
			b.StopTimer()
			b.ReportMetric(float64(series), "series/op")
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*series), "ns/sample")
		})
	}
}

// BenchmarkScrapeRoundFirstSeen is the first-seen case: every round carries
// identities that have never been appended, so no ref can be reused and the
// series dictionary grows. Keep the iteration count small; memory grows with
// rounds and is not reclaimed inside the measured store.
func BenchmarkScrapeRoundFirstSeen(b *testing.B) {
	series := envInt(b, "STAGE0_SCRAPE_SERIES", 1_000)

	for _, arm := range stage0IngestArms() {
		b.Run(arm.name, func(b *testing.B) {
			st, closeFn := arm.open(b)
			defer closeFn()
			// Build every round's fresh identities before the timer starts: label
			// construction is fixture work, not ingestion cost, and it would
			// otherwise dominate the measurement on both arms.
			roundLabels := make([][]labels.Labels, b.N)
			for round := range roundLabels {
				ls := make([]labels.Labels, series)
				for i := range ls {
					ls[i] = labels.FromStrings(
						labels.MetricName, "churn_requests_total",
						"instance", "localhost:9100",
						"job", "node_exporter",
						"series", strconv.Itoa(round*series+i),
					)
				}
				roundLabels[round] = ls
			}
			round := 0
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				ls := roundLabels[round]
				refs := make([]storage.SeriesRef, series)
				scrapeAppendRound(b, st.AppenderV2(context.Background()), ls, refs,
					f0Mint+int64(round)*scrapeIntervalMs, float64(round))
				round++
			}
			b.StopTimer()
			b.ReportMetric(float64(series), "series/op")
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*series), "ns/sample")
		})
	}
}

// BenchmarkScrapeRoundBatch is the direct typed dense batch control: the same
// established series and the same one sample per series, but as one AppendBatch
// transaction with prebuilt columns instead of one scalar append per sample.
func BenchmarkScrapeRoundBatch(b *testing.B) {
	series := envInt(b, "STAGE0_SCRAPE_SERIES", 1_000)
	warmRounds := envInt(b, "STAGE0_SCRAPE_WARM", 5)
	f := newFixture(series, 1)

	st, err := columnar.NewStorage(b.TempDir(), columnar.StorageOptions{
		FlushSamples: 1 << 40,
		DisableWAL:   true,
	})
	require.NoError(b, err)
	defer func() { require.NoError(b, st.Close()) }()

	batch := columnar.Batch{Series: make([]columnar.BatchSeries, series)}
	for i := range series {
		batch.Series[i] = columnar.BatchSeries{
			Labels:     f.labels[i],
			Timestamps: make([]int64, 1),
			Kinds:      []columnar.SampleKind{columnar.SampleFloat},
			Values:     make([]float64, 1),
		}
	}

	round := 0
	appendRound := func(tb testing.TB) {
		t := f.mint + int64(round)*scrapeIntervalMs
		for i := range batch.Series {
			batch.Series[i].Timestamps[0] = t
			batch.Series[i].Values[0] = float64(round)
		}
		require.NoError(tb, st.AppendBatch(context.Background(), batch))
		round++
	}
	for range warmRounds {
		appendRound(b)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		appendRound(b)
	}
	b.StopTimer()
	b.ReportMetric(float64(series), "series/op")
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*series), "ns/sample")
}

// churnCorpus is a controlled churn fixture: live cardinality stays constant
// while a fixed number of identities depart and arrive per epoch, and every
// retained sample of a departed identity stays queryable.
type churnCorpus struct {
	live            int
	turnover        int
	samplesPerEpoch int
	// series holds every identity ever created, in creation order.
	series []*churnSeries
	// epochSamples[e] is the number of samples appended in epoch e.
	epochSamples []int
	mint         int64
	maxt         int64
}

type churnSeries struct {
	labels labels.Labels
	ts     []int64
	vs     []float64
	// created is the first epoch the identity is active in and retired the first
	// epoch it is no longer active in; retired is -1 while still live at the end
	// of the fixture.
	created int
	retired int
}

func newChurnCorpus(live, epochs, samplesPerEpoch int) *churnCorpus {
	turnover := live / 10
	if turnover < 1 {
		turnover = 1
	}
	c := &churnCorpus{
		live:            live,
		turnover:        turnover,
		samplesPerEpoch: samplesPerEpoch,
		epochSamples:    make([]int, epochs),
		mint:            f0Mint,
		maxt:            f0Mint + int64(epochs*samplesPerEpoch-1)*scrapeIntervalMs,
	}

	newSeries := func(identity, epoch int) *churnSeries {
		return &churnSeries{
			labels: labels.FromStrings(
				labels.MetricName, "churn_requests_total",
				"instance", "localhost:9100",
				"job", "node_exporter",
				"series", strconv.Itoa(identity),
			),
			created: epoch,
			retired: -1,
		}
	}

	// Seed the first cohort before epoch 0 so live cardinality is constant from
	// the first epoch onwards: epoch e retires and replaces the same count.
	for i := range live {
		c.series = append(c.series, newSeries(i, 0))
	}
	for e := range epochs {
		if e > 0 {
			retired := 0
			for _, s := range c.series {
				if retired == turnover {
					break
				}
				if s.retired == -1 && s.created < e {
					s.retired = e
					retired++
				}
			}
			for i := range turnover {
				c.series = append(c.series, newSeries(live+(e-1)*turnover+i, e))
			}
		}
		base := f0Mint + int64(e)*int64(samplesPerEpoch)*scrapeIntervalMs
		for _, s := range c.series {
			if s.created > e || (s.retired != -1 && s.retired <= e) {
				continue
			}
			for k := range samplesPerEpoch {
				s.ts = append(s.ts, base+int64(k)*scrapeIntervalMs)
				s.vs = append(s.vs, float64(e*samplesPerEpoch+k))
				c.epochSamples[e]++
			}
		}
	}
	return c
}

// activeAt returns the identities live during epoch e.
func (c *churnCorpus) activeAt(e int) int {
	n := 0
	for _, s := range c.series {
		if s.created <= e && (s.retired == -1 || s.retired > e) {
			n++
		}
	}
	return n
}

// maxtAt returns the last timestamp retained at the end of epoch e.
func (c *churnCorpus) maxtAt(e int) int64 {
	return c.mint + int64((e+1)*c.samplesPerEpoch-1)*scrapeIntervalMs
}

// churnPerBlock is the number of epochs the columnar threshold seals into one
// block. The TSDB control writes one block per group to match.
const churnPerBlock = 4

// buildColumnarChurn replays the churn fixture epoch by epoch and lets the
// sample-count threshold seal every churnPerBlock epochs, so the resident layout
// is the mechanism's own rather than a hand-made one.
func buildColumnarChurn(tb testing.TB, dir string, c *churnCorpus, epochs int) (*columnar.Storage, int) {
	tb.Helper()
	st, err := columnar.NewStorage(dir, columnar.StorageOptions{
		FlushSamples: c.live * c.samplesPerEpoch * churnPerBlock,
		DisableWAL:   true,
	})
	require.NoError(tb, err)

	for e := range epochs {
		app := st.AppenderV2(context.Background())
		base := c.mint + int64(e)*int64(c.samplesPerEpoch)*scrapeIntervalMs
		for _, s := range c.series {
			if s.created > e || (s.retired != -1 && s.retired <= e) {
				continue
			}
			for k := range c.samplesPerEpoch {
				t := base + int64(k)*scrapeIntervalMs
				v := float64(e*c.samplesPerEpoch + k)
				if _, err := app.Append(0, s.labels, 0, t, v, nil, nil, storage.AOptions{}); err != nil {
					tb.Fatalf("churn append %s at %d (epoch %d): %v", s.labels, t, e, err)
				}
			}
		}
		require.NoError(tb, app.Commit())
	}
	wantBlocks := (epochs + churnPerBlock - 1) / churnPerBlock
	require.Equal(tb, wantBlocks, st.NumBlocks(), "columnar churn blocks")
	require.Equal(tb, wantBlocks, st.ReadStats().Blocks)
	return st, wantBlocks
}

// buildTSDBChurn writes one block per churnPerBlock-epoch group so the control
// arm has the same source shape as the columnar threshold sealing.
func buildTSDBChurn(tb testing.TB, dir string, c *churnCorpus, epochs int) (*tsdb.DB, int) {
	tb.Helper()
	logger := slogDiscard()

	for groupStart := 0; groupStart < epochs; groupStart += churnPerBlock {
		groupEnd := min(groupStart+churnPerBlock, epochs)
		from := c.mint + int64(groupStart)*int64(c.samplesPerEpoch)*scrapeIntervalMs
		to := c.mint + int64(groupEnd)*int64(c.samplesPerEpoch)*scrapeIntervalMs
		var series []storage.Series
		for _, s := range c.series {
			var samples []chunks.Sample
			for i, t := range s.ts {
				if t >= from && t < to {
					samples = append(samples, fxSample{t: t, v: s.vs[i]})
				}
			}
			if len(samples) > 0 {
				series = append(series, storage.NewListSeries(s.labels, samples))
			}
		}
		if len(series) == 0 {
			continue
		}
		blockDir := tb.TempDir()
		if _, err := tsdb.CreateBlock(series, blockDir, tsdbBlockChunkRangeMs, logger); err != nil {
			tb.Fatalf("creating churn block %d: %v", groupStart, err)
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
	return db, len(db.Blocks())
}

// churnCheckpoints returns the epoch counts to report.
func churnCheckpoints(tb testing.TB, epochs int) []int {
	tb.Helper()
	v := os.Getenv("STAGE0_CHURN_CHECKPOINTS")
	if v == "" {
		out := []int{}
		for _, e := range []int{4, 8, 16} {
			if e <= epochs {
				out = append(out, e)
			}
		}
		if len(out) == 0 || out[len(out)-1] != epochs {
			out = append(out, epochs)
		}
		return out
	}
	var out []int
	for _, part := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n <= 0 || n > epochs {
			tb.Fatalf("STAGE0_CHURN_CHECKPOINTS=%q: %q is not an epoch count in (0,%d]", v, part, epochs)
		}
		out = append(out, n)
	}
	return out
}

// heapAllocMiB reports live heap after a collection. It is a coarse resident
// proxy shared by both arms, not an attribution of bytes to a structure.
func heapAllocMiB() float64 {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return float64(stats.HeapAlloc) / (1 << 20)
}

func sumInts(v []int) int {
	n := 0
	for _, x := range v {
		n += x
	}
	return n
}

// BenchmarkChurnResident measures query cost at successive churn checkpoints and
// reports the retained state that cost must be read alongside.
func BenchmarkChurnResident(b *testing.B) {
	live := envInt(b, "STAGE0_CHURN_LIVE", 1_000)
	epochs := envInt(b, "STAGE0_CHURN_EPOCHS", 16)
	samplesPerEpoch := envInt(b, "STAGE0_CHURN_SAMPLES", 4)
	c := newChurnCorpus(live, epochs, samplesPerEpoch)

	matchers := []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "churn_requests_total")}
	for _, checkpoint := range churnCheckpoints(b, epochs) {
		maxt := c.maxtAt(checkpoint - 1)
		arms := []struct {
			name  string
			build func(tb testing.TB, dir string) (storage.Queryable, int, func())
		}{
			{"columnar", func(tb testing.TB, dir string) (storage.Queryable, int, func()) {
				st, blocks := buildColumnarChurn(tb, dir, c, checkpoint)
				return st, blocks, func() { _ = st.Close() }
			}},
			{"tsdb", func(tb testing.TB, dir string) (storage.Queryable, int, func()) {
				db, blocks := buildTSDBChurn(tb, dir, c, checkpoint)
				return db, blocks, func() { _ = db.Close() }
			}},
		}
		for _, arm := range arms {
			b.Run(fmt.Sprintf("epochs=%d/engine=%s", checkpoint, arm.name), func(b *testing.B) {
				dir := b.TempDir()
				q, blocks, closeFn := arm.build(b, dir)
				defer closeFn()

				historical := 0
				for _, s := range c.series {
					if s.created < checkpoint {
						historical++
					}
				}
				active := c.activeAt(checkpoint - 1)
				retained := sumInts(c.epochSamples[:checkpoint])
				total, _ := dirSizes(dir)

				runtime.GC()
				b.ReportAllocs()
				b.ResetTimer()
				var sink int
				for range b.N {
					querier, err := q.Querier(c.mint, maxt)
					require.NoError(b, err)
					selected, samples := drainSelection(b, querier, matchers)
					sink += selected + samples
					require.NoError(b, querier.Close())
				}
				b.StopTimer()
				require.NotZero(b, sink)
				b.ReportMetric(float64(active), "live/series")
				b.ReportMetric(float64(historical), "historical/series")
				b.ReportMetric(float64(retained), "retained/samples")
				b.ReportMetric(float64(blocks), "blocks")
				b.ReportMetric(float64(total)/float64(retained), "store-B/sample")
				b.ReportMetric(heapAllocMiB(), "heap-MiB")
			})
		}
	}
}

// TestStage0ChurnEquivalence checks that both engines return identical complete
// results over the churn fixture and records the retained-state accounting.
func TestStage0ChurnEquivalence(t *testing.T) {
	live := envInt(t, "STAGE0_CHURN_LIVE", 400)
	epochs := envInt(t, "STAGE0_CHURN_EPOCHS", 8)
	samplesPerEpoch := envInt(t, "STAGE0_CHURN_SAMPLES", 4)
	c := newChurnCorpus(live, epochs, samplesPerEpoch)

	for _, checkpoint := range churnCheckpoints(t, epochs) {
		maxt := c.maxtAt(checkpoint - 1)
		colDir := t.TempDir()
		tsdbDir := t.TempDir()
		col, colBlocks := buildColumnarChurn(t, colDir, c, checkpoint)
		db, tsdbBlocks := buildTSDBChurn(t, tsdbDir, c, checkpoint)

		f := &fixture{mint: c.mint, maxt: maxt}
		expr := `rate(churn_requests_total[1m])`
		want := canonicalPromQL(t, db, f, expr)
		got := canonicalPromQL(t, col, f, expr)
		require.NotEmpty(t, want, "checkpoint %d produced no result", checkpoint)
		require.Equal(t, want, got, "checkpoint %d: complete churn result", checkpoint)

		colTotal, colData := dirSizes(colDir)
		tsdbTotal, tsdbData := dirSizes(tsdbDir)
		retained := sumInts(c.epochSamples[:checkpoint])
		historical := 0
		for _, s := range c.series {
			if s.created < checkpoint {
				historical++
			}
		}
		t.Logf("checkpoint=%d active=%d historical=%d retainedSamples=%d columnarBlocks=%d tsdbBlocks=%d "+
			"columnarStoreB=%d columnarDataB=%d tsdbStoreB=%d tsdbDataB=%d resultSeries=%d heapMiB=%.1f",
			checkpoint, c.activeAt(checkpoint-1), historical, retained, colBlocks, tsdbBlocks,
			colTotal, colData, tsdbTotal, tsdbData, len(want), heapAllocMiB())

		require.NoError(t, col.Close())
		require.NoError(t, db.Close())
	}
}

// infoCorpus is the native-metadata fixture shared by both arms.
type infoCorpus struct {
	mode    string
	owners  map[string]map[string]string
	changed map[string]map[string]string
	mint    int64
	maxt    int64
	mid     int64
	series  int
	samples int
}

func infoFixture(series, perSeries int, mode string) infoCorpus {
	start := int64(1_700_000_000_000)
	c := infoCorpus{
		mode:    mode,
		owners:  map[string]map[string]string{},
		changed: map[string]map[string]string{},
		mint:    start,
		maxt:    start + int64(perSeries-1)*scrapeIntervalMs,
		mid:     start + int64(perSeries/2)*scrapeIntervalMs,
		series:  series,
		samples: perSeries,
	}
	switch mode {
	case "shared":
		c.owners["localhost:9100"] = map[string]string{"host.name": "host-1", "region": "east"}
		c.changed["localhost:9100"] = map[string]string{"host.name": "host-1", "region": "west"}
	case "per-series":
		for i := range series {
			instance := "host-" + strconv.Itoa(i) + ":9100"
			c.owners[instance] = map[string]string{"host.name": instance, "region": "east"}
			if i%10 == 0 {
				c.changed[instance] = map[string]string{"host.name": instance, "region": "west"}
			}
		}
	default:
		panic("unknown owners mode " + mode)
	}
	return c
}

// instanceFor returns the identifying instance of data series i.
func (c infoCorpus) instanceFor(i int) string {
	if c.mode == "per-series" {
		return "host-" + strconv.Itoa(i) + ":9100"
	}
	return "localhost:9100"
}

// dataLabels returns the data series labels for identity i.
func (c infoCorpus) dataLabels(i int) labels.Labels {
	return labels.FromStrings(
		labels.MetricName, "node_cpu_seconds_total",
		"instance", c.instanceFor(i),
		"job", "node_exporter",
		"cpu", strconv.Itoa(i%16),
		"mode", []string{"idle", "user", "system", "iowait"}[i%4],
		"series", strconv.Itoa(i),
	)
}

func buildColumnarInfo(tb testing.TB, c infoCorpus) (storage.Queryable, func()) {
	tb.Helper()
	st, err := columnar.NewStorage(tb.TempDir(), columnar.StorageOptions{
		// One block for the whole fixture: the metadata path, not block count, is
		// what this matrix measures.
		FlushSamples:       c.series * c.samples,
		DisableWAL:         true,
		MetadataVersionCap: -1,
	})
	require.NoError(tb, err)

	ownerKeys := make([]columnar.OwnerKey, c.series)
	seriesBatch := make([]columnar.BatchSeries, c.series)
	updates := make([]columnar.AssociationUpdate, 0, c.series)
	for i := range c.series {
		instance := c.instanceFor(i)
		owner := columnar.ResourceOwner(map[string]string{
			"service.name": "node_exporter", "service.instance.id": instance,
		})
		ownerKeys[i] = owner
		ls := labels.NewBuilder(c.dataLabels(i)).Set(columnar.OTELResourceLabel, owner.Ref()).Labels()
		timestamps := make([]int64, c.samples)
		values := make([]float64, c.samples)
		kinds := make([]columnar.SampleKind, c.samples)
		for j := range c.samples {
			timestamps[j] = c.mint + int64(j)*scrapeIntervalMs
			values[j] = float64(j)
		}
		seriesBatch[i] = columnar.BatchSeries{Labels: ls, Timestamps: timestamps, Kinds: kinds, Values: values}
		updates = append(updates, columnar.AssociationUpdate{Labels: ls, Update: columnar.Update{
			Owner: owner, EffectiveFrom: c.mint, Content: columnar.Content{Attributes: c.owners[instance]},
		}})
	}
	require.NoError(tb, st.AppendBatch(context.Background(), columnar.Batch{Series: seriesBatch, Updates: updates}))

	// One Resource version change at the midpoint: an owner-scoped update, not one
	// membership write per series, which is the mechanism under measurement.
	seen := map[string]struct{}{}
	for i := range c.series {
		instance := c.instanceFor(i)
		content, ok := c.changed[instance]
		if !ok {
			continue
		}
		if _, done := seen[instance]; done {
			continue
		}
		seen[instance] = struct{}{}
		require.NoError(tb, st.MetadataUpdate(columnar.Update{
			Owner: ownerKeys[i], EffectiveFrom: c.mid, Content: columnar.Content{Attributes: content},
		}))
	}
	return st, func() { _ = st.Close() }
}

func buildTSDBInfo(tb testing.TB, c infoCorpus) (storage.Queryable, func()) {
	tb.Helper()
	st := teststorage.New(tb, func(o *tsdb.Options) { o.WALSegmentSize = -1 })
	app := st.AppenderV2(context.Background())

	md := metadata.Metadata{Type: model.MetricTypeGauge}
	for i := range c.series {
		ls := c.dataLabels(i)
		for j := range c.samples {
			t := c.mint + int64(j)*scrapeIntervalMs
			_, err := app.Append(0, ls, 0, t, float64(j), nil, nil, storage.AOptions{})
			require.NoError(tb, err)
		}
		// target_info is scraped every interval, as a real target_info is, so the
		// version change lands on the same step as the native association.
		instance := c.instanceFor(i)
		for j := range c.samples {
			t := c.mint + int64(j)*scrapeIntervalMs
			content := c.owners[instance]
			if changed, ok := c.changed[instance]; ok && t >= c.mid {
				content = changed
			}
			b := labels.NewBuilder(labels.FromStrings(
				labels.MetricName, "target_info",
				"instance", instance,
				"job", "node_exporter",
			))
			for k, v := range content {
				b.Set(k, v)
			}
			_, err := app.Append(0, b.Labels(), 0, t, 1, nil, nil, storage.AOptions{Metadata: md})
			require.NoError(tb, err)
		}
	}
	require.NoError(tb, app.Commit())
	return st, func() { _ = st.Close() }
}

// BenchmarkInfoMetadataMatrix measures info() over 10k series with either one
// shared Resource owner or one owner per series, at a short and a long range,
// with one Resource version change. The TSDB arm carries the same attributes in
// target_info series, which is the only way that control can express them.
func BenchmarkInfoMetadataMatrix(b *testing.B) {
	series := envInt(b, "STAGE0_INFO_SERIES", 10_000)
	perSeries := envInt(b, "STAGE0_INFO_SAMPLES", 60)
	fullSpan := time.Duration(perSeries-1) * 15 * time.Second

	for _, mode := range []string{"shared", "per-series"} {
		c := infoFixture(series, perSeries, mode)
		for _, qr := range []struct {
			name string
			span time.Duration
		}{
			{"short", 2 * time.Minute},
			{"long", fullSpan},
		} {
			for _, arm := range []struct {
				name  string
				build func(tb testing.TB, c infoCorpus) (storage.Queryable, func())
			}{
				{"columnar", buildColumnarInfo},
				{"tsdb", buildTSDBInfo},
			} {
				b.Run(fmt.Sprintf("owners=%s/range=%s/engine=%s", mode, qr.name, arm.name), func(b *testing.B) {
					q, closeFn := arm.build(b, c)
					defer closeFn()
					end := time.UnixMilli(c.maxt)
					start := end.Add(-qr.span)
					engine := stage0InfoEngine(b)

					count := runInfoQuery(b, engine, q, start, end)
					require.NotZero(b, count)

					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						runInfoQuery(b, engine, q, start, end)
					}
					b.StopTimer()
					b.ReportMetric(float64(count), "series/op")
					b.ReportMetric(qr.span.Seconds(), "span-s")
				})
			}
		}
	}
}

// runInfoQuery executes info() once and returns the result series count.
func runInfoQuery(tb testing.TB, engine promql.QueryEngine, q storage.Queryable, start, end time.Time) int {
	tb.Helper()
	query, err := engine.NewRangeQuery(context.Background(), q, nil, `info(node_cpu_seconds_total)`, start, end, 15*time.Second)
	require.NoError(tb, err)
	defer query.Close()
	result := query.Exec(context.Background())
	require.NoError(tb, result.Err)
	matrix, ok := result.Value.(promql.Matrix)
	require.True(tb, ok)
	return len(matrix)
}

// stage0InfoEngine is the info() engine configuration both arms use. UTF-8 label
// names are allowed so Resource attribute names survive the join.
func stage0InfoEngine(tb testing.TB) promql.QueryEngine {
	tb.Helper()
	opts := stage0EngineOpts()
	opts.LabelNamerConfig = &promql.LabelNamerConfig{UTF8Allowed: true}
	return promqltest.NewTestEngineWithOpts(tb, opts)
}

// TestStage0InfoEquivalence compares complete info() results between the native
// metadata arm and the target_info control, ignoring only the identity label the
// control cannot represent.
func TestStage0InfoEquivalence(t *testing.T) {
	series := envInt(t, "STAGE0_INFO_SERIES", 200)
	perSeries := envInt(t, "STAGE0_INFO_SAMPLES", 20)

	for _, mode := range []string{"shared", "per-series"} {
		c := infoFixture(series, perSeries, mode)
		col, colClose := buildColumnarInfo(t, c)
		db, dbClose := buildTSDBInfo(t, c)

		end := time.UnixMilli(c.maxt)
		start := end.Add(-time.Duration(perSeries-1) * 15 * time.Second)
		got := canonicalInfoResult(t, col, start, end)
		want := canonicalInfoResult(t, db, start, end)
		require.NotEmpty(t, want, "%s: control produced no result", mode)
		require.Equal(t, want, got, "%s: complete info() result", mode)
		if mode == "shared" {
			// The version change must be observable, or the fixture measures nothing.
			require.Contains(t, strings.Join(got, "\n"), `region="west"`)
		}
		t.Logf("owners=%s series=%d resultSeries=%d", mode, series, len(got))

		colClose()
		dbClose()
	}
}

// canonicalInfoResult renders each result series without the native identity
// label, which has no target_info counterpart.
func canonicalInfoResult(tb testing.TB, q storage.Queryable, start, end time.Time) []string {
	tb.Helper()
	engine := stage0InfoEngine(tb)
	query, err := engine.NewRangeQuery(context.Background(), q, nil, `info(node_cpu_seconds_total)`, start, end, 15*time.Second)
	require.NoError(tb, err)
	defer query.Close()
	result := query.Exec(context.Background())
	require.NoError(tb, result.Err)
	matrix, ok := result.Value.(promql.Matrix)
	require.True(tb, ok)

	out := make([]string, 0, len(matrix))
	for _, series := range matrix {
		sanitized := labels.NewBuilder(series.Metric).Del(columnar.OTELResourceLabel).Labels()
		points := make([]string, 0, len(series.Floats))
		for _, p := range series.Floats {
			points = append(points, fmt.Sprintf("%d=%s", p.T, strconv.FormatFloat(p.F, 'g', -1, 64)))
		}
		out = append(out, sanitized.String()+" "+strings.Join(points, " "))
	}
	sort.Strings(out)
	return out
}

// BenchmarkStage0Instrumentation is the diagnostic A/B: the same columnar
// fixture and query with phase timings off and on. ReadStats atomics stay on in
// both arms because this HEAD gives them no switch.
func BenchmarkStage0Instrumentation(b *testing.B) {
	series := envInt(b, "STAGE0_SERIES", stage0DefaultSeries)
	perSeries := envInt(b, "STAGE0_SAMPLES", stage0DefaultSamples)
	f := newFixture(series, perSeries)
	plan := corpusPlan{"head+3sealed", 3, 1}
	shape := stage0SourceShapes[2]

	for _, timings := range []bool{false, true} {
		b.Run(fmt.Sprintf("timings=%t", timings), func(b *testing.B) {
			per, err := plan.windowSamples(f.nSamples)
			require.NoError(b, err)
			st, err := columnar.NewStorage(b.TempDir(), columnar.StorageOptions{
				FlushSamples:       len(f.labels) * per * 2,
				DisableWAL:         true,
				EnableQueryTimings: timings,
			})
			require.NoError(b, err)
			defer func() { require.NoError(b, st.Close()) }()
			for w := range plan.windows() {
				app := st.AppenderV2(context.Background())
				for j := w * per; j < (w+1)*per; j++ {
					for i := range f.labels {
						s := f.samples[i][j]
						_, err := app.Append(0, f.labels[i], 0, s.T(), s.F(), nil, nil, storage.AOptions{})
						require.NoError(b, err)
					}
				}
				require.NoError(b, app.Commit())
				if w < plan.sealed {
					require.NoError(b, st.Flush())
				}
			}
			require.Equal(b, timings, st.ReadTimings().Enabled)

			runtime.GC()
			b.ReportAllocs()
			b.ResetTimer()
			var sink int
			for range b.N {
				q, err := st.Querier(f.mint, f.maxt)
				require.NoError(b, err)
				selected, samples := drainSelection(b, q, shape.matchers)
				sink += selected + samples
				require.NoError(b, q.Close())
			}
			b.StopTimer()
			require.NotZero(b, sink)
			b.ReportMetric(float64(st.ReadStats().Timings.SelectNanos), "selectNs-total")
		})
	}
}

// BenchmarkStage0LabelHashCost measures the series-identity hash the ingestion
// path performs on every sample at this HEAD: once in the scalar appender and
// again when the committed batch is prepared. It grounds the ingestion cost
// model in a measured primitive rather than an assumed share.
func BenchmarkStage0LabelHashCost(b *testing.B) {
	f := newFixture(1_000, 1)
	scratch := make([]byte, 0, 512)
	b.ReportAllocs()
	b.ResetTimer()
	var sink uint64
	for range b.N {
		for i := range f.labels {
			h, buf := labels.StableHashInto(f.labels[i], scratch)
			scratch = buf
			sink += h
		}
	}
	b.StopTimer()
	require.NotZero(b, sink)
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(f.labels)), "ns/hash")
}

// BenchmarkStage0MutexCost measures one uncontended mutex acquisition, the unit
// previousLocked charges per sample on the scalar append path.
func BenchmarkStage0MutexCost(b *testing.B) {
	var mu sync.Mutex
	b.ResetTimer()
	for range b.N {
		mu.Lock()
		mu.Unlock()
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N), "ns/lock")
}

// BenchmarkStage0CounterCost bounds the unconditional ReadStats overhead. Each
// iteration performs the same uncontended atomic adds the instrumentation does,
// so per-query counter counts can be converted into nanoseconds.
func BenchmarkStage0CounterCost(b *testing.B) {
	const adds = 64
	var counters [adds]atomic.Uint64
	b.ReportAllocs()
	for range b.N {
		for i := range counters {
			counters[i].Add(1)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*adds), "ns/add")
}

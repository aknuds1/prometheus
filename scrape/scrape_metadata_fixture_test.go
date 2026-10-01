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

package scrape

// This file is a shared benchmark fixture for native metadata experiments.

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promslog"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/util/teststorage"
)

const (
	committedScrapeWarmup      = 64
	committedScrapeHelpBytes   = 64
	committedScrapeChurnPeriod = 10 // Each family replaces its series every 10 scrapes.
	committedScrapeContentType = "application/openmetrics-text"
)

type committedScrapeShape struct {
	name                      string
	targets, families, perFam int
	churn                     bool
}

var committedScrapeShapes = []committedScrapeShape{
	{name: "base/targets=1", targets: 1, families: 100, perFam: 10},
	{name: "base/targets=10", targets: 10, families: 100, perFam: 10},
	{name: "cardinality/churned", targets: 1, families: 8000, perFam: 1, churn: true},
	{name: "cardinality/stable", targets: 1, families: 8000, perFam: 1},
}

// Metadata modes. The oracle preloads each target's scrape cache with the
// native values its own series hold, which bounds what sharing can recover.
const (
	committedScrapeNative   = "native"
	committedScrapeWAL      = "wal"
	committedScrapeDisabled = "disabled"
	committedScrapeOracle   = "oracle"
)

var committedScrapeModes = []string{committedScrapeNative, committedScrapeWAL, committedScrapeDisabled, committedScrapeOracle}

// committedScrapeReports adds build-specific measurements to the benchmark.
// They run outside timing at every sharing checkpoint and must not change
// the scrapes; the final values are reported as metrics.
var committedScrapeReports []func(tb testing.TB, r *committedScrapeRun) (unit string, value float64)

func (s committedScrapeShape) samplesPerScrape() int { return s.targets * s.families * s.perFam }

// generation returns the series generation of family f at scrape i. With churn,
// a tenth of the families replace their series on every scrape.
func (s committedScrapeShape) generation(f, i int) int {
	if !s.churn {
		return 0
	}
	return (i + f) / committedScrapeChurnPeriod
}

// firstScrape returns the scrape at which generation g of family f appears.
func (s committedScrapeShape) firstScrape(f, g int) int {
	if !s.churn {
		return 0
	}
	return max(0, g*committedScrapeChurnPeriod-f)
}

func (s committedScrapeShape) seriesCreated(scrapes int) int {
	n := 0
	for f := range s.families {
		n += s.generation(f, scrapes-1) - s.generation(f, 0) + 1
	}
	return n * s.targets * s.perFam
}

func committedScrapeFamily(f int) string { return fmt.Sprintf("fam_%05d_seconds", f) }

func committedScrapeMetadata(f int) metadata.Metadata {
	help := fmt.Sprintf("Benchmark family %05d help ", f)
	return metadata.Metadata{Type: model.MetricTypeGauge, Unit: "seconds", Help: help + strings.Repeat("x", committedScrapeHelpBytes-len(help))}
}

func (s committedScrapeShape) payload(buf []byte, i int) []byte {
	buf = buf[:0]
	for f := range s.families {
		name, m := committedScrapeFamily(f), committedScrapeMetadata(f)
		buf = fmt.Appendf(buf, "# TYPE %s %s\n# UNIT %s %s\n# HELP %s %s\n", name, m.Type, name, m.Unit, name, m.Help)
		g := s.generation(f, i)
		for j := range s.perFam {
			buf = fmt.Appendf(buf, "%s{series=\"%d\",generation=\"%d\"} 1\n", name, j, g)
		}
	}
	return append(buf, "# EOF\n"...)
}

// committedScrapeRun scrapes several targets into one TSDB through a fanout and
// commits every scrape, as production scrape loops do.
type committedScrapeRun struct {
	shape   committedScrapeShape
	mode    string
	db      *teststorage.TestStorage
	loops   []*scrapeLoop
	payload []byte
	scrapes int
	// base precedes the scrape loop's future-sample limit for every scrape.
	base time.Time
}

func newCommittedScrapeRun(tb testing.TB, shape committedScrapeShape, mode string) *committedScrapeRun {
	tb.Helper()
	native := mode == committedScrapeNative || mode == committedScrapeOracle
	db := teststorage.New(tb, func(o *tsdb.Options) {
		o.EnableNativeMetadata = native
		o.EnableMetadataWALRecords = mode == committedScrapeWAL
		o.EnableExemplarStorage = false
	})
	db.DisableCompactions()
	fanout := storage.NewFanout(promslog.NewNopLogger(), db)
	r := &committedScrapeRun{shape: shape, mode: mode, db: db, loops: make([]*scrapeLoop, shape.targets), base: time.Now().Add(-7 * 24 * time.Hour).Truncate(time.Second)}
	for t := range r.loops {
		target := "t" + strconv.Itoa(t)
		r.loops[t], _ = newTestScrapeLoop(tb, func(sl *scrapeLoop) {
			sl.appendableV2 = fanout
			sl.passMetadata = mode != committedScrapeDisabled
			sl.sampleMutator = func(l labels.Labels) labels.Labels {
				b := labels.NewBuilder(l)
				b.Set("instance", target)
				return b.Labels()
			}
		})
	}
	return r
}

// prepare builds the next scrape's payload. It is untimed.
func (r *committedScrapeRun) prepare() {
	if r.payload == nil || r.shape.churn {
		r.payload = r.shape.payload(r.payload, r.scrapes)
	}
}

// scrape appends and commits one scrape for every target.
func (r *committedScrapeRun) scrape(tb testing.TB) {
	ts := r.base.Add(time.Duration(r.scrapes) * 15 * time.Second)
	for _, sl := range r.loops {
		app := sl.appender()
		_, added, _, err := app.append(r.payload, committedScrapeContentType, ts)
		if err != nil {
			_ = app.Rollback()
			tb.Fatal(err)
		}
		if err := app.Commit(); err != nil {
			tb.Fatal(err)
		}
		if added != r.shape.families*r.shape.perFam {
			tb.Fatalf("scrape %d added %d samples", r.scrapes, added)
		}
	}
	r.scrapes++
}

func (r *committedScrapeRun) nativeSeries(tb testing.TB) []tsdb.NativeMetricMetadataSeries {
	tb.Helper()
	series, _, err := r.db.NativeMetricMetadata(tb.Context(), [][]*labels.Matcher{{labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, "fam_.+")}}, 0)
	require.NoError(tb, err)
	return series
}

func (r *committedScrapeRun) native() bool {
	return r.mode == committedScrapeNative || r.mode == committedScrapeOracle
}

// preloadOracle replaces each target's cached metadata strings with those its
// own series hold natively, without marking the entries changed.
func (r *committedScrapeRun) preloadOracle(tb testing.TB) {
	tb.Helper()
	for _, s := range r.nativeSeries(tb) {
		sl := r.loops[committedScrapeTarget(tb, s.Labels)]
		sl.cache.metaMtx.Lock()
		e := sl.cache.metadata[s.Labels.Get(labels.MetricName)]
		if e == nil {
			sl.cache.metaMtx.Unlock()
			tb.Fatalf("missing cached metadata for %s", s.Labels)
		}
		e.Metadata = s.Versions[len(s.Versions)-1].Metadata
		sl.cache.metaMtx.Unlock()
	}
}

func committedScrapeTarget(tb testing.TB, ls labels.Labels) int {
	t, err := strconv.Atoi(strings.TrimPrefix(ls.Get("instance"), "t"))
	if err != nil {
		tb.Fatalf("invalid target in %s", ls)
	}
	return t
}

// sharing counts the last scrape's samples whose non-empty metadata strings
// all share their data pointers with the series' current native value.
func (r *committedScrapeRun) sharing(tb testing.TB) (shared, total int) {
	tb.Helper()
	current := map[uint64]metadata.Metadata{}
	for _, s := range r.nativeSeries(tb) {
		current[s.Labels.Hash()] = s.Versions[len(s.Versions)-1].Metadata
	}
	for _, sl := range r.loops {
		sl.cache.metaMtx.Lock()
		for _, ce := range sl.cache.series {
			if ce.lastIter != sl.cache.iter-1 {
				continue
			}
			native, ok := current[ce.lset.Hash()]
			if !ok {
				sl.cache.metaMtx.Unlock()
				tb.Fatalf("no native metadata for %s", ce.lset)
			}
			cached := sl.cache.metadata[ce.lset.Get(labels.MetricName)].Metadata
			total++
			if committedScrapeShared(string(cached.Type), string(native.Type)) && committedScrapeShared(cached.Unit, native.Unit) && committedScrapeShared(cached.Help, native.Help) {
				shared++
			}
		}
		sl.cache.metaMtx.Unlock()
	}
	return shared, total
}

func committedScrapeShared(a, b string) bool {
	return a == "" && b == "" || a != "" && len(a) == len(b) && unsafe.StringData(a) == unsafe.StringData(b)
}

// validate checks series counts and, in native modes, every series' history.
func (r *committedScrapeRun) validate(tb testing.TB) {
	tb.Helper()
	s := r.shape
	created := s.seriesCreated(r.scrapes)
	require.Equal(tb, uint64(created), r.db.Head().NumSeries(), "head series")
	if !r.native() {
		return
	}
	series := r.nativeSeries(tb)
	require.Len(tb, series, created, "native series")
	distinct := map[metadata.Metadata]struct{}{}
	for _, ns := range series {
		f, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(ns.Labels.Get(labels.MetricName), "fam_"), "_seconds"))
		require.NoError(tb, err)
		g, err := strconv.Atoi(ns.Labels.Get("generation"))
		require.NoError(tb, err)
		want := committedScrapeMetadata(f)
		require.Len(tb, ns.Versions, 1, "versions of %s", ns.Labels)
		require.Equal(tb, want, ns.Versions[0].Metadata, "metadata of %s", ns.Labels)
		first := r.base.Add(time.Duration(s.firstScrape(f, g)) * 15 * time.Second)
		require.Equal(tb, first.UnixMilli(), ns.Versions[0].EffectiveFrom, "start of %s", ns.Labels)
		require.False(tb, ns.Truncated)
		distinct[ns.Versions[0].Metadata] = struct{}{}
	}
	require.Len(tb, distinct, s.families, "distinct metadata values")
}

type committedScrapeTotals struct {
	elapsed time.Duration
	mallocs uint64
	samples int
}

func (t committedScrapeTotals) report(b *testing.B, prefix string) {
	if t.samples == 0 {
		return
	}
	b.ReportMetric(float64(t.elapsed.Nanoseconds())/float64(t.samples), prefix+"ns/sample")
	b.ReportMetric(float64(t.mallocs)/float64(t.samples), prefix+"allocs/sample")
}

// BenchmarkScrapeLoopAppendCommitted measures committed multi-target scrapes.
// The first scrapes are reported separately as warm-up, and sharing is
// computed outside timing at doubling intervals and at the end.
//
// Use a fixed -benchtime=Nx with N > 64 so that every arm does the same work.
func BenchmarkScrapeLoopAppendCommitted(b *testing.B) {
	for _, shape := range committedScrapeShapes {
		for _, mode := range committedScrapeModes {
			b.Run(fmt.Sprintf("%s/mode=%s", shape.name, mode), func(b *testing.B) {
				r := newCommittedScrapeRun(b, shape, mode)
				var warmup, steady committedScrapeTotals
				var before, after runtime.MemStats
				var sharingLog []string
				b.ReportAllocs()
				b.ResetTimer()
				b.StopTimer()
				for i := range b.N {
					r.prepare()
					totals := &steady
					if i < committedScrapeWarmup {
						totals = &warmup
					}
					runtime.ReadMemStats(&before)
					start := time.Now()
					if totals == &steady {
						b.StartTimer()
					}
					r.scrape(b)
					if totals == &steady {
						b.StopTimer()
					}
					totals.elapsed += time.Since(start)
					runtime.ReadMemStats(&after)
					totals.mallocs += after.Mallocs - before.Mallocs
					totals.samples += shape.samplesPerScrape()
					if i == 0 && mode == committedScrapeOracle {
						r.preloadOracle(b)
					}
					if r.native() && ((i+1)&i == 0 || i == b.N-1) {
						shared, total := r.sharing(b)
						entry := fmt.Sprintf("%d:%d/%d", i+1, shared, total)
						for _, report := range committedScrapeReports {
							unit, value := report(b, r)
							entry += fmt.Sprintf(",%s=%g", unit, value)
							if i == b.N-1 {
								b.ReportMetric(value, unit)
							}
						}
						sharingLog = append(sharingLog, entry)
						if i == b.N-1 {
							b.ReportMetric(100*float64(shared)/float64(total), "sharing-%")
						}
					}
				}
				warmup.report(b, "warmup-")
				steady.report(b, "")
				if len(sharingLog) > 0 {
					b.Logf("sharing by scrape: %s", strings.Join(sharingLog, " "))
				}
				r.validate(b)
			})
		}
	}
}

func TestScrapeLoopAppendCommittedFixture(t *testing.T) {
	for _, shape := range committedScrapeShapes {
		for _, mode := range committedScrapeModes {
			t.Run(fmt.Sprintf("%s/mode=%s", shape.name, mode), func(t *testing.T) {
				r := newCommittedScrapeRun(t, shape, mode)
				for range 3 {
					r.prepare()
					r.scrape(t)
					if r.scrapes == 1 && mode == committedScrapeOracle {
						r.preloadOracle(t)
					}
				}
				r.validate(t)
				if !r.native() {
					return
				}
				shared, total := r.sharing(t)
				require.Equal(t, shape.families*shape.perFam*shape.targets, total)
				if mode == committedScrapeOracle && !shape.churn {
					// The oracle must itself be a valid upper bound.
					require.GreaterOrEqual(t, float64(shared), 0.99*float64(total))
				}
			})
		}
	}

	t.Run("churn bookkeeping", func(t *testing.T) {
		s := committedScrapeShape{families: 8000, perFam: 1, targets: 1, churn: true}
		require.Equal(t, 8000+800*4, s.seriesCreated(5))
		for f := range 20 {
			for i := range 30 {
				g := s.generation(f, i)
				require.LessOrEqual(t, s.firstScrape(f, g), i)
				require.Equal(t, g, s.generation(f, s.firstScrape(f, g)))
			}
		}
	})
}

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

//go:build linux || darwin

package remote

import (
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"strings"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/util/compression"
)

// The distinct-store diagnostic's sender kernels. Each stores a workload's
// change records after its seed, as the W1 kernel PS does, under one lifetime
// and GC mode:
//   - Int: decompression, decoding and the store in one window, GC on;
//   - NoGC: the same window with automatic GC off, then a live and a dropped
//     collection and a reclamation gate;
//   - Split: decoding, resolution and application timed per record, GC off;
//   - Retained and Collected: stores only, of records predecoded after or
//     before the forced GC;
//   - Warm: NoGC's window with a warm interner;
//   - Replay: isolated replays of the values the store resolves.
//
// Every iteration ends, outside its timed regions, with checks of every
// series' history and, on builds with the WAL metadata interner, of the
// interner and of which points share an object, against an independent model.
// This file is identical on every build. metadata_ds_interner_test.go is
// identical on builds with the WAL metadata interner, and
// metadata_ds_adapter_test.go holds each build's own parts.

var dsWorkloads = []string{"db", "sb", "dv"}

const (
	// dsGCOffCeiling bounds allocation while automatic GC is off.
	dsGCOffCeiling = 512 << 20
	// dsProbeSeries is the number of series whose history storage and newest
	// value the reclamation gate observes.
	dsProbeSeries = 32
	// dsPoison overwrites a record's memory once it is stored.
	dsPoison = 0xa5
)

// dsVersion is one version of a series' history, independently of any
// build's types.
type dsVersion struct {
	from int64
	m    *metadata.Metadata
}

// dsExpected returns each series' value at each step, indexed by step and
// slot.
func dsExpected(workload string, steps int) [][]metadata.Metadata {
	expected := make([][]metadata.Metadata, steps+1)
	for step := range expected {
		expected[step] = make([]metadata.Metadata, w1Series)
		for slot := range w1Series {
			expected[step][slot] = w1Metadata(workload, slot, step)
		}
	}
	return expected
}

// dsRecords returns a workload's records, with extra further change sweeps.
// Each repeats the last sweep's groups at the next step.
func dsRecords(tb testing.TB, workload string, extra int) *w1Records {
	if extra == 0 {
		return w1RecordsFor(tb, workload)
	}
	base := w1RecordsFor(tb, workload)
	r := &w1Records{seed: base.seed, changes: slices.Clone(base.changes), compressed: slices.Clone(base.compressed), newest: map[chunks.HeadSeriesRef]metadata.Metadata{}}
	last := w1Groups(workload)[w1Sweeps]
	for step := w1Sweeps + 1; step <= w1Sweeps+extra; step++ {
		for _, txn := range last {
			next := make([]w1Group, len(txn))
			for i, g := range txn {
				m := w1Metadata(workload, int(g.ref)-1, step)
				next[i] = w1Group{ref: g.ref, points: []w1Point{{from: w1Timestamp(step), m: m}}}
				r.newest[g.ref] = m
			}
			rec := w1EncodeRecord(next)
			compressed, err := compression.Encode(compression.Snappy, rec, nil)
			require.NoError(tb, err)
			r.changes = append(r.changes, rec)
			r.compressed = append(r.compressed, slices.Clone(compressed))
		}
	}
	return r
}

// dsRun is one iteration's state: a fresh queue and process interner, warm
// if requested, with the seed stored.
type dsRun struct {
	workload string
	warm     bool
	records  *w1Records
	qm       *QueueManager
	decoder  *w1Decoder
	decBuf   compression.DecodeBuffer
	restore  func()
}

func newDSRun(tb testing.TB, workload string, warm bool) *dsRun {
	r := &dsRun{workload: workload, warm: warm, records: w1RecordsFor(tb, workload), decBuf: compression.NewSyncDecodeBuffer()}
	r.restore = w1UseInterner(nil)
	r.qm = w1NewQueueManager(tb)
	r.decoder = newW1Decoder()
	for _, rec := range r.records.seed {
		r.decoder.decode(tb, rec)
		r.decoder.store(r.qm)
	}
	if warm {
		dsWarmProcessInterner()
	}
	return r
}

// apply decompresses, decodes and stores the change records through the
// production entry point.
func (r *dsRun) apply(tb testing.TB) {
	for _, compressed := range r.records.compressed {
		rec, err := compression.Decode(compression.Snappy, compressed, r.decBuf)
		if err != nil {
			require.NoError(tb, err)
		}
		r.decoder.decode(tb, rec)
		r.decoder.store(r.qm)
	}
}

func (r *dsRun) close() { r.restore() }

// dsCheckRun checks every series' history against the workload's values, and
// the interner and shared objects against the model, after an iteration's
// window.
func dsCheckRun(tb testing.TB, r *dsRun, expected [][]metadata.Metadata) {
	dsCheckHistories(tb, r.qm, expected, func(int) int { return w1Sweeps }, nil)
	dsCheckModel(tb, r)
}

// dsStepAfter returns, after change record i, the step each slot has
// reached: a sweep's records cover consecutive slots, w1Commit each.
func dsStepAfter(i int) func(slot int) int {
	perSweep := w1Series / w1Commit
	step, covered := 1+i/perSweep, (i%perSweep+1)*w1Commit
	return func(slot int) int {
		if slot < covered {
			return step
		}
		return step - 1
	}
}

// dsCheckHistories checks that each series holds its values up to its step,
// in order and with their starts, as deep as the build keeps them. With
// poison set, each string must also be free of it.
func dsCheckHistories(tb testing.TB, qm *QueueManager, expected [][]metadata.Metadata, stepOf func(slot int) int, poison []byte) {
	var versions []dsVersion
	for slot := range w1Series {
		step := stepOf(slot)
		depth := min(step+1, dsHistoryDepth())
		ref := chunks.HeadSeriesRef(slot + 1)
		var truncated bool
		versions, truncated = dsVersions(qm, ref, versions[:0])
		if len(versions) != depth {
			require.Len(tb, versions, depth, "series %d", ref)
		}
		if want := step+1 > depth && !dsLegacy(); truncated != want {
			require.Equal(tb, want, truncated, "series %d truncation", ref)
		}
		for i, v := range versions {
			s := step + 1 - depth + i
			if *v.m != expected[s][slot] {
				require.Equal(tb, expected[s][slot], *v.m, "series %d version %d", ref, i)
			}
			if !dsLegacy() && v.from != w1Timestamp(s) {
				require.Equal(tb, w1Timestamp(s), v.from, "series %d version %d start", ref, i)
			}
			if poison != nil && (strings.Contains(v.m.Help, string(poison)) || strings.Contains(v.m.Unit, string(poison))) {
				tb.Fatalf("series %d version %d retains a stored record's memory", ref, i)
			}
		}
	}
}

// dsPartition returns which history versions share an object, as each
// version's index of first occurrence, in ref and version order.
func dsPartition(qm *QueueManager) []int {
	first := map[*metadata.Metadata]int{}
	var out []int
	var versions []dsVersion
	for slot := range w1Series {
		versions, _ = dsVersions(qm, chunks.HeadSeriesRef(slot+1), versions[:0])
		for _, v := range versions {
			id, ok := first[v.m]
			if !ok {
				id = len(first)
				first[v.m] = id
			}
			out = append(out, id)
		}
	}
	return out
}

// dsStatesMismatch returns how two queues' histories differ, in content,
// starts, truncation or which versions share an object, or nil.
func dsStatesMismatch(a, b *QueueManager) error {
	var x, y []dsVersion
	for slot := range w1Series {
		ref := chunks.HeadSeriesRef(slot + 1)
		var xt, yt bool
		x, xt = dsVersions(a, ref, x[:0])
		y, yt = dsVersions(b, ref, y[:0])
		if len(x) != len(y) || xt != yt {
			return fmt.Errorf("series %d holds %d versions (truncated %t) and %d (truncated %t)", ref, len(x), xt, len(y), yt)
		}
		for i := range x {
			if x[i].from != y[i].from || *x[i].m != *y[i].m {
				return fmt.Errorf("series %d version %d differs", ref, i)
			}
		}
	}
	if !slices.Equal(dsPartition(a), dsPartition(b)) {
		return errors.New("the histories share objects differently")
	}
	return nil
}

// dsTimer extends w1Timer with the heap's state at each window's start and
// end, and the runtime's GC CPU estimate, which is reported only.
type dsTimer struct {
	*w1Timer
	windows                                int
	liveStart, goalStart, liveEnd, goalEnd float64
	gcClassCPU                             float64
	gcOffAllocStart, gcOffAllocMax         uint64
	samples                                []metrics.Sample
}

func newDSTimer(b *testing.B) *dsTimer {
	return &dsTimer{w1Timer: w1Leaf(b), samples: []metrics.Sample{
		{Name: "/gc/heap/live:bytes"},
		{Name: "/gc/heap/goal:bytes"},
		{Name: "/gc/heap/allocs:bytes"},
		{Name: "/cpu/classes/gc/total:cpu-seconds"},
	}}
}

func (t *dsTimer) read() (live, goal, allocs uint64, gcCPU float64) {
	metrics.Read(t.samples)
	return t.samples[0].Value.Uint64(), t.samples[1].Value.Uint64(), t.samples[2].Value.Uint64(), t.samples[3].Value.Float64()
}

func (t *dsTimer) time(work func()) {
	live, goal, _, gcBefore := t.read()
	t.w1Timer.time(work)
	liveEnd, goalEnd, _, gcAfter := t.read()
	t.windows++
	t.liveStart += float64(live)
	t.goalStart += float64(goal)
	t.liveEnd += float64(liveEnd)
	t.goalEnd += float64(goalEnd)
	t.gcClassCPU += (gcAfter - gcBefore) * 1e9
}

// gcOff turns automatic GC off, which the registered environment has on at
// 100.
func (t *dsTimer) gcOff(tb testing.TB) {
	if previous := debug.SetGCPercent(-1); previous != 100 {
		tb.Fatalf("GC percent was %d, not 100", previous)
	}
	_, _, t.gcOffAllocStart, _ = t.read()
}

// gcOn turns automatic GC back on, recording the allocation while it was off.
func (t *dsTimer) gcOn(tb testing.TB) {
	_, _, allocs, _ := t.read()
	t.gcOffAllocMax = max(t.gcOffAllocMax, allocs-t.gcOffAllocStart)
	if t.gcOffAllocMax >= dsGCOffCeiling {
		tb.Fatalf("allocated %d bytes with GC off, beyond %d", t.gcOffAllocMax, dsGCOffCeiling)
	}
	if previous := debug.SetGCPercent(100); previous != -1 {
		tb.Fatalf("GC percent was %d, not -1", previous)
	}
}

func (t *dsTimer) report(unit string, perIteration int, gcOff bool) {
	t.w1Timer.report(unit, perIteration)
	n := float64(t.windows)
	t.b.ReportMetric(t.liveStart/n, "heap-live-start-B")
	t.b.ReportMetric(t.liveEnd/n, "heap-live-end-B")
	// With automatic GC off, the runtime has no heap goal.
	if !gcOff {
		t.b.ReportMetric(t.goalStart/n, "heap-goal-start-B")
		t.b.ReportMetric(t.goalEnd/n, "heap-goal-end-B")
	}
	t.b.ReportMetric(t.gcClassCPU/(float64(t.b.N)*float64(perIteration)), "gc-class-cpu-ns/"+unit)
	if gcOff {
		t.b.ReportMetric(float64(t.gcOffAllocMax), "gc-off-alloc-max-B")
	}
}

// dsCollections times M2's two explicit collections and applies the
// reclamation gate.
type dsCollections struct {
	live, dropped, diff []float64
	unverified          int
}

// measure runs the live collection with r reachable, releases every reference
// to r's sender state, runs the dropped collection, and counts the probes
// that still observe their allocation. Automatic GC must be off.
func (c *dsCollections) measure(rp **dsRun) {
	r := *rp
	probes := dsProbes(r.qm)
	start := w1CPU()
	runtime.GC()
	live := w1CPU() - start
	runtime.KeepAlive(r)
	r.close()
	*rp, r = nil, nil
	start = w1CPU()
	runtime.GC()
	dropped := w1CPU() - start
	for _, p := range probes {
		if p.alive() {
			c.unverified++
		}
	}
	c.live = append(c.live, float64(live.Nanoseconds()))
	c.dropped = append(c.dropped, float64(dropped.Nanoseconds()))
	c.diff = append(c.diff, float64((live - dropped).Nanoseconds()))
}

func (c *dsCollections) report(b *testing.B) {
	b.ReportMetric(dsMedian(c.live), "live-gc-cpu-ns")
	b.ReportMetric(dsMedian(c.dropped), "dropped-gc-cpu-ns")
	b.ReportMetric(dsMedian(c.diff), "live-minus-dropped-cpu-ns")
	b.ReportMetric(float64(c.unverified), "unverified-probes")
}

func dsMedian(values []float64) float64 {
	s := slices.Clone(values)
	slices.Sort(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// dsProbe observes one allocation through a weak pointer.
type dsProbe struct {
	name  string
	alive func() bool
}

func dsWeakProbe[T any](name string, p *T) dsProbe {
	w := weak.Make(p)
	return dsProbe{name: name, alive: func() bool { return w.Value() != nil }}
}

// dsProbeRefs returns the sampled series.
func dsProbeRefs() []chunks.HeadSeriesRef {
	refs := make([]chunks.HeadSeriesRef, dsProbeSeries)
	for i := range refs {
		refs[i] = chunks.HeadSeriesRef(1 + i*(w1Series/dsProbeSeries))
	}
	return refs
}

// dsCounts are a workload's exact counts, from a counting run of their own.
type dsCounts struct {
	resolutions, newValues, copied, versions, growths, depth float64
}

func (c dsCounts) report(b *testing.B, records int) {
	b.ReportMetric(c.resolutions/float64(records), "resolutions/record")
	b.ReportMetric(c.newValues, "new-values")
	b.ReportMetric(c.copied, "copied-B")
	b.ReportMetric(c.versions, "versions")
	b.ReportMetric(c.growths, "growths")
	b.ReportMetric(c.depth, "depth")
}

// dsCount counts, in a run of its own, what storing the change records does:
// interner calls, the new owned values they create and the bytes they copy,
// the versions histories gain, the times a history's slice grows, and the
// final depth.
func dsCount(tb testing.TB, workload string) dsCounts {
	var c dsCounts
	r := newDSRun(tb, workload, false)
	defer r.close()
	counter := dsNewCounter(r)
	before := dsTotalVersions(r.qm)
	caps := map[chunks.HeadSeriesRef]int{}
	for _, compressed := range r.records.compressed {
		rec, err := compression.Decode(compression.Snappy, compressed, r.decBuf)
		require.NoError(tb, err)
		r.decoder.decode(tb, rec)
		refs := dsDecodedRefs(r.decoder)
		for _, ref := range refs {
			caps[ref] = dsOlderCap(r.qm, ref)
		}
		counter.store(r.decoder, r.qm)
		for _, ref := range refs {
			if dsOlderCap(r.qm, ref) != caps[ref] {
				c.growths++
			}
		}
	}
	c.resolutions, c.newValues, c.copied = counter.counts()
	c.versions = float64(dsTotalVersions(r.qm) - before)
	var versions []dsVersion
	versions, _ = dsVersions(r.qm, 1, versions)
	c.depth = float64(len(versions))
	predicted := dsCheckModel(tb, r)
	require.Equal(tb, [3]float64{predicted.resolutions, predicted.newValues, predicted.copied}, [3]float64{c.resolutions, c.newValues, c.copied},
		"the counting run's interner calls, new values and copied bytes against the model's")
	return c
}

func dsTotalVersions(qm *QueueManager) int {
	n := 0
	var versions []dsVersion
	for slot := range w1Series {
		versions, _ = dsVersions(qm, chunks.HeadSeriesRef(slot+1), versions[:0])
		n += len(versions)
	}
	return n
}

// The kernels with one window per iteration: Int (M1), NoGC (M2) and Warm
// (M6).

func BenchmarkMetadataDSInt(b *testing.B)  { dsKernel(b, "int") }
func BenchmarkMetadataDSNoGC(b *testing.B) { dsKernel(b, "nogc") }
func BenchmarkMetadataDSWarm(b *testing.B) { dsKernel(b, "warm") }

func dsKernel(b *testing.B, mode string) {
	for _, workload := range dsWorkloads {
		b.Run("workload="+workload, func(b *testing.B) {
			timer := newDSTimer(b)
			if reason := dsUnavailable(mode, workload); reason != "" {
				b.Skip("unavailable on this build: " + reason)
			}
			records := w1RecordsFor(b, workload)
			expected := dsExpected(workload, w1Sweeps)
			counts := dsCount(b, workload)
			var collections dsCollections
			gcOff := mode != "int"
			timer.start()
			for range b.N {
				r := newDSRun(b, workload, mode == "warm")
				runtime.GC()
				if gcOff {
					timer.gcOff(b)
				}
				timer.time(func() { r.apply(b) })
				dsCheckRun(b, r, expected)
				if mode == "nogc" {
					collections.measure(&r)
				} else {
					r.close()
				}
				if gcOff {
					timer.gcOn(b)
				}
			}
			timer.report("point", records.points(), gcOff)
			if mode == "nogc" {
				collections.report(b)
			}
			counts.report(b, len(records.changes))
		})
	}
}

// BenchmarkMetadataDSSplit (M3) times each record's decompression and
// decoding (D), resolution (R) and application (A) separately, with automatic
// GC off, so that the gaps between them hide no collection. The gaps check the
// series each record changed, against poison written over its memory.
func BenchmarkMetadataDSSplit(b *testing.B) {
	for _, workload := range dsWorkloads {
		b.Run("workload="+workload, func(b *testing.B) {
			timer := newDSTimer(b)
			if reason := dsUnavailable("split", workload); reason != "" {
				b.Skip("unavailable on this build: " + reason)
			}
			records := w1RecordsFor(b, workload)
			expected := dsExpected(workload, w1Sweeps)
			counts := dsCount(b, workload)
			var d, res, a time.Duration
			split := &dsSplit{}
			timer.start()
			for range b.N {
				r := newDSRun(b, workload, false)
				runtime.GC()
				timer.gcOff(b)
				timer.time(func() {
					for i, compressed := range r.records.compressed {
						t0 := w1CPU()
						rec, err := compression.Decode(compression.Snappy, compressed, r.decBuf)
						if err != nil {
							require.NoError(b, err)
						}
						r.decoder.decode(b, rec)
						t1 := w1CPU()
						split.resolve(r.decoder, r.qm, dsProcessIntern())
						t2 := w1CPU()
						split.apply(r.decoder, r.qm)
						t3 := w1CPU()
						d, res, a = d+t1-t0, res+t2-t1, a+t3-t2
						dsCheckRecord(b, r, i, rec, expected)
					}
				})
				dsCheckRun(b, r, expected)
				r.close()
				timer.gcOn(b)
			}
			units := float64(b.N) * float64(records.points())
			timer.report("point", records.points(), true)
			b.ReportMetric(float64(d.Nanoseconds())/units, "cpu-d-ns/point")
			b.ReportMetric(float64(res.Nanoseconds())/units, "cpu-r-ns/point")
			b.ReportMetric(float64(a.Nanoseconds())/units, "cpu-a-ns/point")
			b.ReportMetric(float64((d+res+a).Nanoseconds())/units, "cpu-dra-ns/point")
			counts.report(b, len(records.changes))
		})
	}
}

// dsCheckRecord poisons record i's memory, then checks, without allocating,
// that every series it changed holds its new value and none of the poison.
func dsCheckRecord(tb testing.TB, r *dsRun, i int, rec []byte, expected [][]metadata.Metadata) {
	refs := dsDecodedRefs(r.decoder)
	for j := range rec {
		rec[j] = dsPoison
	}
	step := 1 + i/(w1Series/w1Commit)
	var versions []dsVersion
	for _, ref := range refs {
		versions, _ = dsVersions(r.qm, ref, versions[:0])
		newest := versions[len(versions)-1].m
		if *newest != expected[step][ref-1] || strings.IndexByte(newest.Help, dsPoison) >= 0 || strings.IndexByte(newest.Unit, dsPoison) >= 0 {
			tb.Fatalf("record %d: series %d holds %+v, not %+v", i, ref, *newest, expected[step][ref-1])
		}
	}
}

// The stores-only kernels: Retained (M4) predecodes the records after the
// forced GC, as the W1 store component does; Collected (M5) predecodes them
// before it.

func BenchmarkMetadataDSRetained(b *testing.B)  { dsStores(b, "retained") }
func BenchmarkMetadataDSCollected(b *testing.B) { dsStores(b, "collected") }

func dsStores(b *testing.B, mode string) {
	for _, workload := range dsWorkloads {
		b.Run("workload="+workload, func(b *testing.B) {
			timer := newDSTimer(b)
			if reason := dsUnavailable(mode, workload); reason != "" {
				b.Skip("unavailable on this build: " + reason)
			}
			records := w1RecordsFor(b, workload)
			expected := dsExpected(workload, w1Sweeps)
			counts := dsCount(b, workload)
			timer.start()
			for range b.N {
				r := newDSRun(b, workload, false)
				if mode == "retained" {
					runtime.GC()
				}
				decoders := dsPredecode(b, records)
				if mode == "collected" {
					runtime.GC()
				}
				timer.time(func() {
					for _, d := range decoders {
						d.store(r.qm)
					}
				})
				dsCheckRun(b, r, expected)
				r.close()
			}
			timer.report("point", records.points(), false)
			counts.report(b, len(records.changes))
		})
	}
}

// dsPredecode decodes each change record from a buffer of its own, which its
// borrowed contents may alias.
func dsPredecode(tb testing.TB, records *w1Records) []*w1Decoder {
	decoders := make([]*w1Decoder, len(records.compressed))
	for i, compressed := range records.compressed {
		rec, err := compression.Decode(compression.Snappy, compressed, nil)
		require.NoError(tb, err)
		decoders[i] = newW1Decoder()
		decoders[i].decode(tb, rec)
	}
	return decoders
}

// BenchmarkMetadataDSReplay (M7) replays the values the store resolves, in its
// order, outside any store: fingerprints alone, owned copies alone, and the
// interner, fresh and warm.
func BenchmarkMetadataDSReplay(b *testing.B) {
	for _, workload := range dsWorkloads {
		b.Run("workload="+workload, func(b *testing.B) {
			timer := newDSTimer(b)
			if reason := dsUnavailable("replay", workload); reason != "" {
				b.Skip("unavailable on this build: " + reason)
			}
			dsReplay(b, timer, workload)
		})
	}
}

// TestMetadataDSEquivalence checks each variant's store against the
// integrated reference after every record: every series' history, which
// versions share an object, and the interner's state, with both interners
// sharing one fingerprint function. After each record, poison overwrites its
// memory in both, and every history string must stay free of it. The
// eviction case adds a fifth change sweep, so that histories evict and set
// their truncation flag.
func TestMetadataDSEquivalence(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	type tc struct {
		workload string
		extra    int
	}
	cases := []tc{{"db", 0}, {"sb", 0}, {"dv", 0}, {"db", 1}}
	for _, c := range cases {
		for _, variant := range []string{"split", "retained", "collected", "nogc", "warm"} {
			name := "workload=" + c.workload + "/variant=" + variant
			if c.extra > 0 {
				name = "eviction/" + name
			}
			t.Run(name, func(t *testing.T) {
				if reason := dsUnavailable(variant, c.workload); reason != "" {
					t.Skip("unavailable on this build: " + reason)
				}
				dsEquivalence(t, c.workload, c.extra, variant)
			})
		}
	}
}

func dsEquivalence(t *testing.T, workload string, extra int, variant string) {
	records := dsRecords(t, workload, extra)
	expected := dsExpected(workload, w1Sweeps+extra)
	refIntern, refHandle := dsNewInterner(nil)
	varIntern, varHandle := dsNewInterner(refHandle)
	if variant == "warm" {
		dsWarmInterner(refHandle)
		dsWarmInterner(varHandle)
	}
	refQM, varQM := w1NewQueueManager(t), w1NewQueueManager(t)
	for _, rec := range records.seed {
		for _, q := range []struct {
			qm     *QueueManager
			intern func(metadata.Metadata) *metadata.Metadata
		}{{refQM, refIntern}, {varQM, varIntern}} {
			d := newW1Decoder()
			d.decode(t, slices.Clone(rec))
			dsStoreWith(d, q.qm, q.intern)
		}
	}
	var predecoded []*w1Decoder
	var predecodedRecs [][]byte
	if variant == "retained" || variant == "collected" {
		for _, compressed := range records.compressed {
			rec, err := compression.Decode(compression.Snappy, compressed, nil)
			require.NoError(t, err)
			d := newW1Decoder()
			d.decode(t, rec)
			predecoded = append(predecoded, d)
			predecodedRecs = append(predecodedRecs, rec)
		}
	}
	refDecoder, varDecoder := newW1Decoder(), newW1Decoder()
	refBuf, varBuf := compression.NewSyncDecodeBuffer(), compression.NewSyncDecodeBuffer()
	split := &dsSplit{}
	poison := []byte{dsPoison}
	for i, compressed := range records.compressed {
		refRec, err := compression.Decode(compression.Snappy, compressed, refBuf)
		require.NoError(t, err)
		refDecoder.decode(t, refRec)
		dsStoreWith(refDecoder, refQM, refIntern)
		var varRec []byte
		switch variant {
		case "split":
			varRec, err = compression.Decode(compression.Snappy, compressed, varBuf)
			require.NoError(t, err)
			varDecoder.decode(t, varRec)
			split.resolve(varDecoder, varQM, varIntern)
			split.apply(varDecoder, varQM)
		case "retained", "collected":
			varRec = predecodedRecs[i]
			dsStoreWith(predecoded[i], varQM, varIntern)
		case "nogc":
			previous := debug.SetGCPercent(-1)
			varRec, err = compression.Decode(compression.Snappy, compressed, varBuf)
			require.NoError(t, err)
			varDecoder.decode(t, varRec)
			dsStoreWith(varDecoder, varQM, varIntern)
			debug.SetGCPercent(previous)
		case "warm":
			varRec, err = compression.Decode(compression.Snappy, compressed, varBuf)
			require.NoError(t, err)
			varDecoder.decode(t, varRec)
			dsStoreWith(varDecoder, varQM, varIntern)
		}
		for j := range refRec {
			refRec[j] = dsPoison
		}
		for j := range varRec {
			varRec[j] = dsPoison
		}
		dsCheckHistories(t, refQM, expected, dsStepAfter(i), poison)
		dsCheckHistories(t, varQM, expected, dsStepAfter(i), poison)
		require.NoError(t, dsStatesMismatch(refQM, varQM), "record %d", i)
		dsCompareInterners(t, refHandle, varHandle, poison)
	}
}

// TestMetadataDSProbes shows what each of the reclamation gate's probes
// observes: each case keeps one strong reference, releases everything else and
// collects, and only the probes observing what it keeps may survive.
func TestMetadataDSProbes(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	for _, c := range dsRetentionCases() {
		t.Run(c.name, func(t *testing.T) {
			r := newDSRun(t, "db", false)
			r.apply(t)
			probes := dsProbes(r.qm)
			want := c.keep(r)
			r.close()
			r = nil
			runtime.GC()
			var survivors []string
			for _, p := range probes {
				if p.alive() {
					survivors = append(survivors, p.name)
				}
			}
			require.ElementsMatch(t, want, survivors)
			dsRetained = nil
			runtime.GC()
			for _, p := range probes {
				require.False(t, p.alive(), "%s survives once nothing is retained", p.name)
			}
		})
	}
}

// dsRetained holds what a retention case keeps.
var dsRetained any

// dsRetentionCase keeps one reference to a run's state in dsRetained, and
// returns the probes that it keeps observing.
type dsRetentionCase struct {
	name string
	keep func(r *dsRun) []string
}

// TestMetadataDSModel checks that the model detects a changed interner, and
// that the counting run's counts agree with the model's.
func TestMetadataDSModel(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	for _, workload := range dsWorkloads {
		t.Run("workload="+workload, func(t *testing.T) {
			dsCount(t, workload)
			r := newDSRun(t, workload, true)
			defer r.close()
			r.apply(t)
			dsCheckRun(t, r, dsExpected(workload, w1Sweeps))
			dsCheckModelDetects(t, r)
		})
	}
}

// TestMetadataDSEquivalenceDetects checks that the equivalence comparisons
// catch broken variants: one that hands out fresh copies, breaking sharing;
// one that skips a record; and one that makes the interner sight an extra
// value.
func TestMetadataDSEquivalenceDetects(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	type broken struct {
		name, workload string
		// wrap changes the variant's intern; skip, if set, skips a record.
		wrap     func(func(metadata.Metadata) *metadata.Metadata) func(metadata.Metadata) *metadata.Metadata
		skip     int
		interner bool
	}
	cases := []broken{
		{name: "fresh copies", workload: "sb", wrap: func(intern func(metadata.Metadata) *metadata.Metadata) func(metadata.Metadata) *metadata.Metadata {
			return func(m metadata.Metadata) *metadata.Metadata { c := *intern(m); return &c }
		}, interner: true},
		{name: "a skipped record", workload: "db", skip: 5},
		{name: "an extra sighting", workload: "db", wrap: func(intern func(metadata.Metadata) *metadata.Metadata) func(metadata.Metadata) *metadata.Metadata {
			return func(m metadata.Metadata) *metadata.Metadata {
				intern(metadata.Metadata{Help: "extra"})
				return intern(m)
			}
		}, interner: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			refIntern, refHandle := dsNewInterner(nil)
			varIntern, varHandle := dsNewInterner(refHandle)
			if c.interner && refHandle == nil {
				t.Skip("unavailable on this build: no WAL metadata interner")
			}
			if c.wrap != nil {
				varIntern = c.wrap(varIntern)
			}
			records := w1RecordsFor(t, c.workload)
			refQM, varQM := w1NewQueueManager(t), w1NewQueueManager(t)
			store := func(rec []byte, qm *QueueManager, intern func(metadata.Metadata) *metadata.Metadata) {
				d := newW1Decoder()
				d.decode(t, slices.Clone(rec))
				dsStoreWith(d, qm, intern)
			}
			for _, rec := range records.seed {
				store(rec, refQM, refIntern)
				store(rec, varQM, varIntern)
			}
			detected := false
			for i, rec := range records.changes {
				store(rec, refQM, refIntern)
				if c.skip == 0 || i != c.skip {
					store(rec, varQM, varIntern)
				}
				err := dsStatesMismatch(refQM, varQM)
				if err == nil && refHandle != nil {
					err = dsInternerMismatch(refHandle, varHandle)
				}
				if err != nil {
					t.Logf("record %d: %v", i, err)
					detected = true
					break
				}
			}
			require.True(t, detected, "the comparisons missed the broken variant")
		})
	}
}

// TestMetadataDSCollections checks the reclamation gate as M2 applies it: it
// verifies a fully released run, and reports a run whose queue is kept.
func TestMetadataDSCollections(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("kept=%t", keep), func(t *testing.T) {
			r := newDSRun(t, "db", false)
			r.apply(t)
			if keep {
				dsRetained = r.qm
			}
			previous := debug.SetGCPercent(-1)
			var c dsCollections
			c.measure(&r)
			debug.SetGCPercent(previous)
			dsRetained = nil
			require.Nil(t, r)
			if keep {
				require.Positive(t, c.unverified)
			} else {
				require.Zero(t, c.unverified)
			}
		})
	}
}

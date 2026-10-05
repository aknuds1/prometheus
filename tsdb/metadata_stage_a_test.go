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

package tsdb

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/tsdb/wlog"
	"github.com/prometheus/prometheus/util/compression"
)

// The stage A writer screen commits the pipeline fixture's transactions to a
// fresh database in every iteration; the record, clone and history screens
// time single stages. Base-specific parts are in
// metadata_stage_a_adapter_test.go; this file is identical on every base.

const (
	stageASeries   = 10000
	stageACommit   = 1000
	stageAVersions = 4
	// A fixed base keeps chunk boundaries independent of the time of day.
	stageABase = 1767225600000 // 2026-01-01T00:00:00Z.
)

// stageALabels and stageAMetadata reproduce the pipeline fixture's series.
func stageALabels(id int) labels.Labels {
	return labels.FromStrings(labels.MetricName, "pipeline_seconds_total", "id", strconv.Itoa(id),
		"job", "pipeline", "cluster", "benchmark", "region", "local", "environment", "test")
}

func stageAMetadata(values, slot, version int) metadata.Metadata {
	prefix := fmt.Sprintf("family %d version %d ", slot%values, version)
	return metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: prefix + strings.Repeat("x", 64-len(prefix))}
}

func stageATimestamp(step int) int64 { return stageABase + int64(step)*15000 }

type stageAWriterCase struct {
	mode     string // native or wal.
	workload string // sb and db change every step; u and d never do.
}

func (c stageAWriterCase) name() string { return "mode=" + c.mode + "/workload=" + c.workload }

func (c stageAWriterCase) values() int {
	if c.workload == "sb" || c.workload == "u" {
		return 100
	}
	return stageASeries
}

func (c stageAWriterCase) changes() bool { return c.workload == "sb" || c.workload == "db" }

func stageAWriterCases() []stageAWriterCase {
	var cases []stageAWriterCase
	for _, mode := range []string{"native", "wal"} {
		for _, w := range []string{"sb", "db", "u", "d"} {
			cases = append(cases, stageAWriterCase{mode, w})
		}
	}
	return cases
}

// stageAWriterInput holds a workload's labels and its metadata by slot and
// version, built once and never modified, as a scrape cache would hold them.
type stageAWriterInput struct {
	labels   []labels.Labels
	metadata [][]metadata.Metadata
}

var (
	stageAInputsMtx sync.Mutex
	stageAInputs    = map[int]*stageAWriterInput{}
)

func stageAInputFor(values int) *stageAWriterInput {
	stageAInputsMtx.Lock()
	defer stageAInputsMtx.Unlock()
	if in := stageAInputs[values]; in != nil {
		return in
	}
	in := &stageAWriterInput{}
	for slot := range stageASeries {
		in.labels = append(in.labels, stageALabels(slot))
		versions := make([]metadata.Metadata, stageAVersions+1)
		for v := range versions {
			versions[v] = stageAMetadata(values, slot, v)
		}
		in.metadata = append(in.metadata, versions)
	}
	stageAInputs[values] = in
	return in
}

type stageAWriterResult struct {
	depth int
	// The WAL's metadata records, seed included: their uncompressed payload
	// bytes, their entries, and a digest of their bytes in WAL order.
	walMetadataBytes, walMetadataEntries int
	walMetadataDigest                    uint64
}

// stageARunWriter runs one iteration of c against a fresh database: the seed
// commits outside the timed region, then four sweeps of 1,000-series commits
// inside it.
func stageARunWriter(tb testing.TB, c stageAWriterCase, timed func(work func())) stageAWriterResult {
	in := stageAInputFor(c.values())
	dir := tb.TempDir()
	defer os.RemoveAll(dir)
	opts := DefaultOptions()
	opts.EnableNativeMetadata = c.mode == "native"
	opts.EnableMetadataWALRecords = c.mode == "wal"
	opts.WALCompression = compression.Snappy
	db, err := Open(dir, nil, nil, opts, nil)
	require.NoError(tb, err)
	db.DisableCompactions()
	ctx := context.Background()
	refs := make([]storage.SeriesRef, stageASeries)
	sweep := func(step, version int) {
		for offset := 0; offset < stageASeries; offset += stageACommit {
			app := db.AppenderV2(ctx)
			for slot := offset; slot < offset+stageACommit; slot++ {
				refs[slot], err = app.Append(refs[slot], in.labels[slot], 0, stageATimestamp(step), float64(step+1), nil, nil, storage.AOptions{Metadata: in.metadata[slot][version]})
				require.NoError(tb, err)
			}
			require.NoError(tb, app.Commit())
		}
	}
	sweep(0, 0)
	runtime.GC()
	timed(func() {
		for step := 1; step <= stageAVersions; step++ {
			version := 0
			if c.changes() {
				version = step
			}
			sweep(step, version)
		}
	})

	last := 0
	if c.changes() {
		last = stageAVersions
	}
	r := stageAWriterResult{depth: last + 1}
	if c.mode == "native" {
		series, _, err := db.NativeMetricMetadata(ctx, [][]*labels.Matcher{{labels.MustNewMatcher(labels.MatchEqual, "job", "pipeline")}}, 0)
		require.NoError(tb, err)
		require.Len(tb, series, stageASeries)
		for _, s := range series {
			slot, err := strconv.Atoi(s.Labels.Get("id"))
			require.NoError(tb, err)
			require.Len(tb, s.Versions, r.depth, "series %d", slot)
			newest := s.Versions[len(s.Versions)-1]
			require.Equal(tb, in.metadata[slot][last], newest.Metadata, "series %d", slot)
			require.Equal(tb, stageATimestamp(last), newest.EffectiveFrom, "series %d", slot)
		}
	}
	require.NoError(tb, db.Close())
	records := stageAWALMetadata(tb, filepath.Join(dir, "wal"))
	r.walMetadataBytes, r.walMetadataEntries, r.walMetadataDigest, err = stageACheckWALMetadata(tb, c, in, refs, records)
	require.NoError(tb, err)
	return r
}

// stageAWALMetadata returns copies of the WAL's metadata records in order.
func stageAWALMetadata(tb testing.TB, dir string) [][]byte {
	segments, err := wlog.NewSegmentsReader(dir)
	require.NoError(tb, err)
	defer segments.Close()
	var dec record.Decoder
	var records [][]byte
	r := wlog.NewReader(segments)
	for r.Next() {
		if rec := r.Record(); dec.Type(rec) == record.Metadata {
			records = append(records, slices.Clone(rec))
		}
	}
	require.NoError(tb, r.Err())
	return records
}

// stageAStart is a native metadata entry's series, help and start.
type stageAStart struct {
	ref  chunks.HeadSeriesRef
	help string
	from int64
}

// stageACheckWALMetadata checks the WAL's metadata records against the
// fixture: every change point the base logs appears exactly once, as legacy
// readers see it, each record holds one step, and records follow step order.
// Native records must also carry each step's start. It returns the records'
// payload bytes, their entries, and the top 48 bits of a SHA-256 digest of
// their bytes in order, which a float64 metric holds exactly.
func stageACheckWALMetadata(tb testing.TB, c stageAWriterCase, in *stageAWriterInput, refs []storage.SeriesRef, records [][]byte) (int, int, uint64, error) {
	type change struct {
		ref chunks.HeadSeriesRef
		m   metadata.Metadata
	}
	steps := map[change]int{}
	if c.mode == "wal" || stageANativeLogsMetadata() {
		last := 0
		if c.changes() {
			last = stageAVersions
		}
		for step := 0; step <= last; step++ {
			for slot := range stageASeries {
				steps[change{chunks.HeadSeriesRef(refs[slot]), in.metadata[slot][step]}] = step
			}
		}
	}
	want := maps.Clone(steps)
	digest := sha256.New()
	var dec record.Decoder
	bytes, entries, previous := 0, 0, 0
	for _, rec := range records {
		bytes += len(rec)
		digest.Write(binary.BigEndian.AppendUint64(nil, uint64(len(rec))))
		digest.Write(rec)
		meta, err := dec.Metadata(rec, nil)
		if err != nil {
			return 0, 0, 0, err
		}
		step := -1
		for _, e := range meta {
			key := change{e.Ref, metadata.Metadata{Type: record.ToMetricType(e.Type), Unit: e.Unit, Help: e.Help}}
			s, ok := want[key]
			if !ok {
				return 0, 0, 0, fmt.Errorf("unexpected or repeated metadata entry %+v", e)
			}
			delete(want, key)
			if step < 0 {
				step = s
			}
			if s != step {
				return 0, 0, 0, fmt.Errorf("a record mixes steps %d and %d", step, s)
			}
			entries++
		}
		if step < previous {
			return 0, 0, 0, fmt.Errorf("a record of step %d follows step %d", step, previous)
		}
		previous = step
		if c.mode == "native" {
			starts := stageANativeRecordStarts(tb, rec)
			if len(starts) != len(meta) {
				return 0, 0, 0, fmt.Errorf("%d native entries, %d legacy ones", len(starts), len(meta))
			}
			for i, start := range starts {
				key := change{start.ref, metadata.Metadata{Type: record.ToMetricType(meta[i].Type), Unit: meta[i].Unit, Help: start.help}}
				if from := stageATimestamp(steps[key]); start.from != from {
					return 0, 0, 0, fmt.Errorf("series %d starts at %d, not %d", start.ref, start.from, from)
				}
			}
		}
	}
	if len(want) > 0 {
		return 0, 0, 0, fmt.Errorf("%d metadata entries missing", len(want))
	}
	return bytes, entries, binary.BigEndian.Uint64(digest.Sum(nil)) >> 16, nil
}

func BenchmarkMetadataStageAWriter(b *testing.B) {
	for _, c := range stageAWriterCases() {
		b.Run(c.name(), func(b *testing.B) {
			timer := stageALeaf(b)
			timer.start()
			var r stageAWriterResult
			for range b.N {
				r = stageARunWriter(b, c, timer.time)
			}
			timer.report("sample", stageAVersions*stageASeries)
			b.ReportMetric(float64(r.depth), "depth")
			b.ReportMetric(float64(r.walMetadataBytes), "wal-metadata-B")
			b.ReportMetric(float64(r.walMetadataEntries), "wal-metadata-entries")
			b.ReportMetric(float64(r.walMetadataDigest), "wal-metadata-digest48")
		})
	}
}

// BenchmarkMetadataStageAPreLog times P2a's pre-log pass over one prepared
// transaction: 1,000 series, each with a new version pending.
func BenchmarkMetadataStageAPreLog(b *testing.B) {
	for _, workload := range []string{"sb", "db"} {
		b.Run("workload="+workload, func(b *testing.B) {
			timer := stageALeaf(b)
			run, done, reason := stageAPreparePreLog(b, stageAWriterCase{mode: "native", workload: workload})
			if run == nil {
				b.Skip("unavailable on this base: " + reason)
			}
			defer done()
			timer.start()
			for range b.N {
				runtime.GC()
				timer.time(func() { require.Equal(b, stageACommit, run()) })
			}
			timer.report("entry", stageACommit)
		})
	}
}

type stageARecordCase struct{ record, stage, workload string }

func (c stageARecordCase) name() string {
	return "record=" + c.record + "/stage=" + c.stage + "/workload=" + c.workload
}

func stageARecordCases() []stageARecordCase {
	var cases []stageARecordCase
	for _, rec := range []string{"legacy", "native"} {
		for _, stage := range []string{"encode", "snappy-encode", "snappy-decode"} {
			for _, w := range []string{"sb", "db"} {
				cases = append(cases, stageARecordCase{rec, stage, w})
			}
		}
	}
	return cases
}

// stageAChangeRecord returns one commit of version 1 as a record: entries
// for 1,000 consecutive slots.
func stageAChangeRecord(rec, workload string, buf []byte) ([]byte, bool) {
	values := 100
	if workload == "db" {
		values = stageASeries
	}
	points := make([]stageAChange, stageACommit)
	for slot := range stageACommit {
		points[slot] = stageAChange{ref: chunks.HeadSeriesRef(slot + 1), from: stageATimestamp(1), m: stageAMetadata(values, slot, 1)}
	}
	if rec == "native" {
		return stageAEncodeNativeRecord(points, buf)
	}
	entries := make([]record.RefMetadata, len(points))
	for i, p := range points {
		entries[i] = record.RefMetadata{Ref: p.ref, Type: record.GetMetricType(p.m.Type), Unit: p.m.Unit, Help: p.m.Help}
	}
	var enc record.Encoder
	return enc.Metadata(entries, buf), true
}

type stageAChange struct {
	ref  chunks.HeadSeriesRef
	from int64
	m    metadata.Metadata
}

// BenchmarkMetadataStageARecord times encoding one commit's metadata record
// and compressing and decompressing it, as the WAL does.
func BenchmarkMetadataStageARecord(b *testing.B) {
	for _, c := range stageARecordCases() {
		b.Run(c.name(), func(b *testing.B) {
			timer := stageALeaf(b)
			rec, ok := stageAChangeRecord(c.record, c.workload, nil)
			if !ok {
				b.Skip("unavailable on this base: no native metadata records")
			}
			encoded := compression.NewSyncEncodeBuffer()
			compressed, err := compression.Encode(compression.Snappy, rec, encoded)
			require.NoError(b, err)
			compressed = append([]byte(nil), compressed...)
			var buf []byte
			var work func()
			switch c.stage {
			case "encode":
				// Building the entries is setup; only encoding them is timed.
				work = stageAEncodeWork(c, &buf)
			case "snappy-encode":
				work = func() { _, err = compression.Encode(compression.Snappy, rec, encoded) }
			case "snappy-decode":
				// The WAL readers reuse one decode buffer.
				decoded := compression.NewSyncDecodeBuffer()
				work = func() { buf, err = compression.Decode(compression.Snappy, compressed, decoded) }
			}
			timer.start()
			for range b.N {
				timer.time(work)
				require.NoError(b, err)
			}
			timer.report("entry", stageACommit)
			b.ReportMetric(float64(len(rec)), "record-B")
			b.ReportMetric(float64(len(compressed)), "compressed-B")
		})
	}
}

// stageAEncodeWork returns a function encoding c's prepared entries into buf.
func stageAEncodeWork(c stageARecordCase, buf *[]byte) func() {
	values := 100
	if c.workload == "db" {
		values = stageASeries
	}
	points := make([]stageAChange, stageACommit)
	for slot := range stageACommit {
		points[slot] = stageAChange{ref: chunks.HeadSeriesRef(slot + 1), from: stageATimestamp(1), m: stageAMetadata(values, slot, 1)}
	}
	if c.record == "native" {
		return stageANativeEncodeWork(points, buf)
	}
	entries := make([]record.RefMetadata, len(points))
	for i, p := range points {
		entries[i] = record.RefMetadata{Ref: p.ref, Type: record.GetMetricType(p.m.Type), Unit: p.m.Unit, Help: p.m.Help}
	}
	var enc record.Encoder
	return func() { *buf = enc.Metadata(entries, (*buf)[:0]) }
}

// stageACloneShapes are the ownership shapes a native metadata clone handles,
// each from caller strings that a large backing could retain.
func stageACloneShapes() map[string][]metadata.Metadata {
	backing := strings.Repeat("padding", 100_000)
	shapes := map[string][]metadata.Metadata{}
	for i := range stageACommit {
		help := stageAMetadata(stageASeries, i, 1).Help
		custom := "custom" + strconv.Itoa(i)
		shapes["distinct"] = append(shapes["distinct"], metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: help})
		shapes["empty-unit"] = append(shapes["empty-unit"], metadata.Metadata{Type: model.MetricTypeCounter, Help: help})
		shapes["empty-help"] = append(shapes["empty-help"], metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds"})
		shapes["empty-both"] = append(shapes["empty-both"], metadata.Metadata{Type: model.MetricTypeCounter})
		shapes["unknown-type"] = append(shapes["unknown-type"], metadata.Metadata{Type: model.MetricType(custom), Unit: "seconds", Help: help})
		padded := "counter" + "seconds" + help + backing
		shapes["padded"] = append(shapes["padded"], metadata.Metadata{Type: model.MetricType(padded[:7]), Unit: padded[7:14], Help: padded[14 : 14+len(help)]})
	}
	return shapes
}

func BenchmarkMetadataStageAClone(b *testing.B) {
	shapes := stageACloneShapes()
	for _, shape := range []string{"distinct", "empty-unit", "empty-help", "empty-both", "unknown-type", "padded"} {
		b.Run("shape="+shape, func(b *testing.B) {
			timer := stageALeaf(b)
			inputs := shapes[shape]
			clones := make([]*metadata.Metadata, len(inputs))
			timer.start()
			for range b.N {
				clear(clones)
				runtime.GC()
				timer.time(func() {
					for i, m := range inputs {
						clones[i] = cloneNativeMetricMetadata(m)
					}
				})
			}
			for i, m := range inputs {
				require.Equal(b, m, *clones[i])
			}
			timer.report("clone", len(inputs))
		})
	}
}

// BenchmarkMetadataStageAHistoryDepth grows 10,000 histories to a depth and
// reports their older points' capacity and the live heap they retain.
func BenchmarkMetadataStageAHistoryDepth(b *testing.B) {
	const histories = 10000
	values := make([]*metadata.Metadata, stageAVersions+1)
	for v := range values {
		m := stageAMetadata(1, 0, v)
		values[v] = &m
	}
	for depth := 1; depth <= stageAVersions+1; depth++ {
		b.Run("depth="+strconv.Itoa(depth), func(b *testing.B) {
			timer := stageALeaf(b)
			timer.start()
			var capacity, retained float64
			for range b.N {
				h := stageANewHistories(histories)
				before := stageALiveHeap()
				timer.time(func() {
					for i := range histories {
						for v := range depth {
							h.merge(i, stageATimestamp(v), values[v])
						}
					}
				})
				after := stageALiveHeap()
				require.Equal(b, depth, h.depth(0))
				capacity = float64(h.olderCapacityBytes(0))
				retained = float64(after-before) / histories
				runtime.KeepAlive(h)
			}
			timer.report("history", histories)
			b.ReportMetric(capacity, "older-capacity-B/history")
			b.ReportMetric(retained, "retained-B/history")
		})
	}
}

// stageAHistories is a base's per-series native metadata histories.
type stageAHistories interface {
	merge(i int, from int64, m *metadata.Metadata)
	depth(i int) int
	olderCapacityBytes(i int) int
}

// stageACPU returns the process CPU time. It covers all threads, including
// background GC, so a timed window measures process CPU during the window,
// not CPU attributable to the operation alone.
func stageACPU() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_PROCESS_CPUTIME_ID, &ts); err != nil {
		panic(err)
	}
	return time.Duration(ts.Nano())
}

// stageAClockOverhead returns the CPU time of one clock read.
func stageAClockOverhead() float64 {
	const reads = 1000
	start := stageACPU()
	for range reads {
		stageACPU()
	}
	return float64((stageACPU() - start).Nanoseconds()) / (reads + 1)
}

func stageAMetric(name string) metrics.Value {
	sample := []metrics.Sample{{Name: name}}
	metrics.Read(sample)
	return sample[0].Value
}

func stageAGCCycles() uint64 { return stageAMetric("/gc/cycles/total:gc-cycles").Uint64() }

// stageALiveHeap returns the heap marked live by a forced collection.
func stageALiveHeap() int64 {
	runtime.GC()
	return int64(stageAMetric("/gc/heap/live:bytes").Uint64())
}

// stageATimer runs timed work under b's timer and accumulates its process CPU
// and GC cycles. The CPU reads sit inside StartTimer and StopTimer, which read
// memory statistics, so neither those reads, setup nor checks are counted.
type stageATimer struct {
	b   *testing.B
	cpu time.Duration
	gcs uint64
}

// stageALeaf stops b's timer on entering a leaf benchmark. Go starts the timer
// before running the body, so setup would otherwise count towards elapsed
// time and allocations.
func stageALeaf(b *testing.B) *stageATimer {
	b.StopTimer()
	return &stageATimer{b: b}
}

// start zeroes the elapsed time and allocation counters once setup is done.
// The timer stays stopped; time runs it around each iteration's work.
func (t *stageATimer) start() {
	t.b.ReportAllocs()
	t.b.ResetTimer()
}

func (t *stageATimer) time(work func()) {
	gcs := stageAGCCycles()
	t.b.StartTimer()
	start := stageACPU()
	work()
	end := stageACPU()
	t.b.StopTimer()
	t.cpu += end - start
	t.gcs += stageAGCCycles() - gcs
}

// report reports per-unit CPU, elapsed time, GC cycles and clock overhead.
// Allocation figures are per iteration; units per iteration convert them.
func (t *stageATimer) report(unit string, perIteration int) {
	units := float64(t.b.N) * float64(perIteration)
	t.b.ReportMetric(float64(t.cpu.Nanoseconds())/units, "cpu-ns/"+unit)
	t.b.ReportMetric(float64(t.b.Elapsed().Nanoseconds())/units, "elapsed-ns/"+unit)
	t.b.ReportMetric(float64(perIteration), unit+"s/op")
	t.b.ReportMetric(float64(t.gcs)/float64(t.b.N), "gcs/op")
	t.b.ReportMetric(stageAClockOverhead(), "clock-ns/read")
}

// TestMetadataStageA checks that every available stage A case starts each
// iteration from its registered state and does identical work in each. It
// reports no performance figures.
func TestMetadataStageA(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	direct := func(work func()) { work() }
	for _, c := range stageAWriterCases() {
		t.Run(c.name(), func(t *testing.T) {
			first := stageARunWriter(t, c, direct)
			t.Logf("depth %d, WAL metadata payload %d B in %d entries, digest %x", first.depth, first.walMetadataBytes, first.walMetadataEntries, first.walMetadataDigest)
			for range 2 {
				require.Equal(t, first, stageARunWriter(t, c, direct))
			}
			switch {
			case c.mode == "native" && !stageANativeLogsMetadata():
				require.Zero(t, first.walMetadataEntries, "this base keeps native metadata out of the WAL")
			case c.changes():
				require.Equal(t, (stageAVersions+1)*stageASeries, first.walMetadataEntries)
			default:
				require.Equal(t, stageASeries, first.walMetadataEntries, "unchanged metadata is logged once")
			}
		})
	}
	for _, workload := range []string{"sb", "db"} {
		t.Run("pre-log/workload="+workload, func(t *testing.T) {
			run, done, _ := stageAPreparePreLog(t, stageAWriterCase{mode: "native", workload: workload})
			if run == nil {
				t.Skip("unavailable on this base")
			}
			defer done()
			for range 3 {
				require.Equal(t, stageACommit, run(), "every series has an unstable group")
			}
		})
	}
	for _, c := range stageARecordCases() {
		t.Run(c.name(), func(t *testing.T) {
			rec, ok := stageAChangeRecord(c.record, c.workload, nil)
			if !ok {
				t.Skip("unavailable on this base")
			}
			var buf []byte
			stageAEncodeWork(c, &buf)()
			require.Equal(t, rec, buf)
			compressed, err := compression.Encode(compression.Snappy, rec, nil)
			require.NoError(t, err)
			decoded, err := compression.Decode(compression.Snappy, compressed, nil)
			require.NoError(t, err)
			require.Equal(t, rec, decoded)
		})
	}
	t.Run("clone shapes", func(t *testing.T) {
		for shape, inputs := range stageACloneShapes() {
			for _, m := range inputs[:10] {
				require.Equal(t, m, *cloneNativeMetricMetadata(m), shape)
			}
		}
	})
	t.Run("history depth", func(t *testing.T) {
		values := make([]*metadata.Metadata, stageAVersions+1)
		for v := range values {
			m := stageAMetadata(1, 0, v)
			values[v] = &m
		}
		for range 2 {
			h := stageANewHistories(1)
			var capacities []int
			for v := range values {
				h.merge(0, stageATimestamp(v), values[v])
				require.Equal(t, v+1, h.depth(0))
				capacities = append(capacities, h.olderCapacityBytes(0))
			}
			t.Logf("older capacity bytes by depth %v", capacities)
		}
	})
	t.Run("the WAL contents oracle rejects altered records", func(t *testing.T) {
		c := stageAWriterCase{mode: "wal", workload: "sb"}
		in := stageAInputFor(c.values())
		refs := make([]storage.SeriesRef, stageASeries)
		var enc record.Encoder
		encode := func(step, offset int, help func(slot int) string) []byte {
			var entries []record.RefMetadata
			for slot := offset; slot < offset+stageACommit; slot++ {
				refs[slot] = storage.SeriesRef(slot + 1)
				m := in.metadata[slot][step]
				entries = append(entries, record.RefMetadata{Ref: chunks.HeadSeriesRef(slot + 1), Type: record.GetMetricType(m.Type), Unit: m.Unit, Help: help(slot)})
			}
			return enc.Metadata(entries, nil)
		}
		var records [][]byte
		for step := 0; step <= stageAVersions; step++ {
			for offset := 0; offset < stageASeries; offset += stageACommit {
				records = append(records, encode(step, offset, func(slot int) string { return in.metadata[slot][step].Help }))
			}
		}
		_, entries, _, err := stageACheckWALMetadata(t, c, in, refs, records)
		require.NoError(t, err)
		require.Equal(t, (stageAVersions+1)*stageASeries, entries)
		altered := slices.Clone(records)
		altered[3] = encode(0, 3*stageACommit, func(slot int) string { return in.metadata[slot][1].Help })
		for name, records := range map[string][][]byte{
			"a missing record":          records[:len(records)-1],
			"a repeated record":         append(slices.Clone(records), records[0]),
			"records out of step order": append(slices.Clone(records[10:]), records[:10]...),
			"an altered entry":          altered,
		} {
			_, _, _, err := stageACheckWALMetadata(t, c, in, refs, records)
			require.Error(t, err, name)
		}
	})
	t.Run("setup is not counted", func(t *testing.T) {
		benchtime := flag.Lookup("test.benchtime").Value
		saved := benchtime.String()
		require.NoError(t, benchtime.Set("10x"))
		defer func() { require.NoError(t, benchtime.Set(saved)) }()
		var sink [][]byte
		allocate := func(n int) {
			for range n {
				sink = append(sink, make([]byte, 64))
			}
		}
		// A leaf that allocates in its setup and before each iteration's
		// timed work, but not in the timed work itself.
		leaf := func(stopOnEntry bool) testing.BenchmarkResult {
			return testing.Benchmark(func(b *testing.B) {
				timer := &stageATimer{b: b}
				if stopOnEntry {
					timer = stageALeaf(b)
				}
				allocate(1000)
				timer.start()
				for range b.N {
					allocate(10)
					timer.time(func() {})
				}
			})
		}
		counted := leaf(true)
		require.Equal(t, 10, counted.N)
		require.Zero(t, counted.MemAllocs, "setup allocations are not counted")
		// Zeroing the counters alone leaves the timer running into the first
		// iteration's setup.
		require.GreaterOrEqual(t, leaf(false).MemAllocs, uint64(10))
		// The pattern this replaced counted the setup before its first
		// StopTimer.
		replaced := testing.Benchmark(func(b *testing.B) {
			allocate(1000)
			b.ReportAllocs()
			for range b.N {
				b.StopTimer()
				b.StartTimer()
			}
		})
		require.GreaterOrEqual(t, replaced.MemAllocs, uint64(1000))
		runtime.KeepAlive(sink)
	})
	t.Run("the CPU clock", func(t *testing.T) {
		require.Positive(t, stageAClockOverhead())
		if runtime.GOOS == "linux" {
			require.Zero(t, testing.AllocsPerRun(100, func() { stageACPU() }))
		}
	})
}

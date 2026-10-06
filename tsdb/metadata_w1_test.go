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
	"errors"
	"fmt"
	"math"
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

// The W1 screen's writer kernels: PW builds, encodes and compresses one
// sweep's merge group records from staged transactions; its components time
// each of those stages alone; and G1 commits unchanged metadata, log()
// included. Base-specific parts are in metadata_w1_adapter_test.go; this file
// is identical on every build.

const (
	w1Series = 10000
	w1Commit = 1000
	w1Sweeps = 4
	// The multi-point probe: 10 commits of 100 groups of 10 points.
	w1MPSeries = 1000
	w1MPCommit = 100
	w1MPPoints = 10
	// A fixed base keeps chunk boundaries independent of the time of day.
	w1Base = 1767225600000 // 2026-01-01T00:00:00Z.
)

// Changing workloads: sb has 1,000 single-point groups over 100 values per
// record, db 1,000 distinct values sharing type and unit, and mp 100 groups of
// 10 points over 2 values each. G1's unchanged workloads u and d repeat sb's
// and db's seed values.
var (
	w1Workloads         = []string{"sb", "db", "mp"}
	w1UnchangedWorkload = map[string]string{"u": "sb", "d": "db"}
)

func w1Metadata(workload string, slot, version int) metadata.Metadata {
	family := slot
	if workload == "sb" {
		family = slot % 100
	}
	prefix := fmt.Sprintf("family %d version %d ", family, version)
	return metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: prefix + strings.Repeat("x", 64-len(prefix))}
}

func w1Labels(slot int) labels.Labels {
	return labels.FromStrings(labels.MetricName, "w1_seconds_total", "id", strconv.Itoa(slot), "job", "w1")
}

func w1Timestamp(step int) int64 { return w1Base + int64(step)*15000 }

// w1Observation is one sample's metadata in a transaction.
type w1Observation struct {
	slot int
	ts   int64
	m    metadata.Metadata
}

// w1Input holds a workload's labels and transactions, built once and never
// modified, as a scrape cache holds its strings. sweeps[step][commit] holds a
// transaction's observations in append order; sweep 0 seeds every series.
type w1Input struct {
	workload string
	labels   []labels.Labels
	sweeps   [][][]w1Observation
}

func (in *w1Input) series() int { return len(in.labels) }

// points returns the observations of one sweep after the seed.
func (in *w1Input) points() int {
	n := 0
	for _, txn := range in.sweeps[1] {
		n += len(txn)
	}
	return n
}

var (
	w1InputsMtx sync.Mutex
	w1Inputs    = map[string]*w1Input{}
)

func w1InputFor(workload string) *w1Input {
	w1InputsMtx.Lock()
	defer w1InputsMtx.Unlock()
	if in := w1Inputs[workload]; in != nil {
		return in
	}
	in := &w1Input{workload: workload}
	series, commit := w1Series, w1Commit
	if workload == "mp" {
		series, commit = w1MPSeries, w1MPCommit
	}
	for slot := range series {
		in.labels = append(in.labels, w1Labels(slot))
	}
	// Values are built once per slot and version, so that observations of one
	// value share its strings.
	values := map[[2]int]metadata.Metadata{}
	value := func(slot, version int) metadata.Metadata {
		key := [2]int{slot, version}
		if workload == "sb" {
			key[0] %= 100
		}
		m, ok := values[key]
		if !ok {
			m = w1Metadata(workload, slot, version)
			values[key] = m
		}
		return m
	}
	for step := 0; step <= w1Sweeps; step++ {
		var sweep [][]w1Observation
		for offset := 0; offset < series; offset += commit {
			var txn []w1Observation
			for slot := offset; slot < offset+commit; slot++ {
				if workload != "mp" || step == 0 {
					txn = append(txn, w1Observation{slot: slot, ts: w1Timestamp(step), m: value(slot, step)})
					continue
				}
				for i := range w1MPPoints {
					version := 2*step - 1
					if i >= w1MPPoints/2 {
						version = 2 * step
					}
					txn = append(txn, w1Observation{slot: slot, ts: w1Timestamp(step) + int64(i), m: value(slot, version)})
				}
			}
			sweep = append(sweep, txn)
		}
		in.sweeps = append(in.sweeps, sweep)
	}
	w1Inputs[workload] = in
	return in
}

// w1Point and w1Group are a merge group, independently of any build's
// record types.
type (
	w1Point struct {
		from int64
		m    metadata.Metadata
	}
	w1Group struct {
		ref    chunks.HeadSeriesRef
		points []w1Point
	}
)

// w1Groups returns the merge groups a transaction logs against committed seed
// state, by ref.
func w1Groups(txn []w1Observation, refs []storage.SeriesRef) map[chunks.HeadSeriesRef]w1Group {
	out := map[chunks.HeadSeriesRef]w1Group{}
	for _, o := range txn {
		ref := chunks.HeadSeriesRef(refs[o.slot])
		g := out[ref]
		g.ref = ref
		g.points = append(g.points, w1Point{from: o.ts, m: o.m})
		out[ref] = g
	}
	return out
}

// w1OpenDB returns a database in mode, native or wal, with every series of in
// committed at its seed value, and the series' refs by slot.
func w1OpenDB(tb testing.TB, mode string, in *w1Input) (*DB, []storage.SeriesRef) {
	dir := tb.TempDir()
	opts := DefaultOptions()
	opts.EnableNativeMetadata = mode == "native"
	opts.EnableMetadataWALRecords = mode == "wal"
	opts.WALCompression = compression.Snappy
	db, err := Open(dir, nil, nil, opts, nil)
	require.NoError(tb, err)
	db.DisableCompactions()
	refs := make([]storage.SeriesRef, in.series())
	for _, txn := range in.sweeps[0] {
		w1CommitTransaction(tb, db, in, refs, txn, 0)
	}
	return db, refs
}

// w1CommitTransaction appends and commits txn, at its own timestamps or, with
// step above 0, at that step's.
func w1CommitTransaction(tb testing.TB, db *DB, in *w1Input, refs []storage.SeriesRef, txn []w1Observation, step int) {
	app := db.AppenderV2(context.Background())
	for _, o := range txn {
		ts := o.ts
		if step > 0 {
			ts = w1Timestamp(step)
		}
		var err error
		refs[o.slot], err = app.Append(refs[o.slot], in.labels[o.slot], 0, ts, float64(ts), nil, nil, storage.AOptions{Metadata: o.m})
		if err != nil {
			require.NoError(tb, errors.Join(err, app.Rollback()))
		}
	}
	require.NoError(tb, app.Commit())
}

// w1Digest48 returns 48 bits of the SHA-256 of records in order, so that it
// is exact as a float64 benchmark metric.
func w1Digest48(records [][]byte) uint64 {
	h := sha256.New()
	for _, rec := range records {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(rec)))
		h.Write(size[:])
		h.Write(rec)
	}
	return binary.BigEndian.Uint64(h.Sum(nil)) >> 16
}

// w1CheckRecords checks that records hold exactly the merge groups of one
// sweep's transactions, one record per transaction, and returns the number of
// dictionary values they hold, where the format has a dictionary.
func w1CheckRecords(tb testing.TB, in *w1Input, refs []storage.SeriesRef, records [][]byte) int {
	require.Len(tb, records, len(in.sweeps[1]))
	values := 0
	for c, rec := range records {
		groups, n := w1DecodeGroups(tb, rec)
		values += n
		want := w1Groups(in.sweeps[1][c], refs)
		require.Len(tb, groups, len(want), "record %d", c)
		for _, g := range groups {
			require.Equal(tb, want[g.ref], g, "record %d, ref %d", c, g.ref)
		}
	}
	return values
}

// w1Timer runs timed work under b's timer and accumulates its process CPU and
// GC cycles. The CPU reads sit inside StartTimer and StopTimer, which read
// memory statistics, so neither those reads, setup nor checks are counted.
type w1Timer struct {
	b   *testing.B
	cpu time.Duration
	gcs uint64
}

// w1Leaf stops b's timer on entering a leaf benchmark. Go starts the timer
// before running the body, so setup would otherwise count towards elapsed
// time and allocations.
func w1Leaf(b *testing.B) *w1Timer {
	b.StopTimer()
	return &w1Timer{b: b}
}

// start zeroes the elapsed time and allocation counters once setup is done.
// The timer stays stopped; time runs it around each iteration's work.
func (t *w1Timer) start() {
	t.b.ReportAllocs()
	t.b.ResetTimer()
}

func (t *w1Timer) time(work func()) {
	gcs := w1GCCycles()
	t.b.StartTimer()
	start := w1CPU()
	work()
	end := w1CPU()
	t.b.StopTimer()
	t.cpu += end - start
	t.gcs += w1GCCycles() - gcs
}

// report reports per-unit CPU, elapsed time, GC cycles and clock overhead.
// Allocation figures are per iteration; units per iteration convert them.
func (t *w1Timer) report(unit string, perIteration int) {
	units := float64(t.b.N) * float64(perIteration)
	t.b.ReportMetric(float64(t.cpu.Nanoseconds())/units, "cpu-ns/"+unit)
	t.b.ReportMetric(float64(t.b.Elapsed().Nanoseconds())/units, "elapsed-ns/"+unit)
	t.b.ReportMetric(float64(perIteration), unit+"s/op")
	t.b.ReportMetric(float64(t.gcs)/float64(t.b.N), "gcs/op")
	t.b.ReportMetric(w1ClockOverhead(), "clock-ns/read")
}

// w1CPU returns the process CPU time. It covers all threads, including
// background GC, so a timed window measures process CPU during the window, not
// CPU attributable to the operation alone.
func w1CPU() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_PROCESS_CPUTIME_ID, &ts); err != nil {
		panic(err)
	}
	return time.Duration(ts.Nano())
}

// w1ClockOverhead returns the CPU time of one clock read.
func w1ClockOverhead() float64 {
	const reads = 1000
	start := w1CPU()
	for range reads {
		w1CPU()
	}
	return float64((w1CPU() - start).Nanoseconds()) / (reads + 1)
}

func w1GCCycles() uint64 {
	sample := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// w1WriterRun stages one sweep's transactions against seeded state and
// returns their records.
type w1WriterRun struct {
	in     *w1Input
	db     *DB
	refs   []storage.SeriesRef
	staged []w1Staged
	// Each transaction's record and compressed record buffers, reused.
	records    [][]byte
	compressed []compression.EncodeBuffer
}

func newW1WriterRun(tb testing.TB, workload string) *w1WriterRun {
	in := w1InputFor(workload)
	db, refs := w1OpenDB(tb, "native", in)
	tb.Cleanup(func() { require.NoError(tb, db.Close()) })
	r := &w1WriterRun{in: in, db: db, refs: refs, staged: make([]w1Staged, len(in.sweeps[1])), records: make([][]byte, len(in.sweeps[1]))}
	for range in.sweeps[1] {
		r.compressed = append(r.compressed, compression.NewSyncEncodeBuffer())
	}
	return r
}

func (r *w1WriterRun) stage(tb testing.TB) {
	for c, txn := range r.in.sweeps[1] {
		r.staged[c] = w1Stage(tb, r.db, r.refs, txn)
	}
}

// release returns the staged transactions' appenders to their pool, and the
// WAL scratch bytes they then retain.
func (r *w1WriterRun) release() int {
	scratch := 0
	for c := range r.staged {
		scratch += w1Release(r.db, r.staged[c])
		r.staged[c] = nil
	}
	return scratch
}

func (r *w1WriterRun) encode(c int) {
	r.records[c] = w1Encode(r.staged[c], r.records[c][:0])
}

func (r *w1WriterRun) compress(tb testing.TB, c int) int {
	out, err := compression.Encode(compression.Snappy, r.records[c], r.compressed[c])
	if err != nil {
		require.NoError(tb, err)
	}
	return len(out)
}

func BenchmarkMetadataW1Writer(b *testing.B) {
	for _, workload := range w1Workloads {
		b.Run("workload="+workload, func(b *testing.B) {
			timer := w1Leaf(b)
			if reason := w1WriterUnavailable(workload); reason != "" {
				b.Skip("unavailable on this build: " + reason)
			}
			r := newW1WriterRun(b, workload)
			timer.start()
			var recordBytes, compressedBytes, values, scratch int
			for i := range b.N {
				r.stage(b)
				recordBytes, compressedBytes = 0, 0
				timer.time(func() {
					for c := range r.staged {
						w1Construct(r.staged[c])
						r.encode(c)
						recordBytes += len(r.records[c])
						compressedBytes += r.compress(b, c)
					}
				})
				if i == 0 {
					values = w1CheckRecords(b, r.in, r.refs, r.records)
				}
				scratch = r.release()
			}
			timer.report("point", r.in.points())
			b.ReportMetric(float64(recordBytes), "record-B")
			b.ReportMetric(float64(compressedBytes), "compressed-B")
			b.ReportMetric(float64(values), "values")
			b.ReportMetric(float64(w1Digest48(r.records)), "record-digest48")
			b.ReportMetric(float64(scratch), "scratch-B")
		})
	}
}

var w1WriterComponents = []string{"construct", "encode", "snappy"}

func BenchmarkMetadataW1WriterComponents(b *testing.B) {
	for _, component := range w1WriterComponents {
		for _, workload := range w1Workloads {
			b.Run("component="+component+"/workload="+workload, func(b *testing.B) {
				timer := w1Leaf(b)
				if reason := w1ComponentUnavailable(component, workload); reason != "" {
					b.Skip("unavailable on this build: " + reason)
				}
				r := newW1WriterRun(b, workload)
				timer.start()
				var recordBytes int
				for range b.N {
					r.stage(b)
					switch component {
					case "construct":
						timer.time(func() {
							for c := range r.staged {
								w1Construct(r.staged[c])
							}
						})
						for c := range r.staged {
							r.encode(c)
						}
					case "encode":
						for c := range r.staged {
							w1Construct(r.staged[c])
						}
						timer.time(func() {
							for c := range r.staged {
								r.encode(c)
							}
						})
					case "snappy":
						for c := range r.staged {
							w1Construct(r.staged[c])
							r.encode(c)
						}
						timer.time(func() {
							for c := range r.staged {
								r.compress(b, c)
							}
						})
					}
					recordBytes = 0
					for _, rec := range r.records {
						recordBytes += len(rec)
					}
					r.release()
				}
				timer.report("point", r.in.points())
				b.ReportMetric(float64(recordBytes), "record-B")
				b.ReportMetric(float64(w1Digest48(r.records)), "record-digest48")
			})
		}
	}
}

// BenchmarkMetadataW1Commit is the G1 guard: whole commits of unchanged
// metadata, appends and log() included, after the seed.
func BenchmarkMetadataW1Commit(b *testing.B) {
	for _, workload := range []string{"u", "d"} {
		b.Run("workload="+workload, func(b *testing.B) {
			timer := w1Leaf(b)
			in := w1InputFor(w1UnchangedWorkload[workload])
			mode := w1CommitMode()
			db, refs := w1OpenDB(b, mode, in)
			defer func() { require.NoError(b, db.Close()) }()
			seedRecords := w1WALMetadataRecords(b, db)
			runtime.GC()
			timer.start()
			for i := range b.N {
				timer.time(func() {
					for _, txn := range in.sweeps[0] {
						w1CommitTransaction(b, db, in, refs, txn, i+1)
					}
				})
			}
			timer.report("sample", in.series())
			// Unchanged metadata logs no metadata records after the seed.
			b.ReportMetric(float64(w1WALMetadataRecords(b, db)-seedRecords), "metadata-records")
		})
	}
}

// w1WALMetadataRecords counts the metadata records in db's WAL segments.
func w1WALMetadataRecords(tb testing.TB, db *DB) int {
	require.NoError(tb, db.head.wal.Sync())
	sr, err := wlog.NewSegmentsRangeReader(wlog.SegmentRange{Dir: filepath.Join(db.Dir(), "wal"), Last: math.MaxInt32})
	require.NoError(tb, err)
	defer sr.Close()
	var dec record.Decoder
	n := 0
	r := wlog.NewReader(sr)
	for r.Next() {
		if w1MetadataRecordType(dec.Type(r.Record())) {
			n++
		}
	}
	require.NoError(tb, r.Err())
	return n
}

// TestMetadataW1 checks the writer kernels' records and their determinism. It
// reports no performance figures.
func TestMetadataW1(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	for _, workload := range w1Workloads {
		t.Run("workload="+workload, func(t *testing.T) {
			if reason := w1WriterUnavailable(workload); reason != "" {
				t.Skip("unavailable on this build: " + reason)
			}
			r := newW1WriterRun(t, workload)
			var digest uint64
			for i := range 3 {
				r.stage(t)
				for c := range r.staged {
					w1Construct(r.staged[c])
					r.encode(c)
				}
				w1CheckRecords(t, r.in, r.refs, r.records)
				if i == 0 {
					digest = w1Digest48(r.records)
				}
				// Pooled appenders give the same records.
				require.Equal(t, digest, w1Digest48(r.records))
				r.release()
			}
			t.Logf("%s: %d record bytes", workload, len(slices.Concat(r.records...)))
		})
	}
	t.Run("unchanged commits log no metadata", func(t *testing.T) {
		in := w1InputFor("sb")
		db, refs := w1OpenDB(t, w1CommitMode(), in)
		defer func() { require.NoError(t, db.Close()) }()
		seed := w1WALMetadataRecords(t, db)
		for _, txn := range in.sweeps[0] {
			w1CommitTransaction(t, db, in, refs, txn, 1)
		}
		require.Equal(t, seed, w1WALMetadataRecords(t, db))
	})
	t.Run("the CPU clock", func(t *testing.T) {
		require.Positive(t, w1ClockOverhead())
	})
}

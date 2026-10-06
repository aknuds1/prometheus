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
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/util/compression"
)

// The W1 screen's sender kernels: PS decompresses, decodes and stores a
// workload's change records, as the watcher passes them to a queue, into a
// fresh queue with a fresh process interner, after the seed's records; its
// components time decompression and decoding, and storing, alone. The records
// are those Commit logs for the writer kernels' transactions, in the build's
// format. Base-specific parts are in metadata_w1_adapter_test.go; this file is
// identical on every build.

const (
	w1Series   = 10000
	w1Commit   = 1000
	w1Sweeps   = 4
	w1MPSeries = 1000
	w1MPCommit = 100
	w1MPPoints = 10
	w1Base     = 1767225600000 // 2026-01-01T00:00:00Z.
	// Commit orders a record's groups by stripe, in order of first
	// observation, then by ref.
	w1Stripes = 256
)

var (
	w1Workloads = []string{"sb", "db", "mp"}
	// The byte evidence adds dv, distinct values with less repetitive
	// strings than db's padded helps.
	w1ByteWorkloads = []string{"sb", "db", "mp", "dv"}
)

func w1Metadata(workload string, slot, version int) metadata.Metadata {
	if workload == "dv" {
		return w1Varied(slot, version)
	}
	family := slot
	if workload == "sb" {
		family = slot % 100
	}
	prefix := fmt.Sprintf("family %d version %d ", family, version)
	return metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: prefix + strings.Repeat("x", 64-len(prefix))}
}

// w1Vocabulary holds dv's 64 words.
var w1Vocabulary = strings.Fields(`total number of requests handled by the server since process start
including failed and retried calls latency in seconds bytes received sent over network connections
currently open per client queue length waiting jobs scheduled tasks completed errors observed during
garbage collection memory heap allocated objects cache hits misses evictions disk reads writes rate
limited throttled dropped expired pending active idle shards replicas partitions regions`)

// w1Varied returns dv's value for a slot and version, derived from the
// SHA-256 of "w1 dv <slot> <version>": its first byte selects a type, its
// second a unit, its third 5 to 12 words, and the following bytes the words.
func w1Varied(slot, version int) metadata.Metadata {
	h := sha256.Sum256(fmt.Appendf(nil, "w1 dv %d %d", slot, version))
	types := []model.MetricType{model.MetricTypeCounter, model.MetricTypeGauge, model.MetricTypeHistogram, model.MetricTypeSummary}
	units := []string{"seconds", "bytes", "requests", "", "ratio", "celsius"}
	words := make([]string, 5+int(h[2]%8))
	for i := range words {
		words[i] = w1Vocabulary[int(h[3+i])%len(w1Vocabulary)]
	}
	return metadata.Metadata{Type: types[h[0]%4], Unit: units[h[1]%6], Help: strings.Join(words, " ")}
}

func w1Timestamp(step int) int64 { return w1Base + int64(step)*15000 }

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

// w1Groups returns a workload's merge groups: sweeps[step][commit] holds one
// transaction's groups in Commit's order. Refs are slots plus one, as a fresh
// head assigns them to the seed's series.
func w1Groups(workload string) [][][]w1Group {
	series, commit := w1Series, w1Commit
	if workload == "mp" {
		series, commit = w1MPSeries, w1MPCommit
	}
	var sweeps [][][]w1Group
	for step := 0; step <= w1Sweeps; step++ {
		var sweep [][]w1Group
		for offset := 0; offset < series; offset += commit {
			var txn []w1Group
			for slot := offset; slot < offset+commit; slot++ {
				e := w1Group{ref: chunks.HeadSeriesRef(slot + 1)}
				point := func(from int64, version int) {
					e.points = append(e.points, w1Point{from: from, m: w1Metadata(workload, slot, version)})
				}
				if workload != "mp" || step == 0 {
					point(w1Timestamp(step), step)
				} else {
					for i := range w1MPPoints {
						version := 2*step - 1
						if i >= w1MPPoints/2 {
							version = 2 * step
						}
						point(w1Timestamp(step)+int64(i), version)
					}
				}
				txn = append(txn, e)
			}
			first := map[chunks.HeadSeriesRef]int{}
			for i, e := range txn {
				if _, ok := first[e.ref%w1Stripes]; !ok {
					first[e.ref%w1Stripes] = i
				}
			}
			slices.SortStableFunc(txn, func(a, b w1Group) int {
				return cmp.Or(cmp.Compare(first[a.ref%w1Stripes], first[b.ref%w1Stripes]), cmp.Compare(a.ref, b.ref))
			})
			sweep = append(sweep, txn)
		}
		sweeps = append(sweeps, sweep)
	}
	return sweeps
}

// w1Records holds a workload's records in the build's format, encoded once
// and never modified.
type w1Records struct {
	seed, changes [][]byte
	// The compressed changes, as the WAL holds them.
	compressed [][]byte
	// newest is each series' newest value after the changes.
	newest map[chunks.HeadSeriesRef]metadata.Metadata
	// The first sweep's records, which the writer kernels build.
	firstSweep [][]byte
}

var (
	w1RecordsMtx   sync.Mutex
	w1RecordsCache = map[string]*w1Records{}
)

func w1RecordsFor(tb testing.TB, workload string) *w1Records {
	w1RecordsMtx.Lock()
	defer w1RecordsMtx.Unlock()
	if r := w1RecordsCache[workload]; r != nil {
		return r
	}
	r := &w1Records{newest: map[chunks.HeadSeriesRef]metadata.Metadata{}}
	for step, sweep := range w1Groups(workload) {
		for _, txn := range sweep {
			rec := w1EncodeRecord(txn)
			if step == 0 {
				r.seed = append(r.seed, rec)
				continue
			}
			if step == 1 {
				r.firstSweep = append(r.firstSweep, rec)
			}
			r.changes = append(r.changes, rec)
			compressed, err := compression.Encode(compression.Snappy, rec, nil)
			require.NoError(tb, err)
			r.compressed = append(r.compressed, slices.Clone(compressed))
			for _, e := range txn {
				r.newest[e.ref] = e.points[len(e.points)-1].m
			}
		}
	}
	w1RecordsCache[workload] = r
	return r
}

func (r *w1Records) points() int { return len(r.changes) * w1Commit }

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

func w1Sum(records [][]byte) int {
	n := 0
	for _, rec := range records {
		n += len(rec)
	}
	return n
}

// TestMetadataW1Bytes checks that each workload's first-sweep records, as this
// build encodes them, decompress to themselves. With W1_BYTES_DIR set, it
// writes them raw and compressed, each file a sequence of 8-byte big-endian
// lengths and records, with their inputs and a summary of sizes and digests,
// for the byte evidence. Each digest is that of its file's contents.
func TestMetadataW1Bytes(t *testing.T) {
	format := w1RecordFormat()
	if format == "" {
		t.Skip("this build logs no metadata records")
	}
	dir := os.Getenv("W1_BYTES_DIR")
	var summary []string
	for _, workload := range w1ByteWorkloads {
		if reason := w1SenderUnavailable(workload); reason != "" {
			summary = append(summary, fmt.Sprintf("%s %s unavailable: %s", format, workload, reason))
			continue
		}
		var raw, compressed [][]byte
		var inputs strings.Builder
		for c, txn := range w1Groups(workload)[1] {
			rec := w1EncodeRecord(txn)
			out, err := compression.Encode(compression.Snappy, rec, nil)
			require.NoError(t, err)
			out = slices.Clone(out)
			back, err := compression.Decode(compression.Snappy, out, nil)
			require.NoError(t, err)
			require.Equal(t, rec, back)
			raw, compressed = append(raw, rec), append(compressed, out)
			for _, g := range txn {
				for _, p := range g.points {
					fmt.Fprintf(&inputs, "%d\t%d\t%d\t%s\t%s\t%q\n", c, g.ref, p.from, p.m.Type, p.m.Unit, p.m.Help)
				}
			}
		}
		line := fmt.Sprintf("%s %s records=%d raw=%d compressed=%d raw-digest48=%d compressed-digest48=%d",
			format, workload, len(raw), w1Sum(raw), w1Sum(compressed), w1Digest48(raw), w1Digest48(compressed))
		summary = append(summary, line)
		t.Log(line)
		if dir == "" {
			continue
		}
		for suffix, records := range map[string][][]byte{"raw": raw, "snappy": compressed} {
			var file []byte
			for _, rec := range records {
				file = binary.BigEndian.AppendUint64(file, uint64(len(rec)))
				file = append(file, rec...)
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%s.%s", format, workload, suffix)), file, 0o644))
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("inputs-%s.tsv", workload)), []byte(inputs.String()), 0o644))
	}
	if dir != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, format+".txt"), []byte(strings.Join(summary, "\n")+"\n"), 0o644))
	}
}

// w1SenderRun is one iteration's state: a fresh queue and process interner,
// with the seed stored.
type w1SenderRun struct {
	records *w1Records
	qm      *QueueManager
	restore func()
	decoder *w1Decoder
	decBuf  compression.DecodeBuffer
}

func newW1SenderRun(tb testing.TB, workload string, intern func(func(metadata.Metadata) *metadata.Metadata) func(metadata.Metadata) *metadata.Metadata) *w1SenderRun {
	r := &w1SenderRun{records: w1RecordsFor(tb, workload), restore: w1UseInterner(intern), decBuf: compression.NewSyncDecodeBuffer()}
	r.qm = w1NewQueueManager(tb)
	r.decoder = newW1Decoder()
	for _, rec := range r.records.seed {
		r.decoder.decode(tb, rec)
		r.decoder.store(r.qm)
	}
	return r
}

// apply decompresses, decodes and stores the change records.
func (r *w1SenderRun) apply(tb testing.TB) {
	for _, compressed := range r.records.compressed {
		rec, err := compression.Decode(compression.Snappy, compressed, r.decBuf)
		if err != nil {
			require.NoError(tb, err)
		}
		r.decoder.decode(tb, rec)
		r.decoder.store(r.qm)
	}
}

// check checks that every series holds its newest value, with the history
// depth the build keeps.
func (r *w1SenderRun) check(tb testing.TB) int {
	depth := -1
	for ref, want := range r.records.newest {
		d, got := w1View(tb, r.qm, ref)
		require.NotNil(tb, got, "series %d", ref)
		require.Equal(tb, want, *got, "series %d", ref)
		if depth < 0 {
			depth = d
		}
		require.Equal(tb, depth, d, "series %d", ref)
	}
	return depth
}

func (r *w1SenderRun) close() {
	r.restore()
}

// w1Resolutions counts the interner calls that storing the change records
// makes, in a run of its own.
func w1Resolutions(tb testing.TB, workload string) int {
	calls := 0
	r := newW1SenderRun(tb, workload, func(intern func(metadata.Metadata) *metadata.Metadata) func(metadata.Metadata) *metadata.Metadata {
		return func(m metadata.Metadata) *metadata.Metadata {
			calls++
			return intern(m)
		}
	})
	defer r.close()
	calls = 0
	r.apply(tb)
	return calls
}

func BenchmarkMetadataW1Sender(b *testing.B) {
	for _, workload := range w1Workloads {
		b.Run("workload="+workload, func(b *testing.B) {
			timer := w1Leaf(b)
			if reason := w1SenderUnavailable(workload); reason != "" {
				b.Skip("unavailable on this build: " + reason)
			}
			records := w1RecordsFor(b, workload)
			timer.start()
			depth := 0
			for range b.N {
				r := newW1SenderRun(b, workload, nil)
				runtime.GC()
				timer.time(func() { r.apply(b) })
				depth = r.check(b)
				r.close()
			}
			timer.report("point", records.points())
			b.ReportMetric(float64(depth), "depth")
			b.ReportMetric(float64(w1Resolutions(b, workload))/float64(len(records.changes)), "resolutions/record")
			b.ReportMetric(float64(w1Sum(records.changes)), "record-B")
			b.ReportMetric(float64(w1Sum(records.compressed)), "compressed-B")
			b.ReportMetric(float64(w1Digest48(records.firstSweep)), "first-sweep-digest48")
		})
	}
}

var w1SenderComponents = []string{"decode", "store"}

func BenchmarkMetadataW1SenderComponents(b *testing.B) {
	for _, component := range w1SenderComponents {
		for _, workload := range w1Workloads {
			b.Run("component="+component+"/workload="+workload, func(b *testing.B) {
				timer := w1Leaf(b)
				if reason := w1SenderUnavailable(workload); reason != "" {
					b.Skip("unavailable on this build: " + reason)
				}
				records := w1RecordsFor(b, workload)
				timer.start()
				for range b.N {
					r := newW1SenderRun(b, workload, nil)
					runtime.GC()
					switch component {
					case "decode":
						timer.time(func() {
							for _, compressed := range records.compressed {
								rec, err := compression.Decode(compression.Snappy, compressed, r.decBuf)
								if err != nil {
									require.NoError(b, err)
								}
								r.decoder.decode(b, rec)
							}
						})
					case "store":
						// Each record is decoded from a buffer of its own, which
						// its borrowed contents may alias.
						decoders := make([]*w1Decoder, len(records.compressed))
						for i, compressed := range records.compressed {
							rec, err := compression.Decode(compression.Snappy, compressed, nil)
							require.NoError(b, err)
							decoders[i] = newW1Decoder()
							decoders[i].decode(b, rec)
						}
						timer.time(func() {
							for _, d := range decoders {
								d.store(r.qm)
							}
						})
						r.check(b)
					}
					r.close()
				}
				timer.report("point", records.points())
			})
		}
	}
}

// TestMetadataW1 checks the sender kernels' state and determinism. It reports
// no performance figures.
func TestMetadataW1(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	for _, workload := range w1Workloads {
		t.Run("workload="+workload, func(t *testing.T) {
			if reason := w1SenderUnavailable(workload); reason != "" {
				t.Skip("unavailable on this build: " + reason)
			}
			var depth int
			for i := range 3 {
				r := newW1SenderRun(t, workload, nil)
				r.apply(t)
				d := r.check(t)
				if i == 0 {
					depth = d
				}
				require.Equal(t, depth, d)
				r.close()
			}
			// Resolutions are deterministic.
			resolutions := w1Resolutions(t, workload)
			require.Equal(t, resolutions, w1Resolutions(t, workload))
			records := w1RecordsFor(t, workload)
			t.Logf("%s: depth %d, %d resolutions per record, first sweep digest %d", workload, depth,
				resolutions/len(records.changes), w1Digest48(records.firstSweep))
		})
	}
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

func (t *w1Timer) report(unit string, perIteration int) {
	units := float64(t.b.N) * float64(perIteration)
	t.b.ReportMetric(float64(t.cpu.Nanoseconds())/units, "cpu-ns/"+unit)
	t.b.ReportMetric(float64(t.b.Elapsed().Nanoseconds())/units, "elapsed-ns/"+unit)
	t.b.ReportMetric(float64(perIteration), unit+"s/op")
	t.b.ReportMetric(float64(t.gcs)/float64(t.b.N), "gcs/op")
	t.b.ReportMetric(w1ClockOverhead(), "clock-ns/read")
}

// w1CPU returns the process CPU time, all threads included.
func w1CPU() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_PROCESS_CPUTIME_ID, &ts); err != nil {
		panic(err)
	}
	return time.Duration(ts.Nano())
}

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

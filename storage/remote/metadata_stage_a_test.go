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
	"flag"
	"fmt"
	"runtime"
	"runtime/metrics"
	"sync"
	"testing"
	"time"

	remoteapi "github.com/prometheus/client_golang/exp/api/remote"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
)

// The stage A sender screen decodes and stores the pipeline fixture's own
// metadata record sequences, as the watcher does. Every iteration starts from
// its registered state: fresh queues and a fresh process interner, and for the
// changes state the seed applied outside the timed region. Base-specific paths
// are in metadata_stage_a_adapter_test.go; this file is identical on every base.

const (
	stageASeries   = 10000
	stageACommit   = 1000
	stageAVersions = 4
	// A fixed base keeps timestamps independent of the time of day.
	stageABase = 1767225600000 // 2026-01-01T00:00:00Z.
)

type stageAPoint struct {
	ref  chunks.HeadSeriesRef
	from int64
	m    metadata.Metadata
}

type stageAWorkload struct {
	name   string
	values int
}

var stageAWorkloads = []stageAWorkload{{"sb", 100}, {"db", stageASeries}}

// stageACommits returns the fixture's metadata change points per commit: the
// seed's version 0, then versions 1 to 4, each in commits of consecutive slots.
func stageACommits(values int) (seed, changes [][]stageAPoint) {
	c := metadataPipelineConfig{Values: values, Base: stageABase}
	for step := 0; step <= stageAVersions; step++ {
		for offset := 0; offset < stageASeries; offset += stageACommit {
			commit := make([]stageAPoint, 0, stageACommit)
			for slot := offset; slot < offset+stageACommit; slot++ {
				commit = append(commit, stageAPoint{ref: chunks.HeadSeriesRef(slot + 1), from: c.timestamp(step), m: c.metadata(slot, step)})
			}
			if step == 0 {
				seed = append(seed, commit)
			} else {
				changes = append(changes, commit)
			}
		}
	}
	return seed, changes
}

func stageALegacyRecord(points []stageAPoint) []byte {
	entries := make([]record.RefMetadata, len(points))
	for i, p := range points {
		entries[i] = record.RefMetadata{Ref: p.ref, Type: record.GetMetricType(p.m.Type), Unit: p.m.Unit, Help: p.m.Help}
	}
	var enc record.Encoder
	return enc.Metadata(entries, nil)
}

// stageARecords holds a workload's records for one path, encoded once and
// never modified.
type stageARecords struct {
	seed, changes [][]byte
	// newest is each series' newest value after the changes.
	newest map[chunks.HeadSeriesRef]metadata.Metadata
}

var (
	stageARecordsMtx   sync.Mutex
	stageARecordsCache = map[string]*stageARecords{}
)

func stageARecordsFor(path string, w stageAWorkload) *stageARecords {
	stageARecordsMtx.Lock()
	defer stageARecordsMtx.Unlock()
	key := path + "/" + w.name
	if r := stageARecordsCache[key]; r != nil {
		return r
	}
	seed, changes := stageACommits(w.values)
	encode := stageALegacyRecord
	if path == "native" {
		encode = stageAEncodeNative
	}
	r := &stageARecords{newest: map[chunks.HeadSeriesRef]metadata.Metadata{}}
	for _, commit := range seed {
		r.seed = append(r.seed, encode(commit))
	}
	for _, commit := range changes {
		r.changes = append(r.changes, encode(commit))
		for _, p := range commit {
			r.newest[p.ref] = p.m
		}
	}
	stageARecordsCache[key] = r
	return r
}

type stageASenderCase struct {
	path     string // legacy or native.
	interner string // default, bypass or none.
	workload stageAWorkload
	state    string // seed or changes.
	queues   int
	lag      int // Records the second queue trails the first by.
}

func (c stageASenderCase) name() string {
	return fmt.Sprintf("path=%s/interner=%s/workload=%s/state=%s/queues=%d/lag=%d", c.path, c.interner, c.workload.name, c.state, c.queues, c.lag)
}

// stageASenderCases returns every frozen sender case. Bases lacking a path or
// interner skip the cases that need it, saying so.
func stageASenderCases() []stageASenderCase {
	var cases []stageASenderCase
	for _, path := range []string{"legacy", "native"} {
		for _, interner := range []string{"default", "bypass", "none"} {
			for _, w := range stageAWorkloads {
				for _, state := range []string{"seed", "changes"} {
					cases = append(cases, stageASenderCase{path: path, interner: interner, workload: w, state: state, queues: 1})
				}
				if interner != "bypass" {
					for _, lag := range []int{0, 10} {
						cases = append(cases, stageASenderCase{path: path, interner: interner, workload: w, state: "changes", queues: 2, lag: lag})
					}
				}
			}
		}
	}
	return cases
}

// stageASenderAvailable reports whether this base has the case's path and
// interner, and why not.
func stageASenderAvailable(c stageASenderCase) (bool, string) {
	switch {
	case c.path == "native" && !stageANativeSupported():
		return false, "no native metadata records"
	case c.interner == "none" && stageAInternerSupported():
		return false, "the base interns WAL metadata"
	case c.interner != "none" && !stageAInternerSupported():
		return false, "no WAL metadata interner"
	}
	return true, ""
}

// stageAInternerState counts the entries in an interner's generations and
// the values and fingerprints its ledger holds, where it has one.
type stageAInternerState struct {
	entries, ledgerValues, ledgerFingerprints int
	hasLedger                                 bool
}

type stageASenderResult struct {
	// objectsPerValue counts the first queue's distinct newest objects per
	// distinct newest value.
	objectsPerValue float64
	// crossQueueShared is the fraction of series whose second queue holds the
	// first queue's newest object.
	crossQueueShared float64
	depth            int
	interner         stageAInternerState
	// retainedState and retainedInterner are live heap bytes held by the
	// queues and interner, and by the interner after the queues are released.
	retainedState, retainedInterner int64
}

// stageARunSender runs one iteration of c. timed must run its argument once,
// and is where benchmarks time it.
func stageARunSender(tb testing.TB, c stageASenderCase, timed func(work func())) stageASenderResult {
	records := stageARecordsFor(c.path, c.workload)
	baseline := stageALiveHeap()
	restore, internerState := stageAUseInterner(c.interner)
	queues := make([]*QueueManager, c.queues)
	stores := make([]func([]byte), c.queues)
	for i := range queues {
		queues[i] = stageANewQueueManager(tb, c.path)
		stores[i] = stageAStore(tb, c.path, queues[i])
		require.Zero(tb, stageAStoredSeries(c.path, queues[i]), "a new queue holds no metadata")
	}
	fresh := internerState()
	require.Zero(tb, fresh.entries+fresh.ledgerValues+fresh.ledgerFingerprints, "every iteration starts with a fresh interner")
	measured := records.seed
	if c.state == "changes" {
		for _, rec := range records.seed {
			for _, store := range stores {
				store(rec)
			}
		}
		measured = records.changes
	}
	runtime.GC()
	timed(func() {
		for i := 0; i < len(measured)+c.lag; i++ {
			if i < len(measured) {
				stores[0](measured[i])
			}
			if j := i - c.lag; c.queues > 1 && j >= 0 {
				stores[1](measured[j])
			}
		}
	})

	want, depth := records.newest, stageAVersions+1
	if c.state == "seed" {
		want, depth = map[chunks.HeadSeriesRef]metadata.Metadata{}, 1
		seed, _ := stageACommits(c.workload.values)
		for _, commit := range seed {
			for _, p := range commit {
				want[p.ref] = p.m
			}
		}
	}
	if c.path == "legacy" {
		depth = 1
	}
	r := stageASenderResult{depth: depth}
	objects := map[metadata.Metadata]map[*metadata.Metadata]struct{}{}
	shared := 0
	for ref, m := range want {
		var newest [2]*metadata.Metadata
		for i, qm := range queues {
			d, v := stageAView(tb, c.path, qm, ref)
			require.Equal(tb, depth, d, "series %d", ref)
			require.Equal(tb, m, *v, "series %d", ref)
			newest[i] = v
		}
		if objects[m] == nil {
			objects[m] = map[*metadata.Metadata]struct{}{}
		}
		objects[m][newest[0]] = struct{}{}
		if c.queues > 1 && newest[0] == newest[1] {
			shared++
		}
	}
	total := 0
	for _, set := range objects {
		total += len(set)
	}
	r.objectsPerValue = float64(total) / float64(len(objects))
	if c.queues > 1 {
		r.crossQueueShared = float64(shared) / float64(len(want))
	}
	r.interner = internerState()

	withState := stageALiveHeap()
	// Releasing the queues leaves only the interner retaining values.
	clear(queues)
	clear(stores)
	withInterner := stageALiveHeap()
	restore()
	released := stageALiveHeap()
	r.retainedState, r.retainedInterner = withState-baseline, withInterner-released
	return r
}

// stageANewLegacyQueueManager returns a remote write 2.0 queue that stores
// legacy metadata.
func stageANewLegacyQueueManager(tb testing.TB) *QueueManager {
	return newTestQueueManager(tb, testDefaultQueueConfig(), config.DefaultMetadataConfig, defaultFlushDeadline, NewNopWriteClient(), remoteapi.WriteV2MessageType)
}

// stageAStore returns a function that applies one record to qm.
func stageAStore(tb testing.TB, path string, qm *QueueManager) func([]byte) {
	if path == "native" {
		return stageANativeStore(tb, qm)
	}
	var dec record.Decoder
	var meta []record.RefMetadata
	return func(rec []byte) {
		var err error
		meta, err = dec.Metadata(rec, meta[:0])
		require.NoError(tb, err)
		qm.StoreMetadata(meta)
	}
}

func stageAStoredSeries(path string, qm *QueueManager) int {
	if path == "native" {
		return stageANativeSeries(qm)
	}
	return len(qm.seriesMetadata)
}

// stageAView returns the depth of ref's history and its newest value.
func stageAView(tb testing.TB, path string, qm *QueueManager, ref chunks.HeadSeriesRef) (int, *metadata.Metadata) {
	if path == "native" {
		return stageANativeView(tb, qm, ref)
	}
	v := qm.seriesMetadata[ref]
	require.NotNil(tb, v, "series %d", ref)
	return 1, v
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

func BenchmarkMetadataStageASender(b *testing.B) {
	for _, c := range stageASenderCases() {
		b.Run(c.name(), func(b *testing.B) {
			timer := stageALeaf(b)
			if ok, reason := stageASenderAvailable(c); !ok {
				b.Skip("unavailable on this base: " + reason)
			}
			records := stageARecordsFor(c.path, c.workload)
			measured := records.changes
			if c.state == "seed" {
				measured = records.seed
			}
			timer.start()
			var r stageASenderResult
			for range b.N {
				r = stageARunSender(b, c, timer.time)
			}
			timer.report("point", c.queues*len(measured)*stageACommit)
			b.ReportMetric(r.objectsPerValue, "objects/value")
			b.ReportMetric(r.crossQueueShared, "cross-queue-shared")
			b.ReportMetric(float64(r.depth), "depth")
			b.ReportMetric(float64(r.interner.entries), "interner-entries")
			b.ReportMetric(float64(r.interner.ledgerValues), "ledger-values")
			b.ReportMetric(float64(r.interner.ledgerFingerprints), "ledger-fingerprints")
			b.ReportMetric(float64(r.retainedState), "retained-state-B")
			b.ReportMetric(float64(r.retainedInterner), "retained-interner-B")
		})
	}
}

// stageADecodeCase is one decoder over a workload's change records.
type stageADecodeCase struct {
	record, decoder string
	workload        stageAWorkload
}

func (c stageADecodeCase) name() string {
	return fmt.Sprintf("record=%s/decoder=%s/workload=%s", c.record, c.decoder, c.workload.name)
}

func stageADecodeCases() []stageADecodeCase {
	var cases []stageADecodeCase
	for _, rec := range []string{"legacy", "native"} {
		for _, decoder := range []string{"copying", "borrowed"} {
			for _, w := range stageAWorkloads {
				cases = append(cases, stageADecodeCase{rec, decoder, w})
			}
		}
	}
	return cases
}

func BenchmarkMetadataStageADecode(b *testing.B) {
	for _, c := range stageADecodeCases() {
		b.Run(c.name(), func(b *testing.B) {
			timer := stageALeaf(b)
			if c.record == "native" && !stageANativeSupported() {
				b.Skip("unavailable on this base: no native metadata records")
			}
			decode, reason := stageADecoder(c.record, c.decoder)
			if decode == nil {
				b.Skip("unavailable on this base: " + reason)
			}
			records := stageARecordsFor(c.record, c.workload).changes
			timer.start()
			for range b.N {
				runtime.GC()
				timer.time(func() {
					for _, rec := range records {
						require.NoError(b, decode(rec))
					}
				})
			}
			timer.report("point", len(records)*stageACommit)
		})
	}
}

// stageADecoder returns a function decoding one record with reused slices, or
// why this base cannot decode the case.
func stageADecoder(rec, decoder string) (func([]byte) error, string) {
	if rec == "native" {
		return stageANativeDecoder(decoder)
	}
	if decoder != "copying" {
		return nil, "legacy records decode only by copying"
	}
	var dec record.Decoder
	var meta []record.RefMetadata
	return func(rec []byte) error {
		var err error
		meta, err = dec.Metadata(rec, meta[:0])
		return err
	}, ""
}

// TestMetadataStageA checks that every available stage A case starts each
// iteration from its registered state, so repeated iterations give identical
// histories, sharing and interner state. It reports no performance figures.
func TestMetadataStageA(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	direct := func(work func()) { work() }
	for _, c := range stageASenderCases() {
		t.Run(c.name(), func(t *testing.T) {
			if ok, _ := stageASenderAvailable(c); !ok {
				t.Skip("unavailable on this base")
			}
			first := stageARunSender(t, c, direct)
			t.Logf("objects/value %v, cross-queue shared %v, depth %d, interner %+v", first.objectsPerValue, first.crossQueueShared, first.depth, first.interner)
			for range 2 {
				r := stageARunSender(t, c, direct)
				require.Equal(t, first.depth, r.depth)
				if !first.interner.hasLedger {
					require.Equal(t, first.objectsPerValue, r.objectsPerValue)
					require.Equal(t, first.crossQueueShared, r.crossQueueShared)
					require.Equal(t, first.interner.entries, r.interner.entries)
					continue
				}
				// Which values share a ledger set depends on each interner's
				// fingerprint seed, so sharing varies a little between
				// iterations.
				require.InDelta(t, first.objectsPerValue, r.objectsPerValue, 0.1)
				require.InDelta(t, first.crossQueueShared, r.crossQueueShared, 0.05)
				require.InDelta(t, first.interner.entries, r.interner.entries, 0.05*float64(first.interner.entries)+50)
				require.InDelta(t, first.interner.ledgerValues, r.interner.ledgerValues, float64(stageACommit))
			}
			if c.interner == "none" || c.interner == "bypass" {
				require.Zero(t, first.interner.entries)
				require.Zero(t, first.interner.ledgerValues+first.interner.ledgerFingerprints)
			}
			if c.queues == 1 && c.workload.name == "db" {
				require.Equal(t, 1.0, first.objectsPerValue, "distinct values have one object each")
			}
		})
	}
	for _, c := range stageADecodeCases() {
		t.Run(c.name(), func(t *testing.T) {
			if c.record == "native" && !stageANativeSupported() {
				t.Skip("unavailable on this base")
			}
			decode, _ := stageADecoder(c.record, c.decoder)
			if decode == nil {
				t.Skip("unavailable on this base")
			}
			for _, rec := range stageARecordsFor(c.record, c.workload).changes {
				require.NoError(t, decode(rec))
			}
		})
	}
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

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

package remote

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/tsdb/wlog"
	"github.com/prometheus/prometheus/util/compression"
)

// metadataPipelineSample is one append of a trace.
type metadataPipelineSample struct{ id, slot, step int }

// metadataPipelineAppend is one append call of a trace, for steps first
// through last.
type metadataPipelineAppend struct{ first, last int }

// appends returns a trace's append calls in order. A restart follows the call
// ending at RestartStep.
func (c metadataPipelineConfig) appends() []metadataPipelineAppend {
	switch {
	case c.Case == "cold":
		return []metadataPipelineAppend{{0, 0}}
	case c.RestartStep > 0:
		return []metadataPipelineAppend{{0, 0}, {1, metadataPipelineRestartHistory}, {metadataPipelineRestartHistory + 1, c.RestartStep}, {c.RestartStep + 1, c.lastStep()}}
	default:
		return []metadataPipelineAppend{{0, 0}, {1, c.lastStep()}}
	}
}

// transactions calls fn with each transaction of an append call, in append
// order. Each writer's transactions are in order; different writers'
// transactions interleave arbitrarily, but never share a series.
func (c metadataPipelineConfig) transactions(a metadataPipelineAppend, fn func([]metadataPipelineSample)) {
	writers := c.writers(a.first, a.last)
	var samples []metadataPipelineSample
	for writer := range writers {
		begin, end := c.Series*writer/writers, c.Series*(writer+1)/writers
		for batch := a.first; batch <= a.last; batch += max(1, c.StepsPerCommit) {
			batchEnd := min(c.lastStepInCommit(batch), a.last)
			for offset := begin; offset < end; offset += c.CommitSize {
				samples = samples[:0]
				for slot := offset; slot < min(offset+c.CommitSize, end); slot++ {
					for step := batch; step <= batchEnd; step++ {
						samples = append(samples, metadataPipelineSample{id: slot + c.generation(slot, step)*c.Series, slot: slot, step: step})
					}
				}
				fn(samples)
			}
		}
	}
}

// metadataPipelineWALRecords holds the series and raw metadata records in WAL
// segments.
type metadataPipelineWALRecords struct {
	ids      map[chunks.HeadSeriesRef]int // Series IDs by ref.
	metadata [][]byte
}

// readMetadataPipelineWAL reads the segments in dir from index first,
// excluding checkpoints.
func readMetadataPipelineWAL(dir string, first int) (metadataPipelineWALRecords, error) {
	w := metadataPipelineWALRecords{ids: map[chunks.HeadSeriesRef]int{}}
	sr, err := wlog.NewSegmentsRangeReader(wlog.SegmentRange{Dir: dir, First: first, Last: math.MaxInt32})
	if err != nil {
		return w, err
	}
	defer sr.Close()
	var dec record.Decoder
	var series []record.RefSeries
	r := wlog.NewReader(sr)
	for r.Next() {
		switch dec.Type(r.Record()) {
		case record.Series:
			if series, err = dec.Series(r.Record(), series[:0]); err != nil {
				return w, err
			}
			for _, s := range series {
				id, err := strconv.Atoi(s.Labels.Get("id"))
				if err != nil {
					return w, err
				}
				w.ids[s.Ref] = id
			}
		case record.Metadata:
			w.metadata = append(w.metadata, slices.Clone(r.Record()))
		}
	}
	return w, r.Err()
}

// payload returns the bytes of all metadata records.
func (w metadataPipelineWALRecords) payload() int {
	n := 0
	for _, rec := range w.metadata {
		n += len(rec)
	}
	return n
}

// refs returns a series ref for each ID.
func (w metadataPipelineWALRecords) refs() map[int]chunks.HeadSeriesRef {
	refs := make(map[int]chunks.HeadSeriesRef, len(w.ids))
	for ref, id := range w.ids {
		refs[id] = ref
	}
	return refs
}

// metadataPipelineExpected is a multiset of expected metadata entries, keyed by
// a canonical description including the series ID, with the exact payload of
// the records holding them.
type metadataPipelineExpected struct {
	entries map[string]int
	records int
	bytes   int
}

func (e *metadataPipelineExpected) add(key string, size int) {
	if e.entries == nil {
		e.entries = map[string]int{}
	}
	e.entries[key]++
	e.bytes += size
}

// check compares decoded entries, keyed as expected, and the records' payload.
func (e metadataPipelineExpected) check(what string, got map[string]int, records, payload int) error {
	if records != e.records {
		return fmt.Errorf("%s: %d metadata records, want %d", what, records, e.records)
	}
	for key, n := range e.entries {
		if got[key] != n {
			return fmt.Errorf("%s: entry %s occurs %d times, want %d", what, key, got[key], n)
		}
	}
	for key, n := range got {
		if e.entries[key] != n {
			return fmt.Errorf("%s: unexpected entry %s (%d times)", what, key, n)
		}
	}
	if want := e.records + e.bytes; payload != want {
		return fmt.Errorf("%s: %d metadata payload bytes, want %d", what, payload, want)
	}
	return nil
}

func metadataPipelineLegacyKey(id int, m record.RefMetadata) string {
	return fmt.Sprintf("%d|%d|%s|%s", id, m.Type, m.Unit, m.Help)
}

// metadataPipelineLegacyEntries models legacy WAL metadata entries. A sample
// logs an entry whenever its metadata differs from the series' committed
// legacy metadata, which commit updates. Replay restores committed metadata,
// so records after a restart are modelled from the restored state.
// Transactions with entries log one record each.
func metadataPipelineLegacyEntries(c metadataPipelineConfig, refs map[int]chunks.HeadSeriesRef, appends []metadataPipelineAppend, fromAppend int) (metadataPipelineExpected, error) {
	var e metadataPipelineExpected
	var enc record.Encoder
	committed := map[int]int{}
	var err error
	for i, a := range appends {
		c.transactions(a, func(samples []metadataPipelineSample) {
			pending := map[int]int{}
			entries := 0
			for _, s := range samples {
				version := c.version(s.slot, s.step)
				if v, ok := committed[s.id]; ok && v == version {
					continue
				}
				pending[s.id] = version
				if i < fromAppend {
					continue
				}
				ref, ok := refs[s.id]
				if !ok {
					err = errors.Join(err, fmt.Errorf("no series record for id %d", s.id))
					continue
				}
				m := c.metadata(s.slot, version)
				entry := record.RefMetadata{Ref: ref, Type: record.GetMetricType(m.Type), Unit: m.Unit, Help: m.Help}
				e.add(metadataPipelineLegacyKey(s.id, entry), len(enc.Metadata([]record.RefMetadata{entry}, nil))-1)
				entries++
			}
			if entries > 0 {
				e.records++
			}
			maps.Copy(committed, pending)
		})
	}
	return e, err
}

// decodeLegacy decodes metadata records' entries with the legacy decoder.
func (w metadataPipelineWALRecords) decodeLegacy(ids map[chunks.HeadSeriesRef]int) (map[string]int, error) {
	got := map[string]int{}
	var dec record.Decoder
	for _, rec := range w.metadata {
		entries, err := dec.Metadata(rec, nil)
		if err != nil {
			return nil, err
		}
		for _, m := range entries {
			id, ok := ids[m.Ref]
			if !ok {
				return nil, fmt.Errorf("metadata for unknown ref %d", m.Ref)
			}
			got[metadataPipelineLegacyKey(id, m)]++
		}
	}
	return got, nil
}

// metadataPipelineMetadataOracle checks the metadata records that a trace
// left in the WAL segments read, which start from the append call fromAppend.
// The default describes builds whose native metadata never reaches the WAL;
// builds that log native entries replace it.
var metadataPipelineMetadataOracle = metadataPipelineLegacyMetadataOracle

func metadataPipelineLegacyMetadataOracle(c metadataPipelineConfig, w metadataPipelineWALRecords, refs map[int]chunks.HeadSeriesRef, fromAppend int) error {
	if c.Source != "wal" {
		if len(w.metadata) > 0 {
			return fmt.Errorf("%d metadata records, want none for source %s", len(w.metadata), c.Source)
		}
		return nil
	}
	want, err := metadataPipelineLegacyEntries(c, refs, c.appends(), fromAppend)
	if err != nil {
		return err
	}
	got, err := w.decodeLegacy(w.ids)
	if err != nil {
		return err
	}
	if c.Mixed {
		// Transactions log a record per sample-type batch.
		want.records = len(w.metadata)
	}
	return want.check("WAL", got, len(w.metadata), w.payload())
}

// checkMetadataPipelineWAL applies the metadata oracle to a closed database's
// WAL. For restarts, it reads only the segments written after the restart,
// whose refs come from the checkpoint and earlier segments' series records.
func checkMetadataPipelineWAL(c metadataPipelineConfig, walDir string, restart *metadataPipelineRestart) error {
	first, fromAppend := 0, 0
	if restart != nil {
		first, fromAppend = restart.FirstSegment, len(c.appends())-1
	}
	w, err := readMetadataPipelineWAL(walDir, first)
	if err != nil {
		return err
	}
	refs := w.refs()
	if restart != nil {
		// Restarted storage logs no series records for replayed series.
		refs = restart.refs
		w.ids = map[chunks.HeadSeriesRef]int{}
		for id, ref := range refs {
			w.ids[ref] = id
		}
	}
	if len(refs) != c.residentSeries() {
		return fmt.Errorf("%d series records, want %d", len(refs), c.residentSeries())
	}
	return metadataPipelineMetadataOracle(c, w, refs, fromAppend)
}

// metadataPipelineCheckpointOracle checks the metadata in the restart
// trace's checkpoint, which must include the whole metadata history. The
// default describes builds whose checkpoints keep each series' latest legacy
// entry; builds that log native entries replace it.
var metadataPipelineCheckpointOracle = metadataPipelineLegacyCheckpointOracle

func metadataPipelineLegacyCheckpointOracle(c metadataPipelineConfig, w metadataPipelineWALRecords) error {
	if c.Source != "wal" {
		if len(w.metadata) > 0 {
			return fmt.Errorf("checkpoint: %d metadata records, want none for source %s", len(w.metadata), c.Source)
		}
		return nil
	}
	var want metadataPipelineExpected
	var enc record.Encoder
	refs := w.refs()
	for slot := range c.Series {
		m := c.metadata(slot, metadataPipelineRestartHistory)
		entry := record.RefMetadata{Ref: refs[slot], Type: record.GetMetricType(m.Type), Unit: m.Unit, Help: m.Help}
		want.add(metadataPipelineLegacyKey(slot, entry), len(enc.Metadata([]record.RefMetadata{entry}, nil))-1)
	}
	got, err := w.decodeLegacy(w.ids)
	if err != nil {
		return err
	}
	// Checkpoints may split entries across records; framing counts per record.
	want.records = len(w.metadata)
	return want.check("checkpoint", got, len(w.metadata), w.payload())
}

// checkMetadataPipelineCheckpoint checks a restart trace's checkpoint: every
// series is kept, and its metadata matches the oracle.
func checkMetadataPipelineCheckpoint(c metadataPipelineConfig, dir string) (map[int]chunks.HeadSeriesRef, error) {
	w, err := readMetadataPipelineWAL(dir, 0)
	if err != nil {
		return nil, err
	}
	refs := w.refs()
	if len(w.ids) != c.Series || len(refs) != c.Series {
		return nil, fmt.Errorf("checkpoint keeps %d series refs for %d IDs, want %d", len(w.ids), len(refs), c.Series)
	}
	for id := range c.Series {
		if _, ok := refs[id]; !ok {
			return nil, fmt.Errorf("checkpoint lacks series %d", id)
		}
	}
	return refs, metadataPipelineCheckpointOracle(c, w)
}

// metadataPipelineHeapAttribution attributes in-use heap bytes by allocation
// stack: to the sender (remote-write queues, their WAL watcher and HTTP
// clients), to the fixture, or to everything else, chiefly the Head. It needs
// a memory profile rate of 1. Allocation stacks do not establish which object
// retains an allocation, so this approximates ownership.
type metadataPipelineHeapAttribution struct {
	Sender, Fixture, Other   int64
	SenderObjects            int64
	ProfiledInUse, HeapAlloc uint64
	SenderBytesByFunction    map[string]int64 `json:",omitempty"`
}

// metadataPipelineSenderFrame classifies a frame: whether it decides an
// allocation's owner, and whether that owner is the sender or the fixture.
func metadataPipelineSenderFrame(function string) (decides, sender, fixture bool) {
	const module = "github.com/prometheus/prometheus/"
	switch {
	case strings.HasPrefix(function, module+"storage/remote."):
		if strings.Contains(function, "etadataPipeline") {
			return true, false, true
		}
		return true, true, false
	case strings.HasPrefix(function, module+"tsdb/wlog.(*Watcher)"),
		strings.HasPrefix(function, module+"tsdb/wlog.NewWatcher"),
		strings.HasPrefix(function, module+"tsdb/wlog.(*LiveReader)"),
		strings.HasPrefix(function, module+"tsdb/wlog.NewLiveReader"):
		return true, true, false
	case strings.HasPrefix(function, "net/http.(*Transport)"), strings.HasPrefix(function, "net/http.(*persistConn)"):
		// The receivers are subprocesses, so remote write owns every HTTP client.
		return true, true, false
	case strings.HasPrefix(function, module):
		// Shared libraries are transparent; their caller decides.
		for _, library := range []string{"tsdb/encoding.", "tsdb/record.", "tsdb/nativemetadata.", "tsdb/chunks.", "tsdb/chunkenc.", "tsdb/fileutil.", "tsdb/wlog.", "model/", "util/", "prompb/", "storage.", "config."} {
			if strings.HasPrefix(function, module+library) {
				return false, false, false
			}
		}
		return true, false, false
	}
	return false, false, false
}

// metadataPipelineSenderHeap attributes the in-use heap after two collections.
// Callers keep the sender and its queues alive.
func metadataPipelineSenderHeap() (metadataPipelineHeapAttribution, error) {
	var a metadataPipelineHeapAttribution
	if runtime.MemProfileRate != 1 {
		return a, fmt.Errorf("sender heap attribution needs a memory profile rate of 1, not %d", runtime.MemProfileRate)
	}
	runtime.GC()
	runtime.GC()
	var records []runtime.MemProfileRecord
	for n, ok := runtime.MemProfile(nil, true); !ok; {
		records = make([]runtime.MemProfileRecord, n+64)
		if n, ok = runtime.MemProfile(records, true); ok {
			records = records[:n]
		}
	}
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	a.HeapAlloc = stats.HeapAlloc
	a.SenderBytesByFunction = map[string]int64{}
	for _, r := range records {
		bytes := r.InUseBytes()
		if bytes == 0 {
			continue
		}
		a.ProfiledInUse += uint64(bytes)
		frames := runtime.CallersFrames(r.Stack())
		owner, decider := "other", ""
		for {
			frame, more := frames.Next()
			if decides, sender, fixture := metadataPipelineSenderFrame(frame.Function); decides {
				decider = frame.Function
				switch {
				case sender:
					owner = "sender"
				case fixture:
					owner = "fixture"
				}
				break
			}
			if !more {
				break
			}
		}
		switch owner {
		case "sender":
			a.Sender += bytes
			a.SenderObjects += r.InUseObjects()
			a.SenderBytesByFunction[decider] += bytes
		case "fixture":
			a.Fixture += bytes
		default:
			a.Other += bytes
		}
	}
	return a, nil
}

func TestRemoteWriteMetadataPipelineOracles(t *testing.T) {
	// Seed transactions of four and two series log one metadata record each;
	// unchanged sweeps log none.
	c := metadataPipelineConfig{Case: "unchanged", Source: "wal", Series: 6, Values: 6, Sweeps: 2, Writers: 2, CommitSize: 4, Base: 1000}
	legacy := func(id int, ref chunks.HeadSeriesRef) record.RefMetadata {
		m := c.metadata(id, 0)
		return record.RefMetadata{Ref: ref, Type: record.GetMetricType(m.Type), Unit: m.Unit, Help: m.Help}
	}
	writeWAL := func(t *testing.T, ref func(id int) chunks.HeadSeriesRef, records ...[]record.RefMetadata) string {
		dir := t.TempDir()
		w, err := wlog.New(nil, nil, dir, compression.None)
		require.NoError(t, err)
		var enc record.Encoder
		var series []record.RefSeries
		for id := range c.Series {
			series = append(series, record.RefSeries{Ref: ref(id), Labels: metadataPipelineLabels(id)})
		}
		require.NoError(t, w.Log(enc.Series(series, nil)))
		for _, entries := range records {
			require.NoError(t, w.Log(enc.Metadata(entries, nil)))
		}
		require.NoError(t, w.Close())
		return dir
	}
	seed := func(ref func(id int) chunks.HeadSeriesRef) [][]record.RefMetadata {
		var first, second []record.RefMetadata
		for id := range 4 {
			first = append(first, legacy(id, ref(id)))
		}
		for id := 4; id < 6; id++ {
			second = append(second, legacy(id, ref(id)))
		}
		return [][]record.RefMetadata{first, second}
	}
	inOrder := func(id int) chunks.HeadSeriesRef { return chunks.HeadSeriesRef(id + 1) }
	// A different ref assignment, with refs of two uvarint sizes.
	permuted := func(id int) chunks.HeadSeriesRef { return chunks.HeadSeriesRef(200 - 30*id) }

	t.Run("legacy WAL entries", func(t *testing.T) {
		for _, ref := range []func(int) chunks.HeadSeriesRef{inOrder, permuted} {
			require.NoError(t, checkMetadataPipelineWAL(c, writeWAL(t, ref, seed(ref)...), nil))
		}
		records := seed(inOrder)
		changed := legacy(5, inOrder(5))
		changed.Help += "x"
		for name, mutated := range map[string][][]record.RefMetadata{
			"missing entry":  {records[0], records[1][:1]},
			"extra entry":    {records[0], append(slices.Clone(records[1]), records[1][0])},
			"changed entry":  {records[0], {records[1][0], changed}},
			"merged records": {append(slices.Clone(records[0]), records[1]...)},
			"wrong refs":     seed(permuted),
		} {
			require.Error(t, checkMetadataPipelineWAL(c, writeWAL(t, inOrder, mutated...), nil), name)
		}
		native := c
		native.Source = "native"
		require.Error(t, metadataPipelineLegacyMetadataOracle(native, metadataPipelineWALRecords{metadata: [][]byte{{byte(record.Metadata)}}}, nil, 0))
	})

	t.Run("segments after a restart", func(t *testing.T) {
		r := c
		r.Case, r.Sweeps, r.RestartStep = "restart", metadataPipelineRestartHistory+3, metadataPipelineRestartHistory+1
		dir := writeWAL(t, inOrder, seed(inOrder)...)
		// Reopening starts a new segment, as restarted storage does.
		w, err := wlog.New(nil, nil, dir, compression.None)
		require.NoError(t, err)
		require.NoError(t, w.Close())
		_, first, err := wlog.Segments(dir)
		require.NoError(t, err)
		refs := map[int]chunks.HeadSeriesRef{}
		for id := range r.Series {
			refs[id] = inOrder(id)
		}
		// Replay restores committed metadata: restarted sweeps log nothing, and
		// the segments before the restart are not checked here.
		restart := &metadataPipelineRestart{FirstSegment: first, refs: refs}
		require.NoError(t, checkMetadataPipelineWAL(r, dir, restart))
		w, err = wlog.New(nil, nil, dir, compression.None)
		require.NoError(t, err)
		var enc record.Encoder
		require.NoError(t, w.Log(enc.Metadata([]record.RefMetadata{legacy(0, inOrder(0))}, nil)))
		require.NoError(t, w.Close())
		require.Error(t, checkMetadataPipelineWAL(r, dir, restart))
	})

	t.Run("sender heap attribution", func(t *testing.T) {
		defer func(rate int) { runtime.MemProfileRate = rate }(runtime.MemProfileRate)
		runtime.MemProfileRate = 1
		const series, help = 200, 4096
		c := metadataPipelineConfig{Case: "unchanged", Source: "wal", Series: series, Values: series, HelpBytes: help, Sweeps: 2, Writers: 2, Shards: 2, Batch: 50, Capacity: 100, CommitSize: 50, ReceiverProcs: 2, Base: time.Now().Add(time.Hour).UnixMilli()}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f, err := newMetadataPipeline(ctx, c)
		require.NoError(t, err)
		defer func() { require.NoError(t, f.close()) }()
		require.NoError(t, f.open(t.TempDir()))
		require.NoError(t, f.append(ctx, 0, c.Sweeps))
		_, err = f.drain(ctx, f.expectedItems(c.Sweeps+1))
		require.NoError(t, err)
		a, err := metadataPipelineSenderHeap()
		require.NoError(t, err)
		// Distinct long help strings, decoded by the watcher, stay retained by
		// the sender, although their allocation stacks have no remote frame.
		decoded := a.SenderBytesByFunction["github.com/prometheus/prometheus/tsdb/wlog.(*Watcher).readSegment"]
		t.Logf("sender %d (decoded by the watcher %d), fixture %d, other %d, profiled %d, heap %d", a.Sender, decoded, a.Fixture, a.Other, a.ProfiledInUse, a.HeapAlloc)
		require.GreaterOrEqual(t, decoded, int64(series*help))
		require.GreaterOrEqual(t, a.Sender, decoded)
		require.Less(t, a.Sender, int64(a.ProfiledInUse))
		require.LessOrEqual(t, a.ProfiledInUse, a.HeapAlloc)
		// The fixture's own metadata copies are not the sender's.
		require.GreaterOrEqual(t, a.Fixture, int64(series*help))
		runtime.MemProfileRate = 0
		_, err = metadataPipelineSenderHeap()
		require.Error(t, err)
	})

	t.Run("two endpoints", func(t *testing.T) {
		c := metadataPipelineConfig{Case: "unchanged", Source: "wal", Series: 100, Values: 10, Sweeps: 2, Writers: 2, Shards: 2, Batch: 20, Capacity: 100, CommitSize: 50, ReceiverProcs: 2, Base: time.Now().Add(time.Hour).UnixMilli(), Endpoints: 2}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f, err := newMetadataPipeline(ctx, c)
		require.NoError(t, err)
		defer func() { require.NoError(t, f.close()) }()
		require.NoError(t, f.open(t.TempDir()))
		require.Len(t, f.extraReceivers, 1)
		require.Len(t, f.extraQueues, 1)
		require.NoError(t, f.append(ctx, 0, c.Sweeps))
		// Both receivers validate and count every item.
		_, err = f.drain(ctx, f.expectedItems(c.Sweeps+1))
		require.NoError(t, err)
		_, err = newMetadataPipeline(ctx, metadataPipelineConfig{Case: "backlog", Series: 100, Writers: 1, CommitSize: 10, Endpoints: 2})
		require.Error(t, err)
	})
}

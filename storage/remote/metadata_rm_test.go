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

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/util/compression"
)

// The resolution diagnostic's harness: the first-sighting miss path under
// interner states built from production calls. Replays time the interner
// alone; store kernels time the store under each state. Every prepared
// interner is checked against a model that builds the same state with the
// interner's own fingerprint function. This file is identical on every build
// with the WAL metadata interner.

// rmState is an interner state, named by how it is built: its generations
// before the seed, then its ledger condition after it.
type rmState struct {
	name string
	// admitted values build the generations, each sighted twice in a row.
	admitted int
	// conditioning values are sighted once each, after the seed.
	conditioning int
	// fingerprintsOnly gives the ledger a byte bound of 0, so that its slots
	// keep no values.
	fingerprintsOnly bool
}

var (
	rmGenerations = []struct {
		name     string
		admitted int
	}{{"G0", 0}, {"G1", 1}, {"Gw", 16384}, {"G2", 49152}}
	rmLedgers = []struct {
		name             string
		conditioning     int
		fingerprintsOnly bool
	}{{"L0", 0, false}, {"Lv", 8192, false}, {"Lf", 0, true}}
)

func rmStates() []rmState {
	var out []rmState
	for _, g := range rmGenerations {
		for _, l := range rmLedgers {
			out = append(out, rmState{name: g.name + l.name, admitted: g.admitted, conditioning: l.conditioning, fingerprintsOnly: l.fingerprintsOnly})
		}
	}
	return out
}

// rmStatesFor returns a workload's states. Lf changes sharing, so sb runs
// none with it.
func rmStatesFor(workload string) []rmState {
	var out []rmState
	for _, s := range rmStates() {
		if workload != "sb" || !s.fingerprintsOnly {
			out = append(out, s)
		}
	}
	return out
}

func rmStateNamed(tb testing.TB, name string) rmState {
	for _, s := range rmStates() {
		if s.name == name {
			return s
		}
	}
	tb.Fatalf("no state %s", name)
	return rmState{}
}

// rmBuildValue returns the nth value of a kind that builds states: shaped like
// db's, with a 64-byte help, and disjoint from every workload's values.
func rmBuildValue(kind string, n int) metadata.Metadata {
	prefix := fmt.Sprintf("%s %d ", kind, n)
	return metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: prefix + strings.Repeat("x", 64-len(prefix))}
}

func (s rmState) newInterner() *metadataInterner {
	ledgerBytes := metadataInternerLedgerBytes
	if s.fingerprintsOnly {
		ledgerBytes = 0
	}
	return newLedgerMetadataInterner(metadataInternerEntries, metadataInternerBytes, metadataInternerLedgerSets, ledgerBytes)
}

// buildGenerations admits s's values, sighting each twice in a row: the first
// sighting takes a ledger slot, and the second admits the value and frees it.
func (s rmState) buildGenerations(intern func(metadata.Metadata) *metadata.Metadata) {
	for n := range s.admitted {
		v := rmBuildValue("state", n)
		intern(v)
		intern(v)
	}
}

func (s rmState) condition(intern func(metadata.Metadata) *metadata.Metadata) {
	for n := range s.conditioning {
		intern(rmBuildValue("ledger", n))
	}
}

// rmSnapshot is an interner's state in counts.
type rmSnapshot struct {
	current, older, genBytes, occupied, kept, ledgerBytes int
}

func rmSnapshotOf(i *metadataInterner) rmSnapshot {
	s := rmSnapshot{current: len(i.current), older: len(i.older), genBytes: i.bytes, ledgerBytes: i.ledgerBytes}
	for _, slot := range i.ledger {
		if slot.fingerprint != 0 {
			s.occupied++
		}
		if slot.value != nil {
			s.kept++
		}
	}
	return s
}

func rmModelSnapshot(m *dsModel) rmSnapshot {
	s := rmSnapshot{current: len(m.current), older: len(m.older), genBytes: m.bytes, ledgerBytes: m.ledgerBytes}
	for _, slot := range m.ledger {
		if slot.fingerprint != 0 {
			s.occupied++
		}
		if slot.kept {
			s.kept++
		}
	}
	return s
}

// rmPaths counts the paths sightings take through the interner, as the model
// predicts them.
type rmPaths struct {
	hitCurrent, hitOlder, admitKept, admitNew, missFree, missKept, missPrint, oversize int
	// hashingLookups are generation lookups in a non-empty generation, which
	// hash the value; a lookup in an empty one returns before hashing.
	hashingLookups, fingerprints  int
	newValues, newObjects, copied int
}

// rmOwnObjects returns the objects ownMetadata allocates for m: the value,
// and one payload if any string it copies is non-empty.
func rmOwnObjects(m metadata.Metadata) int {
	if dsOwnedBytes(m) > 0 {
		return 2
	}
	return 1
}

// rmClassify counts in p the path v takes, from the model's state before it
// sights v. It reads the model and changes nothing.
func rmClassify(m *dsModel, v metadata.Metadata, p *rmPaths) {
	created := func() {
		p.newValues++
		p.newObjects += rmOwnObjects(v)
		p.copied += dsOwnedBytes(v)
	}
	if metadataCost(v) > m.limit {
		p.oversize++
		created()
		return
	}
	if len(m.current) > 0 {
		p.hashingLookups++
	}
	if _, ok := m.current[v]; ok {
		p.hitCurrent++
		return
	}
	if len(m.older) > 0 {
		p.hashingLookups++
	}
	if _, ok := m.older[v]; ok {
		p.hitOlder++
		return
	}
	p.fingerprints++
	fingerprint := m.fingerprint(v)
	if fingerprint == 0 {
		fingerprint = 1
	}
	set := int(fingerprint & uint64(len(m.next)-1))
	ways := m.ledger[set*metadataLedgerWays : (set+1)*metadataLedgerWays]
	free := -1
	for w := range ways {
		slot := &ways[w]
		if slot.fingerprint == fingerprint && (!slot.kept || slot.value == v) {
			if slot.kept {
				p.admitKept++
			} else {
				p.admitNew++
				created()
			}
			return
		}
		if slot.fingerprint == 0 && free < 0 {
			free = w
		}
	}
	created()
	switch {
	case free >= 0:
		p.missFree++
	case ways[m.next[set]].kept:
		p.missKept++
	default:
		p.missPrint++
	}
}

// rmModeled is a model of an interner built in a state, with the snapshots
// after the seed and after conditioning, the paths, values and objects of the
// change sightings, and each point's object.
type rmModeled struct {
	m                  *dsModel
	postSeed, postCond rmSnapshot
	paths              rmPaths
	changeValues       []metadata.Metadata
	changeIDs          []int
	objects            map[dsPointRef]int
}

// rmModel builds s through a model of i: s's generations, the seed's
// sightings, s's conditioning, and, if applied, the change sightings.
func rmModel(i *metadataInterner, s rmState, seed, changes []dsSighting, applied bool) *rmModeled {
	r := &rmModeled{m: newDSModel(i), objects: map[dsPointRef]int{}}
	s.buildGenerations(func(v metadata.Metadata) *metadata.Metadata { r.m.intern(v); return nil })
	for _, sg := range seed {
		id := r.m.intern(sg.value)
		for _, p := range sg.points {
			r.objects[p] = id
		}
	}
	r.postSeed = rmModelSnapshot(r.m)
	s.condition(func(v metadata.Metadata) *metadata.Metadata { r.m.intern(v); return nil })
	r.postCond = rmModelSnapshot(r.m)
	if applied {
		for _, sg := range changes {
			rmClassify(r.m, sg.value, &r.paths)
			id := r.m.intern(sg.value)
			r.changeValues, r.changeIDs = append(r.changeValues, sg.value), append(r.changeIDs, id)
			for _, p := range sg.points {
				r.objects[p] = id
			}
		}
	}
	return r
}

// rmMismatch returns the first difference between i and the model: the
// bounds, the generations and their values' content, the ledger and its
// replacement order, the returned values' content, and which objects the
// ledger, the generations and, if set, qm's histories and the returned values
// share.
func rmMismatch(i *metadataInterner, r *rmModeled, qm *QueueManager, returned []*metadata.Metadata) error {
	m := r.m
	if len(i.current) > i.entries || len(i.older) > i.entries || i.bytes > i.limit || i.ledgerBytes > i.ledgerLimit {
		return fmt.Errorf("bounds exceeded: generations %d and %d values in %d bytes, ledger %d bytes", len(i.current), len(i.older), i.bytes, i.ledgerBytes)
	}
	if len(i.current) != len(m.current) || len(i.older) != len(m.older) || i.bytes != m.bytes {
		return fmt.Errorf("generations hold %d and %d values in %d bytes, the model %d and %d in %d", len(i.current), len(i.older), i.bytes, len(m.current), len(m.older), m.bytes)
	}
	if i.ledgerBytes != m.ledgerBytes {
		return fmt.Errorf("the ledger holds %d bytes, the model %d", i.ledgerBytes, m.ledgerBytes)
	}
	for set := range i.next {
		if i.next[set] != m.next[set] {
			return fmt.Errorf("ledger set %d replaces slot %d next, the model %d", set, i.next[set], m.next[set])
		}
	}
	byPointer, byObject := map[*metadata.Metadata]int{}, map[int]*metadata.Metadata{}
	match := func(p *metadata.Metadata, id int, where string) error {
		if q, ok := byObject[id]; ok && q != p {
			return fmt.Errorf("%s: the model's object %d is two objects", where, id)
		}
		if other, ok := byPointer[p]; ok && other != id {
			return fmt.Errorf("%s: one object is the model's objects %d and %d", where, other, id)
		}
		byPointer[p], byObject[id] = id, p
		return nil
	}
	for idx, slot := range i.ledger {
		ms := m.ledger[idx]
		if slot.fingerprint != ms.fingerprint || (slot.value != nil) != ms.kept {
			return fmt.Errorf("ledger slot %d differs from the model", idx)
		}
		if slot.value != nil {
			if *slot.value != ms.value {
				return fmt.Errorf("ledger slot %d holds %+v, the model %+v", idx, *slot.value, ms.value)
			}
			if err := match(slot.value, ms.object, fmt.Sprintf("ledger slot %d", idx)); err != nil {
				return err
			}
		}
	}
	for _, gen := range []struct {
		actual map[metadata.Metadata]*metadata.Metadata
		model  map[metadata.Metadata]int
	}{{i.current, m.current}, {i.older, m.older}} {
		for k, id := range gen.model {
			v, ok := gen.actual[k]
			if !ok {
				return fmt.Errorf("a generation lacks %+v", k)
			}
			if v == nil {
				return fmt.Errorf("a generation holds nil for %+v", k)
			}
			if *v != k {
				return fmt.Errorf("a generation holds %+v for %+v", *v, k)
			}
			if err := match(v, id, "a generation"); err != nil {
				return err
			}
		}
	}
	if returned != nil {
		if len(returned) != len(r.changeIDs) {
			return fmt.Errorf("%d values returned, the model %d", len(returned), len(r.changeIDs))
		}
		for k, v := range returned {
			if v == nil {
				return fmt.Errorf("returned value %d is nil", k)
			}
			if *v != r.changeValues[k] {
				return fmt.Errorf("returned value %d is %+v, not %+v", k, *v, r.changeValues[k])
			}
			if err := match(v, r.changeIDs[k], fmt.Sprintf("returned value %d", k)); err != nil {
				return err
			}
		}
	}
	if qm != nil {
		var versions []dsVersion
		for slot := range w1Series {
			ref := chunks.HeadSeriesRef(slot + 1)
			versions, _ = dsVersions(qm, ref, versions[:0])
			for _, v := range versions {
				id, ok := r.objects[dsPointRef{ref: ref, from: v.from}]
				if !ok {
					return fmt.Errorf("series %d's version at %d was never sighted", ref, v.from)
				}
				if err := match(v.m, id, fmt.Sprintf("series %d", ref)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// rmCheckState models i as built in s from records and, if applied, after
// the change sightings, and returns the model's change paths and the first
// difference: from the snapshots taken after the seed and after
// conditioning, then as rmMismatch compares.
func rmCheckState(tb testing.TB, i *metadataInterner, s rmState, records *w1Records, applied bool, postSeed, postCond rmSnapshot, qm *QueueManager, returned []*metadata.Metadata) (rmPaths, error) {
	r := rmModel(i, s, dsSightingsOf(tb, records.seed), dsSightingsOf(tb, records.changes), applied)
	if postSeed != r.postSeed {
		return r.paths, fmt.Errorf("after the seed, the interner is %+v, the model %+v", postSeed, r.postSeed)
	}
	if postCond != r.postCond {
		return r.paths, fmt.Errorf("after conditioning, the interner is %+v, the model %+v", postCond, r.postCond)
	}
	return r.paths, rmMismatch(i, r, qm, returned)
}

func rmMustCheckState(tb testing.TB, i *metadataInterner, s rmState, records *w1Records, applied bool, postSeed, postCond rmSnapshot, qm *QueueManager, returned []*metadata.Metadata) rmPaths {
	paths, err := rmCheckState(tb, i, s, records, applied, postSeed, postCond, qm, returned)
	if err != nil {
		tb.Fatalf("state %s: %v", s.name, err)
	}
	return paths
}

// rmStateReports accumulates each iteration's snapshots and paths, which runs
// report as medians.
type rmStateReports struct {
	seed, cond []rmSnapshot
	paths      []rmPaths
}

func (r *rmStateReports) add(seed, cond rmSnapshot, paths rmPaths) {
	r.seed, r.cond, r.paths = append(r.seed, seed), append(r.cond, cond), append(r.paths, paths)
}

func (r *rmStateReports) report(b *testing.B) {
	median := func(n int, field func(int) int) float64 {
		values := make([]float64, n)
		for k := range values {
			values[k] = float64(field(k))
		}
		return dsMedian(values)
	}
	for _, s := range []struct {
		prefix string
		snaps  []rmSnapshot
	}{{"seed", r.seed}, {"cond", r.cond}} {
		n := len(s.snaps)
		b.ReportMetric(median(n, func(k int) int { return s.snaps[k].current }), s.prefix+"-current")
		b.ReportMetric(median(n, func(k int) int { return s.snaps[k].older }), s.prefix+"-older")
		b.ReportMetric(median(n, func(k int) int { return s.snaps[k].genBytes }), s.prefix+"-gen-B")
		b.ReportMetric(median(n, func(k int) int { return s.snaps[k].occupied }), s.prefix+"-ledger-occupied")
		b.ReportMetric(median(n, func(k int) int { return s.snaps[k].kept }), s.prefix+"-ledger-kept")
		b.ReportMetric(median(n, func(k int) int { return s.snaps[k].ledgerBytes }), s.prefix+"-ledger-B")
	}
	p, n := r.paths, len(r.paths)
	for unit, field := range map[string]func(k int) int{
		"path-hit-current": func(k int) int { return p[k].hitCurrent }, "path-hit-older": func(k int) int { return p[k].hitOlder },
		"path-admit-kept": func(k int) int { return p[k].admitKept }, "path-admit-new": func(k int) int { return p[k].admitNew },
		"path-miss-free": func(k int) int { return p[k].missFree }, "path-miss-kept": func(k int) int { return p[k].missKept },
		"path-miss-print": func(k int) int { return p[k].missPrint }, "path-oversize": func(k int) int { return p[k].oversize },
		"hashing-lookups": func(k int) int { return p[k].hashingLookups }, "fingerprints": func(k int) int { return p[k].fingerprints },
		"new-objects": func(k int) int { return p[k].newObjects },
	} {
		b.ReportMetric(median(n, field), unit)
	}
}

// The replays' sinks. Each holds at most one iteration's last result, and the
// iteration clears the pointer sinks once its window ends.
var (
	rmSink      *metadata.Metadata
	rmSinkFound *metadata.Metadata
	rmSinkBits  uint64
	rmSinkHits  int
)

// rmWindows supplies a replay iteration's timing and GC control: full times
// the full replay through the benchmark's timer, and cpu reads the clock the
// contextual windows are timed with.
type rmWindows struct {
	full        func(work func())
	gcOff, gcOn func()
	cpu         func() time.Duration
}

// rmReplayer is one replay run's inputs and accumulators. Its inputs, the
// seed's and the changes' values in the store's order, are captured once and
// live throughout the run. Nothing it keeps across iterations refers to an
// interner or a replay's output.
type rmReplayer struct {
	state                    rmState
	records                  *w1Records
	seed, values             []metadata.Metadata
	owned                    []*metadata.Metadata
	fingerprint, own, lookup time.Duration
	reports                  rmStateReports
}

func newRMReplayer(tb testing.TB, workload string, s rmState) *rmReplayer {
	r := &rmReplayer{state: s, records: w1RecordsFor(tb, workload)}
	for _, sg := range dsSightingsOf(tb, r.records.seed) {
		r.seed = append(r.seed, sg.value)
	}
	for _, sg := range dsSightingsOf(tb, r.records.changes) {
		r.values = append(r.values, sg.value)
	}
	r.owned = make([]*metadata.Metadata, len(r.values))
	return r
}

// prepare builds a fresh interner in r's state: its generations, the seed's
// sightings, then its conditioning. It returns the interner and its
// snapshots after the seed and after conditioning.
func (r *rmReplayer) prepare() (*metadataInterner, rmSnapshot, rmSnapshot) {
	i := r.state.newInterner()
	r.state.buildGenerations(i.internBorrowed)
	for _, v := range r.seed {
		i.internBorrowed(v)
	}
	postSeed := rmSnapshotOf(i)
	r.state.condition(i.internBorrowed)
	return i, postSeed, rmSnapshotOf(i)
}

// iterate runs one iteration. The full replay runs on an instance prepared for
// it alone, in its own window, which no contextual pass in the iteration
// precedes; the previous iteration's come before this one's preparation and
// forced collection. The contextual replays then run on a second,
// independently prepared instance, each in its own window. ev sees each step
// with the instance it acts on.
func (r *rmReplayer) iterate(tb testing.TB, w rmWindows, ev func(step string, i *metadataInterner)) {
	full, seedState, condState := r.prepare()
	ev("prepare full", full)
	runtime.GC()
	w.gcOff()
	ev("full", full)
	w.full(func() {
		var last *metadata.Metadata
		for _, v := range r.values {
			last = full.internBorrowed(v)
		}
		rmSink = last
	})
	w.gcOn()
	rmSink = nil
	r.reports.add(seedState, condState, rmMustCheckState(tb, full, r.state, r.records, true, seedState, condState, nil, nil))
	ev("checked full", full)
	full = nil

	ctx, ctxSeed, ctxCond := r.prepare()
	ev("prepare contextual", ctx)
	runtime.GC()
	w.gcOff()
	ev("fingerprint", ctx)
	t0 := w.cpu()
	var bits uint64
	for _, v := range r.values {
		bits ^= ctx.fingerprint(v)
	}
	t1 := w.cpu()
	ev("own", ctx)
	t2 := w.cpu()
	for k, v := range r.values {
		r.owned[k] = ownMetadata(v)
	}
	t3 := w.cpu()
	ev("lookup", ctx)
	t4 := w.cpu()
	var hits int
	var found *metadata.Metadata
	for _, v := range r.values {
		if p, ok := ctx.current[v]; ok {
			hits, found = hits+1, p
		} else if p, ok := ctx.older[v]; ok {
			hits, found = hits+1, p
		}
	}
	t5 := w.cpu()
	rmSinkBits, rmSinkHits, rmSinkFound = bits, hits, found
	clear(r.owned)
	w.gcOn()
	rmSinkFound = nil
	r.fingerprint, r.own, r.lookup = r.fingerprint+t1-t0, r.own+t3-t2, r.lookup+t5-t4
	// The contextual passes read the instance only.
	rmMustCheckState(tb, ctx, r.state, r.records, false, ctxSeed, ctxCond, nil, nil)
	ev("checked contextual", ctx)
}

// count replays the changes once, untimed and with automatic GC off, keeping
// what each sighting returns, and checks those values' sharing against the
// model. It returns the objects the replay allocated, counted exactly, and
// the model's paths.
func (r *rmReplayer) count(tb testing.TB) (uint64, rmPaths) {
	i, postSeed, postCond := r.prepare()
	returned := make([]*metadata.Metadata, len(r.values))
	runtime.GC()
	previous := debug.SetGCPercent(-1)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for k, v := range r.values {
		returned[k] = i.internBorrowed(v)
	}
	runtime.ReadMemStats(&after)
	debug.SetGCPercent(previous)
	paths := rmMustCheckState(tb, i, r.state, r.records, true, postSeed, postCond, nil, returned)
	return after.Mallocs - before.Mallocs, paths
}

// retained returns the heap a prepared instance retains: the live heap at a
// forced collection after building it, less that before.
func (r *rmReplayer) retained() float64 {
	sample := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	runtime.GC()
	metrics.Read(sample)
	before := sample[0].Value.Uint64()
	i, _, _ := r.prepare()
	runtime.GC()
	metrics.Read(sample)
	after := sample[0].Value.Uint64()
	runtime.KeepAlive(i)
	return float64(after) - float64(before)
}

// BenchmarkMetadataRMReplay replays each workload's change values through an
// interner in each state: the full call in a window of its own, then
// fingerprints, owned copies and generation lookups on another instance,
// reported only and never summed.
func BenchmarkMetadataRMReplay(b *testing.B) {
	for _, workload := range dsWorkloads {
		for _, s := range rmStatesFor(workload) {
			b.Run("workload="+workload+"/state="+s.name, func(b *testing.B) {
				timer := newDSTimer(b)
				r := newRMReplayer(b, workload, s)
				objects, paths := r.count(b)
				if workload != "sb" && objects != uint64(paths.newObjects) {
					b.Fatalf("the replay allocated %d objects, the model %d", objects, paths.newObjects)
				}
				retained := r.retained()
				w := rmWindows{full: timer.time, gcOff: func() { timer.gcOff(b) }, gcOn: func() { timer.gcOn(b) }, cpu: w1CPU}
				ev := func(string, *metadataInterner) {}
				timer.start()
				for range b.N {
					r.iterate(b, w, ev)
				}
				timer.report("value", len(r.values), true)
				n := float64(b.N) * float64(len(r.values))
				b.ReportMetric(float64(r.fingerprint.Nanoseconds())/n, "fingerprint-ns/value")
				b.ReportMetric(float64(r.own.Nanoseconds())/n, "own-ns/value")
				b.ReportMetric(float64(r.lookup.Nanoseconds())/n, "lookup-ns/value")
				b.ReportMetric(float64(objects), "replay-objects")
				b.ReportMetric(retained, "state-retained-B")
				r.reports.report(b)
			})
		}
	}
}

// newRMStoreRun builds a store kernel's iteration: a process interner in
// state s, its generations built, the seed's records stored, then its
// conditioning. It returns the run, the interner and its snapshots after the
// seed and after conditioning.
func newRMStoreRun(tb testing.TB, workload string, s rmState) (*dsRun, *metadataInterner, rmSnapshot, rmSnapshot) {
	previous := walMetadataInterner
	i := s.newInterner()
	walMetadataInterner = i
	s.buildGenerations(i.internBorrowed)
	r := &dsRun{
		workload: workload, records: w1RecordsFor(tb, workload), decBuf: compression.NewSyncDecodeBuffer(),
		restore: func() { walMetadataInterner, w1InternHook = previous, nil },
	}
	r.qm = w1NewQueueManager(tb)
	r.decoder = newW1Decoder()
	for _, rec := range r.records.seed {
		r.decoder.decode(tb, rec)
		r.decoder.store(r.qm)
	}
	postSeed := rmSnapshotOf(i)
	s.condition(i.internBorrowed)
	return r, i, postSeed, rmSnapshotOf(i)
}

// rmCount counts, in a run of its own, what storing the change records does
// under state s, as dsCount does with a fresh interner, and checks the counts
// against the model's.
func rmCount(tb testing.TB, workload string, s rmState) dsCounts {
	var c dsCounts
	r, i, postSeed, postCond := newRMStoreRun(tb, workload, s)
	defer r.close()
	counter := dsNewCounter(r)
	before := dsTotalVersions(r.qm)
	caps := map[chunks.HeadSeriesRef]int{}
	for _, compressed := range r.records.compressed {
		rec, err := compression.Decode(compression.Snappy, compressed, r.decBuf)
		require.NoError(tb, err)
		r.decoder.decode(tb, rec)
		refs := dsDecodedRefs(r.decoder, nil)
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
	versions, _ := dsVersions(r.qm, 1, nil)
	c.depth = float64(len(versions))
	paths := rmMustCheckState(tb, i, s, r.records, true, postSeed, postCond, r.qm, nil)
	resolutions := paths.hitCurrent + paths.hitOlder + paths.admitKept + paths.admitNew + paths.missFree + paths.missKept + paths.missPrint + paths.oversize
	require.Equal(tb, [3]float64{float64(resolutions), float64(paths.newValues), float64(paths.copied)}, [3]float64{c.resolutions, c.newValues, c.copied},
		"the counting run's interner calls, new values and copied bytes against the model's")
	return c
}

// BenchmarkMetadataRMStore (GC off) and BenchmarkMetadataRMStoreGC (GC on,
// context only) time the diagnostic's sender window with the interner in each
// state.
func BenchmarkMetadataRMStore(b *testing.B)   { rmStoreKernel(b, false) }
func BenchmarkMetadataRMStoreGC(b *testing.B) { rmStoreKernel(b, true) }

func rmStoreKernel(b *testing.B, gcOn bool) {
	for _, workload := range dsWorkloads {
		for _, s := range rmStatesFor(workload) {
			b.Run("workload="+workload+"/state="+s.name, func(b *testing.B) {
				timer := newDSTimer(b)
				records := w1RecordsFor(b, workload)
				counts := rmCount(b, workload, s)
				var reports rmStateReports
				timer.start()
				for range b.N {
					r, i, postSeed, postCond := newRMStoreRun(b, workload, s)
					runtime.GC()
					if !gcOn {
						timer.gcOff(b)
					}
					timer.time(func() { r.apply(b) })
					dsCheckHistories(b, r.qm, workload, func(int) int { return w1Sweeps }, nil)
					reports.add(postSeed, postCond, rmMustCheckState(b, i, s, records, true, postSeed, postCond, r.qm, nil))
					r.close()
					if !gcOn {
						timer.gcOn(b)
					}
				}
				timer.report("point", records.points(), !gcOn)
				counts.report(b, len(records.changes))
				reports.report(b)
			})
		}
	}
}

// TestMetadataRMStates checks every state as built: the snapshots and the
// interner against the model, the generations' exact contents where they
// don't depend on the fingerprint seed, and that the model detects a state
// built differently from its name.
func TestMetadataRMStates(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	wantGenerations := map[string][2]int{"G0": {0, 0}, "G1": {1, 0}, "Gw": {16384, 0}, "G2": {16384, 32768}}
	for _, workload := range dsWorkloads {
		for _, s := range rmStatesFor(workload) {
			t.Run("workload="+workload+"/state="+s.name, func(t *testing.T) {
				r := newRMReplayer(t, workload, s)
				i, postSeed, postCond := r.prepare()
				_, err := rmCheckState(t, i, s, r.records, false, postSeed, postCond, nil, nil)
				require.NoError(t, err)
				g := wantGenerations[s.name[:2]]
				if workload == "sb" {
					// The seed's 100 values are admitted, which the model checks.
					require.GreaterOrEqual(t, postSeed.current, g[0])
				} else {
					want := rmSnapshot{current: g[0], older: g[1], genBytes: g[0] * metadataCost(rmBuildValue("state", 0))}
					got := postSeed
					got.occupied, got.kept, got.ledgerBytes = 0, 0, 0
					require.Equal(t, want, got, "the generations after the seed")
					require.Equal(t, postSeed.current, postCond.current, "conditioning admits nothing")
				}
				if s.fingerprintsOnly {
					require.Zero(t, postCond.kept)
					require.Zero(t, postCond.ledgerBytes)
				} else if workload == "db" {
					require.Equal(t, postCond.occupied, postCond.kept, "every db value fits the ledger")
				}
				// A state built differently fails the check.
				for _, other := range []rmState{
					{name: s.name, admitted: s.admitted + 1, conditioning: s.conditioning, fingerprintsOnly: s.fingerprintsOnly},
					{name: s.name, admitted: s.admitted, conditioning: s.conditioning + 1, fingerprintsOnly: s.fingerprintsOnly},
				} {
					_, err := rmCheckState(t, i, other, r.records, false, postSeed, postCond, nil, nil)
					require.Error(t, err, "a state built with %d admitted and %d conditioning values", other.admitted, other.conditioning)
				}
			})
		}
	}
}

// TestMetadataRMReplay checks the replays' protocol and what they keep: the
// full replay runs on its own instance, in its own window, with no contextual
// pass on that instance or before its window; every sink is cleared and no
// instance outlives its iteration; the counting run's allocations and the
// values it returns agree with the model; and the paths are as the workloads
// fix them.
func TestMetadataRMReplay(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	previous := 100
	noTiming := rmWindows{
		full:  func(work func()) { work() },
		gcOff: func() { previous = debug.SetGCPercent(-1) },
		gcOn:  func() { debug.SetGCPercent(previous) },
		cpu:   w1CPU,
	}
	t.Run("windows and lifetimes", func(t *testing.T) {
		r := newRMReplayer(t, "db", rmStateNamed(t, "GwLv"))
		for range 2 {
			var steps []string
			instances := map[string]*metadataInterner{}
			var probes []weak.Pointer[metadataInterner]
			r.iterate(t, noTiming, func(step string, i *metadataInterner) {
				steps = append(steps, step)
				if _, ok := instances[step]; !ok {
					probes = append(probes, weak.Make(i))
				}
				instances[step] = i
			})
			require.Equal(t, []string{"prepare full", "full", "checked full", "prepare contextual", "fingerprint", "own", "lookup", "checked contextual"}, steps)
			full := instances["prepare full"]
			require.Same(t, full, instances["full"])
			for _, step := range []string{"fingerprint", "own", "lookup", "checked contextual"} {
				require.Same(t, instances["prepare contextual"], instances[step], step)
				require.NotSame(t, full, instances[step], step)
			}
			clear(instances)
			require.Nil(t, rmSink)
			require.Nil(t, rmSinkFound)
			for k, v := range r.owned {
				require.Nil(t, v, "owned copy %d kept", k)
			}
			runtime.GC()
			for k, p := range probes {
				require.Nil(t, p.Value(), "instance %d outlives its iteration", k)
			}
		}
		// Each iteration's full replay was checked against the model, which
		// gave its paths.
		require.Len(t, r.reports.paths, 2)
		for _, p := range r.reports.paths {
			require.Equal(t, len(r.values), p.fingerprints)
		}
	})
	t.Run("counting run", func(t *testing.T) {
		for _, workload := range []string{"db", "dv", "sb"} {
			for _, s := range rmStatesFor(workload) {
				r := newRMReplayer(t, workload, s)
				objects, paths := r.count(t)
				g := map[string]int{"G0": 0, "G1": 1, "Gw": 1, "G2": 2}[s.name[:2]]
				n := len(r.values)
				if workload == "sb" {
					require.Positive(t, paths.hitCurrent, s.name)
					continue
				}
				require.Equal(t, uint64(paths.newObjects), objects, "%s %s: allocations against the model", workload, s.name)
				want := rmPaths{hashingLookups: g * n, fingerprints: n, newValues: n, newObjects: 2 * n, copied: paths.copied}
				got := paths
				got.missFree, got.missKept, got.missPrint = 0, 0, 0
				require.Equal(t, want, got, "%s %s", workload, s.name)
				require.Equal(t, n, paths.missFree+paths.missKept+paths.missPrint, "%s %s: every sighting misses", workload, s.name)
				if s.fingerprintsOnly {
					require.Zero(t, paths.missKept, "%s %s", workload, s.name)
				} else if workload == "db" {
					require.Zero(t, paths.missPrint, "%s %s: every db value fits the ledger", workload, s.name)
				}
			}
		}
	})
	t.Run("detected changes", func(t *testing.T) {
		r := newRMReplayer(t, "db", rmStateNamed(t, "G2Lv"))
		i, postSeed, postCond := r.prepare()
		returned := make([]*metadata.Metadata, len(r.values))
		for k, v := range r.values {
			returned[k] = i.internBorrowed(v)
		}
		_, err := rmCheckState(t, i, r.state, r.records, true, postSeed, postCond, nil, returned)
		require.NoError(t, err)
		// A returned value replaced by an equal copy breaks the sharing the
		// ledger records.
		inLedger := map[*metadata.Metadata]bool{}
		for _, slot := range i.ledger {
			if slot.value != nil {
				inLedger[slot.value] = true
			}
		}
		kept := slices.IndexFunc(returned, func(v *metadata.Metadata) bool { return inLedger[v] })
		require.GreaterOrEqual(t, kept, 0)
		saved := returned[kept]
		c := *saved
		returned[kept] = &c
		_, err = rmCheckState(t, i, r.state, r.records, true, postSeed, postCond, nil, returned)
		require.Error(t, err, "a returned copy")
		returned[kept] = saved
		// An extra sighting changes the interner.
		i.internBorrowed(rmBuildValue("extra", 0))
		_, err = rmCheckState(t, i, r.state, r.records, true, postSeed, postCond, nil, returned)
		require.Error(t, err, "an extra sighting")
		// A snapshot that doesn't match fails.
		other := postCond
		other.occupied++
		_, err = rmCheckState(t, i, r.state, r.records, true, postSeed, other, nil, returned)
		require.Error(t, err, "a changed snapshot")
	})
}

// TestMetadataRMContent checks that the comparison reads content, not only
// pointers: a generation's value that differs from its key, a returned value
// that differs from its sighting, and a nil one in either, each fail it, even
// where no other holder shares the object.
func TestMetadataRMContent(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	replayed := func(t *testing.T, name string) (*rmReplayer, *metadataInterner, rmSnapshot, rmSnapshot, []*metadata.Metadata) {
		r := newRMReplayer(t, "db", rmStateNamed(t, name))
		i, postSeed, postCond := r.prepare()
		returned := make([]*metadata.Metadata, len(r.values))
		for k, v := range r.values {
			returned[k] = i.internBorrowed(v)
		}
		_, err := rmCheckState(t, i, r.state, r.records, true, postSeed, postCond, nil, returned)
		require.NoError(t, err)
		return r, i, postSeed, postCond, returned
	}
	wrong := func(m *metadata.Metadata) *metadata.Metadata {
		c := *m
		c.Help += " wrong"
		return &c
	}
	for _, c := range []struct{ name, state, generation string }{{"current", "G1L0", "current"}, {"older", "G2L0", "older"}} {
		t.Run(c.name+" generation", func(t *testing.T) {
			r, i, postSeed, postCond, returned := replayed(t, c.state)
			gen := i.current
			if c.generation == "older" {
				gen = i.older
			}
			var key metadata.Metadata
			for k := range gen {
				key = k
				break
			}
			saved := gen[key]
			for _, v := range []*metadata.Metadata{wrong(saved), nil} {
				gen[key] = v
				_, err := rmCheckState(t, i, r.state, r.records, true, postSeed, postCond, nil, returned)
				gen[key] = saved
				require.Error(t, err, "%s holds %+v for %+v", c.generation, v, key)
			}
		})
	}
	t.Run("returned value", func(t *testing.T) {
		r, i, postSeed, postCond, returned := replayed(t, "G0Lf")
		saved := returned[0]
		for _, v := range []*metadata.Metadata{wrong(saved), nil} {
			returned[0] = v
			_, err := rmCheckState(t, i, r.state, r.records, true, postSeed, postCond, nil, returned)
			returned[0] = saved
			require.Error(t, err, "returned %+v", v)
		}
	})
}

// TestMetadataRMStore checks the store under the states that change most:
// after every record, every series holds its values with none of the poison
// written over the record, and the interner keeps none of it; afterwards the
// interner and the histories' sharing agree with the model, and the counting
// run with the model's counts.
func TestMetadataRMStore(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	for _, workload := range dsWorkloads {
		for _, name := range []string{"GwLv", "G2Lv", "G0Lf"} {
			s := rmStateNamed(t, name)
			if workload == "sb" && s.fingerprintsOnly {
				continue
			}
			t.Run("workload="+workload+"/state="+name, func(t *testing.T) {
				rmCount(t, workload, s)
				r, i, postSeed, postCond := newRMStoreRun(t, workload, s)
				defer r.close()
				poison := []byte{dsPoison}
				for k, compressed := range r.records.compressed {
					rec, err := compression.Decode(compression.Snappy, compressed, r.decBuf)
					require.NoError(t, err)
					r.decoder.decode(t, rec)
					r.decoder.store(r.qm)
					for j := range rec {
						rec[j] = dsPoison
					}
					dsCheckHistories(t, r.qm, workload, dsStepAfter(k), poison)
					require.NoError(t, rmPoisoned(i, poison), "record %d", k)
				}
				rmMustCheckState(t, i, s, r.records, true, postSeed, postCond, r.qm, nil)
				// A history holding an equal copy instead of a value it shares
				// fails the check. An object only one holder holds is
				// indistinguishable from a copy, so pick one the ledger, a
				// generation or another series also holds.
				holders := map[*metadata.Metadata]int{}
				for _, slot := range i.ledger {
					if slot.value != nil {
						holders[slot.value]++
					}
				}
				for _, gen := range []map[metadata.Metadata]*metadata.Metadata{i.current, i.older} {
					for _, v := range gen {
						holders[v]++
					}
				}
				for _, state := range r.qm.seriesNativeMetadata {
					holders[state.Metadata]++
				}
				ref := chunks.HeadSeriesRef(0)
				for slot := range w1Series {
					if holders[r.qm.seriesNativeMetadata[chunks.HeadSeriesRef(slot+1)].Metadata] > 1 {
						ref = chunks.HeadSeriesRef(slot + 1)
						break
					}
				}
				if ref == 0 {
					// A ledger that keeps no values shares nothing in a distinct
					// workload.
					require.True(t, s.fingerprintsOnly && workload != "sb", "no series shares its newest value")
					return
				}
				state := r.qm.seriesNativeMetadata[ref]
				copied := *state.Metadata
				changed := state
				changed.Metadata = &copied
				r.qm.seriesNativeMetadata[ref] = changed
				_, err := rmCheckState(t, i, s, r.records, true, postSeed, postCond, r.qm, nil)
				r.qm.seriesNativeMetadata[ref] = state
				require.Error(t, err, "series %d's copy", ref)
			})
		}
	}
}

// rmPoisoned returns an error if any value i keeps contains poison.
func rmPoisoned(i *metadataInterner, poison []byte) error {
	check := func(m metadata.Metadata) error {
		if strings.Contains(m.Help, string(poison)) || strings.Contains(m.Unit, string(poison)) || strings.Contains(string(m.Type), string(poison)) {
			return errors.New("the interner retains a stored record's memory")
		}
		return nil
	}
	for _, gen := range []map[metadata.Metadata]*metadata.Metadata{i.current, i.older} {
		for k, v := range gen {
			if err := errors.Join(check(k), check(*v)); err != nil {
				return err
			}
		}
	}
	for _, slot := range i.ledger {
		if slot.value != nil {
			if err := check(*slot.value); err != nil {
				return err
			}
		}
	}
	return nil
}

// TestMetadataRMOwnObjects checks that rmOwnObjects agrees with the objects
// ownMetadata allocates.
func TestMetadataRMOwnObjects(t *testing.T) {
	for _, m := range []metadata.Metadata{
		{},
		{Type: model.MetricTypeCounter},
		{Type: model.MetricTypeCounter, Help: "h"},
		{Type: model.MetricTypeGauge, Unit: "u", Help: "h"},
		{Type: "custom"},
		rmBuildValue("state", 1),
		w1Metadata("dv", 3, 2),
	} {
		got := testing.AllocsPerRun(10, func() { rmSink = ownMetadata(m) })
		require.Equal(t, float64(rmOwnObjects(m)), got, "%+v", m)
	}
	rmSink = nil
}

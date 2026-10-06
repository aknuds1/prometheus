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
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
)

// The distinct-store diagnostic's parts for builds with the WAL metadata
// interner and native histories. This file is identical on every such build.

const (
	// A warm interner holds dsWarmAdmitted admitted values, and its ledger
	// has seen dsWarmSighted more, once each.
	dsWarmAdmitted = 16384
	dsWarmSighted  = 8192
)

// dsSink keeps the replayed fingerprints live.
var dsSink uint64

// dsSighting is one value the store resolves, with the points it serves.
type dsSighting struct {
	value  metadata.Metadata
	points []dsPointRef
}

type dsPointRef struct {
	ref  chunks.HeadSeriesRef
	from int64
}

func dsHistoryDepth() int { return 5 }

func dsLegacy() bool { return false }

func dsVersions(qm *QueueManager, ref chunks.HeadSeriesRef, dst []dsVersion) ([]dsVersion, bool) {
	state := qm.seriesNativeMetadata[ref]
	for _, p := range state.Older {
		dst = append(dst, dsVersion{from: p.EffectiveFrom, m: p.Metadata})
	}
	if state.Metadata != nil {
		dst = append(dst, dsVersion{from: state.EffectiveFrom, m: state.Metadata})
	}
	return dst, state.Truncated
}

func dsOlderCap(qm *QueueManager, ref chunks.HeadSeriesRef) int {
	return cap(qm.seriesNativeMetadata[ref].Older)
}

func dsProcessIntern() func(metadata.Metadata) *metadata.Metadata {
	return walMetadataInterner.internBorrowed
}

// dsNewInterner returns a fresh interner's borrowing entry point and the
// interner. With shared set, it fingerprints with shared's function.
func dsNewInterner(shared any) (func(metadata.Metadata) *metadata.Metadata, any) {
	i := newMetadataInterner(metadataInternerEntries, metadataInternerBytes)
	if shared != nil {
		i.fingerprint = shared.(*metadataInterner).fingerprint
	}
	return i.internBorrowed, i
}

func dsWarmValue(n int) metadata.Metadata {
	return metadata.Metadata{Type: model.MetricTypeGauge, Help: fmt.Sprintf("warm %05d", n)}
}

// dsWarm admits dsWarmAdmitted unrelated values, each sighted twice in a row,
// then sights dsWarmSighted more once each.
func dsWarm(i *metadataInterner) {
	for n := range dsWarmAdmitted {
		v := dsWarmValue(n)
		i.intern(v)
		i.intern(v)
	}
	for n := range dsWarmSighted {
		i.intern(dsWarmValue(dsWarmAdmitted + n))
	}
}

func dsWarmInterner(handle any) { dsWarm(handle.(*metadataInterner)) }

func dsWarmProcessInterner() { dsWarm(walMetadataInterner) }

// dsOwnedBytes returns the bytes an owned copy of m copies: its non-empty
// unit and help, and its type unless it is a known type's shared constant.
func dsOwnedBytes(m metadata.Metadata) int {
	n := len(m.Unit) + len(m.Help)
	if record.ToMetricType(record.GetMetricType(m.Type)) != m.Type {
		n += len(m.Type)
	}
	return n
}

// dsModel replays an interner's sightings independently, with the interner's
// own fingerprint function, and predicts its generations, its ledger, and
// which sightings return the same object. Objects are numbered as created.
type dsModel struct {
	fingerprint                 func(metadata.Metadata) uint64
	entries, limit, ledgerLimit int
	current, older              map[metadata.Metadata]int
	bytes                       int
	ledger                      []dsModelSlot
	next                        []uint8
	ledgerBytes                 int
	objects                     int
	sightings, newValues        int
	copied                      int
}

type dsModelSlot struct {
	fingerprint uint64
	object      int
	kept        bool
	value       metadata.Metadata
}

func newDSModel(i *metadataInterner) *dsModel {
	return &dsModel{
		fingerprint: i.fingerprint, entries: i.entries, limit: i.limit, ledgerLimit: i.ledgerLimit,
		current: map[metadata.Metadata]int{}, ledger: make([]dsModelSlot, len(i.ledger)), next: make([]uint8, len(i.next)),
	}
}

func (m *dsModel) newObject(v metadata.Metadata) int {
	m.objects++
	m.newValues++
	m.copied += dsOwnedBytes(v)
	return m.objects
}

func (m *dsModel) intern(v metadata.Metadata) int {
	m.sightings++
	cost := metadataCost(v)
	if cost > m.limit {
		return m.newObject(v)
	}
	if id, ok := m.current[v]; ok {
		return id
	}
	id, ok := m.older[v]
	if !ok {
		var admit bool
		if id, admit = m.sight(v, cost); !admit {
			return id
		}
	}
	if len(m.current) >= m.entries || m.bytes+cost > m.limit {
		m.older, m.current, m.bytes = m.current, map[metadata.Metadata]int{}, 0
	}
	m.current[v] = id
	m.bytes += cost
	return id
}

func (m *dsModel) sight(v metadata.Metadata, cost int) (int, bool) {
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
			id, kept := slot.object, slot.kept
			m.release(slot)
			if !kept {
				id = m.newObject(v)
			}
			return id, true
		}
		if slot.fingerprint == 0 && free < 0 {
			free = w
		}
	}
	if free < 0 {
		free = int(m.next[set])
		m.next[set] = uint8((free + 1) % metadataLedgerWays)
	}
	slot := &ways[free]
	id := m.newObject(v)
	m.release(slot)
	slot.fingerprint = fingerprint
	if cost <= m.ledgerLimit-m.ledgerBytes {
		slot.object, slot.kept, slot.value = id, true, v
		m.ledgerBytes += cost
	}
	return id, false
}

func (m *dsModel) release(slot *dsModelSlot) {
	if slot.kept {
		m.ledgerBytes -= metadataCost(slot.value)
	}
	*slot = dsModelSlot{}
}

// dsSightingsOf decodes records and returns the values the store resolves,
// in its order, with the points each serves.
func dsSightingsOf(tb testing.TB, records [][]byte) []dsSighting {
	var out []dsSighting
	for _, rec := range records {
		d := newW1Decoder()
		d.decode(tb, rec)
		out = append(out, dsSightings(d)...)
	}
	return out
}

// dsModelMismatch replays r's sightings through a model of r's interner and
// returns the model's counts for the change records, and the first difference
// from the interner and the histories: the generations, the ledger, and which
// objects the histories, the ledger and the generations share.
func dsModelMismatch(tb testing.TB, r *dsRun) (dsCounts, error) {
	i := walMetadataInterner
	m := newDSModel(i)
	objects := map[dsPointRef]int{}
	replay := func(sightings []dsSighting) {
		for _, s := range sightings {
			id := m.intern(s.value)
			for _, p := range s.points {
				objects[p] = id
			}
		}
	}
	replay(dsSightingsOf(tb, r.records.seed))
	if r.warm {
		for n := range dsWarmAdmitted {
			m.intern(dsWarmValue(n))
			m.intern(dsWarmValue(n))
		}
		for n := range dsWarmSighted {
			m.intern(dsWarmValue(dsWarmAdmitted + n))
		}
	}
	m.sightings, m.newValues, m.copied = 0, 0, 0
	replay(dsSightingsOf(tb, r.records.changes))
	counts := dsCounts{resolutions: float64(m.sightings), newValues: float64(m.newValues), copied: float64(m.copied)}

	if len(i.current) != len(m.current) || len(i.older) != len(m.older) || i.bytes != m.bytes {
		return counts, fmt.Errorf("generations hold %d and %d values in %d bytes, the model %d and %d in %d", len(i.current), len(i.older), i.bytes, len(m.current), len(m.older), m.bytes)
	}
	if i.ledgerBytes != m.ledgerBytes {
		return counts, fmt.Errorf("the ledger holds %d bytes, the model %d", i.ledgerBytes, m.ledgerBytes)
	}
	for set := range i.next {
		if i.next[set] != m.next[set] {
			return counts, fmt.Errorf("ledger set %d replaces slot %d next, the model %d", set, i.next[set], m.next[set])
		}
	}
	// Objects must correspond one to one with the model's.
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
			return counts, fmt.Errorf("ledger slot %d differs from the model", idx)
		}
		if slot.value != nil {
			if *slot.value != ms.value {
				return counts, fmt.Errorf("ledger slot %d holds %+v, the model %+v", idx, *slot.value, ms.value)
			}
			if err := match(slot.value, ms.object, fmt.Sprintf("ledger slot %d", idx)); err != nil {
				return counts, err
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
				return counts, fmt.Errorf("a generation lacks %+v", k)
			}
			if err := match(v, id, "a generation"); err != nil {
				return counts, err
			}
		}
	}
	var versions []dsVersion
	for slot := range w1Series {
		ref := chunks.HeadSeriesRef(slot + 1)
		versions, _ = dsVersions(r.qm, ref, versions[:0])
		for _, v := range versions {
			id, ok := objects[dsPointRef{ref: ref, from: v.from}]
			if !ok {
				return counts, fmt.Errorf("series %d's version at %d was never sighted", ref, v.from)
			}
			if err := match(v.m, id, fmt.Sprintf("series %d", ref)); err != nil {
				return counts, err
			}
		}
	}
	return counts, nil
}

// dsCheckModel fails unless r's interner and histories agree with the model,
// and returns the model's counts.
func dsCheckModel(tb testing.TB, r *dsRun) dsCounts {
	counts, err := dsModelMismatch(tb, r)
	if err != nil {
		tb.Fatalf("model: %v", err)
	}
	return counts
}

// dsCheckModelDetects checks that the model reports each kind of change to
// r's interner, restoring each afterwards.
func dsCheckModelDetects(t *testing.T, r *dsRun) {
	i := walMetadataInterner
	expectMismatch := func(name string, change, undo func()) {
		change()
		_, err := dsModelMismatch(t, r)
		undo()
		require.Error(t, err, name)
		_, err = dsModelMismatch(t, r)
		require.NoError(t, err, name+" restored")
	}
	held := map[*metadata.Metadata]bool{}
	var versions []dsVersion
	for slot := range w1Series {
		versions, _ = dsVersions(r.qm, chunks.HeadSeriesRef(slot+1), versions[:0])
		for _, v := range versions {
			held[v.m] = true
		}
	}
	occupied := -1
	for idx := range i.ledger {
		if i.ledger[idx].fingerprint != 0 {
			occupied = idx
			break
		}
	}
	require.GreaterOrEqual(t, occupied, 0, "a ledger slot is occupied")
	saved := i.ledger[occupied]
	expectMismatch("a changed ledger fingerprint", func() { i.ledger[occupied].fingerprint++ }, func() { i.ledger[occupied] = saved })
	// An equal copy of an object only one holder holds is indistinguishable,
	// so replace an object a history also holds.
	replaced := 0
	for idx := range i.ledger {
		if v := i.ledger[idx].value; v != nil && held[v] {
			slot := i.ledger[idx]
			expectMismatch("a replaced shared ledger object", func() { c := *slot.value; i.ledger[idx].value = &c }, func() { i.ledger[idx] = slot })
			replaced++
			break
		}
	}
	for k, v := range i.current {
		if held[v] {
			expectMismatch("a replaced shared admitted object", func() { c := *v; i.current[k] = &c }, func() { i.current[k] = v })
			replaced++
			break
		}
	}
	require.Positive(t, replaced, "an object the interner and a history share")
	next := i.next[0]
	expectMismatch("a changed replacement order", func() { i.next[0] = (next + 1) % metadataLedgerWays }, func() { i.next[0] = next })
	extra := metadata.Metadata{Help: "extra"}
	expectMismatch("an extra admitted value", func() { i.current[extra] = &extra; i.bytes += metadataCost(extra) },
		func() { delete(i.current, extra); i.bytes -= metadataCost(extra) })
	// Two series that hold distinct objects come to share one.
	a, b := r.qm.seriesNativeMetadata[1], r.qm.seriesNativeMetadata[2]
	expectMismatch("a merged object", func() { s := b; s.Metadata = a.Metadata; r.qm.seriesNativeMetadata[2] = s },
		func() { r.qm.seriesNativeMetadata[2] = b })
}

// dsInternerMismatch returns how two interners that share a fingerprint
// function differ, or nil.
func dsInternerMismatch(a, b any) error {
	x, y := a.(*metadataInterner), b.(*metadataInterner)
	if len(x.current) != len(y.current) || len(x.older) != len(y.older) || x.bytes != y.bytes {
		return fmt.Errorf("generations hold %d and %d values in %d bytes, against %d and %d in %d", len(x.current), len(x.older), x.bytes, len(y.current), len(y.older), y.bytes)
	}
	if x.ledgerBytes != y.ledgerBytes {
		return fmt.Errorf("ledgers hold %d and %d bytes", x.ledgerBytes, y.ledgerBytes)
	}
	for set := range x.next {
		if x.next[set] != y.next[set] {
			return fmt.Errorf("ledger set %d replaces slots %d and %d next", set, x.next[set], y.next[set])
		}
	}
	for k := range x.current {
		if _, ok := y.current[k]; !ok {
			return fmt.Errorf("only one interner admitted %+v", k)
		}
	}
	for k := range x.older {
		if _, ok := y.older[k]; !ok {
			return fmt.Errorf("only one interner's older generation holds %+v", k)
		}
	}
	for idx := range x.ledger {
		s, t := x.ledger[idx], y.ledger[idx]
		if s.fingerprint != t.fingerprint || (s.value != nil) != (t.value != nil) || (s.value != nil && *s.value != *t.value) {
			return fmt.Errorf("ledger slot %d differs", idx)
		}
	}
	return nil
}

// dsCompareInterners fails unless two interners that share a fingerprint
// function hold the same state, with every string free of poison.
func dsCompareInterners(tb testing.TB, a, b any, poison []byte) {
	require.NoError(tb, dsInternerMismatch(a, b))
	clean := func(m metadata.Metadata) {
		if strings.Contains(m.Help, string(poison)) || strings.Contains(m.Unit, string(poison)) || strings.Contains(string(m.Type), string(poison)) {
			tb.Fatalf("the interner retains a stored record's memory: %+v", m)
		}
	}
	for _, i := range []*metadataInterner{a.(*metadataInterner), b.(*metadataInterner)} {
		for k, v := range i.current {
			clean(k)
			clean(*v)
		}
		for k, v := range i.older {
			clean(k)
			clean(*v)
		}
		for _, slot := range i.ledger {
			if slot.value != nil {
				clean(*slot.value)
			}
		}
	}
}

// dsProbes observes, by weak pointers, the queue manager and interner structs,
// the ledger's backing array, and the history backing array and newest value
// of each sampled series. No probe observes a map's storage, or any
// allocation not sampled.
func dsProbes(qm *QueueManager) []dsProbe {
	i := walMetadataInterner
	probes := []dsProbe{dsWeakProbe("queue manager", qm), dsWeakProbe("interner", i), dsWeakProbe("ledger", &i.ledger[0])}
	for _, ref := range dsProbeRefs() {
		state := qm.seriesNativeMetadata[ref]
		probes = append(probes, dsWeakProbe(fmt.Sprintf("history %d", ref), &state.Older[0]), dsWeakProbe(fmt.Sprintf("value %d", ref), state.Metadata))
	}
	return probes
}

// dsHeldValues returns the sampled values' probe names that i's ledger or
// generations hold.
func dsHeldValues(qm *QueueManager, i *metadataInterner, ledgerOnly bool) []string {
	held := map[*metadata.Metadata]bool{}
	for _, slot := range i.ledger {
		if slot.value != nil {
			held[slot.value] = true
		}
	}
	if !ledgerOnly {
		for _, gen := range []map[metadata.Metadata]*metadata.Metadata{i.current, i.older} {
			for _, v := range gen {
				held[v] = true
			}
		}
	}
	var names []string
	for _, ref := range dsProbeRefs() {
		if held[qm.seriesNativeMetadata[ref].Metadata] {
			names = append(names, fmt.Sprintf("value %d", ref))
		}
	}
	return names
}

func dsAllSeriesProbes() []string {
	var names []string
	for _, ref := range dsProbeRefs() {
		names = append(names, fmt.Sprintf("history %d", ref), fmt.Sprintf("value %d", ref))
	}
	return names
}

func dsRetentionCases() []dsRetentionCase {
	return []dsRetentionCase{
		{"nothing retained", func(*dsRun) []string { return nil }},
		{"the queue manager", func(r *dsRun) []string {
			dsRetained = r.qm
			return append([]string{"queue manager"}, dsAllSeriesProbes()...)
		}},
		{"the interner", func(r *dsRun) []string {
			i := walMetadataInterner
			dsRetained = i
			return append([]string{"interner", "ledger"}, dsHeldValues(r.qm, i, false)...)
		}},
		{"the ledger's backing array", func(r *dsRun) []string {
			i := walMetadataInterner
			dsRetained = i.ledger
			return append([]string{"ledger"}, dsHeldValues(r.qm, i, true)...)
		}},
		{"one history's backing array", func(r *dsRun) []string {
			ref := dsProbeRefs()[0]
			dsRetained = r.qm.seriesNativeMetadata[ref].Older
			return []string{fmt.Sprintf("history %d", ref)}
		}},
		{"the series map", func(r *dsRun) []string {
			dsRetained = r.qm.seriesNativeMetadata
			return dsAllSeriesProbes()
		}},
	}
}

// dsCounter counts what the store's interner calls do: the calls, the new
// owned values they return, and the bytes those copy. Objects that already
// exist before the change records count as seen.
type dsCounter struct {
	calls, newValues, copied int
	seen                     map[*metadata.Metadata]bool
}

func dsNewCounter(r *dsRun) *dsCounter {
	c := &dsCounter{seen: map[*metadata.Metadata]bool{}}
	var versions []dsVersion
	for slot := range w1Series {
		versions, _ = dsVersions(r.qm, chunks.HeadSeriesRef(slot+1), versions[:0])
		for _, v := range versions {
			c.seen[v.m] = true
		}
	}
	i := walMetadataInterner
	for _, slot := range i.ledger {
		if slot.value != nil {
			c.seen[slot.value] = true
		}
	}
	for _, gen := range []map[metadata.Metadata]*metadata.Metadata{i.current, i.older} {
		for _, v := range gen {
			c.seen[v] = true
		}
	}
	intern := i.internBorrowed
	w1InternHook = func(m metadata.Metadata) *metadata.Metadata {
		c.calls++
		v := intern(m)
		if !c.seen[v] {
			c.seen[v] = true
			c.newValues++
			c.copied += dsOwnedBytes(*v)
		}
		return v
	}
	return c
}

func (*dsCounter) store(d *w1Decoder, qm *QueueManager) { d.store(qm) }

func (c *dsCounter) counts() (resolutions, newValues, copied float64) {
	return float64(c.calls), float64(c.newValues), float64(c.copied)
}

// dsReplay times, per value the store resolves for the change records, in
// its order and with GC off: a fresh interner's fingerprint; an owned copy;
// a fresh interner's borrowing entry point; and a warm one's. Each interner
// has first resolved the seed's values, as the store's has.
func dsReplay(b *testing.B, timer *dsTimer, workload string) {
	records := w1RecordsFor(b, workload)
	var seed, changes []metadata.Metadata
	for _, s := range dsSightingsOf(b, records.seed) {
		seed = append(seed, s.value)
	}
	for _, s := range dsSightingsOf(b, records.changes) {
		changes = append(changes, s.value)
	}
	var fingerprint, own, fresh, warm time.Duration
	var sink uint64
	var kept *metadata.Metadata
	timer.start()
	for range b.N {
		freshI := newMetadataInterner(metadataInternerEntries, metadataInternerBytes)
		warmI := newMetadataInterner(metadataInternerEntries, metadataInternerBytes)
		for _, v := range seed {
			freshI.internBorrowed(v)
			warmI.internBorrowed(v)
		}
		dsWarm(warmI)
		runtime.GC()
		timer.gcOff(b)
		timer.time(func() {
			t0 := w1CPU()
			for _, v := range changes {
				sink ^= freshI.fingerprint(v)
			}
			t1 := w1CPU()
			for _, v := range changes {
				kept = ownMetadata(v)
			}
			t2 := w1CPU()
			for _, v := range changes {
				freshI.internBorrowed(v)
			}
			t3 := w1CPU()
			for _, v := range changes {
				warmI.internBorrowed(v)
			}
			t4 := w1CPU()
			fingerprint, own, fresh, warm = fingerprint+t1-t0, own+t2-t1, fresh+t3-t2, warm+t4-t3
		})
		timer.gcOn(b)
	}
	runtime.KeepAlive(kept)
	dsSink = sink
	n := float64(b.N) * float64(len(changes))
	timer.report("value", len(changes), true)
	b.ReportMetric(float64(fingerprint.Nanoseconds())/n, "fingerprint-ns/value")
	b.ReportMetric(float64(own.Nanoseconds())/n, "own-ns/value")
	b.ReportMetric(float64(fresh.Nanoseconds())/n, "intern-fresh-ns/value")
	b.ReportMetric(float64(warm.Nanoseconds())/n, "intern-warm-ns/value")
}

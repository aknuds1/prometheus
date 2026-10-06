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

package tsdb

import (
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	prom_testutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/encoding"
	"github.com/prometheus/prometheus/tsdb/nativemetadata"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/tsdb/wlog"
	"github.com/prometheus/prometheus/util/compression"
)

// nativeMetadataWAL is the content of a WAL directory in WAL order: the last
// checkpoint, if any, then the segments after it.
type nativeMetadataWAL struct {
	types   []record.Type
	entries []record.RefNativeMetadata
	// values holds the dictionary size of each compact record.
	values []int
}

func readNativeMetadataWAL(t testing.TB, dir string) nativeMetadataWAL {
	t.Helper()
	var ranges []wlog.SegmentRange
	first := 0
	checkpoint, index, err := wlog.LastCheckpoint(dir)
	if err == nil {
		ranges = append(ranges, wlog.SegmentRange{Dir: checkpoint, Last: math.MaxInt32})
		first = index + 1
	} else {
		require.ErrorIs(t, err, record.ErrNotFound)
	}
	ranges = append(ranges, wlog.SegmentRange{Dir: dir, First: first, Last: math.MaxInt32})
	sr, err := wlog.NewSegmentsRangeReader(ranges...)
	require.NoError(t, err)
	defer sr.Close()
	var out nativeMetadataWAL
	var dec record.Decoder
	r := wlog.NewReader(sr)
	var compact record.CompactNativeMetadata
	for r.Next() {
		typ := dec.Type(r.Record())
		out.types = append(out.types, typ)
		switch typ {
		case record.Metadata:
			entries, _, err := dec.NativeMetadata(r.Record(), nil, nil)
			require.NoError(t, err)
			out.entries = append(out.entries, entries...)
		case record.NativeMetadataCompact:
			require.NoError(t, dec.CompactNativeMetadata(r.Record(), &compact))
			out.entries, _ = compact.AppendNativeMetadata(out.entries, nil)
			out.values = append(out.values, len(compact.Values))
		}
	}
	require.NoError(t, r.Err())
	return out
}

// reduce applies the entries in WAL order, as forwarding and replay do.
func (w nativeMetadataWAL) reduce() map[chunks.HeadSeriesRef]nativemetadata.State {
	states := map[chunks.HeadSeriesRef]nativemetadata.State{}
	intern := func(m metadata.Metadata) *metadata.Metadata { return &m }
	for _, e := range w.entries {
		if e.Ignored() {
			continue
		}
		state := states[e.Ref]
		state.Apply(e.Kind, e.Truncated, nativemetadata.AppendRecordPoints(nil, e.Points, intern))
		states[e.Ref] = state
	}
	return states
}

// compactNativeMetadataForTest returns entries, which must be groups or
// overrides, as a compact record whose dictionary holds each distinct value once.
func compactNativeMetadataForTest(entries ...record.RefNativeMetadata) record.CompactNativeMetadata {
	var rec record.CompactNativeMetadata
	indices := map[record.NativeMetadataValue]uint32{}
	for _, e := range entries {
		compact := record.RefCompactNativeMetadata{Ref: e.Ref, Kind: e.Kind, Truncated: e.Truncated}
		for _, p := range e.Points {
			v := record.NativeMetadataValue{Type: p.Type, Unit: p.Unit, Help: p.Help}
			index, ok := indices[v]
			if !ok {
				index = uint32(len(rec.Values))
				indices[v] = index
				rec.Values = append(rec.Values, v)
			}
			compact.Points = append(compact.Points, record.CompactNativeMetadataPoint{EffectiveFrom: p.EffectiveFrom, Value: index})
		}
		rec.Entries = append(rec.Entries, compact)
	}
	return rec
}

// unknownCompactNativeMetadataForTest encodes a compact record of one entry of
// an unknown kind, with value v at from if it has a point.
func unknownCompactNativeMetadataForTest(ref chunks.HeadSeriesRef, v *record.NativeMetadataValue, from int64) []byte {
	const unknownKind, inlineFormat = 7, 3
	buf := encoding.Encbuf{}
	buf.PutByte(byte(record.NativeMetadataCompact))
	buf.PutByte(inlineFormat)
	if v == nil {
		buf.PutVarint64(0)
	} else {
		buf.PutVarint64(from)
	}
	buf.PutUvarint(1)
	buf.PutByte(unknownKind)
	buf.PutVarint64(int64(ref))
	if v == nil {
		buf.PutUvarint(0)
		return buf.Get()
	}
	// One point, at the base, defining v.
	buf.PutUvarint(1)
	buf.PutUvarint(0)
	buf.PutUvarint(0)
	buf.PutByte(v.Type)
	buf.PutUvarintStr(v.Unit)
	buf.PutUvarintStr(v.Help)
	return buf.Get()
}

func nativeMetadataVersions(s nativemetadata.State) []NativeMetricMetadataVersion {
	var versions []NativeMetricMetadataVersion
	for _, p := range s.AppendPoints(nil) {
		versions = append(versions, NativeMetricMetadataVersion{EffectiveFrom: p.EffectiveFrom, Metadata: *p.Metadata})
	}
	return versions
}

func nativeMetadataSnapshot(t testing.TB, db *DB) map[string]NativeMetricMetadataSeries {
	t.Helper()
	series, _, err := db.NativeMetricMetadata(t.Context(), [][]*labels.Matcher{{labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+")}}, 0)
	require.NoError(t, err)
	out := make(map[string]NativeMetricMetadataSeries, len(series))
	for _, s := range series {
		out[s.Labels.String()] = s
	}
	return out
}

func TestNativeMetricMetadataWAL(t *testing.T) {
	m := func(help string) metadata.Metadata {
		return metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: help}
	}
	point := func(from int64, help string) record.RefNativeMetadataPoint {
		return record.RefNativeMetadataPoint{EffectiveFrom: from, Type: uint8(record.Counter), Unit: "seconds", Help: help}
	}
	newDB := func(t *testing.T) *DB {
		opts := DefaultOptions()
		opts.EnableNativeMetadata = true
		opts.OutOfOrderTimeWindow = math.MaxInt32
		return newTestDB(t, withOpts(opts))
	}
	a, b, c := labels.FromStrings(labels.MetricName, "a"), labels.FromStrings(labels.MetricName, "b"), labels.FromStrings(labels.MetricName, "c")

	t.Run("commits log unstable groups before samples", func(t *testing.T) {
		db := newDB(t)
		app := db.AppenderV2(t.Context())
		refA, err := app.Append(0, a, 0, 100, 1, nil, nil, storage.AOptions{Metadata: m("a")})
		require.NoError(t, err)
		require.NoError(t, app.Commit())
		app = db.AppenderV2(t.Context())
		_, err = app.Append(refA, a, 0, 200, 2, nil, nil, storage.AOptions{Metadata: m("a")})
		require.NoError(t, err)
		refB, err := app.Append(0, b, 0, 200, 1, nil, nil, storage.AOptions{Metadata: m("b")})
		require.NoError(t, err)
		require.NoError(t, app.Commit())
		// One batched transaction: a stable sample, then a change. Every later
		// observation of the series is a point; none is redundant in general.
		app = db.AppenderV2(t.Context())
		for i, ts := range []int64{300, 400, 500, 600} {
			help := "c"
			if i == 0 {
				help = "a"
			}
			_, err = app.Append(refA, a, 0, ts, 3, nil, nil, storage.AOptions{Metadata: m(help)})
			require.NoError(t, err)
		}
		require.NoError(t, app.Commit())

		wal := readNativeMetadataWAL(t, db.Head().wal.Dir())
		require.Equal(t, []record.Type{
			record.Series, record.NativeMetadataCompact, record.Samples,
			record.Series, record.NativeMetadataCompact, record.Samples,
			record.NativeMetadataCompact, record.Samples,
		}, wal.types)
		// Points of one value share its dictionary entry.
		require.Equal(t, []int{1, 1, 1}, wal.values)
		require.Equal(t, []record.RefNativeMetadata{
			{Ref: chunks.HeadSeriesRef(refA), Kind: record.NativeMetadataGroup, Points: []record.RefNativeMetadataPoint{point(100, "a")}},
			{Ref: chunks.HeadSeriesRef(refB), Kind: record.NativeMetadataGroup, Points: []record.RefNativeMetadataPoint{point(200, "b")}},
			{Ref: chunks.HeadSeriesRef(refA), Kind: record.NativeMetadataGroup, Points: []record.RefNativeMetadataPoint{point(400, "c"), point(500, "c"), point(600, "c")}},
		}, wal.entries)
	})

	t.Run("rollbacks log no native metadata", func(t *testing.T) {
		db := newDB(t)
		app := db.AppenderV2(t.Context())
		_, err := app.Append(0, c, 0, 100, 1, nil, nil, storage.AOptions{Metadata: m("c")})
		require.NoError(t, err)
		require.NoError(t, app.Rollback())
		wal := readNativeMetadataWAL(t, db.Head().wal.Dir())
		require.Equal(t, []record.Type{record.Series}, wal.types)
	})

	t.Run("out-of-order backfill payload", func(t *testing.T) {
		// Older samples are always observed, so each transaction logs one
		// group per series with a point per sample, deduplicated per timestamp.
		for _, batched := range []bool{false, true} {
			w := metadataBackfill{series: 300, rounds: 3, batched: batched}
			db := w.open(t, "native", nil)
			w.run(t, db, 0, w.rounds)
			require.NoError(t, db.Close())
			// Each transaction's record holds its series' groups in ref order.
			// Its dictionary holds each value once, in order of first use:
			// past the transaction's 128 deduplicated values, a value repeats
			// only in consecutive observations, which share a reference.
			var enc record.Encoder
			want := int64(0)
			for round := 0; round <= w.rounds; round++ {
				for offset := 0; offset < w.series; offset += 100 {
					var entries []record.RefNativeMetadata
					for id := offset; id < min(offset+100, w.series); id++ {
						entry := record.RefNativeMetadata{Ref: chunks.HeadSeriesRef(id + 1), Kind: record.NativeMetadataGroup}
						w.samples(round, func(ts int64, version int) {
							m := w.metadata(id, version)
							entry.Points = append(entry.Points, record.RefNativeMetadataPoint{EffectiveFrom: ts, Type: record.GetMetricType(m.Type), Unit: m.Unit, Help: m.Help})
						})
						entries = append(entries, entry)
					}
					rec := compactNativeMetadataForTest(entries...)
					want += int64(len(enc.CompactNativeMetadata(rec.Values, rec.Entries, nil)))
				}
			}
			require.Equal(t, want, metadataPayload(t, filepath.Join(db.Dir(), "wal")), "batched=%t", batched)
		}
	})

	t.Run("WAL order reproduces native histories", func(t *testing.T) {
		// A single writer per series never reorders merge groups, so reducing
		// the WAL must reproduce native histories exactly, including
		// out-of-order points, eviction and the truncation flag.
		rng := rand.New(rand.NewPCG(3, 4))
		for iteration := range 40 {
			db := newDB(t)
			lsets := []labels.Labels{a, b, c}
			refs := make([]storage.SeriesRef, len(lsets))
			used := make([]map[int64]bool, len(lsets))
			for i := range used {
				used[i] = map[int64]bool{}
			}
			for range 30 {
				app := db.AppenderV2(t.Context())
				for i, lset := range lsets {
					for range rng.IntN(4) {
						ts := int64(rng.IntN(2000))
						if used[i][ts] {
							continue
						}
						used[i][ts] = true
						ref, err := app.Append(refs[i], lset, 0, ts, float64(ts), nil, nil, storage.AOptions{Metadata: m(string(rune('a' + rng.IntN(4))))})
						require.NoError(t, err)
						refs[i] = ref
					}
				}
				require.NoError(t, app.Commit())
			}
			states := readNativeMetadataWAL(t, db.Head().wal.Dir()).reduce()
			snapshot := nativeMetadataSnapshot(t, db)
			require.Len(t, snapshot, len(states), "iteration %d", iteration)
			for i, lset := range lsets {
				state, ok := states[chunks.HeadSeriesRef(refs[i])]
				if !ok {
					continue
				}
				native := snapshot[lset.String()]
				require.Equal(t, native.Versions, nativeMetadataVersions(state), "iteration %d, series %s", iteration, lset)
				require.Equal(t, native.Truncated, state.Truncated, "iteration %d, series %s", iteration, lset)
			}
			// Every open DB maps its chunk segments; 40 of them exhaust a
			// 32-bit address space.
			require.NoError(t, db.Close())
		}
	})

	t.Run("compact records hold the reference merge groups", func(t *testing.T) {
		// The record's entries are the string-valued groups the appender's
		// observations reduce to, and its dictionary holds each value
		// reference the points use once, with the caller's strings. The
		// transaction table deduplicates 128 values by content; others are
		// shared only by consecutive observations.
		rng := rand.New(rand.NewPCG(5, 6))
		store := newNativeMetricMetadataStore()
		values := make([]metadata.Metadata, 300)
		for i := range values {
			typ := model.MetricTypeCounter
			if i%7 == 0 {
				typ = model.MetricTypeGauge
			}
			values[i] = metadata.Metadata{Type: typ, Unit: strconv.Itoa(i % 3), Help: strings.Repeat("h", i%5) + strconv.Itoa(i)}
		}
		overflow := 0
		for iteration := range 400 {
			a := store.getAppender()
			distinct := 1 + rng.IntN(3)*rng.IntN(150)
			var last metadata.Metadata
			callerStrings := map[unsafe.Pointer]bool{}
			for range rng.IntN(600) {
				ref := chunks.HeadSeriesRef(1 + rng.IntN(40))
				m := values[rng.IntN(distinct)]
				if rng.IntN(4) == 0 {
					m = last // Consecutive repetition.
				}
				if state := nativeMetadataForTest(store.seriesForTest(ref)); state != nil && rng.IntN(3) == 0 {
					m = *state.Metadata // Possibly stable.
				}
				// Fresh copies, so that dictionary strings can be traced to
				// their observations.
				m = metadata.Metadata{Type: m.Type, Unit: strings.Clone(m.Unit), Help: strings.Clone(m.Help)}
				callerStrings[unsafe.Pointer(unsafe.StringData(m.Help))] = true
				a.observe(store.seriesForTest(ref), int64(rng.IntN(60)), m)
				last = m
			}
			want := nativeMetadataWALEntriesForTest(a)
			recValues, entries := a.appendWALRecord()
			got, _ := (&record.CompactNativeMetadata{Values: recValues, Entries: entries}).AppendNativeMetadata(nil, nil)
			require.Equal(t, normalizeNativeMetadataForTest(want), normalizeNativeMetadataForTest(got), "iteration %d", iteration)
			used := make([]bool, len(recValues))
			for _, e := range entries {
				for _, p := range e.Points {
					used[p.Value] = true
				}
			}
			require.NotContains(t, used, false, "iteration %d: every value is used", iteration)
			// Values beyond the transaction table appear once per run of
			// consecutive observations, as owned copies; within it, once, with
			// the caller's strings.
			if len(a.directValues) == 0 {
				seen := map[record.NativeMetadataValue]bool{}
				for _, v := range recValues {
					require.False(t, seen[v], "iteration %d: duplicate value", iteration)
					seen[v] = true
					if v.Help != "" {
						require.True(t, callerStrings[unsafe.Pointer(unsafe.StringData(v.Help))], "iteration %d: values keep the caller's strings", iteration)
					}
				}
			} else {
				overflow++
			}
			// A second pass returns the same record.
			again, _ := a.appendWALRecord()
			require.Equal(t, recValues, again)
			store.commitAppender(a)
			store.putAppender(a)
		}
		require.Positive(t, overflow, "transactions exceed the transaction table")
	})
}

// nativeMetadataWALEntriesForTest is the string-valued construction of the
// appender's merge groups that compact records replace, for comparisons.
func nativeMetadataWALEntriesForTest(a *nativeMetricMetadataAppender) []record.RefNativeMetadata {
	var entries []record.RefNativeMetadata
	for _, stripe := range a.touched {
		first := a.stripeFirst[stripe]
		if nativeMetricMetadataStripeStable(a, first) {
			continue
		}
		a.sortStripe(first)
		for position := 0; position < len(a.sorted); {
			end := position + 1
			series := a.observations[a.sorted[position]-1].series
			for end < len(a.sorted) && a.observations[a.sorted[end]-1].series.ref == series.ref {
				end++
			}
			series.Lock()
			stable := nativeMetricMetadataGroupStable(series.nativeMetadataLocked(), a, a.sorted[position:end])
			series.Unlock()
			if !stable {
				entry := record.RefNativeMetadata{Ref: series.ref, Kind: record.NativeMetadataGroup}
				for _, observationRef := range a.sorted[position:end] {
					observation := a.observations[observationRef-1]
					m := a.metadataValue(observation.metadataRef)
					point := record.RefNativeMetadataPoint{EffectiveFrom: observation.effectiveFrom, Type: record.GetMetricType(m.Type), Unit: m.Unit, Help: m.Help}
					if last := len(entry.Points) - 1; last >= 0 && entry.Points[last].EffectiveFrom == point.EffectiveFrom {
						entry.Points[last] = point
					} else {
						entry.Points = append(entry.Points, point)
					}
				}
				entries = append(entries, entry)
			}
			position = end
		}
	}
	return entries
}

// normalizeNativeMetadataForTest clears capacity differences for comparisons.
func normalizeNativeMetadataForTest(entries []record.RefNativeMetadata) []record.RefNativeMetadata {
	out := make([]record.RefNativeMetadata, 0, len(entries))
	for _, e := range entries {
		e.Points = slices.Clip(e.Points)
		out = append(out, e)
	}
	return out
}

func TestNativeMetricMetadataReplay(t *testing.T) {
	m := func(help string) metadata.Metadata {
		return metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: help}
	}
	l := labels.FromStrings(labels.MetricName, "metric")
	nativeOpts := func() *Options {
		opts := DefaultOptions()
		opts.EnableNativeMetadata = true
		return opts
	}
	reopen := func(t *testing.T, db *DB, opts *Options) *DB {
		dir := db.Dir()
		require.NoError(t, db.Close())
		return newTestDB(t, withDir(dir), withOpts(opts))
	}
	commit := func(t *testing.T, db *DB, ts int64, md metadata.Metadata) {
		app := db.AppenderV2(t.Context())
		_, err := app.Append(0, l, 0, ts, 1, nil, nil, storage.AOptions{Metadata: md})
		require.NoError(t, err)
		require.NoError(t, app.Commit())
	}

	for _, c := range []struct {
		name      string
		helps     []string
		want      NativeMetricMetadataVersion
		truncated bool
	}{
		{name: "one version", helps: []string{"a"}, want: NativeMetricMetadataVersion{EffectiveFrom: 100, Metadata: m("a")}},
		{name: "older versions are dropped", helps: []string{"a", "b", "c"}, want: NativeMetricMetadataVersion{EffectiveFrom: 300, Metadata: m("c")}, truncated: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := newTestDB(t, withOpts(nativeOpts()))
			for i, help := range c.helps {
				commit(t, db, int64(100*(i+1)), m(help))
			}
			db = reopen(t, db, nativeOpts())
			require.Equal(t, map[string]NativeMetricMetadataSeries{l.String(): {Labels: l, Versions: []NativeMetricMetadataVersion{c.want}, Truncated: c.truncated}}, nativeMetadataSnapshot(t, db))
			require.Equal(t, int64(1), db.head.nativeMetricMetadata.series.Load())
			require.Equal(t, int64(1), db.head.nativeMetricMetadata.versions.Load())
			// Seeded state is committed state: an unchanged sample is stable.
			commit(t, db, 1000, c.want.Metadata)
			wal := readNativeMetadataWAL(t, db.Head().wal.Dir())
			require.Len(t, wal.entries, len(c.helps))
		})
	}

	t.Run("legacy entries start at an unknown time", func(t *testing.T) {
		opts := DefaultOptions()
		opts.EnableMetadataWALRecords = true
		db := newTestDB(t, withOpts(opts))
		commit(t, db, 100, m("a"))
		db = reopen(t, db, nativeOpts())
		require.Equal(t, []NativeMetricMetadataVersion{{EffectiveFrom: math.MinInt64, Metadata: m("a")}}, nativeMetadataSnapshot(t, db)[l.String()].Versions)
	})

	t.Run("reduction precedes ref remapping", func(t *testing.T) {
		point := func(from int64, help string) []record.RefNativeMetadataPoint {
			return []record.RefNativeMetadataPoint{{EffectiveFrom: from, Type: uint8(record.Counter), Unit: "seconds", Help: help}}
		}
		group := func(ref chunks.HeadSeriesRef, from int64, help string) []record.RefNativeMetadata {
			return []record.RefNativeMetadata{{Ref: ref, Kind: record.NativeMetadataGroup, Points: point(from, help)}}
		}
		other := labels.FromStrings(labels.MetricName, "other")
		for _, c := range []struct {
			name string
			recs []any
			want map[string]NativeMetricMetadataVersion
		}{
			{
				// Ref 1 was retired and its label set recreated as ref 2. A
				// delayed entry for ref 1 must not replace ref 2's history.
				name: "latest series record wins",
				recs: []any{
					[]record.RefSeries{{Ref: 1, Labels: l}},
					group(1, 100, "a"),
					[]record.RefSeries{{Ref: 2, Labels: l}},
					group(2, 200, "b"),
					[]record.RefSample{{Ref: 2, T: 200, V: 1}},
					group(1, 300, "c"),
				},
				want: map[string]NativeMetricMetadataVersion{l.String(): {EffectiveFrom: 200, Metadata: m("b")}},
			},
			{
				name: "the current incarnation without metadata",
				recs: []any{
					[]record.RefSeries{{Ref: 1, Labels: l}},
					group(1, 100, "a"),
					[]record.RefSample{{Ref: 1, T: 100, V: 1}},
					[]record.RefSeries{{Ref: 2, Labels: l}},
					[]record.RefSample{{Ref: 2, T: 200, V: 1}},
				},
				want: map[string]NativeMetricMetadataVersion{},
			},
			{
				// Another appender may commit metadata for a published series
				// before its creator logs the series record.
				name: "entries before their series record",
				recs: []any{
					group(7, 100, "a"), []record.RefSeries{{Ref: 7, Labels: other}}, []record.RefSample{{Ref: 7, T: 100, V: 1}},
				},
				want: map[string]NativeMetricMetadataVersion{other.String(): {EffectiveFrom: 100, Metadata: m("a")}},
			},
		} {
			for _, compact := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/compact=%t", c.name, compact), func(t *testing.T) {
					recs := slices.Clone(c.recs)
					for i, rec := range recs {
						if entries, ok := rec.([]record.RefNativeMetadata); ok && compact {
							recs[i] = compactNativeMetadataForTest(entries...)
						}
					}
					dir := t.TempDir()
					w, err := wlog.New(nil, nil, filepath.Join(dir, "wal"), compression.None)
					require.NoError(t, err)
					populateTestWL(t, w, recs, nil, false)
					require.NoError(t, w.Close())
					db := newTestDB(t, withDir(dir), withOpts(nativeOpts()))
					got := map[string]NativeMetricMetadataVersion{}
					for name, s := range nativeMetadataSnapshot(t, db) {
						require.Len(t, s.Versions, 1)
						got[name] = s.Versions[0]
					}
					require.Equal(t, c.want, got)
				})
			}
		}
	})

	t.Run("compact records", func(t *testing.T) {
		point := func(from int64, help string) record.RefNativeMetadataPoint {
			return record.RefNativeMetadataPoint{EffectiveFrom: from, Type: uint8(record.Counter), Unit: "seconds", Help: help}
		}
		entry := func(ref chunks.HeadSeriesRef, kind record.NativeMetadataKind, truncated bool, points ...record.RefNativeMetadataPoint) record.RefNativeMetadata {
			return record.RefNativeMetadata{Ref: ref, Kind: kind, Truncated: truncated, Points: points}
		}
		ls := []labels.Labels{l, labels.FromStrings(labels.MetricName, "two"), labels.FromStrings(labels.MetricName, "three")}
		d := record.NativeMetadataValue{Type: uint8(record.Counter), Unit: "seconds", Help: "d"}
		// A type-6 entry of unknown kind always has a point, its main fields.
		unknownMetadata := encoding.Encbuf{}
		unknownMetadata.PutByte(byte(record.Metadata))
		unknownMetadata.PutUvarint64(3)
		unknownMetadata.PutByte(uint8(record.Counter))
		unknownMetadata.PutUvarint(3)
		unknownMetadata.PutUvarintStr("UNIT")
		unknownMetadata.PutUvarintStr("seconds")
		unknownMetadata.PutUvarintStr("HELP")
		unknownMetadata.PutUvarintStr("e")
		unknownMetadata.PutUvarintStr("k")
		unknownMetadata.PutUvarintBytes([]byte{1 << 7})
		for _, c := range []struct {
			name  string
			last  []byte
			three NativeMetricMetadataSeries
		}{
			{
				// An unknown entry without points leaves its ref's state as it was.
				name:  "an unknown compact entry without points",
				last:  unknownCompactNativeMetadataForTest(3, nil, 0),
				three: NativeMetricMetadataSeries{Labels: ls[2], Versions: []NativeMetricMetadataVersion{{EffectiveFrom: 60, Metadata: m("b")}}, Truncated: true},
			},
			{
				// The contrast: a Metadata record's unknown entry replaces the
				// history with its main fields at an unknown start.
				name:  "an unknown Metadata entry",
				last:  unknownMetadata.Get(),
				three: NativeMetricMetadataSeries{Labels: ls[2], Versions: []NativeMetricMetadataVersion{{EffectiveFrom: math.MinInt64, Metadata: m("e")}}},
			},
		} {
			t.Run(c.name, func(t *testing.T) {
				recs := []any{
					[]record.RefSeries{{Ref: 1, Labels: ls[0]}, {Ref: 2, Labels: ls[1]}, {Ref: 3, Labels: ls[2]}},
					[]record.RefSample{{Ref: 1, T: 300, V: 1}, {Ref: 2, T: 300, V: 1}, {Ref: 3, T: 300, V: 1}},
					[]record.RefMetadata{{Ref: 1, Type: uint8(record.Counter), Unit: "seconds", Help: "a"}},
					compactNativeMetadataForTest(
						entry(1, record.NativeMetadataGroup, false, point(100, "b")),
						entry(2, record.NativeMetadataGroup, false, point(100, "a"), point(200, "c")),
						entry(3, record.NativeMetadataOverride, true, point(50, "a"), point(60, "b")),
					),
					// Mixed WALs apply both kinds of record in WAL order.
					[]record.RefNativeMetadata{entry(1, record.NativeMetadataGroup, false, point(300, "c"))},
					unknownCompactNativeMetadataForTest(2, &d, 250),
					c.last,
				}
				dir := t.TempDir()
				w, err := wlog.New(nil, nil, filepath.Join(dir, "wal"), compression.None)
				require.NoError(t, err)
				populateTestWL(t, w, recs, nil, false)
				require.NoError(t, w.Close())
				db := newTestDB(t, withDir(dir), withOpts(nativeOpts()))
				require.Equal(t, map[string]NativeMetricMetadataSeries{
					ls[0].String(): {Labels: ls[0], Versions: []NativeMetricMetadataVersion{{EffectiveFrom: 300, Metadata: m("c")}}, Truncated: true},
					// An unknown entry with a point applies as a single-point override.
					ls[1].String(): {Labels: ls[1], Versions: []NativeMetricMetadataVersion{{EffectiveFrom: 250, Metadata: m("d")}}},
					ls[2].String(): c.three,
				}, nativeMetadataSnapshot(t, db))
				require.Equal(t, 2.0, prom_testutil.ToFloat64(db.head.metrics.nativeMetadataUnknownEntries))
			})
		}
	})

	t.Run("compact records in legacy mode", func(t *testing.T) {
		// Legacy mode reads each entry's newest point as legacy metadata, and
		// an empty override as empty metadata, as it reads Metadata records'
		// native entries. Entries of unknown kind without points change
		// nothing.
		for _, walRecords := range []bool{false, true} {
			dir := t.TempDir()
			w, err := wlog.New(nil, nil, filepath.Join(dir, "wal"), compression.None)
			require.NoError(t, err)
			two, three := labels.FromStrings(labels.MetricName, "two"), labels.FromStrings(labels.MetricName, "three")
			legacy := func(ref chunks.HeadSeriesRef, help string) record.RefMetadata {
				return record.RefMetadata{Ref: ref, Type: uint8(record.Counter), Unit: "seconds", Help: help}
			}
			populateTestWL(t, w, []any{
				[]record.RefSeries{{Ref: 1, Labels: l}, {Ref: 2, Labels: two}, {Ref: 3, Labels: three}},
				[]record.RefSample{{Ref: 1, T: 2, V: 1}, {Ref: 2, T: 2, V: 1}, {Ref: 3, T: 2, V: 1}},
				[]record.RefMetadata{legacy(2, "a"), legacy(3, "c")},
				compactNativeMetadataForTest(
					record.RefNativeMetadata{Ref: 1, Kind: record.NativeMetadataGroup, Points: []record.RefNativeMetadataPoint{{EffectiveFrom: 1, Help: "x"}, {EffectiveFrom: 2, Type: uint8(record.Gauge), Unit: "seconds", Help: "b"}}},
					record.RefNativeMetadata{Ref: 2, Kind: record.NativeMetadataOverride},
				),
				unknownCompactNativeMetadataForTest(3, nil, 0),
			}, nil, false)
			require.NoError(t, w.Close())
			opts := DefaultOptions()
			opts.EnableMetadataWALRecords = walRecords
			db := newTestDB(t, withDir(dir), withOpts(opts))
			require.Equal(t, &metadata.Metadata{Type: model.MetricTypeGauge, Unit: "seconds", Help: "b"}, legacyMetadataForTest(db.head.series.getByID(1)))
			require.Equal(t, &metadata.Metadata{Type: model.MetricTypeUnknown}, legacyMetadataForTest(db.head.series.getByID(2)), "an empty override")
			require.Equal(t, &metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: "c"}, legacyMetadataForTest(db.head.series.getByID(3)), "an ignored entry")
		}
	})

	t.Run("checkpoints", func(t *testing.T) {
		opts := nativeOpts()
		opts.WALSegmentSize = 32 << 10
		db := newTestDB(t, withOpts(opts))
		lsets := make([]labels.Labels, 200)
		for i := range lsets {
			lsets[i] = labels.FromStrings(labels.MetricName, "metric", "id", strconv.Itoa(i))
		}
		refs := make([]storage.SeriesRef, len(lsets))
		for step := range 60 {
			app := db.AppenderV2(t.Context())
			for i, lset := range lsets {
				ref, err := app.Append(refs[i], lset, 0, int64(step*1000), 1, nil, nil, storage.AOptions{Metadata: m(strconv.Itoa(min(step, 6) % 3))})
				require.NoError(t, err)
				refs[i] = ref
			}
			require.NoError(t, app.Commit())
		}
		want := nativeMetadataSnapshot(t, db)
		before := readNativeMetadataWAL(t, db.Head().wal.Dir()).reduce()
		require.NoError(t, db.Head().Truncate(30000))
		_, _, err := wlog.LastCheckpoint(db.Head().wal.Dir())
		require.NoError(t, err)
		after := readNativeMetadataWAL(t, db.Head().wal.Dir())
		require.Contains(t, after.types, record.NativeMetadataCompact)
		reduced := after.reduce()
		for i := range lsets {
			ref := chunks.HeadSeriesRef(refs[i])
			require.Equal(t, nativeMetadataVersions(before[ref]), nativeMetadataVersions(reduced[ref]))
			require.Equal(t, before[ref].Truncated, reduced[ref].Truncated)
			require.Equal(t, want[lsets[i].String()].Versions, nativeMetadataVersions(reduced[ref]))
		}
		db = reopen(t, db, opts)
		for _, s := range nativeMetadataSnapshot(t, db) {
			require.Equal(t, []NativeMetricMetadataVersion{{EffectiveFrom: 6000, Metadata: m("0")}}, s.Versions)
			require.True(t, s.Truncated)
		}
	})
}

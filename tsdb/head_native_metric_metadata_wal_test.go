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
	"math"
	"math/rand/v2"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
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
	for r.Next() {
		typ := dec.Type(r.Record())
		out.types = append(out.types, typ)
		if typ == record.Metadata {
			entries, _, err := dec.NativeMetadata(r.Record(), nil, nil)
			require.NoError(t, err)
			out.entries = append(out.entries, entries...)
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
		state := states[e.Ref]
		state.Apply(e.Kind, e.Truncated, nativemetadata.AppendRecordPoints(nil, e.Points, intern))
		states[e.Ref] = state
	}
	return states
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
			record.Series, record.Metadata, record.Samples,
			record.Series, record.Metadata, record.Samples,
			record.Metadata, record.Samples,
		}, wal.types)
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
			var enc record.Encoder
			want := int64(0)
			for round := 0; round <= w.rounds; round++ {
				for offset := 0; offset < w.series; offset += 100 {
					want++
					for id := offset; id < min(offset+100, w.series); id++ {
						entry := record.RefNativeMetadata{Ref: chunks.HeadSeriesRef(id + 1), Kind: record.NativeMetadataGroup}
						w.samples(round, func(ts int64, version int) {
							m := w.metadata(id, version)
							entry.Points = append(entry.Points, record.RefNativeMetadataPoint{EffectiveFrom: ts, Type: record.GetMetricType(m.Type), Unit: m.Unit, Help: m.Help})
						})
						want += int64(len(enc.NativeMetadata([]record.RefNativeMetadata{entry}, nil)) - 1)
					}
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
		}
	})
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
			t.Run(c.name, func(t *testing.T) {
				dir := t.TempDir()
				w, err := wlog.New(nil, nil, filepath.Join(dir, "wal"), compression.None)
				require.NoError(t, err)
				populateTestWL(t, w, c.recs, nil, false)
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
		require.Contains(t, after.types, record.Metadata)
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

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
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/nativemetadata"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/util/compression"
)

// The stage A adapter for P2a bases: native metadata records, the pre-log
// pass, and the histories that the Head and the sender share.

func stageANativeLogsMetadata() bool { return true }

// stageAPreparePreLog appends version 1 for one commit's series without
// committing, and returns a function running the pre-log pass over them. The
// pass changes no state but its scratch, so repeated runs do identical work.
func stageAPreparePreLog(tb testing.TB, c stageAWriterCase) (func() int, func(), string) {
	db, in, refs := stageAPreparedDB(tb, c)
	app := db.head.appenderV2()
	for slot := range stageACommit {
		_, err := app.Append(refs[slot], in.labels[slot], 0, stageATimestamp(1), 2, nil, nil, storage.AOptions{Metadata: in.metadata[slot][1]})
		require.NoError(tb, err)
	}
	native := app.nativeMetricMetadata
	require.NotNil(tb, native)
	done := func() {
		require.NoError(tb, app.Rollback())
		require.NoError(tb, db.Close())
	}
	return func() int { return len(native.appendWALEntries()) }, done, ""
}

// stageAPreparedDB returns a database seeded with version 0 of one commit's
// series.
func stageAPreparedDB(tb testing.TB, c stageAWriterCase) (*DB, *stageAWriterInput, []storage.SeriesRef) {
	in := stageAInputFor(c.values())
	opts := DefaultOptions()
	opts.EnableNativeMetadata = true
	opts.WALCompression = compression.Snappy
	db, err := Open(tb.TempDir(), nil, nil, opts, nil)
	require.NoError(tb, err)
	db.DisableCompactions()
	app := db.AppenderV2(context.Background())
	refs := make([]storage.SeriesRef, stageACommit)
	for slot := range stageACommit {
		refs[slot], err = app.Append(0, in.labels[slot], 0, stageATimestamp(0), 1, nil, nil, storage.AOptions{Metadata: in.metadata[slot][0]})
		require.NoError(tb, err)
	}
	require.NoError(tb, app.Commit())
	return db, in, refs
}

// stageANativeRecordStarts returns a native record's series, helps and
// starts; every entry must be a one-point merge group.
func stageANativeRecordStarts(tb testing.TB, rec []byte) []stageAStart {
	var dec record.Decoder
	entries, _, err := dec.NativeMetadata(rec, nil, nil)
	require.NoError(tb, err)
	starts := make([]stageAStart, 0, len(entries))
	for _, e := range entries {
		require.Equal(tb, record.NativeMetadataGroup, e.Kind)
		require.Len(tb, e.Points, 1)
		starts = append(starts, stageAStart{ref: e.Ref, help: e.Points[0].Help, from: e.Points[0].EffectiveFrom})
	}
	return starts
}

func stageANativeEntries(points []stageAChange) []record.RefNativeMetadata {
	entries := make([]record.RefNativeMetadata, len(points))
	for i, p := range points {
		entries[i] = record.RefNativeMetadata{Ref: p.ref, Kind: record.NativeMetadataGroup, Points: []record.RefNativeMetadataPoint{{
			EffectiveFrom: p.from, Type: record.GetMetricType(p.m.Type), Unit: p.m.Unit, Help: p.m.Help,
		}}}
	}
	return entries
}

func stageAEncodeNativeRecord(points []stageAChange, buf []byte) ([]byte, bool) {
	var enc record.Encoder
	return enc.NativeMetadata(stageANativeEntries(points), buf), true
}

func stageANativeEncodeWork(points []stageAChange, buf *[]byte) func() {
	entries := stageANativeEntries(points)
	var enc record.Encoder
	return func() { *buf = enc.NativeMetadata(entries, (*buf)[:0]) }
}

// stageANativeHistories are the histories that the Head's native state and
// the sender's reduced state both use.
type stageANativeHistories []nativemetadata.History

func stageANewHistories(n int) stageAHistories {
	return stageANativeHistories(make([]nativemetadata.History, n))
}

func (h stageANativeHistories) merge(i int, from int64, m *metadata.Metadata) {
	h[i].Merge([]nativemetadata.Point{{EffectiveFrom: from, Metadata: m}})
}

func (h stageANativeHistories) depth(i int) int { return h[i].Len() }

func (h stageANativeHistories) olderCapacityBytes(i int) int {
	return cap(h[i].Older) * int(unsafe.Sizeof(nativemetadata.Point{}))
}

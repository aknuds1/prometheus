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
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
)

// The W1 writer adapter for A0: Commit's merge groups in Metadata records,
// built with string-valued points from the transaction's observations.

// w1Transaction is a staged transaction: an appender holding its
// observations, and the entries built from them.
type w1Transaction struct {
	a       *nativeMetricMetadataAppender
	entries []record.RefNativeMetadata
}

type w1Staged = *w1Transaction

func w1WriterUnavailable(string) string { return "" }

func w1ComponentUnavailable(string, string) string { return "" }

func w1CommitMode() string { return "native" }

func w1MetadataRecordType(t record.Type) bool { return t == record.Metadata }

// w1Stage observes txn in a fresh appender, as appends do.
func w1Stage(tb testing.TB, db *DB, refs []storage.SeriesRef, txn []w1Observation) w1Staged {
	a := db.head.nativeMetricMetadata.getAppender()
	for _, o := range txn {
		series := db.head.series.getByID(chunks.HeadSeriesRef(refs[o.slot]))
		require.NotNil(tb, series)
		a.observe(series, o.ts, o.m)
	}
	return &w1Transaction{a: a}
}

// w1Construct builds the record's entries, as log() does.
func w1Construct(s w1Staged) {
	s.entries = s.a.appendWALEntries()
}

func w1Encode(s w1Staged, buf []byte) []byte {
	var enc record.Encoder
	return enc.NativeMetadata(s.entries, buf)
}

// w1Release returns the appender to its pool, and the bytes of WAL scratch
// it then retains.
func w1Release(db *DB, s w1Staged) int {
	a := s.a
	db.head.nativeMetricMetadata.putAppender(a)
	return cap(a.walEntries)*int(unsafe.Sizeof(record.RefNativeMetadata{})) +
		cap(a.walPoints)*int(unsafe.Sizeof(record.RefNativeMetadataPoint{}))
}

// w1DecodeGroups decodes a record's merge groups. Metadata records have no
// dictionary.
func w1DecodeGroups(tb testing.TB, rec []byte) ([]w1Group, int) {
	var dec record.Decoder
	require.Equal(tb, record.Metadata, dec.Type(rec))
	entries, _, err := dec.NativeMetadata(rec, nil, nil)
	require.NoError(tb, err)
	return w1GroupsOf(tb, entries), 0
}

func w1GroupsOf(tb testing.TB, entries []record.RefNativeMetadata) []w1Group {
	groups := make([]w1Group, 0, len(entries))
	for _, e := range entries {
		require.Equal(tb, record.NativeMetadataGroup, e.Kind)
		require.False(tb, e.Truncated)
		g := w1Group{ref: e.Ref}
		for _, p := range e.Points {
			g.points = append(g.points, w1Point{from: p.EffectiveFrom, m: metadata.Metadata{Type: record.ToMetricType(p.Type), Unit: p.Unit, Help: p.Help}})
		}
		groups = append(groups, g)
	}
	return groups
}

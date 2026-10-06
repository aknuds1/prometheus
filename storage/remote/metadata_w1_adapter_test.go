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
	"testing"

	remoteapi "github.com/prometheus/client_golang/exp/api/remote"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
)

// The W1 sender adapter for A0: merge groups in Metadata records, with
// string-valued points, stored as borrowed entries as the watcher passes them
// to queues.

func w1SenderUnavailable(string) string { return "" }

// w1EncodeRecord encodes one transaction's groups as Commit does.
func w1EncodeRecord(txn []w1Group) []byte {
	entries := make([]record.RefNativeMetadata, 0, len(txn))
	for _, g := range txn {
		e := record.RefNativeMetadata{Ref: g.ref, Kind: record.NativeMetadataGroup}
		for _, p := range g.points {
			e.Points = append(e.Points, record.RefNativeMetadataPoint{EffectiveFrom: p.from, Type: record.GetMetricType(p.m.Type), Unit: p.m.Unit, Help: p.m.Help})
		}
		entries = append(entries, e)
	}
	var enc record.Encoder
	return enc.NativeMetadata(entries, nil)
}

// w1RecordFormat names this build's metadata record format for the byte
// evidence.
func w1RecordFormat() string { return "metadata" }

func w1NewQueueManager(tb testing.TB) *QueueManager {
	return newTestQueueManager(tb, testDefaultQueueConfig(), config.DefaultMetadataConfig, defaultFlushDeadline, NewNopWriteClient(), remoteapi.WriteV2MessageType, true)
}

// w1Decoder decodes records with reused buffers, lending their strings, and
// stores them as the watcher passes them to queues.
type w1Decoder struct {
	dec     record.Decoder
	entries []record.RefNativeMetadata
	points  []record.RefNativeMetadataPoint
}

func newW1Decoder() *w1Decoder { return &w1Decoder{} }

func (d *w1Decoder) decode(tb testing.TB, rec []byte) {
	var err error
	d.entries, d.points, err = d.dec.NativeMetadataBorrowed(rec, d.entries[:0], d.points[:0])
	if err != nil {
		require.NoError(tb, err)
	}
}

func (d *w1Decoder) store(qm *QueueManager) {
	w := nativeMetadataWriter{qm}
	if w1InternHook != nil {
		w.storeNativeMetadata(d.entries, w1InternHook)
		return
	}
	w.StoreBorrowedNativeMetadata(d.entries)
}

func w1View(_ testing.TB, qm *QueueManager, ref chunks.HeadSeriesRef) (int, *metadata.Metadata) {
	state := qm.seriesNativeMetadata[ref]
	return state.Len(), state.Metadata
}

// w1InternHook replaces the store's interning while a counting run is in
// progress.
var w1InternHook func(metadata.Metadata) *metadata.Metadata

// w1UseInterner installs a fresh process interner, wrapping its borrowing
// entry point with wrap if set, and returns a function that restores the
// previous one.
func w1UseInterner(wrap func(func(metadata.Metadata) *metadata.Metadata) func(metadata.Metadata) *metadata.Metadata) func() {
	previous := walMetadataInterner
	fresh := newMetadataInterner(metadataInternerEntries, metadataInternerBytes)
	walMetadataInterner = fresh
	if wrap != nil {
		w1InternHook = wrap(fresh.internBorrowed)
	}
	return func() {
		walMetadataInterner = previous
		w1InternHook = nil
	}
}

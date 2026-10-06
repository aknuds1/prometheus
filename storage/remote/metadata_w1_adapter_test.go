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

// The W1 sender adapter for W1-V with values inline: compact records that
// define each value once, at its first use, stored whole as the watcher passes
// them to queues.

func w1SenderUnavailable(string) string { return "" }

// w1RecordFormat names this build's metadata record format for the byte
// evidence.
func w1RecordFormat() string { return "inline" }

// w1EncodeRecord encodes one transaction's groups as Commit does.
func w1EncodeRecord(txn []w1Group) []byte {
	values, entries := w1Dictionary(txn)
	var enc record.Encoder
	return enc.CompactNativeMetadata(values, entries, nil)
}

// w1Dictionary returns txn's values, each once in order of first use, as
// Commit builds them, and its entries indexing them.
func w1Dictionary(txn []w1Group) ([]record.NativeMetadataValue, []record.RefCompactNativeMetadata) {
	var values []record.NativeMetadataValue
	entries := make([]record.RefCompactNativeMetadata, 0, len(txn))
	indices := map[record.NativeMetadataValue]uint32{}
	for _, g := range txn {
		c := record.RefCompactNativeMetadata{Ref: g.ref, Kind: record.NativeMetadataGroup}
		for _, p := range g.points {
			v := record.NativeMetadataValue{Type: record.GetMetricType(p.m.Type), Unit: p.m.Unit, Help: p.m.Help}
			index, ok := indices[v]
			if !ok {
				index = uint32(len(values))
				indices[v] = index
				values = append(values, v)
			}
			c.Points = append(c.Points, record.CompactNativeMetadataPoint{EffectiveFrom: p.from, Value: index})
		}
		entries = append(entries, c)
	}
	return values, entries
}

func w1NewQueueManager(tb testing.TB) *QueueManager {
	return newTestQueueManager(tb, testDefaultQueueConfig(), config.DefaultMetadataConfig, defaultFlushDeadline, NewNopWriteClient(), remoteapi.WriteV2MessageType, true)
}

// w1Decoder decodes records with reused buffers, lending their strings, and
// stores them as the watcher passes them to queues.
type w1Decoder struct {
	dec     record.Decoder
	compact record.CompactNativeMetadata
}

func newW1Decoder() *w1Decoder { return &w1Decoder{} }

func (d *w1Decoder) decode(tb testing.TB, rec []byte) {
	if err := d.dec.CompactNativeMetadataBorrowed(rec, &d.compact); err != nil {
		require.NoError(tb, err)
	}
}

func (d *w1Decoder) store(qm *QueueManager) {
	w := nativeMetadataWriter{qm}
	if w1InternHook != nil {
		w.storeNativeMetadataRecord(&d.compact, w1InternHook)
		return
	}
	w.StoreBorrowedNativeMetadataRecord(&d.compact)
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

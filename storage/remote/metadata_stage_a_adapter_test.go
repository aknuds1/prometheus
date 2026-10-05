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
	"reflect"
	"testing"

	remoteapi "github.com/prometheus/client_golang/exp/api/remote"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
)

// The stage A adapter for P2a bases: native metadata records decoded and
// stored as the watcher does, and the WAL metadata interner. Builds whose
// writer accepts borrowed entries get them, as from the watcher.

func stageANativeSupported() bool   { return true }
func stageAInternerSupported() bool { return true }

func stageAEncodeNative(points []stageAPoint) []byte {
	entries := make([]record.RefNativeMetadata, len(points))
	for i, p := range points {
		entries[i] = record.RefNativeMetadata{Ref: p.ref, Kind: record.NativeMetadataGroup, Points: []record.RefNativeMetadataPoint{{
			EffectiveFrom: p.from, Type: record.GetMetricType(p.m.Type), Unit: p.m.Unit, Help: p.m.Help,
		}}}
	}
	var enc record.Encoder
	return enc.NativeMetadata(entries, nil)
}

// The borrowing capability and decoder, declared here so that this adapter
// compiles on builds without them.
type (
	stageABorrowWriter interface {
		StoreBorrowedNativeMetadata([]record.RefNativeMetadata)
	}
	stageABorrowDecoder interface {
		NativeMetadataBorrowed([]byte, []record.RefNativeMetadata, []record.RefNativeMetadataPoint) ([]record.RefNativeMetadata, []record.RefNativeMetadataPoint, error)
	}
)

func stageANewQueueManager(tb testing.TB, path string) *QueueManager {
	if path == "native" {
		return newTestQueueManager(tb, testDefaultQueueConfig(), config.DefaultMetadataConfig, defaultFlushDeadline, NewNopWriteClient(), remoteapi.WriteV2MessageType, true)
	}
	return stageANewLegacyQueueManager(tb)
}

func stageANativeStore(tb testing.TB, qm *QueueManager) func([]byte) {
	var writer any = nativeMetadataWriter{qm}
	borrower, borrows := writer.(stageABorrowWriter)
	dec := &record.Decoder{}
	borrowing, _ := any(dec).(stageABorrowDecoder)
	require.True(tb, !borrows || borrowing != nil, "a writer that borrows needs a decoder that lends")
	var entries []record.RefNativeMetadata
	var points []record.RefNativeMetadataPoint
	return func(rec []byte) {
		var err error
		if borrows {
			entries, points, err = borrowing.NativeMetadataBorrowed(rec, entries[:0], points[:0])
			require.NoError(tb, err)
			borrower.StoreBorrowedNativeMetadata(entries)
		} else {
			entries, points, err = dec.NativeMetadata(rec, entries[:0], points[:0])
			require.NoError(tb, err)
			nativeMetadataWriter{qm}.StoreNativeMetadata(entries)
		}
		clear(entries)
		clear(points)
	}
}

func stageANativeSeries(qm *QueueManager) int { return len(qm.seriesNativeMetadata) }

func stageANativeView(tb testing.TB, qm *QueueManager, ref chunks.HeadSeriesRef) (int, *metadata.Metadata) {
	state, ok := qm.seriesNativeMetadata[ref]
	require.True(tb, ok, "series %d", ref)
	return state.Len(), state.Metadata
}

func stageANativeDecoder(decoder string) (func([]byte) error, string) {
	dec := &record.Decoder{}
	var entries []record.RefNativeMetadata
	var points []record.RefNativeMetadataPoint
	switch decoder {
	case "copying":
		return func(rec []byte) error {
			var err error
			entries, points, err = dec.NativeMetadata(rec, entries[:0], points[:0])
			return err
		}, ""
	case "borrowed":
		borrowing, ok := any(dec).(stageABorrowDecoder)
		if !ok {
			return nil, "no lending decoder"
		}
		return func(rec []byte) error {
			var err error
			entries, points, err = borrowing.NativeMetadataBorrowed(rec, entries[:0], points[:0])
			return err
		}, ""
	}
	return nil, "unknown decoder"
}

// stageAUseInterner installs a fresh process interner of kind, returning a
// function that restores the previous one and a reader of the fresh one.
func stageAUseInterner(kind string) (func(), func() stageAInternerState) {
	previous := walMetadataInterner
	fresh := newMetadataInterner(metadataInternerEntries, metadataInternerBytes)
	if kind == "bypass" {
		fresh = newMetadataInterner(0, -1)
	}
	walMetadataInterner = fresh
	return func() { walMetadataInterner = previous }, func() stageAInternerState { return stageAInternerStateOf(fresh) }
}

// stageAInternerStateOf reads i's generations, and its ledger through
// reflection, since builds differ in what a ledger slot holds.
func stageAInternerStateOf(i *metadataInterner) stageAInternerState {
	i.mtx.Lock()
	defer i.mtx.Unlock()
	s := stageAInternerState{entries: len(i.current) + len(i.older)}
	ledger := reflect.ValueOf(i).Elem().FieldByName("ledger")
	if !ledger.IsValid() {
		return s
	}
	s.hasLedger = true
	for n := range ledger.Len() {
		slot := ledger.Index(n)
		switch slot.Kind() {
		case reflect.Uint64:
			if slot.Uint() != 0 {
				s.ledgerFingerprints++
			}
		case reflect.Struct:
			if slot.FieldByName("fingerprint").Uint() != 0 {
				s.ledgerFingerprints++
			}
			if !slot.FieldByName("value").IsNil() {
				s.ledgerValues++
			}
		default:
			panic("unexpected ledger slot " + slot.Kind().String())
		}
	}
	return s
}

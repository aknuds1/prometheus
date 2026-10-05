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

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/chunks"
)

// The stage A adapter for the comparability base: legacy metadata records and
// the WAL metadata interner, without native metadata records.

func stageANativeSupported() bool   { return false }
func stageAInternerSupported() bool { return true }

func stageANewQueueManager(tb testing.TB, _ string) *QueueManager {
	return stageANewLegacyQueueManager(tb)
}

func stageAEncodeNative([]stageAPoint) []byte {
	panic("stage A: no native metadata records on this base")
}

func stageANativeStore(testing.TB, *QueueManager) func([]byte) {
	panic("stage A: no native metadata records on this base")
}

func stageANativeSeries(*QueueManager) int {
	panic("stage A: no native metadata records on this base")
}

func stageANativeView(testing.TB, *QueueManager, chunks.HeadSeriesRef) (int, *metadata.Metadata) {
	panic("stage A: no native metadata records on this base")
}

func stageANativeDecoder(string) (func([]byte) error, string) {
	return nil, "no native metadata records"
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

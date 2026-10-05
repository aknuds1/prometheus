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

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/chunks"
)

// The stage A adapter for unmodified ACTE: legacy metadata records, stored
// without a WAL metadata interner or native metadata records.

func stageANativeSupported() bool   { return false }
func stageAInternerSupported() bool { return false }

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

// stageAUseInterner leaves this base's storage unchanged: it has no interner.
func stageAUseInterner(string) (func(), func() stageAInternerState) {
	return func() {}, func() stageAInternerState { return stageAInternerState{} }
}

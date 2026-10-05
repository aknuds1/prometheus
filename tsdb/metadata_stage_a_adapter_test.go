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

	"github.com/prometheus/prometheus/model/metadata"
)

// The stage A adapter for ACTE bases: native metadata stays out of the WAL,
// there is no pre-log pass, and histories are the Head's own.

func stageANativeLogsMetadata() bool { return false }

func stageAPreparePreLog(testing.TB, stageAWriterCase) (func() int, func(), string) {
	return nil, nil, "no pre-log pass"
}

func stageANativeRecordStarts(testing.TB, []byte) []stageAStart {
	panic("stage A: no native metadata records on this base")
}

func stageAEncodeNativeRecord([]stageAChange, []byte) ([]byte, bool) { return nil, false }

func stageANativeEncodeWork([]stageAChange, *[]byte) func() {
	panic("stage A: no native metadata records on this base")
}

type stageAHeadHistories []nativeSeriesMetadata

func stageANewHistories(n int) stageAHistories {
	return stageAHeadHistories(make([]nativeSeriesMetadata, n))
}

func (h stageAHeadHistories) merge(i int, from int64, m *metadata.Metadata) {
	h[i].mergeLocked([]nativeMetricMetadataPoint{{effectiveFrom: from, metadata: m}})
}

func (h stageAHeadHistories) depth(i int) int {
	if h[i].metadata == nil {
		return 0
	}
	return len(h[i].older) + 1
}

func (h stageAHeadHistories) olderCapacityBytes(i int) int {
	return cap(h[i].older) * int(unsafe.Sizeof(nativeMetricMetadataPoint{}))
}

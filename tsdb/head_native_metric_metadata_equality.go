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

import "github.com/prometheus/prometheus/model/metadata"

const (
	nativeMetricMetadataEqualityMinBytes   = 1024
	nativeMetricMetadataEqualityMaxEntries = 128
	nativeMetricMetadataEqualityMaxBytes   = 4 << 20
)

// nativeMetricMetadataEqualityMemo retains exact equality proofs for one
// transaction. Keys own immutable committed values; values retain caller strings
// only until Commit or Rollback. It must never determine observation timestamps.
type nativeMetricMetadataEqualityMemo struct {
	values map[*metadata.Metadata]metadata.Metadata
	bytes  int
}

// nativeMetricMetadataEqualityCost charges both sides of an equality proof.
// This bounds logical string payload, not backing allocations or total heap.
// Zero means the value exceeds the budget.
func nativeMetricMetadataEqualityCost(m metadata.Metadata) int {
	remaining := nativeMetricMetadataEqualityMaxBytes
	for _, value := range [...]string{string(m.Type), m.Unit, m.Help} {
		if len(value) > remaining/2 {
			return 0
		}
		remaining -= 2 * len(value)
	}
	return nativeMetricMetadataEqualityMaxBytes - remaining
}

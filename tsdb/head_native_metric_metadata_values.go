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
	"hash/maphash"
	"slices"
	"strings"
	"sync"
	"unsafe"

	"github.com/prometheus/common/model"
	"go.uber.org/atomic"

	"github.com/prometheus/prometheus/model/metadata"
)

const (
	nativeMetadataValueShards       = 64
	nativeMetadataValuesPerShard    = 64
	nativeMetadataRecentMisses      = 64
	nativeMetadataSharedStringBytes = 4 << 20
)

// nativeMetricMetadataValueCache shares immutable values within one Head. Entry
// and string-payload limits bound cache ownership, not live histories or results.
// Values own their strings and never refer back to this cache.
type nativeMetricMetadataValueCache struct {
	bytes  atomic.Int64
	seed   maphash.Seed
	shards [nativeMetadataValueShards]atomic.Pointer[nativeMetricMetadataValueShard]
}

func newNativeMetricMetadataValueCache() *nativeMetricMetadataValueCache {
	return &nativeMetricMetadataValueCache{seed: maphash.MakeSeed()}
}

// resolve returns an immutable owned value. Callers must not hold series locks.
// Sharing is opportunistic; exact value equality never depends on admission.
func (c *nativeMetricMetadataValueCache) resolve(m metadata.Metadata) *metadata.Metadata {
	// Check without overflowing int, including on 32-bit builds. Oversized
	// values are fully retained by their users, but never by the sharing cache.
	size := len(m.Type)
	if size > nativeMetadataSharedStringBytes || len(m.Unit) > nativeMetadataSharedStringBytes-size {
		return cloneNativeMetricMetadata(m)
	}
	size += len(m.Unit)
	if len(m.Help) > nativeMetadataSharedStringBytes-size {
		return cloneNativeMetricMetadata(m)
	}
	size += len(m.Help)
	hash := maphash.Comparable(c.seed, m)
	slot := &c.shards[hash%nativeMetadataValueShards]
	shard := slot.Load()
	if shard == nil {
		shard = &nativeMetricMetadataValueShard{}
		if !slot.CompareAndSwap(nil, shard) {
			shard = slot.Load()
		}
	}
	shard.Lock()
	if existing := shard.values[m]; existing != nil {
		shard.Unlock()
		return existing
	}
	shard.Unlock()

	owned := cloneNativeMetricMetadata(m)
	shard.Lock()
	defer shard.Unlock()
	if existing := shard.values[m]; existing != nil {
		return existing
	}
	// A second observation admits a value. Fingerprints retain no strings;
	// collisions can cause admission but cannot select a different value.
	if !slices.Contains(shard.recent[:shard.recentCount], hash) {
		shard.recent[shard.recentNext] = hash
		shard.recentNext = (shard.recentNext + 1) % len(shard.recent)
		shard.recentCount = min(shard.recentCount+1, len(shard.recent))
		return owned
	}
	for shard.count >= len(shard.fifo) || !c.reserve(int64(size)) {
		if shard.count == 0 {
			return owned
		}
		old := shard.fifo[shard.first]
		shard.fifo[shard.first] = nil
		shard.first = (shard.first + 1) % len(shard.fifo)
		shard.count--
		delete(shard.values, *old)
		c.bytes.Add(-int64(len(old.Type) + len(old.Unit) + len(old.Help)))
	}
	if shard.values == nil {
		shard.values = make(map[metadata.Metadata]*metadata.Metadata)
	}
	// The key must own strings too: using m here could pin caller buffers even
	// though the returned value has been cloned.
	shard.values[*owned] = owned
	shard.fifo[(shard.first+shard.count)%len(shard.fifo)] = owned
	shard.count++
	return owned
}

func (c *nativeMetricMetadataValueCache) reserve(size int64) bool {
	if size == 0 {
		return true
	}
	for {
		used := c.bytes.Load()
		if size > nativeMetadataSharedStringBytes-used {
			return false
		}
		if c.bytes.CompareAndSwap(used, used+size) {
			return true
		}
	}
}

// nativeMetricMetadataValueShard owns its map, FIFO and recent-miss ledger under
// one lock. Eviction releases references; published values are never modified.
type nativeMetricMetadataValueShard struct {
	sync.Mutex
	values      map[metadata.Metadata]*metadata.Metadata
	fifo        [nativeMetadataValuesPerShard]*metadata.Metadata
	first       int
	count       int
	recent      [nativeMetadataRecentMisses]uint64
	recentCount int
	recentNext  int
}

// cloneNativeMetricMetadata returns a copy of m that does not alias m's memory.
// Known metric types are shared constants. The other non-empty strings share
// one exact-size payload, so retaining any of them retains all of them.
func cloneNativeMetricMetadata(m metadata.Metadata) *metadata.Metadata {
	owned := &metadata.Metadata{Type: knownMetricType(m.Type), Unit: m.Unit, Help: m.Help}
	typ := ""
	if owned.Type == "" {
		typ = string(m.Type)
	}
	ownCompactStrings(&typ, &owned.Unit, &owned.Help)
	if typ != "" {
		owned.Type = model.MetricType(typ)
	}
	return owned
}

// knownMetricType returns t's shared constant, or "" if t is not a known type.
func knownMetricType(t model.MetricType) model.MetricType {
	switch t {
	case model.MetricTypeCounter:
		return model.MetricTypeCounter
	case model.MetricTypeGauge:
		return model.MetricTypeGauge
	case model.MetricTypeHistogram:
		return model.MetricTypeHistogram
	case model.MetricTypeGaugeHistogram:
		return model.MetricTypeGaugeHistogram
	case model.MetricTypeSummary:
		return model.MetricTypeSummary
	case model.MetricTypeInfo:
		return model.MetricTypeInfo
	case model.MetricTypeStateset:
		return model.MetricTypeStateset
	case model.MetricTypeUnknown:
		return model.MetricTypeUnknown
	}
	return ""
}

// ownCompactStrings replaces the non-empty strings with copies. A single
// string gets its own allocation; several share one exact-size payload.
// Concatenation cannot be used: it returns an operand when the others are empty.
func ownCompactStrings(fields ...*string) {
	size, owned := 0, 0
	for _, f := range fields {
		if *f != "" {
			size += len(*f)
			owned++
		}
	}
	switch owned {
	case 0:
		return
	case 1:
		for _, f := range fields {
			*f = strings.Clone(*f)
		}
		return
	}
	payload := make([]byte, size)
	offset := 0
	for _, f := range fields {
		if *f == "" {
			continue
		}
		n := copy(payload[offset:], *f)
		*f = unsafe.String(&payload[offset], n)
		offset += n
	}
}

func equalNativeMetricMetadata(a, b *metadata.Metadata) bool {
	return a == b || a != nil && b != nil && *a == *b
}

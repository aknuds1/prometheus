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
	"fmt"
	"hash/maphash"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/metadata"
)

func TestNativeMetricMetadataValueCache(t *testing.T) {
	t.Run("keys and values own strings and admission needs two observations", func(t *testing.T) {
		cache := newNativeMetricMetadataValueCache()
		backing := "gaugesecondsdescription" + strings.Repeat("padding", 100_000)
		input := metadata.Metadata{Type: model.MetricType(backing[:5]), Unit: backing[5:12], Help: backing[12:23]}
		first := cache.resolve(input)
		require.Zero(t, cache.bytes.Load(), "first observation must not admit a value")
		second := cache.resolve(input)
		require.NotSame(t, first, second)
		require.Same(t, second, cache.resolve(input))
		require.Equal(t, int64(23), cache.bytes.Load())
		for _, owned := range []*metadata.Metadata{first, second} {
			require.Equal(t, input, *owned)
			require.NotSame(t, unsafe.StringData(string(input.Type)), unsafe.StringData(string(owned.Type)))
			require.NotSame(t, unsafe.StringData(input.Unit), unsafe.StringData(owned.Unit))
			require.NotSame(t, unsafe.StringData(input.Help), unsafe.StringData(owned.Help))
		}
		shard := cache.shards[maphash.Comparable(cache.seed, input)%nativeMetadataValueShards].Load()
		require.Len(t, shard.values, 1)
		for key, value := range shard.values {
			require.Same(t, second, value)
			require.Same(t, unsafe.StringData(string(key.Type)), unsafe.StringData(string(value.Type)))
			require.Same(t, unsafe.StringData(key.Unit), unsafe.StringData(value.Unit))
			require.Same(t, unsafe.StringData(key.Help), unsafe.StringData(value.Help))
		}
	})

	t.Run("FIFO limits entries and never mutates evicted values", func(t *testing.T) {
		cache := newNativeMetricMetadataValueCache()
		var values []metadata.Metadata
		for i := 0; len(values) <= nativeMetadataValuesPerShard; i++ {
			m := metadata.Metadata{Help: fmt.Sprintf("value-%d", i)}
			if maphash.Comparable(cache.seed, m)%nativeMetadataValueShards == 0 {
				values = append(values, m)
			}
		}
		var retained *metadata.Metadata
		for i, m := range values {
			cache.resolve(m)
			value := cache.resolve(m)
			if i == 0 {
				retained = value
			}
		}
		shard := cache.shards[0].Load()
		require.Len(t, shard.values, nativeMetadataValuesPerShard)
		require.Equal(t, nativeMetadataValuesPerShard, shard.count)
		require.NotContains(t, shard.values, values[0])
		require.Contains(t, shard.values, values[1])
		var bytes int64
		for key := range shard.values {
			bytes += int64(len(key.Help))
		}
		require.Equal(t, bytes, cache.bytes.Load())
		runtime.GC()
		require.Equal(t, values[0], *retained)
	})

	t.Run("fingerprint false positives cannot substitute values", func(t *testing.T) {
		cache := newNativeMetricMetadataValueCache()
		first := metadata.Metadata{Help: "first"}
		cache.resolve(first)
		cached := cache.resolve(first)
		index := maphash.Comparable(cache.seed, first) % nativeMetadataValueShards
		var other metadata.Metadata
		for i := 0; ; i++ {
			other = metadata.Metadata{Help: fmt.Sprintf("other-%d", i)}
			if maphash.Comparable(cache.seed, other)%nativeMetadataValueShards == index {
				break
			}
		}
		shard := cache.shards[index].Load()
		// Model a recent unrelated miss having the other value's fingerprint.
		shard.recent[0] = maphash.Comparable(cache.seed, other)
		shard.recentCount = 1
		resolved := cache.resolve(other)
		require.Equal(t, other, *resolved)
		require.NotSame(t, cached, resolved)
		require.Same(t, cached, cache.resolve(first))
		require.Same(t, resolved, cache.resolve(other))
	})

	t.Run("large values share but oversized values bypass", func(t *testing.T) {
		cache := newNativeMetricMetadataValueCache()
		for i := range 4 {
			m := metadata.Metadata{Help: strings.Repeat(string(rune('a'+i)), 256<<10)}
			cache.resolve(m)
			owned := cache.resolve(m)
			require.Same(t, owned, cache.resolve(m))
		}
		require.Equal(t, int64(1<<20), cache.bytes.Load())
		for _, m := range []metadata.Metadata{
			{Help: strings.Repeat("x", nativeMetadataSharedStringBytes+1)},
			{Type: model.MetricType(strings.Repeat("x", nativeMetadataSharedStringBytes+1))},
			{Type: "x", Unit: strings.Repeat("x", nativeMetadataSharedStringBytes)},
		} {
			owned := cache.resolve(m)
			require.Equal(t, m, *owned)
			require.NotSame(t, owned, cache.resolve(m))
			require.Equal(t, int64(1<<20), cache.bytes.Load())
		}
	})

	t.Run("concurrent reservations obey the Head-wide budget", func(t *testing.T) {
		cache := newNativeMetricMetadataValueCache()
		require.Zero(t, uintptr(unsafe.Pointer(&cache.bytes))%8)
		var workers sync.WaitGroup
		for worker := range 16 {
			workers.Go(func() {
				for i := range 64 {
					m := metadata.Metadata{Help: fmt.Sprintf("%d/%d/", worker, i) + strings.Repeat("x", 64<<10)}
					cache.resolve(m)
					owned := cache.resolve(m)
					if *owned != m {
						t.Error("cache changed the value")
						return
					}
					if used := cache.bytes.Load(); used < 0 || used > nativeMetadataSharedStringBytes {
						t.Errorf("cache payload out of bounds: %d", used)
						return
					}
				}
			})
		}
		workers.Wait()
		var bytes int64
		for i := range cache.shards {
			if shard := cache.shards[i].Load(); shard != nil {
				require.Len(t, shard.values, shard.count)
				require.LessOrEqual(t, shard.count, nativeMetadataValuesPerShard)
				for key, value := range shard.values {
					require.Equal(t, key, *value)
					bytes += int64(len(key.Type) + len(key.Unit) + len(key.Help))
				}
			}
		}
		require.Equal(t, bytes, cache.bytes.Load())
		require.LessOrEqual(t, bytes, int64(nativeMetadataSharedStringBytes))
	})

	t.Run("reset drops cache ownership and pooled appenders use the new cache", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		m := metadata.Metadata{Help: "retained result"}
		store.values.resolve(m)
		retained := store.values.resolve(m)
		old := store.values
		appender := store.getAppender()
		store.putAppender(appender)
		require.Nil(t, appender.valueCache)
		store.reset()
		require.NotSame(t, old, store.values)
		require.Zero(t, store.values.bytes.Load())
		appender = store.getAppender()
		require.Same(t, store.values, appender.valueCache)
		store.putAppender(appender)
		runtime.GC()
		require.Equal(t, m, *retained)
	})
}

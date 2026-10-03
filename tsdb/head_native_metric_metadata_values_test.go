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
		first := cache.resolve(input, false)
		require.Zero(t, cache.bytes.Load(), "first observation must not admit a value")
		second := cache.resolve(input, false)
		require.NotSame(t, first, second)
		require.Same(t, second, cache.resolve(input, false))
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

	t.Run("an unowned value allocates only its copy", func(t *testing.T) {
		// Measured through resolve, whose results escape as in ingestion.
		m := metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: "description"}
		var sink *metadata.Metadata
		clone := testing.AllocsPerRun(100, func() { sink = cloneNativeMetricMetadata(m) })
		runtime.KeepAlive(sink)

		cache := newNativeMetricMetadataValueCache()
		oversized := metadata.Metadata{Help: strings.Repeat("x", nativeMetadataSharedStringBytes+1)}
		require.Equal(t, testing.AllocsPerRun(100, func() { cloneNativeMetricMetadata(oversized) }),
			testing.AllocsPerRun(100, func() { cache.resolve(oversized, false) }))
		require.Equal(t, 1.0, testing.AllocsPerRun(100, func() { cache.resolve(oversized, true) }))

		// First sightings in shards that already exist cost their value and
		// the same overhead whoever owns the strings.
		for i := 0; ; i++ {
			cache.resolve(metadata.Metadata{Help: fmt.Sprintf("warm-%d", i)}, false)
			warm := true
			for j := range cache.shards {
				warm = warm && cache.shards[j].Load() != nil
			}
			if warm {
				break
			}
		}
		const runs = 100
		sighting := func(owned bool, prefix string) float64 {
			values := make([]metadata.Metadata, runs+1)
			for i := range values {
				values[i] = metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: fmt.Sprintf("%s-%d", prefix, i)}
			}
			next := 0
			return testing.AllocsPerRun(runs, func() {
				cache.resolve(values[next], owned)
				next++
			})
		}
		require.Equal(t, clone-1, sighting(false, "unowned")-sighting(true, "owned"))
	})

	t.Run("caller-owned strings are kept and unowned strings never are", func(t *testing.T) {
		for _, ownedFirst := range []bool{true, false} {
			cache := newNativeMetricMetadataValueCache()
			backing := "gaugesecondsdescription" + strings.Repeat("padding", 100_000)
			unowned := metadata.Metadata{Type: model.MetricType(backing[:5]), Unit: backing[5:12], Help: backing[12:23]}
			owned := metadata.Metadata{Type: model.MetricTypeGauge, Unit: strings.Clone("seconds"), Help: strings.Clone("description")}
			require.Equal(t, owned, unowned)
			var first, admitted *metadata.Metadata
			if ownedFirst {
				first = cache.resolve(owned, true)
				require.Same(t, unsafe.StringData(owned.Help), unsafe.StringData(first.Help), "owned strings must not be copied")
				admitted = cache.resolve(unowned, false)
				require.NotSame(t, unsafe.StringData(owned.Help), unsafe.StringData(admitted.Help), "admission copies the unowned observation")
			} else {
				first = cache.resolve(unowned, false)
				admitted = cache.resolve(owned, true)
				require.Same(t, unsafe.StringData(owned.Help), unsafe.StringData(admitted.Help), "admission keeps the owned observation's strings")
			}
			require.NotSame(t, first, admitted)
			require.Same(t, admitted, cache.resolve(unowned, false), "admission shares the value")
			require.Same(t, admitted, cache.resolve(owned, true))
			require.Equal(t, int64(23), cache.bytes.Load())
			for _, m := range []*metadata.Metadata{first, admitted} {
				require.Equal(t, owned, *m)
				for _, field := range [][2]string{{string(m.Type), string(unowned.Type)}, {m.Unit, unowned.Unit}, {m.Help, unowned.Help}} {
					require.NotSame(t, unsafe.StringData(field[0]), unsafe.StringData(field[1]), "unowned strings must never be kept")
				}
			}
		}
		cache := newNativeMetricMetadataValueCache()
		oversized := metadata.Metadata{Help: strings.Repeat("x", nativeMetadataSharedStringBytes+1)}
		require.Same(t, unsafe.StringData(oversized.Help), unsafe.StringData(cache.resolve(oversized, true).Help))
		require.NotSame(t, unsafe.StringData(oversized.Help), unsafe.StringData(cache.resolve(oversized, false).Help))
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
			cache.resolve(m, false)
			value := cache.resolve(m, false)
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
		cache.resolve(first, false)
		cached := cache.resolve(first, false)
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
		resolved := cache.resolve(other, false)
		require.Equal(t, other, *resolved)
		require.NotSame(t, cached, resolved)
		require.Same(t, cached, cache.resolve(first, false))
		require.Same(t, resolved, cache.resolve(other, false))
	})

	t.Run("large values share but oversized values bypass", func(t *testing.T) {
		cache := newNativeMetricMetadataValueCache()
		for i := range 4 {
			m := metadata.Metadata{Help: strings.Repeat(string(rune('a'+i)), 256<<10)}
			cache.resolve(m, false)
			owned := cache.resolve(m, false)
			require.Same(t, owned, cache.resolve(m, false))
		}
		require.Equal(t, int64(1<<20), cache.bytes.Load())
		for _, m := range []metadata.Metadata{
			{Help: strings.Repeat("x", nativeMetadataSharedStringBytes+1)},
			{Type: model.MetricType(strings.Repeat("x", nativeMetadataSharedStringBytes+1))},
			{Type: "x", Unit: strings.Repeat("x", nativeMetadataSharedStringBytes)},
		} {
			owned := cache.resolve(m, false)
			require.Equal(t, m, *owned)
			require.NotSame(t, owned, cache.resolve(m, false))
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
					cache.resolve(m, false)
					owned := cache.resolve(m, false)
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
		store.values.resolve(m, false)
		retained := store.values.resolve(m, false)
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

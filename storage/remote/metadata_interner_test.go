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

package remote

import (
	"strconv"
	"strings"
	"sync"
	"testing"

	remoteapi "github.com/prometheus/client_golang/exp/api/remote"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/record"
)

func TestMetadataInterner(t *testing.T) {
	value := func(i int) metadata.Metadata {
		return metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: "help " + strconv.Itoa(i)}
	}
	t.Run("equal values share a pointer", func(t *testing.T) {
		i := newMetadataInterner(4, 1<<10)
		// The second sighting admits the value.
		i.intern(value(1))
		first := i.intern(value(1))
		require.Equal(t, value(1), *first)
		require.Same(t, first, i.intern(value(1)))
		require.NotSame(t, first, i.intern(value(2)))
	})
	t.Run("generations are bounded by entries", func(t *testing.T) {
		i := newMetadataInterner(4, 1<<10)
		i.intern(value(0))
		i.intern(value(1))
		kept, evicted := i.intern(value(0)), i.intern(value(1))
		for n := 2; n < 100; n++ {
			// The second sighting admits each value.
			i.intern(value(n))
			i.intern(value(n))
			// A value that keeps being seen survives every rotation.
			require.Same(t, kept, i.intern(value(0)))
			require.LessOrEqual(t, len(i.current), 4)
			require.LessOrEqual(t, len(i.older), 4)
		}
		require.NotSame(t, evicted, i.intern(value(1)), "evicted values are interned again")
	})
	t.Run("generations are bounded by string bytes", func(t *testing.T) {
		i := newMetadataInterner(1<<10, 64)
		large := metadata.Metadata{Help: strings.Repeat("x", 40)}
		i.intern(large)
		first := i.intern(large)
		require.Same(t, first, i.intern(large))
		other := metadata.Metadata{Help: strings.Repeat("y", 40)}
		i.intern(other)
		i.intern(other)
		require.Len(t, i.current, 1)
		require.Same(t, first, i.older[large])
		require.LessOrEqual(t, i.bytes, 64)
		oversized := metadata.Metadata{Help: strings.Repeat("z", 65)}
		unshared := i.intern(oversized)
		require.Equal(t, oversized, *unshared)
		require.NotSame(t, unshared, i.intern(oversized))
		require.NotContains(t, i.current, oversized)
	})
	t.Run("hits do not allocate", func(t *testing.T) {
		i := newMetadataInterner(4, 1<<10)
		m := value(1)
		i.intern(m)
		want := i.intern(m)
		// Call through a method value, as queue managers' callbacks do.
		intern := i.intern
		var got *metadata.Metadata
		require.Zero(t, testing.AllocsPerRun(100, func() { got = intern(m) }))
		require.Same(t, want, got)
		// Promotion from the older generation keeps the value, and later hits
		// do not allocate. Promotion itself may grow the new generation's map.
		for n := 2; n <= 5; n++ {
			i.intern(value(n))
			i.intern(value(n))
		}
		require.NotContains(t, i.current, m)
		require.Contains(t, i.older, m)
		require.Same(t, want, intern(m))
		require.Zero(t, testing.AllocsPerRun(100, func() { got = intern(m) }))
		require.Same(t, want, got)
	})
	t.Run("concurrent callers", func(t *testing.T) {
		i := newMetadataInterner(8, 1<<10)
		var wg sync.WaitGroup
		for w := range 4 {
			wg.Go(func() {
				for n := range 1000 {
					require.Equal(t, value(n%16+w), *i.intern(value(n%16 + w)))
				}
			})
		}
		wg.Wait()
	})
	t.Run("queues share stored metadata from its second sighting", func(t *testing.T) {
		var queues []*QueueManager
		for range 3 {
			queues = append(queues, newTestQueueManager(t, testDefaultQueueConfig(), config.DefaultMetadataConfig, defaultFlushDeadline, NewNopWriteClient(), remoteapi.WriteV2MessageType))
		}
		help := "shared " + t.Name()
		for _, qm := range queues {
			qm.StoreMetadata([]record.RefMetadata{{Ref: 1, Type: record.GetMetricType(model.MetricTypeGauge), Unit: "bytes", Help: strings.Clone(help)}})
		}
		for _, qm := range queues {
			require.Equal(t, metadata.Metadata{Type: model.MetricTypeGauge, Unit: "bytes", Help: help}, *qm.seriesMetadata[1])
		}
		// The ledger keeps no values, so the first sighting stays unshared.
		require.NotSame(t, queues[0].seriesMetadata[1], queues[1].seriesMetadata[1])
		require.Same(t, queues[1].seriesMetadata[1], queues[2].seriesMetadata[1])
	})
	t.Run("a second sighting admits a new value", func(t *testing.T) {
		i := newMetadataInterner(4, 1<<10)
		first := i.intern(value(1))
		require.NotContains(t, i.current, value(1), "a value seen once is not admitted")
		second := i.intern(value(1))
		require.NotSame(t, first, second)
		require.Equal(t, value(1), *second)
		require.Same(t, second, i.current[value(1)])
	})
	t.Run("values that alternate in one set are admitted", func(t *testing.T) {
		i := newLedgerMetadataInterner(8, 1<<10, 1)
		i.intern(value(1))
		i.intern(value(2))
		require.Same(t, i.intern(value(1)), i.current[value(1)])
		require.Same(t, i.intern(value(2)), i.current[value(2)])
	})
	t.Run("a full set delays admission", func(t *testing.T) {
		i := newLedgerMetadataInterner(8, 1<<10, 1)
		for n := 1; n <= 5; n++ {
			i.intern(value(n))
		}
		// The single set replaced value 1, its oldest slot, so it starts over.
		i.intern(value(1))
		require.NotContains(t, i.current, value(1))
		admitted := i.intern(value(1))
		require.Same(t, admitted, i.current[value(1)])
	})
	t.Run("a fingerprint collision admits early but never returns a different value", func(t *testing.T) {
		i := newLedgerMetadataInterner(4, 1<<10, 1)
		i.fingerprint = func(metadata.Metadata) uint64 { return 42 }
		i.intern(value(1))
		got := i.intern(value(2))
		require.Equal(t, value(2), *got)
		require.Same(t, got, i.current[value(2)])
	})
}

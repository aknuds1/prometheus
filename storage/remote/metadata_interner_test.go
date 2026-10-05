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
	"unsafe"

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
		first := i.intern(value(1))
		require.Equal(t, value(1), *first)
		require.Same(t, first, i.intern(value(1)))
		require.NotSame(t, first, i.intern(value(2)))
	})
	t.Run("generations are bounded by entries", func(t *testing.T) {
		i := newMetadataInterner(4, 1<<10)
		kept, evicted := i.intern(value(0)), i.intern(value(1))
		require.Same(t, kept, i.intern(value(0)))
		require.Same(t, evicted, i.intern(value(1)))
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
	t.Run("a second sighting admits the first sighting's value", func(t *testing.T) {
		i := newMetadataInterner(4, 1<<10)
		first := i.intern(value(1))
		require.NotContains(t, i.current, value(1), "a value seen once is not admitted")
		require.Same(t, first, i.intern(value(1)))
		require.Same(t, first, i.current[value(1)])
		require.Zero(t, i.ledgerBytes, "admission releases the ledger's charge")
	})
	t.Run("values that alternate in one set are admitted", func(t *testing.T) {
		i := newLedgerMetadataInterner(8, 1<<10, 1, 1<<10)
		a, b := i.intern(value(1)), i.intern(value(2))
		require.Same(t, a, i.intern(value(1)))
		require.Same(t, b, i.intern(value(2)))
	})
	t.Run("a full set delays admission", func(t *testing.T) {
		i := newLedgerMetadataInterner(8, 1<<10, 1, 1<<10)
		first := i.intern(value(1))
		for n := 2; n <= 5; n++ {
			i.intern(value(n))
		}
		// The single set replaced value 1, its oldest slot, so it starts over.
		second := i.intern(value(1))
		require.NotSame(t, first, second)
		require.Equal(t, value(1), *second)
		require.NotContains(t, i.current, value(1))
		require.Same(t, second, i.intern(value(1)))
		require.Contains(t, i.current, value(1))
	})
	t.Run("a fingerprint collision never returns a different value", func(t *testing.T) {
		i := newLedgerMetadataInterner(4, 1<<10, 1, 1<<10)
		i.fingerprint = func(metadata.Metadata) uint64 { return 42 }
		i.intern(value(1))
		got := i.intern(value(2))
		require.Equal(t, value(2), *got)
		require.NotContains(t, i.current, value(2), "an unequal value in the slot is a miss")
		require.Same(t, got, i.intern(value(2)))
		require.Contains(t, i.current, value(2))
	})
	t.Run("values over the ledger's byte cap keep only their fingerprint", func(t *testing.T) {
		i := newLedgerMetadataInterner(4, 1<<10, 1, 8)
		first := i.intern(value(1))
		require.Zero(t, i.ledgerBytes)
		second := i.intern(value(1))
		require.NotSame(t, first, second, "the second sighting admits a new value")
		require.Equal(t, value(1), *second)
		require.Same(t, second, i.current[value(1)])
	})
	t.Run("the ledger charges exactly the bytes it pins", func(t *testing.T) {
		i := newLedgerMetadataInterner(4, 1<<10, 1, 64)
		for n := range 1000 {
			i.intern(value(n * 7919 % 37))
			pinned := 0
			for _, slot := range i.ledger {
				if slot.value != nil {
					pinned += len(slot.value.Type) + len(slot.value.Unit) + len(slot.value.Help)
				}
			}
			require.Equal(t, pinned, i.ledgerBytes)
			require.LessOrEqual(t, i.ledgerBytes, 64)
		}
	})
	t.Run("borrowed values are owned and never kept", func(t *testing.T) {
		i := newMetadataInterner(4, 1<<10)
		var buffers [][]byte
		borrow := func(n int) metadata.Metadata {
			v := value(n)
			buf := []byte(v.Unit + v.Help)
			buffers = append(buffers, buf)
			return metadata.Metadata{Type: v.Type, Unit: unsafe.String(&buf[0], len(v.Unit)), Help: unsafe.String(&buf[len(v.Unit)], len(v.Help))}
		}
		requireOwned := func(m metadata.Metadata) {
			for _, s := range []string{string(m.Type), m.Unit, m.Help} {
				p := uintptr(unsafe.Pointer(unsafe.StringData(s)))
				for _, buf := range buffers {
					start := uintptr(unsafe.Pointer(&buf[0]))
					require.False(t, p >= start && p < start+uintptr(len(buf)), "a borrowed string is kept")
				}
			}
		}
		check := func(got *metadata.Metadata, n int) {
			t.Helper()
			for _, buf := range buffers {
				for k := range buf {
					buf[k] = 'z'
				}
			}
			require.Equal(t, value(n), *got)
			requireOwned(*got)
			for _, generation := range []map[metadata.Metadata]*metadata.Metadata{i.current, i.older} {
				for key, v := range generation {
					requireOwned(key)
					requireOwned(*v)
				}
			}
			for _, slot := range i.ledger {
				if slot.value != nil {
					requireOwned(*slot.value)
				}
			}
		}
		miss := i.internBorrowed(borrow(1))
		check(miss, 1)
		require.NotZero(t, i.ledgerBytes, "the ledger holds the first sighting")
		hit := i.internBorrowed(borrow(1))
		check(hit, 1)
		require.Same(t, miss, hit)
		for n := 2; n <= 5; n++ {
			check(i.internBorrowed(borrow(n)), n)
			check(i.internBorrowed(borrow(n)), n)
		}
		require.NotContains(t, i.current, value(1))
		promoted := i.internBorrowed(borrow(1))
		check(promoted, 1)
		require.Same(t, miss, promoted)
		m := borrow(1)
		require.Zero(t, testing.AllocsPerRun(100, func() { promoted = i.internBorrowed(m) }))

		oversized := newMetadataInterner(4, 8)
		check(oversized.internBorrowed(borrow(1)), 1)
	})
	t.Run("borrowed misses and bypasses copy each value once", func(t *testing.T) {
		// Inputs and map capacity are prepared outside the counts, so that only
		// the interner's own allocations are counted.
		inputs := make([]metadata.Metadata, 101)
		for n := range inputs {
			inputs[n] = value(1000 + n)
		}
		i := newMetadataInterner(1<<15, 4<<20)
		i.current = make(map[metadata.Metadata]*metadata.Metadata, 1024)
		n := 0
		// A miss allocates the value and one payload for its unit and help.
		require.Equal(t, 2.0, testing.AllocsPerRun(100, func() {
			i.internBorrowed(inputs[n])
			n++
		}))
		bypass := newMetadataInterner(0, -1)
		m := value(1)
		require.Equal(t, 2.0, testing.AllocsPerRun(100, func() { bypass.internBorrowed(m) }))
		require.Equal(t, 1.0, testing.AllocsPerRun(100, func() { bypass.intern(m) }), "a copying bypass allocates only the value")
	})
	t.Run("owned values keep no empty substring's backing", func(t *testing.T) {
		backing := strings.Repeat("padding", 100_000)
		empty := backing[len(backing)/2 : len(backing)/2]
		require.NotNil(t, unsafe.StringData(empty), "the empty substring refers to the backing")
		for _, m := range []metadata.Metadata{
			{Type: model.MetricType(empty), Unit: empty, Help: empty},
			{Type: model.MetricTypeCounter, Unit: empty, Help: empty},
			{Type: "custom", Unit: empty, Help: "help"},
			{Type: model.MetricTypeCounter, Unit: empty, Help: "help"},
			{Type: model.MetricType(empty), Unit: "seconds", Help: "help"},
		} {
			owned := ownMetadata(m)
			require.Equal(t, m, *owned)
			for _, field := range []string{string(owned.Type), owned.Unit, owned.Help} {
				if field == "" {
					require.Nil(t, unsafe.StringData(field), "%+v", m)
				}
			}
		}
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
	t.Run("queues share stored metadata", func(t *testing.T) {
		var queues []*QueueManager
		for range 2 {
			queues = append(queues, newTestQueueManager(t, testDefaultQueueConfig(), config.DefaultMetadataConfig, defaultFlushDeadline, NewNopWriteClient(), remoteapi.WriteV2MessageType))
		}
		help := "shared " + t.Name()
		for _, qm := range queues {
			qm.StoreMetadata([]record.RefMetadata{{Ref: 1, Type: record.GetMetricType(model.MetricTypeGauge), Unit: "bytes", Help: strings.Clone(help)}})
		}
		require.Equal(t, metadata.Metadata{Type: model.MetricTypeGauge, Unit: "bytes", Help: help}, *queues[0].seriesMetadata[1])
		require.Same(t, queues[0].seriesMetadata[1], queues[1].seriesMetadata[1])
	})
	t.Run("callers share a value seen once by each while its slot survives", func(t *testing.T) {
		i := newLedgerMetadataInterner(8, 1<<10, 1, 1<<10)
		first := i.intern(value(1))
		require.Same(t, first, i.intern(value(1)))
		lost := i.intern(value(2))
		for n := 3; n <= 6; n++ {
			i.intern(value(n))
		}
		// Value 6 replaced value 2's slot, so a later caller gets an equal copy.
		require.NotSame(t, lost, i.intern(value(2)))
	})
}

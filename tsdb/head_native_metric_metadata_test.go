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
	"context"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unique"
	"unsafe"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/util/compression"
)

func makeNativeMetricMetadataPoint(timestamp int64, m metadata.Metadata) nativeMetricMetadataPoint {
	if m.Type == "" {
		m.Type = model.MetricTypeUnknown
	}
	return nativeMetricMetadataPoint{
		effectiveFrom: timestamp,
		metadata:      unique.Make(m),
	}
}

// nativeMetadataTxn returns the appender's open metadata transaction, or nil if
// it never needed one. The first appender on a fresh Head is wrapped.
func nativeMetadataTxn(app storage.AppenderV2) *nativeMetricMetadataAppender {
	if init, ok := app.(*initAppenderV2); ok {
		if init.app == nil {
			return nil
		}
		app = init.app
	}
	return app.(*headAppenderV2).nativeMetricMetadata
}

// seriesForTest reuses the series that owns committed history, or constructs a
// stand-in for a new reference. Uncommitted observations are grouped by reference.
func (s *nativeMetricMetadataStore) seriesForTest(ref chunks.HeadSeriesRef) *memSeries {
	if series := s.indexedSeries(ref); series != nil {
		return series
	}
	return &memSeries{ref: ref}
}

func legacyMetadataForTest(s *memSeries) *metadata.Metadata {
	s.Lock()
	defer s.Unlock()
	return s.legacyMetadataLocked()
}

func nativeMetadataForTest(s *memSeries) *nativeSeriesMetadata {
	s.Lock()
	defer s.Unlock()
	native := s.nativeMetadataLocked()
	if native == nil {
		return nil
	}
	nativeCopy := &nativeSeriesMetadata{
		metadata: native.metadata, effectiveFrom: native.effectiveFrom,
		handle: native.handle, older: slices.Clone(native.older),
	}
	nativeCopy.flags.Store(native.flags.Load())
	return nativeCopy
}

// snapshot copies ref's series-owned history while holding its series lock.
// It reports whether the history exists, leaving snapshot unchanged on a miss.
func (s *nativeMetricMetadataStore) snapshot(ref chunks.HeadSeriesRef, snapshot *nativeMetricMetadataSnapshot) bool {
	series := s.indexedSeries(ref)
	if series == nil {
		return false
	}
	series.Lock()
	native := series.nativeMetadataLocked()
	snapshot.count = copy(snapshot.points[:], native.older)
	snapshot.points[snapshot.count] = nativeMetricMetadataPoint{effectiveFrom: native.effectiveFrom, metadata: native.handle}
	snapshot.count++
	snapshot.truncated = native.flags.Load()&nativeMetadataTruncated != 0
	series.Unlock()
	return true
}

func (s *nativeMetricMetadataStore) get(ref chunks.HeadSeriesRef) ([]NativeMetricMetadataVersion, bool, bool) {
	var snapshot nativeMetricMetadataSnapshot
	ok := s.snapshot(ref, &snapshot)
	if !ok {
		return nil, false, false
	}
	return snapshot.expand(), snapshot.truncated, true
}

// commitNativeMetricMetadata applies observations for ref as one transaction
// through the same path ingestion uses.
func commitNativeMetricMetadata(store *nativeMetricMetadataStore, ref chunks.HeadSeriesRef, observations ...nativeMetricMetadataPoint) {
	appender := store.getAppender()
	for _, observation := range observations {
		appender.observe(store.seriesForTest(ref), observation.effectiveFrom, observation.metadata.Value())
	}
	store.commitAppender(appender)
	store.putAppender(appender)
}

func TestNativeMetricMetadataStore(t *testing.T) {
	t.Run("atomic counter alignment", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		for _, tc := range []struct {
			name    string
			pointer unsafe.Pointer
		}{
			{name: "series", pointer: unsafe.Pointer(&store.series)},
			{name: "versions", pointer: unsafe.Pointer(&store.versions)},
			{name: "evictions", pointer: unsafe.Pointer(&store.evictions)},
			{name: "directory generation", pointer: unsafe.Pointer(&store.directoryGeneration)},
		} {
			require.Zero(t, uintptr(tc.pointer)%8, "%s counter must be 64-bit aligned", tc.name)
		}
	})

	t.Run("presence uses full references and resets without mutating retired state", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		refs := []chunks.HeadSeriesRef{1, 1 + (1 << 32), 1 + (2 << 32)}
		for _, ref := range refs {
			for version := range maxNativeMetricMetadataVersions + 1 {
				commitNativeMetricMetadata(store, ref, makeNativeMetricMetadataPoint(int64(version), metadata.Metadata{Help: strconv.Itoa(version)}))
			}
		}
		require.Equal(t, int64(len(refs)), store.series.Load())
		require.Equal(t, uint64(len(refs)), store.evictions.Load())
		retired := nativeMetadataForTest(store.indexedSeries(refs[0]))
		store.delete(map[storage.SeriesRef]struct{}{storage.SeriesRef(refs[0]): {}})
		store.delete(map[storage.SeriesRef]struct{}{storage.SeriesRef(refs[0]): {}})
		require.False(t, store.has(refs[0]))
		require.True(t, store.has(refs[1]))
		require.Equal(t, int64(2*maxNativeMetricMetadataVersions), store.versions.Load())
		store.reset()
		for _, ref := range refs {
			require.False(t, store.has(ref))
		}
		require.Zero(t, store.series.Load())
		require.Zero(t, store.versions.Load())
		require.Equal(t, uint64(len(refs)), store.evictions.Load())
		require.Len(t, retired.older, maxNativeMetricMetadataVersions-1)
		require.Equal(t, "5", retired.metadata.Help)
	})

	t.Run("concurrent first publication has one owner and one count", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		series := store.seriesForTest(1)
		start := make(chan struct{})
		var workers sync.WaitGroup
		for i := range 16 {
			workers.Go(func() {
				appender := store.getAppender()
				appender.observe(series, 100, metadata.Metadata{Help: strconv.Itoa(i)})
				<-start
				store.commitAppender(appender)
				store.putAppender(appender)
			})
		}
		close(start)
		workers.Wait()
		require.Same(t, series, store.indexedSeries(1))
		require.Equal(t, int64(1), store.series.Load())
		require.Equal(t, int64(1), store.versions.Load())
		require.Zero(t, store.evictions.Load())
		current := nativeMetadataForTest(series)
		require.Equal(t, current.handle.Value(), *current.metadata)
		require.Nil(t, current.older)
	})

	t.Run("merge matches an independent bounded history model", func(t *testing.T) {
		random := rand.New(rand.NewPCG(19, 73))
		for trial := range 32 {
			store := newNativeMetricMetadataStore()
			var want []NativeMetricMetadataVersion
			var evictions uint64
			for round := range 64 {
				byTime := make(map[int64]metadata.Metadata)
				for _, point := range want {
					byTime[point.EffectiveFrom] = point.Metadata
				}
				points := make([]nativeMetricMetadataPoint, 1+random.IntN(12))
				for i := range points {
					timestamp := int64(random.IntN(512))
					if round%3 == 0 {
						timestamp = int64(1024 + round*20 + i)
					}
					points[i] = makeNativeMetricMetadataPoint(timestamp, metadata.Metadata{Help: strconv.Itoa(random.IntN(4))})
					byTime[timestamp] = points[i].metadata.Value()
				}
				timestamps := make([]int64, 0, len(byTime))
				for timestamp := range byTime {
					timestamps = append(timestamps, timestamp)
				}
				slices.Sort(timestamps)
				want = nil
				for _, timestamp := range timestamps {
					m := byTime[timestamp]
					if len(want) == 0 || want[len(want)-1].Metadata != m {
						want = append(want, NativeMetricMetadataVersion{EffectiveFrom: timestamp, Metadata: m})
					}
				}
				if extra := len(want) - maxNativeMetricMetadataVersions; extra > 0 {
					evictions += uint64(extra)
					want = want[extra:]
				}
				commitNativeMetricMetadata(store, 1, points...)
				got, truncated, found := store.get(1)
				require.True(t, found)
				require.Equal(t, want, got, "trial %d round %d", trial, round)
				require.Equal(t, evictions > 0, truncated)
				require.Equal(t, evictions, store.evictions.Load())
				require.Equal(t, int64(len(want)), store.versions.Load())
				require.Equal(t, int64(1), store.series.Load())
				current := nativeMetadataForTest(store.indexedSeries(1))
				require.Equal(t, want[len(want)-1].Metadata, *current.metadata)
				require.Equal(t, want[len(want)-1].EffectiveFrom, current.effectiveFrom)
				if len(want) == 1 {
					require.Nil(t, current.older)
				}
			}
		}
	})

	t.Run("older storage grows lazily and is reused at the cap", func(t *testing.T) {
		series := &memSeries{}
		native := &nativeSeriesMetadata{}
		capacities := []int{0, 1, 2, 4, 4}
		for i, capacity := range capacities {
			point := makeNativeMetricMetadataPoint(int64(i), metadata.Metadata{Help: strconv.Itoa(i)})
			series.Lock()
			native.mergeLocked([]nativeMetricMetadataPoint{point})
			series.Unlock()
			require.Equal(t, capacity, cap(native.older))
		}
		point := makeNativeMetricMetadataPoint(10, metadata.Metadata{Help: "next"})
		previous := native.handle
		require.Zero(t, testing.AllocsPerRun(1000, func() {
			series.Lock()
			native.mergeLocked([]nativeMetricMetadataPoint{point})
			series.Unlock()
			point.effectiveFrom++
			point.metadata, previous = previous, point.metadata
		}))
		require.NotZero(t, native.flags.Load()&nativeMetadataTruncated)
	})
}

func TestNativeMetricMetadataAppender(t *testing.T) {
	t.Run("reference encoding", func(t *testing.T) {
		require.Equal(t, uintptr(4), unsafe.Sizeof(nativeMetricMetadataValueRef(0)))
		require.Equal(t, uintptr(4), unsafe.Sizeof(nativeMetricMetadataObservationRef(0)))
		appender := newNativeMetricMetadataAppender()
		store := newNativeMetricMetadataStore()
		for i := range maxNativeMetricMetadataValues {
			m := metadata.Metadata{Help: strconv.Itoa(i)}
			ref := appender.metadataReference(store.seriesForTest(1), m)
			require.Equal(t, nativeMetricMetadataValueRef(i+1), ref)
			require.Equal(t, m, appender.metadataValue(ref))
		}
		m := metadata.Metadata{Help: "direct"}
		appender.observe(store.seriesForTest(1), 100, m)
		require.Len(t, appender.observations, 1)
		require.Len(t, appender.directHandles, 1)
		ref := appender.observations[0].metadataRef
		require.Equal(t, nativeMetricMetadataDirectRefMask, ref)
		require.Equal(t, m, appender.metadataValue(ref))
		require.Equal(t, nativeMetricMetadataObservationRef(1), appender.lastObservation)
		require.Zero(t, appender.observations[0].next)
	})

	t.Run("reuses adjacent metadata across series", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		appender := store.getAppender()
		m := metadata.Metadata{Type: model.MetricTypeUnknown, Help: "shared"}

		appender.observe(store.seriesForTest(1), 100, m)
		metadataRef := appender.observations[0].metadataRef
		appender.observe(store.seriesForTest(2), 200, m)
		appender.observe(store.seriesForTest(2), 300, m)

		require.Len(t, appender.values, 1)
		require.Len(t, appender.observations, 3)
		require.Equal(t, metadataRef, appender.observations[1].metadataRef)
		require.Equal(t, metadataRef, appender.observations[2].metadataRef)
		require.Equal(t, nativeMetricMetadataObservationRef(3), appender.lastObservation)
		store.putAppender(appender)
	})

	t.Run("reuses an adjacent direct metadata reference", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		appender := store.getAppender()
		for i := range maxNativeMetricMetadataValues {
			appender.metadataReference(store.seriesForTest(chunks.HeadSeriesRef(i+1)), metadata.Metadata{Type: model.MetricTypeUnknown, Help: strconv.Itoa(i)})
		}
		m := metadata.Metadata{Type: model.MetricTypeUnknown, Help: "direct"}

		appender.observe(store.seriesForTest(1), 100, m)
		require.Len(t, appender.directHandles, 1)
		metadataRef := appender.observations[0].metadataRef
		appender.observe(store.seriesForTest(2), 200, m)

		require.Len(t, appender.directHandles, 1)
		require.Equal(t, metadataRef, appender.observations[1].metadataRef)
		store.putAppender(appender)
	})

	t.Run("defers interning and bounds raw values", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		appender := store.getAppender()
		m := metadata.Metadata{Type: model.MetricTypeUnknown, Help: "A"}

		first := appender.metadataReference(store.seriesForTest(1), m)
		require.Equal(t, first, appender.metadataReference(store.seriesForTest(1), m))
		require.Len(t, appender.values, 1)
		require.False(t, appender.values[0].resolved)
		require.Empty(t, appender.directHandles)
		require.Equal(t, unique.Make(m), appender.metadataHandle(first))
		require.True(t, appender.values[0].resolved)

		for i := 1; i <= maxNativeMetricMetadataValues; i++ {
			appender.metadataReference(store.seriesForTest(chunks.HeadSeriesRef(i+1)), metadata.Metadata{Type: model.MetricTypeUnknown, Help: strconv.Itoa(i)})
		}
		require.Len(t, appender.values, maxNativeMetricMetadataValues)
		require.Len(t, appender.directHandles, 1)
		store.putAppender(appender)
	})

	t.Run("reuses a committed handle after the raw value limit", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		appender := store.getAppender()
		ref := chunks.HeadSeriesRef(1)
		m := metadata.Metadata{Type: model.MetricTypeUnknown, Help: "stable"}
		point := makeNativeMetricMetadataPoint(100, m)
		commitNativeMetricMetadata(store, ref, point)
		for i := range maxNativeMetricMetadataValues {
			appender.metadataReference(store.seriesForTest(chunks.HeadSeriesRef(i+2)), metadata.Metadata{Type: model.MetricTypeUnknown, Help: strconv.Itoa(i)})
		}

		metadataRef := appender.metadataReference(store.seriesForTest(ref), m)
		require.Equal(t, nativeMetricMetadataDirectRefMask, metadataRef)
		require.Len(t, appender.directHandles, 1)
		require.Equal(t, point.metadata, appender.metadataHandle(metadataRef))

		changed := metadata.Metadata{Type: model.MetricTypeUnknown, Help: "changed"}
		metadataRef = appender.metadataReference(store.seriesForTest(ref), changed)
		require.Equal(t, nativeMetricMetadataDirectRefMask|nativeMetricMetadataValueRef(1), metadataRef)
		require.Len(t, appender.directHandles, 2)
		require.Equal(t, changed, appender.metadataValue(metadataRef))
		store.putAppender(appender)
	})

	t.Run("pool reset releases transaction state", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		appender := store.getAppender()
		m := metadata.Metadata{Type: model.MetricTypeUnknown, Help: "A"}
		appender.observe(store.seriesForTest(1), 100, m)
		appender.observe(store.seriesForTest(1+nativeMetricMetadataStripes), 100, m)
		require.NotZero(t, appender.multiSeriesStripes)
		appender.metadataHandle(appender.observations[0].metadataRef)
		for i := 1; i <= maxNativeMetricMetadataValues; i++ {
			appender.metadataReference(store.seriesForTest(chunks.HeadSeriesRef(i+1)), metadata.Metadata{Type: model.MetricTypeUnknown, Help: strconv.Itoa(i)})
		}
		require.NotEmpty(t, appender.directHandles)
		appender.points = append(appender.points, makeNativeMetricMetadataPoint(100, m), makeNativeMetricMetadataPoint(200, m))
		appender.points = appender.points[:1]
		store.putAppender(appender)

		// Inspect this exact object; sync.Pool need not return it again.
		require.Empty(t, appender.observations)
		require.Empty(t, appender.values)
		require.Empty(t, appender.valueRefs)
		require.Empty(t, appender.directHandles)
		require.Empty(t, appender.sorted)
		require.Empty(t, appender.points)
		for _, point := range appender.points[:cap(appender.points)] {
			require.Equal(t, nativeMetricMetadataPoint{}, point)
		}
		require.Empty(t, appender.groups)
		require.Empty(t, appender.touched)
		require.Zero(t, appender.stripeFirst)
		require.Zero(t, appender.stripeLast)
		require.Zero(t, appender.multiSeriesStripes)
		require.False(t, appender.haveLast)
	})

	t.Run("tracks observations in shared stripes", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		appender := store.getAppender()
		m := metadata.Metadata{Type: model.MetricTypeUnknown, Help: "A"}
		firstRef := chunks.HeadSeriesRef(1)
		secondRef := firstRef + nativeMetricMetadataStripes
		thirdRef := secondRef + nativeMetricMetadataStripes

		require.False(t, appender.mayHaveObservedSeries(firstRef))
		appender.observe(store.seriesForTest(firstRef), 100, m)
		require.True(t, appender.mayHaveObservedSeries(firstRef))
		require.False(t, appender.mayHaveObservedSeries(secondRef))

		appender.observe(store.seriesForTest(secondRef), 100, m)
		require.True(t, appender.mayHaveObservedSeries(secondRef))
		require.True(t, appender.mayHaveObservedSeries(thirdRef), "a stripe with multiple observed series must be conservative")
		store.putAppender(appender)
	})
}

func TestNativeMetricMetadataAppenderCommit(t *testing.T) {
	a := metadata.Metadata{Type: model.MetricTypeGauge, Help: "A"}
	b := metadata.Metadata{Type: model.MetricTypeCounter, Help: "B"}
	c := metadata.Metadata{Type: model.MetricTypeUnknown, Help: "C"}

	t.Run("accounts for growth collapse and eviction within one batch", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		first := chunks.HeadSeriesRef(1)
		second := first + nativeMetricMetadataStripes
		third := second + nativeMetricMetadataStripes
		commitNativeMetricMetadata(store, first,
			makeNativeMetricMetadataPoint(100, a), makeNativeMetricMetadataPoint(200, b),
			makeNativeMetricMetadataPoint(300, a), makeNativeMetricMetadataPoint(400, c))
		commitNativeMetricMetadata(store, third,
			makeNativeMetricMetadataPoint(0, a), makeNativeMetricMetadataPoint(10, b),
			makeNativeMetricMetadataPoint(20, a), makeNativeMetricMetadataPoint(30, b), makeNativeMetricMetadataPoint(40, a))
		require.Equal(t, int64(9), store.versions.Load())
		appender := store.getAppender()
		appender.observe(store.seriesForTest(first), 200, a)
		appender.observe(store.seriesForTest(second), 100, b)
		appender.observe(store.seriesForTest(third), 50, b)
		store.commitAppender(appender)
		store.putAppender(appender)
		require.Equal(t, int64(3), store.series.Load())
		require.Equal(t, int64(8), store.versions.Load())
		require.Equal(t, uint64(1), store.evictions.Load())
		store.delete(map[storage.SeriesRef]struct{}{storage.SeriesRef(first): {}, storage.SeriesRef(third): {}})
		require.Equal(t, int64(1), store.series.Load())
		require.Equal(t, int64(1), store.versions.Load())
		require.Equal(t, uint64(1), store.evictions.Load())
	})

	t.Run("does not intern a stable stripe", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		ref := chunks.HeadSeriesRef(1)
		commitNativeMetricMetadata(store, ref, makeNativeMetricMetadataPoint(100, a))
		appender := store.getAppender()
		appender.observe(store.seriesForTest(ref), 200, a)
		require.False(t, appender.values[0].resolved)
		store.commitAppender(appender)
		require.False(t, appender.values[0].resolved)

		versions, _, _ := store.get(ref)
		require.Equal(t, []NativeMetricMetadataVersion{{EffectiveFrom: 100, Metadata: a}}, versions)
		store.putAppender(appender)
	})

	t.Run("sorts observations and applies the last value at an equal timestamp", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		ref := chunks.HeadSeriesRef(1)
		commitNativeMetricMetadata(store, ref, makeNativeMetricMetadataPoint(100, a))
		appender := store.getAppender()
		appender.observe(store.seriesForTest(ref), 300, a)
		appender.observe(store.seriesForTest(ref), 150, b)
		appender.observe(store.seriesForTest(ref), 200, c)
		appender.observe(store.seriesForTest(ref), 200, b)
		store.commitAppender(appender)

		versions, truncated, ok := store.get(ref)
		require.True(t, ok)
		require.False(t, truncated)
		require.Equal(t, []NativeMetricMetadataVersion{
			{EffectiveFrom: 100, Metadata: a},
			{EffectiveFrom: 150, Metadata: b},
			{EffectiveFrom: 300, Metadata: a},
		}, versions)
		store.putAppender(appender)
	})

	t.Run("retains the time range of a consecutive value", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		ref := chunks.HeadSeriesRef(1)
		commitNativeMetricMetadata(store, ref,
			makeNativeMetricMetadataPoint(50, a),
			makeNativeMetricMetadataPoint(150, b),
		)
		appender := store.getAppender()
		appender.observe(store.seriesForTest(ref), 300, a)
		appender.observe(store.seriesForTest(ref), 100, a)
		appender.observe(store.seriesForTest(ref), 200, a)
		require.Len(t, appender.observations, 3)
		store.commitAppender(appender)

		versions, _, _ := store.get(ref)
		require.Equal(t, []NativeMetricMetadataVersion{
			{EffectiveFrom: 50, Metadata: a},
			{EffectiveFrom: 150, Metadata: b},
			{EffectiveFrom: 200, Metadata: a},
		}, versions)
		store.putAppender(appender)
	})

	t.Run("merges interleaved stable and changing series in one stripe", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		firstRef := chunks.HeadSeriesRef(1)
		secondRef := firstRef + nativeMetricMetadataStripes
		commitNativeMetricMetadata(store, firstRef, makeNativeMetricMetadataPoint(100, a))
		commitNativeMetricMetadata(store, secondRef, makeNativeMetricMetadataPoint(100, a))
		appender := store.getAppender()
		appender.observe(store.seriesForTest(firstRef), 200, a)
		appender.observe(store.seriesForTest(secondRef), 200, b)
		appender.observe(store.seriesForTest(firstRef), 250, a)
		store.commitAppender(appender)

		firstVersions, _, _ := store.get(firstRef)
		require.Equal(t, []NativeMetricMetadataVersion{{EffectiveFrom: 100, Metadata: a}}, firstVersions)
		secondVersions, _, _ := store.get(secondRef)
		require.Equal(t, []NativeMetricMetadataVersion{
			{EffectiveFrom: 100, Metadata: a},
			{EffectiveFrom: 200, Metadata: b},
		}, secondVersions)
		require.False(t, appender.values[0].resolved)
		require.True(t, appender.values[1].resolved)
		store.putAppender(appender)
	})

	t.Run("commits more than one bounded batch", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		const series = 2*maxNativeMetricMetadataBatch + 1
		appender := store.getAppender()
		for i := range series {
			ref := chunks.HeadSeriesRef(1 + i*nativeMetricMetadataStripes)
			commitNativeMetricMetadata(store, ref, makeNativeMetricMetadataPoint(100, a))
			appender.observe(store.seriesForTest(ref), 200, b)
		}
		store.commitAppender(appender)

		require.Equal(t, int64(series), store.series.Load())
		require.Equal(t, int64(2*series), store.versions.Load())
		for _, i := range []int{0, series - 1} {
			ref := chunks.HeadSeriesRef(1 + i*nativeMetricMetadataStripes)
			versions, _, _ := store.get(ref)
			require.Equal(t, []NativeMetricMetadataVersion{
				{EffectiveFrom: 100, Metadata: a},
				{EffectiveFrom: 200, Metadata: b},
			}, versions)
		}
		store.putAppender(appender)
	})

	t.Run("advances past a stable batch", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		appender := store.getAppender()
		for i := range maxNativeMetricMetadataBatch + 1 {
			ref := chunks.HeadSeriesRef(1 + i*nativeMetricMetadataStripes)
			commitNativeMetricMetadata(store, ref, makeNativeMetricMetadataPoint(100, a))
			m := a
			if i == maxNativeMetricMetadataBatch {
				m = b
			}
			appender.observe(store.seriesForTest(ref), 200, m)
		}
		store.commitAppender(appender)
		require.False(t, appender.values[0].resolved, "the stable batch needs no interning")
		require.True(t, appender.values[1].resolved)
		require.Equal(t, int64(maxNativeMetricMetadataBatch+2), store.versions.Load())
		lastRef := chunks.HeadSeriesRef(1 + maxNativeMetricMetadataBatch*nativeMetricMetadataStripes)
		versions, _, ok := store.get(lastRef)
		require.True(t, ok)
		require.Equal(t, []NativeMetricMetadataVersion{{EffectiveFrom: 100, Metadata: a}, {EffectiveFrom: 200, Metadata: b}}, versions)
		store.putAppender(appender)
	})

	t.Run("keeps a large observation group intact", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		appender := store.getAppender()
		const observations = maxNativeMetricMetadataBatch + 1
		for i := range observations {
			// Exercise equal-timestamp precedence across the observation boundary.
			m := a
			if i%2 != 0 {
				m = b
			}
			appender.observe(store.seriesForTest(1), 100, m)
		}
		store.commitAppender(appender)
		versions, truncated, ok := store.get(1)
		require.True(t, ok)
		require.False(t, truncated)
		require.Equal(t, []NativeMetricMetadataVersion{{EffectiveFrom: 100, Metadata: a}}, versions)
		require.Len(t, appender.groups, 1)
		require.Equal(t, observations, appender.groups[0].end-appender.groups[0].start)
		store.putAppender(appender)
	})

	t.Run("commits concurrent series sharing a presence stripe", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		const workers = 8
		for i := range workers {
			ref := chunks.HeadSeriesRef(1 + i*nativeMetricMetadataStripes)
			commitNativeMetricMetadata(store, ref, makeNativeMetricMetadataPoint(100, a))
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(workers)
		for i := range workers {
			go func() {
				defer wg.Done()
				ref := chunks.HeadSeriesRef(1 + i*nativeMetricMetadataStripes)
				appender := store.getAppender()
				appender.observe(store.seriesForTest(ref), 200, b)
				<-start
				store.commitAppender(appender)
				store.putAppender(appender)
			}()
		}
		close(start)
		wg.Wait()

		for i := range workers {
			ref := chunks.HeadSeriesRef(1 + i*nativeMetricMetadataStripes)
			versions, _, _ := store.get(ref)
			require.Equal(t, []NativeMetricMetadataVersion{
				{EffectiveFrom: 100, Metadata: a},
				{EffectiveFrom: 200, Metadata: b},
			}, versions)
		}
	})

	t.Run("older transaction cannot overwrite the newest value", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		ref := chunks.HeadSeriesRef(1)
		commitNativeMetricMetadata(store, ref, makeNativeMetricMetadataPoint(100, a))
		series := store.seriesForTest(ref)
		stale := store.getAppender()
		stale.observe(series, 150, c)
		commitNativeMetricMetadata(store, ref, makeNativeMetricMetadataPoint(200, b), makeNativeMetricMetadataPoint(300, a))
		current := nativeMetadataForTest(series).metadata
		store.commitAppender(stale)
		got := nativeMetadataForTest(series)
		require.Equal(t, int64(300), got.effectiveFrom)
		require.Same(t, current, got.metadata)
		require.Equal(t, a, *got.metadata)
		store.putAppender(stale)
	})
}

func TestNativeMetricMetadataStoreVersioning(t *testing.T) {
	ref := chunks.HeadSeriesRef(1)
	a := metadata.Metadata{Type: model.MetricTypeGauge, Help: "A"}
	b := metadata.Metadata{Type: model.MetricTypeCounter, Help: "B"}
	c := metadata.Metadata{Type: model.MetricTypeUnknown, Help: "C"}

	transactions := [][]nativeMetricMetadataPoint{
		{
			makeNativeMetricMetadataPoint(200, b),
			makeNativeMetricMetadataPoint(100, a),
			makeNativeMetricMetadataPoint(150, c),
			makeNativeMetricMetadataPoint(150, b),
		},
		{
			makeNativeMetricMetadataPoint(250, c),
			makeNativeMetricMetadataPoint(300, c),
		},
		{makeNativeMetricMetadataPoint(150, a)},
		{
			makeNativeMetricMetadataPoint(50, a),
			makeNativeMetricMetadataPoint(300, a),
		},
	}
	for _, tc := range []struct {
		name         string
		transactions [][]nativeMetricMetadataPoint
		want         []NativeMetricMetadataVersion
	}{
		{
			name:         "unordered input and later observation wins timestamp tie",
			transactions: transactions[:1],
			want: []NativeMetricMetadataVersion{
				{EffectiveFrom: 100, Metadata: a},
				{EffectiveFrom: 150, Metadata: b},
			},
		},
		{
			name:         "identical canonical metadata values coalesce",
			transactions: transactions[:2],
			want: []NativeMetricMetadataVersion{
				{EffectiveFrom: 100, Metadata: a},
				{EffectiveFrom: 150, Metadata: b},
				{EffectiveFrom: 250, Metadata: c},
			},
		},
		{
			name:         "later transaction wins timestamp tie",
			transactions: transactions[:3],
			want: []NativeMetricMetadataVersion{
				{EffectiveFrom: 100, Metadata: a},
				{EffectiveFrom: 250, Metadata: c},
			},
		},
		{
			name:         "equal incoming values preserve an intervening change",
			transactions: transactions,
			want: []NativeMetricMetadataVersion{
				{EffectiveFrom: 50, Metadata: a},
				{EffectiveFrom: 250, Metadata: c},
				{EffectiveFrom: 300, Metadata: a},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newNativeMetricMetadataStore()
			for _, observations := range tc.transactions {
				commitNativeMetricMetadata(store, ref, observations...)
			}
			versions, truncated, ok := store.get(ref)
			require.True(t, ok)
			require.False(t, truncated)
			require.Equal(t, tc.want, versions)
		})
	}
}

func TestNativeMetricMetadataStoreCapsVersions(t *testing.T) {
	store := newNativeMetricMetadataStore()
	ref := chunks.HeadSeriesRef(1)
	a := metadata.Metadata{Type: model.MetricTypeGauge, Help: "A"}
	b := metadata.Metadata{Type: model.MetricTypeCounter, Help: "B"}

	const observations = 4096
	points := make([]nativeMetricMetadataPoint, observations)
	for i := range points {
		points[i] = makeNativeMetricMetadataPoint(int64(i), a)
	}
	commitNativeMetricMetadata(store, ref, points...)

	series := store.indexedSeries(ref)
	backing := series.nativeMetadataLocked()
	require.Nil(t, backing.older, "one version needs no older-point allocation")
	require.Zero(t, backing.flags.Load()&nativeMetadataTruncated)
	require.Zero(t, store.evictions.Load())

	commitNativeMetricMetadata(store, ref, makeNativeMetricMetadataPoint(observations, a))
	commitNativeMetricMetadata(store, ref, makeNativeMetricMetadataPoint(0, a))
	require.Same(t, backing, series.nativeMetadataLocked())
	require.Equal(t, int64(1), store.versions.Load())

	points = make([]nativeMetricMetadataPoint, observations)
	for i := range points {
		m := a
		if i%2 != 0 {
			m = b
		}
		points[i] = makeNativeMetricMetadataPoint(int64(observations+1+i), m)
	}
	commitNativeMetricMetadata(store, ref, points...)

	versions, truncated, ok := store.get(ref)
	require.True(t, ok)
	require.True(t, truncated)
	require.Len(t, versions, maxNativeMetricMetadataVersions)
	require.Equal(t, int64(2*observations+1-maxNativeMetricMetadataVersions), versions[0].EffectiveFrom)
	require.Equal(t, uint64(observations-maxNativeMetricMetadataVersions), store.evictions.Load())
	require.Same(t, backing, series.nativeMetadataLocked())
	require.Equal(t, maxNativeMetricMetadataVersions-1, cap(backing.older))
	require.Equal(t, int64(maxNativeMetricMetadataVersions), store.versions.Load())

	commitNativeMetricMetadata(store, ref, makeNativeMetricMetadataPoint(2*observations+1, a))
	versions, truncated, ok = store.get(ref)
	require.True(t, ok)
	require.True(t, truncated)
	require.Len(t, versions, maxNativeMetricMetadataVersions)
	require.Equal(t, int64(2*observations+2-maxNativeMetricMetadataVersions), versions[0].EffectiveFrom)
	require.Equal(t, uint64(observations-maxNativeMetricMetadataVersions+1), store.evictions.Load())
	require.Equal(t, int64(maxNativeMetricMetadataVersions), store.versions.Load())

	store.delete(map[storage.SeriesRef]struct{}{storage.SeriesRef(ref): {}})
	_, _, ok = store.get(ref)
	require.False(t, ok)
	require.Zero(t, store.series.Load())
	require.Zero(t, store.versions.Load())
}

func TestHeadAppenderV2MetadataSidecar(t *testing.T) {
	t.Run("atomic pointer has no size overhead", func(t *testing.T) {
		var series memSeries
		require.Equal(t, unsafe.Sizeof((*memSeriesMetadata)(nil)), unsafe.Sizeof(series.metadata))
		require.Equal(t, 2*unsafe.Sizeof((*metadata.Metadata)(nil)), unsafe.Sizeof(memSeriesMetadata{}))
	})
	t.Run("native allocation layout", func(t *testing.T) {
		var allocation struct {
			sidecar memSeriesMetadata
			native  nativeSeriesMetadata
		}
		if strconv.IntSize == 64 {
			require.Equal(t, uintptr(56), unsafe.Sizeof(allocation.native))
			require.Equal(t, uintptr(16), unsafe.Offsetof(allocation.native))
			require.Equal(t, uintptr(72), unsafe.Sizeof(allocation))
		} else {
			// On 386, combining the 8-byte and 32-byte allocations would round
			// 40 bytes up to 48. Keep the separate-allocation path on 32-bit builds.
			require.Equal(t, uintptr(32), unsafe.Sizeof(allocation.native))
			require.Equal(t, uintptr(8), unsafe.Offsetof(allocation.native))
			require.Equal(t, uintptr(40), unsafe.Sizeof(allocation))
		}
	})

	meta := metadata.Metadata{Type: model.MetricTypeCounter, Unit: "requests", Help: "requests"}
	for _, order := range []string{"native first", "legacy first", "concurrent"} {
		t.Run("initialization order "+order, func(t *testing.T) {
			opts := newTestHeadDefaultOptions(1000, false)
			opts.EnableNativeMetadata = true
			head, _ := newTestHeadWithOptions(t, compression.None, opts)
			seed := head.AppenderV2(t.Context())
			ref, err := seed.Append(0, labels.FromStrings(labels.MetricName, "initialization_order"), 0, 100, 1, nil, nil, storage.AOptions{})
			require.NoError(t, err)
			require.NoError(t, seed.Commit())
			series := head.series.getByID(chunks.HeadSeriesRef(ref))
			require.Nil(t, series.metadata.Load())

			legacyValue := metadata.Metadata{Type: model.MetricTypeGauge, Help: "legacy"}
			legacy := head.Appender(t.Context())
			_, err = legacy.UpdateMetadata(ref, labels.EmptyLabels(), legacyValue)
			require.NoError(t, err)
			native := head.AppenderV2(t.Context())
			_, err = native.Append(ref, labels.EmptyLabels(), 0, 200, 2, nil, nil, storage.AOptions{Metadata: meta})
			require.NoError(t, err)
			require.Nil(t, series.metadata.Load(), "preparing either transaction must remain lazy")

			var first *memSeriesMetadata
			switch order {
			case "native first":
				require.NoError(t, native.Commit())
				first = series.metadata.Load()
				require.Same(t, first.native, series.nativeMetadataLocked())
				if strconv.IntSize == 64 {
					require.Equal(t, unsafe.Sizeof(*first), uintptr(unsafe.Pointer(first.native))-uintptr(unsafe.Pointer(first)), "native state follows the sidecar in the same allocation")
				}
				require.NoError(t, legacy.Commit())
			case "legacy first":
				require.NoError(t, legacy.Commit())
				first = series.metadata.Load()
				require.Nil(t, series.nativeMetadataLocked(), "a legacy sidecar is not committed native history")
				require.Zero(t, head.nativeMetricMetadata.series.Load())
				require.Zero(t, head.nativeMetricMetadata.versions.Load())
				require.False(t, head.nativeMetricMetadata.has(chunks.HeadSeriesRef(ref)))
				require.NoError(t, native.Commit())
			case "concurrent":
				start := make(chan struct{})
				done := make(chan error, 2)
				go func() { <-start; done <- legacy.Commit() }()
				go func() { <-start; done <- native.Commit() }()
				close(start)
				// Join both writers before assertions can end the subtest.
				firstErr, secondErr := <-done, <-done
				require.NoError(t, firstErr)
				require.NoError(t, secondErr)
			}
			if first != nil {
				require.Same(t, first, series.metadata.Load(), "adding the other mode must not replace the sidecar")
			}
			require.Equal(t, int64(1), head.nativeMetricMetadata.series.Load())
			require.Equal(t, int64(1), head.nativeMetricMetadata.versions.Load())
			require.True(t, head.nativeMetricMetadata.has(chunks.HeadSeriesRef(ref)))
			require.Equal(t, legacyValue, *legacyMetadataForTest(series))
			lookups := []storage.NativeMetricMetadataLookup{{Ref: ref, Timestamp: 200}}
			require.NoError(t, head.LookupNativeMetricMetadata(t.Context(), lookups))
			require.Equal(t, &meta, lookups[0].Metadata)
			retained := lookups[0].Metadata
			first = series.metadata.Load()
			series.Lock()
			backing := series.nativeMetadataLocked()
			series.Unlock()

			legacy = head.Appender(t.Context())
			legacyValue.Help = "updated legacy"
			_, err = legacy.UpdateMetadata(ref, labels.EmptyLabels(), legacyValue)
			require.NoError(t, err)
			require.NoError(t, legacy.Commit())
			require.Same(t, first, series.metadata.Load())
			series.Lock()
			unchanged := backing == series.nativeMetadataLocked()
			series.Unlock()
			require.True(t, unchanged, "legacy updates must retain the native backing")
			require.Equal(t, legacyValue, *legacyMetadataForTest(series))
			require.NoError(t, head.LookupNativeMetricMetadata(t.Context(), lookups))
			require.Same(t, retained, lookups[0].Metadata)
		})
	}
	for _, tc := range []struct {
		name   string
		legacy bool
		native bool
	}{
		{name: "off"},
		{name: "legacy", legacy: true},
		{name: "native", native: true},
		{name: "dual", legacy: true, native: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := newTestHeadDefaultOptions(1000, true)
			opts.EnableMetadataWALRecords = tc.legacy
			opts.EnableNativeMetadata = tc.native
			head, _ := newTestHeadWithOptions(t, compression.None, opts)
			lset := labels.FromStrings(labels.MetricName, "requests_total", "job", "api")

			app := head.AppenderV2(context.Background())
			ref, err := app.Append(0, lset, 0, 100, 1, nil, nil, storage.AOptions{Metadata: meta})
			require.NoError(t, err)
			series := head.series.getByID(chunks.HeadSeriesRef(ref))
			series.Lock()
			hasSidecar := series.metadata.Load() != nil
			series.Unlock()
			require.False(t, hasSidecar, "uncommitted metadata must not allocate the sidecar")
			require.NoError(t, app.Commit())

			series.Lock()
			hasSidecar = series.metadata.Load() != nil
			hasLegacy := series.legacyMetadataLocked() != nil
			hasNative := series.nativeMetadataLocked() != nil
			series.Unlock()
			require.Equal(t, tc.legacy || tc.native, hasSidecar)
			require.Equal(t, tc.legacy, hasLegacy)
			require.Equal(t, tc.native, hasNative)
		})
	}

	t.Run("rollback and empty metadata do not allocate", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, true)
		opts.EnableMetadataWALRecords = true
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		ctx := context.Background()

		app := head.AppenderV2(ctx)
		ref, err := app.Append(0, labels.FromStrings(labels.MetricName, "rolled_back"), 0, 100, 1, nil, nil, storage.AOptions{Metadata: meta})
		require.NoError(t, err)
		series := head.series.getByID(chunks.HeadSeriesRef(ref))
		require.NoError(t, app.Rollback())
		series.Lock()
		hasSidecar := series.metadata.Load() != nil
		series.Unlock()
		require.False(t, hasSidecar)

		app = head.AppenderV2(ctx)
		ref, err = app.Append(0, labels.FromStrings(labels.MetricName, "empty"), 0, 100, 1, nil, nil, storage.AOptions{})
		require.NoError(t, err)
		require.NoError(t, app.Commit())
		series = head.series.getByID(chunks.HeadSeriesRef(ref))
		series.Lock()
		hasSidecar = series.metadata.Load() != nil
		series.Unlock()
		require.False(t, hasSidecar)
	})

	t.Run("rejected append does not allocate", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, true)
		opts.EnableMetadataWALRecords = true
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		ctx := context.Background()
		lset := labels.FromStrings(labels.MetricName, "rejected")

		seed := head.AppenderV2(ctx)
		ref, err := seed.Append(0, lset, 0, 100, 1, nil, nil, storage.AOptions{})
		require.NoError(t, err)
		require.NoError(t, seed.Commit())

		app := head.AppenderV2(ctx)
		_, err = app.Append(ref, lset, 0, 50, 0.5, nil, nil, storage.AOptions{Metadata: meta, RejectOutOfOrder: true})
		require.ErrorIs(t, err, storage.ErrOutOfOrderSample)
		require.NoError(t, app.Rollback())
		series := head.series.getByID(chunks.HeadSeriesRef(ref))
		series.Lock()
		hasSidecar := series.metadata.Load() != nil
		series.Unlock()
		require.False(t, hasSidecar)
	})

	t.Run("WAL failure does not allocate", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, true)
		opts.EnableMetadataWALRecords = true
		opts.EnableNativeMetadata = true
		head, wal := newTestHeadWithOptions(t, compression.None, opts)

		app := head.AppenderV2(context.Background())
		ref, err := app.Append(0, labels.FromStrings(labels.MetricName, "wal_failure"), 0, 100, 1, nil, nil, storage.AOptions{Metadata: meta})
		require.NoError(t, err)
		series := head.series.getByID(chunks.HeadSeriesRef(ref))
		require.NoError(t, wal.Close())
		require.Error(t, app.Commit())
		series.Lock()
		hasSidecar := series.metadata.Load() != nil
		series.Unlock()
		require.False(t, hasSidecar)
	})
}

func TestHeadMetadataWALReplayPopulatesOnlyLegacySidecarState(t *testing.T) {
	const numMetadataSeries = 32
	versions := []metadata.Metadata{
		{Type: model.MetricTypeCounter, Unit: "requests", Help: "requests"},
		{Type: model.MetricTypeCounter, Unit: "requests", Help: "updated requests"},
	}
	for _, tc := range []struct {
		name   string
		legacy bool
		native bool
	}{
		{name: "off"},
		{name: "legacy", legacy: true},
		{name: "native", native: true},
		{name: "dual", legacy: true, native: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, concurrency := range []int{1, 4} {
				t.Run(strconv.Itoa(concurrency)+" replay workers", func(t *testing.T) {
					dir := t.TempDir()
					// Seed both paths independently of the flags used when reopening.
					seedOpts := DefaultOptions()
					seedOpts.EnableMetadataWALRecords = true
					seedOpts.EnableNativeMetadata = true
					db := newTestDB(t, withDir(dir), withOpts(seedOpts))
					ctx := context.Background()
					lsets := make([]labels.Labels, numMetadataSeries+1)
					refs := make([]storage.SeriesRef, len(lsets))
					for i := range lsets {
						lsets[i] = labels.FromStrings(labels.MetricName, "requests_total", "instance", strconv.Itoa(i))
					}
					wantSamples := make(map[string][]chunks.Sample, len(lsets))
					for version, meta := range versions {
						app := db.AppenderV2(ctx)
						for i, lset := range lsets {
							var opts storage.AOptions
							if i < numMetadataSeries {
								opts.Metadata = meta
							}
							timestamp, value := int64(100+version), float64(version)
							ref, err := app.Append(refs[i], lset, 0, timestamp, value, nil, nil, opts)
							require.NoError(t, err)
							refs[i] = ref
							wantSamples[lset.String()] = append(wantSamples[lset.String()], sample{t: timestamp, f: value})
						}
						require.NoError(t, app.Commit())
					}
					require.Equal(t, int64(numMetadataSeries), db.head.nativeMetricMetadata.series.Load())
					require.NoError(t, db.Close())

					replayOpts := DefaultOptions()
					replayOpts.EnableMetadataWALRecords = tc.legacy
					replayOpts.EnableNativeMetadata = tc.native
					replayOpts.WALReplayConcurrency = concurrency
					reopened := newTestDB(t, withDir(dir), withOpts(replayOpts))
					require.Equal(t, uint64(len(lsets)), reopened.head.NumSeries())
					if tc.native {
						require.NotNil(t, reopened.head.nativeMetricMetadata)
					} else {
						require.Nil(t, reopened.head.nativeMetricMetadata)
					}
					for i, lset := range lsets {
						series := reopened.head.series.getByHash(lset.Hash(), lset)
						require.NotNil(t, series)
						series.Lock()
						hasSidecar := series.metadata.Load() != nil
						series.Unlock()
						require.Equal(t, i < numMetadataSeries, hasSidecar)
						if i < numMetadataSeries {
							require.Equal(t, &versions[len(versions)-1], legacyMetadataForTest(series))
						} else {
							require.Nil(t, legacyMetadataForTest(series))
						}
						require.Nil(t, nativeMetadataForTest(series))
						if tc.native {
							_, _, ok := reopened.head.nativeMetricMetadata.get(series.ref)
							require.False(t, ok)
						}
					}
					querier, err := NewBlockQuerier(reopened.head, 100, 101)
					require.NoError(t, err)
					require.Equal(t, wantSamples, query(t, querier, labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "requests_total")))
				})
			}
		})
	}
}

func TestHeadAppenderV2NativeMetricMetadataLifecycle(t *testing.T) {
	opts := newTestHeadDefaultOptions(1000, true)
	opts.EnableNativeMetadata = true
	opts.EnableMetadataWALRecords = true
	head, _ := newTestHeadWithOptions(t, compression.None, opts)
	ctx := context.Background()
	seriesLabels := labels.FromStrings(labels.MetricName, "requests_total", "job", "api")
	a := metadata.Metadata{Type: model.MetricTypeCounter, Unit: "requests", Help: "A"}
	b := metadata.Metadata{Type: model.MetricTypeCounter, Unit: "requests", Help: "B"}
	c := metadata.Metadata{Type: model.MetricTypeCounter, Unit: "requests", Help: "C"}

	app := head.AppenderV2(ctx)
	ref, err := app.Append(0, seriesLabels, 0, 100, 1, nil, nil, storage.AOptions{
		Metadata: a,
	})
	require.NoError(t, err)
	series := head.series.getByID(chunks.HeadSeriesRef(ref))
	series.Lock()
	pending := series.pendingCommitCount()
	series.Unlock()
	require.Equal(t, uint32(2), pending) // Created series and sample.
	require.NoError(t, app.Commit())
	require.Equal(t, &a, legacyMetadataForTest(series))

	// The shared observation populates both native and legacy metadata stores.
	app = head.AppenderV2(ctx)
	_, err = app.Append(ref, seriesLabels, 0, 150, 1.5, nil, nil, storage.AOptions{Metadata: b})
	require.NoError(t, err)
	require.NoError(t, app.Commit())
	require.Equal(t, &b, legacyMetadataForTest(series))

	app = head.AppenderV2(ctx)
	_, err = app.Append(ref, seriesLabels, 0, 200, 2, nil, nil, storage.AOptions{
		Metadata: c,
	})
	require.NoError(t, err)
	series.Lock()
	pending = series.pendingCommitCount()
	series.Unlock()
	require.Equal(t, uint32(1), pending) // Sample.
	require.NoError(t, app.Rollback())

	nameMatcher := labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "requests_total")
	jobMatcher := labels.MustNewMatcher(labels.MatchEqual, "job", "api")
	result, truncated, err := head.nativeMetricMetadataForMatchers(ctx, [][]*labels.Matcher{{nameMatcher}, {jobMatcher}}, 0)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []NativeMetricMetadataSeries{{
		Labels: seriesLabels,
		Versions: []NativeMetricMetadataVersion{
			{EffectiveFrom: 100, Metadata: a},
			{EffectiveFrom: 150, Metadata: b},
		},
	}}, result)

	app = head.AppenderV2(ctx)
	_, err = app.Append(ref, seriesLabels, 0, 300, math.Float64frombits(value.StaleNaN), nil, nil, storage.AOptions{
		Metadata: c,
	})
	require.NoError(t, err)
	require.NoError(t, app.Commit())
	result, _, err = head.nativeMetricMetadataForMatchers(ctx, [][]*labels.Matcher{{nameMatcher}}, 0)
	require.NoError(t, err)
	require.Equal(t, []NativeMetricMetadataVersion{
		{EffectiveFrom: 100, Metadata: a},
		{EffectiveFrom: 150, Metadata: b},
		{EffectiveFrom: 300, Metadata: c},
	}, result[0].Versions)
	require.Equal(t, &b, legacyMetadataForTest(series)) // Legacy metadata ignores stale samples.

	head.gcSeries([]storage.SeriesRef{ref}, 301, func(*memSeries) bool { return true })
	result, _, err = head.nativeMetricMetadataForMatchers(ctx, [][]*labels.Matcher{{nameMatcher}}, 0)
	require.NoError(t, err)
	require.Empty(t, result)
}

func TestHeadAppenderV2NativeMetricMetadataTransactions(t *testing.T) {
	t.Run("multiple observations add no reservations", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, true)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		seriesLabels := labels.FromStrings(labels.MetricName, "requests_total", "job", "api")
		a := metadata.Metadata{Type: model.MetricTypeCounter, Help: "A"}
		b := metadata.Metadata{Type: model.MetricTypeCounter, Help: "B"}
		c := metadata.Metadata{Type: model.MetricTypeCounter, Help: "C"}

		app := head.AppenderV2(context.Background())
		ref, err := app.Append(0, seriesLabels, 0, 100, 1, nil, nil, storage.AOptions{Metadata: a})
		require.NoError(t, err)
		_, err = app.Append(ref, seriesLabels, 0, 200, 2, nil, nil, storage.AOptions{Metadata: b})
		require.NoError(t, err)
		_, err = app.Append(ref, seriesLabels, 0, 200, 2, nil, nil, storage.AOptions{Metadata: c})
		require.NoError(t, err)

		series := head.series.getByID(chunks.HeadSeriesRef(ref))
		series.Lock()
		pending := series.pendingCommitCount()
		series.Unlock()
		require.Equal(t, uint32(4), pending) // Created series and three samples.
		require.NoError(t, app.Commit())

		versions, truncated, ok := head.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
		require.True(t, ok)
		require.False(t, truncated)
		require.Equal(t, []NativeMetricMetadataVersion{
			{EffectiveFrom: 100, Metadata: a},
			{EffectiveFrom: 200, Metadata: c},
		}, versions)
		series.Lock()
		pending = series.pendingCommitCount()
		series.Unlock()
		require.Zero(t, pending)
	})

	t.Run("outstanding unchanged metadata does not restore an intervening change", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, true)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		seriesLabels := labels.FromStrings(labels.MetricName, "requests_total", "job", "api")
		a := metadata.Metadata{Type: model.MetricTypeCounter, Help: "A"}
		b := metadata.Metadata{Type: model.MetricTypeCounter, Help: "B"}

		seed := head.AppenderV2(context.Background())
		ref, err := seed.Append(0, seriesLabels, 0, 100, 1, nil, nil, storage.AOptions{Metadata: a})
		require.NoError(t, err)
		require.NoError(t, seed.Commit())

		later := head.AppenderV2(context.Background())
		_, err = later.Append(ref, seriesLabels, 0, 200, 2, nil, nil, storage.AOptions{Metadata: a})
		require.NoError(t, err)

		intervening := head.AppenderV2(context.Background())
		_, err = intervening.Append(ref, seriesLabels, 0, 150, 1.5, nil, nil, storage.AOptions{Metadata: b})
		require.NoError(t, err)
		require.NoError(t, intervening.Commit())
		require.NoError(t, later.Commit())

		// later observed metadata that already matched the series, so it
		// recorded nothing and has nothing left to re-assert once intervening
		// moves the series to b. The history therefore ends at b rather than
		// restoring a at 200. This is the cost of deciding at append time; see
		// the concurrency note in docs/feature_flags.md.
		versions, truncated, ok := head.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
		require.True(t, ok)
		require.False(t, truncated)
		require.Equal(t, []NativeMetricMetadataVersion{
			{EffectiveFrom: 100, Metadata: a},
			{EffectiveFrom: 150, Metadata: b},
		}, versions)
	})

	t.Run("later out-of-order change can follow a discarded stable observation", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, true)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		ctx := context.Background()
		lset := labels.FromStrings(labels.MetricName, "requests_total", "job", "api")
		a := metadata.Metadata{Type: model.MetricTypeCounter, Help: "A"}
		b := metadata.Metadata{Type: model.MetricTypeCounter, Help: "B"}

		seed := head.AppenderV2(ctx)
		ref, err := seed.Append(0, lset, 0, 100, 1, nil, nil, storage.AOptions{Metadata: a})
		require.NoError(t, err)
		require.NoError(t, seed.Commit())

		app := head.AppenderV2(ctx)
		_, err = app.Append(ref, lset, 0, 300, 3, nil, nil, storage.AOptions{Metadata: a})
		require.NoError(t, err)
		require.Nil(t, nativeMetadataTxn(app), "the stable observation is discarded before later appends arrive")
		_, err = app.Append(ref, lset, 0, 200, 2, nil, nil, storage.AOptions{Metadata: b})
		require.NoError(t, err)
		require.NoError(t, app.Commit())

		versions, _, ok := head.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
		require.True(t, ok)
		require.Equal(t, []NativeMetricMetadataVersion{
			{EffectiveFrom: 100, Metadata: a},
			{EffectiveFrom: 200, Metadata: b},
		}, versions)
	})

	t.Run("restores metadata changed earlier in the same transaction", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, true)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		ctx := context.Background()
		lset := labels.FromStrings(labels.MetricName, "requests_total", "job", "api")
		a := metadata.Metadata{Type: model.MetricTypeCounter, Help: "A"}
		b := metadata.Metadata{Type: model.MetricTypeCounter, Help: "B"}

		seed := head.AppenderV2(ctx)
		ref, err := seed.Append(0, lset, 0, 100, 1, nil, nil, storage.AOptions{Metadata: a})
		require.NoError(t, err)
		require.NoError(t, seed.Commit())

		app := head.AppenderV2(ctx)
		_, err = app.Append(ref, lset, 0, 200, 2, nil, nil, storage.AOptions{Metadata: b})
		require.NoError(t, err)
		_, err = app.Append(ref, lset, 0, 300, 3, nil, nil, storage.AOptions{Metadata: a})
		require.NoError(t, err)
		require.NoError(t, app.Commit())

		versions, _, ok := head.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
		require.True(t, ok)
		require.Equal(t, []NativeMetricMetadataVersion{
			{EffectiveFrom: 100, Metadata: a},
			{EffectiveFrom: 200, Metadata: b},
			{EffectiveFrom: 300, Metadata: a},
		}, versions)
	})

	for _, tc := range []struct {
		name     string
		rollback bool
	}{
		{name: "sample reservation protects metadata through commit"},
		{name: "rollback releases the sample reservation", rollback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := newTestHeadDefaultOptions(1000, true)
			opts.EnableNativeMetadata = true
			head, _ := newTestHeadWithOptions(t, compression.None, opts)
			seriesLabels := labels.FromStrings(labels.MetricName, "requests_total", "job", "api")

			seed := head.AppenderV2(context.Background())
			ref, err := seed.Append(0, seriesLabels, 0, 50, 0.5, nil, nil, storage.AOptions{})
			require.NoError(t, err)
			require.NoError(t, seed.Commit())

			app := head.AppenderV2(context.Background())
			meta := metadata.Metadata{Type: model.MetricTypeCounter, Help: "requests"}
			_, err = app.Append(ref, seriesLabels, 0, 100, 1, nil, nil, storage.AOptions{Metadata: meta})
			require.NoError(t, err)

			series := head.series.getByID(chunks.HeadSeriesRef(ref))
			series.Lock()
			pending := series.pendingCommitCount()
			series.Unlock()
			require.Equal(t, uint32(1), pending)
			require.Empty(t, head.gcSeries([]storage.SeriesRef{ref}, math.MaxInt64, func(*memSeries) bool { return true }))
			_, _, ok := head.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
			require.False(t, ok)

			if tc.rollback {
				require.NoError(t, app.Rollback())
				series.Lock()
				pending = series.pendingCommitCount()
				series.Unlock()
				require.Zero(t, pending)
				require.Contains(t, head.gcSeries([]storage.SeriesRef{ref}, math.MaxInt64, func(*memSeries) bool { return true }), ref)
				_, _, ok = head.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
				require.False(t, ok)
				return
			}

			require.NoError(t, app.Commit())
			series.Lock()
			pending = series.pendingCommitCount()
			series.Unlock()
			require.Zero(t, pending)
			versions, truncated, ok := head.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
			require.True(t, ok)
			require.False(t, truncated)
			require.Equal(t, []NativeMetricMetadataVersion{{EffectiveFrom: 100, Metadata: meta}}, versions)
		})
	}

	t.Run("unchanged metadata opens no metadata transaction", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, true)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		ctx := context.Background()
		lset := labels.FromStrings(labels.MetricName, "requests_total", "job", "api")
		a := metadata.Metadata{Type: model.MetricTypeCounter, Help: "A"}
		b := metadata.Metadata{Type: model.MetricTypeCounter, Help: "B"}

		app := head.AppenderV2(ctx)
		ref, err := app.Append(0, lset, 0, 100, 1, nil, nil, storage.AOptions{Metadata: a})
		require.NoError(t, err)
		require.NotNil(t, nativeMetadataTxn(app), "first append must record")
		require.NoError(t, app.Commit())

		// Same metadata again: nothing to record, so the appender never takes a
		// metadata transaction from the pool and commit has no work to do.
		app = head.AppenderV2(ctx)
		_, err = app.Append(ref, lset, 0, 200, 2, nil, nil, storage.AOptions{Metadata: a})
		require.NoError(t, err)
		require.Nil(t, nativeMetadataTxn(app))
		require.NoError(t, app.Commit())

		// Changed metadata still records.
		app = head.AppenderV2(ctx)
		_, err = app.Append(ref, lset, 0, 300, 3, nil, nil, storage.AOptions{Metadata: b})
		require.NoError(t, err)
		require.NotNil(t, nativeMetadataTxn(app))
		require.NoError(t, app.Commit())

		versions, _, ok := head.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
		require.True(t, ok)
		require.Equal(t, []NativeMetricMetadataVersion{
			{EffectiveFrom: 100, Metadata: a},
			{EffectiveFrom: 300, Metadata: b},
		}, versions)
	})

	t.Run("changing metadata reuses the series cache entry", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, true)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		ctx := context.Background()
		lset := labels.FromStrings(labels.MetricName, "requests_total", "job", "api")

		app := head.AppenderV2(ctx)
		ref, err := app.Append(0, lset, 0, 100, 1, nil, nil, storage.AOptions{
			Metadata: metadata.Metadata{Type: model.MetricTypeCounter, Help: "A"},
		})
		require.NoError(t, err)
		require.NoError(t, app.Commit())

		series := head.series.getByID(chunks.HeadSeriesRef(ref))
		series.Lock()
		first := series.metadata.Load()
		series.Unlock()
		require.NotNil(t, first)

		// Metadata that keeps changing must not allocate a fresh entry per
		// commit; the series keeps one for its lifetime.
		for i := range 5 {
			app = head.AppenderV2(ctx)
			_, err = app.Append(ref, lset, 0, int64(200+i*100), float64(i), nil, nil, storage.AOptions{
				Metadata: metadata.Metadata{Type: model.MetricTypeCounter, Help: strconv.Itoa(i)},
			})
			require.NoError(t, err)
			require.NoError(t, app.Commit())

			series.Lock()
			same := series.metadata.Load()
			series.Unlock()
			require.Same(t, first, same)
		}
	})

	t.Run("series sharing metadata share one cached copy", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, true)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		ctx := context.Background()
		meta := metadata.Metadata{Type: model.MetricTypeCounter, Unit: "requests", Help: "shared help"}
		other := metadata.Metadata{Type: model.MetricTypeCounter, Unit: "requests", Help: "other help"}

		const series = 8
		refs := make([]storage.SeriesRef, series)
		app := head.AppenderV2(ctx)
		for i := range series {
			// Half the series carry one metadata value, half the other.
			m := meta
			if i%2 == 1 {
				m = other
			}
			lset := labels.FromStrings(labels.MetricName, "requests_total", "id", strconv.Itoa(i))
			ref, err := app.Append(0, lset, 0, 100, float64(i), nil, nil, storage.AOptions{Metadata: m})
			require.NoError(t, err)
			refs[i] = ref
		}
		require.NoError(t, app.Commit())

		cached := func(i int) *metadata.Metadata {
			s := head.series.getByID(chunks.HeadSeriesRef(refs[i]))
			s.Lock()
			defer s.Unlock()
			native := s.nativeMetadataLocked()
			require.NotNil(t, native)
			return native.metadata
		}
		// One copy per distinct value, not per series: that is what keeps the
		// cache to a pointer rather than 48 bytes of string headers each.
		for i := 2; i < series; i++ {
			require.Same(t, cached(i%2), cached(i), "series %d", i)
		}
		require.NotSame(t, cached(0), cached(1))
		require.Equal(t, meta, *cached(0))
		require.Equal(t, other, *cached(1))
	})

	t.Run("out-of-order append behind the newest version still records", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, true)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		ctx := context.Background()
		lset := labels.FromStrings(labels.MetricName, "requests_total", "job", "api")
		a := metadata.Metadata{Type: model.MetricTypeCounter, Help: "A"}
		b := metadata.Metadata{Type: model.MetricTypeCounter, Help: "B"}

		var ref storage.SeriesRef
		for _, o := range []struct {
			t int64
			m metadata.Metadata
		}{{100, a}, {200, b}, {150, a}, {250, a}} {
			app := head.AppenderV2(ctx)
			got, err := app.Append(ref, lset, 0, o.t, float64(o.t), nil, nil, storage.AOptions{Metadata: o.m})
			require.NoError(t, err)
			ref = got
			require.NoError(t, app.Commit())
		}

		// The out-of-order a@150 must not leave the series cached as "newest is
		// a@150": the newest version is still b@200, so a@250 is a real change.
		versions, _, ok := head.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
		require.True(t, ok)
		require.Equal(t, []NativeMetricMetadataVersion{
			{EffectiveFrom: 100, Metadata: a},
			{EffectiveFrom: 200, Metadata: b},
			{EffectiveFrom: 250, Metadata: a},
		}, versions)
	})

	t.Run("metadata WAL state does not stand in for native state", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, true)
		opts.EnableNativeMetadata = true
		opts.EnableMetadataWALRecords = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		ctx := context.Background()
		lset := labels.FromStrings(labels.MetricName, "requests_total", "job", "api")
		a := metadata.Metadata{Type: model.MetricTypeCounter, Help: "A"}

		// The V1 appender sets legacy metadata without going near the native
		// store. A later V2 append carrying the same metadata must still record.
		v1 := head.Appender(ctx)
		ref, err := v1.Append(0, lset, 50, 1)
		require.NoError(t, err)
		_, err = v1.UpdateMetadata(ref, lset, a)
		require.NoError(t, err)
		require.NoError(t, v1.Commit())

		series := head.series.getByID(chunks.HeadSeriesRef(ref))
		require.Equal(t, &a, legacyMetadataForTest(series))
		require.Nil(t, nativeMetadataForTest(series))

		app := head.AppenderV2(ctx)
		_, err = app.Append(ref, lset, 0, 100, 1, nil, nil, storage.AOptions{Metadata: a})
		require.NoError(t, err)
		require.NoError(t, app.Commit())

		versions, _, ok := head.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
		require.True(t, ok)
		require.Equal(t, []NativeMetricMetadataVersion{{EffectiveFrom: 100, Metadata: a}}, versions)
	})

	t.Run("more distinct values than one transaction retains raw", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, true)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		ctx := context.Background()

		// Values beyond maxNativeMetricMetadataValues are interned as they are
		// observed rather than held raw until commit.
		const series = 3 * maxNativeMetricMetadataValues
		app := head.AppenderV2(ctx)
		for i := range series {
			lset := labels.FromStrings(labels.MetricName, "requests_total", "id", strconv.Itoa(i))
			_, err := app.Append(0, lset, 0, 100, float64(i), nil, nil, storage.AOptions{
				Metadata: metadata.Metadata{Type: model.MetricTypeCounter, Help: strconv.Itoa(i)},
			})
			require.NoError(t, err)
		}
		require.NoError(t, app.Commit())

		nameMatcher := labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "requests_total")
		result, truncated, err := head.nativeMetricMetadataForMatchers(ctx, [][]*labels.Matcher{{nameMatcher}}, 0)
		require.NoError(t, err)
		require.False(t, truncated)
		require.Len(t, result, series)
		for _, item := range result {
			require.Equal(t, []NativeMetricMetadataVersion{{
				EffectiveFrom: 100,
				Metadata:      metadata.Metadata{Type: model.MetricTypeCounter, Help: item.Labels.Get("id")},
			}}, item.Versions, "series %s", item.Labels)
		}
	})

	t.Run("concurrent garbage collection leaves no orphans", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, true)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		ctx := context.Background()

		const (
			workers      = 4
			rounds       = 20
			perTx        = 512
			evictAllTime = math.MaxInt64
		)
		observe := func(app storage.AppenderV2, worker, round, i int) error {
			lset := labels.FromStrings(labels.MetricName, "requests_total",
				"worker", strconv.Itoa(worker), "id", strconv.Itoa(i))
			_, err := app.Append(0, lset, 0, int64(round+1), float64(round), nil, nil, storage.AOptions{
				Metadata: metadata.Metadata{Type: model.MetricTypeCounter, Help: strconv.Itoa(round)},
			})
			return err
		}

		// Metadata is published while the appender's pending commits still keep
		// its series in the Head. Committing it any later would let a
		// concurrent collection delete the series first, leaving metadata that
		// no later collection can reach.
		var appendWG sync.WaitGroup
		for worker := range workers {
			appendWG.Go(func() {
				for round := range rounds {
					app := head.AppenderV2(ctx)
					for i := range perTx {
						if err := observe(app, worker, round, i); err != nil {
							t.Errorf("append failed: %v", err)
							_ = app.Rollback()
							return
						}
					}
					if err := app.Commit(); err != nil {
						t.Errorf("commit failed: %v", err)
						return
					}
				}
			})
		}

		done := make(chan struct{})
		var gcWG sync.WaitGroup
		gcWG.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				refs := make([]storage.SeriesRef, 0, workers*perTx)
				for all := head.postings.All(); all.Next(); {
					refs = append(refs, all.At())
				}
				head.gcSeries(refs, evictAllTime, func(*memSeries) bool { return true })
			}
		})

		appendWG.Wait()
		close(done)
		gcWG.Wait()

		// Leave something behind so an empty store cannot pass vacuously.
		app := head.AppenderV2(ctx)
		for i := range perTx {
			require.NoError(t, observe(app, 0, rounds, i))
		}
		require.NoError(t, app.Commit())

		var (
			orphans []chunks.HeadSeriesRef
			stored  int64
		)
		head.nativeMetricMetadata.directory.Range(func(key, value any) bool {
			first := chunks.HeadSeriesRef(key.(uint64) << nativeMetadataPageBits)
			for offset := range nativeMetadataPageSize {
				ref := first + chunks.HeadSeriesRef(offset)
				if nativeMetadataDirectorySeries(value, ref) != nil {
					stored++
					if head.series.getByID(ref) == nil {
						orphans = append(orphans, ref)
					}
				}
			}
			return true
		})
		require.Empty(t, orphans, "metadata retained for series the Head no longer has")
		require.Positive(t, stored)
		require.Equal(t, stored, head.nativeMetricMetadata.series.Load())
	})
}

func TestHeadNativeMetricMetadataWALReplayDeletion(t *testing.T) {
	opts := newTestHeadDefaultOptions(1000, false)
	opts.EnableNativeMetadata = true
	head, _ := newTestHeadWithOptions(t, compression.None, opts)

	app := head.AppenderV2(context.Background())
	ref, err := app.Append(0, labels.FromStrings(labels.MetricName, "requests_total"), 0, 100, 1, nil, nil, storage.AOptions{
		Metadata: metadata.Metadata{Type: model.MetricTypeCounter},
	})
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	_, _, ok := head.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
	require.True(t, ok)
	require.Equal(t, int64(1), head.nativeMetricMetadata.series.Load())
	require.Equal(t, int64(1), head.nativeMetricMetadata.versions.Load())
	require.Equal(t, uint64(1), head.NumSeries())

	// Native metadata is not reconstructed during WAL replay, so call the
	// replay-only deletion path directly to verify it cleans a populated store.
	head.deleteSeriesByID([]chunks.HeadSeriesRef{chunks.HeadSeriesRef(ref)})

	_, _, ok = head.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
	require.False(t, ok)
	require.Zero(t, head.nativeMetricMetadata.series.Load())
	require.Zero(t, head.nativeMetricMetadata.versions.Load())
	require.Zero(t, head.NumSeries())
}

func TestHeadNativeMetricMetadataIsFeatureGatedAndNonPersistent(t *testing.T) {
	ctx := context.Background()
	matcher := labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+")

	t.Run("disabled", func(t *testing.T) {
		head, _ := newTestHead(t, 1000, compression.None, false)
		_, _, err := head.nativeMetricMetadataForMatchers(ctx, [][]*labels.Matcher{{matcher}}, 0)
		require.ErrorIs(t, err, ErrNativeMetadataDisabled)
	})

	t.Run("reset clears in-memory metadata", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, false)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		app := head.AppenderV2(ctx)
		_, err := app.Append(0, labels.FromStrings(labels.MetricName, "metric"), 0, 100, 1, nil, nil, storage.AOptions{
			Metadata: metadata.Metadata{Type: model.MetricTypeGauge},
		})
		require.NoError(t, err)
		require.NoError(t, app.Commit())
		require.NoError(t, head.resetInMemoryState())
		result, _, err := head.nativeMetricMetadataForMatchers(ctx, [][]*labels.Matcher{{matcher}}, 0)
		require.NoError(t, err)
		require.Empty(t, result)
	})

	t.Run("native metadata does not enable legacy metadata", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, false)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		seriesLabels := labels.FromStrings(labels.MetricName, "metric")
		meta := metadata.Metadata{Type: model.MetricTypeGauge, Help: "metric"}
		app := head.AppenderV2(ctx)
		ref, err := app.Append(0, seriesLabels, 0, 100, 1, nil, nil, storage.AOptions{Metadata: meta})
		require.NoError(t, err)
		require.NoError(t, app.Commit())

		series := head.series.getByID(chunks.HeadSeriesRef(ref))
		require.Nil(t, legacyMetadataForTest(series))
		versions, truncated, ok := head.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
		require.True(t, ok)
		require.False(t, truncated)
		require.Equal(t, []NativeMetricMetadataVersion{{EffectiveFrom: 100, Metadata: meta}}, versions)
	})
}

func TestHeadNativeMetricMetadataSampleKinds(t *testing.T) {
	for _, kind := range []string{"float", "histogram", "float-histogram"} {
		t.Run(kind, func(t *testing.T) {
			opts := newTestHeadDefaultOptions(1000, false)
			opts.EnableNativeMetadata = true
			opts.EnableSTAsZeroSample = true
			head, _ := newTestHeadWithOptions(t, compression.None, opts)
			lset := labels.FromStrings(labels.MetricName, "a")
			var hist *histogram.Histogram
			var floatHist *histogram.FloatHistogram
			if kind == "histogram" {
				hist = &histogram.Histogram{}
			}
			if kind == "float-histogram" {
				floatHist = &histogram.FloatHistogram{}
			}
			m := metadata.Metadata{Help: "description", Unit: "seconds"}
			app := head.AppenderV2(t.Context())
			ref, err := app.Append(0, lset, 50, 100, 1, hist, floatHist, storage.AOptions{Metadata: m})
			require.NoError(t, err)
			require.NoError(t, app.Commit())
			app = head.AppenderV2(t.Context())
			_, err = app.Append(ref, lset, 50, 200, 2, hist, floatHist, storage.AOptions{Metadata: metadata.Metadata{Help: strings.Clone(m.Help), Unit: strings.Clone(m.Unit)}})
			require.NoError(t, err)
			require.Nil(t, nativeMetadataTxn(app), "unchanged content must not stage metadata")
			require.NoError(t, app.Commit())
			versions, _, ok := head.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
			require.True(t, ok)
			wantMetadata := metadata.Metadata{Type: model.MetricTypeUnknown, Help: m.Help, Unit: m.Unit}
			require.Equal(t, []NativeMetricMetadataVersion{{EffectiveFrom: 100, Metadata: wantMetadata}}, versions, "synthetic ST samples must not carry metadata")
		})
	}
}

func TestNativeMetricMetadataCacheOwnership(t *testing.T) {
	for _, mode := range []string{"raw", "direct", "committed"} {
		t.Run(mode, func(t *testing.T) {
			store := newNativeMetricMetadataStore()
			m := metadata.Metadata{Type: model.MetricTypeUnknown, Help: strings.Clone("published metadata"), Unit: "seconds"}
			first, second := store.seriesForTest(1), store.seriesForTest(1+nativeMetricMetadataStripes)
			appender := store.getAppender()
			if mode == "direct" {
				for i := range maxNativeMetricMetadataValues {
					appender.metadataReference(first, metadata.Metadata{Help: strconv.Itoa(i)})
				}
			}
			appender.observe(first, 100, m)
			appender.observe(second, 100, m)
			require.Equal(t, mode == "direct", appender.observations[0].metadataRef&nativeMetricMetadataDirectRefMask != 0)
			store.commitAppender(appender)
			cached := nativeMetadataForTest(first)
			require.Same(t, cached.metadata, nativeMetadataForTest(second).metadata)
			require.Equal(t, m, *cached.metadata)
			if mode == "raw" {
				require.Same(t, unsafe.StringData(m.Help), unsafe.StringData(cached.metadata.Help), "retain caller-backed comparison strings")
			}
			store.putAppender(appender)
			if mode == "committed" {
				appender = store.getAppender()
				older := metadata.Metadata{Help: "older"}
				appender.observe(first, 50, older)
				appender.observe(second, 50, older)
				store.commitAppender(appender)
				require.Same(t, cached.metadata, nativeMetadataForTest(first).metadata)
				store.putAppender(appender)
			}
			// Overwrite this exact object's scratch; pool identity is unspecified.
			poison := metadata.Metadata{Help: "reused transaction"}
			appender.values = append(appender.values, nativeMetricMetadataValue{metadata: poison})
			appender.points = append(appender.points, makeNativeMetricMetadataPoint(500, poison))
			appender.shared[cached.handle] = &poison
			runtime.GC()
			require.Equal(t, m, *cached.metadata)
			require.Equal(t, m, *nativeMetadataForTest(second).metadata)
		})
	}

	t.Run("deletion leaves retired history immutable", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		commitNativeMetricMetadata(store, 1,
			makeNativeMetricMetadataPoint(100, metadata.Metadata{Help: "old"}),
			makeNativeMetricMetadataPoint(200, metadata.Metadata{Help: "current"}))
		series := store.indexedSeries(1)
		cached := nativeMetadataForTest(series)
		older := cached.older[0]
		stale := store.getAppender()
		stale.observe(series, 300, metadata.Metadata{Help: "stale publisher"})
		// Head GC marks the series before removing presence.
		series.Lock()
		series.setGCed()
		series.Unlock()
		store.delete(map[storage.SeriesRef]struct{}{1: {}})
		store.commitAppender(stale)
		store.putAppender(stale)
		require.False(t, store.has(1))
		require.Same(t, cached.metadata, nativeMetadataForTest(series).metadata)
		require.Equal(t, older, cached.older[0])
		require.Zero(t, store.series.Load())
		require.Zero(t, store.versions.Load())
	})
}

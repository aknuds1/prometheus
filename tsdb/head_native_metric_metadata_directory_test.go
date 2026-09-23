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
	"math"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
)

func TestNativeMetricMetadataDirectory(t *testing.T) {
	t.Run("random membership matches full width map", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		want := map[chunks.HeadSeriesRef]*memSeries{}
		refs := []chunks.HeadSeriesRef{0, 1, 255, 256, 257, 1 << 32, 1<<32 + 255, math.MaxUint64}
		for ref := range 512 {
			refs = append(refs, chunks.HeadSeriesRef(ref))
		}
		rng := rand.New(rand.NewPCG(17, 29))
		point := makeNativeMetricMetadataPoint(100, metadata.Metadata{Help: "test"})
		for range 5000 {
			ref := refs[rng.IntN(len(refs))]
			if rng.IntN(3) != 0 {
				commitNativeMetricMetadata(store, ref, point)
				series := store.indexedSeries(ref)
				require.NotNil(t, series)
				if previous := want[ref]; previous != nil {
					require.Same(t, previous, series, "duplicate publication changes no membership")
				}
				want[ref] = series
			} else {
				if series := want[ref]; series != nil {
					series.Lock()
					series.setGCed()
					series.Unlock()
				}
				store.delete(map[storage.SeriesRef]struct{}{storage.SeriesRef(ref): {}})
				delete(want, ref)
			}
			for _, ref := range refs {
				require.Equal(t, want[ref], store.indexedSeries(ref))
				require.Equal(t, want[ref] != nil, store.has(ref))
			}
			require.Equal(t, int64(len(want)), store.series.Load())
			require.Equal(t, int64(len(want)), store.versions.Load())
		}
	})

	t.Run("last deletion races with first insertion in same page", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		point := makeNativeMetricMetadataPoint(100, metadata.Metadata{Help: "test"})
		for round := range 200 {
			first := chunks.HeadSeriesRef(round*256 + 1)
			second := first + 1
			commitNativeMetricMetadata(store, first, point)
			retired := store.indexedSeries(first)
			retired.Lock()
			retired.setGCed()
			retired.Unlock()
			var wg sync.WaitGroup
			wg.Go(func() { store.delete(map[storage.SeriesRef]struct{}{storage.SeriesRef(first): {}}) })
			wg.Go(func() { commitNativeMetricMetadata(store, second, point) })
			wg.Wait()
			require.Nil(t, store.indexedSeries(first))
			require.NotNil(t, store.indexedSeries(second))
			require.Equal(t, int64(1), store.series.Load())
			store.delete(map[storage.SeriesRef]struct{}{storage.SeriesRef(second): {}})
			require.Zero(t, store.series.Load())
		}
	})
}

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
	"runtime"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unique"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/util/compression"
)

type nativeMetadataNotifyFunc func()

func (f nativeMetadataNotifyFunc) Notify() { f() }

func TestHeadLookupNativeMetricMetadata(t *testing.T) {
	a := metadata.Metadata{Type: model.MetricTypeCounter, Help: "a", Unit: "seconds"}
	b := metadata.Metadata{Type: model.MetricTypeGauge, Help: "b"}
	t.Run("lookup does not need the series mutex", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, false)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		var ref storage.SeriesRef
		for version, m := range []metadata.Metadata{a, b} {
			app := head.AppenderV2(t.Context())
			var err error
			ref, err = app.Append(ref, labels.FromStrings(labels.MetricName, "metric"), 0, int64(100+100*version), 1, nil, nil, storage.AOptions{Metadata: m})
			require.NoError(t, err)
			require.NoError(t, app.Commit())
		}
		series := head.series.getByID(chunks.HeadSeriesRef(ref))
		lookups := []storage.NativeMetricMetadataLookup{{Ref: ref, Timestamp: 100}, {Ref: ref, Timestamp: 200}}
		series.Lock()
		done := make(chan error, 1)
		go func() { done <- head.LookupNativeMetricMetadata(t.Context(), lookups) }()
		select {
		case err := <-done:
			series.Unlock()
			require.NoError(t, err)
		case <-time.After(time.Second):
			// Release the lock and join the reader even on a regression.
			series.Unlock()
			<-done
			t.Fatal("lookup waited for the series mutex")
		}
		require.Equal(t, &a, lookups[0].Metadata)
		require.Equal(t, &b, lookups[1].Metadata)
	})

	t.Run("concurrent legacy sidecar installation", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, false)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		const count = 1024
		refs := make([]storage.SeriesRef, count)
		app := head.AppenderV2(t.Context())
		for i := range refs {
			var err error
			refs[i], err = app.Append(0, labels.FromStrings(labels.MetricName, "metric", "id", strconv.Itoa(i)), 0, 100, 1, nil, nil, storage.AOptions{})
			require.NoError(t, err)
		}
		require.NoError(t, app.Commit())
		start := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			<-start
			legacy := head.Appender(t.Context())
			for _, ref := range refs {
				if _, err := legacy.UpdateMetadata(ref, labels.EmptyLabels(), a); err != nil {
					_ = legacy.Rollback()
					done <- err
					return
				}
			}
			done <- legacy.Commit()
		}()
		lookups := make([]storage.NativeMetricMetadataLookup, count)
		for i, ref := range refs {
			lookups[i] = storage.NativeMetricMetadataLookup{Ref: ref, Timestamp: 100}
		}
		close(start)
		defer func() { require.NoError(t, <-done) }()
		for range 20 {
			require.NoError(t, head.LookupNativeMetricMetadata(t.Context(), lookups))
			for _, lookup := range lookups {
				require.Nil(t, lookup.Metadata, "legacy metadata is not native history")
			}
		}
	})

	for _, enabled := range []bool{false, true} {
		t.Run("enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
			opts := newTestHeadDefaultOptions(1000, false)
			opts.EnableNativeMetadata = enabled
			head, _ := newTestHeadWithOptions(t, compression.None, opts)
			var ref storage.SeriesRef
			for i, m := range []metadata.Metadata{a, b, a} {
				app := head.AppenderV2(t.Context())
				var err error
				ref, err = app.Append(ref, labels.FromStrings(labels.MetricName, "metric"), 0, int64(100+i*100), 1, nil, nil, storage.AOptions{Metadata: m})
				require.NoError(t, err)
				require.NoError(t, app.Commit())
			}
			for _, tc := range []struct {
				timestamp int64
				want      *metadata.Metadata
			}{{99, nil}, {100, &a}, {199, &a}, {200, &b}, {299, &b}, {300, &a}, {400, &a}} {
				lookups := []storage.NativeMetricMetadataLookup{{Ref: ref, Timestamp: tc.timestamp, Metadata: &b}}
				require.NoError(t, head.LookupNativeMetricMetadata(t.Context(), lookups))
				if !enabled {
					tc.want = nil
				}
				require.Equal(t, tc.want, lookups[0].Metadata, "timestamp %d", tc.timestamp)
				lookups[0].Ref = ref + 1000
				require.NoError(t, head.LookupNativeMetricMetadata(t.Context(), lookups))
				require.Nil(t, lookups[0].Metadata, "reused result on a miss")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			require.ErrorIs(t, head.LookupNativeMetricMetadata(ctx, nil), context.Canceled)
		})
	}

	t.Run("mixed batches and concurrent readers", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, false)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		refs := make([]storage.SeriesRef, 2)
		for version, m := range []metadata.Metadata{a, b} {
			app := head.AppenderV2(t.Context())
			for i := range refs {
				options := storage.AOptions{}
				if i == 0 {
					options.Metadata = m
				}
				var err error
				refs[i], err = app.Append(refs[i], labels.FromStrings(labels.MetricName, "metric", "id", strconv.Itoa(i)), 0, int64(100+version*100), 1, nil, nil, options)
				require.NoError(t, err)
			}
			require.NoError(t, app.Commit())
		}
		// Legacy-only metadata must not be mistaken for native history.
		legacy := head.Appender(t.Context())
		_, err := legacy.UpdateMetadata(refs[1], labels.EmptyLabels(), a)
		require.NoError(t, err)
		require.NoError(t, legacy.Commit())
		cases := []struct {
			ref       storage.SeriesRef
			timestamp int64
			want      *metadata.Metadata
		}{{refs[0], 200, &b}, {refs[0], 100, &a}, {refs[0], 99, nil}, {refs[1], 200, nil}, {refs[1] + 1000, 200, nil}}
		var readers sync.WaitGroup
		for range 4 {
			readers.Go(func() {
				lookups := make([]storage.NativeMetricMetadataLookup, 2*maxNativeMetricMetadataLookups+1)
				for range 20 {
					for i := range lookups {
						tc := cases[i%len(cases)]
						lookups[i] = storage.NativeMetricMetadataLookup{Ref: tc.ref, Timestamp: tc.timestamp, Metadata: &a}
					}
					if err := head.LookupNativeMetricMetadata(t.Context(), lookups); err != nil {
						t.Error(err)
						return
					}
					for i, lookup := range lookups {
						want := cases[i%len(cases)].want
						if want == nil {
							if lookup.Metadata != nil {
								t.Errorf("unexpected metadata for lookup %d: %v", i, lookup.Metadata)
							}
						} else if lookup.Metadata == nil || *lookup.Metadata != *want {
							t.Errorf("lookup %d: got %v, want %v", i, lookup.Metadata, want)
						}
					}
				}
			})
		}
		readers.Wait()
		for _, tc := range []struct {
			ref       storage.SeriesRef
			timestamp int64
		}{{refs[0], 200}, {refs[0], 99}, {refs[1], 200}, {refs[1] + 1000, 200}} {
			scratch, err := head.selectNativeMetricMetadataBatch(t.Context(), []storage.NativeMetricMetadataLookup{{Ref: tc.ref, Timestamp: tc.timestamp}})
			require.NoError(t, err)
			require.Nil(t, scratch, "current values and misses must not borrow historical scratch")
		}
	})

	t.Run("materialization after publication and scratch reuse", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, false)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		var ref storage.SeriesRef
		for version, m := range []metadata.Metadata{a, b} {
			app := head.AppenderV2(t.Context())
			var err error
			ref, err = app.Append(ref, labels.FromStrings(labels.MetricName, "metric"), 0, int64(100+version*100), 1, nil, nil, storage.AOptions{Metadata: m})
			require.NoError(t, err)
			require.NoError(t, app.Commit())
		}
		lookups := []storage.NativeMetricMetadataLookup{{Ref: ref, Timestamp: 100}, {Ref: ref, Timestamp: 200}, {Ref: ref, Timestamp: 100}}
		scratch, err := head.selectNativeMetricMetadataBatch(t.Context(), lookups)
		require.NoError(t, err)
		require.NotNil(t, scratch)
		t.Cleanup(func() {
			scratch.reset()
			nativeMetricMetadataLookupPool.Put(scratch)
		})
		// Selection must release the barrier before the caller materializes values.
		require.True(t, head.nativeMetricMetadata.publication.TryAcquire(nativeMetricMetadataPublicationPermits))
		head.nativeMetricMetadata.publication.Release(nativeMetricMetadataPublicationPermits)
		current := lookups[1].Metadata
		for version := 3; version <= 8; version++ {
			app := head.AppenderV2(t.Context())
			_, err := app.Append(ref, labels.EmptyLabels(), 0, int64(version*100), 1, nil, nil, storage.AOptions{Metadata: metadata.Metadata{Help: strconv.Itoa(version)}})
			require.NoError(t, err)
			require.NoError(t, app.Commit())
		}
		deleted, _, _, _, _, _ := head.series.gcSeries([]storage.SeriesRef{ref}, math.MaxInt64, func(*memSeries) bool { return true })
		require.Len(t, deleted, 1)
		head.nativeMetricMetadata.delete(deleted)
		runtime.GC()
		require.NoError(t, scratch.materialize(t.Context(), lookups))
		historical := lookups[0].Metadata
		require.Same(t, historical, lookups[2].Metadata)
		require.Equal(t, a, *historical)
		require.Equal(t, b, *current)

		// Reuse this exact scratch object instead of depending on pool identity.
		scratch.reset()
		require.Empty(t, scratch.copies)
		require.Equal(t, [maxNativeMetricMetadataLookups]unique.Handle[metadata.Metadata]{}, scratch.historical)
		scratch.historical[0] = unique.Make(b)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		reused := []storage.NativeMetricMetadataLookup{{Ref: ref, Timestamp: 200}}
		require.ErrorIs(t, scratch.materialize(ctx, reused), context.Canceled)
		require.Nil(t, reused[0].Metadata)
		require.Empty(t, scratch.copies)
		scratch.reset()
		scratch.historical[0] = unique.Make(b)
		require.NoError(t, scratch.materialize(t.Context(), reused))
		require.Equal(t, b, *reused[0].Metadata)
		require.Equal(t, a, *historical, "retained results must not alias scratch")
	})

	t.Run("ownership eviction and deletion", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, false)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		refs := make([]storage.SeriesRef, maxNativeMetricMetadataLookups+1)
		lookups := make([]storage.NativeMetricMetadataLookup, len(refs))
		for version := range 2 {
			app := head.AppenderV2(t.Context())
			for i := range refs {
				var err error
				refs[i], err = app.Append(refs[i], labels.FromStrings(labels.MetricName, "metric", "id", strconv.Itoa(i)), 0, int64(100+version*100), 1, nil, nil, storage.AOptions{Metadata: []metadata.Metadata{a, b}[version]})
				require.NoError(t, err)
				lookups[i] = storage.NativeMetricMetadataLookup{Ref: refs[i], Timestamp: 200}
			}
			require.NoError(t, app.Commit())
		}
		require.NoError(t, head.LookupNativeMetricMetadata(t.Context(), lookups))
		current := lookups[0].Metadata
		for i := range lookups {
			require.Equal(t, &b, lookups[i].Metadata)
			require.Same(t, current, lookups[i].Metadata, "current values reuse the committed pointer")
			lookups[i].Timestamp = 100
		}
		require.NoError(t, head.LookupNativeMetricMetadata(t.Context(), lookups))
		historical := lookups[0].Metadata
		require.Same(t, historical, lookups[1].Metadata, "historical values are shared within a batch")
		for i := 3; i <= 8; i++ {
			app := head.AppenderV2(t.Context())
			_, err := app.Append(refs[0], labels.EmptyLabels(), 0, int64(i*100), 1, nil, nil, storage.AOptions{Metadata: metadata.Metadata{Help: strconv.Itoa(i)}})
			require.NoError(t, err)
			require.NoError(t, app.Commit())
		}
		check := []storage.NativeMetricMetadataLookup{{Ref: refs[0], Timestamp: 300}, {Ref: refs[0], Timestamp: 400}}
		require.NoError(t, head.LookupNativeMetricMetadata(t.Context(), check))
		require.Nil(t, check[0].Metadata, "evicted history must not borrow a future value")
		require.Equal(t, "4", check[1].Metadata.Help)

		// Model the GC window after the series disappears but before its history is removed.
		start := make(chan struct{})
		var readers sync.WaitGroup
		for range 4 {
			readers.Go(func() {
				<-start
				for range 20 {
					lookups := []storage.NativeMetricMetadataLookup{{Ref: refs[0], Timestamp: 400}, {Ref: refs[0], Timestamp: 1000}}
					if err := head.LookupNativeMetricMetadata(t.Context(), lookups); err != nil {
						t.Error(err)
						return
					}
					// A lookup overlapping deletion may select a value before
					// deletion, or miss. It must never select another version.
					for i, lookup := range lookups {
						want := []string{"4", "8"}[i]
						if lookup.Metadata != nil && lookup.Metadata.Help != want {
							t.Errorf("got %v during deletion, want %s or a miss", lookup.Metadata, want)
						}
					}
				}
			})
		}
		close(start)
		deleted, _, _, _, _, _ := head.series.gcSeries(refs[:1], math.MaxInt64, func(*memSeries) bool { return true })
		readers.Wait()
		require.Len(t, deleted, 1)
		check[0].Timestamp = 1000
		require.NoError(t, head.LookupNativeMetricMetadata(t.Context(), check))
		require.Nil(t, check[0].Metadata)
		require.Nil(t, check[1].Metadata)
		head.nativeMetricMetadata.delete(deleted)
		app := head.AppenderV2(t.Context())
		newRef, err := app.Append(0, labels.FromStrings(labels.MetricName, "metric", "id", "0"), 0, 1000, 1, nil, nil, storage.AOptions{Metadata: a})
		require.NoError(t, err)
		require.NoError(t, app.Commit())
		require.NotEqual(t, refs[0], newRef)
		require.NoError(t, head.LookupNativeMetricMetadata(t.Context(), check))
		require.Nil(t, check[0].Metadata)
		runtime.GC()
		require.Equal(t, a, *historical)
		require.Equal(t, b, *current)
	})
}

func TestHeadLookupNativeMetricMetadataPublication(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial metadata.Metadata
		failWAL bool
	}{
		{name: "first metadata"},
		{name: "first metadata WAL failure", failWAL: true},
		{name: "replacement", initial: metadata.Metadata{Help: "old"}},
		{name: "replacement WAL failure", initial: metadata.Metadata{Help: "old"}, failWAL: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := newTestHeadDefaultOptions(1000, true)
			opts.EnableNativeMetadata = true
			head, wal := newTestHeadWithOptions(t, compression.None, opts)
			app := head.AppenderV2(t.Context())
			ref, err := app.Append(0, labels.FromStrings(labels.MetricName, "metric"), 0, 100, 1, nil, nil, storage.AOptions{Metadata: tc.initial})
			require.NoError(t, err)
			require.NoError(t, app.Commit())
			app = head.AppenderV2(t.Context())
			_, err = app.Append(ref, labels.EmptyLabels(), 0, 200, 2, nil, nil, storage.AOptions{Metadata: metadata.Metadata{Help: "new"}})
			require.NoError(t, err)
			lookups := []storage.NativeMetricMetadataLookup{{Ref: ref, Timestamp: 200}}
			if tc.failWAL {
				require.NoError(t, wal.Close())
				require.Error(t, app.Commit())
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				require.NoError(t, head.LookupNativeMetricMetadata(ctx, lookups), "failed commit must release the barrier")
				if tc.initial.IsEmpty() {
					require.Nil(t, lookups[0].Metadata)
				} else {
					require.Equal(t, "old", lookups[0].Metadata.Help)
				}
				return
			}
			_, before, err := wal.LastSegmentAndOffset()
			require.NoError(t, err)
			stripe := head.nativeMetricMetadata.stripe(chunks.HeadSeriesRef(ref))
			stripe.mtx.Lock()
			locked := true
			defer func() {
				if locked {
					stripe.mtx.Unlock()
				}
			}()
			done := make(chan error, 1)
			go func() { done <- app.Commit() }()
			require.Eventually(t, func() bool {
				_, after, err := wal.LastSegmentAndOffset()
				return err == nil && after > before
			}, time.Second, time.Millisecond)
			// The new sample is already visible in the WAL, but publication is
			// blocked. Without the barrier this returns old or absent metadata.
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			require.ErrorIs(t, head.LookupNativeMetricMetadata(ctx, lookups), context.DeadlineExceeded)
			require.Nil(t, lookups[0].Metadata)
			stripe.mtx.Unlock()
			locked = false
			require.NoError(t, <-done)
			require.NoError(t, head.LookupNativeMetricMetadata(t.Context(), lookups))
			require.Equal(t, "new", lookups[0].Metadata.Help)
		})
	}

	t.Run("notification and unchanged commits do not hold the barrier", func(t *testing.T) {
		opts := newTestHeadDefaultOptions(1000, false)
		opts.EnableNativeMetadata = true
		head, _ := newTestHeadWithOptions(t, compression.None, opts)
		m := metadata.Metadata{Help: "native"}
		app := head.AppenderV2(t.Context())
		ref, err := app.Append(0, labels.FromStrings(labels.MetricName, "metric"), 0, 100, 1, nil, nil, storage.AOptions{Metadata: m})
		require.NoError(t, err)
		head.writeNotified = nativeMetadataNotifyFunc(func() {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			lookups := []storage.NativeMetricMetadataLookup{{Ref: ref, Timestamp: 100}}
			require.NoError(t, head.LookupNativeMetricMetadata(ctx, lookups))
			require.Equal(t, "native", lookups[0].Metadata.Help)
		})
		require.NoError(t, app.Commit())
		head.writeNotified = nil
		app = head.AppenderV2(t.Context())
		_, err = app.Append(ref, labels.EmptyLabels(), 0, 200, 1, nil, nil, storage.AOptions{Metadata: m})
		require.NoError(t, err)
		require.NoError(t, head.nativeMetricMetadata.publication.Acquire(t.Context(), nativeMetricMetadataPublicationPermits))
		defer head.nativeMetricMetadata.publication.Release(nativeMetricMetadataPublicationPermits)
		done := make(chan error, 1)
		go func() { done <- app.Commit() }()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("unchanged commit waited for the publication barrier")
		}
	})

	t.Run("waiting lookup precedes later commits", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			store := newNativeMetricMetadataStore()
			require.NoError(t, store.publication.Acquire(t.Context(), 1))
			lookup := make(chan struct{})
			release := make(chan struct{})
			go func() {
				_ = store.publication.Acquire(t.Context(), nativeMetricMetadataPublicationPermits)
				close(lookup)
				<-release
				store.publication.Release(nativeMetricMetadataPublicationPermits)
			}()
			synctest.Wait()
			commit := make(chan struct{})
			go func() {
				_ = store.publication.Acquire(t.Context(), 1)
				close(commit)
				store.publication.Release(1)
			}()
			synctest.Wait()
			store.publication.Release(1)
			<-lookup
			select {
			case <-commit:
				t.Fatal("later commit bypassed a waiting lookup")
			default:
			}
			close(release)
			<-commit
		})
	})
}

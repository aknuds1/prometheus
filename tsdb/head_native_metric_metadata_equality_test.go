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
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/compression"
)

func TestHeadAppenderNativeMetadataEquality(t *testing.T) {
	t.Run("proofs preserve observation decisions", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		app := &headAppenderBase{head: &Head{nativeMetricMetadata: store}}
		defer app.clearNativeMetadataEquality()
		defer app.clearNativeMetricMetadata()
		s := store.seriesForTest(1)
		m := metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: strings.Repeat("a", 1024)}
		commitNativeMetricMetadata(store, s.ref, makeNativeMetricMetadataPoint(100, m))
		s = store.indexedSeries(s.ref)
		observe := func(timestamp int64, value *metadata.Metadata) bool {
			s.Lock()
			defer s.Unlock()
			return app.shouldObserveNativeMetricMetadataLocked(s, timestamp, value)
		}
		require.False(t, observe(100, &m))
		require.Nil(t, app.nativeMetricMetadata, "equality must not open an observation transaction")
		owned := nativeMetadataForTest(s).metadata
		require.Equal(t, m, app.nativeMetadataEquality.values[owned])
		fresh := m
		fresh.Help = strings.Clone(m.Help)
		require.False(t, observe(101, &fresh))
		require.Same(t, unsafe.StringData(fresh.Help), unsafe.StringData(app.nativeMetadataEquality.values[owned].Help))
		require.True(t, observe(99, &m), "matching values can move the beginning of history")
		require.False(t, observe(101, nil))
		for _, changed := range []metadata.Metadata{
			{Type: model.MetricTypeGauge, Unit: m.Unit, Help: m.Help},
			{Type: m.Type, Unit: "bytes", Help: m.Help},
			{Type: m.Type, Unit: m.Unit, Help: m.Help + "b"},
		} {
			require.True(t, observe(101, &changed))
			require.Equal(t, fresh, app.nativeMetadataEquality.values[owned], "negative comparisons must not replace proofs")
		}
		changed := m
		changed.Help += "changed"
		commitNativeMetricMetadata(store, s.ref, makeNativeMetricMetadataPoint(102, changed))
		require.True(t, observe(103, &m), "an old proof cannot match a changed committed pointer")
		require.False(t, observe(103, &changed))
		app.recordNativeMetricMetadata(s, 104, m)
		require.True(t, observe(105, &changed), "pending A then committed B must retain B")
	})

	t.Run("admission bounds", func(t *testing.T) {
		for _, size := range []int{1023, 1024, nativeMetricMetadataEqualityMaxBytes / 2, nativeMetricMetadataEqualityMaxBytes/2 + 1} {
			t.Run(strconv.Itoa(size), func(t *testing.T) {
				store := newNativeMetricMetadataStore()
				app := &headAppenderBase{head: &Head{nativeMetricMetadata: store}}
				defer app.clearNativeMetadataEquality()
				s := store.seriesForTest(1)
				m := metadata.Metadata{Help: strings.Repeat("x", size)}
				s.Lock()
				s.ensureMetadataLocked().native = &nativeSeriesMetadata{metadata: cloneNativeMetricMetadata(m), effectiveFrom: 100}
				require.False(t, app.shouldObserveNativeMetricMetadataLocked(s, 100, &m))
				s.Unlock()
				if size < 1024 || size > nativeMetricMetadataEqualityMaxBytes/2 {
					require.Nil(t, app.nativeMetadataEquality)
				} else {
					require.Len(t, app.nativeMetadataEquality.values, 1)
					require.Equal(t, 2*size, app.nativeMetadataEquality.bytes)
				}
			})
		}
		for _, size := range []int{1024, 256 << 10} {
			store := newNativeMetricMetadataStore()
			app := &headAppenderBase{head: &Head{nativeMetricMetadata: store}}
			for range nativeMetricMetadataEqualityMaxEntries + 1 {
				s := &memSeries{}
				m := metadata.Metadata{Help: strings.Repeat("x", size)}
				s.Lock()
				s.ensureMetadataLocked().native = &nativeSeriesMetadata{metadata: cloneNativeMetricMetadata(m)}
				require.False(t, app.shouldObserveNativeMetricMetadataLocked(s, 100, &m))
				s.Unlock()
			}
			require.Len(t, app.nativeMetadataEquality.values, min(nativeMetricMetadataEqualityMaxEntries, nativeMetricMetadataEqualityMaxBytes/(2*size)))
			require.LessOrEqual(t, app.nativeMetadataEquality.bytes, nativeMetricMetadataEqualityMaxBytes)
			memo := app.nativeMetadataEquality
			app.clearNativeMetadataEquality()
			require.Empty(t, memo.values)
			require.Zero(t, memo.bytes)
			require.Nil(t, app.nativeMetadataEquality)
		}
	})

	t.Run("terminal paths release proofs without GC", func(t *testing.T) {
		for _, terminal := range []string{"commit", "rollback", "WAL failure"} {
			for _, changes := range []bool{false, true} {
				t.Run(terminal+"/changes="+strconv.FormatBool(changes), func(t *testing.T) {
					opts := newTestHeadDefaultOptions(1000, true)
					opts.EnableNativeMetadata = true
					head, wal := newTestHeadWithOptions(t, compression.None, opts)
					m := metadata.Metadata{Type: model.MetricTypeCounter, Help: strings.Repeat("x", 1024)}
					ls := labels.FromStrings(labels.MetricName, "memo")
					seed := head.AppenderV2(t.Context())
					ref, err := seed.Append(0, ls, 0, 100, 1, nil, nil, storage.AOptions{Metadata: m})
					require.NoError(t, err)
					require.NoError(t, seed.Commit())
					app := head.AppenderV2(t.Context()).(*headAppenderV2)
					_, err = app.Append(ref, ls, 0, 200, 1, nil, nil, storage.AOptions{Metadata: m})
					require.NoError(t, err)
					require.Nil(t, app.nativeMetricMetadata)
					memo := app.nativeMetadataEquality
					require.NotNil(t, memo)
					require.NotEmpty(t, memo.values)
					if changes {
						m.Help += "changed"
						_, err = app.Append(ref, ls, 0, 201, 1, nil, nil, storage.AOptions{Metadata: m})
						require.NoError(t, err)
					}
					switch terminal {
					case "commit":
						require.NoError(t, app.Commit())
					case "rollback":
						require.NoError(t, app.Rollback())
					case "WAL failure":
						require.NoError(t, wal.Close())
						require.Error(t, app.Commit())
					}
					require.True(t, app.closed)
					require.Nil(t, app.nativeMetadataEquality)
					require.Nil(t, app.nativeMetricMetadata)
					require.Empty(t, memo.values)
					require.Zero(t, memo.bytes)
					require.ErrorIs(t, app.Rollback(), ErrAppenderClosed)
				})
			}
		}
	})
}

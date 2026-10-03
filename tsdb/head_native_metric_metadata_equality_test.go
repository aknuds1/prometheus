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

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/nativemetadata"
	"github.com/prometheus/prometheus/util/compression"
)

func TestHeadAppenderNativeMetadataEquality(t *testing.T) {
	t.Run("saturation preserves hits and observation decisions", func(t *testing.T) {
		for _, bound := range []string{"entries", "bytes"} {
			t.Run(bound, func(t *testing.T) {
				store := newNativeMetricMetadataStore()
				app := &headAppenderBase{head: &Head{nativeMetricMetadata: store}, batches: []*appendBatch{{}}}
				defer app.batches[0].close(app.head)
				count, size := nativeMetricMetadataEqualityMaxEntries, 1024
				if bound == "bytes" {
					count, size = 1, nativeMetricMetadataEqualityMaxBytes/2
				}
				m := metadata.Metadata{Help: strings.Repeat("x", size)}
				series := make([]*memSeries, count+1)
				for i := range series {
					s := &memSeries{}
					series[i] = s
					s.Lock()
					s.ensureMetadataLocked().native = &nativeSeriesMetadata{History: nativemetadata.History{Metadata: cloneNativeMetricMetadata(m), EffectiveFrom: 100}}
					observe, proof := app.shouldObserveNativeMetricMetadataLocked(s, 100, &m)
					s.Unlock()
					require.False(t, observe)
					if i == count {
						require.Nil(t, proof, "a saturated memo must not return another admission proof")
					} else {
						require.NotNil(t, proof)
						app.rememberNativeMetadataEquality(proof, m)
					}
				}
				memo := app.batches[0].nativeMetadataEquality
				require.Len(t, memo.values, count)
				fresh := metadata.Metadata{Help: strings.Clone(m.Help)}
				s := series[0]
				s.Lock()
				observe, proof := app.shouldObserveNativeMetricMetadataLocked(s, 101, &fresh)
				s.Unlock()
				require.False(t, observe)
				require.Nil(t, proof)
				require.Same(t, unsafe.StringData(fresh.Help), unsafe.StringData(memo.values[nativeMetadataForTest(s).Metadata].Help))
				for _, current := range []*memSeries{s, series[count]} {
					current.Lock()
					observe, proof = app.shouldObserveNativeMetricMetadataLocked(current, 99, &m)
					require.True(t, observe, "saturation cannot suppress older observations")
					require.Nil(t, proof)
					changed := metadata.Metadata{Help: m.Help + "changed"}
					observe, proof = app.shouldObserveNativeMetricMetadataLocked(current, 101, &changed)
					current.Unlock()
					require.True(t, observe)
					require.Nil(t, proof)
				}
			})
		}
	})

	t.Run("remaining allowance rejects large values without disabling smaller ones", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		app := &headAppenderBase{head: &Head{nativeMetricMetadata: store}, batches: []*appendBatch{{}}}
		defer app.batches[0].close(app.head)
		for i, size := range []int{(nativeMetricMetadataEqualityMaxBytes - 4096) / 2, 4096, 1024, 1024} {
			m := metadata.Metadata{Help: strings.Repeat("x", size)}
			s := &memSeries{}
			s.Lock()
			s.ensureMetadataLocked().native = &nativeSeriesMetadata{History: nativemetadata.History{Metadata: cloneNativeMetricMetadata(m), EffectiveFrom: 100}}
			observe, proof := app.shouldObserveNativeMetricMetadataLocked(s, 100, &m)
			s.Unlock()
			require.False(t, observe)
			require.NotNil(t, proof)
			app.rememberNativeMetadataEquality(proof, m)
			memo := app.batches[0].nativeMetadataEquality
			require.Len(t, memo.values, []int{1, 1, 2, 3}[i])
			require.Equal(t, []int{4096, 4096, 2048, 0}[i], nativeMetricMetadataEqualityMaxBytes-memo.bytes)
		}
	})

	t.Run("cost includes every field and remaining allowance", func(t *testing.T) {
		m := metadata.Metadata{Type: model.MetricTypeGauge, Unit: "seconds", Help: strings.Repeat("x", 1024)}
		cost := 2 * (len(m.Type) + len(m.Unit) + len(m.Help))
		for _, allowance := range []int{0, cost - 1, cost, cost + 1, nativeMetricMetadataEqualityMaxBytes} {
			got := nativeMetricMetadataEqualityCost(m, allowance)
			if allowance < cost {
				require.Zero(t, got)
			} else {
				require.Equal(t, cost, got)
			}
		}
	})

	t.Run("first proof survives committed state changing before admission", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		app := &headAppenderBase{head: &Head{nativeMetricMetadata: store}}
		m := metadata.Metadata{Type: model.MetricTypeCounter, Help: strings.Repeat("a", 1024)}
		s := &memSeries{}
		s.Lock()
		s.ensureMetadataLocked().native = &nativeSeriesMetadata{History: nativemetadata.History{Metadata: cloneNativeMetricMetadata(m), EffectiveFrom: 100}}
		observe, proof := app.shouldObserveNativeMetricMetadataLocked(s, 100, &m)
		s.Unlock()
		require.False(t, observe)
		require.NotNil(t, proof)
		require.Empty(t, app.batches, "verification must not allocate a batch")
		changed := m
		changed.Help += "changed"
		s.Lock()
		s.nativeMetadataLocked().Metadata = cloneNativeMetricMetadata(changed)
		s.Unlock()
		app.batches = []*appendBatch{{}}
		defer app.batches[0].close(app.head)
		app.rememberNativeMetadataEquality(proof, m)
		require.Equal(t, m, app.batches[0].nativeMetadataEquality.values[proof])
		s.Lock()
		observe, proof = app.shouldObserveNativeMetricMetadataLocked(s, 101, &m)
		s.Unlock()
		require.True(t, observe, "the old proof must not apply to a changed committed pointer")
		require.Nil(t, proof)
	})

	t.Run("first append admits proofs for every sample type across batches", func(t *testing.T) {
		for _, firstType := range []string{"float", "histogram", "float histogram"} {
			t.Run(firstType, func(t *testing.T) {
				opts := newTestHeadDefaultOptions(1000, true)
				opts.EnableNativeMetadata = true
				head, _ := newTestHeadWithOptions(t, compression.None, opts)
				m := metadata.Metadata{Type: model.MetricTypeCounter, Help: strings.Repeat("x", 1024)}
				ls := labels.FromStrings(labels.MetricName, "memo_batches")
				seed := head.AppenderV2(t.Context())
				ref, err := seed.Append(0, ls, 0, 100, 1, nil, nil, storage.AOptions{Metadata: m})
				require.NoError(t, err)
				require.NoError(t, seed.Commit())
				app := head.AppenderV2(t.Context()).(*headAppenderV2)
				_, err = app.Append(ref, ls, 0, 99, 1, nil, nil, storage.AOptions{Metadata: m, RejectOutOfOrder: true})
				require.Error(t, err)
				require.Empty(t, app.batches, "a rejected sample must not allocate a batch or memo")
				for i, kind := range []string{firstType, "histogram", "float histogram", "float"} {
					var h *histogram.Histogram
					var fh *histogram.FloatHistogram
					switch kind {
					case "histogram":
						h = &histogram.Histogram{Count: 1, ZeroCount: 1}
					case "float histogram":
						fh = &histogram.FloatHistogram{Count: 1, ZeroCount: 1}
					}
					_, err = app.Append(ref, ls, 0, int64(200+i), 1, h, fh, storage.AOptions{Metadata: m})
					require.NoError(t, err)
					require.NotNil(t, app.batches[0].nativeMetadataEquality)
					require.Len(t, app.batches[0].nativeMetadataEquality.values, 1)
					require.Nil(t, app.nativeMetricMetadata)
				}
				require.Greater(t, len(app.batches), 1)
				for _, b := range app.batches[1:] {
					require.Nil(t, b.nativeMetadataEquality)
				}
				first := app.batches[0]
				memo := first.nativeMetadataEquality
				require.NoError(t, app.Commit())
				require.Nil(t, first.nativeMetadataEquality)
				require.Empty(t, memo.values)
				require.Zero(t, memo.bytes)
			})
		}
	})

	t.Run("proofs preserve observation decisions", func(t *testing.T) {
		store := newNativeMetricMetadataStore()
		app := &headAppenderBase{head: &Head{nativeMetricMetadata: store}, batches: []*appendBatch{{}}}
		defer app.batches[0].close(app.head)
		defer app.clearNativeMetricMetadata()
		s := store.seriesForTest(1)
		m := metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: strings.Repeat("a", 1024)}
		commitNativeMetricMetadata(store, s.ref, makeNativeMetricMetadataPoint(100, m))
		s = store.indexedSeries(s.ref)
		observe := func(timestamp int64, value *metadata.Metadata) bool {
			s.Lock()
			defer s.Unlock()
			observe, proof := app.shouldObserveNativeMetricMetadataLocked(s, timestamp, value)
			if proof != nil {
				app.rememberNativeMetadataEquality(proof, *value)
			}
			return observe
		}
		require.False(t, observe(100, &m))
		require.Nil(t, app.nativeMetricMetadata, "equality must not open an observation transaction")
		owned := nativeMetadataForTest(s).Metadata
		require.Equal(t, m, app.batches[0].nativeMetadataEquality.values[owned])
		fresh := m
		fresh.Help = strings.Clone(m.Help)
		require.False(t, observe(101, &fresh))
		require.Same(t, unsafe.StringData(fresh.Help), unsafe.StringData(app.batches[0].nativeMetadataEquality.values[owned].Help))
		require.True(t, observe(99, &m), "matching values can move the beginning of history")
		require.False(t, observe(101, nil))
		for _, changed := range []metadata.Metadata{
			{Type: model.MetricTypeGauge, Unit: m.Unit, Help: m.Help},
			{Type: m.Type, Unit: "bytes", Help: m.Help},
			{Type: m.Type, Unit: m.Unit, Help: m.Help + "b"},
		} {
			require.True(t, observe(101, &changed))
			require.Equal(t, fresh, app.batches[0].nativeMetadataEquality.values[owned], "negative comparisons must not replace proofs")
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
				app := &headAppenderBase{head: &Head{nativeMetricMetadata: store}, batches: []*appendBatch{{}}}
				defer app.batches[0].close(app.head)
				s := store.seriesForTest(1)
				m := metadata.Metadata{Help: strings.Repeat("x", size)}
				s.Lock()
				s.ensureMetadataLocked().native = &nativeSeriesMetadata{History: nativemetadata.History{Metadata: cloneNativeMetricMetadata(m), EffectiveFrom: 100}}
				observe, proof := app.shouldObserveNativeMetricMetadataLocked(s, 100, &m)
				require.False(t, observe)
				if proof != nil {
					app.rememberNativeMetadataEquality(proof, m)
				}
				s.Unlock()
				if size < 1024 || size > nativeMetricMetadataEqualityMaxBytes/2 {
					require.Nil(t, app.batches[0].nativeMetadataEquality)
				} else {
					require.Len(t, app.batches[0].nativeMetadataEquality.values, 1)
					require.Equal(t, 2*size, app.batches[0].nativeMetadataEquality.bytes)
				}
			})
		}
		for _, size := range []int{1024, 256 << 10} {
			store := newNativeMetricMetadataStore()
			app := &headAppenderBase{head: &Head{nativeMetricMetadata: store}, batches: []*appendBatch{{}}}
			for range nativeMetricMetadataEqualityMaxEntries + 1 {
				s := &memSeries{}
				m := metadata.Metadata{Help: strings.Repeat("x", size)}
				s.Lock()
				s.ensureMetadataLocked().native = &nativeSeriesMetadata{History: nativemetadata.History{Metadata: cloneNativeMetricMetadata(m)}}
				observe, proof := app.shouldObserveNativeMetricMetadataLocked(s, 100, &m)
				require.False(t, observe)
				if proof != nil {
					app.rememberNativeMetadataEquality(proof, m)
				}
				s.Unlock()
			}
			require.Len(t, app.batches[0].nativeMetadataEquality.values, min(nativeMetricMetadataEqualityMaxEntries, nativeMetricMetadataEqualityMaxBytes/(2*size)))
			require.LessOrEqual(t, app.batches[0].nativeMetadataEquality.bytes, nativeMetricMetadataEqualityMaxBytes)
			memo := app.batches[0].nativeMetadataEquality
			app.batches[0].close(app.head)
			require.Empty(t, memo.values)
			require.Zero(t, memo.bytes)
			require.Nil(t, app.batches[0].nativeMetadataEquality)
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
					first := app.batches[0]
					memo := first.nativeMetadataEquality
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
					require.Nil(t, first.nativeMetadataEquality)
					require.Nil(t, app.nativeMetricMetadata)
					require.Empty(t, memo.values)
					require.Zero(t, memo.bytes)
					require.ErrorIs(t, app.Rollback(), ErrAppenderClosed)
				})
			}
		}
	})
}

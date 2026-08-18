// Copyright 2015 The Prometheus Authors
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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/wlog"
	"github.com/prometheus/prometheus/util/compression"
)

type headGCRegressionAppender struct {
	storage.AppenderTransaction
	base   *headAppenderBase
	append func(storage.SeriesRef, labels.Labels, int64, float64) (storage.SeriesRef, error)
}

type headGCRegressionAppenderFactory struct {
	name string
	new  func(*Head) headGCRegressionAppender
}

func headGCRegressionAppenders(t *testing.T) []headGCRegressionAppenderFactory {
	return []headGCRegressionAppenderFactory{
		{
			name: "v1",
			new: func(h *Head) headGCRegressionAppender {
				a := h.Appender(t.Context()).(*headAppender)
				return headGCRegressionAppender{
					AppenderTransaction: a,
					base:                &a.headAppenderBase,
					append:              a.Append,
				}
			},
		},
		{
			name: "v2",
			new: func(h *Head) headGCRegressionAppender {
				a := h.AppenderV2(t.Context()).(*headAppenderV2)
				return headGCRegressionAppender{
					AppenderTransaction: a,
					base:                &a.headAppenderBase,
					append: func(ref storage.SeriesRef, lset labels.Labels, ts int64, v float64) (storage.SeriesRef, error) {
						return a.Append(ref, lset, 0, ts, v, nil, nil, storage.AOptions{})
					},
				}
			},
		},
	}
}

func requireHeadGCRegressionSamples(t *testing.T, h *Head, lset labels.Labels, stage string, expected []chunks.Sample) {
	t.Helper()

	matchers := make([]*labels.Matcher, 0, lset.Len())
	lset.Range(func(l labels.Label) {
		matchers = append(matchers, labels.MustNewMatcher(labels.MatchEqual, l.Name, l.Value))
	})
	q, err := NewBlockQuerier(h, math.MinInt64, math.MaxInt64)
	require.NoError(t, err)
	result := query(t, q, matchers...)
	require.Len(t, result, 1, "%s", stage)
	requireEqualSamples(t, stage, expected, result[lset.String()])
}

func restartHeadForGCRegression(t *testing.T, h *Head) *Head {
	t.Helper()

	opts := h.opts
	walDir := filepath.Join(opts.ChunkDirRoot, "wal")
	require.NoError(t, h.Close())

	wal, err := wlog.NewSize(nil, nil, walDir, 32768, compression.None)
	require.NoError(t, err)
	restarted, err := NewHead(nil, nil, wal, nil, opts, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = restarted.Close() })
	require.NoError(t, restarted.Init(0))
	return restarted
}

func TestHeadGCRegression(t *testing.T) {
	t.Run("overlapping appenders", func(t *testing.T) {
		interferingClosers := []struct {
			name  string
			close func(headGCRegressionAppender) error
		}{
			{name: "commit", close: func(app headGCRegressionAppender) error { return app.Commit() }},
			{name: "rollback", close: func(app headGCRegressionAppender) error { return app.Rollback() }},
		}

		for _, appender := range headGCRegressionAppenders(t) {
			for _, closer := range interferingClosers {
				t.Run(appender.name+"/"+closer.name, func(t *testing.T) {
					h, _ := newTestHead(t, 1000, compression.None, false)
					h.initTime(0)

					lset := labels.FromStrings("series", "overlapping-appenders")
					baseline := appender.new(h)
					ref, err := baseline.append(0, lset, 100, 1)
					require.NoError(t, err)
					require.NoError(t, baseline.Commit())

					// Leave the newer sample uncommitted so the series remains eligible for deletion
					// by maxTime but must still be protected from GC.
					pending := appender.new(h)
					_, err = pending.append(ref, lset, 200, 2)
					require.NoError(t, err)

					// Use a duplicate so closing this appender exercises pending-state bookkeeping
					// without advancing the series timestamp and masking the bug.
					interfering := appender.new(h)
					_, err = interfering.append(ref, lset, 100, 1)
					require.NoError(t, err)
					require.NoError(t, closer.close(interfering))

					deleted, err := h.truncateSeries([]storage.SeriesRef{ref}, 100, func(*memSeries) bool { return true })
					require.NoError(t, err)
					require.Zero(t, deleted, "GC deleted a series while another appender was pending")
					require.NoError(t, pending.Commit())

					expected := []chunks.Sample{newSample(0, 100, 1, nil, nil), newSample(0, 200, 2, nil, nil)}
					requireHeadGCRegressionSamples(t, h, lset, "before WAL restart", expected)
					h = restartHeadForGCRegression(t, h)
					requireHeadGCRegressionSamples(t, h, lset, "after WAL restart", expected)
				})
			}
		}
	})

	t.Run("resolved series deleted", func(t *testing.T) {
		for _, appender := range headGCRegressionAppenders(t) {
			t.Run(appender.name, func(t *testing.T) {
				h, _ := newTestHead(t, 1000, compression.None, false)
				h.initTime(0)

				lset := labels.FromStrings("series", "resolved-before-gc")
				baseline := appender.new(h)
				oldRef, err := baseline.append(0, lset, 100, 1)
				require.NoError(t, err)
				require.NoError(t, baseline.Commit())
				oldSeries := h.series.getByID(chunks.HeadSeriesRef(oldRef))
				require.NotNil(t, oldSeries)

				pending := appender.new(h)
				lookupDone := make(chan struct{})
				resumeAppend := make(chan struct{})
				// Pause after the appender resolves oldSeries but before it can append,
				// leaving it with a pointer that GC will unlink.
				pending.base.testAfterSeriesLookup = func(series *memSeries) {
					if series != oldSeries {
						return
					}
					close(lookupDone)
					<-resumeAppend
				}

				appendDone := make(chan error, 1)
				go func() {
					_, err := pending.append(oldRef, lset, 200, 2)
					appendDone <- err
				}()
				select {
				case <-lookupDone:
				case appendErr := <-appendDone:
					require.NoError(t, pending.Rollback())
					t.Fatalf("append completed before reaching the series lookup hook: %v", appendErr)
				}

				// Delete the resolved series while the append is paused. On resume, the appender
				// must retry against a live series rather than using this stale pointer.
				deleted, truncateErr := h.truncateSeries([]storage.SeriesRef{oldRef}, 100, func(*memSeries) bool { return true })
				close(resumeAppend)
				appendErr := <-appendDone
				require.NoError(t, truncateErr)
				require.Equal(t, 1, deleted)
				require.NoError(t, appendErr)
				require.NoError(t, pending.Commit())

				// The t=100 sample was intentionally truncated; only the post-GC append
				// should remain in Head and survive WAL replay.
				expected := []chunks.Sample{newSample(0, 200, 2, nil, nil)}
				requireHeadGCRegressionSamples(t, h, lset, "before WAL restart", expected)
				h = restartHeadForGCRegression(t, h)
				requireHeadGCRegressionSamples(t, h, lset, "after WAL restart", expected)
			})
		}
	})

	t.Run("candidate check does not hold stripe lock", func(t *testing.T) {
		series := newStripeSeries(1, noopSeriesLifecycleCallback{})
		existingLabels := labels.FromStrings("series", "existing")
		existing := newMemSeries(existingLabels, 1, 0, defaultIsolationDisabled, false)
		got, created := series.setUnlessAlreadySet(existingLabels.Hash(), existingLabels, existing)
		require.True(t, created)
		require.Same(t, existing, got)

		checkStarted := make(chan struct{})
		continueCheck := make(chan struct{})
		iterationDone := make(chan int, 1)
		// Pause inside the candidate check so the test can probe whether
		// iterForDeletion still holds the stripe lock.
		go func() {
			iterationDone <- series.iterForDeletion(func(_ int, _ uint64, _ *memSeries) bool {
				close(checkStarted)
				<-continueCheck
				// Reject the candidate because this test isolates lock scope, not deletion.
				return false
			}, func(_ int, _ uint64, _ *memSeries, _ map[chunks.HeadSeriesRef]labels.Labels) {
				t.Error("delete callback invoked for a rejected candidate")
			})
		}()
		<-checkStarted

		// A writer must acquire the stripe lock while the check is paused; otherwise
		// it can convoy subsequent readers behind GC's read lock.
		lockAvailable := series.locks[0].TryLock()
		if lockAvailable {
			series.locks[0].Unlock()
		}

		close(continueCheck)
		deleted := <-iterationDone

		require.True(t, lockAvailable, "the candidate check held the stripe lock")
		require.Zero(t, deleted)
	})
}

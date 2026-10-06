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
package wlog

import (
	"fmt"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/promslog"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/model/timestamp"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/nativemetadata"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/util/compression"
	"github.com/prometheus/prometheus/util/testutil"
	"github.com/prometheus/prometheus/util/testwal"
)

var (
	defaultRetryInterval = 100 * time.Millisecond
	defaultRetries       = 100
	wMetrics             = NewWatcherMetrics(prometheus.DefaultRegisterer)
)

// retry executes f() n times at each interval until it returns true.
// TODO(bwplotka): Replace with require.Eventually.
func retry(t *testing.T, interval time.Duration, n int, f func() bool) {
	t.Helper()
	ticker := time.NewTicker(interval)
	for i := 0; i <= n; i++ {
		if f() {
			return
		}
		<-ticker.C
	}
	ticker.Stop()
	t.Log("function returned false")
}

// Overwrite readTimeout defined in watcher.go.
func overwriteReadTimeout(t *testing.T, val time.Duration) {
	initialVal := readTimeout
	readTimeout = val
	t.Cleanup(func() { readTimeout = initialVal })
}

type writeToMock struct {
	mu sync.Mutex

	seriesStored            []record.RefSeries
	metadataStored          []record.RefMetadata
	samplesAppended         []record.RefSample
	exemplarsAppended       []record.RefExemplar
	histogramsAppended      []record.RefHistogramSample
	floatHistogramsAppended []record.RefFloatHistogramSample

	seriesStores           int
	metadataStores         int
	sampleAppends          int
	exemplarAppends        int
	histogramAppends       int
	floatHistogramsAppends int

	seriesSegmentIndexes map[chunks.HeadSeriesRef]int

	// If nonzero, delay reads with a short sleep.
	delay time.Duration
}

func (wtm *writeToMock) Append(s []record.RefSample) bool {
	wtm.mu.Lock()
	defer wtm.mu.Unlock()

	wtm.sampleAppends++
	wtm.samplesAppended = append(wtm.samplesAppended, s...)
	time.Sleep(wtm.delay)
	return true
}

func (wtm *writeToMock) AppendExemplars(e []record.RefExemplar) bool {
	wtm.mu.Lock()
	defer wtm.mu.Unlock()

	time.Sleep(wtm.delay)
	wtm.exemplarAppends++
	wtm.exemplarsAppended = append(wtm.exemplarsAppended, e...)
	return true
}

func (wtm *writeToMock) AppendHistograms(h []record.RefHistogramSample) bool {
	wtm.mu.Lock()
	defer wtm.mu.Unlock()

	time.Sleep(wtm.delay)
	wtm.histogramAppends++
	wtm.histogramsAppended = append(wtm.histogramsAppended, h...)
	return true
}

func (wtm *writeToMock) AppendFloatHistograms(fh []record.RefFloatHistogramSample) bool {
	wtm.mu.Lock()
	defer wtm.mu.Unlock()

	time.Sleep(wtm.delay)
	wtm.floatHistogramsAppends++
	wtm.floatHistogramsAppended = append(wtm.floatHistogramsAppended, fh...)
	return true
}

func (wtm *writeToMock) StoreSeries(series []record.RefSeries, index int) {
	wtm.mu.Lock()
	defer wtm.mu.Unlock()

	wtm.seriesStores++
	wtm.seriesStored = append(wtm.seriesStored, series...)
	for _, s := range series {
		wtm.seriesSegmentIndexes[s.Ref] = index
	}
	time.Sleep(wtm.delay)
}

func (wtm *writeToMock) StoreMetadata(meta []record.RefMetadata) {
	wtm.mu.Lock()
	defer wtm.mu.Unlock()

	wtm.metadataStores++
	wtm.metadataStored = append(wtm.metadataStored, meta...)
	time.Sleep(wtm.delay)
}

func (wtm *writeToMock) UpdateSeriesSegment(series []record.RefSeries, index int) {
	wtm.mu.Lock()
	defer wtm.mu.Unlock()

	for _, s := range series {
		wtm.seriesSegmentIndexes[s.Ref] = index
	}
}

func (wtm *writeToMock) SeriesReset(index int) {
	// Check for series that are in segments older than the checkpoint
	// that were not also present in the checkpoint.
	wtm.mu.Lock()
	defer wtm.mu.Unlock()

	for k, v := range wtm.seriesSegmentIndexes {
		if v < index {
			delete(wtm.seriesSegmentIndexes, k)
		}
	}
}

func (wtm *writeToMock) checkNumSeries() int {
	wtm.mu.Lock()
	defer wtm.mu.Unlock()

	return len(wtm.seriesSegmentIndexes)
}

func newWriteToMock(delay time.Duration) *writeToMock {
	return &writeToMock{
		seriesSegmentIndexes: make(map[chunks.HeadSeriesRef]int),
		delay:                delay,
	}
}

func TestWatcher_Tail(t *testing.T) {
	const (
		pageSize           = 32 * 1024
		batches            = 3
		seriesPerBatch     = 100
		exemplarsPerSeries = 2
	)
	for _, enableSTStorage := range []bool{false, true} {
		for _, compress := range compression.Types() {
			t.Run(fmt.Sprintf("compress=%s/stStorage=%v", compress, enableSTStorage), func(t *testing.T) {
				var (
					now  = time.Now()
					dir  = t.TempDir()
					wdir = path.Join(dir, "wal")
					enc  = record.Encoder{EnableSTStorage: enableSTStorage}
				)
				require.NoError(t, os.Mkdir(wdir, 0o777))

				// Generate test records that represents batches of records data.
				// "batch" simulates a single scrape or RW/OTLP receive message.
				// Watcher does not inspect the data other than watching start timestamp, so records
				// does not need any certain shape.
				records := make([]testwal.Records, batches)
				cbHistogramRecords := make([]testwal.Records, batches)
				for i := range records {
					tsFn := func(_, _ int) int64 {
						return timestamp.FromTime(now.Add(1 * time.Second))
					}
					records[i] = testwal.GenerateRecords(testwal.RecordsCase{
						NoST:       !enableSTStorage,
						RefPadding: i * seriesPerBatch,
						TsFn:       tsFn,

						Series:                   seriesPerBatch,
						SamplesPerSeries:         10,
						HistogramsPerSeries:      5,
						FloatHistogramsPerSeries: 5,
						ExemplarsPerSeries:       exemplarsPerSeries,
					})
					cbHistogramRecords[i] = testwal.GenerateRecords(testwal.RecordsCase{
						NoST:       !enableSTStorage,
						RefPadding: i * seriesPerBatch,
						TsFn:       tsFn,

						Series:                   seriesPerBatch,
						HistogramsPerSeries:      5,
						FloatHistogramsPerSeries: 5,
						HistogramFn: func(ref int) *histogram.Histogram {
							return &histogram.Histogram{
								Schema:        -53,
								ZeroThreshold: 1e-128,
								ZeroCount:     0,
								Count:         2,
								Sum:           0,
								PositiveSpans: []histogram.Span{{Offset: 0, Length: 1}},
								CustomValues:  []float64{float64(ref) + 2},
							}
						},
					})
				}

				// Create WAL for writing.
				w, err := NewSize(nil, nil, wdir, 128*pageSize, compress)
				require.NoError(t, err)
				t.Cleanup(func() {
					require.NoError(t, w.Close())
				})

				// Start watcher to that reads into a mock.
				wt := newWriteToMock(0)
				watcher := NewWatcher(wMetrics, nil, nil, "test", wt, dir, true, true, true, nil)
				// Update the time because we just created samples around "now" time and watcher
				// only starts watching after that time.
				watcher.SetStartTime(now)
				// Start spins up watcher loop in a go-routine.
				watcher.Start()
				t.Cleanup(watcher.Stop)

				// Write to WAL like append commit would do, while watcher is tailing.

				// Write first a few samples before the start time, we don't expect those to be appended.
				require.NoError(t, w.Log(enc.Samples([]record.RefSample{
					{Ref: 1, T: timestamp.FromTime(now), V: 123},
					{Ref: 2, T: timestamp.FromTime(now), V: 123.1},
				}, nil)))

				for i := range records {
					// Similar order as tsdb/head_appender.go.headAppenderBase.log
					// https://github.com/prometheus/prometheus/blob/1751685dd4f6430757ba3078a96cffeffcb2bb47/tsdb/head_append.go#L1053
					require.NoError(t, w.Log(enc.Series(records[i].Series, nil)))
					require.NoError(t, w.Log(enc.Metadata(records[i].Metadata, nil)))
					require.NoError(t, w.Log(enc.Samples(records[i].Samples, nil)))

					hs, cbHs := enc.HistogramSamples(records[i].Histograms, nil)
					require.Empty(t, cbHs)
					require.NoError(t, w.Log(hs))
					fhs, cbFhs := enc.FloatHistogramSamples(records[i].FloatHistograms, nil)
					require.Empty(t, cbFhs)
					require.NoError(t, w.Log(fhs))
					require.NoError(t, w.Log(enc.CustomBucketsHistogramSamples(cbHistogramRecords[i].Histograms, nil)))
					require.NoError(t, w.Log(enc.CustomBucketsFloatHistogramSamples(cbHistogramRecords[i].FloatHistograms, nil)))

					require.NoError(t, w.Log(enc.Exemplars(records[i].Exemplars, nil)))

					// Ping watcher for faster test. Watcher is checking for segment changes or 15s timeout.
					watcher.Notify()
				}

				// Wait for watcher to lead all.
				require.Eventually(t, func() bool {
					wt.mu.Lock()
					defer wt.mu.Unlock()

					// Exemplars are logged as the last one, so assert on those.
					return wt.exemplarAppends >= batches
				}, 2*time.Minute, 1*time.Second)

				wt.mu.Lock()
				defer wt.mu.Unlock()

				require.Equal(t, batches, wt.seriesStores)
				require.Equal(t, batches, wt.metadataStores)
				require.Equal(t, batches, wt.sampleAppends)
				require.Equal(t, 2*batches, wt.histogramAppends)
				require.Equal(t, 2*batches, wt.floatHistogramsAppends)
				require.Equal(t, batches, wt.exemplarAppends)

				for i := range batches {
					sector := len(records[i].Series)
					testutil.RequireEqual(t, records[i].Series, wt.seriesStored[i*sector:(i+1)*sector], i)
					sector = len(records[i].Metadata)
					require.Equal(t, records[i].Metadata, wt.metadataStored[i*sector:(i+1)*sector], i)
					sector = len(records[i].Samples)
					require.Equal(t, records[i].Samples, wt.samplesAppended[i*sector:(i+1)*sector], i)

					sector = len(records[i].Histograms) + len(cbHistogramRecords[i].Histograms)
					require.Equal(t, records[i].Histograms, wt.histogramsAppended[i*sector:i*sector+len(records[i].Histograms)], i)
					require.Equal(t, cbHistogramRecords[i].Histograms, wt.histogramsAppended[i*sector+len(records[i].Histograms):(i+1)*sector])
					sector = len(records[i].FloatHistograms) + len(cbHistogramRecords[i].FloatHistograms)
					require.Equal(t, records[i].FloatHistograms, wt.floatHistogramsAppended[i*sector:i*sector+len(records[i].FloatHistograms)])
					require.Equal(t, cbHistogramRecords[i].FloatHistograms, wt.floatHistogramsAppended[i*sector+len(records[i].FloatHistograms):(i+1)*sector])

					sector = len(records[i].Exemplars)
					testutil.RequireEqual(t, records[i].Exemplars, wt.exemplarsAppended[i*sector:(i+1)*sector])
				}
			})
		}
	}
}

func TestReadToEndNoCheckpoint(t *testing.T) {
	pageSize := 32 * 1024
	const seriesCount = 10
	const samplesCount = 250

	for _, enableSTStorage := range []bool{false, true} {
		for _, compress := range compression.Types() {
			t.Run(fmt.Sprintf("compress=%s,stStorage=%v", compress, enableSTStorage), func(t *testing.T) {
				dir := t.TempDir()
				wdir := path.Join(dir, "wal")
				err := os.Mkdir(wdir, 0o777)
				require.NoError(t, err)

				w, err := NewSize(nil, nil, wdir, 128*pageSize, compress)
				require.NoError(t, err)
				defer func() {
					require.NoError(t, w.Close())
				}()

				var recs [][]byte

				enc := record.Encoder{EnableSTStorage: enableSTStorage}

				for i := range seriesCount {
					series := enc.Series([]record.RefSeries{
						{
							Ref:    chunks.HeadSeriesRef(i),
							Labels: labels.FromStrings("__name__", fmt.Sprintf("metric_%d", i)),
						},
					}, nil)
					recs = append(recs, series)
					for j := range samplesCount {
						sample := enc.Samples([]record.RefSample{
							{
								Ref: chunks.HeadSeriesRef(j),
								T:   int64(i),
								V:   float64(i),
							},
						}, nil)

						recs = append(recs, sample)

						// Randomly batch up records.
						if rand.Intn(4) < 3 {
							require.NoError(t, w.Log(recs...))
							recs = recs[:0]
						}
					}
				}
				require.NoError(t, w.Log(recs...))
				overwriteReadTimeout(t, time.Second)
				_, _, err = Segments(w.Dir())
				require.NoError(t, err)

				wt := newWriteToMock(0)
				watcher := NewWatcher(wMetrics, nil, nil, "", wt, dir, false, false, false, nil)
				go watcher.Start()

				expected := seriesCount
				require.Eventually(t, func() bool {
					return wt.checkNumSeries() == expected
				}, 20*time.Second, 1*time.Second)
				watcher.Stop()
			})
		}
	}
}

func TestReadToEndWithCheckpoint(t *testing.T) {
	segmentSize := 32 * 1024
	// We need something similar to this # of series and samples
	// in order to get enough segments for us to checkpoint.
	const seriesCount = 10
	const samplesCount = 250

	for _, enableSTStorage := range []bool{false, true} {
		for _, compress := range compression.Types() {
			t.Run(fmt.Sprintf("compress=%s,stStorage=%v", compress, enableSTStorage), func(t *testing.T) {
				dir := t.TempDir()

				wdir := path.Join(dir, "wal")
				err := os.Mkdir(wdir, 0o777)
				require.NoError(t, err)

				enc := record.Encoder{EnableSTStorage: enableSTStorage}
				w, err := NewSize(nil, nil, wdir, segmentSize, compress)
				require.NoError(t, err)
				defer func() {
					require.NoError(t, w.Close())
				}()

				// Write to the initial segment then checkpoint.
				for i := range seriesCount {
					ref := i + 100
					series := enc.Series([]record.RefSeries{
						{
							Ref:    chunks.HeadSeriesRef(ref),
							Labels: labels.FromStrings("__name__", fmt.Sprintf("metric_%d", i)),
						},
					}, nil)
					require.NoError(t, w.Log(series))
					// Add in an unknown record type, which should be ignored.
					require.NoError(t, w.Log([]byte{255}))

					for range samplesCount {
						inner := rand.Intn(ref + 1)
						sample := enc.Samples([]record.RefSample{
							{
								Ref: chunks.HeadSeriesRef(inner),
								T:   int64(i),
								V:   float64(i),
							},
						}, nil)
						require.NoError(t, w.Log(sample))
					}
				}

				Checkpoint(promslog.NewNopLogger(), w, 0, 1, func(chunks.HeadSeriesRef) bool { return true }, 0, enableSTStorage)
				w.Truncate(1)

				// Write more records after checkpointing.
				for i := range seriesCount {
					series := enc.Series([]record.RefSeries{
						{
							Ref:    chunks.HeadSeriesRef(i),
							Labels: labels.FromStrings("__name__", fmt.Sprintf("metric_%d", i)),
						},
					}, nil)
					require.NoError(t, w.Log(series))

					for j := range samplesCount {
						sample := enc.Samples([]record.RefSample{
							{
								Ref: chunks.HeadSeriesRef(j),
								T:   int64(i),
								V:   float64(i),
							},
						}, nil)
						require.NoError(t, w.Log(sample))
					}
				}

				_, _, err = Segments(w.Dir())
				require.NoError(t, err)
				overwriteReadTimeout(t, time.Second)
				wt := newWriteToMock(0)
				watcher := NewWatcher(wMetrics, nil, nil, "", wt, dir, false, false, false, nil)
				go watcher.Start()

				expected := seriesCount * 2

				require.Eventually(t, func() bool {
					return wt.checkNumSeries() == expected
				}, 10*time.Second, 1*time.Second)
				watcher.Stop()
			})
		}
	}
}

func TestReadCheckpoint(t *testing.T) {
	pageSize := 32 * 1024
	const seriesCount = 10
	const samplesCount = 250

	for _, enableSTStorage := range []bool{false, true} {
		for _, compress := range compression.Types() {
			t.Run(fmt.Sprintf("compress=%s,stStorage=%v", compress, enableSTStorage), func(t *testing.T) {
				dir := t.TempDir()

				wdir := path.Join(dir, "wal")
				err := os.Mkdir(wdir, 0o777)
				require.NoError(t, err)

				f, err := os.Create(SegmentName(wdir, 30))
				require.NoError(t, err)
				require.NoError(t, f.Close())

				enc := record.Encoder{EnableSTStorage: enableSTStorage}
				w, err := NewSize(nil, nil, wdir, 128*pageSize, compress)
				require.NoError(t, err)
				t.Cleanup(func() {
					require.NoError(t, w.Close())
				})

				// Write to the initial segment then checkpoint.
				for i := range seriesCount {
					ref := i + 100
					series := enc.Series([]record.RefSeries{
						{
							Ref:    chunks.HeadSeriesRef(ref),
							Labels: labels.FromStrings("__name__", fmt.Sprintf("metric_%d", i)),
						},
					}, nil)
					require.NoError(t, w.Log(series))

					for range samplesCount {
						inner := rand.Intn(ref + 1)
						sample := enc.Samples([]record.RefSample{
							{
								Ref: chunks.HeadSeriesRef(inner),
								T:   int64(i),
								V:   float64(i),
							},
						}, nil)
						require.NoError(t, w.Log(sample))
					}
				}
				_, err = w.NextSegmentSync()
				require.NoError(t, err)
				_, err = Checkpoint(promslog.NewNopLogger(), w, 30, 31, func(chunks.HeadSeriesRef) bool { return true }, 0, enableSTStorage)
				require.NoError(t, err)
				require.NoError(t, w.Truncate(32))

				// Start read after checkpoint, no more data written.
				_, _, err = Segments(w.Dir())
				require.NoError(t, err)

				wt := newWriteToMock(0)
				watcher := NewWatcher(wMetrics, nil, nil, "", wt, dir, false, false, false, nil)
				go watcher.Start()

				expectedSeries := seriesCount
				retry(t, defaultRetryInterval, defaultRetries, func() bool {
					return wt.checkNumSeries() >= expectedSeries
				})
				watcher.Stop()
				require.Equal(t, expectedSeries, wt.checkNumSeries())
			})
		}
	}
}

func TestReadCheckpointMultipleSegments(t *testing.T) {
	pageSize := 32 * 1024

	const segments = 1
	const seriesCount = 40
	const samplesCount = 500

	for _, enableSTStorage := range []bool{false, true} {
		for _, compress := range compression.Types() {
			t.Run(fmt.Sprintf("compress=%s,stStorage=%v", compress, enableSTStorage), func(t *testing.T) {
				dir := t.TempDir()

				wdir := path.Join(dir, "wal")
				err := os.Mkdir(wdir, 0o777)
				require.NoError(t, err)

				enc := record.Encoder{EnableSTStorage: enableSTStorage}
				w, err := NewSize(nil, nil, wdir, pageSize, compress)
				require.NoError(t, err)

				// Write a bunch of data.
				for i := range segments {
					for j := range seriesCount {
						ref := j + (i * 100)
						series := enc.Series([]record.RefSeries{
							{
								Ref:    chunks.HeadSeriesRef(ref),
								Labels: labels.FromStrings("__name__", fmt.Sprintf("metric_%d", i)),
							},
						}, nil)
						require.NoError(t, w.Log(series))

						for range samplesCount {
							inner := rand.Intn(ref + 1)
							sample := enc.Samples([]record.RefSample{
								{
									Ref: chunks.HeadSeriesRef(inner),
									T:   int64(i),
									V:   float64(i),
								},
							}, nil)
							require.NoError(t, w.Log(sample))
						}
					}
				}
				require.NoError(t, w.Close())

				// At this point we should have at least 6 segments, lets create a checkpoint dir of the first 5.
				checkpointDir := dir + "/wal/checkpoint.000004"
				err = os.Mkdir(checkpointDir, 0o777)
				require.NoError(t, err)
				for i := 0; i <= 4; i++ {
					err := os.Rename(SegmentName(dir+"/wal", i), SegmentName(checkpointDir, i))
					require.NoError(t, err)
				}

				wt := newWriteToMock(0)
				watcher := NewWatcher(wMetrics, nil, nil, "", wt, dir, false, false, false, nil)
				watcher.MaxSegment = -1

				// Set the Watcher's metrics so they're not nil pointers.
				watcher.SetMetrics()

				lastCheckpoint, _, err := LastCheckpoint(watcher.walDir)
				require.NoError(t, err)

				err = watcher.readCheckpoint(lastCheckpoint, (*Watcher).readSegment)
				require.NoError(t, err)
			})
		}
	}
}

func TestCheckpointSeriesReset(t *testing.T) {
	segmentSize := 64 * 1024
	// We need something similar to this # of series and samples
	// in order to get enough segments for us to checkpoint.
	const seriesCount = 30
	const samplesCount = 700
	testCases := []struct {
		compress        compression.Type
		enableSTStorage bool
		segments        int
	}{
		{compress: compression.None, enableSTStorage: false, segments: 24},
		{compress: compression.Snappy, enableSTStorage: false, segments: 23},
		{compress: compression.None, enableSTStorage: true, segments: 20},
		{compress: compression.Snappy, enableSTStorage: true, segments: 20},
	}

	dir := t.TempDir()
	for _, tc := range testCases {
		t.Run(fmt.Sprintf("compress=%s,stStorage=%v", tc.compress, tc.enableSTStorage), func(t *testing.T) {
			subdir := filepath.Join(dir, fmt.Sprintf("%s-%v", tc.compress, tc.enableSTStorage))
			err := os.MkdirAll(subdir, 0o777)
			require.NoError(t, err)
			wdir := filepath.Join(subdir, "wal")
			err = os.MkdirAll(wdir, 0o777)
			require.NoError(t, err)

			enc := record.Encoder{EnableSTStorage: tc.enableSTStorage}
			w, err := NewSize(nil, nil, wdir, segmentSize, tc.compress)
			require.NoError(t, err)
			defer func() {
				require.NoError(t, w.Close())
			}()

			// Write to the initial segment, then checkpoint later.
			for i := range seriesCount {
				ref := i + 100
				series := enc.Series([]record.RefSeries{
					{
						Ref:    chunks.HeadSeriesRef(ref),
						Labels: labels.FromStrings("__name__", fmt.Sprintf("metric_%d", i)),
					},
				}, nil)
				require.NoError(t, w.Log(series))

				for range samplesCount {
					inner := rand.Intn(ref + 1)
					sample := enc.Samples([]record.RefSample{
						{
							Ref: chunks.HeadSeriesRef(inner),
							T:   int64(i),
							V:   float64(i),
						},
					}, nil)
					require.NoError(t, w.Log(sample))
				}
			}

			_, _, err = Segments(w.Dir())
			require.NoError(t, err)

			overwriteReadTimeout(t, time.Second)
			wt := newWriteToMock(0)
			watcher := NewWatcher(wMetrics, nil, nil, "", wt, subdir, false, false, false, nil)
			watcher.MaxSegment = -1
			go watcher.Start()

			expected := seriesCount
			retry(t, defaultRetryInterval, defaultRetries, func() bool {
				return wt.checkNumSeries() >= expected
			})
			require.Eventually(t, func() bool {
				return wt.checkNumSeries() == seriesCount
			}, 10*time.Second, 1*time.Second)

			_, err = Checkpoint(promslog.NewNopLogger(), w, 2, 4, func(chunks.HeadSeriesRef) bool { return true }, 0, true)
			require.NoError(t, err)

			err = w.Truncate(5)
			require.NoError(t, err)

			_, cpi, err := LastCheckpoint(wdir)
			require.NoError(t, err)
			err = watcher.garbageCollectSeries(cpi + 1)
			require.NoError(t, err)

			watcher.Stop()
			// If you modify the checkpoint and truncate segment #'s run the test to see how
			// many series records you end up with and change the last Equals check accordingly
			// or modify the Equals to Assert(len(wt.seriesLabels) < seriesCount*10)
			require.Eventually(t, func() bool {
				return wt.checkNumSeries() == tc.segments
			}, 20*time.Second, 1*time.Second)
		})
	}
}

func TestRun_StartupTime(t *testing.T) {
	const pageSize = 32 * 1024
	const segments = 20
	const seriesCount = 40
	const samplesCount = 500

	for _, enableSTStorage := range []bool{false, true} {
		for _, compress := range compression.Types() {
			t.Run(fmt.Sprintf("compress=%s,stStorage=%v", compress, enableSTStorage), func(t *testing.T) {
				dir := t.TempDir()

				wdir := path.Join(dir, "wal")
				err := os.Mkdir(wdir, 0o777)
				require.NoError(t, err)

				enc := record.Encoder{EnableSTStorage: enableSTStorage}
				w, err := NewSize(nil, nil, wdir, pageSize, compress)
				require.NoError(t, err)

				for i := range segments {
					for j := range seriesCount {
						ref := j + (i * 100)
						series := enc.Series([]record.RefSeries{
							{
								Ref:    chunks.HeadSeriesRef(ref),
								Labels: labels.FromStrings("__name__", fmt.Sprintf("metric_%d", i)),
							},
						}, nil)
						require.NoError(t, w.Log(series))

						for range samplesCount {
							inner := rand.Intn(ref + 1)
							sample := enc.Samples([]record.RefSample{
								{
									Ref: chunks.HeadSeriesRef(inner),
									T:   int64(i),
									V:   float64(i),
								},
							}, nil)
							require.NoError(t, w.Log(sample))
						}
					}
				}
				require.NoError(t, w.Close())

				wt := newWriteToMock(0)
				watcher := NewWatcher(wMetrics, nil, nil, "", wt, dir, false, false, false, nil)
				watcher.MaxSegment = segments

				watcher.SetMetrics()
				startTime := time.Now()

				err = watcher.Run()
				require.Less(t, time.Since(startTime), readTimeout)
				require.NoError(t, err)
			})
		}
	}
}

func generateWALRecords(w *WL, segment, seriesCount, samplesCount int, enableSTStorage bool) error {
	enc := record.Encoder{EnableSTStorage: enableSTStorage}
	for j := range seriesCount {
		ref := j + (segment * 100)
		series := enc.Series([]record.RefSeries{
			{
				Ref:    chunks.HeadSeriesRef(ref),
				Labels: labels.FromStrings("__name__", fmt.Sprintf("metric_%d", segment)),
			},
		}, nil)
		if err := w.Log(series); err != nil {
			return err
		}

		for range samplesCount {
			inner := rand.Intn(ref + 1)
			sample := enc.Samples([]record.RefSample{
				{
					Ref: chunks.HeadSeriesRef(inner),
					T:   int64(segment),
					V:   float64(segment),
				},
			}, nil)
			if err := w.Log(sample); err != nil {
				return err
			}
		}
	}
	return nil
}

func TestRun_AvoidNotifyWhenBehind(t *testing.T) {
	if runtime.GOOS == "windows" { // Takes a really long time, perhaps because min sleep time is 15ms.
		t.SkipNow()
	}
	const segmentSize = pageSize // Smallest allowed segment size.
	const segmentsToWrite = 5
	const segmentsToRead = segmentsToWrite - 1
	const seriesCount = 10
	const samplesCount = 50

	for _, enableSTStorage := range []bool{false, true} {
		for _, compress := range compression.Types() {
			t.Run(fmt.Sprintf("compress=%s,stStorage=%v", compress, enableSTStorage), func(t *testing.T) {
				dir := t.TempDir()

				wdir := path.Join(dir, "wal")
				err := os.Mkdir(wdir, 0o777)
				require.NoError(t, err)

				w, err := NewSize(nil, nil, wdir, segmentSize, compress)
				require.NoError(t, err)
				// Write to 00000000, the watcher will read series from it.
				require.NoError(t, generateWALRecords(w, 0, seriesCount, samplesCount, enableSTStorage))
				// Create 00000001, the watcher will tail it once started.
				w.NextSegment()

				// Set up the watcher and run it in the background.
				wt := newWriteToMock(time.Millisecond)
				watcher := NewWatcher(wMetrics, nil, nil, "", wt, dir, false, false, false, nil)
				watcher.SetMetrics()
				watcher.MaxSegment = segmentsToRead

				var g errgroup.Group
				g.Go(func() error {
					startTime := time.Now()
					err = watcher.Run()
					if err != nil {
						return err
					}
					// If the watcher was to wait for readTicker to read every new segment, it would need readTimeout * segmentsToRead.
					d := time.Since(startTime)
					if d > readTimeout {
						return fmt.Errorf("watcher ran for %s, it shouldn't rely on readTicker=%s to read the new segments", d, readTimeout)
					}
					return nil
				})

				// The watcher went through 00000000 and is tailing the next one.
				retry(t, defaultRetryInterval, defaultRetries, func() bool {
					return wt.checkNumSeries() == seriesCount
				})

				// In the meantime, add some new segments in bulk.
				// We should end up with segmentsToWrite + 1 segments now.
				for i := 1; i < segmentsToWrite; i++ {
					require.NoError(t, generateWALRecords(w, i, seriesCount, samplesCount, enableSTStorage))
					w.NextSegment()
				}

				// Wait for the watcher.
				require.NoError(t, g.Wait())

				// All series and samples were read.
				require.Equal(t, (segmentsToRead+1)*seriesCount, wt.checkNumSeries()) // Series from 00000000 are also read.
				require.Len(t, wt.samplesAppended, segmentsToRead*seriesCount*samplesCount)
				require.NoError(t, w.Close())
			})
		}
	}
}

// nativeWriteToMock records native metadata entries since its last reset.
type nativeWriteToMock struct {
	*writeToMock
	entries []record.RefNativeMetadata
	resets  int
	onStore func([]record.RefNativeMetadata)
}

func (m *nativeWriteToMock) stored() []record.RefNativeMetadata { return m.entries }

func (m *nativeWriteToMock) StoreNativeMetadata(entries []record.RefNativeMetadata) {
	if m.onStore != nil {
		m.onStore(entries)
	}
	for _, e := range entries {
		e.Points = append([]record.RefNativeMetadataPoint(nil), e.Points...)
		m.entries = append(m.entries, e)
	}
}

func (m *nativeWriteToMock) ResetNativeMetadata() {
	m.entries = nil
	m.resets++
}

// nativeBorrowWriteToMock accepts borrowed entries, copying their strings.
type nativeBorrowWriteToMock struct {
	*nativeWriteToMock
	borrowed, copied int
}

func (m *nativeBorrowWriteToMock) StoreNativeMetadata(entries []record.RefNativeMetadata) {
	m.copied++
	m.nativeWriteToMock.StoreNativeMetadata(entries)
}

func (m *nativeBorrowWriteToMock) StoreBorrowedNativeMetadata(entries []record.RefNativeMetadata) {
	m.borrowed++
	for _, e := range entries {
		e.Points = append([]record.RefNativeMetadataPoint(nil), e.Points...)
		for i := range e.Points {
			e.Points[i].Unit, e.Points[i].Help = strings.Clone(e.Points[i].Unit), strings.Clone(e.Points[i].Help)
		}
		m.entries = append(m.entries, e)
	}
}

// nativeRecordWriteToMock accepts compact records whole, copying their strings.
type nativeRecordWriteToMock struct {
	*nativeBorrowWriteToMock
	records int
}

func (m *nativeRecordWriteToMock) StoreBorrowedNativeMetadataRecord(rec *record.CompactNativeMetadata) {
	m.records++
	owned := record.CompactNativeMetadata{Entries: rec.Entries}
	for _, v := range rec.Values {
		owned.Values = append(owned.Values, record.NativeMetadataValue{Type: v.Type, Unit: strings.Clone(v.Unit), Help: strings.Clone(v.Help)})
	}
	entries, _ := owned.AppendNativeMetadata(nil, nil)
	m.entries = append(m.entries, entries...)
}

func reduceNativeMetadata(entries []record.RefNativeMetadata) map[chunks.HeadSeriesRef]string {
	states := map[chunks.HeadSeriesRef]*nativemetadata.State{}
	intern := func(m metadata.Metadata) *metadata.Metadata { return &m }
	for _, e := range entries {
		if e.Ignored() {
			continue
		}
		if states[e.Ref] == nil {
			states[e.Ref] = &nativemetadata.State{}
		}
		states[e.Ref].Apply(e.Kind, e.Truncated, nativemetadata.AppendRecordPoints(nil, e.Points, intern))
	}
	out := map[chunks.HeadSeriesRef]string{}
	for ref, s := range states {
		out[ref] = fmt.Sprintf("truncated=%v", s.Truncated)
		for _, p := range s.AppendPoints(nil) {
			out[ref] += fmt.Sprintf(" %s@%d", p.Metadata.Help, p.EffectiveFrom)
		}
	}
	return out
}

func TestWatcher_NativeMetadata(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("compact=%t", compact), func(t *testing.T) {
			testWatcherNativeMetadata(t, compact)
		})
	}

	t.Run("owned strings outlive the call", func(t *testing.T) {
		// One segment of many records, whose reader reuses its record buffer.
		dir := t.TempDir()
		w, err := NewSize(nil, nil, filepath.Join(dir, "wal"), 1<<20, compression.None)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		var enc record.Encoder
		require.NoError(t, w.Log(enc.Series([]record.RefSeries{{Ref: 1, Labels: labels.FromStrings("__name__", "a")}}, nil)))
		var want []record.RefNativeMetadata
		for i := range 200 {
			help := strings.Repeat(strconv.Itoa(i), 1+i%7)
			values := []record.NativeMetadataValue{{Type: uint8(record.Gauge), Unit: "u" + help, Help: help}}
			points := []record.CompactNativeMetadataPoint{{EffectiveFrom: int64(2 * i)}, {EffectiveFrom: int64(2*i + 1)}}
			rec := enc.CompactNativeMetadata(values, []record.RefCompactNativeMetadata{{Ref: 1, Kind: record.NativeMetadataGroup, Points: points}}, nil)
			require.NoError(t, w.Log(rec))
			want = append(want, decodeNativeMetadataForTest(t, rec)...)
		}
		_, err = w.NextSegment()
		require.NoError(t, err)

		wt := &nativeWriteToMock{writeToMock: newWriteToMock(0)}
		var kept, clones []string
		wt.onStore = func(entries []record.RefNativeMetadata) {
			for _, e := range entries {
				// Points of one value share its one copy.
				require.Same(t, unsafe.StringData(e.Points[0].Help), unsafe.StringData(e.Points[1].Help))
				for _, p := range e.Points {
					kept = append(kept, p.Unit, p.Help)
					clones = append(clones, strings.Clone(p.Unit), strings.Clone(p.Help))
				}
			}
			// Strings this writer kept from earlier records are unchanged.
			require.Equal(t, clones, kept)
		}
		watcher := NewWatcher(wMetrics, nil, nil, "", wt, dir, false, false, true, nil)
		watcher.SetMetrics()
		watcher.MaxSegment = 0
		require.NoError(t, watcher.Run())
		require.Equal(t, want, wt.entries)
		require.Equal(t, clones, kept)
	})

	t.Run("legacy writers read empty overrides as empty metadata", func(t *testing.T) {
		// As they read Metadata records' native entries. Entries of unknown
		// kind without points reach them not at all.
		dir := t.TempDir()
		w, err := NewSize(nil, nil, filepath.Join(dir, "wal"), 32*1024, compression.None)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		var enc record.Encoder
		require.NoError(t, w.Log(enc.Series([]record.RefSeries{{Ref: 1, Labels: labels.FromStrings("__name__", "a")}, {Ref: 2, Labels: labels.FromStrings("__name__", "b")}}, nil)))
		require.NoError(t, w.Log(enc.CompactNativeMetadata(nil, []record.RefCompactNativeMetadata{{Ref: 1, Kind: record.NativeMetadataOverride}}, nil)))
		require.NoError(t, w.Log(unknownCompactNativeMetadataForTest(2, "", 0)))
		_, err = w.NextSegment()
		require.NoError(t, err)
		wt := newWriteToMock(0)
		watcher := NewWatcher(wMetrics, nil, nil, "", wt, dir, false, false, true, nil)
		watcher.SetMetrics()
		watcher.MaxSegment = 0
		require.NoError(t, watcher.Run())
		require.Equal(t, []record.RefMetadata{{Ref: 1}}, wt.metadataStored)
		// The Metadata record equivalent.
		legacy, err := (&record.Decoder{}).Metadata(enc.NativeMetadata([]record.RefNativeMetadata{{Ref: 1, Kind: record.NativeMetadataOverride}}, nil), nil)
		require.NoError(t, err)
		require.Equal(t, wt.metadataStored, legacy)
	})

	t.Run("ignored entries reach native writers", func(t *testing.T) {
		dir := t.TempDir()
		w, err := NewSize(nil, nil, filepath.Join(dir, "wal"), 32*1024, compression.None)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		var enc record.Encoder
		require.NoError(t, w.Log(enc.Series([]record.RefSeries{{Ref: 1, Labels: labels.FromStrings("__name__", "a")}}, nil)))
		// A compact record of one entry of unknown kind 7, without points.
		require.NoError(t, w.Log([]byte{byte(record.NativeMetadataCompact), 1, 0, 0, 1, 7, 2, 0}))
		_, err = w.NextSegment()
		require.NoError(t, err)
		for _, wt := range []interface {
			WriteTo
			stored() []record.RefNativeMetadata
		}{
			&nativeWriteToMock{writeToMock: newWriteToMock(0)},
			&nativeBorrowWriteToMock{nativeWriteToMock: &nativeWriteToMock{writeToMock: newWriteToMock(0)}},
			&nativeRecordWriteToMock{nativeBorrowWriteToMock: &nativeBorrowWriteToMock{nativeWriteToMock: &nativeWriteToMock{writeToMock: newWriteToMock(0)}}},
		} {
			watcher := NewWatcher(wMetrics, nil, nil, "", wt, dir, false, false, true, nil)
			watcher.SetMetrics()
			watcher.MaxSegment = 0
			require.NoError(t, watcher.Run())
			stored := wt.stored()
			require.Len(t, stored, 1)
			require.True(t, stored[0].Ignored())
			require.Equal(t, chunks.HeadSeriesRef(1), stored[0].Ref)
		}
	})
}

func testWatcherNativeMetadata(t *testing.T, compact bool) {
	var enc record.Encoder
	group := func(ref chunks.HeadSeriesRef, points ...string) []byte {
		e := record.RefNativeMetadata{Ref: ref, Kind: record.NativeMetadataGroup}
		c := record.RefCompactNativeMetadata{Ref: ref, Kind: record.NativeMetadataGroup}
		var values []record.NativeMetadataValue
		for _, p := range points {
			var help string
			var from int64
			_, err := fmt.Sscanf(p, "%1s@%d", &help, &from)
			require.NoError(t, err)
			e.Points = append(e.Points, record.RefNativeMetadataPoint{EffectiveFrom: from, Type: uint8(record.Gauge), Help: help})
			c.Points = append(c.Points, record.CompactNativeMetadataPoint{EffectiveFrom: from, Value: uint32(len(values))})
			values = append(values, record.NativeMetadataValue{Type: uint8(record.Gauge), Help: help})
		}
		if compact {
			return enc.CompactNativeMetadata(values, []record.RefCompactNativeMetadata{c}, nil)
		}
		return enc.NativeMetadata([]record.RefNativeMetadata{e}, nil)
	}
	// Each segment holds one series record and the listed records. Compact
	// WALs keep one legacy Metadata record, so they mix both types.
	segments := [][][]byte{
		{group(1, "A@100"), group(2, "A@10", "B@20", "C@30", "D@40", "E@50")},
		{group(1, "B@150", "C@180"), group(2, "F@60")},
		{enc.Metadata([]record.RefMetadata{{Ref: 3, Help: "L"}}, nil), group(1, "D@200")},
		{group(2, "D@50"), group(3, "M@5")},
		{group(1, "A@120"), group(2, "G@70")},
	}
	type setup struct {
		dir     string
		w       *WL
		entries []record.RefNativeMetadata
	}
	newWAL := func(t *testing.T) setup {
		dir := t.TempDir()
		w, err := NewSize(nil, nil, filepath.Join(dir, "wal"), 32*1024, compression.None)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, w.Close()) })
		var all []record.RefNativeMetadata
		for i, recs := range segments {
			require.NoError(t, w.Log(enc.Series([]record.RefSeries{{Ref: chunks.HeadSeriesRef(i + 1), Labels: labels.FromStrings("__name__", strconv.Itoa(i))}}, nil)))
			for _, rec := range recs {
				require.NoError(t, w.Log(rec))
				all = append(all, decodeNativeMetadataForTest(t, rec)...)
			}
			// Leave an empty tail segment, so that replay reads every segment
			// to its end and returns.
			_, err := w.NextSegment()
			require.NoError(t, err)
		}
		return setup{dir: dir, w: w, entries: all}
	}
	newWatcher := func(s setup) (*Watcher, *nativeWriteToMock) {
		wt := &nativeWriteToMock{writeToMock: newWriteToMock(0)}
		watcher := NewWatcher(wMetrics, nil, nil, "", wt, s.dir, false, false, true, nil)
		watcher.SetMetrics()
		watcher.MaxSegment = len(segments) - 1
		return watcher, wt
	}
	checkpoint := func(t *testing.T, s setup, from, to int) {
		_, err := Checkpoint(promslog.NewNopLogger(), s.w, from, to, func(chunks.HeadSeriesRef) bool { return true }, 0, false)
		require.NoError(t, err)
	}

	t.Run("entries are forwarded in WAL order", func(t *testing.T) {
		s := newWAL(t)
		watcher, wt := newWatcher(s)
		require.NoError(t, watcher.Run())
		require.Equal(t, s.entries, wt.entries)
		require.Zero(t, wt.metadataStores, "legacy metadata is forwarded as native entries")
	})

	t.Run("each replay starts from reset state", func(t *testing.T) {
		for _, withCheckpoint := range []bool{false, true} {
			s := newWAL(t)
			if withCheckpoint {
				checkpoint(t, s, 0, 1)
				require.NoError(t, s.w.Truncate(2))
			}
			watcher, wt := newWatcher(s)
			require.NoError(t, watcher.Run())
			first := wt.entries
			require.NoError(t, watcher.Run())
			require.Equal(t, 2, wt.resets)
			require.Equal(t, first, wt.entries)
			require.Equal(t, reduceNativeMetadata(s.entries), reduceNativeMetadata(wt.entries))
		}
	})

	t.Run("replay starts after the checkpoint's own segment", func(t *testing.T) {
		// A failed truncation leaves the checkpointed segments in place.
		s := newWAL(t)
		checkpoint(t, s, 0, 1)
		watcher, wt := newWatcher(s)
		require.NoError(t, watcher.Run())
		var segment1 []record.RefNativeMetadata
		for _, rec := range segments[1] {
			segment1 = append(segment1, decodeNativeMetadataForTest(t, rec)...)
		}
		for _, e := range wt.entries {
			for _, skipped := range segment1 {
				require.NotEqual(t, skipped, e, "segment 1 is in the checkpoint")
			}
		}
		require.Equal(t, reduceNativeMetadata(s.entries), reduceNativeMetadata(wt.entries))
	})

	t.Run("a newer checkpoint restarts replay", func(t *testing.T) {
		s := newWAL(t)
		checkpoint(t, s, 0, 1)
		require.NoError(t, s.w.Truncate(2))
		watcher, wt := newWatcher(s)
		raced := false
		watcher.testAfterCheckpoint = func() {
			if raced {
				return
			}
			raced = true
			// Checkpointing races the watcher, deleting the next segments.
			checkpoint(t, s, 2, 3)
			require.NoError(t, s.w.Truncate(4))
			require.NoError(t, DeleteCheckpoints(s.w.Dir(), 3))
		}
		require.NoError(t, watcher.Run())
		require.Equal(t, 2, wt.resets)
		_, index, err := LastCheckpoint(s.w.Dir())
		require.NoError(t, err)
		require.Equal(t, 3, index)
		require.Equal(t, reduceNativeMetadata(s.entries), reduceNativeMetadata(wt.entries))
	})

	t.Run("missing segments without a newer checkpoint", func(t *testing.T) {
		s := newWAL(t)
		checkpoint(t, s, 0, 1)
		require.NoError(t, s.w.Truncate(2))
		require.NoError(t, os.Remove(SegmentName(s.w.Dir(), 2)))
		watcher, wt := newWatcher(s)
		require.NoError(t, watcher.Run())
		require.Equal(t, 1, wt.resets)
		require.NotEmpty(t, wt.entries)
	})

	t.Run("writers that accept borrowed entries get them", func(t *testing.T) {
		s := newWAL(t)
		wt := &nativeBorrowWriteToMock{nativeWriteToMock: &nativeWriteToMock{writeToMock: newWriteToMock(0)}}
		watcher := NewWatcher(wMetrics, nil, nil, "", wt, s.dir, false, false, true, nil)
		watcher.SetMetrics()
		watcher.MaxSegment = len(segments) - 1
		require.NoError(t, watcher.Run())
		require.Equal(t, s.entries, wt.entries)
		require.Positive(t, wt.borrowed)
		require.Zero(t, wt.copied, "borrowing writers never get copied entries")
	})

	t.Run("writers that accept records get them", func(t *testing.T) {
		s := newWAL(t)
		wt := &nativeRecordWriteToMock{nativeBorrowWriteToMock: &nativeBorrowWriteToMock{nativeWriteToMock: &nativeWriteToMock{writeToMock: newWriteToMock(0)}}}
		watcher := NewWatcher(wMetrics, nil, nil, "", wt, s.dir, false, false, true, nil)
		watcher.SetMetrics()
		watcher.MaxSegment = len(segments) - 1
		require.NoError(t, watcher.Run())
		require.Equal(t, s.entries, wt.entries)
		require.Zero(t, wt.copied, "borrowing writers never get copied entries")
		if compact {
			// Only the legacy record arrives as entries.
			require.Equal(t, 9, wt.records)
			require.Equal(t, 1, wt.borrowed)
		} else {
			require.Zero(t, wt.records)
			require.Equal(t, 10, wt.borrowed)
		}
	})

	t.Run("legacy writers keep legacy metadata", func(t *testing.T) {
		s := newWAL(t)
		wt := newWriteToMock(0)
		watcher := NewWatcher(wMetrics, nil, nil, "", wt, s.dir, false, false, true, nil)
		watcher.SetMetrics()
		watcher.MaxSegment = len(segments) - 1
		require.NoError(t, watcher.Run())
		require.Len(t, wt.metadataStored, len(s.entries))
	})
}

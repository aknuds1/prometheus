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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	remoteapi "github.com/prometheus/client_golang/exp/api/remote"
	client_testutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/exemplar"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/model/relabel"
	writev2 "github.com/prometheus/prometheus/prompb/io/prometheus/write/v2"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/util/compression"
)

func nativeMetadataPoint(from int64, m metadata.Metadata) record.RefNativeMetadataPoint {
	return record.RefNativeMetadataPoint{EffectiveFrom: from, Type: record.GetMetricType(m.Type), Unit: m.Unit, Help: m.Help}
}

func nativeMetadataEntry(ref chunks.HeadSeriesRef, kind record.NativeMetadataKind, points ...record.RefNativeMetadataPoint) []record.RefNativeMetadata {
	return []record.RefNativeMetadata{{Ref: ref, Kind: kind, Points: points}}
}

// queuedMetadata returns the values of the queued items' metadata, nil for none.
func queuedMetadata(batch []timeSeries) []*metadata.Metadata {
	out := make([]*metadata.Metadata, len(batch))
	for i, s := range batch {
		if s.metadata != nil {
			m := *s.metadata
			out[i] = &m
		}
	}
	return out
}

func TestQueueManagerNativeMetadata(t *testing.T) {
	m := func(help string) metadata.Metadata {
		return metadata.Metadata{Type: model.MetricTypeCounter, Unit: "seconds", Help: help}
	}
	a, b, c := m("a"), m("b"), m("c")
	series := []record.RefSeries{{Ref: 1, Labels: labels.FromStrings(labels.MetricName, "metric")}}
	newQM := func(t *testing.T) (*QueueManager, nativeMetadataWriter, *queue) {
		qm := newTestQueueManager(t, testDefaultQueueConfig(), config.DefaultMetadataConfig, defaultFlushDeadline, NewNopWriteClient(), remoteapi.WriteV2MessageType, true)
		qm.sendExemplars, qm.sendNativeHistograms = true, true
		qm.StoreSeries(series, 0)
		// Inspect the queued values before a sender can consume them.
		q := newQueue(64, 64)
		qm.shards.queues = []*queue{q}
		return qm, nativeMetadataWriter{qm}, q
	}

	t.Run("items are labelled at their own timestamps", func(t *testing.T) {
		timestamps := []int64{50, 100, 150, 200, 250, 300, 1000, 120}
		want := []*metadata.Metadata{nil, &a, &a, &b, &b, &c, &c, &a}
		for _, kind := range []string{"samples", "exemplars", "histograms", "float histograms"} {
			t.Run(kind, func(t *testing.T) {
				qm, w, q := newQM(t)
				// One transaction carries A@100 and B@200, a later one C@300.
				w.StoreNativeMetadata(nativeMetadataEntry(1, record.NativeMetadataGroup, nativeMetadataPoint(100, a), nativeMetadataPoint(200, b)))
				w.StoreNativeMetadata(nativeMetadataEntry(1, record.NativeMetadataGroup, nativeMetadataPoint(300, c)))
				for _, ts := range timestamps {
					var ok bool
					switch kind {
					case "samples":
						ok = qm.Append([]record.RefSample{{Ref: 1, T: ts}})
					case "exemplars":
						ok = qm.AppendExemplars([]record.RefExemplar{{Ref: 1, T: ts}})
					case "histograms":
						ok = qm.AppendHistograms([]record.RefHistogramSample{{Ref: 1, T: ts, H: &histogram.Histogram{}}})
					case "float histograms":
						ok = qm.AppendFloatHistograms([]record.RefFloatHistogramSample{{Ref: 1, T: ts, FH: &histogram.FloatHistogram{}}})
					}
					require.True(t, ok)
				}
				require.Equal(t, want, queuedMetadata(q.batch))
			})
		}
	})

	t.Run("entry kinds", func(t *testing.T) {
		qm, w, q := newQM(t)
		w.StoreNativeMetadata(nativeMetadataEntry(1, record.NativeMetadataLegacy, nativeMetadataPoint(math.MinInt64, a)))
		require.True(t, qm.Append([]record.RefSample{{Ref: 1, T: math.MinInt64 + 1}}))
		w.StoreNativeMetadata(nativeMetadataEntry(1, record.NativeMetadataOverride, nativeMetadataPoint(100, b)))
		require.True(t, qm.Append([]record.RefSample{{Ref: 1, T: 99}, {Ref: 1, T: 100}}))
		w.StoreNativeMetadata(nativeMetadataEntry(1, record.NativeMetadataUnknown, nativeMetadataPoint(200, c)))
		require.True(t, qm.Append([]record.RefSample{{Ref: 1, T: 150}, {Ref: 1, T: 200}}))
		require.Equal(t, 1.0, client_testutil.ToFloat64(qm.metrics.unknownMetadataTotal))
		w.StoreNativeMetadata(nativeMetadataEntry(1, record.NativeMetadataOverride))
		require.True(t, qm.Append([]record.RefSample{{Ref: 1, T: 300}}))
		// Ref 0 never identifies a series.
		w.StoreNativeMetadata(nativeMetadataEntry(0, record.NativeMetadataGroup, nativeMetadataPoint(1, a)))
		require.NotContains(t, qm.seriesNativeMetadata, chunks.HeadSeriesRef(0))
		// Legacy metadata is ignored when forwarding native metadata.
		qm.StoreMetadata([]record.RefMetadata{{Ref: 1, Type: record.GetMetricType(c.Type), Help: "legacy"}})
		require.True(t, qm.Append([]record.RefSample{{Ref: 1, T: 400}}))
		require.Equal(t, []*metadata.Metadata{&a, nil, &b, nil, &c, nil, nil}, queuedMetadata(q.batch))
	})

	t.Run("resets and series garbage collection", func(t *testing.T) {
		qm, w, q := newQM(t)
		w.StoreNativeMetadata(nativeMetadataEntry(1, record.NativeMetadataGroup, nativeMetadataPoint(100, a)))
		// Entries may precede their series record.
		w.StoreNativeMetadata(nativeMetadataEntry(2, record.NativeMetadataGroup, nativeMetadataPoint(100, b)))
		qm.StoreSeries([]record.RefSeries{{Ref: 2, Labels: labels.FromStrings(labels.MetricName, "other")}}, 1)
		require.True(t, qm.Append([]record.RefSample{{Ref: 1, T: 100}, {Ref: 2, T: 100}}))
		qm.SeriesReset(1)
		require.NotContains(t, qm.seriesNativeMetadata, chunks.HeadSeriesRef(1))
		require.Contains(t, qm.seriesNativeMetadata, chunks.HeadSeriesRef(2))
		w.ResetNativeMetadata()
		require.Empty(t, qm.seriesNativeMetadata)
		require.True(t, qm.Append([]record.RefSample{{Ref: 2, T: 100}}))
		require.Equal(t, []*metadata.Metadata{&a, &b, nil}, queuedMetadata(q.batch))
	})

	t.Run("queues share values", func(t *testing.T) {
		_, first, _ := newQM(t)
		_, second, _ := newQM(t)
		help := "shared " + t.Name()
		for _, w := range []nativeMetadataWriter{first, second} {
			w.StoreNativeMetadata(nativeMetadataEntry(1, record.NativeMetadataGroup, nativeMetadataPoint(1, m(strings.Clone(help)))))
		}
		require.Same(t, first.seriesNativeMetadata[1].Metadata, second.seriesNativeMetadata[1].Metadata)
	})

	t.Run("unknown series and old samples are filtered", func(t *testing.T) {
		qm, w, q := newQM(t)
		w.StoreNativeMetadata(nativeMetadataEntry(1, record.NativeMetadataGroup, nativeMetadataPoint(0, a)))
		require.True(t, qm.Append([]record.RefSample{{Ref: 2, T: 1}}))
		qm.cfg.SampleAgeLimit = model.Duration(time.Hour)
		require.True(t, qm.Append([]record.RefSample{{Ref: 1, T: 1}}))
		require.Empty(t, q.batch)
	})

	t.Run("protocols and destinations", func(t *testing.T) {
		for _, native := range []bool{false, true} {
			s := NewWriteStorage(nil, nil, t.TempDir(), defaultFlushDeadline, nil, false, native)
			rw1, rw2 := baseRemoteWriteConfig("http://rw1.test"), baseRemoteWriteConfig("http://rw2.test")
			rw1.ProtobufMessage, rw2.ProtobufMessage = remoteapi.WriteV1MessageType, remoteapi.WriteV2MessageType
			rw1.MetadataConfig.Send, rw2.MetadataConfig.Send = false, false
			cfg := &config.Config{RemoteWriteConfigs: []*config.RemoteWriteConfig{rw1, rw2}}
			require.NoError(t, s.ApplyConfig(cfg))
			original := make(map[*QueueManager]bool)
			check := func() {
				for _, qm := range s.queues {
					want := native && qm.protoMsg == remoteapi.WriteV2MessageType
					require.Equal(t, want, qm.nativeMetadata)
					require.Equal(t, qm.protoMsg == remoteapi.WriteV2MessageType, qm.shards.nextReady != nil)
				}
			}
			check()
			for _, qm := range s.queues {
				original[qm] = true
			}
			rw1.ProtobufMessage, rw2.ProtobufMessage = rw2.ProtobufMessage, rw1.ProtobufMessage
			require.NoError(t, s.ApplyConfig(cfg))
			require.Len(t, s.queues, 2)
			for _, qm := range s.queues {
				require.False(t, original[qm], "protocol changes must create a new fixed policy")
			}
			check()
			require.NoError(t, s.Close())
		}
	})
}

func TestQueueManagerNativeMetadataRetryAndReshard(t *testing.T) {
	old := metadata.Metadata{Type: model.MetricTypeCounter, Help: "old"}
	newest := metadata.Metadata{Type: model.MetricTypeCounter, Help: "new"}
	var w nativeMetadataWriter
	requests := make(chan []byte, 3)
	attempts := 0
	client := &MockWriteClient{
		NameFunc: func() string { return "test" }, EndpointFunc: func() string { return "http://test" },
		StoreFunc: func(_ context.Context, req []byte, _ int) (WriteResponseStats, error) {
			requests <- bytes.Clone(req)
			attempts++
			if attempts == 1 {
				// Later WAL entries must not change queued items.
				w.StoreNativeMetadata(nativeMetadataEntry(1, record.NativeMetadataOverride, nativeMetadataPoint(0, newest)))
				return WriteResponseStats{}, RecoverableError{errors.New("retry"), 0}
			}
			return WriteResponseStats{Samples: 1, Confirmed: true}, nil
		},
	}
	cfg := testDefaultQueueConfig()
	cfg.MaxSamplesPerSend = 1
	qm := newTestQueueManager(t, cfg, config.DefaultMetadataConfig, defaultFlushDeadline, client, remoteapi.WriteV2MessageType, true)
	w = nativeMetadataWriter{qm}
	qm.StoreSeries([]record.RefSeries{{Ref: 1, Labels: labels.FromStrings(labels.MetricName, "metric")}}, 0)
	w.StoreNativeMetadata(nativeMetadataEntry(1, record.NativeMetadataGroup, nativeMetadataPoint(0, old)))
	qm.shards.start(1)
	defer qm.shards.stop()
	require.True(t, qm.Append([]record.RefSample{{Ref: 1, T: 100}}))
	var first []byte
	for i := range 2 {
		select {
		case request := <-requests:
			if i == 0 {
				first = request
			} else {
				require.Equal(t, first, request, "retry must not resolve newer metadata")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("missing retry")
		}
	}
	qm.shards.stop()
	qm.shards.start(2)
	require.True(t, qm.Append([]record.RefSample{{Ref: 1, T: 200}}))
	select {
	case request := <-requests:
		for i, encoded := range [][]byte{first, request} {
			decoded, err := compression.Decode(compression.Snappy, encoded, nil)
			require.NoError(t, err)
			var req writev2.Request
			require.NoError(t, req.Unmarshal(decoded))
			require.Len(t, req.Timeseries, 1)
			m, err := req.Timeseries[0].ToMetadata(req.Symbols)
			require.NoError(t, err)
			require.Equal(t, []metadata.Metadata{old, newest}[i], m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing delivery after resharding")
	}
}

func TestQueueManagerAppendBackpressure(t *testing.T) {
	for _, mode := range []string{"RW1", "RW2 disabled", "RW2 WAL", "native"} {
		for _, kind := range []string{"sample", "exemplar", "histogram", "float histogram"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					protoMsg := remoteapi.WriteV2MessageType
					if strings.HasPrefix(mode, "RW1") {
						protoMsg = remoteapi.WriteV1MessageType
					}
					native := mode == "native"
					cfg := testDefaultQueueConfig()
					cfg.MinBackoff = model.Duration(20 * time.Millisecond)
					qm := newTestQueueManager(t, cfg, config.DefaultMetadataConfig, defaultFlushDeadline, NewNopWriteClient(), protoMsg, native)
					qm.sendExemplars, qm.sendNativeHistograms = true, true
					qm.StoreSeries([]record.RefSeries{{Ref: 1, Labels: labels.FromStrings("__name__", "test")}}, 0)
					switch mode {
					case "RW2 WAL":
						qm.StoreMetadata([]record.RefMetadata{{Ref: 1, Type: record.GetMetricType(model.MetricTypeGauge), Help: "wal"}})
					case "native":
						nativeMetadataWriter{qm}.StoreNativeMetadata(nativeMetadataEntry(1, record.NativeMetadataGroup, nativeMetadataPoint(0, metadata.Metadata{Help: "old"})))
					}
					q := newQueue(1, 1)
					q.notifyCapacity = qm.shards.nextReady != nil
					qm.shards.queues = []*queue{q}
					require.True(t, q.Append(timeSeries{}))
					done := make(chan bool, 1)
					go func() {
						switch kind {
						case "sample":
							done <- qm.Append([]record.RefSample{{Ref: 1, T: 1000}})
						case "exemplar":
							done <- qm.AppendExemplars([]record.RefExemplar{{Ref: 1, T: 1000}})
						case "histogram":
							done <- qm.AppendHistograms([]record.RefHistogramSample{{Ref: 1, T: 1000, H: &histogram.Histogram{}}})
						case "float histogram":
							done <- qm.AppendFloatHistograms([]record.RefFloatHistogramSample{{Ref: 1, T: 1000, FH: &histogram.FloatHistogram{}}})
						}
					}()
					synctest.Wait()
					require.Empty(t, done)
					require.Equal(t, 1.0, client_testutil.ToFloat64(qm.metrics.enqueueRetriesTotal))
					if native {
						// Waiting producers keep the version resolved before waiting.
						nativeMetadataWriter{qm}.StoreNativeMetadata(nativeMetadataEntry(1, record.NativeMetadataOverride, nativeMetadataPoint(0, metadata.Metadata{Help: "new"})))
					}
					q.Batch()
					synctest.Wait()
					if protoMsg == remoteapi.WriteV1MessageType {
						require.Nil(t, q.spaceAvailable)
						require.Nil(t, qm.shards.nextReady)
						require.Empty(t, done, "RW1 producers retain their original backoff after capacity returns")
						backoff := 5 * time.Millisecond
						if kind == "exemplar" {
							backoff = time.Duration(cfg.MinBackoff)
						}
						time.Sleep(backoff - time.Nanosecond)
						synctest.Wait()
						require.Empty(t, done)
						time.Sleep(time.Nanosecond)
						synctest.Wait()
					}
					require.Len(t, done, 1)
					require.True(t, <-done)
					queued := q.Batch()
					require.Len(t, queued, 1)
					require.Equal(t, int64(1000), queued[0].timestamp)
					switch mode {
					case "native":
						require.Equal(t, "old", queued[0].metadata.Help)
					case "RW2 WAL":
						require.Equal(t, "wal", queued[0].metadata.Help)
					default:
						require.Nil(t, queued[0].metadata)
					}
				})
			})
		}
	}
}

func TestQueueManagerCapacityPolicy(t *testing.T) {
	for _, protoMsg := range []remoteapi.WriteMessageType{remoteapi.WriteV1MessageType, remoteapi.WriteV2MessageType} {
		for _, native := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/native=%t", protoMsg, native), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					// Every RW2 queue waits for capacity, whatever its metadata source.
					waiting := protoMsg == remoteapi.WriteV2MessageType
					entered := make(chan struct{}, 1)
					client := &MockWriteClient{
						NameFunc: func() string { return "test" }, EndpointFunc: func() string { return "http://test" },
						StoreFunc: func(context.Context, []byte, int) (WriteResponseStats, error) {
							entered <- struct{}{}
							return WriteResponseStats{Samples: 1, Confirmed: true}, nil
						},
					}
					cfg := testDefaultQueueConfig()
					cfg.MaxSamplesPerSend, cfg.Capacity = 1, 1
					cfg.BatchSendDeadline = model.Duration(time.Hour)
					qm := newTestQueueManager(t, cfg, config.DefaultMetadataConfig, defaultFlushDeadline, client, protoMsg, native)
					for _, shards := range []int{1, 2} {
						qm.shards.start(shards)
						require.Equal(t, waiting, qm.shards.nextReady != nil)
						for _, q := range qm.shards.queues {
							require.Equal(t, waiting, q.notifyCapacity)
							require.Nil(t, q.spaceAvailable)
						}
						q := qm.shards.queues[0]
						// RW1 receives must reach HTTP without taking batchMtx.
						if !waiting {
							q.batchMtx.Lock()
						}
						qm.metrics.pendingSamples.Inc()
						qm.shards.enqueuedSamples.Inc()
						q.batchQueue <- []timeSeries{{seriesLabels: labels.FromStrings("__name__", "test"), timestamp: 1000}}
						synctest.Wait()
						require.Len(t, entered, 1)
						if !waiting {
							q.batchMtx.Unlock()
						}
						synctest.Wait()
						require.Len(t, entered, 1)
						<-entered
						qm.shards.stop()
					}
				})
			})
		}
	}
}

func TestNativeMetadataWALDelivery(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run("restart="+strconv.FormatBool(restart), func(t *testing.T) {
			dir := t.TempDir()
			opts := tsdb.DefaultOptions()
			opts.EnableNativeMetadata = true
			opts.EnableMetadataWALRecords = false
			opts.EnableExemplarStorage = true
			opts.MaxExemplars = 100
			opts.EnableSTAsZeroSample = true
			db, err := tsdb.Open(dir, nil, nil, opts, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			base := time.Now().Add(time.Hour).UnixMilli()
			refs := make([]storage.SeriesRef, 3)
			unknown := metadata.Metadata{Type: model.MetricTypeUnknown}
			expected := map[string]metadata.Metadata{}
			key := func(kind string, index int, timestamp int64) string {
				return fmt.Sprintf("%s/%d/%d", kind, index, timestamp)
			}
			var previous metadata.Metadata
			newest := make([]metadata.Metadata, len(refs))
			for version := range 3 {
				app := db.AppenderV2(t.Context())
				timestamp := base + int64(version*100)
				for i := range refs {
					m := metadata.Metadata{Type: model.MetricTypeCounter, Help: []string{"a", "b", "a"}[version], Unit: "seconds"}
					if i > 0 {
						m.Type = model.MetricTypeHistogram
					} else if version == 1 {
						m.Type = model.MetricTypeUnknown
					}
					var h *histogram.Histogram
					var fh *histogram.FloatHistogram
					switch i {
					case 1:
						h = &histogram.Histogram{Count: 1, ZeroCount: 1, ZeroThreshold: 0.001}
					case 2:
						fh = &histogram.FloatHistogram{Count: 1, ZeroCount: 1, ZeroThreshold: 0.001}
					}
					refs[i], err = app.Append(refs[i], labels.FromStrings(labels.MetricName, "metric", "id", strconv.Itoa(i)), base-10, timestamp, 1, h, fh, storage.AOptions{Metadata: m})
					require.NoError(t, err)
					expected[key("sample", i, timestamp)] = m
					newest[i] = m
					if version == 0 {
						expected[key("sample", i, base-10)] = unknown
					}
					if i == 0 {
						_, err := app.AppendExemplars(refs[i], labels.EmptyLabels(), []exemplar.Exemplar{{Labels: labels.FromStrings("trace_id", strconv.Itoa(version)), Value: 1, Ts: timestamp - 1, HasTs: true}})
						require.NoError(t, err)
						if version == 0 {
							previous = unknown
						}
						expected[key("exemplar", i, timestamp-1)] = previous
						previous = m
					}
				}
				require.NoError(t, app.Commit())
			}
			if restart {
				require.NoError(t, db.Close())
				db, err = tsdb.Open(dir, nil, nil, opts, nil)
				require.NoError(t, err)
				// The watcher skips old segments on startup. Append to the new
				// segment without supplying metadata: replay seeded each series
				// with its newest version.
				clear(expected)
				app := db.AppenderV2(t.Context())
				for i, ref := range refs {
					_, err := app.Append(ref, labels.EmptyLabels(), 0, base+1000, 1, nil, nil, storage.AOptions{})
					require.NoError(t, err)
					expected[key("sample", i, base+1000)] = newest[i]
				}
				require.NoError(t, app.Commit())
			}

			received := make(chan *writev2.Request, 32)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				decoded, err := compression.Decode(compression.Snappy, body, nil)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				req := new(writev2.Request)
				if err := req.Unmarshal(decoded); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				var samples, histograms, exemplars int
				for _, series := range req.Timeseries {
					samples += len(series.Samples)
					histograms += len(series.Histograms)
					exemplars += len(series.Exemplars)
				}
				w.Header().Set("X-Prometheus-Remote-Write-Samples-Written", strconv.Itoa(samples))
				w.Header().Set("X-Prometheus-Remote-Write-Histograms-Written", strconv.Itoa(histograms))
				w.Header().Set("X-Prometheus-Remote-Write-Exemplars-Written", strconv.Itoa(exemplars))
				received <- req
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			s := NewStorage(nil, nil, db.StartTime, dir, defaultFlushDeadline, nil, false, true)
			defer s.Close()
			rw := baseRemoteWriteConfig(server.URL)
			rw.ProtobufMessage = remoteapi.WriteV2MessageType
			rw.SendExemplars, rw.SendNativeHistograms = true, true
			rw.QueueConfig = testDefaultQueueConfig()
			rw.QueueConfig.MaxShards = 1
			rw.MetadataConfig.Send = false
			rw.WriteRelabelConfigs = []*relabel.Config{{Action: relabel.Replace, Regex: relabel.MustNewRegexp(".*"), TargetLabel: labels.MetricName, Replacement: "renamed", NameValidationScheme: model.UTF8Validation}}
			require.NoError(t, s.ApplyConfig(&config.Config{GlobalConfig: config.GlobalConfig{ExternalLabels: labels.FromStrings("cluster", "test")}, RemoteWriteConfigs: []*config.RemoteWriteConfig{rw}}))
			deadline := time.After(5 * time.Second)
			notify := time.NewTicker(10 * time.Millisecond)
			defer notify.Stop()
			builder := labels.NewScratchBuilder(0)
			for len(expected) > 0 {
				select {
				case <-notify.C:
					s.Notify()
				case req := <-received:
					for _, series := range req.Timeseries {
						lset, err := series.ToLabels(&builder, req.Symbols)
						require.NoError(t, err)
						require.Equal(t, "renamed", lset.Get(labels.MetricName))
						require.Equal(t, "test", lset.Get("cluster"))
						id, err := strconv.Atoi(lset.Get("id"))
						require.NoError(t, err)
						m, err := series.ToMetadata(req.Symbols)
						require.NoError(t, err)
						var k string
						switch {
						case len(series.Samples) > 0:
							k = key("sample", id, series.Samples[0].Timestamp)
						case len(series.Histograms) > 0:
							k = key("sample", id, series.Histograms[0].Timestamp)
						case len(series.Exemplars) > 0:
							k = key("exemplar", id, series.Exemplars[0].Timestamp)
						}
						want, ok := expected[k]
						require.True(t, ok, "unexpected or duplicate item %s", k)
						require.Equal(t, want, m, k)
						delete(expected, k)
					}
				case <-deadline:
					t.Fatalf("missing metadata deliveries: %v", expected)
				}
			}
		})
	}

	// Each item gets the version in effect at its own timestamp, as of its WAL
	// position. A sender that starts after all commits reads them in WAL order,
	// as a delayed watcher would.
	t.Run("WAL-prefix semantics", func(t *testing.T) {
		type commit struct {
			offset int64
			help   string
		}
		for _, c := range []struct {
			name    string
			commits []commit
		}{
			{
				// Backfilled B@125, committed after the sample at 150, does not
				// relabel it. Lookups after the backfill would return B.
				name:    "delayed watcher",
				commits: []commit{{100, "A"}, {150, "A"}, {125, "B"}},
			},
			{
				// Native storage evicts A and B. Lookups would miss them.
				name:    "backlog beyond the version cap",
				commits: []commit{{100, "A"}, {200, "B"}, {300, "C"}, {400, "D"}, {500, "E"}, {600, "F"}, {700, "G"}},
			},
		} {
			t.Run(c.name, func(t *testing.T) {
				dir := t.TempDir()
				opts := tsdb.DefaultOptions()
				opts.EnableNativeMetadata = true
				opts.OutOfOrderTimeWindow = time.Hour.Milliseconds()
				db, err := tsdb.Open(dir, nil, nil, opts, nil)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				base := time.Now().Add(time.Hour).UnixMilli()
				lset := labels.FromStrings(labels.MetricName, "metric")
				expected := map[int64]string{}
				for _, commit := range c.commits {
					app := db.AppenderV2(t.Context())
					_, err := app.Append(0, lset, 0, base+commit.offset, 1, nil, nil, storage.AOptions{Metadata: metadata.Metadata{Type: model.MetricTypeGauge, Help: commit.help}})
					require.NoError(t, err)
					require.NoError(t, app.Commit())
					expected[base+commit.offset] = commit.help
				}

				received := make(chan *writev2.Request, 32)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err == nil {
						body, err = compression.Decode(compression.Snappy, body, nil)
					}
					req := new(writev2.Request)
					if err == nil {
						err = req.Unmarshal(body)
					}
					if err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					samples := 0
					for _, series := range req.Timeseries {
						samples += len(series.Samples)
					}
					w.Header().Set("X-Prometheus-Remote-Write-Samples-Written", strconv.Itoa(samples))
					received <- req
					w.WriteHeader(http.StatusNoContent)
				}))
				defer server.Close()
				s := NewStorage(nil, nil, db.StartTime, dir, defaultFlushDeadline, nil, false, true)
				defer s.Close()
				rw := baseRemoteWriteConfig(server.URL)
				rw.ProtobufMessage = remoteapi.WriteV2MessageType
				rw.QueueConfig = testDefaultQueueConfig()
				rw.MetadataConfig.Send = false
				require.NoError(t, s.ApplyConfig(&config.Config{RemoteWriteConfigs: []*config.RemoteWriteConfig{rw}}))
				deadline := time.After(5 * time.Second)
				notify := time.NewTicker(10 * time.Millisecond)
				defer notify.Stop()
				for len(expected) > 0 {
					select {
					case <-notify.C:
						s.Notify()
					case req := <-received:
						for _, series := range req.Timeseries {
							m, err := series.ToMetadata(req.Symbols)
							require.NoError(t, err)
							for _, sample := range series.Samples {
								want, ok := expected[sample.Timestamp]
								require.True(t, ok, "unexpected or duplicate sample at %d", sample.Timestamp)
								require.Equal(t, want, m.Help, "sample at %d", sample.Timestamp-base)
								delete(expected, sample.Timestamp)
							}
						}
					case <-deadline:
						t.Fatalf("missing deliveries: %v", expected)
					}
				}
			})
		}
	})
}

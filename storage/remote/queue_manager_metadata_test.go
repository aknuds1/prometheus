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
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	remoteapi "github.com/prometheus/client_golang/exp/api/remote"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/exemplar"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/model/relabel"
	writev2 "github.com/prometheus/prometheus/prompb/io/prometheus/write/v2"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/util/compression"
)

type nativeMetadataReaderFunc func(context.Context, []storage.NativeMetricMetadataLookup) error

func (f nativeMetadataReaderFunc) LookupNativeMetricMetadata(ctx context.Context, lookups []storage.NativeMetricMetadataLookup) error {
	return f(ctx, lookups)
}

func TestQueueManagerNativeMetadata(t *testing.T) {
	native := metadata.Metadata{Type: model.MetricTypeCounter, Help: "native", Unit: "seconds"}
	legacy := metadata.Metadata{Type: model.MetricTypeGauge, Help: "legacy"}
	for _, tc := range []struct {
		name    string
		missing bool
		fail    bool
		legacy  bool
		want    *metadata.Metadata
	}{
		{name: "native", want: &native},
		{name: "native precedes legacy", legacy: true, want: &native},
		{name: "legacy fallback", missing: true, legacy: true, want: &legacy},
		{name: "no metadata", missing: true},
		{name: "failed lookup discards partial results", fail: true, legacy: true, want: &legacy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewTestWriteClient(remoteapi.WriteV2MessageType)
			cfg := testDefaultQueueConfig()
			cfg.MaxSamplesPerSend = 10
			qm := newTestQueueManager(t, cfg, config.DefaultMetadataConfig, defaultFlushDeadline, c, remoteapi.WriteV2MessageType)
			qm.metadataContext, qm.cancelMetadata = context.WithCancel(t.Context())
			defer qm.cancelMetadata()
			qm.sendExemplars, qm.sendNativeHistograms = true, true
			series := []record.RefSeries{{Ref: 1, Labels: labels.FromStrings(labels.MetricName, "metric")}}
			qm.StoreSeries(series, 0)
			if tc.legacy {
				qm.StoreMetadata([]record.RefMetadata{{Ref: 1, Type: record.GetMetricType(legacy.Type), Help: legacy.Help}})
			}
			// Inspect the actual queued values before a sender can consume them.
			qm.shards.queues = []*queue{newQueue(nativeMetadataBatchSize*4, nativeMetadataBatchSize*4)}
			for _, size := range []int{0, 1, nativeMetadataBatchSize - 1, nativeMetadataBatchSize, nativeMetadataBatchSize + 1} {
				t.Run("size="+strconv.Itoa(size), func(t *testing.T) {
					for _, kind := range []string{"samples", "exemplars", "histograms", "float histograms"} {
						t.Run(kind, func(t *testing.T) {
							q := qm.shards.queues[0]
							clear(q.batch)
							q.batch = q.batch[:0]
							calls, seen := 0, 0
							qm.metadataReader = nativeMetadataReaderFunc(func(_ context.Context, lookups []storage.NativeMetricMetadataLookup) error {
								calls++
								require.LessOrEqual(t, len(lookups), nativeMetadataBatchSize)
								for i := range lookups {
									require.Equal(t, storage.SeriesRef(1), lookups[i].Ref)
									require.Equal(t, int64(100+seen), lookups[i].Timestamp)
									seen++
									if !tc.missing {
										lookups[i].Metadata = &native
									}
								}
								if tc.fail {
									return errors.New("lookup failed")
								}
								return nil
							})
							want := make([]timeSeries, size)
							samples := make([]record.RefSample, size)
							exemplars := make([]record.RefExemplar, size)
							histograms := make([]record.RefHistogramSample, size)
							floatHistograms := make([]record.RefFloatHistogramSample, size)
							for i := range want {
								timestamp, value := int64(100+i), float64(i)+0.5
								want[i] = timeSeries{seriesLabels: series[0].Labels, metadata: tc.want, timestamp: timestamp}
								switch kind {
								case "samples":
									samples[i] = record.RefSample{Ref: 1, ST: 50, T: timestamp, V: value}
									want[i].sType, want[i].startTimestamp, want[i].value = tSample, 50, value
								case "exemplars":
									lset := labels.FromStrings("trace_id", strconv.Itoa(i))
									exemplars[i] = record.RefExemplar{Ref: 1, T: timestamp, V: value, Labels: lset}
									want[i].sType, want[i].value, want[i].exemplarLabels = tExemplar, value, lset
								case "histograms":
									h := &histogram.Histogram{Count: 1, Sum: value}
									histograms[i] = record.RefHistogramSample{Ref: 1, ST: 50, T: timestamp, H: h}
									want[i].sType, want[i].startTimestamp, want[i].histogram = tHistogram, 50, h
								case "float histograms":
									h := &histogram.FloatHistogram{Count: 1, Sum: value}
									floatHistograms[i] = record.RefFloatHistogramSample{Ref: 1, ST: 50, T: timestamp, FH: h}
									want[i].sType, want[i].startTimestamp, want[i].floatHistogram = tFloatHistogram, 50, h
								}
							}
							switch kind {
							case "samples":
								require.True(t, qm.Append(samples))
							case "exemplars":
								require.True(t, qm.AppendExemplars(exemplars))
							case "histograms":
								require.True(t, qm.AppendHistograms(histograms))
							case "float histograms":
								require.True(t, qm.AppendFloatHistograms(floatHistograms))
							}
							require.Equal(t, want, q.batch)
							require.Equal(t, (size+nativeMetadataBatchSize-1)/nativeMetadataBatchSize, calls)
						})
					}
				})
			}
			qm.metadataReader = nativeMetadataReaderFunc(func(context.Context, []storage.NativeMetricMetadataLookup) error {
				t.Fatal("unknown series and old samples must be filtered before lookup")
				return nil
			})
			require.True(t, qm.Append([]record.RefSample{{Ref: 2, T: 1}}))
			qm.cfg.SampleAgeLimit = model.Duration(time.Hour)
			require.True(t, qm.Append([]record.RefSample{{Ref: 1, T: 1}}))
		})
	}

	t.Run("staging batch reuse", func(t *testing.T) {
		qm := newTestQueueManager(t, testDefaultQueueConfig(), config.DefaultMetadataConfig, defaultFlushDeadline, NewNopWriteClient(), remoteapi.WriteV2MessageType)
		qm.metadataContext = t.Context()
		qm.metadataReader = nativeMetadataReaderFunc(func(_ context.Context, lookups []storage.NativeMetricMetadataLookup) error {
			for i := range lookups {
				lookups[i].Metadata = &native
			}
			return nil
		})
		qm.shards.queues = []*queue{newQueue(2*nativeMetadataBatchSize, 2*nativeMetadataBatchSize)}
		// Keep ownership of one object throughout; pool reuse is not guaranteed.
		batch := new(nativeMetadataBatch)
		for _, size := range []int{nativeMetadataBatchSize, 1} {
			batch.count = size
			for i := range size {
				batch.lookups[i] = storage.NativeMetricMetadataLookup{Ref: 1, Timestamp: int64(i)}
				// Populate every pointer field to catch retained references.
				batch.series[i] = timeSeries{
					seriesLabels: labels.FromStrings(labels.MetricName, "metric"), timestamp: int64(i),
					exemplarLabels: labels.FromStrings("trace_id", "1"), metadata: &legacy,
					histogram: &histogram.Histogram{}, floatHistogram: &histogram.FloatHistogram{},
				}
			}
			require.True(t, batch.flush(qm, 0))
			require.Zero(t, batch.count)
			require.Equal(t, [nativeMetadataBatchSize]storage.NativeMetricMetadataLookup{}, batch.lookups)
			require.Equal(t, [nativeMetadataBatchSize]timeSeries{}, batch.series)
		}
	})

	t.Run("RW1 ignores native reader", func(t *testing.T) {
		reader := nativeMetadataReaderFunc(func(context.Context, []storage.NativeMetricMetadataLookup) error {
			t.Fatal("RW1 must not look up native metadata")
			return nil
		})
		s := NewWriteStorage(nil, nil, t.TempDir(), defaultFlushDeadline, nil, false, reader)
		defer s.Close()
		rw := baseRemoteWriteConfig("http://test-storage.com")
		rw.ProtobufMessage = remoteapi.WriteV1MessageType
		rw.MetadataConfig.Send = false
		require.NoError(t, s.ApplyConfig(&config.Config{RemoteWriteConfigs: []*config.RemoteWriteConfig{rw}}))
		for _, qm := range s.queues {
			require.Nil(t, qm.metadataReader)
			require.Nil(t, qm.metadataContext)
		}
	})
}

func TestQueueManagerNativeMetadataShutdown(t *testing.T) {
	for _, reload := range []bool{false, true} {
		t.Run("reload="+strconv.FormatBool(reload), func(t *testing.T) {
			entered := make(chan struct{})
			reader := nativeMetadataReaderFunc(func(ctx context.Context, _ []storage.NativeMetricMetadataLookup) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			})
			s := NewStorage(nil, nil, nil, t.TempDir(), defaultFlushDeadline, nil, false, reader)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			rw := baseRemoteWriteConfig("http://test-storage.com")
			rw.ProtobufMessage = remoteapi.WriteV2MessageType
			rw.QueueConfig = testDefaultQueueConfig()
			require.NoError(t, s.ApplyConfig(&config.Config{RemoteWriteConfigs: []*config.RemoteWriteConfig{rw}}))
			key, err := toHash(rw)
			require.NoError(t, err)
			qm := s.rws.queues[key]
			qm.StoreSeries([]record.RefSeries{{Ref: 1, Labels: labels.FromStrings(labels.MetricName, "metric")}}, 0)
			appended := make(chan bool, 1)
			go func() { appended <- qm.Append([]record.RefSample{{Ref: 1, T: 100}}) }()
			<-entered
			stopped := make(chan error, 1)
			go func() {
				if reload {
					stopped <- s.ApplyConfig(&config.Config{})
				} else {
					qm.Stop()
					stopped <- nil
				}
			}()
			select {
			case err := <-stopped:
				require.NoError(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("shutdown blocked on native metadata lookup")
			}
			require.False(t, <-appended)
			if !reload {
				delete(s.rws.queues, key)
			}
		})
	}
}

func TestQueueManagerNativeMetadataRetryAndReshard(t *testing.T) {
	old := metadata.Metadata{Type: model.MetricTypeCounter, Help: "old"}
	newest := metadata.Metadata{Type: model.MetricTypeCounter, Help: "new"}
	var current atomic.Pointer[metadata.Metadata]
	current.Store(&old)
	requests := make(chan []byte, 3)
	client := &MockWriteClient{
		NameFunc: func() string { return "test" }, EndpointFunc: func() string { return "http://test" },
		StoreFunc: func(_ context.Context, req []byte, attempt int) (WriteResponseStats, error) {
			requests <- bytes.Clone(req)
			if current.Load() == &old && attempt == 0 {
				current.Store(&newest)
				return WriteResponseStats{}, RecoverableError{errors.New("retry"), 0}
			}
			return WriteResponseStats{Samples: 1, Confirmed: true}, nil
		},
	}
	cfg := testDefaultQueueConfig()
	cfg.MaxSamplesPerSend = 1
	qm := newTestQueueManager(t, cfg, config.DefaultMetadataConfig, defaultFlushDeadline, client, remoteapi.WriteV2MessageType)
	qm.metadataContext, qm.cancelMetadata = context.WithCancel(t.Context())
	defer qm.cancelMetadata()
	lookups := 0
	qm.metadataReader = nativeMetadataReaderFunc(func(_ context.Context, batch []storage.NativeMetricMetadataLookup) error {
		lookups++
		for i := range batch {
			batch[i].Metadata = current.Load()
		}
		return nil
	})
	qm.StoreSeries([]record.RefSeries{{Ref: 1, Labels: labels.FromStrings(labels.MetricName, "metric")}}, 0)
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
	require.Equal(t, 2, lookups)
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
				// segment without supplying metadata: replay did not restore it.
				clear(expected)
				app := db.AppenderV2(t.Context())
				for i, ref := range refs {
					_, err := app.Append(ref, labels.EmptyLabels(), 0, base+1000, 1, nil, nil, storage.AOptions{})
					require.NoError(t, err)
					expected[key("sample", i, base+1000)] = unknown
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
			s := NewStorage(nil, nil, db.StartTime, dir, defaultFlushDeadline, nil, false, db)
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
}

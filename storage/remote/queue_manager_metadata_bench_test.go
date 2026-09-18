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
	"context"
	"fmt"
	"runtime"
	"strconv"
	"testing"

	remoteapi "github.com/prometheus/client_golang/exp/api/remote"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	writev2 "github.com/prometheus/prometheus/prompb/io/prometheus/write/v2"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
	"github.com/prometheus/prometheus/util/compression"
)

// BenchmarkQueueManagerMetadataAppend isolates staging from Head lookup and
// encoding costs by returning fixed metadata from the native reader.
func BenchmarkQueueManagerMetadataAppend(b *testing.B) {
	const numSeries = 1000
	for _, kind := range []string{"samples", "exemplars", "histograms", "float histograms"} {
		for _, source := range []string{"legacy", "native"} {
			b.Run(kind+"/source="+source, func(b *testing.B) {
				m := metadata.Metadata{Type: model.MetricTypeCounter, Help: "fixed metadata", Unit: "seconds"}
				qm := newTestQueueManager(b, config.DefaultQueueConfig, config.DefaultMetadataConfig, defaultFlushDeadline, NewNopWriteClient(), remoteapi.WriteV2MessageType)
				qm.sendExemplars, qm.sendNativeHistograms = true, true
				series := make([]record.RefSeries, numSeries)
				legacy := make([]record.RefMetadata, numSeries)
				samples := make([]record.RefSample, numSeries)
				exemplars := make([]record.RefExemplar, numSeries)
				histograms := make([]record.RefHistogramSample, numSeries)
				floatHistograms := make([]record.RefFloatHistogramSample, numSeries)
				for i := range series {
					ref := chunks.HeadSeriesRef(i + 1)
					series[i] = record.RefSeries{Ref: ref, Labels: labels.FromStrings(labels.MetricName, "metric", "id", strconv.Itoa(i))}
					legacy[i] = record.RefMetadata{Ref: ref, Type: record.GetMetricType(m.Type), Help: m.Help, Unit: m.Unit}
					samples[i] = record.RefSample{Ref: ref, ST: 100, T: 200, V: float64(i)}
					exemplars[i] = record.RefExemplar{Ref: ref, T: 200, V: float64(i), Labels: labels.FromStrings("trace_id", strconv.Itoa(i))}
					histograms[i] = record.RefHistogramSample{Ref: ref, ST: 100, T: 200, H: &histogram.Histogram{Count: 1, Sum: float64(i)}}
					floatHistograms[i] = record.RefFloatHistogramSample{Ref: ref, ST: 100, T: 200, FH: &histogram.FloatHistogram{Count: 1, Sum: float64(i)}}
				}
				qm.StoreSeries(series, 0)
				if source == "native" {
					qm.metadataContext = b.Context()
					qm.metadataReader = nativeMetadataReaderFunc(func(_ context.Context, lookups []storage.NativeMetricMetadataLookup) error {
						for i := range lookups {
							lookups[i].Metadata = &m
						}
						return nil
					})
				} else {
					qm.StoreMetadata(legacy)
				}
				appendItems := map[string]func() bool{
					"samples":          func() bool { return qm.Append(samples) },
					"exemplars":        func() bool { return qm.AppendExemplars(exemplars) },
					"histograms":       func() bool { return qm.AppendHistograms(histograms) },
					"float histograms": func() bool { return qm.AppendFloatHistograms(floatHistograms) },
				}[kind]
				q := newQueue(numSeries+1, numSeries+1)
				qm.shards.queues = []*queue{q}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if !appendItems() {
						b.Fatal("append failed")
					}
					clear(q.batch)
					q.batch = q.batch[:0]
				}
				b.StopTimer()
				require.True(b, appendItems())
				require.Len(b, q.batch, numSeries)
				for _, s := range q.batch {
					require.Equal(b, &m, s.metadata)
				}
			})
		}
	}
}

// BenchmarkQueueManagerMetadataAppendEncode compares identical RW2 payloads
// obtained from legacy WAL metadata or native Head lookups. Encoding is synchronous
// so no unfinished asynchronous sends are excluded from the timed work.
// Run with -benchmem -count=6 and compare source sub-benchmarks with benchstat.
func BenchmarkQueueManagerMetadataAppendEncode(b *testing.B) {
	const numSeries = 1000
	for _, families := range []int{10, numSeries} {
		// Missing predates retained history; absent has no native metadata at all.
		for _, state := range []string{"current", "historical", "missing", "absent"} {
			for _, source := range []string{"legacy", "native"} {
				b.Run(fmt.Sprintf("families=%d/state=%s/source=%s", families, state, source), func(b *testing.B) {
					opts := tsdb.DefaultHeadOptions()
					opts.ChunkDirRoot = b.TempDir()
					opts.EnableNativeMetadata = true
					head, err := tsdb.NewHead(nil, nil, nil, nil, opts, nil)
					require.NoError(b, err)
					b.Cleanup(func() { require.NoError(b, head.Close()) })
					require.NoError(b, head.Init(0))
					series := make([]record.RefSeries, numSeries)
					samples := make([]record.RefSample, numSeries)
					legacy := make([]record.RefMetadata, numSeries)
					for version := range 2 {
						app := head.AppenderV2(b.Context())
						for i := range series {
							m := metadata.Metadata{Type: model.MetricTypeCounter, Help: fmt.Sprintf("family %d version %d", i%families, version), Unit: "seconds"}
							if state == "absent" {
								m = metadata.Metadata{}
							}
							lset := labels.FromStrings(labels.MetricName, "metric", "id", strconv.Itoa(i))
							ref, err := app.Append(storage.SeriesRef(series[i].Ref), lset, 0, int64(100+version*100), 1, nil, nil, storage.AOptions{Metadata: m})
							require.NoError(b, err)
							series[i] = record.RefSeries{Ref: chunks.HeadSeriesRef(ref), Labels: lset}
							if state == "current" && version == 1 || state == "historical" && version == 0 {
								legacy[i] = record.RefMetadata{Ref: chunks.HeadSeriesRef(ref), Type: record.GetMetricType(m.Type), Help: m.Help, Unit: m.Unit}
							}
							samples[i] = record.RefSample{Ref: chunks.HeadSeriesRef(ref), T: map[string]int64{"current": 200, "historical": 100, "missing": 50, "absent": 200}[state], V: 1}
						}
						require.NoError(b, app.Commit())
					}
					qm := newTestQueueManager(b, config.DefaultQueueConfig, config.DefaultMetadataConfig, defaultFlushDeadline, NewNopWriteClient(), remoteapi.WriteV2MessageType)
					qm.StoreSeries(series, 0)
					if source == "native" {
						qm.metadataReader = head
						qm.metadataContext, qm.cancelMetadata = context.WithCancel(b.Context())
						b.Cleanup(qm.cancelMetadata)
					} else if state == "current" || state == "historical" {
						qm.StoreMetadata(legacy)
					}
					q := newQueue(numSeries+1, numSeries+1)
					qm.shards.queues = []*queue{q}
					pending := make([]writev2.TimeSeries, numSeries)
					symbols := writev2.NewSymbolTable()
					encoder := compression.NewSyncEncodeBuffer()
					var protobuf []byte
					var size int
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						if !qm.Append(samples) {
							b.Fatal("append failed")
						}
						populateV2TimeSeries(&symbols, q.batch, pending, false, false, false)
						encoded, _, _, _, err := buildV2WriteRequest(qm.logger, pending, symbols.Symbols(), &protobuf, nil, encoder, compression.Snappy)
						if err != nil {
							b.Fatal(err)
						}
						size = len(encoded)
						clear(q.batch)
						q.batch = q.batch[:0]
						symbols.Reset()
					}
					b.StopTimer()
					b.ReportMetric(float64(size), "wire-bytes/op")
					// Validate the selected values without warming lookup scratch before
					// a single-iteration run, which also exercises cold allocations.
					require.True(b, qm.Append(samples))
					for i, s := range q.batch {
						if state == "current" || state == "historical" {
							want := metadata.Metadata{Type: record.ToMetricType(legacy[i].Type), Help: legacy[i].Help, Unit: legacy[i].Unit}
							require.Equal(b, &want, s.metadata)
						} else {
							require.Nil(b, s.metadata)
						}
					}
					clear(q.batch)
					q.batch = q.batch[:0]
					// Measure metadata retained by a backlog separately from lookup
					// scratch and the queue's unchanged allocation. Two GCs discard
					// sync.Pool victim caches as well as transient historical maps.
					const rounds = 100
					q = newQueue(rounds*numSeries+1, rounds*numSeries+1)
					qm.shards.queues[0] = q
					runtime.GC()
					runtime.GC()
					var before, after runtime.MemStats
					runtime.ReadMemStats(&before)
					for range rounds {
						require.True(b, qm.Append(samples))
					}
					runtime.GC()
					runtime.GC()
					runtime.ReadMemStats(&after)
					b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/(rounds*numSeries), "retained-B/sample")
					runtime.KeepAlive(qm)
					runtime.KeepAlive(head)
				})
			}
		}
	}
}

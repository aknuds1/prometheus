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
	"testing"
	"time"

	remoteapi "github.com/prometheus/client_golang/exp/api/remote"
	client_testutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/record"
)

func BenchmarkQueueManagerEnqueue(b *testing.B) {
	for _, backpressure := range []bool{false, true} {
		b.Run(fmt.Sprintf("backpressure=%t", backpressure), func(b *testing.B) {
			cfg := testDefaultQueueConfig()
			cfg.MaxSamplesPerSend, cfg.Capacity = 1, 1
			qm := newTestQueueManager(b, cfg, config.DefaultMetadataConfig, defaultFlushDeadline, NewNopWriteClient(), remoteapi.WriteV2MessageType)
			qm.StoreSeries([]record.RefSeries{{Ref: 1, Labels: labels.FromStrings("__name__", "test")}}, 0)
			q := newQueue(1, 1)
			qm.shards.queues = []*queue{q}
			samples := []record.RefSample{{Ref: 1, T: 1000}}
			if backpressure {
				// Use the actual shard receiver with a one-batch queue and a no-op client.
				qm.shards.start(1)
				defer qm.shards.stop()
			}
			b.ReportAllocs()
			for b.Loop() {
				if !qm.Append(samples) {
					b.Fatal("enqueue interrupted")
				}
				if !backpressure {
					q.ReturnForReuse(q.Batch())
				}
			}
		})
	}
}

func newMetadataBenchmarkQueueManager(b *testing.B, cfg config.QueueConfig, mcfg config.MetadataConfig, deadline time.Duration, client WriteClient, protoMsg remoteapi.WriteMessageType, reader storage.NativeMetricMetadataReader) *QueueManager {
	qm := NewQueueManager(newQueueManagerMetrics(nil, "", ""), nil, nil, nil, b.TempDir(),
		newEWMARate(ewmaWeight, shardUpdateDuration), cfg, mcfg, labels.EmptyLabels(), nil,
		client, deadline, newPool(), newHighestTimestampMetric(), nil, false, false, false,
		protoMsg, record.NewBuffersPool(), false, reader)
	if qm.cancelMetadata != nil {
		b.Cleanup(qm.cancelMetadata)
	}
	return qm
}

// BenchmarkQueueManagerEnqueuePolicy isolates enqueueing under the policy selected
// by NewQueueManager. Lifecycle and WAL costs belong to the pipeline benchmarks.
func BenchmarkQueueManagerEnqueuePolicy(b *testing.B) {
	for _, native := range []bool{false, true} {
		for _, backpressure := range []bool{false, true} {
			b.Run(fmt.Sprintf("native=%t/backpressure=%t", native, backpressure), func(b *testing.B) {
				cfg := config.DefaultQueueConfig
				cfg.MaxSamplesPerSend, cfg.Capacity = 1, 1
				cfg.BatchSendDeadline = model.Duration(time.Hour)
				m := metadata.Metadata{Type: model.MetricTypeCounter, Help: "fixed"}
				var reader storage.NativeMetricMetadataReader
				if native {
					reader = nativeMetadataReaderFunc(func(_ context.Context, lookups []storage.NativeMetricMetadataLookup) error {
						for i := range lookups {
							lookups[i].Metadata = &m
						}
						return nil
					})
				}
				entered, release := make(chan struct{}, 1), make(chan struct{})
				client := &MockWriteClient{
					NameFunc: func() string { return "test" }, EndpointFunc: func() string { return "http://test" },
					StoreFunc: func(ctx context.Context, _ []byte, _ int) (WriteResponseStats, error) {
						entered <- struct{}{}
						select {
						case <-release:
							return WriteResponseStats{Samples: 1, Confirmed: true}, nil
						case <-ctx.Done():
							return WriteResponseStats{}, ctx.Err()
						}
					},
				}
				// Keep this constructor call independent of revision-specific test helpers.
				qm := NewQueueManager(newQueueManagerMetrics(nil, "", ""), nil, nil, nil, b.TempDir(),
					newEWMARate(ewmaWeight, shardUpdateDuration), cfg, config.DefaultMetadataConfig,
					labels.EmptyLabels(), nil, client, defaultFlushDeadline, newPool(),
					newHighestTimestampMetric(), nil, false, false, false, remoteapi.WriteV2MessageType,
					record.NewBuffersPool(), false, reader)
				if qm.cancelMetadata != nil {
					b.Cleanup(qm.cancelMetadata)
				}
				qm.StoreSeries([]record.RefSeries{{Ref: 1, Labels: labels.FromStrings("__name__", "test")}}, 0)
				samples := []record.RefSample{{Ref: 1, T: 1000}}
				// Verify lookup outside timing, without instrumentation in the fixed reader.
				probe := newQueue(2, 2)
				qm.shards.queues = []*queue{probe}
				require.True(b, qm.Append(samples))
				if native {
					require.Same(b, &m, probe.batch[0].metadata)
				} else {
					require.Nil(b, probe.batch[0].metadata)
				}
				qm.shards.start(1)
				require.True(b, qm.Append(samples))
				<-entered
				qm.SetClient(NewNopWriteClient())
				q := qm.shards.queues[0]
				if backpressure {
					// Prove saturation before timing even for a one-iteration smoke run.
					require.True(b, qm.Append(samples))
					done := make(chan bool, 1)
					go func() { done <- qm.Append(samples) }()
					require.Eventually(b, func() bool { return client_testutil.ToFloat64(qm.metrics.enqueueRetriesTotal) > 0 }, time.Second, time.Millisecond)
					close(release)
					require.True(b, <-done)
					require.Eventually(b, func() bool { return client_testutil.ToFloat64(qm.metrics.pendingSamples) == 0 }, time.Second, time.Millisecond)
				}
				retries := client_testutil.ToFloat64(qm.metrics.enqueueRetriesTotal)
				b.ReportAllocs()
				for b.Loop() {
					if !qm.Append(samples) {
						b.Fatal("enqueue interrupted")
					}
					if !backpressure {
						q.ReturnForReuse(q.Batch())
					}
				}
				b.StopTimer()
				if !backpressure {
					require.Equal(b, retries, client_testutil.ToFloat64(qm.metrics.enqueueRetriesTotal), "uncontended timed appends must not retry")
					// The real sender remains parked throughout timing, so it cannot compete
					// with the benchmark's manual drain.
					close(release)
				}
				qm.shards.stop()
			})
		}
	}
}

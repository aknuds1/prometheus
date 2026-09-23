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
	"fmt"
	"testing"

	remoteapi "github.com/prometheus/client_golang/exp/api/remote"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
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

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
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
)

const nativeMetadataBatchSize = 256

var nativeMetadataBatchPool = sync.Pool{New: func() any { return new(nativeMetadataBatch) }}

func (t *QueueManager) getNativeMetadataBatch() *nativeMetadataBatch {
	if t.metadataReader == nil {
		return nil
	}
	return nativeMetadataBatchPool.Get().(*nativeMetadataBatch)
}

// nativeMetadataBatch belongs to one Append call. It freezes metadata before
// enqueueing without retaining a per-queue native cache or growing timeSeries.
// A nil batch makes flush and release no-ops for the legacy path.
type nativeMetadataBatch struct {
	lookups [nativeMetadataBatchSize]storage.NativeMetricMetadataLookup
	series  [nativeMetadataBatchSize]timeSeries
	count   int
}

func (b *nativeMetadataBatch) release() {
	if b == nil {
		return
	}
	clear(b.lookups[:b.count])
	clear(b.series[:b.count])
	b.count = 0
	nativeMetadataBatchPool.Put(b)
}

// Callers must initialize b.series[b.count] before append and must not retain
// the slot across this call.
func (b *nativeMetadataBatch) append(t *QueueManager, ref chunks.HeadSeriesRef, backoff model.Duration) bool {
	b.lookups[b.count] = storage.NativeMetricMetadataLookup{Ref: storage.SeriesRef(ref), Timestamp: b.series[b.count].timestamp}
	b.count++
	return b.count < len(b.series) || b.flush(t, backoff)
}

func (b *nativeMetadataBatch) flush(t *QueueManager, initialBackoff model.Duration) bool {
	if b == nil || b.count == 0 {
		return true
	}
	lookups := b.lookups[:b.count]
	if err := t.metadataReader.LookupNativeMetricMetadata(t.metadataContext, lookups); err != nil {
		if t.metadataContext.Err() != nil {
			return false
		}
		// The WAL watcher does not replay a batch after Append returns. Failed
		// lookups must therefore fall back, not abandon this batch of samples.
		t.logger.Debug("Native metadata lookup failed; using WAL metadata if available", "err", err)
		for i := range lookups {
			lookups[i].Metadata = nil
		}
	}
	for i, lookup := range lookups {
		if lookup.Metadata != nil {
			b.series[i].metadata = lookup.Metadata
		}
		backoff := initialBackoff
		for {
			select {
			case <-t.quit:
				return false
			default:
			}
			if t.shards.enqueue(chunks.HeadSeriesRef(lookup.Ref), b.series[i]) {
				break
			}
			t.metrics.enqueueRetriesTotal.Inc()
			select {
			case <-t.quit:
				return false
			case <-time.After(time.Duration(backoff)):
			}
			backoff = min(backoff*2, t.cfg.MaxBackoff)
		}
	}
	clear(lookups)
	clear(b.series[:b.count])
	b.count = 0
	return true
}

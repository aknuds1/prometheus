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
	"fmt"
	"math"
	"runtime"
	"sync"
	"testing"

	"go.uber.org/atomic"

	"github.com/prometheus/prometheus/storage"
)

// BenchmarkHeadMetricMetadataLookupAppendFixedWork diagnoses allocations with
// b.N total transactions and one lookup consumer. Both scheduling modes use the
// same preallocated scaffolding; shared-budget is not Go's RunParallel scheduler.
// Use fixed iteration counts and compare implementations within each mode.
func BenchmarkHeadMetricMetadataLookupAppendFixedWork(b *testing.B) {
	const workers, perWorker = 8, 1000
	for _, scheduling := range []string{"shared-budget", "balanced"} {
		b.Run("scheduling="+scheduling, func(b *testing.B) {
			h, _, closeHead := newMetricMetadataBenchmarkHead(b, metricMetadataBenchmarkMode{nativeEnabled: true}, 1_000_000_000, false)
			b.Cleanup(closeHead)
			fixture := newMetricMetadataBenchmarkFixture(workers*perWorker, 100, maxNativeMetricMetadataVersions+2)
			refs := make([]storage.SeriesRef, workers*perWorker)
			for version := range maxNativeMetricMetadataVersions {
				appendMetricMetadataBenchmarkRound(b, h, fixture, refs, version, int64(100+version))
			}
			type request struct {
				lookups []storage.NativeMetricMetadataLookup
				done    chan error
			}
			requests := make(chan request)
			lookups := make([][]storage.NativeMetricMetadataLookup, workers)
			acknowledgments := make([]chan error, workers)
			completed := make([]int, workers)
			for worker := range workers {
				acknowledgments[worker] = make(chan error, 1)
				lookups[worker] = make([]storage.NativeMetricMetadataLookup, perWorker)
				for i := range lookups[worker] {
					lookups[worker][i] = storage.NativeMetricMetadataLookup{Ref: refs[worker*perWorker+i], Timestamp: math.MaxInt64}
				}
			}
			var ready, finished, consumer sync.WaitGroup
			ready.Add(workers + 1)
			start := make(chan struct{})
			errors := make(chan error, workers)
			var claimed atomic.Int64
			var acknowledged int
			consumer.Go(func() {
				ready.Done()
				<-start
				for req := range requests {
					err := h.LookupNativeMetricMetadata(b.Context(), req.lookups)
					if err == nil {
						acknowledged++
					}
					req.done <- err
				}
			})
			for worker := range workers {
				finished.Go(func() {
					ready.Done()
					<-start
					quota := b.N / workers
					if worker < b.N%workers {
						quota++
					}
					for round := 0; ; round++ {
						if scheduling == "shared-budget" {
							if claimed.Add(1) > int64(b.N) {
								return
							}
						} else if round == quota {
							return
						}
						app := h.AppenderV2(b.Context())
						for i := worker * perWorker; i < (worker+1)*perWorker; i++ {
							if _, err := app.Append(refs[i], fixture.labels[i], 0, int64(1000+round), 1, nil, nil,
								fixture.options[maxNativeMetricMetadataVersions-1][fixture.familyBySeries[i]]); err != nil {
								_ = app.Rollback()
								errors <- err
								return
							}
						}
						if err := app.Commit(); err != nil {
							errors <- err
							return
						}
						requests <- request{lookups: lookups[worker], done: acknowledgments[worker]}
						if err := <-acknowledgments[worker]; err != nil {
							errors <- err
							return
						}
						completed[worker]++
					}
				})
			}
			ready.Wait()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			b.ReportAllocs()
			b.ResetTimer()
			close(start)
			finished.Wait()
			close(requests)
			consumer.Wait()
			b.StopTimer()
			runtime.ReadMemStats(&after)
			close(errors)
			for err := range errors {
				b.Error(err)
			}
			if b.Failed() {
				return
			}
			var transactions int
			for worker, count := range completed {
				transactions += count
				if scheduling == "balanced" {
					want := b.N / workers
					if worker < b.N%workers {
						want++
					}
					if count != want {
						b.Fatalf("worker %d completed %d transactions, want %d", worker, count, want)
					}
				}
				b.ReportMetric(float64(count), fmt.Sprintf("worker%d-transactions", worker))
				if count == 0 {
					continue
				}
				for i, lookup := range lookups[worker] {
					want := fixture.options[maxNativeMetricMetadataVersions-1][fixture.familyBySeries[worker*perWorker+i]].Metadata
					if lookup.Metadata == nil || *lookup.Metadata != want {
						b.Fatalf("unexpected metadata for worker %d series %d: got %v, want %v", worker, i, lookup.Metadata, want)
					}
				}
			}
			if transactions != b.N || acknowledged != b.N {
				b.Fatalf("completed %d transactions and %d lookups, want %d each", transactions, acknowledged, b.N)
			}
			samples := int64(transactions) * perWorker
			validateMetricMetadataBenchmarkState(b, h, metricMetadataBenchmarkMode{nativeEnabled: true}, fixture, refs, maxNativeMetricMetadataVersions)
			validateMetricMetadataBenchmarkSamples(b, h, samples+int64(len(refs)*maxNativeMetricMetadataVersions))
			b.ReportMetric(float64(transactions), "transactions")
			b.ReportMetric(float64(acknowledged), "acknowledgments")
			b.ReportMetric(float64(samples), "samples")
			b.ReportMetric(perWorker, "samples/op")
			b.ReportMetric(float64(after.NumGC-before.NumGC), "GC-cycles")
			b.ReportMetric(float64(after.PauseTotalNs-before.PauseTotalNs), "GC-pause-ns")
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(samples), "ns/sample")
		})
	}
}

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
	"sync"
	"testing"

	"github.com/prometheus/prometheus/storage"
)

// BenchmarkHeadMetricMetadataAppendFixedWork diagnoses allocations with exactly
// b.N transactions per worker. Use -benchtime=1024x or 4096x and fresh processes.
// Unlike RunParallel, every series receives the same number of timed samples.
func BenchmarkHeadMetricMetadataAppendFixedWork(b *testing.B) {
	const perWorker = 1000
	for _, tc := range []struct {
		name     string
		workers  int
		versions int
		mode     metricMetadataBenchmarkMode
	}{
		{name: "fixed-concurrency", workers: 8, versions: 5, mode: metricMetadataBenchmarkMode{nativeEnabled: true}},
		{name: "lookup-append-zero-destinations", workers: 8, versions: 5, mode: metricMetadataBenchmarkMode{nativeEnabled: true}},
		{name: "legacy-stable", workers: 1, versions: 1, mode: metricMetadataBenchmarkMode{legacyEnabled: true}},
	} {
		b.Run("case="+tc.name, func(b *testing.B) {
			h, _, closeHead := newMetricMetadataBenchmarkHead(b, tc.mode, 1_000_000_000, false)
			b.Cleanup(closeHead)
			variants := 1
			if tc.mode.nativeEnabled {
				variants = maxNativeMetricMetadataVersions + 2
			}
			fixture := newMetricMetadataBenchmarkFixture(tc.workers*perWorker, 100, variants)
			refs := make([]storage.SeriesRef, len(fixture.labels))
			for version := range tc.versions {
				appendMetricMetadataBenchmarkRound(b, h, fixture, refs, version, int64(100+version))
			}
			completed := make([]int, tc.workers)
			// Zero destinations needs no lookup buffers or consumer goroutines.
			// All worker coordination is also allocated before timing starts.
			errors := make(chan error, tc.workers)
			appendWorker := func(worker int) error {
				for round := range b.N {
					app := h.AppenderV2(b.Context())
					value := float64(round)
					switch tc.name {
					case "lookup-append-zero-destinations":
						value = 1
					case "legacy-stable":
						value = float64(1000 + round)
					}
					for i := worker * perWorker; i < (worker+1)*perWorker; i++ {
						ref, err := app.Append(refs[i], fixture.labels[i], 0, int64(1000+round), value, nil, nil,
							fixture.options[tc.versions-1][fixture.familyBySeries[i]])
						if err != nil {
							_ = app.Rollback()
							return err
						}
						refs[i] = ref
					}
					if err := app.Commit(); err != nil {
						return err
					}
					completed[worker]++
				}
				return nil
			}
			b.ReportAllocs()
			if tc.workers == 1 {
				// Preserve the legacy fixture's single writer on the benchmark goroutine.
				b.ResetTimer()
				err := appendWorker(0)
				b.StopTimer()
				errors <- err
			} else {
				var ready, done sync.WaitGroup
				ready.Add(tc.workers)
				start := make(chan struct{})
				for worker := range tc.workers {
					done.Go(func() {
						ready.Done()
						<-start
						errors <- appendWorker(worker)
					})
				}
				ready.Wait()
				b.ResetTimer()
				close(start)
				done.Wait()
				b.StopTimer()
			}
			close(errors)
			for err := range errors {
				if err != nil {
					b.Fatal(err)
				}
			}
			for worker, count := range completed {
				if count != b.N {
					b.Fatalf("worker %d completed %d transactions, want %d", worker, count, b.N)
				}
			}
			transactions := int64(tc.workers) * int64(b.N)
			samples := transactions * perWorker
			validateMetricMetadataBenchmarkState(b, h, tc.mode, fixture, refs, tc.versions)
			validateMetricMetadataBenchmarkSamples(b, h, samples+int64(len(refs)*tc.versions))
			b.ReportMetric(float64(transactions), "transactions")
			b.ReportMetric(float64(samples), "samples")
			b.ReportMetric(float64(tc.workers*perWorker), "samples/op")
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(samples), "ns/sample")
		})
	}
}

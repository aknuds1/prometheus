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
	"time"

	"go.uber.org/atomic"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
)

// BenchmarkHeadMetricMetadataLookupAppendConcurrent measures eight ingestion
// workers feeding zero, one, or two sequential metadata lookup consumers, matching
// the WAL watcher's one-consumer-per-destination model. Each operation waits for
// its committed batch's lookups, keeping the amount of work per operation fixed;
// this includes publication-barrier contention without asynchronous work escaping
// the timed region. Run with -cpu=1,8 -benchmem -count=6.
func BenchmarkHeadMetricMetadataLookupAppendConcurrent(b *testing.B) {
	const workers, perWorker = 8, 1000
	for _, lookupState := range []string{"current", "historical"} {
		for _, changing := range []bool{false, true} {
			for _, destinations := range []int{0, 1, 2} {
				if lookupState == "historical" && destinations == 0 {
					continue
				}
				b.Run(fmt.Sprintf("lookup=%s/changing=%t/destinations=%d", lookupState, changing, destinations), func(b *testing.B) {
					procs := runtime.GOMAXPROCS(0)
					if procs > workers || workers%procs != 0 {
						b.Skipf("GOMAXPROCS must divide %d", workers)
					}
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
					queues := make([]chan request, destinations)
					var consumers sync.WaitGroup
					for i := range queues {
						queues[i] = make(chan request)
						consumers.Go(func() {
							for req := range queues[i] {
								req.done <- h.LookupNativeMetricMetadata(b.Context(), req.lookups)
							}
						})
					}
					var nextWorker atomic.Uint64
					b.SetParallelism(workers / procs)
					b.ReportAllocs()
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						worker := int(nextWorker.Add(1) - 1)
						done := make(chan error, 1)
						lookups := make([]storage.NativeMetricMetadataLookup, perWorker)
						for i := range lookups {
							lookups[i] = storage.NativeMetricMetadataLookup{Ref: refs[worker*perWorker+i], Timestamp: math.MaxInt64}
							if lookupState == "historical" {
								lookups[i].Timestamp = 100 + maxNativeMetricMetadataVersions - 2
							}
						}
						var round int64
						for pb.Next() {
							variant := maxNativeMetricMetadataVersions - 1
							if changing {
								variant = maxNativeMetricMetadataVersions + int(round%2)
							}
							app := h.AppenderV2(b.Context())
							for i := worker * perWorker; i < (worker+1)*perWorker; i++ {
								if _, err := app.Append(refs[i], fixture.labels[i], 0, 1000+round, 1, nil, nil, fixture.options[variant][fixture.familyBySeries[i]]); err != nil {
									b.Fatal(err)
								}
							}
							if err := app.Commit(); err != nil {
								b.Fatal(err)
							}
							if lookupState == "historical" && changing {
								// This worker owns its series and waits for both destinations,
								// so the preceding version cannot be evicted by the next round.
								for i := range lookups {
									lookups[i].Timestamp = 1000 + round - 1
								}
							}
							for _, queue := range queues {
								queue <- request{lookups: lookups, done: done}
								if err := <-done; err != nil {
									b.Fatal(err)
								}
							}
							round++
						}
						if destinations > 0 && round > 0 {
							wantVariant := maxNativeMetricMetadataVersions - 1
							if changing {
								if lookupState == "current" {
									wantVariant = maxNativeMetricMetadataVersions + int((round-1)%2)
								} else if round > 1 {
									wantVariant = maxNativeMetricMetadataVersions + int((round-2)%2)
								}
							} else if lookupState == "historical" {
								wantVariant--
							}
							for i, lookup := range lookups {
								want := fixture.options[wantVariant][fixture.familyBySeries[worker*perWorker+i]].Metadata
								if lookup.Metadata == nil || *lookup.Metadata != want {
									b.Errorf("unexpected metadata at %d: got %v, want %v", lookup.Timestamp, lookup.Metadata, want)
								}
							}
						}
					})
					b.StopTimer()
					for _, queue := range queues {
						close(queue)
					}
					consumers.Wait()
					b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*perWorker), "ns/sample")
				})
			}
		}
	}
}

// BenchmarkHeadMetricMetadataAppendFixedConcurrency keeps the series, worker
// count, batch size, and total work fixed while GOMAXPROCS changes.
// CPU counts that do not divide eight are skipped.
func BenchmarkHeadMetricMetadataAppendFixedConcurrency(b *testing.B) {
	const workers, perWorker, families = 8, 1000, 100
	for _, changedPercent := range []int{0, 1, 100} {
		for _, mode := range []metricMetadataBenchmarkMode{{name: "off"}, {name: "native", nativeEnabled: true}} {
			b.Run(fmt.Sprintf("change=%d/mode=%s", changedPercent, mode.name), func(b *testing.B) {
				procs := runtime.GOMAXPROCS(0)
				if procs > workers || workers%procs != 0 {
					b.Skipf("GOMAXPROCS must divide %d, got %d", workers, procs)
				}
				h, _, closeHead := newMetricMetadataBenchmarkHead(b, mode, 1_000_000_000, false)
				b.Cleanup(closeHead)
				fixture := newMetricMetadataBenchmarkFixture(workers*perWorker, families, maxNativeMetricMetadataVersions+2)
				refs := make([]storage.SeriesRef, workers*perWorker)
				for version := range maxNativeMetricMetadataVersions {
					appendMetricMetadataBenchmarkRound(b, h, fixture, refs, version, 100+int64(version))
				}
				var changed [workers * perWorker]bool
				for worker := range workers {
					for i := range perWorker {
						// Ten series per worker, spread across 80 distinct families.
						changed[worker*perWorker+i] = changedPercent == 100 || changedPercent == 1 && i%100 == (i/100+worker*10)%100
					}
				}
				var nextWorker atomic.Uint64
				var completed [workers]int64
				b.ReportAllocs()
				b.SetParallelism(workers / procs)
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					worker := int(nextWorker.Add(1) - 1)
					if worker >= workers {
						b.Fatal("too many workers")
					}
					var rounds int64
					for pb.Next() {
						app := h.AppenderV2(b.Context())
						for i := worker * perWorker; i < (worker+1)*perWorker; i++ {
							variant := maxNativeMetricMetadataVersions - 1
							if changed[i] {
								variant = maxNativeMetricMetadataVersions + int(rounds%2)
							}
							ref, err := app.Append(refs[i], fixture.labels[i], 0, 1000+rounds, float64(rounds), nil, nil, fixture.options[variant][fixture.familyBySeries[i]])
							if err != nil {
								b.Fatal(err)
							}
							refs[i] = ref
						}
						if err := app.Commit(); err != nil {
							b.Fatal(err)
						}
						rounds++
					}
					completed[worker] = rounds
				})
				b.StopTimer()
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*perWorker), "ns/sample")
				var rounds, evictions int64
				for _, n := range completed {
					rounds += n
				}
				if rounds != int64(b.N) || nextWorker.Load() != workers {
					b.Fatalf("unexpected completed work: %d rounds, %d workers", rounds, nextWorker.Load())
				}
				validateMetricMetadataBenchmarkSamples(b, h, workers*perWorker*maxNativeMetricMetadataVersions+rounds*perWorker)
				if !mode.nativeEnabled {
					validateMetricMetadataBenchmarkState(b, h, mode, fixture, refs, maxNativeMetricMetadataVersions)
					return
				}
				for i, ref := range refs {
					n := completed[i/perWorker]
					variant := maxNativeMetricMetadataVersions - 1
					updated := changed[i] && n > 0
					if updated {
						variant = maxNativeMetricMetadataVersions + int((n-1)%2)
						evictions += n
					}
					versions, truncated, ok := h.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
					if !ok || len(versions) != maxNativeMetricMetadataVersions || truncated != updated || !versions[len(versions)-1].Metadata.Equals(fixture.options[variant][fixture.familyBySeries[i]].Metadata) {
						b.Fatalf("unexpected metadata for series %d after %d rounds", i, n)
					}
				}
				if h.NumSeries() != workers*perWorker || h.nativeMetricMetadata.series.Load() != workers*perWorker || h.nativeMetricMetadata.versions.Load() != workers*perWorker*maxNativeMetricMetadataVersions || h.nativeMetricMetadata.evictions.Load() != uint64(evictions) {
					b.Fatal("unexpected series, version, or eviction counters")
				}
			})
		}
	}
}

// BenchmarkHeadMetricMetadataQueryAppendConcurrent measures one query and eight
// transactions per round. Operation timings exclude the round barriers.
func BenchmarkHeadMetricMetadataQueryAppendConcurrent(b *testing.B) {
	const workers, perWorker, numSeries = 8, 1000, 8000
	for _, every := range []int{1, 100} {
		for _, changing := range []bool{false, true} {
			b.Run(fmt.Sprintf("every=%d/changing=%t", every, changing), func(b *testing.B) {
				h, _, closeHead := newMetricMetadataBenchmarkHead(b, metricMetadataBenchmarkMode{nativeEnabled: true}, 1_000_000_000, false)
				b.Cleanup(closeHead)
				fixture := newMetricMetadataBenchmarkFixture(numSeries, 100, maxNativeMetricMetadataVersions+2)
				refs := make([]storage.SeriesRef, numSeries)
				appendRound := func(worker, variant int, timestamp int64) error {
					app := h.AppenderV2(b.Context())
					for i := worker * perWorker; i < (worker+1)*perWorker; i++ {
						var opts storage.AOptions
						if i%every == 0 {
							opts = fixture.options[variant][fixture.familyBySeries[i]]
						}
						ref, err := app.Append(refs[i], fixture.labels[i], 0, timestamp, float64(timestamp), nil, nil, opts)
						if err != nil {
							_ = app.Rollback()
							return err
						}
						refs[i] = ref
					}
					return app.Commit()
				}
				for version := range maxNativeMetricMetadataVersions {
					for worker := range workers {
						if err := appendRound(worker, version, 100+int64(version)); err != nil {
							b.Fatal(err)
						}
					}
				}
				matchers := [][]*labels.Matcher{{labels.MustNewMatcher(labels.MatchEqual, "job", "metadata-benchmark")}}
				expected, _, err := h.nativeMetricMetadataForMatchers(b.Context(), matchers, 10)
				if err != nil || len(expected) != 10 {
					b.Fatalf("invalid query fixture: %d rows, %v", len(expected), err)
				}
				type round struct {
					start <-chan struct{}
					n     int64
				}
				type result struct {
					reader  bool
					elapsed time.Duration
					err     error
				}
				var starts [workers + 1]chan round
				finished := make(chan result, workers+1)
				var wg sync.WaitGroup
				for worker := range starts {
					starts[worker] = make(chan round)
					wg.Go(func() {
						for current := range starts[worker] {
							<-current.start
							began := time.Now()
							var err error
							if worker == workers {
								var rows []NativeMetricMetadataSeries
								var truncated bool
								rows, truncated, err = h.nativeMetricMetadataForMatchers(b.Context(), matchers, 10)
								elapsed := time.Since(began)
								if err == nil {
									if len(rows) != len(expected) || !truncated {
										err = fmt.Errorf("unexpected query: %d rows, truncated=%t", len(rows), truncated)
									} else {
										for i, row := range rows {
											if !labels.Equal(row.Labels, expected[i].Labels) || len(row.Versions) != maxNativeMetricMetadataVersions {
												err = fmt.Errorf("unexpected query row %d", i)
												break
											}
										}
									}
								}
								finished <- result{reader: true, elapsed: elapsed, err: err}
							} else {
								variant := maxNativeMetricMetadataVersions - 1
								if changing {
									variant = maxNativeMetricMetadataVersions + int(current.n%2)
								}
								err = appendRound(worker, variant, 1000+current.n)
								finished <- result{elapsed: time.Since(began), err: err}
							}
						}
					})
				}
				b.Cleanup(func() {
					for _, ch := range starts {
						close(ch)
					}
					wg.Wait()
				})
				var readerTime, writerTime time.Duration
				var rounds int64
				b.ReportAllocs()
				for b.Loop() {
					start := make(chan struct{})
					for _, ch := range starts {
						ch <- round{start: start, n: rounds}
					}
					close(start)
					for range workers + 1 {
						got := <-finished
						if got.err != nil {
							b.Fatal(got.err)
						}
						if got.reader {
							readerTime += got.elapsed
						} else {
							writerTime += got.elapsed
						}
					}
					rounds++
				}
				b.ReportMetric(float64(readerTime.Nanoseconds())/float64(rounds), "reader-ns/query")
				b.ReportMetric(float64(writerTime.Nanoseconds())/float64(rounds*workers), "writer-ns/txn")
				validateMetricMetadataBenchmarkSamples(b, h, numSeries*(maxNativeMetricMetadataVersions+rounds))
				var evictions int64
				for i, ref := range refs {
					versions, truncated, ok := h.nativeMetricMetadata.get(chunks.HeadSeriesRef(ref))
					if i%every != 0 {
						if ok {
							b.Fatal("metadata density changed")
						}
						continue
					}
					variant := maxNativeMetricMetadataVersions - 1
					if changing && rounds > 0 {
						variant = maxNativeMetricMetadataVersions + int((rounds-1)%2)
						evictions += rounds
					}
					if !ok || len(versions) != maxNativeMetricMetadataVersions || truncated != (changing && rounds > 0) || !versions[len(versions)-1].Metadata.Equals(fixture.options[variant][fixture.familyBySeries[i]].Metadata) {
						b.Fatalf("unexpected metadata for series %d", i)
					}
				}
				if h.NumSeries() != numSeries || h.nativeMetricMetadata.series.Load() != int64(numSeries/every) || h.nativeMetricMetadata.versions.Load() != int64(numSeries/every*maxNativeMetricMetadataVersions) || h.nativeMetricMetadata.evictions.Load() != uint64(evictions) {
					b.Fatal("unexpected series, version, or eviction counters")
				}
			})
		}
	}
}

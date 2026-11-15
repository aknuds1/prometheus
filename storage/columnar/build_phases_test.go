// Copyright 2026 The Prometheus Authors
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

package columnar_test

// Ingestion instrumentation: append and seal timed separately, so that later
// changes can be attributed rather than inferred. A single build number dilutes
// every delta with append cost.

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
)

func BenchmarkBuildPhases(b *testing.B) {
	const series, perSeries = 10_000, 60
	f := newFixture(series, perSeries)
	total := float64(series * perSeries)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := columnar.NewStorage(b.TempDir(), columnar.StorageOptions{
			// Above the fixture size so Commit cannot seal; Flush is timed on its own.
			FlushSamples: series * perSeries * 4,
			DisableWAL:   true,
		})
		if err != nil {
			b.Fatal(err)
		}
		app := st.AppenderV2(context.Background())

		appendStart := time.Now()
		for s := range f.labels {
			for _, sample := range f.samples[s] {
				if _, err := app.Append(0, f.labels[s], 0, sample.T(), sample.F(), nil, nil, storage.AOptions{}); err != nil {
					b.Fatal(err)
				}
			}
		}
		if err := app.Commit(); err != nil {
			b.Fatal(err)
		}
		appendDur := time.Since(appendStart)

		sealStart := time.Now()
		if err := st.Flush(); err != nil {
			b.Fatal(err)
		}
		sealDur := time.Since(sealStart)
		totalDur := appendDur + sealDur
		if err := st.Close(); err != nil {
			b.Fatal(err)
		}

		b.ReportMetric(float64(appendDur.Nanoseconds())/total, "append-ns/sample")
		b.ReportMetric(float64(sealDur.Nanoseconds())/total, "seal-ns/sample")
		b.ReportMetric(total/totalDur.Seconds(), "samples/s")
	}
}

// BenchmarkAppendRefReuse settles whether a reused storage.SeriesRef skips label
// hashing. Scrape passes the returned ref back on every subsequent append; the
// scaling harness passes 0, so if ref reuse works the harness has been paying a
// cost that production does not.
func BenchmarkAppendRefReuse(b *testing.B) {
	f := newFixture(1, 600)
	const samples = 600
	for _, mode := range []string{"zero_ref", "reuse_ref"} {
		b.Run(mode, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				st, err := columnar.NewStorage(b.TempDir(), columnar.StorageOptions{
					FlushSamples: 1 << 30,
					DisableWAL:   true,
				})
				if err != nil {
					b.Fatal(err)
				}
				app := st.AppenderV2(context.Background())
				var ref storage.SeriesRef

				b.StartTimer()
				for _, sample := range f.samples[0] {
					if mode == "reuse_ref" {
						ref, err = app.Append(ref, f.labels[0], 0, sample.T(), sample.F(), nil, nil, storage.AOptions{})
					} else {
						_, err = app.Append(0, f.labels[0], 0, sample.T(), sample.F(), nil, nil, storage.AOptions{})
					}
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				if err := app.Commit(); err != nil {
					b.Fatal(err)
				}
				if err := st.Close(); err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*samples), "ns/sample")
			}
		})
	}
}

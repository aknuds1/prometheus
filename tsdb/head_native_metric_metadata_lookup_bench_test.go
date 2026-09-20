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
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
)

// BenchmarkHeadMetricMetadataLookup isolates timestamp-aware lookup over fixed
// histories. Each operation visits the entire working set, including value
// copies; setup and result validation are outside timing. Parallel cases share
// one Head but have independent result buffers, modeling multiple destinations.
func BenchmarkHeadMetricMetadataLookup(b *testing.B) {
	const series = 4096
	for _, tc := range []struct {
		name              string
		values, helpBytes int
	}{
		{"shared", 100, 64},
		{"distinct1000", 1000, 64},
		{"distinct4096", series, 64},
		{"oversized", 4, 256 << 10},
	} {
		for _, state := range []string{"current", "historical", "missing", "disabled"} {
			for _, parallel := range []bool{false, true} {
				b.Run(fmt.Sprintf("values=%s/state=%s/parallel=%t", tc.name, state, parallel), func(b *testing.B) {
					h, _, closeHead := newMetricMetadataBenchmarkHead(b, metricMetadataBenchmarkMode{nativeEnabled: state != "disabled"}, 1_000_000_000, false)
					b.Cleanup(closeHead)
					refs := make([]storage.SeriesRef, series)
					values := make([]metadata.Metadata, tc.values)
					for i := range values {
						values[i].Type = model.MetricTypeCounter
						values[i].Help = strconv.Itoa(i) + strings.Repeat("x", tc.helpBytes)
					}
					for version := range 2 {
						app := h.AppenderV2(b.Context())
						for i := range refs {
							m := values[i%len(values)]
							if version == 1 {
								m = metadata.Metadata{Type: model.MetricTypeCounter, Help: "current"}
							}
							var err error
							refs[i], err = app.Append(refs[i], labels.FromStrings(labels.MetricName, "lookup", "id", strconv.Itoa(i)), 0, int64(100+100*version), 1, nil, nil, storage.AOptions{Metadata: m})
							require.NoError(b, err)
						}
						require.NoError(b, app.Commit())
					}
					timestamp := map[string]int64{"current": 200, "historical": 100, "missing": 50, "disabled": 100}[state]
					lookups := make([]storage.NativeMetricMetadataLookup, series)
					for i, ref := range refs {
						lookups[i] = storage.NativeMetricMetadataLookup{Ref: ref, Timestamp: timestamp}
					}
					b.ReportAllocs()
					b.ResetTimer()
					if parallel {
						b.RunParallel(func(pb *testing.PB) {
							local := append([]storage.NativeMetricMetadataLookup(nil), lookups...)
							for pb.Next() {
								if err := h.LookupNativeMetricMetadata(b.Context(), local); err != nil {
									b.Error(err)
									return
								}
							}
						})
					} else {
						for b.Loop() {
							if err := h.LookupNativeMetricMetadata(b.Context(), lookups); err != nil {
								b.Fatal(err)
							}
						}
					}
					b.StopTimer()
					require.NoError(b, h.LookupNativeMetricMetadata(b.Context(), lookups))
					for i, lookup := range lookups {
						switch state {
						case "historical":
							require.Equal(b, &values[i%len(values)], lookup.Metadata)
						case "current":
							require.Equal(b, &metadata.Metadata{Type: model.MetricTypeCounter, Help: "current"}, lookup.Metadata)
						default:
							require.Nil(b, lookup.Metadata)
						}
					}
				})
			}
		}
	}
}

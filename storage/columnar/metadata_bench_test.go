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

package columnar_test

import (
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/util/teststorage"
)

// BenchmarkInfoMetadataStageE compares identical sample-series metadata
// enrichment. The TSDB control has a real region/host target_info series; the
// columnar arm resolves the equivalent shared Resource owner/version per step.
func BenchmarkInfoMetadataStageE(b *testing.B) {
	const seriesCount = 500
	allLabels, timestamps, values := syntheticSeries(seriesCount, 100)
	start := time.UnixMilli(timestamps[0][0])
	end := time.UnixMilli(timestamps[0][len(timestamps[0])-1])

	owners := columnar.ResourceOwner(map[string]string{
		"service.name": "node_exporter", "service.instance.id": "localhost:9100",
	})
	resourceContent := columnar.Content{Attributes: map[string]string{"host.name": "host-1", "region": "east"}}
	nativeSeries := make([]columnar.BatchSeries, 0, seriesCount)
	associations := make([]columnar.AssociationUpdate, 0, seriesCount)
	for i := range seriesCount {
		ls := labels.NewBuilder(allLabels[i]).Set(columnar.OTELResourceLabel, owners.Ref()).Labels()
		nativeSeries = append(nativeSeries, columnar.BatchSeries{
			Labels: ls, Timestamps: timestamps[i], StartTimestamps: make([]int64, len(timestamps[i])),
			Kinds: make([]columnar.SampleKind, len(timestamps[i])), Values: values[i], Histograms: make([][]byte, len(timestamps[i])),
		})
		associations = append(associations, columnar.AssociationUpdate{Labels: ls, Update: columnar.Update{
			Owner: owners, EffectiveFrom: timestamps[i][0], Content: resourceContent,
		}})
	}

	b.Run("columnar", func(b *testing.B) {
		s, err := columnar.NewStorage(b.TempDir(), columnar.StorageOptions{
			FlushSamples: seriesCount * len(timestamps[0]),
			DisableWAL:   true,
		})
		require.NoError(b, err)
		defer s.Close()
		require.NoError(b, s.AppendBatch(b.Context(), columnar.Batch{Series: nativeSeries, Updates: associations}))
		engine := stageEInfoEngine(b, true)
		benchmarkStageEInfo(b, engine, s, start, end, seriesCount)
	})

	b.Run("tsdb", func(b *testing.B) {
		s := teststorage.New(b)
		app := s.AppenderV2(b.Context())
		for i := range seriesCount {
			for j, timestamp := range timestamps[i] {
				_, err := app.Append(0, allLabels[i], 0, timestamp, values[i][j], nil, nil, storage.AOptions{})
				require.NoError(b, err)
			}
		}
		targetInfo := labels.FromStrings(
			labels.MetricName, "target_info", "job", "node_exporter", "instance", "localhost:9100",
			"host.name", "host-1", "region", "east",
		)
		md := model.MetricTypeGauge
		for _, timestamp := range timestamps[0] {
			_, err := app.Append(0, targetInfo, 0, timestamp, 1, nil, nil, storage.AOptions{
				Metadata: metadata.Metadata{Type: md},
			})
			require.NoError(b, err)
		}
		require.NoError(b, app.Commit())
		engine := stageEInfoEngine(b, true)
		benchmarkStageEInfo(b, engine, s, start, end, seriesCount)
	})
}

func benchmarkStageEInfo(b *testing.B, engine promql.QueryEngine, queryable storage.Queryable, start, end time.Time, count int) {
	b.Helper()
	b.ReportAllocs()
	b.ReportMetric(float64(count), "series/op")
	b.ResetTimer()
	for range b.N {
		query, err := engine.NewRangeQuery(b.Context(), queryable, nil, `info(node_cpu_seconds_total)`, start, end, 15*time.Second)
		require.NoError(b, err)
		result := query.Exec(b.Context())
		require.NoError(b, result.Err)
		matrix, ok := result.Value.(promql.Matrix)
		require.True(b, ok)
		require.Len(b, matrix, count)
		query.Close()
	}
}

func stageEInfoEngine(tb testing.TB, utf8Allowed bool) promql.QueryEngine {
	tb.Helper()
	return promqltest.NewTestEngineWithOpts(tb, promql.EngineOpts{
		MaxSamples:               50_000_000,
		Timeout:                  5 * time.Minute,
		LookbackDelta:            5 * time.Minute,
		EnableAtModifier:         true,
		EnableNegativeOffset:     true,
		EnableDelayedNameRemoval: true,
		Parser:                   parser.NewParser(promqltest.TestParserOpts),
		LabelNamerConfig: &promql.LabelNamerConfig{
			UTF8Allowed: utf8Allowed,
		},
	})
}

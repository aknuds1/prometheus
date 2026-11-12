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

// Mechanism accounting for the ingestion path: how many label hashes each call
// site performs for a scrape-shaped transaction. Counters are per transaction
// and published once per commit, so they are exact without adding a per-sample
// atomic to the path being measured.

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
)

func ingestTestLabels(i int) labels.Labels {
	return labels.FromStrings(
		labels.MetricName, "node_cpu_seconds_total",
		"instance", "localhost:9100",
		"job", "node_exporter",
		"cpu", strconv.Itoa(i%16),
		"mode", "idle",
		"series", strconv.Itoa(i),
	)
}

// hashCounts is the counter subset a test asserts on.
type hashCounts struct {
	seriesFor, refFor, prepare, prepareSeries, trusted, fallback uint64
}

func readHashCounts(s *columnar.Storage) hashCounts {
	stats := s.ReadStats()
	return hashCounts{
		seriesFor: stats.HashCallsSeriesFor, refFor: stats.HashCallsRefFor,
		prepare: stats.HashCallsPrepare, prepareSeries: stats.HashCallsPrepareSeries,
		trusted: stats.RefTrusted, fallback: stats.RefFallback,
	}
}

func (h hashCounts) sub(o hashCounts) hashCounts {
	return hashCounts{
		seriesFor: h.seriesFor - o.seriesFor, refFor: h.refFor - o.refFor,
		prepare: h.prepare - o.prepare, prepareSeries: h.prepareSeries - o.prepareSeries,
		trusted: h.trusted - o.trusted, fallback: h.fallback - o.fallback,
	}
}

// TestIngestHashCountsEstablished pins the per-call-site hash count for a
// scrape-shaped round against an already known series, with the reference
// returned by the previous round passed back in.
func TestIngestHashCountsEstablished(t *testing.T) {
	const series = 100
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{
		FlushSamples: 1 << 30,
		DisableWAL:   true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	refs := make([]storage.SeriesRef, series)
	appendRound := func(round int) {
		app := s.AppenderV2(context.Background())
		for i := range series {
			ref, err := app.Append(refs[i], ingestTestLabels(i), 0, int64(round)*15_000, float64(round), nil, nil, storage.AOptions{})
			require.NoError(t, err)
			refs[i] = ref
		}
		require.NoError(t, app.Commit())
	}
	appendRound(0)

	before := readHashCounts(s)
	appendRound(1)
	got := readHashCounts(s).sub(before)

	// One sample per series per round, so every count is per sample.
	require.Equal(t, hashCounts{
		seriesFor: series, refFor: series, prepare: series, prepareSeries: series,
		trusted: 0, fallback: series,
	}, got, "established-series round hash accounting")
}

// TestIngestHashCountsFirstSeen pins the same accounting for identities that
// have never been appended, where the reference cannot be reused.
func TestIngestHashCountsFirstSeen(t *testing.T) {
	const series = 100
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{
		FlushSamples: 1 << 30,
		DisableWAL:   true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	before := readHashCounts(s)
	app := s.AppenderV2(context.Background())
	for i := range series {
		_, err := app.Append(0, ingestTestLabels(i), 0, 0, 1, nil, nil, storage.AOptions{})
		require.NoError(t, err)
	}
	require.NoError(t, app.Commit())
	got := readHashCounts(s).sub(before)

	require.Equal(t, hashCounts{
		seriesFor: series, refFor: series, prepare: series, prepareSeries: series,
		trusted: 0, fallback: series,
	}, got, "first-seen round hash accounting")
}

// TestIngestHashCountsTypedBatch pins the accounting for a direct typed batch,
// which never goes through the scalar appender.
func TestIngestHashCountsTypedBatch(t *testing.T) {
	const series = 100
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{
		FlushSamples: 1 << 30,
		DisableWAL:   true,
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	batch := columnar.Batch{Series: make([]columnar.BatchSeries, series)}
	for i := range batch.Series {
		batch.Series[i] = columnar.BatchSeries{
			Labels:     ingestTestLabels(i),
			Timestamps: []int64{0},
			Kinds:      []columnar.SampleKind{columnar.SampleFloat},
			Values:     []float64{1},
		}
	}

	before := readHashCounts(s)
	require.NoError(t, s.AppendBatch(context.Background(), batch))
	got := readHashCounts(s).sub(before)

	require.Equal(t, hashCounts{prepare: series, prepareSeries: series}, got, "typed batch hash accounting")
}

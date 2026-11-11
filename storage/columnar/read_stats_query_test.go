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

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
)

// costModelStorage builds the multi-block layout so that a narrow window really
// does exclude segments; a single-block fixture cannot distinguish pruning that
// works from pruning that never had anything to skip.
func costModelStorage(t *testing.T) *columnar.Storage {
	t.Helper()
	const series, samples, group = 50, 100, 10
	allLabels, timestamps, values := syntheticSeries(series, samples)
	st, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{FlushSamples: group * series * 4})
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	app := st.AppenderV2(t.Context())
	for j := range samples {
		for i := range series {
			require.NoError(t, func() error {
				_, err := app.Append(0, allLabels[i], 0, timestamps[i][j], values[i][j], nil, nil, storage.AOptions{})
				return err
			}())
		}
		if (j+1)%group == 0 {
			require.NoError(t, app.Commit())
			app = st.AppenderV2(t.Context())
		}
	}
	require.NoError(t, app.Commit())
	return st
}

func TestQueryCostCountersDescribePruning(t *testing.T) {
	st := costModelStorage(t)
	before := st.ReadStats()
	require.Zero(t, before.Queries)

	// The sample layout starts at a fixed epoch and steps by 15s; this window
	// covers only the first few samples of the first block.
	q, err := st.Querier(1_700_000_000_000, 1_700_000_030_000)
	require.NoError(t, err)
	defer q.Close()
	set := q.Select(t.Context(), false, nil, labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "node_cpu_seconds_total"))
	for set.Next() {
		it := set.At().Iterator(nil)
		for it.Next() != 0 {
			break
		}
	}
	require.NoError(t, set.Err())

	after := st.ReadStats()
	require.Equal(t, uint64(1), after.Queries-before.Queries)
	require.NotZero(t, after.SeriesMatched-before.SeriesMatched)
	require.NotZero(t, after.BlocksOverlapping-before.BlocksOverlapping)
	require.NotZero(t, after.SegmentsPlanned-before.SegmentsPlanned)
	require.NotZero(t, after.PayloadBytesRead-before.PayloadBytesRead)
	// Characterisation, not an aspiration: a series holds one segment per block and
	// each segment spans that whole block, so a narrow window inside a large block
	// leaves nothing for the per-segment time bound to skip. Time pruning is
	// therefore block-granular today and gains nothing from the segment bounds.
	// If a future layout emits finer segments this assertion must change, which is
	// the point of pinning it.
	require.Zero(t, after.SegmentsOutsideWindow-before.SegmentsOutsideWindow)
	require.GreaterOrEqual(t, after.SegmentsAvailable-before.SegmentsAvailable, after.SegmentsPlanned-before.SegmentsPlanned)
	require.LessOrEqual(t, after.SegmentsRead-before.SegmentsRead, after.SegmentsPlanned-before.SegmentsPlanned)
}

func TestReadStatsHandlerServesCostModel(t *testing.T) {
	st := costModelStorage(t)
	q, err := st.Querier(1_700_000_000_000, 1_700_000_300_000)
	require.NoError(t, err)
	defer q.Close()
	require.True(t, q.Select(t.Context(), false, nil, labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "node_cpu_seconds_total")).Next())

	rec := httptest.NewRecorder()
	columnar.NewReadStatsHandler(st).ServeHTTP(rec, httptest.NewRequest("GET", "/debug/readstats", nil))
	require.Equal(t, 200, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	for _, key := range []string{"SeriesMatched", "SegmentsAvailable", "SegmentsPlanned", "SegmentsOutsideWindow", "BlocksOverlapping", "Queries", "Timings"} {
		require.Contains(t, got, key)
	}
}

func TestHeadIndexNarrowsScanToMetricName(t *testing.T) {
	st, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{FlushSamples: 1_000_000, DisableWAL: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	// Three metric names with distinguishable cardinality, all left in the head.
	shapes := map[string]int{"alpha_total": 40, "beta_total": 7, "gamma_total": 3}
	app := st.AppenderV2(t.Context())
	for name, n := range shapes {
		for i := range n {
			lset := labels.FromStrings(labels.MetricName, name, "i", strconv.Itoa(i))
			_, err := app.Append(0, lset, 0, 1_700_000_000_000, float64(i), nil, nil, storage.AOptions{})
			require.NoError(t, err)
		}
	}
	require.NoError(t, app.Commit())

	before := st.ReadStats()
	q, err := st.Querier(1_600_000_000_000, 1_800_000_000_000)
	require.NoError(t, err)
	defer q.Close()
	set := q.Select(t.Context(), false, nil, labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "beta_total"))
	var got []string
	for set.Next() {
		got = append(got, set.At().Labels().Get("i"))
	}
	require.NoError(t, set.Err())
	require.Len(t, got, shapes["beta_total"])
	slices.Sort(got)
	require.Equal(t, []string{"0", "1", "2", "3", "4", "5", "6"}, got)

	after := st.ReadStats()
	require.Equal(t, uint64(1), after.HeadIndexHits-before.HeadIndexHits)
	// The scan must cover only the matching series, not the whole head.
	require.Equal(t, uint64(shapes["beta_total"]), after.HeadSeriesScanned-before.HeadSeriesScanned)

	// A regex name matcher cannot use the index and must fall back to a full scan.
	before = st.ReadStats()
	q2, err := st.Querier(1_600_000_000_000, 1_800_000_000_000)
	require.NoError(t, err)
	defer q2.Close()
	re := q2.Select(t.Context(), false, nil, labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, "beta.*"))
	count := 0
	for re.Next() {
		count++
	}
	require.NoError(t, re.Err())
	require.Equal(t, shapes["beta_total"], count)
	after = st.ReadStats()
	require.Equal(t, uint64(0), after.HeadIndexHits-before.HeadIndexHits)
	require.Equal(t, uint64(50), after.HeadSeriesScanned-before.HeadSeriesScanned)
}

func TestRegexMatcherResolutionIsCounted(t *testing.T) {
	st := costModelStorage(t)
	query := func(m *labels.Matcher) columnar.ReadStats {
		before := st.ReadStats()
		q, err := st.Querier(1_700_000_000_000, 1_700_000_300_000)
		require.NoError(t, err)
		defer q.Close()
		set := q.Select(t.Context(), false, nil, m)
		for set.Next() {
		}
		require.NoError(t, set.Err())
		return subStats(st.ReadStats(), before)
	}
	eq := query(labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "node_cpu_seconds_total"))
	re := query(labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, "node_cpu_seconds.*"))

	// An equality matcher resolves by binary search and never walks the postings.
	require.Zero(t, eq.PostingsScanned)
	require.Zero(t, eq.MatcherCalls)
	// A regex matcher walks them, and must record how many it looked at.
	require.NotZero(t, re.PostingsScanned)
	require.NotZero(t, re.MatcherCalls)
	require.LessOrEqual(t, re.PostingsNameMatched, re.PostingsScanned)
	require.Equal(t, eq.SelectCalls, re.SelectCalls)
}

// sub returns the counters that advanced between the receiver and earlier.
func subStats(r, earlier columnar.ReadStats) columnar.ReadStats {
	return columnar.ReadStats{
		PostingsScanned:     r.PostingsScanned - earlier.PostingsScanned,
		PostingsNameMatched: r.PostingsNameMatched - earlier.PostingsNameMatched,
		MatcherCalls:        r.MatcherCalls - earlier.MatcherCalls,
		SelectCalls:         r.SelectCalls - earlier.SelectCalls,
	}
}

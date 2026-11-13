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

// Route-level accounting and complete-state digests for the ordinary OTLP corpus the
// primary-ingestion benchmark times. The counter is a test-local wrapper around the
// appender the receiver is given, so it measures the ordinary boundary without adding
// fields to the storage's exported statistics and without instrumenting the timed arms
// (the wrapper is only used by this test).

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
)

// ordinaryBoundaryCounts is what the ordinary consumer asks the appender for.
type ordinaryBoundaryCounts struct {
	appends int
	// appendsWithMetadata counts appends that carried non-empty AOptions.Metadata,
	// which is what turns into one metadata observation each.
	appendsWithMetadata int
	// series and contexts count distinct complete label sets and distinct
	// (series, metadata) pairs, which is the derivation work a per-series and
	// per-context reuse could share.
	series   map[string]struct{}
	contexts map[string]struct{}
}

// countingAppendableV2 records the ordinary consumer's appender calls.
type countingAppendableV2 struct {
	storage.AppendableV2
	counts *ordinaryBoundaryCounts
}

func (a countingAppendableV2) AppenderV2(ctx context.Context) storage.AppenderV2 {
	return countingAppenderV2{AppenderV2: a.AppendableV2.AppenderV2(ctx), counts: a.counts}
}

type countingAppenderV2 struct {
	storage.AppenderV2
	counts *ordinaryBoundaryCounts
}

func (a countingAppenderV2) Append(ref storage.SeriesRef, ls labels.Labels, st, t int64, v float64, h *histogram.Histogram, fh *histogram.FloatHistogram, opts storage.AOptions) (storage.SeriesRef, error) {
	a.counts.appends++
	series := ls.String()
	a.counts.series[series] = struct{}{}
	if !opts.Metadata.IsEmpty() {
		a.counts.appendsWithMetadata++
		a.counts.contexts[fmt.Sprintf("%s\x00%s\x00%s\x00%s", series, opts.Metadata.Type, opts.Metadata.Unit, opts.Metadata.Help)] = struct{}{}
	}
	return a.AppenderV2.Append(ref, ls, st, t, v, h, fh, opts)
}

// TestOrdinaryOTLPBoundaryCounts measures the ordinary corpus the benchmark times: how
// many observations the receiver produces, how many distinct series state them, and how
// many distinct exact contexts those series use. It is the route-level figure the
// per-sample derivation cost is multiplied by.
func TestOrdinaryOTLPBoundaryCounts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape ingestShape
		ring  int
	}{
		{"B-1000x100", ingestShape{series: 1_000, perSeries: 100}, 3},
		{"C-10000x10", ingestShape{series: 10_000, perSeries: 10}, 2},
		{"B-sparse", ingestShape{series: 1_000, perSeries: 10, intervalMs: 10 * primaryIntervalMs}, 2},
		{"B-first-seen", ingestShape{series: 1_000, perSeries: 100, firstSeen: true}, 2},
		{"B-metric-help-change", ingestShape{series: 1_000, perSeries: 100, metricHelpChurn: true}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := buildIngestRing(tc.shape, tc.ring)
			metrics := otlpRing(entries)
			st := newPrimaryStorage(t)
			counts := &ordinaryBoundaryCounts{series: map[string]struct{}{}, contexts: map[string]struct{}{}}
			consumer := ordinaryConsumer(t, countingAppendableV2{AppendableV2: st, counts: counts})
			for i := range metrics {
				require.NoError(t, consumer.ConsumeMetrics(context.Background(), metrics[i]))
			}

			wantSamples := 0
			for _, entry := range entries {
				wantSamples += entry.samples
			}
			require.Equal(t, wantSamples, counts.appends, "one append per stored sample")
			require.Equal(t, wantSamples, counts.appendsWithMetadata,
				"the ordinary translator states metadata on every datapoint it appends")

			// One gauge and one sum series per logical record identity, and one exact
			// metadata context per (identity, metric) pair: a shape that changes the
			// metric description states two contexts for the same series, while a
			// first-seen shape states a new identity per entry.
			wantSeries, wantContexts := ordinaryDistinctShape(entries)
			require.Equal(t, wantSeries, len(counts.series), "distinct series the receiver appends to")
			require.Equal(t, wantContexts, len(counts.contexts),
				"distinct (series, exact metadata context) pairs")

			mint, maxt := fixtureRange(entries)
			ownerCount := len(ordinaryOwners(t, st, mint, maxt))
			require.Equal(t, wantSeries, ownerCount, "one Metric owner per distinct series")

			t.Logf("%s: commits=%d samples/commit=%d series=%d contexts=%d metric-owners=%d",
				tc.name, len(entries), entries[0].samples, len(counts.series), len(counts.contexts), ownerCount)
		})
	}
}

// ordinaryStoredSeries lists every stored series' complete labels.
func ordinaryStoredSeries(t *testing.T, q storage.Queryable, mint, maxt int64) []labels.Labels {
	t.Helper()
	querier, err := q.Querier(mint, maxt)
	require.NoError(t, err)
	defer func() { require.NoError(t, querier.Close()) }()
	set := querier.Select(context.Background(), true, nil,
		labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+"))
	var out []labels.Labels
	for set.Next() {
		out = append(out, set.At().Labels())
	}
	require.NoError(t, set.Err())
	slices.SortFunc(out, func(a, b labels.Labels) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// ordinaryDistinctShape counts the distinct series and distinct (series, metadata
// context) pairs the fixture's entries state, from the fixture rather than from the
// storage being checked.
func ordinaryDistinctShape(entries []ingestEntry) (series, contexts int) {
	identities := map[string]struct{}{}
	pairs := map[string]struct{}{}
	for _, entry := range entries {
		for _, rec := range entry.records {
			key := rec.labels.String()
			identities[key] = struct{}{}
			for _, name := range []string{primaryGaugeName, primarySumName} {
				pairs[key+"\x00"+name+"\x00"+entry.metricHelp] = struct{}{}
			}
		}
	}
	return 2 * len(identities), len(pairs)
}

// ordinaryOwners enumerates the Metric owners the ordinary corpus recorded. It asks
// for one unpaginated page: QueryMetadata's owner cursor does not advance past an
// already-returned page, which is a pre-existing pagination defect outside this
// experiment's scope.
func ordinaryOwners(t *testing.T, s *columnar.Storage, mint, maxt int64) []string {
	t.Helper()
	page, err := s.QueryMetadata(context.Background(), columnar.MetadataQuery{
		Kind: columnar.MetadataMetric, Start: mint, End: maxt,
	})
	require.NoError(t, err)
	require.Empty(t, page.NextToken)
	out := make([]string, 0, len(page.Owners))
	for _, owner := range page.Owners {
		out = append(out, owner.ID)
	}
	return out
}

// TestOrdinaryOTLPCompleteStateDigest pins the complete logical state of the ordinary
// corpus the benchmark times: every stored sample against the stock TSDB control, and
// every Metric owner version with its observation bounds. The digest is logged so two
// builds of the same corpus can be compared exactly.
func TestOrdinaryOTLPCompleteStateDigest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape ingestShape
		ring  int
	}{
		{"B-1000x100", ingestShape{series: 100, perSeries: 100}, 3},
		{"B-metric-help-change", ingestShape{series: 100, perSeries: 100, metricHelpChurn: true}, 3},
		{"B-sparse", ingestShape{series: 100, perSeries: 10, intervalMs: 10 * primaryIntervalMs}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := buildIngestRing(tc.shape, tc.ring)
			metrics := otlpRing(entries)
			columnarStore := newPrimaryStorage(t)
			tsdbControl := openPrimaryTSDB(t)

			colConsumer := ordinaryConsumer(t, columnarStore)
			tsdbConsumer := ordinaryConsumer(t, tsdbControl)
			for i := range metrics {
				require.NoError(t, colConsumer.ConsumeMetrics(context.Background(), metrics[i]))
				require.NoError(t, tsdbConsumer.ConsumeMetrics(context.Background(), metrics[i]))
			}

			mint, maxt := fixtureRange(entries)
			colSamples := canonicalStored(t, columnarStore, mint, maxt, false)
			tsdbSamples := canonicalStored(t, tsdbControl, mint, maxt, false)
			require.Equal(t, tsdbSamples, colSamples,
				"the ordinary route stores exactly the conventional logical result")

			// The series set is the one the two engines agree on above; the metadata
			// expectations come from the fixture's commit sequence.
			series := ordinaryStoredSeries(t, columnarStore, mint, maxt)
			owners := ordinaryMetricOwners(t, columnarStore, series)

			wantVersions := 1
			if tc.shape.metricHelpChurn {
				wantVersions = len(entries)
			}
			for _, ls := range series {
				history, ok := columnarStore.MetadataHistory(columnar.MetricOwner(ls))
				require.True(t, ok, "series %s must have a Metric owner", ls)
				require.Len(t, history.Versions, wantVersions, "series %s", ls)
				for i, version := range history.Versions {
					first := entries[i].records[0].ts[0]
					last := entries[i].records[0].ts[len(entries[0].records[0].ts)-1]
					if wantVersions == 1 {
						// One unchanged content: the single version's bound covers every commit.
						last = entries[len(entries)-1].records[0].ts[len(entries[0].records[0].ts)-1]
					}
					require.Equal(t, first, version.EffectiveFrom, "series %s version %d", ls, i)
					require.Equal(t, first, version.ObservedFrom, "series %s version %d", ls, i)
					require.Equal(t, last, version.ObservedUntil, "series %s version %d", ls, i)
					wantType := "counter"
					if ls.Get(labels.MetricName) == "primary_gauge_ratio" {
						wantType = "gauge"
					}
					require.Equal(t, wantType, version.Content.Attributes["type"], "series %s version %d", ls, i)
					require.Equal(t, "", version.Content.Attributes["unit"])
					require.Equal(t, entries[i].metricHelp, version.Content.Attributes["help"], "series %s version %d", ls, i)
				}
			}

			digest := ordinaryStateDigest(colSamples, owners)
			t.Logf("%s: series=%d samples=%d owners=%d versions=%d digest=%s",
				tc.name, len(series), len(colSamples), len(owners), wantVersions, digest)
		})
	}
}

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

// Complete native association state, recorded before and after a change to how
// the native builder derives associations.
//
// A sample-count or sample-multiset check cannot see a derivation change:
// timestamps and values stay correct while the identity and content a sample is
// associated with move. This harness therefore records every public result of a
// bounded native corpus in one canonical document: complete labels, every
// sample's timestamp, exact value bits, kind and start timestamp, every owner
// history with its content, effective and observation bounds, owner pages,
// memberships with their validity and version views, MetadataAtLabels before, at
// and after each boundary, and complete info() output.
//
// The document is compared against a checked-in golden, so the exact pre-change
// state is the control. Regenerate with -update-native-parity only when a
// reviewed semantic change is intended; a mechanism change must leave it
// byte-identical.
//
// Only the live head is recorded. WAL replay, explicit flush and reopened blocks
// are exercised with complete expectations by the accepted native observation and
// identity suites, which also run against this mechanism.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/storage/remote/otap"
)

// updateNativeParityGolden rewrites the recorded native state instead of comparing
// it. It is a review aid for an intended semantic change, never for a mechanism
// change.
var updateNativeParityGolden = flag.Bool("update-native-parity", false,
	"rewrite the recorded native association state golden")

// nativeParityCase is one bounded corpus. Every case is small enough to run in the
// ordinary unit-test suite and to review as a golden.
type nativeParityCase struct {
	name  string
	shape ingestShape
	ring  int
}

func nativeParityCases() []nativeParityCase {
	return []nativeParityCase{
		{name: "dense", shape: ingestShape{series: 8, perSeries: 5}, ring: 3},
		{name: "per_series_resources", shape: ingestShape{series: 6, perSeries: 4, resources: resourcePerSeries}, ring: 2},
		{name: "first_seen", shape: ingestShape{series: 4, perSeries: 4, firstSeen: true}, ring: 2},
		{name: "sparse", shape: ingestShape{series: 6, perSeries: 2, intervalMs: 10 * primaryIntervalMs}, ring: 2},
		{name: "changing_content", shape: ingestShape{series: 6, perSeries: 4, contentChurn: true}, ring: 3},
		{name: "multi_resource_changing_content", shape: ingestShape{
			series: 4, perSeries: 3, resources: resourcePerSeries, contentChurn: true,
		}, ring: 2},
	}
}

// nativeParitySeries is the complete stored state of one series. Every sample is
// rendered as timestamp/value-bits/kind/start, so a changed float bit, sample kind
// or start timestamp cannot hide behind an equal-looking value.
type nativeParitySeries struct {
	Labels  string   `json:"labels"`
	Samples []string `json:"samples"`
}

// nativeParityVersion is one owner version with its validity and observation bounds.
type nativeParityVersion struct {
	Content       string `json:"content"`
	EffectiveFrom int64  `json:"effective_from"`
	ObservedFrom  int64  `json:"observed_from"`
	ObservedUntil int64  `json:"observed_until"`
}

// nativeParityOwner is one owner's complete history.
type nativeParityOwner struct {
	Owner     string                `json:"owner"`
	Truncated bool                  `json:"truncated"`
	Versions  []nativeParityVersion `json:"versions"`
}

// nativeParityPageOwner is one owner page entry: identity, truncation and the
// versions whose validity overlaps the query period.
type nativeParityPageOwner struct {
	Identifying      map[string]string     `json:"identifying"`
	HistoryTruncated bool                  `json:"history_truncated"`
	Versions         []nativeParityVersion `json:"versions"`
}

// nativeParityDump is every public result of one case, in canonical order.
type nativeParityDump struct {
	Series      []nativeParitySeries `json:"series"`
	Owners      []nativeParityOwner  `json:"owners"`
	Pages       map[string][]string  `json:"owner_pages"`
	Memberships map[string][]string  `json:"memberships"`
	AtLabels    []string             `json:"metadata_at_labels"`
	Info        map[string][]string  `json:"info"`
}

// TestNativeAssociationCompleteState records and compares the complete native
// state of the parity corpus.
func TestNativeAssociationCompleteState(t *testing.T) {
	for _, tc := range nativeParityCases() {
		t.Run(tc.name, func(t *testing.T) {
			entries := buildIngestRing(tc.shape, tc.ring)
			batches := otapRing(t, entries)

			store := newPrimaryStorage(t)
			engine := nativeParityEngine(t)
			sink := columnar.NewOTAPSink(store, slog.New(slog.DiscardHandler))
			stream := otap.NewStreamConsumer(nil, otap.WithRecordSink(sink))
			t.Cleanup(func() { require.NoError(t, stream.Close()) })
			for i := range batches[:tc.ring] {
				require.NoError(t, stream.ConsumeBatch(context.Background(), batches[i]))
			}

			// The logical expectation of which series and samples exist is derived
			// from the fixture, not from the store, so the golden cannot excuse a
			// changed sample set or a changed series identity.
			requireNativeSamples(t, store, entries)

			owners := nativeParityExpectedOwners(t, store, entries)
			dump := captureNativeParityState(t, store, engine, owners)

			encoded, err := json.MarshalIndent(dump, "", "  ")
			require.NoError(t, err)
			path := filepath.Join("testdata", "native_association_state", tc.name+".json")
			if *updateNativeParityGolden {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, append(encoded, '\n'), 0o600))
				return
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err, "the recorded native state is missing; regenerate it at the base commit")
			require.Equal(t, string(want), string(encoded)+"\n",
				"complete native association state changed")
		})
	}
}

// requireNativeSamples derives every expected series and sample from the fixture
// and proves the store holds exactly those, with no synthetic metadata series.
func requireNativeSamples(t *testing.T, store *columnar.Storage, entries []ingestEntry) {
	t.Helper()
	all, err := store.SelectSeries(context.Background(), 0, math.MaxInt64, nil, 0)
	require.NoError(t, err)

	want := map[string][]int64{}
	wantValues := map[string][]float64{}
	for _, entry := range entries {
		for _, record := range entry.records {
			owner := columnar.ResourceOwner(record.attrs)
			for _, name := range []string{primaryGaugeName, primarySumName} {
				// The native conversion states the point attribute, the Resource's
				// job and instance compatibility labels and the Resource identity
				// reference. It promotes no descriptive attribute and adds no
				// synthetic series, so this is the complete expected label set.
				seriesLabels := labels.FromStrings(
					labels.MetricName, name,
					"series", record.labels.Get("series"),
					model.JobLabel, record.attrs["service.name"],
					model.InstanceLabel, record.attrs["service.instance.id"],
					columnar.OTELResourceLabel, owner.Ref(),
				)
				key := seriesLabels.String()
				want[key] = append(want[key], record.ts...)
				values := record.vs
				if name == primarySumName {
					values = sumValues(record.vs)
				}
				wantValues[key] = append(wantValues[key], values...)
			}
		}
	}
	require.Len(t, all, len(want), "every fixture series is stored exactly once")
	for _, series := range all {
		key := series.Labels.String()
		require.Equal(t, want[key], series.Timestamps, "timestamps of %s", key)
		require.Equal(t, wantValues[key], series.Values, "values of %s", key)
		for _, kind := range series.Kinds {
			require.Equal(t, columnar.SampleFloat, kind, "kind in %s", key)
		}
	}

	synthetic, err := store.SelectSeries(context.Background(), 0, math.MaxInt64,
		[][]*labels.Matcher{{labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "target_info")}}, 0)
	require.NoError(t, err)
	require.Empty(t, synthetic, "the native route must not invent metadata samples")
}

// nativeParityExpectedOwners returns the owners the fixture's inputs state: the
// Resource of every record, its schema, the one Scope the fixture uses, the
// ingestion source and every exact stored series. Each expected owner must have a
// history, and the owner pages must report exactly as many owners of each kind as
// are expected, so a lost or invented owner fails here.
func nativeParityExpectedOwners(t *testing.T, store *columnar.Storage, entries []ingestEntry) []columnar.OwnerKey {
	t.Helper()
	expected := map[columnar.OwnerKey]struct{}{}
	for _, entry := range entries {
		for _, record := range entry.records {
			owner := columnar.ResourceOwner(record.attrs)
			expected[owner] = struct{}{}
			expected[columnar.ResourceSchemaOwner(owner)] = struct{}{}
		}
	}
	expected[columnar.ScopeOwner("prometheus.primary.ingest", "1.0.0")] = struct{}{}
	expected[columnar.SourceOwner("otap")] = struct{}{}

	all, err := store.SelectSeries(context.Background(), 0, math.MaxInt64, nil, 0)
	require.NoError(t, err)
	for _, series := range all {
		expected[columnar.MetricOwner(series.Labels)] = struct{}{}
	}

	owners := make([]columnar.OwnerKey, 0, len(expected))
	perKind := map[columnar.MetadataKind]int{}
	for owner := range expected {
		owners = append(owners, owner)
		perKind[owner.Kind]++
	}
	slices.SortFunc(owners, func(a, b columnar.OwnerKey) int { return strings.Compare(a.String(), b.String()) })

	for _, kind := range columnar.KindNames {
		metadataKind, ok := nativeParityKind(kind)
		require.True(t, ok)
		page, err := store.QueryMetadata(context.Background(), columnar.MetadataQuery{
			Kind: metadataKind, Start: 0, End: math.MaxInt64,
		})
		require.NoError(t, err)
		require.Len(t, page.Owners, perKind[metadataKind],
			"the store's %s owner count must be exactly the count the fixture states", kind)
	}
	for _, owner := range owners {
		_, ok := store.MetadataHistory(owner)
		require.True(t, ok, "missing owner history for %s", owner)
	}
	return owners
}

// nativeParityKind resolves a kind name back to its kind.
func nativeParityKind(name string) (columnar.MetadataKind, bool) {
	for i, candidate := range columnar.KindNames {
		if candidate == name {
			return columnar.MetadataKind(i), true
		}
	}
	return 0, false
}

// captureNativeParityState records every public result of the corpus.
func captureNativeParityState(
	t *testing.T, store *columnar.Storage, engine promql.QueryEngine, owners []columnar.OwnerKey,
) nativeParityDump {
	t.Helper()
	dump := nativeParityDump{
		Pages:       map[string][]string{},
		Memberships: map[string][]string{},
		Info:        map[string][]string{},
	}

	all, err := store.SelectSeries(context.Background(), 0, math.MaxInt64, nil, 0)
	require.NoError(t, err)

	dump.Series = make([]nativeParitySeries, 0, len(all))
	var mint, maxt int64 = math.MaxInt64, math.MinInt64
	for _, series := range all {
		entry := nativeParitySeries{Labels: series.Labels.String()}
		require.Len(t, series.Histograms, len(series.Timestamps))
		for i, timestamp := range series.Timestamps {
			require.Nil(t, series.Histograms[i], "the fixture states no histogram sample")
			entry.Samples = append(entry.Samples, fmt.Sprintf("%d/%016x/%d/%d",
				timestamp, math.Float64bits(series.Values[i]), series.Kinds[i], series.StartTimestamps[i]))
		}
		if len(series.Timestamps) > 0 {
			mint = min(mint, series.Timestamps[0])
			maxt = max(maxt, series.Timestamps[len(series.Timestamps)-1])
		}
		dump.Series = append(dump.Series, entry)
	}
	require.NotEqual(t, math.MaxInt64, mint)
	slices.SortFunc(dump.Series, func(a, b nativeParitySeries) int { return strings.Compare(a.Labels, b.Labels) })
	seriesIndex := make(map[string]int, len(dump.Series))
	for i, series := range dump.Series {
		seriesIndex[series.Labels] = i
	}

	for _, owner := range owners {
		history, ok := store.MetadataHistory(owner)
		require.True(t, ok, "owner %s is missing", owner)
		record := nativeParityOwner{Owner: owner.String(), Truncated: history.Truncated}
		require.NotEmpty(t, history.Versions, "owner %s has no version", owner)
		for _, version := range history.Versions {
			record.Versions = append(record.Versions, nativeParityVersion{
				Content:       version.Content.Canonical(),
				EffectiveFrom: version.EffectiveFrom,
				ObservedFrom:  version.ObservedFrom,
				ObservedUntil: version.ObservedUntil,
			})
		}
		dump.Owners = append(dump.Owners, record)
	}

	for _, kind := range columnar.KindNames {
		metadataKind, ok := nativeParityKind(kind)
		require.True(t, ok)
		page, err := store.QueryMetadata(context.Background(), columnar.MetadataQuery{
			Kind: metadataKind, Start: 0, End: math.MaxInt64,
		})
		require.NoError(t, err)
		require.Empty(t, page.NextToken, "the parity corpus fits in one owner page")
		for _, owner := range page.Owners {
			rendered, err := json.Marshal(nativeParityPageOwner{
				Identifying:      owner.Identifying,
				HistoryTruncated: owner.HistoryTruncated,
				Versions:         nativeParityValueViews(owner.Versions),
			})
			require.NoError(t, err)
			dump.Pages[kind] = append(dump.Pages[kind], string(rendered))
		}
		slices.Sort(dump.Pages[kind])

		series, err := store.QueryMetadataSeries(context.Background(), columnar.MetadataSeriesQuery{
			Kind: metadataKind, Start: 0, End: math.MaxInt64,
		})
		require.NoError(t, err)
		for _, row := range series.Series {
			index, ok := seriesIndex[row.Labels.String()]
			require.True(t, ok, "membership for an unrecorded series %s", row.Labels)
			until := "open"
			if row.MembershipUntil != nil {
				until = strconv.FormatInt(*row.MembershipUntil, 10)
			}
			dump.Memberships[kind] = append(dump.Memberships[kind], fmt.Sprintf(
				"series[%d] owner=%s identifying=%s start=%d until=%s observed=%d versions=%s",
				index, row.Owner, nativeParityCanonical(row.OwnerIdentifying), row.MembershipStart, until,
				row.MembershipObservedTo, nativeParityVersionList(nativeParityValueViews(row.Versions))))
		}
	}

	// MetadataAtLabels before, at and after every boundary the corpus states.
	for _, series := range all {
		instants := []int64{mint - 1, mint, (mint + maxt) / 2, maxt + 1}
		for _, kind := range []string{"resource", "scope", "metric"} {
			metadataKind, ok := nativeParityKind(kind)
			require.True(t, ok)
			for _, at := range instants {
				content, found, truncated := store.MetadataAtLabels(series.Labels, metadataKind, at)
				dump.AtLabels = append(dump.AtLabels, fmt.Sprintf("series[%d] kind=%s at=%d found=%t truncated=%t content=%s",
					seriesIndex[series.Labels.String()], kind, at, found, truncated, content.Canonical()))
			}
		}
	}

	for _, query := range []struct{ name, expr string }{
		{"all", `info({__name__=~"primary_.*"})`},
		{"gauge", `info(primary_gauge)`},
		{"sum_selector", `info(primary_requests_total{series="3"})`},
	} {
		var reported []string
		for _, at := range []int64{mint, (mint + maxt) / 2, maxt} {
			for _, metric := range nativeParityInfoLabels(t, engine, store, query.expr, at) {
				reported = append(reported, fmt.Sprintf("%d %s", at, metric.String()))
			}
		}
		slices.Sort(reported)
		dump.Info[query.name] = reported
	}
	return dump
}

// nativeParityValueViews renders version views.
func nativeParityValueViews(views []columnar.MetadataValueView) []nativeParityVersion {
	out := make([]nativeParityVersion, 0, len(views))
	for _, view := range views {
		out = append(out, nativeParityVersion{
			Content:       columnar.Content{Attributes: view.Descriptive}.Canonical(),
			EffectiveFrom: view.EffectiveFrom,
			ObservedFrom:  view.ObservedFrom,
			ObservedUntil: view.ObservedUntil,
		})
	}
	return out
}

// nativeParityCanonical renders a string map deterministically.
func nativeParityCanonical(attributes map[string]string) string {
	return columnar.Content{Attributes: attributes}.Canonical()
}

// nativeParityVersionList renders versions as content@effective(observed range).
func nativeParityVersionList(versions []nativeParityVersion) string {
	rendered := make([]string, 0, len(versions))
	for _, version := range versions {
		rendered = append(rendered, fmt.Sprintf("%s@%d[%d,%d]",
			version.Content, version.EffectiveFrom, version.ObservedFrom, version.ObservedUntil))
	}
	return "[" + strings.Join(rendered, " ") + "]"
}

// nativeParityInfoLabels evaluates an info() expression at one instant.
func nativeParityInfoLabels(
	t *testing.T, engine promql.QueryEngine, store *columnar.Storage, expr string, at int64,
) []labels.Labels {
	t.Helper()
	query, err := engine.NewInstantQuery(context.Background(), store, nil, expr, time.UnixMilli(at))
	require.NoError(t, err)
	defer query.Close()
	result := query.Exec(context.Background())
	require.NoError(t, result.Err)
	vector, ok := result.Value.(promql.Vector)
	require.True(t, ok)
	out := make([]labels.Labels, 0, len(vector))
	for _, sample := range vector {
		out = append(out, sample.Metric.Copy())
	}
	return out
}

// nativeParityEngine is the PromQL engine the corpus' info() forms need.
func nativeParityEngine(t *testing.T) promql.QueryEngine {
	t.Helper()
	return promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
		MaxSamples: promqltest.DefaultMaxSamplesPerQuery, Timeout: time.Minute, LookbackDelta: 5 * time.Minute,
		LabelNamerConfig: &promql.LabelNamerConfig{
			UnderscoreLabelSanitization: true, PreserveMultipleUnderscores: true,
		},
		Parser: parser.NewParser(promqltest.TestParserOpts),
	})
}

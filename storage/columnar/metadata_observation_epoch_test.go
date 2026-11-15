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
	"fmt"
	"log/slog"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage/columnar"
	otlpprw "github.com/prometheus/prometheus/storage/remote/otlptranslator/prometheusremotewrite"
)

// Folding repeated observations of one unchanged association may only happen
// inside one ordered epoch. These tests cover the boundary: an assertion that can
// change the same owner's or series' temporal domain closes the epoch, so a later
// re-statement of the earlier content is emitted as a new ordered change point
// instead of moving an already emitted observation bound across it.
//
// TestNativeMetadataObservationOrderedEpochs drives the real native OTAP sink. The
// producer groups equal Resources, so the only way to interleave two equal-content
// epochs of one owner through it is to alternate the Scope schema URL within one
// Resource and then re-state the first one from a second Resource occurrence of the
// same identity, with PromoteScopeMetadata off so all occurrences remain one
// series. TestMetadataObservationResourceEpochs states the equivalent Resource
// content order directly, which the producer cannot emit for one owner because it
// sorts metric rows by Resource and merges occurrences with equal attributes.

const (
	observationOrderResourceSchemaA = "observation-order-resource-a"
	observationOrderResourceSchemaB = "observation-order-resource-b"
	observationOrderScopeSchemaA    = "observation-order-scope-a"
	observationOrderScopeSchemaB    = "observation-order-scope-b"
	observationOrderRoute           = "/ordered"
)

// observationOrderSettings keeps Scope metadata out of the series labels so all
// occurrences of the fixture state one series, and disables synthetic target_info.
func observationOrderSettings() otlpprw.Settings {
	settings := observationSettings()
	settings.PromoteScopeMetadata = false
	return settings
}

// observationOrderBatch returns the fixture's four points in input order: the first
// Scope-schema epoch, another one, and the first one again.
func observationOrderBatch() []observationPoint {
	return []observationPoint{
		{
			resource:       observationContentMap("pod-a", "host-1"),
			resourceSchema: observationOrderResourceSchemaA, scopeSchema: observationOrderScopeSchemaA,
			route: observationOrderRoute, timestamp: 1000,
		},
		{
			resource:       observationContentMap("pod-a", "host-1"),
			resourceSchema: observationOrderResourceSchemaA, scopeSchema: observationOrderScopeSchemaA,
			route: observationOrderRoute, timestamp: 2000,
		},
		{
			resource:       observationContentMap("pod-a", "host-1"),
			resourceSchema: observationOrderResourceSchemaA, scopeSchema: observationOrderScopeSchemaB,
			route: observationOrderRoute, timestamp: 3000,
		},
		{
			resource:       observationContentMap("pod-a", "host-1"),
			resourceSchema: observationOrderResourceSchemaB, scopeSchema: observationOrderScopeSchemaA,
			route: observationOrderRoute, timestamp: 4000,
		},
	}
}

// observationOrderSeriesLabels is the one series the fixture produces.
func observationOrderSeriesLabels(owner columnar.OwnerKey) labels.Labels {
	return labels.FromStrings(
		labels.MetricName, observationMetricName,
		columnar.OTELResourceLabel, owner.Ref(),
		"__type__", "gauge",
		"instance", "pod-a",
		"job", "shop/checkout",
		"route", observationOrderRoute,
	)
}

// observationLabelsEnrichment is the label set of ls with the fixture Resource
// content added, which is exactly what info() returns for a native series.
func observationLabelsEnrichment(ls labels.Labels, host string) map[string]string {
	out := map[string]string{}
	ls.Range(func(l labels.Label) { out[l.Name] = l.Value })
	maps.Copy(out, map[string]string{
		"deployment_environment_name": "prod",
		"host_name":                   host,
		"process_pid":                 "4242",
		"sandboxed":                   "true",
	})
	return out
}

// observationOrderEnrichment is the info() label set of the ordering fixture.
func observationOrderEnrichment(owner columnar.OwnerKey, host string) map[string]string {
	return observationLabelsEnrichment(observationOrderSeriesLabels(owner), host)
}

// observationSchemaContent is the owner content of a schema URL version.
func observationSchemaContent(schemaURL string) map[string]string {
	return map[string]string{"schema_url": schemaURL}
}

// requireObservationOrderState asserts the complete public state of the ordered
// native fixture: the Scope owner re-states its first content after a different
// one, the Resource owner is untouched, and every other owner keeps the bounds of
// its own observations.
func requireObservationOrderState(t *testing.T, s *columnar.Storage, engine promql.QueryEngine, phase string) {
	t.Helper()
	resource := observationOwner("pod-a")
	schema := columnar.ResourceSchemaOwner(resource)
	scope := columnar.ScopeOwner("observation-sdk", "v1")
	source := columnar.SourceOwner("otap")
	seriesLabels := observationOrderSeriesLabels(resource)

	t.Run("scope_history", func(t *testing.T) {
		history, ok := s.MetadataHistory(scope)
		require.True(t, ok, phase)
		require.False(t, history.Truncated)
		require.Equal(t, []columnar.Version{
			{
				Content:       columnar.Content{Attributes: observationSchemaContent(observationOrderScopeSchemaA)},
				EffectiveFrom: 1000, ObservedFrom: 1000, ObservedUntil: 2000,
			},
			{
				Content:       columnar.Content{Attributes: observationSchemaContent(observationOrderScopeSchemaB)},
				EffectiveFrom: 3000, ObservedFrom: 3000, ObservedUntil: 3000,
			},
			{
				Content:       columnar.Content{Attributes: observationSchemaContent(observationOrderScopeSchemaA)},
				EffectiveFrom: 4000, ObservedFrom: 4000, ObservedUntil: 4000,
			},
		}, history.Versions)
	})

	t.Run("resource_history", func(t *testing.T) {
		history, ok := s.MetadataHistory(resource)
		require.True(t, ok, phase)
		require.Equal(t, []columnar.Version{{
			Content:       columnar.Content{Attributes: observationContent("host-1")},
			EffectiveFrom: 1000, ObservedFrom: 1000, ObservedUntil: 4000,
		}}, history.Versions)
	})

	t.Run("schema_history", func(t *testing.T) {
		history, ok := s.MetadataHistory(schema)
		require.True(t, ok, phase)
		require.Equal(t, []columnar.Version{
			{
				Content:       columnar.Content{Attributes: observationSchemaContent(observationOrderResourceSchemaA)},
				EffectiveFrom: 1000, ObservedFrom: 1000, ObservedUntil: 3000,
			},
			{
				Content:       columnar.Content{Attributes: observationSchemaContent(observationOrderResourceSchemaB)},
				EffectiveFrom: 4000, ObservedFrom: 4000, ObservedUntil: 4000,
			},
		}, history.Versions)
	})

	t.Run("source_and_metric_history", func(t *testing.T) {
		// The fixture states no Scrape owner; the native sink states one Source and
		// one per-series Metric owner.
		sourceHistory, ok := s.MetadataHistory(source)
		require.True(t, ok, phase)
		require.Equal(t, []columnar.Version{{
			Content:       sourceHistory.Versions[0].Content.Clone(),
			EffectiveFrom: 1000, ObservedFrom: 1000, ObservedUntil: 4000,
		}}, sourceHistory.Versions)

		metricHistory, ok := s.MetadataHistory(columnar.MetricOwner(seriesLabels))
		require.True(t, ok, phase)
		require.Len(t, metricHistory.Versions, 1)
		require.Equal(t, int64(1000), metricHistory.Versions[0].EffectiveFrom)
		require.Equal(t, int64(4000), metricHistory.Versions[0].ObservedUntil)
	})

	t.Run("scope_query", func(t *testing.T) {
		owners, err := s.QueryMetadata(t.Context(), columnar.MetadataQuery{
			Kind: columnar.MetadataScope, Start: 0, End: observationEnd,
		})
		require.NoError(t, err)
		require.Len(t, owners.Owners, 1)
		require.Equal(t, scope.Ref(), owners.Owners[0].ID)
		require.Equal(t, []columnar.MetadataValueView{
			{
				Descriptive: observationSchemaContent(observationOrderScopeSchemaA), EffectiveFrom: 1000,
				ValidUntil: int64Ptr(3000), ObservedFrom: 1000, ObservedUntil: 2000,
			},
			{
				Descriptive: observationSchemaContent(observationOrderScopeSchemaB), EffectiveFrom: 3000,
				ValidUntil: int64Ptr(4000), ObservedFrom: 3000, ObservedUntil: 3000,
			},
			{
				Descriptive: observationSchemaContent(observationOrderScopeSchemaA), EffectiveFrom: 4000,
				ObservedFrom: 4000, ObservedUntil: 4000,
			},
		}, owners.Owners[0].Versions)
	})

	t.Run("scope_membership", func(t *testing.T) {
		page, err := s.QueryMetadataSeries(t.Context(), columnar.MetadataSeriesQuery{
			Kind: columnar.MetadataScope, Start: 0, End: observationEnd,
		})
		require.NoError(t, err)
		require.Len(t, page.Series, 1, "one series keeps one Scope membership")
		membership := page.Series[0]
		require.Equal(t, seriesLabels, membership.Labels)
		require.Equal(t, scope.Ref(), membership.Owner)
		require.Equal(t, int64(1000), membership.MembershipStart)
		require.Nil(t, membership.MembershipUntil)
		require.Equal(t, int64(4000), membership.MembershipObservedTo)
		require.Len(t, membership.Versions, 3)

		resourcePage, err := s.QueryMetadataSeries(t.Context(), columnar.MetadataSeriesQuery{
			Kind: columnar.MetadataResource, Start: 0, End: observationEnd,
		})
		require.NoError(t, err)
		require.Len(t, resourcePage.Series, 1, "one series keeps one Resource membership")
		require.Equal(t, int64(1000), resourcePage.Series[0].MembershipStart)
		require.Equal(t, int64(4000), resourcePage.Series[0].MembershipObservedTo)
		require.Len(t, resourcePage.Series[0].Versions, 1)
	})

	t.Run("scope_content_per_band", func(t *testing.T) {
		for _, tc := range []struct {
			at        int64
			content   string
			truncated bool
		}{
			{at: 999},
			{at: 1000, content: observationOrderScopeSchemaA},
			{at: 2999, content: observationOrderScopeSchemaA},
			{at: 3000, content: observationOrderScopeSchemaB},
			{at: 3999, content: observationOrderScopeSchemaB},
			{at: 4000, content: observationOrderScopeSchemaA},
			{at: 5000, content: observationOrderScopeSchemaA},
		} {
			content, found, truncated := s.MetadataAtLabels(seriesLabels, columnar.MetadataScope, tc.at)
			require.False(t, truncated)
			require.Equal(t, tc.content != "", found, "at %d", tc.at)
			if found {
				require.Equal(t, tc.content, content.Attributes["schema_url"], "at %d", tc.at)
			}
		}
	})

	t.Run("samples", func(t *testing.T) {
		series, err := s.SelectSeries(t.Context(), 0, observationEnd, nil, 0)
		require.NoError(t, err)
		require.Len(t, series, 1, "one series, no fabricated metadata series")
		require.Equal(t, seriesLabels, series[0].Labels)
		require.Equal(t, []int64{1000, 2000, 3000, 4000}, series[0].Timestamps)
		require.Equal(t, []float64{1000, 2000, 3000, 4000}, series[0].Values)
	})

	t.Run("info_enrichment", func(t *testing.T) {
		// The Resource content is one unchanged epoch here, so info() adds the same
		// complete enrichment in every band.
		enriched := observationOrderEnrichment(resource, "host-1")
		for _, at := range []int64{1000, 1500, 3500, 4500} {
			result := queryInfoLabels(t, engine, s, fmt.Sprintf("info(%s)", observationMetricName), at)
			require.Len(t, result, 1, "at %d", at)
			require.Equal(t, enriched, result[0].Map(), "at %d", at)
		}
		selected := queryInfoLabels(t, engine, s,
			fmt.Sprintf("info(%s, {host_name=%q})", observationMetricName, "host-1"), 3500)
		require.Len(t, selected, 1)
		require.NotEmpty(t, selected[0].Get("host_name"))
	})
}

// TestNativeMetadataObservationOrderedEpochs asserts the ordered-epoch result
// through the real native sink, in the head, after WAL recovery and after block
// recovery. The decode order of the fixture is asserted by
// TestNativeMetadataObservationDecodeOrder in the package tests.
func TestNativeMetadataObservationOrderedEpochs(t *testing.T) {
	engine := observationEngine(t)
	ingest := func(t *testing.T, s *columnar.Storage) {
		t.Helper()
		sink := columnar.NewOTAPSink(s, slog.New(slog.DiscardHandler), observationOrderSettings)
		require.NoError(t, sink.ConsumeRecords(t.Context(), recordsFor(t, observationMetrics(observationOrderBatch()...))))
	}

	t.Run("in_memory_head", func(t *testing.T) {
		s := newObservationStorage(t, t.TempDir(), true)
		ingest(t, s)
		requireObservationOrderState(t, s, engine, "in_memory_head")
	})

	t.Run("wal_recovery", func(t *testing.T) {
		dir := t.TempDir()
		s := newObservationStorage(t, dir, false)
		ingest(t, s)
		requireObservationOrderState(t, s, engine, "wal_recovery_live")
		require.NoError(t, s.Close())
		requireObservationOrderState(t, reopenObservationStorage(t, dir), engine, "wal_recovery_reopened")
	})

	t.Run("published_metadata", func(t *testing.T) {
		dir := t.TempDir()
		s := newObservationStorage(t, dir, false)
		ingest(t, s)
		require.NoError(t, s.Flush())
		requireObservationOrderState(t, s, engine, "after_flush")
		require.NoError(t, s.Close())
		requireObservationOrderState(t, reopenObservationStorage(t, dir), engine, "after_reopen")
	})
}

// TestMetadataObservationResourceEpochs states the required Resource-owner order
// directly: content A observed twice, then content B, then content A again. It must
// produce three versions in input order - not one extended first epoch - one
// membership observed through the last assertion, exactly the submitted samples, and
// info() enrichment with A, B, then A in their own validity bands.
func TestMetadataObservationResourceEpochs(t *testing.T) {
	engine := observationEngine(t)
	resource := observationOwner("pod-a")
	seriesLabels := observationOrderSeriesLabels(resource)
	contentA := columnar.Content{Attributes: observationContent("host-1")}
	contentB := columnar.Content{Attributes: observationContent("host-2")}

	requireEpochState := func(t *testing.T, s *columnar.Storage, phase string) {
		t.Helper()
		history, ok := s.MetadataHistory(resource)
		require.True(t, ok, phase)
		require.False(t, history.Truncated)
		require.Equal(t, []columnar.Version{
			{Content: contentA, EffectiveFrom: 1000, ObservedFrom: 1000, ObservedUntil: 2000},
			{Content: contentB, EffectiveFrom: 3000, ObservedFrom: 3000, ObservedUntil: 3000},
			{Content: contentA, EffectiveFrom: 4000, ObservedFrom: 4000, ObservedUntil: 4000},
		}, history.Versions)

		owners, err := s.QueryMetadata(t.Context(), columnar.MetadataQuery{
			Kind: columnar.MetadataResource, Start: 0, End: observationEnd,
		})
		require.NoError(t, err)
		require.Len(t, owners.Owners, 1)
		require.Equal(t, []columnar.MetadataValueView{
			{
				Descriptive: observationContent("host-1"), EffectiveFrom: 1000,
				ValidUntil: int64Ptr(3000), ObservedFrom: 1000, ObservedUntil: 2000,
			},
			{
				Descriptive: observationContent("host-2"), EffectiveFrom: 3000,
				ValidUntil: int64Ptr(4000), ObservedFrom: 3000, ObservedUntil: 3000,
			},
			{
				Descriptive: observationContent("host-1"), EffectiveFrom: 4000,
				ObservedFrom: 4000, ObservedUntil: 4000,
			},
		}, owners.Owners[0].Versions)

		memberships, err := s.QueryMetadataSeries(t.Context(), columnar.MetadataSeriesQuery{
			Kind: columnar.MetadataResource, Start: 0, End: observationEnd,
		})
		require.NoError(t, err)
		require.Len(t, memberships.Series, 1, "one membership, not one per content epoch")
		require.Equal(t, int64(1000), memberships.Series[0].MembershipStart)
		require.Nil(t, memberships.Series[0].MembershipUntil)
		require.Equal(t, int64(4000), memberships.Series[0].MembershipObservedTo)
		require.Len(t, memberships.Series[0].Versions, 3)

		for _, tc := range []struct {
			at   int64
			host string
		}{
			{at: 999},
			{at: 1000, host: "host-1"},
			{at: 2999, host: "host-1"},
			{at: 3000, host: "host-2"},
			{at: 3999, host: "host-2"},
			{at: 4000, host: "host-1"},
			{at: 5000, host: "host-1"},
		} {
			content, found, truncated := s.MetadataAtLabels(seriesLabels, columnar.MetadataResource, tc.at)
			require.False(t, truncated)
			require.Equal(t, tc.host != "", found, "at %d", tc.at)
			if found {
				require.Equal(t, tc.host, content.Attributes["host.name"], "at %d", tc.at)
			}
		}

		series, err := s.SelectSeries(t.Context(), 0, observationEnd, nil, 0)
		require.NoError(t, err)
		require.Len(t, series, 1)
		require.Equal(t, []int64{1000, 2000, 3000, 4000}, series[0].Timestamps)
		require.Equal(t, []float64{1000, 2000, 3000, 4000}, series[0].Values)

		for _, tc := range []struct {
			at   int64
			host string
		}{
			{at: 1500, host: "host-1"},
			{at: 3500, host: "host-2"},
			{at: 4500, host: "host-1"},
		} {
			result := queryInfoLabels(t, engine, s, fmt.Sprintf("info(%s)", observationMetricName), tc.at)
			require.Len(t, result, 1, "at %d", tc.at)
			enriched := observationLabelsEnrichment(seriesLabels, tc.host)
			require.Equal(t, enriched, result[0].Map(), "info() at %d must use that band's content", tc.at)
		}
	}

	appendBatch := func(t *testing.T, s *columnar.Storage) {
		t.Helper()
		updates := make([]columnar.AssociationUpdate, 0, 4)
		for _, update := range []columnar.Update{
			{Owner: resource, Content: contentA, EffectiveFrom: 1000},
			{Owner: resource, Content: contentA, EffectiveFrom: 2000},
			{Owner: resource, Content: contentB, EffectiveFrom: 3000},
			{Owner: resource, Content: contentA, EffectiveFrom: 4000},
		} {
			updates = append(updates, columnar.AssociationUpdate{Labels: seriesLabels, Update: update})
		}
		require.NoError(t, s.AppendBatch(t.Context(), columnar.Batch{
			Series: []columnar.BatchSeries{{
				Labels: seriesLabels, Timestamps: []int64{1000, 2000, 3000, 4000},
				Kinds:  []columnar.SampleKind{columnar.SampleFloat, columnar.SampleFloat, columnar.SampleFloat, columnar.SampleFloat},
				Values: []float64{1000, 2000, 3000, 4000},
			}},
			Updates: updates,
		}))
	}

	t.Run("in_memory_head", func(t *testing.T) {
		s := newObservationStorage(t, t.TempDir(), true)
		appendBatch(t, s)
		requireEpochState(t, s, "in_memory_head")
	})

	t.Run("wal_recovery", func(t *testing.T) {
		dir := t.TempDir()
		s := newObservationStorage(t, dir, false)
		appendBatch(t, s)
		requireEpochState(t, s, "wal_recovery_live")
		require.NoError(t, s.Close())
		requireEpochState(t, reopenObservationStorage(t, dir), "wal_recovery_reopened")
	})

	t.Run("published_metadata", func(t *testing.T) {
		dir := t.TempDir()
		s := newObservationStorage(t, dir, false)
		appendBatch(t, s)
		require.NoError(t, s.Flush())
		requireEpochState(t, s, "after_flush")
		require.NoError(t, s.Close())
		requireEpochState(t, reopenObservationStorage(t, dir), "after_reopen")
	})
}

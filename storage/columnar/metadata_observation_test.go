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
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
	otlpprw "github.com/prometheus/prometheus/storage/remote/otlptranslator/prometheusremotewrite"
)

// This file is the regression gate for metadata observation bounds. Validity is
// defined by effective change points; observation bounds record when a context
// was actually seen. They are separate facts, so an unchanged context keeps one
// version and one membership whose observation bounds advance with every
// observation, in the head, through WAL replay and through block recovery.
//
// The main fixture is one unchanged Resource/Scope/metric context ingested
// through the real native OTAP sink in successive batches. Synthetic target_info
// is disabled, so only the native Resource metadata is observed. Every
// expectation is read back through exported APIs: MetadataHistory,
// QueryMetadata, QueryMetadataSeries, MetadataAtLabels, SelectSeries and PromQL
// info().

const (
	// observationMetric is the fixture metric name. The translator emits
	// observationMetricName for it because AddMetricSuffixes is enabled.
	observationMetric     = "native_observation_metric"
	observationMetricName = "native_observation_metric_ratio"
	observationResource   = "observation-resource-v1"
	observationScope      = "observation-scope-v1"
	observationRoute      = "/observed"
	// observationEnd covers every submitted sample of the fixture.
	observationEnd = 4000
)

// observationIdentity is the identifying Resource identity of the fixture. It is
// the whole owner key and never enters descriptive content.
func observationIdentity(instance string) map[string]string {
	return map[string]string{
		"service.name": "checkout", "service.namespace": "shop", "service.instance.id": instance,
	}
}

// observationContent is the descriptive Resource content of the fixture. Every
// attribute becomes an info() label, so enrichment is observable.
func observationContent(host string) map[string]string {
	return map[string]string{
		"deployment.environment.name": "prod",
		"host.name":                   host,
		"process.pid":                 "4242",
		"sandboxed":                   "true",
	}
}

// observationContentMap is the complete attribute set of one Resource:
// identifying and descriptive attributes together.
func observationContentMap(instance, host string) map[string]string {
	attributes := observationIdentity(instance)
	maps.Copy(attributes, observationContent(host))
	return attributes
}

func observationOwner(instance string) columnar.OwnerKey {
	return columnar.ResourceOwner(observationIdentity(instance))
}

// observationSeriesLabels is the series the native route produces for the
// fixture: translated identifying attributes, promoted Scope metadata and the
// one data point attribute, with the Resource reference carrying the owner.
func observationSeriesLabels(owner columnar.OwnerKey) labels.Labels {
	return labels.FromStrings(
		labels.MetricName, observationMetricName,
		columnar.OTELResourceLabel, owner.Ref(),
		"__type__", "gauge",
		"instance", "pod-a",
		"job", "shop/checkout",
		"otel_scope_name", "observation-sdk",
		"otel_scope_schema_url", observationScope,
		"otel_scope_version", "v1",
		"route", observationRoute,
	)
}

// observationPoint is one number data point of the fixture. resource carries the
// complete attribute set, so a point can state changed descriptive content for
// the same identifying identity. resourceSchema and scopeSchema select the Resource
// and Scope occurrence the point belongs to; empty means the fixture defaults.
type observationPoint struct {
	resource       map[string]string
	resourceSchema string
	scopeSchema    string
	route          string
	timestamp      int64
}

// observationResourceSchema and observationScopeSchema resolve a point's schema
// URLs, defaulting to the single-occurrence fixture.
func (p observationPoint) resourceURL() string {
	if p.resourceSchema != "" {
		return p.resourceSchema
	}
	return observationResource
}

func (p observationPoint) scopeURL() string {
	if p.scopeSchema != "" {
		return p.scopeSchema
	}
	return observationScope
}

// observationBatch returns the points of the unchanged context at timestamps.
func observationBatch(timestamps ...int64) []observationPoint {
	points := make([]observationPoint, 0, len(timestamps))
	for _, timestamp := range timestamps {
		points = append(points, observationPoint{
			resource: observationContentMap("pod-a", "host-1"), route: observationRoute, timestamp: timestamp,
		})
	}
	return points
}

// observationMetrics converts points into one OTAP batch. Points are grouped
// into Resource scopes in first-appearance order, and their row order inside a
// Resource is the order given, which is the input order the sink must not
// reorder.
func observationMetrics(points ...observationPoint) pmetric.Metrics {
	md := pmetric.NewMetrics()
	byResource := map[string]int{}
	for _, point := range points {
		key := fmt.Sprint(point.resource) + "\x00" + point.resourceURL()
		if _, seen := byResource[key]; seen {
			continue
		}
		byResource[key] = len(byResource)
		rm := md.ResourceMetrics().AppendEmpty()
		rm.SetSchemaUrl(point.resourceURL())
		for name, value := range point.resource {
			rm.Resource().Attributes().PutStr(name, value)
		}
	}
	// Scopes are separate occurrences of their Resource, in first-appearance order.
	byScope := map[string]int{}
	for _, point := range points {
		resource := byResource[fmt.Sprint(point.resource)+"\x00"+point.resourceURL()]
		key := strconv.Itoa(resource) + "\x00" + point.scopeURL()
		if _, seen := byScope[key]; seen {
			continue
		}
		sm := md.ResourceMetrics().At(resource).ScopeMetrics().AppendEmpty()
		sm.SetSchemaUrl(point.scopeURL())
		sm.Scope().SetName("observation-sdk")
		sm.Scope().SetVersion("v1")
		metric := sm.Metrics().AppendEmpty()
		metric.SetName(observationMetric)
		metric.SetDescription("Observation bounds")
		metric.SetUnit("1")
		metric.SetEmptyGauge()
		byScope[key] = md.ResourceMetrics().At(resource).ScopeMetrics().Len() - 1
	}
	for _, point := range points {
		resource := byResource[fmt.Sprint(point.resource)+"\x00"+point.resourceURL()]
		scope := byScope[strconv.Itoa(resource)+"\x00"+point.scopeURL()]
		gauge := md.ResourceMetrics().At(resource).ScopeMetrics().At(scope).Metrics().At(0).Gauge()
		dp := gauge.DataPoints().AppendEmpty()
		dp.SetTimestamp(pcommon.Timestamp(point.timestamp * 1_000_000))
		dp.SetDoubleValue(float64(point.timestamp))
		dp.Attributes().PutStr("route", point.route)
	}
	return md
}

// observationSettings disables synthetic target_info so the native Resource
// metadata path is exercised on its own, and keeps descriptive Resource
// attributes out of labels so they remain native content.
func observationSettings() otlpprw.Settings {
	return otlpprw.Settings{
		AddMetricSuffixes:                    true,
		AllowUTF8:                            false,
		DisableTargetInfo:                    true,
		KeepIdentifyingResourceAttributes:    true,
		PromoteScopeMetadata:                 true,
		AllowDeltaTemporality:                true,
		LookbackDelta:                        5 * time.Minute,
		EnableTypeAndUnitLabels:              true,
		LabelNameUnderscoreSanitization:      true,
		LabelNamePreserveMultipleUnderscores: true,
	}
}

func observationEngine(t *testing.T) promql.QueryEngine {
	t.Helper()
	return promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
		MaxSamples: promqltest.DefaultMaxSamplesPerQuery, Timeout: time.Minute, LookbackDelta: 5 * time.Minute,
		LabelNamerConfig: &promql.LabelNamerConfig{UnderscoreLabelSanitization: true, PreserveMultipleUnderscores: true},
		Parser:           parser.NewParser(promqltest.TestParserOpts),
	})
}

// observationOptions is the storage configuration of the fixture: a flush
// threshold it cannot reach by itself, so a block exists only when a test asks
// for one.
var observationOptions = columnar.StorageOptions{FlushSamples: 1 << 20}

// newObservationStorage opens a store of the fixture and returns its directory.
func newObservationStorage(t *testing.T, dir string, disableWAL bool) *columnar.Storage {
	t.Helper()
	opts := observationOptions
	opts.DisableWAL = disableWAL
	s, err := columnar.NewStorage(dir, opts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

// reopenObservationStorage reopens the fixture directory, which recovers
// published blocks and then replays the WAL.
func reopenObservationStorage(t *testing.T, dir string) *columnar.Storage {
	t.Helper()
	reopened, err := columnar.NewStorage(dir, observationOptions)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	return reopened
}

// consumeObservation ingests one batch through the real native OTAP sink.
func consumeObservation(t *testing.T, s *columnar.Storage, points ...observationPoint) {
	t.Helper()
	require.NoError(t, consumeObservationErr(t, s, points...))
}

func consumeObservationErr(t *testing.T, s *columnar.Storage, points ...observationPoint) error {
	t.Helper()
	sink := columnar.NewOTAPSink(s, slog.New(slog.DiscardHandler), observationSettings)
	return sink.ConsumeRecords(t.Context(), recordsFor(t, observationMetrics(points...)))
}

// walTrace reports the WAL files and their sizes, so a rejected commit can be
// shown to leave no trace at all.
func walTrace(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "wal"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		require.NoError(t, err)
		out = append(out, fmt.Sprintf("%s:%d", entry.Name(), info.Size()))
	}
	return out
}

// observedSeries is one selected series, captured from a public query.
type observedSeries struct {
	Labels     string
	Timestamps []int64
	Values     []float64
}

// observedAt is one MetadataAtLabels answer.
type observedAt struct {
	At        int64
	Canonical string
	Found     bool
	Truncated bool
}

// metadataObservation is the complete public observation of one phase. Comparing
// two of them compares every documented result structure, not a timestamp
// multiset.
type metadataObservation struct {
	Phase        string
	Blocks       int
	HistoryFound bool
	History      columnar.OwnerHistory
	Owners       columnar.MetadataOwnerPage
	Memberships  columnar.MetadataSeriesPage
	Series       []observedSeries
	At           []observedAt
	Info         map[string]map[string]string
}

// comparable drops the two fields that legitimately differ between observation
// points of the same logical state: the phase name and the block count.
func (o metadataObservation) comparable() metadataObservation {
	o.Phase = ""
	o.Blocks = 0
	return o
}

// requireObservation compares every public result of one observation, one aspect
// at a time, so a failure names the structure that disagreed.
func requireObservation(t *testing.T, want, got metadataObservation) {
	t.Helper()
	require.Equal(t, want.HistoryFound, got.HistoryFound, "owner history presence")
	require.Equal(t, want.History, got.History, "owner history")
	require.Equal(t, want.Owners, got.Owners, "QueryMetadata owner page")
	require.Equal(t, want.Memberships, got.Memberships, "QueryMetadataSeries membership page")
	require.Equal(t, want.Series, got.Series, "selected samples")
	require.Equal(t, want.At, got.At, "MetadataAtLabels over validity boundaries")
	require.Equal(t, want.Info, got.Info, "info() enrichment")
}

// observationInfoQueries reads the enriched labels at the start of the first
// version, between the submitted samples, and after the last one. metricName is
// the name the translator produced, so the queries address the ingested series.
func observationInfoQueries(metricName string) []struct {
	expr string
	at   int64
} {
	return []struct {
		expr string
		at   int64
	}{
		{expr: fmt.Sprintf("info(%s)", metricName), at: 1000},
		{expr: fmt.Sprintf("info(%s)", metricName), at: 1500},
		{expr: fmt.Sprintf("info(%s)", metricName), at: 3000},
		{expr: fmt.Sprintf("info(%s, {host_name=%q})", metricName, "host-1"), at: 3000},
	}
}

// observationInfoNames names observationInfoQueries entries for comparison.
var observationInfoNames = []string{"info_at_first_version", "info_at_1500", "info_at_3000", "info_selector_at_3000"}

// observationAtInstants resolves the content either side of every boundary of
// the fixture, so lookahead or an off-by-one in the inclusive start shows up.
var observationAtInstants = []int64{0, 999, 1000, 1999, 2000, 3999}

// observeMetadata captures every public metadata result of the fixture.
func observeMetadata(t *testing.T, s *columnar.Storage, engine promql.QueryEngine, owner columnar.OwnerKey, phase string) metadataObservation {
	t.Helper()
	out := metadataObservation{Phase: phase, Blocks: s.NumBlocks(), Info: map[string]map[string]string{}}

	out.History, out.HistoryFound = s.MetadataHistory(owner)

	owners, err := s.QueryMetadata(t.Context(), columnar.MetadataQuery{
		Kind: columnar.MetadataResource, Start: 0, End: observationEnd,
	})
	require.NoError(t, err)
	out.Owners = owners

	memberships, err := s.QueryMetadataSeries(t.Context(), columnar.MetadataSeriesQuery{
		Kind: columnar.MetadataResource, Start: 0, End: observationEnd,
	})
	require.NoError(t, err)
	out.Memberships = memberships

	series, err := s.SelectSeries(t.Context(), 0, observationEnd, nil, 0)
	require.NoError(t, err)
	for _, item := range series {
		out.Series = append(out.Series, observedSeries{
			Labels: item.Labels.String(), Timestamps: item.Timestamps, Values: item.Values,
		})
	}
	slices.SortFunc(out.Series, func(a, b observedSeries) int { return strings.Compare(a.Labels, b.Labels) })

	seriesLabels := observationSeriesLabels(owner)
	for _, at := range observationAtInstants {
		content, found, truncated := s.MetadataAtLabels(seriesLabels, columnar.MetadataResource, at)
		answer := observedAt{At: at, Found: found, Truncated: truncated}
		if found {
			answer.Canonical = content.Canonical()
		}
		out.At = append(out.At, answer)
	}

	for i, query := range observationInfoQueries(observationMetricName) {
		enriched := queryInfoLabels(t, engine, s, query.expr, query.at)
		if len(enriched) == 0 {
			out.Info[observationInfoNames[i]] = map[string]string{}
			continue
		}
		out.Info[observationInfoNames[i]] = enriched[0].Map()
	}
	return out
}

// int64Ptr returns the pointer form a MetadataValueView uses for a successor
// change point.
func int64Ptr(v int64) *int64 { return &v }

// observationValues is the fixture's value column: the value echoes the
// timestamp, so a misplaced sample changes both columns.
func observationValues(timestamps []int64) []float64 {
	values := make([]float64, len(timestamps))
	for i, timestamp := range timestamps {
		values[i] = float64(timestamp)
	}
	return values
}

// observationEnrichment is the label set a Resource owner contributes to info(),
// flattened the way PromQL label names are.
func observationEnrichment(owner columnar.OwnerKey, extra map[string]string) map[string]string {
	out := map[string]string{}
	observationSeriesLabels(owner).Range(func(l labels.Label) { out[l.Name] = l.Value })
	maps.Copy(out, extra)
	return out
}

// expectedUnchangedObservation is the complete public state of the unchanged
// fixture at one phase. Every field is spelled out, so a difference is a
// semantic difference rather than a definitional one.
func expectedUnchangedObservation(owner columnar.OwnerKey, observedUntil int64, timestamps []int64) metadataObservation {
	content := observationContent("host-1")
	version := columnar.MetadataValueView{
		Descriptive: content, EffectiveFrom: 1000, ObservedFrom: 1000, ObservedUntil: observedUntil,
	}
	seriesLabels := observationSeriesLabels(owner)
	enriched := observationEnrichment(owner, map[string]string{
		"deployment_environment_name": content["deployment.environment.name"],
		"host_name":                   content["host.name"],
		"process_pid":                 content["process.pid"],
		"sandboxed":                   content["sandboxed"],
	})
	selected := observationEnrichment(owner, map[string]string{"host_name": content["host.name"]})

	out := metadataObservation{
		HistoryFound: true,
		History: columnar.OwnerHistory{
			Owner: owner,
			Versions: []columnar.Version{{
				Content:       columnar.Content{Attributes: content},
				EffectiveFrom: 1000,
				ObservedFrom:  1000,
				ObservedUntil: observedUntil,
			}},
		},
		Owners: columnar.MetadataOwnerPage{Owners: []columnar.MetadataOwnerView{{
			ID: owner.Ref(), Kind: "resource", Identifying: observationIdentity("pod-a"),
			Versions: []columnar.MetadataValueView{version},
		}}},
		Memberships: columnar.MetadataSeriesPage{Series: []columnar.MetadataSeriesResult{{
			Labels: seriesLabels, Owner: owner.Ref(), Kind: "resource",
			OwnerIdentifying:     observationIdentity("pod-a"),
			MembershipStart:      1000,
			MembershipObservedTo: observedUntil,
			Versions:             []columnar.MetadataValueView{version},
		}}},
		Series: []observedSeries{{
			Labels: seriesLabels.String(), Timestamps: timestamps, Values: observationValues(timestamps),
		}},
		Info: map[string]map[string]string{
			"info_at_first_version": enriched,
			"info_at_1500":          enriched,
			"info_at_3000":          enriched,
			"info_selector_at_3000": selected,
		},
	}
	for _, at := range observationAtInstants {
		answer := observedAt{At: at}
		if at >= 1000 {
			answer.Canonical = (columnar.Content{Attributes: content}).Canonical()
			answer.Found = true
		}
		out.At = append(out.At, answer)
	}
	return out
}

// TestNativeMetadataObservationBounds asserts the observation bounds of an
// unchanged native context in the head, after WAL recovery and after block
// recovery. Each state is ingested independently, and the recovered states must
// reproduce the live head exactly: a bound that only exists before recovery is
// not an observation history.
func TestNativeMetadataObservationBounds(t *testing.T) {
	owner := observationOwner("pod-a")
	engine := observationEngine(t)

	// Two observations in the first batch and one in the second. The context never
	// changes, so both batches describe one version and one membership.
	batches := [][]observationPoint{observationBatch(1000, 2000), observationBatch(3000)}
	phases := []struct {
		name          string
		observedUntil int64
		timestamps    []int64
	}{
		{name: "after_batch_1", observedUntil: 2000, timestamps: []int64{1000, 2000}},
		{name: "after_batch_2", observedUntil: 3000, timestamps: []int64{1000, 2000, 3000}},
	}

	// ingest drives one independent storage through the batches and returns the
	// observation after each of them, plus the observation after an explicit flush.
	ingest := func(t *testing.T, s *columnar.Storage, flush bool) []metadataObservation {
		t.Helper()
		out := make([]metadataObservation, 0, len(batches)+1)
		for i, batch := range batches {
			consumeObservation(t, s, batch...)
			out = append(out, observeMetadata(t, s, engine, owner, phases[i].name))
		}
		if flush {
			require.NoError(t, s.Flush())
			out = append(out, observeMetadata(t, s, engine, owner, "after_flush"))
		}
		return out
	}

	t.Run("in_memory_head", func(t *testing.T) {
		s := newObservationStorage(t, t.TempDir(), true)
		head := ingest(t, s, false)
		for i, observed := range head {
			requireObservation(t, expectedUnchangedObservation(owner, phases[i].observedUntil, phases[i].timestamps), observed.comparable())
			require.Zero(t, observed.Blocks, "an unflushed head publishes no block")
		}
	})

	t.Run("wal_recovery", func(t *testing.T) {
		dir := t.TempDir()
		s := newObservationStorage(t, dir, false)
		live := ingest(t, s, false)
		for i, observed := range live {
			requireObservation(t, expectedUnchangedObservation(owner, phases[i].observedUntil, phases[i].timestamps), observed.comparable())
		}
		require.NoError(t, s.Close())

		// Recovery replays the whole log at once, so the state it must reproduce is
		// the live state after the last batch.
		replayed := reopenObservationStorage(t, dir)
		requireObservation(t, live[len(live)-1].comparable(),
			observeMetadata(t, replayed, engine, owner, "after_replay").comparable())
	})

	t.Run("published_metadata", func(t *testing.T) {
		dir := t.TempDir()
		s := newObservationStorage(t, dir, false)
		published := ingest(t, s, true)
		for i := range phases {
			requireObservation(t, expectedUnchangedObservation(owner, phases[i].observedUntil, phases[i].timestamps), published[i].comparable())
		}
		requireObservation(t, published[1].comparable(), published[2].comparable())
		require.Equal(t, 1, published[2].Blocks, "the flushed fixture publishes one block")
		require.NoError(t, s.Close())

		reopened := reopenObservationStorage(t, dir)
		t.Log("reopened published metadata")
		requireObservation(t,
			expectedUnchangedObservation(owner, 3000, []int64{1000, 2000, 3000}),
			observeMetadata(t, reopened, engine, owner, "after_reopen").comparable())
	})
}

// TestNativeMetadataObservationWithinOneBatch covers the first and last
// observation of one unchanged context inside a single batch, and a backfill that
// extends ObservedFrom independently of a later ObservedUntil extension.
func TestNativeMetadataObservationWithinOneBatch(t *testing.T) {
	owner := observationOwner("pod-a")

	for _, tc := range []struct {
		name          string
		batches       [][]observationPoint
		effectiveFrom int64
		observedFrom  int64
		observedUntil int64
		timestamps    []int64
	}{
		{
			name:          "first_and_last_in_one_batch",
			batches:       [][]observationPoint{observationBatch(1000, 2000)},
			effectiveFrom: 1000, observedFrom: 1000, observedUntil: 2000,
			timestamps: []int64{1000, 2000},
		},
		{
			name:          "first_and_last_of_three_in_one_batch",
			batches:       [][]observationPoint{observationBatch(1000, 2000, 3000)},
			effectiveFrom: 1000, observedFrom: 1000, observedUntil: 3000,
			timestamps: []int64{1000, 2000, 3000},
		},
		{
			name:          "backfill_in_one_batch",
			batches:       [][]observationPoint{observationBatch(3000, 1000)},
			effectiveFrom: 1000, observedFrom: 1000, observedUntil: 3000,
			timestamps: []int64{1000, 3000},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newObservationStorage(t, t.TempDir(), true)
			for _, batch := range tc.batches {
				consumeObservation(t, s, batch...)
			}
			history, ok := s.MetadataHistory(owner)
			require.True(t, ok)
			require.Len(t, history.Versions, 1, "unchanged content is one version, not one per observation")
			version := history.Versions[0]
			require.Equal(t, tc.effectiveFrom, version.EffectiveFrom)
			require.Equal(t, tc.observedFrom, version.ObservedFrom)
			require.Equal(t, tc.observedUntil, version.ObservedUntil)
			require.Equal(t, observationContent("host-1"), version.Content.Attributes)

			memberships, err := s.QueryMetadataSeries(t.Context(), columnar.MetadataSeriesQuery{
				Kind: columnar.MetadataResource, Start: 0, End: observationEnd,
			})
			require.NoError(t, err)
			require.Len(t, memberships.Series, 1, "unchanged ownership is one membership")
			require.Equal(t, tc.effectiveFrom, memberships.Series[0].MembershipStart)
			require.Equal(t, tc.observedUntil, memberships.Series[0].MembershipObservedTo)

			series, err := s.SelectSeries(t.Context(), 0, observationEnd, nil, 0)
			require.NoError(t, err)
			require.Len(t, series, 1, "the fixture must not manufacture a metadata sample series")
			require.Equal(t, tc.timestamps, series[0].Timestamps)
			require.Equal(t, observationValues(tc.timestamps), series[0].Values)
		})
	}

	// A metadata-only backfill extends ObservedFrom of a version that a later
	// observation already extended, without adding a sample or a version. Samples
	// cannot express this: the head rejects a sample older than its tail.
	t.Run("metadata_backfill_after_a_later_observation", func(t *testing.T) {
		s := newObservationStorage(t, t.TempDir(), true)
		consumeObservation(t, s, observationBatch(2000, 3000)...)
		_, err := s.MetadataAssociate(observationSeriesLabels(owner), columnar.Update{
			Owner: owner, Content: columnar.Content{Attributes: observationContent("host-1")}, EffectiveFrom: 1000,
		})
		require.NoError(t, err)

		history, ok := s.MetadataHistory(owner)
		require.True(t, ok)
		require.Len(t, history.Versions, 1)
		require.Equal(t, int64(1000), history.Versions[0].EffectiveFrom)
		require.Equal(t, int64(1000), history.Versions[0].ObservedFrom)
		require.Equal(t, int64(3000), history.Versions[0].ObservedUntil)
	})
}

// TestNativeMetadataObservationRepeatedBatches re-observes one unchanged context
// in five batches. Each batch may only extend the existing bound: no version, no
// membership and no sample may be added, and the result must survive a flush and
// a reopen.
func TestNativeMetadataObservationRepeatedBatches(t *testing.T) {
	owner := observationOwner("pod-a")
	dir := t.TempDir()
	s := newObservationStorage(t, dir, false)

	for _, timestamp := range []int64{1000, 2000, 3000, 4000, 5000} {
		consumeObservation(t, s, observationBatch(timestamp)...)
		history, ok := s.MetadataHistory(owner)
		require.True(t, ok)
		require.Len(t, history.Versions, 1, "re-observing unchanged content must not create an owner version")
		require.Equal(t, int64(1000), history.Versions[0].EffectiveFrom)
		require.Equal(t, int64(1000), history.Versions[0].ObservedFrom)
		require.Equal(t, timestamp, history.Versions[0].ObservedUntil)
	}
	memberships, err := s.QueryMetadataSeries(t.Context(), columnar.MetadataSeriesQuery{
		Kind: columnar.MetadataResource, Start: 0, End: 6000,
	})
	require.NoError(t, err)
	require.Len(t, memberships.Series, 1, "re-observing unchanged ownership must not create a membership")
	require.Equal(t, int64(5000), memberships.Series[0].MembershipObservedTo)

	require.NoError(t, s.Flush())
	require.NoError(t, s.Close())
	history, ok := reopenObservationStorage(t, dir).MetadataHistory(owner)
	require.True(t, ok)
	require.Len(t, history.Versions, 1)
	require.Equal(t, int64(1000), history.Versions[0].EffectiveFrom)
	require.Equal(t, int64(5000), history.Versions[0].ObservedUntil)
}

// TestNativeMetadataObservationContentChange asserts that a descriptive content
// change is a version change that creates no membership, that history keeps each
// version's own observation bounds, and that info() enriches each validity band
// with the content of that band.
func TestNativeMetadataObservationContentChange(t *testing.T) {
	owner := observationOwner("pod-a")
	engine := observationEngine(t)
	dir := t.TempDir()
	s := newObservationStorage(t, dir, false)

	changed := observationPoint{resource: observationContentMap("pod-a", "host-2"), route: observationRoute, timestamp: 3000}
	consumeObservation(t, s, append(observationBatch(1000, 2000), changed)...)

	history, ok := s.MetadataHistory(owner)
	require.True(t, ok)
	require.Len(t, history.Versions, 2, "changed content is a new version, not an observation")
	require.Equal(t, int64(1000), history.Versions[0].EffectiveFrom)
	require.Equal(t, int64(1000), history.Versions[0].ObservedFrom)
	require.Equal(t, int64(2000), history.Versions[0].ObservedUntil)
	require.Equal(t, observationContent("host-1"), history.Versions[0].Content.Attributes)
	require.Equal(t, int64(3000), history.Versions[1].EffectiveFrom)
	require.Equal(t, int64(3000), history.Versions[1].ObservedFrom)
	require.Equal(t, int64(3000), history.Versions[1].ObservedUntil)
	require.Equal(t, observationContent("host-2"), history.Versions[1].Content.Attributes)

	memberships, err := s.QueryMetadataSeries(t.Context(), columnar.MetadataSeriesQuery{
		Kind: columnar.MetadataResource, Start: 0, End: observationEnd,
	})
	require.NoError(t, err)
	require.Len(t, memberships.Series, 1, "a content change must not create a membership")
	require.Equal(t, int64(1000), memberships.Series[0].MembershipStart)
	require.Equal(t, int64(3000), memberships.Series[0].MembershipObservedTo)
	require.Len(t, memberships.Series[0].Versions, 2)

	// info() must enrich each band with that band's content, never with the later
	// content retroactively.
	at1500 := queryInfoLabels(t, engine, s, fmt.Sprintf("info(%s)", observationMetricName), 1500)
	require.Len(t, at1500, 1)
	require.Equal(t, "host-1", at1500[0].Get("host_name"))
	at3000 := queryInfoLabels(t, engine, s, fmt.Sprintf("info(%s)", observationMetricName), 3000)
	require.Len(t, at3000, 1)
	require.Equal(t, "host-2", at3000[0].Get("host_name"))

	require.NoError(t, s.Flush())
	require.NoError(t, s.Close())
	recovered, ok := reopenObservationStorage(t, dir).MetadataHistory(owner)
	require.True(t, ok)
	require.Equal(t, history, recovered, "block recovery must keep each version's own observation bounds")
}

// TestMetadataObservationMembershipChange asserts that a membership change keeps
// each membership's own observation bound: observing another series must not
// extend or shorten it.
func TestMetadataObservationMembershipChange(t *testing.T) {
	first := observationOwner("pod-a")
	second := observationOwner("pod-b")
	s := newObservationStorage(t, t.TempDir(), true)
	engine := observationEngine(t)
	secondLabels := func(route string) labels.Labels {
		return labels.FromStrings(
			labels.MetricName, observationMetricName, columnar.OTELResourceLabel, first.Ref(),
			"__type__", "gauge", "instance", "pod-a", "job", "shop/checkout",
			"otel_scope_name", "observation-sdk", "otel_scope_schema_url", observationScope,
			"otel_scope_version", "v1", "route", route,
		)
	}

	consumeObservation(t, s, observationBatch(1000, 2000, 3000)...)
	consumeObservation(t, s,
		observationPoint{resource: observationContentMap("pod-a", "host-1"), route: "/second", timestamp: 4000},
		observationPoint{resource: observationContentMap("pod-b", "host-1"), route: "/moved", timestamp: 5000},
	)

	memberships, err := s.QueryMetadataSeries(t.Context(), columnar.MetadataSeriesQuery{
		Kind: columnar.MetadataResource, Start: 0, End: 6000,
	})
	require.NoError(t, err)
	byLabels := map[string]columnar.MetadataSeriesResult{}
	for _, item := range memberships.Series {
		byLabels[item.Labels.String()] = item
	}
	require.Len(t, byLabels, 3, "each series keeps one membership row")
	original := byLabels[observationSeriesLabels(first).String()]
	require.Equal(t, int64(1000), original.MembershipStart)
	require.Equal(t, int64(3000), original.MembershipObservedTo)
	sibling := byLabels[secondLabels("/second").String()]
	require.Equal(t, int64(4000), sibling.MembershipStart)
	require.Equal(t, int64(4000), sibling.MembershipObservedTo)
	moved := byLabels[labels.FromStrings(
		labels.MetricName, observationMetricName, columnar.OTELResourceLabel, second.Ref(),
		"__type__", "gauge", "instance", "pod-b", "job", "shop/checkout",
		"otel_scope_name", "observation-sdk", "otel_scope_schema_url", observationScope,
		"otel_scope_version", "v1", "route", "/moved",
	).String()]
	require.Equal(t, int64(5000), moved.MembershipStart)
	require.Equal(t, int64(5000), moved.MembershipObservedTo)

	firstHistory, ok := s.MetadataHistory(first)
	require.True(t, ok)
	require.Len(t, firstHistory.Versions, 1)
	require.Equal(t, int64(4000), firstHistory.Versions[0].ObservedUntil)
	secondHistory, ok := s.MetadataHistory(second)
	require.True(t, ok)
	require.Len(t, secondHistory.Versions, 1)
	require.Equal(t, int64(5000), secondHistory.Versions[0].ObservedFrom)
	require.Equal(t, int64(5000), secondHistory.Versions[0].ObservedUntil)

	// info() resolves each series through its own membership and content.
	enriched := queryInfoLabels(t, engine, s, fmt.Sprintf("info(%s)", observationMetricName), 5500)
	require.Len(t, enriched, 3)
	hosts := map[string]string{}
	for _, item := range enriched {
		hosts[item.Get("route")] = item.Get("host_name")
	}
	require.Equal(t, map[string]string{"/observed": "host-1", "/second": "host-1", "/moved": "host-1"}, hosts)
}

// TestMetadataObservationInterleavedContexts applies interleaved observations in
// one batch. The store must keep the original assertion order: a later content is
// never applied retroactively and map iteration must not reorder change points.
func TestMetadataObservationInterleavedContexts(t *testing.T) {
	first := observationOwner("pod-a")
	second := observationOwner("pod-b")
	seriesA := labels.FromStrings(labels.MetricName, "interleaved_metric", columnar.OTELResourceLabel, first.Ref(), "route", "/a")
	seriesB := labels.FromStrings(labels.MetricName, "interleaved_metric", columnar.OTELResourceLabel, first.Ref(), "route", "/b")
	seriesC := labels.FromStrings(labels.MetricName, "interleaved_metric", columnar.OTELResourceLabel, second.Ref(), "route", "/c")
	contentA := columnar.Content{Attributes: observationContent("host-1")}
	contentB := columnar.Content{Attributes: observationContent("host-2")}

	s := newObservationStorage(t, t.TempDir(), true)
	require.NoError(t, s.AppendBatch(t.Context(), columnar.Batch{Updates: []columnar.AssociationUpdate{
		{Labels: seriesA, Update: columnar.Update{Owner: first, Content: contentA, EffectiveFrom: 1000}},
		{Labels: seriesB, Update: columnar.Update{Owner: first, Content: contentB, EffectiveFrom: 2000}},
		{Labels: seriesC, Update: columnar.Update{Owner: second, Content: contentA, EffectiveFrom: 1500}},
		{Labels: seriesA, Update: columnar.Update{Owner: first, Content: contentA, EffectiveFrom: 3000}},
	}}))

	history, ok := s.MetadataHistory(first)
	require.True(t, ok)
	require.Len(t, history.Versions, 3, "A, B, A is two change points, in input order")
	for i, want := range []struct {
		effectiveFrom int64
		host          string
	}{
		{effectiveFrom: 1000, host: "host-1"},
		{effectiveFrom: 2000, host: "host-2"},
		{effectiveFrom: 3000, host: "host-1"},
	} {
		require.Equal(t, want.effectiveFrom, history.Versions[i].EffectiveFrom)
		require.Equal(t, want.effectiveFrom, history.Versions[i].ObservedFrom)
		require.Equal(t, want.effectiveFrom, history.Versions[i].ObservedUntil)
		require.Equal(t, want.host, history.Versions[i].Content.Attributes["host.name"])
	}

	other, ok := s.MetadataHistory(second)
	require.True(t, ok)
	require.Len(t, other.Versions, 1, "an interleaved owner keeps its own history")
	require.Equal(t, int64(1500), other.Versions[0].EffectiveFrom)
	require.Equal(t, int64(1500), other.Versions[0].ObservedUntil)

	// The same series observed again in a later batch extends its own membership
	// and the owner's latest version bound, and adds no version.
	require.NoError(t, s.AppendBatch(t.Context(), columnar.Batch{Updates: []columnar.AssociationUpdate{
		{Labels: seriesA, Update: columnar.Update{Owner: first, Content: contentA, EffectiveFrom: 4000}},
		{Labels: seriesA, Update: columnar.Update{Owner: first, Content: contentA, EffectiveFrom: 5000}},
	}}))
	history, ok = s.MetadataHistory(first)
	require.True(t, ok)
	require.Len(t, history.Versions, 3, "unchanged re-observation must not add a version")
	require.Equal(t, int64(5000), history.Versions[2].ObservedUntil)

	for _, tc := range []struct {
		at    int64
		host  string
		found bool
	}{
		{at: 999, found: false},
		{at: 1000, host: "host-1", found: true},
		{at: 1999, host: "host-1", found: true},
		{at: 2000, host: "host-2", found: true},
		{at: 2999, host: "host-2", found: true},
		{at: 3000, host: "host-1", found: true},
		{at: 4999, host: "host-1", found: true},
	} {
		content, found, truncated := s.MetadataAtLabels(seriesA, columnar.MetadataResource, tc.at)
		require.Equal(t, tc.found, found, "at %d", tc.at)
		require.False(t, truncated)
		if tc.found {
			require.Equal(t, tc.host, content.Attributes["host.name"], "at %d", tc.at)
		}
	}
}

// TestMetadataObservationConflictsLeaveNoState asserts that same-time conflicts
// stay all-or-nothing: no sample, version, membership, observation extension or
// WAL trace survives a rejected commit.
func TestMetadataObservationConflictsLeaveNoState(t *testing.T) {
	resource := observationOwner("pod-a")
	resourceLabels := observationSeriesLabels(resource)
	sourceLabels := labels.FromStrings(labels.MetricName, "conflict_metric")
	contentA := columnar.Content{Attributes: observationContent("host-1")}
	contentB := columnar.Content{Attributes: observationContent("host-2")}

	for _, tc := range []struct {
		name           string
		series         labels.Labels
		owner          columnar.OwnerKey
		rejectedOwner  columnar.OwnerKey
		rejected       columnar.Content
		failure        error
		acceptedRetain bool
	}{
		{
			name: "conflicting_content", series: resourceLabels, owner: resource,
			rejectedOwner: resource, rejected: contentB, failure: columnar.ErrConflictingMetadata,
		},
		{
			name: "conflicting_membership", series: sourceLabels, owner: columnar.SourceOwner("otap"),
			rejectedOwner: columnar.SourceOwner("other"), rejected: contentA, failure: columnar.ErrConflictingMembership,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s := newObservationStorage(t, dir, false)

			// The accepted commit observes the context once and appends one sample.
			require.NoError(t, s.AppendBatch(t.Context(), columnar.Batch{
				Series: []columnar.BatchSeries{{
					Labels: tc.series, Timestamps: []int64{1000},
					Kinds: []columnar.SampleKind{columnar.SampleFloat}, Values: []float64{1},
				}},
				Updates: []columnar.AssociationUpdate{{Labels: tc.series, Update: columnar.Update{
					Owner: tc.owner, Content: contentA, EffectiveFrom: 1000,
				}}},
			}))
			accepted, ok := s.MetadataHistory(tc.owner)
			require.True(t, ok)
			wal := walTrace(t, dir)

			// The rejected commit restates the same instant with different content or
			// a different owner, and also carries a sample that would extend the
			// observation bound if it were applied.
			if tc.rejectedOwner != tc.owner {
				// The rejected owner alone must not appear either.
				_, ok = s.MetadataHistory(tc.rejectedOwner)
				require.False(t, ok)
			}
			err := s.AppendBatch(t.Context(), columnar.Batch{
				Series: []columnar.BatchSeries{{
					Labels: tc.series, Timestamps: []int64{2000},
					Kinds: []columnar.SampleKind{columnar.SampleFloat}, Values: []float64{2},
				}},
				Updates: []columnar.AssociationUpdate{{Labels: tc.series, Update: columnar.Update{
					Owner: tc.rejectedOwner, Content: tc.rejected, EffectiveFrom: 1000,
				}}},
			})
			require.ErrorIs(t, err, tc.failure)

			history, ok := s.MetadataHistory(tc.owner)
			require.True(t, ok)
			require.Equal(t, accepted, history, "a rejected commit must not change version or observation state")
			require.Equal(t, int64(1000), history.Versions[0].ObservedUntil, "a rejected commit must not extend an observation bound")

			series, err := s.SelectSeries(t.Context(), 0, observationEnd, nil, 0)
			require.NoError(t, err)
			require.Len(t, series, 1)
			require.Equal(t, []int64{1000}, series[0].Timestamps, "a rejected commit must not append samples")
			require.Equal(t, wal, walTrace(t, dir), "a rejected commit must not write a WAL record")
		})
	}
}

// TestNativeMetadataObservationRejectedBatchIngestsNothing asserts that a
// rejected native batch leaves no sample, no metadata and no WAL trace, which is
// the observation-bound variant of the all-or-nothing import contract.
func TestNativeMetadataObservationRejectedBatchIngestsNothing(t *testing.T) {
	owner := observationOwner("pod-a")
	dir := t.TempDir()
	s := newObservationStorage(t, dir, false)

	consumeObservation(t, s, observationBatch(1000)...)
	accepted, ok := s.MetadataHistory(owner)
	require.True(t, ok)
	wal := walTrace(t, dir)

	// Two Resources share one identity but state different content at one
	// instant, and neither content is the one active before it, so the conflict
	// does not depend on which of them the payload lists first.
	first := observationContentMap("pod-a", "host-2")
	second := observationContentMap("pod-a", "host-1")
	second["process.pid"] = "4243"
	err := consumeObservationErr(t, s,
		observationPoint{resource: first, route: "/first", timestamp: 2000},
		observationPoint{resource: second, route: "/second", timestamp: 2000},
	)
	require.ErrorIs(t, err, columnar.ErrConflictingMetadata)

	history, ok := s.MetadataHistory(owner)
	require.True(t, ok)
	require.Equal(t, accepted, history)
	series, err := s.SelectSeries(t.Context(), 0, observationEnd, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Equal(t, []int64{1000}, series[0].Timestamps)
	require.Equal(t, wal, walTrace(t, dir))
}

// TestMetadataObservationHistoricalVersionDelta asserts that extending the
// observation bound of an older version publishes one append-only row for that
// change point: recovery merges it monotonically, keeps the later versions where
// they were, and never turns the observation time into a validity boundary.
func TestMetadataObservationHistoricalVersionDelta(t *testing.T) {
	dir := t.TempDir()
	s := newObservationStorage(t, dir, false)
	owner := observationOwner("pod-a")
	observe := func(at int64, host string) {
		require.NoError(t, s.MetadataUpdate(columnar.Update{
			Owner: owner, Content: columnar.Content{Attributes: observationContent(host)}, EffectiveFrom: at,
		}))
	}

	observe(1000, "host-1")
	observe(2000, "host-2")
	observe(3000, "host-3")
	require.NoError(t, s.Flush())
	require.Equal(t, 1, s.NumBlocks())

	// A backfilled observation of the first version only. It is not a change
	// point, so it must not publish a version, a membership or a validity bound.
	observe(1500, "host-1")
	require.NoError(t, s.Flush())
	require.Equal(t, 2, s.NumBlocks())
	blocks, err := columnar.ListBlocks(dir)
	require.NoError(t, err)
	require.Len(t, blocks, 2)
	// The delta block restates exactly the change point it extends, and nothing
	// else: one owner, one content, one version row and no membership.
	require.Equal(t, &columnar.MetadataSummary{
		NumOwners: 1, NumContents: 1, NumVersions: 1,
		MinEffectiveFrom: 1000, MaxEffectiveFrom: 1000,
		Kinds:          []string{"resource"},
		AttributeNames: []string{"deployment.environment.name", "host.name", "process.pid", "sandboxed"},
	}, blocks[1].Meta().Metadata, "an observation time is not a validity boundary")

	history, ok := s.MetadataHistory(owner)
	require.True(t, ok)
	require.Len(t, history.Versions, 3)
	require.Equal(t, int64(1500), history.Versions[0].ObservedUntil)
	require.Equal(t, int64(2000), history.Versions[1].ObservedUntil, "a later version keeps its own observation bound")
	require.Equal(t, int64(3000), history.Versions[2].ObservedUntil)
	owners, err := s.QueryMetadata(t.Context(), columnar.MetadataQuery{
		Kind: columnar.MetadataResource, Start: 0, End: observationEnd,
	})
	require.NoError(t, err)
	require.Len(t, owners.Owners, 1)
	// Validity is still ended by the next change point, so the backfilled
	// observation moves ObservedUntil only.
	require.Equal(t, []columnar.MetadataValueView{
		{
			Descriptive: observationContent("host-1"), EffectiveFrom: 1000, ValidUntil: int64Ptr(2000),
			ObservedFrom: 1000, ObservedUntil: 1500,
		},
		{
			Descriptive: observationContent("host-2"), EffectiveFrom: 2000, ValidUntil: int64Ptr(3000),
			ObservedFrom: 2000, ObservedUntil: 2000,
		},
		{
			Descriptive: observationContent("host-3"), EffectiveFrom: 3000,
			ObservedFrom: 3000, ObservedUntil: 3000,
		},
	}, owners.Owners[0].Versions)
	require.NoError(t, s.Close())

	recovered, ok := reopenObservationStorage(t, dir).MetadataHistory(owner)
	require.True(t, ok)
	require.Equal(t, history, recovered, "the delta row must merge into the version it restates")
}

// TestMetadataObservationDuplicateWithinBounds asserts that a duplicate
// observation inside the recorded bounds is not a change: it adds no version, no
// membership and no block, which is the distinction between a re-observation that
// extends a bound and one that repeats it.
func TestMetadataObservationDuplicateWithinBounds(t *testing.T) {
	owner := observationOwner("pod-a")
	s := newObservationStorage(t, t.TempDir(), false)

	consumeObservation(t, s, observationBatch(1000, 2000)...)
	require.NoError(t, s.Flush())
	require.Equal(t, 1, s.NumBlocks())
	published, ok := s.MetadataHistory(owner)
	require.True(t, ok)
	require.Equal(t, int64(2000), published.Versions[0].ObservedUntil)

	// 1500 and 2000 are inside the recorded [1000, 2000] observation bounds. They
	// are restated without samples, because the head already holds the samples.
	for _, at := range []int64{1500, 2000} {
		_, err := s.MetadataAssociate(observationSeriesLabels(owner), columnar.Update{
			Owner: owner, Content: columnar.Content{Attributes: observationContent("host-1")}, EffectiveFrom: at,
		})
		require.NoError(t, err)
	}
	require.NoError(t, s.Flush())
	require.Equal(t, 1, s.NumBlocks(), "a duplicate observation inside the recorded bounds publishes nothing")
	repeated, ok := s.MetadataHistory(owner)
	require.True(t, ok)
	require.Equal(t, published, repeated)
}

// TestMetadataObservationOrdinaryAppend covers the ordinary scalar metadata path,
// which shares the change detection: re-observing unchanged metric metadata keeps
// one version and one membership whose bounds advance, rather than one per sample.
func TestMetadataObservationOrdinaryAppend(t *testing.T) {
	dir := t.TempDir()
	s := newObservationStorage(t, dir, false)
	ls := labels.FromStrings(labels.MetricName, "ordinary_metric")
	owner := columnar.MetricOwner(ls)
	md := metadata.Metadata{Type: model.MetricTypeGauge, Help: "ordinary help"}

	// One append per commit, so the second commit only re-observes metadata that
	// the first one already recorded.
	for _, timestamp := range []int64{1000, 2000} {
		app := s.AppenderV2(t.Context())
		_, err := app.Append(0, ls, 0, timestamp, float64(timestamp), nil, nil, storage.AOptions{Metadata: md})
		require.NoError(t, err)
		require.NoError(t, app.Commit())
	}

	history, ok := s.MetadataHistory(owner)
	require.True(t, ok)
	require.Len(t, history.Versions, 1, "one unchanged metric metadata observation is one version")
	require.Equal(t, int64(1000), history.Versions[0].EffectiveFrom)
	require.Equal(t, int64(1000), history.Versions[0].ObservedFrom)
	require.Equal(t, int64(2000), history.Versions[0].ObservedUntil)
	require.Equal(t, "ordinary help", history.Versions[0].Content.Attributes["help"])

	memberships, err := s.QueryMetadataSeries(t.Context(), columnar.MetadataSeriesQuery{
		Kind: columnar.MetadataMetric, Start: 0, End: observationEnd,
	})
	require.NoError(t, err)
	require.Len(t, memberships.Series, 1)
	require.Equal(t, int64(2000), memberships.Series[0].MembershipObservedTo)

	require.NoError(t, s.Flush())
	require.NoError(t, s.Close())
	recovered, ok := reopenObservationStorage(t, dir).MetadataHistory(owner)
	require.True(t, ok)
	require.Equal(t, history, recovered)
}

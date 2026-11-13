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

// A series context is derived once per Resource/metric/point context, not once
// per series identity. Two Resources can project to the same complete label set
// - identical identifying attributes, so the same Resource identity reference,
// and identical promoted labels - while stating different descriptive content.
// The derived context must keep them apart: a cache keyed by the final series
// identity or by the projected labels would silently replace one Resource's
// content with the other's.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/open-telemetry/otel-arrow/go/pkg/otel/arrow_record"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/storage/remote/otap"
)

// nativeReuseFixture is two Resources with equal identity and different content.
type nativeReuseResource struct {
	environment string
	timestamps  []int64
	values      []float64
}

func TestNativeAssociationKeepsPerResourceContent(t *testing.T) {
	resources := []nativeReuseResource{
		{environment: "prod", timestamps: []int64{1000, 2000}, values: []float64{1, 2}},
		{environment: "staging", timestamps: []int64{3000, 4000}, values: []float64{3, 4}},
	}

	md := pmetric.NewMetrics()
	for _, resource := range resources {
		rm := md.ResourceMetrics().AppendEmpty()
		rm.Resource().Attributes().PutStr("service.name", "checkout")
		rm.Resource().Attributes().PutStr("service.instance.id", "pod-1")
		rm.Resource().Attributes().PutStr("deployment.environment", resource.environment)
		sm := rm.ScopeMetrics().AppendEmpty()
		sm.Scope().SetName("native.reuse")
		sm.Scope().SetVersion("1.0.0")
		metric := sm.Metrics().AppendEmpty()
		metric.SetName("primary_reuse_gauge")
		points := metric.SetEmptyGauge().DataPoints()
		for i, timestamp := range resource.timestamps {
			point := points.AppendEmpty()
			point.SetTimestamp(pcommon.Timestamp(timestamp * int64(time.Millisecond)))
			point.SetDoubleValue(resource.values[i])
		}
	}

	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })
	batch, err := producer.BatchArrowRecordsFromMetrics(md)
	require.NoError(t, err)

	store := newPrimaryStorage(t)
	sink := columnar.NewOTAPSink(store, slog.New(slog.DiscardHandler))
	stream := otap.NewStreamConsumer(nil, otap.WithRecordSink(sink))
	t.Cleanup(func() { require.NoError(t, stream.Close()) })
	require.NoError(t, stream.ConsumeBatch(context.Background(), batch))

	// Both Resources project to one complete series, because they share every
	// identifying attribute and every promoted label.
	all, err := store.SelectSeries(context.Background(), 0, 1<<62, nil, 0)
	require.NoError(t, err)
	require.Len(t, all, 1)
	series := all[0]
	require.Equal(t, "checkout", series.Labels.Get("job"))
	require.Equal(t, "pod-1", series.Labels.Get("instance"))
	require.Equal(t, []int64{1000, 2000, 3000, 4000}, series.Timestamps)
	require.Equal(t, []float64{1, 2, 3, 4}, series.Values)

	// One Resource identity, two versions: the content of each Resource is reached
	// through its own series context, and the later version does not rewrite the
	// earlier one's observation bound.
	resourceOwner := columnar.ResourceOwner(map[string]string{
		"service.name": "checkout", "service.instance.id": "pod-1",
	})
	history, ok := store.MetadataHistory(resourceOwner)
	require.True(t, ok)
	require.Len(t, history.Versions, 2, "each Resource content is its own version")
	require.Equal(t, "prod", history.Versions[0].Content.Attributes["deployment.environment"])
	require.Equal(t, int64(1000), history.Versions[0].EffectiveFrom)
	require.Equal(t, int64(1000), history.Versions[0].ObservedFrom)
	require.Equal(t, int64(2000), history.Versions[0].ObservedUntil)
	require.Equal(t, "staging", history.Versions[1].Content.Attributes["deployment.environment"])
	require.Equal(t, int64(3000), history.Versions[1].EffectiveFrom)
	require.Equal(t, int64(3000), history.Versions[1].ObservedFrom)
	require.Equal(t, int64(4000), history.Versions[1].ObservedUntil)

	for _, want := range []struct {
		at          int64
		environment string
	}{
		{at: 1000, environment: "prod"},
		{at: 2000, environment: "prod"},
		{at: 2999, environment: "prod"},
		{at: 3000, environment: "staging"},
		{at: 4000, environment: "staging"},
		{at: 5000, environment: "staging"},
	} {
		content, found, truncated := store.MetadataAtLabels(series.Labels, columnar.MetadataResource, want.at)
		require.True(t, found, "Resource content at %d", want.at)
		require.False(t, truncated)
		require.Equal(t, want.environment, content.Attributes["deployment.environment"],
			"Resource content at %d belongs to the Resource that stated it", want.at)
	}

	// One series, so one membership row per owner kind: the Resource identity did
	// not change and the Metric owner is the same series.
	memberships, err := store.QueryMetadataSeries(context.Background(), columnar.MetadataSeriesQuery{
		Kind: columnar.MetadataResource, Start: 0, End: 1 << 62,
	})
	require.NoError(t, err)
	require.Len(t, memberships.Series, 1)
	require.Equal(t, int64(1000), memberships.Series[0].MembershipStart)
	require.Equal(t, int64(4000), memberships.Series[0].MembershipObservedTo)
	require.Nil(t, memberships.Series[0].MembershipUntil)

	metric, ok := store.MetadataHistory(columnar.MetricOwner(series.Labels))
	require.True(t, ok, "the metric owner is the stored series")
	require.Len(t, metric.Versions, 1)
	require.Equal(t, int64(1000), metric.Versions[0].ObservedFrom)
	require.Equal(t, int64(4000), metric.Versions[0].ObservedUntil)
}

// TestNativeAssociationReusesRepeatedPointContent pins the reuse the mechanism
// depends on: the encoder writes one attribute row per data point even when the
// content repeats, so the context key is the attribute content. A change of content
// within one metric row must still produce a distinct context.
func TestNativeAssociationReusesRepeatedPointContent(t *testing.T) {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "checkout")
	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("native.reuse")

	appendPoints := func(name, series string, timestamps []int64) {
		sm.Metrics().AppendEmpty()
		metric := sm.Metrics().At(sm.Metrics().Len() - 1)
		metric.SetName(name)
		points := metric.SetEmptyGauge().DataPoints()
		for _, timestamp := range timestamps {
			point := points.AppendEmpty()
			point.SetTimestamp(pcommon.Timestamp(timestamp * int64(time.Millisecond)))
			point.SetDoubleValue(float64(timestamp))
			point.Attributes().PutStr("series", series)
		}
	}
	appendPoints("primary_reuse_a", "s1", []int64{1000, 2000, 3000})
	appendPoints("primary_reuse_b", "s1", []int64{1000, 2000, 3000})
	appendPoints("primary_reuse_c", "s2", []int64{1000, 2000, 3000})

	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })
	batch, err := producer.BatchArrowRecordsFromMetrics(md)
	require.NoError(t, err)

	store := newPrimaryStorage(t)
	sink := columnar.NewOTAPSink(store, slog.New(slog.DiscardHandler))
	stream := otap.NewStreamConsumer(nil, otap.WithRecordSink(sink))
	t.Cleanup(func() { require.NoError(t, stream.Close()) })
	require.NoError(t, stream.ConsumeBatch(context.Background(), batch))

	stats := store.ReadStats().NativeDerivation
	// Three metric rows over three points each: nine samples, but only three series
	// contexts, because every row repeats one attribute content three times.
	require.Equal(t, uint64(9), stats.Samples)
	require.Equal(t, uint64(3), stats.Series, "one series per distinct name and series label")
	require.Equal(t, uint64(3), stats.TemplateMisses)
	require.Equal(t, uint64(6), stats.TemplateHits)
	require.Equal(t, uint64(9), stats.PointContextCanonicalizations,
		"the point attribute content is identified on every row that states it")

	// Every metric row holds its own exact timestamps.
	all, err := store.SelectSeries(context.Background(), 0, 1<<62, nil, 0)
	require.NoError(t, err)
	require.Len(t, all, 3)
	for _, series := range all {
		require.Equal(t, []int64{1000, 2000, 3000}, series.Timestamps, "%s", series.Labels)
	}

	// A reused context still states its own observation bound for its own series.
	for _, series := range all {
		owner := columnar.MetricOwner(series.Labels)
		history, ok := store.MetadataHistory(owner)
		require.True(t, ok)
		require.Len(t, history.Versions, 1)
		require.Equal(t, int64(1000), history.Versions[0].ObservedFrom)
		require.Equal(t, int64(3000), history.Versions[0].ObservedUntil)
	}
}

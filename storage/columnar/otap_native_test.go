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
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
	otlpprw "github.com/prometheus/prometheus/storage/remote/otlptranslator/prometheusremotewrite"
)

func TestOTAPSinkCapturesNativeMetadata(t *testing.T) {
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{FlushSamples: 1000})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	settings := nativeOTAPTestSettings()
	sink := nativeOTAPTestSink(s, settings)
	require.NoError(t, sink.ConsumeRecords(t.Context(), recordsFor(t, nativeOTAPTestMetrics(true))))

	series, err := s.SelectSeries(t.Context(), 0, 3_000, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 3, "gauge, sum and coexistence target_info")
	byName := map[string]columnar.Series{}
	for _, item := range series {
		byName[item.Labels.Get(labels.MetricName)] = item
	}

	gauge, ok := byName["http_server_requests_milliseconds"]
	require.True(t, ok)
	require.Equal(t, []int64{1000, 2000}, gauge.Timestamps)
	require.Equal(t, []int64{500, 500}, gauge.StartTimestamps)
	require.Equal(t, columnar.SampleFloat, gauge.Kinds[0])
	require.Equal(t, columnar.SampleStale, gauge.Kinds[1])
	require.Empty(t, gauge.Labels.Get("service.name"))
	require.Equal(t, "shop/checkout", gauge.Labels.Get("job"))
	require.Equal(t, "pod-a", gauge.Labels.Get("instance"))
	require.Equal(t, "west", gauge.Labels.Get("k8s_cluster_name"))
	require.Equal(t, "200", gauge.Labels.Get("status"))
	require.Equal(t, "my-sdk", gauge.Labels.Get("otel_scope_name"))
	require.Equal(t, "native-sdk", gauge.Labels.Get("otel_scope_call_kind"))
	require.Equal(t, "gauge", gauge.Labels.Get("__type__"))
	require.Equal(t, "milliseconds", gauge.Labels.Get("__unit__"))
	require.Empty(t, gauge.Labels.Get("host_name"), "descriptive Resource context is native, not a label")

	owner := columnar.ResourceOwner(map[string]string{
		"service.name": "checkout", "service.namespace": "shop", "service.instance.id": "pod-a",
	})
	require.Equal(t, owner.Ref(), gauge.Labels.Get(columnar.OTELResourceLabel))
	resource, ok, truncated := s.MetadataAtLabels(gauge.Labels, columnar.MetadataResource, 1500)
	require.True(t, ok)
	require.False(t, truncated)
	require.Equal(t, "host-1", resource.Attributes["host.name"])
	require.Equal(t, "42", resource.Attributes["pod.number"])
	require.Equal(t, "true", resource.Attributes["cache.ready"])
	require.Equal(t, `["native"]`, resource.Attributes["resource.tags"])
	require.NotContains(t, resource.Attributes, "service.name")
	require.NotContains(t, resource.Attributes, "call.kind", "Scope attributes do not become Resource content")

	resourceSchema, ok, _ := s.MetadataAtLabels(gauge.Labels, columnar.MetadataResourceSchema, 1500)
	require.True(t, ok)
	require.Equal(t, "resource-v1", resourceSchema.Attributes["schema_url"])
	scope, ok, _ := s.MetadataAtLabels(gauge.Labels, columnar.MetadataScope, 1500)
	require.True(t, ok)
	require.Equal(t, "scope-v1", scope.Attributes["schema_url"])
	require.Equal(t, map[string]string{"schema_url": "scope-v1"}, scope.Attributes)

	source, ok, _ := s.MetadataAtLabels(gauge.Labels, columnar.MetadataSource, 1500)
	require.True(t, ok)
	require.Equal(t, "otap", source.Attributes["transport"])
	_, ok, _ = s.MetadataAtLabels(gauge.Labels, columnar.MetadataScrape, 1500)
	require.False(t, ok, "OTAP carries no authoritative scrape cadence")
	metricMetadata, ok, _ := s.MetadataAtLabels(gauge.Labels, columnar.MetadataMetric, 1500)
	require.True(t, ok)
	require.Equal(t, "Request duration", metricMetadata.Attributes["help"])
	require.Equal(t, "milliseconds", metricMetadata.Attributes["unit"])
	require.Equal(t, "gauge", metricMetadata.Attributes["type"])

	sum, ok := byName["request_count_total"]
	require.True(t, ok)
	sumMetadata, ok, _ := s.MetadataAtLabels(sum.Labels, columnar.MetadataMetric, 1000)
	require.True(t, ok)
	require.Equal(t, "counter", sumMetadata.Attributes["type"])
	require.Equal(t, "requests", sumMetadata.Attributes["help"])

	targetInfo, ok := byName["target_info"]
	require.True(t, ok)
	require.Equal(t, owner.Ref(), targetInfo.Labels.Get(columnar.OTELResourceLabel))
	require.Equal(t, "host-1", targetInfo.Labels.Get("host_name"))
	require.Equal(t, []int64{1000, 2000}, targetInfo.Timestamps)
	require.Equal(t, "checkout", targetInfo.Labels.Get("service_name"), "legacy identifying attribute retention is preserved")

	history, ok := s.MetadataHistory(owner)
	require.True(t, ok)
	require.Len(t, history.Versions, 1, "all series and points in one Resource context share one version")

	engine := promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
		MaxSamples: promqltest.DefaultMaxSamplesPerQuery, Timeout: time.Minute, LookbackDelta: 5 * time.Minute,
		LabelNamerConfig: &promql.LabelNamerConfig{
			UnderscoreLabelSanitization: true, PreserveMultipleUnderscores: true,
		},
		Parser: parser.NewParser(promqltest.TestParserOpts),
	})
	enriched := queryInfoLabels(t, engine, s, `info(http_server_requests_milliseconds, {host_name="host-1"})`, 1500)
	require.Len(t, enriched, 1)
	require.Equal(t, "host-1", enriched[0].Get("host_name"))
	require.Empty(t, enriched[0].Get("transport"), "source metadata is not a Resource label")
}

func nativeOTAPTestMetrics(withStale bool) pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.SetSchemaUrl("resource-v1")
	attributes := rm.Resource().Attributes()
	attributes.PutStr("service.name", "checkout")
	attributes.PutStr("service.namespace", "shop")
	attributes.PutStr("service.instance.id", "pod-a")
	attributes.PutStr("host.name", "host-1")
	attributes.PutStr("k8s.cluster.name", "west")
	attributes.PutInt("pod.number", 42)
	attributes.PutBool("cache.ready", true)
	tags := attributes.PutEmptySlice("resource.tags")
	tags.AppendEmpty().SetStr("native")

	sm := rm.ScopeMetrics().AppendEmpty()
	sm.SetSchemaUrl("scope-v1")
	sm.Scope().SetName("my-sdk")
	sm.Scope().SetVersion("v2")
	sm.Scope().Attributes().PutStr("call.kind", "native-sdk")

	gauge := sm.Metrics().AppendEmpty()
	gauge.SetName("http.server.requests")
	gauge.SetDescription("Request duration")
	gauge.SetUnit("ms")
	dp := gauge.SetEmptyGauge().DataPoints().AppendEmpty()
	dp.SetStartTimestamp(pcommon.Timestamp(500_000_000))
	dp.SetTimestamp(pcommon.Timestamp(1_000_000_000))
	dp.SetDoubleValue(21.5)
	dp.Attributes().PutStr("route", "/checkout")
	dp.Attributes().PutInt("status", 200)
	if withStale {
		stale := gauge.Gauge().DataPoints().AppendEmpty()
		stale.SetStartTimestamp(pcommon.Timestamp(500_000_000))
		stale.SetTimestamp(pcommon.Timestamp(2_000_000_000))
		stale.Attributes().PutStr("route", "/checkout")
		stale.Attributes().PutInt("status", 200)
		stale.SetFlags(stale.Flags().WithNoRecordedValue(true))
	}

	sumMetric := sm.Metrics().AppendEmpty()
	sumMetric.SetName("request.count")
	sumMetric.SetDescription("requests")
	sumMetric.SetUnit("1")
	sum := sumMetric.SetEmptySum()
	sum.SetIsMonotonic(true)
	sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	sumPoint := sum.DataPoints().AppendEmpty()
	sumPoint.SetTimestamp(pcommon.Timestamp(1_000_000_000))
	sumPoint.SetIntValue(7)
	return md
}

func nativeOTAPTestSettings() otlpprw.Settings {
	return otlpprw.Settings{
		AddMetricSuffixes: true,
		AllowUTF8:         false,
		PromoteResourceAttributes: otlpprw.NewPromoteResourceAttributes(config.OTLPConfig{
			PromoteResourceAttributes: []string{"k8s.cluster.name"},
		}),
		KeepIdentifyingResourceAttributes:    true,
		PromoteScopeMetadata:                 true,
		AllowDeltaTemporality:                true,
		LookbackDelta:                        5 * time.Minute,
		EnableTypeAndUnitLabels:              true,
		LabelNameUnderscoreSanitization:      true,
		LabelNamePreserveMultipleUnderscores: true,
	}
}

func nativeOTAPTestSink(storage *columnar.Storage, settings otlpprw.Settings) *columnar.OTAPSink {
	return columnar.NewOTAPSink(storage, slog.New(slog.DiscardHandler), func() otlpprw.Settings { return settings })
}

func TestOTAPSinkResourceIdentityAndDescriptiveVersions(t *testing.T) {
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	settings := otlpprw.Settings{AllowUTF8: true, DisableTargetInfo: true, AllowDeltaTemporality: true}
	sink := nativeOTAPTestSink(s, settings)

	makeBatch := func(firstHost string, timestamp int64) pmetric.Metrics {
		md := pmetric.NewMetrics()
		for i, host := range []string{firstHost, "second-host"} {
			rm := md.ResourceMetrics().AppendEmpty()
			rm.Resource().Attributes().PutStr("service.name", "payments")
			rm.Resource().Attributes().PutStr("service.namespace", "prod")
			rm.Resource().Attributes().PutStr("service.instance.id", []string{"one", "two"}[i])
			rm.Resource().Attributes().PutStr("host.name", host)
			metric := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
			metric.SetName("resource_series")
			dp := metric.SetEmptyGauge().DataPoints().AppendEmpty()
			dp.SetTimestamp(pcommon.Timestamp(timestamp * 1_000_000))
			dp.SetDoubleValue(1)
		}
		return md
	}
	require.NoError(t, sink.ConsumeRecords(t.Context(), recordsFor(t, makeBatch("first-host", 1000))))
	series, err := s.SelectSeries(t.Context(), 0, 2000, nil, 0)
	require.NoError(t, err)
	require.Len(t, series, 2)

	ownerOne := columnar.ResourceOwner(map[string]string{
		"service.name": "payments", "service.namespace": "prod", "service.instance.id": "one",
	})
	ownerTwo := columnar.ResourceOwner(map[string]string{
		"service.name": "payments", "service.namespace": "prod", "service.instance.id": "two",
	})
	require.NotEqual(t, ownerOne.Ref(), ownerTwo.Ref())
	byInstance := map[string]columnar.Series{}
	for _, item := range series {
		byInstance[item.Labels.Get(model.InstanceLabel)] = item
	}
	firstSeries := byInstance["one"]
	secondSeries := byInstance["two"]
	require.Equal(t, ownerOne.Ref(), firstSeries.Labels.Get(columnar.OTELResourceLabel))
	require.Equal(t, ownerTwo.Ref(), secondSeries.Labels.Get(columnar.OTELResourceLabel))
	require.Equal(t, "prod/payments", firstSeries.Labels.Get(model.JobLabel))
	require.Equal(t, "prod/payments", secondSeries.Labels.Get(model.JobLabel))

	require.NoError(t, sink.ConsumeRecords(t.Context(), recordsFor(t, makeBatch("changed-host", 2000))))
	firstHistory, ok := s.MetadataHistory(ownerOne)
	require.True(t, ok)
	require.Len(t, firstHistory.Versions, 2)
	secondHistory, ok := s.MetadataHistory(ownerTwo)
	require.True(t, ok)
	require.Len(t, secondHistory.Versions, 1, "an unchanged sibling Resource adds no version")
	before, ok, _ := s.MetadataAtLabels(firstSeries.Labels, columnar.MetadataResource, 1000)
	require.True(t, ok)
	require.Equal(t, "first-host", before.Attributes["host.name"])
	after, ok, _ := s.MetadataAtLabels(firstSeries.Labels, columnar.MetadataResource, 2000)
	require.True(t, ok)
	require.Equal(t, "changed-host", after.Attributes["host.name"])
	require.Equal(t, ownerOne.Ref(), firstSeries.Labels.Get(columnar.OTELResourceLabel), "descriptive changes preserve series identity")

	sourceHistory, ok := s.MetadataHistory(columnar.SourceOwner("otap"))
	require.True(t, ok)
	require.Len(t, sourceHistory.Versions, 1, "all Resources share the actual ingestion-source owner")
}

type capturedOTLPRecord struct {
	labels labels.Labels
	start  int64
	t      int64
	value  float64
	opts   storage.AOptions
}

type captureOTLPAppender struct {
	records []capturedOTLPRecord
}

func (a *captureOTLPAppender) Append(_ storage.SeriesRef, ls labels.Labels, st, t int64, v float64, h *histogram.Histogram, fh *histogram.FloatHistogram, opts storage.AOptions) (storage.SeriesRef, error) {
	if h != nil || fh != nil {
		return 0, errors.New("unexpected histogram in native number parity test")
	}
	a.records = append(a.records, capturedOTLPRecord{labels: ls.Copy(), start: st, t: t, value: v, opts: opts})
	return storage.SeriesRef(len(a.records)), nil
}

func (*captureOTLPAppender) Commit() error   { return nil }
func (*captureOTLPAppender) Rollback() error { return nil }

func TestOTAPSinkMatchesOTLPTranslation(t *testing.T) {
	for _, temporality := range []pmetric.AggregationTemporality{
		pmetric.AggregationTemporalityCumulative,
		pmetric.AggregationTemporalityDelta,
	} {
		t.Run(temporality.String(), func(t *testing.T) {
			settings := nativeOTAPTestSettings()
			input := nativeOTAPTestMetrics(true)
			input.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(1).Sum().SetAggregationTemporality(temporality)

			captured := &captureOTLPAppender{}
			converter := otlpprw.NewPrometheusConverter(captured)
			_, err := converter.FromMetrics(t.Context(), input, settings)
			require.NoError(t, err)

			s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			sink := nativeOTAPTestSink(s, settings)
			require.NoError(t, sink.ConsumeRecords(t.Context(), recordsFor(t, input)))
			series, err := s.SelectSeries(t.Context(), 0, 3000, nil, 0)
			require.NoError(t, err)

			expected := map[uint64][]capturedOTLPRecord{}
			for _, record := range captured.records {
				hash := labels.StableHash(record.labels)
				expected[hash] = append(expected[hash], record)
			}
			actualSamples := 0
			for _, item := range series {
				withoutResource := labels.NewBuilder(item.Labels).Del(columnar.OTELResourceLabel).Labels()
				records := expected[labels.StableHash(withoutResource)]
				require.NotEmpty(t, records, "unexpected OTAP series %s", item.Labels)
				for i, record := range records {
					require.True(t, labels.Equal(withoutResource, record.labels))
					require.Equal(t, []int64{record.t}, item.Timestamps[i:i+1])
					require.Equal(t, []int64{record.start}, item.StartTimestamps[i:i+1])
					if item.Kinds[i] == columnar.SampleStale {
						require.True(t, value.IsStaleNaN(record.value))
					} else {
						require.Equal(t, record.value, item.Values[i])
					}

					nativeMetadata, ok, truncated := s.MetadataAtLabels(item.Labels, columnar.MetadataMetric, record.t)
					require.True(t, ok)
					require.False(t, truncated)
					require.Equal(t, string(record.opts.Metadata.Type), nativeMetadata.Attributes["type"])
					require.Equal(t, record.opts.Metadata.Unit, nativeMetadata.Attributes["unit"])
					require.Equal(t, record.opts.Metadata.Help, nativeMetadata.Attributes["help"])
				}
				actualSamples += len(item.Timestamps)
			}
			require.Equal(t, len(captured.records), actualSamples)
		})
	}
}

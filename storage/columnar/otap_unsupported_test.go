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
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/prometheus/prometheus/storage/columnar"
	otlpprw "github.com/prometheus/prometheus/storage/remote/otlptranslator/prometheusremotewrite"
)

// otapWALDirName is where the columnar storage keeps its segment log below the
// storage directory. It is spelled out here because the API offers no accessor and
// a rejected batch must leave no segment behind.
const otapWALDirName = "wal"

// otapUnsupportedTestSettings enables the compatibility features a rejection must
// not publish either: target_info samples and verbatim UTF-8 names.
func otapUnsupportedTestSettings() otlpprw.Settings {
	return otlpprw.Settings{AllowUTF8: true}
}

// otapTestScope returns a fresh batch whose single Resource and Scope carry the
// attributes the native path turns into labels and metadata.
func otapTestScope() (pmetric.Metrics, pmetric.ScopeMetrics) {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "checkout")
	rm.Resource().Attributes().PutStr("service.instance.id", "pod-a")
	rm.Resource().Attributes().PutStr("host.name", "host-1")
	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("otap.rejections")
	return md, sm
}

// appendOTAPHistogram adds one cumulative classic histogram with one data point.
func appendOTAPHistogram(sm pmetric.ScopeMetrics, name string, timestamp pcommon.Timestamp) {
	metric := sm.Metrics().AppendEmpty()
	metric.SetName(name)
	histogram := metric.SetEmptyHistogram()
	histogram.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	dp := histogram.DataPoints().AppendEmpty()
	dp.SetTimestamp(timestamp)
	dp.SetCount(3)
	dp.SetSum(6)
	dp.BucketCounts().FromRaw([]uint64{1, 2})
	dp.ExplicitBounds().FromRaw([]float64{1})
}

// appendOTAPSummary adds one summary with one data point.
func appendOTAPSummary(sm pmetric.ScopeMetrics, name string, timestamp pcommon.Timestamp) {
	metric := sm.Metrics().AppendEmpty()
	metric.SetName(name)
	summary := metric.SetEmptySummary()
	dp := summary.DataPoints().AppendEmpty()
	dp.SetTimestamp(timestamp)
	dp.SetCount(2)
	dp.SetSum(3)
	quantile := dp.QuantileValues().AppendEmpty()
	quantile.SetQuantile(0.5)
	quantile.SetValue(1.5)
}

// appendOTAPExponentialHistogram adds one cumulative exponential histogram with one
// data point.
func appendOTAPExponentialHistogram(sm pmetric.ScopeMetrics, name string, timestamp pcommon.Timestamp) {
	metric := sm.Metrics().AppendEmpty()
	metric.SetName(name)
	histogram := metric.SetEmptyExponentialHistogram()
	histogram.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	dp := histogram.DataPoints().AppendEmpty()
	dp.SetTimestamp(timestamp)
	dp.SetCount(2)
	dp.SetSum(3)
	dp.SetScale(1)
	dp.Positive().SetOffset(0)
	dp.Positive().BucketCounts().FromRaw([]uint64{1, 1})
}

// appendOTAPGauge adds one gauge with the given double-valued points. Each value
// lands one millisecond after the previous one, so a multi-value call produces
// distinct samples on one series.
func appendOTAPGauge(sm pmetric.ScopeMetrics, name string, timestamp pcommon.Timestamp, values ...float64) pmetric.NumberDataPointSlice {
	metric := sm.Metrics().AppendEmpty()
	metric.SetName(name)
	points := metric.SetEmptyGauge().DataPoints()
	for i, value := range values {
		dp := points.AppendEmpty()
		dp.SetTimestamp(timestamp + pcommon.Timestamp(i)*1_000_000)
		dp.SetDoubleValue(value)
	}
	return points
}

// otapMetadataSeries returns the complete native metadata state, per kind, so a
// rejected batch can be shown to publish no association, membership or observed
// bound alongside its samples.
func otapMetadataSeries(t *testing.T, s *columnar.Storage) map[string]columnar.MetadataSeriesPage {
	t.Helper()

	pages := map[string]columnar.MetadataSeriesPage{}
	for kind := columnar.MetadataResource; kind <= columnar.MetadataSource; kind++ {
		page, err := s.QueryMetadataSeries(t.Context(), columnar.MetadataSeriesQuery{
			Kind: kind, Start: 0, End: 2_000_000_000_000,
		})
		require.NoError(t, err)
		pages[kind.String()] = page
	}
	return pages
}

// otapWALSegments describes the published WAL segments by name and size.
func otapWALSegments(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(dir, otapWALDirName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	segments := make([]string, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		require.NoError(t, err)
		segments = append(segments, fmt.Sprintf("%s:%d", entry.Name(), info.Size()))
	}
	return segments
}

// TestOTAPSinkRejectsUnsupportedPayloads proves the sink rejects a batch as a whole
// whenever it carries data the number path cannot convert. Every case asserts the
// three things a silent drop would violate: the error names the payload or the row,
// nothing was appended, and nothing reached the store or the WAL, not even the
// supported prefix of a mixed batch.
func TestOTAPSinkRejectsUnsupportedPayloads(t *testing.T) {
	for _, tc := range []struct {
		name    string
		metrics func() pmetric.Metrics
		want    string
	}{
		{
			name: "classic histogram",
			metrics: func() pmetric.Metrics {
				md, sm := otapTestScope()
				appendOTAPHistogram(sm, "request_duration_seconds", 1_700_000_000_000_000_000)
				return md
			},
			want: "classic histogram data points (1 row)",
		},
		{
			name: "summary",
			metrics: func() pmetric.Metrics {
				md, sm := otapTestScope()
				appendOTAPSummary(sm, "rpc_duration_seconds", 1_700_000_000_000_000_000)
				return md
			},
			want: "summary data points (1 row)",
		},
		{
			name: "summary and classic histogram",
			metrics: func() pmetric.Metrics {
				md, sm := otapTestScope()
				appendOTAPHistogram(sm, "request_duration_seconds", 1_700_000_000_000_000_000)
				appendOTAPSummary(sm, "rpc_duration_seconds", 1_700_000_000_000_000_000)
				return md
			},
			// Payload types are listed in payload-type order, not in record order, so
			// the same input cannot produce two different messages.
			want: "summary data points (1 row), classic histogram data points (1 row)",
		},
		{
			name: "exponential histogram",
			metrics: func() pmetric.Metrics {
				md, sm := otapTestScope()
				appendOTAPExponentialHistogram(sm, "request_size_bytes", 1_700_000_000_000_000_000)
				return md
			},
			want: "exponential histogram data points (1 row)",
		},
		{
			name: "supported numbers and a classic histogram",
			metrics: func() pmetric.Metrics {
				md, sm := otapTestScope()
				appendOTAPGauge(sm, "queue_depth", 1_700_000_000_000_000_000, 1, 2)
				appendOTAPHistogram(sm, "request_duration_seconds", 1_700_000_000_000_000_000)
				return md
			},
			want: "classic histogram data points (1 row)",
		},
		{
			name: "supported number carrying an exemplar",
			metrics: func() pmetric.Metrics {
				md, sm := otapTestScope()
				points := appendOTAPGauge(sm, "queue_depth", 1_700_000_000_000_000_000, 1)
				exemplar := points.At(0).Exemplars().AppendEmpty()
				exemplar.SetTimestamp(1_699_999_999_000_000_000)
				exemplar.SetDoubleValue(0.5)
				return md
			},
			want: "number data point exemplars (1 row)",
		},
		{
			name: "number data point of an unnamed metric",
			metrics: func() pmetric.Metrics {
				md, sm := otapTestScope()
				appendOTAPGauge(sm, "", 1_700_000_000_000_000_000, 1)
				return md
			},
			want: "which the sink does not convert",
		},
		{
			name: "number data point without a value",
			metrics: func() pmetric.Metrics {
				md, sm := otapTestScope()
				metric := sm.Metrics().AppendEmpty()
				metric.SetName("queue_depth")
				metric.SetEmptyGauge().DataPoints().AppendEmpty().
					SetTimestamp(1_700_000_000_000_000_000)
				return md
			},
			want: `number data point row 0 of metric "queue_depth" carries neither an integer nor a double value`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := columnar.NewStorage(dir, columnar.StorageOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			settings := otapUnsupportedTestSettings()
			sink := columnar.NewOTAPSink(s, slog.New(slog.DiscardHandler), func() otlpprw.Settings { return settings })

			err = sink.ConsumeRecords(t.Context(), recordsFor(t, tc.metrics()))
			require.ErrorIs(t, err, columnar.ErrUnsupportedPayload)
			require.ErrorContains(t, err, tc.want)

			series, err := s.SelectSeries(t.Context(), 0, 2_000_000_000_000, nil, 0)
			require.NoError(t, err)
			require.Empty(t, series, "a rejected batch must not append a supported prefix, a target_info sample or any other series")
			require.Zero(t, s.ReadStats().CommitCount, "a rejected batch must not reach a store transaction")
			require.Empty(t, otapWALSegments(t, dir), "a rejected batch must not publish a WAL segment")
			for kind, page := range otapMetadataSeries(t, s) {
				require.Empty(t, page.Series, "a rejected batch must not commit %s metadata", kind)
			}
		})
	}
}

// TestOTAPSinkAcceptsEmptyBatches pins the boundary between an input that carries
// nothing and an input that carries something unconvertible: the first stays a
// successful no-op, the second is rejected. Conflating the two would either reject
// legitimate empty batches or acknowledge dropped data.
func TestOTAPSinkAcceptsEmptyBatches(t *testing.T) {
	for _, tc := range []struct {
		name    string
		metrics func() pmetric.Metrics
	}{
		{
			name:    "no Resource at all",
			metrics: pmetric.NewMetrics,
		},
		{
			name: "metric without data points",
			metrics: func() pmetric.Metrics {
				md, sm := otapTestScope()
				metric := sm.Metrics().AppendEmpty()
				metric.SetName("queue_depth")
				metric.SetEmptyGauge()
				return md
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := columnar.NewStorage(dir, columnar.StorageOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			settings := otapUnsupportedTestSettings()
			sink := columnar.NewOTAPSink(s, slog.New(slog.DiscardHandler), func() otlpprw.Settings { return settings })

			require.NoError(t, sink.ConsumeRecords(t.Context(), recordsFor(t, tc.metrics())))

			series, err := s.SelectSeries(t.Context(), 0, 2_000_000_000_000, nil, 0)
			require.NoError(t, err)
			require.Empty(t, series)
			require.Zero(t, s.ReadStats().CommitCount)
			require.Empty(t, otapWALSegments(t, dir))
		})
	}
}

// TestOTAPSinkRejectionLeavesPopulatedStoreUntouched rejects a batch over a store
// that already holds samples and metadata, and compares the complete observable
// state. A rejection that still committed a prefix, bumped an observation bound,
// extended a membership or published a WAL segment would surface as a difference in
// one of these snapshots.
func TestOTAPSinkRejectionLeavesPopulatedStoreUntouched(t *testing.T) {
	dir := t.TempDir()
	s, err := columnar.NewStorage(dir, columnar.StorageOptions{FlushSamples: 1000})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	settings := otapUnsupportedTestSettings()
	sink := columnar.NewOTAPSink(s, slog.New(slog.DiscardHandler), func() otlpprw.Settings { return settings })

	accepted := func() pmetric.Metrics {
		md, sm := otapTestScope()
		appendOTAPGauge(sm, "queue_depth", 1_700_000_000_000_000_000, 1, 2)
		sum := sm.Metrics().AppendEmpty()
		sum.SetName("requests_total")
		counter := sum.SetEmptySum()
		counter.SetIsMonotonic(true)
		counter.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		counter.DataPoints().AppendEmpty().SetIntValue(7)
		counter.DataPoints().At(0).SetTimestamp(1_700_000_000_000_000_000)
		return md
	}
	require.NoError(t, sink.ConsumeRecords(t.Context(), recordsFor(t, accepted())))

	stored := func() []columnar.Series {
		series, err := s.SelectSeries(t.Context(), 0, 2_000_000_000_000, nil, 0)
		require.NoError(t, err)
		return series
	}
	beforeSeries := stored()
	require.Len(t, beforeSeries, 3, "two queue_depth points on one series, one counter series and target_info")
	beforeMetadata := otapMetadataSeries(t, s)
	beforeSegments := otapWALSegments(t, dir)
	beforeCommits := s.ReadStats().CommitCount
	require.NotEmpty(t, beforeSegments, "the accepted batch must have published a WAL segment")
	require.NotEmpty(t, beforeMetadata["resource"].Series, "the accepted batch must have committed Resource metadata")

	// The rejected batch mixes a supported gauge with a classic histogram, so a
	// partial append would be visible as a new queue_depth series with extra points.
	md, sm := otapTestScope()
	appendOTAPGauge(sm, "queue_depth", 1_700_000_001_000_000_000, 3)
	appendOTAPHistogram(sm, "request_duration_seconds", 1_700_000_001_000_000_000)
	points := appendOTAPGauge(sm, "exemplar_gauge", 1_700_000_001_000_000_000, 4)
	exemplar := points.At(0).Exemplars().AppendEmpty()
	exemplar.SetTimestamp(1_700_000_000_500_000_000)
	exemplar.SetDoubleValue(0.5)

	err = sink.ConsumeRecords(t.Context(), recordsFor(t, md))
	require.ErrorIs(t, err, columnar.ErrUnsupportedPayload)
	require.ErrorContains(t, err, "classic histogram data points (1 row)")

	require.Equal(t, beforeSeries, stored(), "existing samples must be unchanged and no new series added")
	require.Equal(t, beforeMetadata, otapMetadataSeries(t, s), "existing metadata histories, memberships and observed bounds must be unchanged")
	require.Equal(t, beforeSegments, otapWALSegments(t, dir), "a rejected batch must not publish a WAL segment")
	require.Equal(t, beforeCommits, s.ReadStats().CommitCount, "a rejected batch must not reach a store transaction")
}

// TestOTAPSinkRollsBackWithinOneMixedBatch rejects a batch whose valid points are
// followed by an unsupported payload in record order, so the failure happens after
// the builder has already collected samples. The transaction-local batch those
// samples belong to must never be published.
func TestOTAPSinkRollsBackWithinOneMixedBatch(t *testing.T) {
	dir := t.TempDir()
	s, err := columnar.NewStorage(dir, columnar.StorageOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	sink := columnar.NewOTAPSink(s, slog.New(slog.DiscardHandler))

	md, sm := otapTestScope()
	for i := range 5 {
		appendOTAPGauge(sm, fmt.Sprintf("queue_depth_%d", i), 1_700_000_000_000_000_000, float64(i))
	}
	// A data point row without a value fails while the builder is converting, after
	// the five supported series above are already in the transaction-local batch.
	broken := sm.Metrics().AppendEmpty()
	broken.SetName("broken_gauge")
	broken.SetEmptyGauge().DataPoints().AppendEmpty().SetTimestamp(1_700_000_000_000_000_000)

	err = sink.ConsumeRecords(t.Context(), recordsFor(t, md))
	require.ErrorIs(t, err, columnar.ErrUnsupportedPayload)

	series, err := s.SelectSeries(t.Context(), 0, 2_000_000_000_000, nil, 0)
	require.NoError(t, err)
	require.Empty(t, series, "samples collected before the failing row must not be published")
	require.Zero(t, s.ReadStats().CommitCount)
	require.Empty(t, otapWALSegments(t, dir))
}

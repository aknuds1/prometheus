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

// The ordinary OTLP route's complete logical state: samples, translation output and
// the Metric metadata the columnar appender derives from AOptions.
//
// The control is stock TSDB through the same consumer, so the conventional label
// sets, values and target_info samples are decided by the shared translator rather
// than by the storage under test. Metric metadata is asserted against expectations
// built directly from the OTLP fixture, which is the only way to check content a
// conventional TSDB head does not expose.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/storage/remote"
)

// ordinaryTimestamps are the two sample times of the first ordinary commit and the
// two of the second, which states changed metadata for part of the fixture.
var (
	ordinaryFirst  = []int64{1_000_000, 1_030_000}
	ordinarySecond = []int64{1_060_000, 1_090_000}
)

// ordinaryResourceAttrs returns the descriptive Resource attributes of resource r.
func ordinaryResourceAttrs(r int) map[string]string {
	if r == 0 {
		return map[string]string{
			"service.name":           "shop",
			"service.instance.id":    "i-1",
			"deployment.environment": "prod",
		}
	}
	return map[string]string{
		"service.name":        "shop",
		"service.instance.id": "i-2",
	}
}

// ordinaryGaugeHelp is the gauge description of the first commit; the second commit
// states ordinaryGaugeHelpV2 for resource 0, which is a metadata version change that
// must not rewrite the first version's observation bounds.
const (
	ordinaryGaugeHelp   = "gauge help"
	ordinaryGaugeHelpV2 = "gauge help v2"
)

// ordinaryMetrics builds one ordinary commit of the fixture. change states the second
// commit: the gauge help changes, one gauge data point carries no recorded value, and
// every series states a later timestamp.
func ordinaryMetrics(change bool) pmetric.Metrics {
	md := pmetric.NewMetrics()
	times := ordinaryFirst
	if change {
		times = ordinarySecond
	}
	for r := range 2 {
		rm := md.ResourceMetrics().AppendEmpty()
		attrs := ordinaryResourceAttrs(r)
		for name, v := range attrs {
			rm.Resource().Attributes().PutStr(name, v)
		}
		sm := rm.ScopeMetrics().AppendEmpty()
		sm.Scope().SetName("ord.lib")
		sm.Scope().SetVersion("1.2.3")
		sm.SetSchemaUrl("scope/v1")

		gaugeHelp := ordinaryGaugeHelp
		if change && r == 0 {
			gaugeHelp = ordinaryGaugeHelpV2
		}
		gauge := sm.Metrics().AppendEmpty()
		gauge.SetName("ord_gauge")
		gauge.SetUnit("By")
		gauge.SetDescription(gaugeHelp)
		gaugePoints := gauge.SetEmptyGauge().DataPoints()
		for i, route := range ordinaryRoutes() {
			for j, ts := range times {
				dp := gaugePoints.AppendEmpty()
				dp.SetTimestamp(pcommon.Timestamp(ts * int64(time.Millisecond)))
				dp.SetDoubleValue(ordinaryValue(r, i, j, change))
				dp.Attributes().PutStr("route", route)
				if change && r == 0 && i == 1 && j == 0 {
					// A point the exporter marks as carrying no recorded value becomes
					// the stale marker sample, not a synthetic zero.
					dp.SetFlags(dp.Flags().WithNoRecordedValue(true))
					dp.SetDoubleValue(math.Float64frombits(value.StaleNaN))
				}
			}
		}

		sum := sm.Metrics().AppendEmpty()
		sum.SetName("ord_total")
		sum.SetUnit("1")
		sum.SetDescription("total help")
		summer := sum.SetEmptySum()
		summer.SetIsMonotonic(true)
		summer.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		sumPoints := summer.DataPoints()
		for i, route := range ordinaryRoutes() {
			for j, ts := range times {
				dp := sumPoints.AppendEmpty()
				dp.SetTimestamp(pcommon.Timestamp(ts * int64(time.Millisecond)))
				dp.SetIntValue(int64(100 * (1 + i) * (1 + j) * (1 + r)))
				dp.Attributes().PutStr("route", route)
			}
		}

		hist := sm.Metrics().AppendEmpty()
		hist.SetName("ord_hist")
		hist.SetUnit("s")
		hist.SetDescription("hist help")
		histogram := hist.SetEmptyHistogram()
		histogram.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		for j, ts := range times {
			dp := histogram.DataPoints().AppendEmpty()
			dp.SetTimestamp(pcommon.Timestamp(ts * int64(time.Millisecond)))
			dp.SetCount(4)
			dp.SetSum(3.5 + float64(j))
			dp.ExplicitBounds().FromRaw([]float64{0.5, 1, 2})
			dp.BucketCounts().FromRaw([]uint64{1, 1, 1, 1})
			dp.Attributes().PutStr("route", "/a")
		}

		summary := sm.Metrics().AppendEmpty()
		summary.SetName("ord_summary")
		summary.SetDescription("summary help")
		summaryData := summary.SetEmptySummary()
		for _, ts := range times {
			dp := summaryData.DataPoints().AppendEmpty()
			dp.SetTimestamp(pcommon.Timestamp(ts * int64(time.Millisecond)))
			dp.SetCount(2)
			dp.SetSum(9)
			q := dp.QuantileValues().AppendEmpty()
			q.SetQuantile(0.5)
			q.SetValue(1)
			q = dp.QuantileValues().AppendEmpty()
			q.SetQuantile(0.9)
			q.SetValue(8)
			dp.Attributes().PutStr("route", "/a")
		}
	}
	return md
}

// ordinaryRoutes are the point attribute values of the fixture.
func ordinaryRoutes() []string { return []string{"/a", "/b"} }

// ordinaryValue returns the gauge value of resource r, route i, point j.
func ordinaryValue(r, i, j int, change bool) float64 {
	v := float64(1+r)*100 + float64(i)*10 + float64(j)
	if change {
		v += 1000
	}
	return v
}

// ordinaryConsumer returns the shared ordinary consumer the deployed receiver
// builds, unchanged, over the given appendable.
func ordinaryConsumer(t *testing.T, appendable storage.AppendableV2) interface {
	ConsumeMetrics(context.Context, pmetric.Metrics) error
} {
	t.Helper()
	return remote.NewMetricsConsumer(
		slog.New(slog.DiscardHandler), prometheus.NewRegistry(), appendable,
		func() config.Config { return config.Config{OTLPConfig: config.DefaultOTLPConfig} },
		remote.OTLPOptions{LookbackDelta: 5 * time.Minute},
	)
}

// ordinaryColumnar opens the subject storage with its WAL enabled and a flush
// threshold above the fixture, so the head, the WAL and an explicit flush are all
// exercised by the same corpus.
func ordinaryColumnar(t *testing.T, dir string) *columnar.Storage {
	t.Helper()
	st, err := columnar.NewStorage(dir, columnar.StorageOptions{FlushSamples: 1_000_000})
	require.NoError(t, err)
	return st
}

// ordinaryCompleteSamples renders every stored sample of both engines: complete
// labels, timestamp, exact value bits, sample kind and any target_info.
func ordinaryCompleteSamples(t *testing.T, q storage.Queryable, mint, maxt int64) []string {
	t.Helper()
	return canonicalStored(t, q, mint, maxt, false)
}

// ordinaryStateDigest is the SHA-256 of a complete state dump. It is logged rather
// than asserted, so two builds of the same corpus can be compared exactly without
// pinning a weak hash as the oracle.
func ordinaryStateDigest(lines ...[]string) string {
	var flat []string
	for _, l := range lines {
		flat = append(flat, l...)
	}
	sum := sha256.Sum256([]byte(strings.Join(flat, "\n")))
	return hex.EncodeToString(sum[:])
}

// ordinaryMetricOwners returns the Metric owner histories of every series the
// fixture is expected to produce, rendered completely.
func ordinaryMetricOwners(t *testing.T, s *columnar.Storage, series []labels.Labels) []string {
	t.Helper()
	var out []string
	for _, ls := range series {
		owner := columnar.MetricOwner(ls)
		history, ok := s.MetadataHistory(owner)
		require.True(t, ok, "every fixture series states OTLP metric metadata, so its Metric owner must exist: %s", ls)
		var b strings.Builder
		fmt.Fprintf(&b, "owner=%s kind=%s truncated=%t", owner.String(), owner.Kind, history.Truncated)
		for _, version := range history.Versions {
			fmt.Fprintf(&b, " version{effective=%d observed=%d..%d content=%s}",
				version.EffectiveFrom, version.ObservedFrom, version.ObservedUntil,
				canonicalContent(version.Content.Attributes))
		}
		out = append(out, b.String())
	}
	slices.Sort(out)
	return out
}

// canonicalContent renders content attributes in a stable order.
func canonicalContent(attrs map[string]string) string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%q,", k, attrs[k])
	}
	return b.String()
}

// ordinarySeriesLabels returns the complete label sets the fixture's ordinary
// translation produces, written out rather than discovered, so the metadata oracle
// does not depend on the storage being checked.
func ordinarySeriesLabels() []labels.Labels {
	var out []labels.Labels
	for r := range 2 {
		// With the default OTLP settings only the identifying Resource attributes become
		// labels; descriptive ones reach the series through target_info, not directly.
		base := map[string]string{
			"job":      "shop",
			"instance": ordinaryResourceAttrs(r)["service.instance.id"],
		}
		with := func(name string, extra map[string]string) labels.Labels {
			l := map[string]string{labels.MetricName: name}
			for k, v := range base {
				l[k] = v
			}
			for k, v := range extra {
				l[k] = v
			}
			return labels.FromMap(l)
		}
		for _, route := range ordinaryRoutes() {
			out = append(out,
				with("ord_gauge_bytes", map[string]string{"route": route}),
				with("ord_total", map[string]string{"route": route}),
			)
		}
		for _, le := range []string{"0.5", "1", "2", "+Inf"} {
			out = append(out, with("ord_hist_seconds_bucket", map[string]string{"le": le, "route": "/a"}))
		}
		out = append(out,
			with("ord_hist_seconds_sum", map[string]string{"route": "/a"}),
			with("ord_hist_seconds_count", map[string]string{"route": "/a"}),
		)
		for _, q := range []string{"0.5", "0.9"} {
			out = append(out, with("ord_summary", map[string]string{"quantile": q, "route": "/a"}))
		}
		out = append(out,
			with("ord_summary_sum", map[string]string{"route": "/a"}),
			with("ord_summary_count", map[string]string{"route": "/a"}),
		)
	}
	return out
}

// TestOrdinaryOTLPLogicalState is the complete oracle of the ordinary route: both
// engines must store the identical conventional result, the columnar head must carry
// exactly the Metric metadata the OTLP fixture states, and the state must survive a
// flush and a reopen byte for byte.
func TestOrdinaryOTLPLogicalState(t *testing.T) {
	dir := t.TempDir()
	col := ordinaryColumnar(t, dir)
	tsdbControl := openPrimaryTSDB(t)

	colConsumer := ordinaryConsumer(t, col)
	tsdbConsumer := ordinaryConsumer(t, tsdbControl)
	for i, md := range []pmetric.Metrics{ordinaryMetrics(false), ordinaryMetrics(true)} {
		require.NoError(t, colConsumer.ConsumeMetrics(context.Background(), md), "ordinary commit %d", i)
		require.NoError(t, tsdbConsumer.ConsumeMetrics(context.Background(), md), "ordinary control commit %d", i)
	}

	mint, maxt := ordinaryFirst[0], ordinarySecond[len(ordinarySecond)-1]
	colSamples := ordinaryCompleteSamples(t, col, mint, maxt)
	tsdbSamples := ordinaryCompleteSamples(t, tsdbControl, mint, maxt)
	require.Equal(t, tsdbSamples, colSamples,
		"the columnar ordinary route stores exactly the conventional result of the shared translator")
	require.NotEmpty(t, colSamples)

	// The translation output itself is pinned, so a later change to series selection,
	// suffixes or target_info shows up here rather than as a silently smaller oracle.
	names := map[string]int{}
	sampleCounts := map[string]int{}
	for _, line := range colSamples {
		name := canonicalSeriesName(t, line)
		names[name]++
		sampleCounts[name] += canonicalSampleCount(t, line)
	}
	require.Equal(t, map[string]int{
		"ord_gauge_bytes":         2 * 2, // 2 resources x 2 routes
		"ord_total":               2 * 2, // 2 resources x 2 routes
		"ord_hist_seconds_bucket": 2 * 4, // 2 resources x (3 explicit bounds + Inf)
		"ord_hist_seconds_sum":    2,
		"ord_hist_seconds_count":  2,
		"ord_summary":             2 * 2, // 2 resources x 2 quantiles
		"ord_summary_sum":         2,
		"ord_summary_count":       2,
		"target_info":             1, // only resource 0 has a descriptive attribute
	}, names)
	require.Equal(t, 4*4, sampleCounts["ord_gauge_bytes"], "4 gauge series, two points from each commit")
	require.Equal(t, 2*4, sampleCounts["ord_hist_seconds_sum"], "2 histogram series, two points from each commit")
	require.Equal(t, 4, sampleCounts["target_info"],
		"only resource 0 produces target_info: two points from each commit, because the fixture spans more than one lookback interval step")

	// Exactly the fixture's series and the translator's target_info exist: no
	// synthetic metadata sample was invented by the storage.
	series := ordinarySeriesLabels()
	require.Len(t, series, 28)
	for _, ls := range series {
		found := false
		for _, line := range colSamples {
			if strings.HasPrefix(line, ls.String()+" ") {
				found = true
				break
			}
		}
		require.True(t, found, "expected series %s is missing from the ordinary route", ls)
	}

	// The Metric metadata of every fixture series is exactly the OTLP type, unit and
	// help, with the observation bounds of the commits that stated it. Resource 0's
	// gauge help changes in the second commit, which begins a second version instead
	// of rewriting the first one; every other series re-observes unchanged content and
	// only advances its bound.
	type expectedVersion struct {
		effectiveFrom, observedFrom, observedUntil int64
		content                                    map[string]string
	}
	want := map[string][]expectedVersion{}
	for _, ls := range series {
		name := ls.Get(labels.MetricName)
		content := map[string]string{"type": "histogram", "unit": "seconds", "help": "hist help"}
		switch {
		case name == "ord_gauge_bytes":
			content = map[string]string{"type": "gauge", "unit": "bytes", "help": ordinaryGaugeHelp}
			if ls.Get("instance") == "i-1" {
				want[ls.String()] = []expectedVersion{
					{1_000_000, 1_000_000, 1_030_000, content},
					{1_060_000, 1_060_000, 1_090_000, map[string]string{
						"type": "gauge", "unit": "bytes", "help": ordinaryGaugeHelpV2,
					}},
				}
				continue
			}
		case name == "ord_total":
			content = map[string]string{"type": "counter", "unit": "", "help": "total help"}
		case strings.HasPrefix(name, "ord_summary"):
			content = map[string]string{"type": "summary", "unit": "", "help": "summary help"}
		}
		want[ls.String()] = []expectedVersion{{1_000_000, 1_000_000, 1_090_000, content}}
	}
	require.Len(t, want, len(series))

	for _, ls := range series {
		for _, kind := range []columnar.MetadataKind{
			columnar.MetadataResource, columnar.MetadataResourceSchema,
			columnar.MetadataScope, columnar.MetadataScrape, columnar.MetadataSource,
		} {
			_, ok, _ := col.MetadataAtLabels(ls, kind, ordinarySecond[len(ordinarySecond)-1])
			require.False(t, ok, "ordinary series %s must not have %s metadata", ls, kind)
		}
		// The point query resolves through the version valid at the requested instant.
		for _, version := range want[ls.String()] {
			content, ok, truncated := col.MetadataAtLabels(ls, columnar.MetadataMetric, version.effectiveFrom)
			require.True(t, ok, "series %s must have Metric metadata at %d", ls, version.effectiveFrom)
			require.False(t, truncated)
			require.Equal(t, version.content, content.Attributes, "series %s at %d", ls, version.effectiveFrom)
		}
	}

	// The owner history carries the same versions and bounds, which is the state a
	// reuse mechanism must not coarsen.
	for _, ls := range series {
		history, ok := col.MetadataHistory(columnar.MetricOwner(ls))
		require.True(t, ok, "series %s must have a Metric owner", ls)
		require.False(t, history.Truncated)
		wantVersions := want[ls.String()]
		require.Len(t, history.Versions, len(wantVersions), "series %s", ls)
		for i, version := range history.Versions {
			require.Equal(t, wantVersions[i].effectiveFrom, version.EffectiveFrom, "series %s version %d", ls, i)
			require.Equal(t, wantVersions[i].observedFrom, version.ObservedFrom, "series %s version %d", ls, i)
			require.Equal(t, wantVersions[i].observedUntil, version.ObservedUntil, "series %s version %d", ls, i)
			require.Equal(t, wantVersions[i].content, version.Content.Attributes, "series %s version %d", ls, i)
		}
	}

	// Before the first observation no Metric value exists: absence is absence, not a
	// zero-valued version.
	for _, ls := range series {
		_, ok, _ := col.MetadataAtLabels(ls, columnar.MetadataMetric, ordinaryFirst[0]-1)
		require.False(t, ok, "series %s must have no Metric metadata before its first observation", ls)
	}

	owners := ordinaryMetricOwners(t, col, series)
	digest := ordinaryStateDigest(colSamples, owners)
	t.Logf("ordinary logical state: series=%d samples=%d digest=%s", len(series), len(colSamples), digest)

	// target_info is a conventional translation product with its own Metric owner:
	// the translator passes metadata for it, and the storage records it like any
	// other series instead of inventing a Resource association.
	targetInfo := ordinarySeriesNamed(t, col, "target_info", mint, maxt)
	content, ok, _ := col.MetadataAtLabels(targetInfo, columnar.MetadataMetric, ordinaryFirst[0])
	require.True(t, ok)
	require.Equal(t, "gauge", content.Attributes["type"])
	require.Equal(t, "Target metadata", content.Attributes["help"])
	require.Equal(t, "", content.Attributes["unit"], "the translator states no unit for target_info")

	// An explicit flush then a reopen must reproduce the same complete state.
	require.NoError(t, col.Flush())
	reopen := ordinaryColumnar(t, dir)
	t.Cleanup(func() { require.NoError(t, reopen.Close()) })
	reopened := ordinaryCompleteSamples(t, reopen, mint, maxt)
	require.Equal(t, colSamples, reopened, "flush and reopen preserve every stored sample")
	require.Equal(t, owners, ordinaryMetricOwners(t, reopen, series),
		"flush and reopen preserve every Metric owner version and its observation bounds")
	require.Equal(t, digest, ordinaryStateDigest(reopened, ordinaryMetricOwners(t, reopen, series)))
	require.NoError(t, col.Close())
}

// canonicalSampleCount returns the number of samples a canonicalStored line holds.
func canonicalSampleCount(t *testing.T, line string) int {
	t.Helper()
	i := strings.Index(line, "} ")
	require.GreaterOrEqual(t, i, 0, "canonical line must hold a label set: %s", line)
	return len(strings.Fields(line[i+2:]))
}

// canonicalSeriesName returns the __name__ of a canonicalStored line.
func canonicalSeriesName(t *testing.T, line string) string {
	t.Helper()
	const prefix = `__name__="`
	i := strings.Index(line, prefix)
	require.GreaterOrEqual(t, i, 0, "canonical line must start with a __name__ label: %s", line)
	rest := line[i+len(prefix):]
	return rest[:strings.Index(rest, `"`)]
}

// ordinarySeriesNamed returns the single stored series carrying name.
func ordinarySeriesNamed(t *testing.T, q storage.Queryable, name string, mint, maxt int64) labels.Labels {
	t.Helper()
	querier, err := q.Querier(mint, maxt)
	require.NoError(t, err)
	defer func() { require.NoError(t, querier.Close()) }()
	set := querier.Select(context.Background(), true, nil,
		labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, name))
	var out []labels.Labels
	for set.Next() {
		out = append(out, set.At().Labels())
	}
	require.NoError(t, set.Err())
	require.Len(t, out, 1, "exactly one series carries %s", name)
	return out[0]
}

// TestOrdinaryOTLPMetadataQueries checks the metadata query surface an ordinary
// corpus produces: owner pages, series memberships and the point queries between
// versions.
func TestOrdinaryOTLPMetadataQueries(t *testing.T) {
	col := ordinaryColumnar(t, t.TempDir())
	t.Cleanup(func() { require.NoError(t, col.Close()) })
	consumer := ordinaryConsumer(t, col)
	require.NoError(t, consumer.ConsumeMetrics(context.Background(), ordinaryMetrics(false)))

	series := ordinarySeriesLabels()
	owners, err := col.QueryMetadata(context.Background(), columnar.MetadataQuery{
		Kind: columnar.MetadataMetric, Start: 0, End: ordinarySecond[len(ordinarySecond)-1],
	})
	require.NoError(t, err)
	require.Len(t, owners.Owners, len(series)+1,
		"one Metric owner per translated series, plus the translator's target_info series")
	for _, owner := range owners.Owners {
		require.Equal(t, "metric", owner.Kind)
		require.False(t, owner.HistoryTruncated)
		require.Len(t, owner.Versions, 1, "one unchanged content is one version")
		version := owner.Versions[0]
		require.Equal(t, ordinaryFirst[0], version.EffectiveFrom)
		require.Equal(t, ordinaryFirst[0], version.ObservedFrom)
		require.Equal(t, ordinaryFirst[len(ordinaryFirst)-1], version.ObservedUntil)
		require.Nil(t, version.ValidUntil)
	}

	memberships, err := col.QueryMetadataSeries(context.Background(), columnar.MetadataSeriesQuery{
		Kind: columnar.MetadataMetric, Start: 0, End: ordinarySecond[len(ordinarySecond)-1],
	})
	require.NoError(t, err)
	require.Len(t, memberships.Series, len(series)+1)
	for _, membership := range memberships.Series {
		require.Equal(t, "metric", membership.Kind)
		require.Equal(t, ordinaryFirst[0], membership.MembershipStart)
		require.Equal(t, ordinaryFirst[len(ordinaryFirst)-1], membership.MembershipObservedTo)
		require.Nil(t, membership.MembershipUntil, "a single membership is open-ended")
		require.Len(t, membership.Versions, 1)
	}
}

// TestOrdinaryMetadataPresenceAndContextChanges covers the appender-level ordinary
// cases the OTLP route cannot state: metadata absent for a sample, a context change,
// and a complete-label change on the same reference.
func TestOrdinaryMetadataPresenceAndContextChanges(t *testing.T) {
	s := ordinaryColumnar(t, t.TempDir())
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	ls := labels.FromStrings(labels.MetricName, "presence_metric", "instance", "a")
	other := labels.FromStrings(labels.MetricName, "presence_metric", "instance", "b")
	gauge := metadata.Metadata{Type: model.MetricTypeGauge, Unit: "1", Help: "first help"}
	changed := metadata.Metadata{Type: model.MetricTypeCounter, Unit: "1", Help: "second help"}

	app := s.AppenderV2(context.Background())
	firstRef, err := app.Append(0, ls, 0, 1_000, 1, nil, nil, storage.AOptions{Metadata: gauge})
	require.NoError(t, err)
	// A sample without metadata states no observation: it must not extend a bound or
	// invent a version.
	reused, err := app.Append(firstRef, ls, 0, 2_000, 2, nil, nil, storage.AOptions{})
	require.NoError(t, err)
	require.Equal(t, firstRef, reused)
	// A different complete label set on the same storage is a different owner.
	_, err = app.Append(0, other, 0, 1_000, 3, nil, nil, storage.AOptions{Metadata: gauge})
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	history, ok := s.MetadataHistory(columnar.MetricOwner(ls))
	require.True(t, ok)
	require.Len(t, history.Versions, 1)
	require.Equal(t, int64(1_000), history.Versions[0].EffectiveFrom)
	require.Equal(t, int64(1_000), history.Versions[0].ObservedUntil,
		"the sample without metadata is not an observation")
	require.Equal(t, "first help", history.Versions[0].Content.Attributes["help"])

	// The next commit changes the metadata context at a later time. The new version
	// must not be applied retroactively, and the first version keeps its bounds.
	app = s.AppenderV2(context.Background())
	_, err = app.Append(firstRef, ls, 0, 3_000, 4, nil, nil, storage.AOptions{Metadata: changed})
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	history, ok = s.MetadataHistory(columnar.MetricOwner(ls))
	require.True(t, ok)
	require.Len(t, history.Versions, 2)
	require.Equal(t, int64(1_000), history.Versions[0].EffectiveFrom)
	require.Equal(t, int64(1_000), history.Versions[0].ObservedFrom)
	require.Equal(t, int64(1_000), history.Versions[0].ObservedUntil)
	require.Equal(t, int64(3_000), history.Versions[1].EffectiveFrom)
	require.Equal(t, "second help", history.Versions[1].Content.Attributes["help"])

	content, ok, _ := s.MetadataAtLabels(ls, columnar.MetadataMetric, 2_999)
	require.True(t, ok)
	require.Equal(t, "first help", content.Attributes["help"], "a later version is not applied retroactively")
	content, ok, _ = s.MetadataAtLabels(ls, columnar.MetadataMetric, 3_000)
	require.True(t, ok)
	require.Equal(t, "second help", content.Attributes["help"])

	// A stale reference for a different series falls back to the complete labels and
	// must not move the sample to the referenced series' owner.
	app = s.AppenderV2(context.Background())
	staleRef := firstRef + 1_000
	_, err = app.Append(staleRef, other, 0, 2_000, 5, nil, nil, storage.AOptions{Metadata: changed})
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	otherHistory, ok := s.MetadataHistory(columnar.MetricOwner(other))
	require.True(t, ok)
	require.Len(t, otherHistory.Versions, 2)
	require.Equal(t, "first help", otherHistory.Versions[0].Content.Attributes["help"])
	require.Equal(t, "second help", otherHistory.Versions[1].Content.Attributes["help"])
	require.Equal(t, uint64(2), s.ReadStats().RefTrusted,
		"the reference the storage returned is trusted while it verifies: once inside one transaction and once against the committed tail")
	require.Equal(t, uint64(3), s.ReadStats().RefFallback,
		"the two zero references and the stale reference fall back to hashing the complete labels")
}

// TestOrdinaryOTLPInfoFallback checks the complete info() behaviour the ordinary
// route supports: an ordinary series carries no Resource owner, so info() enriches it
// from the translator's target_info sample rather than from invented OTLP Resource
// metadata.
func TestOrdinaryOTLPInfoFallback(t *testing.T) {
	col := ordinaryColumnar(t, t.TempDir())
	t.Cleanup(func() { require.NoError(t, col.Close()) })
	consumer := ordinaryConsumer(t, col)
	require.NoError(t, consumer.ConsumeMetrics(context.Background(), ordinaryMetrics(false)))

	engine := promqltest.NewTestEngine(t, false, 5*time.Minute, promqltest.DefaultMaxSamplesPerQuery)
	info := queryInfoLabels(t, engine, col, `info(ord_gauge_bytes)`, ordinaryFirst[0])
	require.Len(t, info, 4, "every gauge series is joined with target_info")

	byInstance := map[string]labels.Labels{}
	for _, ls := range info {
		byInstance[ls.Get("instance")] = ls
	}
	require.Equal(t, "prod", byInstance["i-1"].Get("deployment_environment"),
		"resource 0's descriptive attribute reaches its series through target_info")
	require.Empty(t, byInstance["i-2"].Get("deployment_environment"),
		"resource 1 states no descriptive attribute, so nothing is invented for it")
	require.Equal(t, "shop", byInstance["i-1"].Get("job"))
}

// TestOrdinaryMetadataRollbackAndRejection proves the ordinary path publishes nothing
// when a transaction is rolled back or rejected, including the metadata it staged.
func TestOrdinaryMetadataRollbackAndRejection(t *testing.T) {
	s := ordinaryColumnar(t, t.TempDir())
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ls := labels.FromStrings(labels.MetricName, "rollback_metric")
	gauge := metadata.Metadata{Type: model.MetricTypeGauge, Unit: "1", Help: "rollback help"}

	app := s.AppenderV2(context.Background())
	_, err := app.Append(0, ls, 0, 1_000, 1, nil, nil, storage.AOptions{Metadata: gauge})
	require.NoError(t, err)
	require.NoError(t, app.Rollback())

	_, ok := s.MetadataHistory(columnar.MetricOwner(ls))
	require.False(t, ok, "a rolled back transaction must record no owner history")
	series, samples := countStored(t, s, 0, 10_000, "")
	require.Zero(t, series)
	require.Zero(t, samples)

	// The same-time conflicting content observation is rejected as a whole: the sample
	// it arrived with must not be committed either.
	err = s.AppendBatch(context.Background(), columnar.Batch{
		Series: []columnar.BatchSeries{{
			Labels:     ls,
			Timestamps: []int64{1_000},
			Kinds:      []columnar.SampleKind{columnar.SampleFloat},
			Values:     []float64{1},
		}},
		Updates: []columnar.AssociationUpdate{
			{Labels: ls, Update: columnar.Update{
				Owner: columnar.MetricOwner(ls), EffectiveFrom: 1_000,
				Content: columnar.Content{Attributes: map[string]string{"type": "gauge", "help": "a"}},
			}},
			{Labels: ls, Update: columnar.Update{
				Owner: columnar.MetricOwner(ls), EffectiveFrom: 1_000,
				Content: columnar.Content{Attributes: map[string]string{"type": "gauge", "help": "b"}},
			}},
		},
	})
	require.ErrorIs(t, err, columnar.ErrConflictingMetadata)
	_, ok = s.MetadataHistory(columnar.MetricOwner(ls))
	require.False(t, ok, "a rejected batch must record no owner history")
	series, samples = countStored(t, s, 0, 10_000, "")
	require.Zero(t, series, "a rejected batch must commit no sample")
	require.Zero(t, samples)
}

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

// Decision 5 requires provenance observed at ingestion to record actual receipt
// coverage, never validity or receipt at an earlier metric sample time, and it
// gives each observation a typed provenance record rather than one opaque
// store-global string.
//
// The native OTAP sink currently does neither: it associates the single owner
// SourceOwner("otap") with {"transport": "otap"} for every series of every
// batch, and it passes each sample's timestamp as the observation's
// EffectiveFrom. The store then writes the version and membership observation
// bounds equal to that effective time. Transport accepted now is therefore
// reported as valid and observed at the historical sample instant, and every
// producer behind every collector shares one owner.
//
// This test reproduces that divergence as evidence for the manifest's
// defect-source-sample-time obligation. It records incumbent behaviour; it is
// not a passing contract golden, and the accepted decision-5 model is not
// exercised or implied here.

import (
	"log/slog"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
)

// TestOTAPSourceStampsTransportAtSampleTime shows that the native OTAP route
// records one opaque Source owner for every producer and collector and stamps
// its transport content at the sample timestamp in both validity and
// observation bounds.
func TestOTAPSourceStampsTransportAtSampleTime(t *testing.T) {
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{FlushSamples: 1000})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	// Two Resources with different service.instance.id are distinct logical
	// streams, but they share one Source owner.
	const timestampMillis = 1000
	md := pmetric.NewMetrics()
	for i, instance := range []string{"one", "two"} {
		rm := md.ResourceMetrics().AppendEmpty()
		rm.Resource().Attributes().PutStr("service.name", "payments")
		rm.Resource().Attributes().PutStr("service.namespace", "prod")
		rm.Resource().Attributes().PutStr("service.instance.id", instance)
		metric := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
		metric.SetName("source_provenance_metric")
		dp := metric.SetEmptyGauge().DataPoints().AppendEmpty()
		dp.SetTimestamp(pcommon.Timestamp(timestampMillis * 1_000_000))
		dp.SetDoubleValue(float64(i + 1))
	}
	sink := columnar.NewOTAPSink(s, slog.New(slog.DiscardHandler))
	require.NoError(t, sink.ConsumeRecords(t.Context(), recordsFor(t, md)))

	owner := columnar.SourceOwner("otap")
	history, ok := s.MetadataHistory(owner)
	require.True(t, ok)
	require.Len(t, history.Versions, 1,
		"every producer and collector in the store shares one opaque Source owner")
	version := history.Versions[0]
	require.Equal(t, map[string]string{"transport": "otap"}, version.Content.Attributes,
		"the owner carries a transport fact, not producer or sender roles")
	require.Equal(t, int64(timestampMillis), version.EffectiveFrom,
		"transport is stamped with the sample time")
	require.Equal(t, int64(timestampMillis), version.ObservedFrom,
		"the store aliases the observation lower bound to the sample time")
	require.Equal(t, int64(timestampMillis), version.ObservedUntil,
		"the store aliases the observation upper bound to the sample time")

	page, err := s.QueryMetadataSeries(t.Context(), columnar.MetadataSeriesQuery{
		Kind: columnar.MetadataSource, Start: 0, End: math.MaxInt64,
		Attributes: []columnar.MetadataAttributeCondition{{Name: "transport", Value: "otap"}},
	})
	require.NoError(t, err)
	require.Len(t, page.Series, 2)
	for _, row := range page.Series {
		require.Equal(t, map[string]string{"source": "otap"}, row.OwnerIdentifying,
			"both distinct streams report the same opaque source identity")
		require.Equal(t, int64(timestampMillis), row.MembershipStart)
		require.Equal(t, int64(timestampMillis), row.MembershipObservedTo,
			"the membership observation bound is the sample time, not a receipt time")
	}

	// Source content belongs to its own domain: it does not become a Resource
	// attribute name that info() could promote.
	q, err := s.Querier(0, math.MaxInt64)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, q.Close()) })
	var names []string
	require.NoError(t, q.(storage.ResourceQuerier).IterUniqueAttributeNames(func(name string) {
		names = append(names, name)
	}))
	require.NotContains(t, names, "transport",
		"Source content must not enter the Resource attribute namespace")
}

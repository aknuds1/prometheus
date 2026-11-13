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

package columnar

// The ordinary appender's metadata-derivation model, asserted exactly instead of
// inferred from a profile. The counts are read from the storage's own accumulator,
// which is unexported: the ordinary path adds no exported statistics surface, and
// these assertions are the mechanism proof a timing delta cannot provide.

import (
	"context"
	"strconv"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
)

// ordinaryCountFixture is one ordinary transaction corpus: series that each state
// one exact metadata context, alternating between two contexts so no two adjacent
// series share one.
type ordinaryCountFixture struct {
	series    int
	perSeries int
}

// ordinaryContextMetadata returns the metadata context series i states.
func ordinaryContextMetadata(i int) metadata.Metadata {
	if i%2 == 1 {
		return metadata.Metadata{Type: model.MetricTypeCounter, Unit: "1", Help: "ord_total"}
	}
	return metadata.Metadata{Type: model.MetricTypeGauge, Unit: "By", Help: "ord_gauge"}
}

// ordinaryContextLabels returns the complete labels of series i.
func ordinaryContextLabels(i int) labels.Labels {
	name := "ord_gauge"
	if i%2 == 1 {
		name = "ord_total"
	}
	return labels.FromStrings(
		labels.MetricName, name,
		"instance", "instance-"+strconv.Itoa(i%7),
		"series", strconv.Itoa(i),
	)
}

// appendOrdinary appends one transaction of the fixture. Timestamps are strictly
// increasing per series and distinct per commit, so no append can be a duplicate.
func appendOrdinary(t *testing.T, s *Storage, f ordinaryCountFixture, base int64) {
	t.Helper()
	app := s.appender()
	for i := range f.series {
		ls := ordinaryContextLabels(i)
		md := ordinaryContextMetadata(i)
		for j := range f.perSeries {
			ts := base + int64(j)*15_000
			if _, err := app.Append(0, ls, 0, ts, float64(j), nil, nil, storage.AOptions{Metadata: md}); err != nil {
				t.Fatalf("append %d/%d: %v", i, j, err)
			}
		}
	}
	require.NoError(t, app.Commit())
}

// ordinaryDerivationDelta returns the counts one commit added.
func ordinaryDerivationDelta(after, before ordinaryDerivationStats) ordinaryDerivationStats {
	out := after
	out.Appends -= before.Appends
	out.SeriesWithMetadata -= before.SeriesWithMetadata
	out.ContextRuns -= before.ContextRuns
	out.SeriesCacheHits -= before.SeriesCacheHits
	out.AliasedSeriesHashes -= before.AliasedSeriesHashes
	out.ContextCacheHits -= before.ContextCacheHits
	out.OwnerDerivations -= before.OwnerDerivations
	out.ContentBuilds -= before.ContentBuilds
	out.ContextCanonicalizations -= before.ContextCanonicalizations
	out.Observations -= before.Observations
	out.ChangeDetectionContentCanonicalizations -= before.ChangeDetectionContentCanonicalizations
	out.ApplyContentCanonicalizations -= before.ApplyContentCanonicalizations
	out.ChangeDetectionSeriesHashes -= before.ChangeDetectionSeriesHashes
	out.ApplySeriesHashes -= before.ApplySeriesHashes
	out.OwnerHistoryLookups -= before.OwnerHistoryLookups
	out.CompleteLabelChecks -= before.CompleteLabelChecks
	out.MembershipSearches -= before.MembershipSearches
	return out
}

// newOrdinaryCountStorage is the mechanism arm: WAL and flush off, so the head
// grows and no seal is measured.
func newOrdinaryCountStorage(t *testing.T) *Storage {
	t.Helper()
	s, err := NewStorage(t.TempDir(), StorageOptions{FlushSamples: 1 << 40, DisableWAL: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestOrdinaryDerivationCounts(t *testing.T) {
	const series, perSeries = 40, 50
	const appends = series * perSeries
	fixture := ordinaryCountFixture{series: series, perSeries: perSeries}
	s := newOrdinaryCountStorage(t)

	appendOrdinary(t, s, fixture, 1_000_000)
	first := s.reads.ordinaryDerivationSnapshot()

	// The derivation now scales with the exact series and the exact metadata context.
	// Before the reuse it was one complete owner string and one type/unit/help map per
	// sample (2,000 each), and the store canonicalized observed content twice for every
	// observation (3,920 here).
	require.Equal(t, uint64(appends), first.Appends)
	require.Equal(t, uint64(appends), first.Observations, "one ordered observation per sample, as before")
	require.Equal(t, uint64(series), first.SeriesWithMetadata)
	require.Equal(t, uint64(series), first.OwnerDerivations, "one Metric owner per exact series")
	require.Equal(t, uint64(2), first.ContentBuilds, "one type/unit/help map per exact metadata context")
	require.Equal(t, uint64(2), first.ContextCanonicalizations)
	require.Equal(t, uint64(appends-series), first.SeriesCacheHits)
	require.Equal(t, uint64(appends-2), first.ContextCacheHits)
	require.Zero(t, first.AliasedSeriesHashes, "no hash in this fixture aliases two label sets")

	// The observation-dependent work is unchanged: it is what makes the metadata model
	// exact, so it must not move. The pre-derived identity removes the label hashes and
	// the repeated canonicalizations, nothing else.
	require.Equal(t, uint64(series), first.ContextRuns)
	require.Zero(t, first.ChangeDetectionSeriesHashes, "the staged identity carries the verified hash")
	require.Zero(t, first.ApplySeriesHashes, "association and membership validation reuse that hash")
	require.Equal(t, uint64(series), first.OwnerHistoryLookups)
	require.Equal(t, uint64(series), first.ApplyContentCanonicalizations,
		"one canonical identity per stored version per transaction, not per observation")
	require.Equal(t, uint64(4*(appends-series)), first.CompleteLabelChecks,
		"every repeated association of an established series is still checked in each collision guard")
	require.Equal(t, uint64(2*appends), first.MembershipSearches,
		"the apply phase still searches the owner history and the series membership per observation")

	require.Equal(t, uint64(appends), s.ReadStats().RefFallback,
		"the ordinary consumer appends with a zero reference, so every append hashes")
	require.Equal(t, uint64(0), s.ReadStats().RefTrusted)
	require.Zero(t, s.ReadStats().NativeDerivation.Samples,
		"ordinary ingestion must not contaminate the native builder's accounting")

	// A second commit re-observes the same content at later timestamps: an observation
	// extends the recorded bounds, so it is still a change, and the transaction derives
	// its series and contexts again because the cache dies with the first transaction.
	appendOrdinary(t, s, fixture, 2_000_000)
	second := s.reads.ordinaryDerivationSnapshot()
	repeated := ordinaryDerivationDelta(second, first)

	require.Equal(t, uint64(appends), repeated.Appends)
	require.Equal(t, uint64(appends), repeated.Observations)
	require.Equal(t, uint64(series), repeated.OwnerDerivations)
	require.Equal(t, uint64(2), repeated.ContentBuilds)
	require.Equal(t, uint64(appends-series), repeated.SeriesCacheHits)
	require.Equal(t, uint64(series), repeated.SeriesWithMetadata)
	require.Equal(t, uint64(series), repeated.ContextRuns)
	require.Equal(t, uint64(series), repeated.OwnerHistoryLookups,
		"owner histories are resolved once per transaction, so a commit resolves them again")
	require.Zero(t, repeated.ApplySeriesHashes)
	require.Equal(t, uint64(1), repeated.ChangeDetectionContentCanonicalizations,
		"one comparison about the first association, with the observation's identity reused")
	require.Equal(t, uint64(series), repeated.ApplyContentCanonicalizations)
	require.Equal(t, uint64(4*appends), repeated.CompleteLabelChecks)
	require.Equal(t, uint64(2*appends), repeated.MembershipSearches)

	// A changed metadata context for half the series is a new owner version: the same
	// observation, a different exact context. Nothing is applied retroactively.
	app := s.appender()
	for i := range series {
		ls := ordinaryContextLabels(i)
		md := ordinaryContextMetadata(i)
		if i%2 == 0 {
			md.Help = "ord_gauge_v2"
		}
		for j := range perSeries {
			ts := 3_000_000 + int64(j)*15_000
			if _, err := app.Append(0, ls, 0, ts, float64(j), nil, nil, storage.AOptions{Metadata: md}); err != nil {
				t.Fatalf("changed append %d/%d: %v", i, j, err)
			}
		}
	}
	require.NoError(t, app.Commit())
	changed := ordinaryDerivationDelta(s.reads.ordinaryDerivationSnapshot(), second)
	require.Equal(t, uint64(appends), changed.Appends)
	require.Equal(t, uint64(series), changed.OwnerHistoryLookups)
	require.Equal(t, uint64(2), changed.ContentBuilds,
		"the changed gauge help is one exact context in this commit and the counter context is reused")

	// The metadata itself must still be exactly right: one version per context, with the
	// last version's observation bound covering the whole third commit.
	for i := range series {
		ls := ordinaryContextLabels(i)
		wantVersions, wantEffective, wantHelp := 1, int64(1_000_000), ordinaryContextMetadata(i).Help
		if i%2 == 0 {
			wantVersions, wantEffective, wantHelp = 2, int64(3_000_000), "ord_gauge_v2"
		}
		history, ok := s.MetadataHistory(MetricOwner(ls))
		require.True(t, ok)
		require.Len(t, history.Versions, wantVersions)
		last := history.Versions[len(history.Versions)-1]
		require.Equal(t, wantHelp, last.Content.Attributes["help"])
		require.Equal(t, wantEffective, last.EffectiveFrom)
		require.Equal(t, wantEffective, last.ObservedFrom)
		require.Equal(t, 3_000_000+int64(perSeries-1)*15_000, last.ObservedUntil)
	}
}

// TestOrdinaryMetadataCacheRejectsAliasedHash proves the reuse verification: a hit is a
// hash that locates a candidate, and the complete labels decide. Two label sets forced
// onto one hash must not share an owner, a label set or a content identity.
func TestOrdinaryMetadataCacheRejectsAliasedHash(t *testing.T) {
	s := newOrdinaryCountStorage(t)
	app := s.appender()
	first := labels.FromStrings(labels.MetricName, "aliased_a")
	second := labels.FromStrings(labels.MetricName, "aliased_b")
	md := metadata.Metadata{Type: model.MetricTypeGauge, Unit: "1", Help: "aliased help"}

	firstUpdate, firstIdentity := app.metadataObservation(first, 42, 1_000, md)
	secondUpdate, secondIdentity := app.metadataObservation(second, 42, 2_000, md)

	require.Equal(t, first.String(), firstUpdate.Labels.String())
	require.Equal(t, second.String(), secondUpdate.Labels.String(),
		"the aliasing observation must keep its own complete labels")
	require.NotEqual(t, firstUpdate.Update.Owner, secondUpdate.Update.Owner,
		"an aliasing hash must not move a series to another owner")
	require.Equal(t, MetricOwner(second), secondUpdate.Update.Owner)
	require.Equal(t, uint64(1), app.counts.ordinary.AliasedSeriesHashes)
	require.Zero(t, app.counts.ordinary.SeriesCacheHits,
		"an aliasing hash is derived instead of counted as a hit")
	// The identity the store is given is the caller's verified hash, so the store still
	// hashes nothing; the aliasing guard lives in the cache, not in the identity.
	require.Equal(t, firstIdentity.seriesHash, secondIdentity.seriesHash)
	require.Equal(t, firstIdentity.canonical, secondIdentity.canonical)
}

// TestOrdinaryDerivationCountsIgnoreNativeBatches proves the gating that keeps the
// two accounting models apart: a batch that declares native accounting is applied
// through the same store functions but must not appear in the ordinary counters.
func TestOrdinaryDerivationCountsIgnoreNativeBatches(t *testing.T) {
	s := newOrdinaryCountStorage(t)
	ls := labels.FromStrings(labels.MetricName, "native_shaped_metric")
	owner := MetricOwner(ls)
	batch := Batch{
		Series: []BatchSeries{{
			Labels:     ls,
			Timestamps: []int64{1_000},
			Kinds:      []SampleKind{SampleFloat},
			Values:     []float64{1},
		}},
		Updates: []AssociationUpdate{{
			Labels: ls,
			Update: Update{
				Owner:         owner,
				Content:       Content{Attributes: map[string]string{"type": "gauge"}},
				EffectiveFrom: 1_000,
			},
		}},
		nativeDerivation: NativeDerivationStats{Batches: 1, Samples: 1, Series: 1},
	}
	require.NoError(t, s.AppendBatch(context.Background(), batch))

	require.Zero(t, s.reads.ordinaryDerivationSnapshot(),
		"a native batch must not be attributed to the ordinary counters")
	require.Equal(t, uint64(1), s.ReadStats().NativeDerivation.Batches)

	// The native batch still recorded its metadata: the gating is accounting only.
	content, ok, _ := s.MetadataAtLabels(ls, MetadataMetric, 1_000)
	require.True(t, ok)
	require.Equal(t, "gauge", content.Attributes["type"])
}

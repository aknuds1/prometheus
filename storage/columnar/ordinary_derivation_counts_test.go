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

	"github.com/stretchr/testify/require"

	"github.com/prometheus/common/model"

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

// metadata returns the metadata context of series i.
func (f ordinaryCountFixture) metadataOf(i int) metadata.Metadata {
	if i%2 == 1 {
		return metadata.Metadata{Type: model.MetricTypeCounter, Unit: "1", Help: "ord_total"}
	}
	return metadata.Metadata{Type: model.MetricTypeGauge, Unit: "By", Help: "ord_gauge"}
}

func (f ordinaryCountFixture) labelsOf(i int) labels.Labels {
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
		ls := f.labelsOf(i)
		md := f.metadataOf(i)
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
	var out ordinaryDerivationStats
	out.add(after)
	var negated ordinaryDerivationStats
	negated.Appends = before.Appends
	negated.SeriesWithMetadata = before.SeriesWithMetadata
	negated.ContextRuns = before.ContextRuns
	negated.OwnerDerivations = before.OwnerDerivations
	negated.ContentBuilds = before.ContentBuilds
	negated.Observations = before.Observations
	negated.ChangeDetectionContentCanonicalizations = before.ChangeDetectionContentCanonicalizations
	negated.ApplyContentCanonicalizations = before.ApplyContentCanonicalizations
	negated.ChangeDetectionSeriesHashes = before.ChangeDetectionSeriesHashes
	negated.ApplySeriesHashes = before.ApplySeriesHashes
	negated.OwnerHistoryLookups = before.OwnerHistoryLookups
	negated.CompleteLabelChecks = before.CompleteLabelChecks
	negated.MembershipSearches = before.MembershipSearches
	out.Appends -= negated.Appends
	out.SeriesWithMetadata -= negated.SeriesWithMetadata
	out.ContextRuns -= negated.ContextRuns
	out.OwnerDerivations -= negated.OwnerDerivations
	out.ContentBuilds -= negated.ContentBuilds
	out.Observations -= negated.Observations
	out.ChangeDetectionContentCanonicalizations -= negated.ChangeDetectionContentCanonicalizations
	out.ApplyContentCanonicalizations -= negated.ApplyContentCanonicalizations
	out.ChangeDetectionSeriesHashes -= negated.ChangeDetectionSeriesHashes
	out.ApplySeriesHashes -= negated.ApplySeriesHashes
	out.OwnerHistoryLookups -= negated.OwnerHistoryLookups
	out.CompleteLabelChecks -= negated.CompleteLabelChecks
	out.MembershipSearches -= negated.MembershipSearches
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

	// The appender's own derivation is one per sample on the incumbent: every sample
	// re-derives the Metric owner (a complete label string) and rebuilds the
	// type/unit/help attribute map, and stages one association.
	require.Equal(t, uint64(appends), first.Appends)
	require.Equal(t, uint64(appends), first.Observations)
	require.Equal(t, uint64(appends), first.OwnerDerivations)
	require.Equal(t, uint64(appends), first.ContentBuilds)
	require.Equal(t, uint64(series), first.SeriesWithMetadata,
		"distinct series stating metadata is a property of the corpus, not of the append count")
	require.Equal(t, uint64(series), first.ContextRuns,
		"adjacent series alternate contexts, so the corpus has one metadata run per series")

	// The shared store then re-derives what the appender already knew. Change
	// detection stops at the first association that changes the published catalog,
	// so it is cheap on a first commit; the apply phase pays for every observation.
	require.Equal(t, uint64(1), first.ChangeDetectionSeriesHashes)
	require.Equal(t, uint64(0), first.ChangeDetectionContentCanonicalizations,
		"the first association of a new owner is a change without a content comparison")
	require.Equal(t, uint64(3*appends), first.ApplySeriesHashes,
		"association, owner-version and membership validation each hash the complete labels")
	require.Equal(t, uint64(2*(appends-series)), first.ApplyContentCanonicalizations,
		"every re-observation compares content; each comparison canonicalizes both sides")
	require.Equal(t, uint64(series), first.OwnerHistoryLookups,
		"one owner history per distinct Metric owner")
	require.Equal(t, uint64(4*(appends-series)), first.CompleteLabelChecks,
		"a repeated association of an established series is checked in each collision guard")
	require.Equal(t, uint64(2*appends), first.MembershipSearches,
		"the apply phase searches the owner history and the series membership once per observation")

	require.Equal(t, uint64(appends), s.ReadStats().RefFallback,
		"the ordinary consumer appends with a zero reference, so every append hashes")
	require.Equal(t, uint64(0), s.ReadStats().RefTrusted)
	require.Zero(t, s.ReadStats().NativeDerivation.Samples,
		"ordinary ingestion must not contaminate the native builder's accounting")

	// A second commit re-observes the same content at later timestamps: an
	// observation extends the recorded bounds, so it is a change, and the store pays
	// the same per-observation apply cost again, having resolved the same owners.
	appendOrdinary(t, s, fixture, 2_000_000)
	second := s.reads.ordinaryDerivationSnapshot()
	repeated := ordinaryDerivationDelta(second, first)

	require.Equal(t, uint64(appends), repeated.Appends)
	require.Equal(t, uint64(appends), repeated.Observations)
	require.Equal(t, uint64(appends), repeated.OwnerDerivations,
		"the incumbent derives the owner again for every sample of the second commit")
	require.Equal(t, uint64(appends), repeated.ContentBuilds)
	require.Equal(t, uint64(series), repeated.SeriesWithMetadata)
	require.Equal(t, uint64(series), repeated.ContextRuns)
	require.Equal(t, uint64(series), repeated.OwnerHistoryLookups,
		"owner histories are resolved once per transaction, so a commit resolves them again")
	require.Equal(t, uint64(1), repeated.ChangeDetectionSeriesHashes)
	require.Equal(t, uint64(2), repeated.ChangeDetectionContentCanonicalizations,
		"a re-observation is a change: the bound it extends is compared once")
	require.Equal(t, uint64(3*appends), repeated.ApplySeriesHashes)
	require.Equal(t, uint64(2*appends), repeated.ApplyContentCanonicalizations,
		"every established owner now compares content on re-observation")
	require.Equal(t, uint64(4*appends), repeated.CompleteLabelChecks)
	require.Equal(t, uint64(2*appends), repeated.MembershipSearches)

	// A changed metadata context for half the series is a new owner version, so it
	// stops being a re-observation: the content comparison moves from the extended
	// bound to the change that creates the version.
	app := s.appender()
	for i := range series {
		ls := fixture.labelsOf(i)
		md := fixture.metadataOf(i)
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
	// Every observation still compares its content with the version active before it:
	// the half that keeps its context to extend a bound, the other half to establish
	// that its content really differs before inserting a version.
	require.Equal(t, uint64(2*appends), changed.ApplyContentCanonicalizations)

	// The metadata itself must still be exactly right: the incumbent records one
	// version per context and extends the last version's observation bound over the
	// whole third commit.
	for i := range series {
		ls := fixture.labelsOf(i)
		wantVersions, wantEffective, wantHelp := 1, int64(1_000_000), fixture.metadataOf(i).Help
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
		require.Equal(t, int64(3_000_000+int64(perSeries-1)*15_000), last.ObservedUntil)
	}
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

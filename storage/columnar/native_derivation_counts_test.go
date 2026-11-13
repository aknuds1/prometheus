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

// The native builder's derivation model, asserted exactly instead of inferred
// from a profile. These are the counts of the mechanism before any association
// reuse: every count below is per sample, which is the property a per-context
// derivation must move without changing the ordered-fold counts.

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/storage/columnar"
	"github.com/prometheus/prometheus/storage/remote/otap"
)

// nativeDerivationSites sums the per-site association label hashes.
func nativeDerivationSites(stats columnar.NativeDerivationStats) uint64 {
	total := uint64(0)
	for _, hashes := range stats.AssocKeyHashes {
		total += hashes
	}
	return total
}

func TestNativeDerivationCounts(t *testing.T) {
	const records, perSeries = 20, 8
	shape := ingestShape{series: records, perSeries: perSeries}
	entries := buildIngestRing(shape, 2)
	batches := otapRing(t, entries)

	store := newPrimaryStorage(t)
	sink := columnar.NewOTAPSink(store, slog.New(slog.DiscardHandler))
	stream := otap.NewStreamConsumer(nil, otap.WithRecordSink(sink))
	t.Cleanup(func() { require.NoError(t, stream.Close()) })

	const samples = records * perSeries * 2
	const series = records * 2
	const owners = 5 // Resource, Resource schema, Scope, Source and Metric.

	require.NoError(t, stream.ConsumeBatch(context.Background(), batches[0]))
	first := store.ReadStats().NativeDerivation

	require.Equal(t, uint64(1), first.Batches)
	require.Equal(t, uint64(samples), first.Samples)
	require.Equal(t, uint64(series), first.Series)
	require.Equal(t, uint64(1), first.Resources)
	require.Equal(t, [6]uint64{0: 1, 1: 1, 2: series, 3: 1, 5: 1}, first.OwnersByKind,
		"one Resource, its schema, one Scope, one Source and one Metric owner per series")

	// Derivation before reuse: every complete-label hash, canonicalization, owner
	// construction, reference digest and label build happens once per sample and
	// per association site.
	require.Equal(t, uint64(samples), first.SeriesLookupHashes, "one series hash per sample")
	require.Equal(t, uint64(series), first.SeriesLookupNew)
	require.Equal(t, uint64(samples-series), first.SeriesLookupHits)
	for site, hashes := range first.AssocKeyHashes {
		require.Equal(t, uint64(samples), hashes, "association site %d hashes per sample", site)
		for _, other := range first.AssocKeyHashes[site+1:] {
			require.Equal(t, hashes, other, "every association site hashes the same label set")
		}
	}
	require.Equal(t, uint64(owners*samples), nativeDerivationSites(first))
	require.Equal(t, uint64(owners*samples), first.ContentCanonicalizations)
	require.Equal(t, uint64(owners*samples), first.ContentDerivations)
	require.Equal(t, uint64(2*samples), first.SchemaContentDerivations,
		"one Resource schema and one Scope schema content per sample")
	require.Equal(t, uint64(samples), first.OwnerConstructors[columnar.MetadataResource])
	require.Equal(t, uint64(samples), first.OwnerConstructors[columnar.MetadataResourceSchema])
	require.Equal(t, uint64(samples), first.OwnerConstructors[columnar.MetadataScope])
	require.Equal(t, uint64(samples), first.OwnerConstructors[columnar.MetadataSource])
	require.Equal(t, uint64(samples), first.OwnerConstructors[columnar.MetadataMetric])
	require.Equal(t, uint64(samples), first.OwnerReferences, "one identity digest per sample")
	require.Equal(t, uint64(samples), first.LabelBuilds, "one complete label set per sample")

	// The ordered fold is the correctness work that must not move: one content and
	// one series-membership probe per association, one change point and one
	// observation bound per (series, owner) epoch, and every later observation folded.
	require.Equal(t, uint64(owners*samples), first.EpochOwnerChecks)
	require.Equal(t, uint64(owners*samples), first.EpochMembershipChecks)
	require.Equal(t, uint64(owners*series), first.EmittedChangePoints)
	require.Equal(t, uint64(owners*series), first.EmittedObservationBounds)
	require.Equal(t, uint64(owners*samples-owners*series), first.FoldedObservations)
	require.Equal(t, uint64(0), first.EpochRestatements)
	require.Equal(t, uint64(0), first.OwnerRevisionChanges)

	// The existing per-metric and per-label-name caches already reuse their
	// translation; they are reported so their hits are not mistaken for the
	// association reuse this measurement is about.
	require.Equal(t, uint64(2), first.TranslationMisses, "one translation per metric row")
	require.Equal(t, uint64(samples-2), first.TranslationHits)
	require.Equal(t, uint64(1), first.LabelNameMisses, "one distinct point attribute name")
	require.Equal(t, uint64(samples-1), first.LabelNameHits)

	require.NoError(t, stream.ConsumeBatch(context.Background(), batches[1]))
	second := store.ReadStats().NativeDerivation

	// A second commit re-observes the same identities: nothing is a new series, and
	// the derivation counts are cumulative.
	require.Equal(t, uint64(2), second.Batches)
	require.Equal(t, 2*first.Samples, second.Samples)
	require.Equal(t, 2*first.Series, second.Series, "the batch shape counters are per commit")
	require.Equal(t, 2*first.Resources, second.Resources)
	// The builder and its series map are batch-scoped, so each commit resolves its
	// own series and the counter of lookups that did not create a series is again
	// one fewer than the number of series in that commit.
	require.Equal(t, 2*first.SeriesLookupNew, second.SeriesLookupNew)
	require.Equal(t, 2*first.SeriesLookupHits, second.SeriesLookupHits)
	require.Equal(t, 2*first.ContentCanonicalizations, second.ContentCanonicalizations)
}

// TestNativeDerivationCountsIgnoreDirectCommits proves the native accounting is not
// attributed to batches that do not go through the native builder.
func TestNativeDerivationCountsIgnoreDirectCommits(t *testing.T) {
	shape := ingestShape{series: 4, perSeries: 3}
	entries := buildIngestRing(shape, 1)
	store := newPrimaryStorage(t)
	for _, batches := range directRing(shape, entries) {
		for _, batch := range batches {
			require.NoError(t, store.AppendBatch(context.Background(), batch))
		}
	}
	require.Equal(t, columnar.NativeDerivationStats{}, store.ReadStats().NativeDerivation)
}

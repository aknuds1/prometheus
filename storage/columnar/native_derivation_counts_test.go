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
// from a profile. Derivation counts scale with distinct series contexts in the
// batch; the ordered-fold counts stay per sample. The test also pins the
// pre-reuse counts of the same corpus, so the movement between the two models is
// recorded rather than remembered.

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

	const sites = 5 // Resource, Resource schema, Scope, Source and Metric.
	const samples = records * perSeries * 2
	const series = records * 2
	// One context per metric row and point attribute set: two metric rows over the
	// same 20 point attribute sets.
	const contexts = records * 2

	require.NoError(t, stream.ConsumeBatch(context.Background(), batches[0]))
	first := store.ReadStats().NativeDerivation

	require.Equal(t, uint64(1), first.Batches)
	require.Equal(t, uint64(samples), first.Samples)
	require.Equal(t, uint64(series), first.Series)
	require.Equal(t, uint64(1), first.Resources)
	require.Equal(t, [6]uint64{0: 1, 1: 1, 2: series, 3: 1, 5: 1}, first.OwnersByKind,
		"one Resource, its schema, one Scope, one Source and one Metric owner per series")

	// Derivation now scales with distinct contexts, not with samples: before the
	// reuse each of these counts was one per sample (and per association site).
	require.Equal(t, uint64(contexts), first.TemplateMisses)
	require.Equal(t, uint64(samples-contexts), first.TemplateHits)
	require.Equal(t, uint64(contexts), first.SeriesLookupHashes, "one series hash per context")
	require.Equal(t, uint64(series), first.SeriesLookupNew, "one series per context")
	require.Equal(t, uint64(0), first.SeriesLookupHits, "every context states its own series")
	require.Equal(t, uint64(0), nativeDerivationSites(first),
		"an association keyed from a reused context takes no label hash of its own")
	require.Equal(t, uint64(sites*contexts), first.ContentCanonicalizations)
	require.Equal(t, uint64(sites*contexts), first.ContentDerivations)
	require.Equal(t, uint64(2*contexts), first.SchemaContentDerivations,
		"one Resource schema and one Scope schema content per context")
	require.Equal(t, uint64(contexts), first.OwnerConstructors[columnar.MetadataResource])
	require.Equal(t, uint64(contexts), first.OwnerConstructors[columnar.MetadataResourceSchema])
	require.Equal(t, uint64(contexts), first.OwnerConstructors[columnar.MetadataScope])
	require.Equal(t, uint64(contexts), first.OwnerConstructors[columnar.MetadataSource])
	require.Equal(t, uint64(contexts), first.OwnerConstructors[columnar.MetadataMetric])
	require.Equal(t, uint64(contexts), first.OwnerReferences, "one identity digest per context")
	require.Equal(t, uint64(contexts), first.LabelBuilds, "one complete label set per context")

	// The ordered fold is the correctness work that must not move: one content and
	// one series-membership probe per association, one change point and one
	// observation bound per (series, owner) epoch, and every later observation folded.
	require.Equal(t, uint64(sites*samples), first.EpochOwnerChecks)
	require.Equal(t, uint64(sites*samples), first.EpochMembershipChecks)
	require.Equal(t, uint64(sites*series), first.EmittedChangePoints)
	require.Equal(t, uint64(sites*series), first.EmittedObservationBounds)
	require.Equal(t, uint64(sites*samples-sites*series), first.FoldedObservations)
	require.Equal(t, uint64(0), first.EpochRestatements)
	require.Equal(t, uint64(0), first.OwnerRevisionChanges)

	// The per-metric and per-label-name caches are now probed once per context.
	require.Equal(t, uint64(2), first.TranslationMisses, "one translation per metric row")
	require.Equal(t, uint64(contexts-2), first.TranslationHits)
	require.Equal(t, uint64(1), first.LabelNameMisses, "one distinct point attribute name")
	require.Equal(t, uint64(contexts-1), first.LabelNameHits)

	require.NoError(t, stream.ConsumeBatch(context.Background(), batches[1]))
	second := store.ReadStats().NativeDerivation

	// A second commit re-observes the same identities: nothing is a new series, and
	// the derivation counts are cumulative.
	require.Equal(t, uint64(2), second.Batches)
	require.Equal(t, 2*first.Samples, second.Samples)
	require.Equal(t, 2*first.Series, second.Series, "the batch shape counters are per commit")
	require.Equal(t, 2*first.Resources, second.Resources)
	// The template cache and the builder's series map are batch-scoped, so a second
	// commit derives its contexts again: the reuse is per batch, not across commits,
	// and a counter that survived the batch would be a cache lifetime bug.
	require.Equal(t, 2*first.TemplateMisses, second.TemplateMisses)
	require.Equal(t, 2*first.TemplateHits, second.TemplateHits)
	require.Equal(t, 2*first.SeriesLookupNew, second.SeriesLookupNew)
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

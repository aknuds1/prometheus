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

package columnar

// Owner-cardinality diagnosis of the primary-ingestion metadata anomaly.
//
// BenchmarkPrimaryIngestMetadata records a per-record Resource layout with an owner
// version change at ~16x its no-metadata cell, with an unexplained magnitude. This
// file adds a separately named benchmark and a complete semantic oracle over the same
// corpus shape, varying only the number of resident Resource owners, so the cost can be
// attributed to a mechanism instead of to a corpus accident. The legacy benchmark is
// not modified.
//
// Nothing here changes production behaviour: the fixtures, the expected state, the
// benchmark and the oracle live in tests only.

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

const (
	// ownerCostGaugeName and ownerCostSumName are the two metric series every logical
	// record stores, matching the legacy primary fixture's gauge/sum pair.
	ownerCostGaugeName = "primary_gauge"
	ownerCostSumName   = "primary_requests_total"
	// ownerCostIntervalMs is the dense 15 s scrape interval of the fixture.
	ownerCostIntervalMs = int64(15_000)
	// ownerCostMint is the first timestamp of ring entry 0.
	ownerCostMint = int64(1_700_000_000_000)
	// ownerCostTimedEntries is the number of non-overlapping timed commits one
	// benchmark run may perform.
	ownerCostTimedEntries = 8
)

// ownerCardinalityMode is the timed metadata input of one controlled arm.
type ownerCardinalityMode int

const (
	// ownerCardinalityNoMetadata states no associations and no owner updates.
	ownerCardinalityNoMetadata ownerCardinalityMode = iota
	// ownerCardinalityUnchanged re-observes the currently active content for every
	// record in every entry.
	ownerCardinalityUnchanged
	// ownerCardinalityVersionAndAssociations states one association per record for the
	// content the preceding owner update established, plus one explicit owner version
	// for each changed owner.
	ownerCardinalityVersionAndAssociations
	// ownerCardinalityOwnerOnlyVersion states owner versions without associations.
	ownerCardinalityOwnerOnlyVersion
	// ownerCardinalityLegacyRestoringVersion is the legacy fixture's input: association
	// content that differs from the content its owner update establishes, so every
	// entry restores a version before inserting the next one. It is reproduced only by
	// the legacy accounting fixture and is not a measured arm.
	ownerCardinalityLegacyRestoringVersion
)

func (m ownerCardinalityMode) String() string {
	switch m {
	case ownerCardinalityNoMetadata:
		return "no-metadata"
	case ownerCardinalityUnchanged:
		return "unchanged"
	case ownerCardinalityVersionAndAssociations:
		return "version-plus-associations"
	case ownerCardinalityOwnerOnlyVersion:
		return "owner-only-version"
	case ownerCardinalityLegacyRestoringVersion:
		return "legacy-restoring-version"
	}
	return "unknown"
}

// ownerCardinalitySpec fixes one arm's corpus. The benchmark uses ownerCostFullSpec;
// the oracle and lifecycle tests use smaller instances of the same builder, so a
// semantic check costs no more than the property it proves.
type ownerCardinalitySpec struct {
	mode    ownerCardinalityMode
	records int
	points  int
	owners  int
	// updated is the number of owners that receive a new version per entry. Zero
	// means every resident owner does.
	updated int
	timed   int
	// versionCap is the arm's per-owner version cap; zero is uncapped. A store opened
	// with MetadataVersionCap -1 has no cap, which is this model's zero.
	versionCap int
}

// ownerCostFullSpec returns the fixed benchmark shape: 1,000 records, 100 dense
// timestamps each, 2,000 series and 200,000 samples per commit, eight timed commits.
func ownerCostFullSpec(mode ownerCardinalityMode, owners, updated int) ownerCardinalitySpec {
	return ownerCardinalitySpec{
		mode: mode, records: 1_000, points: 100, owners: owners, updated: updated,
		timed: ownerCostTimedEntries,
	}
}

// interval is the fixture's dense sample interval.
func (ownerCardinalitySpec) interval() int64 { return ownerCostIntervalMs }

// span is the time one ring entry covers, so no two entries overlap.
func (s ownerCardinalitySpec) span() int64 { return int64(s.points+2) * s.interval() }

// base returns the first timestamp of ring entry e.
func (s ownerCardinalitySpec) base(e int) int64 { return ownerCostMint + int64(e)*s.span() }

// mid returns the effective time of entry e's owner updates: the middle timestamp of
// the entry's dense cohort, exactly as the legacy fixture places it.
func (s ownerCardinalitySpec) mid(e int) int64 { return s.base(e) + int64(s.points/2)*s.interval() }

// samples returns the number of samples one entry carries: gauge plus sum per record.
func (s ownerCardinalitySpec) samples() int { return 2 * s.records * s.points }

// series returns the number of metric series one entry carries.
func (s ownerCardinalitySpec) series() int { return 2 * s.records }

// ownerIndex maps record i to its owner index, assigned evenly over the owners.
func (s ownerCardinalitySpec) ownerIndex(i int) int { return i % s.owners }

// versionOwners is the number of owners that receive an explicit version in an entry.
func (s ownerCardinalitySpec) versionOwners() int {
	if s.updated > 0 {
		return s.updated
	}
	return s.owners
}

// associations returns the number of associations one entry states.
func (s ownerCardinalitySpec) associations() int {
	switch s.mode {
	case ownerCardinalityUnchanged, ownerCardinalityVersionAndAssociations:
		return s.records
	}
	return 0
}

// ownerCardinalityIdentity returns the identifying Resource attributes of owner j.
func ownerCardinalityIdentity(j int) map[string]string {
	// service.namespace is part of the owner key even when it is empty, because the
	// identifying attribute list is fixed rather than derived from the stated map.
	return map[string]string{
		"service.name":        "checkout",
		"service.namespace":   "",
		"service.instance.id": "instance-" + strconv.Itoa(j),
	}
}

// ownerCardinalityContent returns the descriptive content of owner j at version v.
// Identity attributes stay constant, so a version change never moves the owner key.
// Every owner and every version states distinct content, unlike the legacy fixture's
// changedResourceAttrs(0), which states one owner's identity as every owner's content.
func ownerCardinalityContent(j, v int) Content {
	attributes := ownerCardinalityIdentity(j)
	attributes["deployment.environment"] = "env-" + strconv.Itoa(v)
	return Content{Attributes: attributes}
}

// ownerCardinalityLabels returns one record's labels for a metric name. The Resource
// reference label is part of the series identity, exactly as the native sink writes
// it, so a record that moved to another Resource would be a different series.
func ownerCardinalityLabels(i, j int, owner OwnerKey, name string) labels.Labels {
	return labels.FromStrings(
		labels.MetricName, name,
		"job", "checkout",
		"instance", ownerCardinalityIdentity(j)["service.instance.id"],
		"path", "/api/v"+strconv.Itoa(i%7),
		"code", strconv.Itoa(200+i%5),
		"series", strconv.Itoa(i),
		OTELResourceLabel, owner.Ref(),
	)
}

// ownerCostKinds returns a dense float kind column.
func ownerCostKinds(n int) []SampleKind {
	out := make([]SampleKind, n)
	for i := range out {
		out[i] = SampleFloat
	}
	return out
}

// ownerCardinalityVersion is one expected owner history entry, derived from fixture
// inputs only.
type ownerCardinalityVersion struct {
	content       Content
	effectiveFrom int64
	observedFrom  int64
	observedUntil int64
}

// ownerCardinalityState is the fixture-derived expected logical state after an entry.
type ownerCardinalityState struct {
	ownerVersions [][]ownerCardinalityVersion
	truncated     []bool

	// The single membership row every associated record states, and its observed
	// bound. The fixture gives every record the same timestamp vector, so every
	// membership row has identical bounds.
	hasMembership           bool
	membershipEffective     int64
	membershipObservedUntil int64

	// Counts the fixture declares for the entry.
	versionsInserted       int
	versionsEvicted        int
	versionBoundExtensions int
	versionsBoundExtended  int
	membershipInserted     int
	membershipBoundExtends int
}

// ownerCardinalityEntry is one prepared commit and the expected state after it.
type ownerCardinalityEntry struct {
	batch  Batch
	expect ownerCardinalityState
}

// ownerCardinalityFixture is one arm: its specification, owner keys and prepared
// commits with their expected states.
type ownerCardinalityFixture struct {
	spec    ownerCardinalitySpec
	owners  []OwnerKey
	entries []ownerCardinalityEntry
}

// versionAt returns the version effective at t: the newest version whose effective
// time is not after t. There is no lookahead.
func versionAt(versions []ownerCardinalityVersion, t int64) (ownerCardinalityVersion, bool) {
	found := -1
	for i := range versions {
		if versions[i].effectiveFrom <= t {
			found = i
		}
	}
	if found < 0 {
		return ownerCardinalityVersion{}, false
	}
	return versions[found], true
}

// buildOwnerCardinalityFixture materializes every commit and the expected state after
// each one from the fixture inputs alone. It never calls the production update
// algorithm: the expected histories are produced by the rules the arm declares.
func buildOwnerCardinalityFixture(tb testing.TB, spec ownerCardinalitySpec) *ownerCardinalityFixture {
	tb.Helper()
	require.Positive(tb, spec.records)
	require.Positive(tb, spec.points)
	require.Positive(tb, spec.owners)
	require.Positive(tb, spec.timed)
	require.LessOrEqual(tb, spec.owners, spec.records)
	require.Zero(tb, spec.records%spec.owners, "records are assigned evenly across owners")
	if spec.updated > 0 {
		require.LessOrEqual(tb, spec.updated, spec.owners)
	}

	fx := &ownerCardinalityFixture{spec: spec, owners: make([]OwnerKey, spec.owners)}
	for j := range fx.owners {
		fx.owners[j] = ResourceOwner(ownerCardinalityIdentity(j))
	}
	expect := ownerCardinalityState{
		ownerVersions: make([][]ownerCardinalityVersion, spec.owners),
		truncated:     make([]bool, spec.owners),
	}
	for e := 0; e <= spec.timed; e++ {
		entry, next := buildOwnerCardinalityEntry(spec, fx.owners, expect, e)
		entry.expect = next
		fx.entries = append(fx.entries, entry)
		expect = next
	}
	return fx
}

// buildOwnerCardinalityEntry builds one entry's commit and the state it must produce.
func buildOwnerCardinalityEntry(spec ownerCardinalitySpec, owners []OwnerKey, before ownerCardinalityState, e int) (ownerCardinalityEntry, ownerCardinalityState) {
	next := ownerCardinalityState{
		ownerVersions:           make([][]ownerCardinalityVersion, len(before.ownerVersions)),
		truncated:               slices.Clone(before.truncated),
		hasMembership:           before.hasMembership,
		membershipEffective:     before.membershipEffective,
		membershipObservedUntil: before.membershipObservedUntil,
		versionsInserted:        before.versionsInserted,
		versionsEvicted:         before.versionsEvicted,
		versionBoundExtensions:  before.versionBoundExtensions,
		versionsBoundExtended:   before.versionsBoundExtended,
		membershipInserted:      before.membershipInserted,
		membershipBoundExtends:  before.membershipBoundExtends,
	}
	for j := range before.ownerVersions {
		next.ownerVersions[j] = slices.Clone(before.ownerVersions[j])
	}

	base, mid := spec.base(e), spec.mid(e)
	entry := ownerCardinalityEntry{}

	// Samples: every record contributes a gauge and a sum series with the fixture's
	// dense cohort, so no arm can differ in physical sample work.
	for i := 0; i < spec.records; i++ {
		ts := make([]int64, spec.points)
		gauge := make([]float64, spec.points)
		sum := make([]float64, spec.points)
		for p := range ts {
			ts[p] = base + int64(p)*spec.interval()
			gauge[p] = float64(e*1_000_000 + i*100 + p)
			sum[p] = gauge[p] * 100
		}
		j := spec.ownerIndex(i)
		entry.batch.Series = append(entry.batch.Series,
			BatchSeries{
				Labels:     ownerCardinalityLabels(i, j, owners[j], ownerCostGaugeName),
				Timestamps: ts,
				Kinds:      ownerCostKinds(spec.points),
				Values:     gauge,
			},
			BatchSeries{
				Labels:     ownerCardinalityLabels(i, j, owners[j], ownerCostSumName),
				Timestamps: ts,
				Kinds:      ownerCostKinds(spec.points),
				Values:     sum,
			})
	}

	// Metadata input, and the expected state the input must produce.
	if spec.associations() > 0 {
		for i := 0; i < spec.records; i++ {
			j := spec.ownerIndex(i)
			content := spec.associationContent(before, j, e)
			entry.batch.Updates = append(entry.batch.Updates, AssociationUpdate{
				Labels: ownerCardinalityLabels(i, j, owners[j], ownerCostSumName),
				Update: Update{Owner: owners[j], Content: content, EffectiveFrom: base},
			})
			observeExpectedVersion(&next, j, content, base, spec.versionCap)
		}
		if !next.hasMembership {
			next.membershipInserted += spec.records
			next.hasMembership = true
			next.membershipEffective = base
			next.membershipObservedUntil = base
		} else {
			next.membershipBoundExtends += spec.records
			next.membershipObservedUntil = max(next.membershipObservedUntil, base)
		}
	}
	if spec.ownerUpdates() > 0 {
		for j := 0; j < spec.versionOwners(); j++ {
			content := ownerCardinalityContent(j, spec.versionVersion(e))
			entry.batch.OwnerUpdates = append(entry.batch.OwnerUpdates, Update{
				Owner: owners[j], Content: content, EffectiveFrom: mid,
			})
			insertExpectedVersion(&next, j, content, mid, spec.versionCap)
		}
	}
	return entry, next
}

// ownerUpdates returns the number of owner updates one entry states.
func (s ownerCardinalitySpec) ownerUpdates() int {
	switch s.mode {
	case ownerCardinalityVersionAndAssociations, ownerCardinalityOwnerOnlyVersion:
		return s.versionOwners()
	}
	return 0
}

// associationContent is the content an entry's association states for owner j. In the
// controlled version arm it is exactly the content the preceding owner update
// established, which is what keeps the arm free of a restoration transition. In the
// unchanged arm it is the prewarm content, which the owner never changes.
func (s ownerCardinalitySpec) associationContent(before ownerCardinalityState, j, e int) Content {
	switch s.mode {
	case ownerCardinalityUnchanged:
		return ownerCardinalityContent(j, 0)
	case ownerCardinalityVersionAndAssociations:
		active, ok := versionAt(before.ownerVersions[j], s.base(e))
		if !ok {
			return ownerCardinalityContent(j, 0)
		}
		return active.content
	}
	return Content{}
}

// versionVersion is the content version an entry's owner update states for owner j.
func (s ownerCardinalitySpec) versionVersion(e int) int {
	if s.mode == ownerCardinalityOwnerOnlyVersion {
		return e
	}
	return e + 1
}

// observeExpectedVersion applies one association to the expected state: it extends the
// bounds of the version active at the observation time when the content is equal, and
// otherwise inserts a version, which is what makes a restoration observable.
func observeExpectedVersion(state *ownerCardinalityState, j int, content Content, t int64, versionCap int) {
	if active, ok := versionAt(state.ownerVersions[j], t); ok {
		canonical := content.Canonical()
		for v := range state.ownerVersions[j] {
			version := &state.ownerVersions[j][v]
			if version.effectiveFrom != active.effectiveFrom {
				continue
			}
			if version.content.Canonical() != canonical {
				break
			}
			state.versionBoundExtensions++
			if t < version.observedFrom || t > version.observedUntil {
				state.versionsBoundExtended++
				version.observedFrom = min(version.observedFrom, t)
				version.observedUntil = max(version.observedUntil, t)
			}
			return
		}
	}
	insertExpectedVersion(state, j, content, t, versionCap)
}

// insertExpectedVersion inserts one expected version, applying the arm's version cap.
func insertExpectedVersion(state *ownerCardinalityState, j int, content Content, t int64, versionCap int) {
	versions := state.ownerVersions[j]
	version := ownerCardinalityVersion{
		content: content, effectiveFrom: t, observedFrom: t, observedUntil: t,
	}
	at := len(versions)
	for i := range versions {
		if versions[i].effectiveFrom >= t {
			at = i
			break
		}
	}
	versions = slices.Insert(versions, at, version)
	state.versionsInserted++
	if versionCap > 0 && len(versions) > versionCap {
		state.versionsEvicted += len(versions) - versionCap
		versions = slices.Clone(versions[len(versions)-versionCap:])
		state.truncated[j] = true
	}
	state.ownerVersions[j] = versions
}

// newOwnerCardinalityStorage opens a controlled arm's store: metadata histories
// uncapped, WAL off and a flush threshold far above the fixture, so the timed loop
// measures the head and the metadata transaction only.
func newOwnerCardinalityStorage(tb testing.TB) *Storage {
	tb.Helper()
	st, err := NewStorage(tb.TempDir(), StorageOptions{
		FlushSamples:       1 << 40,
		DisableWAL:         true,
		MetadataVersionCap: -1,
	})
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = st.Close() })
	return st
}

// BenchmarkPrimaryIngestOwnerCardinality measures the owner-cardinality matrix at the
// legacy corpus shape. Each arm commits one prewarm entry outside the timer and then
// exactly one distinct ring entry per timed iteration.
func BenchmarkPrimaryIngestOwnerCardinality(b *testing.B) {
	for _, mode := range []ownerCardinalityMode{
		ownerCardinalityNoMetadata, ownerCardinalityUnchanged,
		ownerCardinalityVersionAndAssociations, ownerCardinalityOwnerOnlyVersion,
	} {
		for _, owners := range []int{1, 10, 100, 1_000} {
			spec := ownerCostFullSpec(mode, owners, owners)
			b.Run(fmt.Sprintf("mode=%s,owners=%d", mode, owners), func(b *testing.B) {
				benchmarkOwnerCardinalityArm(b, spec)
			})
		}
	}
	// Resident-versus-changed-owner contrast at fixed resident cardinality.
	for _, updated := range []int{1, 10, 100, 1_000} {
		spec := ownerCostFullSpec(ownerCardinalityVersionAndAssociations, 1_000, updated)
		b.Run(fmt.Sprintf("mode=%s,owners=1000,updated=%d", ownerCardinalityVersionAndAssociations, updated), func(b *testing.B) {
			benchmarkOwnerCardinalityArm(b, spec)
		})
	}
}

// benchmarkOwnerCardinalityArm times one arm's timed commits and publishes the arm's
// per-commit accounting after the timer stops, so no reporting work is timed.
func benchmarkOwnerCardinalityArm(b *testing.B, spec ownerCardinalitySpec) {
	fx := buildOwnerCardinalityFixture(b, spec)
	if b.N > spec.timed {
		b.Fatalf("b.N=%d exceeds the %d-entry ring; use -benchtime=%dx so no iteration repeats a commit",
			b.N, spec.timed, spec.timed)
	}

	st := newOwnerCardinalityStorage(b)
	require.NoError(b, st.AppendBatch(b.Context(), fx.entries[0].batch))
	prewarm := st.reads.metadataWorkSnapshot()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := st.AppendBatch(b.Context(), fx.entries[1+i].batch); err != nil {
			b.Fatalf("AppendBatch entry %d: %v", i+1, err)
		}
	}
	b.StopTimer()

	elapsed := float64(b.Elapsed().Nanoseconds())
	perCommit := elapsed / float64(b.N)
	b.ReportMetric(float64(spec.samples()), "samples/op")
	b.ReportMetric(float64(spec.series()), "series/op")
	b.ReportMetric(float64(spec.associations()), "associations/op")
	b.ReportMetric(float64(spec.ownerUpdates()), "owner-updates/op")
	b.ReportMetric(float64(spec.ownerUpdates()), "changed-owners/op")
	b.ReportMetric(perCommit, "ns/commit")
	b.ReportMetric(elapsed/float64(b.N*spec.samples()), "ns/sample")
	if spec.ownerUpdates() > 0 {
		b.ReportMetric(perCommit/float64(spec.ownerUpdates()), "ns/changed-owner")
	}
	b.ReportMetric(liveHeapMiB(), "live-heap-MiB")

	// Timed work is the counter delta of the timed loop, never the whole run.
	timed := st.reads.metadataWorkSnapshot().since(prewarm)
	reportOwnerCardinalityWork(b, timed, b.N)
	require.Equal(b, b.N*spec.ownerUpdates(), timed.versionsInserted,
		"exactly the versions the fixture declares, per commit")
	if spec.mode == ownerCardinalityNoMetadata {
		require.Zero(b, timed.metadataTransactions, "no metadata transaction")
	} else {
		require.Equal(b, b.N, timed.metadataTransactions, "one metadata transaction per commit")
	}
	require.Zero(b, timed.versionsEvicted, "the controlled matrix is uncapped")
	require.Zero(b, timed.membershipDuplicateNoops)
	// Every association reaches the observation-only path once, and exactly one
	// observation per resident owner actually moves that version's bound; the rest
	// repeat bounds the entry already extended.
	if spec.associations() == 0 {
		require.Zero(b, timed.versionBoundsExtended, "no association means no observation bound")
		require.Zero(b, timed.versionDuplicateNoops)
	} else {
		require.Equal(b, b.N*spec.associations(), timed.versionBoundsExtended, "observation-only applies")
		require.Equal(b, b.N*(spec.associations()-spec.owners), timed.versionDuplicateNoops,
			"repeated observations of already-extended bounds")
	}

	require.Len(b, st.buffer, spec.series(), "every fixture series is resident")
	require.Equal(b, spec.samples()*(b.N+1), st.bufferN, "exactly the fixture's samples are buffered")
	require.Zero(b, st.ReadStats().SealCount, "no seal inside the timed loop")
	requireOwnerCardinalityResident(b, st, fx, b.N)
	requireOwnerCardinalityHistories(b, st, fx, fx.entries[b.N].expect)
}

// reportOwnerCardinalityWork publishes the timed loop's physical work per commit. The
// bucket-entry count is the decisive one: it separates a fixed-price bucket clone from
// a clone whose size is the resident owner count.
func reportOwnerCardinalityWork(b *testing.B, w metadataWork, commits int) {
	b.Helper()
	perCommit := func(n int) float64 { return float64(n) / float64(commits) }
	b.ReportMetric(perCommit(w.versionsInserted), "versions/op")
	b.ReportMetric(perCommit(w.versionBoundsExtended), "version-bounds/op")
	b.ReportMetric(perCommit(w.versionsEvicted), "evicted/op")
	b.ReportMetric(perCommit(w.membershipsInserted), "memberships/op")
	b.ReportMetric(perCommit(w.membershipBoundsExtended), "membership-bounds/op")
	b.ReportMetric(perCommit(w.changedOwners), "changed-owners/op")
	b.ReportMetric(perCommit(w.residentOwners), "resident-owners/op")
	b.ReportMetric(perCommit(w.ownerHistoryResolutions), "owner-resolutions/op")
	b.ReportMetric(perCommit(w.versionSearches), "version-searches/op")
	b.ReportMetric(perCommit(w.versionSearchComparisons), "version-search-cmp/op")
	b.ReportMetric(perCommit(w.membershipSearches), "membership-searches/op")
	b.ReportMetric(perCommit(w.seriesHashes), "series-hashes/op")
	b.ReportMetric(perCommit(w.completeLabelChecks), "label-checks/op")
	b.ReportMetric(perCommit(w.changeDetectionCanonicalizations), "detect-canonical/op")
	b.ReportMetric(perCommit(w.applyCanonicalizations), "apply-canonical/op")
	b.ReportMetric(perCommit(w.contentComparisons), "content-compares/op")
	b.ReportMetric(perCommit(w.contentClones), "content-clones/op")
	b.ReportMetric(perCommit(w.cloneByOwnerEntries), "clone-owners/op")
	b.ReportMetric(perCommit(w.cloneBySeriesEntries), "clone-series/op")
	b.ReportMetric(perCommit(w.cloneAttributeConds), "clone-index-conds/op")
	b.ReportMetric(perCommit(w.ownerCOPCopies), "owner-copies/op")
	b.ReportMetric(perCommit(w.ownerCOPVersionRows), "owner-cop-rows/op")
	b.ReportMetric(perCommit(w.membershipCOPRows), "membership-cop-rows/op")
	b.ReportMetric(perCommit(w.indexCalls), "index-calls/op")
	b.ReportMetric(perCommit(w.indexVersionsVisited), "index-versions/op")
	b.ReportMetric(perCommit(w.indexOldConditions), "index-old-conds/op")
	b.ReportMetric(perCommit(w.indexNewConditions), "index-new-conds/op")
	b.ReportMetric(perCommit(w.indexBucketClones), "bucket-clones/op")
	b.ReportMetric(perCommit(w.indexBucketClonedEntries), "bucket-entries/op")
	b.ReportMetric(perCommit(w.indexGlobalRangeUpdates), "index-range-updates/op")
}

// liveHeapMiB returns the live heap after a collection, which is retained state rather
// than the allocation volume the benchmark reports separately.
func liveHeapMiB() float64 {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return float64(stats.HeapAlloc) / (1 << 20)
}

// requireOwnerCardinalityResident checks the resident catalog shape after the last
// applied entry: the exact owner set, version counts, memberships and chains.
func requireOwnerCardinalityResident(tb testing.TB, st *Storage, fx *ownerCardinalityFixture, applied int) {
	tb.Helper()
	expect := fx.entries[applied].expect
	if fx.spec.mode == ownerCardinalityNoMetadata {
		require.Empty(tb, st.metadata.byOwner)
		require.Empty(tb, st.metadata.bySeries)
		return
	}
	require.Len(tb, st.metadata.byOwner, fx.spec.owners)
	for j, owner := range fx.owners {
		history, ok := st.metadata.byOwner[owner]
		require.True(tb, ok, "owner %d is resident", j)
		require.Len(tb, history.versions, len(expect.ownerVersions[j]), "owner %d version count", j)
		require.Equal(tb, expect.truncated[j], history.truncated, "owner %d truncation", j)
	}
	if expect.hasMembership {
		require.Len(tb, st.metadata.bySeries, fx.spec.records)
		require.Len(tb, st.metadata.chains, fx.spec.records)
		return
	}
	require.Empty(tb, st.metadata.bySeries)
	require.Empty(tb, st.metadata.chains)
}

// requireOwnerCardinalityHistories compares every owner's complete history.
func requireOwnerCardinalityHistories(tb testing.TB, st *Storage, fx *ownerCardinalityFixture, expect ownerCardinalityState) {
	tb.Helper()
	for j, owner := range fx.owners {
		history, ok := st.MetadataHistory(owner)
		if len(expect.ownerVersions[j]) == 0 {
			require.False(tb, ok, "owner %d has no history in this arm", j)
			continue
		}
		require.True(tb, ok, "owner %d has a history", j)
		require.Equal(tb, expect.truncated[j], history.Truncated, "owner %d truncation", j)
		require.Len(tb, history.Versions, len(expect.ownerVersions[j]), "owner %d version count", j)
		for i, want := range expect.ownerVersions[j] {
			got := history.Versions[i]
			require.Equal(tb, want.content.Canonical(), got.Content.Canonical(), "owner %d version %d content", j, i)
			require.Equal(tb, want.effectiveFrom, got.EffectiveFrom, "owner %d version %d effective", j, i)
			require.Equal(tb, want.observedFrom, got.ObservedFrom, "owner %d version %d observed from", j, i)
			require.Equal(tb, want.observedUntil, got.ObservedUntil, "owner %d version %d observed until", j, i)
		}
	}
}

// verifyOwnerCardinalityState compares the complete observable state of one arm with
// the fixture-derived expectation after the given number of applied entries.
func verifyOwnerCardinalityState(tb testing.TB, st *Storage, fx *ownerCardinalityFixture, applied int) {
	tb.Helper()
	expect := fx.entries[applied].expect
	requireOwnerCardinalityResident(tb, st, fx, applied)
	requireOwnerCardinalityHistories(tb, st, fx, expect)
	verifyOwnerCardinalitySamples(tb, st, fx, applied)
	verifyOwnerCardinalityOwnerPage(tb, st, fx, expect)
	verifyOwnerCardinalityMembershipPage(tb, st, fx, expect)
	verifyOwnerCardinalityBoundaries(tb, st, fx, expect)
	verifyOwnerCardinalityUnassociated(tb, st, fx)
	verifyOwnerCardinalityCountersZero(tb, st)
}

// ownerCostStoredSeries is one stored series and its samples, in label order.
type ownerCostStoredSeries struct {
	labels  labels.Labels
	samples []ownerCostSample
}

// verifyOwnerCardinalitySamples compares every stored series, every sample timestamp and
// every exact float value bit with the fixture, and checks the buffered head's kinds and
// start timestamps, which the sample iterator does not expose for float samples.
func verifyOwnerCardinalitySamples(tb testing.TB, st *Storage, fx *ownerCardinalityFixture, applied int) {
	tb.Helper()
	spec := fx.spec
	mint, maxt := spec.base(0), spec.base(applied)+int64(spec.points-1)*spec.interval()
	querier, err := st.Querier(mint, maxt)
	require.NoError(tb, err)
	defer func() { require.NoError(tb, querier.Close()) }()

	set := querier.Select(context.Background(), true, nil,
		labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+"))
	var got []ownerCostStoredSeries
	for set.Next() {
		series := set.At()
		stored := ownerCostStoredSeries{labels: series.Labels()}
		it := series.Iterator(nil)
		for vt := it.Next(); vt != chunkenc.ValNone; vt = it.Next() {
			ts, value := it.At()
			stored.samples = append(stored.samples, ownerCostSample{timestamp: ts, bits: math.Float64bits(value)})
		}
		require.NoError(tb, it.Err())
		got = append(got, stored)
	}
	require.NoError(tb, set.Err())

	// One expected series per logical identity, holding every applied entry's samples.
	byHash := map[uint64]*ownerCostStoredSeries{}
	for e := 0; e <= applied; e++ {
		for i := 0; i < spec.records; i++ {
			j := spec.ownerIndex(i)
			for _, name := range []string{ownerCostGaugeName, ownerCostSumName} {
				ls := ownerCardinalityLabels(i, j, fx.owners[j], name)
				hash := metadataSeriesKey(ls)
				stored := byHash[hash]
				if stored == nil {
					stored = &ownerCostStoredSeries{labels: ls}
					byHash[hash] = stored
				}
				for p := 0; p < spec.points; p++ {
					value := float64(e*1_000_000 + i*100 + p)
					if name == ownerCostSumName {
						value *= 100
					}
					stored.samples = append(stored.samples, ownerCostSample{
						timestamp: spec.base(e) + int64(p)*spec.interval(),
						bits:      math.Float64bits(value),
					})
				}
			}
		}
	}
	require.Len(tb, byHash, spec.series(), "the fixture states distinct series")
	want := make([]ownerCostStoredSeries, 0, len(byHash))
	for _, stored := range byHash {
		want = append(want, *stored)
	}
	slices.SortFunc(want, func(a, b ownerCostStoredSeries) int { return labels.Compare(a.labels, b.labels) })
	slices.SortFunc(got, func(a, b ownerCostStoredSeries) int { return labels.Compare(a.labels, b.labels) })
	require.Len(tb, got, len(want), "stored series count")
	for i := range want {
		require.True(tb, labels.Equal(want[i].labels, got[i].labels),
			"stored series %d complete labels: want %s, got %s", i, want[i].labels, got[i].labels)
		require.Len(tb, got[i].samples, len(want[i].samples), "series %s sample count", want[i].labels)
		for k := range want[i].samples {
			if want[i].samples[k] != got[i].samples[k] {
				tb.Fatalf("series %s sample %d: want timestamp %d bits %#x, got timestamp %d bits %#x",
					want[i].labels, k, want[i].samples[k].timestamp, want[i].samples[k].bits,
					got[i].samples[k].timestamp, got[i].samples[k].bits)
			}
		}
	}

	buffered, nonFloat := 0, 0
	for _, hs := range st.buffer {
		buffered += len(hs.ts)
		for _, kind := range hs.kind {
			if kind != SampleFloat {
				nonFloat++
			}
		}
		require.Empty(tb, hs.st, "the fixture states no start timestamps")
		require.Empty(tb, hs.hist, "the fixture states no histogram payloads")
	}
	require.Zero(tb, nonFloat, "fixture samples are floats")
	require.Equal(tb, st.bufferN, buffered, "head accounting matches the buffered columns")
}

// ownerCostSample is one stored sample: its timestamp and exact float bits.
type ownerCostSample struct {
	timestamp int64
	bits      uint64
}

// expectedOwnerView renders the fixture's expected QueryMetadata view of one owner.
func expectedOwnerView(fx *ownerCardinalityFixture, expect ownerCardinalityState, j int) MetadataOwnerView {
	versions := expect.ownerVersions[j]
	view := MetadataOwnerView{
		ID: fx.owners[j].Ref(), Kind: MetadataResource.String(),
		Identifying:      ownerCardinalityIdentity(j),
		HistoryTruncated: expect.truncated[j],
	}
	for i := range versions {
		view.Versions = append(view.Versions, expectedValueView(versions, i))
	}
	return view
}

// expectedValueView renders the fixture's expected validity and observation view.
func expectedValueView(versions []ownerCardinalityVersion, i int) MetadataValueView {
	value := MetadataValueView{
		Descriptive:   versions[i].content.Clone().Attributes,
		EffectiveFrom: versions[i].effectiveFrom,
		ObservedFrom:  versions[i].observedFrom,
		ObservedUntil: versions[i].observedUntil,
	}
	if i+1 < len(versions) {
		next := versions[i+1].effectiveFrom
		value.ValidUntil = &next
	}
	return value
}

// verifyOwnerCardinalityOwnerPage compares the full-range owner query with the fixture.
func verifyOwnerCardinalityOwnerPage(tb testing.TB, st *Storage, fx *ownerCardinalityFixture, expect ownerCardinalityState) {
	tb.Helper()
	page, err := st.QueryMetadata(context.Background(), MetadataQuery{
		Kind: MetadataResource, Start: math.MinInt64, End: math.MaxInt64,
	})
	require.NoError(tb, err)
	if fx.spec.mode == ownerCardinalityNoMetadata {
		require.Empty(tb, page.Owners, "no metadata catalog entries")
		return
	}
	// The page is ordered by the owner key, not by the reference rendered in the view.
	order := make([]int, fx.spec.owners)
	for j := range order {
		order[j] = j
	}
	slices.SortFunc(order, func(a, b int) int {
		switch {
		case fx.owners[a].Kind < fx.owners[b].Kind:
			return -1
		case fx.owners[a].Kind > fx.owners[b].Kind:
			return 1
		case fx.owners[a].ID < fx.owners[b].ID:
			return -1
		case fx.owners[a].ID > fx.owners[b].ID:
			return 1
		}
		return 0
	})
	want := make([]MetadataOwnerView, 0, fx.spec.owners)
	for _, j := range order {
		want = append(want, expectedOwnerView(fx, expect, j))
	}
	require.Equal(tb, want, page.Owners, "full-range owner page")
}

// verifyOwnerCardinalityMembershipPage compares the full-range membership query.
func verifyOwnerCardinalityMembershipPage(tb testing.TB, st *Storage, fx *ownerCardinalityFixture, expect ownerCardinalityState) {
	tb.Helper()
	page, err := st.QueryMetadataSeries(context.Background(), MetadataSeriesQuery{
		Kind: MetadataResource, Start: math.MinInt64, End: math.MaxInt64,
	})
	require.NoError(tb, err)
	if !expect.hasMembership {
		require.Empty(tb, page.Series, "no memberships expected")
		return
	}
	want := make([]MetadataSeriesResult, 0, fx.spec.records)
	for i := 0; i < fx.spec.records; i++ {
		j := fx.spec.ownerIndex(i)
		versions := expect.ownerVersions[j]
		result := MetadataSeriesResult{
			Labels:               ownerCardinalityLabels(i, j, fx.owners[j], ownerCostSumName),
			Owner:                fx.owners[j].Ref(),
			Kind:                 MetadataResource.String(),
			OwnerIdentifying:     ownerCardinalityIdentity(j),
			MembershipStart:      expect.membershipEffective,
			MembershipObservedTo: expect.membershipObservedUntil,
		}
		for k := range versions {
			result.Versions = append(result.Versions, expectedValueView(versions, k))
		}
		want = append(want, result)
	}
	slices.SortFunc(want, func(a, b MetadataSeriesResult) int { return labels.Compare(a.Labels, b.Labels) })
	require.Equal(tb, want, page.Series, "full-range membership page")
}

// verifyOwnerCardinalityBoundaries probes association and content resolution
// immediately before, exactly at and immediately after every boundary the fixture
// states for each owner.
func verifyOwnerCardinalityBoundaries(tb testing.TB, st *Storage, fx *ownerCardinalityFixture, expect ownerCardinalityState) {
	tb.Helper()
	probes := 0
	for j, owner := range fx.owners {
		versions := expect.ownerVersions[j]
		var ls labels.Labels
		for i := 0; i < fx.spec.records; i++ {
			if fx.spec.ownerIndex(i) == j {
				ls = ownerCardinalityLabels(i, j, owner, ownerCostSumName)
				break
			}
		}
		require.NotNil(tb, ls, "owner %d owns at least one record", j)
		probed := map[int64]struct{}{}
		for _, version := range versions {
			for _, t := range []int64{version.effectiveFrom - 1, version.effectiveFrom, version.effectiveFrom + 1} {
				if _, done := probed[t]; done {
					continue
				}
				probed[t] = struct{}{}
				probes++
				gotOwner, associated := st.SeriesMetadataAssociation(metadataSeriesKey(ls), MetadataResource, t)
				gotContent, hasContent, truncated := st.MetadataAtLabels(ls, MetadataResource, t)
				require.Equal(tb, expect.truncated[j], truncated, "owner %d truncation at %d", j, t)
				wantVersion, wantOK := versionAt(versions, t)
				if !wantOK || !expect.hasMembership || t < expect.membershipEffective {
					require.False(tb, associated, "owner %d unassociated at %d", j, t)
					require.False(tb, hasContent, "owner %d has no content at %d", j, t)
					require.Equal(tb, OwnerKey{}, gotOwner)
					continue
				}
				require.True(tb, associated, "owner %d associated at %d", j, t)
				require.Equal(tb, owner, gotOwner, "owner %d association at %d", j, t)
				require.True(tb, hasContent, "owner %d content at %d", j, t)
				require.Equal(tb, wantVersion.content.Canonical(), gotContent.Canonical(), "owner %d content at %d", j, t)
			}
		}
	}
	if fx.spec.mode != ownerCardinalityNoMetadata {
		require.Positive(tb, probes, "the fixture states boundaries to probe")
	}
}

// verifyOwnerCardinalityUnassociated proves the gauge series is intentionally
// unassociated: the direct fixture association belongs to the sum series only.
func verifyOwnerCardinalityUnassociated(tb testing.TB, st *Storage, fx *ownerCardinalityFixture) {
	tb.Helper()
	for i := 0; i < fx.spec.records; i++ {
		j := fx.spec.ownerIndex(i)
		ls := ownerCardinalityLabels(i, j, fx.owners[j], ownerCostGaugeName)
		_, associated := st.SeriesMetadataAssociation(metadataSeriesKey(ls), MetadataResource, fx.spec.base(0))
		require.False(tb, associated, "gauge series %d is unassociated", i)
		_, hasContent, _ := st.MetadataAtLabels(ls, MetadataResource, fx.spec.base(0))
		require.False(tb, hasContent, "gauge series %d has no content", i)
	}
}

// verifyOwnerCardinalityCountersZero proves these direct batches publish no native or
// ordinary derivation accounting, so nothing from those routes can leak into the
// diagnosis.
func verifyOwnerCardinalityCountersZero(tb testing.TB, st *Storage) {
	tb.Helper()
	require.Equal(tb, NativeDerivationStats{}, st.ReadStats().NativeDerivation)
	require.Equal(tb, ordinaryDerivationStats{}, st.reads.ordinaryDerivationSnapshot())
}

// cloneOwnerCardinalityState copies an expectation so a later entry cannot mutate the
// state an earlier entry is asserted against.
func cloneOwnerCardinalityState(state ownerCardinalityState) ownerCardinalityState {
	out := state
	out.truncated = slices.Clone(state.truncated)
	out.ownerVersions = make([][]ownerCardinalityVersion, len(state.ownerVersions))
	for j := range state.ownerVersions {
		out.ownerVersions[j] = slices.Clone(state.ownerVersions[j])
	}
	return out
}

// TestPrimaryIngestOwnerCardinalityState asserts the complete observable state of every
// controlled arm against the fixture-derived expectation.
func TestPrimaryIngestOwnerCardinalityState(t *testing.T) {
	for _, mode := range []ownerCardinalityMode{
		ownerCardinalityNoMetadata, ownerCardinalityUnchanged,
		ownerCardinalityVersionAndAssociations, ownerCardinalityOwnerOnlyVersion,
	} {
		for _, owners := range []int{1, 10, 100, 1_000} {
			t.Run(fmt.Sprintf("mode=%s,owners=%d", mode, owners), func(t *testing.T) {
				fx := buildOwnerCardinalityFixture(t, ownerCostFullSpec(mode, owners, owners))
				st := newOwnerCardinalityStorage(t)
				require.NoError(t, st.AppendBatch(context.Background(), fx.entries[0].batch))
				requireOwnerCardinalityHistories(t, st, fx, fx.entries[0].expect)
				for e := 1; e <= fx.spec.timed; e++ {
					require.NoError(t, st.AppendBatch(context.Background(), fx.entries[e].batch))
					requireOwnerCardinalityHistories(t, st, fx, fx.entries[e].expect)
				}
				verifyOwnerCardinalityState(t, st, fx, fx.spec.timed)
			})
		}
	}
	// The resident-versus-changed-owner arms keep the same complete oracle.
	for _, updated := range []int{1, 10, 100, 1_000} {
		t.Run(fmt.Sprintf("mode=%s,owners=1000,updated=%d", ownerCardinalityVersionAndAssociations, updated), func(t *testing.T) {
			fx := buildOwnerCardinalityFixture(t, ownerCostFullSpec(ownerCardinalityVersionAndAssociations, 1_000, updated))
			st := newOwnerCardinalityStorage(t)
			for e := 0; e <= fx.spec.timed; e++ {
				require.NoError(t, st.AppendBatch(context.Background(), fx.entries[e].batch))
			}
			verifyOwnerCardinalityState(t, st, fx, fx.spec.timed)
		})
	}
}

// TestPrimaryIngestOwnerCardinalityCounts asserts the exact per-entry counts the
// fixture declares: actual versions, observation-only version-bound extensions,
// memberships, unchanged duplicates and evictions, read back from committed state.
func TestPrimaryIngestOwnerCardinalityCounts(t *testing.T) {
	for _, mode := range []ownerCardinalityMode{
		ownerCardinalityNoMetadata, ownerCardinalityUnchanged,
		ownerCardinalityVersionAndAssociations, ownerCardinalityOwnerOnlyVersion,
	} {
		for _, owners := range []int{1, 10, 100, 1_000} {
			t.Run(fmt.Sprintf("mode=%s,owners=%d", mode, owners), func(t *testing.T) {
				fx := buildOwnerCardinalityFixture(t, ownerCostFullSpec(mode, owners, owners))
				st := newOwnerCardinalityStorage(t)
				require.NoError(t, st.AppendBatch(context.Background(), fx.entries[0].batch))
				for e := 1; e <= fx.spec.timed; e++ {
					before := fx.entries[e-1].expect
					after := fx.entries[e].expect
					beforeWork := st.reads.metadataWorkSnapshot()
					require.NoError(t, st.AppendBatch(context.Background(), fx.entries[e].batch))
					work := st.reads.metadataWorkSnapshot().since(beforeWork)

					inserted, extended := 0, 0
					for j, owner := range fx.owners {
						history, ok := st.MetadataHistory(owner)
						if len(after.ownerVersions[j]) == 0 {
							require.False(t, ok, "owner %d has no history in this arm", j)
							continue
						}
						require.True(t, ok, "owner %d has a history", j)
						inserted += len(history.Versions) - len(before.ownerVersions[j])
						for _, wantBefore := range before.ownerVersions[j] {
							for _, got := range history.Versions {
								if got.EffectiveFrom != wantBefore.effectiveFrom {
									continue
								}
								if got.ObservedFrom != wantBefore.observedFrom || got.ObservedUntil != wantBefore.observedUntil {
									extended++
								}
							}
						}
					}
					require.Equal(t, after.versionsInserted-before.versionsInserted, inserted,
						"entry %d inserted versions", e)
					require.Equal(t, after.versionsBoundExtended-before.versionsBoundExtended, extended,
						"entry %d extended versions", e)

					// The committed counters describe the same entry, and no more: one
					// commit, the declared input shape, and the declared applied work.
					require.Equal(t, 1, work.commits, "entry %d commits", e)
					require.Equal(t, fx.spec.samples(), work.inputSamples, "entry %d input samples", e)
					require.Equal(t, fx.spec.series(), work.inputSeries, "entry %d input series", e)
					require.Equal(t, fx.spec.associations(), work.inputAssociations, "entry %d associations", e)
					require.Equal(t, fx.spec.ownerUpdates(), work.inputOwnerUpdates, "entry %d owner updates", e)
					require.Equal(t, inserted, work.versionsInserted, "entry %d counted versions", e)
					require.Equal(t, extended, changedVersionsOf(before, after),
						"entry %d distinct bound extensions", e)
					require.Equal(t, after.versionBoundExtensions-before.versionBoundExtensions,
						work.versionBoundsExtended, "entry %d apply-call extensions", e)
					require.Equal(t, inserted, work.indexCalls,
						"entry %d rebuilds exactly one index per inserted Resource version", e)
					require.Zero(t, work.versionsEvicted, "entry %d evictions", e)
					require.Zero(t, work.versionsCoalesced, "entry %d coalescing", e)
					// Re-observing content several times in one entry extends a version
					// once and repeats the same bounds for every further observation.
					require.Equal(t, work.versionBoundsExtended-changedVersionsOf(before, after),
						work.versionDuplicateNoops, "entry %d duplicate version no-ops", e)
					require.Zero(t, work.membershipDuplicateNoops, "entry %d duplicate membership no-ops", e)
					if fx.spec.mode == ownerCardinalityNoMetadata {
						require.Zero(t, work.metadataTransactions, "entry %d has no metadata transaction", e)
						require.Zero(t, work.clones)
					} else {
						require.Equal(t, 1, work.metadataTransactions, "entry %d metadata transaction", e)
						require.Equal(t, 1, work.clones, "entry %d clone", e)
						require.Equal(t, fx.spec.owners, work.residentOwners, "entry %d resident owners", e)
						wantSeries := 0
						if after.hasMembership {
							wantSeries = fx.spec.records
						}
						require.Equal(t, wantSeries, work.residentSeries, "entry %d resident series", e)
						require.Equal(t, changedOwnersOf(before, after), work.changedOwners,
							"entry %d distinct changed owners", e)
					}

					page, err := st.QueryMetadataSeries(context.Background(), MetadataSeriesQuery{
						Kind: MetadataResource, Start: math.MinInt64, End: math.MaxInt64,
					})
					require.NoError(t, err)
					wantMemberships := 0
					if after.hasMembership {
						wantMemberships = fx.spec.records
					}
					require.Equal(t, after.membershipInserted-before.membershipInserted,
						work.membershipsInserted, "entry %d memberships inserted", e)
					require.Equal(t, after.membershipBoundExtends-before.membershipBoundExtends,
						work.membershipBoundsExtended, "entry %d membership bound extensions", e)
					require.Len(t, page.Series, wantMemberships, "entry %d membership rows", e)
					if after.hasMembership {
						for _, result := range page.Series {
							require.Equal(t, after.membershipEffective, result.MembershipStart)
							require.Equal(t, after.membershipObservedUntil, result.MembershipObservedTo,
								"entry %d membership observation bound", e)
							require.Nil(t, result.MembershipUntil, "the fixture states one membership row per series")
						}
					}
				}
				verifyOwnerCardinalityState(t, st, fx, fx.spec.timed)
			})
		}
	}
}

// changedVersionsOf counts the versions one expected state kept but extended.
func changedVersionsOf(before, after ownerCardinalityState) int {
	extended := 0
	for j := range after.ownerVersions {
		for _, want := range before.ownerVersions[j] {
			for _, got := range after.ownerVersions[j] {
				if got.effectiveFrom != want.effectiveFrom {
					continue
				}
				if got.observedFrom != want.observedFrom || got.observedUntil != want.observedUntil {
					extended++
				}
			}
		}
	}
	return extended
}

// changedOwnersOf counts the owners whose committed history changed between two
// expected states.
func changedOwnersOf(before, after ownerCardinalityState) int {
	changed := 0
	for j := range after.ownerVersions {
		if len(after.ownerVersions[j]) != len(before.ownerVersions[j]) ||
			after.truncated[j] != before.truncated[j] {
			changed++
		}
	}
	return changed
}

// TestPrimaryIngestOwnerCardinalityRejectedCommit proves that same-time conflicting
// content or membership is rejected without samples, metadata, a WAL prefix or a
// committed diagnostic count.
func TestPrimaryIngestOwnerCardinalityRejectedCommit(t *testing.T) {
	fx := buildOwnerCardinalityFixture(t, ownerCardinalitySpec{
		mode: ownerCardinalityVersionAndAssociations, records: 4, points: 4, owners: 2, timed: 1,
	})
	dir := t.TempDir()
	st, err := NewStorage(dir, StorageOptions{FlushSamples: 1 << 40, MetadataVersionCap: -1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	require.NoError(t, st.AppendBatch(context.Background(), fx.entries[0].batch))

	sumLabels := ownerCardinalityLabels(0, 0, fx.owners[0], ownerCostSumName)
	rejectedAt := fx.spec.mid(0)
	scopeOwner := ScopeOwner("sdk", "v1")
	otherScope := ScopeOwner("sdk", "v2")

	for _, tc := range []struct {
		name  string
		err   error
		batch Batch
	}{
		{
			name: "conflicting content at the same effective time",
			err:  ErrConflictingMetadata,
			batch: Batch{OwnerUpdates: []Update{
				{Owner: fx.owners[0], Content: ownerCardinalityContent(0, 7), EffectiveFrom: rejectedAt},
				{Owner: fx.owners[0], Content: ownerCardinalityContent(0, 8), EffectiveFrom: rejectedAt},
			}},
		},
		{
			name: "conflicting content against published state",
			err:  ErrConflictingMetadata,
			batch: Batch{OwnerUpdates: []Update{
				{Owner: fx.owners[1], Content: ownerCardinalityContent(1, 9), EffectiveFrom: rejectedAt},
			}},
		},
		{
			name: "Resource identity label disagrees with the owner key",
			err:  ErrConflictingMembership,
			batch: Batch{Updates: []AssociationUpdate{
				{Labels: sumLabels, Update: Update{
					Owner: fx.owners[1], Content: ownerCardinalityContent(1, 1), EffectiveFrom: fx.spec.base(1),
				}},
			}},
		},
		{
			name: "conflicting membership at the same effective time",
			err:  ErrConflictingMembership,
			// Two different scope owners claimed for one series at one effective
			// time. Scope owners carry no identity label, so the ordered
			// membership rows are the check that rejects it.
			batch: Batch{Updates: []AssociationUpdate{
				{Labels: sumLabels, Update: Update{
					Owner: scopeOwner, Content: Content{Attributes: map[string]string{"schema_url": "a"}},
					EffectiveFrom: fx.spec.base(1),
				}},
				{Labels: sumLabels, Update: Update{
					Owner: otherScope, Content: Content{Attributes: map[string]string{"schema_url": "b"}},
					EffectiveFrom: fx.spec.base(1),
				}},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeSamples := st.bufferN
			beforeSeries := len(st.buffer)
			beforeCatalog := ownerCardinalityCatalogOf(t, st)
			beforeWAL := walBytes(t, dir)
			beforeWork := st.reads.metadataWorkSnapshot()

			err := st.AppendBatch(context.Background(), tc.batch)
			require.ErrorIs(t, err, tc.err)

			afterWork := st.reads.metadataWorkSnapshot()
			require.Equal(t, beforeWork.attempts+1, afterWork.attempts, "the attempt is visible")
			require.Equal(t, ownerCardinalityWorkWithoutAttempts(beforeWork),
				ownerCardinalityWorkWithoutAttempts(afterWork),
				"no committed metadata work is published")

			require.Equal(t, beforeSamples, st.bufferN, "no sample becomes visible")
			require.Len(t, st.buffer, beforeSeries, "no series becomes visible")
			require.Equal(t, beforeCatalog, ownerCardinalityCatalogOf(t, st),
				"no metadata becomes visible")
			require.Equal(t, beforeWAL, walBytes(t, dir), "no WAL prefix is published")
		})
	}
}

// ownerCardinalityCatalog is the complete published catalog: every owner history and
// every membership of both kinds the rejection tests touch.
type ownerCardinalityCatalog struct {
	Owners      MetadataOwnerPage
	Series      MetadataSeriesPage
	ScopeOwners MetadataOwnerPage
	ScopeSeries MetadataSeriesPage
}

// ownerCardinalityCatalogOf reads the complete catalog so a rejected commit can be
// compared for total absence of effect.
func ownerCardinalityCatalogOf(tb testing.TB, st *Storage) ownerCardinalityCatalog {
	tb.Helper()
	owners, err := st.QueryMetadata(context.Background(), MetadataQuery{
		Kind: MetadataResource, Start: math.MinInt64, End: math.MaxInt64,
	})
	require.NoError(tb, err)
	series, err := st.QueryMetadataSeries(context.Background(), MetadataSeriesQuery{
		Kind: MetadataResource, Start: math.MinInt64, End: math.MaxInt64,
	})
	require.NoError(tb, err)
	scopes, err := st.QueryMetadata(context.Background(), MetadataQuery{
		Kind: MetadataScope, Start: math.MinInt64, End: math.MaxInt64,
	})
	require.NoError(tb, err)
	scopeSeries, err := st.QueryMetadataSeries(context.Background(), MetadataSeriesQuery{
		Kind: MetadataScope, Start: math.MinInt64, End: math.MaxInt64,
	})
	require.NoError(tb, err)
	return ownerCardinalityCatalog{
		Owners: owners, Series: series, ScopeOwners: scopes, ScopeSeries: scopeSeries,
	}
}

// ownerCardinalityWorkWithoutAttempts clears the one counter a rejected transaction
// does publish, so the rest can be compared for total absence of effect.
func ownerCardinalityWorkWithoutAttempts(w metadataWork) metadataWork {
	w.attempts = 0
	return w
}

// walBytes sums the sizes of the store's WAL segments.
func walBytes(tb testing.TB, dir string) int64 {
	tb.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, walDirName))
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(tb, err)
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		require.NoError(tb, err)
		total += info.Size()
	}
	return total
}

// newLegacyOwnerVersionFixture reproduces the input of
// BenchmarkPrimaryIngestMetadata/per-series-resources/metadata-version-change at a
// smaller shape: one Resource owner per record, association content stating only the
// identifying attributes, and one owner update per owner stating record 0's identity
// plus deployment.environment. Every owner therefore changes content twice per entry:
// the association restores the identity-only content and the update re-applies the
// descriptive content. The store default version cap applies.
func newLegacyOwnerVersionFixture(tb testing.TB, records, points, timed int) *ownerCardinalityFixture {
	tb.Helper()
	spec := ownerCardinalitySpec{
		mode: ownerCardinalityLegacyRestoringVersion, records: records, points: points,
		owners: records, timed: timed, versionCap: DefaultMetadataVersionCap,
	}
	fx := &ownerCardinalityFixture{spec: spec, owners: make([]OwnerKey, spec.owners)}
	for j := range fx.owners {
		fx.owners[j] = ResourceOwner(ownerCardinalityIdentity(j))
	}
	expect := ownerCardinalityState{
		ownerVersions: make([][]ownerCardinalityVersion, spec.owners),
		truncated:     make([]bool, spec.owners),
	}
	descriptive := func() Content {
		attributes := ownerCardinalityIdentity(0)
		attributes["deployment.environment"] = "prod"
		return Content{Attributes: attributes}
	}
	for e := 0; e <= spec.timed; e++ {
		base, mid := spec.base(e), spec.mid(e)
		entry := ownerCardinalityEntry{}
		for i := 0; i < spec.records; i++ {
			ts := make([]int64, spec.points)
			gauge := make([]float64, spec.points)
			sum := make([]float64, spec.points)
			for p := range ts {
				ts[p] = base + int64(p)*spec.interval()
				gauge[p] = float64(e*1_000_000 + i*100 + p)
				sum[p] = gauge[p] * 100
			}
			entry.batch.Series = append(entry.batch.Series,
				BatchSeries{
					Labels:     ownerCardinalityLabels(i, i, fx.owners[i], ownerCostGaugeName),
					Timestamps: ts,
					Kinds:      ownerCostKinds(spec.points),
					Values:     gauge,
				},
				BatchSeries{
					Labels:     ownerCardinalityLabels(i, i, fx.owners[i], ownerCostSumName),
					Timestamps: ts,
					Kinds:      ownerCostKinds(spec.points),
					Values:     sum,
				})
			identity := Content{Attributes: ownerCardinalityIdentity(i)}
			entry.batch.Updates = append(entry.batch.Updates, AssociationUpdate{
				Labels: ownerCardinalityLabels(i, i, fx.owners[i], ownerCostSumName),
				Update: Update{Owner: fx.owners[i], Content: identity, EffectiveFrom: base},
			})
			observeExpectedVersion(&expect, i, identity, base, spec.versionCap)
			entry.batch.OwnerUpdates = append(entry.batch.OwnerUpdates, Update{
				Owner: fx.owners[i], Content: descriptive(), EffectiveFrom: mid,
			})
			insertExpectedVersion(&expect, i, descriptive(), mid, spec.versionCap)
		}
		if !expect.hasMembership {
			expect.membershipInserted += spec.records
			expect.hasMembership = true
			expect.membershipEffective = base
		}
		expect.membershipObservedUntil = max(expect.membershipObservedUntil, base)
		expect.membershipBoundExtends += spec.records
		entry.expect = cloneOwnerCardinalityState(expect)
		fx.entries = append(fx.entries, entry)
	}
	return fx
}

// TestPrimaryIngestOwnerCardinalityLegacyAccounting records the actual versions,
// memberships, evictions and truncation of the legacy version-change corpus by entry.
// It does not relabel that cell as one actual change per owner.
func TestPrimaryIngestOwnerCardinalityLegacyAccounting(t *testing.T) {
	fx := newLegacyOwnerVersionFixture(t, 8, 4, 8)
	st, err := NewStorage(t.TempDir(), StorageOptions{FlushSamples: 1 << 40, DisableWAL: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	require.NoError(t, st.AppendBatch(context.Background(), fx.entries[0].batch))
	t.Logf("entry=0 associations=%d owner-updates=%d inserted=%d evicted=%d truncated=%v",
		len(fx.entries[0].batch.Updates), len(fx.entries[0].batch.OwnerUpdates),
		fx.entries[0].expect.versionsInserted, fx.entries[0].expect.versionsEvicted,
		fx.entries[0].expect.truncated[0])
	for e := 1; e <= fx.spec.timed; e++ {
		before := fx.entries[e-1].expect
		after := fx.entries[e].expect
		beforeWork := st.reads.metadataWorkSnapshot()
		require.NoError(t, st.AppendBatch(context.Background(), fx.entries[e].batch))
		work := st.reads.metadataWorkSnapshot().since(beforeWork)
		inserted := after.versionsInserted - before.versionsInserted
		evicted := after.versionsEvicted - before.versionsEvicted
		require.Equal(t, 2*fx.spec.records, inserted,
			"entry %d restores the identity content and then re-applies the descriptive content", e)
		require.Zero(t, after.membershipInserted-before.membershipInserted,
			"memberships are stated once per series")
		require.Equal(t, inserted, work.versionsInserted, "entry %d counted versions", e)
		require.Equal(t, evicted, work.versionsEvicted, "entry %d counted evictions", e)
		require.Equal(t, fx.spec.records, work.changedOwners,
			"entry %d changes every owner, twice", e)
		require.Equal(t, inserted, work.indexCalls, "entry %d index rebuilds", e)
		t.Logf("entry=%d associations=%d owner-updates=%d inserted=%d evicted=%d resident=%d "+
			"index-calls=%d bucket-clones=%d bucket-entries=%d clone-owners=%d",
			e, len(fx.entries[e].batch.Updates), len(fx.entries[e].batch.OwnerUpdates),
			inserted, evicted, len(after.ownerVersions[0]),
			work.indexCalls, work.indexBucketClones, work.indexBucketClonedEntries, work.cloneByOwnerEntries)
		requireOwnerCardinalityHistories(t, st, fx, after)
	}
	requireOwnerCardinalityResident(t, st, fx, fx.spec.timed)
	page, err := st.QueryMetadataSeries(context.Background(), MetadataSeriesQuery{
		Kind: MetadataResource, Start: math.MinInt64, End: math.MaxInt64,
	})
	require.NoError(t, err)
	require.Len(t, page.Series, fx.spec.records)
	require.Equal(t, fx.spec.base(fx.spec.timed), page.Series[0].MembershipObservedTo)
}

// TestPrimaryIngestOwnerCardinalityLifecycle proves that observation-only publication
// stays an append-only delta after an explicit flush, and that WAL replay after flush,
// close and reopen preserves histories, samples, memberships and observation bounds.
func TestPrimaryIngestOwnerCardinalityLifecycle(t *testing.T) {
	spec := ownerCardinalitySpec{
		mode: ownerCardinalityVersionAndAssociations, records: 4, points: 4, owners: 2, timed: 3,
	}
	fx := buildOwnerCardinalityFixture(t, spec)
	dir := t.TempDir()
	st, err := NewStorage(dir, StorageOptions{FlushSamples: 1 << 40, MetadataVersionCap: -1})
	require.NoError(t, err)
	closed := false
	t.Cleanup(func() {
		if !closed {
			require.NoError(t, st.Close())
		}
	})

	require.NoError(t, st.AppendBatch(context.Background(), fx.entries[0].batch))
	require.NoError(t, st.AppendBatch(context.Background(), fx.entries[1].batch))
	require.NoError(t, st.Flush())
	require.NotNil(t, st.metadataPublished, "the first flush publishes a snapshot")

	// An unchanged entry and one version entry after a flush must only append: no
	// replacement sets, no re-ordering, no membership rewrite.
	require.NoError(t, st.AppendBatch(context.Background(), fx.entries[2].batch))
	expect := fx.entries[2].expect
	delta := st.metadata.snapshot().pendingSince(st.metadataPublished)
	require.NotEmpty(t, delta.versions, "observation-only bounds publish rows")
	for _, owner := range delta.owners {
		require.False(t, owner.replace, "a delta never replaces an owner history")
	}
	for _, version := range delta.versions {
		wantVersion, ok := versionAt(expect.ownerVersions[0], version.effectiveFrom)
		require.True(t, ok, "every delta version restates a retained version")
		require.Equal(t, wantVersion.observedUntil, version.observedUntil, "delta rows state the new bound")
	}
	require.NotEmpty(t, delta.memberships, "membership bounds publish rows")
	for _, membership := range delta.memberships {
		require.False(t, membership.replace, "a delta never replaces a membership group")
		require.Equal(t, expect.membershipObservedUntil, membership.observedUntil)
	}
	// Every delta version row restates a change point the fixture already published.
	published := map[int64]struct{}{}
	for _, version := range expect.ownerVersions[0] {
		published[version.effectiveFrom] = struct{}{}
	}
	for _, version := range delta.versions {
		_, ok := published[version.effectiveFrom]
		require.True(t, ok, "the delta adds no unreferenced change point")
	}

	require.NoError(t, st.Flush())
	require.NoError(t, st.AppendBatch(context.Background(), fx.entries[3].batch))
	require.NoError(t, st.Close())
	closed = true

	reopened, err := NewStorage(dir, StorageOptions{FlushSamples: 1 << 40, MetadataVersionCap: -1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	verifyOwnerCardinalityState(t, reopened, fx, fx.spec.timed)
}

// TestPrimaryIngestOwnerCardinalityTruncation proves truncation stays exact under a
// small explicit version cap.
func TestPrimaryIngestOwnerCardinalityTruncation(t *testing.T) {
	const versionCap = 3
	spec := ownerCardinalitySpec{
		mode: ownerCardinalityVersionAndAssociations, records: 4, points: 4, owners: 2,
		timed: 3, versionCap: versionCap,
	}
	fx := buildOwnerCardinalityFixture(t, spec)
	st, err := NewStorage(t.TempDir(), StorageOptions{
		FlushSamples: 1 << 40, DisableWAL: true, MetadataVersionCap: versionCap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	for e := 0; e <= spec.timed; e++ {
		require.NoError(t, st.AppendBatch(context.Background(), fx.entries[e].batch))
		requireOwnerCardinalityHistories(t, st, fx, fx.entries[e].expect)
	}
	for j, owner := range fx.owners {
		history, ok := st.MetadataHistory(owner)
		require.True(t, ok)
		require.Len(t, history.Versions, versionCap, "owner %d is capped", j)
		require.True(t, history.Truncated, "owner %d reports truncation rather than a silent current value", j)
		want := fx.entries[spec.timed].expect.ownerVersions[j]
		require.Equal(t, want[len(want)-1].effectiveFrom, history.Versions[versionCap-1].EffectiveFrom,
			"the retained versions are the newest ones")
	}
}

// TestPrimaryIngestOwnerCardinalityInfo proves that info() enriches the associated sum
// series from the fixture's Resource content and leaves the unassociated gauge series
// alone.
func TestPrimaryIngestOwnerCardinalityInfo(t *testing.T) {
	spec := ownerCardinalitySpec{
		mode: ownerCardinalityUnchanged, records: 4, points: 4, owners: 2, timed: 1,
	}
	fx := buildOwnerCardinalityFixture(t, spec)
	st := newOwnerCardinalityStorage(t)
	for e := 0; e <= spec.timed; e++ {
		require.NoError(t, st.AppendBatch(context.Background(), fx.entries[e].batch))
	}

	engine := promqltest.NewTestEngine(t, false, 5*time.Minute, 50_000)
	at := time.UnixMilli(spec.mid(1))

	// The sum series is associated, so info() states the owner's content as labels.
	query, err := engine.NewInstantQuery(t.Context(), st, nil, fmt.Sprintf(`info(%s{series="1"})`, ownerCostSumName), at)
	require.NoError(t, err)
	result := query.Exec(t.Context())
	require.NoError(t, result.Err)
	vector, ok := result.Value.(promql.Vector)
	require.True(t, ok)
	require.Len(t, vector, 1)
	want := labels.NewBuilder(ownerCardinalityLabels(1, 1, fx.owners[1], ownerCostSumName)).
		Set("service.name", "checkout").
		Set("service.instance.id", "instance-1").
		Set("deployment.environment", "env-0").
		Labels()
	require.Equal(t, want.String(), vector[0].Metric.String(), "the associated series is enriched from its owner")
	query.Close()

	// The gauge series is intentionally unassociated: no fixture owner is claimed and
	// no attribute is invented for it.
	query, err = engine.NewInstantQuery(t.Context(), st, nil, fmt.Sprintf(`info(%s{series="1"})`, ownerCostGaugeName), at)
	require.NoError(t, err)
	result = query.Exec(t.Context())
	require.NoError(t, result.Err)
	vector, ok = result.Value.(promql.Vector)
	require.True(t, ok)
	require.Len(t, vector, 1)
	require.Equal(t, ownerCardinalityLabels(1, 1, fx.owners[1], ownerCostGaugeName).String(),
		vector[0].Metric.String(), "the unassociated series is not enriched")
	query.Close()
}

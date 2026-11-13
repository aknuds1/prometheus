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

import (
	"math"
	"time"

	"go.uber.org/atomic"
)

// ReadStats instruments physical reads made by the columnar backend. Metadata
// opens are distinct from sample projections and label-only reads so callers can
// test pruning instead of relying on timing alone.
type ReadStats struct {
	Timings              QueryTimingStats
	LabelPayloadOpens    uint64
	SamplePayloadOpens   uint64
	MetadataPayloadOpens uint64
	RowGroupsRead        uint64
	SampleRowsDecoded    uint64
	SampleBatchesRead    uint64
	// PayloadBytesRead and SegmentsRead prove that segmentation pruning is real:
	// both scale with matched series, and are zero when nothing matches.
	PayloadBytesRead uint64
	SegmentsRead     uint64
	// SeriesMatched, SegmentsAvailable, SegmentsPlanned and SegmentsOutsideWindow
	// describe the cost model of one query: how many series matcher resolution
	// kept, how many segments those series hold, how many survived the per-series
	// time bound, and how many were therefore skipped without being read.
	SeriesMatched         uint64
	SegmentsAvailable     uint64
	SegmentsPlanned       uint64
	SegmentsOutsideWindow uint64
	BlocksOverlapping     uint64
	// HeadIndexHits and HeadSeriesScanned show whether a head selection was
	// narrowed by metric name, and how many series it still had to examine.
	HeadIndexHits     uint64
	HeadSeriesScanned uint64
	// PostingsScanned, PostingsNameMatched, MatcherCalls and SelectCalls exist to
	// explain one discrepancy: a regex name matcher is measured at ~6 ms per block,
	// which reading the resolution loop does not account for. Multiplicity is a
	// candidate explanation, hence call counts alongside loop counts.
	PostingsScanned      uint64
	PostingsNameMatched  uint64
	MatcherCalls         uint64
	PostingsValueMatched uint64
	SelectCalls          uint64
	Queries              uint64
	// CommitCount/HoldNanos cover AppendBatch, which holds the mutex for the whole
	// append and any inline seal. AppendLockWaits cover the scalar append path,
	// which takes the same mutex per sample - so a seal blocks every concurrent
	// append, not only a handful of commits.
	CommitCount            uint64
	CommitAcquireWaitNanos uint64
	CommitAcquireWaitMax   uint64
	CommitHoldNanos        uint64
	CommitHoldMax          uint64
	AppendLockWaits        uint64
	AppendLockWaitNanos    uint64
	AppendLockWaitMax      uint64
	SealCount              uint64
	SealNanos              uint64
	LastSealNanos          uint64
	// Blocks, BlockMinTime and BlockMaxTime describe the sealed corpus at read
	// time. An overlap count is only interpretable alongside the block duration
	// that produces it, and duration is what a sample-count threshold stops
	// controlling as cardinality grows.
	Blocks                  int
	BlockMinTime            int64
	BlockMaxTime            int64
	LabelRowsVisited        uint64
	MetadataIndexCandidates uint64
	MetadataIndexFallbacks  uint64
	// HashCallsSeriesFor, HashCallsRefFor and HashCallsPrepare count label
	// hashes at each remaining call site of the ingestion path. Together with the
	// reference counters they prove that a hash or lookup stopped happening,
	// which a timing delta alone cannot.
	HashCallsSeriesFor uint64
	HashCallsRefFor    uint64
	HashCallsPrepare   uint64
	// RefTrusted counts appends that reused a verified series reference;
	// RefFallback counts appends whose reference was absent or unverified and
	// therefore hashed.
	RefTrusted  uint64
	RefFallback uint64
	// NativeDerivation is the committed native OTAP association builder's mechanism
	// accounting: which derivations run per sample and which run per distinct
	// context, plus the ordered-fold work that is retained for correctness.
	NativeDerivation NativeDerivationStats
}

// QueryTimingStats reports accumulated phase durations in nanoseconds. Callers
// difference two samples around a burst of queries; Queries gives the divisor.
type QueryTimingStats struct {
	SelectNanos  int64
	MatchNanos   int64
	PlanNanos    int64
	ReadNanos    int64
	DecodeNanos  int64
	DrainNanos   int64
	HeadNanos    int64
	ScalarNanos  int64
	UnionNanos   int64
	MatcherNanos int64
	DrainCalls   uint64
	Enabled      bool
}

type readStats struct {
	timings            *queryTimings
	labels             atomic.Uint64
	samples            atomic.Uint64
	metadata           atomic.Uint64
	rowGroups          atomic.Uint64
	sampleRows         atomic.Uint64
	sampleBatches      atomic.Uint64
	payloadBytes       atomic.Uint64
	segmentsRead       atomic.Uint64
	seriesMatched      atomic.Uint64
	segmentsAvailable  atomic.Uint64
	segmentsPlanned    atomic.Uint64
	segmentsOutsideWin atomic.Uint64
	blocksOverlapping  atomic.Uint64
	headIndexHits      atomic.Uint64
	headScanned        atomic.Uint64
	postingsScanned    atomic.Uint64
	postingsByName     atomic.Uint64
	matcherCalls       atomic.Uint64
	postingsValueMatch atomic.Uint64
	selectCalls        atomic.Uint64
	queries            atomic.Uint64
	commits            atomic.Uint64
	commitAcquireNanos atomic.Uint64
	commitAcquireMax   atomic.Uint64
	commitHoldNanos    atomic.Uint64
	commitHoldMax      atomic.Uint64
	appendLockWaits    atomic.Uint64
	appendLockNanos    atomic.Uint64
	appendLockMax      atomic.Uint64
	seals              atomic.Uint64
	sealNanos          atomic.Uint64
	lastSealNanos      atomic.Uint64
	labelRows          atomic.Uint64
	metadataCandidates atomic.Uint64
	metadataFallbacks  atomic.Uint64
	hashSeriesFor      atomic.Uint64
	hashRefFor         atomic.Uint64
	hashPrepare        atomic.Uint64
	refTrusted         atomic.Uint64
	refFallback        atomic.Uint64
	native             nativeDerivationAccumulator
}

// nativeDerivationAccumulator aggregates committed native builder accounting. It
// is written once per commit under the storage lock and read by a snapshot, so a
// field per counter keeps publication a fixed number of atomic adds.
type nativeDerivationAccumulator struct {
	batches, samples, series, resources           atomic.Uint64
	ownersByKind                                  [6]atomic.Uint64
	ownerRevisionChanges                          atomic.Uint64
	seriesLookupHashes, seriesLookupHits          atomic.Uint64
	seriesLookupNew                               atomic.Uint64
	assocKeyHashes                                [NativeDerivationAssociationSites]atomic.Uint64
	contentCanonicalizations, contentDerivations  atomic.Uint64
	schemaContentDerivations                      atomic.Uint64
	ownerConstructors                             [6]atomic.Uint64
	ownerReferences, labelBuilds                  atomic.Uint64
	labelNameHits, labelNameMisses                atomic.Uint64
	translationHits, translationMisses            atomic.Uint64
	epochOwnerChecks, epochMembershipChecks       atomic.Uint64
	emittedChangePoints, emittedObservationBounds atomic.Uint64
	foldedObservations, epochRestatements         atomic.Uint64
}

func (a *nativeDerivationAccumulator) add(c NativeDerivationStats) {
	a.batches.Add(c.Batches)
	a.samples.Add(c.Samples)
	a.series.Add(c.Series)
	a.resources.Add(c.Resources)
	for i := range c.OwnersByKind {
		a.ownersByKind[i].Add(c.OwnersByKind[i])
		a.ownerConstructors[i].Add(c.OwnerConstructors[i])
	}
	for i := range c.AssocKeyHashes {
		a.assocKeyHashes[i].Add(c.AssocKeyHashes[i])
	}
	a.ownerRevisionChanges.Add(c.OwnerRevisionChanges)
	a.seriesLookupHashes.Add(c.SeriesLookupHashes)
	a.seriesLookupHits.Add(c.SeriesLookupHits)
	a.seriesLookupNew.Add(c.SeriesLookupNew)
	a.contentCanonicalizations.Add(c.ContentCanonicalizations)
	a.contentDerivations.Add(c.ContentDerivations)
	a.schemaContentDerivations.Add(c.SchemaContentDerivations)
	a.ownerReferences.Add(c.OwnerReferences)
	a.labelBuilds.Add(c.LabelBuilds)
	a.labelNameHits.Add(c.LabelNameHits)
	a.labelNameMisses.Add(c.LabelNameMisses)
	a.translationHits.Add(c.TranslationHits)
	a.translationMisses.Add(c.TranslationMisses)
	a.epochOwnerChecks.Add(c.EpochOwnerChecks)
	a.epochMembershipChecks.Add(c.EpochMembershipChecks)
	a.emittedChangePoints.Add(c.EmittedChangePoints)
	a.emittedObservationBounds.Add(c.EmittedObservationBounds)
	a.foldedObservations.Add(c.FoldedObservations)
	a.epochRestatements.Add(c.EpochRestatements)
}

func (a *nativeDerivationAccumulator) snapshot() NativeDerivationStats {
	out := NativeDerivationStats{
		Batches:                  a.batches.Load(),
		Samples:                  a.samples.Load(),
		Series:                   a.series.Load(),
		Resources:                a.resources.Load(),
		OwnerRevisionChanges:     a.ownerRevisionChanges.Load(),
		SeriesLookupHashes:       a.seriesLookupHashes.Load(),
		SeriesLookupHits:         a.seriesLookupHits.Load(),
		SeriesLookupNew:          a.seriesLookupNew.Load(),
		ContentCanonicalizations: a.contentCanonicalizations.Load(),
		ContentDerivations:       a.contentDerivations.Load(),
		SchemaContentDerivations: a.schemaContentDerivations.Load(),
		OwnerReferences:          a.ownerReferences.Load(),
		LabelBuilds:              a.labelBuilds.Load(),
		LabelNameHits:            a.labelNameHits.Load(),
		LabelNameMisses:          a.labelNameMisses.Load(),
		TranslationHits:          a.translationHits.Load(),
		TranslationMisses:        a.translationMisses.Load(),
		EpochOwnerChecks:         a.epochOwnerChecks.Load(),
		EpochMembershipChecks:    a.epochMembershipChecks.Load(),
		EmittedChangePoints:      a.emittedChangePoints.Load(),
		EmittedObservationBounds: a.emittedObservationBounds.Load(),
		FoldedObservations:       a.foldedObservations.Load(),
		EpochRestatements:        a.epochRestatements.Load(),
	}
	for i := range out.OwnersByKind {
		out.OwnersByKind[i] = a.ownersByKind[i].Load()
		out.OwnerConstructors[i] = a.ownerConstructors[i].Load()
	}
	for i := range out.AssocKeyHashes {
		out.AssocKeyHashes[i] = a.assocKeyHashes[i].Load()
	}
	return out
}

func (a *nativeDerivationAccumulator) reset() {
	a.batches.Store(0)
	a.samples.Store(0)
	a.series.Store(0)
	a.resources.Store(0)
	for i := range a.ownersByKind {
		a.ownersByKind[i].Store(0)
		a.ownerConstructors[i].Store(0)
	}
	for i := range a.assocKeyHashes {
		a.assocKeyHashes[i].Store(0)
	}
	a.ownerRevisionChanges.Store(0)
	a.seriesLookupHashes.Store(0)
	a.seriesLookupHits.Store(0)
	a.seriesLookupNew.Store(0)
	a.contentCanonicalizations.Store(0)
	a.contentDerivations.Store(0)
	a.schemaContentDerivations.Store(0)
	a.ownerReferences.Store(0)
	a.labelBuilds.Store(0)
	a.labelNameHits.Store(0)
	a.labelNameMisses.Store(0)
	a.translationHits.Store(0)
	a.translationMisses.Store(0)
	a.epochOwnerChecks.Store(0)
	a.epochMembershipChecks.Store(0)
	a.emittedChangePoints.Store(0)
	a.emittedObservationBounds.Store(0)
	a.foldedObservations.Store(0)
	a.epochRestatements.Store(0)
}

func (s *Storage) ReadStats() ReadStats {
	s.mtx.Lock()
	blocks := len(s.blocks)
	blockMin, blockMax := int64(math.MaxInt64), int64(math.MinInt64)
	for _, b := range s.blocks {
		if b.meta.MinTime < blockMin {
			blockMin = b.meta.MinTime
		}
		if b.meta.MaxTime > blockMax {
			blockMax = b.meta.MaxTime
		}
	}
	s.mtx.Unlock()

	return ReadStats{
		Blocks:                  blocks,
		BlockMinTime:            blockMin,
		BlockMaxTime:            blockMax,
		LabelPayloadOpens:       s.reads.labels.Load(),
		SamplePayloadOpens:      s.reads.samples.Load(),
		MetadataPayloadOpens:    s.reads.metadata.Load(),
		RowGroupsRead:           s.reads.rowGroups.Load(),
		SampleRowsDecoded:       s.reads.sampleRows.Load(),
		SampleBatchesRead:       s.reads.sampleBatches.Load(),
		PayloadBytesRead:        s.reads.payloadBytes.Load(),
		SegmentsRead:            s.reads.segmentsRead.Load(),
		SeriesMatched:           s.reads.seriesMatched.Load(),
		SegmentsAvailable:       s.reads.segmentsAvailable.Load(),
		SegmentsPlanned:         s.reads.segmentsPlanned.Load(),
		SegmentsOutsideWindow:   s.reads.segmentsOutsideWin.Load(),
		BlocksOverlapping:       s.reads.blocksOverlapping.Load(),
		HeadIndexHits:           s.reads.headIndexHits.Load(),
		HeadSeriesScanned:       s.reads.headScanned.Load(),
		PostingsScanned:         s.reads.postingsScanned.Load(),
		PostingsNameMatched:     s.reads.postingsByName.Load(),
		MatcherCalls:            s.reads.matcherCalls.Load(),
		PostingsValueMatched:    s.reads.postingsValueMatch.Load(),
		SelectCalls:             s.reads.selectCalls.Load(),
		Queries:                 s.reads.queries.Load(),
		CommitCount:             s.reads.commits.Load(),
		CommitAcquireWaitNanos:  s.reads.commitAcquireNanos.Load(),
		CommitAcquireWaitMax:    s.reads.commitAcquireMax.Load(),
		CommitHoldNanos:         s.reads.commitHoldNanos.Load(),
		CommitHoldMax:           s.reads.commitHoldMax.Load(),
		AppendLockWaits:         s.reads.appendLockWaits.Load(),
		AppendLockWaitNanos:     s.reads.appendLockNanos.Load(),
		AppendLockWaitMax:       s.reads.appendLockMax.Load(),
		SealCount:               s.reads.seals.Load(),
		SealNanos:               s.reads.sealNanos.Load(),
		LastSealNanos:           s.reads.lastSealNanos.Load(),
		Timings:                 s.ReadTimings(),
		LabelRowsVisited:        s.reads.labelRows.Load(),
		MetadataIndexCandidates: s.reads.metadataCandidates.Load(),
		MetadataIndexFallbacks:  s.reads.metadataFallbacks.Load(),
		HashCallsSeriesFor:      s.reads.hashSeriesFor.Load(),
		HashCallsRefFor:         s.reads.hashRefFor.Load(),
		HashCallsPrepare:        s.reads.hashPrepare.Load(),
		RefTrusted:              s.reads.refTrusted.Load(),
		RefFallback:             s.reads.refFallback.Load(),
		NativeDerivation:        s.reads.native.snapshot(),
	}
}

// ReadTimings reports the accumulated phase durations.
func (s *Storage) ReadTimings() QueryTimingStats {
	t := s.reads.timings
	return QueryTimingStats{
		SelectNanos:  t.selectTotal.Load(),
		MatchNanos:   t.match.Load(),
		PlanNanos:    t.plan.Load(),
		ReadNanos:    t.read.Load(),
		DecodeNanos:  t.decode.Load(),
		DrainNanos:   t.drain.Load(),
		HeadNanos:    t.headSelect.Load(),
		ScalarNanos:  t.scalar.Load(),
		UnionNanos:   t.union.Load(),
		MatcherNanos: t.matcherMatch.Load(),
		DrainCalls:   t.drainCalls.Load(),
		Enabled:      t.Enabled(),
	}
}

// reset is intentionally test/package-internal; individual callers may be
// reading concurrently, so tests reset only when no other query is active.
func (r *readStats) reset() {
	r.labels.Store(0)
	r.samples.Store(0)
	r.metadata.Store(0)
	r.rowGroups.Store(0)
	r.sampleRows.Store(0)
	r.sampleBatches.Store(0)
	r.payloadBytes.Store(0)
	r.segmentsRead.Store(0)
	r.seriesMatched.Store(0)
	r.segmentsAvailable.Store(0)
	r.segmentsPlanned.Store(0)
	r.segmentsOutsideWin.Store(0)
	r.blocksOverlapping.Store(0)
	r.headIndexHits.Store(0)
	r.headScanned.Store(0)
	r.postingsScanned.Store(0)
	r.postingsByName.Store(0)
	r.matcherCalls.Store(0)
	r.postingsValueMatch.Store(0)
	r.selectCalls.Store(0)
	r.queries.Store(0)
	r.commits.Store(0)
	r.commitAcquireNanos.Store(0)
	r.commitAcquireMax.Store(0)
	r.commitHoldNanos.Store(0)
	r.commitHoldMax.Store(0)
	r.appendLockWaits.Store(0)
	r.appendLockNanos.Store(0)
	r.appendLockMax.Store(0)
	r.seals.Store(0)
	r.sealNanos.Store(0)
	r.lastSealNanos.Store(0)
	r.labelRows.Store(0)
	r.metadataCandidates.Store(0)
	r.metadataFallbacks.Store(0)
	r.hashSeriesFor.Store(0)
	r.hashRefFor.Store(0)
	r.hashPrepare.Store(0)
	r.refTrusted.Store(0)
	r.refFallback.Store(0)
	r.native.reset()
}

// atomicMax stores v if it exceeds the current value.
func atomicMax(a *atomic.Uint64, v uint64) {
	for {
		cur := a.Load()
		if v <= cur || a.CompareAndSwap(cur, v) {
			return
		}
	}
}

// observeAppendLockWait records one scalar-append mutex acquisition.
func (r *readStats) observeAppendLockWait(d time.Duration) {
	n := uint64(d.Nanoseconds())
	r.appendLockWaits.Inc()
	r.appendLockNanos.Add(n)
	atomicMax(&r.appendLockMax, n)
}

// observeCommit records one AppendBatch mutex acquisition. The hold is recorded
// separately when the lock is released, because the seal it may contain happens
// while the lock is held rather than being waited for.
func (r *readStats) observeCommit(acquire time.Duration) {
	a := uint64(acquire.Nanoseconds())
	r.commits.Inc()
	r.commitAcquireNanos.Add(a)
	atomicMax(&r.commitAcquireMax, a)
}

func (r *readStats) observeCommitHold(h time.Duration) {
	n := uint64(h.Nanoseconds())
	r.commitHoldNanos.Add(n)
	atomicMax(&r.commitHoldMax, n)
}

// observeIngestCounts publishes one transaction's mechanism accounting with a
// fixed number of atomic adds, independent of the number of samples.
func (r *readStats) observeIngestCounts(c ingestCounts) {
	r.hashSeriesFor.Add(uint64(c.seriesForHash))
	r.hashRefFor.Add(uint64(c.refForHash))
	r.hashPrepare.Add(uint64(c.prepareHash))
	r.refTrusted.Add(uint64(c.refTrusted))
	r.refFallback.Add(uint64(c.refFallback))
	r.observeNativeDerivation(c.native)
}

// observeNativeDerivation adds one committed native batch's accounting.
func (r *readStats) observeNativeDerivation(c NativeDerivationStats) {
	r.native.add(c)
}

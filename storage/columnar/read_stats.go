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

import "go.uber.org/atomic"

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
	SeriesMatched           uint64
	SegmentsAvailable       uint64
	SegmentsPlanned         uint64
	SegmentsOutsideWindow   uint64
	BlocksOverlapping       uint64
	Queries                 uint64
	LabelRowsVisited        uint64
	MetadataIndexCandidates uint64
	MetadataIndexFallbacks  uint64
}

// QueryTimingStats reports accumulated phase durations in nanoseconds. Callers
// difference two samples around a burst of queries; Queries gives the divisor.
type QueryTimingStats struct {
	SelectNanos int64
	MatchNanos  int64
	PlanNanos   int64
	ReadNanos   int64
	DecodeNanos int64
	DrainNanos  int64
	DrainCalls  uint64
	Enabled     bool
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
	queries            atomic.Uint64
	labelRows          atomic.Uint64
	metadataCandidates atomic.Uint64
	metadataFallbacks  atomic.Uint64
}

func (s *Storage) ReadStats() ReadStats {
	return ReadStats{
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
		Queries:                 s.reads.queries.Load(),
		Timings:                 s.ReadTimings(),
		LabelRowsVisited:        s.reads.labelRows.Load(),
		MetadataIndexCandidates: s.reads.metadataCandidates.Load(),
		MetadataIndexFallbacks:  s.reads.metadataFallbacks.Load(),
	}
}

// ReadTimings reports the accumulated phase durations.
func (s *Storage) ReadTimings() QueryTimingStats {
	t := s.reads.timings
	return QueryTimingStats{
		SelectNanos: t.selectTotal.Load(),
		MatchNanos:  t.match.Load(),
		PlanNanos:   t.plan.Load(),
		ReadNanos:   t.read.Load(),
		DecodeNanos: t.decode.Load(),
		DrainNanos:  t.drain.Load(),
		DrainCalls:  t.drainCalls.Load(),
		Enabled:     t.Enabled(),
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
	r.queries.Store(0)
	r.labelRows.Store(0)
	r.metadataCandidates.Store(0)
	r.metadataFallbacks.Store(0)
}

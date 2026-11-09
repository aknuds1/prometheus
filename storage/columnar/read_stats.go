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
	LabelPayloadOpens       uint64
	SamplePayloadOpens      uint64
	MetadataPayloadOpens    uint64
	RowGroupsRead           uint64
	SampleRowsDecoded       uint64
	LabelRowsVisited        uint64
	MetadataIndexCandidates uint64
	MetadataIndexFallbacks  uint64
}

type readStats struct {
	labels             atomic.Uint64
	samples            atomic.Uint64
	metadata           atomic.Uint64
	rowGroups          atomic.Uint64
	sampleRows         atomic.Uint64
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
		LabelRowsVisited:        s.reads.labelRows.Load(),
		MetadataIndexCandidates: s.reads.metadataCandidates.Load(),
		MetadataIndexFallbacks:  s.reads.metadataFallbacks.Load(),
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
	r.labelRows.Store(0)
	r.metadataCandidates.Store(0)
	r.metadataFallbacks.Store(0)
}

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

//go:build linux || darwin

package remote

import (
	"strings"

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/nativemetadata"
	"github.com/prometheus/prometheus/tsdb/record"
)

// The distinct-store diagnostic's adapter for A0: merge groups in Metadata
// records, whose points the store resolves one by one, in entry order.

func dsUnavailable(string, string) string { return "" }

// dsStoreWith stores d's entries as StoreBorrowedNativeMetadata does,
// resolving values with intern.
func dsStoreWith(d *w1Decoder, qm *QueueManager, intern func(metadata.Metadata) *metadata.Metadata) {
	nativeMetadataWriter{qm}.storeNativeMetadata(d.entries, intern)
}

// dsSplit is storeNativeMetadata in two phases. Its slice of resolved values,
// one per point, is the split's only storage beyond the production loop's.
type dsSplit struct {
	resolved []*metadata.Metadata
}

// resolve resolves every point of d's entries, in the order the store would
// while applying them.
func (s *dsSplit) resolve(d *w1Decoder, qm *QueueManager, intern func(metadata.Metadata) *metadata.Metadata) {
	qm.seriesMtx.Lock()
	defer qm.seriesMtx.Unlock()
	s.resolved = s.resolved[:0]
	for _, e := range d.entries {
		if e.Ref == 0 {
			continue
		}
		for _, p := range e.Points {
			s.resolved = append(s.resolved, intern(metadata.Metadata{Type: record.ToMetricType(p.Type), Unit: p.Unit, Help: p.Help}))
		}
	}
}

// apply applies d's entries with the resolved values, as storeNativeMetadata
// does.
func (s *dsSplit) apply(d *w1Decoder, qm *QueueManager) {
	t := qm
	t.seriesMtx.Lock()
	defer t.seriesMtx.Unlock()
	k := 0
	for _, e := range d.entries {
		if e.Ref == 0 {
			continue
		}
		t.nativePoints = t.nativePoints[:0]
		for _, p := range e.Points {
			t.nativePoints = append(t.nativePoints, nativemetadata.Point{EffectiveFrom: p.EffectiveFrom, Metadata: s.resolved[k]})
			k++
		}
		state := t.seriesNativeMetadata[e.Ref]
		if state.Apply(e.Kind, e.Truncated, t.nativePoints) {
			t.metrics.unknownMetadataTotal.Inc()
		}
		t.seriesNativeMetadata[e.Ref] = state
	}
	clear(t.nativePoints)
	clear(s.resolved)
}

// dsSightings returns the values the store resolves for d's entries, one per
// point in entry order, as owned copies.
func dsSightings(d *w1Decoder) []dsSighting {
	var out []dsSighting
	for _, e := range d.entries {
		if e.Ref == 0 {
			continue
		}
		for _, p := range e.Points {
			out = append(out, dsSighting{
				value:  metadata.Metadata{Type: record.ToMetricType(p.Type), Unit: strings.Clone(p.Unit), Help: strings.Clone(p.Help)},
				points: []dsPointRef{{ref: e.Ref, from: p.EffectiveFrom}},
			})
		}
	}
	return out
}

// dsDecodedRefs returns the refs of d's entries.
func dsDecodedRefs(d *w1Decoder) []chunks.HeadSeriesRef {
	refs := make([]chunks.HeadSeriesRef, 0, len(d.entries))
	for _, e := range d.entries {
		refs = append(refs, e.Ref)
	}
	return refs
}

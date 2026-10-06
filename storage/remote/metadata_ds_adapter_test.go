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

// The distinct-store diagnostic's adapter for W1-V with values inline: the
// store resolves each dictionary value lazily, at its first use, which is
// definition order.

func dsUnavailable(string, string) string { return "" }

// dsStoreWith stores d's record as StoreBorrowedNativeMetadataRecord does,
// resolving values with intern.
func dsStoreWith(d *w1Decoder, qm *QueueManager, intern func(metadata.Metadata) *metadata.Metadata) {
	nativeMetadataWriter{qm}.storeNativeMetadataRecord(&d.compact, intern)
}

// dsSplit is storeNativeMetadataRecord in two phases. Its scratch slices are
// the split's only storage beyond the production loop's.
type dsSplit struct {
	used    []bool
	points  []record.CompactNativeMetadataPoint
	scratch []nativemetadata.Point
}

// resolve resolves the values of d's record that the store uses, in the
// order of their first use, into the queue's dictionary, as the store would
// while applying them.
func (s *dsSplit) resolve(d *w1Decoder, qm *QueueManager, intern func(metadata.Metadata) *metadata.Metadata) {
	rec := &d.compact
	qm.seriesMtx.Lock()
	defer qm.seriesMtx.Unlock()
	qm.nativeDictionary.Reset(rec.Values, intern)
	if cap(s.used) < len(rec.Values) {
		s.used = make([]bool, len(rec.Values))
	}
	s.used = s.used[:len(rec.Values)]
	clear(s.used)
	s.points = s.points[:0]
	for _, e := range rec.Entries {
		if e.Ref == 0 || e.Ignored() {
			continue
		}
		for _, p := range e.Points {
			if !s.used[p.Value] {
				s.used[p.Value] = true
				s.points = append(s.points, record.CompactNativeMetadataPoint{Value: p.Value})
			}
		}
	}
	s.scratch = qm.nativeDictionary.AppendPoints(s.scratch[:0], s.points)
	clear(s.scratch)
}

// apply applies d's entries with the dictionary's resolved values, as
// storeNativeMetadataRecord does.
func (*dsSplit) apply(d *w1Decoder, qm *QueueManager) {
	rec := &d.compact
	t := qm
	t.seriesMtx.Lock()
	defer t.seriesMtx.Unlock()
	for _, e := range rec.Entries {
		if e.Ref == 0 {
			continue
		}
		if e.Ignored() {
			t.metrics.unknownMetadataTotal.Inc()
			continue
		}
		t.nativePoints = t.nativeDictionary.AppendPoints(t.nativePoints[:0], e.Points)
		state := t.seriesNativeMetadata[e.Ref]
		if state.Apply(e.Kind, e.Truncated, t.nativePoints) {
			t.metrics.unknownMetadataTotal.Inc()
		}
		t.seriesNativeMetadata[e.Ref] = state
	}
	clear(t.nativePoints)
	t.nativeDictionary.Reset(nil, nil)
}

// dsSightings returns the values the store resolves for d's record, in its
// order, each with the points it serves, as owned copies.
func dsSightings(d *w1Decoder) []dsSighting {
	rec := &d.compact
	index := make([]int, len(rec.Values))
	var out []dsSighting
	for _, e := range rec.Entries {
		if e.Ref == 0 || e.Ignored() {
			continue
		}
		for _, p := range e.Points {
			if index[p.Value] == 0 {
				v := rec.Values[p.Value]
				out = append(out, dsSighting{value: metadata.Metadata{Type: record.ToMetricType(v.Type), Unit: strings.Clone(v.Unit), Help: strings.Clone(v.Help)}})
				index[p.Value] = len(out)
			}
			s := &out[index[p.Value]-1]
			s.points = append(s.points, dsPointRef{ref: e.Ref, from: p.EffectiveFrom})
		}
	}
	return out
}

// dsDecodedRefs returns the refs of d's record's entries.
func dsDecodedRefs(d *w1Decoder) []chunks.HeadSeriesRef {
	refs := make([]chunks.HeadSeriesRef, 0, len(d.compact.Entries))
	for _, e := range d.compact.Entries {
		refs = append(refs, e.Ref)
	}
	return refs
}

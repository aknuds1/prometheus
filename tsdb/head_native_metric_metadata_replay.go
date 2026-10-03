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

package tsdb

import (
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/nativemetadata"
	"github.com/prometheus/prometheus/tsdb/record"
)

// nativeMetadataReplayInternerLimit bounds the distinct values replay shares.
const nativeMetadataReplayInternerLimit = 1 << 16

// nativeMetadataReplayRecord carries one decoded Metadata record to the replay
// loop. Entries alias points.
type nativeMetadataReplayRecord struct {
	entries []record.RefNativeMetadata
	points  []record.RefNativeMetadataPoint
}

// nativeMetadataReplay reduces native metadata WAL entries during replay, per
// original WAL ref: a retired incarnation may receive entries after its
// successor's Series record. A surviving series' current incarnation is the
// ref of its latest Series record.
type nativeMetadataReplay struct {
	states map[chunks.HeadSeriesRef]nativemetadata.State
	// current maps surviving refs to later duplicate Series records. Without
	// an entry, a surviving series is its own current incarnation.
	current  map[chunks.HeadSeriesRef]chunks.HeadSeriesRef
	interner *nativemetadata.Interner
	points   []nativemetadata.Point
	unknown  uint64
}

func newNativeMetadataReplay() *nativeMetadataReplay {
	return &nativeMetadataReplay{
		states:   map[chunks.HeadSeriesRef]nativemetadata.State{},
		current:  map[chunks.HeadSeriesRef]chunks.HeadSeriesRef{},
		interner: nativemetadata.NewInterner(nativeMetadataReplayInternerLimit),
	}
}

// declare records a Series record for walRef, replayed as series surviving.
func (r *nativeMetadataReplay) declare(surviving, walRef chunks.HeadSeriesRef) {
	if walRef == surviving {
		delete(r.current, surviving)
		return
	}
	r.current[surviving] = walRef
}

// apply reduces entries in WAL order, including entries for refs whose Series
// record has not been replayed yet. Ref 0 never identifies a series.
func (r *nativeMetadataReplay) apply(entries []record.RefNativeMetadata) {
	for _, e := range entries {
		if e.Ref == 0 {
			continue
		}
		r.points = nativemetadata.AppendRecordPoints(r.points[:0], e.Points, r.interner.Intern)
		state := r.states[e.Ref]
		if state.Apply(e.Kind, e.Truncated, r.points) {
			r.unknown++
		}
		r.states[e.Ref] = state
	}
	clear(r.points)
}

// seedNativeMetricMetadata gives each replayed series the newest version of
// its current incarnation's reduced history, as a first commit would. Dropped
// older versions set the truncation flag, as does a truncated history.
// Replay must have finished; multiRef maps duplicate WAL refs to survivors.
func (h *Head) seedNativeMetricMetadata(r *nativeMetadataReplay, multiRef map[chunks.HeadSeriesRef]chunks.HeadSeriesRef) {
	store := h.nativeMetricMetadata
	var seeded int64
	for walRef, state := range r.states {
		if state.Metadata == nil {
			continue
		}
		surviving := walRef
		if ref, ok := multiRef[walRef]; ok {
			surviving = ref
		}
		if current, ok := r.current[surviving]; (ok && current != walRef) || (!ok && surviving != walRef) {
			continue
		}
		series := h.series.getByID(surviving)
		if series == nil {
			continue
		}
		value := store.values.resolve(*state.Metadata)
		series.Lock()
		if series.nativeMetadataLocked() != nil {
			series.Unlock()
			continue
		}
		native := createNativeMetadataLocked(series)
		points := [1]nativeMetricMetadataPoint{{EffectiveFrom: state.EffectiveFrom, Metadata: value}}
		native.mergeLocked(points[:])
		if state.Truncated || len(state.Older) > 0 {
			native.setFlag(nativeMetadataTruncated)
		}
		store.publishLocked(series)
		series.Unlock()
		seeded++
	}
	store.series.Add(seeded)
	store.versions.Add(seeded)
	h.metrics.nativeMetadataUnknownEntries.Add(float64(r.unknown))
}

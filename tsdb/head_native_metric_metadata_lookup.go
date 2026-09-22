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
	"context"
	"slices"
	"sync"
	"unique"

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
)

const maxNativeMetricMetadataLookups = 256

var nativeMetricMetadataLookupPool = sync.Pool{New: func() any { return new(nativeMetricMetadataLookupScratch) }}

// nativeMetricMetadataLookupScratch belongs to one lookup batch until its
// historical results are materialized. Only scratch is pooled, never results.
type nativeMetricMetadataLookupScratch struct {
	historical [maxNativeMetricMetadataLookups]unique.Handle[metadata.Metadata]
	copies     map[unique.Handle[metadata.Metadata]]*metadata.Metadata
}

// materialize copies selected values without accessing Head or holding its locks.
// Handles keep values alive across intervening commits, eviction and series GC.
func (s *nativeMetricMetadataLookupScratch) materialize(ctx context.Context, lookups []storage.NativeMetricMetadataLookup) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// No publication waits remain. Check cancellation at the boundaries of
	// this bounded batch rather than once per copied handle.
	for i, handle := range s.historical[:len(lookups)] {
		if handle == (unique.Handle[metadata.Metadata]{}) {
			continue
		}
		if s.copies == nil {
			s.copies = make(map[unique.Handle[metadata.Metadata]]*metadata.Metadata)
		}
		m := s.copies[handle]
		if m == nil {
			value := handle.Value()
			m = &value
			s.copies[handle] = m
		}
		lookups[i].Metadata = m
	}
	return ctx.Err()
}

func (s *nativeMetricMetadataLookupScratch) reset() {
	clear(s.historical[:])
	clear(s.copies)
}

// LookupNativeMetricMetadata implements storage.NativeMetricMetadataReader.
func (db *DB) LookupNativeMetricMetadata(ctx context.Context, lookups []storage.NativeMetricMetadataLookup) error {
	return db.head.LookupNativeMetricMetadata(ctx, lookups)
}

// LookupNativeMetricMetadata implements storage.NativeMetricMetadataReader.
// Disabled native metadata is treated as unavailable history.
func (h *Head) LookupNativeMetricMetadata(ctx context.Context, lookups []storage.NativeMetricMetadataLookup) error {
	for i := range lookups {
		lookups[i].Metadata = nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.nativeMetricMetadata == nil {
		return nil
	}
	for len(lookups) > 0 {
		batch := lookups[:min(len(lookups), maxNativeMetricMetadataLookups)]
		historical, err := h.selectNativeMetricMetadataBatch(ctx, batch)
		if historical != nil {
			if err == nil {
				err = historical.materialize(ctx, batch)
			}
			historical.reset()
			nativeMetricMetadataLookupPool.Put(historical)
		}
		if err != nil {
			return err
		}
		lookups = lookups[len(batch):]
	}
	return nil
}

// selectNativeMetricMetadataBatch freezes current pointers and historical handles
// under the publication barrier. The caller owns returned scratch, even on error.
// Results must be cleared and the batch limited to maxNativeMetricMetadataLookups.
func (h *Head) selectNativeMetricMetadataBatch(ctx context.Context, lookups []storage.NativeMetricMetadataLookup) (*nativeMetricMetadataLookupScratch, error) {
	store := h.nativeMetricMetadata
	if err := store.publication.Acquire(ctx, nativeMetricMetadataPublicationPermits); err != nil {
		return nil, err
	}
	defer store.publication.Release(nativeMetricMetadataPublicationPermits)

	var historical *nativeMetricMetadataLookupScratch
	for i := range lookups {
		if err := ctx.Err(); err != nil {
			return historical, err
		}
		lookup := &lookups[i]
		ref := chunks.HeadSeriesRef(lookup.Ref)
		// Publication excludes native changes. The Head index protects the
		// liveness check while we select a pointer/handle; GC never clears the
		// retired history. Do not take a series lock under this index lock.
		index := h.series.refStripe(ref)
		h.series.locks[index].RLock()
		var selected unique.Handle[metadata.Metadata]
		if series := h.series.series[index][ref]; series != nil {
			// Legacy commits may install the sidecar concurrently. Its pointer
			// is atomic; legacy fields and packed series state are not read here.
			if sidecar := series.metadata.Load(); sidecar != nil && sidecar.native.metadata != nil {
				native := &sidecar.native
				if native.effectiveFrom <= lookup.Timestamp {
					lookup.Metadata = native.metadata
				} else {
					for _, point := range slices.Backward(native.older) {
						if point.effectiveFrom <= lookup.Timestamp {
							selected = point.metadata
							break
						}
					}
				}
			}
		}
		h.series.locks[index].RUnlock()
		if selected != (unique.Handle[metadata.Metadata]{}) {
			if historical == nil {
				historical = nativeMetricMetadataLookupPool.Get().(*nativeMetricMetadataLookupScratch)
			}
			historical.historical[i] = selected
		}
	}
	return historical, nil
}

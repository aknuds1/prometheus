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

	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
)

const maxNativeMetricMetadataLookups = 256

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
		if err := h.selectNativeMetricMetadataBatch(ctx, batch); err != nil {
			return err
		}
		lookups = lookups[len(batch):]
	}
	return ctx.Err()
}

// selectNativeMetricMetadataBatch selects immutable current and historical values
// under the publication barrier. Results own their lifetime independently of Head.
// Results must be cleared and the batch limited to maxNativeMetricMetadataLookups.
func (h *Head) selectNativeMetricMetadataBatch(ctx context.Context, lookups []storage.NativeMetricMetadataLookup) error {
	store := h.nativeMetricMetadata
	if err := store.publication.Acquire(ctx, nativeMetricMetadataPublicationPermits); err != nil {
		return err
	}
	defer store.publication.Release(nativeMetricMetadataPublicationPermits)

	var page any
	lastPage := ^uint64(0)
	// Acquisition checks cancellation before this bounded batch; check again
	// after selection instead of checking the context for every lookup.
	for i := range lookups {
		lookup := &lookups[i]
		ref := chunks.HeadSeriesRef(lookup.Ref)
		key := uint64(ref) >> nativeMetadataPageBits
		if key != lastPage {
			page, _ = store.directory.Load(key)
			lastPage = key
		}
		// Only cache within this publication-protected batch. GC may detach a
		// page, but its pointers remain safe and retirement is checked below.
		series := nativeMetadataDirectorySeries(page, ref)
		if series == nil {
			continue
		}
		// Publication excludes changes. Directory publication guarantees native
		// initialization, but Head removal may precede directory cleanup.
		native := series.metadata.Load().native
		if native.flags.Load()&nativeMetadataRetired != 0 {
			continue
		}
		if native.EffectiveFrom <= lookup.Timestamp {
			lookup.Metadata = native.Metadata
			continue
		}
		for _, point := range slices.Backward(native.Older) {
			if point.EffectiveFrom <= lookup.Timestamp {
				lookup.Metadata = point.Metadata
				break
			}
		}
	}
	return ctx.Err()
}

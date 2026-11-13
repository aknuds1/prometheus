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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const retiredBlockPrefix = ".retired-"

// retireBlocks moves obsolete directories out of the published namespace before
// deleting their contents. Once retired, a cleanup failure cannot resurrect the
// block after restart. A rename failure rolls back earlier renames so its caller
// can retain its previous in-memory model.
func retireBlocks(dir string, blockIDs []string) error {
	var retired []string
	for _, id := range blockIDs {
		from, to := filepath.Join(dir, id), filepath.Join(dir, retiredBlockPrefix+id)
		if err := os.Rename(from, to); err != nil {
			var rollbackErrors []error
			for _, path := range slices.Backward(retired) {
				if rollbackErr := os.Rename(path, filepath.Join(dir, strings.TrimPrefix(filepath.Base(path), retiredBlockPrefix))); rollbackErr != nil {
					rollbackErrors = append(rollbackErrors, rollbackErr)
				}
			}
			return errors.Join(append([]error{fmt.Errorf("retiring block %s: %w", id, err)}, rollbackErrors...)...)
		}
		retired = append(retired, to)
	}
	var cleanupErrors []error
	for _, path := range retired {
		if err := os.RemoveAll(path); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}

// RetainBefore removes sealed blocks whose exclusive MaxTime is no later than
// cutoff. Partially overlapping blocks and the flushed head are not pruned.
// Sample tail state is rebuilt from retained blocks. Metadata histories remain
// available for surviving identities, while owners with neither retained samples
// nor retained owner-only versions are collected. Metadata is not expired solely
// because old samples are. The cutoff remains in force for appends until restart;
// this deliberately simple
// experiment does not implement persistent tombstones or arbitrary range delete.
// The retention cutoff is not persisted across restart; retained sample tails
// still reject duplicates but are not a durable lower bound for unseen series.
func (s *Storage) RetainBefore(ctx context.Context, cutoff int64) (int, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.closed {
		return 0, errors.New("columnar: storage is closed")
	}
	if len(s.buffer) > 0 || s.metadataDirty {
		return 0, ErrHeadNotFlushed
	}
	var retained []*Block
	var removed []string
	for _, block := range s.blocks {
		if block.meta.MaxTime <= cutoff {
			removed = append(removed, block.meta.ULID)
		} else {
			retained = append(retained, block)
		}
	}
	if len(removed) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	retainedSeries := map[uint64]struct{}{}
	for _, block := range retained {
		for _, entry := range block.catalog.Series {
			retainedSeries[entry.ID] = struct{}{}
		}
	}
	var retainedMetadata *metadataStore
	if s.metadata != nil {
		var err error
		retainedMetadata, err = s.metadata.clone()
		if err != nil {
			return 0, err
		}
		// Active memberships carry their applicable retained version histories,
		// even when those changes originally entered a now-expired delta block.
		retainedOwners := map[OwnerKey]struct{}{}
		for hash, kinds := range retainedMetadata.bySeries {
			if _, ok := retainedSeries[hash]; !ok {
				delete(retainedMetadata.bySeries, hash)
				delete(retainedMetadata.chains, hash)
				continue
			}
			for _, memberships := range kinds {
				for _, membership := range memberships {
					retainedOwners[membership.owner] = struct{}{}
				}
			}
		}
		// Owner-only versions survive only through owner rows in retained blocks.
		for _, block := range retained {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			if block.meta.Metadata == nil {
				continue
			}
			relations, err := block.readMetadataRelations(ctx)
			if err != nil {
				return 0, err
			}
			for _, owner := range relations.owners {
				retainedOwners[owner.key] = struct{}{}
			}
		}
		for owner := range retainedMetadata.byOwner {
			if _, ok := retainedOwners[owner]; !ok {
				retainedMetadata.deleteOwner(owner)
			}
		}
	}
	if err := retireBlocks(s.dir, removed); err != nil {
		return 0, err
	}
	expired := make([]*Block, 0, len(removed))
	for _, block := range s.blocks {
		if slices.Contains(retained, block) {
			continue
		}
		expired = append(expired, block)
	}
	s.closeRetired(expired)
	s.blocks = retained
	s.minValidTime = max(s.minValidTime, cutoff)
	if err := s.rebuildSeriesLabels(); err != nil {
		return len(removed), err
	}
	previousLast := s.last
	s.last = map[uint64]*seriesState{}
	for _, block := range retained {
		if err := s.restoreSeriesTail(block); err != nil {
			// Retained blocks were read successfully at startup/publication. If an
			// external corruption appears here, keep the error explicit: the new
			// in-memory tail state is not a promise to repair arbitrary deletion.
			s.last = previousLast
			return len(removed), fmt.Errorf("rebuilding retained sample tails: %w", err)
		}
	}
	if s.metadata != nil {
		retainedMetadata.reads = s.reads
		s.metadata = retainedMetadata
		s.setIndexedResourceAttrsLocked(s.opts.IndexedResourceAttributes)
		published := retainedMetadata.snapshot()
		s.metadataPublished = &published
	}
	return len(removed), errors.Join(retiredCleanupErrors(s.dir, removed)...)
}

// retiredCleanupErrors reports residual disk use without affecting logical
// retention: retired directories are excluded from ListBlocks.
func retiredCleanupErrors(dir string, ids []string) []error {
	var out []error
	for _, id := range ids {
		path := filepath.Join(dir, retiredBlockPrefix+id)
		if _, err := os.Stat(path); err == nil {
			if err := os.RemoveAll(path); err != nil {
				out = append(out, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			out = append(out, err)
		}
	}
	return out
}

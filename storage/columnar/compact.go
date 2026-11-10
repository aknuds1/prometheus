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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/oklog/ulid/v2"
)

// ErrHeadNotFlushed prevents live compaction or retention from silently omitting
// pending head samples and metadata associations from their publication model.
var ErrHeadNotFlushed = errors.New("columnar: flush the head before lifecycle publication")

// compactBlockSnapshot reads and merges a fixed immutable block set. CompactTo
// uses this for separate output; Compact then publishes exactly this snapshot.
func (s *Storage) compactBlockSnapshot(ctx context.Context, blocks []*Block, dest string) (Meta, *metadataStore, blockCatalog, error) {
	var samples []Series
	var compacted *metadataStore
	if s.metadata != nil {
		compacted = newMetadataStore(s.opts.MetadataVersionCap)
	}
	var walSequence uint64
	for _, block := range blocks {
		if err := ctx.Err(); err != nil {
			return Meta{}, nil, blockCatalog{}, err
		}
		walSequence = max(walSequence, block.meta.WALSequence)
		if block.meta.NumSamples > 0 {
			series, err := block.Series(ctx)
			if err != nil {
				return Meta{}, nil, blockCatalog{}, err
			}
			samples = append(samples, series...)
		}
		if compacted != nil && block.meta.Metadata != nil {
			relations, err := block.readMetadataRelations(ctx)
			if err != nil {
				return Meta{}, nil, blockCatalog{}, err
			}
			if err := compacted.restoreRelations(relations); err != nil {
				return Meta{}, nil, blockCatalog{}, fmt.Errorf("compacting metadata from block %s: %w", block.meta.ULID, err)
			}
		}
	}
	var metadata *metadataRelations
	if compacted != nil {
		relations := compacted.snapshot()
		if relations.hasChanges() {
			metadata = &relations
		}
	}
	var catalog blockCatalog
	meta, err := writeBlock(dest, mergeSeries(samples), metadata, s.opts.WriteOptions, walSequence, &catalog, s.sharedSeriesLabels)
	return meta, compacted, catalog, err
}

// CompactTo merges the current immutable blocks into one block at dest. It does
// not include the unflushed head, delete input blocks, or replace live storage.
// It remains a separate measurement/publication primitive rather than a second
// implicit production policy.
func (s *Storage) CompactTo(ctx context.Context, dest string) (Meta, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.closed {
		return Meta{}, errors.New("columnar: storage is closed")
	}
	if filepath.Clean(dest) == filepath.Clean(s.dir) {
		return Meta{}, errors.New("columnar: compaction output must use a separate directory")
	}
	meta, _, _, err := s.compactBlockSnapshot(ctx, s.blocks, dest)
	return meta, err
}

// Compact merges all published blocks and replaces the live block set. The head
// must be flushed first. The replacement manifest names its input parents before
// the block is moved into place, so a crash that leaves parents behind does not
// resurrect duplicate data or make later reads open every old block again.
func (s *Storage) Compact(ctx context.Context) (Meta, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.closed {
		return Meta{}, errors.New("columnar: storage is closed")
	}
	if len(s.buffer) > 0 || s.metadataDirty {
		return Meta{}, ErrHeadNotFlushed
	}
	if len(s.blocks) == 0 {
		return Meta{}, ErrNoBlocks
	}
	parents := make([]string, 0, len(s.blocks))
	for _, block := range s.blocks {
		parents = append(parents, block.meta.ULID)
	}

	staging := filepath.Join(s.dir, ".compaction-"+ulid.Make().String()+tmpDirSuffix)
	defer os.RemoveAll(staging)
	meta, compactedMetadata, catalog, err := s.compactBlockSnapshot(ctx, s.blocks, staging)
	if err != nil {
		return Meta{}, err
	}
	meta.Replaces = parents
	raw, err := json.Marshal(meta)
	if err != nil {
		return Meta{}, err
	}
	if err := os.WriteFile(blockMetaPath(filepath.Join(staging, meta.ULID)), raw, 0o600); err != nil {
		return Meta{}, err
	}
	publishedPath := filepath.Join(s.dir, meta.ULID)
	if err := os.Rename(filepath.Join(staging, meta.ULID), publishedPath); err != nil {
		return Meta{}, err
	}
	published, err := openBlock(publishedPath, &catalog)
	if err != nil {
		return Meta{}, fmt.Errorf("opening published compaction %s: %w", meta.ULID, err)
	}
	published.reads = s.reads

	// Adopt the validated snapshot before deleting inputs. If cleanup fails, the
	// new block is still complete and ListBlocks suppresses its named parents on
	// reopening. The returned error makes the residual disk cost explicit.
	s.blocks = []*Block{published}
	if err := s.rebuildSeriesLabels(); err != nil {
		return meta, err
	}
	if s.metadata != nil {
		compactedMetadata.reads = s.reads
		s.metadata = compactedMetadata
		s.setIndexedResourceAttrsLocked(s.opts.IndexedResourceAttributes)
		publishedRelations := compactedMetadata.snapshot()
		s.metadataPublished = &publishedRelations
	}
	if err := retireBlocks(s.dir, parents); err != nil {
		return meta, fmt.Errorf("compaction %s published, but parent cleanup failed: %w", meta.ULID, err)
	}
	return meta, nil
}

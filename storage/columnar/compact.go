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
	"path/filepath"
)

// CompactTo merges the current immutable blocks into one block at dest. It does
// not include the unflushed head, delete input blocks, or replace live storage.
// This is an experiment for measuring history-validity and merge costs; lifecycle
// publication and retention remain a separate experiment.
func (s *Storage) CompactTo(ctx context.Context, dest string) (Meta, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if s.closed {
		return Meta{}, errors.New("columnar: storage is closed")
	}
	if filepath.Clean(dest) == filepath.Clean(s.dir) {
		return Meta{}, errors.New("columnar: compaction output must use a separate directory")
	}

	var samples []Series
	var metadata *metadataRelations
	var compacted *metadataStore
	if s.metadata != nil {
		compacted = newMetadataStore(s.opts.MetadataVersionCap)
	}
	var walSequence uint64
	for _, block := range s.blocks {
		if err := ctx.Err(); err != nil {
			return Meta{}, err
		}
		walSequence = max(walSequence, block.meta.WALSequence)
		if block.meta.NumSamples > 0 {
			series, err := block.Series(ctx)
			if err != nil {
				return Meta{}, err
			}
			samples = append(samples, series...)
		}
		if compacted != nil && block.meta.Metadata != nil {
			relations, err := block.readMetadataRelations(ctx)
			if err != nil {
				return Meta{}, err
			}
			if err := compacted.restoreRelations(relations); err != nil {
				return Meta{}, fmt.Errorf("compacting metadata from block %s: %w", block.meta.ULID, err)
			}
		}
	}
	if compacted != nil {
		relations := compacted.snapshot()
		if relations.hasChanges() {
			metadata = &relations
		}
	}
	return writeBlock(dest, mergeSeries(samples), metadata, s.opts.WriteOptions, walSequence)
}

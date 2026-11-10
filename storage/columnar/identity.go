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
	"sync"

	"github.com/prometheus/prometheus/model/labels"
)

// hashScratchPool reuses the hashing scratch buffer. labels.StableHash
// allocated a fresh 1 KB buffer per call, and it runs per sample and per
// catalog probe on ingestion hot paths.
var hashScratchPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 1024)
		return &b
	},
}

// stableSeriesHash is the common, stable series key for the head, block catalog
// and metadata memberships. Complete-label collision checks remain mandatory.
// The hash is identical to labels.StableHash.
func stableSeriesHash(ls labels.Labels) uint64 {
	scratch := hashScratchPool.Get().(*[]byte)
	h, buf := labels.StableHashInto(ls, *scratch)
	*scratch = buf
	hashScratchPool.Put(scratch)
	return h
}

// resourceLabel returns the exact label used in OTLP-derived metric series.
func resourceLabel(owner OwnerKey) labels.Label {
	return labels.Label{Name: OTELResourceLabel, Value: owner.Ref()}
}

// metadataSeriesKey is the series identity shared with storage.ResourceQuerier.
// It routes through the pooled hasher: labels.StableHash allocates a 1 KB
// scratch buffer and this key is computed for every series of every block.
func metadataSeriesKey(ls labels.Labels) uint64 { return stableSeriesHash(ls) }

// sharedSeriesLabels returns one immutable label allocation per stable identity.
// The caller copies an arbitrary input once on first use; every later use checks
// full labels before reusing the stored allocation. Calls hold Storage.mtx.
func (s *Storage) sharedSeriesLabels(ls labels.Labels) (labels.Labels, error) {
	identity := stableSeriesHash(ls)
	if previous, exists := s.seriesLabels[identity]; exists {
		if !labels.Equal(previous, ls) {
			return labels.EmptyLabels(), ErrSeriesHashCollision
		}
		return previous, nil
	}
	owned := ls.Copy()
	s.seriesLabels[identity] = owned
	return owned, nil
}

// reuseBlockLabels replaces duplicate per-block label allocations with the
// immutable process cache. It must run before publishing a newly opened block.
func (s *Storage) reuseBlockLabels(block *Block) error {
	for i, entry := range block.catalog.Series {
		ls, err := s.sharedSeriesLabels(entry.Labels)
		if err != nil {
			return err
		}
		block.catalog.Series[i].Labels = ls
	}
	return nil
}

// rebuildSeriesLabels releases removed series after compaction/retention.
func (s *Storage) rebuildSeriesLabels() error {
	s.seriesLabels = map[uint64]labels.Labels{}
	for _, block := range s.blocks {
		if err := s.reuseBlockLabels(block); err != nil {
			return err
		}
	}
	return nil
}

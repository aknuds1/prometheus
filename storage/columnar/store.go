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
	"slices"

	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/oklog/ulid/v2"

	"github.com/prometheus/prometheus/model/labels"
)

// Meta is the manifest of a block.
//
// Times are in milliseconds and follow Prometheus block conventions: MinTime is
// inclusive, MaxTime exclusive.
type Meta struct {
	ULID          string `json:"ulid"`
	MinTime       int64  `json:"min_time"`
	MaxTime       int64  `json:"max_time"`
	NumSeries     int    `json:"num_series"`
	NumSamples    int    `json:"num_samples"`
	FormatVersion int    `json:"format_version"`
}

// WriteBlock writes a new block directory below dir containing series.
//
// The block is written to a temporary directory and renamed into place, so a
// reader never observes a partially written block. Series are sorted by label
// set, which gives the writer a deterministic layout and lets row groups hold
// contiguous ranges of the label order.
//
// All timestamps of the block are taken from the samples themselves; mint and
// maxt are not passed in, because a block that disagrees with its samples is
// worse than one whose range is derived.
func WriteBlock(dir string, series []Series, opts WriteOptions) (Meta, error) {
	opts = opts.withDefaults()

	series = slices.Clone(series)
	for _, s := range series {
		if err := s.Validate(); err != nil {
			return Meta{}, err
		}
	}
	slices.SortFunc(series, func(a, b Series) int { return labels.Compare(a.Labels, b.Labels) })

	meta := Meta{
		ULID:          ulid.Make().String(),
		FormatVersion: FormatVersion,
		NumSeries:     len(series),
	}
	if len(series) > 0 {
		// An empty block has no meaningful range; leave it zero.
		meta.MinTime = series[0].Timestamps[0]
		meta.MaxTime = series[0].Timestamps[len(series[0].Timestamps)-1] + 1
	}
	for _, s := range series {
		meta.NumSamples += len(s.Timestamps)
		if len(s.Timestamps) == 0 {
			continue
		}
		meta.MinTime = min(meta.MinTime, s.Timestamps[0])
		meta.MaxTime = max(meta.MaxTime, s.Timestamps[len(s.Timestamps)-1]+1)
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Meta{}, err
	}
	tmpDir := filepath.Join(dir, meta.ULID+tmpDirSuffix)
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return Meta{}, err
	}
	// The temporary directory must not survive a failed write.
	defer func() {
		if _, err := os.Stat(tmpDir); err == nil {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	if err := writeBlockFile(blockDataPath(tmpDir), series, opts); err != nil {
		return Meta{}, fmt.Errorf("writing block data: %w", err)
	}

	raw, err := json.Marshal(meta)
	if err != nil {
		return Meta{}, err
	}
	if err := os.WriteFile(blockMetaPath(tmpDir), raw, 0o600); err != nil {
		return Meta{}, err
	}

	finalDir := filepath.Join(dir, meta.ULID)
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// Block is a readable columnar block.
type Block struct {
	dir  string
	meta Meta

	mem memory.Allocator
	// rowGroups is the number of row groups in the data file, used to expose
	// scan granularity.
	rowGroups int
}

// OpenBlock opens the block in dir.
func OpenBlock(dir string) (*Block, error) {
	raw, err := os.ReadFile(blockMetaPath(dir))
	if err != nil {
		return nil, err
	}
	var meta Meta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("parsing block metadata: %w", err)
	}
	if meta.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("block %s has format version %d, this build reads version %d", dir, meta.FormatVersion, FormatVersion)
	}

	f, err := os.Open(blockDataPath(dir))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	pf, err := file.NewParquetReader(f)
	if err != nil {
		return nil, err
	}
	defer pf.Close()

	return &Block{
		dir:       dir,
		meta:      meta,
		mem:       memory.NewGoAllocator(),
		rowGroups: pf.NumRowGroups(),
	}, nil
}

// Dir returns the block directory.
func (b *Block) Dir() string { return b.dir }

// Meta returns the block manifest.
func (b *Block) Meta() Meta { return b.meta }

// NumRowGroups returns the number of row groups in the block.
func (b *Block) NumRowGroups() int { return b.rowGroups }

// Series reads every series of the block.
//
// This is the brute-force reader: it materialises the whole block. The query
// path must not use it, because the only way Parquet lets a selector avoid work
// is by skipping row groups, and this reads all of them.
func (b *Block) Series(ctx context.Context) ([]Series, error) {
	return readAllSeries(ctx, blockDataPath(b.dir))
}

// Overlaps reports whether the block may contain samples in [mint, maxt).
func (b *Block) Overlaps(mint, maxt int64) bool {
	return b.meta.NumSeries > 0 && b.meta.MaxTime > mint && b.meta.MinTime < maxt
}

// ErrNoBlocks is returned when a directory contains no blocks.
var ErrNoBlocks = errors.New("no columnar blocks")

// ListBlocks returns the blocks in dir, sorted by ULID, which sorts by creation
// time.
func ListBlocks(dir string) ([]*Block, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoBlocks
		}
		return nil, err
	}

	blocks := make([]*Block, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || filepath.Ext(entry.Name()) == tmpDirSuffix {
			continue
		}
		block, err := OpenBlock(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	if len(blocks) == 0 {
		return nil, ErrNoBlocks
	}
	slices.SortFunc(blocks, func(a, b *Block) int {
		switch {
		case a.meta.ULID < b.meta.ULID:
			return -1
		case a.meta.ULID > b.meta.ULID:
			return 1
		default:
			return 0
		}
	})
	return blocks, nil
}

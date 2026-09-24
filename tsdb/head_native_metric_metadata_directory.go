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
	"math/bits"
	"slices"

	"go.uber.org/atomic"

	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
)

const (
	nativeMetadataPageBits                = 8
	nativeMetadataPageSize                = 1 << nativeMetadataPageBits
	nativeMetadataDirectoryHashMultiplier = uint64(0x9e3779b97f4a7c15)
)

// The sentinel reserves pointer identity, not a series reference (including zero).
var nativeMetadataDirectoryTombstone = new(memSeries)

// nativeMetricMetadataPage holds multiple series within one 256-reference page.
// Readers access only the immutable representation and atomic slots. The page-key
// write lock protects live/used counts and all mutations; replacements are never
// pooled, so captured pages remain safe until Go's GC reclaims them.
type nativeMetricMetadataPage struct {
	slots     []atomic.Pointer[memSeries]
	dense     bool
	hashShift uint8
	live      int
	used      int
}

func newNativeMetricMetadataPage(capacity int) *nativeMetricMetadataPage {
	return &nativeMetricMetadataPage{
		slots: make([]atomic.Pointer[memSeries], capacity),
		dense: capacity == nativeMetadataPageSize,
		// Use high product bits to spread both adjacent and strided references.
		hashShift: uint8(64 - bits.TrailingZeros(uint(capacity))),
	}
}

func (p *nativeMetricMetadataPage) lookup(ref chunks.HeadSeriesRef) *memSeries {
	if p.dense {
		return p.slots[uint8(ref)].Load()
	}
	mask := len(p.slots) - 1
	start := int((uint64(ref) * nativeMetadataDirectoryHashMultiplier) >> p.hashShift)
	for probe := range len(p.slots) {
		series := p.slots[(start+probe)&mask].Load()
		if series == nil {
			return nil
		}
		if series != nativeMetadataDirectoryTombstone && series.ref == ref {
			return series
		}
	}
	return nil
}

// slot returns the existing slot or a place to insert, preferring a tombstone.
// The page-key write lock excludes another writer from changing that choice.
func (p *nativeMetricMetadataPage) slot(ref chunks.HeadSeriesRef) int {
	if p.dense {
		return int(uint8(ref))
	}
	firstTombstone := -1
	mask := len(p.slots) - 1
	start := int((uint64(ref) * nativeMetadataDirectoryHashMultiplier) >> p.hashShift)
	for probe := range len(p.slots) {
		index := (start + probe) & mask
		series := p.slots[index].Load()
		switch series {
		case nil:
			if firstTombstone >= 0 {
				return firstTombstone
			}
			return index
		case nativeMetadataDirectoryTombstone:
			if firstTombstone < 0 {
				firstTombstone = index
			}
		default:
			if series.ref == ref {
				return index
			}
		}
	}
	return firstTombstone
}

func (p *nativeMetricMetadataPage) insert(series *memSeries) {
	index := p.slot(series.ref)
	old := p.slots[index].Load()
	if old == nil || old == nativeMetadataDirectoryTombstone {
		p.live++
		if old == nil {
			p.used++
		}
	}
	p.slots[index].Store(series)
}

func (p *nativeMetricMetadataPage) rebuild(capacity int) *nativeMetricMetadataPage {
	replacement := newNativeMetricMetadataPage(capacity)
	for i := range p.slots {
		series := p.slots[i].Load()
		if series != nil && series != nativeMetadataDirectoryTombstone {
			replacement.insert(series)
		}
	}
	return replacement
}

// nativeMetadataDirectorySeries resolves a captured registry value. Singleton
// pages store a series directly; absent pages consume no per-reference space.
func nativeMetadataDirectorySeries(value any, ref chunks.HeadSeriesRef) *memSeries {
	switch page := value.(type) {
	case *memSeries:
		if page.ref == ref {
			return page
		}
	case *nativeMetricMetadataPage:
		return page.lookup(ref)
	}
	return nil
}

// publishLocked publishes initialized state before releasing the series lock.
// Directory writers never acquire a series or Head-index lock while locked.
func (s *nativeMetricMetadataStore) publishLocked(series *memSeries) {
	key := uint64(series.ref) >> nativeMetadataPageBits
	lock := &s.directoryLocks[key%nativeMetricMetadataStripes]
	lock.Lock()
	defer lock.Unlock()
	value, _ := s.directory.Load(key)
	switch page := value.(type) {
	case nil:
		s.directory.Store(key, series)
	case *memSeries:
		if page.ref == series.ref {
			s.directory.Store(key, series)
			return
		}
		replacement := newNativeMetricMetadataPage(4)
		replacement.insert(page)
		replacement.insert(series)
		s.directory.Store(key, replacement)
	case *nativeMetricMetadataPage:
		if page.dense || page.lookup(series.ref) != nil {
			page.insert(series)
			return
		}
		replacement := page
		if (page.live+1)*4 > len(page.slots)*3 {
			// Sparse tables stop at 128 slots; the 97th entry promotes to dense.
			replacement = page.rebuild(len(page.slots) * 2)
		} else if page.slots[page.slot(series.ref)].Load() == nil && (page.used+1)*4 > len(page.slots)*3 {
			replacement = page.rebuild(len(page.slots))
		}
		replacement.insert(series)
		if replacement != page {
			s.directory.Store(key, replacement)
		}
	}
}

func (s *nativeMetricMetadataStore) delete(refs map[storage.SeriesRef]struct{}) {
	byPage := make(map[uint64][]chunks.HeadSeriesRef)
	for ref := range refs {
		key := uint64(ref) >> nativeMetadataPageBits
		byPage[key] = append(byPage[key], chunks.HeadSeriesRef(ref))
	}
	var retired []*memSeries
	for key, refs := range byPage {
		lock := &s.directoryLocks[key%nativeMetricMetadataStripes]
		lock.Lock()
		value, _ := s.directory.Load(key)
		switch page := value.(type) {
		case *memSeries:
			if slices.Contains(refs, page.ref) {
				s.directory.Delete(key)
				retired = append(retired, page)
			}
		case *nativeMetricMetadataPage:
			for _, ref := range refs {
				index := page.slot(ref)
				if index < 0 {
					continue
				}
				series := page.slots[index].Load()
				if series == nil || series == nativeMetadataDirectoryTombstone || series.ref != ref {
					continue
				}
				retired = append(retired, series)
				if page.dense {
					page.slots[index].Store(nil)
					page.used--
				} else {
					page.slots[index].Store(nativeMetadataDirectoryTombstone)
				}
				page.live--
			}
			switch {
			case page.live == 0:
				s.directory.Delete(key)
			case page.live == 1:
				for i := range page.slots {
					if series := page.slots[i].Load(); series != nil && series != nativeMetadataDirectoryTombstone {
						s.directory.Store(key, series)
						break
					}
				}
			case page.dense && page.live <= 80:
				s.directory.Store(key, page.rebuild(128))
			case !page.dense && page.live <= len(page.slots)/4:
				capacity := 4
				for page.live*4 > capacity*3 {
					capacity *= 2
				}
				s.directory.Store(key, page.rebuild(capacity))
			case !page.dense && page.used-page.live > page.live:
				s.directory.Store(key, page.rebuild(len(page.slots)))
			}
		}
		lock.Unlock()
	}
	// Head deletion has excluded pending commits. Never take series locks under
	// directory locks, and never clear history held by detached-page readers.
	var versions int64
	for _, series := range retired {
		series.Lock()
		versions += int64(len(series.nativeMetadataLocked().older) + 1)
		series.Unlock()
	}
	s.series.Add(-int64(len(retired)))
	s.versions.Add(-versions)
}

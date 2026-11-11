// Copyright 2026 The Prometheus Authors
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
	"math/bits"
	"sync"
)

// idMarker accumulates series IDs in a reused bitset so that resolving a matcher
// against many postings is linear in the total number of IDs rather than a copy
// per posting. The previous approach merged each matching posting into a growing
// slice, which is O(postings * series): measured at 35 ms for a regex matching
// 1,384 postings against 4 us for one matching 28.
type idMarker struct {
	bits []uint64
}

var idMarkerPool = sync.Pool{New: func() any { return &idMarker{} }}

// getIDMarker returns a zeroed marker able to hold IDs below size.
func getIDMarker(size int) *idMarker {
	m, _ := idMarkerPool.Get().(*idMarker)
	m.reset(size)
	return m
}

func putIDMarker(m *idMarker) {
	m.bits = m.bits[:0]
	idMarkerPool.Put(m)
}

func (m *idMarker) reset(size int) {
	words := size/64 + 1
	if cap(m.bits) < words {
		m.bits = make([]uint64, words)
		return
	}
	m.bits = m.bits[:words]
	for i := range m.bits {
		m.bits[i] = 0
	}
}

func (m *idMarker) add(id uint64) { m.bits[id>>6] |= 1 << (id & 63) }

func (m *idMarker) has(id uint64) bool { return m.bits[id>>6]&(1<<(id&63)) != 0 }

// appendTo appends the marked IDs to dst in ascending order. Callers rely on that
// order, and scanning the bitset yields it without a separate sort.
func (m *idMarker) appendTo(dst []uint64) []uint64 {
	for wordIndex, word := range m.bits {
		for word != 0 {
			offset := bits.TrailingZeros64(word)
			dst = append(dst, uint64(wordIndex)<<6|uint64(offset))
			word &^= 1 << uint(offset)
		}
	}
	return dst
}

// markerSize returns a bound on series IDs in ids.
func markerSize(ids []uint64) int {
	var maxID uint64
	for _, id := range ids {
		if id > maxID {
			maxID = id
		}
	}
	return int(maxID) + 1
}

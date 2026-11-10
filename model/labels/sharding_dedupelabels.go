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

//go:build dedupelabels

package labels

import (
	"github.com/cespare/xxhash/v2"
)

// StableHash is a labels hashing implementation which is guaranteed to not change over time.
// This function should be used whenever labels hashing backward compatibility must be guaranteed.
func StableHash(ls Labels) uint64 {
	h, _ := StableHashInto(ls, nil)
	return h
}

// StableHashInto is StableHash reusing the caller's scratch buffer. The hash is
// identical to StableHash.
func StableHashInto(ls Labels, scratch []byte) (uint64, []byte) {
	// Use xxhash.Sum64(b) for fast path as it's faster.
	var b []byte
	if cap(scratch) >= 1024 {
		b = scratch[:0]
	} else {
		b = make([]byte, 0, 1024)
	}
	for pos := 0; pos < len(ls.data); {
		name, newPos := decodeString(ls.syms, ls.data, pos)
		value, newPos := decodeString(ls.syms, ls.data, newPos)
		if len(b)+len(name)+len(value)+2 >= cap(b) {
			// If labels entry is 1KB+, hash the rest of them via Write().
			h := xxhash.New()
			_, _ = h.Write(b)
			for pos < len(ls.data) {
				name, pos = decodeString(ls.syms, ls.data, pos)
				value, pos = decodeString(ls.syms, ls.data, pos)
				_, _ = h.WriteString(name)
				_, _ = h.Write(seps)
				_, _ = h.WriteString(value)
				_, _ = h.Write(seps)
			}
			return h.Sum64(), b[:0]
		}

		b = append(b, name...)
		b = append(b, sep)
		b = append(b, value...)
		b = append(b, sep)
		pos = newPos
	}
	return xxhash.Sum64(b), b
}

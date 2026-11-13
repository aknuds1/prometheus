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

//go:build !slicelabels && !dedupelabels

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
	var h *xxhash.Digest
	for i := 0; i < len(ls.data); {
		var v Label
		v.Name, i = decodeString(ls.data, i)
		v.Value, i = decodeString(ls.data, i)
		if h == nil && len(b)+len(v.Name)+len(v.Value)+2 >= cap(b) {
			// If labels entry is 1KB+, switch to Write API. Copy in the values up to this point.
			h = xxhash.New()
			_, _ = h.Write(b)
		}
		if h != nil {
			_, _ = h.WriteString(v.Name)
			_, _ = h.Write(seps)
			_, _ = h.WriteString(v.Value)
			_, _ = h.Write(seps)
			continue
		}

		b = append(b, v.Name...)
		b = append(b, sep)
		b = append(b, v.Value...)
		b = append(b, sep)
	}
	if h != nil {
		return h.Sum64(), b[:0]
	}
	return xxhash.Sum64(b), b
}

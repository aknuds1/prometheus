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

//go:build slicelabels

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

// StableHashInto is StableHash reusing the caller's scratch buffer, which avoids
// a fresh allocation per call on hot ingestion paths. It returns the buffer for
// reuse; the hash is identical to StableHash.
func StableHashInto(ls Labels, scratch []byte) (uint64, []byte) {
	// Use xxhash.Sum64(b) for fast path as it's faster.
	var b []byte
	if cap(scratch) >= 1024 {
		b = scratch[:0]
	} else {
		b = make([]byte, 0, 1024)
	}
	for i, v := range ls {
		if len(b)+len(v.Name)+len(v.Value)+2 >= cap(b) {
			// If labels entry is 1KB+ do not allocate whole entry.
			h := xxhash.New()
			_, _ = h.Write(b)
			for _, v := range ls[i:] {
				_, _ = h.WriteString(v.Name)
				_, _ = h.Write(seps)
				_, _ = h.WriteString(v.Value)
				_, _ = h.Write(seps)
			}
			return h.Sum64(), b[:0]
		}

		b = append(b, v.Name...)
		b = append(b, sep)
		b = append(b, v.Value...)
		b = append(b, sep)
	}
	return xxhash.Sum64(b), b
}

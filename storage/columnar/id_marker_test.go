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
	"math/rand"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// The marker replaced a repeated copy-merge, so it must produce exactly what that
// produced: ascending, deduplicated.
func TestIDMarkerMatchesUnionReference(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 200; trial++ {
		var want []uint64
		m := getIDMarker(1 << 14)
		for posting := 0; posting < 1+rng.Intn(8); posting++ {
			n := rng.Intn(40)
			ids := make([]uint64, 0, n)
			next := 0
			for range n {
				next += 1 + rng.Intn(50)
				ids = append(ids, uint64(next))
			}
			slices.Sort(ids)
			ids = slices.Compact(ids)
			for _, id := range ids {
				m.add(id)
			}
			want = unionSortedIDs(want, ids)
		}
		got := m.appendTo(nil)
		putIDMarker(m)
		require.Equal(t, want, got, "trial %d", trial)
	}
}

func TestIDMarkerReuseClearsPreviousMarks(t *testing.T) {
	m := getIDMarker(1024)
	m.add(5)
	m.add(600)
	require.Equal(t, []uint64{5, 600}, m.appendTo(nil))
	m.reset(1024)
	require.Empty(t, m.appendTo(nil))
	m.add(7)
	require.Equal(t, []uint64{7}, m.appendTo(nil))
	putIDMarker(m)
}

func TestIDMarkerSizeBound(t *testing.T) {
	require.Equal(t, 1, markerSize(nil))
	require.Equal(t, 11, markerSize([]uint64{3, 10, 7}))
}

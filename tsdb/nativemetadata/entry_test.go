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

package nativemetadata

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/record"
)

func TestStateApply(t *testing.T) {
	apply := func(start History, kind record.NativeMetadataKind, truncated bool, spec string) (State, bool) {
		s := State{History: start}
		unknown := s.Apply(kind, truncated, points(spec))
		return s, unknown
	}
	for _, c := range []struct {
		name      string
		start     string
		kind      record.NativeMetadataKind
		truncated bool
		points    string
		want      string
		truncate  bool
		unknown   bool
	}{
		{name: "group", start: "A@100", kind: record.NativeMetadataGroup, points: "B@50 C@150", want: "B@50 A@100 C@150"},
		{name: "evicting group", start: "A@10 B@20 C@30 D@40 E@50", kind: record.NativeMetadataGroup, points: "F@60", want: "B@20 C@30 D@40 E@50 F@60", truncate: true},
		{name: "override", start: "A@10 B@20", kind: record.NativeMetadataOverride, truncated: true, points: "C@5 D@15", want: "C@5 D@15", truncate: true},
		{name: "empty override", start: "A@10", kind: record.NativeMetadataOverride, want: ""},
		{name: "legacy", start: "A@10 B@20", kind: record.NativeMetadataLegacy, points: fmt.Sprintf("C@%d", int64(math.MinInt64)), want: fmt.Sprintf("C@%d", int64(math.MinInt64))},
		{name: "unknown", start: "A@10", kind: record.NativeMetadataUnknown, truncated: true, points: "C@30", want: "C@30", truncate: true, unknown: true},
		{name: "unordered group", start: "A@10", kind: record.NativeMetadataGroup, points: "B@30 C@20", want: "C@20", unknown: true},
		{name: "oversized override", start: "A@10", kind: record.NativeMetadataOverride, points: "B@1 C@2 D@3 E@4 F@5 G@6", want: "G@6", unknown: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, unknown := apply(history(c.start), c.kind, c.truncated, c.points)
			require.Equal(t, c.want, format(&s.History))
			require.Equal(t, c.truncate, s.Truncated)
			require.Equal(t, c.unknown, unknown)
		})
	}
	t.Run("legacy entries keep the truncation flag", func(t *testing.T) {
		s := State{History: history("A@10"), Truncated: true}
		require.False(t, s.Apply(record.NativeMetadataLegacy, false, points("B@1")))
		require.True(t, s.Truncated)
	})
}

// TestStateApplyRecords checks that reducing encoded WAL entries reproduces
// native storage's merge: groups apply as one Merge call each, and an override
// written from a reduced state restores it, including the truncation flag.
func TestStateApplyRecords(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	helps := []string{"a", "b", "c", "d"}
	intern := func(m metadata.Metadata) *metadata.Metadata { return &m }
	randomGroup := func() []Point {
		var group []Point
		for from := int64(rng.IntN(5)); from < 60 && len(group) < 8; from += 1 + int64(rng.IntN(12)) {
			group = append(group, Point{EffectiveFrom: from, Metadata: value(helps[rng.IntN(len(helps))])})
		}
		return group
	}
	var enc record.Encoder
	var dec record.Decoder
	for i := range 20000 {
		var native, reduced State
		for range 1 + rng.IntN(8) {
			group := randomGroup()
			if _, evictions := native.Merge(group); evictions > 0 {
				native.Truncated = true
			}
			entry := record.RefNativeMetadata{Ref: 1, Kind: record.NativeMetadataGroup}
			for _, p := range group {
				entry.Points = AppendRecordPoint(entry.Points, p)
			}
			entries, _, err := dec.NativeMetadata(enc.NativeMetadata([]record.RefNativeMetadata{entry}, nil), nil, nil)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.False(t, reduced.Apply(entries[0].Kind, entries[0].Truncated, AppendRecordPoints(nil, entries[0].Points, intern)))
			require.Equal(t, format(&native.History), format(&reduced.History), "iteration %d", i)
			require.Equal(t, native.Truncated, reduced.Truncated, "iteration %d", i)
		}
		override := record.RefNativeMetadata{Ref: 1, Kind: record.NativeMetadataOverride, Truncated: reduced.Truncated}
		for _, p := range reduced.AppendPoints(nil) {
			override.Points = AppendRecordPoint(override.Points, p)
		}
		entries, _, err := dec.NativeMetadata(enc.NativeMetadata([]record.RefNativeMetadata{override}, nil), nil, nil)
		require.NoError(t, err)
		restored := State{History: history("Z@-100 Y@-50"), Truncated: !reduced.Truncated}
		require.False(t, restored.Apply(entries[0].Kind, entries[0].Truncated, AppendRecordPoints(nil, entries[0].Points, intern)))
		require.Equal(t, format(&native.History), format(&restored.History), "iteration %d", i)
		require.Equal(t, native.Truncated, restored.Truncated, "iteration %d", i)
		require.True(t, slices.IsSortedFunc(restored.AppendPoints(nil), func(a, b Point) int { return int(a.EffectiveFrom - b.EffectiveFrom) }))
	}
}

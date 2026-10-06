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

package record

import (
	"cmp"
	"fmt"
	"math"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/encoding"
)

func TestCompactNativeMetadataRecord(t *testing.T) {
	// Values in order of first use; equal values may repeat.
	values := []NativeMetadataValue{
		{Type: uint8(Counter), Unit: "seconds", Help: "a"},
		{Type: uint8(Gauge), Unit: "seconds", Help: strings.Repeat("b", 300)},
		{},
		{Type: uint8(Summary), Help: "c"},
		{Type: uint8(Counter), Unit: "seconds", Help: "a"},
	}
	point := func(from int64, value uint32) CompactNativeMetadataPoint {
		return CompactNativeMetadataPoint{EffectiveFrom: from, Value: value}
	}
	entries := []RefCompactNativeMetadata{
		{Ref: 7, Kind: NativeMetadataGroup, Points: []CompactNativeMetadataPoint{point(100, 0)}},
		{Ref: 2, Kind: NativeMetadataGroup, Points: []CompactNativeMetadataPoint{point(-5, 1), point(150, 0), point(180, 2)}},
		{Ref: math.MaxUint64, Kind: NativeMetadataOverride, Points: []CompactNativeMetadataPoint{point(math.MinInt64, 3), point(math.MaxInt64, 2)}},
		{Ref: 0, Kind: NativeMetadataOverride, Truncated: true, Points: []CompactNativeMetadataPoint{point(10, 1)}},
		{Ref: 5, Kind: NativeMetadataOverride, Truncated: true},
		{Ref: 6, Kind: NativeMetadataOverride},
		{Ref: 6, Kind: NativeMetadataOverride, Points: []CompactNativeMetadataPoint{point(1, 0), point(2, 1), point(3, 2), point(4, 3), point(5, 4)}},
	}
	var enc Encoder
	var dec Decoder
	rec := enc.CompactNativeMetadata(values, entries, nil)
	require.Equal(t, NativeMetadataCompact, dec.Type(rec))
	require.Equal(t, "native_metadata_compact", dec.Type(rec).String())

	t.Run("round trip", func(t *testing.T) {
		var decoded CompactNativeMetadata
		require.NoError(t, dec.CompactNativeMetadata(rec, &decoded))
		require.Equal(t, values, decoded.Values)
		require.Equal(t, entries, normalizeCompactNativeMetadata(decoded.Entries))
		// Decoding again reuses the record and replaces its contents.
		require.NoError(t, dec.CompactNativeMetadata(enc.CompactNativeMetadata(values[:1], entries[:1], nil), &decoded))
		require.Equal(t, values[:1], decoded.Values)
		require.Equal(t, entries[:1], normalizeCompactNativeMetadata(decoded.Entries))
	})

	t.Run("values in any order are renumbered by first use", func(t *testing.T) {
		// The first entry uses value 4 first, and value 2 is unused, so it is
		// not encoded.
		shuffled := []RefCompactNativeMetadata{
			{Ref: 1, Kind: NativeMetadataGroup, Points: []CompactNativeMetadataPoint{point(1, 4), point(2, 0)}},
			{Ref: 2, Kind: NativeMetadataGroup, Points: []CompactNativeMetadataPoint{point(3, 0), point(4, 3)}},
		}
		before := slices.Clone(shuffled[0].Points)
		var decoded CompactNativeMetadata
		require.NoError(t, dec.CompactNativeMetadata(enc.CompactNativeMetadata(values, shuffled, nil), &decoded))
		require.Equal(t, []NativeMetadataValue{values[4], values[0], values[3]}, decoded.Values)
		got, _ := decoded.AppendNativeMetadata(nil, nil)
		want, _ := (&CompactNativeMetadata{Values: values, Entries: shuffled}).AppendNativeMetadata(nil, nil)
		require.Equal(t, normalizeNativeMetadata(want), normalizeNativeMetadata(got))
		require.Equal(t, before, shuffled[0].Points, "the caller's points are unchanged")
	})

	t.Run("borrowed strings alias the record", func(t *testing.T) {
		buf := slices.Clone(rec)
		var borrowed, copied CompactNativeMetadata
		require.NoError(t, dec.CompactNativeMetadataBorrowed(buf, &borrowed))
		require.NoError(t, dec.CompactNativeMetadata(buf, &copied))
		require.Equal(t, values, borrowed.Values)
		start := uintptr(unsafe.Pointer(unsafe.SliceData(buf)))
		inRecord := func(s string) bool {
			p := uintptr(unsafe.Pointer(unsafe.StringData(s)))
			return p >= start && p < start+uintptr(len(buf))
		}
		for i, v := range borrowed.Values {
			for _, s := range []string{v.Unit, v.Help} {
				if s != "" {
					require.True(t, inRecord(s))
				}
			}
			require.False(t, inRecord(copied.Values[i].Unit) || inRecord(copied.Values[i].Help))
		}
		// Empty strings refer to no memory, so they never pin the record.
		require.Nil(t, unsafe.StringData(borrowed.Values[2].Help))
		require.Nil(t, unsafe.StringData(borrowed.Values[3].Unit))
		clear(buf)
		require.Equal(t, values, copied.Values)
		require.NotEqual(t, values, borrowed.Values)
	})

	t.Run("payload bytes", func(t *testing.T) {
		// The size of a record follows from its contents. Exact WAL byte
		// checks rely on this formula.
		zigzag := func(x int64) uint64 { return uint64(x<<1) ^ uint64(x>>63) }
		base := int64(math.MinInt64)
		size := 1 + 1 + uvarintSize(zigzag(base)) + uvarintSize(uint64(len(entries)))
		var previous chunks.HeadSeriesRef
		defined := uint32(0)
		for _, e := range entries {
			size += 1 + uvarintSize(zigzag(int64(e.Ref-previous))) + uvarintSize(uint64(len(e.Points)))
			previous = e.Ref
			for _, p := range e.Points {
				size += uvarintSize(uint64(p.EffectiveFrom) - uint64(base))
				if p.Value < defined {
					size += uvarintSize(uint64(p.Value) + 1)
					continue
				}
				v := values[p.Value]
				size += 1 + 1 + uvarintSize(uint64(len(v.Unit))) + len(v.Unit) + uvarintSize(uint64(len(v.Help))) + len(v.Help)
				defined++
			}
		}
		require.Len(t, rec, size)
	})

	t.Run("materialized entries", func(t *testing.T) {
		var decoded CompactNativeMetadata
		require.NoError(t, dec.CompactNativeMetadata(rec, &decoded))
		materialized, points := decoded.AppendNativeMetadata(nil, nil)
		require.Len(t, materialized, len(entries))
		total := 0
		for i, e := range entries {
			want := RefNativeMetadata{Ref: e.Ref, Kind: e.Kind, Truncated: e.Truncated}
			for _, p := range e.Points {
				v := values[p.Value]
				want.Points = append(want.Points, RefNativeMetadataPoint{EffectiveFrom: p.EffectiveFrom, Type: v.Type, Unit: v.Unit, Help: v.Help})
			}
			require.Equal(t, want, normalizeNativeMetadata(materialized[i : i+1])[0])
			total += len(e.Points)
		}
		require.Len(t, points, total)
		// Points of one value share its strings.
		require.Same(t, unsafe.StringData(materialized[0].Points[0].Help), unsafe.StringData(materialized[1].Points[1].Help))
		require.Same(t, unsafe.StringData(decoded.Values[1].Help), unsafe.StringData(materialized[1].Points[0].Help))

		// Legacy readers get each entry's newest value, and empty metadata
		// for an override without points, as for Metadata records.
		require.Equal(t, []RefMetadata{
			{Ref: 7, Type: uint8(Counter), Unit: "seconds", Help: "a"},
			{Ref: 2},
			{Ref: math.MaxUint64},
			{Ref: 0, Type: uint8(Gauge), Unit: "seconds", Help: values[1].Help},
			{Ref: 5},
			{Ref: 6},
			{Ref: 6, Type: uint8(Counter), Unit: "seconds", Help: "a"},
		}, decoded.AppendLegacy(nil))
		legacy, err := dec.Metadata(enc.NativeMetadata([]RefNativeMetadata{{Ref: 5, Kind: NativeMetadataOverride, Truncated: true}}, nil), nil)
		require.NoError(t, err)
		require.Equal(t, []RefMetadata{{Ref: 5}}, legacy, "the Metadata record equivalent")
	})

	t.Run("records without points", func(t *testing.T) {
		empty := []RefCompactNativeMetadata{{Ref: 3, Kind: NativeMetadataOverride}, {Ref: 4, Kind: NativeMetadataOverride, Truncated: true}}
		rec := enc.CompactNativeMetadata(nil, empty, nil)
		require.Equal(t, []byte{byte(NativeMetadataCompact), compactNativeMetadataInlineFormat, 0, 2, 1, 6, 0, 9, 2, 0}, rec)
		var decoded CompactNativeMetadata
		require.NoError(t, dec.CompactNativeMetadata(rec, &decoded))
		require.Equal(t, empty, normalizeCompactNativeMetadata(decoded.Entries))
		// Decoders ignore the base of a record without points.
		rec[2] = 0x7f
		require.NoError(t, dec.CompactNativeMetadata(rec, &decoded))
		require.Equal(t, empty, normalizeCompactNativeMetadata(decoded.Entries))
	})

	t.Run("unknown kinds and flags", func(t *testing.T) {
		for _, c := range []struct {
			name      string
			kind      byte
			truncated bool
		}{
			{name: "future kind", kind: 2},
			{name: "highest kind", kind: 7},
			{name: "future kind with truncation", kind: 5 | compactNativeMetadataTruncatedFlag, truncated: true},
			{name: "group with future flag", kind: compactNativeMetadataGroup | 1<<4},
			{name: "override with future flags", kind: compactNativeMetadataOverride | compactNativeMetadataTruncatedFlag | 0xf0, truncated: true},
		} {
			t.Run(c.name, func(t *testing.T) {
				for _, points := range [][]CompactNativeMetadataPoint{nil, {point(50, 0)}, {point(200, 0), point(100, 1), point(300, 0)}} {
					rec := compactNativeMetadataTestRecord(values[:2], 9, c.kind, points)
					var decoded CompactNativeMetadata
					require.NoError(t, dec.CompactNativeMetadata(rec, &decoded))
					want := RefCompactNativeMetadata{Ref: 9, Kind: NativeMetadataUnknown, Truncated: c.truncated}
					if len(points) > 0 {
						// The newest point is the last, even out of order.
						want.Points = points[len(points)-1:]
					}
					got := normalizeCompactNativeMetadata(decoded.Entries)
					require.Equal(t, []RefCompactNativeMetadata{want}, got)
					require.Equal(t, len(points) == 0, got[0].Ignored())
					// Legacy readers skip ignored entries.
					require.Len(t, decoded.AppendLegacy(nil), len(want.Points))
					materialized, _ := decoded.AppendNativeMetadata(nil, nil)
					require.Equal(t, len(points) == 0, materialized[0].Ignored())
				}
			})
		}
		// Known kinds are never ignored, even without points.
		require.False(t, RefCompactNativeMetadata{Kind: NativeMetadataOverride}.Ignored())
		require.False(t, RefNativeMetadata{Kind: NativeMetadataOverride}.Ignored())
	})

	t.Run("values defined in unknown entries stay defined", func(t *testing.T) {
		// An unknown entry keeps only its newest point, but later entries may
		// refer to every value it defines.
		buf := encoding.Encbuf{}
		buf.PutByte(byte(NativeMetadataCompact))
		buf.PutByte(compactNativeMetadataInlineFormat)
		buf.PutVarint64(10)
		buf.PutUvarint(2)
		buf.PutByte(7)
		buf.PutVarint64(1)
		buf.PutUvarint(2)
		for _, help := range []string{"x", "y"} {
			buf.PutUvarint(0)
			buf.PutUvarint(0)
			buf.PutByte(uint8(Gauge))
			buf.PutUvarintStr("")
			buf.PutUvarintStr(help)
		}
		buf.PutByte(compactNativeMetadataGroup)
		buf.PutVarint64(1)
		buf.PutUvarint(1)
		buf.PutUvarint(5)
		buf.PutUvarint(1)
		var decoded CompactNativeMetadata
		require.NoError(t, dec.CompactNativeMetadata(buf.Get(), &decoded))
		require.Equal(t, []NativeMetadataValue{{Type: uint8(Gauge), Help: "x"}, {Type: uint8(Gauge), Help: "y"}}, decoded.Values)
		require.Equal(t, []RefCompactNativeMetadata{
			{Ref: 1, Kind: NativeMetadataUnknown, Points: []CompactNativeMetadataPoint{point(10, 1)}},
			{Ref: 2, Kind: NativeMetadataGroup, Points: []CompactNativeMetadataPoint{point(15, 0)}},
		}, normalizeCompactNativeMetadata(decoded.Entries))
	})

	t.Run("framing errors", func(t *testing.T) {
		// Counts are explicit, so every cut is detected.
		for n := range len(rec) {
			var decoded CompactNativeMetadata
			require.Error(t, dec.CompactNativeMetadata(rec[:n], &decoded), "cut at %d", n)
			require.Empty(t, decoded.Values)
			require.Empty(t, decoded.Entries)
			require.Error(t, dec.CompactNativeMetadataBorrowed(rec[:n], &decoded), "cut at %d", n)
		}
		header := func(format byte, entries uint64) encoding.Encbuf {
			buf := encoding.Encbuf{}
			buf.PutByte(byte(NativeMetadataCompact))
			buf.PutByte(format)
			buf.PutVarint64(0)
			buf.PutUvarint64(entries)
			return buf
		}
		entry := func(buf *encoding.Encbuf, kind byte, points uint64) {
			buf.PutByte(kind)
			buf.PutVarint64(1)
			buf.PutUvarint64(points)
		}
		definition := func(buf *encoding.Encbuf, help string) {
			buf.PutUvarint(0)
			buf.PutUvarint(0)
			buf.PutByte(uint8(Counter))
			buf.PutUvarintStr("")
			buf.PutUvarintStr(help)
		}
		reference := func(buf *encoding.Encbuf, tag uint64) {
			buf.PutUvarint(0)
			buf.PutUvarint64(tag)
		}
		cases := map[string][]byte{
			"wrong record type": enc.Metadata(nil, nil),
			// An appended complete entry is caught as trailing bytes.
			"appended entry": append(slices.Clone(rec), compactNativeMetadataOverride, 0, 0),
			"empty group": func() []byte {
				buf := header(compactNativeMetadataInlineFormat, 1)
				entry(&buf, compactNativeMetadataGroup, 0)
				return buf.Get()
			}(),
			"empty truncated group": func() []byte {
				buf := header(compactNativeMetadataInlineFormat, 1)
				entry(&buf, compactNativeMetadataGroup|compactNativeMetadataTruncatedFlag, 0)
				return buf.Get()
			}(),
			"reference without definitions": func() []byte {
				buf := header(compactNativeMetadataInlineFormat, 1)
				entry(&buf, compactNativeMetadataGroup, 1)
				reference(&buf, 1)
				return buf.Get()
			}(),
			"reference to the value being defined": func() []byte {
				buf := header(compactNativeMetadataInlineFormat, 1)
				entry(&buf, compactNativeMetadataGroup, 2)
				definition(&buf, "a")
				reference(&buf, 2)
				return buf.Get()
			}(),
			"reference past every definition": func() []byte {
				buf := header(compactNativeMetadataInlineFormat, 1)
				entry(&buf, compactNativeMetadataGroup, 2)
				definition(&buf, "a")
				reference(&buf, math.MaxUint64)
				return buf.Get()
			}(),
			"reference from an unknown entry without definitions": func() []byte {
				buf := header(compactNativeMetadataInlineFormat, 1)
				entry(&buf, 7, 1)
				reference(&buf, 1)
				return buf.Get()
			}(),
			"definition beyond the record": func() []byte {
				buf := header(compactNativeMetadataInlineFormat, 1)
				entry(&buf, compactNativeMetadataGroup, 1)
				buf.PutUvarint(0)
				buf.PutUvarint(0)
				buf.PutByte(0)
				buf.PutUvarint(5)
				buf.PutString("ab")
				return buf.Get()
			}(),
		}
		// Each count must fit the remaining bytes at its minimum element size.
		for name, minBytes := range map[string]int{"entries": compactNativeMetadataEntryMinBytes, "points": compactNativeMetadataPointMinBytes} {
			for _, count := range []uint64{2, math.MaxInt32 + 1, math.MaxUint32 + 1, math.MaxInt64 + 1, math.MaxUint64} {
				var buf encoding.Encbuf
				switch name {
				case "entries":
					buf = header(compactNativeMetadataInlineFormat, count)
				case "points":
					buf = header(compactNativeMetadataInlineFormat, 1)
					entry(&buf, compactNativeMetadataOverride, count)
				}
				// Room for one fewer element than the smallest count.
				buf.PutBytes(make([]byte, minBytes))
				cases[fmt.Sprintf("%s count %d", name, count)] = buf.Get()
			}
		}
		for format := range 256 {
			if byte(format) != compactNativeMetadataInlineFormat {
				buf := header(byte(format), 0)
				cases[fmt.Sprintf("format %d", format)] = buf.Get()
			}
		}
		for name, rec := range cases {
			var decoded CompactNativeMetadata
			err := dec.CompactNativeMetadata(rec, &decoded)
			require.Error(t, err, name)
			require.Equal(t, err, dec.CompactNativeMetadataBorrowed(rec, &decoded), name)
			require.Empty(t, decoded.Values, name)
		}
		// The smallest valid record has no entries.
		var decoded CompactNativeMetadata
		require.NoError(t, dec.CompactNativeMetadata([]byte{byte(NativeMetadataCompact), compactNativeMetadataInlineFormat, 0, 0}, &decoded))
	})

	t.Run("random contents", func(t *testing.T) {
		r := rand.New(rand.NewSource(1))
		froms := []int64{math.MinInt64, math.MinInt64 + 1, -1, 0, 1, math.MaxInt64 - 1, math.MaxInt64}
		var decoded CompactNativeMetadata
		for range 2000 {
			values := make([]NativeMetadataValue, r.Intn(5))
			for i := range values {
				values[i] = NativeMetadataValue{Type: uint8(r.Intn(9)), Unit: strings.Repeat("u", r.Intn(3)), Help: strings.Repeat("h", r.Intn(200))}
			}
			entries := make([]RefCompactNativeMetadata, r.Intn(6))
			for i := range entries {
				e := &entries[i]
				e.Ref = chunks.HeadSeriesRef(r.Uint64() >> r.Intn(64))
				e.Kind = NativeMetadataOverride
				e.Truncated = r.Intn(2) == 0
				if r.Intn(2) == 0 {
					e.Kind, e.Truncated = NativeMetadataGroup, false
				}
				count := r.Intn(6)
				if e.Kind == NativeMetadataGroup {
					count = 1 + r.Intn(5)
				}
				if len(values) == 0 {
					count = 0
					e.Kind = NativeMetadataOverride
				}
				for range count {
					from := r.Int63n(1000) - 500
					if r.Intn(4) == 0 {
						from = froms[r.Intn(len(froms))]
					}
					e.Points = append(e.Points, CompactNativeMetadataPoint{EffectiveFrom: from, Value: uint32(r.Intn(len(values)))})
				}
				slices.SortFunc(e.Points, func(a, b CompactNativeMetadataPoint) int { return cmp.Compare(a.EffectiveFrom, b.EffectiveFrom) })
			}
			rec := enc.CompactNativeMetadata(values, entries, nil)
			require.NoError(t, dec.CompactNativeMetadata(rec, &decoded))
			// The points carry the same values; only the values used are
			// defined, in order of first use.
			got, _ := decoded.AppendNativeMetadata(nil, nil)
			want, _ := (&CompactNativeMetadata{Values: values, Entries: entries}).AppendNativeMetadata(nil, nil)
			require.Equal(t, normalizeNativeMetadata(want), normalizeNativeMetadata(got))
			require.True(t, compactNativeMetadataInFirstUseOrder(decoded.Entries))
			used := map[uint32]bool{}
			for _, e := range entries {
				for _, p := range e.Points {
					used[p.Value] = true
				}
			}
			require.Len(t, decoded.Values, len(used))
		}
	})
}

// compactNativeMetadataTestRecord encodes a record of one entry with any kind
// byte, including unknown kinds and flags, and any points.
func compactNativeMetadataTestRecord(values []NativeMetadataValue, ref chunks.HeadSeriesRef, kind byte, points []CompactNativeMetadataPoint) []byte {
	var enc Encoder
	rec := enc.CompactNativeMetadata(values, []RefCompactNativeMetadata{{Ref: ref, Kind: NativeMetadataOverride, Points: points}}, nil)
	// The kind byte follows the type, format, base and a one-byte count.
	var base int64
	for i, p := range points {
		if i == 0 || p.EffectiveFrom < base {
			base = p.EffectiveFrom
		}
	}
	rec[2+uvarintSize(uint64(base<<1)^uint64(base>>63))+1] = kind
	return rec
}

// normalizeCompactNativeMetadata clears capacity differences for comparisons.
func normalizeCompactNativeMetadata(entries []RefCompactNativeMetadata) []RefCompactNativeMetadata {
	if len(entries) == 0 {
		return nil
	}
	out := make([]RefCompactNativeMetadata, len(entries))
	for i, e := range entries {
		if len(e.Points) == 0 {
			e.Points = nil
		}
		out[i] = e
	}
	return out
}

func FuzzDecoderCompactNativeMetadata(f *testing.F) {
	var enc Encoder
	values := []NativeMetadataValue{{Type: uint8(Counter), Unit: "u", Help: "a"}, {Help: "b"}}
	f.Add(enc.CompactNativeMetadata(values, []RefCompactNativeMetadata{
		{Ref: 1, Kind: NativeMetadataGroup, Points: []CompactNativeMetadataPoint{{EffectiveFrom: 1}, {EffectiveFrom: 2, Value: 1}}},
		{Ref: 2, Kind: NativeMetadataOverride, Truncated: true},
		{Ref: 3, Kind: NativeMetadataGroup, Points: []CompactNativeMetadataPoint{{EffectiveFrom: 3, Value: 1}, {EffectiveFrom: 4}}},
	}, nil))
	f.Add(compactNativeMetadataTestRecord(values, 3, 0xff, []CompactNativeMetadataPoint{{EffectiveFrom: math.MinInt64, Value: 1}}))
	f.Add([]byte{byte(NativeMetadataCompact), compactNativeMetadataInlineFormat, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01})
	f.Fuzz(func(t *testing.T, rec []byte) {
		var dec Decoder
		var copied, borrowed CompactNativeMetadata
		err := dec.CompactNativeMetadata(rec, &copied)
		require.Equal(t, err, dec.CompactNativeMetadataBorrowed(rec, &borrowed))
		require.Equal(t, copied, borrowed)
		if err != nil {
			require.Empty(t, copied.Entries)
			return
		}
		known := true
		for _, e := range copied.Entries {
			for _, p := range e.Points {
				require.Less(t, int(p.Value), len(copied.Values))
			}
			switch e.Kind {
			case NativeMetadataGroup:
				require.NotEmpty(t, e.Points)
			case NativeMetadataOverride:
			case NativeMetadataUnknown:
				require.LessOrEqual(t, len(e.Points), 1)
				known = false
			default:
				t.Fatalf("unexpected kind %d", e.Kind)
			}
		}
		if !known {
			return
		}
		// Records of known kinds define their values in order of first use,
		// and survive re-encoding.
		require.True(t, compactNativeMetadataInFirstUseOrder(copied.Entries))
		var again CompactNativeMetadata
		require.NoError(t, dec.CompactNativeMetadata(enc.CompactNativeMetadata(copied.Values, copied.Entries, nil), &again))
		require.Equal(t, copied.Values, again.Values)
		require.Equal(t, normalizeCompactNativeMetadata(copied.Entries), normalizeCompactNativeMetadata(again.Entries))
	})
}

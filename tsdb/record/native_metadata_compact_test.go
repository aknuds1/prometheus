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
	values := []NativeMetadataValue{
		{Type: uint8(Counter), Unit: "seconds", Help: "a"},
		{Type: uint8(Gauge), Unit: "seconds", Help: strings.Repeat("b", 300)},
		{},
		{Type: uint8(Summary), Help: "c"},
		{Type: uint8(Counter), Unit: "seconds", Help: "a"}, // Equal values may repeat.
	}
	point := func(from int64, value uint32) CompactNativeMetadataPoint {
		return CompactNativeMetadataPoint{EffectiveFrom: from, Value: value}
	}
	entries := []RefCompactNativeMetadata{
		{Ref: 7, Kind: NativeMetadataGroup, Points: []CompactNativeMetadataPoint{point(100, 0)}},
		{Ref: 2, Kind: NativeMetadataGroup, Points: []CompactNativeMetadataPoint{point(-5, 1), point(150, 0), point(180, 4)}},
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
		size := 1 + 1 + uvarintSize(zigzag(base)) + uvarintSize(uint64(len(values)))
		for _, v := range values {
			size += 1 + uvarintSize(uint64(len(v.Unit))) + len(v.Unit) + uvarintSize(uint64(len(v.Help))) + len(v.Help)
		}
		size += uvarintSize(uint64(len(entries)))
		var previous chunks.HeadSeriesRef
		for _, e := range entries {
			size += 1 + uvarintSize(zigzag(int64(e.Ref-previous))) + uvarintSize(uint64(len(e.Points)))
			previous = e.Ref
			for _, p := range e.Points {
				size += uvarintSize(uint64(p.EffectiveFrom)-uint64(base)) + uvarintSize(uint64(p.Value))
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
			{Ref: 2, Type: uint8(Counter), Unit: "seconds", Help: "a"},
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
		require.Equal(t, []byte{byte(NativeMetadataCompact), compactNativeMetadataValueFormat, 0, 0, 2, 1, 6, 0, 9, 2, 0}, rec)
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
				for _, points := range [][]CompactNativeMetadataPoint{nil, {point(50, 1)}, {point(200, 0), point(100, 1), point(300, 0)}} {
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

	t.Run("framing errors", func(t *testing.T) {
		// Counts are explicit, so every cut is detected.
		for n := range len(rec) {
			var decoded CompactNativeMetadata
			require.Error(t, dec.CompactNativeMetadata(rec[:n], &decoded), "cut at %d", n)
			require.Empty(t, decoded.Values)
			require.Empty(t, decoded.Entries)
			require.Error(t, dec.CompactNativeMetadataBorrowed(rec[:n], &decoded), "cut at %d", n)
		}
		header := func(format byte, count uint64) encoding.Encbuf {
			buf := encoding.Encbuf{}
			buf.PutByte(byte(NativeMetadataCompact))
			buf.PutByte(format)
			buf.PutVarint64(0)
			buf.PutUvarint64(count)
			return buf
		}
		entry := func(buf *encoding.Encbuf, kind byte, points uint64) {
			buf.PutByte(kind)
			buf.PutVarint64(1)
			buf.PutUvarint64(points)
		}
		cases := map[string][]byte{
			"wrong record type": enc.Metadata(nil, nil),
			// An appended complete entry is caught as trailing bytes.
			"appended entry": append(slices.Clone(rec), compactNativeMetadataOverride, 0, 0),
			"empty group": func() []byte {
				buf := header(compactNativeMetadataValueFormat, 0)
				buf.PutUvarint(1)
				entry(&buf, compactNativeMetadataGroup, 0)
				return buf.Get()
			}(),
			"empty truncated group": func() []byte {
				buf := header(compactNativeMetadataValueFormat, 0)
				buf.PutUvarint(1)
				entry(&buf, compactNativeMetadataGroup|compactNativeMetadataTruncatedFlag, 0)
				return buf.Get()
			}(),
			"value index outside the dictionary":                     compactNativeMetadataTestRecord(values[:2], 1, compactNativeMetadataGroup, []CompactNativeMetadataPoint{point(1, 2)}),
			"value index of an unknown entry outside the dictionary": compactNativeMetadataTestRecord(values[:2], 1, 7, []CompactNativeMetadataPoint{point(1, math.MaxUint32)}),
			"string beyond the record": func() []byte {
				buf := header(compactNativeMetadataValueFormat, 1)
				buf.PutByte(0)
				buf.PutUvarint(5)
				buf.PutString("ab")
				return buf.Get()
			}(),
		}
		// Each count must fit the remaining bytes at its minimum element size.
		for name, minBytes := range map[string]int{"values": compactNativeMetadataValueMinBytes, "entries": compactNativeMetadataEntryMinBytes, "points": compactNativeMetadataPointMinBytes} {
			for _, count := range []uint64{2, math.MaxInt32 + 1, math.MaxUint32 + 1, math.MaxInt64 + 1, math.MaxUint64} {
				buf := header(compactNativeMetadataValueFormat, 0)
				switch name {
				case "values":
					buf = header(compactNativeMetadataValueFormat, count)
				case "entries":
					buf.PutUvarint64(count)
				case "points":
					buf.PutUvarint(1)
					entry(&buf, compactNativeMetadataOverride, count)
				}
				// Room for one fewer element than the smallest count.
				buf.PutBytes(make([]byte, minBytes))
				cases[fmt.Sprintf("%s count %d", name, count)] = buf.Get()
			}
		}
		for format := range 256 {
			if byte(format) != compactNativeMetadataValueFormat {
				buf := header(byte(format), 0)
				buf.PutUvarint(0)
				cases[fmt.Sprintf("format %d", format)] = buf.Get()
			}
		}
		for name, rec := range cases {
			var decoded CompactNativeMetadata
			err := dec.CompactNativeMetadata(rec, &decoded)
			require.Error(t, err, name)
			require.Equal(t, err, dec.CompactNativeMetadataBorrowed(rec, &decoded), name)
		}
		// The smallest valid record has an empty dictionary and no entries.
		var decoded CompactNativeMetadata
		require.NoError(t, dec.CompactNativeMetadata([]byte{byte(NativeMetadataCompact), compactNativeMetadataValueFormat, 0, 0, 0}, &decoded))
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
			require.Equal(t, normalizeValues(values), normalizeValues(decoded.Values))
			require.Equal(t, normalizeCompactNativeMetadata(entries), normalizeCompactNativeMetadata(decoded.Entries))
		}
	})
}

// compactNativeMetadataTestRecord encodes a record of one entry with any kind
// byte, including unknown kinds and flags, and any points.
func compactNativeMetadataTestRecord(values []NativeMetadataValue, ref chunks.HeadSeriesRef, kind byte, points []CompactNativeMetadataPoint) []byte {
	var enc Encoder
	rec := enc.CompactNativeMetadata(values, []RefCompactNativeMetadata{{Ref: ref, Kind: NativeMetadataOverride, Points: points}}, nil)
	// The entry is the record's tail: kind byte, ref delta, count and points.
	tail := 1 + uvarintSize(uint64(ref)<<1) + uvarintSize(uint64(len(points)))
	var base int64
	for i, p := range points {
		if i == 0 || p.EffectiveFrom < base {
			base = p.EffectiveFrom
		}
	}
	for _, p := range points {
		tail += uvarintSize(uint64(p.EffectiveFrom)-uint64(base)) + uvarintSize(uint64(p.Value))
	}
	rec[len(rec)-tail] = kind
	return rec
}

func normalizeValues(values []NativeMetadataValue) []NativeMetadataValue {
	if len(values) == 0 {
		return nil
	}
	return values
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
	}, nil))
	f.Add(compactNativeMetadataTestRecord(values, 3, 0xff, []CompactNativeMetadataPoint{{EffectiveFrom: math.MinInt64, Value: 1}}))
	f.Add([]byte{byte(NativeMetadataCompact), compactNativeMetadataValueFormat, 0, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01})
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
		// Records of known kinds survive re-encoding.
		var again CompactNativeMetadata
		require.NoError(t, dec.CompactNativeMetadata(enc.CompactNativeMetadata(copied.Values, copied.Entries, nil), &again))
		require.Equal(t, normalizeValues(copied.Values), normalizeValues(again.Values))
		require.Equal(t, normalizeCompactNativeMetadata(copied.Entries), normalizeCompactNativeMetadata(again.Entries))
	})
}

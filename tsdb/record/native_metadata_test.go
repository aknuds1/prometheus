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
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/tsdb/encoding"
)

func TestNativeMetadataRecord(t *testing.T) {
	point := func(from int64, typ MetricType, help string) RefNativeMetadataPoint {
		return RefNativeMetadataPoint{EffectiveFrom: from, Type: uint8(typ), Unit: "seconds", Help: help}
	}
	entries := []RefNativeMetadata{
		{Ref: 1, Kind: NativeMetadataGroup, Points: []RefNativeMetadataPoint{point(100, Counter, "a")}},
		{Ref: 2, Kind: NativeMetadataGroup, Points: []RefNativeMetadataPoint{point(-5, Gauge, "b"), point(150, Counter, "c"), point(180, HistogramSample, strings.Repeat("d", 300))}},
		{Ref: 3, Kind: NativeMetadataOverride, Points: []RefNativeMetadataPoint{point(math.MinInt64, Summary, ""), point(math.MaxInt64, Info, "e")}},
		{Ref: 4, Kind: NativeMetadataOverride, Truncated: true, Points: []RefNativeMetadataPoint{point(10, Stateset, "f")}},
		{Ref: 5, Kind: NativeMetadataOverride, Truncated: true},
		{Ref: 6, Kind: NativeMetadataOverride},
		{Ref: 7, Kind: NativeMetadataLegacy, Points: []RefNativeMetadataPoint{point(math.MinInt64, GaugeHistogram, "g")}},
	}
	var enc Encoder
	var dec Decoder
	rec := enc.NativeMetadata(entries, nil)
	require.Equal(t, Metadata, dec.Type(rec))

	t.Run("round trip", func(t *testing.T) {
		decoded, points, err := dec.NativeMetadata(rec, nil, nil)
		require.NoError(t, err)
		require.Equal(t, entries, normalizeNativeMetadata(decoded))
		require.Len(t, points, 8)
		// Appending keeps earlier entries intact.
		appended, _, err := dec.NativeMetadata(rec, decoded, points)
		require.NoError(t, err)
		require.Len(t, appended, 2*len(entries))
		require.Equal(t, entries, normalizeNativeMetadata(appended[:len(entries)]))
		require.Equal(t, entries, normalizeNativeMetadata(appended[len(entries):]))
		reused, _, err := dec.NativeMetadata(rec, decoded[:0], points[:0])
		require.NoError(t, err)
		require.Equal(t, entries, normalizeNativeMetadata(reused))
	})

	t.Run("older decoders read the newest point", func(t *testing.T) {
		legacy, err := dec.Metadata(rec, nil)
		require.NoError(t, err)
		require.Len(t, legacy, len(entries))
		for i, e := range entries {
			want := RefMetadata{Ref: e.Ref, Type: uint8(UnknownMT)}
			if n := len(e.Points); n > 0 {
				newest := e.Points[n-1]
				want = RefMetadata{Ref: e.Ref, Type: newest.Type, Unit: newest.Unit, Help: newest.Help}
			}
			require.Equal(t, want, legacy[i])
		}
		// Legacy entries are byte-identical to legacy records.
		require.Equal(t, enc.Metadata([]RefMetadata{legacy[6]}, nil), enc.NativeMetadata(entries[6:], nil))
	})

	t.Run("legacy records", func(t *testing.T) {
		decoded, _, err := dec.NativeMetadata(enc.Metadata([]RefMetadata{{Ref: 9, Type: uint8(Gauge), Unit: "u", Help: "h"}}, nil), nil, nil)
		require.NoError(t, err)
		require.Equal(t, []RefNativeMetadata{{Ref: 9, Kind: NativeMetadataLegacy, Points: []RefNativeMetadataPoint{{EffectiveFrom: math.MinInt64, Type: uint8(Gauge), Unit: "u", Help: "h"}}}}, decoded)
	})

	t.Run("payload bytes", func(t *testing.T) {
		// The size of each entry follows from its fields. Exact WAL byte checks
		// rely on this formula.
		size := 1
		for _, e := range entries {
			var newest RefNativeMetadataPoint
			if n := len(e.Points); n > 0 {
				newest = e.Points[n-1]
			}
			size += uvarintSize(uint64(e.Ref)) + 1 + 1
			size += 1 + len(unitMetaName) + uvarintSize(uint64(len(newest.Unit))) + len(newest.Unit)
			size += 1 + len(helpMetaName) + uvarintSize(uint64(len(newest.Help))) + len(newest.Help)
			if e.Kind == NativeMetadataLegacy {
				continue
			}
			if len(e.Points) > 0 {
				size += 11
			}
			if len(e.Points) > 1 {
				blob := uvarintSize(uint64(len(e.Points) - 1))
				for _, p := range e.Points[:len(e.Points)-1] {
					blob += 9 + uvarintSize(uint64(len(p.Unit))) + len(p.Unit) + uvarintSize(uint64(len(p.Help))) + len(p.Help)
				}
				size += 2 + uvarintSize(uint64(blob)) + blob
			}
			if e.Kind == NativeMetadataOverride {
				size += 4
			}
		}
		require.Len(t, rec, size)
	})

	t.Run("unknown kinds", func(t *testing.T) {
		for _, c := range []struct {
			name      string
			fields    []string
			from      int64
			truncated bool
		}{
			{name: "future flag", fields: []string{"f", "g", "k\x11"}, from: 200},
			{name: "future flag without start", fields: []string{"k\x13"}, from: math.MinInt64, truncated: true},
			{name: "zero flags", fields: []string{"f", "k\x00"}, from: 200},
			{name: "truncation without override", fields: []string{"f", "k\x02"}, from: 200, truncated: true},
			{name: "earlier points without start", fields: []string{"g"}, from: math.MinInt64},
			{name: "override with earlier points without start", fields: []string{"g", "k\x01"}, from: math.MinInt64},
		} {
			t.Run(c.name, func(t *testing.T) {
				buf := encoding.Encbuf{}
				buf.PutByte(byte(Metadata))
				buf.PutUvarint64(0)
				buf.PutByte(uint8(Counter))
				buf.PutUvarint(2 + len(c.fields) + 1)
				buf.PutUvarintStr(unitMetaName)
				buf.PutUvarintStr("u")
				buf.PutUvarintStr(helpMetaName)
				buf.PutUvarintStr("h")
				for _, field := range c.fields {
					switch field[0] {
					case 'f':
						buf.PutUvarintStr("f")
						buf.PutUvarint(8)
						buf.PutBE64int64(200)
					case 'g':
						buf.PutUvarintStr("g")
						buf.PutUvarint(12)
						buf.PutUvarint(1)
						buf.PutBE64int64(100)
						buf.PutByte(uint8(Gauge))
						buf.PutUvarintStr("")
						buf.PutUvarintStr("")
					case 'k':
						buf.PutUvarintStr("k")
						buf.PutUvarintStr(field[1:])
					}
				}
				// Unknown fields are skipped.
				buf.PutUvarintStr("future")
				buf.PutUvarintStr("value")
				decoded, _, err := dec.NativeMetadata(buf.Get(), nil, nil)
				require.NoError(t, err)
				require.Equal(t, []RefNativeMetadata{{Kind: NativeMetadataUnknown, Truncated: c.truncated, Points: []RefNativeMetadataPoint{{EffectiveFrom: c.from, Type: uint8(Counter), Unit: "u", Help: "h"}}}}, decoded)
			})
		}
	})

	t.Run("framing errors", func(t *testing.T) {
		for n := 1; n < len(rec); n++ {
			// Only cuts at entry boundaries leave a valid record.
			decoded, _, err := dec.NativeMetadata(rec[:n], nil, nil)
			if err == nil {
				require.Less(t, len(decoded), len(entries))
				require.Equal(t, entries[:len(decoded)], normalizeNativeMetadata(decoded))
			}
		}
		for _, c := range []struct {
			name  string
			field string
			value []byte
		}{
			{name: "short start", field: "f", value: make([]byte, 7)},
			{name: "long flags", field: "k", value: []byte{1, 0}},
			{name: "empty group", field: "g", value: []byte{0}},
			{name: "oversized group count", field: "g", value: append([]byte{2}, make([]byte, 12)...)},
			{name: "trailing group bytes", field: "g", value: append([]byte{1}, make([]byte, 12)...)},
		} {
			t.Run(c.name, func(t *testing.T) {
				buf := encoding.Encbuf{}
				buf.PutByte(byte(Metadata))
				buf.PutUvarint64(1)
				buf.PutByte(uint8(Counter))
				buf.PutUvarint(1)
				buf.PutUvarintStr(c.field)
				buf.PutUvarintBytes(c.value)
				_, _, err := dec.NativeMetadata(buf.Get(), nil, nil)
				require.Error(t, err)
			})
		}
		_, _, err := dec.NativeMetadata(enc.Series(nil, nil), nil, nil)
		require.Error(t, err)
	})
}

// normalizeNativeMetadata clears capacity differences for comparisons.
func normalizeNativeMetadata(entries []RefNativeMetadata) []RefNativeMetadata {
	out := make([]RefNativeMetadata, len(entries))
	for i, e := range entries {
		if len(e.Points) == 0 {
			e.Points = nil
		}
		out[i] = e
	}
	return out
}

func FuzzDecoderNativeMetadata(f *testing.F) {
	var enc Encoder
	f.Add(enc.NativeMetadata([]RefNativeMetadata{
		{Ref: 1, Kind: NativeMetadataGroup, Points: []RefNativeMetadataPoint{{EffectiveFrom: 1, Help: "a"}, {EffectiveFrom: 2, Help: "b"}}},
		{Ref: 2, Kind: NativeMetadataOverride, Truncated: true},
	}, nil))
	f.Add(enc.Metadata([]RefMetadata{{Ref: 1, Unit: "u", Help: "h"}}, nil))
	// A field length above math.MaxInt64 once panicked instead of failing.
	f.Add([]byte("\x06000\xd8\xd8\xd8\xd8\xd8\xd8\xd8\xd8\xd8\x01"))
	f.Fuzz(func(t *testing.T, rec []byte) {
		var dec Decoder
		entries, points, err := dec.NativeMetadata(rec, nil, nil)
		if err != nil {
			return
		}
		total := 0
		for _, e := range entries {
			total += len(e.Points)
			switch e.Kind {
			case NativeMetadataLegacy, NativeMetadataUnknown:
				require.Len(t, e.Points, 1)
			case NativeMetadataGroup:
				require.NotEmpty(t, e.Points)
			case NativeMetadataOverride:
			default:
				t.Fatalf("unexpected kind %d", e.Kind)
			}
		}
		require.Len(t, points, total)
		// Older decoders accept every record the native decoder accepts.
		legacy, err := dec.Metadata(rec, nil)
		require.NoError(t, err)
		require.Len(t, legacy, len(entries))
	})
}

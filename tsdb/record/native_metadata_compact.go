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
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/encoding"
)

// A NativeMetadataCompact record holds native metadata entries whose points
// carry their values inline, each value defined at its first use and referred
// to by index after it:
//
//	record = type byte, format byte, base (varint), count (uvarint), count × entry
//	entry  = kind byte, ref delta (varint), count (uvarint), count × point
//	point  = start delta (uvarint), value
//	value  = 0 (uvarint), type byte, unit, help   the definition of the next value
//	       | n (uvarint), n ≥ 1                     a reference to value n−1
//
// Values are numbered in order of definition across the record's entries, of
// every kind; a reference to a value not defined before it is corruption.
// Strings are uvarint-length prefixed, and varints are zigzag-encoded. The base
// is the record's smallest start, or 0 without points. A point's start delta is
// its start minus the base, and an entry's ref delta is its ref minus the
// previous entry's, both modulo 2^64. The kind byte's low three bits select the
// kind and bit 3 is the truncation flag; other kinds and bits are unknown.
const (
	compactNativeMetadataInlineFormat byte = 3

	compactNativeMetadataKindMask      byte = 0x07
	compactNativeMetadataGroup         byte = 0
	compactNativeMetadataOverride      byte = 1
	compactNativeMetadataTruncatedFlag byte = 1 << 3

	// Smallest encodings of an entry and a point; a point that refers to a
	// defined value is the smallest.
	compactNativeMetadataEntryMinBytes = 1 + 1 + 1
	compactNativeMetadataPointMinBytes = 1 + 1
)

// NativeMetadataValue is a metadata value in a compact record's dictionary.
type NativeMetadataValue struct {
	Type       uint8
	Unit, Help string
}

// RefCompactNativeMetadata is one entry of a compact native metadata record.
// Kinds are as in RefNativeMetadata, except that compact records have no legacy
// entries, and an unknown entry has no points if it was written without any.
type RefCompactNativeMetadata struct {
	Ref       chunks.HeadSeriesRef
	Kind      NativeMetadataKind
	Truncated bool
	// Points are chronological for valid entries; the last is the newest.
	Points []CompactNativeMetadataPoint
}

// Ignored reports whether e leaves its ref's state unchanged: it is of unknown
// kind and has no points. Consumers count such entries as unknown.
func (e RefCompactNativeMetadata) Ignored() bool {
	return e.Kind == NativeMetadataUnknown && len(e.Points) == 0
}

// CompactNativeMetadataPoint is a metadata change point in milliseconds whose
// value is an index into its record's dictionary.
type CompactNativeMetadataPoint struct {
	EffectiveFrom int64
	Value         uint32
}

// CompactNativeMetadata is a decoded compact native metadata record. Decoding
// into it reuses its slices.
type CompactNativeMetadata struct {
	Values []NativeMetadataValue
	// Entries' points alias memory that the next decoding reuses.
	Entries []RefCompactNativeMetadata
	points  []CompactNativeMetadataPoint
}

// Reset empties r, keeping its capacity but no strings or points.
func (r *CompactNativeMetadata) Reset() {
	clear(r.Values)
	clear(r.Entries)
	r.Values, r.Entries, r.points = r.Values[:0], r.Entries[:0], r.points[:0]
}

// AppendNativeMetadata appends r's entries to entries, and their points to
// points, each point carrying its value's strings, which points of one value
// share. Each entry's Points alias the returned point slice.
func (r *CompactNativeMetadata) AppendNativeMetadata(entries []RefNativeMetadata, points []RefNativeMetadataPoint) ([]RefNativeMetadata, []RefNativeMetadataPoint) {
	for _, e := range r.Entries {
		start := len(points)
		for _, p := range e.Points {
			v := r.Values[p.Value]
			points = append(points, RefNativeMetadataPoint{EffectiveFrom: p.EffectiveFrom, Type: v.Type, Unit: v.Unit, Help: v.Help})
		}
		entries = append(entries, RefNativeMetadata{
			Ref: e.Ref, Kind: e.Kind, Truncated: e.Truncated,
			Points: points[start:len(points):len(points)],
		})
	}
	return entries, points
}

// AppendLegacy appends r's entries to dst as legacy metadata, as legacy
// decoders read native entries in Metadata records: each entry's newest point,
// and empty metadata for an override without points. Entries that leave state
// unchanged are skipped.
func (r *CompactNativeMetadata) AppendLegacy(dst []RefMetadata) []RefMetadata {
	for _, e := range r.Entries {
		switch n := len(e.Points); {
		case n > 0:
			v := r.Values[e.Points[n-1].Value]
			dst = append(dst, RefMetadata{Ref: e.Ref, Type: v.Type, Unit: v.Unit, Help: v.Help})
		case !e.Ignored():
			dst = append(dst, RefMetadata{Ref: e.Ref, Type: uint8(UnknownMT)})
		}
	}
	return dst
}

// CompactNativeMetadata appends a compact native metadata record to b. Entries
// must be groups or overrides, groups must have points, and points must be
// chronological per entry and index values. Each value a point uses is defined
// at its first use; values no point uses are not encoded. Callers that order
// values by first use, as Commit and checkpoints do, avoid a renumbered copy.
func (*Encoder) CompactNativeMetadata(values []NativeMetadataValue, entries []RefCompactNativeMetadata, b []byte) []byte {
	if !compactNativeMetadataInFirstUseOrder(entries) {
		values, entries = compactNativeMetadataFirstUseOrder(values, entries)
	}
	var base int64
	havePoints := false
	for _, e := range entries {
		for _, p := range e.Points {
			if !havePoints || p.EffectiveFrom < base {
				base, havePoints = p.EffectiveFrom, true
			}
		}
	}
	buf := encoding.Encbuf{B: b}
	buf.PutByte(byte(NativeMetadataCompact))
	buf.PutByte(compactNativeMetadataInlineFormat)
	buf.PutVarint64(base)
	buf.PutUvarint(len(entries))
	var previous chunks.HeadSeriesRef
	var defined uint32
	for _, e := range entries {
		kind := compactNativeMetadataGroup
		if e.Kind == NativeMetadataOverride {
			kind = compactNativeMetadataOverride
		}
		if e.Truncated {
			kind |= compactNativeMetadataTruncatedFlag
		}
		buf.PutByte(kind)
		buf.PutVarint64(int64(e.Ref - previous))
		previous = e.Ref
		buf.PutUvarint(len(e.Points))
		for _, p := range e.Points {
			buf.PutUvarint64(uint64(p.EffectiveFrom) - uint64(base))
			if p.Value < defined {
				buf.PutUvarint32(p.Value + 1)
				continue
			}
			v := values[p.Value]
			buf.PutByte(0)
			buf.PutByte(v.Type)
			buf.PutUvarintStr(v.Unit)
			buf.PutUvarintStr(v.Help)
			defined++
		}
	}
	return buf.Get()
}

// compactNativeMetadataInFirstUseOrder reports whether entries' points use
// value indices in order of first use: each new index is the next one.
func compactNativeMetadataInFirstUseOrder(entries []RefCompactNativeMetadata) bool {
	var defined uint32
	for _, e := range entries {
		for _, p := range e.Points {
			switch {
			case p.Value == defined:
				defined++
			case p.Value > defined:
				return false
			}
		}
	}
	return true
}

// compactNativeMetadataFirstUseOrder returns copies of values and entries with
// the used values renumbered in order of first use.
func compactNativeMetadataFirstUseOrder(values []NativeMetadataValue, entries []RefCompactNativeMetadata) ([]NativeMetadataValue, []RefCompactNativeMetadata) {
	// One-based new indices by old index; zero before first use.
	renumber := make([]uint32, len(values))
	var used []NativeMetadataValue
	out := make([]RefCompactNativeMetadata, len(entries))
	for i, e := range entries {
		e.Points = slices.Clone(e.Points)
		for j, p := range e.Points {
			if renumber[p.Value] == 0 {
				used = append(used, values[p.Value])
				renumber[p.Value] = uint32(len(used))
			}
			e.Points[j].Value = renumber[p.Value] - 1
		}
		out[i] = e
	}
	return used, out
}

// CompactNativeMetadata decodes a compact native metadata record into r,
// replacing its contents: r.Values holds the record's values in order of
// definition, which points index. Each value's strings are copied once. Framing
// errors are returned, including unknown formats, empty groups, references to
// values not yet defined and trailing bytes. Well-framed entries of unknown
// kind or with unknown flags are decoded as unknown entries with their newest
// point, if they have any; the values they define stay defined.
func (*Decoder) CompactNativeMetadata(rec []byte, r *CompactNativeMetadata) error {
	return decodeCompactNativeMetadata(rec, r, false)
}

// CompactNativeMetadataBorrowed is CompactNativeMetadata without copying
// strings: non-empty units and helps alias rec, so they are valid only while
// rec is unchanged.
func (*Decoder) CompactNativeMetadataBorrowed(rec []byte, r *CompactNativeMetadata) error {
	return decodeCompactNativeMetadata(rec, r, true)
}

func decodeCompactNativeMetadata(rec []byte, r *CompactNativeMetadata, borrow bool) error {
	r.Reset()
	if err := decodeCompactNativeMetadataInto(rec, r, borrow); err != nil {
		r.Reset()
		return err
	}
	return nil
}

func decodeCompactNativeMetadataInto(rec []byte, r *CompactNativeMetadata, borrow bool) error {
	dec := encoding.Decbuf{B: rec}
	if Type(dec.Byte()) != NativeMetadataCompact {
		return errors.New("invalid record type")
	}
	if format := dec.Byte(); dec.Err() == nil && format != compactNativeMetadataInlineFormat {
		return fmt.Errorf("unknown compact native metadata format %d", format)
	}
	base := uint64(dec.Varint64())
	count, err := compactNativeMetadataCount(&dec, compactNativeMetadataEntryMinBytes, "entries")
	if err != nil {
		return err
	}
	var ref uint64
	for range count {
		kind := dec.Byte()
		ref += uint64(dec.Varint64())
		points, err := compactNativeMetadataCount(&dec, compactNativeMetadataPointMinBytes, "points")
		if err != nil {
			return err
		}
		e := RefCompactNativeMetadata{Ref: chunks.HeadSeriesRef(ref), Truncated: kind&compactNativeMetadataTruncatedFlag != 0}
		switch kind &^ compactNativeMetadataTruncatedFlag {
		case compactNativeMetadataGroup:
			if points == 0 {
				return errors.New("compact native metadata group without points")
			}
			e.Kind = NativeMetadataGroup
		case compactNativeMetadataOverride:
			e.Kind = NativeMetadataOverride
		default:
			e.Kind = NativeMetadataUnknown
		}
		start := len(r.points)
		for range points {
			p := CompactNativeMetadataPoint{EffectiveFrom: int64(base + dec.Uvarint64())}
			tag := dec.Uvarint64()
			if dec.Err() != nil {
				return dec.Err()
			}
			switch {
			case tag == 0:
				if uint64(len(r.Values)) == math.MaxUint32 {
					return errors.New("compact native metadata record defines too many values")
				}
				r.Values = append(r.Values, NativeMetadataValue{Type: dec.Byte(), Unit: recordString(dec.UvarintBytes(), borrow), Help: recordString(dec.UvarintBytes(), borrow)})
				if dec.Err() != nil {
					return dec.Err()
				}
				p.Value = uint32(len(r.Values) - 1)
			case tag > uint64(len(r.Values)):
				return fmt.Errorf("compact native metadata reference to value %d with %d values defined", tag-1, len(r.Values))
			default:
				p.Value = uint32(tag - 1)
			}
			r.points = append(r.points, p)
		}
		if e.Kind == NativeMetadataUnknown && len(r.points) > start {
			// Keep only the newest point.
			r.points[start] = r.points[len(r.points)-1]
			r.points = r.points[:start+1]
		}
		e.Points = r.points[start:len(r.points):len(r.points)]
		r.Entries = append(r.Entries, e)
	}
	if dec.Err() != nil {
		return dec.Err()
	}
	if dec.Len() > 0 {
		return fmt.Errorf("unexpected %d bytes left in compact native metadata record", dec.Len())
	}
	return nil
}

// compactNativeMetadataCount decodes a count of elements of at least minBytes
// each, checking it against the remaining bytes before any conversion to int.
func compactNativeMetadataCount(dec *encoding.Decbuf, minBytes int, what string) (int, error) {
	count := dec.Uvarint64()
	if dec.Err() != nil {
		return 0, dec.Err()
	}
	if count > uint64(dec.Len()/minBytes) || count > math.MaxUint32 {
		return 0, fmt.Errorf("compact native metadata record has %d %s in %d bytes", count, what, dec.Len())
	}
	return int(count), nil
}

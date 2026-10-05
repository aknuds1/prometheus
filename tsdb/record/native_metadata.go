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
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unsafe"

	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/encoding"
)

// Native metadata entries extend Metadata records with fields that older
// decoders skip. The TYPE byte, UNIT and HELP always carry an entry's newest
// point, so those decoders read it as legacy metadata.
const (
	// nativeFromMetaName holds the newest point's start: 8 bytes, big-endian.
	nativeFromMetaName = "f"
	// nativeGroupMetaName holds the earlier points: a uvarint count, then per
	// point an 8-byte big-endian start, a type byte, and uvarint-length unit
	// and help strings.
	nativeGroupMetaName = "g"
	// nativeFlagsMetaName holds one byte of NativeMetadataOverrideFlag and
	// NativeMetadataTruncatedFlag bits. Groups omit it.
	nativeFlagsMetaName = "k"

	// nativeMetadataPointMinBytes is the smallest encoding of an earlier point.
	nativeMetadataPointMinBytes = 8 + 1 + 1 + 1
)

// Native metadata entry flags.
const (
	NativeMetadataOverrideFlag  byte = 1 << 0
	NativeMetadataTruncatedFlag byte = 1 << 1
)

// NativeMetadataKind classifies a native metadata entry.
type NativeMetadataKind uint8

const (
	// NativeMetadataLegacy carries no native fields. Its one point has an
	// unknown start, math.MinInt64.
	NativeMetadataLegacy NativeMetadataKind = iota
	// NativeMetadataGroup is one series' points from one transaction.
	NativeMetadataGroup
	// NativeMetadataOverride replaces a series' history and truncation flag.
	// Without points, it replaces the history with no versions.
	NativeMetadataOverride
	// NativeMetadataUnknown is a well-framed entry of a kind this decoder does
	// not know. Its one point is the newest, with an unknown start if absent.
	NativeMetadataUnknown
)

// RefNativeMetadata is one native metadata WAL entry for a series.
type RefNativeMetadata struct {
	Ref       chunks.HeadSeriesRef
	Kind      NativeMetadataKind
	Truncated bool
	// Points are chronological for valid entries; the last is the newest.
	Points []RefNativeMetadataPoint
}

// RefNativeMetadataPoint is a metadata change point in milliseconds.
type RefNativeMetadataPoint struct {
	EffectiveFrom int64
	Type          uint8
	Unit, Help    string
}

// NativeMetadata appends a Metadata record of native entries to b. Entries
// must be legacy, groups or overrides; legacy entries encode their newest point.
func (*Encoder) NativeMetadata(entries []RefNativeMetadata, b []byte) []byte {
	buf := encoding.Encbuf{B: b}
	buf.PutByte(byte(Metadata))
	for _, e := range entries {
		var newest RefNativeMetadataPoint
		if n := len(e.Points); n > 0 {
			newest = e.Points[n-1]
		}
		native := e.Kind != NativeMetadataLegacy
		fields := 2
		if native && len(e.Points) > 0 {
			fields++
		}
		if native && len(e.Points) > 1 {
			fields++
		}
		if e.Kind == NativeMetadataOverride {
			fields++
		}
		buf.PutUvarint64(uint64(e.Ref))
		buf.PutByte(newest.Type)
		buf.PutUvarint(fields)
		buf.PutUvarintStr(unitMetaName)
		buf.PutUvarintStr(newest.Unit)
		buf.PutUvarintStr(helpMetaName)
		buf.PutUvarintStr(newest.Help)
		if !native {
			continue
		}
		if len(e.Points) > 0 {
			buf.PutUvarintStr(nativeFromMetaName)
			buf.PutUvarint(8)
			buf.PutBE64int64(newest.EffectiveFrom)
		}
		if earlier := e.Points[:max(0, len(e.Points)-1)]; len(earlier) > 0 {
			size := uvarintSize(uint64(len(earlier)))
			for _, p := range earlier {
				size += 8 + 1 + uvarintSize(uint64(len(p.Unit))) + len(p.Unit) + uvarintSize(uint64(len(p.Help))) + len(p.Help)
			}
			buf.PutUvarintStr(nativeGroupMetaName)
			buf.PutUvarint(size)
			buf.PutUvarint(len(earlier))
			for _, p := range earlier {
				buf.PutBE64int64(p.EffectiveFrom)
				buf.PutByte(p.Type)
				buf.PutUvarintStr(p.Unit)
				buf.PutUvarintStr(p.Help)
			}
		}
		if e.Kind == NativeMetadataOverride {
			flags := NativeMetadataOverrideFlag
			if e.Truncated {
				flags |= NativeMetadataTruncatedFlag
			}
			buf.PutUvarintStr(nativeFlagsMetaName)
			buf.PutUvarint(1)
			buf.PutByte(flags)
		}
	}
	return buf.Get()
}

func uvarintSize(x uint64) int {
	var b [binary.MaxVarintLen64]byte
	return binary.PutUvarint(b[:], x)
}

// NativeMetadata appends the entries in a Metadata record to entries, and their
// points to points. Each entry's Points alias the returned point slice; reuse
// both slices only after the entries are no longer needed. Framing errors are
// returned; well-framed entries of unknown kind are decoded as such.
func (*Decoder) NativeMetadata(rec []byte, entries []RefNativeMetadata, points []RefNativeMetadataPoint) ([]RefNativeMetadata, []RefNativeMetadataPoint, error) {
	return decodeNativeMetadata(rec, entries, points, false)
}

// NativeMetadataBorrowed is NativeMetadata without copying strings: non-empty
// units and helps alias rec, so they are valid only while rec is unchanged.
func (*Decoder) NativeMetadataBorrowed(rec []byte, entries []RefNativeMetadata, points []RefNativeMetadataPoint) ([]RefNativeMetadata, []RefNativeMetadataPoint, error) {
	return decodeNativeMetadata(rec, entries, points, true)
}

func decodeNativeMetadata(rec []byte, entries []RefNativeMetadata, points []RefNativeMetadataPoint, borrow bool) ([]RefNativeMetadata, []RefNativeMetadataPoint, error) {
	dec := encoding.Decbuf{B: rec}
	if Type(dec.Byte()) != Metadata {
		return nil, nil, errors.New("invalid record type")
	}
	for len(dec.B) > 0 && dec.Err() == nil {
		ref := dec.Uvarint64()
		newest := RefNativeMetadataPoint{EffectiveFrom: math.MinInt64, Type: dec.Byte()}
		numFields := dec.Uvarint()
		var group []byte
		var flags byte
		var haveFrom, haveGroup, haveFlags bool
		for range numFields {
			name := dec.UvarintBytes()
			if dec.Err() != nil {
				break
			}
			switch string(name) {
			case unitMetaName:
				newest.Unit = recordString(dec.UvarintBytes(), borrow)
			case helpMetaName:
				newest.Help = recordString(dec.UvarintBytes(), borrow)
			case nativeFromMetaName:
				value := dec.UvarintBytes()
				if dec.Err() != nil {
					continue
				}
				if len(value) != 8 {
					return nil, nil, fmt.Errorf("native metadata start has %d bytes", len(value))
				}
				newest.EffectiveFrom, haveFrom = int64(binary.BigEndian.Uint64(value)), true
			case nativeGroupMetaName:
				group, haveGroup = dec.UvarintBytes(), true
			case nativeFlagsMetaName:
				value := dec.UvarintBytes()
				if dec.Err() != nil {
					continue
				}
				if len(value) != 1 {
					return nil, nil, fmt.Errorf("native metadata flags have %d bytes", len(value))
				}
				flags, haveFlags = value[0], true
			default:
				dec.UvarintBytes()
			}
		}
		if dec.Err() != nil {
			break
		}
		start := len(points)
		if haveGroup {
			g := encoding.Decbuf{B: group}
			count := g.Uvarint()
			if g.Err() == nil && (count == 0 || count > g.Len()/nativeMetadataPointMinBytes) {
				return nil, nil, fmt.Errorf("native metadata group of %d points in %d bytes", count, len(group))
			}
			for range count {
				points = append(points, RefNativeMetadataPoint{EffectiveFrom: g.Be64int64(), Type: g.Byte(), Unit: recordString(g.UvarintBytes(), borrow), Help: recordString(g.UvarintBytes(), borrow)})
			}
			if g.Err() != nil {
				return nil, nil, fmt.Errorf("native metadata group: %w", g.Err())
			}
			if g.Len() > 0 {
				return nil, nil, fmt.Errorf("unexpected %d bytes left in native metadata group", g.Len())
			}
		}
		e := RefNativeMetadata{Ref: chunks.HeadSeriesRef(ref), Truncated: flags&NativeMetadataTruncatedFlag != 0}
		switch {
		case !haveFrom && !haveGroup && !haveFlags:
			e.Kind = NativeMetadataLegacy
		case haveFrom && !haveFlags:
			e.Kind = NativeMetadataGroup
		case haveFlags && flags&^NativeMetadataTruncatedFlag == NativeMetadataOverrideFlag && (haveFrom || !haveGroup):
			e.Kind = NativeMetadataOverride
		default:
			e.Kind = NativeMetadataUnknown
			points = points[:start]
		}
		if e.Kind != NativeMetadataOverride || haveFrom {
			points = append(points, newest)
		}
		e.Points = points[start:len(points):len(points)]
		entries = append(entries, e)
	}
	if dec.Err() != nil {
		return nil, nil, dec.Err()
	}
	if len(dec.B) > 0 {
		return nil, nil, fmt.Errorf("unexpected %d bytes left in entry", len(dec.B))
	}
	return entries, points, nil
}

// recordString returns b as a string, aliasing b if borrow is set. Empty
// strings never alias, so they cannot keep a record alive.
func recordString(b []byte, borrow bool) string {
	switch {
	case len(b) == 0:
		return ""
	case borrow:
		return unsafe.String(&b[0], len(b))
	default:
		return string(b)
	}
}

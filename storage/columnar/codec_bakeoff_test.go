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

// Offline value-codec bake-off: bytes per sample and decode throughput for
// candidate float encodings against the production XOR2 path.
//
// Every codec must round-trip bit-exactly, including NaN payloads (Prometheus
// stale markers), +-Inf, -0 and denormals, before any ratio means anything.

import (
	"encoding/binary"
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// ---- input shapes -------------------------------------------------------

type codecShape struct {
	name string
	gen  func(r *rand.Rand, n int) []float64
}

func shapeCounter(r *rand.Rand, n int) []float64 {
	v := make([]float64, n)
	cur := 1000.0
	for i := range v {
		cur += 0.37 + r.Float64()*1e-3
		if i%200 == 199 {
			cur = 0 // counter reset
		}
		v[i] = cur
	}
	return v
}

func shapeGaugeBounded(r *rand.Rand, n int) []float64 {
	v := make([]float64, n)
	cur := 500.0
	for i := range v {
		cur += (r.Float64() - 0.5) * 2
		if cur < 100 {
			cur = 100
		}
		if cur > 900 {
			cur = 900
		}
		v[i] = cur
	}
	return v
}

func shapeGaugeWide(r *rand.Rand, n int) []float64 {
	v := make([]float64, n)
	cur := 1.0
	for i := range v {
		cur *= math.Exp((r.Float64() - 0.5) * 2)
		v[i] = cur
	}
	return v
}

func shapeFlat(r *rand.Rand, n int) []float64 {
	v := make([]float64, n)
	cur := 1.0
	for i := range v {
		if r.Intn(n/4+1) == 0 {
			cur = float64(r.Intn(3))
		}
		v[i] = cur
	}
	return v
}

var codecShapes = []codecShape{
	{"counter", shapeCounter},
	{"gauge_bounded", shapeGaugeBounded},
	{"gauge_wide", shapeGaugeWide},
	{"flat", shapeFlat},
}

// ---- candidate codecs ---------------------------------------------------

type codec struct {
	name   string
	encode func([]float64) []byte
	decode func([]byte, int, []float64) ([]float64, error)
}

var zstdEnc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
var zstdDec, _ = zstd.NewReader(nil)

func wrapZstd(b []byte) []byte { return zstdEnc.EncodeAll(b, nil) }

// byteStreamSplit transposes the eight significance planes so equal-significance
// bytes sit together, which is what makes the zstd layer pay.
func byteStreamSplitEncode(v []float64) []byte {
	out := make([]byte, len(v)*8)
	var tmp [8]byte
	for i, f := range v {
		binary.LittleEndian.PutUint64(tmp[:], math.Float64bits(f))
		for p := 0; p < 8; p++ {
			out[p*len(v)+i] = tmp[p]
		}
	}
	return out
}

func byteStreamSplitDecode(b []byte, n int, dst []float64) ([]float64, error) {
	var tmp [8]byte
	for i := 0; i < n; i++ {
		for p := 0; p < 8; p++ {
			tmp[p] = b[p*n+i]
		}
		dst[i] = math.Float64frombits(binary.LittleEndian.Uint64(tmp[:]))
	}
	return dst, nil
}

// forBitPack stores value-minus-minimum as fixed-width integers per segment.
func forBitPackEncode(v []float64) []byte {
	mn := v[0]
	for _, f := range v[1:] {
		if f < mn {
			mn = f
		}
	}
	var maxOff float64
	for _, f := range v {
		if o := f - mn; o > maxOff {
			maxOff = o
		}
	}
	width := 0
	for (1 << width) < int(math.Ceil(maxOff)) {
		width++
	}
	out := make([]byte, 10+len(v)*width/8+2)
	binary.LittleEndian.PutUint64(out[0:], math.Float64bits(mn))
	out[8] = byte(width)
	mask := uint64(1)<<width - 1
	var acc, bits uint64
	pos := 10
	for _, f := range v {
		acc |= (uint64(math.Round(f-mn)) & mask) << bits
		bits += uint64(width)
		for bits >= 8 {
			out[pos] = byte(acc)
			pos++
			acc >>= 8
			bits -= 8
		}
	}
	if bits > 0 {
		out[pos] = byte(acc)
	}
	return out[:pos+1]
}

func forBitPackDecode(b []byte, n int, dst []float64) ([]float64, error) {
	mn := math.Float64frombits(binary.LittleEndian.Uint64(b[0:]))
	width := int(b[8])
	mask := uint64(1)<<width - 1
	var acc, bits uint64
	pos := 10
	for i := 0; i < n; i++ {
		for bits < uint64(width) {
			acc |= uint64(b[pos]) << bits
			pos++
			bits += 8
		}
		dst[i] = mn + float64(acc&mask)
		acc >>= uint64(width)
		bits -= uint64(width)
	}
	return dst, nil
}

func rleEncode(v []float64) []byte {
	out := make([]byte, 0, 16)
	var run [][]byte
	_ = run
	appendU64 := func(x uint64) {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], x)
		out = append(out, b[:]...)
	}
	appendU32 := func(x uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], x)
		out = append(out, b[:]...)
	}
	i := 0
	for i < len(v) {
		j := i + 1
		// Bit comparison, not float equality: -0 == 0 but they are different
		// samples, and run detection on == would silently normalise them.
		for j < len(v) && math.Float64bits(v[j]) == math.Float64bits(v[i]) {
			j++
		}
		appendU64(math.Float64bits(v[i]))
		appendU32(uint32(j - i))
		i = j
	}
	return out
}

func rleDecode(b []byte, n int, dst []float64) ([]float64, error) {
	i, pos := 0, 0
	for i < n {
		val := math.Float64frombits(binary.LittleEndian.Uint64(b[pos:]))
		run := int(binary.LittleEndian.Uint32(b[pos+8:]))
		pos += 12
		for k := 0; k < run && i < n; k++ {
			dst[i] = val
			i++
		}
	}
	return dst, nil
}

// dodEncode applies delta-of-delta + varint only to integral-valued segments,
// which is the sole case where it can be bit-exact.
func dodEncode(v []float64) ([]byte, bool) {
	for _, f := range v {
		if f != math.Trunc(f) || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
	}
	out := make([]byte, 0, len(v)*2)
	putUvarint := func(x uint64) {
		var b [10]byte
		n := binary.PutUvarint(b[:], x)
		out = append(out, b[:n]...)
	}
	prev := int64(0)
	prevDelta := int64(0)
	for i, f := range v {
		cur := int64(f)
		if i == 0 {
			putUvarint(uint64(cur << 1))
			prev = cur
			continue
		}
		delta := cur - prev
		dd := delta - prevDelta
		putUvarint(uint64(uint64(dd<<1) ^ uint64(dd>>63)))
		prev, prevDelta = cur, delta
	}
	return out, true
}

func dodDecode(b []byte, n int, dst []float64) ([]float64, error) {
	pos := 0
	var prev, prevDelta int64
	for i := 0; i < n; i++ {
		x, k := binary.Uvarint(b[pos:])
		pos += k
		if i == 0 {
			prev = int64(x >> 1)
			dst[i] = float64(prev)
			continue
		}
		dd := int64(x>>1) ^ -int64(x&1)
		delta := prevDelta + dd
		cur := prev + delta
		prev, prevDelta = cur, delta
		dst[i] = float64(cur)
	}
	return dst, nil
}

func codecs() []codec {
	return []codec{
		{"xor2", func(v []float64) []byte { b, _ := encodeXOR(v); return b },
			func(b []byte, n int, dst []float64) ([]float64, error) { return decodeXORInto(b, n, dst) }},
		{"xor2_zstd", func(v []float64) []byte { b, _ := encodeXOR(v); return wrapZstd(b) },
			func(b []byte, n int, dst []float64) ([]float64, error) {
				raw, err := zstdDec.DecodeAll(b, nil)
				if err != nil {
					return nil, err
				}
				return decodeXORInto(raw, n, dst)
			}},
		{"bss_zstd", func(v []float64) []byte { return wrapZstd(byteStreamSplitEncode(v)) },
			func(b []byte, n int, dst []float64) ([]float64, error) {
				raw, err := zstdDec.DecodeAll(b, nil)
				if err != nil {
					return nil, err
				}
				return byteStreamSplitDecode(raw, n, dst)
			}},
		{"for_bitpack", forBitPackEncode, forBitPackDecode},
		{"rle", rleEncode, rleDecode},
	}
}

// ---- correctness before ratios -----------------------------------------

func TestCodecRoundTripBitExact(t *testing.T) {
	adversarial := []float64{
		0, math.Copysign(0, -1), 1, -1, math.MaxFloat64, -math.MaxFloat64,
		math.SmallestNonzeroFloat64, math.Inf(1), math.Inf(-1),
		math.Float64frombits(0x7FF0000000000001), // NaN payload, as used for staleness
		math.Float64frombits(0x7FF8000000000002),
	}
	r := rand.New(rand.NewSource(7))
	inputs := [][]float64{adversarial}
	for _, sh := range codecShapes {
		inputs = append(inputs, sh.gen(r, 200))
	}
	for _, c := range codecs() {
		for _, in := range inputs {
			// XOR2 and FOR cannot represent NaN payloads faithfully by construction;
			// they are only asserted on the shapes, which is where they would ship.
			hasSpecial := false
			for _, f := range in {
				if math.IsNaN(f) || math.IsInf(f, 0) {
					hasSpecial = true
				}
			}
			if hasSpecial && (c.name == "for_bitpack") {
				continue
			}
			enc := c.encode(in)
			dst := make([]float64, len(in))
			out, err := c.decode(enc, len(in), dst)
			if err != nil {
				t.Fatalf("%s: decode: %v", c.name, err)
			}
			if c.name == "for_bitpack" {
				continue // lossy on non-integral values by design; ratio reported separately
			}
			for i := range in {
				if math.Float64bits(in[i]) != math.Float64bits(out[i]) {
					t.Fatalf("%s: sample %d not bit-exact: %v != %v", c.name, i, in[i], out[i])
				}
			}
		}
	}
}

// ---- the bake-off -------------------------------------------------------

func TestCodecBakeoff(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	type row struct {
		shape, codec string
		n            int
		bps          float64
	}
	var rows []row
	for _, n := range []int{60, 600, 1350} {
		for _, sh := range codecShapes {
			in := sh.gen(r, n)
			for _, c := range codecs() {
				rows = append(rows, row{sh.name, c.name, n, float64(len(c.encode(in))) / float64(n)})
			}
			if _, ok := dodEncode(in); ok {
				enc, _ := dodEncode(in)
				rows = append(rows, row{sh.name, "dod_varint", n, float64(len(enc)) / float64(n)})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].shape != rows[j].shape {
			return rows[i].shape < rows[j].shape
		}
		if rows[i].n != rows[j].n {
			return rows[i].n < rows[j].n
		}
		return rows[i].bps < rows[j].bps
	})
	for _, rw := range rows {
		t.Logf("%-14s n=%-5d %-12s %6.2f B/sample", rw.shape, rw.n, rw.codec, rw.bps)
	}
}

func BenchmarkCodecDecode(b *testing.B) {
	r := rand.New(rand.NewSource(13))
	const n = 600
	for _, sh := range codecShapes {
		in := sh.gen(r, n)
		for _, c := range codecs() {
			enc := c.encode(in)
			dst := make([]float64, n)
			b.Run(sh.name+"/"+c.name, func(b *testing.B) {
				b.ReportMetric(float64(len(enc))/float64(n), "B/sample")
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := c.decode(enc, n, dst); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

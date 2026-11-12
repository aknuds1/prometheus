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

package columnar

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"

	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

type (
	integerCodec uint8
	floatCodec   uint8
)

const (
	integerConstantInterval integerCodec = iota
	integerDeltaDelta
	integerRLE
	integerPlain
)

const (
	valueALP floatCodec = iota
	valueXOR
	valueRLE
)

// kindUniformFloat is an optional fast path only for the kind column. Stale,
// histogram and mixed-kind segments retain the generic RLE/plain representation.
const (
	kindUniformFloat integerCodec = 4
	integerVarint    integerCodec = 5
)

func encodeTimestamps(values []int64) ([]byte, integerCodec, error) {
	if len(values) == 0 {
		return nil, integerPlain, nil
	}
	if constantInterval(values) {
		out := appendSigned(values[0])
		if len(values) > 1 {
			out = timestampVarints(out, values[1]-values[0])
		}
		return out, integerConstantInterval, nil
	}

	out := appendSigned(values[0])
	if len(values) == 1 {
		return out, integerDeltaDelta, nil
	}
	previousDelta := values[1] - values[0]
	out = timestampVarints(out, previousDelta)
	for i := 2; i < len(values); i++ {
		delta := values[i] - values[i-1]
		difference, overflowed := subtractInt64(delta, previousDelta)
		if overflowed {
			return encodeIntPlain(values), integerPlain, nil
		}
		out = timestampVarints(out, difference)
		previousDelta = delta
	}
	if len(out) > len(values)*8 {
		return encodeIntPlain(values), integerPlain, nil
	}
	return out, integerDeltaDelta, nil
}

func encodeIntegerColumn(values []int64) ([]byte, integerCodec, error) {
	if len(values) == 0 {
		return nil, integerPlain, nil
	}
	rle := encodeIntRLE(values)
	if len(rle) <= len(values)*8 {
		return rle, integerRLE, nil
	}
	return encodeIntPlain(values), integerPlain, nil
}

func encodeIntRLE(values []int64) []byte {
	var out []byte
	for start := 0; start < len(values); {
		end := start + 1
		for end < len(values) && values[end] == values[start] {
			end++
		}
		out = timestampVarints(out, values[start])
		out = binary.AppendUvarint(out, uint64(end-start))
		start = end
	}
	return out
}

func encodeIntPlain(values []int64) []byte {
	out := make([]byte, len(values)*8)
	for i, value := range values {
		binary.LittleEndian.PutUint64(out[i*8:], uint64(value))
	}
	return out
}

func constantInterval(values []int64) bool {
	if len(values) < 3 {
		return true
	}
	delta := values[1] - values[0]
	for i := 2; i < len(values); i++ {
		if values[i]-values[i-1] != delta {
			return false
		}
	}
	return true
}

func appendSigned(value int64) []byte { return binary.AppendVarint(nil, value) }

func appendSignedTo(out []byte, value int64) []byte { return binary.AppendVarint(out, value) }

// Kept here so timestamp construction reads as append(value), not hiding that
// binary.AppendVarint is its encoding basis.
func timestampVarints(out []byte, values ...int64) []byte {
	for _, value := range values {
		out = appendSignedTo(out, value)
	}
	return out
}

// subtractInt64 returns a-b and reports whether that subtraction overflowed int64.
// The second result is true when the signed result does not fit; callers must not
// read it as "the difference is usable".
//
// The wrapped difference is returned even on overflow because two's-complement
// wrapping is exact modulo 2^64, so a decoder that adds the difference back
// reconstructs the same int64 delta. Overflow is still a signal that the deltas are
// pathological and that a plain column is the cheaper and more honest choice.
func subtractInt64(a, b int64) (difference int64, overflowed bool) {
	difference = a - b
	return difference, (a^b)&(a^difference) < 0
}

func decodeIntegerColumn(data []byte, count int, codec integerCodec) ([]int64, error) {
	return decodeIntegerColumnInto(data, count, codec, nil)
}

func decodeIntegerColumnInto(data []byte, count int, codec integerCodec, destination []int64) ([]int64, error) {
	if count < 0 {
		return nil, errors.New("negative integer column count")
	}
	if count == 0 {
		return []int64{}, nil
	}
	values := reuseInt64s(destination, count)
	var (
		n   int
		err error
	)
	switch codec {
	case integerConstantInterval:
		values[0], n, err = readSigned(data)
		if err != nil {
			return nil, err
		}
		interval := int64(0)
		if count > 1 {
			var consumed int
			interval, consumed, err = readSigned(data[n:])
			if err != nil {
				return nil, err
			}
			n += consumed
		}
		if n != len(data) {
			return nil, errors.New("trailing bytes in constant-interval timestamp column")
		}
		for i := 1; i < count; i++ {
			values[i] = values[i-1] + interval
		}
	case integerDeltaDelta:
		values[0], n, err = readSigned(data)
		if err != nil {
			return nil, err
		}
		if count == 1 {
			if n != len(data) {
				return nil, errors.New("trailing bytes in single timestamp")
			}
			break
		}
		previousDelta, n2, err := readSigned(data[n:])
		if err != nil {
			return nil, err
		}
		n += n2
		values[1] = values[0] + previousDelta
		for i := 2; i < count; i++ {
			difference, consumed, err := readSigned(data[n:])
			if err != nil {
				return nil, err
			}
			n += consumed
			values[i] = values[i-1] + previousDelta + difference
			previousDelta = values[i] - values[i-1]
		}
		if n != len(data) {
			return nil, errors.New("trailing bytes in delta-of-delta timestamp column")
		}
	case integerRLE:
		n := 0
		for i := 0; i < count; {
			value, bytes, err := readSigned(data[n:])
			if err != nil {
				return nil, err
			}
			n += bytes
			run, bytes, err := readUnsigned(data[n:])
			if err == nil && bytes <= 0 {
				err = errors.New("invalid unsigned integer length")
			}
			if err != nil || run == 0 {
				return nil, errors.New("invalid integer run length")
			}
			n += bytes
			if run > uint64(count-i) {
				return nil, errors.New("integer RLE run exceeds remaining samples")
			}
			for range int(run) {
				if i >= count {
					return nil, errors.New("integer RLE exceeds sample count")
				}
				values[i] = value
				i++
			}
		}
		if n != len(data) {
			return nil, errors.New("trailing bytes in integer RLE column")
		}
	case integerPlain:
		if len(data) != count*8 {
			return nil, fmt.Errorf("plain integer payload has %d bytes, need %d", len(data), count*8)
		}
		for i := range count {
			values[i] = int64(binary.LittleEndian.Uint64(data[i*8:]))
		}
	case integerVarint:
		for i := range count {
			value, consumed, readErr := readSigned(data[n:])
			if readErr != nil {
				return nil, readErr
			}
			n += consumed
			values[i] = value
		}
		if n != len(data) {
			return nil, errors.New("trailing bytes in varint integer column")
		}
	default:
		return nil, fmt.Errorf("unknown integer codec %d", codec)
	}
	return values, nil
}

func encodeFloatColumn(values []float64) ([]byte, floatCodec, error) {
	if len(values) == 0 {
		return nil, valueXOR, nil
	}
	// The XOR2 chunk is at most built once: it is both the comparison baseline
	// and the fallback, so building it twice doubled encode work and allocation.
	var (
		xorBytes []byte
		xorErr   error
		xorBuilt bool
	)
	xorOnce := func() ([]byte, error) {
		if !xorBuilt {
			xorBytes, xorErr = encodeXOR(values)
			xorBuilt = true
		}
		return xorBytes, xorErr
	}
	probeCount := min(len(values), 64)
	for precision := 0; precision <= 8; precision++ {
		if _, ok := encodeALP(values[:probeCount], precision); !ok {
			continue
		}
		full, exact := encodeALP(values, precision)
		if !exact {
			continue
		}
		xb, err := xorOnce()
		if err != nil {
			return nil, 0, err
		}
		if len(full) <= len(xb) {
			return full, valueALP, nil
		}
		// Coarser scaling never shrinks the column further.
		break
	}
	xb, err := xorOnce()
	if err != nil {
		return nil, 0, err
	}
	return xb, valueXOR, nil
}

func encodeALP(values []float64, precision int) ([]byte, bool) {
	factor := math.Pow10(precision)
	if len(values) == 0 {
		return []byte{byte(precision), 0}, true
	}
	scaled := make([]int64, len(values))
	minValue, maxValue := int64(math.MaxInt64), int64(math.MinInt64)
	for i, value := range values {
		product := value * factor
		rounded := math.Round(product)
		if math.IsNaN(product) || math.IsInf(product, 0) || rounded < float64(math.MinInt64) || rounded >= float64(math.MaxInt64) {
			return nil, false
		}
		integer := int64(rounded)
		// Reject values whose scaled representation is not bit-exact, including
		// the sign distinction between -0 and +0.
		if math.Float64bits(float64(integer)/factor) != math.Float64bits(value) {
			return nil, false
		}
		scaled[i] = integer
		minValue = min(minValue, integer)
		maxValue = max(maxValue, integer)
	}

	rangeWidth := bits.Len64(uint64(maxValue) - uint64(minValue))
	out := []byte{byte(precision), byte(rangeWidth)}
	out = binary.LittleEndian.AppendUint64(out, uint64(minValue))
	writer := bitPacker{}
	for _, value := range scaled {
		writer.write(uint64(value)-uint64(minValue), rangeWidth)
	}
	out = append(out, writer.finish()...)
	return out, true
}

func decodeFloatColumn(data []byte, count int, codec floatCodec) ([]float64, error) {
	return decodeFloatColumnInto(data, count, codec, nil)
}

func encodeFloatRLEColumn(values []float64) ([]byte, floatCodec, error) {
	best, codec, err := encodeFloatColumn(values)
	if err != nil {
		return nil, 0, err
	}
	var rle []byte
	for start := 0; start < len(values); {
		end := start + 1
		bits := math.Float64bits(values[start])
		for end < len(values) && math.Float64bits(values[end]) == bits {
			end++
		}
		rle = binary.LittleEndian.AppendUint64(rle, bits)
		rle = binary.AppendUvarint(rle, uint64(end-start))
		start = end
	}
	if len(rle) <= len(best) {
		return rle, valueRLE, nil
	}
	return best, codec, nil
}

func decodeFloatColumnInto(data []byte, count int, codec floatCodec, destination []float64) ([]float64, error) {
	if count < 0 {
		return nil, errors.New("negative float column count")
	}
	if count == 0 {
		return []float64{}, nil
	}
	if codec == valueXOR {
		return decodeXORInto(data, count, destination)
	}
	if codec == valueRLE {
		values := reuseFloat64s(destination, count)
		position := 0
		for index := 0; index < count; {
			if position+8 > len(data) {
				return nil, errors.New("short float RLE value")
			}
			value := math.Float64frombits(binary.LittleEndian.Uint64(data[position:]))
			position += 8
			run, consumed, err := readUnsigned(data[position:])
			if err != nil || run == 0 || run > uint64(count-index) {
				return nil, errors.New("invalid float RLE run")
			}
			position += consumed
			for range int(run) {
				values[index] = value
				index++
			}
		}
		if position != len(data) {
			return nil, errors.New("trailing bytes in float RLE column")
		}
		return values, nil
	}
	if codec != valueALP {
		return nil, fmt.Errorf("unknown float codec %d", codec)
	}
	if len(data) < 10 {
		return nil, errors.New("short ALP header")
	}
	if count == 0 {
		return []float64{}, nil
	}
	precision, width := int(data[0]), int(data[1])
	if precision > 8 || width > 64 {
		return nil, errors.New("invalid ALP precision or width")
	}
	minimum := int64(binary.LittleEndian.Uint64(data[2:]))
	reader := bitUnpacker{data: data[10:]}
	factor := math.Pow10(precision)
	values := reuseFloat64s(destination, count)
	for i := range count {
		packed, err := reader.read(width)
		if err != nil {
			return nil, err
		}
		values[i] = float64(int64(uint64(minimum)+packed)) / factor
	}
	if reader.position != len(reader.data) {
		return nil, errors.New("trailing bytes in ALP column")
	}
	return values, nil
}

func encodeXOR(values []float64) ([]byte, error) {
	chunk := chunkenc.NewXOR2Chunk()
	appender, err := chunk.Appender()
	if err != nil {
		return nil, err
	}
	for i, value := range values {
		appender.Append(0, int64(i), value)
	}
	return slices.Clone(chunk.Bytes()), nil
}

func decodeXOR(data []byte, count int) ([]float64, error) {
	return decodeXORInto(data, count, nil)
}

func decodeXORInto(data []byte, count int, destination []float64) ([]float64, error) {
	chunk := chunkenc.NewXOR2Chunk()
	chunk.Reset(data)
	if chunk.NumSamples() != count {
		return nil, fmt.Errorf("XOR chunk has %d samples, need %d", chunk.NumSamples(), count)
	}
	iterator := chunk.Iterator(nil)
	values := destination[:0]
	if cap(values) < count {
		values = make([]float64, 0, count)
	}
	for {
		typeOfSample := iterator.Next()
		if typeOfSample == chunkenc.ValNone {
			break
		}
		if typeOfSample != chunkenc.ValFloat {
			return nil, fmt.Errorf("XOR chunk sample is not a float: %v", typeOfSample)
		}
		_, value := iterator.At()
		values = append(values, value)
	}
	if err := iterator.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

type bitPacker struct {
	data    []byte
	current uint64
	bits    uint
}

func (p *bitPacker) write(value uint64, width int) {
	for width > 0 {
		available := 64 - int(p.bits)
		n := min(width, available)
		p.current |= lowBits(value, n) << p.bits
		p.bits += uint(n)
		value >>= uint(n)
		width -= n
		if p.bits == 64 {
			completed := p.current
			p.current = 0
			p.bits = 0
			p.data = binary.LittleEndian.AppendUint64(p.data, completed)
		}
	}
}

func (p *bitPacker) finish() []byte {
	if p.bits > 0 {
		p.data = binary.LittleEndian.AppendUint64(p.data, p.current)
		p.current, p.bits = 0, 0
	}
	return p.data
}

type bitUnpacker struct {
	data     []byte
	current  uint64
	bits     uint
	position int
}

func (p *bitUnpacker) read(width int) (uint64, error) {
	if width <= int(p.bits) {
		out := lowBits(p.current, width)
		p.current >>= uint(width)
		p.bits -= uint(width)
		return out, nil
	}
	var out uint64
	read := 0
	for read < width {
		if p.bits == 0 {
			if p.position+8 > len(p.data) {
				return 0, errors.New("short packed integer data")
			}
			p.current = binary.LittleEndian.Uint64(p.data[p.position:])
			p.position += 8
			p.bits = 64
		}
		n := min(width-read, int(p.bits))
		out |= lowBits(p.current, n) << uint(read)
		p.current >>= uint(n)
		p.bits -= uint(n)
		read += n
	}
	return out, nil
}

func readUnsigned(data []byte) (uint64, int, error) {
	value, consumed := binary.Uvarint(data)
	switch {
	case consumed == 0:
		return 0, 0, errors.New("short unsigned integer")
	case consumed < 0:
		return 0, 0, errors.New("unsigned integer overflow")
	default:
		return value, consumed, nil
	}
}

func readSigned(data []byte) (int64, int, error) {
	value, consumed := binary.Varint(data)
	switch {
	case consumed == 0:
		return 0, 0, errors.New("short signed integer")
	case consumed < 0:
		return 0, 0, errors.New("signed integer overflow")
	default:
		return value, consumed, nil
	}
}

func reuseInt64s(destination []int64, count int) []int64 {
	if cap(destination) >= count {
		return destination[:count]
	}
	return make([]int64, count)
}

func reuseFloat64s(destination []float64, count int) []float64 {
	if cap(destination) >= count {
		return destination[:count]
	}
	return make([]float64, count)
}

func lowBits(value uint64, count int) uint64 {
	if count == 64 {
		return value
	}
	return value & (uint64(1)<<uint(count) - 1)
}

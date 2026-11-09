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
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSegmentIntegerCodecs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		values    []int64
		timestamp bool
		want      integerCodec
		anyCodec  bool
	}{
		{name: "empty", timestamp: true, want: integerPlain},
		{name: "one timestamp", values: []int64{-123456}, timestamp: true, want: integerConstantInterval},
		{name: "constant interval", values: []int64{1000, 2000, 3000, 4000}, timestamp: true, want: integerConstantInterval},
		{name: "irregular small deltas", values: []int64{1000, 1010, 1030, 1080, 1150}, timestamp: true, anyCodec: true},
		{name: "extreme timestamp deltas", values: []int64{math.MinInt64, 0, math.MaxInt64}, timestamp: true, anyCodec: true},
		{name: "integer runs", values: []int64{7, 7, 7, 9, 9, 0, 0}, want: integerRLE},
		{name: "large varints can use plain", values: []int64{math.MinInt64, math.MinInt64 + 1, math.MinInt64 + 2}, want: integerPlain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var encoded []byte
			var codec integerCodec
			var err error
			if tc.timestamp {
				encoded, codec, err = encodeTimestamps(tc.values)
			} else {
				encoded, codec, err = encodeIntegerColumn(tc.values)
			}
			require.NoError(t, err)
			if tc.anyCodec {
				require.Contains(t, []integerCodec{integerDeltaDelta, integerPlain, integerConstantInterval}, codec)
			} else {
				require.Equal(t, tc.want, codec)
			}
			decoded, err := decodeIntegerColumn(encoded, len(tc.values), codec)
			require.NoError(t, err)
			if len(tc.values) == 0 {
				require.Empty(t, decoded)
			} else {
				require.Equal(t, tc.values, decoded)
			}
		})
	}
}

func TestSegmentIntegerCodecRejectsTruncatedInput(t *testing.T) {
	for _, codec := range []integerCodec{integerConstantInterval, integerDeltaDelta, integerRLE, integerPlain} {
		_, err := decodeIntegerColumn(nil, 2, codec)
		require.Error(t, err)
	}
	_, err := decodeIntegerColumn(nil, 0, integerCodec(99))
	require.NoError(t, err, "an empty column needs no encoded payload")
}

func TestSegmentFloatCodecs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []float64
		want   floatCodec
	}{
		{name: "constant decimals", values: []float64{1.25, 1.25, 1.25, 1.25}, want: valueALP},
		{name: "scaled decimals", values: []float64{1, 1.37, 2.74, -12.34, 0, 1000.5}, want: valueALP},
		{name: "signed zero must not share signedness", values: []float64{math.Copysign(0, -1), 0}, want: valueXOR},
		{name: "NaN and infinity should use XOR", values: []float64{math.NaN(), math.Inf(1), 1}, want: valueXOR},
		{name: "next-after float", values: []float64{math.Nextafter(1, 2)}, want: valueXOR},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, codec, err := encodeFloatColumn(tc.values)
			require.NoError(t, err)
			require.Equal(t, tc.want, codec)
			decoded, err := decodeFloatColumn(encoded, len(tc.values), codec)
			require.NoError(t, err)
			require.Len(t, decoded, len(tc.values))
			for i := range tc.values {
				require.Equal(t, math.Float64bits(tc.values[i]), math.Float64bits(decoded[i]), "sample %d", i)
			}
		})
	}
}

func TestSegmentPackedIntegerRoundTripAcrossWords(t *testing.T) {
	for _, width := range []int{0, 1, 3, 8, 15, 31, 63, 64} {
		maximum := uint64(0)
		if width == 64 {
			maximum = math.MaxUint64
		} else if width > 0 {
			maximum = uint64(1)<<uint(width) - 1
		}
		inputs := []uint64{0, maximum, maximum / 2, maximum, 0}
		writer := bitPacker{}
		for _, value := range inputs {
			writer.write(value, width)
		}
		reader := bitUnpacker{data: writer.finish()}
		for _, expected := range inputs {
			actual, err := reader.read(width)
			require.NoError(t, err)
			require.Equal(t, expected, actual, "width %d", width)
		}
		require.Equal(t, len(reader.data), reader.position)
	}
}

func TestSegmentXORControlRoundTrip(t *testing.T) {
	values := []float64{1.2, 2.4, 3.6, 4.8, 7.2}
	encoded, err := encodeXOR(values)
	require.NoError(t, err)
	decoded, err := decodeXOR(encoded, len(values))
	require.NoError(t, err)
	require.Equal(t, values, decoded)
}

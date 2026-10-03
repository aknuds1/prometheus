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

package encoding

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecbufUvarintBytes(t *testing.T) {
	for _, length := range []uint64{4, math.MaxInt64 + 1, math.MaxUint64} {
		e := Encbuf{}
		e.PutUvarint64(length)
		e.PutString("abc")
		d := Decbuf{B: e.Get()}
		require.Empty(t, d.UvarintBytes())
		require.ErrorIs(t, d.Err(), ErrInvalidSize)
	}
	e := Encbuf{}
	e.PutUvarintStr("abc")
	d := Decbuf{B: e.Get()}
	require.Equal(t, "abc", d.UvarintStr())
	require.NoError(t, d.Err())
}

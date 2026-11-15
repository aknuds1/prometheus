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
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
)

// The P1 gate: read cost must scale with matched series, not with block size.
// Format 9 paid for every series in a Parquet row group regardless of the
// selector; format 10 reads one payload per matched series.
func TestSegmentPruningScalesWithMatchedSeries(t *testing.T) {
	const total = 500
	dir := t.TempDir()
	series := make([]Series, 0, total)
	for i := 0; i < total; i++ {
		series = append(series, NewFloatSeries(
			labels.FromStrings(labels.MetricName, "prune_metric", "shard", strconv.Itoa(i)),
			[]int64{1000, 2000, 3000},
			[]float64{float64(i), float64(i), float64(i)},
		))
	}
	meta, err := WriteBlock(dir, series, WriteOptions{})
	require.NoError(t, err)
	block, err := OpenBlock(joinBlockFile(dir, meta.ULID))
	require.NoError(t, err)
	block.reads = &readStats{}

	read := func(matchValue string) ReadStats {
		block.reads.reset()
		got, err := block.Select(t.Context(), 0, 4000, [][]*labels.Matcher{{
			labels.MustNewMatcher(labels.MatchEqual, "shard", matchValue),
		}}, 0)
		require.NoError(t, err)
		stats := ReadStats{
			SegmentsRead:     block.reads.segmentsRead.Load(),
			PayloadBytesRead: block.reads.payloadBytes.Load(),
		}
		if matchValue == "" {
			require.Empty(t, got, "a mismatched selector must not return series")
		} else {
			require.Len(t, got, 1)
		}
		return stats
	}

	absent := read("")
	require.Zero(t, absent.SegmentsRead, "an unmatched selector must read no payloads")
	require.Zero(t, absent.PayloadBytesRead)

	one := read("42")
	require.Equal(t, uint64(1), one.SegmentsRead, "one matched series reads one payload")
	require.Positive(t, one.PayloadBytesRead)

	// The gate itself: one matched series out of 500 costs a tiny fraction of a
	// full scan, which is exactly what a Parquet row group could not do.
	block.reads.reset()
	all, err := block.Series(t.Context())
	require.NoError(t, err)
	require.Len(t, all, total)
	fullScanBytes := block.reads.payloadBytes.Load()
	fullScanSegments := block.reads.segmentsRead.Load()
	require.Equal(t, uint64(total), fullScanSegments)

	t.Logf("one series: %d bytes / %d segment; full scan: %d bytes / %d segments",
		one.PayloadBytesRead, one.SegmentsRead, fullScanBytes, fullScanSegments)
	require.Less(t, one.PayloadBytesRead*50, fullScanBytes,
		"a single matched series must read far less than one fiftieth of the block")
}

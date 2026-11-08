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
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
)

func TestWriteAndReadBlock(t *testing.T) {
	dir := t.TempDir()
	series := []Series{
		{
			Labels:     labels.FromStrings("__name__", "b_metric", "instance", "localhost:9100"),
			Timestamps: []int64{1000, 2000, 3000},
			Values:     []float64{1.5, 2.5, 3.5},
		},
		{
			Labels:     labels.FromStrings("__name__", "a_metric", "cpu", "0"),
			Timestamps: []int64{1000, 2000},
			Values:     []float64{-1, 0},
		},
	}

	meta, err := WriteBlock(dir, series, WriteOptions{})
	require.NoError(t, err)
	require.Equal(t, FormatVersion, meta.FormatVersion)
	require.Equal(t, 2, meta.NumSeries)
	require.Equal(t, 5, meta.NumSamples)
	require.Equal(t, int64(1000), meta.MinTime)
	require.Equal(t, int64(3001), meta.MaxTime, "MaxTime is exclusive, as in TSDB blocks")
	require.NotEmpty(t, meta.ULID)

	block, err := OpenBlock(filepath.Join(dir, meta.ULID))
	require.NoError(t, err)
	require.Equal(t, meta, block.Meta())
	require.Equal(t, 1, block.NumRowGroups())

	got, err := block.Series(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 2)

	// Series come back sorted by label set, not in write order.
	require.Equal(t, "a_metric", got[0].Labels.Get(labels.MetricName))
	require.Equal(t, "b_metric", got[1].Labels.Get(labels.MetricName))
	require.Equal(t, series[1].Timestamps, got[0].Timestamps)
	require.Equal(t, series[1].Values, got[0].Values)
	require.Equal(t, series[0].Timestamps, got[1].Timestamps)
	require.Equal(t, series[0].Values, got[1].Values)
	require.True(t, labels.Equal(series[1].Labels, got[0].Labels), "labels must survive the round trip unchanged")
}

func TestWriteBlockSortsSeriesAndPreservesLabels(t *testing.T) {
	dir := t.TempDir()

	var series []Series
	for _, name := range []string{"zzz", "aaa", "mmm"} {
		series = append(series, Series{
			Labels: labels.FromStrings(
				labels.MetricName, name,
				"mountpoint", "/var/lib/ü-does-not-matter",
				"weird", "with space",
			),
			Timestamps: []int64{1},
			Values:     []float64{1},
		})
	}

	meta, err := WriteBlock(dir, series, WriteOptions{})
	require.NoError(t, err)
	block, err := OpenBlock(filepath.Join(dir, meta.ULID))
	require.NoError(t, err)
	got, err := block.Series(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 3)

	names := make([]string, 0, len(got))
	for _, s := range got {
		names = append(names, s.Labels.Get(labels.MetricName))
		require.Equal(t, "/var/lib/ü-does-not-matter", s.Labels.Get("mountpoint"))
		require.Equal(t, "with space", s.Labels.Get("weird"))
	}
	require.True(t, slices.IsSorted(names), "series must come back in label order, got %v", names)
}

func TestWriteBlockMultipleRowGroups(t *testing.T) {
	dir := t.TempDir()

	const seriesCount = 7
	var series []Series
	for i := range seriesCount {
		series = append(series, Series{
			Labels:     labels.FromStrings(labels.MetricName, "rowgroup_test", "idx", string(rune('a'+i))),
			Timestamps: []int64{int64(i), int64(i + 1)},
			Values:     []float64{float64(i), float64(i)},
		})
	}

	meta, err := WriteBlock(dir, series, WriteOptions{MaxSeriesPerRowGroup: 2})
	require.NoError(t, err)

	block, err := OpenBlock(filepath.Join(dir, meta.ULID))
	require.NoError(t, err)
	// 7 rows at 2 rows per group, rounded up, is the point: row groups are the
	// only scan granularity Parquet offers, so the count is a design parameter,
	// not an artefact.
	require.Greater(t, block.NumRowGroups(), 1, "expected more than one row group")

	got, err := block.Series(t.Context())
	require.NoError(t, err)
	require.Len(t, got, seriesCount, "splitting row groups must not change the result")
}

func TestWriteBlockRejectsMalformedSeries(t *testing.T) {
	dir := t.TempDir()

	for _, tc := range []struct {
		name   string
		series Series
		errMsg string
	}{
		{
			name: "length mismatch",
			series: Series{
				Labels:     labels.FromStrings(labels.MetricName, "m"),
				Timestamps: []int64{1, 2},
				Values:     []float64{1},
			},
			errMsg: "timestamps but",
		},
		{
			name: "duplicate timestamp",
			series: Series{
				Labels:     labels.FromStrings(labels.MetricName, "m"),
				Timestamps: []int64{1, 1},
				Values:     []float64{1, 2},
			},
			errMsg: "timestamps not strictly increasing",
		},
		{
			name: "unsorted timestamps",
			series: Series{
				Labels:     labels.FromStrings(labels.MetricName, "m"),
				Timestamps: []int64{2, 1},
				Values:     []float64{1, 2},
			},
			errMsg: "timestamps not strictly increasing",
		},
		{
			name: "no labels",
			series: Series{
				Timestamps: []int64{1},
				Values:     []float64{1},
			},
			errMsg: "without labels",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := WriteBlock(dir, []Series{tc.series}, WriteOptions{})
			require.ErrorContains(t, err, tc.errMsg)
		})
	}

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "a rejected block must not leave a directory behind")
}

func TestEmptyBlock(t *testing.T) {
	dir := t.TempDir()

	meta, err := WriteBlock(dir, nil, WriteOptions{})
	require.NoError(t, err)
	require.Zero(t, meta.NumSeries)
	require.Zero(t, meta.NumSamples)

	block, err := OpenBlock(filepath.Join(dir, meta.ULID))
	require.NoError(t, err)
	require.False(t, block.Overlaps(0, 1<<62), "an empty block must never be selected")

	got, err := block.Series(t.Context())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestListBlocks(t *testing.T) {
	dir := t.TempDir()

	_, err := ListBlocks(dir)
	require.ErrorIs(t, err, ErrNoBlocks)

	mk := func(i int) Meta {
		meta, err := WriteBlock(dir, []Series{{
			Labels:     labels.FromStrings(labels.MetricName, "list_test", "idx", string(rune('a'+i))),
			Timestamps: []int64{int64(i)},
			Values:     []float64{1},
		}}, WriteOptions{})
		require.NoError(t, err)
		return meta
	}
	first, second := mk(0), mk(1)

	// A leftover temporary directory from an interrupted write must be ignored.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "01ABCDEF"+tmpDirSuffix), 0o750))

	blocks, err := ListBlocks(dir)
	require.NoError(t, err)
	require.Len(t, blocks, 2)
	require.Equal(t, first.ULID, blocks[0].Meta().ULID)
	require.Equal(t, second.ULID, blocks[1].Meta().ULID)
}

func TestBlockOverlaps(t *testing.T) {
	dir := t.TempDir()
	meta, err := WriteBlock(dir, []Series{{
		Labels:     labels.FromStrings(labels.MetricName, "overlap_test"),
		Timestamps: []int64{100, 200},
		Values:     []float64{1, 2},
	}}, WriteOptions{})
	require.NoError(t, err)

	block, err := OpenBlock(filepath.Join(dir, meta.ULID))
	require.NoError(t, err)

	// Block range is [100, 201).
	require.True(t, block.Overlaps(0, 101))
	require.True(t, block.Overlaps(200, 400))
	require.False(t, block.Overlaps(201, 400))
	require.False(t, block.Overlaps(0, 100))
}

func TestOpenBlockRejectsWrongFormatVersion(t *testing.T) {
	dir := t.TempDir()
	meta, err := WriteBlock(dir, nil, WriteOptions{})
	require.NoError(t, err)

	path := filepath.Join(dir, meta.ULID, metaFileName)
	require.NoError(t, os.WriteFile(path, []byte(`{"ulid":"x","format_version":999}`), 0o600))

	_, err = OpenBlock(filepath.Join(dir, meta.ULID))
	require.ErrorContains(t, err, "format version")
}

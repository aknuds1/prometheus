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

// Direct storage-boundary tests for the native histogram validation that stock
// TSDB performs in its appender. The real-route reproducer lives beside the
// ordinary OTLP handler in storage/remote; these tests pin the exact model
// semantics, the transaction/publication behavior and the valid positive
// controls that the routes rely on.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
)

// storedHistogram returns the single native histogram sample of one exact series.
func storedHistogram(t *testing.T, q storage.Queryable, ls labels.Labels) (chunkenc.ValueType, *histogram.Histogram, *histogram.FloatHistogram) {
	t.Helper()
	querier, err := q.Querier(math.MinInt64, math.MaxInt64)
	require.NoError(t, err)
	defer func() { require.NoError(t, querier.Close()) }()
	set := querier.Select(context.Background(), true, nil,
		labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, ls.Get(labels.MetricName)))
	defer func() { require.NoError(t, set.Err()) }()
	for set.Next() {
		if !labels.Equal(set.At().Labels(), ls) {
			continue
		}
		iterator := set.At().Iterator(nil)
		for typ := iterator.Next(); typ != chunkenc.ValNone; typ = iterator.Next() {
			switch typ {
			case chunkenc.ValHistogram:
				_, h := iterator.AtHistogram(nil)
				require.NoError(t, iterator.Err())
				return typ, h, nil
			case chunkenc.ValFloatHistogram:
				_, fh := iterator.AtFloatHistogram(nil)
				require.NoError(t, iterator.Err())
				return typ, nil, fh
			default:
				t.Fatalf("series %s has non-histogram sample type %v", ls, typ)
			}
		}
		require.NoError(t, iterator.Err())
	}
	require.FailNow(t, "histogram series not found", "%s", ls)
	return chunkenc.ValNone, nil, nil
}

// requireNoSeries requires the exact series to be absent.
func requireNoSeries(t *testing.T, q storage.Queryable, ls labels.Labels) {
	t.Helper()
	querier, err := q.Querier(math.MinInt64, math.MaxInt64)
	require.NoError(t, err)
	defer func() { require.NoError(t, querier.Close()) }()
	set := querier.Select(context.Background(), true, nil,
		labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, ls.Get(labels.MetricName)))
	for set.Next() {
		if labels.Equal(set.At().Labels(), ls) {
			t.Fatalf("series %s must not be retained", ls)
		}
	}
	require.NoError(t, set.Err())
}

func TestAppendHistogramValidationMatchesStock(t *testing.T) {
	countBuckets := func(count uint64) *histogram.Histogram {
		return &histogram.Histogram{
			Count: count, Sum: 10.5, Schema: 0,
			PositiveSpans: []histogram.Span{{Offset: 1, Length: 2}}, PositiveBuckets: []int64{1, 3},
		}
	}
	validFloat := &histogram.FloatHistogram{
		Count: 1, Sum: 1, Schema: 0,
		PositiveSpans: []histogram.Span{{Offset: 0, Length: 1}}, PositiveBuckets: []float64{1},
	}

	cases := []struct {
		name    string
		h       *histogram.Histogram
		fh      *histogram.FloatHistogram
		wantErr error
	}{
		{
			name:    "count smaller than resolved bucket observations",
			h:       countBuckets(4),
			wantErr: histogram.ErrHistogramCountMismatch,
		},
		{
			name:    "count larger than resolved bucket observations",
			h:       countBuckets(6),
			wantErr: histogram.ErrHistogramCountMismatch,
		},
		{
			name: "valid integer histogram",
			h:    countBuckets(5),
		},
		{
			name: "valid float histogram",
			fh:   validFloat,
		},
		{
			// Histogram.FloatHistogram.Validate does not require Count to equal the
			// bucket sum, so stock TSDB accepts this. The columnar appender must not
			// invent a stricter float rule.
			name: "float count mismatch follows stock semantics",
			fh: &histogram.FloatHistogram{
				Count: 2, Sum: 1, Schema: 0,
				PositiveSpans: []histogram.Span{{Offset: 0, Length: 1}}, PositiveBuckets: []float64{1},
			},
		},
		{
			name:    "negative float bucket population",
			fh:      &histogram.FloatHistogram{Count: 1, Sum: 1, Schema: 0, PositiveSpans: []histogram.Span{{Offset: 0, Length: 1}}, PositiveBuckets: []float64{-1}},
			wantErr: histogram.ErrHistogramNegativeBucketCount,
		},
		{
			name: "empty histogram",
			h:    &histogram.Histogram{Count: 0, Sum: 0, Schema: 0},
		},
		{
			// The model accepts a zero count with a non-zero sum; stock TSDB stores
			// it and the OTLP translator only emits a warning.
			name: "zero count with non-zero sum",
			h:    &histogram.Histogram{Count: 0, Sum: 10.5, Schema: 0},
		},
	}

	for _, routeName := range []string{contractRouteColumnar, contractRouteTSDB} {
		for _, tc := range cases {
			t.Run(routeName+"/"+tc.name, func(t *testing.T) {
				route := contractNewRoute(t, routeName)
				ls := labels.FromStrings(labels.MetricName, "histogram_validation")
				app := route.appendable()
				_, err := app.Append(0, ls, 0, 1000, 0, tc.h, tc.fh, storage.AOptions{})
				if tc.wantErr != nil {
					require.ErrorIs(t, err, tc.wantErr)
					require.NoError(t, app.Rollback())
					requireNoSeries(t, route.queryable(), ls)
					return
				}
				require.NoError(t, err)
				require.NoError(t, app.Commit())
				_, h, fh := storedHistogram(t, route.queryable(), ls)
				if tc.h != nil {
					require.NotNil(t, h)
					require.True(t, h.Equals(tc.h), "stored histogram %+v differs from the appended histogram %+v", h, tc.h)
				}
				if tc.fh != nil {
					require.NotNil(t, fh)
					require.True(t, fh.Equals(tc.fh), "stored float histogram %+v differs from the appended float histogram %+v", fh, tc.fh)
				}
			})
		}
	}
}

// contractSeriesSamples returns the rendered samples of one exact series.
func contractSeriesSamples(t *testing.T, route *contractRoute, ls labels.Labels) []string {
	t.Helper()
	for _, series := range route.captureSeries(t) {
		if series.Labels == ls.String() {
			return series.Samples
		}
	}
	return nil
}

func TestAppendMixedValidAndInconsistentMatchesStock(t *testing.T) {
	inconsistent := &histogram.Histogram{
		Count: 4, Sum: 10.5, Schema: 0,
		PositiveSpans: []histogram.Span{{Offset: 1, Length: 2}}, PositiveBuckets: []int64{1, 3},
	}
	validLS := labels.FromStrings(labels.MetricName, "mixed_valid")
	invalidLS := labels.FromStrings(labels.MetricName, "mixed_invalid")
	validMetadata := metadata.Metadata{Type: model.MetricTypeGauge, Help: "mixed help", Unit: "mixed unit"}

	routes := []struct {
		name  string
		route *contractRoute
	}{
		{name: "columnar", route: contractNewRoute(t, contractRouteColumnar)},
		{name: "stock_tsdb", route: contractNewRoute(t, contractRouteTSDB)},
	}
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			app := route.route.appendable()
			_, err := app.Append(0, validLS, 0, 1000, 1.5, nil, nil, storage.AOptions{Metadata: validMetadata})
			require.NoError(t, err)
			_, err = app.Append(0, invalidLS, 0, 1000, 0, inconsistent, nil, storage.AOptions{})
			require.ErrorIs(t, err, histogram.ErrHistogramCountMismatch)
			require.NoError(t, app.Commit())

			// The valid sample and its metadata commit; the rejected one was never
			// staged, matching stock's per-append failure semantics.
			require.Equal(t, []string{"1000|0x3ff8000000000000|float|0"},
				contractSeriesSamples(t, route.route, validLS))
			requireNoSeries(t, route.route.queryable(), invalidLS)
			if route.name == "columnar" {
				history, ok := route.route.columnar.MetadataHistory(MetricOwner(validLS))
				require.True(t, ok)
				require.Len(t, history.Versions, 1)
				require.Equal(t, map[string]string{
					"type": "gauge", "help": "mixed help", "unit": "mixed unit",
				}, history.Versions[0].Content.Attributes)
			}
		})
	}
}

// walDigest hashes every retained WAL segment name and byte, so a rejected
// transaction cannot change WAL contents unnoticed.
func walDigest(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, walDirName))
	require.NoError(t, err)
	hash := sha256.New()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(dir, walDirName, entry.Name()))
		require.NoError(t, err)
		_, _ = hash.Write([]byte(entry.Name()))
		_, _ = hash.Write(contents)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func TestAppendRejectedHistogramPublishesNothing(t *testing.T) {
	route := contractNewRoute(t, contractRouteColumnar)
	seed := labels.FromStrings(labels.MetricName, "histogram_validation_seed")
	app := route.appendable()
	_, err := app.Append(0, seed, 0, 1000, 1, nil, nil, storage.AOptions{
		Metadata: metadata.Metadata{Type: model.MetricTypeGauge, Help: "seed help", Unit: "seed unit"},
	})
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	beforeSeries := route.captureSeries(t)
	beforeOwners := route.captureOwners(t)
	beforeStats := route.stats(t)
	beforeWAL := walDigest(t, route.dir)

	inconsistent := &histogram.Histogram{
		Count: 4, Sum: 10.5, Schema: 0,
		PositiveSpans: []histogram.Span{{Offset: 1, Length: 2}}, PositiveBuckets: []int64{1, 3},
	}
	app = route.appendable()
	_, err = app.Append(0, labels.FromStrings(labels.MetricName, "histogram_validation_bad"), 0, 2000, 0, inconsistent, nil, storage.AOptions{})
	require.ErrorIs(t, err, histogram.ErrHistogramCountMismatch)
	require.NoError(t, app.Commit())

	require.Equal(t, beforeSeries, route.captureSeries(t), "a rejected append must not change samples")
	require.Equal(t, beforeOwners, route.captureOwners(t), "a rejected append must not change metadata")
	require.Equal(t, beforeStats, route.stats(t), "a rejected append must not change WAL or committed counters")
	require.Equal(t, beforeWAL, walDigest(t, route.dir), "a rejected append must not change WAL contents")
}

func TestValidHistogramsSurviveFlushAndReopen(t *testing.T) {
	route := contractNewRoute(t, contractRouteColumnar)
	intLS := labels.FromStrings(labels.MetricName, "valid_int_histogram")
	floatLS := labels.FromStrings(labels.MetricName, "valid_float_histogram")
	intH := &histogram.Histogram{
		Count: 5, Sum: 10.5, Schema: 0,
		PositiveSpans: []histogram.Span{{Offset: 1, Length: 2}}, PositiveBuckets: []int64{1, 3},
	}
	floatFH := &histogram.FloatHistogram{
		Count: 1.5, Sum: 1, Schema: 0,
		PositiveSpans: []histogram.Span{{Offset: 0, Length: 1}}, PositiveBuckets: []float64{1.5},
	}
	app := route.appendable()
	_, err := app.Append(0, intLS, 0, 1000, 0, intH, nil, storage.AOptions{})
	require.NoError(t, err)
	_, err = app.Append(0, floatLS, 0, 1000, 0, nil, floatFH, storage.AOptions{})
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	requireExact := func(t *testing.T) {
		t.Helper()
		_, h, _ := storedHistogram(t, route.queryable(), intLS)
		require.NotNil(t, h)
		require.True(t, h.Equals(intH))
		_, _, fh := storedHistogram(t, route.queryable(), floatLS)
		require.NotNil(t, fh)
		require.True(t, fh.Equals(floatFH))
	}
	requireExact(t)
	require.NoError(t, route.columnar.Flush())
	requireExact(t)
	_, err = route.columnar.Compact(context.Background())
	require.NoError(t, err)
	requireExact(t)
	route.reopen(t)
	requireExact(t)
}

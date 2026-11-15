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

// This file is the executable evidence for the remote-write route obligation.
// It sends real RW1 and RW2 requests through the actual remote-write handler
// and asserts what each store does with them. It is test-only and changes no
// production path.
//
// The stock TSDB route uses the supported start-timestamp configuration, so the
// comparison is not silently ST-enabled candidate against ST-disabled
// incumbent. The columnar route is expected to fail closed today because its
// appender does not implement storage.ExemplarAppenderV2, which the handler
// requires before appending anything; that negative result is asserted exactly
// instead of being repaired here.

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	remoteapi "github.com/prometheus/client_golang/exp/api/remote"
	"github.com/prometheus/common/promslog"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/exemplar"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	writev2 "github.com/prometheus/prometheus/prompb/io/prometheus/write/v2"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/remote"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/util/compression"
	"github.com/prometheus/prometheus/util/teststorage"
)

// Remote-write response headers, as specified by PRW 2.0.
const (
	rwSamplesWrittenHeader    = "X-Prometheus-Remote-Write-Samples-Written"
	rwHistogramsWrittenHeader = "X-Prometheus-Remote-Write-Histograms-Written"
	rwExemplarsWrittenHeader  = "X-Prometheus-Remote-Write-Exemplars-Written"
)

// rwHistogram is the hand-authored native histogram every remote-write case
// uses: one observation in the (0, 1] bucket.
var rwHistogram = histogram.Histogram{
	Count: 1, Sum: 1, Schema: 0,
	PositiveSpans:   []histogram.Span{{Offset: 0, Length: 1}},
	PositiveBuckets: []int64{1},
}

// TestRemoteWriteRouteExecutesRealRequests is the remote-write route evidence.
func TestRemoteWriteRouteExecutesRealRequests(t *testing.T) {
	t.Run("stock_rw1", testRemoteWriteStockRW1)
	t.Run("stock_rw2", testRemoteWriteStockRW2)
	t.Run("columnar", testRemoteWriteColumnarRejects)
}

// testRemoteWriteStockRW1 sends one real RW1 request and asserts the complete
// stored result, including native histograms and exemplars, then the
// stock-compatible duplicate and out-of-order responses.
func testRemoteWriteStockRW1(t *testing.T) {
	store := newSTEnabledStock(t)
	handler := remote.NewWriteHandler(promslog.NewNopLogger(), nil, store,
		[]remoteapi.WriteMessageType{remoteapi.WriteV1MessageType, remoteapi.WriteV2MessageType}, false, true)

	req := &prompb.WriteRequest{
		Timeseries: []prompb.TimeSeries{
			{
				Labels: []prompb.Label{
					{Name: "__name__", Value: "contract_rw_float"},
					{Name: "zone", Value: "a"},
				},
				Samples: []prompb.Sample{
					{Value: 1.5, Timestamp: 2000},
					{Value: math.Float64frombits(0x8000000000000000), Timestamp: 3000},
				},
				Exemplars: []prompb.Exemplar{{
					Labels: []prompb.Label{{Name: "trace_id", Value: "rw1"}}, Value: 1.25, Timestamp: 2500,
				}},
			},
			{
				Labels: []prompb.Label{
					{Name: "__name__", Value: "contract_rw_hist"},
					{Name: "zone", Value: "a"},
				},
				Histograms: []prompb.Histogram{
					prompb.FromIntHistogram(4000, &rwHistogram),
					prompb.FromFloatHistogram(5000, rwHistogram.ToFloat(nil)),
				},
			},
		},
		// RW1 request-level metadata is part of the wire message. The receiver
		// has no code path that ingests it, so it must have no stored effect.
		Metadata: []prompb.MetricMetadata{{
			Type: prompb.MetricMetadata_COUNTER, MetricFamilyName: "contract_rw_float",
			Help: "ignored by the RW1 receiver", Unit: "widgets",
		}},
	}
	payload, err := proto.Marshal(req)
	require.NoError(t, err)
	recorder := rwPost(t, handler, remoteapi.WriteV1MessageType, "", snappy.Encode(nil, payload))
	require.Equal(t, http.StatusNoContent, recorder.Code, "body: %s", recorder.Body.String())

	floatSeries := labels.FromStrings("__name__", "contract_rw_float", "zone", "a")
	histSeries := labels.FromStrings("__name__", "contract_rw_hist", "zone", "a")
	// RW1's proto3 implicit presence drops a negative zero sample value: -0.0
	// compares equal to the zero value, so it is omitted on the wire and decodes
	// as +0.0 before the receiver sees it. That is a protocol representation
	// difference, not a store behaviour; the same -0.0 survives the scrape route.
	require.Equal(t, []rwFloat{
		{t: 2000, bits: "0x3ff8000000000000", st: 0},
		{t: 3000, bits: "0x0000000000000000", st: 0},
	}, rwFloats(t, store, floatSeries))
	require.Equal(t, []rwHistogramSample{
		{t: 4000, st: 0, h: &rwHistogram},
		{t: 5000, st: 0, fh: rwHistogram.ToFloat(nil)},
	}, rwHistograms(t, store, histSeries))

	// Exemplars are stored on the stock route.
	exemplarQuerier, err := store.ExemplarQuerier(t.Context())
	require.NoError(t, err)
	results, err := exemplarQuerier.Select(0, 10000,
		[]*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "contract_rw_float")})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, []exemplar.Exemplar{{
		Labels: labels.FromStrings("trace_id", "rw1"), Value: 1.25, Ts: 2500, HasTs: true,
	}}, results[0].Exemplars)

	// Repeating the newest sample is rejected as a duplicate, a bad request, and
	// changes nothing. (Repeating the whole request instead fails earlier as out
	// of order, because the first repeated sample is older than the stored tail.)
	duplicateRequest := &prompb.WriteRequest{Timeseries: []prompb.TimeSeries{{
		Labels:  []prompb.Label{{Name: "__name__", Value: "contract_rw_float"}, {Name: "zone", Value: "a"}},
		Samples: []prompb.Sample{{Value: 3.5, Timestamp: 3000}},
	}}}
	duplicatePayload, err := proto.Marshal(duplicateRequest)
	require.NoError(t, err)
	duplicate := rwPost(t, handler, remoteapi.WriteV1MessageType, "", snappy.Encode(nil, duplicatePayload))
	require.Equal(t, http.StatusBadRequest, duplicate.Code)
	require.Contains(t, duplicate.Body.String(), "duplicate sample for timestamp")
	require.Len(t, rwFloats(t, store, floatSeries), 2, "a rejected duplicate must not add a sample")

	// An out-of-order sample is rejected as a bad request and changes nothing.
	outOfOrder := &prompb.WriteRequest{Timeseries: []prompb.TimeSeries{{
		Labels:  []prompb.Label{{Name: "__name__", Value: "contract_rw_float"}, {Name: "zone", Value: "a"}},
		Samples: []prompb.Sample{{Value: 2, Timestamp: 1000}},
	}}}
	payload, err = proto.Marshal(outOfOrder)
	require.NoError(t, err)
	response := rwPost(t, handler, remoteapi.WriteV1MessageType, "", snappy.Encode(nil, payload))
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Contains(t, response.Body.String(), "out of order sample")
	require.Len(t, rwFloats(t, store, floatSeries), 2, "a rejected out-of-order sample must not be stored")
}

// testRemoteWriteStockRW2 sends one real RW2 request with symbols, per-series
// metadata and start timestamps, and asserts the complete stored result plus the
// partial-error responses.
func testRemoteWriteStockRW2(t *testing.T) {
	store := newSTEnabledStock(t)
	handler := remote.NewWriteHandler(promslog.NewNopLogger(), nil, store,
		[]remoteapi.WriteMessageType{remoteapi.WriteV1MessageType, remoteapi.WriteV2MessageType}, false, true)

	req := buildRW2Request()
	payload, err := proto.Marshal(req)
	require.NoError(t, err)
	recorder := rwPost(t, handler, remoteapi.WriteV2MessageType, remoteapi.WriteModeStateless, snappy.Encode(nil, payload))
	require.Equal(t, http.StatusNoContent, recorder.Code, "body: %s", recorder.Body.String())
	require.Equal(t, "1", recorder.Header().Get(rwSamplesWrittenHeader))
	require.Equal(t, "2", recorder.Header().Get(rwHistogramsWrittenHeader))
	require.Equal(t, "1", recorder.Header().Get(rwExemplarsWrittenHeader))

	series := labels.FromStrings("__name__", "contract_rw2_float", "zone", "a")
	require.Equal(t, []rwFloat{{t: 2000, bits: "0x4004000000000000", st: 1500}}, rwFloats(t, store, series))
	require.Equal(t, []rwHistogramSample{
		{t: 4000, st: 3900, h: &rwHistogram},
		{t: 5000, st: 4900, fh: rwHistogram.ToFloat(nil)},
	}, rwHistograms(t, store, series))

	exemplarQuerier, err := store.ExemplarQuerier(t.Context())
	require.NoError(t, err)
	results, err := exemplarQuerier.Select(0, 10000,
		[]*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "contract_rw2_float")})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, []exemplar.Exemplar{{
		Labels: labels.FromStrings("trace_id", "rw2"), Value: 2.5, Ts: 2500, HasTs: true,
	}}, results[0].Exemplars)

	// A series with an invalid label set is a stock-compatible partial error: the
	// request is a bad request and nothing from it is stored.
	invalidTable := writev2.NewSymbolTable()
	invalidRefs := invalidTable.SymbolizeLabels(labels.FromStrings("test_metric1", "test_metric1"), nil)
	invalid := &writev2.Request{
		Symbols: invalidTable.Symbols(),
		Timeseries: []writev2.TimeSeries{{
			LabelsRefs: invalidRefs,
			Samples:    []writev2.Sample{{Value: 1, Timestamp: 6000}},
		}},
	}
	payload, err = proto.Marshal(invalid)
	require.NoError(t, err)
	response := rwPost(t, handler, remoteapi.WriteV2MessageType, remoteapi.WriteModeStateless, snappy.Encode(nil, payload))
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Contains(t, response.Body.String(), `invalid metric name or labels, got {test_metric1="test_metric1"}`)
	require.Equal(t, "0", response.Header().Get(rwSamplesWrittenHeader))

	// An out-of-order sample is a partial error and is not stored.
	outOfOrder := buildRW2Request()
	outOfOrder.Timeseries[0].Samples = []writev2.Sample{{Value: 9, Timestamp: 1000, StartTimestamp: 900}}
	payload, err = proto.Marshal(outOfOrder)
	require.NoError(t, err)
	response = rwPost(t, handler, remoteapi.WriteV2MessageType, remoteapi.WriteModeStateless, snappy.Encode(nil, payload))
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Contains(t, response.Body.String(), "out of order sample")
	require.Equal(t, "0", response.Header().Get(rwSamplesWrittenHeader))
	require.Equal(t, []rwFloat{{t: 2000, bits: "0x4004000000000000", st: 1500}}, rwFloats(t, store, series))

	// A metadata reference outside the symbol table is rejected while parsing the
	// request, which proves the receiver consumed the per-series metadata fields.
	badMetadata := buildRW2Request()
	badMetadata.Timeseries[0].Metadata.UnitRef = 999
	payload, err = proto.Marshal(badMetadata)
	require.NoError(t, err)
	response = rwPost(t, handler, remoteapi.WriteV2MessageType, remoteapi.WriteModeStateless, snappy.Encode(nil, payload))
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Contains(t, response.Body.String(), "metadata unit_ref 999 outside of symbols table")
}

// testRemoteWriteColumnarRejects records the exact negative result of real RW1
// and RW2 requests against the columnar store.
func testRemoteWriteColumnarRejects(t *testing.T) {
	store, err := NewStorage(t.TempDir(), StorageOptions{FlushSamples: 1_000})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	// The handler requires this optional extension before appending anything.
	_, exemplarAppender := any(store.AppenderV2(t.Context())).(storage.ExemplarAppenderV2)
	require.False(t, exemplarAppender,
		"the columnar appender does not implement storage.ExemplarAppenderV2; if that changes, this evidence must be revisited")

	handler := remote.NewWriteHandler(promslog.NewNopLogger(), nil, store,
		[]remoteapi.WriteMessageType{remoteapi.WriteV1MessageType, remoteapi.WriteV2MessageType}, false, true)

	rw1 := &prompb.WriteRequest{Timeseries: []prompb.TimeSeries{{
		Labels:  []prompb.Label{{Name: "__name__", Value: "contract_rw_columnar"}},
		Samples: []prompb.Sample{{Value: 1, Timestamp: 2000}},
	}}}
	payload, err := proto.Marshal(rw1)
	require.NoError(t, err)
	response := rwPost(t, handler, remoteapi.WriteV1MessageType, "", snappy.Encode(nil, payload))
	require.Equal(t, http.StatusInternalServerError, response.Code)
	require.Contains(t, response.Body.String(),
		"remote write 1.0 receiver requires a storage.ExemplarAppenderV2, got *columnar.appender")

	rw2 := buildRW2Request()
	payload, err = proto.Marshal(rw2)
	require.NoError(t, err)
	response = rwPost(t, handler, remoteapi.WriteV2MessageType, remoteapi.WriteModeStateless, snappy.Encode(nil, payload))
	require.Equal(t, http.StatusInternalServerError, response.Code)
	require.Contains(t, response.Body.String(), "remote write 2.x receiver requires a storage.ExemplarAppenderV2")

	// Nothing is stored by either rejected request.
	querier, err := store.Querier(math.MinInt64, math.MaxInt64)
	require.NoError(t, err)
	defer func() { require.NoError(t, querier.Close()) }()
	set := querier.Select(t.Context(), true, nil,
		labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+"))
	require.False(t, set.Next(), "a rejected remote-write request must publish no samples")
	require.NoError(t, set.Err())
}

// buildRW2Request is the hand-authored RW2 request: one series with a sample and
// two native histograms, an exemplar, metric metadata, and symbol references.
func buildRW2Request() *writev2.Request {
	symbols := writev2.NewSymbolTable()
	labelRefs := symbols.SymbolizeLabels(labels.FromStrings("__name__", "contract_rw2_float", "zone", "a"), nil)
	exemplarRefs := symbols.SymbolizeLabels(labels.FromStrings("trace_id", "rw2"), nil)
	helpRef := symbols.Symbolize("RW2 help text.")
	unitRef := symbols.Symbolize("widgets")
	return &writev2.Request{
		Symbols: symbols.Symbols(),
		Timeseries: []writev2.TimeSeries{{
			LabelsRefs: labelRefs,
			Metadata: writev2.Metadata{
				Type: writev2.Metadata_METRIC_TYPE_GAUGE, HelpRef: helpRef, UnitRef: unitRef,
			},
			Samples:   []writev2.Sample{{Value: 2.5, Timestamp: 2000, StartTimestamp: 1500}},
			Exemplars: []writev2.Exemplar{{LabelsRefs: exemplarRefs, Value: 2.5, Timestamp: 2500}},
			Histograms: []writev2.Histogram{
				writev2.FromIntHistogram(3900, 4000, &rwHistogram),
				writev2.FromFloatHistogram(4900, 5000, rwHistogram.ToFloat(nil)),
			},
		}},
	}
}

// newSTEnabledStock opens stock TSDB in the supported start-timestamp
// configuration, which the oracle's tsdb_st route uses as well.
func newSTEnabledStock(t *testing.T) *teststorage.TestStorage {
	t.Helper()
	return teststorage.New(t, func(opts *tsdb.Options) {
		opts.EnableSTStorage = true
		opts.FloatChunkEncoding = chunkenc.EncXOR2
		opts.EnableHistogramSTEncoding = true
	})
}

// rwPost sends one real remote-write request through the handler.
func rwPost(t *testing.T, handler http.Handler, messageType remoteapi.WriteMessageType, mode remoteapi.WriteMode, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/write", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentTypeHeader(messageType, mode))
	request.Header.Set("Content-Encoding", compression.Snappy)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// contentTypeHeader renders the content type for one protocol and mode.
func contentTypeHeader(messageType remoteapi.WriteMessageType, mode remoteapi.WriteMode) string {
	header := remoteapi.ContentTypeHeader(messageType, mode)
	return header
}

// rwFloat is one rendered float sample.
type rwFloat struct {
	t    int64
	bits string
	st   int64
}

// rwHistogramSample is one rendered native histogram sample.
type rwHistogramSample struct {
	t  int64
	st int64
	h  *histogram.Histogram
	fh *histogram.FloatHistogram
}

// rwFloats renders every float sample of one exact series.
func rwFloats(t *testing.T, q storage.Queryable, ls labels.Labels) []rwFloat {
	t.Helper()
	out := []rwFloat{}
	forEachSample(t, q, ls, func(typ chunkenc.ValueType, ts, st int64, f float64, _ *histogram.Histogram, _ *histogram.FloatHistogram) {
		if typ != chunkenc.ValFloat {
			return
		}
		out = append(out, rwFloat{t: ts, bits: fmt.Sprintf("%#016x", math.Float64bits(f)), st: st})
	})
	return out
}

// rwHistograms renders every native histogram sample of one exact series.
func rwHistograms(t *testing.T, q storage.Queryable, ls labels.Labels) []rwHistogramSample {
	t.Helper()
	out := []rwHistogramSample{}
	forEachSample(t, q, ls, func(typ chunkenc.ValueType, ts, st int64, _ float64, h *histogram.Histogram, fh *histogram.FloatHistogram) {
		if typ == chunkenc.ValFloat {
			return
		}
		out = append(out, rwHistogramSample{t: ts, st: st, h: h, fh: fh})
	})
	return out
}

// forEachSample visits every sample of one exact series through the exported
// query surface.
func forEachSample(t *testing.T, q storage.Queryable, ls labels.Labels, visit func(chunkenc.ValueType, int64, int64, float64, *histogram.Histogram, *histogram.FloatHistogram)) {
	t.Helper()
	querier, err := q.Querier(math.MinInt64, math.MaxInt64)
	require.NoError(t, err)
	defer func() { require.NoError(t, querier.Close()) }()
	set := querier.Select(context.Background(), true, nil,
		labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, ls.Get(labels.MetricName)))
	for set.Next() {
		if !labels.Equal(set.At().Labels(), ls) {
			continue
		}
		iterator := set.At().Iterator(nil)
		for typ := iterator.Next(); typ != chunkenc.ValNone; typ = iterator.Next() {
			switch typ {
			case chunkenc.ValFloat:
				_, f := iterator.At()
				visit(typ, iterator.AtT(), iterator.AtST(), f, nil, nil)
			case chunkenc.ValHistogram:
				_, h := iterator.AtHistogram(nil)
				visit(typ, iterator.AtT(), iterator.AtST(), 0, h, nil)
			case chunkenc.ValFloatHistogram:
				_, fh := iterator.AtFloatHistogram(nil)
				visit(typ, iterator.AtT(), iterator.AtST(), 0, nil, fh)
			default:
				t.Fatalf("unexpected value type %v", typ)
			}
		}
		require.NoError(t, iterator.Err())
	}
	require.NoError(t, set.Err())
}

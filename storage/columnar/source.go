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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// The Arrow endpoint is the SeriesSource-shaped boundary from the community
// discussion: the caller supplies label matchers and a time range, the store
// decides what to read, and the answer is Arrow rather than a PromQL
// implementation. The wire format is the block schema (see BlockSchema):
//
//   - one row per series, holding the label set and its samples as lists,
//   - timestamps in milliseconds, ascending within a row,
//   - a kind column that says whether a sample is a float (0), an integer
//     histogram (1), a float histogram (2), or a staleness marker (3),
//   - histogram payloads as the remote write histogram protobuf, so that a
//     consumer does not need a bespoke decoder for this prototype.
//
// Rows are ordered by label set, and a series appears at most once in the
// stream: samples that live in different blocks are merged before encoding.
// That costs the store a merge it could defer to the consumer, and it keeps the
// contract "one row per series" true, which is what makes the output usable by
// a query engine without a grouping step first.
//
// Two things a consumer must know. Staleness is carried by the kind column and
// not by the value, because Arrow and Parquet canonicalise NaN payloads and a
// Prometheus stale marker is a signalling NaN. And the values column is null for
// histogram samples, so a reader must look at the kind before reading a value.

// DefaultBatchSeries is the default number of series per Arrow record batch.
const DefaultBatchSeries = 1024

// ArrowOptions configures the Arrow endpoint.
type ArrowOptions struct {
	// BatchSeries is the maximum number of series per record batch. Zero selects
	// DefaultBatchSeries.
	BatchSeries int
	// MaxSeries is the maximum number of series a single request may return.
	// Zero selects DefaultMaxSeries.
	MaxSeries int
}

// DefaultMaxSeries is the default per-request series limit.
const DefaultMaxSeries = 100_000

// SelectSeries returns the raw columnar series matching matcherSets within
// [mint, maxt], with samples from different blocks merged into one row per
// series.
//
// matcherSets are unioned: a series is selected if it matches any set, as in the
// Prometheus API. A series that has no samples in the range is omitted. The
// result is sorted by label set and truncated to limit when limit is positive.
func (s *Storage) SelectSeries(ctx context.Context, mint, maxt int64, matcherSets [][]*labels.Matcher, limit int) ([]Series, error) {
	s.mtx.Lock()
	buffer := make([]Series, 0, len(s.buffer))
	for _, hs := range s.buffer {
		buffer = append(buffer, headToSeries(hs))
	}
	blocks := slices.Clone(s.blocks)
	closed := s.closed
	s.mtx.Unlock()

	if closed {
		return nil, errors.New("columnar: storage is closed")
	}

	all := buffer
	for _, block := range blocks {
		if !block.Overlaps(mint, maxt) {
			continue
		}
		// Select prunes row groups by labels, so a narrow selector does not pay for
		// the whole block.
		series, err := block.Select(ctx, mint, maxt, matcherSets, 0)
		if err != nil {
			return nil, fmt.Errorf("reading block %s: %w", block.Meta().ULID, err)
		}
		all = append(all, series...)
	}

	merged := mergeSeries(all)
	out := make([]Series, 0, len(merged))
	for _, series := range merged {
		if !matchesAny(series.Labels, matcherSets) {
			continue
		}
		trimmed := trimSeries(series, mint, maxt)
		if trimmed.Len() == 0 {
			continue
		}
		out = append(out, trimmed)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// matchesAny reports whether ls matches any of the matcher sets. An empty
// matcherSets matches everything.
func matchesAny(ls labels.Labels, matcherSets [][]*labels.Matcher) bool {
	if len(matcherSets) == 0 {
		return true
	}
	for _, matchers := range matcherSets {
		if matchesAll(ls, matchers) {
			return true
		}
	}
	return false
}

// mergeSeries combines series that share a label set into one series per label
// set, with samples ordered by timestamp and duplicate timestamps removed.
//
// Blocks can overlap in time, so the same series may be present in several of
// them. Keeping the first sample seen for a timestamp is a choice, not a
// correctness guarantee: this store has no notion of which block is newer.
func mergeSeries(series []Series) []Series {
	byLabels := map[uint64][]Series{}
	var keys []uint64
	for _, s := range series {
		hash := s.Labels.Hash()
		if _, ok := byLabels[hash]; !ok {
			keys = append(keys, hash)
		}
		byLabels[hash] = append(byLabels[hash], s)
	}

	out := make([]Series, 0, len(keys))
	for _, hash := range keys {
		group := byLabels[hash]
		if len(group) == 1 {
			out = append(out, group[0])
			continue
		}
		out = append(out, mergeOne(group))
	}
	slices.SortFunc(out, func(a, b Series) int { return labels.Compare(a.Labels, b.Labels) })
	return out
}

// mergeOne merges series that share a label set.
func mergeOne(group []Series) Series {
	type entry struct {
		t    int64
		st   int64
		kind SampleKind
		v    float64
		hist []byte
	}
	var entries []entry
	for _, s := range group {
		for i := range s.Len() {
			entries = append(entries, entry{t: s.Timestamps[i], st: s.StartTimestamps[i], kind: s.Kinds[i], v: s.Values[i], hist: s.Histograms[i]})
		}
	}
	slices.SortStableFunc(entries, func(a, b entry) int {
		switch {
		case a.t < b.t:
			return -1
		case a.t > b.t:
			return 1
		default:
			return 0
		}
	})

	merged := Series{
		Labels:          group[0].Labels,
		Timestamps:      make([]int64, 0, len(entries)),
		StartTimestamps: make([]int64, 0, len(entries)),
		Kinds:           make([]SampleKind, 0, len(entries)),
		Values:          make([]float64, 0, len(entries)),
		Histograms:      make([][]byte, 0, len(entries)),
	}
	for i, e := range entries {
		if i > 0 && e.t == entries[i-1].t {
			continue
		}
		merged.Timestamps = append(merged.Timestamps, e.t)
		merged.StartTimestamps = append(merged.StartTimestamps, e.st)
		merged.Kinds = append(merged.Kinds, e.kind)
		merged.Values = append(merged.Values, e.v)
		merged.Histograms = append(merged.Histograms, e.hist)
	}
	return merged
}

// ArrowHandler serves series as an Arrow IPC stream.
//
// Request parameters:
//
//	match[]  label selector, repeatable; sets are unioned. Required.
//	start    start of the time range, as unix seconds. Optional.
//	end      end of the time range, as unix seconds. Optional.
//	limit    maximum number of series. Optional.
//
// The response is an Arrow IPC stream of record batches in the format described
// above. Errors detected before the first batch produce an HTTP error; after
// that the stream is truncated, because the status code is already sent.
type ArrowHandler struct {
	storage *Storage
	logger  *slog.Logger
	opts    ArrowOptions
	parser  parser.Parser
}

// NewArrowHandler returns a handler serving the storage as Arrow.
func NewArrowHandler(storage *Storage, logger *slog.Logger, opts ArrowOptions) *ArrowHandler {
	if opts.BatchSeries <= 0 {
		opts.BatchSeries = DefaultBatchSeries
	}
	if opts.MaxSeries <= 0 {
		opts.MaxSeries = DefaultMaxSeries
	}
	return &ArrowHandler{
		storage: storage,
		logger:  logger,
		opts:    opts,
		parser:  parser.NewParser(parser.Options{}),
	}
}

func (h *ArrowHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	matcherSets, err := h.parseMatcherSets(query["match[]"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(matcherSets) == 0 {
		http.Error(w, "at least one match[] parameter is required", http.StatusBadRequest)
		return
	}

	mint, maxt, err := parseTimeRange(query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	limit := h.opts.MaxSeries
	if s := query.Get("limit"); s != "" {
		parsed, err := strconv.Atoi(s)
		if err != nil || parsed < 0 {
			http.Error(w, "limit must be a non-negative integer", http.StatusBadRequest)
			return
		}
		if parsed > 0 && parsed < limit {
			limit = parsed
		}
	}

	series, err := h.storage.SelectSeries(r.Context(), mint, maxt, matcherSets, limit)
	if err != nil {
		h.logger.Error("Failed to select series for Arrow export", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apache.arrow.stream")
	w.WriteHeader(http.StatusOK)

	writer := ipc.NewWriter(w, ipc.WithSchema(BlockSchema()))
	defer writer.Close()

	mem := memory.NewGoAllocator()
	for start := 0; start < len(series); start += h.opts.BatchSeries {
		end := min(start+h.opts.BatchSeries, len(series))
		record, err := buildRecord(mem, series[start:end])
		if err != nil {
			// The status code is already sent, so the only honest option is to stop
			// the stream and log it.
			h.logger.Error("Failed to build Arrow record batch", "err", err)
			return
		}
		err = writer.Write(record)
		record.Release()
		if err != nil {
			h.logger.Error("Failed to write Arrow record batch", "err", err)
			return
		}
	}
}

// parseMatcherSets parses every match[] parameter.
func (h *ArrowHandler) parseMatcherSets(values []string) ([][]*labels.Matcher, error) {
	sets := make([][]*labels.Matcher, 0, len(values))
	for _, value := range values {
		matchers, err := h.parser.ParseMetricSelector(value)
		if err != nil {
			return nil, fmt.Errorf("invalid match[] parameter %q: %w", value, err)
		}
		sets = append(sets, matchers)
	}
	return sets, nil
}

// parseTimeRange parses the start and end parameters as unix seconds.
//
// Missing bounds are open: the whole time range of the store.
func parseTimeRange(query url.Values) (mint, maxt int64, err error) {
	mint, maxt = math.MinInt64, math.MaxInt64

	if s := query.Get("start"); s != "" {
		seconds, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid start parameter %q: %w", s, err)
		}
		mint = int64(seconds * 1000)
	}
	if s := query.Get("end"); s != "" {
		seconds, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid end parameter %q: %w", s, err)
		}
		maxt = int64(seconds * 1000)
	}
	if maxt < mint {
		return 0, 0, errors.New("end must not be before start")
	}
	return mint, maxt, nil
}

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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

const (
	defaultMetadataPageSize = 100
	maxMetadataPageSize     = 10_000
	maxMetadataMetricNames  = 1_000
)

// MetadataHandler exposes the owner-scoped native metadata API for the
// experimental columnar server. Responses use the same status/data envelope as
// the prototype query endpoints, without claiming the normal Prometheus API.
type MetadataHandler struct {
	storage *Storage
	logger  *slog.Logger
}

func NewMetadataHandler(storage *Storage, logger *slog.Logger) *MetadataHandler {
	return &MetadataHandler{storage: storage, logger: logger}
}

func (h *MetadataHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.fail(w, http.StatusMethodNotAllowed, errors.New("method must be GET"))
		return
	}
	var data any
	var err error
	switch r.URL.Path {
	case "/resources":
		data, err = h.resources(r)
	case "/resources/series":
		data, err = h.series(r)
	case "/resources/diff":
		data, err = h.diff(r)
	default:
		h.fail(w, http.StatusNotFound, errors.New("unknown metadata endpoint"))
		return
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrMetadataDisabled) {
			status = http.StatusNotImplemented
		} else if errors.Is(err, ErrSeriesHashCollision) {
			status = http.StatusInternalServerError
		}
		h.fail(w, status, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": data}); err != nil && h.logger != nil {
		h.logger.Error("Failed to encode native metadata response", "err", err)
	}
}

func (h *MetadataHandler) resources(r *http.Request) (any, error) {
	query, err := parseMetadataOwnerRequest(r)
	if err != nil {
		return nil, err
	}
	if r.URL.Query().Get("format") == "attributes" {
		data, err := h.storage.DiscoverMetadata(r.Context(), MetadataDiscoveryQuery{
			Kind: query.Kind, Start: query.Start, End: query.End,
			Projection: query.Projection, After: query.After, Limit: query.Limit,
		})
		return encodeMetadataNextToken(data), err
	}
	if r.URL.Query().Get("format") != "" {
		return nil, fmt.Errorf("invalid metadata format %q", r.URL.Query().Get("format"))
	}
	query.Latest = isTrue(r.URL.Query().Get("latest"))
	page, err := h.storage.QueryMetadata(r.Context(), query)
	return encodeMetadataNextToken(page), err
}

func (h *MetadataHandler) series(r *http.Request) (any, error) {
	query, err := parseMetadataSeriesRequest(r)
	if err != nil {
		return nil, err
	}
	view := r.URL.Query().Get("view")
	if view == "" {
		view = "full"
	}
	if view != "full" && r.URL.Query().Has("next_token") {
		return nil, errors.New("next_token is only supported for the full metadata-series view")
	}
	if view != "full" {
		if r.URL.Query().Has("limit") {
			return nil, errors.New("limit is only supported for the full metadata-series view")
		}
		query.Limit = 0
	}
	page, err := h.storage.QueryMetadataSeries(r.Context(), query)
	if err != nil {
		return nil, err
	}
	switch view {
	case "full":
		return encodeMetadataNextToken(page), nil
	case "names", "summary":
		return metadataSeriesAggregate(page.Series, view)
	default:
		return nil, fmt.Errorf("invalid metadata-series view %q", view)
	}
}

func metadataSeriesAggregate(results []MetadataSeriesResult, view string) (any, error) {
	names := map[string]struct{}{}
	for _, result := range results {
		names[result.Labels.Get(labels.MetricName)] = struct{}{}
	}
	ordered := slices.Sorted(maps.Keys(names))
	truncated := len(ordered) > maxMetadataMetricNames
	if truncated {
		ordered = ordered[:maxMetadataMetricNames]
	}
	if view == "names" {
		return map[string]any{"names": ordered, "truncated": truncated}, nil
	}
	return map[string]any{"matched_series": len(results), "metric_families": len(names), "names_truncated": truncated}, nil
}

func (h *MetadataHandler) diff(r *http.Request) (any, error) {
	kind, err := parseMetadataKind(r.URL.Query().Get("kind"))
	if err != nil {
		return nil, err
	}
	matches, err := parseMetadataMatchers(r.URL.Query()["match[]"])
	if err != nil {
		return nil, err
	}
	at, err := parseMetadataTime(r, "time", math.MaxInt64)
	if err != nil {
		return nil, err
	}
	projection, err := parseMetadataProjection(r)
	if err != nil {
		return nil, err
	}
	limit, cursor, err := parseMetadataPage(r)
	if err != nil {
		return nil, err
	}
	page, err := h.storage.DiffMetadata(r.Context(), MetadataDiffQuery{
		Kind: kind, Time: at, Projection: projection, Matchers: matches, After: cursor, Limit: limit,
	})
	return encodeMetadataNextToken(page), err
}

func parseMetadataOwnerRequest(r *http.Request) (MetadataQuery, error) {
	h := MetadataQuery{Start: 0, End: math.MaxInt64}
	kind, err := parseMetadataKind(r.URL.Query().Get("kind"))
	if err != nil {
		return h, err
	}
	h.Kind = kind
	if h.Start, err = parseMetadataTime(r, "start", 0); err != nil {
		return h, err
	}
	if h.End, err = parseMetadataTime(r, "end", math.MaxInt64); err != nil {
		return h, err
	}
	if h.Start > h.End {
		return h, errors.New("metadata start must not be after end")
	}
	if h.Projection, err = parseMetadataProjection(r); err != nil {
		return h, err
	}
	if h.Matchers, err = parseMetadataMatchers(r.URL.Query()["match[]"]); err != nil {
		return h, err
	}
	if h.Limit, h.After, err = parseMetadataPage(r); err != nil {
		return h, err
	}
	return h, nil
}

func parseMetadataSeriesRequest(r *http.Request) (MetadataSeriesQuery, error) {
	query := MetadataSeriesQuery{}
	kind, err := parseMetadataKind(r.URL.Query().Get("kind"))
	if err != nil {
		return query, err
	}
	query.Kind = kind
	if query.Start, err = parseMetadataTime(r, "start", 0); err != nil {
		return query, err
	}
	if query.End, err = parseMetadataTime(r, "end", math.MaxInt64); err != nil {
		return query, err
	}
	if query.Start > query.End {
		return query, errors.New("metadata start must not be after end")
	}
	if query.Projection, err = parseMetadataProjection(r); err != nil {
		return query, err
	}
	if query.Matchers, err = parseMetadataMatchers(r.URL.Query()["match[]"]); err != nil {
		return query, err
	}
	if query.Limit, query.After, err = parseMetadataPage(r); err != nil {
		return query, err
	}
	for _, raw := range r.URL.Query()["resource.attr"] {
		name, value, ok := strings.Cut(raw, ":")
		if !ok || name == "" {
			return query, fmt.Errorf("resource.attr %q must have the form name:value", raw)
		}
		query.Attributes = append(query.Attributes, MetadataAttributeCondition{Name: name, Value: value})
	}
	if len(query.Attributes) == 0 {
		return query, errors.New("at least one resource.attr=name:value filter is required")
	}
	return query, nil
}

func parseMetadataKind(raw string) (MetadataKind, error) {
	if raw == "" {
		raw = KindNames[MetadataResource]
	}
	for kind, name := range KindNames {
		if raw == name {
			return MetadataKind(kind), nil
		}
	}
	return MetadataResource, fmt.Errorf("unknown metadata kind %q", raw)
}

func parseMetadataTime(r *http.Request, name string, fallback int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, fmt.Errorf("%s must be a finite number of seconds", name)
	}
	milliseconds := seconds * 1000
	if milliseconds > float64(math.MaxInt64) || milliseconds < float64(math.MinInt64) {
		return 0, fmt.Errorf("%s is outside the millisecond timestamp range", name)
	}
	return int64(milliseconds), nil
}

func parseMetadataProjection(r *http.Request) ([]string, error) {
	var names []string
	for _, key := range []string{"attributes", "projection"} {
		for _, raw := range r.URL.Query()[key] {
			for name := range strings.SplitSeq(raw, ",") {
				name = strings.TrimSpace(name)
				if name == "*" || name == "context=*" {
					return nil, fmt.Errorf("unbounded metadata projection %q is not supported", name)
				}
				if name != "" {
					names = append(names, name)
				}
			}
		}
	}
	return names, nil
}

func parseMetadataMatchers(raw []string) ([][]*labels.Matcher, error) {
	var result [][]*labels.Matcher
	promParser := parser.NewParser(parser.Options{})
	for _, expression := range raw {
		matchers, err := promParser.ParseMetricSelector(expression)
		if err != nil {
			return nil, fmt.Errorf("invalid match[] selector %q: %w", expression, err)
		}
		result = append(result, matchers)
	}
	return result, nil
}

func parseMetadataPage(r *http.Request) (limit int, after string, err error) {
	limit = defaultMetadataPageSize
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 0 || limit > maxMetadataPageSize {
			return 0, "", fmt.Errorf("limit must be between 0 and %d", maxMetadataPageSize)
		}
	}
	if raw := r.URL.Query().Get("next_token"); raw != "" {
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(raw)
		if decodeErr != nil {
			return 0, "", fmt.Errorf("invalid next_token: %w", decodeErr)
		}
		after = string(decoded)
	}
	return limit, after, nil
}

// encodeMetadataNextToken makes cursors safe to round-trip through HTTP query
// parameters while keeping the storage-facing Before/After values semantic.
func encodeMetadataNextToken(value any) any {
	switch page := value.(type) {
	case MetadataOwnerPage:
		page.NextToken = encodeMetadataCursor(page.NextToken)
		return page
	case MetadataSeriesPage:
		page.NextToken = encodeMetadataCursor(page.NextToken)
		return page
	case MetadataDiffPage:
		page.NextToken = encodeMetadataCursor(page.NextToken)
		return page
	case MetadataDiscoveryPage:
		page.NextToken = encodeMetadataCursor(page.NextToken)
		return page
	default:
		return value
	}
}

func encodeMetadataCursor(cursor string) string {
	if cursor == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(cursor))
}

func isTrue(raw string) bool {
	return raw == "1" || strings.EqualFold(raw, "true")
}

func (*MetadataHandler) fail(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "error", "errorType": "bad_data", "error": err.Error(),
	})
}

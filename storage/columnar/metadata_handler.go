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

// MetadataHandler exposes the experimental decision-7 native metadata HTTP
// surface. Only the separate experimental columnar listener registers these
// routes; stock Prometheus API routes are unchanged.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
)

const maxMetadataMetricNames = 1_000

// MetadataHandler exposes the owner-scoped native metadata API for the
// experimental columnar server.
type MetadataHandler struct {
	storage *Storage
	logger  *slog.Logger
}

// NewMetadataHandler returns one experimental metadata handler.
func NewMetadataHandler(storage *Storage, logger *slog.Logger) *MetadataHandler {
	return &MetadataHandler{storage: storage, logger: logger}
}

// ServeHTTP routes the contractual metadata endpoints.
func (h *MetadataHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/resources":
		h.routeGET(w, r, func() (any, error) { return h.resources(r) })
	case r.URL.Path == "/resources/series":
		h.routeGET(w, r, func() (any, error) { return h.series(r) })
	case r.URL.Path == "/resources/diff":
		h.routeGET(w, r, func() (any, error) { return h.diff(r) })
	case strings.HasPrefix(r.URL.Path, "/resources/snapshots/"):
		h.cancelSnapshot(w, r)
	default:
		h.fail(w, metadataFailure(404, "metadata_parameter_invalid", "unknown metadata endpoint"))
	}
}

func (h *MetadataHandler) routeGET(w http.ResponseWriter, r *http.Request, handler func() (any, error)) {
	if r.Method != http.MethodGet {
		h.fail(w, metadataFailure(405, "metadata_parameter_invalid", "method must be GET"))
		return
	}
	if err := h.storage.requireMetadataEnabled(); err != nil {
		h.fail(w, err)
		return
	}
	data, err := handler()
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond(w, data)
}

func (h *MetadataHandler) resources(r *http.Request) (any, error) {
	format, present, err := singleMetadataParameter(r.URL.Query(), "format")
	if err != nil {
		return nil, err
	}
	if present && format != "" && format != "attributes" {
		return nil, metadataFailure(400, "metadata_parameter_invalid", fmt.Sprintf("invalid metadata format %q", format))
	}
	if format == "attributes" {
		return h.discoveryPage(r)
	}
	return h.ownerPage(r)
}

func (h *MetadataHandler) ownerPage(r *http.Request) (any, error) {
	query, err := parseMetadataHTTPQuery(r, metadataHTTPHistory)
	if err != nil {
		return nil, err
	}
	snapshot, rawAfter, err := h.pageSnapshot(r, query)
	if err != nil {
		return nil, err
	}
	after, err := metadataAfterPosition[metadataOwnerPosition](rawAfter)
	if err != nil {
		return nil, err
	}
	if after != nil && !after.valid() {
		return nil, metadataFailure(400, "metadata_cursor_corrupt", "next_token has an invalid owner position")
	}
	rows, next, annotations, err := snapshot.ownerPage(r.Context(), query, after)
	if err != nil {
		return nil, err
	}
	token, err := h.nextToken(snapshot, query, next)
	if err != nil {
		return nil, err
	}
	return metadataHTTPOwnerPage{
		SnapshotID: snapshot.id, NextToken: token, Annotations: requireNonNilAnnotations(annotations), Owners: rows,
	}, nil
}

func (h *MetadataHandler) discoveryPage(r *http.Request) (any, error) {
	query, err := parseMetadataHTTPQuery(r, metadataHTTPDiscovery)
	if err != nil {
		return nil, err
	}
	snapshot, rawAfter, err := h.pageSnapshot(r, query)
	if err != nil {
		return nil, err
	}
	after, err := metadataAfterPosition[metadataDiscoveryPosition](rawAfter)
	if err != nil {
		return nil, err
	}
	if after != nil && !after.valid() {
		return nil, metadataFailure(400, "metadata_cursor_corrupt", "next_token has an invalid discovery position")
	}
	rows, next, annotations, err := snapshot.discoveryPage(r.Context(), query, after)
	if err != nil {
		return nil, err
	}
	token, err := h.nextToken(snapshot, query, next)
	if err != nil {
		return nil, err
	}
	return metadataHTTPDiscoveryPage{
		SnapshotID: snapshot.id, NextToken: token, Annotations: requireNonNilAnnotations(annotations), Attributes: rows,
	}, nil
}

func (h *MetadataHandler) series(r *http.Request) (any, error) {
	query, err := parseMetadataHTTPQuery(r, metadataHTTPSeriesFull)
	if err != nil {
		return nil, err
	}
	switch query.view {
	case "full":
		query.operation = metadataHTTPSeriesFull
		return h.fullSeriesPage(r, query)
	case "combined":
		query.operation = metadataHTTPSeriesCombined
		return h.combinedSeriesPage(r, query)
	case "names", "summary":
		return h.derivedSeriesView(r, query)
	default:
		return nil, metadataFailure(400, "metadata_parameter_invalid", fmt.Sprintf("invalid metadata-series view %q", query.view))
	}
}

func (h *MetadataHandler) fullSeriesPage(r *http.Request, query metadataHTTPQuery) (any, error) {
	snapshot, rawAfter, err := h.pageSnapshot(r, query)
	if err != nil {
		return nil, err
	}
	after, err := metadataAfterPosition[metadataSeriesPosition](rawAfter)
	if err != nil {
		return nil, err
	}
	if after != nil && !after.valid() {
		return nil, metadataFailure(400, "metadata_cursor_corrupt", "next_token has an invalid series position")
	}
	rows, next, annotations, err := snapshot.seriesPage(r.Context(), query, after)
	if err != nil {
		return nil, err
	}
	token, err := h.nextToken(snapshot, query, next)
	if err != nil {
		return nil, err
	}
	return metadataHTTPSeriesPage{
		SnapshotID: snapshot.id, NextToken: token, Annotations: requireNonNilAnnotations(annotations), Series: rows,
	}, nil
}

func (h *MetadataHandler) combinedSeriesPage(r *http.Request, query metadataHTTPQuery) (any, error) {
	snapshot, rawAfter, err := h.pageSnapshot(r, query)
	if err != nil {
		return nil, err
	}
	after, err := metadataAfterPosition[metadataGroupPosition](rawAfter)
	if err != nil {
		return nil, err
	}
	if after != nil && !after.valid() {
		return nil, metadataFailure(400, "metadata_cursor_corrupt", "next_token has an invalid combined position")
	}
	rows, next, annotations, err := snapshot.combinedPage(r.Context(), query, after)
	if err != nil {
		return nil, err
	}
	token, err := h.nextToken(snapshot, query, next)
	if err != nil {
		return nil, err
	}
	return metadataHTTPGroupPage{
		SnapshotID: snapshot.id, NextToken: token, Annotations: requireNonNilAnnotations(annotations), Groups: rows,
	}, nil
}

// derivedSeriesView keeps the noncontractual names/summary views. They are not
// snapshot-paginated and their truncation fields remain derivative behavior.
func (h *MetadataHandler) derivedSeriesView(r *http.Request, query metadataHTTPQuery) (any, error) {
	if r.URL.Query().Has("limit") {
		return nil, metadataFailure(400, "metadata_parameter_invalid",
			"limit is only supported for the full metadata-series view")
	}
	if r.URL.Query().Has("next_token") {
		return nil, metadataFailure(400, "metadata_parameter_invalid",
			"next_token is only supported for the full metadata-series view")
	}
	query.limit = math.MaxInt
	snapshot, err := h.storage.pinMetadataSnapshot()
	if err != nil {
		return nil, err
	}
	rows, _, annotations, err := snapshot.seriesPage(r.Context(), query, nil)
	if err != nil {
		return nil, err
	}
	names := map[string]struct{}{}
	for _, row := range rows {
		for _, label := range row.Series {
			if label.Name == labels.MetricName {
				names[label.Value] = struct{}{}
				break
			}
		}
	}
	ordered := slices.Sorted(maps.Keys(names))
	truncated := len(ordered) > maxMetadataMetricNames
	if truncated {
		ordered = ordered[:maxMetadataMetricNames]
	}
	if query.view == "names" {
		return map[string]any{
			"snapshot_id": snapshot.id, "next_token": nil,
			"annotations": requireNonNilAnnotations(annotations),
			"names":       ordered, "truncated": truncated,
		}, nil
	}
	return map[string]any{
		"snapshot_id": snapshot.id, "next_token": nil,
		"annotations":     requireNonNilAnnotations(annotations),
		"matched_series":  len(rows),
		"metric_families": len(names),
		"names_truncated": truncated,
	}, nil
}

func (h *MetadataHandler) diff(r *http.Request) (any, error) {
	query, err := parseMetadataHTTPQuery(r, metadataHTTPDiff)
	if err != nil {
		return nil, err
	}
	snapshot, rawAfter, err := h.pageSnapshot(r, query)
	if err != nil {
		return nil, err
	}
	after, err := metadataAfterPosition[metadataDiffPosition](rawAfter)
	if err != nil {
		return nil, err
	}
	if after != nil && !after.valid() {
		return nil, metadataFailure(400, "metadata_cursor_corrupt", "next_token has an invalid diff position")
	}
	rows, next, annotations, err := snapshot.diffPage(r.Context(), query, after)
	if err != nil {
		return nil, err
	}
	token, err := h.nextToken(snapshot, query, next)
	if err != nil {
		return nil, err
	}
	return metadataHTTPDiffPage{
		SnapshotID: snapshot.id, NextToken: token, Annotations: requireNonNilAnnotations(annotations), Changes: rows,
	}, nil
}

// pageSnapshot returns the pinned snapshot for a first page or the snapshot a
// cursor was issued for. Only the limit may change between pages.
func (h *MetadataHandler) pageSnapshot(r *http.Request, query metadataHTTPQuery) (*metadataSnapshot, json.RawMessage, error) {
	raw, present, err := singleMetadataParameter(r.URL.Query(), "next_token")
	if err != nil {
		return nil, nil, err
	}
	if !present {
		snapshot, err := h.storage.pinMetadataSnapshot()
		return snapshot, nil, err
	}
	if raw == "" {
		return nil, nil, metadataFailure(400, "metadata_cursor_malformed", "next_token must not be empty")
	}
	return h.storage.decodeMetadataCursor(raw, query.binding())
}

func (h *MetadataHandler) nextToken(snapshot *metadataSnapshot, query metadataHTTPQuery, next any) (*string, error) {
	if next == nil {
		return nil, nil
	}
	token, err := h.storage.encodeMetadataCursor(snapshot, query.binding(), next)
	if err != nil {
		return nil, err
	}
	return &token, nil
}

// metadataAfterPosition decodes a cursor after-position or returns nil for a
// first page.
func metadataAfterPosition[T any](raw json.RawMessage) (*T, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, metadataFailure(400, "metadata_cursor_corrupt", "next_token position cannot be decoded")
	}
	return &value, nil
}

func (h *MetadataHandler) cancelSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := h.storage.requireMetadataEnabled(); err != nil {
		h.fail(w, err)
		return
	}
	if r.Method != http.MethodDelete {
		h.fail(w, metadataFailure(405, "metadata_parameter_invalid", "method must be DELETE"))
		return
	}
	if len(r.URL.Query()) != 0 {
		h.fail(w, metadataFailure(400, "metadata_parameter_invalid", "snapshot cancellation accepts no query parameters"))
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/resources/snapshots/")
	if id == "" || strings.Contains(id, "/") {
		h.fail(w, metadataFailure(404, "metadata_snapshot_unknown", "metadata snapshot is unknown"))
		return
	}
	if err := h.storage.cancelMetadataSnapshot(id); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *MetadataHandler) respond(w http.ResponseWriter, data any) {
	payload, err := json.Marshal(map[string]any{"status": "success", "data": data})
	if err != nil {
		h.fail(w, metadataFailure(500, "metadata_internal", "response encoding failed"))
		return
	}
	if h.storage != nil && h.storage.reads != nil {
		h.storage.reads.metadataHTTPBytes.Add(uint64(len(payload)))
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(payload); err != nil && h.logger != nil {
		h.logger.Error("Failed to encode native metadata response", "err", err)
	}
}

func (*MetadataHandler) fail(w http.ResponseWriter, err error) {
	failure := metadataErrorFor(err)
	details := map[string]any{"code": failure.Code}
	maps.Copy(details, failure.Details)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(failure.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "error",
		"errorType": metadataErrorType(failure.Status),
		"error":     failure.Message,
		"data":      nil,
		"details":   details,
	})
}

// metadataErrorFor maps internal failures onto stable experimental classes.
func metadataErrorFor(err error) *metadataHTTPError {
	if failure, ok := errors.AsType[*metadataHTTPError](err); ok {
		return failure
	}
	if errors.Is(err, ErrMetadataDisabled) {
		return metadataFailure(501, "metadata_disabled", "native metadata is disabled")
	}
	return metadataFailure(500, "metadata_internal", err.Error())
}

// parseMetadataKind parses one owner kind, defaulting to resource. It keeps the
// exported-query compatibility used by the contract oracle.
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

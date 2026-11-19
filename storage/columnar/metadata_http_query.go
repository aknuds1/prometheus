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

// Decision 7 request parsing, query binding and snapshot reads. Filters and
// matchers are resolved from metadata-only series rows and owner histories; no
// path here opens a sample payload or segment.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/grafana/regexp"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

const (
	metadataHTTPDefaultLimit = 100
	metadataHTTPMaxLimit     = 10_000
)

// metadataHTTPOperation names one contractual page operation.
type metadataHTTPOperation string

const (
	metadataHTTPHistory        metadataHTTPOperation = "history"
	metadataHTTPDiscovery      metadataHTTPOperation = "discovery"
	metadataHTTPSeriesFull     metadataHTTPOperation = "series_full"
	metadataHTTPSeriesCombined metadataHTTPOperation = "series_combined"
	metadataHTTPDiff           metadataHTTPOperation = "diff"
)

// metadataHTTPQuery is the parsed contractual request. Limit is excluded from the
// cursor binding; every other semantic field is bound.
type metadataHTTPQuery struct {
	operation  metadataHTTPOperation
	kind       MetadataKind
	start, end int64
	time       int64
	matchers   [][]*labels.Matcher
	attributes []MetadataAttributeCondition
	projection []string
	latest     bool
	limit      int
	view       string
}

// metadataHTTPBinding is the canonical, commutative query identity a cursor
// binds. Map iteration order and matcher order never participate.
type metadataHTTPBinding struct {
	Operation  string   `json:"operation"`
	Kind       string   `json:"kind"`
	StartMS    int64    `json:"start_ms"`
	EndMS      int64    `json:"end_ms"`
	TimeMS     *int64   `json:"time_ms,omitempty"`
	Matchers   []string `json:"matchers"`
	Attributes []string `json:"attributes"`
	Projection []string `json:"projection"`
	Latest     bool     `json:"latest"`
}

// binding returns the canonical query identity.
func (q metadataHTTPQuery) binding() string {
	binding := metadataHTTPBinding{
		Operation: string(q.operation), Kind: q.kind.String(),
		StartMS: q.start, EndMS: q.end,
		Matchers: canonicalMetadataMatchers(q.matchers), Attributes: canonicalMetadataAttributes(q.attributes),
		Projection: canonicalMetadataProjection(q.projection), Latest: q.latest,
	}
	if q.operation == metadataHTTPDiff {
		at := q.time
		binding.TimeMS = &at
	}
	raw, err := json.Marshal(binding)
	if err != nil {
		// The binding contains only strings, integers and booleans.
		panic(err)
	}
	return string(raw)
}

func canonicalMetadataMatchers(matcherSets [][]*labels.Matcher) []string {
	out := make([]string, 0, len(matcherSets))
	for _, matchers := range matcherSets {
		encoded := make([]string, 0, len(matchers))
		for _, matcher := range matchers {
			encoded = append(encoded, matcher.Type.String()+"\x00"+matcher.Name+"\x00"+matcher.Value)
		}
		slices.Sort(encoded)
		out = append(out, strings.Join(encoded, "\x01"))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func canonicalMetadataAttributes(conditions []MetadataAttributeCondition) []string {
	out := make([]string, 0, len(conditions))
	for _, condition := range conditions {
		out = append(out, condition.Name+"\x00"+condition.Value)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func canonicalMetadataProjection(projection []string) []string {
	if len(projection) == 0 {
		return []string{}
	}
	out := slices.Clone(projection)
	slices.Sort(out)
	return slices.Compact(out)
}

// parseMetadataHTTPQuery validates one contractual request. Inapplicable and
// unknown parameters fail instead of being silently ignored.
func parseMetadataHTTPQuery(r *http.Request, operation metadataHTTPOperation) (metadataHTTPQuery, error) {
	values := r.URL.Query()
	query := metadataHTTPQuery{operation: operation, limit: metadataHTTPDefaultLimit}
	allowed := map[string]bool{
		"kind": true, "start": true, "end": true, "projection": true, "attributes": true,
		"limit": true, "next_token": true,
	}
	switch operation {
	case metadataHTTPHistory:
		allowed["match[]"] = true
		allowed["latest"] = true
	case metadataHTTPDiscovery:
		// match[] and latest are deliberately not accepted here.
	case metadataHTTPSeriesFull, metadataHTTPSeriesCombined:
		allowed["match[]"] = true
		allowed["resource.attr"] = true
		allowed["view"] = true
	case metadataHTTPDiff:
		allowed = map[string]bool{
			"kind": true, "time": true, "match[]": true, "projection": true,
			"attributes": true, "limit": true, "next_token": true,
		}
	}
	if operation == metadataHTTPHistory || operation == metadataHTTPDiscovery {
		allowed["format"] = true
	}
	if err := rejectMetadataParameters(values, allowed); err != nil {
		return query, err
	}
	kind, err := parseMetadataHTTPKind(values)
	if err != nil {
		return query, err
	}
	query.kind = kind

	switch operation {
	case metadataHTTPDiff:
		raw, present, err := singleMetadataParameter(values, "time")
		if err != nil {
			return query, err
		}
		if !present {
			return query, metadataFailure(400, "metadata_time_invalid", "time is required for a metadata diff")
		}
		query.time, err = parseMetadataSeconds(raw, "time")
		if err != nil {
			return query, err
		}
	default:
		query.start, query.end, err = parseMetadataRange(values)
		if err != nil {
			return query, err
		}
	}

	if operation == metadataHTTPDiff || operation == metadataHTTPHistory || operation == metadataHTTPSeriesFull || operation == metadataHTTPSeriesCombined {
		matchValues := values["match[]"]
		if query.matchers, err = parseMetadataHTTPMatchers(matchValues); err != nil {
			return query, err
		}
	}
	if operation == metadataHTTPSeriesFull || operation == metadataHTTPSeriesCombined {
		conditions, err := parseMetadataHTTPAttributes(values)
		if err != nil {
			return query, err
		}
		query.attributes = conditions
		view, err := parseMetadataHTTPView(values)
		if err != nil {
			return query, err
		}
		query.view = view
	}
	if operation == metadataHTTPHistory {
		latest, err := parseMetadataHTTPLatest(values)
		if err != nil {
			return query, err
		}
		query.latest = latest
	}
	if query.projection, err = parseMetadataHTTPProjection(values); err != nil {
		return query, err
	}
	limit, err := parseMetadataHTTPLimit(values)
	if err != nil {
		return query, err
	}
	query.limit = limit
	return query, nil
}

func rejectMetadataParameters(values url.Values, allowed map[string]bool) error {
	for name := range values {
		if !allowed[name] {
			return metadataFailure(400, "metadata_parameter_invalid",
				fmt.Sprintf("parameter %q is not supported for this metadata endpoint", name))
		}
	}
	return nil
}

func singleMetadataParameter(values url.Values, name string) (string, bool, error) {
	list, present := values[name]
	if !present {
		return "", false, nil
	}
	if len(list) != 1 {
		return "", false, metadataFailure(400, "metadata_parameter_invalid",
			fmt.Sprintf("parameter %q must be supplied at most once", name))
	}
	return list[0], true, nil
}

func parseMetadataHTTPKind(values url.Values) (MetadataKind, error) {
	raw, present, err := singleMetadataParameter(values, "kind")
	if err != nil {
		return MetadataResource, err
	}
	if !present {
		return MetadataResource, nil
	}
	if raw == "" {
		return MetadataResource, metadataFailure(400, "metadata_kind_unknown", "kind must not be empty")
	}
	for kind, name := range KindNames {
		if raw == name {
			return MetadataKind(kind), nil
		}
	}
	return MetadataResource, metadataFailure(400, "metadata_kind_unknown", fmt.Sprintf("unknown metadata kind %q", raw))
}

// parseMetadataRange parses inclusive start/end seconds exactly into
// milliseconds. Omitted start and end retain the incumbent open range.
func parseMetadataRange(values url.Values) (int64, int64, error) {
	start := int64(0)
	end := int64(math.MaxInt64)
	raw, present, err := singleMetadataParameter(values, "start")
	if err != nil {
		return 0, 0, err
	}
	if present {
		if start, err = parseMetadataSeconds(raw, "start"); err != nil {
			return 0, 0, err
		}
	}
	raw, present, err = singleMetadataParameter(values, "end")
	if err != nil {
		return 0, 0, err
	}
	if present {
		if end, err = parseMetadataSeconds(raw, "end"); err != nil {
			return 0, 0, err
		}
	}
	if start > end {
		return 0, 0, metadataFailure(400, "metadata_range_invalid",
			fmt.Sprintf("start %d must not be after end %d", start, end))
	}
	return start, end, nil
}

// metadataDecimalSeconds is the accepted decimal-seconds syntax: an optional
// sign, decimal mantissa and optional exponent. Hexadecimal, binary, octal,
// fractions and digit separators are rejected.
var metadataDecimalSeconds = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// parseMetadataSeconds parses decimal seconds without float64 truncation. A
// value must be finite and exactly representable at millisecond resolution.
func parseMetadataSeconds(raw, name string) (int64, error) {
	if raw == "" || !metadataDecimalSeconds.MatchString(raw) {
		return 0, metadataFailure(400, "metadata_time_invalid", fmt.Sprintf("%s must be a finite decimal number of seconds", name))
	}
	seconds, ok := new(big.Rat).SetString(raw)
	if !ok {
		return 0, metadataFailure(400, "metadata_time_invalid", fmt.Sprintf("%s must be a finite decimal number of seconds", name))
	}
	milliseconds := new(big.Rat).Mul(seconds, big.NewRat(1000, 1))
	if !milliseconds.IsInt() {
		return 0, metadataFailure(400, "metadata_timestamp_out_of_range",
			fmt.Sprintf("%s is not exactly representable at millisecond resolution", name))
	}
	numerator := milliseconds.Num()
	if !numerator.IsInt64() {
		return 0, metadataFailure(400, "metadata_timestamp_out_of_range",
			fmt.Sprintf("%s is outside the signed millisecond timestamp range", name))
	}
	return numerator.Int64(), nil
}

func parseMetadataHTTPProjection(values url.Values) ([]string, error) {
	var names []string
	for _, parameter := range []string{"projection", "attributes"} {
		for _, raw := range values[parameter] {
			for name := range strings.SplitSeq(raw, ",") {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				if strings.Contains(name, "*") {
					return nil, metadataFailure(400, "metadata_projection_invalid",
						fmt.Sprintf("wildcard metadata projection %q is not supported", name))
				}
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

func parseMetadataHTTPMatchers(raw []string) ([][]*labels.Matcher, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	result := make([][]*labels.Matcher, 0, len(raw))
	promParser := parser.NewParser(parser.Options{})
	for _, expression := range raw {
		matchers, err := promParser.ParseMetricSelector(expression)
		if err != nil {
			return nil, metadataFailure(400, "metadata_matcher_invalid",
				fmt.Sprintf("invalid match[] selector %q: %v", expression, err))
		}
		slices.SortFunc(matchers, func(a, b *labels.Matcher) int {
			return strings.Compare(a.Type.String()+"\x00"+a.Name+"\x00"+a.Value, b.Type.String()+"\x00"+b.Name+"\x00"+b.Value)
		})
		result = append(result, matchers)
	}
	slices.SortFunc(result, func(a, b []*labels.Matcher) int {
		return strings.Compare(canonicalMetadataMatcherSet(a), canonicalMetadataMatcherSet(b))
	})
	return result, nil
}

func canonicalMetadataMatcherSet(matchers []*labels.Matcher) string {
	encoded := make([]string, 0, len(matchers))
	for _, matcher := range matchers {
		encoded = append(encoded, matcher.Type.String()+"\x00"+matcher.Name+"\x00"+matcher.Value)
	}
	slices.Sort(encoded)
	return strings.Join(encoded, "\x01")
}

func parseMetadataHTTPAttributes(values url.Values) ([]MetadataAttributeCondition, error) {
	raw := values["resource.attr"]
	conditions := make([]MetadataAttributeCondition, 0, len(raw))
	for _, value := range raw {
		name, attributeValue, ok := strings.Cut(value, ":")
		if !ok || name == "" {
			return nil, metadataFailure(400, "metadata_matcher_invalid",
				fmt.Sprintf("resource.attr %q must have the form name:value", value))
		}
		conditions = append(conditions, MetadataAttributeCondition{Name: name, Value: attributeValue})
	}
	slices.SortFunc(conditions, func(a, b MetadataAttributeCondition) int {
		return strings.Compare(a.Name+"\x00"+a.Value, b.Name+"\x00"+b.Value)
	})
	return slices.CompactFunc(conditions, func(a, b MetadataAttributeCondition) bool {
		return a.Name == b.Name && a.Value == b.Value
	}), nil
}

func parseMetadataHTTPLatest(values url.Values) (bool, error) {
	raw, present, err := singleMetadataParameter(values, "latest")
	if err != nil {
		return false, err
	}
	if !present {
		return false, nil
	}
	switch raw {
	case "1", "true":
		return true, nil
	case "0", "false":
		return false, nil
	default:
		return false, metadataFailure(400, "metadata_parameter_invalid", "latest must be true or false")
	}
}

func parseMetadataHTTPView(values url.Values) (string, error) {
	raw, present, err := singleMetadataParameter(values, "view")
	if err != nil {
		return "", err
	}
	if !present || raw == "" {
		return "full", nil
	}
	switch raw {
	case "full", "combined", "names", "summary":
		return raw, nil
	default:
		return "", metadataFailure(400, "metadata_parameter_invalid", fmt.Sprintf("invalid metadata-series view %q", raw))
	}
}

func parseMetadataHTTPLimit(values url.Values) (int, error) {
	raw, present, err := singleMetadataParameter(values, "limit")
	if err != nil {
		return 0, err
	}
	if !present {
		return metadataHTTPDefaultLimit, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > metadataHTTPMaxLimit {
		return 0, metadataFailure(400, "metadata_limit_invalid",
			fmt.Sprintf("limit must be an integer between 1 and %d", metadataHTTPMaxLimit))
	}
	return limit, nil
}

// metadataHTTPPageLimit slices one already-ordered eligible sequence and reports
// the position of the last retained object exactly when a further eligible
// object exists. Limit changes are allowed because the binding excludes it.
func metadataHTTPPageLimit[T any](items []T, limit int, position func(T) any) ([]T, any) {
	if limit <= 0 || len(items) <= limit {
		return items, nil
	}
	return items[:limit], position(items[limit-1])
}

// ownerPage renders one snapshot-pinned owner-history page.
func (snap *metadataSnapshot) ownerPage(ctx context.Context, query metadataHTTPQuery, after *metadataOwnerPosition) ([]metadataHTTPOwnerRow, any, []map[string]any, error) {
	catalog, err := snap.beginRead()
	if err != nil {
		return nil, nil, nil, err
	}
	defer snap.endRead()
	owners := matchingOwners(catalog, query)
	sortOwnerKeys(owners)
	type candidate struct {
		owner       OwnerKey
		row         metadataHTTPOwnerRow
		annotations []map[string]any
	}
	candidates := make([]candidate, 0, len(owners))
	for _, owner := range owners {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, metadataFailure(500, "metadata_internal", err.Error())
		}
		position := metadataOwnerPosition{Kind: owner.Kind.String(), ID: owner.ID}
		if after != nil && compareMetadataOwnerPosition(position, *after) <= 0 {
			continue
		}
		history := catalog.byOwner[owner]
		versions := ownerAssertions(catalog, owner, query.start, query.end, query.projection, query.latest)
		if len(versions) == 0 {
			continue
		}
		item := candidate{
			owner: owner,
			row:   metadataHTTPOwnerRow{Owner: metadataHTTPOwnerFor(owner), Versions: versions},
		}
		if annotation := metadataHTTPOwnerHistoryAnnotation(&owner, labels.EmptyLabels(), "owner_history", query, history); annotation != nil {
			item.annotations = append(item.annotations, annotation)
		}
		candidates = append(candidates, item)
	}
	page, next := metadataHTTPPageLimit(candidates, query.limit, func(item candidate) any {
		return metadataOwnerPosition{Kind: item.owner.Kind.String(), ID: item.owner.ID}
	})
	rows := make([]metadataHTTPOwnerRow, 0, len(page))
	var annotations []map[string]any
	for _, item := range page {
		rows = append(rows, item.row)
		annotations = append(annotations, item.annotations...)
		snap.countOwnerMaterialization(item.row.Owner)
		snap.countResponseValues(item.row.Versions)
	}
	snap.countResponseRows(len(rows))
	if next == nil {
		return rows, nil, annotations, nil
	}
	return rows, next.(metadataOwnerPosition), annotations, nil
}

// matchingOwners resolves the owner set without reading samples. Matchers are
// evaluated on metadata-only series membership rows.
func matchingOwners(catalog *metadataStore, query metadataHTTPQuery) []OwnerKey {
	if len(query.matchers) == 0 {
		owners := make([]OwnerKey, 0, len(catalog.byOwner))
		for owner := range catalog.byOwner {
			if owner.Kind == query.kind {
				owners = append(owners, owner)
			}
		}
		return owners
	}
	matched := map[OwnerKey]struct{}{}
	for _, kinds := range catalog.bySeries {
		rows := kinds[query.kind]
		for i, row := range rows {
			if !metadataPeriodOverlaps(row.effectiveFrom, nextMembershipFrom(rows, i), query.start, query.end) {
				continue
			}
			if !matchesAny(row.ls, query.matchers) {
				continue
			}
			matched[row.owner] = struct{}{}
		}
	}
	return slices.Collect(maps.Keys(matched))
}

// ownerAssertions renders the versions overlapping the query range.
func ownerAssertions(catalog *metadataStore, owner OwnerKey, start, end int64, projection []string, latest bool) []metadataHTTPAssertion {
	history := catalog.byOwner[owner]
	if history == nil {
		return nil
	}
	assertions := make([]metadataHTTPAssertion, 0, len(history.versions))
	for i := range history.versions {
		next := nextEffectiveFrom(history.versions, i)
		if !metadataPeriodOverlaps(history.versions[i].EffectiveFrom, next, start, end) {
			continue
		}
		assertions = append(assertions, metadataHTTPAssertionFor(history.versions[i], next, history, projection, start))
	}
	if latest && len(assertions) > 1 {
		assertions = assertions[len(assertions)-1:]
	}
	return assertions
}

// seriesPage renders full series associations. A series with no association of
// the requested kind is reported as explicitly unassociated when no attribute
// filter constrains it.
func (snap *metadataSnapshot) seriesPage(ctx context.Context, query metadataHTTPQuery, after *metadataSeriesPosition) ([]metadataHTTPSeriesRow, any, []map[string]any, error) {
	catalog, err := snap.beginRead()
	if err != nil {
		return nil, nil, nil, err
	}
	defer snap.endRead()
	type candidate struct {
		position    metadataSeriesPosition
		row         metadataHTTPSeriesRow
		annotations []map[string]any
	}
	refs := seriesRefs(catalog)
	candidates := make([]candidate, 0, len(refs))
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, metadataFailure(500, "metadata_internal", err.Error())
		}
		rows := catalog.bySeries[ref.hash][query.kind]
		labelsKey := ref.ls.String()
		if len(rows) == 0 {
			if len(query.attributes) != 0 {
				continue
			}
			if len(query.matchers) != 0 && !matchesAny(ref.ls, query.matchers) {
				continue
			}
			position := metadataSeriesPosition{Labels: labelsKey, State: "unassociated"}
			if after != nil && compareMetadataSeriesPosition(position, *after) <= 0 {
				continue
			}
			candidates = append(candidates, candidate{
				position: position,
				row: metadataHTTPSeriesRow{
					Series: metadataHTTPLabels(ref.ls),
					Association: metadataHTTPAssociation{
						State: "unassociated", Coverage: metadataHTTPAssociationCoverage(),
					},
					Versions: []metadataHTTPAssertion{},
				},
			})
			continue
		}
		history := catalog.byOwner
		for i, row := range rows {
			if !metadataPeriodOverlaps(row.effectiveFrom, nextMembershipFrom(rows, i), query.start, query.end) {
				continue
			}
			if len(query.matchers) != 0 && !matchesAny(row.ls, query.matchers) {
				continue
			}
			ownerHistory := history[row.owner]
			versions := seriesAssertions(row.owner, ownerHistory, i, rows, query)
			if len(versions) == 0 {
				continue
			}
			position := metadataSeriesPosition{
				Labels: labelsKey, State: "applicable", OwnerKind: row.owner.Kind.String(),
				OwnerID: row.owner.ID, FromMS: row.effectiveFrom,
			}
			if after != nil && compareMetadataSeriesPosition(position, *after) <= 0 {
				continue
			}
			owner := metadataHTTPOwnerFor(row.owner)
			validity := metadataHTTPValidity{
				FromMS:  formatMetadataMilliseconds(row.effectiveFrom),
				UntilMS: nullableMetadataMilliseconds(nextMembershipFrom(rows, i)),
			}
			last := formatMetadataMilliseconds(row.observedUntil)
			item := candidate{
				position: position,
				row: metadataHTTPSeriesRow{
					Series: metadataHTTPLabels(row.ls),
					Association: metadataHTTPAssociation{
						State: "applicable", Validity: &validity,
						Observation: &metadataHTTPObservation{FirstMS: nil, LastMS: &last},
						Coverage:    metadataHTTPAssociationObservationCoverage(),
					},
					Owner: &owner, Versions: versions,
				},
			}
			item.annotations = append(item.annotations,
				metadataHTTPAssociationObservationAnnotation(row.owner, row.ls, validity, row.observedUntil, query))
			if annotation := metadataHTTPOwnerHistoryAnnotation(&row.owner, row.ls, "owner_history", query, ownerHistory); annotation != nil {
				item.annotations = append(item.annotations, annotation)
			}
			candidates = append(candidates, item)
		}
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		return compareMetadataSeriesPosition(a.position, b.position)
	})
	page, next := metadataHTTPPageLimit(candidates, query.limit, func(item candidate) any { return item.position })
	rows := make([]metadataHTTPSeriesRow, 0, len(page))
	var annotations []map[string]any
	for _, item := range page {
		rows = append(rows, item.row)
		annotations = append(annotations, item.annotations...)
		snap.countSeriesLabels(item.row.Series)
		if item.row.Owner != nil {
			snap.countOwnerMaterialization(*item.row.Owner)
		}
		snap.countResponseValues(item.row.Versions)
	}
	snap.countResponseRows(len(rows))
	if next == nil {
		return rows, nil, annotations, nil
	}
	return rows, next.(metadataSeriesPosition), annotations, nil
}

// seriesAssertions renders the versions matching both the membership band and
// the query filters.
func seriesAssertions(owner OwnerKey, history *metadataOwnerHistory, membershipIndex int, rows []seriesMembershipRow, query metadataHTTPQuery) []metadataHTTPAssertion {
	if history == nil {
		return nil
	}
	row := rows[membershipIndex]
	nextMembership := nextMembershipFrom(rows, membershipIndex)
	assertions := make([]metadataHTTPAssertion, 0, len(history.versions))
	for j := range history.versions {
		version := history.versions[j]
		nextVersion := nextEffectiveFrom(history.versions, j)
		if !metadataMembershipAndVersionOverlap(row, nextMembership, version, nextVersion, query.start, query.end) {
			continue
		}
		if len(query.attributes) != 0 && !metadataHTTPAttributeMatches(owner, version, query.attributes) {
			continue
		}
		assertions = append(assertions, metadataHTTPAssertionFor(version, nextVersion, history, query.projection, query.start))
	}
	return assertions
}

// metadataHTTPAttributeMatches is an exact present-string matcher. A key absent
// from both content and identity never matches, including the empty string.
// Identifying attributes are present applicable identity strings, not content:
// they participate in matching but are reported under owner.identity rather
// than invented as content entries.
func metadataHTTPAttributeMatches(owner OwnerKey, version Version, conditions []MetadataAttributeCondition) bool {
	identity := metadataOwnerIdentity(owner)
	for _, condition := range conditions {
		value, present := version.Content.Attributes[condition.Name]
		if !present {
			value, present = identity[condition.Name]
			if present && owner.Kind == MetadataResource && value == "" && metadataCompatibilityIdentityKey(condition.Name) {
				// An empty compatibility fallback value is unrecoverable, not a
				// proven present-empty string.
				present = false
			}
		}
		if !present || value != condition.Value {
			return false
		}
	}
	return true
}

// seriesRef is one metadata-only series identity.
type seriesRef struct {
	hash uint64
	ls   labels.Labels
}

func seriesRefs(catalog *metadataStore) []seriesRef {
	refs := make([]seriesRef, 0, len(catalog.bySeries))
	for hash, kinds := range catalog.bySeries {
		for _, rows := range kinds {
			if len(rows) > 0 {
				refs = append(refs, seriesRef{hash: hash, ls: rows[0].ls})
				break
			}
		}
	}
	slices.SortFunc(refs, func(a, b seriesRef) int {
		if byLabels := labels.Compare(a.ls, b.ls); byLabels != 0 {
			return byLabels
		}
		switch {
		case a.hash < b.hash:
			return -1
		case a.hash > b.hash:
			return 1
		default:
			return 0
		}
	})
	return refs
}

// diffPage renders bounded point owner-content diffs.
func (snap *metadataSnapshot) diffPage(ctx context.Context, query metadataHTTPQuery, after *metadataDiffPosition) ([]metadataHTTPDiffRow, any, []map[string]any, error) {
	catalog, err := snap.beginRead()
	if err != nil {
		return nil, nil, nil, err
	}
	defer snap.endRead()
	owners := ownersAtTime(catalog, query)
	sortOwnerKeys(owners)
	type candidate struct {
		position    metadataDiffPosition
		row         metadataHTTPDiffRow
		annotations []map[string]any
	}
	candidates := make([]candidate, 0, len(owners))
	for _, owner := range owners {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, metadataFailure(500, "metadata_internal", err.Error())
		}
		history := catalog.byOwner[owner]
		if history == nil || len(history.versions) == 0 {
			continue
		}
		afterIndex := -1
		for i, version := range history.versions {
			if version.EffectiveFrom <= query.time {
				afterIndex = i
			}
		}
		if afterIndex < 0 {
			continue
		}
		position := metadataDiffPosition{
			Kind: owner.Kind.String(), ID: owner.ID, From: history.versions[afterIndex].EffectiveFrom,
		}
		if after != nil && compareMetadataDiffPosition(position, *after) <= 0 {
			continue
		}
		beforeVersion := Version{}
		haveBefore := afterIndex > 0
		if haveBefore {
			beforeVersion = history.versions[afterIndex-1]
		}
		afterVersion := history.versions[afterIndex]
		if haveBefore && mapsEqual(beforeVersion.Content.Attributes, afterVersion.Content.Attributes) {
			continue
		}
		lostPredecessor := !haveBefore && history.truncated
		coverage := metadataHTTPCoveredCoverage("diff")
		if lostPredecessor {
			coverage = metadataHTTPTruncatedCoverage("diff", history.versions[0].EffectiveFrom, "lost_predecessor", "history_truncated")
		}
		row := metadataHTTPDiffRow{
			Owner:           metadataHTTPOwnerFor(owner),
			EffectiveFromMS: formatMetadataMilliseconds(afterVersion.EffectiveFrom),
			Before:          snap.diffBeforeEndpoint(history, beforeVersion, haveBefore, lostPredecessor, afterIndex, query),
			After: metadataHTTPDiffEndpoint{
				State:     "present",
				Assertion: metadataHTTPAssertionPointer(metadataHTTPAssertionFor(afterVersion, nextEffectiveFrom(history.versions, afterIndex), history, query.projection, query.time)),
				Coverage:  metadataHTTPOwnerCoverage(history, query.time),
			},
			Coverage: coverage,
			Fields:   snap.diffFields(beforeVersion, lostPredecessor, afterVersion, query.projection),
		}
		item := candidate{position: position, row: row}
		if lostPredecessor {
			if annotation := metadataHTTPLostPredecessorAnnotation(owner, query, history); annotation != nil {
				item.annotations = append(item.annotations, annotation)
			}
		}
		candidates = append(candidates, item)
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		return compareMetadataDiffPosition(a.position, b.position)
	})
	page, next := metadataHTTPPageLimit(candidates, query.limit, func(item candidate) any { return item.position })
	rows := make([]metadataHTTPDiffRow, 0, len(page))
	var annotations []map[string]any
	for _, item := range page {
		rows = append(rows, item.row)
		annotations = append(annotations, item.annotations...)
		snap.countOwnerMaterialization(item.row.Owner)
		snap.countResponseValues([]metadataHTTPAssertion{item.row.After.AssertionValue()})
		if item.row.Before.Assertion != nil {
			snap.countResponseValues([]metadataHTTPAssertion{*item.row.Before.Assertion})
		}
		if snap.reads != nil {
			snap.reads.metadataHTTPValues.Add(uint64(len(item.row.Fields) * 2))
		}
	}
	snap.countResponseRows(len(rows))
	if next == nil {
		return rows, nil, annotations, nil
	}
	return rows, next.(metadataDiffPosition), annotations, nil
}

// ownersAtTime resolves owners whose membership is applicable at the exact diff
// time. Without matchers every owner of the kind participates, matching the
// owner-content diff scope.
func ownersAtTime(catalog *metadataStore, query metadataHTTPQuery) []OwnerKey {
	if len(query.matchers) == 0 {
		owners := make([]OwnerKey, 0, len(catalog.byOwner))
		for owner := range catalog.byOwner {
			if owner.Kind == query.kind {
				owners = append(owners, owner)
			}
		}
		return owners
	}
	matched := map[OwnerKey]struct{}{}
	for _, kinds := range catalog.bySeries {
		rows := kinds[query.kind]
		for i, row := range rows {
			next := nextMembershipFrom(rows, i)
			if row.effectiveFrom > query.time || (next != nil && *next <= query.time) {
				continue
			}
			if !matchesAny(row.ls, query.matchers) {
				continue
			}
			matched[row.owner] = struct{}{}
		}
	}
	return slices.Collect(maps.Keys(matched))
}

func metadataHTTPAssertionPointer(assertion metadataHTTPAssertion) *metadataHTTPAssertion {
	return &assertion
}

// AssertionValue returns the endpoint assertion or an empty one for accounting.
func (e metadataHTTPDiffEndpoint) AssertionValue() metadataHTTPAssertion {
	if e.Assertion == nil {
		return metadataHTTPAssertion{}
	}
	return *e.Assertion
}

func (*metadataSnapshot) diffBeforeEndpoint(history *metadataOwnerHistory, before Version, haveBefore, lostPredecessor bool, afterIndex int, query metadataHTTPQuery) metadataHTTPDiffEndpoint {
	switch {
	case haveBefore:
		return metadataHTTPDiffEndpoint{
			State:     "present",
			Assertion: metadataHTTPAssertionPointer(metadataHTTPAssertionFor(before, nextEffectiveFrom(history.versions, afterIndex-1), history, query.projection, query.time)),
			Coverage:  metadataHTTPOwnerCoverage(history, query.time),
		}
	case lostPredecessor:
		return metadataHTTPDiffEndpoint{
			State: "unknown", Reason: "lost_predecessor",
			Coverage: metadataHTTPTruncatedCoverage("diff", history.versions[0].EffectiveFrom, "lost_predecessor", "history_truncated"),
		}
	default:
		return metadataHTTPDiffEndpoint{State: "absent", Coverage: metadataHTTPCoveredCoverage("diff")}
	}
}

func (*metadataSnapshot) diffFields(before Version, lostPredecessor bool, after Version, projection []string) []metadataHTTPDiffField {
	var keys []string
	if projection == nil {
		unique := map[string]struct{}{}
		for key := range before.Content.Attributes {
			unique[key] = struct{}{}
		}
		for key := range after.Content.Attributes {
			unique[key] = struct{}{}
		}
		keys = slices.Sorted(maps.Keys(unique))
	} else {
		keys = projection
	}
	fields := make([]metadataHTTPDiffField, 0, len(keys))
	for _, key := range keys {
		beforeValue, beforePresent := before.Content.Attributes[key]
		afterValue, afterPresent := after.Content.Attributes[key]
		var beforeState metadataTypedValue
		switch {
		case lostPredecessor:
			beforeState = metadataUnknownValue("lost_predecessor")
		case beforePresent:
			beforeState = metadataPresentString(beforeValue)
		default:
			beforeState = metadataAbsentValue()
		}
		afterState := metadataAbsentValue()
		if afterPresent {
			afterState = metadataPresentString(afterValue)
		}
		fields = append(fields, metadataHTTPDiffField{Key: key, Before: beforeState, After: afterState})
	}
	return fields
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, ok := b[key]; !ok || other != value {
			return false
		}
	}
	return true
}

// discoveryPage renders typed attribute discovery with deduplicated values.
func (snap *metadataSnapshot) discoveryPage(ctx context.Context, query metadataHTTPQuery, after *metadataDiscoveryPosition) ([]metadataHTTPDiscoveryRow, any, []map[string]any, error) {
	catalog, err := snap.beginRead()
	if err != nil {
		return nil, nil, nil, err
	}
	defer snap.endRead()
	type discovered struct {
		values            map[string]metadataTypedValue
		truncated         bool
		earliestTruncated int64
	}
	values := map[string]*discovered{}
	for owner, history := range catalog.byOwner {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, metadataFailure(500, "metadata_internal", err.Error())
		}
		if owner.Kind != query.kind {
			continue
		}
		for i, version := range history.versions {
			if !metadataPeriodOverlaps(version.EffectiveFrom, nextEffectiveFrom(history.versions, i), query.start, query.end) {
				continue
			}
			for name, value := range version.Content.Attributes {
				if query.projection != nil && !slices.Contains(query.projection, name) {
					continue
				}
				entry := values[name]
				if entry == nil {
					entry = &discovered{values: map[string]metadataTypedValue{}}
					values[name] = entry
				}
				typed := metadataPresentString(value)
				entry.values[metadataTypedValueCanonical(typed)] = typed
				if history.truncated && len(history.versions) > 0 && query.start < history.versions[0].EffectiveFrom {
					if !entry.truncated || history.versions[0].EffectiveFrom < entry.earliestTruncated {
						entry.earliestTruncated = history.versions[0].EffectiveFrom
					}
					entry.truncated = true
				}
			}
		}
	}
	names := slices.Sorted(maps.Keys(values))
	if after != nil {
		names = slices.DeleteFunc(names, func(name string) bool { return name <= after.Key })
	}
	type candidate struct {
		position    metadataDiscoveryPosition
		row         metadataHTTPDiscoveryRow
		annotations []map[string]any
	}
	candidates := make([]candidate, 0, len(names))
	for _, name := range names {
		entry := values[name]
		var candidateAnnotations []map[string]any
		typedValues := mapsValuesByCanonical(entry.values)
		row := metadataHTTPDiscoveryRow{Key: name, Values: typedValues}
		if entry.truncated {
			earliest := entry.earliestTruncated
			row.Coverage = metadataHTTPTruncatedCoverage("discovery", earliest, "history_truncated")
			candidateAnnotations = append(candidateAnnotations, metadataHTTPAnnotation(metadataHTTPAnnotationParams{
				Scope: "discovery", State: "truncated", Reason: "history_truncated",
				Category: query.kind.String(), Key: name, Query: query, Earliest: &earliest,
				Ranges:  []metadataHTTPRange{{FromMS: formatMetadataMilliseconds(earliest), UntilMS: nil}},
				Reasons: []string{"history_truncated"},
			}))
		} else {
			row.Coverage = metadataHTTPCoveredCoverage("discovery")
		}
		candidates = append(candidates, candidate{
			position: metadataDiscoveryPosition{Key: name}, row: row, annotations: candidateAnnotations,
		})
	}
	page, next := metadataHTTPPageLimit(candidates, query.limit, func(item candidate) any { return item.position })
	rows := make([]metadataHTTPDiscoveryRow, 0, len(page))
	var annotations []map[string]any
	for _, item := range page {
		rows = append(rows, item.row)
		annotations = append(annotations, item.annotations...)
		if snap.reads != nil {
			snap.reads.metadataHTTPValues.Add(uint64(len(item.row.Values)))
		}
	}
	snap.countResponseRows(len(rows))
	if next == nil {
		return rows, nil, annotations, nil
	}
	return rows, next.(metadataDiscoveryPosition), annotations, nil
}

func mapsValuesByCanonical(input map[string]metadataTypedValue) []metadataTypedValue {
	out := make([]metadataTypedValue, 0, len(input))
	for _, value := range input {
		out = append(out, value)
	}
	slices.SortFunc(out, func(a, b metadataTypedValue) int {
		return strings.Compare(metadataTypedValueDisplayKey(a), metadataTypedValueDisplayKey(b))
	})
	return out
}

// combinedPage renders one logical group per series. Without authoritative
// Entity evidence the group contains only that series' original Resource
// component(s); components are never merged and no winner is selected.
func (snap *metadataSnapshot) combinedPage(ctx context.Context, query metadataHTTPQuery, after *metadataGroupPosition) ([]metadataHTTPGroupRow, any, []map[string]any, error) {
	catalog, err := snap.beginRead()
	if err != nil {
		return nil, nil, nil, err
	}
	defer snap.endRead()
	refs := seriesRefs(catalog)
	type candidate struct {
		position    metadataGroupPosition
		row         metadataHTTPGroupRow
		annotations []map[string]any
	}
	candidates := make([]candidate, 0, len(refs))
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, metadataFailure(500, "metadata_internal", err.Error())
		}
		rows := catalog.bySeries[ref.hash][query.kind]
		if !seriesMatches(catalog, ref, rows, query) {
			continue
		}
		var candidateAnnotations []map[string]any
		group := metadataHTTPGroupRow{
			Series: metadataHTTPLabels(ref.ls), Relations: []map[string]any{},
			Components: []metadataHTTPComponent{}, Coverage: metadataHTTPCoveredCoverage("combined"),
		}
		resourceRows := catalog.bySeries[ref.hash][MetadataResource]
		firstOwnerID := ""
		for i, row := range resourceRows {
			if !metadataPeriodOverlaps(row.effectiveFrom, nextMembershipFrom(resourceRows, i), query.start, query.end) {
				continue
			}
			history := catalog.byOwner[row.owner]
			versions := ownerAssertions(catalog, row.owner, query.start, query.end, query.projection, false)
			if len(versions) == 0 {
				continue
			}
			if len(group.Components) == 0 {
				firstOwnerID = row.owner.ID
			}
			group.Components = append(group.Components, metadataHTTPComponent{
				Owner: metadataHTTPOwnerFor(row.owner), Schema: resourceSchemaAssertions(catalog, ref.hash, row.owner, query),
				Versions: versions, SourceAssertions: sourceAssertions(catalog, ref.hash, query),
			})
			coverage := metadataHTTPOwnerCoverage(history, query.start)
			if coverage.State == "truncated" {
				switch {
				case group.Coverage.State != "truncated":
					group.Coverage = coverage
				case coverage.EarliestRetainedMS != nil &&
					(group.Coverage.EarliestRetainedMS == nil || *coverage.EarliestRetainedMS < *group.Coverage.EarliestRetainedMS):
					group.Coverage = coverage
				}
				if annotation := metadataHTTPOwnerHistoryAnnotation(&row.owner, ref.ls, "combined", query, history); annotation != nil {
					candidateAnnotations = append(candidateAnnotations, annotation)
				}
			}
		}
		if len(group.Components) == 0 {
			group.Coverage = metadataHTTPUnknownCoverage("combined", "no_resource_component")
			candidateAnnotations = append(candidateAnnotations, metadataHTTPMissingResourceAnnotation(ref.ls, query))
		}
		position := metadataGroupPosition{Labels: ref.ls.String()}
		if len(group.Components) > 0 {
			position.OwnerKind = group.Components[0].Owner.Kind
			position.OwnerID = firstOwnerID
			first := group.Components[0]
			if len(first.Versions) > 0 {
				position.FromMS = parseMetadataMillisecondsOrZero(first.Versions[0].Validity.FromMS)
			}
		}
		if after != nil && compareMetadataGroupPosition(position, *after) <= 0 {
			continue
		}
		candidates = append(candidates, candidate{position: position, row: group, annotations: candidateAnnotations})
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		return compareMetadataGroupPosition(a.position, b.position)
	})
	page, next := metadataHTTPPageLimit(candidates, query.limit, func(item candidate) any { return item.position })
	rows := make([]metadataHTTPGroupRow, 0, len(page))
	var annotations []map[string]any
	for _, item := range page {
		rows = append(rows, item.row)
		annotations = append(annotations, item.annotations...)
		snap.countSeriesLabels(item.row.Series)
		for _, component := range item.row.Components {
			snap.countOwnerMaterialization(component.Owner)
			snap.countResponseValues(component.Schema)
			snap.countResponseValues(component.Versions)
			for _, assertion := range component.SourceAssertions {
				snap.countResponseValues(assertion.Versions)
			}
		}
	}
	snap.countResponseRows(len(rows))
	if next == nil {
		return rows, nil, annotations, nil
	}
	return rows, next.(metadataGroupPosition), annotations, nil
}

// seriesMatches reports whether one series has an applicable row of the
// requested kind satisfying the query filters.
func seriesMatches(catalog *metadataStore, ref seriesRef, rows []seriesMembershipRow, query metadataHTTPQuery) bool {
	if len(rows) == 0 {
		if len(query.attributes) != 0 {
			return false
		}
		return len(query.matchers) == 0 || matchesAny(ref.ls, query.matchers)
	}
	for i, row := range rows {
		if !metadataPeriodOverlaps(row.effectiveFrom, nextMembershipFrom(rows, i), query.start, query.end) {
			continue
		}
		if len(query.matchers) != 0 && !matchesAny(row.ls, query.matchers) {
			continue
		}
		history := catalog.byOwner[row.owner]
		if len(seriesAssertions(row.owner, history, i, rows, query)) != 0 {
			return true
		}
	}
	return false
}

// resourceSchemaAssertions returns the Resource-schema context associated with
// one original Resource component.
func resourceSchemaAssertions(catalog *metadataStore, seriesHash uint64, resource OwnerKey, query metadataHTTPQuery) []metadataHTTPAssertion {
	wanted := ResourceSchemaOwner(resource)
	rows := catalog.bySeries[seriesHash][MetadataResourceSchema]
	var assertions []metadataHTTPAssertion
	for i, row := range rows {
		if row.owner != wanted {
			continue
		}
		if !metadataPeriodOverlaps(row.effectiveFrom, nextMembershipFrom(rows, i), query.start, query.end) {
			continue
		}
		assertions = append(assertions, ownerAssertions(catalog, row.owner, query.start, query.end, query.projection, false)...)
	}
	if assertions == nil {
		return []metadataHTTPAssertion{}
	}
	return assertions
}

// sourceAssertions preserves independent Source owner content as opaque
// assertions. It deliberately does not invent Source role keys.
func sourceAssertions(catalog *metadataStore, seriesHash uint64, query metadataHTTPQuery) []metadataHTTPSourceAssertion {
	rows := catalog.bySeries[seriesHash][MetadataSource]
	assertions := make([]metadataHTTPSourceAssertion, 0, len(rows))
	for i, row := range rows {
		if !metadataPeriodOverlaps(row.effectiveFrom, nextMembershipFrom(rows, i), query.start, query.end) {
			continue
		}
		versions := ownerAssertions(catalog, row.owner, query.start, query.end, query.projection, false)
		if len(versions) == 0 {
			continue
		}
		assertions = append(assertions, metadataHTTPSourceAssertion{Owner: metadataHTTPOwnerFor(row.owner), Versions: versions})
	}
	return assertions
}

func parseMetadataMillisecondsOrZero(value string) int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func (snap *metadataSnapshot) countOwnerMaterialization(owner metadataHTTPOwner) {
	if snap.reads == nil {
		return
	}
	values := len(owner.Identity.Attributes) + len(owner.Identity.Labels)
	if owner.Identity.Resource != nil {
		values += len(owner.Identity.Resource.Identity.Attributes) + len(owner.Identity.Resource.Identity.Labels)
	}
	snap.reads.metadataHTTPValues.Add(uint64(values))
}

func (snap *metadataSnapshot) countSeriesLabels(series []metadataHTTPLabel) {
	if snap.reads != nil {
		snap.reads.metadataHTTPValues.Add(uint64(len(series)))
	}
}

func (snap *metadataSnapshot) countResponseRows(rows int) {
	if snap.reads != nil {
		snap.reads.metadataHTTPRows.Add(uint64(rows))
	}
}

func (snap *metadataSnapshot) countResponseValues(assertions []metadataHTTPAssertion) {
	if snap.reads == nil {
		return
	}
	values := 0
	for _, assertion := range assertions {
		values += len(assertion.Content.Entries)
	}
	snap.reads.metadataHTTPValues.Add(uint64(values))
}

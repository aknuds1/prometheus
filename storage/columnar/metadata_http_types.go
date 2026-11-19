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

// Decision 7 response types. Every owner-bearing row carries a complete
// structured identity; semantic content is a tagged typed value; validity and
// observation times use decimal-string signed integer milliseconds. next_token
// is always present and is null exactly at terminal completion.
//
// Field contract:
//   - Success envelope: status and data are required. Every page data object
//     requires snapshot_id, next_token (nullable) and annotations (non-null),
//     plus its route array (owners, attributes, series, groups or changes),
//     which is always non-null.
//   - Error envelope: status, errorType, error, data (null) and details.code are
//     required. No page prefix or usable continuation is exposed.
//   - owner: kind and identity are required; ref is optional and is a
//     non-authoritative lookup alias.
//   - identity: kind is required. attributes is used by resource, scope, scrape
//     and source; labels by metric (complete ordered set); resource by
//     resource_schema and references the complete Resource identity. The
//     irrelevant member is omitted, never null.
//   - content: scope and entries are required; entries is non-null and holds
//     exactly one state per materialized key. Omitted keys were not requested.
//   - validity: from_ms is a required decimal string; until_ms is required and
//     null only for open-ended validity.
//   - observation: first_ms and last_ms are required; null would mean an unknown
//     bound requiring non-covered evidence, which the incumbent never emits.
//   - coverage: state, scope, earliest_retained_ms (nullable), ranges and
//     reasons are required; ranges and reasons are non-null.
//   - assertion: content, validity, observation, coverage and source are
//     required. Source attribution is at the assertion, not per field.
//   - series row: series, association and versions are required and non-null;
//     owner is present exactly for the applicable association state and is
//     omitted for unassociated. Association validity and observation are
//     present only for applicable; coverage and state are always present.
//   - diff row: owner, effective_from_ms, before, after, fields and coverage are
//     required. before.state and after.state are required. before.reason is
//     present only for an unknown lost predecessor; before.assertion only for a
//     present predecessor.
//   - discovery row: key, values and coverage are required; values is non-null.
//   - combined group: series, relations, components and coverage are required;
//     relations and components are non-null. Each component requires owner,
//     schema, versions and source_assertions, all non-null. A group is one
//     pagination object; components are never split.
//   - typed value: state is required. present requires type and the payload for
//     that type (value, decimal, bits, items or entries); items and entries are
//     non-null. Maps are sorted and unique. unknown requires reason; ambiguous
//     requires assertions. Irrelevant payload members are omitted.

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// metadataHTTPError is a stable experimental error with a details.code.
type metadataHTTPError struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

// Error returns the safe human-readable error text.
func (e *metadataHTTPError) Error() string { return e.Message }

// metadataFailure builds one stable error.
func metadataFailure(status int, code, message string, details ...any) *metadataHTTPError {
	parsed := map[string]any{}
	for i := 0; i+1 < len(details); i += 2 {
		key, ok := details[i].(string)
		if !ok {
			continue
		}
		parsed[key] = details[i+1]
	}
	return &metadataHTTPError{Status: status, Code: code, Message: message, Details: parsed}
}

// metadataErrorType maps an HTTP status to the accepted errorType vocabulary.
func metadataErrorType(status int) string {
	switch status {
	case 400, 404, 405:
		return "bad_data"
	case 410, 501:
		return "unavailable"
	default:
		return "internal"
	}
}

// metadataHTTPEntry is one exact flattened key and its typed value state.
type metadataHTTPEntry struct {
	Key   string             `json:"key"`
	Value metadataTypedValue `json:"value"`
}

// metadataHTTPLabel is one ordered series label.
type metadataHTTPLabel struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// metadataHTTPIdentity is a structured domain identity. Metric identities keep
// the complete ordered label set; Resource-schema identities reference the
// complete Resource identity.
type metadataHTTPIdentity struct {
	Kind       string              `json:"kind"`
	Attributes []metadataHTTPEntry `json:"attributes,omitempty"`
	Labels     []metadataHTTPLabel `json:"labels,omitempty"`
	Resource   *metadataHTTPOwner  `json:"resource,omitempty"`
}

// metadataHTTPOwner is one complete owner identity and its optional
// non-authoritative lookup alias.
type metadataHTTPOwner struct {
	Kind     string               `json:"kind"`
	Identity metadataHTTPIdentity `json:"identity"`
	Ref      string               `json:"ref,omitempty"`
}

// metadataHTTPContent is one content assertion.
type metadataHTTPContent struct {
	Scope   string              `json:"scope"`
	Entries []metadataHTTPEntry `json:"entries"`
}

// metadataHTTPValidity is an inclusive from and exclusive until bound. A null
// until_ms means open-ended only.
type metadataHTTPValidity struct {
	FromMS  string  `json:"from_ms"`
	UntilMS *string `json:"until_ms"`
}

// metadataHTTPObservation records actual observations. A null bound means the
// bound is unknown and requires non-covered evidence.
type metadataHTTPObservation struct {
	FirstMS *string `json:"first_ms"`
	LastMS  *string `json:"last_ms"`
}

// metadataHTTPRange is one retained-evidence range when representable.
type metadataHTTPRange struct {
	FromMS  string  `json:"from_ms"`
	UntilMS *string `json:"until_ms"`
}

// metadataHTTPCoverage explains whether retained evidence is sufficient.
type metadataHTTPCoverage struct {
	State              string              `json:"state"`
	Scope              string              `json:"scope"`
	EarliestRetainedMS *string             `json:"earliest_retained_ms"`
	Ranges             []metadataHTTPRange `json:"ranges"`
	Reasons            []string            `json:"reasons"`
}

// metadataHTTPAssertion is one owner version or applicable series content.
type metadataHTTPAssertion struct {
	Content     metadataHTTPContent     `json:"content"`
	Validity    metadataHTTPValidity    `json:"validity"`
	Observation metadataHTTPObservation `json:"observation"`
	Coverage    metadataHTTPCoverage    `json:"coverage"`
	Source      map[string]any          `json:"source"`
}

// metadataHTTPOwnerRow is one owner history.
type metadataHTTPOwnerRow struct {
	Owner    metadataHTTPOwner       `json:"owner"`
	Versions []metadataHTTPAssertion `json:"versions"`
}

// metadataHTTPDiscoveryRow is one exact attribute and its deduplicated values.
type metadataHTTPDiscoveryRow struct {
	Key      string               `json:"key"`
	Values   []metadataTypedValue `json:"values"`
	Coverage metadataHTTPCoverage `json:"coverage"`
}

// metadataHTTPAssociation is the explicit series association state.
type metadataHTTPAssociation struct {
	State       string                   `json:"state"`
	Validity    *metadataHTTPValidity    `json:"validity,omitempty"`
	Observation *metadataHTTPObservation `json:"observation,omitempty"`
	Coverage    metadataHTTPCoverage     `json:"coverage"`
}

// metadataHTTPSeriesRow is one complete series association row.
type metadataHTTPSeriesRow struct {
	Series      []metadataHTTPLabel     `json:"series"`
	Association metadataHTTPAssociation `json:"association"`
	Owner       *metadataHTTPOwner      `json:"owner,omitempty"`
	Versions    []metadataHTTPAssertion `json:"versions"`
}

// metadataHTTPDiffEndpoint is one before or after endpoint.
type metadataHTTPDiffEndpoint struct {
	State     string                 `json:"state"`
	Reason    string                 `json:"reason,omitempty"`
	Assertion *metadataHTTPAssertion `json:"assertion,omitempty"`
	Coverage  metadataHTTPCoverage   `json:"coverage"`
}

// metadataHTTPDiffField is one explicit typed before/after field state.
type metadataHTTPDiffField struct {
	Key    string             `json:"key"`
	Before metadataTypedValue `json:"before"`
	After  metadataTypedValue `json:"after"`
}

// metadataHTTPDiffRow is one bounded point owner-content diff.
type metadataHTTPDiffRow struct {
	Owner           metadataHTTPOwner        `json:"owner"`
	EffectiveFromMS string                   `json:"effective_from_ms"`
	Before          metadataHTTPDiffEndpoint `json:"before"`
	After           metadataHTTPDiffEndpoint `json:"after"`
	Fields          []metadataHTTPDiffField  `json:"fields"`
	Coverage        metadataHTTPCoverage     `json:"coverage"`
}

// metadataHTTPSourceAssertion preserves an independent source-attributed
// assertion without inventing a Source role schema.
type metadataHTTPSourceAssertion struct {
	Owner    metadataHTTPOwner       `json:"owner"`
	Versions []metadataHTTPAssertion `json:"versions"`
}

// metadataHTTPComponent is one original Resource component. Components are
// never merged and no canonical winner is selected.
type metadataHTTPComponent struct {
	Owner            metadataHTTPOwner             `json:"owner"`
	Schema           []metadataHTTPAssertion       `json:"schema"`
	Versions         []metadataHTTPAssertion       `json:"versions"`
	SourceAssertions []metadataHTTPSourceAssertion `json:"source_assertions"`
}

// metadataHTTPGroupRow is one combined logical group. Relations stay empty until
// authoritative Entity evidence is representable.
type metadataHTTPGroupRow struct {
	Series     []metadataHTTPLabel     `json:"series"`
	Relations  []map[string]any        `json:"relations"`
	Components []metadataHTTPComponent `json:"components"`
	Coverage   metadataHTTPCoverage    `json:"coverage"`
}

// metadataHTTPOwnerPage is one owner-history page.
type metadataHTTPOwnerPage struct {
	SnapshotID  string                 `json:"snapshot_id"`
	NextToken   *string                `json:"next_token"`
	Annotations []map[string]any       `json:"annotations"`
	Owners      []metadataHTTPOwnerRow `json:"owners"`
}

// metadataHTTPDiscoveryPage is one attribute-discovery page.
type metadataHTTPDiscoveryPage struct {
	SnapshotID  string                     `json:"snapshot_id"`
	NextToken   *string                    `json:"next_token"`
	Annotations []map[string]any           `json:"annotations"`
	Attributes  []metadataHTTPDiscoveryRow `json:"attributes"`
}

// metadataHTTPSeriesPage is one full or combined series page.
type metadataHTTPSeriesPage struct {
	SnapshotID  string                  `json:"snapshot_id"`
	NextToken   *string                 `json:"next_token"`
	Annotations []map[string]any        `json:"annotations"`
	Series      []metadataHTTPSeriesRow `json:"series"`
}

// metadataHTTPGroupPage is one combined-context page.
type metadataHTTPGroupPage struct {
	SnapshotID  string                 `json:"snapshot_id"`
	NextToken   *string                `json:"next_token"`
	Annotations []map[string]any       `json:"annotations"`
	Groups      []metadataHTTPGroupRow `json:"groups"`
}

// metadataHTTPDiffPage is one diff page.
type metadataHTTPDiffPage struct {
	SnapshotID  string                `json:"snapshot_id"`
	NextToken   *string               `json:"next_token"`
	Annotations []map[string]any      `json:"annotations"`
	Changes     []metadataHTTPDiffRow `json:"changes"`
}

// formatMetadataMilliseconds renders a signed millisecond timestamp as a
// decimal JSON string. Response fields ending in _ms never use JSON numbers.
func formatMetadataMilliseconds(value int64) string { return strconv.FormatInt(value, 10) }

// nullableMetadataMilliseconds renders an optional millisecond timestamp.
func nullableMetadataMilliseconds(value *int64) *string {
	if value == nil {
		return nil
	}
	rendered := formatMetadataMilliseconds(*value)
	return &rendered
}

// metadataHTTPIdentityFor renders the complete identity the incumbent stores for
// one owner. Absent-versus-present-empty fallback keys remain a recorded
// incumbent gap for Resource compatibility identity.
func metadataHTTPIdentityFor(owner OwnerKey) metadataHTTPIdentity {
	identity := metadataHTTPIdentity{Kind: owner.Kind.String()}
	switch owner.Kind {
	case MetadataMetric:
		if parsed, err := parser.NewParser(parser.Options{}).ParseMetric(owner.ID); err == nil {
			identity.Labels = metadataHTTPLabels(parsed)
			return identity
		}
		identity.Attributes = []metadataHTTPEntry{{
			Key: "series", Value: metadataPresentString(owner.ID),
		}}
	case MetadataResourceSchema:
		kind, id, ok := strings.Cut(owner.ID, "\x00")
		if ok {
			resourceKind := MetadataResource
			for candidate, name := range KindNames {
				if name == kind {
					resourceKind = MetadataKind(candidate)
					break
				}
			}
			resource := metadataHTTPOwnerFor(OwnerKey{Kind: resourceKind, ID: id})
			identity.Resource = &resource
			return identity
		}
		identity.Attributes = []metadataHTTPEntry{{
			Key: "resource", Value: metadataPresentString(owner.ID),
		}}
	default:
		attributes := metadataOwnerIdentity(owner)
		keys := slices.Sorted(maps.Keys(attributes))
		identity.Attributes = make([]metadataHTTPEntry, 0, len(keys))
		for _, key := range keys {
			identity.Attributes = append(identity.Attributes, metadataHTTPEntry{
				Key: key, Value: metadataPresentString(attributes[key]),
			})
		}
	}
	return identity
}

// metadataHTTPOwnerFor renders one complete owner identity.
func metadataHTTPOwnerFor(owner OwnerKey) metadataHTTPOwner {
	return metadataHTTPOwner{
		Kind: owner.Kind.String(), Identity: metadataHTTPIdentityFor(owner), Ref: owner.Ref(),
	}
}

// metadataHTTPLabels renders the complete ordered label set.
func metadataHTTPLabels(series labels.Labels) []metadataHTTPLabel {
	out := make([]metadataHTTPLabel, 0, series.Len())
	series.Range(func(label labels.Label) {
		out = append(out, metadataHTTPLabel{Name: label.Name, Value: label.Value})
	})
	return out
}

// metadataHTTPContentEntries returns exactly one state entry per materialized
// key, including absent for an explicitly projected key. Omission of an
// unrequested key only means it was not included.
func metadataHTTPContentEntries(attributes map[string]string, projection []string) []metadataHTTPEntry {
	if projection == nil {
		keys := slices.Sorted(maps.Keys(attributes))
		entries := make([]metadataHTTPEntry, 0, len(keys))
		for _, key := range keys {
			entries = append(entries, metadataHTTPEntry{
				Key: key, Value: metadataPresentString(attributes[key]),
			})
		}
		return entries
	}
	entries := make([]metadataHTTPEntry, 0, len(projection))
	for _, key := range projection {
		value, present := attributes[key]
		if present {
			entries = append(entries, metadataHTTPEntry{Key: key, Value: metadataPresentString(value)})
			continue
		}
		entries = append(entries, metadataHTTPEntry{Key: key, Value: metadataAbsentValue()})
	}
	return entries
}

// metadataHTTPContent is the stored complete-authoritative content assertion.
func metadataHTTPContentAssertion(attributes map[string]string, projection []string) metadataHTTPContent {
	return metadataHTTPContent{Scope: "complete_authoritative", Entries: metadataHTTPContentEntries(attributes, projection)}
}

// metadataHTTPOwnerCoverage describes retained owner-history evidence.
func metadataHTTPOwnerCoverage(history *metadataOwnerHistory) metadataHTTPCoverage {
	if history != nil && history.truncated && len(history.versions) > 0 {
		earliest := formatMetadataMilliseconds(history.versions[0].EffectiveFrom)
		return metadataHTTPCoverage{
			State: "truncated", Scope: "owner_history", EarliestRetainedMS: &earliest,
			Ranges:  []metadataHTTPRange{{FromMS: earliest, UntilMS: nil}},
			Reasons: []string{"history_truncated"},
		}
	}
	return metadataHTTPCoverage{
		State: "covered", Scope: "owner_history", Ranges: []metadataHTTPRange{}, Reasons: []string{},
	}
}

// metadataHTTPAssociationCoverage describes retained membership evidence.
func metadataHTTPAssociationCoverage() metadataHTTPCoverage {
	return metadataHTTPCoverage{
		State: "covered", Scope: "series_association", Ranges: []metadataHTTPRange{}, Reasons: []string{},
	}
}

// metadataHTTPSourceAttribution states that the incumbent records no trusted
// per-assertion provenance. Missing trusted roles are unknown, not invented.
func metadataHTTPSourceAttribution() map[string]any {
	return map[string]any{"state": "unknown", "reason": "provenance_not_recorded"}
}

// metadataHTTPAssertionFor renders one owner version.
func metadataHTTPAssertionFor(version Version, validUntil *int64, owners *metadataOwnerHistory, projection []string) metadataHTTPAssertion {
	first := formatMetadataMilliseconds(version.ObservedFrom)
	last := formatMetadataMilliseconds(version.ObservedUntil)
	return metadataHTTPAssertion{
		Content:  metadataHTTPContentAssertion(version.Content.Attributes, projection),
		Validity: metadataHTTPValidity{FromMS: formatMetadataMilliseconds(version.EffectiveFrom), UntilMS: nullableMetadataMilliseconds(validUntil)},
		Observation: metadataHTTPObservation{
			FirstMS: &first, LastMS: &last,
		},
		Coverage: metadataHTTPOwnerCoverage(owners),
		Source:   metadataHTTPSourceAttribution(),
	}
}

// metadataHTTPTruncatedAnnotation records one non-covered answer. Ordinary
// absent, cleared, unassociated, terminated and lossless-coalesced results need
// no uncertainty annotation.
func metadataHTTPTruncatedAnnotation(owner *OwnerKey, series labels.Labels, scope string, earliest int64) map[string]any {
	annotation := map[string]any{
		"category": "coverage", "key": "history_truncated", "axis": "coverage",
		"state": "truncated", "scope": scope, "reason": "history_truncated",
		"earliest_retained_ms": formatMetadataMilliseconds(earliest),
		"interval": map[string]any{
			"from_ms": formatMetadataMilliseconds(earliest), "until_ms": nil,
		},
	}
	if owner != nil {
		annotation["owner"] = metadataHTTPOwnerFor(*owner)
	}
	if series.Len() > 0 {
		annotation["series"] = metadataHTTPLabels(series)
	}
	return annotation
}

// requireNonNilAnnotations keeps every successful response explicit.
func requireNonNilAnnotations(annotations []map[string]any) []map[string]any {
	if annotations == nil {
		return []map[string]any{}
	}
	return annotations
}

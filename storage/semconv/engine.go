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

package semconv

import (
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/model/labels"
)

const (
	// maxSchemaExpansion caps each bounded resolver collection.
	maxSchemaExpansion = 256

	// maxSchemaExpansionWork bounds cumulative resolver work across collections.
	maxSchemaExpansionWork = maxSchemaExpansion * maxSchemaExpansion

	// maxSchemaExpansionKeyBytes bounds cumulative synthesized deduplication keys.
	maxSchemaExpansionKeyBytes = maxRegistryDecompressedBytes
)

var (
	errSchemaExpansion  = errors.New("semconv schema expansion limit exceeded")
	errMetricNameAnchor = errors.New("schema-aware query requires a non-empty equality matcher on __name__")
)

func schemaExpansionError(kind string) error {
	return fmt.Errorf("%w: %s would exceed %d", errSchemaExpansion, kind, maxSchemaExpansion)
}

func schemaExpansionLimitError(kind string, limit uint64) error {
	return fmt.Errorf("%w: %s would exceed %d", errSchemaExpansion, kind, limit)
}

type schemaExpansionLimits struct {
	work     uint64
	keyBytes uint64
}

func productionSchemaExpansionLimits() schemaExpansionLimits {
	return schemaExpansionLimits{
		work:     maxSchemaExpansionWork,
		keyBytes: maxSchemaExpansionKeyBytes,
	}
}

// schemaExpansionBudget is shared by one resolver invocation. It charges
// attempted work, including candidates later discarded as duplicates.
type schemaExpansionBudget struct {
	limits   schemaExpansionLimits
	work     uint64
	keyBytes uint64
}

func newSchemaExpansionBudget(limits schemaExpansionLimits) *schemaExpansionBudget {
	return &schemaExpansionBudget{limits: limits}
}

func (b *schemaExpansionBudget) reserveWork(n uint64) error {
	if b == nil || n == 0 {
		return nil
	}
	if b.work > b.limits.work || n > b.limits.work-b.work {
		return schemaExpansionLimitError("resolver work", b.limits.work)
	}
	b.work += n
	return nil
}

func (b *schemaExpansionBudget) reserveKeyBytes(n uint64) error {
	if b == nil || n == 0 {
		return nil
	}
	if b.keyBytes > b.limits.keyBytes || n > b.limits.keyBytes-b.keyBytes {
		return schemaExpansionLimitError("deduplication key bytes", b.limits.keyBytes)
	}
	b.keyBytes += n
	return nil
}

func (b *schemaExpansionBudget) remainingKeyBytes() uint64 {
	if b == nil {
		return ^uint64(0)
	}
	if b.keyBytes > b.limits.keyBytes {
		return 0
	}
	return b.limits.keyBytes - b.keyBytes
}

// registrySource provides the raw bytes of registry files addressed by their
// registry/<name> path. The embedded registry (embed.FS) satisfies it directly;
// an operator-provided registry is adapted to it via newRegistrySource.
type registrySource interface {
	ReadFile(name string) ([]byte, error)
}

type schemaEngine struct {
	registry registrySource
	limits   schemaExpansionLimits

	otelSchemaCache *staticCache[otelSchema]
	semconvCache    *staticCache[semconv]
}

func newSchemaEngine(registry registrySource) *schemaEngine {
	return &schemaEngine{
		registry:        registry,
		limits:          productionSchemaExpansionLimits(),
		otelSchemaCache: newStaticCache[otelSchema](),
		semconvCache:    newStaticCache[semconv](),
	}
}

func extractMetricName(matchers []*labels.Matcher) (string, error) {
	hasMetricMatcher := false
	for _, m := range matchers {
		if m.Name != model.MetricNameLabel {
			continue
		}
		hasMetricMatcher = true
		if m.Type == labels.MatchEqual && m.Value != "" {
			return m.Value, nil
		}
	}
	if hasMetricMatcher {
		return "", errMetricNameAnchor
	}
	return "", nil
}

// normalizeMetricMatchers evaluates every metric-name constraint against the
// exact name that anchors schema traversal. Compatible constraints are
// redundant once that equality is translated to each naming era.
func normalizeMetricMatchers(matchers []*labels.Matcher) (string, []*labels.Matcher, bool, error) {
	metricName, err := extractMetricName(matchers)
	if err != nil || metricName == "" {
		return metricName, matchers, true, err
	}

	metricMatchers := 0
	for _, matcher := range matchers {
		if matcher.Name != model.MetricNameLabel {
			continue
		}
		metricMatchers++
		if !matcher.Matches(metricName) {
			return metricName, matchers, false, nil
		}
	}
	if metricMatchers == 1 {
		return metricName, matchers, true, nil
	}

	out := make([]*labels.Matcher, 0, len(matchers)-metricMatchers+1)
	insertedMetric := false
	for _, matcher := range matchers {
		if matcher.Name != model.MetricNameLabel {
			out = append(out, matcher)
			continue
		}
		if !insertedMetric {
			out = append(out, labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, metricName))
			insertedMetric = true
		}
	}
	return metricName, out, true, nil
}

// findVersionAnchorIndex returns the index of the largest version <= targetVersion.
// The versions slice must be sorted in ascending semver order.
func findVersionAnchorIndex(versions []versionRenames, targetVersion string) int {
	anchorIdx, _ := findVersionAnchorIndexWithBudget(versions, targetVersion, nil)
	return anchorIdx
}

func findVersionAnchorIndexWithBudget(versions []versionRenames, targetVersion string, budget *schemaExpansionBudget) (int, error) {
	target := strings.TrimPrefix(targetVersion, "v")
	anchorIdx := 0
	for i, v := range versions {
		if err := budget.reserveWork(1); err != nil {
			return 0, err
		}
		if compareSemver(v.version, target) > 0 {
			break
		}
		anchorIdx = i
	}
	return anchorIdx, nil
}

// generateMatcherVariants generates matcher sets for schema version renames,
// anchored at the specified version.
// For each version, applies both metric and attribute renames together.
// Walks backward through versions <= version to find older name variants,
// and forward through versions > version to find newer name variants.
func generateMatcherVariants(version string, schema *otelSchema, matchers []*labels.Matcher) ([][]*labels.Matcher, error) {
	return generateMatcherVariantsWithBudget(version, schema, matchers, newSchemaExpansionBudget(productionSchemaExpansionLimits()))
}

func generateMatcherVariantsWithBudget(version string, schema *otelSchema, matchers []*labels.Matcher, budget *schemaExpansionBudget) ([][]*labels.Matcher, error) {
	if len(schema.versionRenames) == 0 {
		return [][]*labels.Matcher{matchers}, nil
	}

	key, err := matcherKeyWithBudget(matchers, budget)
	if err != nil {
		return nil, err
	}
	variants := [][]*labels.Matcher{matchers}
	seen := map[string]struct{}{key: {}}
	anchorIdx, err := findVersionAnchorIndexWithBudget(schema.versionRenames, version, budget)
	if err != nil {
		return nil, err
	}

	// Backward for older names.
	variants, err = walkVersionsWithBudget(schema.versionRenames[:anchorIdx+1], matchers, seen, variants, true, budget)
	if err != nil {
		return nil, err
	}

	// Forward for newer names.
	variants, err = walkVersionsWithBudget(schema.versionRenames[anchorIdx+1:], matchers, seen, variants, false, budget)
	if err != nil {
		return nil, err
	}

	return variants, nil
}

// walkVersions walks through versions applying renames, chaining results until no new variants.
// If reverse is false, walks oldest→newest; if true, walks newest→oldest.
func walkVersionsWithBudget(
	versions []versionRenames,
	matchers []*labels.Matcher,
	seen map[string]struct{},
	result [][]*labels.Matcher,
	reverse bool,
	budget *schemaExpansionBudget,
) ([][]*labels.Matcher, error) {
	current := matchers
	for {
		found := false
		var versionsIter iter.Seq2[int, versionRenames]
		if reverse {
			versionsIter = slices.Backward(versions)
		} else {
			versionsIter = slices.All(versions)
		}

		for _, v := range versionsIter {
			if err := budget.reserveWork(1); err != nil {
				return nil, err
			}
			transformed := applyVersionRenames(current, v)
			if transformed == nil {
				continue
			}

			key, err := matcherKeyWithBudget(transformed, budget)
			if err != nil {
				return nil, err
			}
			if _, exists := seen[key]; exists {
				continue
			}
			if len(result) >= maxSchemaExpansion {
				return nil, schemaExpansionError("matcher variants")
			}

			seen[key] = struct{}{}
			result = append(result, transformed)
			current = transformed
			found = true
			break
		}
		if !found {
			break
		}
	}
	return result, nil
}

// buildAttributeRenameMap returns a map from each historical or forward
// attribute alias to its name at anchorVersion, for the attributes in
// canonicalAttrs (the metric's attributes declared by the anchor semconv
// version). It is anchored and walked exactly like generateMatcherVariants
// (backward over versions <= anchor, forward over versions > anchor), so every
// alias a returned series can carry maps back to the queried version's name.
// Identity entries (alias == canonical) are omitted; it returns nil when the
// schema renames none of the attributes.
func buildAttributeRenameMap(anchorVersion string, schema *otelSchema, canonicalAttrs []string) (map[string]string, error) {
	return buildAttributeRenameMapWithBudget(anchorVersion, schema, canonicalAttrs, newSchemaExpansionBudget(productionSchemaExpansionLimits()))
}

func buildAttributeRenameMapWithBudget(anchorVersion string, schema *otelSchema, canonicalAttrs []string, budget *schemaExpansionBudget) (map[string]string, error) {
	if len(schema.versionRenames) == 0 || len(canonicalAttrs) == 0 {
		return nil, nil
	}
	if len(canonicalAttrs) > maxSchemaExpansion {
		return nil, schemaExpansionError("canonical attributes")
	}
	anchorIdx, err := findVersionAnchorIndexWithBudget(schema.versionRenames, anchorVersion, budget)
	if err != nil {
		return nil, err
	}
	backward := schema.versionRenames[:anchorIdx+1]
	forward := schema.versionRenames[anchorIdx+1:]

	out := map[string]string{}
	for _, canon := range canonicalAttrs {
		if err := budget.reserveWork(1); err != nil {
			return nil, err
		}
		if err := walkAttributeRenamesWithBudget(backward, canon, true, out, budget); err != nil {
			return nil, err
		}
		if err := walkAttributeRenamesWithBudget(forward, canon, false, out, budget); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// walkAttributeRenames threads canon through the versions' attribute renames,
// recording each distinct produced alias → canon in out. With reverse=true it
// walks newest→oldest, otherwise oldest→newest, chaining via a per-canon seen
// set — mirroring walkVersions so the attribute walk stays consistent with the
// matcher fan-out.
func walkAttributeRenamesWithBudget(versions []versionRenames, canon string, reverse bool, out map[string]string, budget *schemaExpansionBudget) error {
	current := canon
	seen := map[string]struct{}{canon: {}}
	for {
		found := false
		var versionsIter iter.Seq2[int, versionRenames]
		if reverse {
			versionsIter = slices.Backward(versions)
		} else {
			versionsIter = slices.All(versions)
		}

		for _, v := range versionsIter {
			if err := budget.reserveWork(1); err != nil {
				return err
			}
			next, ok := v.attributes[current]
			if !ok {
				continue
			}
			if _, exists := seen[next]; exists {
				continue
			}
			if len(seen) >= maxSchemaExpansion {
				return schemaExpansionError("attribute rename states")
			}
			if _, exists := out[next]; !exists && len(out) >= maxSchemaExpansion {
				return schemaExpansionError("attribute rename mappings")
			}
			seen[next] = struct{}{}
			out[next] = canon
			current = next
			found = true
			break
		}
		if !found {
			break
		}
	}
	return nil
}

// matcherKey generates a string key for a matcher set to detect duplicates.
func matcherKey(matchers []*labels.Matcher) string {
	key, _ := matcherKeyWithBudget(matchers, nil)
	return key
}

func matcherKeyWithBudget(matchers []*labels.Matcher, budget *schemaExpansionBudget) (string, error) {
	if err := budget.reserveWork(1); err != nil {
		return "", err
	}
	remaining := budget.remainingKeyBytes()
	var size uint64
	addSize := func(n uint64) error {
		if n > remaining-size {
			limit := remaining
			if budget != nil {
				limit = budget.limits.keyBytes
			}
			return schemaExpansionLimitError("deduplication key bytes", limit)
		}
		size += n
		return nil
	}
	for i, matcher := range matchers {
		if i > 0 {
			if err := addSize(1); err != nil {
				return "", err
			}
		}
		if err := addSize(uint64(len(matcher.Name))); err != nil {
			return "", err
		}
		if err := addSize(1); err != nil {
			return "", err
		}
		if err := addSize(uint64(len(matcher.Value))); err != nil {
			return "", err
		}
	}
	if err := budget.reserveKeyBytes(size); err != nil {
		return "", err
	}

	var b strings.Builder
	b.Grow(int(size))
	for i, m := range matchers {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteString(m.Name)
		b.WriteByte('=')
		b.WriteString(m.Value)
	}
	return b.String(), nil
}

// applyVersionRenames applies a version's metric and attribute renames to matchers.
// Returns nil if no renames apply. Uses lazy allocation to avoid allocating when no changes are made.
func applyVersionRenames(matchers []*labels.Matcher, renames versionRenames) []*labels.Matcher {
	var result []*labels.Matcher
	for i, m := range matchers {
		var newMatcher *labels.Matcher
		if m.Name == model.MetricNameLabel {
			if variant, ok := renames.metrics[m.Value]; ok {
				newMatcher = labels.MustNewMatcher(m.Type, m.Name, variant)
			}
		} else if variant, ok := renames.attributes[m.Name]; ok {
			newMatcher = labels.MustNewMatcher(m.Type, variant, m.Value)
		}
		if newMatcher != nil {
			if result == nil {
				// Lazy allocate and copy preceding unchanged matchers.
				result = make([]*labels.Matcher, len(matchers))
				copy(result[:i], matchers[:i])
			}
			result[i] = newMatcher
		} else if result != nil {
			result[i] = m
		}
	}

	return result
}

type matcherVariant struct {
	matchers []*labels.Matcher
	mapping  *labelMapping
}

type queryContext struct {
	warnings []string
}

// getSemconv returns the semconv parsed from url, fetching it via the
// embedded registry on a cache miss.
func (e *schemaEngine) getSemconv(url string) (semconv, error) {
	if sc, ok := e.semconvCache.get(url); ok {
		return sc, nil
	}
	sc, err := e.fetchSemconv(url)
	if err != nil {
		return semconv{}, err
	}
	e.semconvCache.set(url, sc)
	return sc, nil
}

// getOTelSchema returns the OTel schema parsed from url, fetching it via the
// embedded registry on a cache miss.
func (e *schemaEngine) getOTelSchema(url string) (otelSchema, error) {
	if s, ok := e.otelSchemaCache.get(url); ok {
		return s, nil
	}
	s, err := e.fetchOTelSchema(url)
	if err != nil {
		return otelSchema{}, err
	}
	e.otelSchemaCache.set(url, s)
	return s, nil
}

// findMatcherVariants returns all variants to match for a single schematized
// metric selection. semconvURL points to a semantic conventions file and is
// always required. In production schemaURL (an OTel schema file with versioned
// renames) is also always set, because classifyMatchers only triggers fan-out
// when both are present; the empty-schemaURL path exists only for the direct
// unit test. It returns one variant per schema-version rename of the metric,
// plus a label mapping for transforming results back to the requested version.
// The returned matchers do not include the reserved schema matchers. It returns
// an error if semconvURL or a non-empty equality __name__ matcher is not provided.
func (e *schemaEngine) findMatcherVariants(semconvURL, schemaURL string, originalMatchers []*labels.Matcher) ([]matcherVariant, queryContext, error) {
	if semconvURL == "" {
		return nil, queryContext{}, errors.New("semconvURL is required")
	}

	// Filter out the wrapper's reserved matchers.
	matchers := stripReservedLabels(originalMatchers)

	metricName, normalizedMatchers, satisfiable, err := normalizeMetricMatchers(matchers)
	if err != nil {
		return nil, queryContext{}, err
	}
	if metricName == "" {
		return nil, queryContext{}, errMetricNameAnchor
	}
	if !satisfiable {
		return []matcherVariant{{matchers: matchers}}, queryContext{}, nil
	}
	matchers = normalizedMatchers

	// Fetch semantic conventions for the anchor version (also validates the URL).
	sc, err := e.getSemconv(semconvURL)
	if err != nil {
		return nil, queryContext{}, err
	}

	// Generate schema-version rename variants. In production schemaURL is always
	// set (classifyMatchers gates fan-out on it); the empty case is reached only
	// by direct unit tests and falls through to the unmodified matchers.
	allVariants := [][]*labels.Matcher{matchers}
	var attrRenames map[string]string
	if schemaURL != "" {
		schema, err := e.getOTelSchema(schemaURL)
		if err != nil {
			return nil, queryContext{}, err
		}
		budget := newSchemaExpansionBudget(e.limits)
		allVariants, err = generateMatcherVariantsWithBudget(sc.version, &schema, matchers, budget)
		if err != nil {
			return nil, queryContext{}, err
		}
		// Map each historical attribute alias back to its anchor-version name so
		// results from older or newer eras merge under the queried version's
		// labels instead of splitting on the renamed attribute. Recomputed per
		// query on purpose: it is a pure function of the cached schema/semconv and
		// costs only a few map ops, far less than the fan-out it feeds.
		attrRenames, err = buildAttributeRenameMapWithBudget(sc.version, &schema, sc.attributesPerMetric[metricName], budget)
		if err != nil {
			return nil, queryContext{}, err
		}
	}

	mapping := buildLabelMapping(metricName, attrRenames)
	variants := make([]matcherVariant, 0, len(allVariants))
	for _, variant := range allVariants {
		variants = append(variants, matcherVariant{matchers: variant, mapping: mapping})
	}
	return variants, queryContext{}, nil
}

// transformSeries returns the series labels rewritten to the canonical OTel
// semantic convention names recorded in q.labelMapping. When no mapping
// applies, any stray __schema_url__ label is stripped and the labels are
// returned otherwise unchanged.
func (*schemaEngine) transformSeries(mapping *labelMapping, originalLabels labels.Labels) (labels.Labels, error) {
	if mapping != nil {
		return transformOTelSchemaLabels(originalLabels, mapping)
	}
	if originalLabels.Get(schemaURLLabel) == "" {
		return originalLabels, nil
	}
	builder := labels.NewBuilder(originalLabels)
	builder.Del(schemaURLLabel)
	return builder.Labels(), nil
}

// labelMapping rewrites a returned series' names to the queried semantic-
// conventions version: translatedMetric is the queried (anchor) metric name
// that every variant collapses to, and translatedLabels maps each historical
// attribute alias back to its anchor-version name.
type labelMapping struct {
	translatedLabels map[string]string // historical attribute alias → anchor name, e.g. "user" -> "tenant"
	translatedMetric string
}

// buildLabelMapping creates the mapping used to rewrite result labels back to
// the requested semantic-conventions version: the result metric name maps to
// the queried (anchor) name, and translatedLabels maps each historical
// attribute alias back to its anchor-version name (nil/empty when no attribute
// was renamed).
func buildLabelMapping(metricName string, translatedLabels map[string]string) *labelMapping {
	return &labelMapping{translatedMetric: metricName, translatedLabels: translatedLabels}
}

// aliasesOf returns name together with every historical alias that maps to it,
// i.e. the set of label names a returned series may carry for the canonical
// name. It is the inverse of translatedLabels and is used to fan LabelValues
// out across a renamed attribute's historical names. The metric name has no
// attribute aliases, so it is returned unchanged.
func (m *labelMapping) aliasesOf(name string) []string {
	aliases := []string{name}
	for alias, canonical := range m.translatedLabels {
		if canonical == name {
			aliases = append(aliases, alias)
		}
	}
	slices.Sort(aliases[1:])
	return aliases
}

// transformOTelSchemaLabels transforms series labels to the current semantic
// conventions version using the label mapping.
func transformOTelSchemaLabels(originalLabels labels.Labels, mapping *labelMapping) (labels.Labels, error) {
	type mappedLabel struct {
		source string
		value  string
	}

	builder := labels.NewScratchBuilder(originalLabels.Len())
	mapped := make(map[string]mappedLabel, originalLabels.Len())
	var transformErr error
	originalLabels.Range(func(l labels.Label) {
		if transformErr != nil {
			return
		}

		name := l.Name
		value := l.Value
		switch l.Name {
		case semconvURLLabel, schemaURLLabel:
			return
		case model.MetricNameLabel:
			value = mapping.translatedMetric
		default:
			if canonical, ok := mapping.translatedLabels[l.Name]; ok {
				name = canonical
			}
		}

		if existing, ok := mapped[name]; ok {
			if existing.value != value {
				transformErr = fmt.Errorf("semconv label transformation maps %q and %q to %q with conflicting values", existing.source, l.Name, name)
			}
			return
		}
		mapped[name] = mappedLabel{source: l.Name, value: value}
		builder.Add(name, value)
	})
	if transformErr != nil {
		return labels.EmptyLabels(), transformErr
	}
	builder.Sort()
	return builder.Labels(), nil
}

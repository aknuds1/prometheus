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

package promql

import (
	"fmt"

	"github.com/prometheus/common/model"
	"github.com/prometheus/otlptranslator"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
)

// resourceInfoQuery carries native-only matchers separately from the original
// metric-join matchers. The same host_name matcher may have to become host.name
// for Resource content but remain host_name for a conventional target_info row.
type resourceInfoQuery struct {
	querier    storage.ResourceQuerier
	membership storage.ResourceMembershipQuerier
	matchers   map[string][]*labels.Matcher
	toOutput   map[string]string
}

// selectsNativeResourceInfo reports whether the info selector is exclusively
// target_info. Selectors that also name conventional info metrics keep the
// metric-join path until native/metric hybrid selection is specified.
func selectsNativeResourceInfo(args parser.Expressions) bool {
	if len(args) == 1 {
		return true
	}

	var nameMatchers []*labels.Matcher
	for _, matcher := range args[1].(*parser.VectorSelector).LabelMatchers {
		if matcher.Name == model.MetricNameLabel {
			nameMatchers = append(nameMatchers, matcher)
		}
	}
	if len(nameMatchers) == 0 {
		return true
	}
	return len(nameMatchers) == 1 && nameMatchers[0].Type == labels.MatchEqual && nameMatchers[0].Value == targetInfo
}

// newResourceInfoQuery maps output label matchers back to original Resource
// attribute names. Nil naming configuration preserves original names.
func newResourceInfoQuery(args parser.Expressions, querier storage.ResourceQuerier, config *LabelNamerConfig) (*resourceInfoQuery, error) {
	if querier == nil {
		return nil, nil
	}

	query := &resourceInfoQuery{
		querier:  querier,
		matchers: map[string][]*labels.Matcher{},
	}
	query.membership, _ = querier.(storage.ResourceMembershipQuerier)
	var toOriginal map[string]string
	if config != nil {
		namer := &otlptranslator.LabelNamer{
			UTF8Allowed:                 config.UTF8Allowed,
			UnderscoreLabelSanitization: config.UnderscoreLabelSanitization, //nolint:staticcheck
			PreserveMultipleUnderscores: config.PreserveMultipleUnderscores,
		}
		query.toOutput = map[string]string{}
		toOriginal = map[string]string{}
		var collision error
		if err := querier.IterUniqueAttributeNames(func(original string) {
			output, err := namer.Build(original)
			if err != nil {
				// An unrepresentable name cannot be selected or emitted as the
				// configured label kind, so leave it out of both mappings.
				return
			}
			if previous, exists := toOriginal[output]; exists && previous != original {
				// Iteration order is deterministic. Retaining the error instead of
				// picking one source attribute prevents ambiguous enrichment.
				if collision == nil {
					collision = fmt.Errorf("resource attributes %q and %q both map to Prometheus label %q", previous, original, output)
				}
				return
			}
			query.toOutput[original] = output
			toOriginal[output] = original
		}); err != nil {
			return nil, err
		}
		if collision != nil {
			return nil, collision
		}
	}

	if len(args) > 1 {
		for _, matcher := range args[1].(*parser.VectorSelector).LabelMatchers {
			if matcher.Name == model.MetricNameLabel {
				continue
			}
			name := matcher.Name
			if original, exists := toOriginal[name]; exists {
				name = original
			}
			query.matchers[name] = append(query.matchers[name], matcher)
		}
	}
	return query, nil
}

// enrichSampleWithResource applies matchers only to Resource attributes. In
// particular, a same-named label on the base metric cannot satisfy a Resource
// matcher when the effective Resource version does not contain the attribute.
func enrichSampleWithResource(
	sample Sample,
	attributes map[string]string,
	dataLabelMatchers map[string][]*labels.Matcher,
	toOutput map[string]string,
	enh *EvalNodeHelper,
) (Sample, bool) {
	for name, matchers := range dataLabelMatchers {
		value := attributes[name]
		for _, matcher := range matchers {
			if !matcher.Matches(value) {
				return Sample{}, false
			}
		}
	}

	baseLabels := sample.Metric.Map()
	enh.resetBuilder(sample.Metric)
	for name, value := range attributes {
		output := name
		if toOutput != nil {
			var exists bool
			output, exists = toOutput[name]
			if !exists {
				continue
			}
		}

		// Native metadata enrichment never overwrites the series' identity or
		// another label that the sample already has, under either name.
		if _, exists := baseLabels[name]; exists {
			continue
		}
		if _, exists := baseLabels[output]; exists {
			continue
		}
		if len(dataLabelMatchers) > 0 {
			if _, selected := dataLabelMatchers[name]; !selected {
				continue
			}
		}
		enh.lb.Set(output, value)
	}

	return Sample{
		Metric:   enh.lb.Labels(),
		F:        sample.F,
		H:        sample.H,
		DropName: sample.DropName,
	}, true
}

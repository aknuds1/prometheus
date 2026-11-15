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
	"cmp"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
)

// This file is the semantic oracle for the identity-reference delta fixture in
// otap_identity_reference_test.go. Every expectation is derived from the
// fixture's logical inputs - the Resource attributes a row states, the Scope it
// states, the metric name and the samples it carries - and never from the
// decoder's output. The results are read back through the exported API only:
// SelectSeries, MetadataHistory, QueryMetadata, QueryMetadataSeries,
// MetadataAtLabels and PromQL info().

// identityIdentifyingAttributes are the Resource attributes the fixture treats as
// Resource identity. They are the owner key and never Resource content.
var identityIdentifyingAttributes = [...]string{"service.name", "service.namespace", "service.instance.id"}

// identityInfoOutput maps the fixture's descriptive Resource attributes to the
// Prometheus label names info() emits for them.
var identityInfoOutput = map[string]string{
	"host.name":        "host_name",
	"k8s.cluster.name": "k8s_cluster_name",
	"host.rack":        "host_rack",
}

// identitySample is one stored sample of an expected series.
type identitySample struct {
	timestamp int64
	value     float64
}

// identityExpectedSeries is one series the fixture must store, with the complete
// labels it must carry and the occurrence that owns it.
type identityExpectedSeries struct {
	labels     labels.Labels
	samples    []identitySample
	output     string
	metricType string
	resource   identityResource
	scope      identityScope
}

func identitySeries(output, metricType string, resource identityResource, scope identityScope, probe string, samples ...identitySample) identityExpectedSeries {
	return identityExpectedSeries{
		labels:     identitySeriesLabels(output, metricType, resource, scope, probe),
		samples:    samples,
		output:     output,
		metricType: metricType,
		resource:   resource,
		scope:      scope,
	}
}

// identitySeriesLabels states the complete labels one stored series must carry.
// The Resource reference is derived from the Resource identity the row states,
// which is what keeps two Resources with one job/instance projection apart.
func identitySeriesLabels(output, metricType string, resource identityResource, scope identityScope, probe string) labels.Labels {
	builder := labels.NewBuilder(labels.EmptyLabels())
	builder.Set(labels.MetricName, output)
	builder.Set(OTELResourceLabel, ResourceOwner(resource.attributes).Ref())
	builder.Set("__type__", metricType)
	builder.Set("instance", resource.attributes["service.instance.id"])
	builder.Set("job", identityJob(resource.attributes))
	builder.Set("k8s_cluster_name", resource.attributes["k8s.cluster.name"])
	builder.Set("otel_scope_name", scope.name)
	builder.Set("otel_scope_node", scope.attributes["node"])
	builder.Set("otel_scope_role", scope.attributes["role"])
	builder.Set("otel_scope_schema_url", scope.schemaURL)
	builder.Set("otel_scope_version", scope.version)
	builder.Set("probe", probe)
	return builder.Labels()
}

// identityJob is the job label a Resource identity projects: namespace/name when
// both are stated, otherwise the name alone.
func identityJob(attributes map[string]string) string {
	name := attributes["service.name"]
	if namespace := attributes["service.namespace"]; namespace != "" && name != "" {
		return namespace + "/" + name
	}
	return name
}

// identityDescriptive is the Resource content the fixture states: every attribute
// that is not part of the Resource identity.
func identityDescriptive(resource identityResource) map[string]string {
	content := map[string]string{}
	for name, value := range resource.attributes {
		if slices.Contains(identityIdentifyingAttributes[:], name) {
			continue
		}
		content[name] = value
	}
	return content
}

// identityExpectedSeriesSet is the complete set of series the fixture must store.
// Series sharing a metric name are told apart by their probe marker, and the two
// beta blocks differ only in the Resource they belong to.
func identityExpectedSeriesSet() []identityExpectedSeries {
	return []identityExpectedSeries{
		identitySeries("native_identity_alpha", "gauge", identityResourceAlpha, identityScopeClient, "/alpha-p1",
			identitySample{timestamp: 1000, value: 11}),
		identitySeries("native_identity_alpha", "gauge", identityResourceAlpha, identityScopeClient, "/alpha-shared",
			identitySample{timestamp: 2000, value: 12}, identitySample{timestamp: 3000, value: 13}),
		identitySeries("native_identity_alpha_sum", "counter", identityResourceAlpha, identityScopeClient, "/alpha-sum",
			identitySample{timestamp: 4000, value: 14}),
		identitySeries("native_identity_beta", "gauge", identityResourceBeta, identityScopeServer, "/beta-p1",
			identitySample{timestamp: 5000, value: 21}),
		identitySeries("native_identity_beta", "gauge", identityResourceBeta, identityScopeServer, "/beta-shared",
			identitySample{timestamp: 6000, value: 22}, identitySample{timestamp: 7000, value: 23}),
		identitySeries("native_identity_beta_sum", "counter", identityResourceBeta, identityScopeServer, "/beta-sum",
			identitySample{timestamp: 8000, value: 24}),
		identitySeries("native_identity_beta_scope2", "gauge", identityResourceBeta, identityScopeSecondary, "/beta-scope2",
			identitySample{timestamp: 9000, value: 25}),
		identitySeries("native_identity_beta", "gauge", identityResourceCollision, identityScopeServer, "/beta-p1",
			identitySample{timestamp: 10000, value: 41}),
		identitySeries("native_identity_beta", "gauge", identityResourceCollision, identityScopeServer, "/beta-shared",
			identitySample{timestamp: 11000, value: 42}, identitySample{timestamp: 12000, value: 43}),
		identitySeries("native_identity_gamma_sum", "counter", identityResourceGamma, identityScopeOther, "/gamma-sum-p1",
			identitySample{timestamp: 13000, value: 31}),
		identitySeries("native_identity_gamma_sum", "counter", identityResourceGamma, identityScopeOther, "/gamma-sum-shared",
			identitySample{timestamp: 14000, value: 32}),
		identitySeries("native_identity_gamma_gauge", "gauge", identityResourceGamma, identityScopeClient, "/gamma-gauge",
			identitySample{timestamp: 15000, value: 33}),
		identitySeries("native_identity_epsilon", "gauge", identityResourceDelta, identityScopeLate, "/epsilon-shared",
			identitySample{timestamp: 16000, value: 51}, identitySample{timestamp: 17000, value: 52}),
	}
}

// identityOwner is the owner of one series for one metadata kind.
func identityOwner(kind MetadataKind, series identityExpectedSeries) OwnerKey {
	switch kind {
	case MetadataResource:
		return ResourceOwner(series.resource.attributes)
	case MetadataResourceSchema:
		return ResourceSchemaOwner(ResourceOwner(series.resource.attributes))
	case MetadataScope:
		return ScopeOwner(series.scope.name, series.scope.version)
	case MetadataSource:
		return SourceOwner("otap")
	case MetadataMetric:
		return MetricOwner(series.labels)
	default:
		panic("identity fixture states no scrape or unknown owner")
	}
}

// identityOwnerContent is the content a fixture occurrence states for a kind.
func identityOwnerContent(kind MetadataKind, series identityExpectedSeries) Content {
	switch kind {
	case MetadataResource:
		return Content{Attributes: identityDescriptive(series.resource)}
	case MetadataResourceSchema:
		return Content{Attributes: map[string]string{"schema_url": series.resource.schemaURL}}
	case MetadataScope:
		return Content{Attributes: map[string]string{"schema_url": series.scope.schemaURL}}
	case MetadataSource:
		return Content{Attributes: map[string]string{"transport": "otap"}}
	case MetadataMetric:
		return Content{Attributes: map[string]string{
			"help": identityHelp, "type": series.metricType, "unit": "",
		}}
	default:
		panic("identity fixture states no scrape or unknown owner")
	}
}

// identityMetadataKinds is the set of kinds the native sink states, in the order
// the tests report them.
var identityMetadataKinds = [...]MetadataKind{
	MetadataResource, MetadataResourceSchema, MetadataScope, MetadataSource, MetadataMetric,
}

// identityExpectedOwners states, for every owner the fixture must produce, the
// complete ordered version history it must keep: a version opens at the first
// observation of a content and every later observation of the same content
// extends that version's observation bound instead of adding a version.
func identityExpectedOwners(series []identityExpectedSeries) map[OwnerKey][]Version {
	type observation struct {
		owner   OwnerKey
		content Content
		at      int64
	}
	var observations []observation
	for _, expected := range series {
		for _, kind := range identityMetadataKinds {
			for _, sample := range expected.samples {
				observations = append(observations, observation{
					owner: identityOwner(kind, expected), content: identityOwnerContent(kind, expected), at: sample.timestamp,
				})
			}
		}
	}
	// The fixture states one owner's observations in time order, so ordering the
	// assertions by time states the required version order unambiguously.
	slices.SortStableFunc(observations, func(a, b observation) int { return cmp.Compare(a.at, b.at) })

	histories := map[OwnerKey][]Version{}
	for _, current := range observations {
		versions := histories[current.owner]
		if len(versions) > 0 && versions[len(versions)-1].Content.Canonical() == current.content.Canonical() {
			versions[len(versions)-1].ObservedUntil = max(versions[len(versions)-1].ObservedUntil, current.at)
			continue
		}
		histories[current.owner] = append(versions, Version{
			Content: current.content, EffectiveFrom: current.at, ObservedFrom: current.at, ObservedUntil: current.at,
		})
	}
	return histories
}

// identityOwnerIdentities states, for every owner the fixture produces, the
// identity a metadata query reports for it.
func identityOwnerIdentities(series []identityExpectedSeries) map[OwnerKey]map[string]string {
	identities := map[OwnerKey]map[string]string{}
	for _, expected := range series {
		resourceIdentity := map[string]string{}
		for _, name := range identityIdentifyingAttributes {
			resourceIdentity[name] = expected.resource.attributes[name]
		}
		for _, kind := range identityMetadataKinds {
			owner := identityOwner(kind, expected)
			switch kind {
			case MetadataResource:
				identities[owner] = resourceIdentity
			case MetadataScope:
				identities[owner] = map[string]string{"name": expected.scope.name, "version": expected.scope.version}
			case MetadataResourceSchema:
				identities[owner] = map[string]string{"resource": owner.ID}
			case MetadataMetric:
				identities[owner] = map[string]string{"series": owner.ID}
			case MetadataSource:
				identities[owner] = map[string]string{"source": owner.ID}
			}
		}
	}
	return identities
}

// identityValueView renders one version the way a metadata query reports it.
func identityValueView(versions []Version, i int) MetadataValueView {
	view := MetadataValueView{
		Descriptive:   versions[i].Content.Clone().Attributes,
		EffectiveFrom: versions[i].EffectiveFrom,
		ObservedFrom:  versions[i].ObservedFrom,
		ObservedUntil: versions[i].ObservedUntil,
	}
	if i+1 < len(versions) {
		next := versions[i+1].EffectiveFrom
		view.ValidUntil = &next
	}
	return view
}

// identityOverlaps reports whether a version is valid inside a membership that
// starts at membershipStart, within a query covering the whole fixture.
func identityOverlaps(versions []Version, i int, membershipStart int64) bool {
	lower := max(versions[i].EffectiveFrom, membershipStart)
	if lower > identityEnd {
		return false
	}
	if i+1 >= len(versions) {
		return true
	}
	next := versions[i+1].EffectiveFrom
	if next > identityEnd {
		return true
	}
	return lower < next
}

// identityExpectedMemberships states every membership the fixture must keep for
// one kind: one row per series, owned by the occurrence it states, effective from
// its first sample and observed through its last.
func identityExpectedMemberships(
	series []identityExpectedSeries,
	histories map[OwnerKey][]Version,
	identities map[OwnerKey]map[string]string,
	kind MetadataKind,
) []MetadataSeriesResult {
	results := make([]MetadataSeriesResult, 0, len(series))
	for _, expected := range series {
		owner := identityOwner(kind, expected)
		versions := histories[owner]
		start := expected.samples[0].timestamp
		observedUntil := expected.samples[0].timestamp
		for _, sample := range expected.samples[1:] {
			start = min(start, sample.timestamp)
			observedUntil = max(observedUntil, sample.timestamp)
		}
		var views []MetadataValueView
		for i := range versions {
			if identityOverlaps(versions, i, start) {
				views = append(views, identityValueView(versions, i))
			}
		}
		results = append(results, MetadataSeriesResult{
			Labels: expected.labels.Copy(), Owner: owner.Ref(), Kind: owner.Kind.String(),
			OwnerIdentifying: identities[owner], MembershipStart: start,
			MembershipObservedTo: observedUntil, Versions: views,
		})
	}
	slices.SortFunc(results, func(a, b MetadataSeriesResult) int { return labels.Compare(a.Labels, b.Labels) })
	return results
}

// identityExpectedOwnerPage states the owner page one kind must return for the
// whole fixture period.
func identityExpectedOwnerPage(
	histories map[OwnerKey][]Version,
	identities map[OwnerKey]map[string]string,
	kind MetadataKind,
) MetadataOwnerPage {
	owners := make([]OwnerKey, 0, len(histories))
	for owner := range histories {
		if owner.Kind == kind {
			owners = append(owners, owner)
		}
	}
	// Owner pages are ordered by owner identity, not by the opaque reference.
	slices.SortFunc(owners, func(a, b OwnerKey) int { return cmp.Compare(a.String(), b.String()) })
	var views []MetadataOwnerView
	for _, owner := range owners {
		versions := histories[owner]
		view := MetadataOwnerView{
			ID: owner.Ref(), Kind: kind.String(), Identifying: identities[owner],
		}
		for i := range versions {
			view.Versions = append(view.Versions, identityValueView(versions, i))
		}
		views = append(views, view)
	}
	return MetadataOwnerPage{Owners: views}
}

// identityExpectedOwnerHistories states the history of every fixture owner.
func identityExpectedOwnerHistories(histories map[OwnerKey][]Version) map[OwnerKey]OwnerHistory {
	out := make(map[OwnerKey]OwnerHistory, len(histories))
	for owner, versions := range histories {
		out[owner] = OwnerHistory{Owner: owner, Versions: versions}
	}
	return out
}

// identityInfoEnrichment is the label set info() must report for one series: the
// stored labels plus every Resource attribute that is not already a label under
// its own or its output name. When only is non-empty, only those attributes are
// emitted, which is the attribute-selector form.
func identityInfoEnrichment(ls labels.Labels, resource identityResource, only ...string) labels.Labels {
	builder := labels.NewBuilder(ls)
	for name, output := range identityInfoOutput {
		if len(only) > 0 && !slices.Contains(only, name) {
			continue
		}
		value := resource.attributes[name]
		if value == "" {
			continue
		}
		if ls.Get(name) != "" || ls.Get(output) != "" {
			continue
		}
		builder.Set(output, value)
	}
	return builder.Labels()
}

// identityInfoQuery is one info() form the fixture asserts.
type identityInfoQuery struct {
	name string
	expr string
	at   int64
}

var identityInfoQueries = []identityInfoQuery{
	{name: "alpha", expr: "info(native_identity_alpha)", at: 2500},
	{name: "beta_before_collision", expr: "info(native_identity_beta)", at: 6500},
	{name: "beta_with_collision", expr: "info(native_identity_beta)", at: 12500},
	{name: "beta_collision_selector", expr: `info(native_identity_beta, {host_name="host-2-shadow"})`, at: 12500},
	{name: "gamma_scope", expr: "info(native_identity_gamma_sum)", at: 13500},
	{name: "epsilon", expr: "info(native_identity_epsilon)", at: 16500},
}

// identityFixtureSeries selects the expected series of one metric name and one
// Resource occurrence. The probe marker alone is not enough: the collision pair
// deliberately shares metric name, probes and Scope, and differs only in the
// Resource it belongs to.
func identityFixtureSeries(output, resourceKey string, probes ...string) []identityExpectedSeries {
	var out []identityExpectedSeries
	for _, expected := range identityExpectedSeriesSet() {
		if expected.output != output || expected.resource.key != resourceKey || !slices.Contains(probes, expected.labels.Get("probe")) {
			continue
		}
		out = append(out, expected)
	}
	return out
}

// identityEnrichedLabels is the info() label set of the given series.
func identityEnrichedLabels(resourceAttributes []string, expected ...identityExpectedSeries) []labels.Labels {
	out := make([]labels.Labels, 0, len(expected))
	for _, item := range expected {
		out = append(out, identityInfoEnrichment(item.labels, item.resource, resourceAttributes...))
	}
	slices.SortFunc(out, labels.Compare)
	return out
}

// identityExpectedInfo states the complete info() result of every query.
func identityExpectedInfo() map[string][]labels.Labels {
	beforeCollision := identityFixtureSeries("native_identity_beta", "beta", "/beta-p1", "/beta-shared")
	withCollision := append(
		identityFixtureSeries("native_identity_beta", "beta", "/beta-p1", "/beta-shared"),
		identityFixtureSeries("native_identity_beta", "collision", "/beta-p1", "/beta-shared")...,
	)
	return map[string][]labels.Labels{
		"alpha": identityEnrichedLabels(nil,
			identityFixtureSeries("native_identity_alpha", "alpha", "/alpha-p1", "/alpha-shared")...),
		// Before the collision Resource reports any sample its series are absent.
		"beta_before_collision": identityEnrichedLabels(nil, beforeCollision...),
		// Both beta Resources project to one job and one instance. info() must
		// enrich each series from its own Resource, so the two contexts stay
		// distinct instead of merging or borrowing each other's content.
		"beta_with_collision": identityEnrichedLabels(nil, withCollision...),
		"beta_collision_selector": identityEnrichedLabels([]string{"host.name"},
			identityFixtureSeries("native_identity_beta", "collision", "/beta-p1", "/beta-shared")...),
		"gamma_scope": identityEnrichedLabels(nil,
			identityFixtureSeries("native_identity_gamma_sum", "gamma", "/gamma-sum-p1")...),
		"epsilon": identityEnrichedLabels(nil,
			identityFixtureSeries("native_identity_epsilon", "delta", "/epsilon-shared")...),
	}
}

// identitySnapshot is every public result of one observed phase, so a recovered
// phase can be compared with the live head as a whole.
type identitySnapshot struct {
	Series      []string
	Samples     map[string][]int64
	Values      map[string][]float64
	Kinds       map[string][]SampleKind
	Starts      map[string][]int64
	Histories   map[OwnerKey]OwnerHistory
	Missing     []OwnerKey
	OwnerPages  map[MetadataKind]MetadataOwnerPage
	Memberships map[MetadataKind][]MetadataSeriesResult
	At          []string
	Info        map[string][]string
}

// identityObserve captures every public result of the fixture.
func identityObserve(t *testing.T, s *Storage, engine promql.QueryEngine, histories map[OwnerKey][]Version) identitySnapshot {
	t.Helper()
	out := identitySnapshot{
		Samples:     map[string][]int64{},
		Values:      map[string][]float64{},
		Kinds:       map[string][]SampleKind{},
		Starts:      map[string][]int64{},
		Histories:   map[OwnerKey]OwnerHistory{},
		OwnerPages:  map[MetadataKind]MetadataOwnerPage{},
		Memberships: map[MetadataKind][]MetadataSeriesResult{},
		Info:        map[string][]string{},
	}

	all, err := s.SelectSeries(t.Context(), 0, identityEnd, nil, 0)
	require.NoError(t, err)
	for _, item := range all {
		key := item.Labels.String()
		out.Series = append(out.Series, key)
		out.Samples[key] = item.Timestamps
		out.Values[key] = item.Values
		out.Kinds[key] = item.Kinds
		// A column with no stated start timestamp is a zero column in the live head
		// and nil in a recovered block. Both state "unknown", so the snapshot keeps
		// only the start timestamps the fixture actually states.
		var starts []int64
		for _, start := range item.StartTimestamps {
			if start != 0 {
				starts = append(starts, start)
			}
		}
		out.Starts[key] = starts
	}
	slices.Sort(out.Series)

	for _, owner := range slices.SortedFunc(maps.Keys(histories), func(a, b OwnerKey) int {
		return cmp.Compare(a.String(), b.String())
	}) {
		history, ok := s.MetadataHistory(owner)
		if !ok {
			out.Missing = append(out.Missing, owner)
			continue
		}
		out.Histories[owner] = history
	}

	for _, kind := range identityMetadataKinds {
		page, err := s.QueryMetadata(t.Context(), MetadataQuery{Kind: kind, Start: 0, End: identityEnd})
		require.NoError(t, err)
		out.OwnerPages[kind] = page

		memberships, err := s.QueryMetadataSeries(t.Context(), MetadataSeriesQuery{Kind: kind, Start: 0, End: identityEnd})
		require.NoError(t, err)
		out.Memberships[kind] = memberships.Series
	}

	for _, expected := range identityExpectedSeriesSet() {
		instants := []int64{0, expected.samples[0].timestamp - 1}
		for _, sample := range expected.samples {
			instants = append(instants, sample.timestamp)
		}
		last := expected.samples[len(expected.samples)-1].timestamp
		instants = append(instants, last+1)
		for _, kind := range identityMetadataKinds {
			for _, at := range instants {
				content, found, truncated := s.MetadataAtLabels(expected.labels, kind, at)
				require.False(t, truncated)
				out.At = append(out.At, fmt.Sprintf("%s %s %d %t %s",
					expected.labels.Get("probe"), kind.String(), at, found, content.Canonical()))
			}
		}
	}

	for _, query := range identityInfoQueries {
		var reported []string
		for _, item := range identityInfoLabels(t, engine, s, query.expr, query.at) {
			reported = append(reported, item.String())
		}
		slices.Sort(reported)
		out.Info[query.name] = reported
	}
	return out
}

// identityInfoLabels evaluates an info() expression at one instant.
func identityInfoLabels(t *testing.T, engine promql.QueryEngine, s *Storage, expr string, at int64) []labels.Labels {
	t.Helper()
	query, err := engine.NewInstantQuery(t.Context(), s, nil, expr, time.UnixMilli(at))
	require.NoError(t, err)
	defer query.Close()
	result := query.Exec(t.Context())
	require.NoError(t, result.Err)
	vector, ok := result.Value.(promql.Vector)
	require.True(t, ok)
	out := make([]labels.Labels, 0, len(vector))
	for _, sample := range vector {
		out = append(out, sample.Metric.Copy())
	}
	return out
}

// identityEngine is the PromQL engine the fixture's info() forms need, with the
// same label naming the native sink applies.
func identityEngine(t *testing.T) promql.QueryEngine {
	t.Helper()
	return promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
		MaxSamples: promqltest.DefaultMaxSamplesPerQuery, Timeout: time.Minute, LookbackDelta: 5 * time.Minute,
		LabelNamerConfig: &promql.LabelNamerConfig{UnderscoreLabelSanitization: true, PreserveMultipleUnderscores: true},
		Parser:           parser.NewParser(promqltest.TestParserOpts),
	})
}

// identityStorage opens a store of the fixture. A flush threshold it cannot reach
// keeps block publication an explicit test step.
func identityStorage(t *testing.T, dir string, disableWAL bool) *Storage {
	t.Helper()
	s, err := NewStorage(dir, StorageOptions{FlushSamples: 1 << 20, DisableWAL: disableWAL})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

// identityReopen reopens the fixture directory, which recovers published blocks
// and then replays the WAL.
func identityReopen(t *testing.T, dir string) *Storage {
	t.Helper()
	reopened, err := NewStorage(dir, StorageOptions{FlushSamples: 1 << 20})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	return reopened
}

// identityIngest appends the fixture through the real native OTAP sink.
func identityIngest(t *testing.T, s *Storage) {
	t.Helper()
	records := identityRecords(t)
	require.Empty(t, otapUnsupportedPayloads(records), "the fixture must carry no unsupported payload row")
	sink := NewOTAPSink(s, slog.New(slog.DiscardHandler), identitySettings)
	require.NoError(t, sink.ConsumeRecords(t.Context(), records))
}

// requireIdentityState asserts the complete public state of the fixture.
func requireIdentityState(t *testing.T, s *Storage, engine promql.QueryEngine, phase string) identitySnapshot {
	t.Helper()
	series := identityExpectedSeriesSet()
	histories := identityExpectedOwners(series)
	identities := identityOwnerIdentities(series)
	observed := identityObserve(t, s, engine, histories)

	t.Run("samples", func(t *testing.T) {
		expectedSeries := make([]string, 0, len(series))
		for _, expected := range series {
			key := expected.labels.String()
			expectedSeries = append(expectedSeries, key)
			require.Equal(t, identityTimestamps(expected), observed.Samples[key], "%s: timestamps of %s", phase, key)
			require.Equal(t, identityValues(expected), observed.Values[key], "%s: values of %s", phase, key)
			require.Equal(t, identityKinds(expected), observed.Kinds[key], "%s: sample kinds of %s", phase, key)
			require.Equal(t, identityStartTimestamps(expected), observed.Starts[key], "%s: stated start timestamps of %s", phase, key)
		}
		slices.Sort(expectedSeries)
		require.Equal(t, expectedSeries, observed.Series, "%s: the exact stored series set", phase)
	})

	t.Run("owner_histories", func(t *testing.T) {
		require.Empty(t, observed.Missing, "%s: every owner the fixture states must exist", phase)
		require.Equal(t, identityExpectedOwnerHistories(histories), observed.Histories, "%s: owner histories", phase)
	})

	t.Run("owner_pages", func(t *testing.T) {
		for _, kind := range identityMetadataKinds {
			require.Equal(t, identityExpectedOwnerPage(histories, identities, kind), observed.OwnerPages[kind],
				"%s: %s owner page", phase, kind)
		}
	})

	t.Run("memberships", func(t *testing.T) {
		for _, kind := range identityMetadataKinds {
			require.Equal(t, identityExpectedMemberships(series, histories, identities, kind), observed.Memberships[kind],
				"%s: %s memberships", phase, kind)
		}
	})

	t.Run("metadata_at_labels", func(t *testing.T) {
		require.Equal(t, identityExpectedAt(series, histories), observed.At, "%s: MetadataAtLabels", phase)
	})

	t.Run("info", func(t *testing.T) {
		expected := map[string][]string{}
		for name, labelSets := range identityExpectedInfo() {
			rendered := make([]string, 0, len(labelSets))
			for _, ls := range labelSets {
				rendered = append(rendered, ls.String())
			}
			slices.Sort(rendered)
			expected[name] = rendered
		}
		require.Equal(t, expected, observed.Info, "%s: info() enrichment", phase)
	})
	return observed
}

// identityTimestamps is a series' expected timestamp column.
func identityTimestamps(expected identityExpectedSeries) []int64 {
	out := make([]int64, 0, len(expected.samples))
	for _, sample := range expected.samples {
		out = append(out, sample.timestamp)
	}
	return out
}

// identityKinds is a series' expected sample-kind column. The fixture states no
// recorded-value flag and no histogram, so every sample is a live float.
func identityKinds(expected identityExpectedSeries) []SampleKind {
	out := make([]SampleKind, 0, len(expected.samples))
	for range expected.samples {
		out = append(out, SampleFloat)
	}
	return out
}

// identityStartTimestamps is a series' expected stated start timestamps. The
// fixture states none, so the column stays unknown.
func identityStartTimestamps(identityExpectedSeries) []int64 { return nil }

// identityValues is a series' expected value column.
func identityValues(expected identityExpectedSeries) []float64 {
	out := make([]float64, 0, len(expected.samples))
	for _, sample := range expected.samples {
		out = append(out, sample.value)
	}
	return out
}

// identityExpectedAt states every MetadataAtLabels answer the fixture requires:
// the content valid at the instant, or no answer before the membership starts.
func identityExpectedAt(series []identityExpectedSeries, histories map[OwnerKey][]Version) []string {
	var out []string
	for _, expected := range series {
		instants := []int64{0, expected.samples[0].timestamp - 1}
		for _, sample := range expected.samples {
			instants = append(instants, sample.timestamp)
		}
		instants = append(instants, expected.samples[len(expected.samples)-1].timestamp+1)
		membershipStart := expected.samples[0].timestamp
		for _, sample := range expected.samples[1:] {
			membershipStart = min(membershipStart, sample.timestamp)
		}
		for _, kind := range identityMetadataKinds {
			versions := histories[identityOwner(kind, expected)]
			for _, at := range instants {
				content := Content{}.Canonical()
				found := at >= membershipStart
				if found {
					content = identityContentAt(versions, at)
				}
				out = append(out, fmt.Sprintf("%s %s %d %t %s", expected.labels.Get("probe"), kind.String(), at, found, content))
			}
		}
	}
	return out
}

// identityContentAt returns the canonical content valid at one instant. The
// caller has already established that the series belongs to the owner then.
func identityContentAt(versions []Version, at int64) string {
	valid := 0
	for i := range versions {
		if versions[i].EffectiveFrom <= at {
			valid = i
		}
	}
	return versions[valid].Content.Canonical()
}

// TestNativeMetadataIdentityReferenceDeltas asserts the complete public result of
// the identity-reference fixture in the live head, after WAL replay and after an
// explicit flush and reopen. It covers exact series labels and samples, every
// owner history, every membership with its effective and observation bounds,
// MetadataAtLabels and info() over each applicable validity band.
func TestNativeMetadataIdentityReferenceDeltas(t *testing.T) {
	engine := identityEngine(t)

	t.Run("in_memory_head", func(t *testing.T) {
		s := identityStorage(t, t.TempDir(), true)
		identityIngest(t, s)
		requireIdentityState(t, s, engine, "in_memory_head")
	})

	t.Run("wal_recovery", func(t *testing.T) {
		dir := t.TempDir()
		s := identityStorage(t, dir, false)
		identityIngest(t, s)
		live := requireIdentityState(t, s, engine, "wal_recovery_live")
		require.NoError(t, s.Close())

		// Replay states the whole fixture at once and must reproduce the live head
		// exactly.
		replayed := identityReopen(t, dir)
		recovered := requireIdentityState(t, replayed, engine, "wal_recovery_reopened")
		require.Equal(t, live, recovered, "WAL replay must reproduce every live result")
	})

	t.Run("published_metadata", func(t *testing.T) {
		dir := t.TempDir()
		s := identityStorage(t, dir, false)
		identityIngest(t, s)
		live := requireIdentityState(t, s, engine, "published_live")
		require.NoError(t, s.Flush())
		require.NotZero(t, s.NumBlocks(), "the flush must publish a block")
		flushed := requireIdentityState(t, s, engine, "published_flushed")
		require.Equal(t, live, flushed, "publishing a block must not change a result")
		require.NoError(t, s.Close())

		// Reopening recovers the published metadata and must reproduce it exactly.
		reopened := identityReopen(t, dir)
		require.Equal(t, live, requireIdentityState(t, reopened, engine, "published_reopened"),
			"block recovery must reproduce every live result")
	})

	t.Run("collision_pair_stays_distinct", func(t *testing.T) {
		// The two beta Resources project to one job, one instance and one promoted
		// attribute set. Stated as complete label maps, their series differ in the
		// native identity reference alone, and their content differs in the
		// descriptive attributes that projection drops.
		s := identityStorage(t, t.TempDir(), true)
		identityIngest(t, s)

		betaLabels := labels.FromStrings(
			labels.MetricName, "native_identity_beta",
			OTELResourceLabel, ResourceOwner(identityResourceBeta.attributes).Ref(),
			"__type__", "gauge",
			"instance", "pod-2",
			"job", "shop/cart",
			"k8s_cluster_name", "west",
			"otel_scope_name", "native-identity-sdk",
			"otel_scope_node", "one",
			"otel_scope_role", "server",
			"otel_scope_schema_url", "scope-schema-1",
			"otel_scope_version", "v1",
			"probe", "/beta-p1",
		)
		collisionLabels := labels.FromStrings(
			labels.MetricName, "native_identity_beta",
			OTELResourceLabel, ResourceOwner(identityResourceCollision.attributes).Ref(),
			"__type__", "gauge",
			"instance", "pod-2",
			"job", "shop/cart",
			"k8s_cluster_name", "west",
			"otel_scope_name", "native-identity-sdk",
			"otel_scope_node", "one",
			"otel_scope_role", "server",
			"otel_scope_schema_url", "scope-schema-1",
			"otel_scope_version", "v1",
			"probe", "/beta-p1",
		)
		require.NotEqual(t, betaLabels.Get(OTELResourceLabel), collisionLabels.Get(OTELResourceLabel),
			"the two Resources must keep distinct identity references")
		withoutReference := func(ls labels.Labels) map[string]string {
			out := map[string]string{}
			ls.Range(func(l labels.Label) {
				if l.Name == OTELResourceLabel {
					return
				}
				out[l.Name] = l.Value
			})
			return out
		}
		require.Equal(t, withoutReference(betaLabels), withoutReference(collisionLabels),
			"the fixture must state one projection for both Resources")

		// Both series exist, each with its own samples.
		series, err := s.SelectSeries(t.Context(), 0, identityEnd, nil, 0)
		require.NoError(t, err)
		byLabels := map[string]Series{}
		for _, item := range series {
			byLabels[item.Labels.String()] = item
		}
		beta, ok := byLabels[betaLabels.String()]
		require.True(t, ok, "the beta Resource series must be stored")
		require.Equal(t, []int64{5000}, beta.Timestamps)
		collision, ok := byLabels[collisionLabels.String()]
		require.True(t, ok, "the colliding-projection Resource series must be stored separately")
		require.Equal(t, []int64{10000}, collision.Timestamps)

		// Each owner keeps its own complete content.
		for _, tc := range []struct {
			name   string
			owner  OwnerKey
			labels labels.Labels
			host   string
			rack   string
			at     int64
		}{
			{name: "beta", owner: ResourceOwner(identityResourceBeta.attributes), labels: betaLabels, host: "host-2", rack: "rack-2", at: 5000},
			{name: "collision", owner: ResourceOwner(identityResourceCollision.attributes), labels: collisionLabels, host: "host-2-shadow", rack: "rack-shared", at: 10000},
		} {
			history, ok := s.MetadataHistory(tc.owner)
			require.True(t, ok, "%s: owner history", tc.name)
			require.Len(t, history.Versions, 1, "%s: one Resource version", tc.name)
			require.Equal(t, tc.rack, history.Versions[0].Content.Attributes["host.rack"],
				"%s: the owner must keep its own Resource content", tc.name)
			content, found, truncated := s.MetadataAtLabels(tc.labels, MetadataResource, tc.at)
			require.False(t, truncated)
			require.True(t, found, "%s: the series must resolve its Resource context", tc.name)
			require.Equal(t, tc.host, content.Attributes["host.name"], "%s: the series must resolve its own Resource content", tc.name)
			require.Equal(t, tc.rack, content.Attributes["host.rack"], "%s: the series must resolve its own Resource content", tc.name)
		}
	})

	t.Run("filtered_rows_advance_the_cursors", func(t *testing.T) {
		// The fixture's first three metric rows are filtered or point-free and the
		// supported rows after them must still resolve their own Resource and Scope.
		// The probe of the point-free row is a series the store must not invent.
		s := identityStorage(t, t.TempDir(), true)
		identityIngest(t, s)
		series, err := s.SelectSeries(t.Context(), 0, identityEnd, nil, 0)
		require.NoError(t, err)
		for _, item := range series {
			require.NotEqual(t, "native_identity_alpha_pointless", item.Labels.Get(labels.MetricName),
				"a metric without points must not produce a series")
			require.NotEqual(t, "native_identity_alpha_histogram", item.Labels.Get(labels.MetricName),
				"a filtered metric must not produce a series")
			require.NotEqual(t, "native_identity_epsilon_summary", item.Labels.Get(labels.MetricName),
				"a filtered metric after supported rows must not produce a series")
			require.NotEmpty(t, item.Labels.Get(labels.MetricName), "an unnamed metric must not produce a series")
		}
		// The supported rows that follow the point-free prefix state their own
		// Scope, which the point-free rows must not hand on.
		for _, expected := range identityExpectedSeriesSet() {
			content, found, _ := s.MetadataAtLabels(expected.labels, MetadataScope, expected.samples[0].timestamp)
			require.True(t, found, "%s must keep a Scope membership", expected.labels)
			require.Equal(t, map[string]string{"schema_url": expected.scope.schemaURL}, content.Attributes,
				"%s must resolve its own Scope content", expected.labels)
			require.Equal(t, expected.scope.name, expected.labels.Get("otel_scope_name"))
		}
	})
}

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
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	colarspb "github.com/open-telemetry/otel-arrow/go/api/experimental/arrow/v1"
	"github.com/open-telemetry/otel-arrow/go/pkg/otel/arrow_record"
	"github.com/open-telemetry/otel-arrow/go/pkg/record_message"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/prometheus/prometheus/config"
	otlpprw "github.com/prometheus/prometheus/storage/remote/otlptranslator/prometheusremotewrite"
)

// The fixture in this file is the identity-reference decode oracle. The OTAP
// metrics record stores resource.id and scope.id with Uint16DeltaBuilder: the
// first value of a record is the initial ID and every later value is the delta
// from the previous row. A decoder that reads those columns without accumulating
// them resolves a later Resource or Scope occurrence to an earlier one's
// attributes, so a batch with three or more Resources or Scope occurrences
// silently misattributes identity and content while the sink still reports
// success.
//
// The fixture is built for that shape: five Resources, five Scope identities in
// eight occurrences, repeated references to the same Resource and Scope across
// metric rows, zero deltas and successive non-zero deltas, a pair of Resources
// that project to identical job/instance/promoted labels, and filtered or
// point-free metric rows placed before and after supported rows. Every metric row
// and every data point carries a unique marker, so each stored result can be
// traced back to its logical input whichever order the producer emits.
//
// Keep this table and the expectations in otap_identity_semantics_test.go in
// step: the mechanism test asserts the decoded references, the semantic test
// asserts the complete public result of the same logical input.

const (
	identityHelp = "identity delta marker"
	// identityEnd covers every sample of the fixture.
	identityEnd = 20000
)

// identityResource is one Resource occurrence: the complete attribute set the
// Resource states and its schema URL.
type identityResource struct {
	key        string
	attributes map[string]string
	schemaURL  string
}

// identityScope is one instrumentation Scope occurrence. Scope identity for the
// producer includes the attributes and the schema URL, while the native Scope
// owner is keyed by name and version only.
type identityScope struct {
	key        string
	name       string
	version    string
	attributes map[string]string
	schemaURL  string
}

// identityPoint is one number data point of the fixture. The probe is a unique
// series marker and the value echoes the point's intent, so a misplaced sample
// changes both the series it lands in and the value it carries.
type identityPoint struct {
	probe     string
	timestamp int64
	value     int64
}

// identityMetricRow is one row of the OTAP metrics record. A row with no points
// still occupies a position in the Resource and Scope delta sequences, and a row
// the sink filters occupies one too.
type identityMetricRow struct {
	resource identityResource
	scope    identityScope
	// name is the stated metric name; empty states an unnamed metric row.
	name   string
	kind   pmetric.MetricType
	points []identityPoint
}

var (
	identityResourceAlpha = identityResource{
		key: "alpha",
		attributes: map[string]string{
			"service.name": "cart", "service.namespace": "shop", "service.instance.id": "pod-1",
			"host.name": "host-1", "k8s.cluster.name": "west", "host.rack": "rack-1",
		},
		schemaURL: "res-schema-alpha",
	}
	identityResourceBeta = identityResource{
		key: "beta",
		attributes: map[string]string{
			"service.name": "cart", "service.namespace": "shop", "service.instance.id": "pod-2",
			"host.name": "host-2", "k8s.cluster.name": "west", "host.rack": "rack-2",
		},
		schemaURL: "res-schema-beta",
	}
	// identityResourceCollision states the same job and instance projection as
	// identityResourceBeta: service.name "shop/cart" without a namespace yields
	// job "shop/cart", and the instance is "pod-2". Only the identity reference
	// and the native descriptive content can keep the two contexts apart.
	identityResourceCollision = identityResource{
		key: "collision",
		attributes: map[string]string{
			"service.name": "shop/cart", "service.instance.id": "pod-2",
			"host.name": "host-2-shadow", "k8s.cluster.name": "west", "host.rack": "rack-shared",
		},
		schemaURL: "res-schema-alpha",
	}
	identityResourceGamma = identityResource{
		key: "gamma",
		attributes: map[string]string{
			"service.name": "cart", "service.namespace": "shop", "service.instance.id": "pod-3",
			"host.name": "host-3", "k8s.cluster.name": "east", "host.rack": "rack-3",
		},
		schemaURL: "res-schema-alpha",
	}
	identityResourceDelta = identityResource{
		key: "delta",
		attributes: map[string]string{
			"service.name": "cart", "service.instance.id": "pod-4",
			"host.name": "host-4", "k8s.cluster.name": "east", "host.rack": "rack-4",
		},
		schemaURL: "res-schema-beta",
	}

	identityScopeServer = identityScope{
		key: "server", name: "native-identity-sdk", version: "v1",
		attributes: map[string]string{"node": "one", "role": "server"}, schemaURL: "scope-schema-1",
	}
	identityScopeClient = identityScope{
		key: "client", name: "native-identity-sdk", version: "v1",
		attributes: map[string]string{"node": "two", "role": "client"}, schemaURL: "scope-schema-1",
	}
	identityScopeSecondary = identityScope{
		key: "secondary", name: "native-identity-sdk", version: "v2",
		attributes: map[string]string{"node": "three"}, schemaURL: "scope-schema-2",
	}
	identityScopeOther = identityScope{
		key: "other", name: "native-identity-other", version: "v1",
		attributes: map[string]string{"node": "four"}, schemaURL: "scope-schema-1",
	}
	identityScopeLate = identityScope{
		key: "late", name: "native-identity-sdk", version: "v2",
		attributes: map[string]string{"node": "five"}, schemaURL: "scope-schema-3",
	}
)

// identityMetricRows returns the fixture's metric rows in input order. The real
// producer sorts them before encoding; the decode test asserts the order it
// actually emits, and the semantic test never depends on a row position.
func identityMetricRows() []identityMetricRow {
	return []identityMetricRow{
		// Rows 0-4 belong to one Resource: the first three state Scope "server"
		// and are filtered or point-free, the last two state Scope "client" and
		// carry the Resource's samples. The filtered rows therefore precede
		// supported rows that reuse the same Resource.
		{
			resource: identityResourceAlpha, scope: identityScopeServer,
			name: "", kind: pmetric.MetricTypeGauge,
		},
		{
			resource: identityResourceAlpha, scope: identityScopeServer,
			name: "native_identity_alpha_pointless", kind: pmetric.MetricTypeGauge,
		},
		{
			resource: identityResourceAlpha, scope: identityScopeServer,
			name: "native_identity_alpha_histogram", kind: pmetric.MetricTypeHistogram,
		},
		{
			resource: identityResourceAlpha, scope: identityScopeClient, name: "native_identity_alpha", kind: pmetric.MetricTypeGauge,
			points: []identityPoint{
				{probe: "/alpha-p1", timestamp: 1000, value: 11},
				{probe: "/alpha-shared", timestamp: 2000, value: 12},
				{probe: "/alpha-shared", timestamp: 3000, value: 13},
			},
		},
		{
			resource: identityResourceAlpha, scope: identityScopeClient, name: "native_identity_alpha_sum", kind: pmetric.MetricTypeSum,
			points: []identityPoint{{probe: "/alpha-sum", timestamp: 4000, value: 14}},
		},
		{
			resource: identityResourceBeta, scope: identityScopeServer, name: "native_identity_beta", kind: pmetric.MetricTypeGauge,
			points: []identityPoint{
				{probe: "/beta-p1", timestamp: 5000, value: 21},
				{probe: "/beta-shared", timestamp: 6000, value: 22},
				{probe: "/beta-shared", timestamp: 7000, value: 23},
			},
		},
		{
			resource: identityResourceBeta, scope: identityScopeServer, name: "native_identity_beta_sum", kind: pmetric.MetricTypeSum,
			points: []identityPoint{{probe: "/beta-sum", timestamp: 8000, value: 24}},
		},
		{
			resource: identityResourceBeta, scope: identityScopeSecondary, name: "native_identity_beta_scope2", kind: pmetric.MetricTypeGauge,
			points: []identityPoint{{probe: "/beta-scope2", timestamp: 9000, value: 25}},
		},
		// The collision Resource states the same metric name, the same Scope and
		// the same point markers as the beta row, so its series labels differ from
		// beta's in the Resource reference only.
		{
			resource: identityResourceCollision, scope: identityScopeServer, name: "native_identity_beta", kind: pmetric.MetricTypeGauge,
			points: []identityPoint{
				{probe: "/beta-p1", timestamp: 10000, value: 41},
				{probe: "/beta-shared", timestamp: 11000, value: 42},
				{probe: "/beta-shared", timestamp: 12000, value: 43},
			},
		},
		{
			resource: identityResourceGamma, scope: identityScopeOther, name: "native_identity_gamma_sum", kind: pmetric.MetricTypeSum,
			points: []identityPoint{
				{probe: "/gamma-sum-p1", timestamp: 13000, value: 31},
				{probe: "/gamma-sum-shared", timestamp: 14000, value: 32},
			},
		},
		{
			resource: identityResourceGamma, scope: identityScopeClient, name: "native_identity_gamma_gauge", kind: pmetric.MetricTypeGauge,
			points: []identityPoint{{probe: "/gamma-gauge", timestamp: 15000, value: 33}},
		},
		{
			resource: identityResourceDelta, scope: identityScopeLate, name: "native_identity_epsilon", kind: pmetric.MetricTypeGauge,
			points: []identityPoint{
				{probe: "/epsilon-shared", timestamp: 16000, value: 51},
				{probe: "/epsilon-shared", timestamp: 17000, value: 52},
			},
		},
		// A filtered row after the supported rows of the same Resource and Scope.
		{
			resource: identityResourceDelta, scope: identityScopeLate,
			name: "native_identity_epsilon_summary", kind: pmetric.MetricTypeSummary,
		},
	}
}

// identitySettings keeps promoted Resource labels to one attribute, promotes the
// Scope metadata that carries the Scope attributes, and disables synthetic
// target_info so every stored series and metadata owner comes from the fixture.
func identitySettings() otlpprw.Settings {
	return otlpprw.Settings{
		AddMetricSuffixes: false,
		AllowUTF8:         false,
		DisableTargetInfo: true,
		PromoteResourceAttributes: otlpprw.NewPromoteResourceAttributes(config.OTLPConfig{
			PromoteResourceAttributes: []string{"k8s.cluster.name"},
		}),
		KeepIdentifyingResourceAttributes:    true,
		PromoteScopeMetadata:                 true,
		AllowDeltaTemporality:                true,
		LookbackDelta:                        5 * time.Minute,
		EnableTypeAndUnitLabels:              true,
		LabelNameUnderscoreSanitization:      true,
		LabelNamePreserveMultipleUnderscores: true,
	}
}

// identityMetrics converts the fixture table into one OTAP batch. Resources and
// Scopes are stated as separate occurrences in first-appearance order, which is
// the input the producer sorts and encodes.
func identityMetrics() pmetric.Metrics {
	md := pmetric.NewMetrics()
	resources := map[string]pmetric.ResourceMetrics{}
	scopes := map[[2]string]pmetric.ScopeMetrics{}
	for _, row := range identityMetricRows() {
		rm, exists := resources[row.resource.key]
		if !exists {
			rm = md.ResourceMetrics().AppendEmpty()
			rm.SetSchemaUrl(row.resource.schemaURL)
			for _, name := range slices.Sorted(maps.Keys(row.resource.attributes)) {
				rm.Resource().Attributes().PutStr(name, row.resource.attributes[name])
			}
			resources[row.resource.key] = rm
		}
		scopeKey := [2]string{row.resource.key, row.scope.key}
		sm, exists := scopes[scopeKey]
		if !exists {
			sm = rm.ScopeMetrics().AppendEmpty()
			sm.SetSchemaUrl(row.scope.schemaURL)
			sm.Scope().SetName(row.scope.name)
			sm.Scope().SetVersion(row.scope.version)
			for _, name := range slices.Sorted(maps.Keys(row.scope.attributes)) {
				sm.Scope().Attributes().PutStr(name, row.scope.attributes[name])
			}
			scopes[scopeKey] = sm
		}
		metric := sm.Metrics().AppendEmpty()
		metric.SetName(row.name)
		metric.SetDescription(identityHelp)
		switch row.kind {
		case pmetric.MetricTypeGauge:
			gauge := metric.SetEmptyGauge()
			for _, point := range row.points {
				dp := gauge.DataPoints().AppendEmpty()
				dp.SetTimestamp(pcommon.Timestamp(point.timestamp * 1_000_000))
				dp.SetIntValue(point.value)
				dp.Attributes().PutStr("probe", point.probe)
			}
		case pmetric.MetricTypeSum:
			sum := metric.SetEmptySum()
			sum.SetIsMonotonic(true)
			sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
			for _, point := range row.points {
				dp := sum.DataPoints().AppendEmpty()
				dp.SetTimestamp(pcommon.Timestamp(point.timestamp * 1_000_000))
				dp.SetIntValue(point.value)
				dp.Attributes().PutStr("probe", point.probe)
			}
		case pmetric.MetricTypeHistogram:
			metric.SetEmptyHistogram()
		case pmetric.MetricTypeSummary:
			metric.SetEmptySummary()
		}
	}
	return md
}

// identityRecords encodes the fixture with the real OTAP producer and decodes it
// with the real consumer, which is the record stream the sink receives.
func identityRecords(t *testing.T) []*record_message.RecordMessage {
	t.Helper()
	producer := arrow_record.NewProducer()
	t.Cleanup(func() { require.NoError(t, producer.Close()) })
	batch, err := producer.BatchArrowRecordsFromMetrics(identityMetrics())
	require.NoError(t, err)
	consumer := arrow_record.NewConsumer()
	t.Cleanup(func() { _ = consumer.Close() })
	records, err := consumer.Consume(batch)
	require.NoError(t, err)
	return records
}

// identityRecordSet is the payload subset the native sink decodes.
type identityRecordSet struct {
	metrics    *record_message.RecordMessage
	resources  *record_message.RecordMessage
	scopes     *record_message.RecordMessage
	numberDPs  *record_message.RecordMessage
	pointAttrs *record_message.RecordMessage
}

func identityPayloads(t *testing.T, records []*record_message.RecordMessage) identityRecordSet {
	t.Helper()
	var set identityRecordSet
	for _, record := range records {
		switch record.PayloadType() {
		case colarspb.ArrowPayloadType_UNIVARIATE_METRICS:
			set.metrics = record
		case colarspb.ArrowPayloadType_RESOURCE_ATTRS:
			set.resources = record
		case colarspb.ArrowPayloadType_SCOPE_ATTRS:
			set.scopes = record
		case colarspb.ArrowPayloadType_NUMBER_DATA_POINTS:
			set.numberDPs = record
		case colarspb.ArrowPayloadType_NUMBER_DP_ATTRS:
			set.pointAttrs = record
		}
	}
	require.NotNil(t, set.metrics, "fixture needs a metrics payload")
	require.NotNil(t, set.numberDPs, "fixture needs a number data point payload")
	return set
}

// identityDecodedRowOrder is the order the real producer emits the fixture's
// metric rows in. Metrics are sorted by Resource identity string, then Scope
// identity string, then metric type and name, which is also the order the
// Resource and Scope IDs are assigned in. The expectation is stated here instead
// of inferred from the decode result: if the producer's ordering ever changes,
// the fixture must be re-examined rather than silently weakening.
var identityDecodedRowOrder = []struct {
	resource   string
	scope      string
	name       string
	metricType pmetric.MetricType
	// samples is the number of data points the row states; zero marks a filtered
	// or point-free row that still occupies a delta position.
	samples int
}{
	{resource: "alpha", scope: "server", name: "", metricType: pmetric.MetricTypeGauge},
	{resource: "alpha", scope: "server", name: "native_identity_alpha_pointless", metricType: pmetric.MetricTypeGauge},
	{resource: "alpha", scope: "server", name: "native_identity_alpha_histogram", metricType: pmetric.MetricTypeHistogram},
	{resource: "alpha", scope: "client", name: "native_identity_alpha", metricType: pmetric.MetricTypeGauge, samples: 3},
	{resource: "alpha", scope: "client", name: "native_identity_alpha_sum", metricType: pmetric.MetricTypeSum, samples: 1},
	{resource: "beta", scope: "server", name: "native_identity_beta", metricType: pmetric.MetricTypeGauge, samples: 3},
	{resource: "beta", scope: "server", name: "native_identity_beta_sum", metricType: pmetric.MetricTypeSum, samples: 1},
	{resource: "beta", scope: "secondary", name: "native_identity_beta_scope2", metricType: pmetric.MetricTypeGauge, samples: 1},
	{resource: "collision", scope: "server", name: "native_identity_beta", metricType: pmetric.MetricTypeGauge, samples: 3},
	{resource: "gamma", scope: "other", name: "native_identity_gamma_sum", metricType: pmetric.MetricTypeSum, samples: 2},
	{resource: "gamma", scope: "client", name: "native_identity_gamma_gauge", metricType: pmetric.MetricTypeGauge, samples: 1},
	{resource: "delta", scope: "late", name: "native_identity_epsilon", metricType: pmetric.MetricTypeGauge, samples: 2},
	{resource: "delta", scope: "late", name: "native_identity_epsilon_summary", metricType: pmetric.MetricTypeSummary},
}

// identityResourceByKey returns the fixture Resource with the given key.
func identityResourceByKey(t *testing.T, key string) identityResource {
	t.Helper()
	for _, resource := range []identityResource{
		identityResourceAlpha, identityResourceBeta, identityResourceCollision,
		identityResourceGamma, identityResourceDelta,
	} {
		if resource.key == key {
			return resource
		}
	}
	t.Fatalf("fixture has no Resource %q", key)
	return identityResource{}
}

// identityScopeByKey returns the fixture Scope with the given key.
func identityScopeByKey(t *testing.T, key string) identityScope {
	t.Helper()
	for _, scope := range []identityScope{
		identityScopeServer, identityScopeClient, identityScopeSecondary,
		identityScopeOther, identityScopeLate,
	} {
		if scope.key == key {
			return scope
		}
	}
	t.Fatalf("fixture has no Scope %q", key)
	return identityScope{}
}

// TestNativeMetadataIdentityReferenceDecodeOrder asserts the Resource and Scope
// references the native sink resolves for the fixture, against an independent
// accumulation of the delta columns the producer wrote. The first value of each
// column is the initial ID and every later value is that row's delta, so the
// accumulated reference of a row is the running sum up to that row - not the row's
// own column value.
func TestNativeMetadataIdentityReferenceDecodeOrder(t *testing.T) {
	set := identityPayloads(t, identityRecords(t))
	resourceAttrs, err := decodeOTAPAttributes[uint16](set.resources)
	require.NoError(t, err)
	scopeAttrs, err := decodeOTAPAttributes[uint16](set.scopes)
	require.NoError(t, err)

	rec := set.metrics.Record()
	require.Equal(t, len(identityDecodedRowOrder), int(rec.NumRows()), "every fixture metric row must reach the metrics record")
	idCol := column(rec, "id")
	resourceIDCol := otapStructColumn(rec, "resource", "id")
	resourceSchemaCol := otapStructColumn(rec, "resource", "schema_url")
	scopeIDCol := otapStructColumn(rec, "scope", "id")
	scopeNameCol := otapStructColumn(rec, "scope", "name")
	scopeVersionCol := otapStructColumn(rec, "scope", "version")
	scopeSchemaCol := column(rec, "schema_url")
	nameCol := column(rec, "name")
	typeCol := column(rec, "metric_type")
	for _, required := range []arrow.Array{idCol, resourceIDCol, resourceSchemaCol, scopeIDCol, scopeNameCol, scopeVersionCol, scopeSchemaCol, nameCol, typeCol} {
		require.NotNil(t, required, "fixture requires every decoded metrics column")
	}

	// Re-derive the reference sequence from the fixture alone: Resource IDs are
	// assigned on first appearance in row order, and Scope IDs on every Scope
	// occurrence change. A row's column value is the difference to the previous
	// row's reference, so the reference of a row is the running sum of the column.
	expectedResourceIDs := make([]uint16, 0, len(identityDecodedRowOrder))
	expectedScopeIDs := make([]uint16, 0, len(identityDecodedRowOrder))
	resourceIDs := map[string]uint16{}
	var scopeOccurrences int
	var previousScope string
	for row, want := range identityDecodedRowOrder {
		resourceID, exists := resourceIDs[want.resource]
		if !exists {
			resourceID = uint16(len(resourceIDs))
			resourceIDs[want.resource] = resourceID
		}
		expectedResourceIDs = append(expectedResourceIDs, resourceID)
		if row == 0 || want.scope != previousScope {
			previousScope = want.scope
			expectedScopeIDs = append(expectedScopeIDs, uint16(scopeOccurrences))
			scopeOccurrences++
			continue
		}
		expectedScopeIDs = append(expectedScopeIDs, uint16(scopeOccurrences-1))
	}
	require.Len(t, resourceIDs, 5, "the fixture states five distinct Resources")
	require.Equal(t, 8, scopeOccurrences, "the fixture states eight distinct Scope occurrences")

	// The fixture must state both zero deltas (repeated references) and non-zero
	// deltas on stored samples, otherwise it could not reproduce a raw read.
	resourceDeltas := map[uint16]struct{}{}
	for row := range identityDecodedRowOrder {
		require.Equal(t, identityDecodedRowOrder[row].name, stringValue(nameCol, row), "decoded row %d metric name", row)
		require.Equal(t, uint64(identityDecodedRowOrder[row].metricType), uintValue(typeCol, row), "decoded row %d metric type", row)
		delta := uint16(uintValue(resourceIDCol, row))
		resourceDeltas[delta] = struct{}{}
		require.Equal(t, resourceDelta(expectedResourceIDs, row), delta, "decoded row %d Resource delta", row)
		require.Equal(t, scopeDelta(expectedScopeIDs, row), uint16(uintValue(scopeIDCol, row)), "decoded row %d Scope delta", row)
	}
	require.Contains(t, resourceDeltas, uint16(0), "the fixture must repeat a Resource reference with a zero delta")
	require.Contains(t, resourceDeltas, uint16(1), "the fixture must advance the Resource reference")

	// A row whose raw column value differs from its accumulated reference is a row
	// a non-accumulating read misattributes. Both references must be misread on
	// rows that carry stored samples, not only on filtered ones.
	misattributedSamples := 0
	for row := range identityDecodedRowOrder {
		if identityDecodedRowOrder[row].samples == 0 {
			continue
		}
		if uint16(uintValue(resourceIDCol, row)) != expectedResourceIDs[row] ||
			uint16(uintValue(scopeIDCol, row)) != expectedScopeIDs[row] {
			misattributedSamples++
		}
	}
	require.NotZero(t, misattributedSamples, "the fixture must state identity references a non-accumulating read misattributes")

	contexts, err := readOTAPMetricContexts(rec, resourceAttrs)
	require.NoError(t, err)
	require.Len(t, contexts, 10, "every convertible metric row is decoded, including point-free rows")

	metricIDs := make([]uint16, len(identityDecodedRowOrder))
	var metricID uint16
	for row := range identityDecodedRowOrder {
		metricID += uint16(uintValue(idCol, row))
		metricIDs[row] = metricID
	}

	// Each row is its own subtest so a defect reports every row it misattributes
	// instead of only the first one.
	for row, want := range identityDecodedRowOrder {
		t.Run(fmt.Sprintf("row_%02d", row), func(t *testing.T) {
			resource := identityResourceByKey(t, want.resource)
			scope := identityScopeByKey(t, want.scope)

			// The record columns carry the occurrence the row belongs to.
			require.Equal(t, resource.attributes, otapAttributeStrings(resourceAttrs.AttributesByID(expectedResourceIDs[row])),
				"resource attributes in the attributes store")
			require.Equal(t, resource.schemaURL, stringValue(resourceSchemaCol, row), "resource schema URL")
			require.Equal(t, scope.attributes, otapAttributeStrings(scopeAttrs.AttributesByID(expectedScopeIDs[row])),
				"scope attributes in the attributes store")
			require.Equal(t, scope.name, stringValue(scopeNameCol, row), "scope name")
			require.Equal(t, scope.version, stringValue(scopeVersionCol, row), "scope version")
			require.Equal(t, scope.schemaURL, stringValue(scopeSchemaCol, row), "scope schema URL")

			ctx, decoded := contexts[metricIDs[row]]
			if want.name == "" || want.metricType == pmetric.MetricTypeHistogram || want.metricType == pmetric.MetricTypeSummary {
				require.False(t, decoded, "a filtered row has no decoded context")
				return
			}
			require.True(t, decoded, "a supported row has a decoded context at metric %d", metricIDs[row])
			require.Equal(t, want.name, ctx.metric.Name(), "metric name")
			require.Equal(t, expectedResourceIDs[row], ctx.resource.id, "Resource reference")
			require.Equal(t, resource.attributes, ctx.resource.attributes, "complete Resource attributes")
			require.Equal(t, ResourceOwner(resource.attributes), ctx.resource.owner, "Resource owner")
			require.Equal(t, resource.schemaURL, ctx.resource.schemaURL, "Resource schema URL")
			require.Equal(t, expectedScopeIDs[row], ctx.scopeID, "Scope reference")
			require.Equal(t, scope.name, ctx.scopeName, "Scope name")
			require.Equal(t, scope.version, ctx.scopeVersion, "Scope version")
			require.Equal(t, scope.schemaURL, ctx.scopeSchemaURL, "Scope schema URL")
			require.Equal(t, scope.attributes, otapAttributeStrings(scopeAttrs.AttributesByID(ctx.scopeID)),
				"the decoded Scope reference must resolve the row's own Scope attributes")
		})
	}

	// The reference cursors advance across rows that produce no sample. The first
	// three rows of the fixture are filtered or point-free and share one Resource
	// and one Scope; the first supported row states the same Resource with a later
	// Scope, and the alternative later Scope occurrences open their own references.
	require.Zero(t, identityDecodedRowOrder[0].samples)
	require.Zero(t, identityDecodedRowOrder[1].samples)
	require.Zero(t, identityDecodedRowOrder[2].samples)
	require.Equal(t, expectedResourceIDs[0], expectedResourceIDs[3],
		"the point-free prefix and the first supported row state one Resource")
	require.Equal(t, expectedScopeIDs[2]+1, expectedScopeIDs[3],
		"the first supported row opens the Scope reference the point-free prefix precedes")
	require.Equal(t, expectedScopeIDs[0], expectedScopeIDs[2], "the point-free prefix keeps one Scope reference")
}

// resourceDelta returns the delta the producer must have written for one row of
// a reference sequence whose expected references are given.
func resourceDelta(expected []uint16, row int) uint16 {
	if row == 0 {
		return expected[0]
	}
	return expected[row] - expected[row-1]
}

// scopeDelta is resourceDelta for a Scope reference sequence.
func scopeDelta(expected []uint16, row int) uint16 { return resourceDelta(expected, row) }

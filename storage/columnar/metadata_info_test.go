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

package columnar_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
)

func TestInfoUsesTemporalResourceMetadata(t *testing.T) {
	for _, tc := range []struct {
		name         string
		flushSamples int
	}{
		{name: "head", flushSamples: 1000},
		{name: "blocks", flushSamples: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newArrowTestStorage(t, tc.flushSamples)
			owner := resourceLabelForTest()
			ls := labels.FromStrings(
				"__name__", "native_info_metric",
				"__otel_resource__", owner.Ref(),
				"zone", "base-zone",
			)
			for _, timestamp := range []int64{0, 1000, 2000, 3000} {
				appendSample(t, s, ls, timestamp, float64(timestamp), nil, nil)
			}
			associateMetadata(t, s, ls, columnar.Update{
				Owner: owner, EffectiveFrom: 1000,
				Content: columnar.Content{Attributes: map[string]string{
					"host": "a", "region": "east", "zone": "resource-zone",
				}},
			})
			associateMetadata(t, s, ls, columnar.Update{
				Owner: owner, EffectiveFrom: 2000,
				Content: columnar.Content{Attributes: map[string]string{
					"host": "b", "region": "east", "zone": "resource-zone",
				}},
			})
			associateMetadata(t, s, ls, columnar.Update{
				Owner: columnar.ScopeOwner("sdk", "v1"), EffectiveFrom: 1000,
				Content: columnar.Content{Attributes: map[string]string{"schema_url": "scope-v1"}},
			})
			associateMetadata(t, s, ls, columnar.Update{
				Owner: columnar.MetricOwner(ls), EffectiveFrom: 1000,
				Content: columnar.Content{Attributes: map[string]string{"help": "metric help", "type": "counter"}},
			})

			engine := promqltest.NewTestEngine(t, false, 5*time.Minute, promqltest.DefaultMaxSamplesPerQuery)
			for _, query := range []struct {
				name   string
				expr   string
				at     int64
				count  int
				host   string
				region string
				zone   string
			}{
				{name: "before association", expr: `info(native_info_metric)`, at: 500, count: 1, zone: "base-zone"},
				{name: "inclusive first version", expr: `info(native_info_metric)`, at: 1000, count: 1, host: "a", region: "east", zone: "base-zone"},
				{name: "before successor", expr: `info(native_info_metric)`, at: 1999, count: 1, host: "a", region: "east", zone: "base-zone"},
				{name: "inclusive successor", expr: `info(native_info_metric)`, at: 2000, count: 1, host: "b", region: "east", zone: "base-zone"},
				{name: "later query", expr: `info(native_info_metric)`, at: 3000, count: 1, host: "b", region: "east", zone: "base-zone"},
				{name: "at modifier", expr: `info(native_info_metric @ 1)`, at: 3000, count: 1, host: "a", region: "east", zone: "base-zone"},
				{name: "attribute selection", expr: `info(native_info_metric, {host="b"})`, at: 2000, count: 1, host: "b", zone: "base-zone"},
				{name: "attribute mismatch", expr: `info(native_info_metric, {host="a"})`, at: 2000, count: 0},
				{name: "explicit target info", expr: `info(native_info_metric, {__name__="target_info", host="a"})`, at: 1000, count: 1, host: "a", zone: "base-zone"},
				{name: "base label does not satisfy matcher", expr: `info(native_info_metric, {zone="base-zone"})`, at: 1000, count: 0},
				{name: "other kind does not satisfy matcher", expr: `info(native_info_metric, {help="metric help"})`, at: 1000, count: 0},
			} {
				t.Run(query.name, func(t *testing.T) {
					result := queryInfoLabels(t, engine, s, query.expr, query.at)
					require.Len(t, result, query.count)
					if query.count == 0 {
						return
					}
					require.Equal(t, query.host, result[0].Get("host"))
					require.Equal(t, query.region, result[0].Get("region"))
					require.Equal(t, query.zone, result[0].Get("zone"))
					require.Empty(t, result[0].Get("help"), "metric metadata must not become Resource labels")
					require.Empty(t, result[0].Get("schema_url"), "scope metadata must not become Resource labels")
				})
			}

			q, err := s.Querier(0, 3000)
			require.NoError(t, err)
			resourceQuerier, ok := q.(storage.ResourceQuerier)
			require.True(t, ok)
			var names []string
			require.NoError(t, resourceQuerier.IterUniqueAttributeNames(func(name string) {
				names = append(names, name)
			}))
			require.Equal(t, []string{"host", "region", "zone"}, names)
			_, ok = resourceQuerier.GetResourceAt(labels.StableHash(ls), 999)
			require.False(t, ok)
			resource, ok := resourceQuerier.GetResourceAt(labels.StableHash(ls), 1000)
			require.True(t, ok)
			require.Equal(t, "a", resource.Descriptive["host"])
			require.Empty(t, resource.Identifying)
			require.NoError(t, q.Close())
		})
	}
}

func TestInfoDoesNotUseTargetInfoForMissingResourceVersion(t *testing.T) {
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{
		FlushSamples:       1000,
		MetadataVersionCap: 2,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	owner := columnar.ResourceOwner(map[string]string{
		"service.name": "api", "service.instance.id": "one",
	})
	ls := labels.FromStrings(
		"__name__", "truncated_resource_metric", "job", "api", "instance", "one", "__otel_resource__", owner.Ref(),
	)
	targetInfoLabels := labels.FromStrings(
		"__name__", "target_info", "job", "api", "instance", "one", "host", "legacy-host", "legacy_only", "yes",
	)
	for _, timestamp := range []int64{0, 100, 200, 300, 400} {
		appendSample(t, s, ls, timestamp, 1, nil, nil)
	}
	appendSample(t, s, targetInfoLabels, 0, 1, nil, nil)
	for _, update := range []struct {
		timestamp int64
		host      string
	}{
		{timestamp: 100, host: "first"},
		{timestamp: 200, host: "second"},
		{timestamp: 300, host: "third"},
	} {
		associateMetadata(t, s, ls, columnar.Update{
			Owner: owner, EffectiveFrom: update.timestamp,
			Content: columnar.Content{Attributes: map[string]string{"host": update.host}},
		})
	}

	history, ok := s.MetadataHistory(owner)
	require.True(t, ok)
	require.True(t, history.Truncated)
	require.Equal(t, int64(200), history.Versions[0].EffectiveFrom)
	content, ok, truncated := s.MetadataAtLabels(ls, columnar.MetadataResource, 100)
	require.False(t, ok)
	require.True(t, truncated)
	require.Empty(t, content.Attributes)

	q, err := s.Querier(0, 400)
	require.NoError(t, err)
	defer q.Close()
	membership, ok := q.(storage.ResourceMembershipQuerier)
	require.True(t, ok)
	require.False(t, membership.HasResourceMembershipAt(labels.StableHash(ls), 0))
	require.True(t, membership.HasResourceMembershipAt(labels.StableHash(ls), 100))

	engine := promqltest.NewTestEngine(t, false, 5*time.Minute, promqltest.DefaultMaxSamplesPerQuery)
	for _, tc := range []struct {
		name string
		at   int64
		host string
	}{
		{name: "before membership uses legacy fallback", at: 0, host: "legacy-host"},
		{name: "effective membership blocks legacy fallback", at: 100, host: ""},
		{name: "before oldest retained version", at: 199, host: ""},
		{name: "first retained version", at: 200, host: "second"},
		{name: "latest retained version", at: 400, host: "third"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := queryInfoLabels(t, engine, s, `info(truncated_resource_metric)`, tc.at)
			require.Len(t, result, 1)
			require.Equal(t, tc.host, result[0].Get("host"))
			if tc.at >= 100 && tc.at < 200 {
				require.Empty(t, result[0].Get("legacy_only"))
			}
		})
	}

	// A positive Resource matcher sees the missing version's attributes as absent,
	// not as matching legacy-only content.
	result := queryInfoLabels(t, engine, s, `info(truncated_resource_metric, {host="legacy-host"})`, 100)
	require.Empty(t, result)
}

func TestInfoTranslatesResourceAttributeNames(t *testing.T) {
	s := newArrowTestStorage(t, 1000)
	owner := resourceLabelForTest()
	ls := labels.FromStrings("__name__", "translated_info_metric", "__otel_resource__", owner.Ref())
	appendSample(t, s, ls, 1000, 1, nil, nil)
	associateMetadata(t, s, ls, columnar.Update{
		Owner: owner, EffectiveFrom: 1000,
		Content: columnar.Content{Attributes: map[string]string{
			"host.name": "a", "deployment.environment.name": "prod",
		}},
	})

	engine := promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
		MaxSamples:       promqltest.DefaultMaxSamplesPerQuery,
		Timeout:          time.Minute,
		LookbackDelta:    5 * time.Minute,
		LabelNamerConfig: &promql.LabelNamerConfig{},
		Parser:           parser.NewParser(promqltest.TestParserOpts),
	})
	result := queryInfoLabels(t, engine, s, `info(translated_info_metric)`, 1000)
	require.Len(t, result, 1)
	require.Equal(t, "a", result[0].Get("host_name"))
	require.Equal(t, "prod", result[0].Get("deployment_environment_name"))
	require.Empty(t, result[0].Get("host.name"))

	result = queryInfoLabels(t, engine, s, `info(translated_info_metric, {host_name="a"})`, 1000)
	require.Len(t, result, 1)
	require.Equal(t, "a", result[0].Get("host_name"))
	require.Empty(t, result[0].Get("deployment_environment_name"), "only selected Resource attributes are added")
	result = queryInfoLabels(t, engine, s, `info(translated_info_metric, {host_name="b"})`, 1000)
	require.Empty(t, result)
}

func TestInfoFallsBackToTargetInfoWithoutResourceAssociation(t *testing.T) {
	s := newArrowTestStorage(t, 1000)
	owner := columnar.ResourceOwner(map[string]string{
		"service.name": "api", "service.instance.id": "one",
	})
	nativeLabels := labels.FromStrings(
		"__name__", "native_metric", "job", "api", "instance", "one", "__otel_resource__", owner.Ref(),
	)
	legacyLabels := labels.FromStrings("__name__", "legacy_metric", "job", "api", "instance", "one")
	targetInfoLabels := labels.FromStrings(
		"__name__", "target_info", "job", "api", "instance", "one", "host", "legacy-host", "legacy_only", "yes",
	)
	for _, ls := range []labels.Labels{nativeLabels, legacyLabels, targetInfoLabels} {
		appendSample(t, s, ls, 1000, 1, nil, nil)
	}
	associateMetadata(t, s, nativeLabels, columnar.Update{
		Owner: owner, EffectiveFrom: 1000,
		Content: columnar.Content{Attributes: map[string]string{"host": "native-host"}},
	})

	engine := promqltest.NewTestEngine(t, false, 5*time.Minute, promqltest.DefaultMaxSamplesPerQuery)
	native := queryInfoLabels(t, engine, s, `info(native_metric)`, 1000)
	require.Len(t, native, 1)
	require.Equal(t, "native-host", native[0].Get("host"))
	require.Empty(t, native[0].Get("legacy_only"), "native Resource selection must not merge target_info")

	legacy := queryInfoLabels(t, engine, s, `info(legacy_metric)`, 1000)
	require.Len(t, legacy, 1)
	require.Equal(t, "legacy-host", legacy[0].Get("host"))
	require.Equal(t, "yes", legacy[0].Get("legacy_only"))
}

func associateMetadata(t *testing.T, s *columnar.Storage, ls labels.Labels, update columnar.Update) {
	t.Helper()
	_, err := s.MetadataAssociate(ls, update)
	require.NoError(t, err)
}

// queryInfoLabels copies result labels before closing the query and recycling
// its result memory.
func queryInfoLabels(t *testing.T, engine promql.QueryEngine, s *columnar.Storage, expr string, at int64) []labels.Labels {
	t.Helper()
	query, err := engine.NewInstantQuery(t.Context(), s, nil, expr, time.UnixMilli(at))
	require.NoError(t, err)
	defer query.Close()
	result := query.Exec(t.Context())
	require.NoError(t, result.Err)
	vector, ok := result.Value.(promql.Vector)
	require.True(t, ok)
	output := make([]labels.Labels, 0, len(vector))
	for _, sample := range vector {
		output = append(output, sample.Metric.Copy())
	}
	return output
}

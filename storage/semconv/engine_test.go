// Copyright 2025 The Prometheus Authors
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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
)

func TestMatcherBuilder(t *testing.T) {
	t.Run("parses metric name", func(t *testing.T) {
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "http.server.duration"),
			labels.MustNewMatcher(labels.MatchEqual, "http.method", "GET"),
		}

		b, err := newMatcherBuilder(matchers)
		require.NoError(t, err)
		require.Equal(t, "http.server.duration", b.metadata.Name)
		require.Len(t, b.other, 1)
		require.Equal(t, "http.method", b.other[0].Name)
	})

	t.Run("requires equal match for __name__", func(t *testing.T) {
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, "http.*"),
		}

		_, err := newMatcherBuilder(matchers)
		require.Error(t, err)
		require.Contains(t, err.Error(), "__name__ matcher must be equal")
	})

	t.Run("skips schema and semconv URL labels", func(t *testing.T) {
		matchers := []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "http.server.duration"),
			labels.MustNewMatcher(labels.MatchEqual, schemaURLLabel, "https://example.com/schema.yaml"),
			labels.MustNewMatcher(labels.MatchEqual, semconvURLLabel, "https://example.com/semconv.yaml"),
			labels.MustNewMatcher(labels.MatchEqual, "http.method", "GET"),
		}

		b, err := newMatcherBuilder(matchers)
		require.NoError(t, err)
		require.Equal(t, "http.server.duration", b.metadata.Name)
		require.Len(t, b.other, 1)
		require.Equal(t, "http.method", b.other[0].Name)
	})
}

func TestFindMatcherVariants_RequiresSemconvURL(t *testing.T) {
	e := newSchemaEngine()

	matchers := []*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, "http.server.duration"),
	}

	_, _, err := e.FindMatcherVariants("", "", matchers)
	require.Error(t, err)
	require.Contains(t, err.Error(), "semconvURL is required")
}

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

	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/columnar"
)

// newConformanceStorage returns a columnar storage whose buffer flushes to a
// block after every append, so that the conformance suite exercises the block
// read path rather than only the in-memory buffer.
func newConformanceStorage(t testing.TB) storage.Storage {
	t.Helper()

	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{FlushSamples: 1})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

// TestConformance runs PromQL against the columnar storage.
func TestConformance(t *testing.T) {
	promqltest.RunTestWithStorage(t, `
load 5m
  http_requests_total{path="/foo"} 1 2 3
  http_requests_total{path="/bar"} 1 3 5

eval instant at 10m sum(http_requests_total)
  {} 8

eval instant at 10m rate(http_requests_total[10m])
  {path="/foo"} 0.0033333333333333335
  {path="/bar"} 0.006666666666666667

eval instant at 10m http_requests_total{path="/foo"}
  http_requests_total{path="/foo"} 3
`, promqltest.NewTestEngine(t, false, 5*time.Minute, promqltest.DefaultMaxSamplesPerQuery), newConformanceStorage)
}

// TestBuiltinConformance runs the full built-in PromQL acceptance suite against
// the columnar storage. This is the gate that decides whether the store is
// semantically usable at all; a layout that cannot pass it is not worth
// measuring.
func TestBuiltinConformance(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factory func(testing.TB) storage.Storage
	}{
		{name: "independent_blocks", factory: newConformanceStorage},
		{name: "typed_head", factory: newConformanceHeadStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			promqltest.RunBuiltinTestsWithStorage(t,
				promqltest.NewTestEngine(t, false, 5*time.Minute, promqltest.DefaultMaxSamplesPerQuery),
				tc.factory,
			)
		})
	}
}

func newConformanceHeadStorage(t testing.TB) storage.Storage {
	t.Helper()
	s, err := columnar.NewStorage(t.TempDir(), columnar.StorageOptions{FlushSamples: 1_000_000, DisableWAL: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

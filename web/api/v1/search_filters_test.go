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

package v1

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/storage"
)

func TestSubstringFilter(t *testing.T) {
	tests := []struct {
		name         string
		query        string
		value        string
		wantAccepted bool
		wantScore    float64
	}{
		{
			name:         "exact match",
			query:        "prometheus",
			value:        "prometheus",
			wantAccepted: true,
			wantScore:    1.0,
		},
		{
			name:         "prefix match",
			query:        "prom",
			value:        "prometheus",
			wantAccepted: true,
			wantScore:    1.0,
		},
		{
			name:         "substring match in middle",
			query:        "meth",
			value:        "prometheus",
			wantAccepted: true,
			// idx=3, maxIdx=10-4=6 -> 1.0 - 0.9*3/6 = 0.55.
			wantScore: 0.55,
		},
		{
			name:         "substring match at end",
			query:        "heus",
			value:        "prometheus",
			wantAccepted: true,
			// idx=6, maxIdx=10-4=6 -> 1.0 - 0.9 = 0.1.
			wantScore: 0.1,
		},
		{
			name:         "case mismatch is not accepted",
			query:        "prometheus",
			value:        "Prometheus",
			wantAccepted: false,
			wantScore:    0.0,
		},
		{
			name:         "no match",
			query:        "grafana",
			value:        "prometheus",
			wantAccepted: false,
			wantScore:    0.0,
		},
		{
			name:         "empty query accepts all",
			query:        "",
			value:        "anything",
			wantAccepted: true,
			wantScore:    1.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := NewSubstringFilter(tt.query)
			accepted, score := filter.Accept(tt.value)
			require.Equal(t, tt.wantAccepted, accepted)
			require.InDelta(t, tt.wantScore, score, 1e-9)
		})
	}
}

func TestFuzzyFilter(t *testing.T) {
	tests := []struct {
		name         string
		query        string
		threshold    float64
		value        string
		wantAccepted bool
		minScore     float64 // Minimum expected score.
	}{
		{
			name:         "exact match",
			query:        "prometheus",
			threshold:    0.8,
			value:        "prometheus",
			wantAccepted: true,
			minScore:     1.0,
		},
		{
			name:         "close match above threshold",
			query:        "prometheus",
			threshold:    0.8,
			value:        "promethus", // Typo: one char different.
			wantAccepted: true,
			minScore:     0.8,
		},
		{
			name:         "distant match below threshold",
			query:        "prometheus",
			threshold:    0.8,
			value:        "grafana",
			wantAccepted: false,
			minScore:     0.0,
		},
		{
			name:         "case mismatch is not accepted",
			query:        "prometheus",
			threshold:    0.8,
			value:        "PROMETHEUS",
			wantAccepted: false,
			minScore:     0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := NewFuzzyFilter(tt.query, tt.threshold)
			accepted, score := filter.Accept(tt.value)
			require.Equal(t, tt.wantAccepted, accepted)
			if tt.wantAccepted {
				require.GreaterOrEqual(t, score, tt.minScore)
			}
		})
	}
}

func TestFuzzyFilterConcurrency(t *testing.T) {
	filter := NewFuzzyFilter("prometheus", 0.8)
	values := []string{"prometheus", "promethus", "promethius", "prmetheus", "prometeus"} //nolint:misspell

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for _, value := range values {
				_, _ = filter.Accept(value)
			}
		})
	}

	wg.Wait()
}

func TestSubsequenceFilter(t *testing.T) {
	tests := []struct {
		name         string
		query        string
		threshold    float64
		value        string
		wantAccepted bool
		wantScore    float64 // -1 means "any positive value".
	}{
		{
			name:         "exact match",
			query:        "prometheus",
			threshold:    1.0,
			value:        "prometheus",
			wantAccepted: true,
			wantScore:    1.0,
		},
		{
			name:         "prefix match scores 1.0",
			query:        "prom",
			threshold:    1.0,
			value:        "prometheus",
			wantAccepted: true,
			wantScore:    1.0,
		},
		{
			name:         "subsequence match above zero threshold",
			query:        "pms",
			threshold:    0.0,
			value:        "prometheus",
			wantAccepted: true,
			wantScore:    -1,
		},
		{
			name:         "non-subsequence rejected",
			query:        "xyz",
			threshold:    0.0,
			value:        "prometheus",
			wantAccepted: false,
			wantScore:    0.0,
		},
		{
			name:         "case mismatch rejected",
			query:        "prom",
			threshold:    0.0,
			value:        "PROMETHEUS",
			wantAccepted: false,
			wantScore:    0.0,
		},
		{
			name:         "below threshold rejected",
			query:        "pms",
			threshold:    0.99,
			value:        "prometheus",
			wantAccepted: false,
			wantScore:    -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := NewSubsequenceFilter(tt.query, tt.threshold)
			accepted, score := filter.Accept(tt.value)
			require.Equal(t, tt.wantAccepted, accepted)
			if tt.wantScore >= 0 {
				require.InDelta(t, tt.wantScore, score, 1e-9)
			}
		})
	}
}

// countingFilter records how many times Accept is called per value.
type countingFilter struct {
	mu     sync.Mutex
	calls  map[string]int
	result map[string]memoEntry
}

func (f *countingFilter) Accept(value string) (bool, float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[value]++
	r := f.result[value]
	return r.accepted, r.score
}

func TestMemoizingFilter(t *testing.T) {
	inner := &countingFilter{
		calls: map[string]int{},
		result: map[string]memoEntry{
			"prometheus": {accepted: true, score: 1.0},
			"grafana":    {accepted: false, score: 0.0},
		},
	}
	memo := newMemoizingFilter(inner)

	// First call computes.
	accepted, score := memo.Accept("prometheus")
	require.True(t, accepted)
	require.Equal(t, 1.0, score)

	// Repeat calls hit the cache.
	for range 5 {
		accepted, score = memo.Accept("prometheus")
		require.True(t, accepted)
		require.Equal(t, 1.0, score)
	}

	// Distinct values are computed once each.
	accepted, score = memo.Accept("grafana")
	require.False(t, accepted)
	require.Equal(t, 0.0, score)
	memo.Accept("grafana")

	require.Equal(t, 1, inner.calls["prometheus"])
	require.Equal(t, 1, inner.calls["grafana"])
}

func TestCaseFoldingFilter(t *testing.T) {
	// Inner filter expects lowercased query and value.
	inner := NewSubstringFilter("prom")
	wrapped := newCaseFoldingFilter(inner)

	accepted, score := wrapped.Accept("Prometheus")
	require.True(t, accepted)
	require.Equal(t, 1.0, score)

	accepted, _ = wrapped.Accept("Grafana")
	require.False(t, accepted)
}

func TestOrFilter(t *testing.T) {
	tests := []struct {
		name           string
		substringQuery string
		fuzzyQuery     string
		fuzzyThreshold float64
		value          string
		wantAccepted   bool
		minScore       float64
	}{
		{
			name:           "substring match only",
			substringQuery: "prom",
			value:          "prometheus",
			wantAccepted:   true,
			minScore:       1.0, // Prefix match.
		},
		{
			name:           "substring rejects",
			substringQuery: "prom",
			value:          "node",
			wantAccepted:   false,
		},
		{
			name:           "fuzzy fallback",
			substringQuery: "go_gor",
			fuzzyQuery:     "go_gor",
			fuzzyThreshold: 0.8,
			value:          "go_goroutins", // Not substring, but fuzzy matches.
			wantAccepted:   true,
			minScore:       0.8,
		},
		{
			name:         "no filters accept all",
			value:        "anything",
			wantAccepted: true,
			minScore:     1.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var substringFilter *SubstringFilter
			var fuzzyFilter *FuzzyFilter

			if tt.substringQuery != "" {
				substringFilter = NewSubstringFilter(tt.substringQuery)
			}
			if tt.fuzzyQuery != "" {
				fuzzyFilter = NewFuzzyFilter(tt.fuzzyQuery, tt.fuzzyThreshold)
			}

			filter := &orFilter{substringFilter: substringFilter, fuzzyFilter: fuzzyFilter}
			accepted, score := filter.Accept(tt.value)

			require.Equal(t, tt.wantAccepted, accepted)
			if tt.wantAccepted {
				require.GreaterOrEqual(t, score, tt.minScore)
			}
		})
	}
}

func TestChainFilter(t *testing.T) {
	type filterSpec struct {
		query       string
		threshold   float64
		isSubstring bool
	}
	tests := []struct {
		name         string
		filters      []filterSpec
		value        string
		wantAccepted bool
		wantScore    float64
	}{
		{
			name: "both filters accept",
			filters: []filterSpec{
				{query: "prom", isSubstring: true},
				{query: "prometheus", threshold: 0.8},
			},
			value:        "prometheus",
			wantAccepted: true,
			wantScore:    1.0,
		},
		{
			name: "first filter rejects",
			filters: []filterSpec{
				{query: "grafana", isSubstring: true},
				{query: "prometheus", threshold: 0.8},
			},
			value:        "prometheus",
			wantAccepted: false,
			wantScore:    0.0,
		},
		{
			name: "second filter rejects",
			filters: []filterSpec{
				{query: "prom", isSubstring: true},
				{query: "grafana", threshold: 0.9},
			},
			value:        "prometheus",
			wantAccepted: false,
			wantScore:    0.0,
		},
		{
			name:         "empty chain accepts all",
			filters:      nil,
			value:        "anything",
			wantAccepted: true,
			wantScore:    1.0,
		},
		{
			name: "max score wins for ranking",
			filters: []filterSpec{
				{query: "prom", isSubstring: true},    // Score: 1.0 (prefix).
				{query: "prometheus", threshold: 0.5}, // Score: 1.0 (exact).
			},
			value:        "prometheus",
			wantAccepted: true,
			wantScore:    1.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var filters []storage.Filter
			for _, f := range tt.filters {
				if f.isSubstring {
					filters = append(filters, NewSubstringFilter(f.query))
				} else {
					filters = append(filters, NewFuzzyFilter(f.query, f.threshold))
				}
			}

			chain := NewChainFilter(filters...)
			accepted, score := chain.Accept(tt.value)
			require.Equal(t, tt.wantAccepted, accepted)
			require.InDelta(t, tt.wantScore, score, 1e-9)
		})
	}
}

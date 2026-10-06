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

//go:build linux || darwin

package remote

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/tsdb/chunks"
)

// The distinct-store diagnostic's adapter for unmodified ACTE in WAL-only
// mode: legacy Metadata records, whose decoder copies each string, stored as
// one owned value per series. It has no WAL metadata interner and no
// histories, so it resolves nothing, and its split has no resolution phase.

func dsUnavailable(mode, _ string) string {
	if mode == "warm" || mode == "replay" {
		return "no WAL metadata interner"
	}
	return ""
}

func dsHistoryDepth() int { return 1 }

func dsLegacy() bool { return true }

func dsVersions(qm *QueueManager, ref chunks.HeadSeriesRef, dst []dsVersion) ([]dsVersion, bool) {
	if m := qm.seriesMetadata[ref]; m != nil {
		dst = append(dst, dsVersion{m: m})
	}
	return dst, false
}

func dsOlderCap(*QueueManager, chunks.HeadSeriesRef) int { return 0 }

func dsProcessIntern() func(metadata.Metadata) *metadata.Metadata { return nil }

func dsNewInterner(any) (func(metadata.Metadata) *metadata.Metadata, any) { return nil, nil }

func dsWarmInterner(any) {}

func dsWarmProcessInterner() {}

func dsCompareInterners(testing.TB, any, any, []byte) {}

func dsInternerMismatch(any, any) error { return nil }

func dsStoreWith(d *w1Decoder, qm *QueueManager, _ func(metadata.Metadata) *metadata.Metadata) {
	qm.StoreMetadata(d.meta)
}

// dsSplit's resolution does nothing: StoreMetadata resolves no value.
type dsSplit struct{}

func (*dsSplit) resolve(*w1Decoder, *QueueManager, func(metadata.Metadata) *metadata.Metadata) {}

func (*dsSplit) apply(d *w1Decoder, qm *QueueManager) { qm.StoreMetadata(d.meta) }

func dsDecodedRefs(d *w1Decoder) []chunks.HeadSeriesRef {
	refs := make([]chunks.HeadSeriesRef, 0, len(d.meta))
	for _, m := range d.meta {
		refs = append(refs, m.Ref)
	}
	return refs
}

// dsModelMismatch checks that every series holds an object of its own, and
// returns the counts the store makes: no interner calls, and one new value
// per stored entry.
func dsModelMismatch(tb testing.TB, r *dsRun) (dsCounts, error) {
	entries := 0
	d := newW1Decoder()
	for _, rec := range r.records.changes {
		d.decode(tb, rec)
		entries += len(d.meta)
	}
	counts := dsCounts{newValues: float64(entries)}
	seen := map[*metadata.Metadata]chunks.HeadSeriesRef{}
	for ref, m := range r.qm.seriesMetadata {
		if other, ok := seen[m]; ok {
			return counts, fmt.Errorf("series %d and %d share an object", other, ref)
		}
		seen[m] = ref
	}
	return counts, nil
}

func dsCheckModel(tb testing.TB, r *dsRun) dsCounts {
	counts, err := dsModelMismatch(tb, r)
	if err != nil {
		tb.Fatalf("model: %v", err)
	}
	return counts
}

func dsCheckModelDetects(t *testing.T, r *dsRun) {
	a, b := r.qm.seriesMetadata[1], r.qm.seriesMetadata[2]
	r.qm.seriesMetadata[2] = a
	_, err := dsModelMismatch(t, r)
	r.qm.seriesMetadata[2] = b
	require.Error(t, err, "a shared object")
	_, err = dsModelMismatch(t, r)
	require.NoError(t, err)
}

// dsProbes observes, by weak pointers, the queue manager struct and the value
// of each sampled series. No probe observes the series map's storage, or any
// allocation not sampled.
func dsProbes(qm *QueueManager) []dsProbe {
	probes := []dsProbe{dsWeakProbe("queue manager", qm)}
	for _, ref := range dsProbeRefs() {
		probes = append(probes, dsWeakProbe(fmt.Sprintf("value %d", ref), qm.seriesMetadata[ref]))
	}
	return probes
}

func dsValueProbes() []string {
	var names []string
	for _, ref := range dsProbeRefs() {
		names = append(names, fmt.Sprintf("value %d", ref))
	}
	return names
}

func dsRetentionCases() []dsRetentionCase {
	return []dsRetentionCase{
		{"nothing retained", func(*dsRun) []string { return nil }},
		{"the queue manager", func(r *dsRun) []string {
			dsRetained = r.qm
			return append([]string{"queue manager"}, dsValueProbes()...)
		}},
		{"the series map", func(r *dsRun) []string {
			dsRetained = r.qm.seriesMetadata
			return dsValueProbes()
		}},
	}
}

// dsCounter counts the entries stored, each of which StoreMetadata gives a new
// value.
type dsCounter struct{ newValues int }

func dsNewCounter(*dsRun) *dsCounter { return &dsCounter{} }

func (c *dsCounter) store(d *w1Decoder, qm *QueueManager) {
	c.newValues += len(d.meta)
	d.store(qm)
}

func (c *dsCounter) counts() (resolutions, newValues, copied float64) {
	return 0, float64(c.newValues), 0
}

func dsReplay(b *testing.B, _ *dsTimer, _ string) {
	b.Fatal("replays need the WAL metadata interner")
}

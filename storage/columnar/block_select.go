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
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/prometheus/prometheus/model/labels"
)

// Select resolves postings entirely in the block catalog, then intersects the
// result with per-series segment time bounds. Only cohort row groups referenced
// by surviving segments are opened. A response still checks complete labels and
// decodes only selected per-series offsets inside a cohort page.
func (b *Block) Select(ctx context.Context, mint, maxt int64, matcherSets [][]*labels.Matcher, limit int) ([]Series, error) {
	if !b.Overlaps(mint, maxt) {
		return nil, nil
	}

	var timings *queryTimings
	if b.reads != nil {
		timings = b.reads.timings
	}
	matchStart := timings.now()
	ids, err := b.resolveMatcherSets(newIDScratch(idScratchInitial), matcherSets)
	timings.add(phaseMatch, matchStart)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	if b.reads != nil {
		b.reads.seriesMatched.Add(uint64(len(ids)))
	}
	planStart := timings.now()
	var available, planned int
	selections := make([]selectedBlockSeries, 0, len(ids))
	for _, id := range ids {
		i := sortSearchSeries(b.catalog.Series, id)
		if i < 0 {
			return nil, fmt.Errorf("postings reference unknown series ID %d", id)
		}
		entry := b.catalog.Series[i]
		segments := make([]blockSegmentRef, 0, len(entry.Segments))
		for _, ref := range entry.Segments {
			if ref.MinTime <= maxt && ref.MaxTime >= mint {
				segments = append(segments, ref)
			}
		}
		available += len(entry.Segments)
		planned += len(segments)
		if len(segments) > 0 {
			selections = append(selections, selectedBlockSeries{entry: entry, segments: segments})
		}
	}
	if b.reads != nil {
		b.reads.segmentsAvailable.Add(uint64(available))
		b.reads.segmentsPlanned.Add(uint64(planned))
		b.reads.segmentsOutsideWin.Add(uint64(available - planned))
	}
	if len(selections) == 0 {
		return nil, nil
	}

	timings.add(phasePlan, planStart)
	selected, err := b.readSelectedSeries(ctx, selections)
	if err != nil {
		return nil, err
	}
	out := make([]Series, 0, len(selected))
	for _, series := range selected {
		// Postings candidates are authoritative only together with complete
		// identity. This final check also prevents regex/empty-label corner cases
		// from ever selecting an arbitrary cohort member.
		if !matchesAny(series.Labels, matcherSets) {
			continue
		}
		trimmed := trimSeries(series, mint, maxt)
		if trimmed.Len() > 0 {
			out = append(out, trimmed)
		}
	}
	slices.SortFunc(out, func(a, b Series) int { return labels.Compare(a.Labels, b.Labels) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// resolveMatcherSets applies sorted-list union and intersection over block-local
// stable series IDs. It never opens either labels or samples from Parquet.
// idScratchInitial bounds the scratch buffers' initial capacity. Sizing them to
// the block made every query allocate and zero memory proportional to block size,
// including queries matching a handful of series.
const idScratchInitial = 1024

// idScratch reuses the intermediate ID buffers of matcher resolution. Every
// union and intersection allocated a fresh slice, per block, per query.
//
// The returned ID slice is backed by scratch and is only valid until the next
// call; callers must consume it immediately.
type idScratch struct {
	bufs  [3][]uint64
	sets  [][]uint64
	index int
}

func newIDScratch(totalSeries int) *idScratch {
	var s idScratch
	for i := range s.bufs {
		s.bufs[i] = make([]uint64, 0, totalSeries)
	}
	return &s
}

// buffer rotates over three buffers, so no live input is ever the destination.
func (s *idScratch) buffer() []uint64 {
	s.index++
	return s.bufs[s.index%len(s.bufs)][:0]
}

func (b *Block) resolveMatcherSets(scratch *idScratch, matcherSets [][]*labels.Matcher) ([]uint64, error) {
	if len(matcherSets) == 0 {
		return b.allSeriesIDs(), nil
	}
	scratch.sets = scratch.sets[:0]
	all := b.allSeriesIDs()
	for _, matchers := range matcherSets {
		ids := all
		for _, matcher := range matchers {
			matching, err := b.positiveMatcherIDs(matcher)
			if err != nil {
				return nil, err
			}
			if matcher.Type == labels.MatchNotEqual || matcher.Type == labels.MatchNotRegexp {
				matching = subtractSortedIDsInto(scratch.buffer(), all, matching)
			}
			ids = intersectSortedIDsInto(scratch.buffer(), ids, matching)
			if len(ids) == 0 {
				break
			}
		}
		scratch.sets = append(scratch.sets, ids)
	}
	return unionSortedIDSetsInto(scratch.buffer(), scratch.sets), nil
}

// positiveMatcherIDs returns IDs whose stored or absent label would satisfy the
// positive form of matcher. Negation is handled explicitly by the caller.
func (b *Block) positiveMatcherIDs(matcher *labels.Matcher) ([]uint64, error) {
	if matcher == nil {
		return nil, errors.New("columnar: nil label matcher")
	}
	if matcher.Type == labels.MatchNotEqual || matcher.Type == labels.MatchNotRegexp {
		var err error
		matcher, err = matcher.Inverse()
		if err != nil {
			return nil, err
		}
	}
	switch matcher.Type {
	case labels.MatchEqual, labels.MatchNotEqual:
		if matcher.Value != "" {
			key := postingLookupKey(matcher.Name, matcher.Value)
			i, found := slices.BinarySearchFunc(b.catalog.Postings, key, func(posting blockPosting, key string) int {
				return compareStrings(postingLookupKey(posting.Name, posting.Value), key)
			})
			if !found {
				return nil, nil
			}
			return b.catalog.Postings[i].IDs, nil
		}
	case labels.MatchRegexp, labels.MatchNotRegexp:
	default:
		return nil, fmt.Errorf("columnar: unsupported matcher type %v", matcher.Type)
	}

	// Counts are accumulated locally: per-posting atomics would perturb the loop
	// being measured, which is the whole point of adding them.
	var scanned, byName, valueMatched uint64
	var timings *queryTimings
	if b.reads != nil {
		timings = b.reads.timings
		b.reads.matcherCalls.Inc()
	}
	// present is only consumed when the matcher can match the empty string, which is
	// how an absent label is handled. Computing it otherwise unions every posting of
	// the name for nothing: on a corpus with hundreds of metric names that dominated
	// the whole resolution, measured at 16.9 ms against 0.06 ms of regex matching.
	needPresent := matcher.Matches("")
	var matching, present []uint64
	for _, posting := range b.catalog.Postings {
		scanned++
		if posting.Name != matcher.Name {
			continue
		}
		byName++
		if needPresent {
			start := timings.now()
			present = unionSortedIDs(present, posting.IDs)
			timings.add(phaseUnion, start)
		}
		start := timings.now()
		start = timings.now()
		hit := matcher.Matches(posting.Value)
		timings.add(phaseMatcherMatch, start)
		if hit {
			valueMatched++
			start = timings.now()
			matching = unionSortedIDs(matching, posting.IDs)
			timings.add(phaseUnion, start)
		}
	}
	if b.reads != nil {
		b.reads.postingsScanned.Add(scanned)
		b.reads.postingsByName.Add(byName)
		b.reads.postingsValueMatch.Add(valueMatched)
	}
	// PromQL label matchers also apply to an absent label, whose value is empty.
	if needPresent {
		start := timings.now()
		matching = unionSortedIDs(matching, subtractSortedIDs(b.allSeriesIDs(), present))
		timings.add(phaseUnion, start)
	}
	return matching, nil
}

// allSeriesIDs returns the catalog's precomputed ID list. Callers must treat it
// as read-only.
func (b *Block) allSeriesIDs() []uint64 {
	return b.catalog.SeriesIDs
}

func postingLookupKey(name, value string) string { return name + "\x00" + value }

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func intersectSortedIDsInto(dst, a, b []uint64) []uint64 {
	dst = dst[:0]
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			dst = append(dst, a[i])
			i++
			j++
		}
	}
	return dst
}

func subtractSortedIDsInto(dst, a, b []uint64) []uint64 {
	dst = dst[:0]
	for i, j := 0, 0; i < len(a); i++ {
		for j < len(b) && b[j] < a[i] {
			j++
		}
		if j < len(b) && b[j] == a[i] {
			continue
		}
		dst = append(dst, a[i])
	}
	return dst
}

func unionSortedIDsInto(dst, a, b []uint64) []uint64 {
	dst = dst[:0]
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i == len(a):
			return append(dst, b[j:]...)
		case j == len(b):
			return append(dst, a[i:]...)
		case a[i] < b[j]:
			dst = append(dst, a[i])
			i++
		case a[i] > b[j]:
			dst = append(dst, b[j])
			j++
		default:
			dst = append(dst, a[i])
			i++
			j++
		}
	}
	return dst
}

func unionSortedIDSetsInto(dst []uint64, sets [][]uint64) []uint64 {
	if len(sets) == 0 {
		return dst[:0]
	}
	if len(sets) == 1 {
		return sets[0]
	}
	acc := sets[0]
	for _, set := range sets[1:] {
		acc = unionSortedIDsInto(dst, acc, set)
	}
	return acc
}

func intersectSortedIDs(a, b []uint64) []uint64 {
	out := make([]uint64, 0, min(len(a), len(b)))
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

func unionSortedIDs(a, b []uint64) []uint64 {
	out := make([]uint64, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i == len(a):
			out = append(out, b[j:]...)
			return out
		case j == len(b):
			out = append(out, a[i:]...)
			return out
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		case a[i] > b[j]:
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

func subtractSortedIDs(a, b []uint64) []uint64 {
	out := make([]uint64, 0, len(a))
	for i, j := 0, 0; i < len(a); i++ {
		for j < len(b) && b[j] < a[i] {
			j++
		}
		if j < len(b) && b[j] == a[i] {
			continue
		}
		out = append(out, a[i])
	}
	return out
}

func unionSortedIDSets(sets [][]uint64) []uint64 {
	if len(sets) == 0 {
		return nil
	}
	out := sets[0]
	for _, set := range sets[1:] {
		out = unionSortedIDs(out, set)
	}
	return out
}
